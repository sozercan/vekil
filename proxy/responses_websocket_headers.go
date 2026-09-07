package proxy

import (
	"fmt"
	"net/http"
	"strings"
)

// Per-turn headers are transport metadata, not Responses request fields. Keep
// them bounded and prevent client metadata from replacing provider credentials
// or server-issued continuation state.
func validateResponsesWebSocketHeaders(headers map[string]string) error {
	if len(headers) > 32 {
		return fmt.Errorf("headers supports at most 32 entries")
	}
	bytes := 0
	seen := make(map[string]struct{}, len(headers))
	for name, value := range headers {
		if name == "" || len(name) > 128 || strings.TrimSpace(name) != name {
			return fmt.Errorf("invalid websocket header name")
		}
		for _, c := range name {
			if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || strings.ContainsRune("!#$%&'*+-.^_`|~", c) {
				continue
			}
			return fmt.Errorf("invalid websocket header name")
		}
		canonical := http.CanonicalHeaderKey(name)
		if _, ok := seen[canonical]; ok {
			return fmt.Errorf("duplicate websocket header %q", canonical)
		}
		seen[canonical] = struct{}{}
		if len(value) > 8<<10 {
			return fmt.Errorf("websocket header value exceeds 8 KiB")
		}
		for _, c := range value {
			if c == '\t' || (c >= ' ' && c != 0x7f) {
				continue
			}
			return fmt.Errorf("invalid websocket header value")
		}
		bytes += len(name) + len(value)
		if bytes > 16<<10 {
			return fmt.Errorf("websocket headers exceed 16 KiB")
		}
	}
	return nil
}

func responsesWebSocketRequestHeaderAllowed(name string) bool {
	name = http.CanonicalHeaderKey(name)
	if name == "X-Codex-Turn-State" {
		return false
	}
	if _, ok := responsesExtraHeaderNames[name]; ok {
		return true
	}
	return responsesWebSocketMetadataHeaderName(name) != ""
}
