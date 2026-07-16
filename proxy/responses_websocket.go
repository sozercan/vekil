package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
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

var responsesLifecycleOrder = struct {
	sync.Mutex
	sequence uint64
}{}

// publishResponsesLifecycleSequence assigns and publishes one globally ordered
// lifecycle event while holding the order lock. Keeping allocation and
// publication in the same critical section prevents a later client-close event
// from becoming visible before an earlier shutdown sequence is published.
func publishResponsesLifecycleSequence(publish func(uint64)) {
	responsesLifecycleOrder.Lock()
	defer responsesLifecycleOrder.Unlock()
	if responsesLifecycleOrder.sequence == ^uint64(0) {
		panic("responses lifecycle sequence exhausted")
	}
	responsesLifecycleOrder.sequence++
	publish(responsesLifecycleOrder.sequence)
}

const responsesWebSocketOutstandingRequestLimit = 2

var (
	responsesWebSocketWriteWait               = 10 * time.Second
	responsesWebSocketPingPeriod              = 30 * time.Second
	responsesWebSocketPongWait                = 60 * time.Second
	responsesWebSocketShutdownCloseWait       = 100 * time.Millisecond
	responsesWebSocketTerminalObservationWait = 500 * time.Millisecond
)

var errResponsesWebSocketClientWrite = errors.New("responses websocket client write failed")
var errResponsesWebSocketStreamTerminal = errors.New("responses websocket upstream stream reached terminal event")

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
// ended with response.failed, response.incomplete, or a top-level error event
// after forwarding the upstream failure event. This path also emits the standard
// websocket error payload so clients can surface the upstream error details.
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
	Type     string                     `json:"type"`
	Code     string                     `json:"code,omitempty"`
	Message  string                     `json:"message,omitempty"`
	Param    string                     `json:"param,omitempty"`
	Headers  map[string]json.RawMessage `json:"headers,omitempty"`
	Response struct {
		ID                string                                    `json:"id"`
		Error             responsesWebSocketStreamError             `json:"error"`
		IncompleteDetails responsesWebSocketStreamIncompleteDetails `json:"incomplete_details"`
		Usage             responsesUsage                            `json:"usage"`
	} `json:"response,omitempty"`
	Error responsesWebSocketStreamError `json:"error,omitempty"`
	Item  json.RawMessage               `json:"item,omitempty"`
}

type responsesWebSocketStreamError struct {
	Type    string                     `json:"type"`
	Code    string                     `json:"code"`
	Message string                     `json:"message"`
	Param   string                     `json:"param,omitempty"`
	Headers map[string]json.RawMessage `json:"headers,omitempty"`
}

type responsesWebSocketStreamIncompleteDetails struct {
	Reason string `json:"reason"`
}

type responsesWebSocketFrame struct {
	messageType int
	payload     []byte
}

type responsesWebSocketShutdownConn interface {
	WriteControl(messageType int, data []byte, deadline time.Time) error
	Close() error
}

type responsesWebSocketCloseBarrier struct {
	done chan struct{}
	once sync.Once
}

type responsesWebSocketCloseCause uint8

const (
	responsesWebSocketCloseCauseUnknown responsesWebSocketCloseCause = iota
	responsesWebSocketCloseCauseClient
	responsesWebSocketCloseCauseServer
)

func newResponsesWebSocketCloseBarrier() *responsesWebSocketCloseBarrier {
	return &responsesWebSocketCloseBarrier{done: make(chan struct{})}
}

func (b *responsesWebSocketCloseBarrier) release() {
	if b == nil {
		return
	}
	b.once.Do(func() { close(b.done) })
}

func (b *responsesWebSocketCloseBarrier) released() bool {
	if b == nil {
		return true
	}
	select {
	case <-b.done:
		return true
	default:
		return false
	}
}

type responsesWebSocketSession struct {
	conn                    *websocket.Conn
	shutdownConn            responsesWebSocketShutdownConn
	ctx                     context.Context
	cancel                  context.CancelFunc
	baseHeaders             http.Header
	userAgent               string
	turnState               string
	turnMetadata            string
	lastResponseID          string
	lastSignature           string
	historyItems            []json.RawMessage
	historyBytes            int
	toolContexts            *ToolExecutionContextStore
	toolScope               string
	handlerDone             chan struct{}
	handlerDoneOnce         sync.Once
	writeWait               time.Duration
	pingPeriod              time.Duration
	pongWait                time.Duration
	terminalObservationWait time.Duration
	inflightMu              sync.Mutex
	inflightCancel          context.CancelFunc
	inflightGen             uint64
	inflightCancelHolds     int
	closing                 bool
	closeCause              responsesWebSocketCloseCause
	closeSequence           uint64
	closeBarrier            *responsesWebSocketCloseBarrier
	socketClosed            bool
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
	internalUsage      responsesUsage
	providerID         string
	providerKind       string
}

func (m *responsesWebSocketRequestMetrics) captureObservedProvider(observer *providerRouteObserver) {
	if m == nil {
		return
	}
	if route, ok := observer.snapshot(); ok {
		m.providerID = route.id
		m.providerKind = route.kind
	}
}

func (m *responsesWebSocketRequestMetrics) captureProvider(resp *http.Response) {
	if m == nil {
		return
	}
	if route, ok := providerRouteFromResponse(resp); ok {
		m.providerID = route.id
		m.providerKind = route.kind
	}
}

func (m *responsesWebSocketRequestMetrics) addInternalUsage(usage responsesUsage) {
	if m == nil {
		return
	}
	m.internalUsage.add(usage)
}

func (m responsesWebSocketRequestMetrics) totalUsage(turnUsage responsesUsage) responsesUsage {
	turnUsage.add(m.internalUsage)
	return turnUsage
}

