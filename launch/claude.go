package launch

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ClaudeAdapter prepares Claude Code to use Vekil's Anthropic-compatible
// surface without modifying the user's Claude settings.
type ClaudeAdapter struct{}

func (ClaudeAdapter) Name() string { return "claude" }

func (ClaudeAdapter) Prepare(input PrepareInput) (PreparedProcess, error) {
	model := strings.TrimSpace(input.Model.ID)
	if model == "" {
		return PreparedProcess{}, fmt.Errorf("claude launch requires a model")
	}
	if !input.DryRun &&
		!modelSupportsEndpoint(input.Model, "/v1/messages") &&
		!modelSupportsEndpoint(input.Model, "/chat/completions") {
		return PreparedProcess{}, fmt.Errorf(
			"model %q is not Claude-compatible: expected /v1/messages or /chat/completions support",
			model,
		)
	}

	executable, err := resolveExecutable(input.Binary, "claude", []string{
		"~/.claude/local/claude",
		"~/.local/bin/claude",
	})
	if err != nil {
		return PreparedProcess{}, err
	}
	baseURL := strings.TrimRight(strings.TrimSpace(input.BaseURL), "/")
	if baseURL == "" {
		return PreparedProcess{}, fmt.Errorf("claude launch requires a Vekil base URL")
	}
	localToken := strings.TrimSpace(input.LocalToken)
	if localToken == "" {
		return PreparedProcess{}, fmt.Errorf("claude launch requires a local proxy token")
	}
	name := strings.TrimSpace(input.Model.Name)
	if name == "" {
		name = model
	}
	if err := validateClaudeForwardedArgs(input.ForwardedArgs); err != nil {
		return PreparedProcess{}, err
	}

	envSet := map[string]string{
		"ANTHROPIC_BASE_URL":                        baseURL,
		"ANTHROPIC_AUTH_TOKEN":                      localToken,
		"ANTHROPIC_API_KEY":                         "",
		"ANTHROPIC_MODEL":                           model,
		"ANTHROPIC_DEFAULT_HAIKU_MODEL":             model,
		"ANTHROPIC_DEFAULT_SONNET_MODEL":            model,
		"ANTHROPIC_DEFAULT_OPUS_MODEL":              model,
		"ANTHROPIC_DEFAULT_FABLE_MODEL":             model,
		"ANTHROPIC_CUSTOM_MODEL_OPTION":             model,
		"ANTHROPIC_CUSTOM_MODEL_OPTION_NAME":        name,
		"ANTHROPIC_CUSTOM_MODEL_OPTION_DESCRIPTION": "Routed through Vekil",
		"CLAUDE_CODE_SUBAGENT_MODEL":                model,
		"CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST":      "vekil",
		"CLAUDE_CODE_SUBPROCESS_ENV_SCRUB":          "1",
	}
	envUnset := []string{
		"ANTHROPIC_CUSTOM_HEADERS",
		"CLAUDE_CODE_OAUTH_TOKEN",
		"CLAUDE_CODE_OAUTH_REFRESH_TOKEN",
		"CLAUDE_CODE_OAUTH_SCOPES",
		"ANTHROPIC_SMALL_FAST_MODEL",
		"ANTHROPIC_SMALL_FAST_MODEL_AWS_REGION",
		"CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY",
		"CLAUDE_AUTO_BACKGROUND_TASKS",
		"CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS",
		"CLAUDE_CODE_USE_BEDROCK",
		"CLAUDE_CODE_USE_VERTEX",
		"CLAUDE_CODE_USE_FOUNDRY",
		"CLAUDE_CODE_USE_MANTLE",
		"CLAUDE_CODE_SKIP_MANTLE_AUTH",
		"CLAUDE_CODE_USE_ANTHROPIC_AWS",
		"CLAUDE_CODE_SKIP_ANTHROPIC_AWS_AUTH",
		"ANTHROPIC_BEDROCK_BASE_URL",
		"ANTHROPIC_VERTEX_BASE_URL",
		"ANTHROPIC_FOUNDRY_BASE_URL",
		"ANTHROPIC_FOUNDRY_API_KEY",
		"ANTHROPIC_FOUNDRY_AUTH_TOKEN",
		"ANTHROPIC_FOUNDRY_BEARER_TOKEN",
		"ANTHROPIC_BEDROCK_MANTLE_BASE_URL",
		"ANTHROPIC_AWS_BASE_URL",
		"ANTHROPIC_AWS_WORKSPACE_ID",
		"ANTHROPIC_AWS_API_KEY",
		"AWS_BEARER_TOKEN_BEDROCK",
	}
	envUnset = mergeEnvironmentKeys(envUnset, input.SensitiveEnv)

	settingsEnv := make(map[string]string, len(envUnset)+len(envSet)+2)
	for _, key := range envUnset {
		settingsEnv[key] = ""
	}
	// Claude settings-file env entries override the process environment. Put
	// every routing-critical override in the highest-precedence --settings
	// source so user or project settings cannot redirect the launched session.
	for key, value := range envSet {
		settingsEnv[key] = value
	}
	if input.NoProxy != "" {
		settingsEnv["NO_PROXY"] = input.NoProxy
		settingsEnv["no_proxy"] = input.NoProxy
	}
	settingsEnv["CLAUDE_CODE_DISABLE_AGENT_VIEW"] = "1"
	settingsEnv["CLAUDE_CODE_DISABLE_BACKGROUND_TASKS"] = "1"
	settingsEnv["CLAUDE_CODE_DISABLE_CRON"] = "1"
	if !input.DryRun {
		probeEnvironment := applyEnvironment(input.Environment, envUnset, envSet)
		probeEnvironment = ensureLoopbackNoProxy(probeEnvironment, baseURL)
		if err := validateClaudeVersion(executable, probeEnvironment); err != nil {
			return PreparedProcess{}, err
		}
	}
	settingsJSON, err := json.Marshal(map[string]interface{}{
		"env":                  settingsEnv,
		"disableRemoteControl": true,
	})
	if err != nil {
		return PreparedProcess{}, fmt.Errorf("build managed Claude settings: %w", err)
	}
	settingsPath, cleanup, err := writePrivateTempFile("vekil-claude-settings-", settingsJSON)
	if err != nil {
		return PreparedProcess{}, fmt.Errorf("write managed Claude settings: %w", err)
	}
	args := append([]string(nil), executable.prefixArgs...)
	args = append(args, "--settings", settingsPath)
	args = append(args, input.ForwardedArgs...)

	return PreparedProcess{
		Path:     executable.path,
		Args:     args,
		EnvSet:   envSet,
		EnvUnset: envUnset,
		Cleanup:  cleanup,
	}, nil
}

