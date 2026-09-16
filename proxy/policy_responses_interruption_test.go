package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sozercan/vekil/auth"
)

type policyResponsesInterruptionFixture struct {
	handler     *ProxyHandler
	upstream    *policyTurnsUpstream
	history     []json.RawMessage
	activeCalls []responsesChatReplayProjectedCall
}

func newPolicyResponsesInterruptionFixture(t *testing.T) policyResponsesInterruptionFixture {
	t.Helper()
	upstream := newPolicyTurnsUpstream(t)
	cfg := directCopilotResponsesPolicyConfig(policyConfigModeEnforce)
	cfg.Providers[0].IncludeModels = []string{"gpt-5.6-sol"}
	cfg.ModelRoutes[0].Targets[0].UpstreamModel = "gpt-5.6-sol"
	cfg.ModelRoutes[0].ReasoningEffort = []string{"low"}
	cfg.ModelRoutes[1].ReasoningEffort = []string{"max"}
	cfg.PolicyProfiles[0].Lightweight.ReasoningEffort = "low"
	cfg.PolicyProfiles[0].Powerful.ReasoningEffort = "max"
	h, err := NewProxyHandler(auth.NewTestAuthenticator("fixture-token"), nil,
		WithCopilotBaseURL(upstream.server.URL), WithProvidersConfig(cfg))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.BeginShutdown)
	if err := h.InitializePolicyRouting(t.Context()); err != nil {
		t.Fatal(err)
	}
	profile := h.policyRoutingController.(*chatPolicyRoutingController).profiles["semantic-policy"]
	routes := policyCompletedReplayRoutes(profile)
	if len(routes) != 2 {
		t.Fatalf("fixture completed replay routes = %+v, want both tiers", routes)
	}
	publish := func(route responsesChatReplayRoute, tag string, count int) responsesChatReplayPublished {
		specs := make([]replayTestCallSpec, count)
		for index := range specs {
			specs[index] = replayTestCallSpec{
				upstreamID: fmt.Sprintf("upstream-%s-%d", tag, index),
				name:       "lookup_synthetic_widget",
				visible:    fmt.Sprintf(`{"widget":"%d"}`, index),
			}
		}
		request := newResponsesChatReplayTestRequest(tag, specs...)
		request.Route = route
		published, err := h.responsesChatReplayStore().Publish(request)
		if err != nil {
			t.Fatal(err)
		}
		return published
	}
	low := publish(routes[0], "public-interruption-low", 1)
	max := publish(routes[1], "public-interruption-max", 2)
	appendCalls := func(history []json.RawMessage, published responsesChatReplayPublished) []json.RawMessage {
		for _, call := range published.Projection.Calls {
			history = append(history, marshalPolicyFactTestBody(t, map[string]any{
				"type": "function_call", "call_id": call.ID, "name": call.Name, "arguments": call.Arguments,
			}))
		}
		return history
	}
	history := appendCalls([]json.RawMessage{policyTurnsMessage(t, "user", "Look up the config.")}, low)
	history = append(history,
		marshalPolicyFactTestBody(t, map[string]any{
			"type": "function_call_output", "call_id": low.Projection.Calls[0].ID, "output": "valid config",
		}),
		policyTurnsMessage(t, "assistant", "The config is valid."),
		policyTurnsMessage(t, "user", policyTurnsComplexTask),
	)
	history = appendCalls(history, max)
	return policyResponsesInterruptionFixture{
		handler: h, upstream: upstream, history: history,
		activeCalls: max.Projection.Calls,
	}
}

func (f policyResponsesInterruptionFixture) requestBody(t *testing.T, results []int, interrupt, stream bool) []byte {
	t.Helper()
	history := append([]json.RawMessage(nil), f.history...)
	for _, index := range results {
		history = append(history, marshalPolicyFactTestBody(t, map[string]any{
			"type": "function_call_output", "call_id": f.activeCalls[index].ID, "output": fmt.Sprintf("result %d", index),
		}))
	}
	if interrupt {
		history = append(history, policyTurnsMessage(t, "user", "Thanks."))
	}
	return marshalPolicyFactTestBody(t, map[string]any{
		"model": "gpt-5.6-semantic", "input": history, "stream": stream, "store": false,
		"reasoning": map[string]any{"effort": "low"},
		"tools": []any{map[string]any{
			"type": "function", "name": "lookup_synthetic_widget", "parameters": map[string]any{
				"type": "object", "properties": map[string]any{"widget": map[string]any{"type": "string"}},
			},
		}},
	})
}

