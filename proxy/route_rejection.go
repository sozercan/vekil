package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
)

func routeHTTPRejectionBodyAllowsReplay(response *capturedRouteResponse) bool {
	if routeResponseBodyAllowsReplay(response.body) {
		return true
	}
	// Preserve HTTP 429 admission errors from plain-text gateway middleware.
	// JSON or SSE advertised as text still needs the structured safety check.
	contentType, _, _ := strings.Cut(headerGetCI(response.header, "Content-Type"), ";")
	if response.statusCode != http.StatusTooManyRequests || !strings.EqualFold(strings.TrimSpace(contentType), "text/plain") {
		return false
	}
	body := bytes.TrimSpace(bytes.TrimPrefix(bytes.TrimSpace(response.body), []byte("\xef\xbb\xbf")))
	return !json.Valid(body) && !bytes.HasPrefix(body, []byte("{")) &&
		!bytes.HasPrefix(body, []byte("[")) && !bytes.Contains(body, []byte("data:")) &&
		!bytes.Contains(body, []byte("event:"))
}

// A rejection status does not override evidence that the upstream performed
// work. Inspect the complete bounded envelope before permitting another send.
func routeResponseBodyAllowsReplay(body []byte) bool {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return true
	}
	if !json.Valid(body) || rejectDuplicateJSONMappingKeys(body) != nil {
		return false
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal(body, &envelope) != nil || envelope == nil {
		return false
	}
	for name, raw := range envelope {
		switch strings.ToLower(name) {
		case "usage":
			var usage any
			decoder := json.NewDecoder(bytes.NewReader(raw))
			decoder.UseNumber()
			if decoder.Decode(&usage) != nil || !routeRejectionUsageIsZero(usage) {
				return false
			}
		case "output", "choices", "tool_calls":
			if responsesOutputHasProgress(raw) {
				return false
			}
		case "content":
			if !bytes.Equal(bytes.TrimSpace(raw), []byte(`""`)) && responsesOutputHasProgress(raw) {
				return false
			}
		case "function_call", "item", "delta":
			if !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
				return false
			}
		case "status":
			var status string
			if json.Unmarshal(raw, &status) != nil {
				return false
			}
			switch strings.ToLower(strings.TrimSpace(status)) {
			case "", "failed", "error":
			default:
				return false
			}
		case "response":
			if !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) && !routeResponseBodyAllowsReplay(raw) {
				return false
			}
		case "error":
			// Some compatible endpoints attach usage to the error itself.
			if bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) && !routeResponseBodyAllowsReplay(raw) {
				return false
			}
		}
	}
	return true
}

func routeRejectionUsageIsZero(value any) bool {
	switch value := value.(type) {
	case nil:
		return true
	case json.Number:
		count, err := strconv.ParseInt(string(value), 10, 64)
		return err == nil && count == 0
	case map[string]any:
		for _, child := range value {
			if !routeRejectionUsageIsZero(child) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

// Non-streaming Responses can report admission failures inside HTTP 200 JSON.
// Only complete, small, replay-safe failures become retryable HTTP errors.
// Larger bodies and read errors retain their original bytes and transport owner.
func prepareAzureRouteJSONRejection(resp *http.Response, target targetBinding, traffic azureRouteTraffic) *http.Response {
	if resp == nil || resp.StatusCode != http.StatusOK || resp.Body == nil ||
		resp.ContentLength > upstreamErrorDetailMaxBodyBytes ||
		strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		return resp
	}
	source := resp.Body
	prefix, readErr := io.ReadAll(io.LimitReader(source, upstreamErrorDetailMaxBodyBytes+1))
	cloned := new(http.Response)
	*cloned = *resp
	cloned.Body = retainRouteAttemptTransportOwnership(&routeRejectionPrefixBody{
		ReadCloser: source,
		prefix:     bytes.NewReader(prefix),
		readErr:    readErr,
	}, routeAttemptTransportOwnership(source))
	if readErr != nil || len(prefix) > upstreamErrorDetailMaxBodyBytes {
		return cloned
	}
	var envelope struct {
		Status string                        `json:"status"`
		Error  responsesWebSocketStreamError `json:"error"`
	}
	if json.Unmarshal(prefix, &envelope) != nil || !strings.EqualFold(strings.TrimSpace(envelope.Status), "failed") {
		return cloned
	}
	if rejectDuplicateJSONMappingKeys(prefix) != nil {
		return cloned
	}
	var event responsesWebSocketStreamEvent
	event.Type = "response.failed"
	event.Response.Error = envelope.Error
	headers := responsesFailureHeaders(event, resp.Header)
	if status, _, ok := classifyResponsesFailure(event, headers); ok {
		traffic.observe(status, headers)
	}
	if status, certified := routeAdapterCertifiesStreamFailure(target, event); certified && routeResponseBodyAllowsReplay(prefix) {
		cloned.StatusCode = status
		cloned.Status = strconv.Itoa(status) + " " + http.StatusText(status)
		cloned.Header = headers.Clone()
	}
	return cloned
}

type routeRejectionPrefixBody struct {
	io.ReadCloser
	prefix  *bytes.Reader
	readErr error
}

func (b *routeRejectionPrefixBody) Read(p []byte) (int, error) {
	if b.prefix.Len() > 0 {
		return b.prefix.Read(p)
	}
	if b.readErr != nil {
		return 0, b.readErr
	}
	return b.ReadCloser.Read(p)
}

func (b *routeRejectionPrefixBody) canceledAtFailure() bool {
	if observed, ok := b.ReadCloser.(interface{ canceledAtFailure() bool }); ok {
		return observed.canceledAtFailure()
	}
	return false
}
