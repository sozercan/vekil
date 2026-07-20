package proxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestExtractExplicitResponsesRequestStateOnlyReadsResponseItems(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantTokens []stateBindingToken
		wantErr    bool
	}{
		{
			name: "top-level metadata is ordinary data",
			body: `{
				"model":"route",
				"metadata":{"encrypted_content":"metadata-value"},
				"input":[{"type":"message","role":"user","content":"hello"}]
			}`,
		},
		{
			name: "nested item metadata is ordinary data",
			body: `{
				"model":"route",
				"input":[{
					"type":"message",
					"role":"user",
					"metadata":{"encrypted_content":42},
					"content":[{
						"type":"input_text",
						"text":"hello",
						"metadata":{"encrypted_content":"content-metadata-value"}
					}]
				}]
			}`,
		},
		{
			name: "non-state response item field is ordinary data",
			body: `{
				"model":"route",
				"input":[{
					"type":"message",
					"role":"user",
					"encrypted_content":"message-extension-value",
					"content":"hello"
				}]
			}`,
		},
		{
			name: "response input items own opaque state",
			body: `{
				"model":"route",
				"input":[
					{"type":"reasoning","encrypted_content":"reasoning-state"},
					{"type":"compaction","encrypted_content":"compaction-state"}
				]
			}`,
			wantTokens: []stateBindingToken{
				{stateType: stateBindingTypeEncryptedContent, value: "reasoning-state"},
				{stateType: stateBindingTypeEncryptedContent, value: "compaction-state"},
			},
		},
		{
			name: "malformed response item state is rejected",
			body: `{
				"model":"route",
				"input":[{"type":"reasoning","encrypted_content":42}]
			}`,
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := extractExplicitResponsesRequestState([]byte(tc.body), nil)
			if tc.wantErr {
				if err == nil {
					t.Fatal("extractExplicitResponsesRequestState() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("extractExplicitResponsesRequestState() error = %v", err)
			}
			requireStateBindingTokens(t, got, tc.wantTokens)
		})
	}
}

