package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

func durableResponsesErrorHeaderState(data []byte) ([]stateBindingToken, error) {
	var event map[string]json.RawMessage
	if err := json.Unmarshal(data, &event); err != nil {
		return nil, err
	}
	_, hasType := event["type"]
	inspect := !hasType
	seenType := false
	for name, raw := range event {
		if !strings.EqualFold(name, "type") {
			continue
		}
		if seenType {
			return nil, errors.New("ambiguous response type aliases")
		}
		seenType = true
		var eventType string
		if err := json.Unmarshal(raw, &eventType); err != nil {
			return nil, err
		}
		switch strings.TrimSpace(eventType) {
		case "", "error", "response.failed":
			inspect = true
		}
	}
	if !inspect {
		return nil, nil
	}
	var tokens []stateBindingToken
	for _, path := range [][]string{{"headers"}, {"error", "headers"}, {"response", "error", "headers"}} {
		values, err := durableResponsesErrorHeaderPath(event, path)
		if err != nil {
			return nil, err
		}
		tokens = append(tokens, values...)
	}
	return tokens, nil
}

func durableResponsesErrorHeaderPath(object map[string]json.RawMessage, path []string) ([]stateBindingToken, error) {
	if len(path) == 0 {
		headers := responsesStreamErrorHeaders(responsesWebSocketStreamError{Headers: object})
		if err := validateResponsesWebSocketTurnStateHeaders(headers); err != nil {
			return nil, err
		}
		return explicitResponseHeaderStateTokens(headers)
	}
	var tokens []stateBindingToken
	seen := false
	for name, raw := range object {
		if !strings.EqualFold(name, path[0]) {
			continue
		}
		if seen {
			return nil, errors.New("ambiguous response error header aliases")
		}
		seen = true
		var nested map[string]json.RawMessage
		if err := json.Unmarshal(raw, &nested); err != nil {
			return nil, err
		}
		values, err := durableResponsesErrorHeaderPath(nested, path[1:])
		if err != nil {
			return nil, err
		}
		tokens = append(tokens, values...)
	}
	return tokens, nil
}

func validateResponsesWebSocketTurnStateHeaders(headers http.Header) error {
	if len(headers.Values("X-Codex-Turn-State")) > 1 {
		return errors.New("multiple response error turn-state values")
	}
	return nil
}

func (h *ProxyHandler) bindDurableFinalWebSocketHeaders(info explicitRouteResponseInfo, headers http.Header) error {
	if h == nil || h.stateBindings == nil || h.stateBindings.durable == nil {
		return nil
	}
	if err := validateResponsesWebSocketTurnStateHeaders(headers); err != nil {
		return err
	}
	return h.bindDurableFinalHeaders(info, headers)
}

func (h *ProxyHandler) bindDurableFinalHeaders(info explicitRouteResponseInfo, headers http.Header) error {
	if h == nil || h.stateBindings == nil || h.stateBindings.durable == nil {
		return nil
	}
	tokens, err := explicitResponseHeaderStateTokens(headers)
	if err != nil || len(tokens) == 0 {
		return err
	}
	if info.stateIdentity == [32]byte{} {
		return fmt.Errorf("final response is missing authenticated state ownership")
	}
	return h.bindExplicitStateTokens(info, tokens)
}

func (h *ProxyHandler) prepareDurableFinalResponseHeaders(resp *http.Response) error {
	info, ok := explicitRouteResponseInfoFromResponse(resp)
	if !ok {
		return nil
	}
	if err := h.bindDurableFinalHeaders(info, resp.Header); err != nil {
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
		return newResponseBodyWriteError(resp, err, false, true, false)
	}
	return nil
}

func (h *ProxyHandler) writeDurableShimPassthrough(w http.ResponseWriter, r *http.Request, upstreamCtx context.Context, resp *http.Response) bool {
	if h.stateBindings == nil || h.stateBindings.durable == nil {
		return false
	}
	info, ok := explicitRouteResponseInfoFromResponse(resp)
	if !ok {
		return false
	}
	var err error
	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusResetContent {
		if err = h.bindExplicitResponseHeaders(info, resp.Header); err == nil {
			copyPassthroughHeaders(w.Header(), resp.Header)
			w.WriteHeader(resp.StatusCode)
			return true
		}
		err = newResponseBodyWriteError(resp, err, false, true, false)
	} else {
		err = writeExplicitResponsesResponse(r.Context(), h, w, resp, info, nil, "")
	}
	if err != nil {
		if !h.handleResponseBodyWriteError(w, r, upstreamCtx, "responses_shim", err) {
			writeOpenAIError(w, http.StatusBadGateway, "failed to validate upstream response state", "server_error")
		}
	}
	return true
}

func durableStateFailureDetails(err error) (message, code string, ok bool) {
	if errors.Is(err, errConversationHistoryCapacity) {
		return errConversationHistoryCapacity.Error(), "conversation_history_capacity_exceeded", true
	}
	if errors.Is(err, errConversationHistoryStorage) {
		return errConversationHistoryStorage.Error(), "conversation_history_storage_unavailable", true
	}
	if errors.Is(err, errDurableStateCapacity) {
		return errDurableStateCapacity.Error(), "state_binding_capacity_exceeded", true
	}
	for _, sentinel := range []error{errDurableStateIO, errDurableStateClosed, errDurableStateCorrupt} {
		if errors.Is(err, sentinel) {
			return "local provider-state storage is unavailable; unrecorded state was withheld; operator recovery is required", "state_binding_storage_unavailable", true
		}
	}
	return "", "", false
}

func writeDurableStateFailure(w http.ResponseWriter, err error) bool {
	message, code, ok := durableStateFailureDetails(err)
	if !ok {
		return false
	}
	writeOpenAIErrorWithDetails(w, http.StatusServiceUnavailable, message, "server_error", "", code)
	return true
}

func writeDurableStateStreamFailure(w io.Writer, err error) bool {
	message, code, ok := durableStateFailureDetails(err)
	if !ok {
		return false
	}
	data, _ := json.Marshal(map[string]any{
		"type":  "error",
		"error": map[string]string{"type": "server_error", "code": code, "message": message},
	})
	_, _ = io.WriteString(w, "event: error\ndata: "+string(data)+"\n\n")
	return true
}
