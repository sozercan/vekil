package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// The upstream completion as Azure emits it: the search action carries result
// sources and the cited text carries annotations.
func conversationWebSearchOutput() []any {
	return []any{
		map[string]any{"type": "reasoning", "id": "rs_east1", "encrypted_content": "private-east-1", "summary": []any{}},
		map[string]any{"type": "web_search_call", "id": "ws_1", "status": "completed", "action": map[string]any{"type": "search", "query": "vekil migration docs", "sources": []any{map[string]any{"type": "url", "url": "https://example.com/docs"}}}},
		map[string]any{"type": "message", "id": "msg_east1", "role": "assistant", "content": []any{map[string]any{
			"type": "output_text", "text": "The docs describe migration.",
			"annotations": []any{map[string]any{"type": "url_citation", "url": "https://example.com/docs", "title": "Docs", "start_index": 0, "end_index": 8}},
		}}},
	}
}

// The same turn as Codex replays it on the next request: prefixed IDs kept,
// unknown action fields and annotations dropped, encrypted reasoning retained.
func conversationWebSearchCodexReplay() []any {
	return []any{
		map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "Find the docs."}}},
		map[string]any{"type": "reasoning", "id": "rs_east1", "encrypted_content": "private-east-1", "summary": []any{}},
		map[string]any{"type": "web_search_call", "id": "ws_1", "status": "completed", "action": map[string]any{"type": "search", "query": "vekil migration docs"}},
		map[string]any{"type": "message", "id": "msg_east1", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "The docs describe migration."}}},
		map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "Summarize."}}},
	}
}

func TestConversationMigrationReplaysWebSearchToDeclaredTargets(t *testing.T) {
	for _, tc := range []struct {
		name        string
		declared    bool
		stream      bool
		fullHistory bool
	}{
		{"declared", true, false, false}, {"declared stream", true, true, false}, {"declared codex full history", true, true, true}, {"undeclared", false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var failed atomic.Bool
			var west atomic.Int32
			var westBody string
			transport := routeExecutorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				if strings.HasSuffix(req.URL.Path, "/models") {
					return routeExecutorTestResponse(req, 200, nil, `{"data":[]}`), nil
				}
				if strings.HasPrefix(req.URL.Host, "east.") {
					if failed.Load() {
						return nil, errors.New("dial failed before request write")
					}
					return conversationResponse(t, req, "east-1", conversationWebSearchOutput()...), nil
				}
				body, _ := io.ReadAll(req.Body)
				westBody = string(body)
				req.Body = io.NopCloser(bytes.NewReader(body))
				return conversationResponse(t, req, fmt.Sprintf("west-%d", west.Add(1)), conversationText("Summary.")), nil
			})
			dir := t.TempDir()
			if err := os.Chmod(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			cfg := ProvidersConfig{
				SchemaVersion:         2,
				StateBindings:         &StateBindingsConfig{Mode: "durable", File: filepath.Join(dir, "bindings.db")},
				ConversationMigration: &ConversationMigrationConfig{Routes: []string{"azure"}},
				Providers: []ProviderConfig{
					{ID: "east", Type: "azure-openai", BaseURL: "https://east.openai.azure.com/openai/v1", APIKey: "east-test-key", HostedTools: []string{"web_search"}},
					{ID: "west", Type: "azure-openai", BaseURL: "https://west.openai.azure.com/openai/v1", APIKey: "west-test-key"},
				},
				ModelRoutes: []ModelRouteConfig{{
					ID: "azure", PublicID: "coding", Endpoints: []string{providerEndpointResponses},
					Targets: []ModelRouteTargetConfig{{ID: "east", Provider: "east", UpstreamModel: "deployment-east"}, {ID: "west", Provider: "west", UpstreamModel: "deployment-west"}},
					Routing: ModelRouteRoutingConfig{Mode: "priority_failover", MaxTargetAttempts: 2, MaxUpstreamSends: 3},
				}},
			}
			if tc.declared {
				cfg.Providers[1].HostedTools = []string{"web_search"}
			}
			h, _ := newConversationAPIHandler(t, transport, &cfg)
			first := conversationCompleted(t, conversationPOST(t, h, map[string]any{
				"input": "Find the docs.", "stream": tc.stream,
				"tools": []any{
					map[string]any{"type": "web_search", "external_web_access": false},
					map[string]any{"type": "function", "name": "edit_file", "parameters": map[string]any{"type": "object"}},
				},
			}, nil), tc.stream)
			if rawJSONString(first["id"]) != "east-1" {
				t.Fatal("initial turn did not use east")
			}
			failed.Store(true)
			fields := map[string]any{"previous_response_id": "east-1", "input": "Summarize.", "stream": tc.stream}
			if tc.fullHistory {
				fields = map[string]any{"input": conversationWebSearchCodexReplay(), "stream": tc.stream, "store": false}
			}
			second := conversationPOST(t, h, fields, http.Header{"X-Codex-Turn-State": {"turn-east-1"}})
			if !tc.declared {
				// West never declared web search, so it cannot replay the call.
				if second.Code < 400 || west.Load() != 0 {
					t.Fatalf("migrated web search history to an undeclared target: %d %s west=%d", second.Code, second.Body.String(), west.Load())
				}
				return
			}
			response := conversationCompleted(t, second, tc.stream)
			if rawJSONString(response["id"]) != "west-1" || !bytes.Contains(response["vekil"], []byte(`"migration":"completed"`)) {
				t.Fatalf("web search conversation did not migrate: %s", second.Body.String())
			}
			for _, want := range []string{`"type":"web_search"`, `"external_web_access":false`, `"action":{"query":"vekil migration docs","type":"search"},"id":"ws_1","status":"completed","type":"web_search_call"`, "The docs describe migration.", "Find the docs.", "Summarize.", "edit_file"} {
				if !strings.Contains(westBody, want) {
					t.Errorf("reconstructed history is missing %s", want)
				}
			}
			// Saved history matches what Codex replays: no citations, no result
			// sources, and none of east's private state.
			for _, unwanted := range []string{"url_citation", "sources", "private-east", "previous_response_id", "msg_east1"} {
				if strings.Contains(westBody, unwanted) {
					t.Errorf("reconstructed history carries %s", unwanted)
				}
			}
		})
	}
}

