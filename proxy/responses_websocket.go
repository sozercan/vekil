package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/sozercan/vekil/logger"
)

const responsesWebSocketRequestHeaderPrefix = "ws_request_header_"

var (
	responsesWebSocketWriteWait  = 10 * time.Second
	responsesWebSocketPingPeriod = 30 * time.Second
)

var errResponsesWebSocketClientWrite = errors.New("responses websocket client write failed")

type responsesWebSocketClientWriteError struct {
	err error
}

func (e *responsesWebSocketClientWriteError) Error() string {
	if e == nil || e.err == nil {
		return errResponsesWebSocketClientWrite.Error()
	}
	return e.err.Error()
}

func (e *responsesWebSocketClientWriteError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

func (e *responsesWebSocketClientWriteError) Is(target error) bool {
	return target == errResponsesWebSocketClientWrite
}

func isResponsesWebSocketClientDisconnect(ctx context.Context, err error) bool {
	if errors.Is(err, errResponsesWebSocketClientWrite) {
		return true
	}
	return ctx != nil && ctx.Err() != nil
}

// errStreamFailedUpstream is a sentinel error indicating the upstream stream
// ended with response.failed or response.incomplete after forwarding the
// upstream failure event. This path also emits the standard websocket error
// payload so clients can surface the upstream error details.
var errStreamFailedUpstream = errors.New("upstream stream ended with response.failed or response.incomplete")

// streamFailedUpstreamError carries the HTTP status that an upstream
// response.failed/incomplete event was classified to (e.g. 429 for a rate
// limit, 503 for an overload), so the turn is recorded in stats with its exact
// semantic status rather than a generic 502. It unwraps to errStreamFailedUpstream
// so existing errors.Is checks keep working.
type streamFailedUpstreamError struct {
	status int
}

func (e *streamFailedUpstreamError) Error() string { return errStreamFailedUpstream.Error() }
func (e *streamFailedUpstreamError) Unwrap() error { return errStreamFailedUpstream }

// streamFailureStatus returns the classified status carried by a stream-failure
// error, or http.StatusBadGateway when none was attached.
func streamFailureStatus(err error) int {
	var sf *streamFailedUpstreamError
	if errors.As(err, &sf) && sf.status != 0 {
		return sf.status
	}
	return http.StatusBadGateway
}

var responsesWebSocketUpgrader = websocket.Upgrader{
	ReadBufferSize:    4096,
	WriteBufferSize:   4096,
	EnableCompression: true,
	CheckOrigin: func(r *http.Request) bool {
		return strings.TrimSpace(r.Header.Get("Origin")) == ""
	},
}

type responsesWebSocketCreateRequest struct {
	Type               string            `json:"type"`
	Model              string            `json:"model"`
	Input              []json.RawMessage `json:"input"`
	PreviousResponseID string            `json:"previous_response_id,omitempty"`
	Generate           *bool             `json:"generate,omitempty"`
	ClientMetadata     map[string]string `json:"client_metadata,omitempty"`
	signatureValue     string
	upstreamFields     []responsesWebSocketJSONField
}

type responsesWebSocketJSONField struct {
	key   string
	value json.RawMessage
}

type responsesWebSocketStreamEvent struct {
	Type     string `json:"type"`
	Response struct {
		ID                string                                    `json:"id"`
		Error             responsesWebSocketStreamError             `json:"error"`
		IncompleteDetails responsesWebSocketStreamIncompleteDetails `json:"incomplete_details"`
		Usage             responsesUsage                            `json:"usage"`
	} `json:"response,omitempty"`
	Item json.RawMessage `json:"item,omitempty"`
}

type responsesWebSocketStreamError struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

type responsesWebSocketStreamIncompleteDetails struct {
	Reason string `json:"reason"`
}

type responsesWebSocketSession struct {
	conn           *websocket.Conn
	ctx            context.Context
	baseHeaders    http.Header
	userAgent      string
	turnState      string
	turnMetadata   string
	lastResponseID string
	lastSignature  string
	historyItems   []json.RawMessage
	historyBytes   int
	toolContexts   *ToolExecutionContextStore
	toolScope      string
	done           chan struct{}
	doneOnce       sync.Once
	inflightMu     sync.Mutex
	inflightCancel context.CancelFunc
	inflightGen    uint64
}

type responsesWebSocketRequestPlan struct {
	signature          string
	resetHistory       bool
	currentInput       []json.RawMessage
	fullReplaySegments [][]json.RawMessage
	useTurnStateDelta  bool
	compactionChecked  bool
	compactionTrigger  bool
}

type responsesWebSocketRequestMetrics struct {
	deltaAttempted     bool
	deltaFallback      bool
	autoCompacted      bool
	compactedFromItems int
	compactedFromBytes int
	compactedToItems   int
	compactedToBytes   int
}

type responsesWebSocketHistoryCompaction struct {
	fromItems int
	fromBytes int
	toItems   int
	toBytes   int
}

func (p responsesWebSocketRequestPlan) upstreamSegments() [][]json.RawMessage {
	if p.useTurnStateDelta {
		return [][]json.RawMessage{p.currentInput}
	}
	return p.fullReplaySegments
}

func (p responsesWebSocketRequestPlan) historyUpdateInput() (bool, []json.RawMessage) {
	if p.hasCompactionTrigger() {
		return true, nil
	}
	return p.resetHistory, p.currentInput
}

func (p responsesWebSocketRequestPlan) hasCompactionTrigger() bool {
	if p.compactionChecked {
		return p.compactionTrigger
	}
	return responsesInputContainsCompactionTrigger(p.currentInput)
}

func responsesInputContainsCompactionTrigger(input []json.RawMessage) bool {
	for _, raw := range input {
		if !bytes.Contains(raw, responsesCompactionMarkerBytes) && !bytes.Contains(raw, []byte(`\u`)) {
			continue
		}
		if responsesInputItemType(raw) == "compaction_trigger" {
			return true
		}
	}
	return false
}

// HandleResponsesWebSocket handles GET /v1/responses websocket upgrades used
// by Codex. Each websocket request is translated into a normal upstream
// streaming /responses HTTP request and the SSE data payloads are forwarded back
// as websocket text frames.
func (h *ProxyHandler) HandleResponsesWebSocket(w http.ResponseWriter, r *http.Request) {
	if !h.responsesWebSocketConfig().Enabled {
		w.Header().Set("Connection", "Upgrade")
		w.Header().Set("Upgrade", "websocket")
		http.Error(w, http.StatusText(http.StatusUpgradeRequired), http.StatusUpgradeRequired)
		return
	}

	if !websocket.IsWebSocketUpgrade(r) {
		w.Header().Set("Connection", "Upgrade")
		w.Header().Set("Upgrade", "websocket")
		http.Error(w, http.StatusText(http.StatusUpgradeRequired), http.StatusUpgradeRequired)
		return
	}
	if h.responsesWebSocketIsDraining() {
		http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
		return
	}

	conn, err := responsesWebSocketUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()

	conn.SetReadLimit(maxRequestBodySize)
	session := newResponsesWebSocketSession(conn, r)
	session.startPingLoop()
	if !h.registerResponsesWebSocketSession(session) {
		session.sendGoingAwayWithDeadline(time.Now().Add(responsesWebSocketWriteWait))
		return
	}
	defer h.unregisterResponsesWebSocketSession(session)
	defer session.closeDone()

	for {
		messageType, payload, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if messageType != websocket.TextMessage {
			session.sendWrappedError(http.StatusBadRequest, "responses websocket only accepts text frames", "invalid_request_error", nil)
			continue
		}

		frameType, err := parseResponsesWebSocketFrameType(payload)
		if err != nil {
			session.sendWrappedError(http.StatusBadRequest, err.Error(), "invalid_request_error", nil)
			continue
		}
		if frameType == "response.processed" {
			continue
		}

		request, err := parseResponsesWebSocketCreateRequest(payload)
		if err != nil {
			session.sendWrappedError(http.StatusBadRequest, err.Error(), "invalid_request_error", nil)
			continue
		}

		if err := session.handleCreateRequest(h, request); err != nil {
			h.log.Debug("responses websocket request failed", logger.Err(err))
			continue
		}
	}
}

func (h *ProxyHandler) registerResponsesWebSocketSession(session *responsesWebSocketSession) bool {
	if h == nil || session == nil {
		return false
	}
	h.responsesWSSessionsMu.Lock()
	defer h.responsesWSSessionsMu.Unlock()
	if h.responsesWSDraining {
		return false
	}
	if h.responsesWSSessions == nil {
		h.responsesWSSessions = make(map[*responsesWebSocketSession]struct{})
	}
	h.responsesWSSessions[session] = struct{}{}
	return true
}

func (h *ProxyHandler) unregisterResponsesWebSocketSession(session *responsesWebSocketSession) {
	if h == nil || session == nil {
		return
	}
	h.responsesWSSessionsMu.Lock()
	defer h.responsesWSSessionsMu.Unlock()
	delete(h.responsesWSSessions, session)
}

func (h *ProxyHandler) responsesWebSocketIsDraining() bool {
	if h == nil {
		return true
	}
	h.responsesWSSessionsMu.Lock()
	defer h.responsesWSSessionsMu.Unlock()
	return h.responsesWSDraining
}

func (h *ProxyHandler) ShutdownWebSocketSessions(ctx context.Context) {
	if h == nil {
		return
	}
	h.responsesWSSessionsMu.Lock()
	h.responsesWSDraining = true
	sessions := make([]*responsesWebSocketSession, 0, len(h.responsesWSSessions))
	for session := range h.responsesWSSessions {
		sessions = append(sessions, session)
	}
	h.responsesWSSessionsMu.Unlock()

	for _, session := range sessions {
		select {
		case <-ctx.Done():
			return
		default:
		}
		session.sendGoingAwayWithDeadline(responsesWebSocketShutdownCloseDeadline(ctx))
	}
}

func (s *responsesWebSocketSession) closeDone() {
	if s == nil || s.done == nil {
		return
	}
	s.doneOnce.Do(func() { close(s.done) })
}

func (s *responsesWebSocketSession) startPingLoop() {
	if s == nil || s.conn == nil || responsesWebSocketPingPeriod <= 0 {
		return
	}
	ticker := time.NewTicker(responsesWebSocketPingPeriod)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				deadline := time.Now().Add(responsesWebSocketWriteWait)
				if err := s.conn.WriteControl(websocket.PingMessage, nil, deadline); err != nil {
					s.cancelInflight()
					_ = s.conn.Close()
					s.closeDone()
					return
				}
			case <-s.done:
				return
			}
		}
	}()
}