func TestIsProxyOwnedEncryptedContentRequiresDecodableSyntheticCheckpoint(t *testing.T) {
	validPayload, err := json.Marshal(syntheticCompactionPayload{Summary: "valid checkpoint"})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	validEncoded := base64.RawURLEncoding.EncodeToString(validPayload)
	malformedEncoded := base64.RawURLEncoding.EncodeToString([]byte(`{"summary":`))

	tests := []struct {
		name  string
		value string
		want  bool
	}{
		{name: "current checkpoint", value: syntheticCompactionPrefix + validEncoded, want: true},
		{name: "legacy checkpoint", value: legacySyntheticCompactionPrefix + validEncoded, want: true},
		{name: "review example malformed token", value: syntheticCompactionPrefix + "not-base64"},
		{name: "current malformed base64", value: syntheticCompactionPrefix + "***"},
		{name: "legacy malformed base64", value: legacySyntheticCompactionPrefix + "***"},
		{name: "current malformed payload", value: syntheticCompactionPrefix + malformedEncoded},
		{name: "legacy malformed payload", value: legacySyntheticCompactionPrefix + malformedEncoded},
		{name: "unsupported version", value: "vekil.compaction.v2:" + validEncoded},
		{name: "missing version", value: "vekil.compaction:" + validEncoded},
		{name: "lookalike prefix", value: "x" + syntheticCompactionPrefix + validEncoded},
		{name: "current prefix only", value: syntheticCompactionPrefix},
		{name: "legacy prefix only", value: legacySyntheticCompactionPrefix},
		{name: "legacy plaintext summary", value: "legacy plaintext checkpoint summary"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isProxyOwnedEncryptedContent(tc.value); got != tc.want {
				t.Fatalf("isProxyOwnedEncryptedContent(%q) = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}

func syntheticCheckpointTokenForTest(t *testing.T, prefix, summary string) string {
	t.Helper()
	payload, err := json.Marshal(syntheticCompactionPayload{Summary: summary})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(payload)
}

func malformedSyntheticCheckpointTokensForTest(t *testing.T) []struct {
	name  string
	token string
} {
	t.Helper()
	validPayload, err := json.Marshal(syntheticCompactionPayload{Summary: "valid checkpoint"})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	validEncoded := base64.RawURLEncoding.EncodeToString(validPayload)
	malformedEncoded := base64.RawURLEncoding.EncodeToString([]byte(`{"summary":`))

	return []struct {
		name  string
		token string
	}{
		{name: "review example malformed token", token: syntheticCompactionPrefix + "not-base64"},
		{name: "current malformed base64", token: syntheticCompactionPrefix + "***"},
		{name: "legacy malformed base64", token: legacySyntheticCompactionPrefix + "***"},
		{name: "current malformed payload", token: syntheticCompactionPrefix + malformedEncoded},
		{name: "legacy malformed payload", token: legacySyntheticCompactionPrefix + malformedEncoded},
		{name: "unsupported version", token: "vekil.compaction.v2:" + validEncoded},
		{name: "malformed prefix", token: "vekil.compaction.v1" + validEncoded},
		{name: "current prefix only", token: syntheticCompactionPrefix},
		{name: "legacy prefix only", token: legacySyntheticCompactionPrefix},
	}
}

func TestExtractExplicitResponsesRequestStateConversation(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantTokens []stateBindingToken
		wantErr    string
	}{
		{
			name: "absent",
			body: `{"model":"route"}`,
		},
		{
			name: "null",
			body: `{"model":"route","conversation":null}`,
		},
		{
			name: "string",
			body: `{"model":"route","conversation":"  conv-string  "}`,
			wantTokens: []stateBindingToken{
				{stateType: stateBindingTypeConversationID, value: "conv-string"},
			},
		},
		{
			name: "object",
			body: `{"model":"route","conversation":{"id":"  conv-object  "}}`,
			wantTokens: []stateBindingToken{
				{stateType: stateBindingTypeConversationID, value: "conv-object"},
			},
		},
		{
			name: "object extensions do not hide the id",
			body: `{"model":"route","conversation":{"id":"conv-object","metadata":{"tenant":"example"}}}`,
			wantTokens: []stateBindingToken{
				{stateType: stateBindingTypeConversationID, value: "conv-object"},
			},
		},
		{
			name:    "empty string",
			body:    `{"model":"route","conversation":""}`,
			wantErr: "conversation must be a non-empty string or an object with a non-empty string id",
		},
		{
			name:    "whitespace string",
			body:    `{"model":"route","conversation":"   "}`,
			wantErr: "conversation must be a non-empty string or an object with a non-empty string id",
		},
		{
			name:    "number",
			body:    `{"model":"route","conversation":42}`,
			wantErr: "conversation must be a non-empty string or an object with a non-empty string id",
		},
		{
			name:    "boolean",
			body:    `{"model":"route","conversation":false}`,
			wantErr: "conversation must be a non-empty string or an object with a non-empty string id",
		},
		{
			name:    "array",
			body:    `{"model":"route","conversation":[]}`,
			wantErr: "conversation must be a non-empty string or an object with a non-empty string id",
		},
		{
			name:    "object missing id",
			body:    `{"model":"route","conversation":{}}`,
			wantErr: "conversation must be a non-empty string or an object with a non-empty string id",
		},
		{
			name:    "object null id",
			body:    `{"model":"route","conversation":{"id":null}}`,
			wantErr: "conversation must be a non-empty string or an object with a non-empty string id",
		},
		{
			name:    "object numeric id",
			body:    `{"model":"route","conversation":{"id":42}}`,
			wantErr: "conversation must be a non-empty string or an object with a non-empty string id",
		},
		{
			name:    "object empty id",
			body:    `{"model":"route","conversation":{"id":""}}`,
			wantErr: "conversation must be a non-empty string or an object with a non-empty string id",
		},
		{
			name:    "object whitespace id",
			body:    `{"model":"route","conversation":{"id":"   "}}`,
			wantErr: "conversation must be a non-empty string or an object with a non-empty string id",
		},
		{
			name:    "cannot combine with previous response",
			body:    `{"model":"route","conversation":"conv-1","previous_response_id":"resp-1"}`,
			wantErr: "previous_response_id cannot be used in conjunction with conversation",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := extractExplicitResponsesRequestState([]byte(tc.body), nil)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatal("extractExplicitResponsesRequestState() error = nil, want error")
				}
				if err.Error() != tc.wantErr {
					t.Fatalf("extractExplicitResponsesRequestState() error = %q, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("extractExplicitResponsesRequestState() error = %v", err)
			}
			requireStateBindingTokens(t, got, tc.wantTokens)
		})
	}
}

func TestExtractExplicitResponsesRequestStatePreviousResponseID(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantTokens []stateBindingToken
		wantErr    string
	}{
		{
			name: "absent",
			body: `{"model":"route"}`,
		},
		{
			name: "null",
			body: `{"model":"route","previous_response_id":null}`,
		},
		{
			name: "valid string",
			body: `{"model":"route","previous_response_id":"  resp-123  "}`,
			wantTokens: []stateBindingToken{
				{stateType: stateBindingTypeResponseID, value: "resp-123"},
			},
		},
		{
			name:    "empty string",
			body:    `{"model":"route","previous_response_id":""}`,
			wantErr: "previous_response_id must be a non-empty string",
		},
		{
			name:    "whitespace string",
			body:    `{"model":"route","previous_response_id":"   "}`,
			wantErr: "previous_response_id must be a non-empty string",
		},
		{
			name:    "number",
			body:    `{"model":"route","previous_response_id":42}`,
			wantErr: "previous_response_id must be a non-empty string",
		},
		{
			name:    "boolean",
			body:    `{"model":"route","previous_response_id":false}`,
			wantErr: "previous_response_id must be a non-empty string",
		},
		{
			name:    "object",
			body:    `{"model":"route","previous_response_id":{}}`,
			wantErr: "previous_response_id must be a non-empty string",
		},
		{
			name:    "array",
			body:    `{"model":"route","previous_response_id":[]}`,
			wantErr: "previous_response_id must be a non-empty string",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := extractExplicitResponsesRequestState([]byte(tc.body), nil)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatal("extractExplicitResponsesRequestState() error = nil, want error")
				}
				if err.Error() != tc.wantErr {
					t.Fatalf("extractExplicitResponsesRequestState() error = %q, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("extractExplicitResponsesRequestState() error = %v", err)
			}
			requireStateBindingTokens(t, got, tc.wantTokens)
		})
	}
}

func TestExtractExplicitResponsesOutputStateOnlyReadsResponseItems(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantTokens []stateBindingToken
	}{
		{
			name: "nested response output",
			body: `{
				"type":"response.completed",
				"id":"event-nested",
				"metadata":{"encrypted_content":"event-metadata"},
				"response":{
					"id":"resp-nested",
					"metadata":{"encrypted_content":"response-metadata"},
					"output":[{
						"type":"reasoning",
						"encrypted_content":"reasoning-state",
						"metadata":{"encrypted_content":"item-metadata"}
					}]
				}
			}`,
			wantTokens: []stateBindingToken{
				{stateType: stateBindingTypeResponseID, value: "resp-nested"},
				{stateType: stateBindingTypeEncryptedContent, value: "reasoning-state"},
			},
		},
		{
			name: "output item event",
			body: `{
				"type":"response.output_item.done",
				"id":"event-output-item",
				"metadata":{"encrypted_content":"event-metadata"},
				"item":{
					"type":"compaction",
					"encrypted_content":"compaction-state",
					"metadata":{"encrypted_content":"item-metadata"}
				}
			}`,
			wantTokens: []stateBindingToken{
				{stateType: stateBindingTypeEncryptedContent, value: "compaction-state"},
			},
		},
		{
			name: "vendor event root id is ordinary event data",
			body: `{
				"type":"vendor.event",
				"id":"event-vendor"
			}`,
		},
		{
			name: "lifecycle event binds nested response id only",
			body: `{
				"type":"response.created",
				"id":"event-lifecycle",
				"response":{"id":"resp-lifecycle"}
			}`,
			wantTokens: []stateBindingToken{
				{stateType: stateBindingTypeResponseID, value: "resp-lifecycle"},
			},
		},
		{
			name: "non-state output item field is ordinary data",
			body: `{
				"id":"resp-message-extension",
				"object":"response",
				"output":[{
					"type":"message",
					"encrypted_content":"message-extension-value",
					"content":[]
				}]
			}`,
			wantTokens: []stateBindingToken{
				{stateType: stateBindingTypeResponseID, value: "resp-message-extension"},
			},
		},
		{
			name: "top-level response output",
			body: `{
				"id":"resp-top-level",
				"object":"response",
				"metadata":{"encrypted_content":"response-metadata"},
				"output":[{
					"type":"message",
					"content":[{
						"type":"output_text",
						"text":"hello",
						"metadata":{"encrypted_content":"content-metadata"}
					}]
				}]
			}`,
			wantTokens: []stateBindingToken{
				{stateType: stateBindingTypeResponseID, value: "resp-top-level"},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := extractExplicitResponsesOutputState([]byte(tc.body))
			if err != nil {
				t.Fatalf("extractExplicitResponsesOutputState() error = %v", err)
			}
			requireStateBindingTokens(t, got, tc.wantTokens)
		})
	}
}

