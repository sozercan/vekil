package main

import (
	"io"
	"strings"
	"testing"

	"github.com/sozercan/vekil/launch"
)

func TestLaunchResponsesOverChatOptedInMirrorsServerDecision(t *testing.T) {
	noTools := false
	supportsTools := true
	cases := []struct {
		name  string
		optIn string
		model launch.ModelInfo
		want  bool
	}{
		{"chat only listed", "a, gemini", launch.ModelInfo{ID: "gemini", SupportedEndpoints: []string{"/chat/completions"}}, true},
		{"known tool support", "gemini", launch.ModelInfo{ID: "gemini", SupportedEndpoints: []string{"/chat/completions"}, Capabilities: launch.ModelCapabilities{Supports: launch.ModelCapabilitySupports{ToolCalls: &supportsTools}}}, true},
		{"known tool rejection", "gemini", launch.ModelInfo{ID: "gemini", SupportedEndpoints: []string{"/chat/completions"}, Capabilities: launch.ModelCapabilities{Supports: launch.ModelCapabilitySupports{ToolCalls: &noTools}}}, false},
		{"not listed", "other", launch.ModelInfo{ID: "gemini", SupportedEndpoints: []string{"/chat/completions"}}, false},
		{"native responses stays native", "gpt", launch.ModelInfo{ID: "gpt", SupportedEndpoints: []string{"/chat/completions", "/responses"}}, false},
		{"no endpoint metadata", "gemini", launch.ModelInfo{ID: "gemini"}, false},
		{"policy model", "policy", launch.ModelInfo{ID: "policy", OwnedBy: launch.PolicyModelOwner, SupportedEndpoints: []string{"/chat/completions"}}, false},
		{"messages only", "claude", launch.ModelInfo{ID: "claude", SupportedEndpoints: []string{"/v1/messages"}}, false},
	}
	for _, tc := range cases {
		if got := launchResponsesOverChatOptedIn(tc.optIn, tc.model); got != tc.want {
			t.Fatalf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestLaunchCodexWithResponsesOverChatRequiresModel(t *testing.T) {
	t.Setenv("RESPONSES_OVER_CHAT_MODELS", "")
	codex, _ := launchTarget("codex")
	if _, err := parseLaunchAgentOptions(codex, []string{"--responses-over-chat-models", "gemini-test"}, io.Discard); err == nil || !strings.Contains(err.Error(), "--model is required with --responses-over-chat-models") {
		t.Fatalf("error = %v, want --model requirement", err)
	}
	t.Setenv("RESPONSES_OVER_CHAT_MODELS", "gemini-test")
	if _, err := parseLaunchAgentOptions(codex, nil, io.Discard); err == nil {
		t.Fatal("environment opt-in without --model was accepted")
	}
	if _, err := parseLaunchAgentOptions(codex, []string{"--model", "gemini-test"}, io.Discard); err != nil {
		t.Fatalf("pinned model rejected: %v", err)
	}
	t.Setenv("RESPONSES_OVER_CHAT_MODELS", "")
	if _, err := parseLaunchAgentOptions(codex, nil, io.Discard); err != nil {
		t.Fatalf("plain codex launch rejected: %v", err)
	}
	claude, _ := launchTarget("claude")
	if _, err := parseLaunchAgentOptions(claude, []string{"--responses-over-chat-models", "gemini-test"}, io.Discard); err != nil {
		t.Fatalf("non-Responses agent rejected: %v", err)
	}
}