type responsesWebSocketStreamResult struct {
	responseID  string
	outputItems []json.RawMessage
	usage       responsesUsage
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

	conn.SetReadLimit(maxRequestBodySize)
	session := newResponsesWebSocketSession(conn, r)
	registered := false
	defer func() {
		session.beginClosing()
		session.hardClose()
		if registered {
			h.unregisterResponsesWebSocketSession(session)
		}
		session.closeHandlerDone()
	}()

	if !h.registerResponsesWebSocketSession(session) {
		session.sendGoingAwayWithDeadline(time.Now().Add(session.effectiveWriteWait()))
		return
	}
	registered = true

	if err := session.configureReadDeadline(); err != nil {
		session.beginClosing()
		return
	}

	frames := make(chan responsesWebSocketFrame, responsesWebSocketOutstandingRequestLimit)
	outstanding := make(chan struct{}, responsesWebSocketOutstandingRequestLimit)
	session.startPingLoop()
	go session.readPump(frames, outstanding)

	for {
		select {
		case <-session.ctx.Done():
			return
		case frame, ok := <-frames:
			if !ok || session.isClosing() {
				return
			}
			err := session.handleFrame(h, frame)
			<-outstanding
			if err != nil {
				h.log.Debug("responses websocket request failed", logger.Err(err))
				if isResponsesWebSocketClientDisconnect(session.ctx, err) || session.isClosing() {
					return
				}
			}
		}
	}
}

func (s *responsesWebSocketSession) handleFrame(h *ProxyHandler, frame responsesWebSocketFrame) error {
	if s == nil || s.isClosing() {
		return context.Canceled
	}
	if frame.messageType != websocket.TextMessage {
		return s.sendWrappedError(http.StatusBadRequest, "responses websocket only accepts text frames", "invalid_request_error", nil)
	}

	frameType, err := parseResponsesWebSocketFrameType(frame.payload)
	if err != nil {
		return s.sendWrappedError(http.StatusBadRequest, err.Error(), "invalid_request_error", nil)
	}
	if frameType == "response.processed" {
		return nil
	}

	request, err := parseResponsesWebSocketCreateRequest(frame.payload)
	if err != nil {
		return s.sendWrappedError(http.StatusBadRequest, err.Error(), "invalid_request_error", nil)
	}
	return s.handleCreateRequest(h, request)
}

func (s *responsesWebSocketSession) readPump(frames chan<- responsesWebSocketFrame, outstanding chan struct{}) {
	defer close(frames)
	for {
		messageType, payload, err := s.conn.ReadMessage()
		if err != nil {
			s.beginClosing()
			s.hardClose()
			return
		}

		// response.processed is an advisory acknowledgement and does not consume
		// the single queued-turn slot while inference is active.
		if messageType == websocket.TextMessage {
			if frameType, parseErr := parseResponsesWebSocketFrameType(payload); parseErr == nil && frameType == "response.processed" {
				continue
			}
		}

		select {
		case outstanding <- struct{}{}:
		default:
			// The two outstanding slots are exactly one active request plus one
			// queued request. They are held until serialized processing finishes,
			// so acceptance is independent of handler scheduling.
			s.closeWithControl(websocket.ClosePolicyViolation, "only one responses request may be queued", time.Now().Add(s.effectiveWriteWait()))
			return
		}

		frame := responsesWebSocketFrame{messageType: messageType, payload: payload}
		select {
		case frames <- frame:
		case <-s.ctx.Done():
			<-outstanding
			return
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

func (h *ProxyHandler) ShutdownWebSocketSessions(ctx context.Context) error {
	if h == nil {
		return nil
	}
	h.responsesWSSessionsMu.Lock()
	h.responsesWSDraining = true
	sessions := make([]*responsesWebSocketSession, 0, len(h.responsesWSSessions))
	for session := range h.responsesWSSessions {
		sessions = append(sessions, session)
	}
	h.responsesWSSessionsMu.Unlock()

	// Phase one marks every session closing before any handler can start new
	// turn or compaction work. A per-session barrier prevents handler teardown,
	// read failures, or ping failures from hard-closing the socket before its
	// graceful close attempt gets a bounded opportunity to run.
	closeDeadline := responsesWebSocketShutdownCloseDeadline(ctx)
	attempts := make([]<-chan struct{}, 0, len(sessions))
	barriers := make([]*responsesWebSocketCloseBarrier, 0, len(sessions))
	for _, session := range sessions {
		session.markServerShutdown()
		barrier, owner := session.startControlClose(responsesWebSocketCloseCauseServer)
		if barrier == nil {
			continue
		}
		barriers = append(barriers, barrier)
		if !owner {
			attempts = append(attempts, barrier.done)
			continue
		}
		attemptDone := make(chan struct{})
		attempts = append(attempts, attemptDone)
		go func() {
			defer close(attemptDone)
			session.writeCloseControl(websocket.CloseGoingAway, "server shutting down", closeDeadline)
		}()
	}
	waitForResponsesWebSocketCloseAttempts(ctx, closeDeadline, attempts)

	// Phase two is unconditional, including when ctx was already canceled: lift
	// every close barrier, cancel active inference/compaction and the session
	// context, then hard-close every hijacked socket.
	for _, barrier := range barriers {
		barrier.release()
	}
	for _, session := range sessions {
		session.beginClosing()
	}
	for _, session := range sessions {
		session.hardClose()
	}

	// Phase three waits only for real handler teardown. handlerDone is closed
	// after unregistration, never merely because shutdown requested a close.
	waitDeadline := responsesWebSocketShutdownWaitDeadline(ctx)
	for _, session := range sessions {
		if !session.waitForHandlerDone(ctx, waitDeadline) {
			if ctx != nil && ctx.Err() != nil {
				return ctx.Err()
			}
			return context.DeadlineExceeded
		}
	}
	return nil
}

func waitForResponsesWebSocketCloseAttempts(ctx context.Context, deadline time.Time, attempts []<-chan struct{}) {
	if len(attempts) == 0 {
		return
	}
	allDone := make(chan struct{})
	go func() {
		defer close(allDone)
		for _, done := range attempts {
			<-done
		}
	}()

	remaining := time.Until(deadline)
	if remaining <= 0 {
		return
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	var ctxDone <-chan struct{}
	if ctx != nil {
		ctxDone = ctx.Done()
	}
	select {
	case <-allDone:
	case <-ctxDone:
	case <-timer.C:
	}
}

func (s *responsesWebSocketSession) closeHandlerDone() {
	if s == nil || s.handlerDone == nil {
		return
	}
	s.handlerDoneOnce.Do(func() { close(s.handlerDone) })
}

func (s *responsesWebSocketSession) configureReadDeadline() error {
	if s == nil || s.conn == nil || s.pongWait <= 0 {
		return nil
	}
	if err := s.conn.SetReadDeadline(time.Now().Add(s.pongWait)); err != nil {
		return err
	}
	s.conn.SetPongHandler(func(string) error {
		return s.conn.SetReadDeadline(time.Now().Add(s.pongWait))
	})
	return nil
}

func (s *responsesWebSocketSession) startPingLoop() {
	if s == nil || s.conn == nil || s.pingPeriod <= 0 {
		return
	}
	ticker := time.NewTicker(s.pingPeriod)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				deadline := time.Now().Add(s.effectiveWriteWait())
				if err := s.conn.WriteControl(websocket.PingMessage, nil, deadline); err != nil {
					s.beginClosing()
					s.hardClose()
					return
				}
			case <-s.ctx.Done():
				return
			case <-s.handlerDone:
				return
			}
		}
	}()
}

func responsesWebSocketShutdownCloseDeadline(ctx context.Context) time.Time {
	wait := responsesWebSocketShutdownCloseWait
	if wait <= 0 || wait > responsesWebSocketWriteWait {
		wait = responsesWebSocketWriteWait
	}
	deadline := time.Now().Add(wait)
	if ctx != nil {
		if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
			return ctxDeadline
		}
		if ctx.Err() != nil {
			return time.Now()
		}
	}
	return deadline
}