func responsesWebSocketShutdownCloseDeadline(ctx context.Context) time.Time {
	deadline := time.Now().Add(responsesWebSocketWriteWait)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		return ctxDeadline
	}
	return deadline
}

func (s *responsesWebSocketSession) sendGoingAwayWithDeadline(deadline time.Time) {
	if s == nil || s.conn == nil {
		return
	}
	s.cancelInflight()
	if deadline.IsZero() {
		deadline = time.Now().Add(responsesWebSocketWriteWait)
	}
	_ = s.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseGoingAway, "server shutting down"), deadline)
	_ = s.conn.Close()
	s.closeDone()
}

// setInflightCancel records the cancel func for the current in-flight upstream
// call and returns a generation token. The token must be passed to
// clearInflightCancel so that only the matching cancel is cleared: nested or
// subsequent in-flight calls (e.g. auto-compaction inside a create request)
// bump the generation, and a stale clear is then a no-op rather than nilling
// out a newer cancel. (context.CancelFunc values are not comparable, so the
// generation token stands in for identity.)
func (s *responsesWebSocketSession) setInflightCancel(cancel context.CancelFunc) uint64 {
	if s == nil || cancel == nil {
		return 0
	}
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()
	s.inflightGen++
	s.inflightCancel = cancel
	return s.inflightGen
}

func (s *responsesWebSocketSession) clearInflightCancel(gen uint64) {
	if s == nil || gen == 0 {
		return
	}
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()
	if s.inflightGen == gen {
		s.inflightCancel = nil
	}
}

func (s *responsesWebSocketSession) cancelInflight() {
	if s == nil {
		return
	}
	s.inflightMu.Lock()
	cancel := s.inflightCancel
	s.inflightMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func parseResponsesWebSocketFrameType(payload []byte) (string, error) {
	var envelope struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return "", fmt.Errorf("invalid JSON in websocket request")
	}
	return envelope.Type, nil
}

func newResponsesWebSocketSession(conn *websocket.Conn, r *http.Request) *responsesWebSocketSession {
	baseHeaders := make(http.Header)
	for _, name := range []string{
		"X-Codex-Beta-Features",
		"OpenAI-Beta",
		"session_id",
		"session-id",
		"thread-id",
		"X-Client-Request-Id",
		"X-Codex-Installation-Id",
		"X-Codex-Inference-Call-Id",
		"X-Codex-Parent-Thread-Id",
		"X-Codex-Window-Id",
		"X-OAI-Attestation",
		"X-OpenAI-Memgen-Request",
		"X-OpenAI-Subagent",
		"X-ResponsesAPI-Include-Timing-Metrics",
		"Traceparent",
		"Tracestate",
	} {
		for _, value := range r.Header.Values(name) {
			baseHeaders.Add(name, value)
		}
	}

	return &responsesWebSocketSession{
		conn:        conn,
		ctx:         r.Context(),
		baseHeaders: baseHeaders,
		userAgent:   r.Header.Get("User-Agent"),
		// Codex treats X-Codex-Turn-State as server-issued, turn-scoped
		// sticky-routing state. This bridge only trusts state it received from
		// upstream during this proxy-owned websocket session.
		toolContexts: NewToolExecutionContextStore(),
		toolScope:    "responses-ws:" + uuid.NewString(),
		done:         make(chan struct{}),
	}
}