func validateClaudeVersion(executable resolvedExecutable, environment []string) error {
	return validateExecutableVersion(executable, environment, "Claude Code", "Claude Code", minimumVersion{major: 2, minor: 1, patch: 83})
}

func validateClaudeForwardedArgs(args []string) error {
	expectValue := false
	seenPositional := false
	printMode := false
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
			if arg == "--print" || arg == "-p" {
				printMode = true
			}
			switch {
			case arg == "--settings" || strings.HasPrefix(arg, "--settings="),
				arg == "--setting-sources" || strings.HasPrefix(arg, "--setting-sources="):
				return fmt.Errorf("claude settings-source overrides are not supported by the managed Vekil launcher")
			case arg == "--bg" || arg == "--background" || strings.HasPrefix(arg, "--background="),
				arg == "--tmux" || strings.HasPrefix(arg, "--tmux="),
				arg == "--remote" || strings.HasPrefix(arg, "--remote="),
				arg == "--cloud" || strings.HasPrefix(arg, "--cloud="),
				arg == "--remote-control" || strings.HasPrefix(arg, "--remote-control=") || arg == "--rc",
				arg == "--fork-session":
				return fmt.Errorf("detached Claude sessions are not supported by an ephemeral Vekil launcher")
			case arg == "--model" || strings.HasPrefix(arg, "--model="),
				arg == "--fallback-model" || strings.HasPrefix(arg, "--fallback-model="),
				arg == "--continue" || arg == "-c",
				arg == "--resume" || strings.HasPrefix(arg, "--resume=") || arg == "-r" || strings.HasPrefix(arg, "-r"),
				arg == "--from-pr" || strings.HasPrefix(arg, "--from-pr="),
				arg == "--teleport" || strings.HasPrefix(arg, "--teleport="),
				arg == "--agent" || strings.HasPrefix(arg, "--agent="),
				arg == "--agents" || strings.HasPrefix(arg, "--agents="):
				return fmt.Errorf("claude model or session overrides are not supported by the managed Vekil launcher")
			}
			if claudeOptionRequiresValue(arg) {
				expectValue = true
			}
			continue
		}
		if !seenPositional {
			if !printMode && (arg == "agents" || arg == "remote-control") {
				return fmt.Errorf("detached Claude agent-management commands are not supported by an ephemeral Vekil launcher")
			}
			seenPositional = true
		}
	}
	return nil
}

func claudeOptionRequiresValue(arg string) bool {
	if strings.Contains(arg, "=") {
		return false
	}
	switch arg {
	case "--add-dir", "--allowedTools", "--allowed-tools", "--append-system-prompt",
		"--betas", "--debug-file", "--disallowedTools", "--disallowed-tools",
		"--effort", "--file", "--input-format", "--json-schema", "--max-budget-usd", "--max-turns",
		"--mcp-config", "--output-format", "--permission-mode", "--plugin-dir",
		"--plugin-marketplace", "--plugin-url", "--prompt-suggestions", "--session-id", "--name", "-n",
		"--remote-control-session-name-prefix",
		"--system-prompt", "--tools":
		return true
	default:
		return false
	}
}

func modelSupportsEndpoint(model ModelInfo, endpoint string) bool {
	for _, candidate := range model.SupportedEndpoints {
		if strings.TrimSpace(candidate) == endpoint {
			return true
		}
	}
	return false
}
