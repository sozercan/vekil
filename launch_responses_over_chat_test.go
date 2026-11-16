package main

import (
	"testing"

	"github.com/sozercan/vekil/launch"
)

func TestLaunchResponsesOverChatOptedInMirrorsServerDecision(t *testing.T) {
	cases := []struct {
		name  string
		optIn string
		model launch.ModelInfo
		want  bool
	}{
		{"chat only listed", "a, gemini", launch.ModelInfo{ID: "gemini", SupportedEndpoints: []string{"/chat/completions"}}, true},
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