func parseResponsesWebSocketCreateRequest(payload []byte) (*responsesWebSocketCreateRequest, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(payload, &raw); err != nil {
		return nil, fmt.Errorf("invalid JSON in websocket request")
	}

	var request responsesWebSocketCreateRequest
	if err := json.Unmarshal(payload, &request); err != nil {
		return nil, fmt.Errorf("invalid websocket request body")
	}
	if request.Type != "response.create" {
		return nil, fmt.Errorf("unsupported websocket request type %q", request.Type)
	}
	if request.Input == nil {
		request.Input = []json.RawMessage{}
	}
	signatureValue, upstreamFields, err := prepareResponsesWebSocketRequest(raw)
	if err != nil {
		return nil, fmt.Errorf("failed to encode websocket request")
	}
	request.signatureValue = signatureValue
	request.upstreamFields = upstreamFields
	return &request, nil
}

func prepareResponsesWebSocketRequest(raw map[string]json.RawMessage) (string, []responsesWebSocketJSONField, error) {
	signatureBody := make(map[string]json.RawMessage, len(raw))
	keys := make([]string, 0, len(raw))
	for key, value := range raw {
		switch key {
		case "type", "input", "previous_response_id", "generate", "client_metadata", "initiator":
		default:
			signatureBody[key] = value
		}

		switch key {
		case "type", "input", "previous_response_id", "generate", "client_metadata", "initiator", "stream":
		default:
			keys = append(keys, key)
		}
	}

	signatureBytes, err := json.Marshal(signatureBody)
	if err != nil {
		return "", nil, err
	}

	sort.Strings(keys)
	fields := make([]responsesWebSocketJSONField, 0, len(keys))
	for _, key := range keys {
		fields = append(fields, responsesWebSocketJSONField{
			key:   key,
			value: raw[key],
		})
	}

	return string(signatureBytes), fields, nil
}

func (r *responsesWebSocketCreateRequest) signature() string {
	return r.signatureValue
}

func (r *responsesWebSocketCreateRequest) upstreamBody(inputSegments ...[]json.RawMessage) ([]byte, error) {
	capacity := len(r.upstreamFields)*16 + rawMessageSegmentsSize(inputSegments...) + 32
	var buf bytes.Buffer
	buf.Grow(capacity)
	buf.WriteByte('{')
	first := true
	for _, field := range r.upstreamFields {
		if !first {
			buf.WriteByte(',')
		}
		first = false
		buf.WriteString(strconv.Quote(field.key))
		buf.WriteByte(':')
		buf.Write(field.value)
	}
	if !first {
		buf.WriteByte(',')
	}
	buf.WriteString(`"input":[`)
	if err := writeRawMessageSegments(&buf, inputSegments...); err != nil {
		return nil, err
	}
	buf.WriteString(`],"stream":true}`)
	return buf.Bytes(), nil
}

func (s *responsesWebSocketSession) handleCreateRequest(h *ProxyHandler, request *responsesWebSocketCreateRequest) error {
	s.syncTurnMetadata(request)
	if request.PreviousResponseID == "" {
		s.turnState = ""
	}

	plan, err := s.planRequest(h, request)
	if err != nil {
		s.sendWrappedError(http.StatusBadRequest, err.Error(), "invalid_request_error", nil)
		return err
	}
	metrics := responsesWebSocketRequestMetrics{}

	if request.Generate != nil && !*request.Generate {
		responseID := "vekil-ws-" + uuid.NewString()
		s.rememberPlannedResponse(plan, responseID, nil)
		s.logRequestMetrics(h, request, responseID, metrics)
		if err := s.writeJSON(map[string]interface{}{
			"type": "response.created",
			"response": map[string]interface{}{
				"id": responseID,
			},
		}); err != nil {
			return err
		}
		return s.writeJSON(map[string]interface{}{
			"type": "response.completed",
			"response": map[string]interface{}{
				"id":    responseID,
				"usage": zeroResponsesUsage(),
			},
		})
	}

	upstreamCtx, upstreamCancel := h.newInferenceUpstreamContext(true)
	// The websocket bridge records each turn as tracked traffic (recordTurnStats),
	// so mark the per-turn upstream context as retry-trackable too — otherwise a
	// retryable 429/503 on a turn would be invisible in the dashboard retry
	// counters. GET /v1/responses is not an inference route, so the middleware
	// never marks it; do it explicitly here.
	upstreamCtx = markRetryStatsTracked(upstreamCtx)
	inflightGen := s.setInflightCancel(upstreamCancel)
	defer func() {
		s.clearInflightCancel(inflightGen)
		upstreamCancel()
	}()

	resp, translated, translatedHeaders, err := h.prepareResponsesStream(s.ctx, upstreamCtx, request.Model, func() (*http.Response, error) {
		attemptPlan, err := s.planRequest(h, request)
		if err != nil {
			return nil, err
		}
		attemptResp, attemptDeltaAttempted, attemptDeltaFallback, err := s.postCreateRequest(h, upstreamCtx, request, attemptPlan)
		metrics.deltaAttempted = metrics.deltaAttempted || attemptDeltaAttempted
		metrics.deltaFallback = metrics.deltaFallback || attemptDeltaFallback
		return attemptResp, err
	})
	if err != nil {
		s.sendWrappedError(upstreamStatusCode(err, http.StatusBadGateway), fmt.Sprintf("upstream request failed: %v", err), "server_error", nil)
		return err
	}
	if translated != nil {
		code := ""
		if translated.failure != nil {
			code = strings.TrimSpace(translated.failure.Response.Error.Code)
		}
		s.sendWrappedError(translated.status, translated.message, code, translatedHeaders)
		// A precommit translated failure (e.g. unsupported-model or rate-limit
		// surfaced before the stream is handed off) is still a failed turn; count
		// it so it appears in the dashboard error stats and recent log, matching
		// the non-200 and stream-error branches below.
		s.recordTurnStats(h, request.Model, translated.status, responsesUsage{})
		return nil
	}
	if resp == nil {
		s.sendWrappedError(http.StatusBadGateway, "upstream request failed", "server_error", nil)
		return fmt.Errorf("upstream websocket bridge returned no response")
	}
	defer func() { _ = resp.Body.Close() }()

	s.updateTurnState(resp.Header)

	if resp.StatusCode != http.StatusOK {
		respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if readErr != nil {
			respBody = nil
		}
		message, code := extractResponsesWebSocketError(resp.StatusCode, respBody)
		s.sendWrappedError(resp.StatusCode, message, code, resp.Header)
		s.recordTurnStats(h, request.Model, resp.StatusCode, responsesUsage{})
		return fmt.Errorf("upstream websocket bridge status %d", resp.StatusCode)
	}

	responseID, outputItems, turnUsage, err := s.streamUpstreamResponse(h, resp.Body, resp.Header)
	if err != nil {
		if isResponsesWebSocketClientDisconnect(s.ctx, err) {
			return err
		}
		if errors.Is(err, errStreamFailedUpstream) {
			// The upstream sent response.failed/incomplete; count it as an errored
			// turn so it shows in the dashboard's error stats and recent log, with
			// the exact classified status (e.g. 429/503) the client was sent rather
			// than a generic 502.
			s.recordTurnStats(h, request.Model, streamFailureStatus(err), responsesUsage{})
			return nil
		}
		s.sendWrappedError(http.StatusBadGateway, err.Error(), "server_error", nil)
		s.recordTurnStats(h, request.Model, http.StatusBadGateway, responsesUsage{})
		return err
	}

	// Record this successful bridge turn into traffic stats. The bridge does not
	// flow through the HTTP request middleware, so it is recorded directly. Every
	// completed turn is counted (with whatever usage it carried, possibly zero),
	// matching the HTTP path's request accounting.
	s.recordTurnStats(h, request.Model, http.StatusOK, turnUsage)

	s.rememberPlannedResponse(plan, responseID, outputItems)
	metrics = s.maybeAutoCompactHistory(h, request, metrics)
	s.logRequestMetrics(h, request, responseID, metrics)
	return nil
}

