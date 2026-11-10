package launch

import (
	"os"
	"testing"
)

func TestAdaptersUnsetInboundAuthToken(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	model := ModelInfo{ID: "m", SupportedEndpoints: []string{"/chat/completions", "/responses"}}
	for name, adapter := range map[string]func(PrepareInput) (PreparedProcess, error){"claude": ClaudeAdapter{}.Prepare, "codex": CodexAdapter{}.Prepare, "copilot": CopilotAdapter{}.Prepare} {
		t.Run(name, func(t *testing.T) {
			prepared, err := adapter(PrepareInput{
				BaseURL: "http://127.0.0.1:43210/", Model: model, Binary: binary,
				LocalToken: "test-token-placeholder", DryRun: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			if !containsString(prepared.EnvUnset, "INBOUND_AUTH_TOKEN") {
				t.Fatalf("EnvUnset missing INBOUND_AUTH_TOKEN: %#v", prepared.EnvUnset)
			}
		})
	}
}
