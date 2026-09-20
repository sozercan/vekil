package proxy

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sozercan/vekil/auth"
)

func conversationCopilotConfig(t *testing.T) ProvidersConfig {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return ProvidersConfig{
		SchemaVersion:         2,
		StateBindings:         &StateBindingsConfig{Mode: "durable", File: filepath.Join(dir, "bindings.db")},
		ConversationMigration: &ConversationMigrationConfig{Routes: []string{"azure"}},
		Providers: []ProviderConfig{
			{ID: "east", Type: "azure-openai", BaseURL: "https://east.openai.azure.com/openai/v1", APIKey: "east-test-key"},
			{ID: "west", Type: "azure-openai", BaseURL: "https://west.openai.azure.com/openai/v1", APIKey: "west-test-key"},
			{ID: "copilot", Type: "copilot"},
		},
		ModelRoutes: []ModelRouteConfig{{
			ID: "azure", PublicID: "coding", Endpoints: []string{providerEndpointResponses},
			Targets: []ModelRouteTargetConfig{
				{ID: "east", Provider: "east", UpstreamModel: "deployment-east"},
				{ID: "west", Provider: "west", UpstreamModel: "deployment-west"},
				{ID: "copilot", Provider: "copilot", UpstreamModel: "copilot-model"},
			},
			Routing: ModelRouteRoutingConfig{Mode: "priority_failover", MaxTargetAttempts: 3, MaxUpstreamSends: 3},
		}},
	}
}

func conversationCopilotModels(req *http.Request) *http.Response {
	return routeExecutorTestResponse(req, http.StatusOK, nil, `{"data":[{"id":"copilot-model","supported_endpoints":["/responses"]}]}`)
}

