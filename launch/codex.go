package launch

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
)

const codexLocalTokenEnv = "VEKIL_CODEX_API_KEY"

var codexCredentialEnvironment = []string{
	"AZURE_OPENAI_API_KEY",
	"AZURE_OPENAI_ENDPOINT",
	"CODEX_API_KEY",
	"OPENAI_ACCOUNT_ID",
	"OPENAI_API_KEY",
	"OPENAI_BASE_URL",
	"OPENAI_ORGANIZATION",
	"OPENAI_ORG_ID",
	"OPENAI_PROJECT",
	"OPENAI_PROJECT_ID",
}

// CodexAdapter prepares OpenAI Codex CLI to use Vekil's Responses API.
type CodexAdapter struct{}

func (CodexAdapter) Name() string { return "codex" }

func (CodexAdapter) Prepare(input PrepareInput) (PreparedProcess, error) {
	model := strings.TrimSpace(input.Model.ID)
	if model == "" {
		return PreparedProcess{}, fmt.Errorf("codex launch requires a model")
	}
	if !input.DryRun && !modelSupportsEndpoint(input.Model, "/responses") {
		return PreparedProcess{}, fmt.Errorf(
			"model %q is not Codex-compatible: expected /responses support",
			model,
		)
	}
	if err := validateCodexForwardedArgs(input.ForwardedArgs); err != nil {
		return PreparedProcess{}, err
	}

	executable, err := resolveExecutable(input.Binary, "codex", []string{
		"~/.npm-global/bin/codex",
		"~/.local/bin/codex",
	})
	if err != nil {
		return PreparedProcess{}, err
	}
	baseURL := strings.TrimRight(strings.TrimSpace(input.BaseURL), "/")
	if baseURL == "" {
		return PreparedProcess{}, fmt.Errorf("codex launch requires a Vekil base URL")
	}
	localToken := strings.TrimSpace(input.LocalToken)
	if localToken == "" {
		return PreparedProcess{}, fmt.Errorf("codex launch requires a local proxy token")
	}

	envUnset := mergeEnvironmentKeys(commonAgentCredentialEnvironment, codexCredentialEnvironment, input.SensitiveEnv)
	envSet := map[string]string{codexLocalTokenEnv: localToken}
	probeEnvironment := applyEnvironment(input.Environment, envUnset, nil)
	probeEnvironment = ensureLoopbackNoProxy(probeEnvironment, baseURL)
	if !input.DryRun {
		if err := validateExecutableVersion(executable, probeEnvironment, "Codex CLI", "codex-cli", minimumVersion{major: 0, minor: 137, patch: 0}); err != nil {
			return PreparedProcess{}, err
		}
	}

	catalogJSON, err := buildCodexModelCatalog(executable, probeEnvironment, input.Model, input.DryRun)
	if err != nil {
		return PreparedProcess{}, fmt.Errorf("build Codex model catalog: %w", err)
	}
	catalogPath, cleanup, err := writePrivateTempFile("vekil-codex-models-", append(catalogJSON, '\n'))
	if err != nil {
		return PreparedProcess{}, fmt.Errorf("write Codex model catalog: %w", err)
	}

	providerID := codexProviderID(localToken)
	args := append([]string(nil), executable.prefixArgs...)
	for _, override := range []string{
		`model_provider=` + configString(providerID),
		`model_providers.` + providerID + `.name="Vekil"`,
		`model_providers.` + providerID + `.base_url=` + configString(openAIBaseURL(baseURL)),
		`model_providers.` + providerID + `.wire_api="responses"`,
		`model_providers.` + providerID + `.env_key=` + configString(codexLocalTokenEnv),
		`model_providers.` + providerID + `.requires_openai_auth=false`,
		`model_providers.` + providerID + `.supports_websockets=false`,
		`model_catalog_json=` + configString(catalogPath),
		`shell_environment_policy.set.` + codexLocalTokenEnv + `=""`,
	} {
		args = append(args, "-c", override)
	}
	args = append(args, "-m", model)
	args = append(args, input.ForwardedArgs...)

	return PreparedProcess{
		Path:     executable.path,
		Args:     args,
		EnvSet:   envSet,
		EnvUnset: envUnset,
		Cleanup:  cleanup,
	}, nil
}

func configString(value string) string {
	body, _ := json.Marshal(value)
	return string(body)
}

func codexProviderID(localToken string) string {
	digest := sha256.Sum256([]byte(localToken))
	return "vekil_launch_" + fmt.Sprintf("%x", digest[:6])
}

func validateCodexForwardedArgs(args []string) error {
	var positionals []string
	expectValue := false
	for _, arg := range args {
		if expectValue {
			expectValue = false
			continue
		}
		if arg == "--" {
			break
		}
		if strings.HasPrefix(arg, "-") {
			switch {
			case arg == "-m" || strings.HasPrefix(arg, "-m=") || (strings.HasPrefix(arg, "-m") && len(arg) > 2) || strings.HasPrefix(arg, "--model="), arg == "--model",
				arg == "-c" || strings.HasPrefix(arg, "-c=") || (strings.HasPrefix(arg, "-c") && len(arg) > 2) || strings.HasPrefix(arg, "--config="), arg == "--config",
				arg == "-p" || strings.HasPrefix(arg, "-p=") || (strings.HasPrefix(arg, "-p") && len(arg) > 2) || strings.HasPrefix(arg, "--profile="), arg == "--profile",
				arg == "--oss", arg == "--local-provider" || strings.HasPrefix(arg, "--local-provider="):
				return fmt.Errorf("codex model or provider overrides are not supported by the managed Vekil launcher")
			case arg == "--remote" || strings.HasPrefix(arg, "--remote="),
				arg == "--remote-auth-token-env" || strings.HasPrefix(arg, "--remote-auth-token-env="):
				return fmt.Errorf("remote Codex sessions are not supported by an ephemeral Vekil launcher")
			}
			if codexOptionRequiresValue(arg) {
				expectValue = true
			}
			continue
		}
		positionals = append(positionals, arg)
	}
	if expectValue {
		return fmt.Errorf("codex option is missing its value")
	}
	if len(positionals) == 0 {
		return nil
	}
	switch positionals[0] {
	case "exec", "e":
		if len(positionals) > 1 && positionals[1] == "resume" {
			return fmt.Errorf("resumed Codex sessions are not supported by an ephemeral Vekil launcher")
		}
		return nil
	case "review":
		return nil
	case "resume", "fork":
		return fmt.Errorf("resumed Codex sessions are not supported by an ephemeral Vekil launcher")
	case "app-server", "remote-control", "cloud", "cloud-tasks", "responses-api-proxy", "stdio-to-uds", "exec-server", "mcp-server":
		return fmt.Errorf("detached or server Codex modes are not supported by an ephemeral Vekil launcher")
	case "login", "logout", "mcp", "plugin", "plugins", "app", "completion", "update", "doctor", "debug", "execpolicy", "apply", "a", "archive", "delete", "unarchive", "features", "sandbox", "help":
		return fmt.Errorf("codex command %q is not an agent session", positionals[0])
	default:
		return nil
	}
}

func codexOptionRequiresValue(arg string) bool {
	if strings.Contains(arg, "=") {
		return false
	}
	switch arg {
	case "--enable", "--disable", "-i", "--image", "-s", "--sandbox", "-C", "--cd", "--add-dir",
		"-a", "--ask-for-approval", "--output-schema", "--color", "-o", "--output-last-message",
		"--remote", "--remote-auth-token-env":
		return true
	default:
		return false
	}
}
