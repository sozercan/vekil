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
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
)

func TestConversationMigrationAssertionHeaderStaysLocal(t *testing.T) {
	for _, mode := range []string{"zero-config", "legacy", "explicit-disabled"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", mode, stream), func(t *testing.T) {
				cfg := ProvidersConfig{}
				if mode != "zero-config" {
					cfg.Providers = []ProviderConfig{{ID: "configured", Type: "openai-compatible", Default: true, BaseURL: "https://provider.example.test/v1", APIKey: "test-key",
						Models: []ProviderModelConfig{{PublicID: "coding", Endpoints: []string{providerEndpointResponses}}}}}
				}
				if mode == "explicit-disabled" {
					cfg.SchemaVersion = 2
					cfg.StateBindings = &StateBindingsConfig{Mode: "memory"}
					cfg.Providers[0].Models = nil
					cfg.ModelRoutes = []ModelRouteConfig{{ID: "configured", PublicID: "coding", Endpoints: []string{providerEndpointResponses},
						Targets: []ModelRouteTargetConfig{{ID: "only", Provider: "configured", UpstreamModel: "physical-model"}}}}
				}
				var sends atomic.Int32
				h, err := NewProxyHandler(auth.NewTestAuthenticator("test-token"), logger.NewWithWriter(logger.LevelError, io.Discard), WithProvidersConfig(cfg), func(h *ProxyHandler) {
					h.client = &http.Client{Transport: routeExecutorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
						if strings.HasSuffix(req.URL.Path, "/models") {
							return routeExecutorTestResponse(req, 200, nil, `{"data":[]}`), nil
						}
						sends.Add(1)
						if req.Header.Get("X-Vekil-History-Complete") != "" || req.Header.Get("X-Client-Request-Id") != "client-request" {
							t.Error("upstream headers leaked the history assertion or lost client attribution")
						}
						return conversationResponse(t, req, "header-response", conversationText("Answer.")), nil
					})}
				})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { stopConversationAPIHandler(t, h) })
				disableColdChatRouteDiscoveryForLegacyTest(h)
				response := conversationPOST(t, h, map[string]any{"input": "Question.", "stream": stream}, http.Header{
					"X-Vekil-History-Complete": {"true"}, "X-Client-Request-Id": {"client-request"},
				})
				if response.Code != http.StatusOK || sends.Load() != 1 {
					t.Fatalf("response = %d %s, sends=%d", response.Code, response.Body.String(), sends.Load())
				}
			})
		}
	}
}

func TestConversationMigrationRetainsNewOwnerReasoning(t *testing.T) {
	var outage atomic.Bool
	var west atomic.Int32
	transport := routeExecutorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, "/models") {
			return routeExecutorTestResponse(req, 200, nil, `{"data":[]}`), nil
		}
		if strings.HasPrefix(req.URL.Host, "east.") {
			if outage.Load() {
				return nil, errors.New("prewrite outage")
			}
			message := conversationText("East answer.")
			message["metadata"] = map[string]string{"turn_id": "east-attribution"}
			message[responsesInternalChatMessageMetadataPassthroughField] = map[string]any{"turn_id": "east-attribution", "create_time": 1.5}
			return conversationResponse(t, req, "reason-east", map[string]any{"type": "reasoning", "encrypted_content": "private-east", "summary": []any{}}, message), nil
		}
		number := west.Add(1)
		body, _ := io.ReadAll(req.Body)
		if bytes.Contains(body, []byte("private-east")) || (number == 2 && !bytes.Contains(body, []byte("private-west"))) {
			t.Errorf("turn %d did not retain only west reasoning", number)
		}
		if !bytes.Contains(body, []byte(fmt.Sprintf("catalog-tool-%d", number))) || bytes.Contains(body, []byte("catalog-tool-0")) {
			t.Errorf("turn %d did not use the current tool catalog", number)
		}
		if bytes.Contains(body, []byte("east-attribution")) {
			t.Error("migration forwarded east message attribution")
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
		return conversationResponse(t, req, fmt.Sprintf("reason-west-%d", number), map[string]any{"type": "reasoning", "encrypted_content": fmt.Sprintf("private-west-%d", number), "summary": []any{}}, conversationText("West answer.")), nil
	})
	h, _ := newConversationAPIHandler(t, transport, nil)
	user := func(text string) any { return map[string]any{"role": "user", "content": text} }
	history := []any{conversationToolCatalog("catalog-tool-0"), user("Initial request.")}
	response := conversationCompleted(t, conversationPOST(t, h, map[string]any{"input": history, "store": false}, nil), false)
	outage.Store(true)
	for i, text := range []string{"Migrate.", "Continue on west."} {
		var output []any
		if err := json.Unmarshal(response["output"], &output); err != nil {
			t.Fatal(err)
		}
		history = append(history, output...)
		history = append(history, user(text))
		history[0] = conversationToolCatalog(fmt.Sprintf("catalog-tool-%d", i+1))
		response = conversationCompleted(t, conversationPOST(t, h, map[string]any{"input": history, "store": false}, nil), false)
	}
	if west.Load() != 2 {
		t.Fatalf("west sends = %d", west.Load())
	}
}

