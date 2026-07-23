package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testBoolPtr(value bool) *bool { return &value }

func testOpenAICodexPoolConfig(t testing.TB, strategy string, accounts ...struct {
	id        string
	accountID string
	fedRAMP   bool
}) *OpenAICodexAccountsConfig {
	t.Helper()
	enabled := true
	config := &OpenAICodexAccountsConfig{
		Strategy:           strategy,
		SessionAffinity:    &enabled,
		SessionAffinityTTL: time.Hour.String(),
	}
	for _, account := range accounts {
		home := t.TempDir()
		path := writeTestOpenAICodexAuth(t, home, testOpenAICodexTokens(t, time.Now().Add(time.Hour), account.accountID, account.fedRAMP, "refresh-"+account.id))
		config.Accounts = append(config.Accounts, OpenAICodexAccountConfig{ID: account.id, AuthFile: path})
	}
	return config
}

func testOpenAICodexCatalogBody(models ...string) string {
	entries := make([]map[string]any, 0, len(models))
	for _, model := range models {
		entries = append(entries, map[string]any{
			"slug":                         model,
			"display_name":                 model,
			"visibility":                   "list",
			"supported_in_api":             true,
			"priority":                     1,
			"supported_reasoning_levels":   []map[string]any{{"effort": "low"}, {"effort": "high"}},
			"supports_parallel_tool_calls": true,
			"context_window":               200000,
		})
	}
	body, _ := json.Marshal(map[string]any{"models": entries})
	return string(body)
}

func TestOpenAICodexAccountPoolRejectsDuplicateUnderlyingAccounts(t *testing.T) {
	config := testOpenAICodexPoolConfig(t, openAICodexAccountStrategyFillFirst,
		struct {
			id        string
			accountID string
			fedRAMP   bool
		}{"personal", "acct-same", false},
		struct {
			id        string
			accountID string
			fedRAMP   bool
		}{"work", "acct-same", false},
	)
	pool, err := newOpenAICodexAccountPool("codex", config)
	if err != nil {
		t.Fatalf("newOpenAICodexAccountPool() error = %v", err)
	}
	if err := pool.initialize(context.Background(), http.DefaultClient); err == nil || !strings.Contains(err.Error(), "same ChatGPT account") {
		t.Fatalf("initialize() error = %v, want duplicate account rejection", err)
	}
}

func TestOpenAICodexAccountPoolRejectsMixedFedRAMP(t *testing.T) {
	config := testOpenAICodexPoolConfig(t, openAICodexAccountStrategyFillFirst,
		struct {
			id        string
			accountID string
			fedRAMP   bool
		}{"commercial", "acct-commercial", false},
		struct {
			id        string
			accountID string
			fedRAMP   bool
		}{"fed", "acct-fed", true},
	)
	pool, err := newOpenAICodexAccountPool("codex", config)
	if err != nil {
		t.Fatalf("newOpenAICodexAccountPool() error = %v", err)
	}
	if err := pool.initialize(context.Background(), http.DefaultClient); err == nil || !strings.Contains(err.Error(), "cannot mix FedRAMP") {
		t.Fatalf("initialize() error = %v, want mixed FedRAMP rejection", err)
	}
}

func TestOpenAICodexAccountPoolRoundRobinIsPerModelAndConcurrentSafe(t *testing.T) {
	config := testOpenAICodexPoolConfig(t, openAICodexAccountStrategyRoundRobin,
		struct {
			id        string
			accountID string
			fedRAMP   bool
		}{"a", "acct-a", false},
		struct {
			id        string
			accountID string
			fedRAMP   bool
		}{"b", "acct-b", false},
		struct {
			id        string
			accountID string
			fedRAMP   bool
		}{"c", "acct-c", false},
	)
	pool, err := newOpenAICodexAccountPool("codex", config)
	if err != nil {
		t.Fatalf("newOpenAICodexAccountPool() error = %v", err)
	}
	if got := pool.candidateOrder("gpt-a", [32]byte{}, false); got[0] != "a" {
		t.Fatalf("first gpt-a candidate = %q, want a", got[0])
	}
	if got := pool.candidateOrder("gpt-a", [32]byte{}, false); got[0] != "b" {
		t.Fatalf("second gpt-a candidate = %q, want b", got[0])
	}
	if got := pool.candidateOrder("gpt-b", [32]byte{}, false); got[0] != "a" {
		t.Fatalf("first gpt-b candidate = %q, want independent a", got[0])
	}

	const workers = 60
	counts := make(map[string]int)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			order := pool.candidateOrder("gpt-concurrent", [32]byte{}, false)
			mu.Lock()
			counts[order[0]]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	for _, id := range []string{"a", "b", "c"} {
		if counts[id] != workers/3 {
			t.Fatalf("concurrent count[%s] = %d, want %d", id, counts[id], workers/3)
		}
	}
}

func TestMergeOpenAICodexProviderModelsIsConservative(t *testing.T) {
	left := providerModel{publicID: "gpt", providerID: "codex", parallelToolCalls: testBoolPtr(true), raw: json.RawMessage(`{
		"id":"gpt","context_window":200000,"supports_reasoning_summaries":true,
		"capabilities":{"limits":{"max_context_window_tokens":200000},"supports":{"parallel_tool_calls":true,"vision":true,"reasoning_effort":["low","medium","high"]}}
	}`)}
	right := providerModel{publicID: "gpt", providerID: "codex", parallelToolCalls: testBoolPtr(false), raw: json.RawMessage(`{
		"id":"gpt","context_window":100000,"supports_reasoning_summaries":false,
		"capabilities":{"limits":{"max_context_window_tokens":100000},"supports":{"parallel_tool_calls":false,"vision":false,"reasoning_effort":["low","high"]}}
	}`)}
	merged := mergeOpenAICodexProviderModels([]providerModel{left, right})
	if merged.parallelToolCalls == nil || *merged.parallelToolCalls {
		t.Fatalf("parallel tool calls = %v, want false", merged.parallelToolCalls)
	}
	var raw map[string]any
	if err := json.Unmarshal(merged.raw, &raw); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if got := raw["context_window"]; got != float64(100000) {
		t.Fatalf("context_window = %#v, want 100000", got)
	}
	caps := raw["capabilities"].(map[string]any)
	supports := caps["supports"].(map[string]any)
	efforts := supports["reasoning_effort"].([]any)
	if fmt.Sprint(efforts) != "[high low]" {
		t.Fatalf("reasoning effort intersection = %v, want [high low]", efforts)
	}
	if supports["parallel_tool_calls"] != false || supports["vision"] != false {
		t.Fatalf("boolean capabilities = %#v, want conservative false", supports)
	}
}

func TestOpenAICodexAccountPoolCatalogUnionUsesIndependentAccountsAndETags(t *testing.T) {
	config := testOpenAICodexPoolConfig(t, openAICodexAccountStrategyFillFirst,
		struct {
			id        string
			accountID string
			fedRAMP   bool
		}{"personal", "acct-personal", false},
		struct {
			id        string
			accountID string
			fedRAMP   bool
		}{"work", "acct-work", false},
	)
	var personalConditional atomic.Bool
	var workConditional atomic.Bool
	h := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Fatalf("path = %q, want /models", r.URL.Path)
		}
		accountID := r.Header.Get("ChatGPT-Account-ID")
		switch accountID {
		case "acct-personal":
			if r.Header.Get("If-None-Match") == `"personal-v1"` {
				personalConditional.Store(true)
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", `"personal-v1"`)
			_, _ = io.WriteString(w, testOpenAICodexCatalogBody("gpt-personal", "gpt-shared"))
		case "acct-work":
			if r.Header.Get("If-None-Match") == `"work-v1"` {
				workConditional.Store(true)
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", `"work-v1"`)
			_, _ = io.WriteString(w, testOpenAICodexCatalogBody("gpt-work", "gpt-shared"))
		default:
			t.Fatalf("unexpected account header %q", accountID)
		}
	})
	provider, err := buildProviderRuntime(ProviderConfig{ID: "codex", Type: string(providerTypeOpenAICodex), BaseURL: h.copilotURL, CodexAccounts: config}, h.copilotURL, nil)
	if err != nil {
		t.Fatalf("buildProviderRuntime() error = %v", err)
	}
	first, err := h.fetchProviderModels(context.Background(), provider, "", "")
	if err != nil {
		t.Fatalf("first fetchProviderModels() error = %v", err)
	}
	got := make([]string, 0, len(first.models))
	for _, model := range first.models {
		got = append(got, model.publicID)
	}
	if fmt.Sprint(got) != "[gpt-personal gpt-shared gpt-work]" {
		t.Fatalf("union models = %v", got)
	}
	if first.etag == "" {
		t.Fatal("synthetic ETag is empty")
	}
	second, err := h.fetchProviderModels(context.Background(), provider, "", first.etag)
	if err != nil {
		t.Fatalf("second fetchProviderModels() error = %v", err)
	}
	if !second.notModified || second.etag != first.etag {
		t.Fatalf("second result = %#v, want not modified with stable ETag", second)
	}
	if !personalConditional.Load() || !workConditional.Load() {
		t.Fatalf("independent member ETags were not sent: personal=%v work=%v", personalConditional.Load(), workConditional.Load())
	}
}

