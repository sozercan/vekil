package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

func newNamespaceTestTranslation(t *testing.T) (*responsesNamespaceTranslation, string) {
	t.Helper()
	_, tr, err := translateCopilotNamespaceTools([]byte(`{"model":"grok-4.7","tools":` + namespaceTestCatalog + `}`))
	if err != nil {
		t.Fatal(err)
	}
	alias := policyResponsesStableToolAlias(policyResponsesToolDescriptor{Kind: "function", Name: "run", Namespace: "shell"})
	if _, ok := tr.tools[alias]; !ok {
		t.Fatalf("alias %q not registered", alias)
	}
	return tr, alias
}

func TestCopilotNamespaceRestorePreservesAmbiguousPayloads(t *testing.T) {
	tr, alias := newNamespaceTestTranslation(t)
	call := fmt.Sprintf(`{"type":"function_call","id":"fc-1","id":"fc-2","call_id":"call-1","name":%q,"arguments":"{}"}`, alias)
	for name, data := range map[string]string{
		"json":     `{"output":[` + call + `]}`,
		"terminal": `{"type":"response.completed","response":{"output":[` + call + `]}}`,
		"item":     `{"type":"response.output_item.done","item":` + call + `}`,
	} {
		t.Run(name, func(t *testing.T) {
			got, changed := tr.restore([]byte(data), "")
			if changed || string(got) != data {
				t.Fatalf("ambiguous payload rewritten: changed=%v %s", changed, got)
			}
		})
	}

	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"output":[` + call + `]}`))}
	req := httptest.NewRequest(http.MethodPost, "/responses", nil)
	req = req.WithContext(context.WithValue(req.Context(), responsesNamespaceContextKey{}, tr))
	restoreCopilotNamespaceResponse(req, resp)
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"output":[`+call+`]}` {
		t.Fatalf("ambiguous JSON body rewritten: %s", raw)
	}
}

func TestCopilotNamespaceRestoresResponseToolChoice(t *testing.T) {
	tr, alias := newNamespaceTestTranslation(t)
	for name, choice := range map[string]string{
		"function": fmt.Sprintf(`{"type":"function","name":%q}`, alias),
		"allowed":  fmt.Sprintf(`{"type":"allowed_tools","mode":"required","tools":[{"type":"function","name":%q},{"type":"function","name":"unrelated"}]}`, alias),
	} {
		for _, wrap := range []string{"json", "lifecycle"} {
			t.Run(name+"/"+wrap, func(t *testing.T) {
				obj := `{"tools":[],"tool_choice":` + choice + `}`
				if wrap == "lifecycle" {
					obj = `{"type":"response.created","response":` + obj + `}`
				}
				got, changed := tr.restore([]byte(obj), "")
				if !changed {
					t.Fatalf("not changed: %s", got)
				}
				var root map[string]json.RawMessage
				if err := json.Unmarshal(got, &root); err != nil {
					t.Fatal(err)
				}
				if wrap == "lifecycle" {
					var event struct {
						Response map[string]json.RawMessage `json:"response"`
					}
					if err := json.Unmarshal(got, &event); err != nil {
						t.Fatal(err)
					}
					root = event.Response
				}
				restored := string(root["tool_choice"])
				if strings.Contains(restored, alias) || !strings.Contains(restored, `"namespace":"shell"`) || !strings.Contains(restored, `"name":"run"`) {
					t.Fatalf("tool_choice not restored: %s", restored)
				}
				if name == "allowed" && !strings.Contains(restored, `"name":"unrelated"`) {
					t.Fatalf("unrelated allowed tool changed: %s", restored)
				}
			})
		}
	}
}

