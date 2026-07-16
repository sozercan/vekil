package proxy

import (
	"reflect"
	"testing"
)

func TestProviderEndpointPolicyDefaults(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		kind           providerType
		wantStatic     []string
		wantDynamic    []string
		wantChatPath   string
		wantMessages   string
		wantModelsPath string
	}{
		{
			name:           "copilot keeps legacy open routing paths",
			kind:           providerTypeCopilot,
			wantStatic:     []string{providerEndpointChatCompletions, providerEndpointResponses},
			wantChatPath:   providerEndpointChatCompletions,
			wantMessages:   providerEndpointMessages,
			wantModelsPath: providerEndpointModels,
		},
		{
			name:           "azure static models default to OpenAI routes",
			kind:           providerTypeAzureOpenAI,
			wantStatic:     []string{providerEndpointChatCompletions, providerEndpointResponses},
			wantChatPath:   providerEndpointChatCompletions,
			wantMessages:   providerEndpointMessages,
			wantModelsPath: providerEndpointModels,
		},
		{
			name:           "openai codex is responses only",
			kind:           providerTypeOpenAICodex,
			wantStatic:     []string{providerEndpointResponses},
			wantDynamic:    []string{providerEndpointResponses},
			wantMessages:   "",
			wantModelsPath: providerEndpointModels,
		},
		{
			name:           "openai compatible static and dynamic default to chat",
			kind:           providerTypeOpenAICompatible,
			wantStatic:     []string{providerEndpointChatCompletions},
			wantDynamic:    []string{providerEndpointChatCompletions},
			wantChatPath:   providerEndpointChatCompletions,
			wantMessages:   providerEndpointMessages,
			wantModelsPath: providerEndpointModels,
		},
		{
			name:           "anthropic compatible defaults to messages",
			kind:           providerTypeAnthropicCompatible,
			wantStatic:     []string{providerEndpointMessages},
			wantDynamic:    []string{providerEndpointMessages},
			wantChatPath:   providerEndpointChatCompletions,
			wantMessages:   providerEndpointMessages,
			wantModelsPath: providerEndpointModels,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			provider := &providerRuntime{kind: tt.kind}
			if got := provider.defaultStaticModelEndpoints(); !reflect.DeepEqual(got, tt.wantStatic) {
				t.Fatalf("defaultStaticModelEndpoints() = %v, want %v", got, tt.wantStatic)
			}
			if got := provider.defaultDynamicModelEndpoints(); !reflect.DeepEqual(got, tt.wantDynamic) {
				t.Fatalf("defaultDynamicModelEndpoints() = %v, want %v", got, tt.wantDynamic)
			}

			paths := providerEndpointPolicyFor(tt.kind).defaultEndpointPaths()
			if paths.chatCompletions != tt.wantChatPath {
				t.Fatalf("chatCompletions path = %q, want %q", paths.chatCompletions, tt.wantChatPath)
			}
			if paths.messages != tt.wantMessages {
				t.Fatalf("messages path = %q, want %q", paths.messages, tt.wantMessages)
			}
			if paths.models != tt.wantModelsPath {
				t.Fatalf("models path = %q, want %q", paths.models, tt.wantModelsPath)
			}
		})
	}
}

