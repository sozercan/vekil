package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxCopilotTrafficEntries        = 256
	maxCopilotProbeWaiters          = 256
	maxCopilotThrottleEvidenceBytes = 4 << 10
)

type copilotThrottleScope uint8

const (
	copilotThrottleUnknown copilotThrottleScope = iota
	copilotThrottleModel
	copilotThrottleAccount
	copilotThrottleIntegration
)

type copilotCooldownKey struct {
	scope    copilotThrottleScope
	identity [32]byte
	model    [32]byte
}

type copilotInferenceRequest struct {
	keys     [3]copilotCooldownKey
	endpoint string
}

type copilotInferenceRequestContextKey struct{}
type copilotAdmissionInboundContextKey struct{}
type copilotSourceFingerprintContextKey struct{}

// Only fixed-size fingerprints enter shared state. Including provider and
// origin prevents credentials reused by different providers from sharing a
// cooldown. Integration limits intentionally cover credentials within that
// provider only.
func withCopilotInferenceRequest(req *http.Request, provider *providerRuntime, endpoint string, body []byte) *http.Request {
	if req == nil || req.URL == nil || provider == nil || provider.kind != providerTypeCopilot {
		return req
	}
	switch endpoint {
	case providerEndpointChatCompletions, providerEndpointMessages, providerEndpointResponses:
	default:
		return req
	}
	origin := req.URL.Scheme + "://" + strings.ToLower(req.URL.Host)
	credential := sha256.Sum256([]byte(req.Header.Get("Authorization")))
	if sourceFingerprint, ok := req.Context().Value(copilotSourceFingerprintContextKey{}).([32]byte); ok {
		credential = sourceFingerprint
	}
	account := copilotTrafficFingerprint(provider.id, origin, string(credential[:]))
	integration := copilotTrafficFingerprint(provider.id, origin, req.Header.Get("Copilot-Integration-ID"))
	metadata := copilotInferenceRequest{
		keys: [3]copilotCooldownKey{
			{scope: copilotThrottleModel, identity: account, model: sha256.Sum256([]byte(extractRequestModel(body)))},
			{scope: copilotThrottleAccount, identity: account},
			{scope: copilotThrottleIntegration, identity: integration},
		},
		endpoint: endpoint,
	}
	return req.WithContext(context.WithValue(req.Context(), copilotInferenceRequestContextKey{}, metadata))
}

func copilotTrafficFingerprint(parts ...string) [32]byte {
	hash := sha256.New()
	for _, part := range parts {
		_, _ = io.WriteString(hash, strconv.Itoa(len(part)))
		_, _ = io.WriteString(hash, ":")
		_, _ = io.WriteString(hash, part)
	}
	var sum [32]byte
	copy(sum[:], hash.Sum(nil))
	return sum
}

type copilotCooldown struct {
	until time.Time
	code  string
	probe chan struct{}
}

type copilotTrafficController struct {
	mu        sync.Mutex
	cooldowns map[copilotCooldownKey]*copilotCooldown
	waiters   int
	now       func() time.Time
}

func (c *copilotTrafficController) timeNow() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

type copilotProbeReservation struct {
	key   copilotCooldownKey
	entry *copilotCooldown
	probe chan struct{}
}

type copilotInferencePermit struct {
	controller *copilotTrafficController
	metadata   copilotInferenceRequest
	probes     []copilotProbeReservation
	once       sync.Once
}

func (p *copilotInferencePermit) release() {
	if p == nil || p.controller == nil {
		return
	}
	p.once.Do(func() {
		c := p.controller
		c.mu.Lock()
		defer c.mu.Unlock()
		for _, reservation := range p.probes {
			entry := reservation.entry
			if entry.probe != reservation.probe {
				continue
			}
			entry.probe = nil
			if !entry.until.After(c.timeNow()) && c.cooldowns[reservation.key] == entry {
				delete(c.cooldowns, reservation.key)
			}
			close(reservation.probe)
		}
	})
}