func TestCopilotNamespaceSSEContentTypeIsCaseInsensitive(t *testing.T) {
	tr, alias := newNamespaceTestTranslation(t)
	pr, pw := io.Pipe()
	defer func() { _ = pw.Close() }()
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"Text/Event-Stream; charset=utf-8"}}, Body: pr}
	req := httptest.NewRequest(http.MethodPost, "/responses", nil)
	req = req.WithContext(context.WithValue(req.Context(), responsesNamespaceContextKey{}, tr))
	restoreCopilotNamespaceResponse(req, resp)

	go func() {
		_, _ = fmt.Fprintf(pw, "data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"function_call\",\"id\":\"fc-1\",\"call_id\":\"call-1\",\"name\":%q,\"arguments\":\"\"}}\n\n", alias)
	}()
	// The stream must deliver events before EOF; the JSON branch would block.
	done := make(chan struct{})
	var got []byte
	var readErr error
	go func() {
		defer close(done)
		buf := make([]byte, 4096)
		for !strings.Contains(string(got), "\n\n") && readErr == nil {
			var n int
			n, readErr = resp.Body.Read(buf)
			got = append(got, buf[:n]...)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("SSE event was not delivered incrementally")
	}
	if readErr != nil {
		t.Fatal(readErr)
	}
	if strings.Contains(string(got), alias) || !strings.Contains(string(got), `"namespace":"shell"`) {
		t.Fatalf("SSE event not restored: %s", got)
	}
}

func TestCopilotNamespaceAcceptsOptionalFunctionFields(t *testing.T) {
	catalog := `[{"type":"function","name":"noargs"},{"type":"function","name":"nullargs","parameters":null,"strict":null},{"type":"namespace","name":"shell","tools":[{"type":"function","name":"bare"},{"type":"function","name":"loose","parameters":{"type":"object"},"strict":null}]}]`
	raw := []byte(`{"model":"grok-4.7","tools":` + catalog + `,"input":[{"type":"additional_tools","tools":` + catalog + `}]}`)
	body, tr, err := translateCopilotNamespaceTools(raw)
	if err != nil {
		t.Fatalf("valid optional fields rejected: %v", err)
	}
	if len(tr.tools) != 4 {
		t.Fatalf("tools=%d", len(tr.tools))
	}
	var root struct {
		Tools []map[string]json.RawMessage `json:"tools"`
		Input []struct {
			Tools []map[string]json.RawMessage `json:"tools"`
		} `json:"input"`
	}
	if err := json.Unmarshal(body, &root); err != nil {
		t.Fatal(err)
	}
	alias := func(name, namespace string) string {
		return policyResponsesStableToolAlias(policyResponsesToolDescriptor{Kind: "function", Name: name, Namespace: namespace})
	}
	for label, tools := range map[string][]map[string]json.RawMessage{"top-level": root.Tools, "additional_tools": root.Input[0].Tools} {
		byName := make(map[string]map[string]json.RawMessage, len(tools))
		for _, tool := range tools {
			byName[rawJSONString(tool["name"])] = tool
		}
		check := func(name string, wantParams, wantStrict string) {
			t.Helper()
			tool, ok := byName[name]
			if !ok {
				t.Fatalf("%s: alias %q missing from %v", label, name, tools)
			}
			for field, want := range map[string]string{"parameters": wantParams, "strict": wantStrict} {
				got, present := tool[field]
				if want == "" {
					if present {
						t.Errorf("%s: %s gained %s=%s", label, name, field, got)
					}
				} else if string(got) != want {
					t.Errorf("%s: %s %s=%s, want %s", label, name, field, got, want)
				}
			}
		}
		check(alias("noargs", ""), "", "")
		check(alias("nullargs", ""), "null", "null")
		check(alias("bare", "shell"), "", "")
		check(alias("loose", "shell"), `{"type":"object"}`, "null")
	}
}