// recordTurnStats records one websocket-bridge turn into traffic stats,
// resolving the provider for attribution. status is the turn outcome (200 for a
// completed turn, an error status for a failed one).
func (s *responsesWebSocketSession) recordTurnStats(h *ProxyHandler, model string, status int, usage responsesUsage) {
	providerID, providerKind := "", ""
	if provider, _, _ := h.resolveProviderModel(model, providerEndpointResponses); provider != nil {
		providerID, providerKind = provider.id, string(provider.kind)
	}
	h.RecordResponsesTurn(model, providerID, providerKind, classifyAgent(s.userAgent), status, usage)
}

func (s *responsesWebSocketSession) planRequest(h *ProxyHandler, request *responsesWebSocketCreateRequest) (responsesWebSocketRequestPlan, error) {
	plan := responsesWebSocketRequestPlan{
		signature:         request.signature(),
		currentInput:      request.Input,
		compactionChecked: true,
		compactionTrigger: responsesInputContainsCompactionTrigger(request.Input),
	}
	if request.PreviousResponseID == "" {
		plan.resetHistory = true
		plan.fullReplaySegments = [][]json.RawMessage{request.Input}
		return plan, nil
	}
	if request.PreviousResponseID != s.lastResponseID {
		return responsesWebSocketRequestPlan{}, fmt.Errorf("unknown previous_response_id %q for websocket session", request.PreviousResponseID)
	}
	if plan.signature != s.lastSignature {
		return responsesWebSocketRequestPlan{}, fmt.Errorf("incremental websocket request changed non-input fields")
	}

	plan.fullReplaySegments = [][]json.RawMessage{s.historyItems, request.Input}
	cfg := h.responsesWebSocketConfig()
	plan.useTurnStateDelta = cfg.TurnStateDelta && s.turnState != "" && !plan.hasCompactionTrigger()
	return plan, nil
}

func (s *responsesWebSocketSession) postCreateRequest(h *ProxyHandler, ctx context.Context, request *responsesWebSocketCreateRequest, plan responsesWebSocketRequestPlan) (*http.Response, bool, bool, error) {
	resp, err := s.postCreateRequestSegments(h, ctx, request, plan.upstreamSegments(), plan.useTurnStateDelta)
	if err != nil || resp == nil {
		return resp, plan.useTurnStateDelta, false, err
	}
	if !plan.useTurnStateDelta {
		resp, err = s.maybeRetryCompactedCreateRequest(h, ctx, request, resp, true)
		return resp, false, false, err
	}
	if resp.StatusCode == http.StatusOK {
		return resp, true, false, nil
	}

	respBody, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	_, code := extractResponsesWebSocketError(resp.StatusCode, respBody)
	if readErr != nil || resp.StatusCode != http.StatusBadRequest || code != "invalid_turn_state" {
		resp.Body = io.NopCloser(bytes.NewReader(respBody))
		return resp, true, false, nil
	}
	h.log.Debug("responses websocket delta replay failed; retrying full history",
		logger.F("model", request.Model),
		logger.F("previous_response_id", request.PreviousResponseID),
		logger.F("had_turn_state", s.turnState != ""),
		logger.F("delta_attempted", true),
		logger.F("delta_fallback", true),
	)
	s.turnState = ""

	resp, err = s.postCreateRequestSegments(h, ctx, request, plan.fullReplaySegments, false)
	if err != nil || resp == nil {
		return resp, true, true, err
	}
	resp, err = s.maybeRetryCompactedCreateRequest(h, ctx, request, resp, true)
	return resp, true, true, err
}

func (s *responsesWebSocketSession) postCreateRequestSegments(h *ProxyHandler, ctx context.Context, request *responsesWebSocketCreateRequest, inputSegments [][]json.RawMessage, includeTurnState bool) (*http.Response, error) {
	bodyBytes, err := request.upstreamBody(inputSegments...)
	if err != nil {
		return nil, err
	}
	bodyBytes = h.rewriteResponsesRequestBodyWithToolOptimizers(ctx, bodyBytes, "responses/websocket", true, s.toolContexts, s.toolScope)
	headers := s.requestHeaders(request, includeTurnState)
	// The websocket bridge records each turn's usage downstream from the streamed
	// response body (recordTurnStats), so the per-turn usage total returned here
	// is not observed separately — discard it.
	if compactionResp, handled, _, err := h.maybeBuildResponsesCompactionTriggerResponse(ctx, bodyBytes, headers, true); handled || err != nil {
		return compactionResp, err
	}
	return h.postResponsesWithHeaders(ctx, bodyBytes, headers)
}