func TestExtractExplicitResponsesOutputStateConversation(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantTokens []stateBindingToken
	}{
		{
			name: "non-streaming response",
			body: `{
				"id":"resp-conversation",
				"object":"response",
				"conversation":{"id":"  conv-non-streaming  "},
				"output":[]
			}`,
			wantTokens: []stateBindingToken{
				{stateType: stateBindingTypeResponseID, value: "resp-conversation"},
				{stateType: stateBindingTypeConversationID, value: "conv-non-streaming"},
			},
		},
		{
			name: "streaming lifecycle response",
			body: `{
				"type":"response.created",
				"response":{
					"id":"resp-streaming",
					"conversation":{"id":"conv-streaming"},
					"output":[]
				}
			}`,
			wantTokens: []stateBindingToken{
				{stateType: stateBindingTypeResponseID, value: "resp-streaming"},
				{stateType: stateBindingTypeConversationID, value: "conv-streaming"},
			},
		},
		{
			name: "nullable response conversation",
			body: `{
				"id":"resp-without-conversation",
				"object":"response",
				"conversation":null,
				"output":[]
			}`,
			wantTokens: []stateBindingToken{
				{stateType: stateBindingTypeResponseID, value: "resp-without-conversation"},
			},
		},
		{
			name: "malformed response conversation does not suppress response id",
			body: `{
				"id":"resp-malformed-conversation",
				"object":"response",
				"conversation":{"id":42},
				"output":[]
			}`,
			wantTokens: []stateBindingToken{
				{stateType: stateBindingTypeResponseID, value: "resp-malformed-conversation"},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := extractExplicitResponsesOutputState([]byte(tc.body))
			if err != nil {
				t.Fatalf("extractExplicitResponsesOutputState() error = %v", err)
			}
			requireStateBindingTokens(t, got, tc.wantTokens)
		})
	}
}

func TestConversationAndResponseIDsUseDistinctStateNamespaces(t *testing.T) {
	h := newRouteStateBindingTestHandler(t, 8)
	sharedID := "shared-provider-id"
	conversationOwner := stateBindingOwner{routeID: "route-a", targetID: "target-conversation"}
	responseOwner := stateBindingOwner{routeID: "route-a", targetID: "target-response"}

	if result := h.stateBindings.bind(stateBindingTypeConversationID, sharedID, conversationOwner); result.outcome != stateBindingLookupKnown {
		t.Fatalf("conversation bind outcome = %s, want known", result.outcome)
	}
	if result := h.stateBindings.bind(stateBindingTypeResponseID, sharedID, responseOwner); result.outcome != stateBindingLookupKnown {
		t.Fatalf("response bind outcome = %s, want known", result.outcome)
	}

	conversationResult := h.stateBindings.lookup(stateBindingTypeConversationID, sharedID)
	if conversationResult.outcome != stateBindingLookupKnown || conversationResult.owner != conversationOwner {
		t.Fatalf("conversation lookup = %#v, want known owner %#v", conversationResult, conversationOwner)
	}
	responseResult := h.stateBindings.lookup(stateBindingTypeResponseID, sharedID)
	if responseResult.outcome != stateBindingLookupKnown || responseResult.owner != responseOwner {
		t.Fatalf("response lookup = %#v, want known owner %#v", responseResult, responseOwner)
	}
}

func TestExtractExplicitResponsesOutputStateRequiresObjectRoot(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "null", body: `null`},
		{name: "array", body: `[]`},
		{name: "string", body: `"response"`},
		{name: "number", body: `42`},
		{name: "boolean", body: `true`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := extractExplicitResponsesOutputState([]byte(tc.body)); err == nil {
				t.Fatal("extractExplicitResponsesOutputState() error = nil, want non-object root error")
			}
		})
	}
}

