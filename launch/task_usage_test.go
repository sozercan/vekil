package launch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSessionSummaryIncludesAuxiliaryUsage(t *testing.T) {
	const fixture = `{"inflight":0,"totals":{"requests":1,"prompt_tokens":7,"completion_tokens":3},"task_usage":{"inflight":0,"totals":{"sends":4,"errors":1,"throttled":1,"usage":{"prompt_tokens":27,"completion_tokens":11,"cached_tokens":6,"reasoning_tokens":2},"copilot_usage":{"total_nano_aiu":99,"compute_units":4}},"by_kind":[{"kind":"inference","sends":2},{"kind":"classifier","sends":1,"usage":{"prompt_tokens":10,"completion_tokens":4}},{"kind":"compaction","sends":1,"usage":{"prompt_tokens":10,"completion_tokens":4}}]}}`
	var snapshot statsSnapshot
	if err := json.Unmarshal([]byte(fixture), &snapshot); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	printSessionSummary(&output, snapshot)
	for _, want := range []string{
		"requests: 1", "tokens:   7 in, 3 out",
		"upstream: 4 sends, 1 errors, 1 throttled",
		"spent:    27 in, 11 out, 6 cached, 2 reasoning",
		"auxiliary classifier: 1 sends, 10 in, 4 out",
		"auxiliary compaction: 1 sends, 10 in, 4 out",
		"accounting: 99 nano-AIU, 4 compute units",
	} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("summary missing %q: %s", want, output.String())
		}
	}
	if strings.Contains(output.String(), "auxiliary inference") {
		t.Fatal("ordinary inference was labeled auxiliary")
	}

	snapshot.Totals = statsTotals{}
	output.Reset()
	printSessionSummary(&output, snapshot)
	if !strings.Contains(output.String(), "vekil session summary") {
		t.Fatal("auxiliary-only session lost its summary")
	}
	output.Reset()
	printSessionSummary(&output, statsSnapshot{})
	if output.Len() != 0 {
		t.Fatal("empty session printed a summary")
	}
}

func TestFetchSettledStatsWaitsForAuxiliarySends(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if calls.Add(1) < 3 {
			_, _ = fmt.Fprint(w, `{"inflight":0,"task_usage":{"inflight":1,"totals":{"sends":1}}}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"inflight":0,"task_usage":{"inflight":0,"totals":{"sends":1,"usage":{"prompt_tokens":8,"completion_tokens":2}}}}`)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	snapshot, err := fetchSettledStats(ctx, server.URL, "local-token")
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 || snapshot.TaskUsage.Inflight != 0 || snapshot.TaskUsage.Totals.Usage.PromptTokens != 8 {
		t.Fatalf("summary returned before auxiliary work settled: %+v, calls=%d", snapshot, calls.Load())
	}
}