func (s *responsesWebSocketSession) requestHeaders(request *responsesWebSocketCreateRequest, includeTurnState bool) http.Header {
	headers := make(http.Header)
	mergeHeaderValues(headers, s.baseHeaders)

	for key, value := range request.ClientMetadata {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}

		if name := responsesWebSocketMetadataHeaderName(key); name != "" {
			headers.Set(name, trimmed)
			continue
		}

		switch {
		case strings.HasPrefix(key, responsesWebSocketRequestHeaderPrefix):
			name := strings.TrimSpace(strings.TrimPrefix(key, responsesWebSocketRequestHeaderPrefix))
			if name != "" && !strings.EqualFold(name, "X-Codex-Turn-State") {
				headers.Set(name, trimmed)
			}
		}
	}

	if includeTurnState && s.turnState != "" {
		headers.Set("X-Codex-Turn-State", s.turnState)
	}

	return headers
}

func (s *responsesWebSocketSession) syncTurnMetadata(request *responsesWebSocketCreateRequest) {
	turnMetadata := strings.TrimSpace(s.requestHeaders(request, false).Get("X-Codex-Turn-Metadata"))
	turnKey := responsesWebSocketTurnMetadataKey(turnMetadata)
	if turnKey == s.turnMetadata {
		return
	}
	if s.turnState != "" {
		s.turnState = ""
	}
	s.turnMetadata = turnKey
}

func responsesWebSocketTurnMetadataKey(turnMetadata string) string {
	turnMetadata = strings.TrimSpace(turnMetadata)
	if turnMetadata == "" {
		return ""
	}

	var metadata map[string]json.RawMessage
	if err := json.Unmarshal([]byte(turnMetadata), &metadata); err == nil {
		var turnID string
		if rawTurnID, ok := metadata["turn_id"]; ok && json.Unmarshal(rawTurnID, &turnID) == nil {
			if turnID = strings.TrimSpace(turnID); turnID != "" {
				// Codex can enrich turn metadata during a turn while keeping
				// the turn_id stable; only a different turn_id resets state.
				return "turn_id:" + turnID
			}
		}
	}

	return "metadata:" + turnMetadata
}

func responsesWebSocketMetadataHeaderName(key string) string {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "x-codex-installation-id":
		return "X-Codex-Installation-Id"
	case "x-codex-inference-call-id":
		return "X-Codex-Inference-Call-Id"
	case "x-codex-parent-thread-id":
		return "X-Codex-Parent-Thread-Id"
	case "x-codex-turn-metadata":
		return "X-Codex-Turn-Metadata"
	case "x-codex-window-id":
		return "X-Codex-Window-Id"
	case "x-codex-ws-stream-request-start-ms":
		return "X-Codex-WS-Stream-Request-Start-Ms"
	case "x-oai-attestation":
		return "X-OAI-Attestation"
	case "x-openai-memgen-request":
		return "X-OpenAI-Memgen-Request"
	case "x-openai-subagent":
		return "X-OpenAI-Subagent"
	case "x-responsesapi-include-timing-metrics":
		return "X-ResponsesAPI-Include-Timing-Metrics"
	default:
		return ""
	}
}

func (s *responsesWebSocketSession) updateTurnState(headers http.Header) {
	if turnState := strings.TrimSpace(headers.Get("X-Codex-Turn-State")); turnState != "" {
		s.turnState = turnState
	}
}

func (s *responsesWebSocketSession) rememberResponse(resetHistory bool, responseID, signature string, currentInput, outputItems []json.RawMessage) {
	s.lastResponseID = responseID
	s.lastSignature = signature
	if resetHistory {
		s.historyItems = nil
		s.historyBytes = 0
	}
	s.historyItems = append(s.historyItems, currentInput...)
	s.historyItems = append(s.historyItems, outputItems...)
	s.historyBytes += rawMessagesSize(currentInput) + rawMessagesSize(outputItems)
}

func (s *responsesWebSocketSession) rememberPlannedResponse(plan responsesWebSocketRequestPlan, responseID string, outputItems []json.RawMessage) {
	if plan.hasCompactionTrigger() {
		s.turnState = ""
	}
	resetHistory, historyInput := plan.historyUpdateInput()
	s.rememberResponse(resetHistory, responseID, plan.signature, historyInput, outputItems)
}

func (s *responsesWebSocketSession) maybeAutoCompactHistory(h *ProxyHandler, request *responsesWebSocketCreateRequest, metrics responsesWebSocketRequestMetrics) responsesWebSocketRequestMetrics {
	ctx, cancel := h.newInferenceUpstreamContext(true)
	// Mark the compaction upstream context as retry-trackable, like the turn
	// itself, so retries during auto-compaction are counted in retry stats.
	ctx = markRetryStatsTracked(ctx)
	inflightGen := s.setInflightCancel(cancel)
	defer func() {
		s.clearInflightCancel(inflightGen)
		cancel()
	}()

	budget := newCompactBudget(h.effectiveCompactMaxAttempts())
	compaction, compacted, err := s.compactHistory(h, ctx, request, false, budget)
	if err != nil {
		h.log.Debug("responses websocket auto-compaction failed",
			logger.Err(err),
			logger.F("model", request.Model),
			logger.F("history_items", len(s.historyItems)),
			logger.F("history_bytes", s.currentHistoryBytes()),
		)
		return metrics
	}
	if !compacted {
		return metrics
	}

	// Auto-compaction spends upstream /responses tokens on an internal compact
	// call that does not flow through the stats middleware. Record that usage as
	// its own turn so long auto-compacting websocket sessions do not underreport
	// total Responses token spend.
	if compactionUsage := budget.usageTotals(); !compactionUsage.isZero() {
		s.recordTurnStats(h, request.Model, http.StatusOK, compactionUsage)
	}

	h.log.Debug("responses websocket auto-compacted history",
		logger.F("prior_items", compaction.fromItems),
		logger.F("prior_bytes", compaction.fromBytes),
		logger.F("new_items", compaction.toItems),
		logger.F("new_bytes", compaction.toBytes),
		logger.F("auto_compacted", true),
	)
	metrics.autoCompacted = true
	metrics.compactedFromItems = compaction.fromItems
	metrics.compactedFromBytes = compaction.fromBytes
	metrics.compactedToItems = compaction.toItems
	metrics.compactedToBytes = compaction.toBytes
	return metrics
}

