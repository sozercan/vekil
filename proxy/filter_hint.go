package proxy

import "strings"

func ResolveFilterHint(command string) string {
	cmdLower := strings.ToLower(strings.TrimSpace(command))
	switch {
	case hasShellCommandPrefix(cmdLower, "git diff"):
		return "git-diff"
	case hasShellCommandPrefix(cmdLower, "git status"):
		return "git-status"
	case hasShellCommandPrefix(cmdLower, "pytest"):
		return "pytest"
	case hasShellCommandPrefix(cmdLower, "cargo test"):
		return "cargo-test"
	case hasShellCommandPrefix(cmdLower, "go test"):
		return "go-test"
	case hasShellCommandPrefix(cmdLower, "vitest"), hasShellCommandPrefix(cmdLower, "npx vitest"):
		return "vitest"
	default:
		return ""
	}
}

func hasShellCommandPrefix(command, prefix string) bool {
	if command == prefix {
		return true
	}
	if !strings.HasPrefix(command, prefix) || len(command) <= len(prefix) {
		return false
	}
	switch command[len(prefix)] {
	case ' ', '\t', '\n', '\r', ';', '|', '&', '(', ')', '<', '>':
		return true
	default:
		return false
	}
}
