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
)

const responsesNativeMaxPendingItems = 4096

var errResponsesNativeClosed = errors.New("native upstream websocket is closed; reconnect with full input")

type responsesNativeRequestContextKey struct{}

// This marker belongs to one client turn. Compaction and compatibility helpers
// receive the original context, so they cannot advance the native connection.
type responsesNativeRequest struct {
	upstream           *responsesNativeUpstream
	model              string
	previousResponseID string
	headers            http.Header
	inputItems         int
	inputBytes         int
	stagedInput        bool
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

// One reader handles upstream control frames even between turns. The current
// turn has a single-frame queue; completed history stays on this connection.
type responsesNativeUpstream struct {
	ctx        context.Context
	mu         sync.Mutex
	conn       *websocket.Conn
	binding    string
	credential [32]byte
	model      string
	responseID string
	turn       *responsesNativeTurn
	closed     bool
	done       chan struct{}
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

func (u *responsesNativeUpstream) previousResponseID() string {
	if u == nil {
		return ""
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.responseID
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
		// Compact framing prevents a pretty-printed JSON message from being
		// mistaken for multiple SSE lines by the shared Responses pipeline.
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

// Reject an unusable session before admission and physical-send reservation.
// Provider model rewrites are checked again at dispatch after request encoding.
func (h *ProxyHandler) maybeRejectNativeResponsesRequest(req *http.Request) *http.Response {
	marker, _ := req.Context().Value(responsesNativeRequestContextKey{}).(*responsesNativeRequest)
	if marker == nil || marker.upstream == nil {
		return nil
	}
	u := marker.upstream
	provider, known := providerRouteFromRequest(req)
	u.mu.Lock()
	if u.closed {
		u.mu.Unlock()
		return responsesNativeErrorResponse(req, http.StatusConflict, errResponsesNativeClosed.Error())
	}
	started, binding, credential := u.conn != nil, u.binding, u.credential
	u.mu.Unlock()
	if !known || provider.kind != string(providerTypeCopilot) {
		if started {
			return responsesNativeErrorResponse(req, http.StatusBadRequest, "native upstream websocket is pinned to its provider and model")
		}
		return nil
	}
	if started && binding != responsesNativeRequestBinding(req, provider.id, marker.model) {
		return responsesNativeErrorResponse(req, http.StatusBadRequest, "native upstream websocket is pinned to its provider and model")
	}
	if started && credential != responsesNativeRequestCredential(req) {
		u.close()
		return responsesNativeErrorResponse(req, http.StatusConflict, "native upstream websocket credential changed; reconnect with full input")
	}
	// Validate only after selecting native transport. Other providers keep the
	// HTTP bridge's input contract even when native transport is enabled.
	if marker.inputItems > responsesNativeMaxPendingItems {
		return responsesNativeErrorResponse(req, http.StatusBadRequest, "websocket input exceeds session item limit")
	}
	if marker.stagedInput && marker.inputBytes > maxRequestBodySize {
		return responsesNativeErrorResponse(req, http.StatusBadRequest, "staged websocket input exceeds session limits")
	}
	if _, err := responsesNativeTurnHeaders(marker.headers); err != nil {
		return responsesNativeErrorResponse(req, http.StatusBadRequest, err.Error())
	}
	if !started {
		if _, _, _, err := h.prepareResponsesNativeDial(req); err != nil {
			return responsesNativeErrorResponse(req, http.StatusBadRequest, err.Error())
		}
	}
	return nil
}

func responsesNativeRequestBinding(req *http.Request, providerID, model string) string {
	info, _ := explicitRouteResponseInfoFromResponse(&http.Response{Request: req})
	return providerID + "\x00" + req.URL.String() + "\x00" + info.routeID + "\x00" + info.targetID + "\x00" + model
}

func responsesNativeRequestCredential(req *http.Request) [32]byte {
	if metadata, ok := req.Context().Value(copilotInferenceRequestContextKey{}).(copilotInferenceRequest); ok {
		for _, key := range metadata.keys {
			if key.scope == copilotThrottleAccount {
				// This identity uses the source credential, so service-token
				// refreshes keep the same account binding.
				return key.identity
			}
		}
	}
	return sha256.Sum256([]byte(req.Header.Get("Authorization")))
}

// maybeSendNativeResponses runs after provider resolution, auth, admission and
// route-send reservation. Its response body reuses the existing stream parsing,
// target binding and accounting paths. An established connection never retries
// or migrates, even if an upstream failure precedes visible output.
func (h *ProxyHandler) maybeSendNativeResponses(req *http.Request) (response *http.Response, handled bool, sendErr error) {
	var receipt *taskInferenceSend
	defer func() { receipt.finish(response, sendErr) }()
	marker, ok := req.Context().Value(responsesNativeRequestContextKey{}).(*responsesNativeRequest)
	if !ok || marker == nil || marker.upstream == nil {
		return nil, false, nil
	}
	if rejected := h.maybeRejectNativeResponsesRequest(req); rejected != nil {
		return rejected, true, nil
	}
	provider, known := providerRouteFromRequest(req)
	if !known || provider.kind != string(providerTypeCopilot) {
		if marker.upstream.started() {
			return responsesNativeErrorResponse(req, http.StatusBadRequest, "native upstream websocket is pinned to its provider and model"), true, nil
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
		if handshake != nil {
			copyCopilotDiagnosticHeaders(responseHeaders, handshake.Header)
		}
		conn.SetReadLimit(openAIStreamScannerMaxBuffer - 64)
		u.mu.Lock()
		if u.closed || u.ctx.Err() != nil {
			u.mu.Unlock()
			_ = conn.Close()
			return nil, true, context.Canceled
		}
		u.conn, u.binding, u.model, u.credential = conn, binding, model, credential
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
		// Once a frame write starts, delivery is ambiguous on any failure.
		trace.WroteHeaders()
	}
	if err := conn.WriteMessage(websocket.TextMessage, payload); err != nil {
		u.close()
	}
	// A frame-write failure stays on the body path. Returning a transport error
	// here could authorize another inference attempt after ambiguous delivery.
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
		// requestHeaders includes custom client metadata accepted by the HTTP
		// bridge. Preserve it without overriding provider or connection state.
		switch canonical {
		case "Authorization", "Api-Key", "X-Api-Key", "Cookie", "Set-Cookie", "Host",
			"Accept", "Accept-Encoding", "Content-Type", "Content-Length", "Content-Encoding",
			"Editor-Version", "Editor-Plugin-Version", "User-Agent", "Copilot-Integration-Id",
			"X-Github-Api-Version", "X-Request-Id", "Openai-Intent", "X-Codex-Turn-State":
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
	// The upgrade uses HTTP/1.1 even when ordinary requests negotiate HTTP/2.
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
		// A completed frame wins a subsequent connection close. Providers may
		// close immediately after publishing an otherwise valid terminal result.
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
