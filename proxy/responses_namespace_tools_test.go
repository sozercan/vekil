package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
)

const namespaceTestCatalog = `[{"type":"function","name":"run","parameters":{"type":"object"}},{"type":"namespace","name":"shell","description":"Shell tools","tools":[{"type":"function","name":"run","description":"Execute","parameters":{"type":"object"},"strict":false}]},{"type":"namespace","name":"browser","tools":[{"type":"function","name":"run","parameters":{"type":"object"}}]}]`

func TestCopilotNamespaceRoundTrip(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, explicitSend := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%v/explicit=%v", stream, explicitSend), func(t *testing.T) {
				sends := 0
				alias := ""
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					sends++
					var body struct {
						Tools  []map[string]json.RawMessage `json:"tools"`
						Input  []map[string]json.RawMessage `json:"input"`
						Choice map[string]json.RawMessage   `json:"tool_choice"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
						return
					}
					if len(body.Tools) != 3 {
						t.Errorf("tools=%v", body.Tools)
						w.WriteHeader(400)
						return
					}
					seen := map[string]bool{}
					for _, tool := range body.Tools {
						name := rawJSONString(tool["name"])
						if rawJSONString(tool["type"]) != "function" || seen[name] {
							t.Errorf("invalid flat tool %s", tool)
						}
						seen[name] = true
					}
					if sends == 1 {
						alias = rawJSONString(body.Tools[1]["name"])
					} else {
						if rawJSONString(body.Input[0]["name"]) != alias || len(body.Input[0]["namespace"]) != 0 || rawJSONString(body.Input[1]["call_id"]) != "call-1" {
							t.Errorf("replay lost identity: %s", body.Input)
						}
					}
					if rawJSONString(body.Choice["name"]) != alias {
						t.Errorf("choice not translated: %s", body.Choice)
					}
					// Alias-looking arguments are opaque and must not be rewritten.
					call := fmt.Sprintf(`{"type":"function_call","id":"fc-1","call_id":"call-1","name":%q,"arguments":%q}`, alias, `{"name":"`+alias+`"}`)
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.output_item.added\",\"item\":%s}\n\n", call)
						_, _ = fmt.Fprint(w, "data: {\"type\":\"response.function_call_arguments.delta\",\"delta\":\"{}\"}\n\n")
						_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.output_item.done\",\"item\":%s}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"output\":[%s]}}\n\n", call, call)
					} else {
						w.Header().Set("Content-Type", "application/json")
						_, _ = fmt.Fprintf(w, `{"output":[%s]}`, call)
					}
				}))
				defer upstream.Close()
				h, err := NewProxyHandler(auth.NewTestAuthenticator("test-token"), logger.New(logger.LevelError), WithCopilotBaseURL(upstream.URL))
				if err != nil {
					t.Fatal(err)
				}
				provider := &providerRuntime{kind: providerTypeCopilot, id: "copilot", baseURL: upstream.URL}
				for turn := 0; turn < 2; turn++ {
					input := `[]`
					if turn == 1 {
						input = `[{"type":"function_call","id":"fc-1","call_id":"call-1","namespace":"shell","name":"run","arguments":"{}"},{"type":"function_call_output","call_id":"call-1","output":"ok"}]`
					}
					body := []byte(fmt.Sprintf(`{"model":"grok-4.7","stream":%v,"tools":%s,"tool_choice":{"type":"function","namespace":"shell","name":"run"},"input":%s}`, stream, namespaceTestCatalog, input))
					req, err := h.newProviderJSONInferenceRequest(context.Background(), provider, http.MethodPost, providerEndpointResponses, body, nil, "")
					if err != nil {
						t.Fatal(err)
					}
					var resp *http.Response
					if explicitSend {
						resp, err = h.singleInferenceSend(req, nil)
					} else {
						resp, err = h.sendRetryRequest(req, true)
					}
					if err != nil {
						t.Fatal(err)
					}
					raw, err := io.ReadAll(resp.Body)
					_ = resp.Body.Close()
					if err != nil {
						t.Fatal(err)
					}
					if !strings.Contains(string(raw), `"namespace":"shell"`) || !strings.Contains(string(raw), `"name":"run"`) {
						t.Fatalf("identity not restored: %s", raw)
					}
					if !strings.Contains(string(raw), alias) {
						t.Fatalf("opaque arguments changed: %s", raw)
					}
				}
				if sends != 2 {
					t.Fatalf("sends=%d", sends)
				}
			})
		}
	}
}

func TestCopilotNamespaceCapabilityAndValidation(t *testing.T) {
	body := []byte(`{"model":"grok-4.7","tools":[{"type":"web_search"}]}`)
	for _, kind := range []providerType{providerTypeCopilot, providerTypeOpenAICompatible} {
		_, got, err := prepareCopilotNamespaceTools(context.Background(), &providerRuntime{kind: kind}, providerEndpointResponses, body)
		if kind == providerTypeCopilot {
			if err == nil || !strings.Contains(err.Error(), "disable hosted web search") {
				t.Fatalf("error=%v", err)
			}
		} else if err != nil || string(got) != string(body) {
			t.Fatalf("unrelated provider changed: %s %v", got, err)
		}
	}
	for _, model := range []string{"gpt-5.3-codex", "claude-sonnet-5"} {
		original := []byte(fmt.Sprintf(`{"model":%q,"tools":[{"type":"web_search"}]}`, model))
		_, got, err := prepareCopilotNamespaceTools(context.Background(), &providerRuntime{kind: providerTypeCopilot}, providerEndpointResponses, original)
		if err != nil || string(got) != string(original) {
			t.Fatalf("unrelated model changed: %s %v", got, err)
		}
	}
	for _, tools := range []string{`[{"type":"namespace","name":"n","tools":[{"type":"custom","name":"c"}]}]`, `[{"type":"function","name":"f","parameters":{},"defer_loading":true}]`} {
		if _, _, err := translateCopilotNamespaceTools([]byte(`{"tools":` + tools + `}`)); err == nil {
			t.Fatal("accepted unsupported tool")
		}
	}
}

func TestCopilotNamespaceAdditionalToolsAndAllowedChoice(t *testing.T) {
	raw := []byte(`{"input":[{"type":"additional_tools","tools":` + namespaceTestCatalog + `}],"tool_choice":{"type":"allowed_tools","mode":"required","tools":[{"type":"function","namespace":"browser","name":"run"}]}}`)
	body, tr, err := translateCopilotNamespaceTools(raw)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), `"type":"namespace"`) || strings.Contains(string(body), `"namespace":"browser"`) {
		t.Fatalf("untranslated catalog/choice: %s", body)
	}
	if len(tr.tools) != 3 {
		t.Fatalf("tools=%d", len(tr.tools))
	}
}