func TestConversationMigrationAzureRejectionAndCooldown(t *testing.T) {
	for _, scenario := range []string{"429", "cooldown", "identity changed during cooldown"} {
		t.Run(scenario, func(t *testing.T) {
			var east, west atomic.Int32
			transport := routeExecutorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				if strings.HasSuffix(req.URL.Path, "/models") {
					return routeExecutorTestResponse(req, 200, nil, `{"data":[]}`), nil
				}
				if strings.HasPrefix(req.URL.Host, "east.") {
					if east.Add(1) > 1 {
						return routeExecutorTestResponse(req, 429, http.Header{"Retry-After": {"60"}}, `{"error":{"code":"rate_limit_exceeded"}}`), nil
					}
					return conversationResponse(t, req, "admission-east", conversationText("saved")), nil
				}
				west.Add(1)
				return conversationResponse(t, req, "admission-west", conversationText("recovered")), nil
			})
			h, cfg := newConversationAPIHandler(t, transport, nil)
			conversationCompleted(t, conversationPOST(t, h, map[string]any{"input": "Seed."}, nil), false)
			if scenario == "identity changed during cooldown" {
				stopConversationAPIHandler(t, h)
				cfg.Providers[0].APIKey = "different-authenticated-owner"
				h, _ = newConversationAPIHandler(t, transport, &cfg)
			}
			if scenario != "429" {
				seed := azureTrafficTestRequest(t, h, t.Context(), "east", "https://east.openai.azure.com", "deployment-east")
				azureRouteTrafficFromRequest(seed).observe(429, http.Header{"Retry-After": {"60"}})
			}
			response := conversationPOST(t, h, map[string]any{"previous_response_id": "admission-east", "input": "Continue."}, nil)
			if scenario == "identity changed during cooldown" {
				if response.Code < 400 || east.Load() != 1 || west.Load() != 0 {
					t.Fatalf("owner mismatch migrated: %d %s east=%d west=%d", response.Code, response.Body.String(), east.Load(), west.Load())
				}
				return
			}
			conversationCompleted(t, response, false)
			wantEast := int32(1)
			if scenario == "429" {
				wantEast = 2
			}
			if east.Load() != wantEast || west.Load() != 1 {
				t.Fatalf("admission consumed recovery sends: east=%d west=%d", east.Load(), west.Load())
			}
		})
	}
}

