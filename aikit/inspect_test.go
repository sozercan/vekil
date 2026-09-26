package aikit

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRangeReaderRedactsRedirectedURLs(t *testing.T) {
	// The signed redirect target is closed, so the request fails there.
	closed := httptest.NewServer(http.NotFoundHandler())
	target := closed.URL + "/blob?X-Amz-Signature=secret-signature"
	closed.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target, http.StatusFound)
	}))
	defer source.Close()
	reader := &rangeReader{ctx: context.Background(), client: secureRedirects(source.Client()), url: source.URL + "/model.gguf"}
	_, err := reader.Read(make([]byte, 16))
	if err == nil || strings.Contains(err.Error(), "secret-signature") || !strings.Contains(err.Error(), "/blob") {
		t.Fatalf("error = %v", err)
	}
}