func TestConversationMigrationCopilotPreservesHistoryAndReconnect(t *testing.T) {
	t.Setenv("COPILOT_GITHUB_TOKEN", "")
	for _, protocol := range []string{"http", "http_omitted_store", "sse", "websocket"} {
		t.Run(protocol, func(t *testing.T) {
			stored := protocol == "http" || protocol == "http_omitted_store"
			var outage atomic.Bool
			var east, west, copilot atomic.Int32
			var mu sync.Mutex
			var copilotBodies []string
			transport := routeExecutorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				if strings.HasSuffix(req.URL.Path, "/models") {
					return conversationCopilotModels(req), nil
				}
				if strings.HasPrefix(req.URL.Host, "east.") {
					number := east.Add(1)
					if outage.Load() {
						return nil, errors.New("east failed before write")
					}
					if number == 1 {
						return conversationResponse(t, req, "east-1",
							map[string]any{"type": "reasoning", "id": "reason-east", "encrypted_content": "private-east", "summary": []any{}},
							map[string]any{"type": "function_call", "id": "item-edit", "call_id": "call-edit", "name": "edit_file", "arguments": `{"text":"cedar"}`}), nil
					}
					return conversationResponse(t, req, "east-2", conversationText("The cedar edit completed.")), nil
				}
				if strings.HasPrefix(req.URL.Host, "west.") {
					west.Add(1)
					return nil, errors.New("west failed before write")
				}
				if req.Header.Get("Authorization") != "Bearer test-token" || req.Header.Get("api-key") != "" {
					t.Error("Copilot received incorrect authentication")
				}
				if req.Context().Value(responsesNativeRequestContextKey{}) != nil {
					t.Error("protected Copilot turn attempted native WebSocket transport")
				}
				body, err := io.ReadAll(req.Body)
				if err != nil {
					return nil, err
				}
				req.Body = io.NopCloser(bytes.NewReader(body))
				if bytes.Contains(body, []byte("private-east")) || bytes.Contains(body, []byte(`"previous_response_id":"east-`)) || req.Header.Get("X-Codex-Turn-State") != "" {
					t.Error("Copilot received Azure-owned state")
				}
				if !bytes.Contains(body, []byte(`"model":"copilot-model"`)) {
					t.Error("Copilot did not receive its configured upstream model")
				}
				if (protocol != "http_omitted_store" && !bytes.Contains(body, []byte(`"store":false`))) || bytes.Contains(body, []byte(`"previous_response_id"`)) {
					t.Error("Copilot received a request requiring upstream response storage")
				}
				mu.Lock()
				copilotBodies = append(copilotBodies, string(body))
				mu.Unlock()
				number := copilot.Add(1)
				return conversationResponse(t, req, fmt.Sprintf("copilot-%d", number), conversationText(fmt.Sprintf("Copilot answer %d.", number))), nil
			})
			cfg := conversationCopilotConfig(t)
			options := []Option{WithResponsesWebSocketConfig(ResponsesWebSocketConfig{Enabled: true, NativeUpstream: true})}
			h, _ := newConversationAPIHandler(t, transport, &cfg, options...)
			post := func(fields map[string]any, headers http.Header, stream bool) {
				if protocol == "http_omitted_store" {
					delete(fields, "store")
				}
				conversationCompleted(t, conversationPOST(t, h, fields, headers), stream)
			}
			post(map[string]any{
				"input": "Edit the cedar file once.", "instructions": "Preserve the original instruction.", "store": stored,
				"tools": []any{map[string]any{"type": "function", "name": "edit_file", "parameters": map[string]any{"type": "object"}}},
			}, nil, false)
			post(map[string]any{
				"previous_response_id": "east-1", "store": stored,
				"input": []any{map[string]any{"type": "function_call_output", "call_id": "call-edit", "output": "wrote cedar exactly once"}},
			}, nil, false)
			stopConversationAPIHandler(t, h)
			outage.Store(true)
			h, _ = newConversationAPIHandler(t, transport, &cfg, options...)
			turn := func(fields map[string]any) {
				fields["store"], fields["stream"] = stored, protocol == "sse"
				post(fields, http.Header{"X-Codex-Turn-State": {"turn-east-2"}}, protocol == "sse")
			}
			closeClient := func() {}
			connect := func() {
				if protocol != "websocket" {
					return
				}
				conn := mustDialResponsesWebSocket(t, startResponsesWebSocketProxyServer(t, h), nil)
				closeClient = func() { _ = conn.Close() }
				turn = func(fields map[string]any) { conversationWebSocketTurn(t, conn, fields) }
			}
			connect()
			turn(map[string]any{"previous_response_id": "east-2", "input": "Continue after the Azure outage."})
			followup := any("Stay on Copilot.")
			if protocol == "websocket" {
				// The HTTP bridge must not inherit native Copilot's 4096-item
				// limit after migration, even when native transport is enabled.
				items := make([]any, responsesNativeMaxPendingItems+1)
				for i := range items {
					items[i] = map[string]any{"role": "user", "content": "Stay on Copilot."}
				}
				followup = items
			}
			turn(map[string]any{"previous_response_id": "copilot-1", "input": followup})
			closeClient()
			stopConversationAPIHandler(t, h)
			h, _ = newConversationAPIHandler(t, transport, &cfg, options...)
			connect()
			defer closeClient()
			turn(map[string]any{"previous_response_id": "copilot-2", "input": "Reconnect after restart."})
			turn(map[string]any{"previous_response_id": "east-2", "input": "Branch from the original Azure answer."})
			mu.Lock()
			defer mu.Unlock()
			if east.Load() != 4 || west.Load() != 2 || copilot.Load() != 4 || len(copilotBodies) != 4 {
				t.Fatalf("unexpected sends east=%d west=%d copilot=%d", east.Load(), west.Load(), copilot.Load())
			}
			for _, want := range []string{"original instruction", "Edit the cedar file once.", "edit_file", "call-edit", "function_call_output", "wrote cedar exactly once", "The cedar edit completed.", "Continue after the Azure outage."} {
				if !strings.Contains(copilotBodies[0], want) {
					t.Errorf("Copilot recovery lost %q", want)
				}
			}
			if strings.Contains(copilotBodies[3], "Stay on Copilot") || strings.Contains(copilotBodies[3], "Copilot answer") {
				t.Fatal("older Azure branch acquired newer Copilot history")
			}
			route, _ := h.resolveModelRouteForRequest("coding", providerEndpointResponses)
			for id, target := range map[string]string{"east-2": "east", "copilot-1": "copilot", "copilot-3": "copilot"} {
				owner := h.stateBindings.lookup(stateBindingTypeResponseID, id)
				if h.stateBindings.ownerTarget(owner.owner, route) != target {
					t.Errorf("%s lost its original %s ownership", id, target)
				}
				if target == "copilot" {
					snapshot, err := h.conversationHistory.lookupResponse("azure", id)
					if err != nil || snapshot.Stored {
						t.Errorf("%s requires Copilot storage: snapshot=%+v err=%v", id, snapshot, err)
					}
				}
			}
		})
	}
}

