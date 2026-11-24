package launch

import (
	"os"
	"strings"
	"testing"
)

func prepareAgyForTest(t *testing.T, model ModelInfo, forwarded ...string) (PreparedProcess, error) {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return (AgyAdapter{}).Prepare(PrepareInput{
		BaseURL:       "http://127.0.0.1:43210/",
		Model:         model,
		Binary:        binary,
		ForwardedArgs: forwarded,
		LocalToken:    "test-token-placeholder",
		SensitiveEnv:  []string{"MY_PROVIDER_TOKEN"},
		DryRun:        true,
	})
}

func TestAgyAdapterPrepareUsesGateway(t *testing.T) {
	prepared, err := prepareAgyForTest(t,
		ModelInfo{ID: "gemini-3.8-flash", SupportedEndpoints: []string{"/chat/completions"}},
		"--dangerously-skip-permissions", "-p", "hello",
	)
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	for key, want := range map[string]string{
		"AGY_CLI_DISABLE_AUTO_UPDATE":   "true",
		"AGY_LLM_GATEWAY_API_KEY":       "test-token-placeholder",
		"AGY_LLM_GATEWAY_URL":           "http://127.0.0.1:43210",
		"AGY_LLM_GATEWAY_WIRE_PROTOCOL": "genai",
	} {
		if got := prepared.EnvSet[key]; got != want {
			t.Errorf("EnvSet[%q] = %q, want %q", key, got, want)
		}
	}
	for _, key := range []string{"GEMINI_API_KEY", "GOOGLE_API_KEY", "GOOGLE_GEMINI_BASE_URL", "AGY_ADC_AUTH", "AGY_LLM_GATEWAY_HEADERS", "AGY_LLM_GATEWAY_MODELS", "GITHUB_TOKEN", "MY_PROVIDER_TOKEN"} {
		if !containsString(prepared.EnvUnset, key) {
			t.Errorf("EnvUnset missing %q: %#v", key, prepared.EnvUnset)
		}
	}
	want := "--settings|" + agySessionSettings + "|--model|gemini-3.8-flash|--dangerously-skip-permissions|-p|hello"
	if got := strings.Join(prepared.Args, "|"); got != want {
		t.Fatalf("args = %q, want %q", got, want)
	}
	if strings.Contains(strings.Join(prepared.Args, "\n"), "test-token-placeholder") {
		t.Fatal("agy argv exposed the local proxy token")
	}
	if len(prepared.Unresolved) != 0 {
		t.Fatalf("Unresolved = %#v, want none", prepared.Unresolved)
	}
}

func TestAgyAdapterAcceptsResponsesBackedModel(t *testing.T) {
	if _, err := prepareAgyForTest(t, ModelInfo{ID: "gpt-5.4", SupportedEndpoints: []string{"/responses"}}); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
}

func TestAgyAdapterRejectsUnsupportedModels(t *testing.T) {
	for _, tc := range []struct {
		name  string
		model ModelInfo
		want  string
	}{
		{"missing model", ModelInfo{}, "requires a model"},
		{"policy profile", ModelInfo{ID: "auto", OwnedBy: PolicyModelOwner, SupportedEndpoints: []string{"/chat/completions"}}, "policy-routing profile"},
		{"messages only", ModelInfo{ID: "claude-native", SupportedEndpoints: []string{"/v1/messages"}}, "not Antigravity CLI-compatible"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := prepareAgyForTest(t, tc.model)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Prepare() error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestAgyAdapterDryRunMarksUnknownEndpointsUnresolved(t *testing.T) {
	prepared, err := prepareAgyForTest(t, ModelInfo{ID: "catalog-model"})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if len(prepared.Unresolved) != 1 || !strings.Contains(prepared.Unresolved[0], "/chat/completions or /responses") {
		t.Fatalf("Unresolved = %#v", prepared.Unresolved)
	}
}

func TestAgyAdapterForwardedArgs(t *testing.T) {
	model := ModelInfo{ID: "gemini-3.8-flash", SupportedEndpoints: []string{"/chat/completions"}}
	for _, args := range [][]string{
		{"--continue"},
		{"-r", "conversation-id"},
		{"--conversation=conversation-id", "-p", "review"},
		{"-p", "install the deps"},
		{"--mode", "plan", "-i", "start"},
		{"models"},
		{"--", "--model"},
	} {
		if _, err := prepareAgyForTest(t, model, args...); err != nil {
			t.Errorf("Prepare(%q) error = %v", args, err)
		}
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--model", "other"}, "model overrides"},
		{[]string{"-model=other"}, "model overrides"},
		{[]string{"--settings", "{}"}, "settings overrides"},
		{[]string{"--remote-agent"}, "remote agy sessions"},
		{[]string{"--remote-control=true"}, "remote agy sessions"},
		{[]string{"acp"}, `"acp" is not an agent session`},
		{[]string{"--sandbox", "update"}, `"update" is not an agent session`},
		{[]string{"-p"}, "missing its value"},
	} {
		_, err := prepareAgyForTest(t, model, tc.args...)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Prepare(%q) error = %v, want %q", tc.args, err, tc.want)
		}
	}
}

func TestAgyAdapterRejectsOldVersion(t *testing.T) {
	if err := validateVersionOutput("1.5.5\n", "Antigravity CLI", "", agyMinimumVersion); err == nil {
		t.Fatal("validateVersionOutput accepted 1.5.5")
	}
	if err := validateVersionOutput("1.5.16\n", "Antigravity CLI", "", agyMinimumVersion); err != nil {
		t.Fatalf("validateVersionOutput(1.5.16) error = %v", err)
	}
}
