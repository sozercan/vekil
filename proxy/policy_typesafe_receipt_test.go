package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sozercan/vekil/logger"
)

const (
	policyReceiptTestGeneration = "gen_0123456789ABCDEFGHIJKLMNOP"
	policyReceiptTestPreflight  = "gen_ABCDEFGHIJKLMNOPQRSTUVWXYZ"
)

func TestPolicyTypeSafeReceiptGenerationID(t *testing.T) {
	camel := `"providerMetadata":{"gateway":{"generationId":"` + policyReceiptTestGeneration + `"}}`
	snake := `"provider_metadata":{"gateway":{"generationId":"` + policyReceiptTestGeneration + `"}}`
	for _, tc := range []struct {
		name, body, want string
	}{
		{"camel case", `{` + camel + `}`, policyReceiptTestGeneration},
		{"snake case", `{` + snake + `}`, policyReceiptTestGeneration},
		{"matching aliases", `{` + camel + `,` + snake + `}`, policyReceiptTestGeneration},
		{"conflicting aliases", `{` + camel + `,` + strings.Replace(snake, policyReceiptTestGeneration, policyReceiptTestPreflight, 1) + `}`, ""},
		{"invalid competing alias", `{` + camel + `,"provider_metadata":null}`, ""},
		{"missing", `{}`, ""},
		{"arbitrary ID", `{"id":"` + policyReceiptTestGeneration + `"}`, ""},
		{"wrong parent", `{"error":{"gateway":{"generationId":"` + policyReceiptTestGeneration + `"}}}`, ""},
		{"wrong outer case", `{` + strings.Replace(camel, "providerMetadata", "ProviderMetadata", 1) + `}`, ""},
		{"wrong gateway case", `{` + strings.Replace(camel, "gateway", "Gateway", 1) + `}`, ""},
		{"wrong ID case", `{` + strings.Replace(camel, "generationId", "generationID", 1) + `}`, ""},
		{"duplicate metadata", `{` + camel + `,` + camel + `}`, ""},
		{"duplicate gateway", `{"providerMetadata":{"gateway":{},"gateway":{"generationId":"` + policyReceiptTestGeneration + `"}}}`, ""},
		{"duplicate ID", `{"providerMetadata":{"gateway":{"generationId":"` + policyReceiptTestGeneration + `","generationId":"` + policyReceiptTestGeneration + `"}}}`, ""},
		{"numeric ID", `{"providerMetadata":{"gateway":{"generationId":123}}}`, ""},
		{"nonobject metadata", `{"providerMetadata":[]}`, ""},
		{"nonobject gateway", `{"providerMetadata":{"gateway":null}}`, ""},
		{"short ID", `{` + strings.Replace(camel, policyReceiptTestGeneration, "gen_0123456789ABCDEFGHIJKLMNO", 1) + `}`, ""},
		{"long ID", `{` + strings.Replace(camel, policyReceiptTestGeneration, policyReceiptTestGeneration+"Q", 1) + `}`, ""},
		{"punctuation", `{` + strings.Replace(camel, policyReceiptTestGeneration, "gen_0123456789ABCDEFGHIJKLMNO-", 1) + `}`, ""},
		{"nonascii", `{` + strings.Replace(camel, policyReceiptTestGeneration, "gen_0123456789ABCDEFGHIJKLMNOé", 1) + `}`, ""},
		{"whitespace", `{` + strings.Replace(camel, policyReceiptTestGeneration, " "+policyReceiptTestGeneration, 1) + `}`, ""},
		{"log injection", `{` + strings.Replace(camel, policyReceiptTestGeneration, policyReceiptTestGeneration+`\nprivate-note`, 1) + `}`, ""},
		{"trailing content", `{` + camel + `}{}`, ""},
		{"truncated", `{` + camel, ""},
		{"oversized", `{` + camel + `,"padding":"` + strings.Repeat("x", policyClassifierResponseLimit) + `"}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := policyTypeSafeGenerationID([]byte(tc.body)); got != tc.want {
				t.Fatalf("generation ID = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPolicyTypeSafeResponseReceipts(t *testing.T) {
	signals := policyClassifierSignals{TurnType: policyTurnTypeLookup, CodeScope: policyCodeScopeNone, RiskLevel: policyRiskLevelLow}
	valid := string(policyTypeSafeTestResponse(t, signals))
	withID := func(body, key, id string) string {
		return strings.TrimSuffix(body, "}") + fmt.Sprintf(`,%q:{"gateway":{"generationId":%q}},"private":"private-upstream-note"}`, key, id)
	}
	preflight := withID(valid, "providerMetadata", policyReceiptTestPreflight)
	success := withID(valid, "provider_metadata", policyReceiptTestGeneration)
	failure := withID(`{"error":{"message":"private-upstream-note"}}`, "providerMetadata", policyReceiptTestGeneration)
	for _, tc := range []struct {
		name, mode, body, wantID string
		status                   int
		reported, readError      bool
		transportError           bool
	}{
		{name: "success", body: success, wantID: policyReceiptTestGeneration, reported: true},
		{name: "observe parent correlation", mode: "observe", body: success, wantID: policyReceiptTestGeneration, reported: true},
		{name: "unavailable without usage", status: 503, body: failure, wantID: policyReceiptTestGeneration},
		{name: "HTTP error with usage", status: 503, body: success, wantID: policyReceiptTestGeneration, reported: true},
		{name: "missing ID", body: valid, reported: true},
		{name: "missing usage", body: failure, wantID: policyReceiptTestGeneration},
		{name: "zero usage", body: withID(`{"usage":{"input_tokens":0,"output_tokens":0}}`, "providerMetadata", policyReceiptTestGeneration), wantID: policyReceiptTestGeneration},
		{name: "malformed with observed usage", body: success + "!", reported: true},
		{name: "truncated HTTP body", body: success, reported: true, readError: true},
		{name: "oversized body", body: `{"usage":{"input_tokens":10,"output_tokens":4},"padding":"` + strings.Repeat("x", policyClassifierResponseLimit) + `"}`, reported: true},
		{name: "no HTTP response", transportError: true},
		{name: "off sends nothing", mode: "off", body: success},
	} {
		t.Run(tc.name, func(t *testing.T) {
			light := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
			power := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
			var calls atomic.Int32
			evaluator := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if calls.Add(1) == 1 {
					_, _ = io.WriteString(w, preflight)
					return
				}
				if tc.transportError {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					_ = conn.Close()
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-Request-ID", policyReceiptTestPreflight)
				w.Header().Set("X-Private", "private-header-note")
				if tc.readError {
					w.Header().Set("Content-Length", fmt.Sprint(len(tc.body)+100))
				}
				if tc.status != 0 {
					w.WriteHeader(tc.status)
				}
				_, _ = io.WriteString(w, tc.body)
			}))
			t.Cleanup(evaluator.Close)
			mode := tc.mode
			if mode == "" {
				mode = "enforce"
			}
			cfg := policyTypeSafeTestConfig(light.server.URL, power.server.URL, evaluator.URL, mode)
			cfg.PolicyProfiles[0].ClassifierUnavailableTier = policyConfigTierPowerful
			var logs bytes.Buffer
			h, err := NewProxyHandler(nil, logger.NewWithWriter(logger.LevelInfo, &logs), WithProvidersConfig(cfg))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(h.BeginShutdown)
			if err := h.InitializePolicyRouting(t.Context()); err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"coding-economy","messages":[{"role":"user","content":"private-user-note"}]}`))
			request.Header.Set("Authorization", "Bearer private-client-note")
			ctx, summary := WithRequestSummary(t.Context())
			recorder := httptest.NewRecorder()
			h.HandleOpenAIChatCompletions(recorder, request.WithContext(ctx))
			if recorder.Code != http.StatusOK {
				t.Fatalf("terminal HTTP status = %d", recorder.Code)
			}
			waitCtx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			if err := h.WaitLifecycleWorkers(waitCtx); err != nil {
				t.Fatal(err)
			}
			receipts := policyReceiptTestLogs(t, &logs)
			if mode == "off" {
				if calls.Load() != 0 || len(receipts) != 0 {
					t.Fatalf("off: calls=%d receipts=%d", calls.Load(), len(receipts))
				}
				return
			}
			wantReceipts := 2
			if tc.transportError {
				wantReceipts = 1
			}
			if calls.Load() != 2 || len(receipts) != wantReceipts {
				t.Fatalf("calls=%d receipts=%d, want 2 calls and %d receipts", calls.Load(), len(receipts), wantReceipts)
			}
			first := receipts[0]
			if first["traffic_bucket"] != "preflight" || first["operation_id"] != nil || first["generation_id"] != policyReceiptTestPreflight || first["reported_usage"] != true || first["status_code"] != float64(200) {
				t.Fatalf("preflight receipt = %v", first)
			}
			if !tc.transportError {
				receipt := receipts[1]
				status := tc.status
				if status == 0 {
					status = http.StatusOK
				}
				if receipt["operation_id"] == "" || receipt["operation_id"] != summary.OperationID() || receipt["traffic_bucket"] != "request" || receipt["status_code"] != float64(status) || receipt["reported_usage"] != tc.reported {
					t.Fatalf("request receipt = %v, parent operation = %q", receipt, summary.OperationID())
				}
				if tc.wantID == "" {
					if _, present := receipt["generation_id"]; present {
						t.Fatal("unavailable generation ID must be omitted")
					}
				} else if receipt["generation_id"] != tc.wantID {
					t.Fatalf("request generation ID = %v", receipt["generation_id"])
				}
			}
			var reported int64
			for _, receipt := range receipts {
				if receipt["policy_id"] != "coding-policy" || receipt["model"] != "test-evaluator" {
					t.Fatalf("configured identities = %v", receipt)
				}
				if receipt["reported_usage"] == true {
					reported++
				}
			}
			var ledger taskUsageTotals
			for _, row := range h.stats.taskUsage.snapshot().ByKind {
				if row.Kind == "classifier" {
					ledger = row.taskUsageTotals
				}
			}
			if ledger.Sends != 2 || ledger.Completed != 2 || ledger.ReportedUsageSends != reported {
				t.Fatalf("receipt accounting mismatch: reported=%d ledger=%+v", reported, ledger)
			}
			lightCalls, _ := light.snapshot()
			powerCalls, _ := power.snapshot()
			if lightCalls+powerCalls != 1 {
				t.Fatalf("terminal calls = %d", lightCalls+powerCalls)
			}
		})
	}
}

