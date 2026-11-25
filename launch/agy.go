package launch

import (
	"fmt"
	"strings"
)

const agySessionSettings = `{"modelProvider":""}`

var agyMinimumVersion = minimumVersion{major: 1, minor: 5, patch: 6}

var agyCredentialEnvironment = []string{
	"AGY_ADC_AUTH",
	"AGY_LLM_GATEWAY_CA_CERT",
	"AGY_LLM_GATEWAY_CLIENT_CERT",
	"AGY_LLM_GATEWAY_CLIENT_KEY",
	"AGY_LLM_GATEWAY_HEADERS",
	"AGY_LLM_GATEWAY_MODELS",
	"AGY_LLM_GATEWAY_PROXY_URL",
	"AGY_LLM_GATEWAY_TIER_FLASH",
	"AGY_LLM_GATEWAY_TIER_FLASH_LITE",
	"AGY_LLM_GATEWAY_TIER_PRO",
	"GEMINI_API_KEY",
	"GOOGLE_API_KEY",
	"GOOGLE_APPLICATION_CREDENTIALS",
	"GOOGLE_GEMINI_BASE_URL",
	"GOOGLE_VERTEX_BASE_URL",
}

type AgyAdapter struct{}

func (AgyAdapter) Name() string { return "agy" }

func (AgyAdapter) Prepare(input PrepareInput) (PreparedProcess, error) {
	model := strings.TrimSpace(input.Model.ID)
	if model == "" {
		return PreparedProcess{}, fmt.Errorf("agy launch requires a model")
	}
	if strings.TrimSpace(input.Model.OwnedBy) == PolicyModelOwner {
		return PreparedProcess{}, fmt.Errorf("model %q is a policy-routing profile, which Gemini routes do not support; pin a direct model", model)
	}
	endpointMetadataKnown := !input.DryRun || input.Model.SupportedEndpoints != nil
	if endpointMetadataKnown &&
		!modelSupportsEndpoint(input.Model, "/chat/completions") &&
		!modelSupportsEndpoint(input.Model, "/responses") {
		return PreparedProcess{}, fmt.Errorf(
			"model %q is not Antigravity CLI-compatible: expected /chat/completions or /responses support",
			model,
		)
	}
	if err := validateAgyForwardedArgs(input.ForwardedArgs); err != nil {
		return PreparedProcess{}, err
	}

	executable, err := resolveExecutable(input.Binary, "agy", []string{
		"~/.local/bin/agy",
	})
	if err != nil {
		return PreparedProcess{}, err
	}
	baseURL := strings.TrimRight(strings.TrimSpace(input.BaseURL), "/")
	if baseURL == "" {
		return PreparedProcess{}, fmt.Errorf("agy launch requires a Vekil base URL")
	}
	localToken := strings.TrimSpace(input.LocalToken)
	if localToken == "" {
		return PreparedProcess{}, fmt.Errorf("agy launch requires a local proxy token")
	}

	envUnset := mergeEnvironmentKeys(commonAgentCredentialEnvironment, agyCredentialEnvironment, input.SensitiveEnv)
	envSet := map[string]string{
		"AGY_CLI_DISABLE_AUTO_UPDATE":   "true",
		"AGY_LLM_GATEWAY_API_KEY":       localToken,
		"AGY_LLM_GATEWAY_URL":           baseURL,
		"AGY_LLM_GATEWAY_WIRE_PROTOCOL": "genai",
	}
	if !input.DryRun {
		probeEnvironment := applyEnvironment(input.Environment, envUnset, envSet)
		probeEnvironment = ensureLoopbackNoProxy(probeEnvironment, baseURL)
		if err := validateExecutableVersion(executable, probeEnvironment, "Antigravity CLI", "", agyMinimumVersion); err != nil {
			return PreparedProcess{}, err
		}
	}

	args := append([]string(nil), executable.prefixArgs...)
	args = append(args, "--settings", agySessionSettings, "--model", model)
	args = append(args, input.ForwardedArgs...)
	prepared := PreparedProcess{
		Path:     executable.path,
		Args:     args,
		EnvSet:   envSet,
		EnvUnset: envUnset,
	}
	if !endpointMetadataKnown {
		prepared.Unresolved = append(prepared.Unresolved,
			"model endpoint compatibility (/chat/completions or /responses) requires live /v1/models metadata",
		)
	}
	return prepared, nil
}

func validateAgyForwardedArgs(args []string) error {
	expectValue := false
	seenPositional := false
	for _, arg := range args {
		if expectValue {
			expectValue = false
			continue
		}
		if arg == "--" {
			return nil
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			if !seenPositional {
				switch arg {
				case "acp", "changelog", "help", "install", "mcp", "mic-serve", "plugin", "plugins", "remote-control", "update":
					return fmt.Errorf("agy command %q is not an agent session", arg)
				}
			}
			seenPositional = true
			continue
		}
		name, _, hasValue := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		switch name {
		case "model":
			return fmt.Errorf("agy model overrides are not supported by the managed Vekil launcher; use the launcher's --model")
		case "settings":
			return fmt.Errorf("agy settings overrides are not supported by the managed Vekil launcher")
		case "remote-agent", "remote-control":
			return fmt.Errorf("remote agy sessions are not supported by an ephemeral Vekil launcher")
		}
		if !hasValue && agyOptionRequiresValue(name) {
			expectValue = true
		}
	}
	if expectValue {
		return fmt.Errorf("agy option is missing its value")
	}
	return nil
}

func agyOptionRequiresValue(name string) bool {
	switch name {
	case "add-dir", "agent", "conversation", "effort", "i", "input-format", "json-schema", "log-file",
		"mode", "model", "output-format", "p", "print", "print-timeout", "project", "prompt",
		"prompt-interactive", "r", "resume", "settings":
		return true
	default:
		return false
	}
}
