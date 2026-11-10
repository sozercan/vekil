package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/sozercan/vekil/auth"
)

const policyTurnsComplexTask = "Plan a coordinated migration across the authentication, storage, and API modules."

type policyTurnsUpstream struct {
	server *httptest.Server
	mu     sync.Mutex
	facts  []policyClassifierFacts
	bodies []string
}

func newPolicyTurnsUpstream(t *testing.T) *policyTurnsUpstream {
	t.Helper()
	toolFixture := string(readResponsesChatStreamFixture(t, "stream_reasoning_tool_call.sse"))
	textFixture := string(readResponsesChatStreamFixture(t, "stream_text.sse"))
	upstream := &policyTurnsUpstream{}
	upstream.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-token" {
			http.Error(w, "missing fixture auth", http.StatusUnauthorized)
			return
		}
		if r.URL.Path == providerEndpointModels {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{
				map[string]any{"id": "gpt-5.6-sol", "object": "model", "supported_endpoints": []string{providerEndpointResponses}},
			}})
			return
		}
		if r.URL.Path != providerEndpointResponses {
			http.NotFound(w, r)
			return
		}
		defer func() { _ = r.Body.Close() }()
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var request struct {
			Model string `json:"model"`
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
			Input []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"input"`
		}
		if err := json.Unmarshal(body, &request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		for _, tool := range request.Tools {
			if tool.Name != policyClassifierToolName {
				continue
			}
			var facts policyClassifierFacts
			for _, item := range request.Input {
				if item.Role == "user" {
					var content []struct {
						Text string `json:"text"`
					}
					if err := json.Unmarshal(item.Content, &content); err != nil || len(content) != 1 {
						http.Error(w, "invalid classifier facts message", http.StatusBadRequest)
						return
					}
					if err := json.Unmarshal([]byte(content[0].Text), &facts); err != nil {
						http.Error(w, err.Error(), http.StatusBadRequest)
						return
					}
				}
			}
			if facts.CurrentUserTask == nil {
				http.Error(w, "missing current user task", http.StatusBadRequest)
				return
			}
			upstream.mu.Lock()
			upstream.facts = append(upstream.facts, facts)
			upstream.mu.Unlock()
			signals := policyClassifierSignals{TurnType: policyTurnTypeChitchat, CodeScope: policyCodeScopeNone, RiskLevel: policyRiskLevelLow}
			if facts.CurrentUserTask.Text == policyTurnsComplexTask {
				signals = policyClassifierSignals{TurnType: policyTurnTypePlanning, CodeScope: policyCodeScopeCrossModule, RiskLevel: policyRiskLevelHigh, RequiresCodebaseContext: true}
			}
			arguments, _ := json.Marshal(signals)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "resp-classifier", "object": "response", "created_at": 1, "status": "completed",
				"output": []any{map[string]any{
					"type": "function_call", "id": "fc-classifier", "call_id": "call-classifier",
					"name": policyClassifierToolName, "arguments": string(arguments), "status": "completed",
				}},
			})
			return
		}

		upstream.mu.Lock()
		upstream.bodies = append(upstream.bodies, string(body))
		requestNumber := len(upstream.bodies)
		upstream.mu.Unlock()
		fixture := textFixture
		if requestNumber == 1 || requestNumber == 4 {
			fixture = strings.ReplaceAll(toolFixture, "synthetic_encrypted_content_not_replayable", fmt.Sprintf("encrypted_history_%d", requestNumber))
		}
		fixture = strings.ReplaceAll(fixture, "_001", fmt.Sprintf("_%03d", requestNumber))
		fixture = strings.ReplaceAll(fixture, "gpt-synthetic-responses", request.Model)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, fixture)
	}))
	t.Cleanup(upstream.server.Close)
	return upstream
}

func (u *policyTurnsUpstream) snapshot() ([]policyClassifierFacts, []string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]policyClassifierFacts(nil), u.facts...), append([]string(nil), u.bodies...)
}

// Exercise the stateless Responses history that Codex sends, including the
// extra model request after a skill read and old tool groups from both tiers.
func TestPolicyResponsesIngressReclassifiesCompletedToolTurns(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
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

			history := []json.RawMessage{policyTurnsMessage(t, "user", "# AGENTS.md instructions\n"+strings.Repeat("Repository instructions. ", 800))}
			steps := []struct {
				task        string
				effort      string
				classifiers int
				toolCall    bool
				hiddenTurns []int
			}{
				{task: "hello how are you", effort: "low", classifiers: 2, toolCall: true},
				{effort: "low", classifiers: 2, hiddenTurns: []int{1}},
				{task: "which reasoning are you runninbg now", effort: "low", classifiers: 3, hiddenTurns: []int{1}},
				{task: policyTurnsComplexTask, effort: "max", classifiers: 4, toolCall: true, hiddenTurns: []int{1}},
				{effort: "max", classifiers: 4, hiddenTurns: []int{1, 4}},
				{task: "What does HTTP stand for?", effort: "low", classifiers: 5, hiddenTurns: []int{1, 4}},
			}
			for index, step := range steps {
				if step.task != "" {
					history = append(history, policyTurnsMessage(t, "user", step.task))
				}
				clientEffort := "max"
				if step.effort == "max" {
					clientEffort = "low"
				}
				body := marshalPolicyFactTestBody(t, map[string]any{
					"model": "gpt-5.6-semantic", "input": history, "stream": stream, "store": false,
					"instructions": strings.Repeat("Coding agent setup and tool instructions. ", 1000),
					"reasoning":    map[string]any{"effort": clientEffort, "summary": "auto"},
					"tools": []any{map[string]any{
						"type": "function", "name": "lookup_synthetic_widget", "parameters": map[string]any{
							"type": "object", "properties": map[string]any{"widget": map[string]any{"type": "string"}},
						},
					}},
				})
				recorder := httptest.NewRecorder()
				h.HandleResponses(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body)))
				if recorder.Code != http.StatusOK {
					t.Fatalf("request %d: status=%d body=%s", index+1, recorder.Code, recorder.Body.String())
				}
				response := decodePolicyTurnsResponse(t, recorder, stream)
				if response.Model != "gpt-5.6-semantic" || response.Status != "completed" {
					t.Fatalf("request %d: model=%q status=%q", index+1, response.Model, response.Status)
				}
				facts, terminalBodies := upstream.snapshot()
				if len(facts) != step.classifiers || len(terminalBodies) != index+1 {
					t.Fatalf("request %d: classifiers=%d terminal sends=%d, want %d/%d", index+1, len(facts), len(terminalBodies), step.classifiers, index+1)
				}
				if step.task != "" {
					fact := facts[len(facts)-1]
					if fact.CurrentUserTask.Text != step.task || fact.Truncation.CurrentUserTask || !fact.Truncation.Anchors {
						t.Fatalf("request %d: current task=%+v truncation=%+v", index+1, fact.CurrentUserTask, fact.Truncation)
					}
				}
				terminal := terminalBodies[len(terminalBodies)-1]
				var sent struct {
					Model     string `json:"model"`
					Reasoning struct {
						Effort string `json:"effort"`
					} `json:"reasoning"`
				}
				if err := json.Unmarshal([]byte(terminal), &sent); err != nil {
					t.Fatal(err)
				}
				if sent.Model != "gpt-5.6-sol" || sent.Reasoning.Effort != step.effort {
					t.Fatalf("request %d: upstream model=%q effort=%q, want gpt-5.6-sol/%s", index+1, sent.Model, sent.Reasoning.Effort, step.effort)
				}
				for _, hiddenTurn := range step.hiddenTurns {
					if got := strings.Count(terminal, fmt.Sprintf("encrypted_history_%d", hiddenTurn)); got != 1 {
						t.Fatalf("request %d: encrypted history from request %d appears %d times", index+1, hiddenTurn, got)
					}
					if !strings.Contains(terminal, fmt.Sprintf("call_synth_lookup_stream_%03d", hiddenTurn)) {
						t.Fatalf("request %d: upstream call ID from request %d was not restored", index+1, hiddenTurn)
					}
				}
				history = append(history, response.Output...)
				calls := 0
				for _, raw := range response.Output {
					var item struct {
						Type   string `json:"type"`
						CallID string `json:"call_id"`
					}
					if err := json.Unmarshal(raw, &item); err != nil {
						t.Fatal(err)
					}
					if item.Type == "function_call" {
						calls++
						if !strings.HasPrefix(item.CallID, "call_vekil_") {
							t.Fatalf("request %d: missing proxy replay ID: %q", index+1, item.CallID)
						}
						history = append(history, marshalPolicyFactTestBody(t, map[string]any{
							"type": "function_call_output", "call_id": item.CallID, "output": "synthetic tool result",
						}))
					}
				}
				if (calls == 1) != step.toolCall || calls > 1 {
					t.Fatalf("request %d: tool calls=%d, want tool call=%t", index+1, calls, step.toolCall)
				}
			}
		})
	}
}

func policyTurnsMessage(t *testing.T, role, text string) json.RawMessage {
	t.Helper()
	return marshalPolicyFactTestBody(t, map[string]any{"type": "message", "role": role, "content": text})
}

type policyTurnsResponse struct {
	Model  string            `json:"model"`
	Status string            `json:"status"`
	Output []json.RawMessage `json:"output"`
}

func decodePolicyTurnsResponse(t *testing.T, recorder *httptest.ResponseRecorder, stream bool) policyTurnsResponse {
	t.Helper()
	var response policyTurnsResponse
	if !stream {
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		return response
	}
	completed := 0
	if err := consumeResponsesSSEMessages(strings.NewReader(recorder.Body.String()), func(message responsesSSEMessage) error {
		var event struct {
			Type     string              `json:"type"`
			Response policyTurnsResponse `json:"response"`
		}
		if err := json.Unmarshal([]byte(message.data), &event); err != nil {
			return err
		}
		if event.Type == "response.completed" {
			completed++
			response = event.Response
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if completed != 1 {
		t.Fatalf("completed events=%d: %s", completed, recorder.Body.String())
	}
	return response
}
