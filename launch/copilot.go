package launch

import (
	"fmt"
	"strconv"
	"strings"
)

var copilotCredentialEnvironment = []string{
	"ANTHROPIC_API_KEY",
	"ANTHROPIC_AUTH_TOKEN",
	"AZURE_OPENAI_API_KEY",
	"COPILOT_ALLOW_ALL",
	"COPILOT_GITHUB_TOKEN",
	"COPILOT_PROVIDER_API_KEY",
	"COPILOT_PROVIDER_BEARER_TOKEN",
	"GH_ENTERPRISE_TOKEN",
	"GH_TOKEN",
	"GITHUB_ENTERPRISE_TOKEN",
	"GITHUB_TOKEN",
	"OPENAI_API_KEY",
	"COPILOT_OTEL_FILE_EXPORTER_PATH",
	"OTEL_EXPORTER_OTLP_ENDPOINT",
	"OTEL_EXPORTER_OTLP_HEADERS",
	"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT",
	"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
	"OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT",
}

// CopilotAdapter prepares GitHub Copilot CLI to use Vekil as a BYOK provider.
type CopilotAdapter struct{}

func (CopilotAdapter) Name() string { return "copilot" }

func (CopilotAdapter) Prepare(input PrepareInput) (PreparedProcess, error) {
	model := strings.TrimSpace(input.Model.ID)
	if model == "" {
		return PreparedProcess{}, fmt.Errorf("copilot launch requires a model")
	}
	wireAPI, err := copilotWireAPI(input.Model, input.DryRun)
	if err != nil {
		return PreparedProcess{}, err
	}
	if err := validateCopilotForwardedArgs(input.ForwardedArgs); err != nil {
		return PreparedProcess{}, err
	}

	executable, err := resolveExecutable(input.Binary, "copilot", []string{
		"~/.local/bin/copilot",
		"~/.copilot/bin/copilot",
	})
	if err != nil {
		return PreparedProcess{}, err
	}
	baseURL := strings.TrimRight(strings.TrimSpace(input.BaseURL), "/")
	if baseURL == "" {
		return PreparedProcess{}, fmt.Errorf("copilot launch requires a Vekil base URL")
	}
	localToken := strings.TrimSpace(input.LocalToken)
	if localToken == "" {
		return PreparedProcess{}, fmt.Errorf("copilot launch requires a local proxy token")
	}

	modelID := strings.TrimSpace(input.Model.Capabilities.Family)
	if modelID == "" {
		modelID = model
	}
	envUnset := mergeEnvironmentKeys(commonAgentCredentialEnvironment, copilotCredentialEnvironment, input.SensitiveEnv, []string{
		"COPILOT_PROVIDER_AZURE_API_VERSION",
		"COPILOT_PROVIDER_MAX_OUTPUT_TOKENS",
		"COPILOT_PROVIDER_MAX_PROMPT_TOKENS",
	})
	envSet := map[string]string{
		"COPILOT_AUTO_UPDATE":           "false",
		"COPILOT_MODEL":                 model,
		"COPILOT_OFFLINE":               "true",
		"COPILOT_OTEL_ENABLED":          "false",
		"COPILOT_PROVIDER_API_KEY":      "",
		"COPILOT_PROVIDER_BASE_URL":     openAIBaseURL(baseURL),
		"COPILOT_PROVIDER_BEARER_TOKEN": localToken,
		"COPILOT_PROVIDER_MODEL_ID":     modelID,
		"COPILOT_PROVIDER_TRANSPORT":    "http",
		"COPILOT_PROVIDER_TYPE":         "openai",
		"COPILOT_PROVIDER_WIRE_API":     wireAPI,
		"COPILOT_PROVIDER_WIRE_MODEL":   model,
	}
	if value := input.Model.Capabilities.Limits.MaxPromptTokens; value > 0 {
		envSet["COPILOT_PROVIDER_MAX_PROMPT_TOKENS"] = strconv.FormatInt(value, 10)
	}
	if value := input.Model.Capabilities.Limits.MaxOutputTokens; value > 0 {
		envSet["COPILOT_PROVIDER_MAX_OUTPUT_TOKENS"] = strconv.FormatInt(value, 10)
	}

	probeEnvironment := applyEnvironment(input.Environment, envUnset, map[string]string{
		"COPILOT_AUTO_UPDATE":  "false",
		"COPILOT_OFFLINE":      "true",
		"COPILOT_OTEL_ENABLED": "false",
	})
	probeEnvironment = ensureLoopbackNoProxy(probeEnvironment, baseURL)
	if !input.DryRun {
		if err := validateExecutableVersion(executable, probeEnvironment, "GitHub Copilot CLI", "GitHub Copilot CLI", minimumVersion{major: 1, minor: 0, patch: 0}); err != nil {
			return PreparedProcess{}, err
		}
	}

	secretNames := sortedEnvironmentNames([]string{"COPILOT_PROVIDER_BEARER_TOKEN"}, envUnset)
	args := append([]string(nil), executable.prefixArgs...)
	args = append(args,
		"--no-auto-update",
		"--no-remote",
		"--no-remote-export",
		"--disable-builtin-mcps",
		"--secret-env-vars="+strings.Join(secretNames, ","),
	)
	args = append(args, input.ForwardedArgs...)
	return PreparedProcess{
		Path:     executable.path,
		Args:     args,
		EnvSet:   envSet,
		EnvUnset: envUnset,
	}, nil
}

