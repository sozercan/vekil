package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestResponsesCompactionCannotConsumeNativeConnection(t *testing.T) {
	var sends atomic.Int32
	h := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/responses" {
			t.Errorf("compaction dispatched %s %s", r.Method, r.URL.Path)
		}
		sends.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp-summary","output":[{"type":"message","content":[{"type":"output_text","text":"summary"}]}],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}`)
	})
	h.stats = newStatsCollector()
	u := newResponsesNativeUpstream(context.Background())
	u.rememberResponse("resp-preserved")
	u.close()
	ctx := context.WithValue(context.Background(), responsesNativeRequestContextKey{}, &responsesNativeRequest{upstream: u})
	var request map[string]json.RawMessage
	_ = json.Unmarshal([]byte(`{"model":"gpt-5.4","input":"summarize"}`), &request)
	summary, resp, err := h.compactResponsesRequest(ctx, request, nil)
	if err != nil || resp != nil || summary != "summary" || sends.Load() != 1 {
		t.Fatalf("compaction result = summary:%q response:%v err:%v sends:%d", summary, resp, err, sends.Load())
	}
	if u.previousResponseID() != "resp-preserved" {
		t.Fatal("compaction changed the native session parent")
	}
	stats := h.stats.snapshot().TaskUsage
	if stats.Totals.Sends != 1 || stats.Totals.Usage.TotalTokens != 5 || len(stats.ByKind) != 1 || stats.ByKind[0].Kind != "compaction" {
		t.Fatalf("compaction task accounting = %+v", stats)
	}
}

func TestResponsesMemoryTaskAccounting(t *testing.T) {
	h := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp-memory","output":[{"type":"message","content":[{"type":"output_text","text":"[{\"trace_summary\":\"trace\",\"memory_summary\":\"memory\"}]"}]}],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}`)
	})
	h.stats = newStatsCollector()
	req := httptest.NewRequest(http.MethodPost, "/v1/memories/trace_summarize", strings.NewReader(`{"model":"gpt-5.4","traces":[{"id":"trace-one","items":[]}]}`))
	w := httptest.NewRecorder()
	h.HandleMemorySummarize(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("memory response = %d %s", w.Code, w.Body.String())
	}
	stats := h.stats.snapshot().TaskUsage
	if stats.Totals.Sends != 1 || stats.Totals.Usage.TotalTokens != 5 || len(stats.ByKind) != 1 || stats.ByKind[0].Kind != "memory" {
		t.Fatalf("memory task accounting = %+v", stats)
	}
}