func TestNeedsDynamicProviderModelValidation(t *testing.T) {
	t.Parallel()

	dynamic := func() *providerRuntime {
		return &providerRuntime{kind: providerTypeOpenAICompatible, modelDiscovery: providerModelDiscoveryOpenAI}
	}
	static := func() *providerRuntime {
		return &providerRuntime{kind: providerTypeOpenAICompatible, modelDiscovery: providerModelDiscoveryStatic}
	}

	tests := []struct {
		name      string
		providers map[string]*providerRuntime
		want      bool
	}{
		{name: "no providers", providers: nil, want: false},
		{name: "single static provider", providers: map[string]*providerRuntime{"static": static()}, want: false},
		{name: "single unfiltered dynamic provider", providers: map[string]*providerRuntime{"dynamic": dynamic()}, want: false},
		{
			name: "single dynamic provider with include_models",
			providers: map[string]*providerRuntime{
				"dynamic": {kind: providerTypeOpenAICompatible, modelDiscovery: providerModelDiscoveryOpenAI, includeModels: map[string]struct{}{"allowed": {}}},
			},
			want: true,
		},
		{
			name: "single dynamic provider with exclude_models",
			providers: map[string]*providerRuntime{
				"dynamic": {kind: providerTypeOpenAICompatible, modelDiscovery: providerModelDiscoveryOpenAI, excludeModels: map[string]struct{}{"blocked": {}}},
			},
			want: true,
		},
		{
			name: "multiple providers with a dynamic provider",
			providers: map[string]*providerRuntime{
				"dynamic": dynamic(),
				"static":  static(),
			},
			want: true,
		},
		{
			name: "multiple static providers",
			providers: map[string]*providerRuntime{
				"first":  static(),
				"second": static(),
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := needsDynamicProviderModelValidation(tt.providers); got != tt.want {
				t.Fatalf("needsDynamicProviderModelValidation() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestProviderEndpointPolicyRouting(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                string
		provider            providerRuntime
		endpoint            string
		wantSupports        bool
		wantUnknownModel    bool
		wantDiscoveredModel bool
	}{
		{
			name:                "copilot preserves legacy passthrough",
			provider:            providerRuntime{kind: providerTypeCopilot},
			endpoint:            providerEndpointMessages,
			wantSupports:        true,
			wantUnknownModel:    true,
			wantDiscoveredModel: true,
		},
		{
			name:                "azure configured catalog rejects unknown models",
			provider:            providerRuntime{kind: providerTypeAzureOpenAI},
			endpoint:            providerEndpointResponses,
			wantSupports:        true,
			wantUnknownModel:    false,
			wantDiscoveredModel: true,
		},
		{
			name:                "openai codex routes responses only",
			provider:            providerRuntime{kind: providerTypeOpenAICodex},
			endpoint:            providerEndpointResponses,
			wantSupports:        true,
			wantUnknownModel:    true,
			wantDiscoveredModel: true,
		},
		{
			name:                "openai codex rejects chat",
			provider:            providerRuntime{kind: providerTypeOpenAICodex},
			endpoint:            providerEndpointChatCompletions,
			wantSupports:        false,
			wantUnknownModel:    false,
			wantDiscoveredModel: false,
		},
		{
			name:                "static openai compatible supports responses but rejects unknown models",
			provider:            providerRuntime{kind: providerTypeOpenAICompatible, modelDiscovery: providerModelDiscoveryStatic},
			endpoint:            providerEndpointResponses,
			wantSupports:        true,
			wantUnknownModel:    false,
			wantDiscoveredModel: true,
		},
		{
			name:                "dynamic openai compatible allows unknown chat models",
			provider:            providerRuntime{kind: providerTypeOpenAICompatible, modelDiscovery: providerModelDiscoveryOpenAI},
			endpoint:            providerEndpointChatCompletions,
			wantSupports:        true,
			wantUnknownModel:    true,
			wantDiscoveredModel: true,
		},
		{
			name: "filtered dynamic openai compatible rejects unknown chat models",
			provider: providerRuntime{
				kind:           providerTypeOpenAICompatible,
				modelDiscovery: providerModelDiscoveryOpenAI,
				includeModels:  map[string]struct{}{"allowed": {}},
			},
			endpoint:            providerEndpointChatCompletions,
			wantSupports:        true,
			wantUnknownModel:    false,
			wantDiscoveredModel: true,
		},
		{
			name:                "dynamic openai compatible still rejects unknown responses models",
			provider:            providerRuntime{kind: providerTypeOpenAICompatible, modelDiscovery: providerModelDiscoveryOpenAI},
			endpoint:            providerEndpointResponses,
			wantSupports:        true,
			wantUnknownModel:    false,
			wantDiscoveredModel: true,
		},
		{
			name:                "dynamic anthropic compatible allows unknown messages models",
			provider:            providerRuntime{kind: providerTypeAnthropicCompatible, modelDiscovery: providerModelDiscoveryOpenAI},
			endpoint:            providerEndpointMessages,
			wantSupports:        true,
			wantUnknownModel:    true,
			wantDiscoveredModel: true,
		},
		{
			name:                "anthropic compatible rejects chat everywhere",
			provider:            providerRuntime{kind: providerTypeAnthropicCompatible, modelDiscovery: providerModelDiscoveryOpenAI},
			endpoint:            providerEndpointChatCompletions,
			wantSupports:        false,
			wantUnknownModel:    false,
			wantDiscoveredModel: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := tt.provider.supportsEndpoint(tt.endpoint); got != tt.wantSupports {
				t.Fatalf("supportsEndpoint(%q) = %v, want %v", tt.endpoint, got, tt.wantSupports)
			}
			if got := tt.provider.allowsUnknownModelEndpoint(tt.endpoint); got != tt.wantUnknownModel {
				t.Fatalf("allowsUnknownModelEndpoint(%q) = %v, want %v", tt.endpoint, got, tt.wantUnknownModel)
			}
			if got := tt.provider.acceptsDiscoveredModelEndpoint(tt.endpoint); got != tt.wantDiscoveredModel {
				t.Fatalf("acceptsDiscoveredModelEndpoint(%q) = %v, want %v", tt.endpoint, got, tt.wantDiscoveredModel)
			}
		})
	}
}