func TestPolicyTypeSafePreflightFailureReceipt(t *testing.T) {
	var logs bytes.Buffer
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = fmt.Fprintf(w, `{"providerMetadata":{"gateway":{"generationId":%q}}}`, policyReceiptTestPreflight)
	}))
	defer server.Close()
	h, err := NewProxyHandler(nil, logger.NewWithWriter(logger.LevelInfo, &logs), WithProvidersConfig(policyTypeSafeTestConfig(server.URL, server.URL, server.URL, "enforce")))
	if err != nil {
		t.Fatal(err)
	}
	defer h.BeginShutdown()
	if err := h.InitializePolicyRouting(t.Context()); err == nil {
		t.Fatal("failed preflight was accepted")
	}
	receipts := policyReceiptTestLogs(t, &logs)
	if len(receipts) != 1 || receipts[0]["traffic_bucket"] != "preflight" || receipts[0]["operation_id"] != nil || receipts[0]["reported_usage"] != false || receipts[0]["status_code"] != float64(503) || receipts[0]["generation_id"] != policyReceiptTestPreflight {
		t.Fatalf("preflight failure receipts = %v", receipts)
	}
}

func policyReceiptTestLogs(t *testing.T, logs *bytes.Buffer) []map[string]any {
	t.Helper()
	var receipts []map[string]any
	decoder := json.NewDecoder(bytes.NewReader(logs.Bytes()))
	for decoder.More() {
		var record map[string]any
		if err := decoder.Decode(&record); err != nil {
			t.Fatal(err)
		}
		if record["msg"] != "policy classifier request completed" {
			continue
		}
		for key := range record {
			switch key {
			case "msg", "time", "level", "policy_id", "model", "traffic_bucket", "status_code", "reported_usage", "operation_id", "generation_id":
			default:
				t.Fatalf("unexpected receipt field %q", key)
			}
		}
		encoded, _ := json.Marshal(record)
		if bytes.Contains(encoded, []byte("private-")) {
			t.Fatal("receipt leaked private request, response, or header text")
		}
		receipts = append(receipts, record)
	}
	return receipts
}
