package proxy

import (
	"encoding/json"
	"testing"
)

func TestChooseChatRoute_KnownChatOnly(t *testing.T) {
	provider := &providerRuntime{id: "native", kind: providerTypeOpenAICompatible}
	owner := providerModel{
		publicID:           "gpt-public",
		upstreamModel:      "gpt-upstream",
		providerID:         provider.id,
		supportedEndpoints: []string{providerEndpointChatCompletions},
	}

	route, err := chooseChatRoute(provider, owner, true, owner.publicID)
	if err != nil {
		t.Fatalf("chooseChatRoute() error = %v", err)
	}
	if route.backend != chatBackendNativeChat {
		t.Fatalf("route.backend = %v, want native Chat", route.backend)
	}
	if route.nativeEndpoint != providerEndpointChatCompletions {
		t.Fatalf("route.nativeEndpoint = %q, want %q", route.nativeEndpoint, providerEndpointChatCompletions)
	}
	if route.provider != provider {
		t.Fatal("route.provider did not capture the selected provider")
	}
	if route.publicModel != owner.publicID || route.upstreamModel != owner.upstreamModel {
		t.Fatalf("route models = public %q upstream %q", route.publicModel, route.upstreamModel)
	}
}

func TestChooseChatRoute_KnownResponsesOnly(t *testing.T) {
	provider := &providerRuntime{id: "responses", kind: providerTypeOpenAICompatible}
	owner := providerModel{
		publicID:           "gpt-public",
		upstreamModel:      "gpt-responses",
		providerID:         provider.id,
		supportedEndpoints: []string{providerEndpointResponses},
	}

	route, err := chooseChatRoute(provider, owner, true, owner.publicID)
	if err != nil {
		t.Fatalf("chooseChatRoute() error = %v", err)
	}
	if route.backend != chatBackendResponses {
		t.Fatalf("route.backend = %v, want Responses", route.backend)
	}
	if route.nativeEndpoint != providerEndpointResponses {
		t.Fatalf("route.nativeEndpoint = %q, want %q", route.nativeEndpoint, providerEndpointResponses)
	}
	if !providerModelSupportsEndpoint(owner, providerEndpointResponses) {
		t.Fatal("test setup lost native Responses metadata")
	}
	if providerModelSupportsEndpoint(owner, providerEndpointChatCompletions) {
		t.Fatal("route selection must not add emulated Chat to native endpoint metadata")
	}
}

func TestChooseChatRoute_BothPrefersNativeChat(t *testing.T) {
	provider := &providerRuntime{id: "both", kind: providerTypeOpenAICompatible}
	owner := providerModel{
		publicID:           "gpt-both",
		upstreamModel:      "gpt-both-upstream",
		providerID:         provider.id,
		supportedEndpoints: []string{providerEndpointResponses, providerEndpointChatCompletions},
	}

	route, err := chooseChatRoute(provider, owner, true, owner.publicID)
	if err != nil {
		t.Fatalf("chooseChatRoute() error = %v", err)
	}
	if route.backend != chatBackendNativeChat || route.nativeEndpoint != providerEndpointChatCompletions {
		t.Fatalf("route = backend %v endpoint %q, want native Chat", route.backend, route.nativeEndpoint)
	}
}

