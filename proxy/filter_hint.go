package proxy

import "strings"

func ResolveFilterHint(command string) string {
	cmd := strings.TrimSpace(command)
	cmdLower := strings.ToLower(cmd)
	switch {
	case strings.HasPrefix(cmdLower, "git diff"):
		return "git-diff"
	case strings.HasPrefix(cmdLower, "git status"):
		return "git-status"
	case strings.HasPrefix(cmdLower, "pytest"):
		return "pytest"
	case strings.HasPrefix(cmdLower, "cargo test"):
		return "cargo-test"
	case strings.HasPrefix(cmdLower, "go test"):
		return "go-test"
	case strings.HasPrefix(cmdLower, "vitest"), strings.HasPrefix(cmdLower, "npx vitest"):
		return "vitest"
	default:
		return ""
	}
}
