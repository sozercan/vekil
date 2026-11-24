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
			candidate, observesClassifiers := streamErrorObservationBranch(fset, src, stmt)
			if !candidate {
				return true
			}
			checked++
			if !observesClassifiers {
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

func TestStreamErrorBranchDetectionIncludesIfInit(t *testing.T) {
	src := []byte(`package proxy
func sample(err error) {
	if terminalErr := chatExecutionErrorFromStreamTermination(err); terminalErr != nil {
		observeOpenAIUsage(nil, nil)
	}
}`)
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "sample.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	var stmt *ast.IfStmt
	ast.Inspect(file, func(n ast.Node) bool {
		if found, ok := n.(*ast.IfStmt); ok {
			stmt = found
			return false
		}
		return true
	})
	if stmt == nil {
		t.Fatal("fixture has no if statement")
	}
	candidate, observesClassifiers := streamErrorObservationBranch(fset, src, stmt)
	if !candidate || observesClassifiers {
		t.Fatalf("branch detection = candidate %v classifiers %v, want true/false", candidate, observesClassifiers)
	}
}

func streamErrorObservationBranch(fset *token.FileSet, src []byte, stmt *ast.IfStmt) (bool, bool) {
	if stmt == nil {
		return false, false
	}
	body := nodeText(fset, src, stmt.Body)
	if !strings.Contains(body, "observe") {
		return false, false
	}
	guard := nodeText(fset, src, stmt.Init) + "\n" + nodeText(fset, src, stmt.Cond)
	if !strings.Contains(guard, "chatExecutionErrorFromStreamTermination") &&
		!strings.Contains(guard, "errors.As") {
		return false, false
	}
	return true, strings.Contains(body, "observeChatExecutionError")
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