func TestCopilotNamespaceSSEFailsClosedOnOversizedEvents(t *testing.T) {
	tr, alias := newNamespaceTestTranslation(t)
	first := fmt.Sprintf("data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"function_call\",\"id\":\"fc-1\",\"call_id\":\"call-1\",\"name\":%q,\"arguments\":\"\"}}\n\n", alias)
	call := fmt.Sprintf(`{"type":"function_call","id":"fc-1","call_id":"call-1","name":%q,"arguments":%q}`, alias, strings.Repeat("x", openAIStreamScannerMaxBuffer))
	for name, oversized := range map[string]string{
		"long line":  "data: {\"type\":\"response.output_item.done\",\"item\":" + call + "}\n\n",
		"many lines": "data: {\"type\":\"response.output_item.done\",\n" + strings.Repeat("data: \n", openAIStreamScannerMaxBuffer/7+1) + "data: \"item\":" + fmt.Sprintf(`{"type":"function_call","name":%q}`, alias) + "}\n\n",
	} {
		t.Run(name, func(t *testing.T) {
			resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(first + oversized + first))}
			req := httptest.NewRequest(http.MethodPost, "/responses", nil)
			req = req.WithContext(context.WithValue(req.Context(), responsesNamespaceContextKey{}, tr))
			restoreCopilotNamespaceResponse(req, resp)
			got, err := io.ReadAll(resp.Body)
			if !errors.Is(err, errResponsesSSEEventTooLarge) {
				t.Fatalf("err=%v, want errResponsesSSEEventTooLarge", err)
			}
			if strings.Contains(string(got), alias) {
				t.Fatal("unrewritten alias exposed to client")
			}
			if !strings.Contains(string(got), `"namespace":"shell"`) {
				t.Fatalf("events before the oversized one were not delivered: %.200s", got)
			}
			// The failure is terminal; later reads must not resume the stream.
			if n, err := resp.Body.Read(make([]byte, 1024)); n != 0 || !errors.Is(err, errResponsesSSEEventTooLarge) {
				t.Fatalf("read after failure n=%d err=%v", n, err)
			}
		})
	}

	// Requests without tool aliases leave responses untouched.
	empty := &responsesNamespaceTranslation{tools: make(policyResponsesToolMap)}
	body := io.NopCloser(strings.NewReader(first))
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: body}
	req := httptest.NewRequest(http.MethodPost, "/responses", nil)
	restoreCopilotNamespaceResponse(req.WithContext(context.WithValue(req.Context(), responsesNamespaceContextKey{}, empty)), resp)
	if resp.Body != body {
		t.Fatal("response without aliases was wrapped")
	}
}

func TestCopilotNamespaceRestoresSSEEventNameWithoutType(t *testing.T) {
	tr, alias := newNamespaceTestTranslation(t)
	call := fmt.Sprintf(`{"type":"function_call","call_id":"call-1","name":%q,"arguments":"{}"}`, alias)
	stream := fmt.Sprintf("event: response.output_item.added\ndata: {\"item\":%s}\n\nevent: response.function_call_arguments.done\ndata: {\"item_id\":\"fc-1\",\"name\":%q,\"arguments\":\"{}\"}\n\nevent: response.completed\ndata: {\"response\":{\"output\":[%s]}}\n\n", call, alias, call)
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream))}
	req := httptest.NewRequest(http.MethodPost, "/responses", nil)
	restoreCopilotNamespaceResponse(req.WithContext(context.WithValue(req.Context(), responsesNamespaceContextKey{}, tr)), resp)
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), alias) || strings.Count(string(got), `"namespace":"shell"`) != 3 {
		t.Fatalf("typeless events not restored: %s", got)
	}
}

func TestCopilotNamespaceDropsNullReasoningContent(t *testing.T) {
	raw := []byte(`{"input":[{"type":"reasoning","summary":[],"content":null,"encrypted_content":"opaque"},{"type":"reasoning","summary":[],"content":[{"type":"reasoning_text","text":"kept"}]},{"type":"message","role":"user","content":null}]}`)
	body, _, err := translateCopilotNamespaceTools(raw)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Input []map[string]json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Input[0]["content"]; ok || rawJSONString(got.Input[0]["encrypted_content"]) != "opaque" {
		t.Fatalf("null reasoning content not dropped: %s", got.Input[0])
	}
	if string(got.Input[1]["content"]) != `[{"type":"reasoning_text","text":"kept"}]` {
		t.Fatalf("reasoning content changed: %s", got.Input[1]["content"])
	}
	// Only reasoning items are normalized.
	if string(got.Input[2]["content"]) != "null" {
		t.Fatalf("message changed: %s", got.Input[2])
	}
}