func responsesWebSocketShutdownWaitDeadline(ctx context.Context) time.Time {
	deadline := time.Now().Add(responsesWebSocketWriteWait)
	if ctx != nil {
		if ctx.Err() != nil {
			return time.Now()
		}
		if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
			return ctxDeadline
		}
	}
	return deadline
}

func (s *responsesWebSocketSession) waitForHandlerDone(ctx context.Context, deadline time.Time) bool {
	if s == nil || s.handlerDone == nil {
		return true
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		select {
		case <-s.handlerDone:
			return true
		default:
			return false
		}
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	var ctxDone <-chan struct{}
	if ctx != nil {
		ctxDone = ctx.Done()
	}
	select {
	case <-s.handlerDone:
		return true
	case <-ctxDone:
		return false
	case <-timer.C:
		return false
	}
}

func (s *responsesWebSocketSession) effectiveTerminalObservationWait() time.Duration {
	if s != nil && s.terminalObservationWait > 0 {
		return s.terminalObservationWait
	}
	return responsesWebSocketTerminalObservationWait
}

func (s *responsesWebSocketSession) effectiveWriteWait() time.Duration {
	if s != nil && s.writeWait > 0 {
		return s.writeWait
	}
	return responsesWebSocketWriteWait
}

func (s *responsesWebSocketSession) writeCloseControl(code int, message string, deadline time.Time) {
	if s == nil {
		return
	}
	conn := s.shutdownConn
	if conn == nil {
		conn = s.conn
	}
	if conn == nil {
		return
	}
	if deadline.IsZero() {
		deadline = time.Now().Add(s.effectiveWriteWait())
	}
	_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, message), deadline)
}

func (s *responsesWebSocketSession) closeWithControl(code int, message string, deadline time.Time) {
	if s == nil {
		return
	}
	barrier, owner := s.startControlClose()
	if barrier == nil {
		s.beginClosing()
		s.hardClose()
		return
	}
	if owner {
		s.writeCloseControl(code, message, deadline)
		barrier.release()
	} else {
		<-barrier.done
	}
	s.beginClosing()
	s.hardClose()
}

func (s *responsesWebSocketSession) sendGoingAwayWithDeadline(deadline time.Time) {
	s.closeWithControl(websocket.CloseGoingAway, "server shutting down", deadline)
}

func (s *responsesWebSocketSession) hardClose() {
	if s == nil {
		return
	}
	for {
		s.inflightMu.Lock()
		if s.socketClosed {
			s.inflightMu.Unlock()
			return
		}
		barrier := s.closeBarrier
		if barrier != nil && !barrier.released() {
			done := barrier.done
			s.inflightMu.Unlock()
			<-done
			continue
		}
		s.socketClosed = true
		conn := s.shutdownConn
		if conn == nil {
			conn = s.conn
		}
		s.inflightMu.Unlock()
		if conn != nil {
			_ = conn.Close()
		}
		return
	}
}

func (s *responsesWebSocketSession) startControlClose(causes ...responsesWebSocketCloseCause) (*responsesWebSocketCloseBarrier, bool) {
	if s == nil {
		return nil, false
	}
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()
	s.closing = true
	cause := responsesWebSocketCloseCauseClient
	if len(causes) > 0 {
		cause = causes[0]
	}
	s.markCloseCauseLocked(cause)
	if s.socketClosed {
		return nil, false
	}
	if s.closeBarrier != nil {
		return s.closeBarrier, false
	}
	barrier := newResponsesWebSocketCloseBarrier()
	s.closeBarrier = barrier
	return barrier, true
}

func (s *responsesWebSocketSession) isClosing() bool {
	if s == nil {
		return true
	}
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()
	return s.closing
}

func (s *responsesWebSocketSession) beginClosing() {
	if s == nil {
		return
	}
	s.inflightMu.Lock()
	s.closing = true
	s.markCloseCauseLocked(responsesWebSocketCloseCauseClient)
	if s.closeBarrier != nil && !s.closeBarrier.released() {
		s.inflightMu.Unlock()
		return
	}
	cancelSession := s.cancel
	cancelInflight := s.inflightCancel
	if s.inflightCancelHolds > 0 {
		cancelInflight = nil
	}
	s.inflightMu.Unlock()
	if cancelSession != nil {
		cancelSession()
	}
	if cancelInflight != nil {
		cancelInflight()
	}
}

func (s *responsesWebSocketSession) markServerShutdown() {
	if s == nil {
		return
	}
	s.inflightMu.Lock()
	s.markCloseCauseLocked(responsesWebSocketCloseCauseServer)
	s.inflightMu.Unlock()
}

func (s *responsesWebSocketSession) markCloseCauseLocked(cause responsesWebSocketCloseCause) {
	if s.closeCause != responsesWebSocketCloseCauseUnknown {
		return
	}
	s.closeCause = cause
	publishResponsesLifecycleSequence(func(sequence uint64) {
		s.closeSequence = sequence
	})
}

func (s *responsesWebSocketSession) clientClosePrecedesShutdown(h *ProxyHandler) bool {
	if s == nil {
		return false
	}
	s.inflightMu.Lock()
	cause := s.closeCause
	closeSequence := s.closeSequence
	s.inflightMu.Unlock()
	if cause != responsesWebSocketCloseCauseClient {
		return false
	}
	shutdownSequence := uint64(0)
	if h != nil {
		shutdownSequence = h.shutdownSequence.Load()
	}
	return shutdownSequence == 0 || (closeSequence != 0 && closeSequence < shutdownSequence)
}