func TestOpenAICodexAccountPoolManagedRouteFailsOverOnQuota(t *testing.T) {
	config := testOpenAICodexPoolConfig(t, openAICodexAccountStrategyFillFirst,
		struct {
			id        string
			accountID string
			fedRAMP   bool
		}{"personal", "acct-personal", false},
		struct {
			id        string
			accountID string
			fedRAMP   bool
		}{"work", "acct-work", false},
	)
	config.SessionAffinity = testBoolPtr(false)
	var inferenceAccounts []string
	var mu sync.Mutex
	h := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		accountID := r.Header.Get("ChatGPT-Account-ID")
		if r.URL.Path == "/models" {
			_, _ = io.WriteString(w, testOpenAICodexCatalogBody("gpt-pool"))
			return
		}
		if r.URL.Path != "/responses" {
			t.Fatalf("path = %q, want /responses", r.URL.Path)
		}
		mu.Lock()
		inferenceAccounts = append(inferenceAccounts, accountID)
		mu.Unlock()
		if accountID == "acct-personal" {
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"code":"rate_limit_exceeded","message":"quota"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Codex-Turn-State", "turn-state-secret")
		_, _ = io.WriteString(w, `{"id":"resp_pool","model":"gpt-pool","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
	})
	h.stats = newStatsCollector()
	cfg := ProvidersConfig{SchemaVersion: ProvidersConfigSchemaVersion2, Providers: []ProviderConfig{{
		ID: "codex", Type: string(providerTypeOpenAICodex), Default: true, BaseURL: h.copilotURL, CodexAccounts: config,
	}}}
	setup, err := h.buildConfiguredProviderSetupWithDynamicValidation(context.Background(), cfg, true)
	if err != nil {
		t.Fatalf("buildConfiguredProviderSetupWithDynamicValidation() error = %v", err)
	}
	h.providersState = setup
	route, known := h.resolveModelRouteForRequest("gpt-pool", providerEndpointResponses)
	if !known || route == nil || !route.legacy || !route.usesManagedExecution() {
		t.Fatalf("pooled catalog route = %#v, want provider-catalog origin with managed execution", route)
	}

	ctx, summary := WithRequestSummary(context.Background())
	body := []byte(`{"model":"gpt-pool","input":"hello","stream":false}`)
	ctx, operation, _, err := h.withExplicitRouteOperation(ctx, ctx, "gpt-pool", providerEndpointResponses)
	if err != nil {
		t.Fatalf("withExplicitRouteOperation() error = %v", err)
	}
	resp, err := h.postResponsesWithHeadersForModel(ctx, body, http.Header{"Session_id": []string{"session-secret"}}, "gpt-pool")
	if err != nil {
		t.Fatalf("postResponsesWithHeadersForModel() error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	mu.Lock()
	gotAccounts := append([]string(nil), inferenceAccounts...)
	mu.Unlock()
	if fmt.Sprint(gotAccounts) != "[acct-personal acct-work]" {
		t.Fatalf("inference accounts = %v, want quota failover to work", gotAccounts)
	}
	if sends, targetSwitches, _ := operation.snapshot(); sends != 2 || targetSwitches != 0 {
		t.Fatalf("operation sends/switches = %d/%d, want 2/0", sends, targetSwitches)
	}
	if summary.TargetSwitchCount() != 0 {
		t.Fatalf("target switch count = %d, account failover must not inflate it", summary.TargetSwitchCount())
	}
	fields := fmt.Sprint(summary.LoggerFields())
	if !strings.Contains(fields, "work") || !strings.Contains(fields, "account_switches") {
		t.Fatalf("summary fields = %s, want final access member and account switch", fields)
	}
	snapshotJSON, _ := json.Marshal(h.stats.snapshot())
	if !strings.Contains(string(snapshotJSON), `"access_member_id":"work"`) || !strings.Contains(string(snapshotJSON), `"selection_reason":"account_failover"`) {
		t.Fatalf("stats snapshot lacks sanitized pool failover attribution: %s", snapshotJSON)
	}
	provider := setup.providerByID("codex")
	credentials, credentialErr := provider.codexPool.members[0].auth.credentials(context.Background(), h.client)
	if credentialErr != nil {
		t.Fatal(credentialErr)
	}
	for _, forbidden := range []string{"acct-personal", "acct-work", config.Accounts[0].AuthFile, config.Accounts[1].AuthFile, credentials.accessToken, "session-secret", "turn-state-secret", "cred_codex_"} {
		if strings.Contains(string(snapshotJSON), forbidden) {
			t.Fatalf("stats snapshot leaked %q: %s", forbidden, snapshotJSON)
		}
	}
}

func TestOpenAICodexAccountPoolStatePinsCredentialAndRejectsStaleGeneration(t *testing.T) {
	config := testOpenAICodexPoolConfig(t, openAICodexAccountStrategyRoundRobin,
		struct {
			id        string
			accountID string
			fedRAMP   bool
		}{"personal", "acct-personal", false},
		struct {
			id        string
			accountID string
			fedRAMP   bool
		}{"work", "acct-work", false},
	)
	config.SessionAffinity = testBoolPtr(false)
	var accounts []string
	var mu sync.Mutex
	h := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		accountID := r.Header.Get("ChatGPT-Account-ID")
		if r.URL.Path == "/models" {
			_, _ = io.WriteString(w, testOpenAICodexCatalogBody("gpt-state"))
			return
		}
		mu.Lock()
		accounts = append(accounts, accountID)
		index := len(accounts)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"resp_%d","model":"gpt-state","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`, index)
	})
	cfg := ProvidersConfig{SchemaVersion: ProvidersConfigSchemaVersion2, Providers: []ProviderConfig{{
		ID: "codex", Type: string(providerTypeOpenAICodex), Default: true, BaseURL: h.copilotURL, CodexAccounts: config,
	}}}
	setup, err := h.buildConfiguredProviderSetupWithDynamicValidation(context.Background(), cfg, true)
	if err != nil {
		t.Fatalf("build setup error = %v", err)
	}
	h.providersState = setup

	serve := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
		rec := httptest.NewRecorder()
		h.HandleResponses(rec, req)
		return rec
	}
	first := serve(`{"model":"gpt-state","input":"first","stream":false}`)
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d body=%s", first.Code, first.Body.String())
	}
	second := serve(`{"model":"gpt-state","previous_response_id":"resp_1","input":"second","stream":false}`)
	if second.Code != http.StatusOK {
		t.Fatalf("second status = %d body=%s", second.Code, second.Body.String())
	}
	mu.Lock()
	got := append([]string(nil), accounts...)
	mu.Unlock()
	if fmt.Sprint(got) != "[acct-personal acct-personal]" {
		t.Fatalf("state-pinned accounts = %v, want same credential despite round robin", got)
	}

	personalPath := config.Accounts[0].AuthFile
	writeTestOpenAICodexAuth(t, filepath.Dir(personalPath), testOpenAICodexTokens(t, time.Now().Add(time.Hour), "acct-replaced", false, "refresh-replaced"))
	before := len(got)
	stale := serve(`{"model":"gpt-state","previous_response_id":"resp_1","input":"stale","stream":false}`)
	if stale.Code != http.StatusBadRequest {
		t.Fatalf("stale status = %d body=%s, want local 400", stale.Code, stale.Body.String())
	}
	mu.Lock()
	after := len(accounts)
	mu.Unlock()
	if after != before {
		t.Fatalf("stale generation dispatched upstream: before=%d after=%d", before, after)
	}
	if strings.Contains(stale.Body.String(), personalPath) || strings.Contains(stale.Body.String(), "acct-personal") {
		t.Fatalf("stale error leaked sensitive identity/path: %s", stale.Body.String())
	}
}