func TestApplyExplicitRequestStateBindingIgnoresMetadataButRejectsUnknownArtifact(t *testing.T) {
	route := &modelRoute{
		public:  publicModelContract{routeID: "route-a"},
		targets: []targetBinding{{id: "target-a"}},
		policy:  routePolicy{mode: routeModePrimaryOnly, maxTargetAttempts: 1, maxUpstreamSends: 1},
	}

	t.Run("metadata remains ordinary data", func(t *testing.T) {
		h := newRouteStateBindingTestHandler(t, 8)
		operation := newRouteOperation(route, context.Background())
		body := []byte(`{
			"model":"route",
			"metadata":{"encrypted_content":"ordinary-metadata"},
			"input":[{"type":"message","role":"user","content":"hello"}]
		}`)
		if err := h.applyExplicitRequestStateBinding(operation, body, nil); err != nil {
			t.Fatalf("applyExplicitRequestStateBinding() error = %v", err)
		}
		if got := operation.pinnedTarget(); got != "" {
			t.Fatalf("pinned target = %q, want none", got)
		}
	})

	t.Run("unknown reasoning artifact is rejected", func(t *testing.T) {
		h := newRouteStateBindingTestHandler(t, 8)
		operation := newRouteOperation(route, context.Background())
		body := []byte(`{
			"model":"route",
			"input":[{"type":"reasoning","encrypted_content":"unknown-provider-state"}]
		}`)
		err := h.applyExplicitRequestStateBinding(operation, body, nil)
		var requestErr *providerRequestError
		if !errors.As(err, &requestErr) || requestErr.statusCode != 400 {
			t.Fatalf("applyExplicitRequestStateBinding() error = %v, want provider 400", err)
		}
		if got := operation.pinnedTarget(); got != "" {
			t.Fatalf("pinned target = %q, want none", got)
		}
	})
}

func TestApplyExplicitRequestStateBindingConversation(t *testing.T) {
	newRoute := func() *modelRoute {
		return newRouteStateBindingTestRoute("route-a", routeModePriorityFailover, "target-a", "target-b")
	}
	bind := func(t *testing.T, h *ProxyHandler, stateType stateBindingType, value string, owner stateBindingOwner) {
		t.Helper()
		if result := h.stateBindings.bind(stateType, value, owner); result.outcome != stateBindingLookupKnown {
			t.Fatalf("bind(%s) outcome = %s, want known", stateType, result.outcome)
		}
	}
	requireBadRequest := func(t *testing.T, err error) {
		t.Helper()
		var requestErr *providerRequestError
		if !errors.As(err, &requestErr) || requestErr.statusCode != http.StatusBadRequest {
			t.Fatalf("applyExplicitRequestStateBinding() error = %v, want provider 400", err)
		}
	}

	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "string form", body: `{"model":"route","conversation":"conv-known"}`},
		{name: "object form", body: `{"model":"route","conversation":{"id":"conv-known"}}`},
	} {
		t.Run("known "+tc.name+" pins exact target", func(t *testing.T) {
			h := newRouteStateBindingTestHandler(t, 8)
			route := newRoute()
			owner := stateBindingOwner{routeID: route.public.routeID, targetID: "target-b"}
			bind(t, h, stateBindingTypeConversationID, "conv-known", owner)
			operation := newRouteOperation(route, context.Background())

			if err := h.applyExplicitRequestStateBinding(operation, []byte(tc.body), nil); err != nil {
				t.Fatalf("applyExplicitRequestStateBinding() error = %v", err)
			}
			if got := operation.pinnedTarget(); got != owner.targetID {
				t.Fatalf("pinned target = %q, want %q", got, owner.targetID)
			}
		})
	}

	t.Run("unknown conversation on multi-target priority failover fails closed", func(t *testing.T) {
		h := newRouteStateBindingTestHandler(t, 8)
		operation := newRouteOperation(newRoute(), context.Background())
		err := h.applyExplicitRequestStateBinding(operation, []byte(`{"model":"route","conversation":"conv-unknown"}`), nil)
		requireBadRequest(t, err)
		if got := operation.pinnedTarget(); got != "" {
			t.Fatalf("pinned target = %q, want none", got)
		}
		if result := h.stateBindings.lookup(stateBindingTypeConversationID, "conv-unknown"); result.outcome != stateBindingLookupUnknown {
			t.Fatalf("conversation binding = %#v, want unknown", result)
		}
	})

	t.Run("malformed conversation fails before pinning", func(t *testing.T) {
		h := newRouteStateBindingTestHandler(t, 8)
		operation := newRouteOperation(newRoute(), context.Background())
		err := h.applyExplicitRequestStateBinding(operation, []byte(`{"model":"route","conversation":{"id":42}}`), nil)
		requireBadRequest(t, err)
		if got := operation.pinnedTarget(); got != "" {
			t.Fatalf("pinned target = %q, want none", got)
		}
	})

	t.Run("previous response mutual exclusion fails before bootstrap", func(t *testing.T) {
		h := newRouteStateBindingTestHandler(t, 8)
		route := newRouteStateBindingTestRoute("route-a", routeModePrimaryOnly, "target-a")
		operation := newRouteOperation(route, context.Background())
		err := h.applyExplicitRequestStateBinding(operation, []byte(`{
			"model":"route",
			"conversation":"conv-mutually-exclusive",
			"previous_response_id":"resp-mutually-exclusive"
		}`), nil)
		requireBadRequest(t, err)
		if got := operation.pinnedTarget(); got != "" {
			t.Fatalf("pinned target = %q, want none", got)
		}
		if result := h.stateBindings.lookup(stateBindingTypeConversationID, "conv-mutually-exclusive"); result.outcome != stateBindingLookupUnknown {
			t.Fatalf("conversation binding = %#v, want unknown", result)
		}
	})

	t.Run("mixed known and unknown state fails closed", func(t *testing.T) {
		h := newRouteStateBindingTestHandler(t, 8)
		route := newRoute()
		owner := stateBindingOwner{routeID: route.public.routeID, targetID: "target-b"}
		bind(t, h, stateBindingTypeConversationID, "conv-known", owner)
		operation := newRouteOperation(route, context.Background())
		body := []byte(`{
			"model":"route",
			"conversation":"conv-known",
			"input":[{"type":"reasoning","encrypted_content":"unknown-reasoning"}]
		}`)

		err := h.applyExplicitRequestStateBinding(operation, body, nil)
		requireBadRequest(t, err)
		if got := operation.pinnedTarget(); got != "" {
			t.Fatalf("pinned target = %q, want none", got)
		}
	})

	t.Run("state owned by different targets fails closed", func(t *testing.T) {
		h := newRouteStateBindingTestHandler(t, 8)
		route := newRoute()
		bind(t, h, stateBindingTypeConversationID, "conv-known", stateBindingOwner{routeID: route.public.routeID, targetID: "target-b"})
		bind(t, h, stateBindingTypeEncryptedContent, "reasoning-known", stateBindingOwner{routeID: route.public.routeID, targetID: "target-a"})
		operation := newRouteOperation(route, context.Background())
		body := []byte(`{
			"model":"route",
			"conversation":{"id":"conv-known"},
			"input":[{"type":"reasoning","encrypted_content":"reasoning-known"}]
		}`)

		err := h.applyExplicitRequestStateBinding(operation, body, nil)
		requireBadRequest(t, err)
		if got := operation.pinnedTarget(); got != "" {
			t.Fatalf("pinned target = %q, want none", got)
		}
	})

	t.Run("cross-route conversation fails closed", func(t *testing.T) {
		h := newRouteStateBindingTestHandler(t, 8)
		route := newRoute()
		bind(t, h, stateBindingTypeConversationID, "conv-other-route", stateBindingOwner{routeID: "route-b", targetID: "target-b"})
		operation := newRouteOperation(route, context.Background())

		err := h.applyExplicitRequestStateBinding(operation, []byte(`{"model":"route","conversation":"conv-other-route"}`), nil)
		requireBadRequest(t, err)
		if got := operation.pinnedTarget(); got != "" {
			t.Fatalf("pinned target = %q, want none", got)
		}
	})
}

