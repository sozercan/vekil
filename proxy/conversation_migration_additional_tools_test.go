package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

func conversationToolCatalog(name string) map[string]any {
	return map[string]any{
		"type": "additional_tools", "id": "shared-catalog-id", "role": "developer",
		"tools": []any{map[string]any{
			"type": "namespace", "name": "local", "description": "Local tools",
			"tools": []any{map[string]any{"type": "custom", "name": name}},
		}},
	}
}

func TestConversationMigrationAdditionalToolsSurviveRestart(t *testing.T) {
	for _, stored := range []bool{true, false} {
		t.Run(fmt.Sprintf("stored=%t", stored), func(t *testing.T) {
			var outage atomic.Bool
			var sends atomic.Int32
			transport := routeExecutorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				if strings.HasSuffix(req.URL.Path, "/models") {
					return routeExecutorTestResponse(req, 200, nil, `{"data":[]}`), nil
				}
				if outage.Load() && strings.HasPrefix(req.URL.Host, "east.") {
					return nil, errors.New("prewrite outage")
				}
				body, _ := io.ReadAll(req.Body)
				req.Body = io.NopCloser(bytes.NewReader(body))
				var request struct {
					Input          []json.RawMessage `json:"input"`
					ClientMetadata map[string]string `json:"client_metadata"`
				}
				if err := json.Unmarshal(body, &request); err != nil {
					t.Fatal(err)
				}
				catalogs, _ := partitionResponsesAdditionalToolsInputItems(request.Input)
				if len(catalogs) != 1 || !bytes.Contains(catalogs[0], []byte("edit_once")) {
					t.Errorf("request lost or duplicated the inherited catalog: %s", body)
				}
				if request.ClientMetadata["thread_id"] != "catalog-client" {
					t.Error("request lost client attribution")
				}
				number := sends.Add(1)
				if number == 1 {
					return conversationResponse(t, req, "catalog-1", map[string]any{
						"type": "custom_tool_call", "call_id": "local-edit", "namespace": "local", "name": "edit_once", "input": "write cedar",
						"metadata": map[string]string{"turn_id": "east-attribution"},
						responsesInternalChatMessageMetadataPassthroughField: map[string]any{"turn_id": "east-attribution", "create_time": 1.5},
					}), nil
				}
				if number == 3 {
					for _, want := range []string{"custom_tool_call", "custom_tool_call_output", "local-edit", "wrote cedar once", "Completed edit."} {
						if !bytes.Contains(body, []byte(want)) {
							t.Errorf("migration lost %q", want)
						}
					}
					if bytes.Contains(body, []byte("previous_response_id")) || bytes.Contains(body, []byte("east-attribution")) || bytes.Contains(catalogs[0], []byte("shared-catalog-id")) {
						t.Error("migration forwarded source-owned IDs")
					}
				}
				return conversationResponse(t, req, fmt.Sprintf("catalog-%d", number), conversationText("Completed edit.")), nil
			})
			h, cfg := newConversationAPIHandler(t, transport, nil)
			post := func(fields map[string]any) map[string]json.RawMessage {
				fields["store"], fields["stream"] = stored, !stored
				fields["client_metadata"] = map[string]string{"thread_id": "catalog-client"}
				return conversationCompleted(t, conversationPOST(t, h, fields, nil), !stored)
			}
			post(map[string]any{"input": []any{conversationToolCatalog("edit_once"), map[string]any{"role": "user", "content": "Edit the file."}}})
			post(map[string]any{
				"previous_response_id": "catalog-1",
				"input":                []any{map[string]any{"type": "custom_tool_call_output", "call_id": "local-edit", "output": "wrote cedar once"}},
			})
			stopConversationAPIHandler(t, h)
			h, _ = newConversationAPIHandler(t, transport, &cfg)
			outage.Store(true)
			migrated := post(map[string]any{"previous_response_id": "catalog-2", "input": "Continue."})
			if !bytes.Contains(migrated["vekil"], []byte(`"migration":"completed"`)) {
				t.Fatal("catalog continuation did not migrate")
			}
			post(map[string]any{"previous_response_id": "catalog-3", "input": "Next west turn."})
			if sends.Load() != 4 {
				t.Fatalf("successful sends = %d", sends.Load())
			}
		})
	}
}

func TestConversationMigrationHostedCatalogsAndInvalidMetadata(t *testing.T) {
	var sends atomic.Int32
	h, _ := newConversationAPIHandler(t, routeExecutorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, "/models") {
			return routeExecutorTestResponse(req, 200, nil, `{"data":[]}`), nil
		}
		return conversationResponse(t, req, fmt.Sprintf("unprotected-%d", sends.Add(1)), conversationText("Started.")), nil
	}), nil)
	for _, scenario := range []string{"hosted", "nested hosted", "non-string metadata", "oversized metadata", "catalog capacity"} {
		t.Run(scenario, func(t *testing.T) {
			catalog := conversationToolCatalog("edit_once")
			fields := map[string]any{"input": []any{catalog, map[string]any{"role": "user", "content": "Start."}}}
			before := sends.Load()
			switch scenario {
			case "hosted":
				catalog["tools"] = []any{map[string]any{"type": "code_interpreter", "container": "foreign"}}
			case "nested hosted":
				catalog["tools"].([]any)[0].(map[string]any)["tools"] = []any{map[string]any{"type": "image_generation"}}
			case "non-string metadata":
				fields["client_metadata"] = map[string]any{"state": map[string]any{"id": "foreign"}}
			case "oversized metadata":
				fields["client_metadata"] = map[string]string{"thread": strings.Repeat("x", 16*1024)}
			case "catalog capacity":
				h.conversationHistory.config.MaxHistoryBytes = 64
				result := conversationPOST(t, h, fields, nil)
				if result.Code < 400 || !strings.Contains(result.Body.String(), "conversation_history_capacity_exceeded") || sends.Load() != before {
					t.Fatalf("oversized catalog was dispatched: %d %s sends=%d", result.Code, result.Body.String(), sends.Load())
				}
				return
			}
			// Unsupported hosted state and unusable attribution run unprotected:
			// the request is forwarded once and nothing is saved.
			result := conversationPOST(t, h, fields, nil)
			body := result.Body.String()
			if result.Code != http.StatusOK || strings.Contains(body, `"history":"saved"`) || sends.Load() != before+1 {
				t.Fatalf("unsupported request was not forwarded unprotected: %d %s sends=%d", result.Code, body, sends.Load())
			}
			if _, err := h.conversationHistory.lookupResponse("azure", fmt.Sprintf("unprotected-%d", sends.Load())); !errors.Is(err, errConversationHistoryMissing) {
				t.Fatalf("unprotected turn was saved: %v", err)
			}
		})
	}
}