func TestOpenAICodexAccountPoolDoesNotSwitchOnGenericForbidden(t *testing.T) {
	config := testOpenAICodexPoolConfig(t, openAICodexAccountStrategyFillFirst,
		struct {
			id        string
			accountID string
			fedRAMP   bool
		}{"personal", "acct-personal", false},
		struct {
			id        string
			accountID string
			fedRAMP   bool
		}{"work", "acct-work", false},
	)
	config.SessionAffinity = testBoolPtr(false)
	var calls atomic.Int64
	h := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			_, _ = io.WriteString(w, testOpenAICodexCatalogBody("gpt-forbidden"))
			return
		}
		calls.Add(1)
		if accountID := r.Header.Get("ChatGPT-Account-ID"); accountID != "acct-personal" {
			t.Fatalf("generic forbidden switched to %q", accountID)
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"code":"forbidden","message":"policy denied for acct-personal"}}`)
	})
	cfg := ProvidersConfig{SchemaVersion: ProvidersConfigSchemaVersion2, Providers: []ProviderConfig{{ID: "codex", Type: string(providerTypeOpenAICodex), Default: true, BaseURL: h.copilotURL, CodexAccounts: config}}}
	setup, err := h.buildConfiguredProviderSetupWithDynamicValidation(context.Background(), cfg, true)
	if err != nil {
		t.Fatalf("build setup error = %v", err)
	}
	h.providersState = setup
	ctx, _ := WithRequestSummary(context.Background())
	ctx, _, _, err = h.withExplicitRouteOperation(ctx, ctx, "gpt-forbidden", providerEndpointResponses)
	if err != nil {
		t.Fatalf("with route error = %v", err)
	}
	resp, err := h.postResponsesWithHeadersForModel(ctx, []byte(`{"model":"gpt-forbidden","input":"hello"}`), nil, "gpt-forbidden")
	if err != nil {
		t.Fatalf("post error = %v", err)
	}
	defer resp.Body.Close()
	responseBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusForbidden || calls.Load() != 1 {
		t.Fatalf("status/calls = %d/%d, want 403/1", resp.StatusCode, calls.Load())
	}
	if strings.Contains(string(responseBody), "acct-personal") {
		t.Fatalf("forbidden response leaked raw account id: %s", responseBody)
	}
}

func testOpenAICodexPoolOperation(model string) *routeOperation {
	return newRouteOperation(&modelRoute{
		public: publicModelContract{id: model, routeID: "route-" + model, endpoints: []string{providerEndpointResponses}},
		policy: routePolicy{mode: routeModePriorityFailover, maxTargetAttempts: 4, maxUpstreamSends: 4},
	}, context.Background())
}

func seedOpenAICodexPoolCatalog(pool *openAICodexAccountPool, model string) {
	pool.mu.Lock()
	for _, member := range pool.members {
		member.catalog = []providerModel{{publicID: model, upstreamModel: model, providerID: pool.providerID, supportedEndpoints: []string{providerEndpointResponses}}}
		member.catalogKnown = true
		member.catalogCredentialID = member.credentialID
		member.catalogSourceDigest = member.sourceDigest
	}
	pool.publishSnapshotLocked()
	pool.mu.Unlock()
}

func TestOpenAICodexAccountPoolSessionAffinityOverridesNextRoundRobinChoice(t *testing.T) {
	config := testOpenAICodexPoolConfig(t, openAICodexAccountStrategyRoundRobin,
		struct {
			id        string
			accountID string
			fedRAMP   bool
		}{"a", "acct-a", false},
		struct {
			id        string
			accountID string
			fedRAMP   bool
		}{"b", "acct-b", false},
	)
	pool, err := newOpenAICodexAccountPool("codex", config)
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.initialize(context.Background(), http.DefaultClient); err != nil {
		t.Fatal(err)
	}
	seedOpenAICodexPoolCatalog(pool, "gpt-affinity")
	headers := http.Header{"Session_id": []string{"session-stable"}}

	first, err := pool.acquireForOperation(context.Background(), http.DefaultClient, testOpenAICodexPoolOperation("gpt-affinity"), "gpt-affinity", headers, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.accessMemberID != "a" {
		t.Fatalf("first member = %q, want a", first.accessMemberID)
	}
	pool.reportSuccess(first, "gpt-affinity")

	second, err := pool.acquireForOperation(context.Background(), http.DefaultClient, testOpenAICodexPoolOperation("gpt-affinity"), "gpt-affinity", headers, nil)
	if err != nil {
		t.Fatal(err)
	}
	if second.accessMemberID != "a" || second.selectionReason != openAICodexSelectionAffinity {
		t.Fatalf("affinity member/reason = %q/%q, want a/session_affinity", second.accessMemberID, second.selectionReason)
	}
}

func TestOpenAICodexAccountPoolCooldownSkipAndSingleHalfOpenProbe(t *testing.T) {
	config := testOpenAICodexPoolConfig(t, openAICodexAccountStrategyFillFirst,
		struct {
			id        string
			accountID string
			fedRAMP   bool
		}{"a", "acct-a", false},
		struct {
			id        string
			accountID string
			fedRAMP   bool
		}{"b", "acct-b", false},
	)
	pool, err := newOpenAICodexAccountPool("codex", config)
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.initialize(context.Background(), http.DefaultClient); err != nil {
		t.Fatal(err)
	}
	seedOpenAICodexPoolCatalog(pool, "gpt-cooldown")
	now := time.Unix(1_700_000_000, 0)
	pool.now = func() time.Time { return now }

	first, err := pool.acquireForOperation(context.Background(), http.DefaultClient, testOpenAICodexPoolOperation("gpt-cooldown"), "gpt-cooldown", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	pool.reportFailure(first, "gpt-cooldown", openAICodexAccountFailureQuota, http.Header{"Retry-After": []string{"10"}})

	skippedOperation := testOpenAICodexPoolOperation("gpt-cooldown")
	second, err := pool.acquireForOperation(context.Background(), http.DefaultClient, skippedOperation, "gpt-cooldown", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if second.accessMemberID != "b" {
		t.Fatalf("cooldown selection = %q, want b", second.accessMemberID)
	}
	skippedOperation.mu.Lock()
	attempts := skippedOperation.codexAccountAttempts
	skippedOperation.mu.Unlock()
	if attempts != 1 {
		t.Fatalf("account attempts = %d, cooling account must not consume an attempt", attempts)
	}
	pool.reportSuccess(second, "gpt-cooldown")

	now = now.Add(11 * time.Second)
	probe, err := pool.acquireForOperation(context.Background(), http.DefaultClient, testOpenAICodexPoolOperation("gpt-cooldown"), "gpt-cooldown", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if probe.accessMemberID != "a" || !probe.halfOpen {
		t.Fatalf("half-open probe = %q halfOpen=%v, want a/true", probe.accessMemberID, probe.halfOpen)
	}
	parallel, err := pool.acquireForOperation(context.Background(), http.DefaultClient, testOpenAICodexPoolOperation("gpt-cooldown"), "gpt-cooldown", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if parallel.accessMemberID != "b" {
		t.Fatalf("parallel half-open selection = %q, want b", parallel.accessMemberID)
	}
	pool.releaseLease(probe, "gpt-cooldown")
}

func newOpenAICodexPoolRouteTestHandler(t *testing.T, transport roundTripFunc) (*ProxyHandler, string) {
	t.Helper()
	config := testOpenAICodexPoolConfig(t, openAICodexAccountStrategyFillFirst,
		struct {
			id        string
			accountID string
			fedRAMP   bool
		}{"a", "acct-a", false},
		struct {
			id        string
			accountID string
			fedRAMP   bool
		}{"b", "acct-b", false},
	)
	config.SessionAffinity = testBoolPtr(false)
	h := newRoundTripTestProxyHandler(t, transport)
	provider, err := buildProviderRuntime(ProviderConfig{ID: "codex", Type: string(providerTypeOpenAICodex), BaseURL: "http://upstream.test", CodexAccounts: config}, h.copilotURL, nil)
	if err != nil {
		t.Fatalf("buildProviderRuntime() error = %v", err)
	}
	if err := provider.codexPool.initialize(context.Background(), h.client); err != nil {
		t.Fatalf("initialize() error = %v", err)
	}
	seedOpenAICodexPoolCatalog(provider.codexPool, "gpt-transport")
	routes, err := newModelRouteRegistry(nil)
	if err != nil {
		t.Fatal(err)
	}
	setup := &providerSetup{
		providers:          map[string]*providerRuntime{"codex": provider},
		routes:             routes,
		providerOrder:      []string{"codex"},
		defaultProviderID:  "codex",
		models:             make(map[string]providerModel),
		hasConfiguredState: true,
	}
	if err := setup.addProviderModels("codex", provider.codexPool.loadSnapshot().models); err != nil {
		t.Fatalf("addProviderModels() error = %v", err)
	}
	h.providersState = setup
	return h, "gpt-transport"
}

func TestOpenAICodexAccountPoolTransportReplayNeverMigratesAccounts(t *testing.T) {
	t.Run("definitely not delivered retries same account once", func(t *testing.T) {
		var accounts []string
		var mu sync.Mutex
		h, model := newOpenAICodexPoolRouteTestHandler(t, func(req *http.Request) (*http.Response, error) {
			mu.Lock()
			accounts = append(accounts, req.Header.Get("ChatGPT-Account-ID"))
			mu.Unlock()
			return nil, fmt.Errorf("dial failed")
		})
		ctx, _ := WithRequestSummary(context.Background())
		ctx, operation, _, err := h.withExplicitRouteOperation(ctx, ctx, model, providerEndpointResponses)
		if err != nil {
			t.Fatal(err)
		}
		_, err = h.postResponsesWithHeadersForModel(ctx, []byte(`{"model":"gpt-transport","input":"hello"}`), nil, model)
		if err == nil {
			t.Fatal("request error = nil, want transport failure")
		}
		mu.Lock()
		got := append([]string(nil), accounts...)
		mu.Unlock()
		if fmt.Sprint(got) != "[acct-a acct-a]" {
			t.Fatalf("transport retry accounts = %v, want same account twice and no migration", got)
		}
		if sends, _, _ := operation.snapshot(); sends != 2 {
			t.Fatalf("physical sends = %d, want 2", sends)
		}
	})

	t.Run("ambiguous delivery does not retry", func(t *testing.T) {
		var accounts []string
		h, model := newOpenAICodexPoolRouteTestHandler(t, func(req *http.Request) (*http.Response, error) {
			accounts = append(accounts, req.Header.Get("ChatGPT-Account-ID"))
			if trace := httptrace.ContextClientTrace(req.Context()); trace != nil && trace.WroteHeaders != nil {
				trace.WroteHeaders()
			}
			return nil, fmt.Errorf("connection reset after write")
		})
		ctx, _ := WithRequestSummary(context.Background())
		ctx, operation, _, err := h.withExplicitRouteOperation(ctx, ctx, model, providerEndpointResponses)
		if err != nil {
			t.Fatal(err)
		}
		_, err = h.postResponsesWithHeadersForModel(ctx, []byte(`{"model":"gpt-transport","input":"hello"}`), nil, model)
		if err == nil {
			t.Fatal("request error = nil, want ambiguous transport failure")
		}
		if fmt.Sprint(accounts) != "[acct-a]" {
			t.Fatalf("ambiguous accounts = %v, want one attempt", accounts)
		}
		if sends, _, _ := operation.snapshot(); sends != 1 {
			t.Fatalf("physical sends = %d, want 1", sends)
		}
	})
}

func TestOpenAICodexAccountPoolPersistentUnauthorizedReloadsThenSwitches(t *testing.T) {
	config := testOpenAICodexPoolConfig(t, openAICodexAccountStrategyFillFirst,
		struct {
			id        string
			accountID string
			fedRAMP   bool
		}{"a", "acct-a", false},
		struct {
			id        string
			accountID string
			fedRAMP   bool
		}{"b", "acct-b", false},
	)
	config.SessionAffinity = testBoolPtr(false)
	var accounts []string
	var mu sync.Mutex
	h := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			_, _ = io.WriteString(w, testOpenAICodexCatalogBody("gpt-auth"))
			return
		}
		accountID := r.Header.Get("ChatGPT-Account-ID")
		mu.Lock()
		accounts = append(accounts, accountID)
		mu.Unlock()
		if accountID == "acct-a" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"code":"invalid_token","message":"expired"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"resp_auth","model":"gpt-auth","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
	})
	cfg := ProvidersConfig{SchemaVersion: ProvidersConfigSchemaVersion2, Providers: []ProviderConfig{{ID: "codex", Type: string(providerTypeOpenAICodex), Default: true, BaseURL: h.copilotURL, CodexAccounts: config}}}
	setup, err := h.buildConfiguredProviderSetupWithDynamicValidation(context.Background(), cfg, true)
	if err != nil {
		t.Fatalf("build setup error = %v", err)
	}
	h.providersState = setup
	ctx, _ := WithRequestSummary(context.Background())
	ctx, operation, _, err := h.withExplicitRouteOperation(ctx, ctx, "gpt-auth", providerEndpointResponses)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := h.postResponsesWithHeadersForModel(ctx, []byte(`{"model":"gpt-auth","input":"hello"}`), nil, "gpt-auth")
	if err != nil {
		t.Fatalf("post error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	mu.Lock()
	got := append([]string(nil), accounts...)
	mu.Unlock()
	if fmt.Sprint(got) != "[acct-a acct-a acct-b]" {
		t.Fatalf("401 retry/switch accounts = %v, want reload same account then switch", got)
	}
	if sends, _, _ := operation.snapshot(); sends != 3 {
		t.Fatalf("physical sends = %d, want hard cap 3", sends)
	}
}

func TestOpenAICodexAccountPoolCoolingExhaustionReturnsEarliestRetry(t *testing.T) {
	config := testOpenAICodexPoolConfig(t, openAICodexAccountStrategyFillFirst,
		struct {
			id        string
			accountID string
			fedRAMP   bool
		}{"a", "acct-a", false},
		struct {
			id        string
			accountID string
			fedRAMP   bool
		}{"b", "acct-b", false},
	)
	config.SessionAffinity = testBoolPtr(false)
	h := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			_, _ = io.WriteString(w, testOpenAICodexCatalogBody("gpt-cooling"))
			return
		}
		retry := "10"
		if r.Header.Get("ChatGPT-Account-ID") == "acct-b" {
			retry = "5"
		}
		w.Header().Set("Retry-After", retry)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"code":"rate_limit_exceeded","message":"quota"}}`)
	})
	cfg := ProvidersConfig{SchemaVersion: ProvidersConfigSchemaVersion2, Providers: []ProviderConfig{{ID: "codex", Type: string(providerTypeOpenAICodex), Default: true, BaseURL: h.copilotURL, CodexAccounts: config}}}
	setup, err := h.buildConfiguredProviderSetupWithDynamicValidation(context.Background(), cfg, true)
	if err != nil {
		t.Fatalf("build setup error = %v", err)
	}
	h.providersState = setup
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-cooling","input":"hello"}`))
	rec := httptest.NewRecorder()
	h.HandleResponses(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d body=%s, want 429", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Retry-After"); got != "5" {
		t.Fatalf("Retry-After = %q, want earliest 5", got)
	}
}