func TestChooseChatRoute_CopilotVendorScopedResponsesPreference(t *testing.T) {
	copilot := &providerRuntime{id: "copilot", kind: providerTypeCopilot}
	nonCopilot := &providerRuntime{id: "other", kind: providerTypeOpenAICompatible}

	for _, tc := range []struct {
		name            string
		provider        *providerRuntime
		known           bool
		preferResponses bool
		endpoints       []string
		wantBackend     chatBackend
		wantEndpoint    string
	}{
		{
			name:            "Copilot Experimental dual-endpoint prefers Responses",
			provider:        copilot,
			known:           true,
			preferResponses: true,
			endpoints:       []string{providerEndpointChatCompletions, providerEndpointResponses},
			wantBackend:     chatBackendResponses,
			wantEndpoint:    providerEndpointResponses,
		},
		{
			name:            "Copilot Google/OpenAI dual-endpoint keeps native Chat",
			provider:        copilot,
			known:           true,
			preferResponses: false,
			endpoints:       []string{providerEndpointChatCompletions, providerEndpointResponses},
			wantBackend:     chatBackendNativeChat,
			wantEndpoint:    providerEndpointChatCompletions,
		},
		{
			name:            "Copilot Microsoft Chat-only keeps native Chat",
			provider:        copilot,
			known:           true,
			preferResponses: true,
			endpoints:       []string{providerEndpointChatCompletions},
			wantBackend:     chatBackendNativeChat,
			wantEndpoint:    providerEndpointChatCompletions,
		},
		{
			name:            "Copilot endpoint-less entry keeps native Chat",
			provider:        copilot,
			known:           true,
			preferResponses: true,
			endpoints:       nil,
			wantBackend:     chatBackendNativeChat,
			wantEndpoint:    providerEndpointChatCompletions,
		},
		{
			name:            "Non-Copilot provider keeps native Chat even with preferResponses",
			provider:        nonCopilot,
			known:           true,
			preferResponses: true,
			endpoints:       []string{providerEndpointChatCompletions, providerEndpointResponses},
			wantBackend:     chatBackendNativeChat,
			wantEndpoint:    providerEndpointChatCompletions,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			owner := providerModel{
				publicID:           "model-under-test",
				upstreamModel:      "model-under-test",
				providerID:         tc.provider.id,
				supportedEndpoints: tc.endpoints,
				preferResponses:    tc.preferResponses,
			}
			route, err := chooseChatRoute(tc.provider, owner, tc.known, owner.publicID)
			if err != nil {
				t.Fatalf("chooseChatRoute() error = %v", err)
			}
			if route.backend != tc.wantBackend || route.nativeEndpoint != tc.wantEndpoint {
				t.Fatalf("route = backend %v endpoint %q, want backend %v endpoint %q",
					route.backend, route.nativeEndpoint, tc.wantBackend, tc.wantEndpoint)
			}
		})
	}
}

func TestChooseChatRoute_CopilotDuplicateCatalogEntriesPreferResponses(t *testing.T) {
	provider := &providerRuntime{id: "copilot", kind: providerTypeCopilot}
	omitted := json.RawMessage(`{"id":"mai-code-2"}`)
	for _, vendor := range []string{"Microsoft", "Experimental"} {
		t.Run(vendor, func(t *testing.T) {
			advertised := json.RawMessage(`{"id":"mai-code-2","vendor":"` + vendor + `","supported_endpoints":["/chat/completions","/responses"]}`)
			disabled := json.RawMessage(`{"id":"mai-code-2","vendor":"` + vendor + `","supported_endpoints":["/chat/completions","/responses"],"policy":{"state":"disabled"}}`)
			google := json.RawMessage(`{"id":"mai-code-2","vendor":"Google"}`)
			enabledChat := json.RawMessage(`{"id":"mai-code-2","vendor":"` + vendor + `","supported_endpoints":["/chat/completions"]}`)
			disabledResponses := json.RawMessage(`{"id":"mai-code-2","supported_endpoints":["/responses"],"policy":{"state":"disabled"}}`)
			for _, tc := range []struct {
				name        string
				data        []json.RawMessage
				wantBackend chatBackend
			}{
				{name: "vendor first", data: []json.RawMessage{advertised, omitted}, wantBackend: chatBackendResponses},
				{name: "vendor last", data: []json.RawMessage{omitted, advertised}, wantBackend: chatBackendResponses},
				{name: "vendor between omitted entries", data: []json.RawMessage{omitted, advertised, omitted}, wantBackend: chatBackendResponses},
				{name: "published vendor wins", data: []json.RawMessage{google, advertised}, wantBackend: chatBackendNativeChat},
				{name: "disabled duplicate cannot fill missing vendor", data: []json.RawMessage{omitted, disabled}, wantBackend: chatBackendNativeChat},
				{name: "enabled entry replaces disabled preference", data: []json.RawMessage{disabled, omitted}, wantBackend: chatBackendNativeChat},
				{name: "enabled entry replaces disabled vendor", data: []json.RawMessage{disabled, google}, wantBackend: chatBackendNativeChat},
				{name: "disabled Responses endpoint last", data: []json.RawMessage{enabledChat, disabledResponses}, wantBackend: chatBackendNativeChat},
				{name: "disabled Responses endpoint first", data: []json.RawMessage{disabledResponses, enabledChat}, wantBackend: chatBackendNativeChat},
			} {
				t.Run(tc.name, func(t *testing.T) {
					body, err := json.Marshal(map[string]any{"data": tc.data})
					if err != nil {
						t.Fatal(err)
					}
					models, err := decodeProviderModelsFromBody(provider, body)
					if err != nil {
						t.Fatal(err)
					}
					if len(models) != 1 {
						t.Fatalf("decoded %d models, want 1", len(models))
					}
					owner := models[0]
					route, err := chooseChatRoute(provider, owner, true, owner.publicID)
					if err != nil {
						t.Fatal(err)
					}
					wantEndpoint := providerEndpointChatCompletions
					if tc.wantBackend == chatBackendResponses {
						wantEndpoint = providerEndpointResponses
					}
					if route.backend != tc.wantBackend || route.nativeEndpoint != wantEndpoint {
						t.Fatalf("route = backend %v endpoint %q, want backend %v endpoint %q", route.backend, route.nativeEndpoint, tc.wantBackend, wantEndpoint)
					}
					if len(owner.supportedEndpoints) != 2 || !supportsEndpoint(owner.supportedEndpoints, providerEndpointChatCompletions) || !supportsEndpoint(owner.supportedEndpoints, providerEndpointResponses) {
						t.Fatalf("native endpoints = %v, want both advertised endpoints", owner.supportedEndpoints)
					}
				})
			}
		})
	}
}