func copilotWireAPI(model ModelInfo, dryRun bool) (string, error) {
	if modelSupportsEndpoint(model, "/responses") {
		return "responses", nil
	}
	if modelSupportsEndpoint(model, "/chat/completions") {
		return "completions", nil
	}
	if dryRun {
		return "responses", nil
	}
	return "", fmt.Errorf(
		"model %q is not GitHub Copilot CLI-compatible: expected /responses or /chat/completions support",
		model.ID,
	)
}

func validateCopilotForwardedArgs(args []string) error {
	var positionals []string
	expectValue := false
	endOptions := false
	for _, arg := range args {
		if expectValue {
			expectValue = false
			continue
		}
		if !endOptions && arg == "--" {
			endOptions = true
			continue
		}
		if !endOptions && strings.HasPrefix(arg, "-") {
			switch {
			case arg == "--model" || strings.HasPrefix(arg, "--model="),
				arg == "--secret-env-vars" || strings.HasPrefix(arg, "--secret-env-vars="),
				arg == "--agent" || strings.HasPrefix(arg, "--agent="):
				return fmt.Errorf("copilot model, agent, or environment overrides are not supported by the managed Vekil launcher")
			case arg == "--no-auto-update" || strings.HasPrefix(arg, "--no-auto-update="),
				arg == "--no-remote" || strings.HasPrefix(arg, "--no-remote="),
				arg == "--no-remote-export" || strings.HasPrefix(arg, "--no-remote-export="),
				arg == "--disable-builtin-mcps" || strings.HasPrefix(arg, "--disable-builtin-mcps="):
				return fmt.Errorf("copilot launcher safety overrides are managed by Vekil")
			case arg == "--connect" || strings.HasPrefix(arg, "--connect="),
				arg == "--continue",
				arg == "--resume" || strings.HasPrefix(arg, "--resume="), arg == "-r" || strings.HasPrefix(arg, "-r="), (strings.HasPrefix(arg, "-r") && len(arg) > 2),
				arg == "--session-id" || strings.HasPrefix(arg, "--session-id="),
				arg == "--remote" || strings.HasPrefix(arg, "--remote="),
				arg == "--remote-export" || strings.HasPrefix(arg, "--remote-export="),
				arg == "--share" || strings.HasPrefix(arg, "--share="), arg == "--share-gist":
				return fmt.Errorf("remote, resumed, or shared Copilot sessions are not supported by an ephemeral Vekil launcher")
			case arg == "--acp":
				return fmt.Errorf("copilot ACP server mode is not supported by an ephemeral Vekil launcher")
			}
			if copilotOptionRequiresValue(arg) {
				expectValue = true
			}
			continue
		}
		positionals = append(positionals, arg)
	}
	if expectValue {
		return fmt.Errorf("copilot option is missing its value")
	}
	if len(positionals) == 0 {
		return nil
	}
	switch positionals[0] {
	case "completion", "help", "init", "login", "mcp", "plugin", "plugins", "skill", "update", "version":
		return fmt.Errorf("copilot command %q is not an agent session", positionals[0])
	default:
		return nil
	}
}

func copilotOptionRequiresValue(arg string) bool {
	if strings.Contains(arg, "=") {
		return false
	}
	switch arg {
	case "--add-dir", "--add-github-mcp-tool", "--add-github-mcp-toolset", "--additional-mcp-config",
		"--agent", "--allow-url", "--attachment", "-C", "--config-dir", "--context", "--deny-url", "--disable-mcp-server",
		"--effort", "--reasoning-effort", "--extension-sdk-path", "-i", "--interactive", "--log-dir", "--log-file", "--log-level",
		"--max-ai-credits", "--max-autopilot-continues", "--mode", "--model", "--name", "-n", "--output-format", "-p", "--prompt", "--plugin-dir",
		"--secret-env-vars", "--session-id", "--stream":
		return true
	default:
		return false
	}
}