func (s *responsesWebSocketSession) maybeRetryCompactedCreateRequest(h *ProxyHandler, ctx context.Context, request *responsesWebSocketCreateRequest, resp *http.Response, fullReplayUsed bool) (*http.Response, error) {
	if resp == nil || !fullReplayUsed || resp.StatusCode != http.StatusRequestEntityTooLarge || strings.TrimSpace(request.PreviousResponseID) == "" {
		return resp, nil
	}

	cfg := h.responsesWebSocketConfig()
	configuredKeepTail := cfg.AutoCompactKeepTail
	if configuredKeepTail <= 0 || strings.TrimSpace(request.Model) == "" {
		return resp, nil
	}

	originalHistory := cloneRawMessages(s.historyItems)
	keepTailSchedule := compactedResponsesRetryKeepTailSchedule(len(originalHistory), configuredKeepTail)
	if len(keepTailSchedule) == 0 {
		return resp, nil
	}

	respBody, truncated, err := readBodyWithCap(resp.Body, compactUpstreamErrorBodySize)
	if err != nil {
		_ = resp.Body.Close()
		return nil, err
	}
	_ = resp.Body.Close()
	lastResp := cloneHTTPResponseWithBody(resp, respBody)
	if truncated {
		lastResp.Header.Del("Content-Length")
		h.log.Debug("truncated initial upstream 413 response body for websocket compact fallback",
			logger.F("status", resp.StatusCode),
			logger.F("max_bytes", compactUpstreamErrorBodySize),
		)
	}

	budget := newCompactBudget(h.effectiveCompactMaxAttempts())
	// The 413 oversized-replay fallback spends upstream /responses tokens on
	// internal compaction calls (accumulated into budget) before retrying the
	// turn. That spend does not flow through the stats middleware, and the retry
	// turn's own usage is recorded separately downstream, so record the
	// compaction usage here as its own turn on every exit path — matching the
	// auto-compaction path — so 413-fallback sessions do not underreport spend.
	defer func() {
		if compactionUsage := budget.usageTotals(); !compactionUsage.isZero() {
			s.recordTurnStats(h, request.Model, http.StatusOK, compactionUsage)
		}
	}()
	for attempt, keepTail := range keepTailSchedule {
		compactedHistory, compaction, compacted, err := s.compactHistoryItemsWithKeepTail(h, ctx, request, originalHistory, keepTail, budget)
		if err != nil {
			h.log.Debug("responses websocket 413 compaction failed",
				logger.Err(err),
				logger.F("model", request.Model),
				logger.F("previous_response_id", request.PreviousResponseID),
				logger.F("history_items", len(originalHistory)),
				logger.F("history_bytes", rawMessagesSize(originalHistory)),
				logger.F("keep_tail", keepTail),
			)
			return lastResp, nil
		}
		if !compacted {
			continue
		}

		s.historyItems = compactedHistory
		s.historyBytes = rawMessagesSize(compactedHistory)
		h.log.Debug("responses websocket compacted oversized replay; retrying request",
			logger.F("model", request.Model),
			logger.F("previous_response_id", request.PreviousResponseID),
			logger.F("prior_items", compaction.fromItems),
			logger.F("prior_bytes", compaction.fromBytes),
			logger.F("new_items", compaction.toItems),
			logger.F("new_bytes", compaction.toBytes),
			logger.F("keep_tail", keepTail),
			logger.F("tail_attempt", attempt+1),
			logger.F("tail_attempts", len(keepTailSchedule)),
		)

		retryResp, retryErr := s.postCreateRequestSegments(h, ctx, request, [][]json.RawMessage{s.historyItems, request.Input}, false)
		if retryErr != nil {
			h.log.Debug("responses websocket 413 retry request failed", logger.F("keep_tail", keepTail), logger.Err(retryErr))
			return lastResp, nil
		}
		if retryResp == nil {
			return lastResp, nil
		}
		if retryResp.StatusCode != http.StatusRequestEntityTooLarge {
			return retryResp, nil
		}

		retryBody, truncated, readErr := readBodyWithCap(retryResp.Body, compactUpstreamErrorBodySize)
		_ = retryResp.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		lastResp = cloneHTTPResponseWithBody(retryResp, retryBody)
		if truncated {
			lastResp.Header.Del("Content-Length")
		}

		if attempt+1 < len(keepTailSchedule) {
			h.log.Debug("responses websocket 413 retry still too large; reducing keep tail",
				logger.F("keep_tail", keepTail),
				logger.F("next_keep_tail", keepTailSchedule[attempt+1]),
				logger.F("tail_attempt", attempt+1),
				logger.F("tail_attempts", len(keepTailSchedule)),
			)
		}
	}

	return lastResp, nil
}

func (s *responsesWebSocketSession) compactHistory(h *ProxyHandler, ctx context.Context, request *responsesWebSocketCreateRequest, force bool, budget *compactBudget) (responsesWebSocketHistoryCompaction, bool, error) {
	var result responsesWebSocketHistoryCompaction

	cfg := h.responsesWebSocketConfig()
	if force {
		if cfg.AutoCompactKeepTail <= 0 {
			return result, false, nil
		}
	} else {
		if !cfg.autoCompactEnabled() {
			return result, false, nil
		}
		if !responsesWebSocketHistoryExceedsThresholdWithBytes(s.historyItems, s.currentHistoryBytes(), cfg) {
			return result, false, nil
		}
	}
	if strings.TrimSpace(request.Model) == "" {
		return result, false, nil
	}

	keepTail := cfg.AutoCompactKeepTail
	if !force {
		keepTail = responsesWebSocketAutoCompactKeepTail(s.historyItems, cfg)
	}

	compacted, result, ok, err := s.compactHistoryItemsWithKeepTail(h, ctx, request, s.historyItems, keepTail, budget)
	if err != nil || !ok {
		return result, ok, err
	}
	s.historyItems = compacted
	s.historyBytes = rawMessagesSize(compacted)
	return result, true, nil
}

func responsesWebSocketAutoCompactKeepTail(items []json.RawMessage, cfg ResponsesWebSocketConfig) int {
	if cfg.AutoCompactKeepTail <= 0 || cfg.AutoCompactMaxBytes <= 0 {
		return cfg.AutoCompactKeepTail
	}

	schedule := compactedResponsesRetryKeepTailSchedule(len(items), cfg.AutoCompactKeepTail)
	if len(schedule) == 0 {
		return cfg.AutoCompactKeepTail
	}

	for _, keepTail := range schedule {
		prefixLen := compactedResponsesAlignedPrefixLen(items, keepTail)
		if prefixLen <= 0 {
			continue
		}
		if rawMessagesSize(items[prefixLen:]) < cfg.AutoCompactMaxBytes {
			return keepTail
		}
	}
	return schedule[len(schedule)-1]
}

