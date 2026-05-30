package proxy

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSanitizeUpstreamErrorTextTruncatesToMaxRunes(t *testing.T) {
	value := strings.Repeat("界", upstreamErrorDetailMaxChars+10)

	got := sanitizeUpstreamErrorText(value)

	if !strings.HasSuffix(got, "…") {
		t.Fatalf("expected truncated message to end with ellipsis, got %q", got)
	}
	if gotRunes := utf8.RuneCountInString(got); gotRunes != upstreamErrorDetailMaxChars {
		t.Fatalf("truncated message rune count = %d, want %d", gotRunes, upstreamErrorDetailMaxChars)
	}
}