func TestApplyExplicitRequestStateBindingConversationBootstrap(t *testing.T) {
	tests := []struct {
		name       string
		route      *modelRoute
		wantTarget string
	}{
		{
			name:       "one target priority failover route",
			route:      newRouteStateBindingTestRoute("route-one", routeModePriorityFailover, "target-only"),
			wantTarget: "target-only",
		},
		{
			name:       "multi-target primary only route",
			route:      newRouteStateBindingTestRoute("route-primary", routeModePrimaryOnly, "target-primary", "target-secondary"),
			wantTarget: "target-primary",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newRouteStateBindingTestHandler(t, 8)
			operation := newRouteOperation(tc.route, context.Background())
			conversationID := "conv-bootstrap"

			if err := h.applyExplicitRequestStateBinding(operation, []byte(`{"model":"route","conversation":"`+conversationID+`"}`), nil); err != nil {
				t.Fatalf("applyExplicitRequestStateBinding() error = %v", err)
			}
			if got := operation.pinnedTarget(); got != tc.wantTarget {
				t.Fatalf("pinned target = %q, want %q", got, tc.wantTarget)
			}
			if operation.allowsAutomaticTargetSwitch(routeAttemptNormal) {
				t.Fatal("bootstrapped conversation did not hard-pin the route operation")
			}
			wantOwner := stateBindingOwner{routeID: tc.route.public.routeID, targetID: tc.wantTarget}
			result := h.stateBindings.lookup(stateBindingTypeConversationID, conversationID)
			if result.outcome != stateBindingLookupKnown || result.owner != wantOwner {
				t.Fatalf("conversation binding = %#v, want known owner %#v", result, wantOwner)
			}
		})
	}
}

func TestApplyExplicitRequestStateBindingConversationConcurrentBootstrapAndConflict(t *testing.T) {
	const workers = 32

	h := newRouteStateBindingTestHandler(t, 64)
	route := newRouteStateBindingTestRoute("route-a", routeModePrimaryOnly, "target-a", "target-b")
	conversationID := "conv-concurrent-bootstrap"
	start := make(chan struct{})
	errs := make(chan error, workers)
	operations := make([]*routeOperation, workers)
	var ready sync.WaitGroup
	var done sync.WaitGroup
	ready.Add(workers)
	done.Add(workers)
	for i := range workers {
		operation := newRouteOperation(route, context.Background())
		operations[i] = operation
		go func() {
			defer done.Done()
			ready.Done()
			<-start
			errs <- h.applyExplicitRequestStateBinding(operation, []byte(`{"model":"route","conversation":"`+conversationID+`"}`), nil)
		}()
	}
	ready.Wait()
	close(start)
	done.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent applyExplicitRequestStateBinding() error = %v", err)
		}
	}
	for i, operation := range operations {
		if got := operation.pinnedTarget(); got != "target-a" {
			t.Errorf("operation %d pinned target = %q, want target-a", i, got)
		}
	}
	wantOwner := stateBindingOwner{routeID: route.public.routeID, targetID: "target-a"}
	if result := h.stateBindings.lookup(stateBindingTypeConversationID, conversationID); result.outcome != stateBindingLookupKnown || result.owner != wantOwner {
		t.Fatalf("conversation binding after concurrent bootstrap = %#v, want known owner %#v", result, wantOwner)
	}
	stats := h.stats.snapshot()
	if stats.StateBindingMisses != 1 || stats.StateBindingHits != workers-1 {
		t.Fatalf("concurrent bootstrap stats = hits:%d misses:%d, want hits:%d misses:1", stats.StateBindingHits, stats.StateBindingMisses, workers-1)
	}

	conflictingOwner := stateBindingOwner{routeID: route.public.routeID, targetID: "target-b"}
	if result := h.stateBindings.bind(stateBindingTypeConversationID, conversationID, conflictingOwner); result.outcome != stateBindingLookupConflict {
		t.Fatalf("conflicting bind outcome = %s, want conflict", result.outcome)
	}
	if result := h.stateBindings.lookup(stateBindingTypeConversationID, conversationID); result.outcome != stateBindingLookupConflict {
		t.Fatalf("conversation binding after conflict = %#v, want conflict tombstone", result)
	}
	operation := newRouteOperation(route, context.Background())
	err := h.applyExplicitRequestStateBinding(operation, []byte(`{"model":"route","conversation":"`+conversationID+`"}`), nil)
	var requestErr *providerRequestError
	if !errors.As(err, &requestErr) || requestErr.statusCode != http.StatusBadRequest {
		t.Fatalf("applyExplicitRequestStateBinding() after conflict error = %v, want provider 400", err)
	}
	if got := operation.pinnedTarget(); got != "" {
		t.Fatalf("pinned target after conflict = %q, want none", got)
	}
}