func (s *responsesWebSocketSession) compactHistoryItemsWithKeepTail(h *ProxyHandler, ctx context.Context, request *responsesWebSocketCreateRequest, history []json.RawMessage, keepTail int, budget *compactBudget) ([]json.RawMessage, responsesWebSocketHistoryCompaction, bool, error) {
	var result responsesWebSocketHistoryCompaction
	if keepTail <= 0 || strings.TrimSpace(request.Model) == "" {
		return nil, result, false, nil
	}

	prefixLen := compactedResponsesAlignedPrefixLen(history, keepTail)
	if prefixLen <= 0 {
		return nil, result, false, nil
	}

	prefix := history[:prefixLen]
	tail := history[prefixLen:]
	result.fromItems = len(history)
	result.fromBytes = rawMessagesSize(history)

	var summary string
	var err error
	if budget == nil {
		summary, err = h.compactResponsesInput(ctx, request.Model, prefix, s.requestHeaders(request, false))
	} else {
		summary, err = h.compactResponsesInputWithBudget(ctx, request.Model, prefix, s.requestHeaders(request, false), budget)
	}
	if err != nil {
		return nil, result, false, err
	}

	checkpoint, err := proxyCompactionContextRawMessage(summary)
	if err != nil {
		return nil, result, false, err
	}

	compacted := make([]json.RawMessage, 0, 1+len(tail))
	compacted = append(compacted, checkpoint)
	compacted = append(compacted, tail...)

	result.toItems = len(compacted)
	result.toBytes = rawMessagesSize(compacted)
	return compacted, result, true, nil
}

func (s *responsesWebSocketSession) logRequestMetrics(h *ProxyHandler, request *responsesWebSocketCreateRequest, responseID string, metrics responsesWebSocketRequestMetrics) {
	h.log.Debug("responses websocket request completed",
		logger.F("model", request.Model),
		logger.F("previous_response_id", request.PreviousResponseID),
		logger.F("response_id", responseID),
		logger.F("delta_attempted", metrics.deltaAttempted),
		logger.F("delta_fallback", metrics.deltaFallback),
		logger.F("auto_compacted", metrics.autoCompacted),
		logger.F("history_items", len(s.historyItems)),
		logger.F("history_bytes", s.currentHistoryBytes()),
		logger.F("compacted_from_items", metrics.compactedFromItems),
		logger.F("compacted_from_bytes", metrics.compactedFromBytes),
		logger.F("compacted_to_items", metrics.compactedToItems),
		logger.F("compacted_to_bytes", metrics.compactedToBytes),
	)
}

func (s *responsesWebSocketSession) streamUpstreamResponse(h *ProxyHandler, body io.Reader, headers http.Header) (string, []json.RawMessage, responsesUsage, error) {
	// Emit a synthetic metadata event so WebSocket clients can discover the
	// actual model used. The Codex CLI parses openai-model from
	// codex.response.metadata frames via response_model().
	if mappedHeaders := responsesWebSocketMetadataHeaders(headers); len(mappedHeaders) > 0 {
		if err := s.writeJSON(map[string]interface{}{
			"type":    "codex.response.metadata",
			"headers": mappedHeaders,
		}); err != nil {
			return "", nil, responsesUsage{}, &responsesWebSocketClientWriteError{err: err}
		}
	}

	var responseID string
	var outputItems []json.RawMessage
	var turnUsage responsesUsage
	sawCompleted := false
	sawSemanticEvent := false

	if err := consumeResponsesSSEData(body, func(data string) error {
		if data == "" || data == "[DONE]" {
			return nil
		}

		var event responsesWebSocketStreamEvent
		parsedEvent := json.Unmarshal([]byte(data), &event) == nil
		if !sawSemanticEvent {
			sawSemanticEvent = true
			if parsedEvent && event.Type == "response.failed" {
				if status, _, ok := classifyPrecommitResponsesFailure(event); ok {
					s.sendWrappedError(status, responsesPrecommitErrorMessage(event, status), strings.TrimSpace(event.Response.Error.Code), headers)
					return &streamFailedUpstreamError{status: status}
				}
			}
		}

		_ = s.conn.SetWriteDeadline(time.Now().Add(responsesWebSocketWriteWait))
		if err := s.conn.WriteMessage(websocket.TextMessage, []byte(data)); err != nil {
			return &responsesWebSocketClientWriteError{err: err}
		}

		if !parsedEvent {
			return nil
		}

		switch event.Type {
		case "response.created":
			if responseID == "" && event.Response.ID != "" {
				responseID = event.Response.ID
			}
		case "response.output_item.done":
			if len(event.Item) > 0 {
				if h != nil {
					h.maybeRewriteOrCaptureToolCommandItem(s.ctx, event.Item, s.toolContexts, s.toolScope, false)
				}
				outputItems = append(outputItems, cloneRawMessage(event.Item))
			}
		case "response.completed":
			sawCompleted = true
			if event.Response.ID != "" {
				responseID = event.Response.ID
			}
			if !event.Response.Usage.isZero() {
				turnUsage = event.Response.Usage
			}
		case "response.failed", "response.incomplete":
			s.sendUpstreamStreamFailure(event, headers)
			// Return the sentinel immediately to break out of the SSE
			// scanner loop. The failure event has already been forwarded to
			// the client above, and we also emit a standard error payload so
			// websocket clients can surface the upstream error details. Carry the
			// classified status so the turn is recorded with its exact semantic
			// status (e.g. 429/503) rather than a generic 502.
			status, _, _ := responsesWebSocketStreamFailureDetails(event)
			return &streamFailedUpstreamError{status: status}
		}

		return nil
	}); err != nil && !errors.Is(err, errStreamFailedUpstream) {
		return "", nil, responsesUsage{}, err
	} else if errors.Is(err, errStreamFailedUpstream) {
		return "", nil, responsesUsage{}, err
	}

	if sawCompleted {
		if responseID == "" {
			return "", nil, responsesUsage{}, fmt.Errorf("response.completed missing response id")
		}
		return responseID, outputItems, turnUsage, nil
	}
	return "", nil, responsesUsage{}, fmt.Errorf("stream ended before response.completed")
}

func (s *responsesWebSocketSession) writeJSON(payload interface{}) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_ = s.conn.SetWriteDeadline(time.Now().Add(responsesWebSocketWriteWait))
	return s.conn.WriteMessage(websocket.TextMessage, encoded)
}

func (s *responsesWebSocketSession) sendUpstreamStreamFailure(event responsesWebSocketStreamEvent, headers http.Header) {
	status, message, code := responsesWebSocketStreamFailureDetails(event)
	if status == 0 || strings.TrimSpace(message) == "" {
		return
	}
	s.sendWrappedError(status, message, code, headers)
}

func (s *responsesWebSocketSession) sendWrappedError(status int, message, code string, headers http.Header) {
	payload := map[string]interface{}{
		"type":        "error",
		"status_code": status,
		"error": map[string]interface{}{
			"message": message,
		},
	}
	if code != "" {
		payload["error"].(map[string]interface{})["code"] = code
	}
	if mappedHeaders := flattenResponsesWebSocketHeaders(headers); len(mappedHeaders) > 0 {
		payload["headers"] = mappedHeaders
	}
	_ = s.writeJSON(payload)
}

