package proxy

import (
	"context"
	"net/http"
	"sort"
	"strings"
)

type copilotRequestMetadataContextKey struct{}

var copilotRequestMetadataHeaderNames = []string{
	"X-Initiator",
	"X-Interaction-Id",
	"X-Client-Session-Id",
}

const diagnosticHeaderValueLimit = 1024

// Caller attribution is optional. Copy only explicit, unambiguous values;
// provider credentials and integration headers remain server-owned.
func copyCopilotRequestMetadata(dst, src http.Header) {
	if dst == nil {
		return
	}
	for _, name := range copilotRequestMetadataHeaderNames {
		value := boundedSingleHeaderValue(src, name)
		if value == "" {
			continue
		}
		if name == "X-Initiator" && value != "user" && value != "agent" {
			continue
		}
		dst.Set(name, value)
	}
}

func withCopilotRequestMetadata(ctx context.Context, headers http.Header) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	metadata := make(http.Header, len(copilotRequestMetadataHeaderNames))
	copyCopilotRequestMetadata(metadata, headers)
	if len(metadata) == 0 {
		return ctx
	}
	return context.WithValue(ctx, copilotRequestMetadataContextKey{}, metadata)
}

func copilotRequestMetadataFromContext(ctx context.Context) http.Header {
	if ctx == nil {
		return nil
	}
	headers, _ := ctx.Value(copilotRequestMetadataContextKey{}).(http.Header)
	return headers
}

func copyCopilotRequestMetadataContext(dst, src context.Context) context.Context {
	if headers := copilotRequestMetadataFromContext(src); len(headers) > 0 {
		return context.WithValue(dst, copilotRequestMetadataContextKey{}, headers)
	}
	return dst
}

func boundedSingleHeaderValue(headers http.Header, name string) string {
	values := headerValuesCI(headers, name)
	if len(values) != 1 {
		return ""
	}
	value := strings.TrimSpace(values[0])
	if len(value) == 0 || len(value) > diagnosticHeaderValueLimit {
		return ""
	}
	for i := range len(value) {
		if value[i] < 0x20 || value[i] == 0x7f {
			return ""
		}
	}
	return value
}

// Preserve bounded operational response metadata through protocol translation.
// This is a response-only allowlist; it never feeds request headers or logs.
func copyCopilotDiagnosticHeaders(dst, src http.Header) {
	if dst == nil || len(src) == 0 {
		return
	}
	names := make([]string, 0, min(len(src), 32))
	for name := range src {
		if isCopilotDiagnosticHeader(name) {
			names = append(names, http.CanonicalHeaderKey(name))
		}
	}
	sort.Strings(names)
	remaining := 16 << 10
	for _, name := range names[:min(len(names), 32)] {
		value := boundedSingleHeaderValue(src, name)
		if value == "" || len(name)+len(value) > remaining {
			continue
		}
		dst.Set(name, value)
		remaining -= len(name) + len(value)
	}
}

func isCopilotDiagnosticHeader(name string) bool {
	if len(name) == 0 || len(name) > 128 {
		return false
	}
	lower := strings.ToLower(name)
	return lower == "retry-after" || lower == "retry-after-ms" ||
		lower == "x-ms-retry-after-ms" || lower == "x-request-id" ||
		lower == "x-github-request-id" || lower == "x-copilot-service-request-id" ||
		lower == "x-azure-request-id" || lower == "openai-request-id" ||
		strings.HasPrefix(lower, "x-ratelimit-") || strings.HasPrefix(lower, "ratelimit-") ||
		strings.HasPrefix(lower, "x-quota-snapshot-") || strings.HasPrefix(lower, "x-usage-ratelimit-")
}