func TestPolicyResponsesIngressCompleteResultsWithUserInterruptionRetainMax(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, reversed := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%t/reversed=%t", stream, reversed), func(t *testing.T) {
				fixture := newPolicyResponsesInterruptionFixture(t)
				factsBefore, bodiesBefore := fixture.upstream.snapshot()
				results := []int{0, 1}
				if reversed {
					results = []int{1, 0}
				}
				body := fixture.requestBody(t, results, true, stream)
				recorder := httptest.NewRecorder()
				fixture.handler.HandleResponses(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body)))
				if recorder.Code != http.StatusOK {
					t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
				}
				response := decodePolicyTurnsResponse(t, recorder, stream)
				if response.Model != "gpt-5.6-semantic" || response.Status != "completed" {
					t.Fatalf("response model=%q status=%q", response.Model, response.Status)
				}
				facts, bodies := fixture.upstream.snapshot()
				if len(facts) != len(factsBefore) || len(bodies) != len(bodiesBefore)+1 {
					t.Fatalf("classifier/terminal sends changed from %d/%d to %d/%d, want no classifier and one terminal", len(factsBefore), len(bodiesBefore), len(facts), len(bodies))
				}
				terminal := bodies[len(bodies)-1]
				var sent struct {
					Model     string `json:"model"`
					Reasoning struct {
						Effort string `json:"effort"`
					} `json:"reasoning"`
				}
				if err := json.Unmarshal([]byte(terminal), &sent); err != nil {
					t.Fatal(err)
				}
				if sent.Model != "gpt-5.6-sol" || sent.Reasoning.Effort != "max" {
					t.Fatalf("upstream model/effort=%q/%q, want gpt-5.6-sol/max", sent.Model, sent.Reasoning.Effort)
				}
				for _, itemID := range []string{"reasoning_public-interruption-low", "reasoning_public-interruption-max"} {
					if count := strings.Count(terminal, itemID); count != 1 {
						t.Fatalf("replayed reasoning item %q appears %d times", itemID, count)
					}
				}
			})
		}
	}
}

// Public policy requests require every parallel result before the next message
// or request end, even though the underlying Chat-over-Responses seam accepts
// partial results for non-policy callers.
func TestPolicyResponsesIngressPartialResultsFailBeforeDispatch(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, interrupt := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%t/interrupt=%t", stream, interrupt), func(t *testing.T) {
				fixture := newPolicyResponsesInterruptionFixture(t)
				factsBefore, bodiesBefore := fixture.upstream.snapshot()
				body := fixture.requestBody(t, []int{0}, interrupt, stream)
				recorder := httptest.NewRecorder()
				fixture.handler.HandleResponses(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body)))
				if recorder.Code != http.StatusBadRequest {
					t.Fatalf("status=%d body=%s, want 400", recorder.Code, recorder.Body.String())
				}
				var response struct {
					Error struct {
						Type    string `json:"type"`
						Message string `json:"message"`
					} `json:"error"`
				}
				if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
					t.Fatal(err)
				}
				if response.Error.Type != "invalid_request_error" ||
					!strings.Contains(response.Error.Message, "missing a following function_call_output") {
					t.Fatalf("error=%+v, want missing function call output", response.Error)
				}
				facts, bodies := fixture.upstream.snapshot()
				if len(facts) != len(factsBefore) || len(bodies) != len(bodiesBefore) {
					t.Fatalf("rejected partial history sent traffic: classifier/terminal sends changed from %d/%d to %d/%d", len(factsBefore), len(bodiesBefore), len(facts), len(bodies))
				}
			})
		}
	}
}
