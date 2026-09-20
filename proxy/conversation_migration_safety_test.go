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
)

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

func TestConversationMigrationHasOneTransitionAndSharedBudget(t *testing.T) {
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
	if failed.Code < 400 || east.Load() != 2 || west.Load() != 1 || third.Load() != 0 || strings.Contains(failed.Body.String(), `"migration":"completed"`) {
		t.Fatalf("more than one transition or false completion: %d %s east=%d west=%d third=%d", failed.Code, failed.Body.String(), east.Load(), west.Load(), third.Load())
	}
	// Both sends provably failed before execution. The original branch remains
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
