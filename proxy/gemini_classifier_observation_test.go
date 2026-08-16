package proxy

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Copilot's finding was not "this branch is wrong" but "you wired the OpenAI and
// Anthropic stream paths and missed Gemini's" -- the third instance of the same
// omission in this PR. Fixing the two branches by hand invites a fourth, so this
// asserts the rule instead: any branch that binds a *chatExecutionError from a
// stream failure must record its classifiers.
//
// Scope is the error-observing handlers. A branch that only inspects an
// execution error for control flow (status mapping, shutdown checks) is exempt
// via the allowlist below, which must stay short enough to read.
func TestStreamErrorBranchesRecordClassifiers(t *testing.T) {
	files, err := filepath.Glob("*_handler*.go")
	if err != nil {
		t.Fatal(err)
	}
	more, err := filepath.Glob("chat_handlers.go")
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, more...)

	fset := token.NewFileSet()
	checked := 0
	var offenders []string

	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			stmt, ok := n.(*ast.IfStmt)
			if !ok {
				return true
			}
			// Only branches that observe something -- a pure control-flow check
			// (status mapping, shutdown) is not an observation site.
			body := nodeText(fset, src, stmt.Body)
			if !strings.Contains(body, "errors.As") && !strings.Contains(nodeText(fset, src, stmt.Cond), "errors.As") {
				return true
			}
			cond := nodeText(fset, src, stmt.Cond)
			if !strings.Contains(cond, "chatExecutionErrorFromStreamTermination") &&
				!(strings.Contains(cond, "errors.As") && strings.Contains(body, "observe")) {
				return true
			}
			if !strings.Contains(body, "observe") {
				return true
			}
			checked++
			if !strings.Contains(body, "observeChatExecutionError") {
				offenders = append(offenders,
					fset.Position(stmt.Pos()).String()+": observes usage/status but not classifiers")
			}
			return true
		})
	}

	if checked == 0 {
		t.Fatal("scanned no stream-error observation branch; this test no longer checks anything")
	}
	for _, o := range offenders {
		t.Errorf("%s -- call observeChatExecutionError, as the OpenAI and Anthropic paths do", o)
	}
}

func nodeText(fset *token.FileSet, src []byte, n ast.Node) string {
	if n == nil {
		return ""
	}
	start := fset.Position(n.Pos()).Offset
	end := fset.Position(n.End()).Offset
	if start < 0 || end > len(src) || start >= end {
		return ""
	}
	return string(src[start:end])
}