func TestOpenAICodexAccountPoolRoutesOnlyToAdvertisingAccount(t *testing.T) {
	config := testOpenAICodexPoolConfig(t, openAICodexAccountStrategyFillFirst,
		struct {
			id        string
			accountID string
			fedRAMP   bool
		}{"a", "acct-a", false},
		struct {
			id        string
			accountID string
			fedRAMP   bool
		}{"b", "acct-b", false},
	)
	config.SessionAffinity = testBoolPtr(false)
	var inferenceAccount string
	h := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		accountID := r.Header.Get("ChatGPT-Account-ID")
		if r.URL.Path == "/models" {
			model := "gpt-a"
			if accountID == "acct-b" {
				model = "gpt-b"
			}
			_, _ = io.WriteString(w, testOpenAICodexCatalogBody(model))
			return
		}
		inferenceAccount = accountID
		_, _ = io.WriteString(w, `{"id":"resp_model","model":"gpt-b","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
	})
	cfg := ProvidersConfig{SchemaVersion: ProvidersConfigSchemaVersion2, Providers: []ProviderConfig{{ID: "codex", Type: string(providerTypeOpenAICodex), Default: true, BaseURL: h.copilotURL, CodexAccounts: config}}}
	setup, err := h.buildConfiguredProviderSetupWithDynamicValidation(context.Background(), cfg, true)
	if err != nil {
		t.Fatalf("build setup error = %v", err)
	}
	h.providersState = setup
	ctx, _ := WithRequestSummary(context.Background())
	ctx, _, _, err = h.withExplicitRouteOperation(ctx, ctx, "gpt-b", providerEndpointResponses)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := h.postResponsesWithHeadersForModel(ctx, []byte(`{"model":"gpt-b","input":"hello"}`), nil, "gpt-b")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if inferenceAccount != "acct-b" {
		t.Fatalf("inference account = %q, want only advertising acct-b", inferenceAccount)
	}
}

func TestOpenAICodexAccountPoolRetainsStaleMemberCatalogOnTemporaryFailure(t *testing.T) {
	config := testOpenAICodexPoolConfig(t, openAICodexAccountStrategyFillFirst,
		struct {
			id        string
			accountID string
			fedRAMP   bool
		}{"a", "acct-a", false},
		struct {
			id        string
			accountID string
			fedRAMP   bool
		}{"b", "acct-b", false},
	)
	var failA atomic.Bool
	h := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		accountID := r.Header.Get("ChatGPT-Account-ID")
		if accountID == "acct-a" && failA.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":{"message":"temporary"}}`)
			return
		}
		etag := `"a-v1"`
		model := "gpt-a"
		if accountID == "acct-b" {
			etag = `"b-v1"`
			model = "gpt-b"
		}
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		_, _ = io.WriteString(w, testOpenAICodexCatalogBody(model))
	})
	provider, err := buildProviderRuntime(ProviderConfig{ID: "codex", Type: string(providerTypeOpenAICodex), BaseURL: h.copilotURL, CodexAccounts: config}, h.copilotURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.fetchProviderModels(context.Background(), provider, "", ""); err != nil {
		t.Fatal(err)
	}
	failA.Store(true)
	result, err := h.fetchProviderModels(context.Background(), provider, "", "")
	if err != nil {
		t.Fatalf("refresh with one temporary failure error = %v", err)
	}
	models := make([]string, 0, len(result.models))
	for _, model := range result.models {
		models = append(models, model.publicID)
	}
	if fmt.Sprint(models) != "[gpt-a gpt-b]" {
		t.Fatalf("stale union models = %v, want both catalogs", models)
	}
}