func TestConversationMigrationCancellationClosesAdmission(t *testing.T) {
	for _, scenario := range []string{"cancel", "shutdown", "deadline"} {
		t.Run(scenario, func(t *testing.T) {
			entered := make(chan struct{})
			var sends, west atomic.Int32
			transport := routeExecutorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				if strings.HasSuffix(req.URL.Path, "/models") {
					return routeExecutorTestResponse(req, 200, nil, `{"data":[]}`), nil
				}
				if strings.HasPrefix(req.URL.Host, "west.") {
					west.Add(1)
				}
				if sends.Add(1) == 1 {
					return conversationResponse(t, req, "cancel-seed", conversationText("saved")), nil
				}
				close(entered)
				<-req.Context().Done()
				return nil, req.Context().Err()
			})
			h, _ := newConversationAPIHandler(t, transport, nil)
			conversationCompleted(t, conversationPOST(t, h, map[string]any{"input": "Seed."}, nil), false)
			var ctx context.Context
			var cancel context.CancelFunc
			if scenario == "deadline" {
				ctx, cancel = context.WithTimeout(t.Context(), time.Second)
			} else {
				ctx, cancel = context.WithCancel(t.Context())
			}
			defer cancel()
			finished := make(chan struct{})
			go func() {
				defer close(finished)
				body := strings.NewReader(`{"model":"coding","previous_response_id":"cancel-seed","input":"Continue."}`)
				req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "/v1/responses", body)
				h.HandleResponses(httptest.NewRecorder(), req)
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("continuation did not dispatch")
			}
			switch scenario {
			case "shutdown":
				h.BeginShutdown()
			case "cancel":
				cancel()
			}
			select {
			case <-finished:
			case <-time.After(5 * time.Second):
				t.Fatal("cancelled recovery did not stop")
			}
			if west.Load() != 0 || sends.Load() != 2 {
				t.Fatalf("cancelled request migrated: sends=%d west=%d", sends.Load(), west.Load())
			}
		})
	}
}

func TestConversationMigrationRejectsRedirects(t *testing.T) {
	for _, status := range []int{300, 301, 302, 303, 304, 307, 308} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("status=%d/stream=%t", status, stream), func(t *testing.T) {
				var redirected, sends, west atomic.Int32
				redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					redirected.Add(1)
					w.WriteHeader(http.StatusOK)
				}))
				defer redirect.Close()
				transport := routeExecutorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
					if strings.HasSuffix(req.URL.Path, "/models") {
						return routeExecutorTestResponse(req, 200, nil, `{"data":[]}`), nil
					}
					if strings.HasPrefix(req.URL.Host, "west.") {
						west.Add(1)
					}
					if sends.Add(1) == 1 {
						return conversationResponse(t, req, "redirect-seed", conversationText("Saved answer.")), nil
					}
					return routeExecutorTestResponse(req, status, http.Header{"Location": {redirect.URL}}, "redirected response"), nil
				})
				h, cfg := newConversationAPIHandler(t, transport, nil)
				conversationCompleted(t, conversationPOST(t, h, map[string]any{"input": "Seed."}, nil), false)
				server := httptest.NewServer(http.HandlerFunc(h.HandleResponses))
				defer server.Close()
				body := fmt.Sprintf(`{"model":"coding","previous_response_id":"redirect-seed","input":"Continue.","stream":%t}`, stream)
				// Use a redirect-following client to prove the proxy cannot expose a
				// Location that replays the protected turn outside its route.
				response, err := server.Client().Post(server.URL+"/v1/responses", "application/json", strings.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				data, err := io.ReadAll(response.Body)
				_ = response.Body.Close()
				if err != nil || response.StatusCode != http.StatusConflict || response.Header.Get("Location") != "" || !bytes.Contains(data, []byte("conversation_execution_uncertain")) {
					t.Fatalf("redirect was not withheld: status=%d body=%s err=%v", response.StatusCode, data, err)
				}
				if sends.Load() != 2 || redirected.Load() != 0 || west.Load() != 0 {
					t.Fatalf("redirect replayed: sends=%d redirected=%d west=%d", sends.Load(), redirected.Load(), west.Load())
				}
				stopConversationAPIHandler(t, h)
				h, _ = newConversationAPIHandler(t, transport, &cfg)
				retry := conversationPOST(t, h, map[string]any{"previous_response_id": "redirect-seed", "input": "Retry."}, nil)
				if retry.Code != http.StatusConflict || !strings.Contains(retry.Body.String(), "conversation_execution_uncertain") || sends.Load() != 2 {
					t.Fatalf("redirect uncertainty was lost on restart: %d %s, sends=%d", retry.Code, retry.Body.String(), sends.Load())
				}
			})
		}
	}
}

