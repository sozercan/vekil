package proxy

import "testing"

func TestResolveFilterHintRequiresCommandTokenBoundaries(t *testing.T) {
	tests := []struct {
		command string
		want    string
	}{
		{command: "go test", want: "go-test"},
		{command: "go test ./...", want: "go-test"},
		{command: "go test; echo done", want: "go-test"},
		{command: "go testify ./...", want: ""},
		{command: "git diff --stat", want: "git-diff"},
		{command: "git diff|cat", want: "git-diff"},
		{command: "git differential", want: ""},
		{command: "git status --short", want: "git-status"},
		{command: "git statusline", want: ""},
		{command: "pytest -q", want: "pytest"},
		{command: "pytester", want: ""},
		{command: "cargo test --workspace", want: "cargo-test"},
		{command: "cargo testing", want: ""},
		{command: "vitest run", want: "vitest"},
		{command: "vitestify", want: ""},
		{command: "npx vitest run", want: "vitest"},
		{command: "npx vitestify", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.command, func(t *testing.T) {
			if got := ResolveFilterHint(tt.command); got != tt.want {
				t.Fatalf("ResolveFilterHint(%q) = %q, want %q", tt.command, got, tt.want)
			}
		})
	}
}