func TestOpenAICodexAccountPoolQueryVariantDoesNotReplaceCanonicalCatalog(t *testing.T) {
	config := testOpenAICodexPoolConfig(t, openAICodexAccountStrategyFillFirst,
		struct {
			id        string
			accountID string
			fedRAMP   bool
		}{"a", "acct-a", false},
	)
	h := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		model := "gpt-canonical"
		if r.URL.Query().Get("view") == "variant" {
			model = "gpt-variant"
		}
		_, _ = io.WriteString(w, testOpenAICodexCatalogBody(model))
	})
	provider, err := buildProviderRuntime(ProviderConfig{ID: "codex", Type: string(providerTypeOpenAICodex), BaseURL: h.copilotURL, CodexAccounts: config}, h.copilotURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := h.fetchProviderModels(context.Background(), provider, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if canonical.models[0].publicID != "gpt-canonical" {
		t.Fatalf("canonical model = %q", canonical.models[0].publicID)
	}
	variant, err := h.fetchProviderModels(context.Background(), provider, "view=variant", "")
	if err != nil {
		t.Fatal(err)
	}
	if variant.models[0].publicID != "gpt-variant" {
		t.Fatalf("variant model = %q", variant.models[0].publicID)
	}
	snapshot := provider.codexPool.loadSnapshot()
	if len(snapshot.models) != 1 || snapshot.models[0].publicID != "gpt-canonical" {
		t.Fatalf("canonical snapshot replaced by query variant: %#v", snapshot.models)
	}
}

func TestOpenAICodexAccountPoolLocalAuthFailureDoesNotConsumeAttemptBudget(t *testing.T) {
	config := testOpenAICodexPoolConfig(t, openAICodexAccountStrategyFillFirst,
		struct {
			id        string
			accountID string
			fedRAMP   bool
		}{"a", "acct-a", false},
		struct {
			id        string
			accountID string
			fedRAMP   bool
		}{"b", "acct-b", false},
	)
	config.MaxAccountAttempts = 1
	pool, err := newOpenAICodexAccountPool("codex", config)
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.initialize(context.Background(), http.DefaultClient); err != nil {
		t.Fatal(err)
	}
	seedOpenAICodexPoolCatalog(pool, "gpt-auth-skip")
	if err := os.WriteFile(config.Accounts[0].AuthFile, []byte(`{"broken":`), 0o600); err != nil {
		t.Fatal(err)
	}
	operation := testOpenAICodexPoolOperation("gpt-auth-skip")
	lease, err := pool.acquireForOperation(context.Background(), http.DefaultClient, operation, "gpt-auth-skip", nil, nil)
	if err != nil {
		t.Fatalf("acquireForOperation() error = %v", err)
	}
	if lease.accessMemberID != "b" {
		t.Fatalf("selected member = %q, want healthy b", lease.accessMemberID)
	}
	operation.mu.Lock()
	attempts := operation.codexAccountAttempts
	operation.mu.Unlock()
	if attempts != 1 {
		t.Fatalf("budgeted account attempts = %d, want one dispatched eligible account", attempts)
	}
}

func TestRouteOperationInitializesCodexCandidatesOnceConcurrently(t *testing.T) {
	config := testOpenAICodexPoolConfig(t, openAICodexAccountStrategyRoundRobin,
		struct {
			id        string
			accountID string
			fedRAMP   bool
		}{"a", "acct-a", false},
		struct {
			id        string
			accountID string
			fedRAMP   bool
		}{"b", "acct-b", false},
	)
	pool, err := newOpenAICodexAccountPool("codex", config)
	if err != nil {
		t.Fatal(err)
	}
	operation := testOpenAICodexPoolOperation("gpt-once")
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			operation.initializeCodexCandidates(pool, "gpt-once", http.Header{"Session_id": []string{"same"}}, nil)
		}()
	}
	wg.Wait()
	pool.mu.Lock()
	cursor := pool.cursor["gpt-once"]
	pool.mu.Unlock()
	if cursor != 1 {
		t.Fatalf("round-robin cursor = %d, want one initialization", cursor)
	}
	operation.mu.Lock()
	order := append([]string(nil), operation.codexCandidateOrder...)
	operation.mu.Unlock()
	if fmt.Sprint(order) != "[a b]" {
		t.Fatalf("candidate order = %v, want [a b]", order)
	}
}

func TestOpenAICodexAccountPoolDiscardsCatalogFromReplacedCredentialGeneration(t *testing.T) {
	config := testOpenAICodexPoolConfig(t, openAICodexAccountStrategyFillFirst,
		struct {
			id        string
			accountID string
			fedRAMP   bool
		}{"a", "acct-a", false},
	)
	started := make(chan struct{})
	release := make(chan struct{})
	h := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		_, _ = io.WriteString(w, testOpenAICodexCatalogBody("gpt-a-only"))
	})
	provider, err := buildProviderRuntime(ProviderConfig{ID: "codex", Type: string(providerTypeOpenAICodex), BaseURL: h.copilotURL, CodexAccounts: config}, h.copilotURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resultCh := make(chan error, 1)
	go func() {
		_, fetchErr := h.fetchProviderModels(context.Background(), provider, "", "")
		resultCh <- fetchErr
	}()
	<-started
	writeTestOpenAICodexAuth(t, filepath.Dir(config.Accounts[0].AuthFile), testOpenAICodexTokens(t, time.Now().Add(time.Hour), "acct-b", false, "refresh-b"))
	member := provider.codexPool.members[0]
	if _, _, err := provider.codexPool.credentialsForMember(context.Background(), h.client, member); err != nil {
		t.Fatalf("load replacement credentials error = %v", err)
	}
	close(release)
	if err := <-resultCh; err == nil {
		t.Fatal("catalog fetch error = nil, want stale generation discarded")
	}
	snapshot := provider.codexPool.loadSnapshot()
	for _, model := range snapshot.models {
		if model.publicID == "gpt-a-only" {
			t.Fatalf("old account catalog was published under replacement generation: %#v", snapshot.models)
		}
	}
}