func TestConversationMigrationCopilotStopsAfterUnsafeBackup(t *testing.T) {
	t.Setenv("COPILOT_GITHUB_TOKEN", "")
	for _, scenario := range []string{"ambiguous delivery", "partial stream", "auth rejection"} {
		t.Run(scenario, func(t *testing.T) {
			var outage atomic.Bool
			var sends, copilot atomic.Int32
			transport := routeExecutorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				if strings.HasSuffix(req.URL.Path, "/models") {
					return conversationCopilotModels(req), nil
				}
				sends.Add(1)
				if strings.HasPrefix(req.URL.Host, "east.") {
					if !outage.Load() {
						return conversationResponse(t, req, "safe-east", conversationText("Saved Azure answer.")), nil
					}
					return nil, errors.New("east failed before write")
				}
				if !strings.HasPrefix(req.URL.Host, "west.") {
					copilot.Add(1)
					return conversationResponse(t, req, "unsafe-copilot", conversationText("Must not execute.")), nil
				}
				switch scenario {
				case "ambiguous delivery":
					if trace := httptrace.ContextClientTrace(req.Context()); trace != nil {
						trace.WroteHeaders()
					}
					return nil, io.ErrUnexpectedEOF
				case "partial stream":
					return routeExecutorTestResponse(req, 200, http.Header{"Content-Type": {"text/event-stream"}}, "data: "+`{"type":"response.output_text.delta","delta":"partial"}`+"\n\n"), nil
				default:
					return routeExecutorTestResponse(req, 403, nil, `{"error":{"code":"forbidden"}}`), nil
				}
			})
			cfg := conversationCopilotConfig(t)
			h, _ := newConversationAPIHandler(t, transport, &cfg)
			conversationCompleted(t, conversationPOST(t, h, map[string]any{"input": "Seed."}, nil), false)
			outage.Store(true)
			response := conversationPOST(t, h, map[string]any{"previous_response_id": "safe-east", "input": "Continue.", "stream": scenario == "partial stream"}, nil)
			if !strings.Contains(response.Body.String(), "conversation_execution_uncertain") || sends.Load() != 3 || copilot.Load() != 0 {
				t.Fatalf("unsafe backup retried: %d %s sends=%d copilot=%d", response.Code, response.Body.String(), sends.Load(), copilot.Load())
			}
			stopConversationAPIHandler(t, h)
			h, _ = newConversationAPIHandler(t, transport, &cfg)
			retry := conversationPOST(t, h, map[string]any{"previous_response_id": "safe-east", "input": "Retry."}, nil)
			if retry.Code != 409 || sends.Load() != 3 || !strings.Contains(retry.Body.String(), "conversation_execution_uncertain") {
				t.Fatalf("uncertainty was lost on restart: %d %s sends=%d", retry.Code, retry.Body.String(), sends.Load())
			}
		})
	}
}