func copilotInferenceAdmissionContext(req *http.Request) (context.Context, context.CancelFunc) {
	ctx := req.Context()
	inbound, _ := ctx.Value(copilotAdmissionInboundContextKey{}).(context.Context)
	if operation := routeOperationFromContext(ctx); operation != nil && operation.inbound != nil {
		inbound = operation.inbound
	}
	if inbound == nil || inbound.Done() == nil {
		return ctx, func() {}
	}
	admissionCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(inbound, cancel)
	if inbound.Err() != nil {
		cancel()
	}
	return admissionCtx, func() { stop(); cancel() }
}

// acquireCopilotInference runs before send reservation or physical-attempt
// accounting. Active cooldowns return a 429 without issuing an upstream request.
// One request probes an expired cooldown; concurrent callers wait until the
// probe has either been accepted or supplied another reset.
func (h *ProxyHandler) acquireCopilotInference(req *http.Request) (*copilotInferencePermit, *http.Response, error) {
	metadata, ok := req.Context().Value(copilotInferenceRequestContextKey{}).(copilotInferenceRequest)
	if h == nil || !ok {
		return nil, nil, nil
	}
	c := &h.copilotTraffic
	ctx, cancel := copilotInferenceAdmissionContext(req)
	defer cancel()
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if h.ShuttingDown() {
			return nil, nil, context.Canceled
		}
		c.mu.Lock()
		now := c.timeNow()
		var active *copilotCooldown
		var wait <-chan struct{}
		for _, key := range metadata.keys {
			if entry := c.cooldowns[key]; entry != nil {
				if entry.until.After(now) && (active == nil || entry.until.After(active.until)) {
					active = entry
				}
				if entry.probe != nil {
					wait = entry.probe
				}
			}
		}
		if active != nil {
			code, delay := active.code, active.until.Sub(now)
			c.mu.Unlock()
			return nil, copilotCooldownResponse(req, metadata.endpoint, code, delay), nil
		}
		if wait != nil {
			if c.waiters >= maxCopilotProbeWaiters {
				c.mu.Unlock()
				return nil, nil, &providerRequestError{statusCode: http.StatusServiceUnavailable, err: fmt.Errorf("too many requests waiting for rate-limit recovery; retry after the active probe finishes")}
			}
			c.waiters++
			c.mu.Unlock()
			select {
			case <-ctx.Done():
			case <-h.lifecycleContext().Done():
			case <-wait:
			}
			c.mu.Lock()
			c.waiters--
			c.mu.Unlock()
			continue
		}
		var permit *copilotInferencePermit
		for _, key := range metadata.keys {
			if entry := c.cooldowns[key]; entry != nil {
				if permit == nil {
					permit = &copilotInferencePermit{controller: c, metadata: metadata}
				}
				entry.probe = make(chan struct{})
				permit.probes = append(permit.probes, copilotProbeReservation{key: key, entry: entry, probe: entry.probe})
			}
		}
		c.mu.Unlock()
		if err := ctx.Err(); err != nil {
			permit.release()
			return nil, nil, err
		}
		return permit, nil, nil
	}
}

func copilotCooldownResponse(req *http.Request, endpoint, code string, delay time.Duration) *http.Response {
	envelope := map[string]any{"error": map[string]string{
		"type": "rate_limit_error", "code": code,
		"message": "The upstream rate limit is still active. Retry after the indicated delay.",
	}}
	if endpoint == providerEndpointMessages {
		envelope["type"] = "error"
	}
	body, _ := json.Marshal(envelope)
	return &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header: http.Header{
			"Content-Type": {"application/json"},
			"Retry-After":  {strconv.FormatInt(durationSecondsCeil(delay), 10)},
		},
		Body: io.NopCloser(strings.NewReader(string(body))), ContentLength: int64(len(body)), Request: req,
	}
}