func TestApplyExplicitRequestStateBindingConversationConcurrentCrossRouteBootstrap(t *testing.T) {
	h := newRouteStateBindingTestHandler(t, 8)
	conversationID := "conv-concurrent-cross-route"
	routes := []*modelRoute{
		newRouteStateBindingTestRoute("route-a", routeModePrimaryOnly, "target-a"),
		newRouteStateBindingTestRoute("route-b", routeModePrimaryOnly, "target-b"),
	}
	operations := []*routeOperation{
		newRouteOperation(routes[0], context.Background()),
		newRouteOperation(routes[1], context.Background()),
	}
	errs := make([]error, len(operations))
	start := make(chan struct{})
	var ready sync.WaitGroup
	var done sync.WaitGroup
	ready.Add(len(operations))
	done.Add(len(operations))
	for i, operation := range operations {
		go func(i int, operation *routeOperation) {
			defer done.Done()
			ready.Done()
			<-start
			errs[i] = h.applyExplicitRequestStateBinding(operation, []byte(`{"model":"route","conversation":"`+conversationID+`"}`), nil)
		}(i, operation)
	}
	ready.Wait()
	close(start)
	done.Wait()

	var winner stateBindingOwner
	successes := 0
	for i, err := range errs {
		if err == nil {
			successes++
			winner = stateBindingOwner{routeID: routes[i].public.routeID, targetID: routes[i].targets[0].id}
			if got := operations[i].pinnedTarget(); got != winner.targetID {
				t.Fatalf("winning operation %d pinned target = %q, want %q", i, got, winner.targetID)
			}
			continue
		}
		var requestErr *providerRequestError
		if !errors.As(err, &requestErr) || requestErr.statusCode != http.StatusBadRequest {
			t.Fatalf("losing operation %d error = %v, want provider 400", i, err)
		}
		if got := operations[i].pinnedTarget(); got != "" {
			t.Fatalf("losing operation %d pinned target = %q, want none", i, got)
		}
	}
	if successes != 1 {
		t.Fatalf("successful concurrent bootstraps = %d, want 1", successes)
	}
	if result := h.stateBindings.lookup(stateBindingTypeConversationID, conversationID); result.outcome != stateBindingLookupKnown || result.owner != winner {
		t.Fatalf("conversation binding after cross-route race = %#v, want winning owner %#v", result, winner)
	}
}

func TestApplyExplicitRequestStateBindingConversationSubsequentExactPinning(t *testing.T) {
	h := newRouteStateBindingTestHandler(t, 8)
	conversationID := "conv-exact-pinning"
	bootstrapRoute := newRouteStateBindingTestRoute("route-a", routeModePrimaryOnly, "target-a", "target-b")
	bootstrapOperation := newRouteOperation(bootstrapRoute, context.Background())
	requestBody := []byte(`{"model":"route","conversation":"` + conversationID + `"}`)

	if err := h.applyExplicitRequestStateBinding(bootstrapOperation, requestBody, nil); err != nil {
		t.Fatalf("bootstrap applyExplicitRequestStateBinding() error = %v", err)
	}
	if got := bootstrapOperation.pinnedTarget(); got != "target-a" {
		t.Fatalf("bootstrap pinned target = %q, want target-a", got)
	}

	// Reverse the route order and enable failover to prove the stored owner, not
	// the current primary, controls every subsequent request.
	followupRoute := newRouteStateBindingTestRoute("route-a", routeModePriorityFailover, "target-b", "target-a")
	followupOperation := newRouteOperation(followupRoute, context.Background())
	if err := h.applyExplicitRequestStateBinding(followupOperation, requestBody, nil); err != nil {
		t.Fatalf("follow-up applyExplicitRequestStateBinding() error = %v", err)
	}
	if got := followupOperation.pinnedTarget(); got != "target-a" {
		t.Fatalf("follow-up pinned target = %q, want exact bootstrapped target-a", got)
	}
	if followupOperation.allowsAutomaticTargetSwitch(routeAttemptNormal) {
		t.Fatal("follow-up conversation binding did not hard-pin the exact owner")
	}
}