func TestChooseChatRoute_NeitherRejectsWithoutUpstream(t *testing.T) {
	provider := &providerRuntime{id: "chat-provider", kind: providerTypeOpenAICompatible}
	owner := providerModel{
		publicID:           "messages-only",
		upstreamModel:      "messages-only",
		providerID:         provider.id,
		supportedEndpoints: []string{providerEndpointMessages},
	}

	_, err := chooseChatRoute(provider, owner, true, owner.publicID)
	if err == nil {
		t.Fatal("chooseChatRoute() error = nil, want local rejection")
	}
	providerErr, ok := err.(*providerRequestError)
	if !ok {
		t.Fatalf("chooseChatRoute() error = %T, want *providerRequestError", err)
	}
	if providerErr.statusCode != 400 {
		t.Fatalf("providerRequestError.statusCode = %d, want 400", providerErr.statusCode)
	}
}

func TestChooseChatRoute_UnknownFallbackPrefersNativeChat(t *testing.T) {
	provider := &providerRuntime{id: "copilot", kind: providerTypeCopilot}
	owner := providerModel{publicID: "unknown", upstreamModel: "unknown", providerID: provider.id}

	route, err := chooseChatRoute(provider, owner, false, owner.publicID)
	if err != nil {
		t.Fatalf("chooseChatRoute() error = %v", err)
	}
	if route.backend != chatBackendNativeChat {
		t.Fatalf("route.backend = %v, want native Chat fallback", route.backend)
	}
}

func TestChooseChatRoute_UnknownCodexFallbackUsesResponses(t *testing.T) {
	provider := &providerRuntime{id: "codex", kind: providerTypeOpenAICodex}
	owner := providerModel{publicID: "unknown", upstreamModel: "unknown", providerID: provider.id}

	route, err := chooseChatRoute(provider, owner, false, owner.publicID)
	if err != nil {
		t.Fatalf("chooseChatRoute() error = %v", err)
	}
	if route.backend != chatBackendResponses || route.nativeEndpoint != providerEndpointResponses {
		t.Fatalf("route = backend %v endpoint %q, want Responses", route.backend, route.nativeEndpoint)
	}
}

func TestChooseChatRoute_CapturesOwnerSnapshot(t *testing.T) {
	parallel := false
	provider := &providerRuntime{id: "snapshot", kind: providerTypeOpenAICompatible}
	owner := providerModel{
		publicID:           "gpt-snapshot",
		upstreamModel:      "deployment-a",
		providerID:         provider.id,
		supportedEndpoints: []string{providerEndpointChatCompletions},
		parallelToolCalls:  &parallel,
		raw:                []byte(`{"id":"gpt-snapshot"}`),
	}

	route, err := chooseChatRoute(provider, owner, true, owner.publicID)
	if err != nil {
		t.Fatalf("chooseChatRoute() error = %v", err)
	}
	owner.supportedEndpoints[0] = providerEndpointResponses
	*owner.parallelToolCalls = true
	owner.raw[0] = 'x'

	if route.owner.supportedEndpoints[0] != providerEndpointChatCompletions {
		t.Fatalf("captured endpoints = %v, want native Chat snapshot", route.owner.supportedEndpoints)
	}
	if route.owner.parallelToolCalls == nil || *route.owner.parallelToolCalls {
		t.Fatalf("captured parallel_tool_calls = %v, want false snapshot", route.owner.parallelToolCalls)
	}
	if string(route.owner.raw) != `{"id":"gpt-snapshot"}` {
		t.Fatalf("captured raw metadata = %q, want immutable snapshot", route.owner.raw)
	}
}
