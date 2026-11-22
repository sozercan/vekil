package launch

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

var dottedClaudeModelIDRegex = regexp.MustCompile(`^(claude(?:-[^-]+)*)-(\d+)\.(\d+)$`)

const ClaudeAutoModeClassifierModel = "claude-sonnet-5"

type ClaudeAdapter struct{}

func (ClaudeAdapter) Name() string { return "claude" }

func claudeCodeModelID(input PrepareInput) string {
	id := strings.TrimSpace(input.Model.ID)
	if strings.TrimSpace(input.Model.OwnedBy) == PolicyModelOwner || input.RoutesLikeModel == nil {
		return id
	}
	if alias := dottedClaudeModelIDRegex.ReplaceAllString(id, "${1}-${2}-${3}"); alias != id && input.RoutesLikeModel(alias) {
		return alias
	}
	return id
}

func (ClaudeAdapter) Prepare(input PrepareInput) (PreparedProcess, error) {
	model := strings.TrimSpace(input.Model.ID)
	hasModel := model != ""
	endpointMetadataKnown := !input.DryRun || input.Model.SupportedEndpoints != nil
	if hasModel && endpointMetadataKnown &&
		!modelSupportsEndpoint(input.Model, "/v1/messages") &&
		!modelSupportsEndpoint(input.Model, "/chat/completions") &&
		!modelSupportsEndpoint(input.Model, "/responses") {
		return PreparedProcess{}, fmt.Errorf(
			"model %q is not Claude-compatible: expected /v1/messages, /chat/completions, or /responses support",
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
	if err := validateClaudeForwardedArgs(input.ForwardedArgs); err != nil {
		return PreparedProcess{}, err
	}

	subprocessScrub := "1"
	if claudePermissionModeRequested(input.ForwardedArgs) {
		subprocessScrub = "0"
	}
	envSet := map[string]string{
		"ANTHROPIC_BASE_URL":                   baseURL,
		"ANTHROPIC_AUTH_TOKEN":                 localToken,
		"ANTHROPIC_API_KEY":                    "",
		"CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST": "vekil",
		"CLAUDE_CODE_SUBPROCESS_ENV_SCRUB":     subprocessScrub,
	}
	if hasModel {
		name := strings.TrimSpace(input.Model.Name)
		if name == "" {
			name = model
		}
		clientModel := claudeCodeModelID(input)
		for key, value := range map[string]string{
			"ANTHROPIC_MODEL":                           clientModel,
			"ANTHROPIC_DEFAULT_HAIKU_MODEL":             clientModel,
			"ANTHROPIC_DEFAULT_SONNET_MODEL":            clientModel,
			"ANTHROPIC_DEFAULT_OPUS_MODEL":              clientModel,
			"ANTHROPIC_DEFAULT_FABLE_MODEL":             clientModel,
			"ANTHROPIC_CUSTOM_MODEL_OPTION":             clientModel,
			"ANTHROPIC_CUSTOM_MODEL_OPTION_NAME":        name,
			"ANTHROPIC_CUSTOM_MODEL_OPTION_DESCRIPTION": "Routed through Vekil",
			"CLAUDE_CODE_SUBAGENT_MODEL":                clientModel,
			"CLAUDE_CODE_AUTO_MODE_MODEL":               ClaudeAutoModeClassifierModel,
		} {
			envSet[key] = value
		}
	}
	envUnset := []string{
		"ANTHROPIC_CUSTOM_HEADERS",
		"CLAUDE_CODE_OAUTH_TOKEN",
		"CLAUDE_CODE_OAUTH_REFRESH_TOKEN",
		"CLAUDE_CODE_OAUTH_SCOPES",
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
	if hasModel {
		envUnset = append(envUnset,
			"ANTHROPIC_SMALL_FAST_MODEL",
			"ANTHROPIC_SMALL_FAST_MODEL_AWS_REGION",
		)
	}
	envUnset = mergeEnvironmentKeys(commonAgentCredentialEnvironment, envUnset, input.SensitiveEnv)

	settingsEnv := make(map[string]string, len(envUnset)+len(envSet)+2)
	for _, key := range envUnset {
		settingsEnv[key] = ""
	}
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
	settings := map[string]interface{}{
		"env":                  settingsEnv,
		"disableRemoteControl": true,
	}
	nativeWebSearch := input.CopilotAbsent && (!hasModel || modelSupportsEndpoint(input.Model, "/v1/messages"))
	if !nativeWebSearch {
		settings["permissions"] = map[string][]string{"deny": {"WebSearch"}}
	}
	settingsJSON, err := json.Marshal(settings)
	if err != nil {
		return PreparedProcess{}, fmt.Errorf("build managed Claude settings: %w", err)
	}
	settingsPath, cleanup, err := writePrivateTempFile("vekil-claude-settings-", settingsJSON)
	if err != nil {
		return PreparedProcess{}, fmt.Errorf("write managed Claude settings: %w", err)
	}
	args := append([]string(nil), executable.prefixArgs...)
	args = append(args, "--settings", settingsPath)
	if webSearchURL := strings.TrimSpace(input.WebSearchMCPURL); webSearchURL != "" {
		mcpPath, mcpCleanup, err := writeClaudeWebSearchMCPConfig(webSearchURL, localToken)
		if err != nil {
			_ = cleanup()
			return PreparedProcess{}, err
		}
		settingsCleanup := cleanup
		cleanup = func() error { return errors.Join(mcpCleanup(), settingsCleanup()) }
		args = append(args, "--mcp-config="+mcpPath)
	}
	args = append(args, input.ForwardedArgs...)

	prepared := PreparedProcess{
		Path:     executable.path,
		Args:     args,
		EnvSet:   envSet,
		EnvUnset: envUnset,
		Cleanup:  cleanup,
	}
	if hasModel && !endpointMetadataKnown {
		prepared.Unresolved = append(prepared.Unresolved,
			"model endpoint compatibility (/v1/messages, /chat/completions, or /responses) requires live /v1/models metadata",
		)
	}
	return prepared, nil
}

func (ClaudeAdapter) usesWebSearchMCP() bool { return true }

func (ClaudeAdapter) auxiliaryModels() []string { return []string{ClaudeAutoModeClassifierModel} }

func writeClaudeWebSearchMCPConfig(url, localToken string) (string, func() error, error) {
	config, err := json.Marshal(map[string]any{"mcpServers": map[string]any{
		"vekil": map[string]any{
			"type":    "http",
			"url":     url,
			"headers": map[string]string{"Authorization": "Bearer " + localToken},
		},
	}})
	if err != nil {
		return "", nil, fmt.Errorf("build Claude MCP config: %w", err)
	}
	path, cleanup, err := writePrivateTempFile("vekil-claude-mcp-", config)
	if err != nil {
		return "", nil, fmt.Errorf("write Claude MCP config: %w", err)
	}
	return path, cleanup, nil
}

func validateClaudeVersion(executable resolvedExecutable, environment []string) error {
	return validateExecutableVersion(executable, environment, "Claude Code", "Claude Code", minimumVersion{major: 2, minor: 1, patch: 83})
}

func validateClaudeForwardedArgs(args []string) error {
	expectValue := false
	seenPositional := false
	printMode := false
	foregroundSession := false
	endOptions := false
	for index, arg := range args {
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
			case arg == "--init-only" || strings.HasPrefix(arg, "--init-only="):
				return fmt.Errorf("claude initialization-only mode is not supported by an ephemeral Vekil launcher")
			case arg == "--settings" || strings.HasPrefix(arg, "--settings="),
				arg == "--setting-sources" || strings.HasPrefix(arg, "--setting-sources="),
				arg == "--managed-settings" || strings.HasPrefix(arg, "--managed-settings="):
				return fmt.Errorf("claude settings-source overrides are not supported by the managed Vekil launcher")
			case arg == "--bg" || arg == "--background" || strings.HasPrefix(arg, "--background="),
				arg == "--tmux" || strings.HasPrefix(arg, "--tmux="),
				arg == "--remote" || strings.HasPrefix(arg, "--remote="),
				arg == "--cloud" || strings.HasPrefix(arg, "--cloud="),
				arg == "--environment" || strings.HasPrefix(arg, "--environment="),
				arg == "--teleport" || strings.HasPrefix(arg, "--teleport="),
				arg == "--remote-control" || strings.HasPrefix(arg, "--remote-control=") || arg == "--rc":
				return fmt.Errorf("detached Claude sessions are not supported by an ephemeral Vekil launcher")
			case arg == "--model" || strings.HasPrefix(arg, "--model="),
				arg == "--fallback-model" || strings.HasPrefix(arg, "--fallback-model="),
				arg == "--agent" || strings.HasPrefix(arg, "--agent="),
				arg == "--agents" || strings.HasPrefix(arg, "--agents="):
				return fmt.Errorf("claude model or custom-agent overrides are not supported by the managed Vekil launcher")
			case claudeForegroundSessionSelector(arg):
				foregroundSession = true
				if claudeSessionSelectorHasOptionalValue(arg) && index+1 < len(args) && !strings.HasPrefix(args[index+1], "-") {
					expectValue = true
				}
			}
			if claudeOptionRequiresValue(arg) && index+1 < len(args) && !strings.HasPrefix(args[index+1], "-") {
				expectValue = true
			}
			continue
		}
		if !seenPositional {
			if !printMode && !foregroundSession && claudeNonAgentCommand(arg) {
				return fmt.Errorf("detached Claude agent-management commands are not supported by an ephemeral Vekil launcher")
			}
			seenPositional = true
		}
	}
	return nil
}

func claudePermissionModeRequested(args []string) bool {
	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch {
		case arg == "--":
			return false
		case arg == "--dangerously-skip-permissions", arg == "--allow-dangerously-skip-permissions":
			return true
		case strings.HasPrefix(arg, "--permission-mode="):
			if claudeNonDefaultPermissionMode(strings.TrimPrefix(arg, "--permission-mode=")) {
				return true
			}
		case claudeOptionRequiresValue(arg) && index+1 < len(args):
			index++
			if arg == "--permission-mode" && claudeNonDefaultPermissionMode(args[index]) {
				return true
			}
		}
	}
	return false
}