func TestConversationMigrationForwardsUnsupportedStateUnprotected(t *testing.T) {
	for _, scenario := range []string{"hosted tool definition", "image input", "hosted output"} {
		t.Run(scenario, func(t *testing.T) {
			var sends atomic.Int32
			h, _ := newConversationAPIHandler(t, routeExecutorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				if strings.HasSuffix(req.URL.Path, "/models") {
					return routeExecutorTestResponse(req, 200, nil, `{"data":[]}`), nil
				}
				number := sends.Add(1)
				if number == 1 {
					return conversationResponse(t, req, "seed", conversationText("Seeded.")), nil
				}
				if scenario == "hosted output" && number == 2 {
					return conversationResponse(t, req, "unprotected",
						map[string]any{"type": "image_generation_call", "id": "ig-1", "status": "completed", "result": "aGVsbG8="},
						conversationText("Generated.")), nil
				}
				return conversationResponse(t, req, fmt.Sprintf("turn-%d", number), conversationText("Continued.")), nil
			}), nil)
			conversationCompleted(t, conversationPOST(t, h, map[string]any{"input": "Seed."}, nil), false)
			fields := map[string]any{"previous_response_id": "seed", "input": "Next."}
			switch scenario {
			case "hosted tool definition":
				fields["tools"] = []any{map[string]any{"type": "code_interpreter", "container": "provider-owned"}}
			case "image input":
				fields["input"] = []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_image", "image_url": "data:image/png;base64,aGVsbG8="}}}}
			}
			result := conversationPOST(t, h, fields, http.Header{"X-Codex-Turn-State": {"turn-seed"}})
			body := result.Body.String()
			if result.Code != http.StatusOK || strings.Contains(body, `"history":"saved"`) || !strings.Contains(body, `"status":"completed"`) || sends.Load() != 2 {
				t.Fatalf("unsupported turn was not forwarded unprotected: %d %s sends=%d", result.Code, body, sends.Load())
			}
			if scenario == "hosted output" {
				if result.Header().Get("X-Vekil-Conversation-Recovery") != "unprotected" {
					t.Fatalf("recovery header = %q, want unprotected", result.Header().Get("X-Vekil-Conversation-Recovery"))
				}
				if _, err := h.conversationHistory.lookupResponse("azure", "unprotected"); !errors.Is(err, errConversationHistoryMissing) {
					t.Fatalf("unprotected completion was saved: %v", err)
				}
				// The unprotected completion has no history to continue from.
				missing := conversationPOST(t, h, map[string]any{"previous_response_id": "unprotected", "input": "More."}, nil)
				if missing.Code != 400 || !strings.Contains(missing.Body.String(), "conversation_history_unavailable") {
					t.Fatalf("continuation from an unprotected completion: %d %s", missing.Code, missing.Body.String())
				}
			}
			// The known outcome leaves the saved seed usable for later branches.
			conversationCompleted(t, conversationPOST(t, h, map[string]any{"previous_response_id": "seed", "input": "Branch."}, nil), false)
		})
	}
}