// setInflightCancel records the cancel func for the current in-flight upstream
// call and returns a generation token. The token must be passed to
// clearInflightCancel so that only the matching cancel is cleared: nested or
// subsequent in-flight calls (e.g. auto-compaction inside a create request)
// bump the generation, and a stale clear is then a no-op rather than nilling
// out a newer cancel. (context.CancelFunc values are not comparable, so the
// generation token stands in for identity.) Work installed after closing starts
// is canceled immediately and is never published as active.
func (s *responsesWebSocketSession) setInflightCancel(cancel context.CancelFunc) uint64 {
	if s == nil || cancel == nil {
		return 0
	}
	s.inflightMu.Lock()
	s.inflightGen++
	gen := s.inflightGen
	closing := s.closing || (s.ctx != nil && s.ctx.Err() != nil)
	if closing {
		s.closing = true
	} else {
		s.inflightCancel = cancel
	}
	s.inflightMu.Unlock()
	if closing {
		cancel()
	}
	return gen
}

// beginClosingForStreamWriteFailure marks a client-side socket failure while
// holding the active upstream cancellation just long enough for the prepared
// stream observer to drain an already-produced terminal event. Ordinary client
// close, pong timeout, and policy-close paths still cancel stalled upstream work
// immediately.
func (s *responsesWebSocketSession) beginClosingForStreamWriteFailure() {
	if s == nil {
		return
	}
	s.inflightMu.Lock()
	s.closing = true
	s.markCloseCauseLocked(responsesWebSocketCloseCauseClient)
	if s.closeCause == responsesWebSocketCloseCauseClient && s.inflightCancel != nil && s.inflightCancelHolds == 0 {
		s.inflightCancelHolds = 1
	}
	if s.closeBarrier != nil && !s.closeBarrier.released() {
		s.inflightMu.Unlock()
		return
	}
	cancelSession := s.cancel
	cancelInflight := s.inflightCancel
	if s.inflightCancelHolds > 0 {
		cancelInflight = nil
	}
	s.inflightMu.Unlock()
	if cancelSession != nil {
		cancelSession()
	}
	if cancelInflight != nil {
		cancelInflight()
	}
}