func TestOpenAICodexAccountPoolDiscardsStaleInFlightHealthOutcome(t *testing.T) {
	config := testOpenAICodexPoolConfig(t, openAICodexAccountStrategyFillFirst,
		struct {
			id        string
			accountID string
			fedRAMP   bool
		}{"a", "acct-a", false},
	)
	pool, err := newOpenAICodexAccountPool("codex", config)
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.initialize(context.Background(), http.DefaultClient); err != nil {
		t.Fatal(err)
	}
	seedOpenAICodexPoolCatalog(pool, "gpt-health")
	lease, err := pool.acquireForOperation(context.Background(), http.DefaultClient, testOpenAICodexPoolOperation("gpt-health"), "gpt-health", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	writeTestOpenAICodexAuth(t, filepath.Dir(config.Accounts[0].AuthFile), testOpenAICodexTokens(t, time.Now().Add(time.Hour), "acct-b", false, "refresh-b"))
	member := pool.members[0]
	if _, _, err := pool.credentialsForMember(context.Background(), http.DefaultClient, member); err != nil {
		t.Fatal(err)
	}
	newCredentialID := member.credentialID
	pool.reportFailure(lease, "gpt-health", openAICodexAccountFailureAuth, nil)
	pool.reportSuccess(lease, "gpt-health")
	pool.mu.Lock()
	quarantined := member.quarantined
	gotCredentialID := member.credentialID
	pool.mu.Unlock()
	if quarantined || gotCredentialID != newCredentialID {
		t.Fatalf("stale outcome mutated replacement: quarantined=%v credential=%q want=%q", quarantined, gotCredentialID, newCredentialID)
	}
}

func TestResponsesWebSocketPinsPooledCredentialBeforeAmbiguousErrorFrame(t *testing.T) {
	provider := &providerRuntime{id: "codex", kind: providerTypeOpenAICodex}
	route := &modelRoute{
		public:         publicModelContract{id: "gpt-ws", routeID: "route-ws", endpoints: []string{providerEndpointResponses}},
		targets:        []targetBinding{{id: "codex", provider: provider, upstreamModel: "gpt-ws"}},
		policy:         routePolicy{mode: routeModePriorityFailover, maxTargetAttempts: 1, maxUpstreamSends: 2},
		legacy:         true,
		managedCatalog: true,
	}
	operation := newRouteOperation(route, context.Background())
	operation.pinOwner(executionOwner{routeID: route.public.routeID, targetID: "codex", credentialID: "credential-ws"})
	session := &responsesWebSocketSession{}
	resp := &http.Response{StatusCode: http.StatusForbidden, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"code":"forbidden"}}`))}
	if err := session.prepareExplicitRouteSuccessResponse(&ProxyHandler{}, resp, route, operation, responsesWebSocketRequestPlan{}); err != nil {
		t.Fatalf("prepareExplicitRouteSuccessResponse() error = %v", err)
	}
	if session.explicitRouteID != route.public.routeID || session.explicitTargetID != "codex" || session.explicitCredentialID != "credential-ws" {
		t.Fatalf("session owner = %q/%q/%q, want full pooled owner", session.explicitRouteID, session.explicitTargetID, session.explicitCredentialID)
	}
}

func TestMergeOpenAICodexProviderModelsTreatsMissingMetadataAsUnsupported(t *testing.T) {
	complete := providerModel{publicID: "gpt", providerID: "codex", parallelToolCalls: testBoolPtr(true), raw: json.RawMessage(`{
		"id":"gpt","context_window":200000,"max_context_window":200000,
		"capabilities":{"limits":{"max_context_window_tokens":200000},"supports":{"parallel_tool_calls":true,"vision":true,"reasoning_effort":["low","high"]}}
	}`)}
	missing := providerModel{publicID: "gpt", providerID: "codex", raw: json.RawMessage(`{"id":"gpt"}`)}
	forward := mergeOpenAICodexProviderModels([]providerModel{complete, missing})
	reverse := mergeOpenAICodexProviderModels([]providerModel{missing, complete})
	for name, merged := range map[string]providerModel{"forward": forward, "reverse": reverse} {
		var raw map[string]any
		if err := json.Unmarshal(merged.raw, &raw); err != nil {
			t.Fatalf("%s Unmarshal() error = %v", name, err)
		}
		if raw["context_window"] != float64(0) || raw["max_context_window"] != float64(0) {
			t.Fatalf("%s context metadata = %#v, want unknown zero", name, raw)
		}
		caps := raw["capabilities"].(map[string]any)
		supports := caps["supports"].(map[string]any)
		if supports["parallel_tool_calls"] != false || supports["vision"] != false || len(supports["reasoning_effort"].([]any)) != 0 {
			t.Fatalf("%s supports = %#v, want conservative unsupported", name, supports)
		}
		limits := caps["limits"].(map[string]any)
		if limits["max_context_window_tokens"] != float64(0) {
			t.Fatalf("%s limit = %#v, want zero", name, limits)
		}
	}
}

func TestOpenAICodexAccountPoolStaleChatReplayMapsToMissingWithoutDispatch(t *testing.T) {
	config := testOpenAICodexPoolConfig(t, openAICodexAccountStrategyFillFirst,
		struct {
			id        string
			accountID string
			fedRAMP   bool
		}{"a", "acct-a", false},
	)
	var inferenceCalls atomic.Int64
	h := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			_, _ = io.WriteString(w, testOpenAICodexCatalogBody("gpt-replay"))
			return
		}
		inferenceCalls.Add(1)
		http.Error(w, "unexpected inference", http.StatusInternalServerError)
	})
	cfg := ProvidersConfig{SchemaVersion: ProvidersConfigSchemaVersion2, Providers: []ProviderConfig{{ID: "codex", Type: string(providerTypeOpenAICodex), Default: true, BaseURL: h.copilotURL, CodexAccounts: config}}}
	setup, err := h.buildConfiguredProviderSetupWithDynamicValidation(context.Background(), cfg, true)
	if err != nil {
		t.Fatal(err)
	}
	h.providersState = setup
	route, ok := h.resolveModelRouteForRequest("gpt-replay", providerEndpointResponses)
	if !ok {
		t.Fatal("pooled replay route missing")
	}
	target, _ := route.primaryTarget()
	credentialID := target.provider.codexPool.members[0].credentialID
	published, err := h.responsesChatReplayStore().Publish(responsesChatReplayPublishRequest{
		Route:            explicitResponsesChatReplayRoute(route, target, credentialID),
		AssistantContent: json.RawMessage(`null`),
		OutputItems: []json.RawMessage{
			json.RawMessage(`{"type":"function_call","id":"item-replay","call_id":"upstream-replay","name":"lookup","arguments":"{}","status":"completed"}`),
		},
		Calls: []responsesChatReplayPublishCall{{UpstreamCallID: "upstream-replay", Name: "lookup", VisibleArguments: `{}`, OutputItemIndex: 0}},
	})
	if err != nil {
		t.Fatal(err)
	}
	call := published.Projection.Calls[0]
	body, _ := json.Marshal(map[string]any{
		"model": "gpt-replay",
		"messages": []any{
			map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": call.ID, "type": "function", "function": map[string]any{"name": call.Name, "arguments": call.Arguments}}}},
			map[string]any{"role": "tool", "tool_call_id": call.ID, "content": "result"},
		},
	})
	writeTestOpenAICodexAuth(t, filepath.Dir(config.Accounts[0].AuthFile), testOpenAICodexTokens(t, time.Now().Add(time.Hour), "acct-b", false, "refresh-b"))
	rec := httptest.NewRecorder()
	h.HandleOpenAIChatCompletions(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s, want 400", rec.Code, rec.Body.String())
	}
	if inferenceCalls.Load() != 0 {
		t.Fatalf("stale replay dispatched %d upstream calls", inferenceCalls.Load())
	}
	var payload struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Error.Code != responsesChatReplayMissingCode {
		t.Fatalf("error code = %q, want %q", payload.Error.Code, responsesChatReplayMissingCode)
	}
}

func TestOpenAICodexCatalogRouteExecutionCompatibilityAndIdentity(t *testing.T) {
	unpooled, err := buildProviderRuntime(ProviderConfig{ID: "codex", Type: string(providerTypeOpenAICodex)}, "http://copilot.test", nil)
	if err != nil {
		t.Fatal(err)
	}
	legacyRoute, err := compileLegacyModelRoute(providerModel{publicID: "gpt-a", providerID: "codex", supportedEndpoints: []string{providerEndpointResponses}}, unpooled)
	if err != nil {
		t.Fatal(err)
	}
	if legacyRoute.usesManagedExecution() {
		t.Fatal("unpooled Codex catalog route unexpectedly changed legacy execution behavior")
	}

	config := testOpenAICodexPoolConfig(t, openAICodexAccountStrategyFillFirst,
		struct {
			id        string
			accountID string
			fedRAMP   bool
		}{"a", "acct-a", false},
	)
	pooled, err := buildProviderRuntime(ProviderConfig{ID: "codex", Type: string(providerTypeOpenAICodex), CodexAccounts: config}, "http://copilot.test", nil)
	if err != nil {
		t.Fatal(err)
	}
	routeA, _ := compileLegacyModelRoute(providerModel{publicID: "gpt-a", providerID: "codex", supportedEndpoints: []string{providerEndpointResponses}}, pooled)
	routeB, _ := compileLegacyModelRoute(providerModel{publicID: "gpt-b", providerID: "codex", supportedEndpoints: []string{providerEndpointResponses}}, pooled)
	if !routeA.legacy || !routeA.usesManagedExecution() {
		t.Fatal("pooled route must retain provider-catalog origin and use managed execution")
	}
	if routeA.public.routeID == routeB.public.routeID {
		t.Fatalf("pooled catalog route ids collide across models: %q", routeA.public.routeID)
	}
	if routeA.policy.maxTargetAttempts != 1 {
		t.Fatalf("pooled target attempts = %d, want one provider target", routeA.policy.maxTargetAttempts)
	}
}

func TestCompactInflightKeySeparatesPooledCredentialOwners(t *testing.T) {
	config := testOpenAICodexPoolConfig(t, openAICodexAccountStrategyFillFirst,
		struct {
			id        string
			accountID string
			fedRAMP   bool
		}{"a", "acct-a", false},
	)
	pool, err := newOpenAICodexAccountPool("codex", config)
	if err != nil {
		t.Fatal(err)
	}
	provider := &providerRuntime{id: "codex", kind: providerTypeOpenAICodex, baseURL: "http://upstream.test", codexPool: pool}
	route, err := compileLegacyModelRoute(providerModel{publicID: "gpt-compact", providerID: "codex", upstreamModel: "gpt-compact", supportedEndpoints: []string{providerEndpointResponses}}, provider)
	if err != nil {
		t.Fatal(err)
	}
	fields := map[string]json.RawMessage{"model": json.RawMessage(`"gpt-compact"`), "input": json.RawMessage(`[]`)}
	keyFor := func(credentialID string) string {
		operation := newRouteOperation(route, context.Background())
		if err := operation.forcePinnedOwner(executionOwner{routeID: route.public.routeID, targetID: "codex", credentialID: credentialID}); err != nil {
			t.Fatal(err)
		}
		key, ok := compactInflightKey(withRouteOperation(context.Background(), operation), fields, nil)
		if !ok {
			t.Fatal("compact inflight key unavailable for hard-pinned pooled owner")
		}
		return key
	}
	if keyFor("credential-a") == keyFor("credential-b") {
		t.Fatal("compaction coalescing key did not separate credential owners")
	}
}

func TestOpenAICodexAccountPoolHardPinnedTransportRetryUsesSameCredential(t *testing.T) {
	var accounts []string
	h, model := newOpenAICodexPoolRouteTestHandler(t, func(req *http.Request) (*http.Response, error) {
		accounts = append(accounts, req.Header.Get("ChatGPT-Account-ID"))
		return nil, fmt.Errorf("dial failed")
	})
	ctx, _ := WithRequestSummary(context.Background())
	ctx, operation, route, err := h.withExplicitRouteOperation(ctx, ctx, model, providerEndpointResponses)
	if err != nil {
		t.Fatal(err)
	}
	target, _ := route.primaryTarget()
	credentialID := target.provider.codexPool.members[0].credentialID
	if err := operation.forcePinnedOwner(executionOwner{routeID: route.public.routeID, targetID: target.id, credentialID: credentialID}); err != nil {
		t.Fatal(err)
	}
	operation.setCommitment(downstreamCommitmentProtocolFrame)
	_, err = h.postResponsesWithHeadersForModel(ctx, []byte(`{"model":"gpt-transport","input":"hello"}`), nil, model)
	if err == nil {
		t.Fatal("request error = nil, want transport failure")
	}
	if fmt.Sprint(accounts) != "[acct-a acct-a]" {
		t.Fatalf("hard-pinned retry accounts = %v, want exact credential twice", accounts)
	}
}

func TestOpenAICodexAccountPoolFreshCompactionCanFailOverBeforeOwnerPin(t *testing.T) {
	config := testOpenAICodexPoolConfig(t, openAICodexAccountStrategyFillFirst,
		struct {
			id        string
			accountID string
			fedRAMP   bool
		}{"a", "acct-a", false},
		struct {
			id        string
			accountID string
			fedRAMP   bool
		}{"b", "acct-b", false},
	)
	config.SessionAffinity = testBoolPtr(false)
	var accounts []string
	h := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			_, _ = io.WriteString(w, testOpenAICodexCatalogBody("gpt-compact-failover"))
			return
		}
		accountID := r.Header.Get("ChatGPT-Account-ID")
		accounts = append(accounts, accountID)
		if accountID == "acct-a" {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"code":"rate_limit_exceeded"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"resp_compact","model":"gpt-compact-failover","output":[]}`)
	})
	cfg := ProvidersConfig{SchemaVersion: ProvidersConfigSchemaVersion2, Providers: []ProviderConfig{{ID: "codex", Type: string(providerTypeOpenAICodex), Default: true, BaseURL: h.copilotURL, CodexAccounts: config}}}
	setup, err := h.buildConfiguredProviderSetupWithDynamicValidation(context.Background(), cfg, true)
	if err != nil {
		t.Fatal(err)
	}
	h.providersState = setup
	ctx, _ := WithRequestSummary(context.Background())
	ctx, _, _, err = h.withExplicitRouteOperation(ctx, ctx, "gpt-compact-failover", providerEndpointResponses)
	if err != nil {
		t.Fatal(err)
	}
	ctx = withRouteAttemptKind(ctx, routeAttemptCompaction)
	resp, err := h.postResponsesWithHeadersForModel(ctx, []byte(`{"model":"gpt-compact-failover","input":"compact"}`), nil, "gpt-compact-failover")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || fmt.Sprint(accounts) != "[acct-a acct-b]" {
		t.Fatalf("compaction status/accounts = %d/%v, want safe pre-owner failover", resp.StatusCode, accounts)
	}
}