func TestConversationWebSearchCallCanonicalForm(t *testing.T) {
	for _, tc := range []struct {
		name string
		item string
		want string
	}{
		{"completed with sources", `{"type":"web_search_call","id":"ws_1","status":"completed","action":{"type":"search","query":"docs","sources":[{"type":"url","url":"https://example.com"}]}}`,
			`{"action":{"query":"docs","type":"search"},"id":"ws_1","status":"completed","type":"web_search_call"}`},
		{"open page", `{"type":"web_search_call","id":"ws_2","status":"completed","action":{"type":"open_page","url":"https://example.com"}}`,
			`{"action":{"type":"open_page","url":"https://example.com"},"id":"ws_2","status":"completed","type":"web_search_call"}`},
		{"open page with foreign field", `{"type":"web_search_call","id":"ws_7","status":"completed","action":{"type":"open_page","url":"https://example.com","query":"extra"}}`,
			`{"action":{"type":"open_page","url":"https://example.com"},"id":"ws_7","status":"completed","type":"web_search_call"}`},
		{"find in page", `{"type":"web_search_call","id":"ws_8","status":"completed","action":{"type":"find_in_page","url":"https://example.com","pattern":"docs"}}`,
			`{"action":{"pattern":"docs","type":"find_in_page","url":"https://example.com"},"id":"ws_8","status":"completed","type":"web_search_call"}`},
		{"search queries", `{"type":"web_search_call","id":"ws_10","status":"completed","action":{"type":"search","queries":["docs","vekil"]}}`,
			`{"action":{"queries":["docs","vekil"],"type":"search"},"id":"ws_10","status":"completed","type":"web_search_call"}`},
		{"non-string query", `{"type":"web_search_call","id":"ws_11","status":"completed","action":{"type":"search","query":{"private":"state"}}}`, ""},
		{"non-list queries", `{"type":"web_search_call","id":"ws_12","status":"completed","action":{"type":"search","queries":"docs"}}`, ""},
		{"unknown action type", `{"type":"web_search_call","id":"ws_9","status":"completed","action":{"type":"fetch","url":"https://example.com"}}`, ""},
		{"unprefixed id", `{"type":"web_search_call","id":"legacy","status":"completed","action":{"type":"search","query":"docs"}}`, ""},
		{"missing status", `{"type":"web_search_call","id":"ws_3","action":{"type":"search","query":"docs"}}`, ""},
		{"in progress", `{"type":"web_search_call","id":"ws_4","status":"in_progress","action":{"type":"search","query":"docs"}}`, ""},
		{"missing id", `{"type":"web_search_call","status":"completed","action":{"type":"search","query":"docs"}}`, ""},
		{"missing action type", `{"type":"web_search_call","id":"ws_5","status":"completed","action":{"query":"docs"}}`, ""},
		{"unknown field", `{"type":"web_search_call","id":"ws_6","status":"completed","action":{"type":"search"},"results":[]}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input, err := canonicalConversationInput(json.RawMessage("["+tc.item+"]"), true)
			if tc.want == "" {
				if !errors.Is(err, errConversationHostedState) {
					t.Fatalf("err = %v, want unsupported state", err)
				}
				return
			}
			if err != nil || len(input.items) != 1 || string(input.items[0]) != tc.want {
				t.Fatalf("canonical = %s (err %v), want %s", input.items, err, tc.want)
			}
		})
	}
}

func TestConversationMigrationOptimizesUnprotectedRequests(t *testing.T) {
	originalOutput := strings.Repeat("original tool output ", 64)
	var sends atomic.Int32
	h, _ := newConversationAPIHandler(t, routeExecutorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, "/models") {
			return routeExecutorTestResponse(req, 200, nil, `{"data":[]}`), nil
		}
		body, _ := io.ReadAll(req.Body)
		req.Body = io.NopCloser(bytes.NewReader(body))
		if bytes.Contains(body, []byte(originalOutput)) || !bytes.Contains(body, []byte("reduced output")) {
			t.Errorf("unprotected request skipped the optimizer: %s", body)
		}
		return conversationResponse(t, req, fmt.Sprintf("unprotected-%d", sends.Add(1)), conversationText("Read.")), nil
	}), nil)
	configureRecordingToolOptimizer(h, &recordingToolOptimizer{})
	result := conversationPOST(t, h, map[string]any{
		"tools": []any{map[string]any{"type": "code_interpreter", "container": "provider-owned"}},
		"input": []any{
			map[string]any{"role": "user", "content": "Read the command output."},
			map[string]any{"type": "function_call", "call_id": "optimizer-call", "name": "shell_command", "arguments": `{"command":"cat big.log"}`},
			map[string]any{"type": "function_call_output", "call_id": "optimizer-call", "output": originalOutput},
		},
	}, http.Header{"X-Vekil-History-Complete": {"true"}})
	if result.Code != http.StatusOK || sends.Load() != 1 || strings.Contains(result.Body.String(), `"history":"saved"`) {
		t.Fatalf("unprotected request = %d %s sends=%d", result.Code, result.Body.String(), sends.Load())
	}
}

func TestProviderHostedToolsConfigValidation(t *testing.T) {
	base := func() ProvidersConfig {
		return ProvidersConfig{
			SchemaVersion: 2,
			Providers:     []ProviderConfig{{ID: "east", Type: "azure-openai", BaseURL: "https://east.openai.azure.com/openai/v1", APIKey: "east-test-key"}},
			ModelRoutes: []ModelRouteConfig{{
				ID: "azure", PublicID: "coding", Endpoints: []string{providerEndpointResponses},
				Targets: []ModelRouteTargetConfig{{ID: "east", Provider: "east", UpstreamModel: "deployment-east"}},
				Routing: ModelRouteRoutingConfig{Mode: "primary_only", MaxTargetAttempts: 1, MaxUpstreamSends: 1},
			}},
		}
	}
	for _, tc := range []struct {
		name  string
		tools []string
		want  string
	}{
		{"unsupported", []string{"image_generation"}, `providers[0].hosted_tools: unsupported hosted tool "image_generation"; supported values are web_search`},
		{"duplicate", []string{"web_search", " web_search "}, `providers[0].hosted_tools: duplicates hosted tool "web_search"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base()
			cfg.Providers[0].HostedTools = tc.tools
			if _, err := validateAndNormalizeProvidersConfig(cfg); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
	t.Run("normalized", func(t *testing.T) {
		cfg := base()
		cfg.Providers[0].HostedTools = []string{" web_search "}
		validated, err := validateAndNormalizeProvidersConfig(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if got := validated.config.Providers[0].HostedTools; len(got) != 1 || got[0] != "web_search" {
			t.Fatalf("hosted_tools = %v, want [web_search]", got)
		}
		if cfg.Providers[0].HostedTools[0] != " web_search " {
			t.Fatal("validation mutated the caller's configuration")
		}
		runtime, err := buildProviderRuntimeForProvidersConfig(validated.config.Providers[0], "", nil, true)
		if err != nil {
			t.Fatal(err)
		}
		if !runtime.supportsHostedTools(map[string]bool{"web_search": true}) || runtime.supportsHostedTools(map[string]bool{"image_generation": true}) {
			t.Fatal("runtime hosted tool capability mismatch")
		}
		var none *providerRuntime
		if !none.supportsHostedTools(nil) || none.supportsHostedTools(map[string]bool{"web_search": true}) {
			t.Fatal("nil runtime supports only conversations without hosted tools")
		}
	})
	t.Run("schema v1", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "providers.yaml")
		body := "schema_version: 1\nproviders:\n  - id: upstream\n    type: openai-compatible\n    base_url: https://example.test/v1\n    auth_type: none\n    hosted_tools: []\n"
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadProvidersConfigFile(path); err == nil || !strings.Contains(err.Error(), "providers[0].hosted_tools: requires schema_version: 2") {
			t.Fatalf("LoadProvidersConfigFile() error = %v", err)
		}
	})
}