func TestConversationMigrationCopilotFullHistoryReconnect(t *testing.T) {
	t.Setenv("COPILOT_GITHUB_TOKEN", "")
	for _, protocol := range []string{"http", "sse", "websocket"} {
		t.Run(protocol, func(t *testing.T) {
			var outage atomic.Bool
			var east, west, copilot atomic.Int32
			eastOutput := conversationText("Original Azure answer.")
			eastOutput["id"] = "east-output"
			transport := routeExecutorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				if strings.HasSuffix(req.URL.Path, "/models") {
					return conversationCopilotModels(req), nil
				}
				if strings.HasPrefix(req.URL.Host, "east.") {
					east.Add(1)
					if outage.Load() {
						return nil, errors.New("east failed before write")
					}
					return conversationResponse(t, req, "east-1", eastOutput), nil
				}
				if strings.HasPrefix(req.URL.Host, "west.") {
					west.Add(1)
					return nil, errors.New("west failed before write")
				}
				number := copilot.Add(1)
				if number > 1 {
					body, err := io.ReadAll(req.Body)
					if err != nil {
						return nil, err
					}
					req.Body = io.NopCloser(bytes.NewReader(body))
					for _, want := range []string{"Original instruction.", "Original Azure answer.", "Saved Copilot answer.", "Continue on Copilot."} {
						if !bytes.Contains(body, []byte(want)) {
							t.Errorf("full-history reconnect lost %q", want)
						}
					}
				}
				output := conversationText("Saved Copilot answer.")
				output["id"] = fmt.Sprintf("copilot-terminal-item-%d", number)
				return conversationResponse(t, req, fmt.Sprintf("copilot-%d", number), output), nil
			})
			cfg := conversationCopilotConfig(t)
			h, _ := newConversationAPIHandler(t, transport, &cfg)
			headers := http.Header{"Session_id": {"full-history-client"}}
			initial := map[string]any{"role": "user", "content": "Initial question."}
			continuation := map[string]any{"role": "user", "content": "Recover after the Azure outage."}
			conversationCompleted(t, conversationPOST(t, h, map[string]any{
				"input": []any{initial}, "instructions": "Original instruction.", "store": false,
			}, headers), false)
			outage.Store(true)
			conversationCompleted(t, conversationPOST(t, h, map[string]any{
				"input": []any{initial, eastOutput, continuation}, "store": false,
			}, headers), false)
			stopConversationAPIHandler(t, h)
			h, _ = newConversationAPIHandler(t, transport, &cfg)
			// Copilot's streamed item ID can differ from the terminal response's
			// saved ID. The retained Azure anchor must not hide a newer exact
			// full-history match within this conversation and client scope.
			replayed := conversationText("Saved Copilot answer.")
			replayed["id"] = "copilot-stream-item-1"
			input := []any{initial, eastOutput, continuation, replayed, map[string]any{"role": "user", "content": "Continue on Copilot."}}
			for _, invalidHeaders := range []http.Header{nil, {"Session_id": {"different-client"}}} {
				response := conversationPOST(t, h, map[string]any{"input": input, "store": false}, invalidHeaders)
				if response.Code != 400 || !strings.Contains(response.Body.String(), "conversation_history_incomplete") || copilot.Load() != 1 || east.Load() != 2 {
					t.Fatalf("unscoped history was dispatched: %d %s", response.Code, response.Body.String())
				}
			}
			altered := append([]any(nil), input...)
			altered[0] = map[string]any{"role": "user", "content": "Changed original question."}
			response := conversationPOST(t, h, map[string]any{"input": altered, "store": false}, headers)
			if response.Code != 400 || copilot.Load() != 1 || east.Load() != 2 {
				t.Fatalf("altered history was dispatched: %d %s", response.Code, response.Body.String())
			}
			if protocol == "websocket" {
				conn := mustDialResponsesWebSocket(t, startResponsesWebSocketProxyServer(t, h), headers)
				defer func() { _ = conn.Close() }()
				conversationWebSocketTurn(t, conn, map[string]any{"input": input})
			} else {
				stream := protocol == "sse"
				conversationCompleted(t, conversationPOST(t, h, map[string]any{"input": input, "store": false, "stream": stream}, headers), stream)
			}
			if east.Load() != 2 || west.Load() != 1 || copilot.Load() != 2 {
				t.Fatalf("full history selected the old owner: east=%d west=%d copilot=%d", east.Load(), west.Load(), copilot.Load())
			}
		})
	}
}

func TestConversationMigrationFullHistoryPreservesAnchoredRoot(t *testing.T) {
	var sends atomic.Int32
	original := conversationText("Original answer.")
	original["id"] = "original-root-anchor"
	h, _ := newConversationAPIHandler(t, routeExecutorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, "/models") {
			return routeExecutorTestResponse(req, 200, nil, `{"data":[]}`), nil
		}
		number := sends.Add(1)
		output := original
		if number > 1 {
			output = conversationText("Independent imported answer.")
		}
		return conversationResponse(t, req, fmt.Sprintf("root-%d", number), output), nil
	}), nil)
	headers := http.Header{"Session_id": {"shared-client-scope"}}
	initial := map[string]any{"role": "user", "content": "Initial question."}
	continuation := map[string]any{"role": "user", "content": "Imported question."}
	conversationCompleted(t, conversationPOST(t, h, map[string]any{
		"input": []any{initial}, "instructions": "Original root instructions.",
	}, headers), false)
	importHeaders := headers.Clone()
	importHeaders.Set("X-Vekil-History-Complete", "true")
	conversationCompleted(t, conversationPOST(t, h, map[string]any{
		"input": []any{initial, original, continuation}, "instructions": "Independent root instructions.",
	}, importHeaders), false)
	response := conversationPOST(t, h, map[string]any{
		"input": []any{initial, original, continuation, conversationText("Independent imported answer."), map[string]any{"role": "user", "content": "Continue."}},
	}, headers)
	if response.Code != 400 || !strings.Contains(response.Body.String(), "conversation_history_incomplete") || sends.Load() != 2 {
		t.Fatalf("a deeper independent prefix replaced anchored ownership: %d %s sends=%d", response.Code, response.Body.String(), sends.Load())
	}
}