func TestConversationMigrationUsesEachTargetOnceWithSharedBudget(t *testing.T) {
	var outage atomic.Bool
	var east, west, third atomic.Int32
	var eastOperation *routeOperation
	var eastDeadline time.Time
	transport := routeExecutorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, "/models") {
			return routeExecutorTestResponse(req, 200, nil, `{"data":[]}`), nil
		}
		switch {
		case strings.HasPrefix(req.URL.Host, "east."):
			number := east.Add(1)
			if !outage.Load() {
				return conversationResponse(t, req, fmt.Sprintf("budget-east-%d", number), conversationText("east answer")), nil
			}
			eastOperation = routeOperationFromContext(req.Context())
			eastDeadline, _ = req.Context().Deadline()
		case strings.HasPrefix(req.URL.Host, "west."):
			west.Add(1)
			operation := routeOperationFromContext(req.Context())
			deadline, _ := req.Context().Deadline()
			if operation != eastOperation || eastDeadline.IsZero() || deadline != eastDeadline {
				t.Error("migration replaced the operation or extended its deadline")
			}
			sends, switches, _ := operation.snapshot()
			if sends != 2 || switches != 1 {
				t.Errorf("migration accounting sends=%d switches=%d", sends, switches)
			}
		default:
			third.Add(1)
			operation := routeOperationFromContext(req.Context())
			deadline, _ := req.Context().Deadline()
			sends, switches, _ := operation.snapshot()
			if operation != eastOperation || deadline != eastDeadline || sends != 3 || switches != 2 {
				t.Errorf("third target replaced the operation, extended the deadline, or reset budgets: sends=%d switches=%d", sends, switches)
			}
		}
		return nil, errors.New("connection failed before write")
	})
	h, cfg := newConversationAPIHandler(t, transport, nil)
	stopConversationAPIHandler(t, h)
	cfg.Providers = append(cfg.Providers, ProviderConfig{ID: "third", Type: "azure-openai", BaseURL: "https://third.openai.azure.com/openai/v1", APIKey: "third-test-key"})
	cfg.ModelRoutes[0].Targets = append(cfg.ModelRoutes[0].Targets, ModelRouteTargetConfig{ID: "third", Provider: "third", UpstreamModel: "deployment-third"})
	cfg.ModelRoutes[0].Routing.MaxTargetAttempts = 3
	cfg.ModelRoutes[0].Routing.MaxUpstreamSends = 3
	h, _ = newConversationAPIHandler(t, transport, &cfg)
	conversationCompleted(t, conversationPOST(t, h, map[string]any{"input": "Seed."}, nil), false)
	outage.Store(true)
	failed := conversationPOST(t, h, map[string]any{"previous_response_id": "budget-east-1", "input": "Continue."}, nil)
	if failed.Code < 400 || east.Load() != 2 || west.Load() != 1 || third.Load() != 1 || strings.Contains(failed.Body.String(), `"migration":"completed"`) {
		t.Fatalf("target repeated or false completion: %d %s east=%d west=%d third=%d", failed.Code, failed.Body.String(), east.Load(), west.Load(), third.Load())
	}
	// All sends provably failed before execution. The original branch remains
	// usable, and a failed switch must not redirect it or retain an uncertainty.
	outage.Store(false)
	response := conversationCompleted(t, conversationPOST(t, h, map[string]any{"previous_response_id": "budget-east-1", "input": "Retry after recovery."}, nil), false)
	if rawJSONString(response["id"]) != "budget-east-3" || bytes.Contains(response["vekil"], []byte("completed")) {
		t.Fatal("failed migration changed the saved owner")
	}
}

func TestConversationMigrationStorageFailureWithholdsCompletion(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, boundary := range []string{"intent", "completion", "capacity"} {
			t.Run(fmt.Sprintf("stream=%t/%s", stream, boundary), func(t *testing.T) {
				var h *ProxyHandler
				var sends atomic.Int32
				transport := routeExecutorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
					if strings.HasSuffix(req.URL.Path, "/models") {
						return routeExecutorTestResponse(req, 200, nil, `{"data":[]}`), nil
					}
					sends.Add(1)
					if boundary == "completion" {
						h.stateBindings.durable.mu.Lock()
						h.stateBindings.durable.beforeCommit = func() error { return syscall.ENOSPC }
						h.stateBindings.durable.mu.Unlock()
					}
					return conversationResponse(t, req, "unsaved-response", conversationText("completed work")), nil
				})
				h, _ = newConversationAPIHandler(t, transport, nil)
				switch boundary {
				case "intent":
					h.stateBindings.durable.beforeCommit = func() error { return syscall.ENOSPC }
				case "capacity":
					h.conversationHistory.config.MaxHistoryBytes = 32
				}
				response := conversationPOST(t, h, map[string]any{"input": "Seed.", "stream": stream}, nil)
				body := response.Body.String()
				if strings.Contains(body, `"history":"saved"`) || strings.Contains(body, `"status":"completed"`) || strings.Contains(body, `"type":"response.completed"`) {
					t.Fatalf("unrecorded completion exposed: %d %s", response.Code, body)
				}
				if !strings.Contains(body, "storage_unavailable") && !strings.Contains(body, "capacity_exceeded") {
					t.Fatalf("missing storage failure: %d %s", response.Code, body)
				}
				if (boundary == "intent" || boundary == "capacity") && sends.Load() != 0 || sends.Load() > 1 {
					t.Fatalf("storage failure dispatched/retried: %d", sends.Load())
				}
			})
		}
	}
}

