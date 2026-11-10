package proxy

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestFormatUpstreamRequestFailureDurableStorage(t *testing.T) {
	for _, cause := range []error{errDurableStateIO, errDurableStateClosed, errDurableStateCorrupt, errDurableStateCapacity} {
		want, _, _ := durableStateFailureDetails(cause)
		for _, err := range []error{cause, &providerRequestError{statusCode: 503, err: fmt.Errorf("private storage fixture: %w", cause)}} {
			if got := formatUpstreamRequestFailure(err, "upstream request failed"); got != want {
				t.Fatalf("storage failure message=%q want=%q", got, want)
			}
		}
	}
}

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