func TestExplicitResponsesConversationStateIsBoundBeforeExposure(t *testing.T) {
	const conversationID = "conv-before-exposure"
	info := explicitRouteResponseInfo{
		routeID:  "route-a",
		publicID: "public-model",
		targetID: "target-a",
	}
	owner := stateBindingOwner{routeID: info.routeID, targetID: info.targetID}
	responseBody := `{
		"id":"resp-before-exposure",
		"object":"response",
		"model":"physical-model",
		"conversation":{"id":"` + conversationID + `"},
		"output":[]
	}`

	t.Run("non-streaming body", func(t *testing.T) {
		h := newRouteStateBindingTestHandler(t, 8)
		writer := &stateBindingOrderResponseWriter{
			header: make(http.Header),
			beforeCommit: func() error {
				result := h.stateBindings.lookup(stateBindingTypeConversationID, conversationID)
				if result.outcome != stateBindingLookupKnown || result.owner != owner {
					return fmt.Errorf("conversation binding before commit = %#v, want known owner %#v", result, owner)
				}
				return nil
			},
		}
		resp := &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(responseBody)),
		}

		if err := writeExplicitResponsesResponse(context.Background(), h, writer, resp, info, nil, ""); err != nil {
			t.Fatalf("writeExplicitResponsesResponse() error = %v", err)
		}
		if writer.commitErr != nil {
			t.Fatal(writer.commitErr)
		}
		if !writer.committed {
			t.Fatal("response was not committed")
		}
		if !strings.Contains(writer.body.String(), conversationID) {
			t.Fatalf("response body = %q, want conversation id", writer.body.String())
		}
	})

	t.Run("streaming lifecycle event", func(t *testing.T) {
		h := newRouteStateBindingTestHandler(t, 8)
		stream := "event: response.created\n" +
			`data: {"type":"response.created","response":{"id":"resp-stream","model":"physical-model","conversation":{"id":"` + conversationID + `"}}}` + "\n\n"
		body := normalizeResponsesStreamBodyWithBinding(h, io.NopCloser(strings.NewReader(stream)), info)
		defer func() { _ = body.Close() }()

		var first [1]byte
		if _, err := io.ReadFull(body, first[:]); err != nil {
			t.Fatalf("read first exposed stream byte: %v", err)
		}
		result := h.stateBindings.lookup(stateBindingTypeConversationID, conversationID)
		if result.outcome != stateBindingLookupKnown || result.owner != owner {
			t.Fatalf("conversation binding before first stream byte = %#v, want known owner %#v", result, owner)
		}
	})

	t.Run("binding conflict prevents non-streaming exposure", func(t *testing.T) {
		h := newRouteStateBindingTestHandler(t, 8)
		otherOwner := stateBindingOwner{routeID: info.routeID, targetID: "target-b"}
		if result := h.stateBindings.bind(stateBindingTypeConversationID, conversationID, otherOwner); result.outcome != stateBindingLookupKnown {
			t.Fatalf("seed bind outcome = %s, want known", result.outcome)
		}
		writer := &stateBindingOrderResponseWriter{header: make(http.Header)}
		resp := &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(responseBody)),
		}

		err := writeExplicitResponsesResponse(context.Background(), h, writer, resp, info, nil, "")
		if err == nil || !strings.Contains(err.Error(), "provider state token collided with another route target") {
			t.Fatalf("writeExplicitResponsesResponse() error = %v, want binding conflict", err)
		}
		if writer.committed || writer.body.Len() != 0 {
			t.Fatalf("response exposure = committed %v, body %q; want none", writer.committed, writer.body.String())
		}
	})
}

type stateBindingOrderResponseWriter struct {
	header       http.Header
	body         strings.Builder
	beforeCommit func() error
	commitErr    error
	committed    bool
}

func (w *stateBindingOrderResponseWriter) Header() http.Header {
	return w.header
}

func (w *stateBindingOrderResponseWriter) WriteHeader(_ int) {
	if w.committed {
		return
	}
	if w.beforeCommit != nil {
		w.commitErr = w.beforeCommit()
	}
	w.committed = true
}

func (w *stateBindingOrderResponseWriter) Write(p []byte) (int, error) {
	if !w.committed {
		w.WriteHeader(http.StatusOK)
	}
	return w.body.Write(p)
}

func TestBindExplicitStateTokensRecordsExactConcurrentEvictions(t *testing.T) {
	const workers = 256

	clock := &stateBindingTestClock{now: time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)}
	store := newTestStateBindingStore(t, 1, time.Hour, clock.Now)
	owner := stateBindingOwner{routeID: "route-a", targetID: "target-a"}
	if result := store.bind(stateBindingTypeResponseID, "seed", owner); result.outcome != stateBindingLookupKnown {
		t.Fatalf("seed bind outcome = %s, want known", result.outcome)
	}

	h := &ProxyHandler{
		stats:         newStatsCollector(),
		stateBindings: store,
	}
	h.stateBindingsOnce.Do(func() {})
	info := explicitRouteResponseInfo{routeID: owner.routeID, targetID: owner.targetID}

	start := make(chan struct{})
	errs := make(chan error, workers)
	var ready sync.WaitGroup
	var done sync.WaitGroup
	ready.Add(workers)
	done.Add(workers)
	for i := 0; i < workers; i++ {
		go func(i int) {
			defer done.Done()
			ready.Done()
			<-start
			err := h.bindExplicitStateTokens(info, []stateBindingToken{{
				stateType: stateBindingTypeResponseID,
				value:     fmt.Sprintf("response-%03d", i),
			}})
			if err != nil {
				errs <- err
			}
		}(i)
	}
	ready.Wait()
	close(start)
	done.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("bindExplicitStateTokens() error = %v", err)
	}

	storeEvictions := store.stats().evictions
	if storeEvictions != workers {
		t.Fatalf("store evictions = %d, want %d", storeEvictions, workers)
	}
	if got := h.stats.snapshot().StateBindingEvictions; got != int64(storeEvictions) {
		t.Fatalf("recorded state binding evictions = %d, want exact store delta %d", got, storeEvictions)
	}
}

func newRouteStateBindingTestHandler(t *testing.T, maxEntries int) *ProxyHandler {
	t.Helper()
	clock := &stateBindingTestClock{now: time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)}
	h := &ProxyHandler{
		stats:         newStatsCollector(),
		stateBindings: newTestStateBindingStore(t, maxEntries, time.Hour, clock.Now),
	}
	h.stateBindingsOnce.Do(func() {})
	return h
}