func TestConversationMigrationDuplicateResponseIDIsContained(t *testing.T) {
	for _, duplicateID := range []string{"affected-seed", "healthy-seed"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", duplicateID, stream), func(t *testing.T) {
				var sends atomic.Int32
				transport := routeExecutorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
					if strings.HasSuffix(req.URL.Path, "/models") {
						return routeExecutorTestResponse(req, 200, nil, `{"data":[]}`), nil
					}
					if strings.HasPrefix(req.URL.Host, "west.") {
						t.Error("response ID collision triggered migration")
					}
					number := sends.Add(1)
					id := fmt.Sprintf("healthy-%d", number)
					switch number {
					case 1:
						id = "affected-seed"
					case 2:
						id = "healthy-seed"
					case 3:
						id = duplicateID
					}
					return conversationResponse(t, req, id, conversationText(fmt.Sprintf("Answer %d.", number))), nil
				})
				h, cfg := newConversationAPIHandler(t, transport, nil)
				var snapshots []*conversationSnapshot
				for _, id := range []string{"affected-seed", "healthy-seed"} {
					conversationCompleted(t, conversationPOST(t, h, map[string]any{"input": id}, nil), false)
					snapshot, err := h.conversationHistory.lookupResponse("azure", id)
					if err != nil {
						t.Fatal(err)
					}
					snapshots = append(snapshots, snapshot)
				}
				response := conversationPOST(t, h, map[string]any{"previous_response_id": "affected-seed", "input": "Continue.", "stream": stream}, nil)
				body := response.Body.String()
				if !strings.Contains(body, "conversation_execution_uncertain") || !stream && response.Code != http.StatusConflict {
					t.Fatalf("collision did not report execution uncertainty: %d %s", response.Code, body)
				}
				if strings.Contains(body, `"history":"saved"`) || strings.Contains(body, `"status":"completed"`) || sends.Load() != 3 {
					t.Fatalf("colliding completion was exposed or retried: %d %s, sends=%d", response.Code, body, sends.Load())
				}
				healthyID := "healthy-seed"
				for _, reopen := range []bool{false, true} {
					if reopen {
						stopConversationAPIHandler(t, h)
						h, _ = newConversationAPIHandler(t, transport, &cfg)
					}
					for _, snapshot := range snapshots {
						requireConversationHistoryStorageSnapshot(t, h.conversationHistory, snapshot)
					}
					before := sends.Load()
					retry := conversationPOST(t, h, map[string]any{"previous_response_id": "affected-seed", "input": "Retry."}, nil)
					if retry.Code != http.StatusConflict || !strings.Contains(retry.Body.String(), "conversation_execution_uncertain") || sends.Load() != before {
						t.Fatalf("collision lost its pending marker after reopen=%t: %d %s, sends=%d", reopen, retry.Code, retry.Body.String(), sends.Load())
					}
					healthy := conversationCompleted(t, conversationPOST(t, h, map[string]any{
						"previous_response_id": healthyID, "input": "Continue the healthy conversation.", "stream": stream,
					}, nil), stream)
					healthyID = rawJSONString(healthy["id"])
					if sends.Load() != before+1 {
						t.Fatalf("healthy continuation sends=%d, want %d", sends.Load(), before+1)
					}
				}
			})
		}
	}
}