func copilotThrottleCode(body []byte) (string, copilotThrottleScope) {
	if len(body) == 0 || len(body) > maxCopilotThrottleEvidenceBytes || rejectDuplicateJSONMappingKeys(body) != nil {
		return "", copilotThrottleUnknown
	}
	var envelope struct {
		Code  string `json:"code"`
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &envelope) != nil || (envelope.Code != "" && envelope.Error.Code != "" && envelope.Code != envelope.Error.Code) {
		return "", copilotThrottleUnknown
	}
	code := envelope.Error.Code
	if code == "" {
		code = envelope.Code
	}
	switch code {
	case "user_model_rate_limited":
		return code, copilotThrottleModel
	case "user_global_rate_limited", "user_weekly_rate_limited":
		return code, copilotThrottleAccount
	case "integration_rate_limited":
		return code, copilotThrottleIntegration
	default:
		return "", copilotThrottleUnknown
	}
}

func (c *copilotTrafficController) observeThrottle(metadata copilotInferenceRequest, status int, retryAfter string, body []byte) {
	if status != http.StatusTooManyRequests {
		return
	}
	code, scope := copilotThrottleCode(body)
	if scope == copilotThrottleUnknown {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.timeNow()
	delay, valid := parseRetryAfterAt(retryAfter, now)
	if !valid {
		return
	}
	var key copilotCooldownKey
	for _, candidate := range metadata.keys {
		if candidate.scope == scope {
			key = candidate
			break
		}
	}
	if c.cooldowns == nil {
		c.cooldowns = make(map[copilotCooldownKey]*copilotCooldown)
	}
	entry := c.cooldowns[key]
	if entry == nil {
		if len(c.cooldowns) >= maxCopilotTrafficEntries {
			var oldestKey copilotCooldownKey
			var oldest *copilotCooldown
			for candidate, stored := range c.cooldowns {
				if stored.probe == nil && (oldest == nil || stored.until.Before(oldest.until)) {
					oldestKey, oldest = candidate, stored
				}
			}
			if oldest == nil {
				return
			}
			delete(c.cooldowns, oldestKey)
		}
		entry = &copilotCooldown{}
		c.cooldowns[key] = entry
	}
	if until := now.Add(delay); until.After(entry.until) {
		entry.until, entry.code = until, code
	}
}

func (h *ProxyHandler) observeCopilotResponseFailure(req *http.Request, event responsesWebSocketStreamEvent, headers http.Header) {
	if h == nil || req == nil {
		return
	}
	metadata, ok := req.Context().Value(copilotInferenceRequestContextKey{}).(copilotInferenceRequest)
	if !ok {
		return
	}
	h.copilotTraffic.observeResponsesFailure(metadata, event, headers)
}

func (c *copilotTrafficController) observeResponsesFailure(metadata copilotInferenceRequest, event responsesWebSocketStreamEvent, headers http.Header) {
	streamErr := responsesStreamEventError(event)
	body, _ := json.Marshal(map[string]string{"code": streamErr.Code})
	retryAfter, _ := selectResponsesRetryAfter(headers)
	c.observeThrottle(metadata, http.StatusTooManyRequests, retryAfter, body)
}

func (h *ProxyHandler) finishCopilotInference(req *http.Request, resp *http.Response, err error, permit *copilotInferencePermit) {
	if h == nil || err != nil || resp == nil || resp.Body == nil {
		permit.release()
		return
	}
	metadata, known := req.Context().Value(copilotInferenceRequestContextKey{}).(copilotInferenceRequest)
	if permit != nil {
		metadata, known = permit.metadata, true
	}
	if !known {
		return
	}
	retryAfter, _ := selectResponsesRetryAfter(resp.Header)
	contentType, _, _ := strings.Cut(resp.Header.Get("Content-Type"), ";")
	// Responses passthrough and WebSockets already observe failures in their
	// prepared stream. The typed Chat adapter needs observation at body reads.
	responsesChat, _ := req.Context().Value(responsesChatStreamContextKey{}).(bool)
	observeStream := (metadata.endpoint != providerEndpointResponses || responsesChat) && resp.StatusCode == http.StatusOK &&
		strings.EqualFold(strings.TrimSpace(contentType), "text/event-stream")
	if observeStream && metadata.endpoint != providerEndpointResponses {
		_, observeStream = parseRetryAfterAt(retryAfter, h.copilotTraffic.timeNow())
	}
	if permit == nil {
		if resp.StatusCode != http.StatusTooManyRequests && !observeStream {
			return
		}
		permit = &copilotInferencePermit{controller: &h.copilotTraffic, metadata: metadata}
	}
	// A successful HTTP status can still carry a streamed rate-limit failure.
	// Keep recovery probes reserved until the consumer observes the outcome and
	// closes the body, installing any renewed cooldown before waking waiters.
	body := &copilotTrafficBody{
		ReadCloser: resp.Body, permit: permit, status: resp.StatusCode,
		retryAfter: retryAfter,
	}
	if observeStream {
		body.stream = &copilotThrottleStreamObserver{maxBytes: maxCopilotThrottleEvidenceBytes}
		if metadata.endpoint == providerEndpointResponses {
			// Failed Responses can include output before the error. Match the
			// accepted event limit so that output cannot hide a valid reset.
			body.stream.maxBytes = openAIStreamScannerMaxBuffer
			body.headers = resp.Header.Clone()
		}
	}
	resp.Body = body
}

type copilotTrafficBody struct {
	io.ReadCloser
	permit      *copilotInferencePermit
	status      int
	retryAfter  string
	headers     http.Header
	mu          sync.Mutex
	prefix      []byte
	complete    bool
	stream      *copilotThrottleStreamObserver
	observeOnce sync.Once
	closeOnce   sync.Once
	closeErr    error
}

func (b *copilotTrafficBody) observe() {
	b.observeOnce.Do(func() {
		b.mu.Lock()
		prefix := append([]byte(nil), b.prefix...)
		complete := b.complete
		b.mu.Unlock()
		if complete {
			b.permit.controller.observeThrottle(b.permit.metadata, b.status, b.retryAfter, prefix)
		}
		b.permit.release()
	})
}

func (b *copilotTrafficBody) Read(p []byte) (int, error) {
	if b.stream != nil {
		// Close first cancels the inner read, then takes this mutex in observe.
		// Finish inspecting any returned frame before it can release the probe.
		b.mu.Lock()
		n, err := b.ReadCloser.Read(p)
		b.stream.observe(p[:n], err == io.EOF, b.observeStreamEvent)
		b.mu.Unlock()
		return n, err
	}
	n, err := b.ReadCloser.Read(p)
	if b.status == http.StatusTooManyRequests {
		b.mu.Lock()
		b.prefix = append(b.prefix, p[:min(n, maxCopilotThrottleEvidenceBytes+1-len(b.prefix))]...)
		b.complete = b.complete || err == io.EOF
		overflow := len(b.prefix) > maxCopilotThrottleEvidenceBytes
		b.mu.Unlock()
		if err != nil || overflow {
			b.observe()
		}
	}
	return n, err
}

func (b *copilotTrafficBody) observeStreamEvent(eventType, data string) bool {
	if b.permit.metadata.endpoint == providerEndpointResponses {
		var envelope struct {
			Type string `json:"type"`
		}
		if json.Unmarshal([]byte(data), &envelope) != nil {
			return true
		}
		typ := strings.TrimSpace(envelope.Type)
		if typ == "" {
			typ = strings.TrimSpace(eventType)
		} else if named := strings.TrimSpace(eventType); named != "" && named != typ {
			return true
		}
		switch typ {
		case "response.failed", "error":
			event, err := parseResponsesStreamEvent(data)
			if err != nil || rejectDuplicateJSONMappingKeys([]byte(data)) != nil {
				return true
			}
			event.Type = typ
			headers := responsesFailureHeaders(event, b.headers)
			b.permit.controller.observeResponsesFailure(b.permit.metadata, event, headers)
			return false
		case "response.completed", "response.incomplete", "response.cancelled", "response.canceled":
			return rejectDuplicateJSONMappingKeys([]byte(data)) != nil
		default:
			return true
		}
	}
	var failureCode string
	if b.permit.metadata.endpoint == providerEndpointMessages {
		body := []byte(data)
		if anthropicStreamDataIsMessageStop(body) && rejectDuplicateJSONMappingKeys(body) == nil {
			return false
		}
		// Content-block endings are not message endings: a later native
		// Messages error can still renew this request's cooldown.
		failure := inspectAnthropicStreamEvent(eventType, data).failure
		if failure == nil {
			return true
		}
		failureCode = failure.code
	} else {
		if strings.TrimSpace(data) == "[DONE]" {
			return false
		}
		streamErr, failure := parseOpenAIStreamError(eventType, data)
		if !failure {
			return true
		}
		failureCode = streamErr.Code
	}
	// The shared validator requires a complete, bounded JSON object with a
	// recognized code. Generated text and malformed error events cannot set
	// shared cooldowns, even when their text mentions a rate-limit code.
	body := []byte(data)
	if code, scope := copilotThrottleCode(body); scope != copilotThrottleUnknown && code == failureCode {
		b.permit.controller.observeThrottle(b.permit.metadata, http.StatusTooManyRequests, b.retryAfter, body)
	}
	return false
}

// Retain at most one bounded SSE event. Observation does not read ahead or
// rewrite bytes, covering native Messages, Responses, and Chat aggregation.
type copilotThrottleStreamObserver struct {
	frame        []byte
	maxBytes     int
	lineNonempty bool
	pendingCR    bool
	overflow     bool
	done         bool
}

func (o *copilotThrottleStreamObserver) observe(p []byte, eof bool, onData func(string, string) bool) {
	if o.done {
		return
	}
	for _, c := range p {
		if o.pendingCR {
			o.pendingCR = false
			if c == '\n' {
				continue
			}
		}
		if c == '\r' || c == '\n' {
			o.pendingCR = c == '\r'
			if !o.lineNonempty {
				if !o.dispatch(onData) {
					o.done = true
					return
				}
				continue
			}
			o.lineNonempty = false
			c = '\n'
		} else {
			o.lineNonempty = true
		}
		if !o.overflow {
			if len(o.frame) >= o.maxBytes {
				o.overflow = true
				o.frame = o.frame[:0]
			} else {
				o.frame = append(o.frame, c)
			}
		}
	}
	if eof {
		_ = o.dispatch(onData)
		o.done = true
	}
}

func (o *copilotThrottleStreamObserver) dispatch(onData func(string, string) bool) bool {
	defer func() { o.frame = o.frame[:0]; o.overflow = false }()
	if o.overflow {
		return true
	}
	var accumulator sseDataAccumulator
	for lines := string(o.frame); lines != ""; {
		line, rest, _ := strings.Cut(lines, "\n")
		if !accumulator.consumeLine(line, onData) {
			return false
		}
		lines = rest
	}
	return accumulator.dispatch(onData)
}

func (b *copilotTrafficBody) Close() error {
	b.closeOnce.Do(func() {
		b.closeErr = b.ReadCloser.Close()
		b.observe()
		b.permit.release()
	})
	return b.closeErr
}

func (b *copilotTrafficBody) routeAttemptTransportOwnership() *routeAttemptTransportOwner {
	return routeAttemptTransportOwnership(b.ReadCloser)
}

func (b *copilotTrafficBody) cancelRouteAttempt() {
	cancelRouteAttemptBody(b.ReadCloser)
}

func (b *copilotTrafficBody) canceledAtFailure() bool {
	if observed, ok := b.ReadCloser.(interface{ canceledAtFailure() bool }); ok {
		return observed.canceledAtFailure()
	}
	return false
}