func consumeResponsesSSEData(body io.Reader, onData func(string) error) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, openAIStreamScannerInitialBuffer), openAIStreamScannerMaxBuffer)

	var dataLines []string
	dispatch := func() error {
		if len(dataLines) == 0 {
			return nil
		}
		data := strings.Join(dataLines, "\n")
		dataLines = dataLines[:0]
		return onData(data)
	}

	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			if err := dispatch(); err != nil {
				return err
			}
		case strings.HasPrefix(line, "data:"):
			data := strings.TrimPrefix(line, "data:")
			data = strings.TrimPrefix(data, " ")
			dataLines = append(dataLines, data)
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("reading SSE stream: %w", err)
	}
	return dispatch()
}

func zeroResponsesUsage() map[string]interface{} {
	return map[string]interface{}{
		"input_tokens":          0,
		"input_tokens_details":  nil,
		"output_tokens":         0,
		"output_tokens_details": nil,
		"total_tokens":          0,
	}
}

func extractResponsesWebSocketError(status int, body []byte) (string, string) {
	var envelope struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil && strings.TrimSpace(envelope.Error.Message) != "" {
		return envelope.Error.Message, envelope.Error.Code
	}

	trimmed := strings.TrimSpace(string(body))
	if trimmed != "" {
		return trimmed, ""
	}

	return http.StatusText(status), ""
}

func responsesWebSocketStreamFailureDetails(event responsesWebSocketStreamEvent) (int, string, string) {
	switch event.Type {
	case "response.failed":
		if status, _, ok := classifyPrecommitResponsesFailure(event); ok {
			message := responsesPrecommitErrorMessage(event, status)
			return status, message, strings.TrimSpace(event.Response.Error.Code)
		}
		errType := strings.TrimSpace(event.Response.Error.Type)
		code := strings.TrimSpace(event.Response.Error.Code)
		message := strings.TrimSpace(event.Response.Error.Message)
		if message == "" {
			if code != "" {
				message = code
			} else {
				message = "upstream response.failed"
			}
		}
		return responsesWebSocketErrorStatus(errType), message, code
	case "response.incomplete":
		reason := strings.TrimSpace(event.Response.IncompleteDetails.Reason)
		if reason == "" {
			return http.StatusConflict, "upstream response.incomplete", "response_incomplete"
		}
		return http.StatusConflict, "upstream response.incomplete: " + reason, reason
	default:
		return 0, "", ""
	}
}

func responsesWebSocketErrorStatus(errType string) int {
	switch strings.TrimSpace(errType) {
	case "invalid_request_error":
		return http.StatusBadRequest
	case "authentication_error":
		return http.StatusUnauthorized
	case "permission_error":
		return http.StatusForbidden
	case "not_found_error":
		return http.StatusNotFound
	case "conflict_error":
		return http.StatusConflict
	case "rate_limit_error":
		return http.StatusTooManyRequests
	case "server_error":
		return http.StatusInternalServerError
	default:
		return http.StatusBadGateway
	}
}

func flattenResponsesWebSocketHeaders(headers http.Header) map[string]interface{} {
	if len(headers) == 0 {
		return nil
	}

	filtered := make(http.Header)
	copyPassthroughHeaders(filtered, headers)
	if len(filtered) == 0 {
		return nil
	}

	result := make(map[string]interface{}, len(filtered))
	for key, values := range filtered {
		switch len(values) {
		case 0:
		case 1:
			result[key] = values[0]
		default:
			result[key] = strings.Join(values, ", ")
		}
	}
	return result
}

// responsesWebSocketMetadataHeaders extracts headers that are meaningful to
// Codex CLI WebSocket clients via the codex.response.metadata frame. The CLI
// parses openai-model from metadata frames using case-insensitive comparison
// (eq_ignore_ascii_case in response_model()); we use lowercase keys to match
// the wire format the real OpenAI backend uses.
func responsesWebSocketMetadataHeaders(headers http.Header) map[string]interface{} {
	if len(headers) == 0 {
		return nil
	}

	result := make(map[string]interface{}, 2)
	// Go's Header.Get is case-insensitive, but we store the JSON key in
	// lowercase to match what the real OpenAI backend sends.
	if value := headers.Get("Openai-Model"); value != "" {
		result["openai-model"] = value
	}
	if value := headers.Get("X-Openai-Model"); value != "" {
		result["x-openai-model"] = value
	}

	if len(result) == 0 {
		return nil
	}
	return result
}

func responsesWebSocketHistoryExceedsThreshold(items []json.RawMessage, cfg ResponsesWebSocketConfig) bool {
	return responsesWebSocketHistoryExceedsThresholdWithBytes(items, 0, cfg)
}

func responsesWebSocketHistoryExceedsThresholdWithBytes(items []json.RawMessage, historyBytes int, cfg ResponsesWebSocketConfig) bool {
	if len(items) <= 1 {
		return false
	}
	if cfg.AutoCompactMaxItems > 0 && len(items) > cfg.AutoCompactMaxItems && responsesWebSocketHistoryCanReduceItemCount(items, cfg.AutoCompactKeepTail) {
		return true
	}
	if cfg.AutoCompactMaxBytes > 0 {
		if historyBytes <= 0 {
			historyBytes = rawMessagesSize(items)
		}
		if historyBytes > cfg.AutoCompactMaxBytes {
			return true
		}
	}
	return false
}

func (s *responsesWebSocketSession) currentHistoryBytes() int {
	if len(s.historyItems) == 0 {
		return 0
	}
	if s.historyBytes <= 0 {
		return rawMessagesSize(s.historyItems)
	}
	return s.historyBytes
}

func responsesWebSocketHistoryCanReduceItemCount(items []json.RawMessage, keepTail int) bool {
	return compactedResponsesAlignedPrefixLen(items, keepTail) > 1
}

func rawMessagesSize(items []json.RawMessage) int {
	return rawMessageSegmentsSize(items)
}

func rawMessageSegmentsSize(segments ...[]json.RawMessage) int {
	size := 0
	for _, segment := range segments {
		for _, item := range segment {
			size += len(item) + 1
		}
	}
	return size
}

func writeRawMessageSegments(buf *bytes.Buffer, segments ...[]json.RawMessage) error {
	first := true
	for _, segment := range segments {
		for _, item := range segment {
			if len(item) == 0 {
				return fmt.Errorf("empty input item")
			}
			if !first {
				buf.WriteByte(',')
			}
			first = false
			buf.Write(item)
		}
	}
	return nil
}

func cloneRawMessages(items []json.RawMessage) []json.RawMessage {
	if len(items) == 0 {
		return nil
	}

	cloned := make([]json.RawMessage, len(items))
	for idx, item := range items {
		cloned[idx] = cloneRawMessage(item)
	}
	return cloned
}

func cloneRawMessage(item json.RawMessage) json.RawMessage {
	if len(item) == 0 {
		return nil
	}
	return append(json.RawMessage(nil), item...)
}