func (s *responsesWebSocketSession) releaseInflightCancellationHold(gen uint64) {
	if s == nil || gen == 0 {
		return
	}
	s.inflightMu.Lock()
	if s.inflightGen == gen && s.inflightCancelHolds > 0 {
		s.inflightCancelHolds--
	}
	shouldCancel := s.closing && s.inflightCancelHolds == 0 && s.inflightGen == gen &&
		(s.closeBarrier == nil || s.closeBarrier.released())
	cancelInflight := s.inflightCancel
	s.inflightMu.Unlock()
	if shouldCancel && cancelInflight != nil {
		cancelInflight()
	}
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
	ctx, cancel := context.WithCancel(r.Context())
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
		conn:         conn,
		shutdownConn: conn,
		ctx:          ctx,
		cancel:       cancel,
		baseHeaders:  baseHeaders,
		userAgent:    r.Header.Get("User-Agent"),
		// Codex treats X-Codex-Turn-State as server-issued, turn-scoped
		// sticky-routing state. This bridge only trusts state it received from
		// upstream during this proxy-owned websocket session.
		toolContexts: NewToolExecutionContextStore(),
		toolScope:    "responses-ws:" + uuid.NewString(),
		handlerDone:  make(chan struct{}),
		writeWait:    responsesWebSocketWriteWait,
		pingPeriod:   responsesWebSocketPingPeriod,
		pongWait:     responsesWebSocketPongWait,
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
	if s == nil || s.isClosing() || (s.ctx != nil && s.ctx.Err() != nil) || h.upstreamShutdownStarted() {
		return context.Canceled
	}
	s.syncTurnMetadata(request)
	if request.PreviousResponseID == "" {
		s.turnState = ""
	}

	plan, err := s.planRequest(h, request)
	if err != nil {
		if writeErr := s.sendWrappedError(http.StatusBadRequest, err.Error(), "invalid_request_error", nil); writeErr != nil {
			return writeErr
		}
		return err
	}
	metrics := responsesWebSocketRequestMetrics{}
	turnRecorded := false
	var turnStatsRecord responsesTurnStatsRecord
	recordTurn := func(status int, usage responsesUsage) {
		if turnRecorded {
			return
		}
		turnRecorded = true
		turnStatsRecord = s.recordTurnStats(h, request.Model, metrics.providerID, metrics.providerKind, status, metrics.totalUsage(usage))
	}
	var peek *peekResult
	var resp *http.Response
	observedTerminalOutcome := func() (int, responsesUsage, bool) {
		terminalPeek := peek
		if (terminalPeek == nil || terminalPeek.terminal == nil) && resp != nil {
			if preparedBody, ok := resp.Body.(*responsesPreparedBody); ok {
				if terminal, hasTerminal := preparedBody.terminalResultWithin(s.effectiveTerminalObservationWait()); hasTerminal {
					terminalPeek = &terminal
				}
			}
		}
		if terminalPeek != nil && terminalPeek.terminal != nil {
			usage := terminalPeek.terminal.Response.Usage
			switch terminalPeek.terminal.Type {
			case "response.completed":
				if strings.TrimSpace(terminalPeek.terminal.Response.ID) == "" {
					return http.StatusBadGateway, usage, true
				}
				return http.StatusOK, usage, true
			case "response.failed", "response.incomplete", "error":
				if terminalPeek.status != 0 {
					return terminalPeek.status, usage, true
				}
				var headers http.Header
				if resp != nil {
					headers = resp.Header
				}
				status, _, _ := responsesWebSocketStreamFailureDetails(*terminalPeek.terminal, headers)
				return status, usage, true
			}
		}
		return 0, responsesUsage{}, false
	}
	observedRecoveredUsage := func() responsesUsage {
		if resp != nil {
			if preparedBody, ok := resp.Body.(*responsesPreparedBody); ok {
				if usage, hasUsage := preparedBody.recoveredUsage(); hasUsage {
					return usage
				}
			}
		}
		return responsesUsage{}
	}
	recordDisconnected := func(status int, usage responsesUsage, allowFallback bool) {
		if status == 0 {
			if terminalStatus, terminalUsage, ok := observedTerminalOutcome(); ok {
				status, usage = terminalStatus, terminalUsage
			}
		}
		if usage.isZero() {
			usage = observedRecoveredUsage()
		}
		if status == 0 {
			if !allowFallback {
				return
			}
			status = 499 // client closed request before a terminal provider outcome
		}
		recordTurn(status, usage)
	}

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
	upstreamCtx, routeObserver := withProviderRouteObserver(upstreamCtx)
	inflightGen := s.setInflightCancel(upstreamCancel)
	var finishUpstreamOnce sync.Once
	finishUpstream := func() {
		finishUpstreamOnce.Do(func() {
			s.releaseInflightCancellationHold(inflightGen)
			s.clearInflightCancel(inflightGen)
			upstreamCancel()
		})
	}
	defer finishUpstream()

	var lifecycleBody *lifecycleAwareReadCloser
	var preparedPeek *peekResult
	var translatedHeaders http.Header
	resp, preparedPeek, translatedHeaders, err = h.prepareResponsesStream(s.ctx, upstreamCtx, request.Model, func() (*http.Response, error) {
		attemptPlan, err := s.planRequest(h, request)
		if err != nil {
			return nil, err
		}
		attemptResp, attemptDeltaAttempted, attemptDeltaFallback, err := s.postCreateRequest(h, upstreamCtx, request, attemptPlan, &metrics)
		metrics.captureObservedProvider(routeObserver)
		metrics.captureProvider(attemptResp)
		metrics.deltaAttempted = metrics.deltaAttempted || attemptDeltaAttempted
		metrics.deltaFallback = metrics.deltaFallback || attemptDeltaFallback
		if err == nil && attemptResp != nil && attemptResp.Body != nil {
			lifecycleBody = newLifecycleAwareReadCloser(attemptResp.Body, upstreamCtx)
			attemptResp.Body = lifecycleBody
		}
		return attemptResp, err
	})
	peek = preparedPeek
	if err != nil {
		lifecycleCanceled := errors.Is(err, context.Canceled) && errors.Is(context.Cause(upstreamCtx), errProxyLifecycleShutdown)
		clientDisconnected := errors.Is(err, errResponsesWebSocketClientWrite) ||
			(s.ctx != nil && s.ctx.Err() != nil && errors.Is(err, context.Canceled))
		if lifecycleCanceled {
			if s.clientClosePrecedesShutdown(h) {
				recordDisconnected(0, responsesUsage{}, true)
			}
			return err
		}
		if clientDisconnected {
			recordDisconnected(0, responsesUsage{}, s.clientClosePrecedesShutdown(h))
			return err
		}
		status := upstreamStatusCode(err, http.StatusBadGateway)
		if status == http.StatusBadGateway && errors.Is(err, context.DeadlineExceeded) {
			status = http.StatusGatewayTimeout
		}
		recordTurn(status, responsesUsage{})
		if s.isClosing() || h.upstreamShutdownStarted() {
			return err
		}
		if writeErr := s.sendWrappedError(status, fmt.Sprintf("upstream request failed: %v", err), "server_error", nil); writeErr != nil {
			return writeErr
		}
		return err
	}
	if peek != nil && peek.decision == responsesPeekDecisionTranslate {
		code, param := "", ""
		if peek.failure != nil {
			streamErr := responsesStreamEventError(*peek.failure)
			code = strings.TrimSpace(streamErr.Code)
			param = strings.TrimSpace(streamErr.Param)
		}
		// Record the classified failure before delivering the client error frame;
		// a disconnected client must not erase dashboard/provider accounting.
		usage := responsesUsage{}
		if peek.failure != nil {
			usage = peek.failure.Response.Usage
		}
		recordTurn(peek.status, usage)
		if s.isClosing() || h.upstreamShutdownStarted() {
			return context.Canceled
		}
		if writeErr := s.sendWrappedErrorDetails(peek.status, peek.message, code, param, translatedHeaders); writeErr != nil {
			return writeErr
		}
		return nil
	}
	if resp == nil {
		if terminalStatus, terminalUsage, ok := observedTerminalOutcome(); ok {
			// A nil prepared response is reserved for inbound cancellation. Retain
			// the authoritative provider accounting, but never fabricate a lossy
			// terminal frame or commit replay state without the original stream.
			recordTurn(terminalStatus, terminalUsage)
			if s.isClosing() || h.upstreamShutdownStarted() {
				return context.Canceled
			}
			if writeErr := s.sendWrappedError(http.StatusBadGateway, "upstream stream canceled before terminal delivery", "server_error", translatedHeaders); writeErr != nil {
				return writeErr
			}
			return fmt.Errorf("upstream stream canceled before terminal delivery")
		}
		if s.isClosing() || h.upstreamShutdownStarted() {
			recordDisconnected(0, responsesUsage{}, s.clientClosePrecedesShutdown(h))
			return context.Canceled
		}
		if writeErr := s.sendWrappedError(http.StatusBadGateway, "upstream request failed", "server_error", nil); writeErr != nil {
			return writeErr
		}
		return fmt.Errorf("upstream websocket bridge returned no response")
	}
	bodyClosed := false
	defer func() {
		if !bodyClosed {
			_ = resp.Body.Close()
		}
	}()

	s.updateTurnState(resp.Header)

	if resp.StatusCode != http.StatusOK {
		respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if readErr != nil {
			respBody = nil
		}
		message, code := extractResponsesWebSocketError(resp.StatusCode, respBody)
		recordTurn(resp.StatusCode, responsesUsage{})
		if s.isClosing() || h.upstreamShutdownStarted() {
			return context.Canceled
		}
		if writeErr := s.sendWrappedError(resp.StatusCode, message, code, resp.Header); writeErr != nil {
			return writeErr
		}
		return fmt.Errorf("upstream websocket bridge status %d", resp.StatusCode)
	}

	streamResult, err := s.streamUpstreamResponse(h, resp.Body, resp.Header, recordTurn)
	if err != nil {
		if errors.Is(err, errResponsesWebSocketClientWrite) ||
			(s.ctx != nil && s.ctx.Err() != nil && errors.Is(err, context.Canceled)) {
			recordDisconnected(0, streamResult.usage, s.clientClosePrecedesShutdown(h))
			return err
		}
		if errors.Is(err, errStreamFailedUpstream) {
			// Parsed response.failed/incomplete events account themselves before
			// client delivery, so the outer handler must not record them again.
			return nil
		}
		if lifecycleBody != nil && lifecycleBody.canceledAtFailure() && errors.Is(context.Cause(upstreamCtx), errProxyLifecycleShutdown) {
			return err
		}
		if terminalStatus, terminalUsage, ok := observedTerminalOutcome(); ok {
			recordTurn(terminalStatus, terminalUsage)
		} else {
			usage := streamResult.usage
			if usage.isZero() {
				usage = observedRecoveredUsage()
			}
			recordTurn(http.StatusBadGateway, usage)
		}
		if s.isClosing() || h.upstreamShutdownStarted() {
			return err
		}
		if writeErr := s.sendWrappedError(http.StatusBadGateway, err.Error(), "server_error", nil); writeErr != nil {
			return writeErr
		}
		return err
	}
	_ = resp.Body.Close()
	bodyClosed = true
	finishUpstream()

	s.rememberPlannedResponse(plan, streamResult.responseID, streamResult.outputItems)
	var autoCompactionUsage responsesUsage
	metrics, autoCompactionUsage = s.maybeAutoCompactHistory(h, request, metrics)
	h.AddResponsesTurnUsage(turnStatsRecord, autoCompactionUsage)

	// streamUpstreamResponse records a structurally valid completion before
	// client delivery. This is a fallback for implementations that return a
	// completed result without invoking the callback; the exactly-once guard keeps
	// it from incrementing request counts twice. Auto-compaction usage was amended
	// onto the existing record above rather than emitted as a synthetic request.
	recordTurn(http.StatusOK, streamResult.usage)
	s.logRequestMetrics(h, request, streamResult.responseID, metrics)
	return nil
}