func TestOpenAICodexAccountPoolHardPinnedQuotaNeverMigrates(t *testing.T) {
	config := testOpenAICodexPoolConfig(t, openAICodexAccountStrategyFillFirst,
		struct {
			id        string
			accountID string
			fedRAMP   bool
		}{"a", "acct-a", false},
		struct {
			id        string
			accountID string
			fedRAMP   bool
		}{"b", "acct-b", false},
	)
	var accounts []string
	h := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			_, _ = io.WriteString(w, testOpenAICodexCatalogBody("gpt-hard-quota"))
			return
		}
		accounts = append(accounts, r.Header.Get("ChatGPT-Account-ID"))
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"code":"rate_limit_exceeded"}}`)
	})
	cfg := ProvidersConfig{SchemaVersion: ProvidersConfigSchemaVersion2, Providers: []ProviderConfig{{ID: "codex", Type: string(providerTypeOpenAICodex), Default: true, BaseURL: h.copilotURL, CodexAccounts: config}}}
	setup, err := h.buildConfiguredProviderSetupWithDynamicValidation(context.Background(), cfg, true)
	if err != nil {
		t.Fatal(err)
	}
	h.providersState = setup
	ctx, _ := WithRequestSummary(context.Background())
	ctx, operation, route, err := h.withExplicitRouteOperation(ctx, ctx, "gpt-hard-quota", providerEndpointResponses)
	if err != nil {
		t.Fatal(err)
	}
	target, _ := route.primaryTarget()
	credentialID := target.provider.codexPool.members[0].credentialID
	if err := operation.forcePinnedOwner(executionOwner{routeID: route.public.routeID, targetID: target.id, credentialID: credentialID}); err != nil {
		t.Fatal(err)
	}
	resp, err := h.postResponsesWithHeadersForModel(ctx, []byte(`{"model":"gpt-hard-quota","input":"hello"}`), nil, "gpt-hard-quota")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests || fmt.Sprint(accounts) != "[acct-a]" {
		t.Fatalf("status/accounts = %d/%v, hard owner must not migrate", resp.StatusCode, accounts)
	}
}

func TestOpenAICodexAccountPoolConcurrentConversationBootstrapConvergesOnOneOwner(t *testing.T) {
	config := testOpenAICodexPoolConfig(t, openAICodexAccountStrategyRoundRobin,
		struct {
			id        string
			accountID string
			fedRAMP   bool
		}{"a", "acct-a", false},
		struct {
			id        string
			accountID string
			fedRAMP   bool
		}{"b", "acct-b", false},
	)
	pool, err := newOpenAICodexAccountPool("codex", config)
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.initialize(context.Background(), http.DefaultClient); err != nil {
		t.Fatal(err)
	}
	seedOpenAICodexPoolCatalog(pool, "gpt-conversation")
	provider := &providerRuntime{id: "codex", kind: providerTypeOpenAICodex, codexPool: pool}
	route, err := compileLegacyModelRoute(providerModel{publicID: "gpt-conversation", providerID: "codex", upstreamModel: "gpt-conversation", supportedEndpoints: []string{providerEndpointResponses}}, provider)
	if err != nil {
		t.Fatal(err)
	}
	target, _ := route.primaryTarget()
	h := &ProxyHandler{}
	token := stateBindingToken{stateType: stateBindingTypeConversationID, value: "conv-shared"}
	operations := []*routeOperation{newRouteOperation(route, context.Background()), newRouteOperation(route, context.Background())}
	leases := make([]*openAICodexAccountLease, 2)
	for index, operation := range operations {
		operation.setPendingConversationToken(token)
		lease, acquireErr := pool.acquireForOperation(context.Background(), http.DefaultClient, operation, "gpt-conversation", nil, nil)
		if acquireErr != nil {
			t.Fatal(acquireErr)
		}
		leases[index] = lease
	}
	if leases[0].credentialID == leases[1].credentialID {
		t.Fatal("round robin did not provide distinct bootstrap candidates")
	}

	errs := make([]error, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for index := range operations {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			errs[index] = h.bindPendingConversationForLease(operations[index], target, leases[index].credentialID)
		}(index)
	}
	close(start)
	wg.Wait()
	winnerCount := 0
	rebindCount := 0
	for _, err := range errs {
		switch {
		case err == nil:
			winnerCount++
		case errors.Is(err, errConversationCredentialRebind):
			rebindCount++
		default:
			t.Fatalf("unexpected bootstrap error: %v", err)
		}
	}
	if winnerCount != 1 || rebindCount != 1 {
		t.Fatalf("bootstrap outcomes winner/rebind = %d/%d, want 1/1", winnerCount, rebindCount)
	}
	if operations[0].pinnedOwner() != operations[1].pinnedOwner() {
		t.Fatalf("bootstrap owners diverged: %#v vs %#v", operations[0].pinnedOwner(), operations[1].pinnedOwner())
	}
}

func TestOpenAICodexHalfOpenCleanupTimeoutFinalizesWithoutStatsRecord(t *testing.T) {
	config := testOpenAICodexPoolConfig(t, openAICodexAccountStrategyFillFirst,
		struct {
			id        string
			accountID string
			fedRAMP   bool
		}{"a", "acct-a", false},
	)
	pool, err := newOpenAICodexAccountPool("codex", config)
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.initialize(context.Background(), http.DefaultClient); err != nil {
		t.Fatal(err)
	}
	seedOpenAICodexPoolCatalog(pool, "gpt-half-open-timeout")
	now := time.Unix(1_700_000_000, 0)
	pool.now = func() time.Time { return now }
	initial, err := pool.acquireForOperation(context.Background(), http.DefaultClient, testOpenAICodexPoolOperation("gpt-half-open-timeout"), "gpt-half-open-timeout", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	pool.reportFailure(initial, "gpt-half-open-timeout", openAICodexAccountFailureQuota, http.Header{"Retry-After": []string{"1"}})
	now = now.Add(2 * time.Second)
	operation := testOpenAICodexPoolOperation("gpt-half-open-timeout")
	lease, err := pool.acquireForOperation(context.Background(), http.DefaultClient, operation, "gpt-half-open-timeout", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !lease.halfOpen {
		t.Fatal("lease is not a half-open probe")
	}
	observer := newRouteAttemptResponseObserver(nil, operation, routeAttemptTrace{StatusCode: http.StatusOK, Decision: routeRetryAccepted}, newRouteSendObservation(now, func() time.Time { return now }), providerEndpointResponses, false, nil)
	observer.codexLease = lease
	observer.codexModel = "gpt-half-open-timeout"
	observer.publishCleanupTimeout()
	pool.mu.Lock()
	probeInFlight := pool.members[0].modelCooldowns["gpt-half-open-timeout"].probeInFlight
	pool.mu.Unlock()
	if probeInFlight {
		t.Fatal("cleanup timeout left half-open probe permanently in flight")
	}
}

func TestOpenAICodexAccountPoolRecognizedEntitlementFailureSwitchesAccount(t *testing.T) {
	config := testOpenAICodexPoolConfig(t, openAICodexAccountStrategyFillFirst,
		struct {
			id        string
			accountID string
			fedRAMP   bool
		}{"a", "acct-a", false},
		struct {
			id        string
			accountID string
			fedRAMP   bool
		}{"b", "acct-b", false},
	)
	config.SessionAffinity = testBoolPtr(false)
	var accounts []string
	h := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			_, _ = io.WriteString(w, testOpenAICodexCatalogBody("gpt-entitlement"))
			return
		}
		accountID := r.Header.Get("ChatGPT-Account-ID")
		accounts = append(accounts, accountID)
		if accountID == "acct-a" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"error":{"code":"model_not_found","message":"not available for this account"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"resp_entitlement","model":"gpt-entitlement","output":[]}`)
	})
	cfg := ProvidersConfig{SchemaVersion: ProvidersConfigSchemaVersion2, Providers: []ProviderConfig{{ID: "codex", Type: string(providerTypeOpenAICodex), Default: true, BaseURL: h.copilotURL, CodexAccounts: config}}}
	setup, err := h.buildConfiguredProviderSetupWithDynamicValidation(context.Background(), cfg, true)
	if err != nil {
		t.Fatal(err)
	}
	h.providersState = setup
	ctx, _ := WithRequestSummary(context.Background())
	ctx, _, _, err = h.withExplicitRouteOperation(ctx, ctx, "gpt-entitlement", providerEndpointResponses)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := h.postResponsesWithHeadersForModel(ctx, []byte(`{"model":"gpt-entitlement","input":"hello"}`), nil, "gpt-entitlement")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || fmt.Sprint(accounts) != "[acct-a acct-b]" {
		t.Fatalf("status/accounts = %d/%v, want recognized entitlement failover", resp.StatusCode, accounts)
	}
}