func newRouteStateBindingTestRoute(routeID string, mode routeMode, targetIDs ...string) *modelRoute {
	targets := make([]targetBinding, 0, len(targetIDs))
	for _, targetID := range targetIDs {
		targets = append(targets, targetBinding{
			id: targetID,
			provider: &providerRuntime{
				id:   "provider-" + targetID,
				kind: providerTypeAzureOpenAI,
			},
		})
	}
	maxTargetAttempts := len(targets)
	if mode == routeModePrimaryOnly {
		maxTargetAttempts = min(maxTargetAttempts, 1)
	}
	return &modelRoute{
		public:  publicModelContract{id: "route", routeID: routeID, endpoints: []string{providerEndpointResponses}},
		targets: targets,
		policy: routePolicy{
			mode:              mode,
			maxTargetAttempts: maxTargetAttempts,
			maxUpstreamSends:  maxTargetAttempts,
		},
	}
}

func requireStateBindingTokens(t *testing.T, got, want []stateBindingToken) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("tokens = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("token[%d] = %#v, want %#v", i, got[i], want[i])
		}
	}
}

func TestExplicitStateBindingTargetRevisionCompatibility(t *testing.T) {
	newRoute := func(baseURL string) *modelRoute {
		route := &modelRoute{
			public: publicModelContract{
				id:        "public-model",
				routeID:   "route-a",
				endpoints: []string{providerEndpointResponses},
			},
			targets: []targetBinding{{
				id: "target-a",
				provider: &providerRuntime{
					id:       "provider-a",
					kind:     providerTypeOpenAICompatible,
					baseURL:  baseURL,
					authType: providerAuthTypeNone,
					paths: providerEndpointPaths{
						responses: providerEndpointResponses,
					},
				},
				upstreamModel: "physical-model",
			}},
			policy: routePolicy{
				mode:              routeModePrimaryOnly,
				maxTargetAttempts: 1,
				maxUpstreamSends:  1,
			},
		}
		ensureModelRouteTargetRevisions(route)
		return route
	}
	installRoute := func(t *testing.T, h *ProxyHandler, route *modelRoute) {
		t.Helper()
		registry, err := newModelRouteRegistry([]*modelRoute{route})
		if err != nil {
			t.Fatalf("newModelRouteRegistry() error = %v", err)
		}
		h.providersState = &providerSetup{routes: registry}
	}
	requestBody := []byte(`{"model":"public-model","input":[{"type":"reasoning","encrypted_content":"state-token"}]}`)

	t.Run("bootstrap and publication carry revision", func(t *testing.T) {
		h := newRouteStateBindingTestHandler(t, 8)
		route := newRoute("https://same.example.invalid/v1")
		installRoute(t, h, route)

		conversationOperation := newRouteOperation(route, context.Background())
		if err := h.applyExplicitRequestStateBinding(conversationOperation, []byte(`{"model":"public-model","conversation":"conversation-revision"}`), nil); err != nil {
			t.Fatalf("conversation bootstrap error = %v", err)
		}
		conversationOwner, ok := h.stateBindings.lookup(stateBindingTypeConversationID, "conversation-revision").knownOwner()
		if !ok || conversationOwner.targetRevision == "" || conversationOwner.targetRevision != route.targets[0].revision {
			t.Fatalf("conversation owner = %#v, want target revision %q", conversationOwner, route.targets[0].revision)
		}

		if err := h.bindExplicitStateTokens(explicitRouteResponseInfo{
			routeID:  route.public.routeID,
			targetID: route.targets[0].id,
		}, []stateBindingToken{{stateType: stateBindingTypeEncryptedContent, value: "state-token"}}); err != nil {
			t.Fatalf("bindExplicitStateTokens() error = %v", err)
		}
		publishedOwner, ok := h.stateBindings.lookup(stateBindingTypeEncryptedContent, "state-token").knownOwner()
		if !ok || publishedOwner.targetRevision == "" || publishedOwner.targetRevision != route.targets[0].revision {
			t.Fatalf("published owner = %#v, want target revision %q", publishedOwner, route.targets[0].revision)
		}
	})

	t.Run("unchanged target resolves and changed target fails closed", func(t *testing.T) {
		h := newRouteStateBindingTestHandler(t, 8)
		original := newRoute("https://same.example.invalid/v1")
		owner := stateBindingOwner{
			routeID:        original.public.routeID,
			targetID:       original.targets[0].id,
			targetRevision: original.targets[0].revision,
		}
		if result := h.stateBindings.bind(stateBindingTypeEncryptedContent, "state-token", owner); result.outcome != stateBindingLookupKnown {
			t.Fatalf("bind original state outcome = %s", result.outcome)
		}

		compatible := newRoute("https://same.example.invalid/v1")
		compatibleOperation := newRouteOperation(compatible, context.Background())
		if err := h.applyExplicitRequestStateBinding(compatibleOperation, requestBody, nil); err != nil {
			t.Fatalf("compatible revision error = %v", err)
		}
		if got := compatibleOperation.pinnedTarget(); got != owner.targetID {
			t.Fatalf("compatible pinned target = %q, want %q", got, owner.targetID)
		}

		changed := newRoute("https://changed.example.invalid/v1")
		if changed.targets[0].revision == owner.targetRevision {
			t.Fatal("changed physical target retained the original revision")
		}
		changedOperation := newRouteOperation(changed, context.Background())
		err := h.applyExplicitRequestStateBinding(changedOperation, requestBody, nil)
		var requestErr *providerRequestError
		if !errors.As(err, &requestErr) || requestErr.statusCode != http.StatusBadRequest {
			t.Fatalf("changed revision error = %v, want provider 400", err)
		}
		if got := changedOperation.pinnedTarget(); got != "" {
			t.Fatalf("changed revision pinned target = %q, want none", got)
		}
	})
}
