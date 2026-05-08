package proxy

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestProxyLogsDoNotIncludeRequestBodies(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read proxy package directory: %v", err)
	}

	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}

		path := filepath.Join(".", name)
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}

		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || !isLoggerFieldCall(call) || len(call.Args) == 0 {
				return true
			}

			key, ok := stringLiteralValue(call.Args[0])
			if !ok {
				return true
			}

			switch key {
			case "prompt", "request", "request_body":
				t.Errorf("%s logs sensitive field %q; log safe metadata such as byte counts instead", fset.Position(call.Pos()), key)
			case "body":
				if len(call.Args) > 1 && stringifiesRequestBodyVariable(call.Args[1]) {
					t.Errorf("%s logs request body bytes under field %q; log safe metadata such as byte counts instead", fset.Position(call.Pos()), key)
				}
			}
			return true
		})
	}
}

func isLoggerFieldCall(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "F" {
		return false
	}
	ident, ok := sel.X.(*ast.Ident)
	return ok && ident.Name == "logger"
}

func stringLiteralValue(expr ast.Expr) (string, bool) {
	lit, ok := expr.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	value, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return value, true
}

func stringifiesRequestBodyVariable(expr ast.Expr) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok || len(call.Args) != 1 {
		return false
	}
	fn, ok := call.Fun.(*ast.Ident)
	if !ok || fn.Name != "string" {
		return false
	}
	ident, ok := call.Args[0].(*ast.Ident)
	if !ok {
		return false
	}
	switch ident.Name {
	case "body", "oaiBody", "reqBody", "requestBody", "rawBody":
		return true
	default:
		return false
	}
}