// recordTurnStats records one websocket-bridge turn into traffic stats,
// resolving the provider for attribution. status is the turn outcome (200 for a
// completed turn, an error status for a failed one).
func (s *responsesWebSocketSession) recordTurnStats(h *ProxyHandler, model, providerID, providerKind string, status int, usage responsesUsage) responsesTurnStatsRecord {
	if h == nil {
		return responsesTurnStatsRecord{}
	}
	if providerID == "" {
		if provider, _, _ := h.resolveProviderModel(model, providerEndpointResponses); provider != nil {
			providerID, providerKind = provider.id, string(provider.kind)
		}
	}
	return h.RecordResponsesTurn(model, providerID, providerKind, classifyAgent(s.userAgent), status, usage)
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

func (s *responsesWebSocketSession) postCreateRequest(h *ProxyHandler, ctx context.Context, request *responsesWebSocketCreateRequest, plan responsesWebSocketRequestPlan, metrics *responsesWebSocketRequestMetrics) (*http.Response, bool, bool, error) {
	resp, err := s.postCreateRequestSegments(h, ctx, request, plan.upstreamSegments(), plan.useTurnStateDelta)
	if err != nil || resp == nil {
		return resp, plan.useTurnStateDelta, false, err
	}
	if !plan.useTurnStateDelta {
		resp, err = s.maybeRetryCompactedCreateRequest(h, ctx, request, resp, true, metrics)
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
	resp, err = s.maybeRetryCompactedCreateRequest(h, ctx, request, resp, true, metrics)
	return resp, true, true, err
}

func (s *responsesWebSocketSession) postCreateRequestSegments(h *ProxyHandler, ctx context.Context, request *responsesWebSocketCreateRequest, inputSegments [][]json.RawMessage, includeTurnState bool) (*http.Response, error) {
	bodyBytes, err := request.upstreamBody(inputSegments...)
	if err != nil {
		return nil, err
	}
	bodyBytes = h.rewriteResponsesRequestBodyWithToolOptimizersForModel(ctx, bodyBytes, request.Model, "responses/websocket", true, s.toolContexts, s.toolScope)
	headers := s.requestHeaders(request, includeTurnState)
	// The websocket bridge records each turn's usage downstream from the streamed
	// response body (recordTurnStats), so the per-turn usage total returned here
	// is not observed separately — discard it.
	if compactionResp, handled, _, err := h.maybeBuildResponsesCompactionTriggerResponse(ctx, bodyBytes, headers, true); handled || err != nil {
		return compactionResp, err
	}
	return h.postResponsesWithHeadersForModel(ctx, bodyBytes, headers, request.Model)
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

func (s *responsesWebSocketSession) maybeAutoCompactHistory(h *ProxyHandler, request *responsesWebSocketCreateRequest, metrics responsesWebSocketRequestMetrics) (responsesWebSocketRequestMetrics, responsesUsage) {
	if s == nil || s.isClosing() || (s.ctx != nil && s.ctx.Err() != nil) {
		return metrics, responsesUsage{}
	}
	ctx, cancel := h.newInferenceUpstreamContext(false)
	// Mark the compaction upstream context as retry-trackable, like the turn
	// itself, so retries during auto-compaction are counted in retry stats.
	ctx = markRetryStatsTracked(ctx)
	inflightGen := s.setInflightCancel(cancel)
	defer func() {
		s.clearInflightCancel(inflightGen)
		cancel()
	}()
	if ctx.Err() != nil {
		return metrics, responsesUsage{}
	}

	budget := newCompactBudget(h.effectiveCompactMaxAttempts())
	compaction, compacted, err := s.compactHistory(h, ctx, request, false, budget)
	autoUsage := budget.usageTotals()
	metrics.addInternalUsage(autoUsage)
	if err != nil {
		h.log.Debug("responses websocket auto-compaction failed",
			logger.Err(err),
			logger.F("model", request.Model),
			logger.F("history_items", len(s.historyItems)),
			logger.F("history_bytes", s.currentHistoryBytes()),
		)
		return metrics, autoUsage
	}
	if !compacted {
		return metrics, autoUsage
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
	return metrics, autoUsage
}

func (s *responsesWebSocketSession) maybeRetryCompactedCreateRequest(h *ProxyHandler, ctx context.Context, request *responsesWebSocketCreateRequest, resp *http.Response, fullReplayUsed bool, metrics *responsesWebSocketRequestMetrics) (*http.Response, error) {
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

	respBody, truncated, readErr := readBodyWithCapAvailable(resp.Body, compactUpstreamErrorBodySize)
	_ = resp.Body.Close()
	lastResp := cloneHTTPResponseWithBody(resp, respBody)
	if truncated || readErr != nil {
		lastResp.Header.Del("Content-Length")
		h.log.Debug("truncated initial upstream 413 response body for websocket compact fallback",
			logger.F("status", resp.StatusCode),
			logger.F("max_bytes", compactUpstreamErrorBodySize),
			logger.Err(readErr),
		)
	}
	if readErr != nil {
		return lastResp, nil
	}

	budget := newCompactBudget(h.effectiveCompactMaxAttempts())
	// The 413 oversized-replay fallback spends upstream /responses tokens before
	// the retried client turn completes. Preserve that spend on every exit path,
	// but fold it into the one client-turn stats record in handleCreateRequest.
	defer func() {
		metrics.addInternalUsage(budget.usageTotals())
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

		retryBody, truncated, readErr := readBodyWithCapAvailable(retryResp.Body, compactUpstreamErrorBodySize)
		_ = retryResp.Body.Close()
		lastResp = cloneHTTPResponseWithBody(retryResp, retryBody)
		if truncated || readErr != nil {
			lastResp.Header.Del("Content-Length")
		}
		if readErr != nil {
			return lastResp, nil
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

func (s *responsesWebSocketSession) completedResponseOutputItems(h *ProxyHandler, data string) []json.RawMessage {
	var envelope struct {
		Response struct {
			Output []json.RawMessage `json:"output"`
		} `json:"response"`
	}
	if err := json.NewDecoder(strings.NewReader(data)).Decode(&envelope); err != nil || len(envelope.Response.Output) == 0 {
		return nil
	}
	items := make([]json.RawMessage, 0, len(envelope.Response.Output))
	for _, item := range envelope.Response.Output {
		if len(item) == 0 {
			continue
		}
		if h != nil {
			h.maybeRewriteOrCaptureToolCommandItem(s.ctx, item, s.toolContexts, s.toolScope, false)
		}
		items = append(items, cloneRawMessage(item))
	}
	return items
}

func (s *responsesWebSocketSession) streamUpstreamResponse(h *ProxyHandler, body io.Reader, headers http.Header, recordTerminal func(int, responsesUsage)) (responsesWebSocketStreamResult, error) {
	var result responsesWebSocketStreamResult

	// Emit a synthetic metadata event so WebSocket clients can discover the
	// actual model used. The Codex CLI parses openai-model from
	// codex.response.metadata frames via response_model().
	if mappedHeaders := responsesWebSocketMetadataHeaders(headers); len(mappedHeaders) > 0 {
		if err := s.writeStreamJSON(map[string]interface{}{
			"type":    "codex.response.metadata",
			"headers": mappedHeaders,
		}); err != nil {
			return result, err
		}
	}

	sawCompleted := false
	completedResponseIDValid := false
	sawSemanticEvent := false

	err := consumeResponsesSSEMessages(body, func(msg responsesSSEMessage) error {
		data := msg.data
		if data == "" {
			return nil
		}
		if data == "[DONE]" {
			return errResponsesWebSocketStreamTerminal
		}

		var event responsesWebSocketStreamEvent
		parsedEvent := json.Unmarshal([]byte(data), &event) == nil
		originalEventType := strings.TrimSpace(event.Type)
		if parsedEvent && originalEventType == "" {
			event.Type = strings.TrimSpace(msg.event)
		}
		wireData := data
		if parsedEvent && originalEventType == "" && event.Type != "" {
			wireData = responsesDataWithEventType(data, event.Type)
		}
		failureStatus := 0
		if parsedEvent && (event.Type == "response.failed" || event.Type == "response.incomplete" || event.Type == "error") {
			failureStatus, _, _ = responsesWebSocketStreamFailureDetails(event, headers)
			if failureStatus != 0 {
				result.usage = event.Response.Usage
				// Account before forwarding either the upstream failure event or the
				// standard error payload. Both writes may fail after client disconnect.
				if recordTerminal != nil {
					recordTerminal(failureStatus, result.usage)
				}
			}
		}
		if !sawSemanticEvent {
			sawSemanticEvent = true
			if parsedEvent && (event.Type == "response.failed" || event.Type == "error") {
				if status, _, ok := classifyResponsesFailure(event, headers); ok {
					failureHeaders := responsesFailureHeaders(event, headers)
					streamErr := responsesStreamEventError(event)
					if writeErr := s.sendWrappedErrorDetails(status, responsesPrecommitErrorMessage(event, status), strings.TrimSpace(streamErr.Code), strings.TrimSpace(streamErr.Param), failureHeaders); writeErr != nil {
						return writeErr
					}
					return &streamFailedUpstreamError{status: failureStatus}
				}
			}
		}

		completedEvent := parsedEvent && event.Type == "response.completed"
		validCompletedEvent := false
		if completedEvent {
			sawCompleted = true
			if responseID := strings.TrimSpace(event.Response.ID); responseID != "" {
				result.responseID = responseID
				validCompletedEvent = true
				completedResponseIDValid = true
			}
			if !event.Response.Usage.isZero() {
				result.usage = event.Response.Usage
			}
			if len(result.outputItems) == 0 && strings.Contains(data, `"output"`) {
				result.outputItems = s.completedResponseOutputItems(h, data)
			}
			if validCompletedEvent && recordTerminal != nil {
				// A structurally valid provider completion is authoritative before
				// client delivery or post-terminal auto-compaction can fail or stall.
				recordTerminal(http.StatusOK, result.usage)
			}
		}

		if err := s.writeStreamTextMessage([]byte(wireData)); err != nil {
			return err
		}

		if !parsedEvent {
			return nil
		}

		switch event.Type {
		case "response.created":
			if result.responseID == "" && event.Response.ID != "" {
				result.responseID = event.Response.ID
			}
		case "response.output_item.done":
			if len(event.Item) > 0 {
				if h != nil {
					h.maybeRewriteOrCaptureToolCommandItem(s.ctx, event.Item, s.toolContexts, s.toolScope, false)
				}
				result.outputItems = append(result.outputItems, cloneRawMessage(event.Item))
			}
		case "response.completed":
			return errResponsesWebSocketStreamTerminal
		case "response.failed", "response.incomplete", "error":
			if writeErr := s.sendUpstreamStreamFailure(event, headers); writeErr != nil {
				return writeErr
			}
			// Return the sentinel immediately to break out of the SSE
			// scanner loop. The failure event has already been forwarded to
			// the client above, and we also emit a standard error payload so
			// websocket clients can surface the upstream error details. Carry the
			// classified status so the turn is recorded with its exact semantic
			// status (e.g. 429/503) rather than a generic 502.
			return &streamFailedUpstreamError{status: failureStatus}
		}

		return nil
	})
	if errors.Is(err, errStreamFailedUpstream) {
		return result, err
	}
	if err != nil && !errors.Is(err, errResponsesWebSocketStreamTerminal) {
		return result, err
	}

	if sawCompleted {
		if !completedResponseIDValid {
			return result, fmt.Errorf("response.completed missing response id")
		}
		return result, nil
	}
	return result, fmt.Errorf("stream ended before response.completed")
}

func (s *responsesWebSocketSession) writeJSON(payload interface{}) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return s.writeTextMessage(encoded)
}

func (s *responsesWebSocketSession) writeStreamJSON(payload interface{}) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return s.writeStreamTextMessage(encoded)
}

func (s *responsesWebSocketSession) writeStreamTextMessage(payload []byte) error {
	if s == nil || s.conn == nil {
		return &responsesWebSocketClientWriteError{err: net.ErrClosed}
	}
	_ = s.conn.SetWriteDeadline(time.Now().Add(s.effectiveWriteWait()))
	if err := s.conn.WriteMessage(websocket.TextMessage, payload); err != nil {
		s.beginClosingForStreamWriteFailure()
		s.hardClose()
		return &responsesWebSocketClientWriteError{err: err}
	}
	return nil
}

func (s *responsesWebSocketSession) writeTextMessage(payload []byte) error {
	if s == nil || s.conn == nil {
		return &responsesWebSocketClientWriteError{err: net.ErrClosed}
	}
	_ = s.conn.SetWriteDeadline(time.Now().Add(s.effectiveWriteWait()))
	if err := s.conn.WriteMessage(websocket.TextMessage, payload); err != nil {
		s.beginClosing()
		s.hardClose()
		return &responsesWebSocketClientWriteError{err: err}
	}
	return nil
}

func (s *responsesWebSocketSession) sendUpstreamStreamFailure(event responsesWebSocketStreamEvent, headers http.Header) error {
	headers = responsesFailureHeaders(event, headers)
	status, message, code := responsesWebSocketStreamFailureDetails(event, headers)
	if status == 0 || strings.TrimSpace(message) == "" {
		return nil
	}
	return s.sendWrappedErrorDetails(status, message, code, strings.TrimSpace(responsesStreamEventError(event).Param), headers)
}

func (s *responsesWebSocketSession) sendWrappedError(status int, message, code string, headers http.Header) error {
	return s.sendWrappedErrorDetails(status, message, code, "", headers)
}

func (s *responsesWebSocketSession) sendWrappedErrorDetails(status int, message, code, param string, headers http.Header) error {
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
	if param != "" {
		payload["error"].(map[string]interface{})["param"] = param
	}
	headers = responsesWebSocketErrorHeaders(status, headers)
	if mappedHeaders := flattenResponsesWebSocketHeaders(headers); len(mappedHeaders) > 0 {
		payload["headers"] = mappedHeaders
	}
	return s.writeJSON(payload)
}

func responsesDataWithEventType(data, eventType string) string {
	var object map[string]json.RawMessage
	if json.Unmarshal([]byte(data), &object) != nil || object == nil {
		return data
	}
	encodedType, _ := json.Marshal(eventType)
	object["type"] = encodedType
	encoded, err := json.Marshal(object)
	if err != nil {
		return data
	}
	return string(encoded)
}

func consumeResponsesSSEData(body io.Reader, onData func(string) error) error {
	return consumeResponsesSSEMessages(body, func(msg responsesSSEMessage) error {
		return onData(msg.data)
	})
}

func consumeResponsesSSEMessages(body io.Reader, onMessage func(responsesSSEMessage) error) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, openAIStreamScannerInitialBuffer), openAIStreamScannerMaxBuffer)

	var eventName string
	var dataLines []string
	firstLine := true
	dispatch := func() error {
		if len(dataLines) == 0 && strings.TrimSpace(eventName) == "" {
			return nil
		}
		msg := responsesSSEMessage{
			event:    eventName,
			data:     strings.Join(dataLines, "\n"),
			semantic: len(dataLines) > 0 || strings.TrimSpace(eventName) != "",
		}
		eventName = ""
		dataLines = dataLines[:0]
		return onMessage(msg)
	}

	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if firstLine {
			line = strings.TrimPrefix(line, "\uFEFF")
			firstLine = false
		}
		if line == "" {
			if err := dispatch(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value := line, ""
		if colon := strings.IndexByte(line, ':'); colon >= 0 {
			field = line[:colon]
			value = strings.TrimPrefix(line[colon+1:], " ")
		}
		switch field {
		case "event":
			eventName = value
		case "data":
			dataLines = append(dataLines, value)
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

func responsesWebSocketStreamFailureDetails(event responsesWebSocketStreamEvent, headers http.Header) (int, string, string) {
	switch event.Type {
	case "response.failed", "error":
		if status, _, ok := classifyResponsesFailure(event, headers); ok {
			message := responsesPrecommitErrorMessage(event, status)
			return status, message, strings.TrimSpace(responsesStreamEventError(event).Code)
		}
		streamErr := responsesStreamEventError(event)
		errType := strings.TrimSpace(streamErr.Type)
		code := strings.TrimSpace(streamErr.Code)
		message := strings.TrimSpace(streamErr.Message)
		if message == "" {
			if code != "" {
				message = code
			} else if event.Type == "error" {
				message = "upstream error event"
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
	switch strings.ToLower(strings.TrimSpace(errType)) {
	case "invalid_request_error", "user_error":
		return http.StatusBadRequest
	case "authentication_error":
		return http.StatusUnauthorized
	case "permission_error", "forbidden":
		return http.StatusForbidden
	case "not_found_error":
		return http.StatusNotFound
	case "conflict_error":
		return http.StatusConflict
	case "rate_limit_error", "too_many_requests":
		return http.StatusTooManyRequests
	case "server_error":
		return http.StatusInternalServerError
	default:
		return http.StatusBadGateway
	}
}

func responsesWebSocketErrorHeaders(status int, headers http.Header) http.Header {
	if status != http.StatusTooManyRequests && status != http.StatusServiceUnavailable && status != http.StatusGatewayTimeout {
		return headers
	}
	if strings.TrimSpace(headerGetCI(headers, "Retry-After")) != "" {
		return headers
	}
	retryAfter, _ := selectResponsesRetryAfter(headers)
	if retryAfter == "" {
		return headers
	}
	result := headers.Clone()
	if result == nil {
		result = make(http.Header)
	}
	result.Set("Retry-After", retryAfter)
	return result
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
