package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"
)

const (
	upstreamErrorDetailMaxChars     = 1024
	upstreamErrorDetailMaxBodyBytes = 4096
	upstreamErrorDetailDrainBytes   = 64 * 1024
)

func formatUpstreamErrorMessage(statusCode int, body []byte) string {
	message := fmt.Sprintf("upstream error (%d)", statusCode)
	if detail := summarizeUpstreamErrorBody(body); detail != "" {
		return message + ": " + detail
	}
	return message
}

func formatUpstreamRequestFailure(err error, fallback string) string {
	var upstreamErr *upstreamError
	if errors.As(err, &upstreamErr) {
		return upstreamErr.Error()
	}
	return fallback
}

func upstreamErrorRetryMetadata(err error) (string, http.Header) {
	var upstreamErr *upstreamError
	if !errors.As(err, &upstreamErr) {
		return "", nil
	}
	return upstreamErr.retryAfter, upstreamErr.headers
}

func writeOpenAIUpstreamRequestFailure(w http.ResponseWriter, statusCode int, err error) {
	retryAfter, upstreamHeaders := upstreamErrorRetryMetadata(err)
	writeOpenAIErrorWithRetryAfter(
		w,
		statusCode,
		formatUpstreamRequestFailure(err, "upstream request failed"),
		"server_error",
		retryAfter,
		upstreamHeaders,
	)
}

func summarizeUpstreamErrorBody(body []byte) string {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return ""
	}

	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &envelope); err == nil {
		if detail := summarizeJSONErrorValue(envelope["error"]); detail != "" {
			return detail
		}
		for _, key := range []string{"message", "error_description", "detail"} {
			if detail := rawJSONErrorString(envelope[key]); detail != "" {
				return detail
			}
		}
	}

	if !utf8.Valid(trimmed) {
		return ""
	}
	return sanitizeUpstreamErrorText(string(trimmed))
}

func summarizeJSONErrorValue(raw json.RawMessage) string {
	if len(bytes.TrimSpace(raw)) == 0 {
		return ""
	}

	if message := rawJSONErrorString(raw); message != "" {
		return message
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return ""
	}

	message := ""
	for _, key := range []string{"message", "error_description", "detail"} {
		if message = rawJSONErrorString(obj[key]); message != "" {
			break
		}
	}

	attrs := make([]string, 0, 4)
	for _, key := range []string{"type", "code", "param", "status"} {
		if value := rawJSONErrorString(obj[key]); value != "" {
			attrs = append(attrs, key+"="+value)
		}
	}

	if message == "" {
		return strings.Join(attrs, ", ")
	}
	if len(attrs) == 0 {
		return message
	}
	return message + " (" + strings.Join(attrs, ", ") + ")"
}

func rawJSONErrorString(raw json.RawMessage) string {
	value := rawJSONString(raw)
	if value == "" {
		return ""
	}
	return sanitizeUpstreamErrorText(value)
}

func sanitizeUpstreamErrorText(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	if value == "" {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= upstreamErrorDetailMaxChars {
		return value
	}
	ellipsis := "…"
	ellipsisRunes := utf8.RuneCountInString(ellipsis)
	if upstreamErrorDetailMaxChars <= ellipsisRunes {
		return string(runes[:upstreamErrorDetailMaxChars])
	}
	return string(runes[:upstreamErrorDetailMaxChars-ellipsisRunes]) + ellipsis
}
