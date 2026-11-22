package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sozercan/vekil/logger"
)

const responsesNativeMaxPendingItems = 4096

var errResponsesNativeClosed = errors.New("native upstream websocket closed; resend the turn to continue from the proxy's history")

type responsesNativeRequestContextKey struct{}

type responsesNativeRequest struct {
	upstream           *responsesNativeUpstream
	model              string
	delta              bool
	previousResponseID string
	headers            http.Header
	inputItems         int
	inputBytes         int
	dispatched         atomic.Bool
}

type responsesNativeFrame struct {
	payload  []byte
	terminal bool
}

type responsesNativeTurn struct {
	frames chan responsesNativeFrame
	done   chan struct{}
	once   sync.Once
}

type responsesNativeUpstream struct {
	ctx         context.Context
	mu          sync.Mutex
	conn        *websocket.Conn
	binding     string
	credential  [32]byte
	model       string
	publicModel string
	responseID  string
	requestID   string
	turn        *responsesNativeTurn
	closed      bool
	done        chan struct{}
}

func newResponsesNativeUpstream(ctx context.Context) *responsesNativeUpstream {
	return &responsesNativeUpstream{ctx: ctx, done: make(chan struct{})}
}

func (u *responsesNativeUpstream) started() bool {
	if u == nil {
		return false
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.conn != nil
}

func (u *responsesNativeUpstream) isClosed() bool {
	if u == nil {
		return false
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.closed
}

func (u *responsesNativeUpstream) continuation() (responseID, publicModel string, open bool) {
	if u == nil {
		return "", "", false
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.responseID, u.publicModel, u.conn != nil && !u.closed
}

func (u *responsesNativeUpstream) previousResponseID() string {
	if u == nil {
		return ""
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.responseID
}

func (u *responsesNativeUpstream) upstreamRequestID() string {
	if u == nil {
		return ""
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.requestID
}

func (u *responsesNativeUpstream) rememberResponse(id string) {
	if u == nil {
		return
	}
	u.mu.Lock()
	u.responseID = id
	u.mu.Unlock()
}

func (u *responsesNativeUpstream) close() {
	if u == nil {
		return
	}
	u.mu.Lock()
	if u.closed {
		u.mu.Unlock()
		return
	}
	u.closed = true
	conn := u.conn
	close(u.done)
	u.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

func (u *responsesNativeUpstream) readPump(conn *websocket.Conn) {
	defer u.close()
	for {
		messageType, payload, err := conn.ReadMessage()
		if err != nil || messageType != websocket.TextMessage {
			return
		}
		var envelope struct {
			Type     string          `json:"type"`
			StreamID json.RawMessage `json:"stream_id"`
		}
		if json.Unmarshal(payload, &envelope) != nil || envelope.Type == "" || len(envelope.StreamID) > 0 {
			return
		}
		var compact bytes.Buffer
		if json.Compact(&compact, payload) != nil {
			return
		}
		frame := responsesNativeFrame{payload: compact.Bytes(), terminal: isResponsesTerminalType(envelope.Type)}
		u.mu.Lock()
		turn := u.turn
		if frame.terminal {
			u.turn = nil
		}
		u.mu.Unlock()
		if turn == nil {
			return
		}
		select {
		case turn.frames <- frame:
		case <-turn.done:
			return
		case <-u.done:
			return
		}
		if envelope.Type == "response.failed" || envelope.Type == "response.cancelled" || envelope.Type == "response.canceled" || envelope.Type == "error" {
			return
		}
	}
}

func (h *ProxyHandler) maybeRejectNativeResponsesRequest(req *http.Request) *http.Response {
	rejected, _, _ := h.selectNativeResponsesTransport(req)
	return rejected
}

func (h *ProxyHandler) selectNativeResponsesTransport(req *http.Request) (rejected *http.Response, native bool, reason string) {
	marker, _ := req.Context().Value(responsesNativeRequestContextKey{}).(*responsesNativeRequest)
	if marker == nil || marker.upstream == nil {
		return nil, false, ""
	}
	u := marker.upstream
	provider, known := providerRouteFromRequest(req)
	copilot := known && provider.kind == string(providerTypeCopilot)
	u.mu.Lock()
	closed, started, binding, credential := u.closed, u.conn != nil, u.binding, u.credential
	u.mu.Unlock()
	sameConnection := copilot && binding == responsesNativeRequestBinding(req, provider.id, marker.model)
	sameCredential := credential == responsesNativeRequestCredential(req)

	if marker.delta {
		switch {
		case closed || !started || !sameConnection:
			u.close()
			return responsesNativeErrorResponse(req, http.StatusConflict, errResponsesNativeClosed.Error()), false, ""
		case !sameCredential:
			u.close()
			return responsesNativeErrorResponse(req, http.StatusConflict, "native upstream websocket credential changed; resend the turn to continue from the proxy's history"), false, ""
		case marker.inputItems > responsesNativeMaxPendingItems:
			return responsesNativeErrorResponse(req, http.StatusBadRequest, "websocket input exceeds session item limit"), false, ""
		case marker.inputBytes > maxRequestBodySize:
			return responsesNativeErrorResponse(req, http.StatusBadRequest, "staged websocket input exceeds session limits"), false, ""
		}
		if _, err := responsesNativeTurnHeaders(marker.headers); err != nil {
			return responsesNativeErrorResponse(req, http.StatusBadRequest, err.Error()), false, ""
		}
		return nil, true, ""
	}

	if closed || !copilot {
		return nil, false, ""
	}
	if started && (!sameConnection || !sameCredential) {
		u.close()
		return nil, false, responsesNativeDeclinedConnectionMismatch
	}
	if !started {
		if eligible, reason := h.responsesNativeModelEligible(req, provider.id); !eligible {
			return nil, false, reason
		}
	}
	if marker.inputItems > responsesNativeMaxPendingItems || marker.inputBytes > maxRequestBodySize {
		return nil, false, responsesNativeDeclinedInputTooLarge
	}
	if _, err := responsesNativeTurnHeaders(marker.headers); err != nil {
		return nil, false, responsesNativeDeclinedTurnHeaders
	}
	if !started {
		if _, _, _, err := h.prepareResponsesNativeDial(req); err != nil {
			return nil, false, responsesNativeDeclinedDialUnavailable
		}
	}
	return nil, true, ""
}

func responsesNativeRequestBinding(req *http.Request, providerID, model string) string {
	info, _ := explicitRouteResponseInfoFromResponse(&http.Response{Request: req})
	return providerID + "\x00" + req.URL.String() + "\x00" + info.routeID + "\x00" + info.targetID + "\x00" + model
}

func responsesNativeRequestCredential(req *http.Request) [32]byte {
	if metadata, ok := req.Context().Value(copilotInferenceRequestContextKey{}).(copilotInferenceRequest); ok {
		for _, key := range metadata.keys {
			if key.scope == copilotThrottleAccount {
				return key.identity
			}
		}
	}
	return sha256.Sum256([]byte(req.Header.Get("Authorization")))
}

func (h *ProxyHandler) maybeSendNativeResponses(req *http.Request) (response *http.Response, handled bool, sendErr error) {
	var receipt *taskInferenceSend
	defer func() { receipt.finish(response, sendErr) }()
	marker, ok := req.Context().Value(responsesNativeRequestContextKey{}).(*responsesNativeRequest)
	if !ok || marker == nil || marker.upstream == nil {
		return nil, false, nil
	}
	rejected, native, reason := h.selectNativeResponsesTransport(req)
	if rejected != nil {
		return rejected, true, nil
	}
	provider, _ := providerRouteFromRequest(req)
	if !native {
		if reason != "" {
			h.log.Debug("responses websocket using HTTP upstream transport",
				logger.F("provider", provider.id),
				logger.F("model", responsesNativeRequestModel(req)),
				logger.F("reason", reason),
			)
		}
		return nil, false, nil
	}
	if req.Body == nil {
		return responsesNativeErrorResponse(req, http.StatusBadRequest, "native upstream websocket requires a request body"), true, nil
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, maxLargeRequestBodySize+1))
	_ = req.Body.Close()
	if err != nil || len(body) > maxLargeRequestBodySize {
		return responsesNativeErrorResponse(req, http.StatusBadRequest, "native websocket request exceeds the request limit"), true, nil
	}
	payload, model, err := buildResponsesNativeCreate(body, marker.previousResponseID, marker.headers)
	if err != nil {
		return responsesNativeErrorResponse(req, http.StatusBadRequest, err.Error()), true, nil
	}
	if len(payload) > maxLargeRequestBodySize {
		return responsesNativeErrorResponse(req, http.StatusRequestEntityTooLarge, "native websocket request exceeds the request limit"), true, nil
	}
	info, _ := explicitRouteResponseInfoFromResponse(&http.Response{Request: req})
	binding := responsesNativeRequestBinding(req, provider.id, marker.model)
	credential := responsesNativeRequestCredential(req)
	u := marker.upstream
	u.mu.Lock()
	if u.closed {
		u.mu.Unlock()
		return responsesNativeErrorResponse(req, http.StatusConflict, errResponsesNativeClosed.Error()), true, nil
	}
	if u.conn != nil && u.credential != credential {
		u.mu.Unlock()
		u.close()
		return responsesNativeErrorResponse(req, http.StatusConflict, "native upstream websocket credential changed; reconnect with full input"), true, nil
	}
	if u.conn != nil && (u.binding != binding || u.model != model) {
		u.mu.Unlock()
		return responsesNativeErrorResponse(req, http.StatusBadRequest, "native upstream websocket is pinned to its provider and model"), true, nil
	}
	conn := u.conn
	u.mu.Unlock()
	responseHeaders := make(http.Header)
	if conn == nil {
		dialer, endpoint, headers, err := h.prepareResponsesNativeDial(req)
		if err != nil {
			return responsesNativeErrorResponse(req, http.StatusBadRequest, err.Error()), true, nil
		}
		var handshake *http.Response
		receipt = h.beginTaskInferenceSend(req)
		conn, handshake, err = dialer.DialContext(req.Context(), endpoint, headers)
		if err != nil {
			if handshake != nil {
				handshake.Request = req
				return handshake, true, nil
			}
			return nil, true, err
		}
		requestID := ""
		if handshake != nil {
			copyCopilotDiagnosticHeaders(responseHeaders, handshake.Header)
			requestID = boundedSingleHeaderValue(handshake.Header, "X-Github-Request-Id")
		}
		h.log.Debug("responses websocket native upstream connected",
			logger.F("provider", provider.id),
			logger.F("model", model),
			logger.F("upstream_request_id", requestID),
		)
		conn.SetReadLimit(openAIStreamScannerMaxBuffer - 64)
		u.mu.Lock()
		if u.closed || u.ctx.Err() != nil {
			u.mu.Unlock()
			_ = conn.Close()
			return nil, true, context.Canceled
		}
		u.conn, u.binding, u.model, u.publicModel, u.credential, u.requestID = conn, binding, model, marker.model, credential, requestID
		u.mu.Unlock()
		go u.readPump(conn)
	}
	u.mu.Lock()
	if u.closed || u.turn != nil {
		u.mu.Unlock()
		if receipt != nil {
			return nil, true, errResponsesNativeClosed
		}
		return responsesNativeErrorResponse(req, http.StatusConflict, errResponsesNativeClosed.Error()), true, nil
	}
	turn := &responsesNativeTurn{frames: make(chan responsesNativeFrame, 1), done: make(chan struct{})}
	u.turn = turn
	u.mu.Unlock()
	if operation := routeOperationFromContext(req.Context()); operation != nil {
		operation.pinTarget(info.targetID)
		operation.setCommitment(downstreamCommitmentProtocolFrame)
	}
	stopCancellation := context.AfterFunc(req.Context(), u.close)
	responseHeaders.Set("Content-Type", "text/event-stream")
	resp := &http.Response{
		StatusCode: http.StatusOK, Header: responseHeaders, Request: req, ContentLength: -1,
		Body: &responsesNativeBody{upstream: u, turn: turn, stopCancellation: stopCancellation},
	}
	deadline := time.Now().Add(responsesWebSocketWriteWait)
	if requestDeadline, ok := req.Context().Deadline(); ok && requestDeadline.Before(deadline) {
		deadline = requestDeadline
	}
	_ = conn.SetWriteDeadline(deadline)
	if receipt == nil {
		receipt = h.beginTaskInferenceSend(req)
	}
	if trace := httptrace.ContextClientTrace(req.Context()); trace != nil && trace.WroteHeaders != nil {
		trace.WroteHeaders()
	}
	marker.dispatched.Store(true)
	if err := conn.WriteMessage(websocket.TextMessage, payload); err != nil {
		u.close()
	}
	return resp, true, nil
}

func buildResponsesNativeCreate(body []byte, previousID string, headers http.Header) ([]byte, string, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil || fields == nil {
		return nil, "", fmt.Errorf("invalid native websocket request")
	}
	model := rawJSONString(fields["model"])
	fields["type"] = json.RawMessage(`"response.create"`)
	delete(fields, "stream")
	delete(fields, "previous_response_id")
	if previousID != "" {
		fields["previous_response_id"], _ = json.Marshal(previousID)
	}
	turnHeaders, err := responsesNativeTurnHeaders(headers)
	if err != nil {
		return nil, "", err
	}
	delete(fields, "headers")
	if len(turnHeaders) > 0 {
		fields["headers"], _ = json.Marshal(turnHeaders)
	}
	payload, err := json.Marshal(fields)
	return payload, model, err
}

func responsesNativeTurnHeaders(headers http.Header) (map[string]string, error) {
	turnHeaders := make(map[string]string)
	for name, values := range headers {
		if len(values) == 0 {
			continue
		}
		canonical := http.CanonicalHeaderKey(name)
		if _, hopByHop := hopByHopHeaders[canonical]; hopByHop {
			continue
		}
		switch canonical {
		case "Authorization", "Api-Key", "X-Api-Key", "Cookie", "Set-Cookie", "Host",
			"Accept", "Accept-Encoding", "Content-Type", "Content-Length", "Content-Encoding",
			"Editor-Version", "Editor-Plugin-Version", "User-Agent", "Copilot-Integration-Id",
			"X-Github-Api-Version", "X-Request-Id", "Openai-Intent", "X-Codex-Turn-State", "X-Vekil-History-Complete":
			continue
		}
		if strings.HasPrefix(canonical, "Sec-Websocket-") {
			continue
		}
		turnHeaders[name] = values[len(values)-1]
	}
	if err := validateResponsesWebSocketHeaders(turnHeaders); err != nil {
		return nil, err
	}
	return turnHeaders, nil
}

func (h *ProxyHandler) prepareResponsesNativeDial(req *http.Request) (*websocket.Dialer, string, http.Header, error) {
	endpoint := *req.URL
	switch endpoint.Scheme {
	case "https":
		endpoint.Scheme = "wss"
	case "http":
		endpoint.Scheme = "ws"
	default:
		return nil, "", nil, fmt.Errorf("native websocket requires an HTTP provider endpoint")
	}
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second, Proxy: http.ProxyFromEnvironment}
	transport := http.DefaultTransport
	if h.client != nil && h.client.Transport != nil {
		transport = h.client.Transport
	}
	if configured, ok := transport.(*http.Transport); ok {
		dialer.Proxy = configured.Proxy
		dialer.NetDialContext = configured.DialContext
		dialer.NetDialTLSContext = configured.DialTLSContext
		if configured.TLSClientConfig != nil {
			dialer.TLSClientConfig = configured.TLSClientConfig.Clone()
		}
	} else {
		return nil, "", nil, fmt.Errorf("native websocket requires an HTTP transport with explicit dial settings")
	}
	if dialer.TLSClientConfig == nil {
		dialer.TLSClientConfig = &tls.Config{}
	}
	dialer.TLSClientConfig.NextProtos = []string{"http/1.1"}
	headers := req.Header.Clone()
	for name := range headers {
		switch strings.ToLower(name) {
		case "connection", "upgrade", "host", "content-type", "content-length", "accept", "accept-encoding":
			delete(headers, name)
		default:
			if strings.HasPrefix(strings.ToLower(name), "sec-websocket-") {
				delete(headers, name)
			}
		}
	}
	return &dialer, endpoint.String(), headers, nil
}

func responsesNativeErrorResponse(req *http.Request, status int, message string) *http.Response {
	body, _ := json.Marshal(map[string]any{"error": map[string]string{"type": "invalid_request_error", "message": message}})
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(body)), Request: req}
}

type responsesNativeBody struct {
	upstream         *responsesNativeUpstream
	turn             *responsesNativeTurn
	stopCancellation func() bool
	pending          []byte
	terminal         atomic.Bool
	closeOnce        sync.Once
}

func (b *responsesNativeBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(b.pending) == 0 {
		if b.terminal.Load() {
			return 0, io.EOF
		}
		var frame responsesNativeFrame
		select {
		case frame = <-b.turn.frames:
		default:
			select {
			case frame = <-b.turn.frames:
			case <-b.turn.done:
				return 0, io.ErrClosedPipe
			case <-b.upstream.done:
				select {
				case frame = <-b.turn.frames:
				default:
					return 0, errResponsesNativeClosed
				}
			}
		}
		b.terminal.Store(frame.terminal)
		if frame.terminal {
			b.stopCancellation()
		}
		b.pending = make([]byte, 0, len(frame.payload)+8)
		b.pending = append(b.pending, "data: "...)
		b.pending = append(b.pending, frame.payload...)
		b.pending = append(b.pending, '\n', '\n')
	}
	n := copy(p, b.pending)
	b.pending = b.pending[n:]
	return n, nil
}

func (b *responsesNativeBody) Close() error {
	b.closeOnce.Do(func() {
		b.stopCancellation()
		b.turn.once.Do(func() { close(b.turn.done) })
		if !b.terminal.Load() {
			b.upstream.close()
		}
	})
	return nil
}
