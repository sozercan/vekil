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

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/sozercan/vekil/logger"
)

const responsesWebSocketRequestHeaderPrefix = "ws_request_header_"

// errStreamFailedUpstream is a sentinel error indicating the upstream stream
// ended with response.failed or response.incomplete after forwarding the
// upstream failure event. This path also emits the standard websocket error
// payload so clients can surface the upstream error details.
var errStreamFailedUpstream = errors.New("upstream stream ended with response.failed or response.incomplete")

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
		Usage             *responsesTokenUsage                      `json:"usage,omitempty"`
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
	turnState      string
	lastResponseID string
	lastSignature  string
	historyItems   []json.RawMessage
}

type responsesWebSocketRequestPlan struct {
	signature          string
	resetHistory       bool
	currentInput       []json.RawMessage
	fullReplaySegments [][]json.RawMessage
	useTurnStateDelta  bool
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

// HandleResponsesWebSocket handles GET /v1/responses websocket upgrades used
// by Codex. Each websocket request is translated into a normal upstream
// streaming /responses HTTP request and the SSE data payloads are forwarded back
// as websocket text frames.
func (h *ProxyHandler) HandleResponsesWebSocket(w http.ResponseWriter, r *http.Request) {
	if !websocket.IsWebSocketUpgrade(r) {
		w.Header().Set("Connection", "Upgrade")
		w.Header().Set("Upgrade", "websocket")
		http.Error(w, http.StatusText(http.StatusUpgradeRequired), http.StatusUpgradeRequired)
		return
	}

	conn, err := responsesWebSocketUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()

	conn.SetReadLimit(maxRequestBodySize)
	session := newResponsesWebSocketSession(conn, r)

	for {
		messageType, payload, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if messageType != websocket.TextMessage {
			session.sendWrappedError(http.StatusBadRequest, "responses websocket only accepts text frames", "invalid_request_error", nil)
			return
		}

		request, err := parseResponsesWebSocketCreateRequest(payload)
		if err != nil {
			session.sendWrappedError(http.StatusBadRequest, err.Error(), "invalid_request_error", nil)
			return
		}

		if err := session.handleCreateRequest(h, request); err != nil {
			h.log.Debug("responses websocket request failed", logger.Err(err))
			return
		}
	}
}

func newResponsesWebSocketSession(conn *websocket.Conn, r *http.Request) *responsesWebSocketSession {
	baseHeaders := make(http.Header)
	for _, name := range []string{"X-Codex-Beta-Features", "X-Codex-Turn-Metadata", "OpenAI-Beta", "session_id", "X-Client-Request-Id", "X-OpenAI-Subagent"} {
		for _, value := range r.Header.Values(name) {
			baseHeaders.Add(name, value)
		}
	}

	return &responsesWebSocketSession{
		conn:        conn,
		ctx:         r.Context(),
		baseHeaders: baseHeaders,
		turnState:   strings.TrimSpace(r.Header.Get("X-Codex-Turn-State")),
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
		case "type", "input", "previous_response_id", "generate", "client_metadata":
		default:
			signatureBody[key] = value
		}

		switch key {
		case "type", "input", "previous_response_id", "generate", "client_metadata", "stream":
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
	plan, err := s.planRequest(h, request)
	if err != nil {
		s.sendWrappedError(http.StatusBadRequest, err.Error(), "invalid_request_error", nil)
		return err
	}
	metrics := responsesWebSocketRequestMetrics{}

	if request.Generate != nil && !*request.Generate {
		responseID := "vekil-ws-" + uuid.NewString()
		s.rememberResponse(plan.resetHistory, responseID, plan.signature, plan.currentInput, nil)
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
	defer upstreamCancel()

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
		return fmt.Errorf("upstream websocket bridge status %d", resp.StatusCode)
	}

	responseID, outputItems, err := s.streamUpstreamResponse(resp.Body, resp.Header)
	if err != nil {
		if errors.Is(err, errStreamFailedUpstream) {
			return nil
		}
		s.sendWrappedError(http.StatusBadGateway, err.Error(), "server_error", nil)
		return err
	}

	s.rememberResponse(plan.resetHistory, responseID, plan.signature, plan.currentInput, outputItems)
	metrics = s.maybeAutoCompactHistory(h, request, metrics)
	s.logRequestMetrics(h, request, responseID, metrics)
	return nil
}

func (s *responsesWebSocketSession) planRequest(h *ProxyHandler, request *responsesWebSocketCreateRequest) (responsesWebSocketRequestPlan, error) {
	plan := responsesWebSocketRequestPlan{
		signature:    request.signature(),
		currentInput: request.Input,
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
	plan.useTurnStateDelta = cfg.TurnStateDelta && s.turnState != ""
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
	bodyBytes = h.rewriteResponsesRequestBody(bodyBytes, "responses/websocket", true)
	return h.postResponsesWithHeaders(ctx, bodyBytes, s.requestHeaders(request, includeTurnState))
}

func (s *responsesWebSocketSession) requestHeaders(request *responsesWebSocketCreateRequest, includeTurnState bool) http.Header {
	headers := make(http.Header)
	mergeHeaderValues(headers, s.baseHeaders)

	for key, value := range request.ClientMetadata {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}

		switch {
		case strings.EqualFold(key, "x-codex-turn-metadata"):
			headers.Set("X-Codex-Turn-Metadata", trimmed)
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
	}
	s.historyItems = append(s.historyItems, currentInput...)
	s.historyItems = append(s.historyItems, outputItems...)
}

func (s *responsesWebSocketSession) maybeAutoCompactHistory(h *ProxyHandler, request *responsesWebSocketCreateRequest, metrics responsesWebSocketRequestMetrics) responsesWebSocketRequestMetrics {
	ctx, cancel := h.newInferenceUpstreamContext(false)
	defer cancel()

	compaction, compacted, err := s.compactHistory(h, ctx, request, false)
	if err != nil {
		h.log.Debug("responses websocket auto-compaction failed",
			logger.Err(err),
			logger.F("model", request.Model),
			logger.F("history_items", len(s.historyItems)),
			logger.F("history_bytes", rawMessagesSize(s.historyItems)),
		)
		return metrics
	}
	if !compacted {
		return metrics
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

	compaction, compacted, err := s.compactHistory(h, ctx, request, true)
	if err != nil {
		h.log.Debug("responses websocket 413 compaction failed",
			logger.Err(err),
			logger.F("model", request.Model),
			logger.F("previous_response_id", request.PreviousResponseID),
			logger.F("history_items", len(s.historyItems)),
			logger.F("history_bytes", rawMessagesSize(s.historyItems)),
		)
		return resp, nil
	}
	if !compacted {
		return resp, nil
	}

	h.log.Debug("responses websocket compacted oversized replay; retrying request",
		logger.F("model", request.Model),
		logger.F("previous_response_id", request.PreviousResponseID),
		logger.F("prior_items", compaction.fromItems),
		logger.F("prior_bytes", compaction.fromBytes),
		logger.F("new_items", compaction.toItems),
		logger.F("new_bytes", compaction.toBytes),
	)

	_ = resp.Body.Close()
	return s.postCreateRequestSegments(h, ctx, request, [][]json.RawMessage{s.historyItems, request.Input}, false)
}

func (s *responsesWebSocketSession) compactHistory(h *ProxyHandler, ctx context.Context, request *responsesWebSocketCreateRequest, force bool) (responsesWebSocketHistoryCompaction, bool, error) {
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
		if !responsesWebSocketHistoryExceedsThreshold(s.historyItems, cfg) {
			return result, false, nil
		}
	}
	if strings.TrimSpace(request.Model) == "" {
		return result, false, nil
	}

	prefixLen := len(s.historyItems) - cfg.AutoCompactKeepTail
	if prefixLen <= 0 {
		return result, false, nil
	}

	prefix := s.historyItems[:prefixLen]
	tail := s.historyItems[prefixLen:]
	result.fromItems = len(s.historyItems)
	result.fromBytes = rawMessagesSize(s.historyItems)

	summary, err := h.compactResponsesInput(ctx, request.Model, prefix, s.requestHeaders(request, false))
	if err != nil {
		return result, false, err
	}

	checkpoint, err := proxyCompactionContextRawMessage(summary)
	if err != nil {
		return result, false, err
	}

	compacted := make([]json.RawMessage, 0, 1+len(tail))
	compacted = append(compacted, checkpoint)
	compacted = append(compacted, tail...)

	result.toItems = len(compacted)
	result.toBytes = rawMessagesSize(compacted)
	s.historyItems = compacted
	return result, true, nil
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
		logger.F("history_bytes", rawMessagesSize(s.historyItems)),
		logger.F("compacted_from_items", metrics.compactedFromItems),
		logger.F("compacted_from_bytes", metrics.compactedFromBytes),
		logger.F("compacted_to_items", metrics.compactedToItems),
		logger.F("compacted_to_bytes", metrics.compactedToBytes),
	)
}

func (s *responsesWebSocketSession) streamUpstreamResponse(body io.Reader, headers http.Header) (string, []json.RawMessage, error) {
	// Emit a synthetic metadata event so WebSocket clients can discover the
	// actual model used. The Codex CLI parses openai-model from
	// codex.response.metadata frames via response_model().
	if mappedHeaders := responsesWebSocketMetadataHeaders(headers); len(mappedHeaders) > 0 {
		_ = s.writeJSON(map[string]interface{}{
			"type":    "codex.response.metadata",
			"headers": mappedHeaders,
		})
	}

	var responseID string
	var outputItems []json.RawMessage
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
					return errStreamFailedUpstream
				}
			}
		}

		if err := s.conn.WriteMessage(websocket.TextMessage, []byte(data)); err != nil {
			return err
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
				outputItems = append(outputItems, cloneRawMessage(event.Item))
			}
		case "response.completed":
			sawCompleted = true
			if event.Response.ID != "" {
				responseID = event.Response.ID
			}
		case "response.failed", "response.incomplete":
			s.sendUpstreamStreamFailure(event, headers)
			// Return the sentinel immediately to break out of the SSE
			// scanner loop. The failure event has already been forwarded to
			// the client above, and we also emit a standard error payload so
			// websocket clients can surface the upstream error details.
			return errStreamFailedUpstream
		}

		return nil
	}); err != nil && !errors.Is(err, errStreamFailedUpstream) {
		return "", nil, err
	} else if errors.Is(err, errStreamFailedUpstream) {
		return "", nil, errStreamFailedUpstream
	}

	if sawCompleted {
		if responseID == "" {
			return "", nil, fmt.Errorf("response.completed missing response id")
		}
		return responseID, outputItems, nil
	}
	return "", nil, fmt.Errorf("stream ended before response.completed")
}

func (s *responsesWebSocketSession) writeJSON(payload interface{}) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
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
	if len(items) <= cfg.AutoCompactKeepTail {
		return false
	}
	if cfg.AutoCompactMaxItems > 0 && len(items) > cfg.AutoCompactMaxItems {
		return true
	}
	if cfg.AutoCompactMaxBytes > 0 && rawMessagesSize(items) > cfg.AutoCompactMaxBytes {
		return true
	}
	return false
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