func TestConversationMigrationSaveFailureAccounting(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, boundary := range []string{"storage", "capacity"} {
			t.Run(fmt.Sprintf("stream=%t/%s", stream, boundary), func(t *testing.T) {
				var h *ProxyHandler
				var sends atomic.Int32
				transport := routeExecutorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
					if strings.HasSuffix(req.URL.Path, "/models") {
						return routeExecutorTestResponse(req, 200, nil, `{"data":[]}`), nil
					}
					sends.Add(1)
					if boundary == "storage" {
						// Commit the response's ownership proof, then fail its history save.
						commits := 0
						h.stateBindings.durable.mu.Lock()
						h.stateBindings.durable.beforeCommit = func() error {
							commits++
							if commits > 1 {
								return syscall.ENOSPC
							}
							return nil
						}
						h.stateBindings.durable.mu.Unlock()
					}
					response := conversationResponse(t, req, "unsaved-history", conversationText(strings.Repeat("answer ", 1024)))
					response.Header.Del("X-Codex-Turn-State")
					return response, nil
				})
				h, _ = newConversationAPIHandler(t, transport, nil)
				if boundary == "capacity" {
					h.conversationHistory.config.MaxHistoryBytes = 2048
				}
				ctx, summary := WithRequestSummary(t.Context())
				req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(fmt.Sprintf(`{"model":"coding","input":"Seed.","stream":%t}`, stream))).WithContext(ctx)
				response := httptest.NewRecorder()
				h.HandleResponses(response, req)
				code := "conversation_history_storage_unavailable"
				if boundary == "capacity" {
					code = "conversation_history_capacity_exceeded"
				}
				if !strings.Contains(response.Body.String(), code) || strings.Contains(response.Body.String(), `"history":"saved"`) || sends.Load() != 1 {
					t.Fatalf("history save failure = %d %s, sends=%d", response.Code, response.Body.String(), sends.Load())
				}
				if summary.FailureStatus() != http.StatusServiceUnavailable || !stream && response.Code != http.StatusServiceUnavailable {
					t.Fatalf("history save failure accounting: HTTP=%d summary=%d", response.Code, summary.FailureStatus())
				}
			})
		}
	}
}

func TestConversationMigrationRequiresOriginalOwnershipProof(t *testing.T) {
	for _, stored := range []bool{false, true} {
		for _, change := range []string{"pruned", "conflicting"} {
			t.Run(fmt.Sprintf("stored=%t/%s", stored, change), func(t *testing.T) {
				var sends atomic.Int32
				transport := routeExecutorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
					if strings.HasSuffix(req.URL.Path, "/models") {
						return routeExecutorTestResponse(req, 200, nil, `{"data":[]}`), nil
					}
					sends.Add(1)
					return conversationResponse(t, req, "ownership-seed", conversationText("original answer")), nil
				})
				h, cfg := newConversationAPIHandler(t, transport, nil)
				h.stateBindings.durable.now = func() time.Time { return time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC) }
				conversationCompleted(t, conversationPOST(t, h, map[string]any{"input": "Seed.", "store": stored}, nil), false)
				if change == "conflicting" {
					snapshot, err := h.conversationHistory.lookupResponse("azure", "ownership-seed")
					if err != nil {
						t.Fatal(err)
					}
					owner := stateBindingOwner{routeID: snapshot.RouteID, targetID: snapshot.TargetID, identity: snapshot.Identity}
					owner.identity[0] ^= 1
					if result := h.stateBindings.bindAll([]stateBindingToken{{stateBindingTypeResponseID, "ownership-seed"}}, owner); result.err != nil {
						t.Fatal(result.err)
					}
				}
				stopConversationAPIHandler(t, h)
				if change == "pruned" {
					if _, err := PruneDurableStateBindings(cfg.StateBindings.File, time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC)); err != nil {
						t.Fatal(err)
					}
				}
				h, _ = newConversationAPIHandler(t, transport, &cfg)
				if _, err := h.conversationHistory.lookupResponse("azure", "ownership-seed"); err != nil {
					t.Fatalf("ownership change unexpectedly removed history: %v", err)
				}
				response := conversationPOST(t, h, map[string]any{"previous_response_id": "ownership-seed", "input": "Continue."}, nil)
				if response.Code != 400 || sends.Load() != 1 {
					t.Fatalf("snapshot bypassed %s ownership proof: %d %s sends=%d", change, response.Code, response.Body.String(), sends.Load())
				}
			})
		}
	}
}