func TestConversationMigrationCopilotRespectsAttemptBudget(t *testing.T) {
	t.Setenv("COPILOT_GITHUB_TOKEN", "")
	var outage atomic.Bool
	var sends, copilot atomic.Int32
	transport := routeExecutorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, "/models") {
			return conversationCopilotModels(req), nil
		}
		number := sends.Add(1)
		if !strings.HasSuffix(req.URL.Host, ".openai.azure.com") {
			copilot.Add(1)
		}
		if outage.Load() {
			return nil, errors.New("connection failed before write")
		}
		return conversationResponse(t, req, fmt.Sprintf("budget-%d", number), conversationText("Saved.")), nil
	})
	cfg := conversationCopilotConfig(t)
	cfg.ModelRoutes[0].Routing.MaxTargetAttempts, cfg.ModelRoutes[0].Routing.MaxUpstreamSends = 2, 2
	h, _ := newConversationAPIHandler(t, transport, &cfg)
	conversationCompleted(t, conversationPOST(t, h, map[string]any{"input": "Seed."}, nil), false)
	outage.Store(true)
	response := conversationPOST(t, h, map[string]any{"previous_response_id": "budget-1", "input": "Continue."}, nil)
	if response.Code < 400 || sends.Load() != 3 || copilot.Load() != 0 {
		t.Fatalf("migration exceeded its budget: %d %s sends=%d copilot=%d", response.Code, response.Body.String(), sends.Load(), copilot.Load())
	}
	outage.Store(false)
	conversationCompleted(t, conversationPOST(t, h, map[string]any{"previous_response_id": "budget-1", "input": "Retry after recovery."}, nil), false)
}

func TestConversationMigrationCopilotCooldownVerifiesOwner(t *testing.T) {
	t.Setenv("COPILOT_GITHUB_TOKEN", "")
	for _, changed := range []bool{false, true} {
		t.Run(fmt.Sprintf("changed_source=%t", changed), func(t *testing.T) {
			var azure, copilot atomic.Int32
			var captured *http.Request
			transport := routeExecutorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				if strings.HasSuffix(req.URL.Path, "/models") {
					return conversationCopilotModels(req), nil
				}
				if strings.HasSuffix(req.URL.Host, ".openai.azure.com") {
					azure.Add(1)
					return conversationResponse(t, req, "azure-recovered", conversationText("Recovered.")), nil
				}
				copilot.Add(1)
				captured = req
				return conversationResponse(t, req, "copilot-source", conversationText("Saved Copilot answer.")), nil
			})
			cfg := conversationCopilotConfig(t)
			targets := cfg.ModelRoutes[0].Targets
			cfg.ModelRoutes[0].Targets = []ModelRouteTargetConfig{targets[2], targets[0], targets[1]}
			h, _ := newConversationAPIHandler(t, transport, &cfg)
			h.auth = auth.NewTestAuthenticatorWithResponsesToken("ghu_original-source", "first-bearer")
			conversationCompleted(t, conversationPOST(t, h, map[string]any{"input": "Seed."}, nil), false)
			// Integration cooldowns span credentials, so a changed source must
			// be rejected before local admission can cause cross-provider replay.
			h.copilotTraffic.observeThrottle(copilotTrafficTestMetadata(t, captured), 429, "60", []byte(`{"error":{"code":"integration_rate_limited"}}`))
			source := "ghu_original-source"
			if changed {
				source = "ghu_different-source"
			}
			h.auth = auth.NewTestAuthenticatorWithResponsesToken(source, "refreshed-bearer")
			response := conversationPOST(t, h, map[string]any{"previous_response_id": "copilot-source", "input": "Continue."}, nil)
			if changed {
				if response.Code != 400 || azure.Load() != 0 || copilot.Load() != 1 {
					t.Fatalf("changed owner migrated: %d %s azure=%d copilot=%d", response.Code, response.Body.String(), azure.Load(), copilot.Load())
				}
			} else {
				conversationCompleted(t, response, false)
				if azure.Load() != 1 || copilot.Load() != 1 {
					t.Fatalf("refreshed owner did not recover without a Copilot send: azure=%d copilot=%d", azure.Load(), copilot.Load())
				}
			}
		})
	}
}