func claudeNonDefaultPermissionMode(mode string) bool {
	mode = strings.TrimSpace(mode)
	return mode != "manual" && mode != "default"
}

func claudeNonAgentCommand(command string) bool {
	switch command {
	case "agents", "attach", "auth", "auto-mode", "doctor", "gateway", "install", "mcp",
		"import", "kill", "logs", "plugin", "plugins", "project", "remote-control", "respawn", "rm",
		"setup-token", "stop", "ultrareview", "update", "upgrade":
		return true
	default:
		return false
	}
}

func claudeForegroundSessionSelector(arg string) bool {
	switch {
	case arg == "--continue" || arg == "-c",
		arg == "--resume" || strings.HasPrefix(arg, "--resume=") || arg == "-r" || strings.HasPrefix(arg, "-r"),
		arg == "--session-id" || strings.HasPrefix(arg, "--session-id="),
		arg == "--from-pr" || strings.HasPrefix(arg, "--from-pr="):
		return true
	default:
		return false
	}
}

func claudeSessionSelectorHasOptionalValue(arg string) bool {
	switch arg {
	case "--resume", "-r", "--from-pr":
		return true
	default:
		return false
	}
}

func claudeOptionRequiresValue(arg string) bool {
	if strings.Contains(arg, "=") {
		return false
	}
	switch arg {
	case "--add-dir", "--allowedTools", "--allowed-tools", "--append-system-prompt", "--append-system-prompt-file",
		"--betas", "--debug-file", "--disallowedTools", "--disallowed-tools",
		"--effort", "--file", "--input-format", "--json-schema", "--max-budget-usd", "--max-turns",
		"--mcp-config", "--output-format", "--permission-mode", "--plugin-dir",
		"--permission-prompt-tool", "--plugin-marketplace", "--plugin-url", "--prompt-suggestions", "--session-id", "--name", "-n",
		"--remote-control-session-name-prefix",
		"--system-prompt", "--system-prompt-file", "--tools":
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
