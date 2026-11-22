package proxy

import (
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClientKeysDoNotReachNewChatInvalidRequest(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	checked := 0
	var offenders []string
	parsedFiles := make([]*ast.File, 0, len(files))

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
		parsedFiles = append(parsedFiles, file)
	}
	bindings := typeInfoForFiles(fset, parsedFiles)

	for _, file := range parsedFiles {
		ast.Inspect(file, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				return true
			}
			tainted := clientTaintedNames(fn, bindings)
			if len(tainted) == 0 {
				return true
			}
			ast.Inspect(fn.Body, func(inner ast.Node) bool {
				call, ok := inner.(*ast.CallExpr)
				if !ok || len(call.Args) == 0 {
					return true
				}
				callee, ok := call.Fun.(*ast.Ident)
				if !ok || callee.Name != "newChatInvalidRequest" {
					return true
				}
				checked++
				if ident := taintedIdentIn(call.Args[0], tainted, bindings); ident != nil {
					offenders = append(offenders, fset.Position(call.Pos()).String()+
						": param derives from client-supplied key via "+ident.Name)
				}
				return true
			})
			return true
		})
	}

	if checked == 0 {
		t.Fatal("scanned no newChatInvalidRequest call in a function handling client keys; this test no longer checks anything")
	}
	for _, offender := range offenders {
		t.Errorf("%s: use newChatInvalidRequestClientField so only the trusted parent is logged", offender)
	}
}

func clientTaintedNames(fn *ast.FuncDecl, bindings *types.Info) map[types.Object]bool {
	slices := sliceTypedNames(fn, bindings)
	tainted := map[types.Object]bool{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		rng, ok := n.(*ast.RangeStmt)
		if !ok {
			return true
		}
		if src, ok := rng.X.(*ast.Ident); ok && slices[bindings.ObjectOf(src)] {
			return true
		}
		if _, ok := rng.X.(*ast.CompositeLit); ok {
			return true
		}
		if key, ok := rng.Key.(*ast.Ident); ok && key.Name != "_" && bindings.ObjectOf(key) != nil {
			tainted[bindings.ObjectOf(key)] = true
		}
		return true
	})
	if len(tainted) == 0 {
		return nil
	}
	for changed := true; changed; {
		changed = false
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			assign, ok := n.(*ast.AssignStmt)
			if !ok {
				return true
			}
			carries := false
			for _, rhs := range assign.Rhs {
				if taintedIdentIn(rhs, tainted, bindings) != nil {
					carries = true
					break
				}
			}
			if !carries {
				return true
			}
			for _, lhs := range assign.Lhs {
				if ident, ok := lhs.(*ast.Ident); ok && ident.Name != "_" && bindings.ObjectOf(ident) != nil && !tainted[bindings.ObjectOf(ident)] {
					tainted[bindings.ObjectOf(ident)] = true
					changed = true
				}
			}
			return true
		})
	}
	return tainted
}

func typeInfoForFiles(fset *token.FileSet, files []*ast.File) *types.Info {
	info := &types.Info{
		Defs: make(map[*ast.Ident]types.Object),
		Uses: make(map[*ast.Ident]types.Object),
	}
	config := types.Config{
		Importer: importer.Default(),
		Error:    func(error) {},
	}
	_, _ = config.Check("github.com/sozercan/vekil/proxy", fset, files, info)
	return info
}

func TestClientTaintedNamesClassifiesTwoVariableRanges(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want bool
	}{
		{
			name: "map key is client-controlled",
			src:  `package p; func inspect(object map[string]string) { for key, value := range object { _, _ = key, value } }`,
			want: true,
		},
		{
			name: "slice index is structural",
			src:  `package p; func inspect(values []string) { for key, value := range values { _, _ = key, value } }`,
			want: false,
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "range.go", testCase.src, 0)
			if err != nil {
				t.Fatal(err)
			}
			bindings := typeInfoForFiles(fset, []*ast.File{file})
			fn, ok := file.Decls[0].(*ast.FuncDecl)
			if !ok {
				t.Fatalf("first declaration = %T, want *ast.FuncDecl", file.Decls[0])
			}
			var key *ast.Ident
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				if rng, ok := node.(*ast.RangeStmt); ok {
					key, _ = rng.Key.(*ast.Ident)
					return false
				}
				return true
			})
			if key == nil || bindings.ObjectOf(key) == nil {
				t.Fatal("range key binding not found")
			}
			if got := clientTaintedNames(fn, bindings)[bindings.ObjectOf(key)]; got != testCase.want {
				t.Fatalf("key tainted = %v, want %v", got, testCase.want)
			}
		})
	}
}

func sliceTypedNames(fn *ast.FuncDecl, bindings *types.Info) map[types.Object]bool {
	names := map[types.Object]bool{}
	markType := func(idents []*ast.Ident, typ ast.Expr) {
		switch typ.(type) {
		case *ast.ArrayType, *ast.Ellipsis:
		default:
			return
		}
		for _, ident := range idents {
			if object := bindings.ObjectOf(ident); object != nil {
				names[object] = true
			}
		}
	}
	if fn.Type.Params != nil {
		for _, field := range fn.Type.Params.List {
			markType(field.Names, field.Type)
		}
	}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch stmt := n.(type) {
		case *ast.DeclStmt:
			decl, ok := stmt.Decl.(*ast.GenDecl)
			if !ok {
				return true
			}
			for _, spec := range decl.Specs {
				if value, ok := spec.(*ast.ValueSpec); ok && value.Type != nil {
					markType(value.Names, value.Type)
				}
			}
		case *ast.AssignStmt:
			for i, rhs := range stmt.Rhs {
				if !isSliceValued(rhs) || i >= len(stmt.Lhs) {
					continue
				}
				if ident, ok := stmt.Lhs[i].(*ast.Ident); ok && bindings.ObjectOf(ident) != nil {
					names[bindings.ObjectOf(ident)] = true
				}
			}
		}
		return true
	})
	return names
}

func isSliceValued(expr ast.Expr) bool {
	switch e := expr.(type) {
	case *ast.CompositeLit:
		_, ok := e.Type.(*ast.ArrayType)
		return ok
	case *ast.CallExpr:
		ident, ok := e.Fun.(*ast.Ident)
		if !ok || ident.Name != "make" || len(e.Args) == 0 {
			return false
		}
		_, ok = e.Args[0].(*ast.ArrayType)
		return ok
	}
	return false
}

func taintedIdentIn(expr ast.Expr, tainted map[types.Object]bool, bindings *types.Info) *ast.Ident {
	var found *ast.Ident
	ast.Inspect(expr, func(n ast.Node) bool {
		if found != nil {
			return false
		}
		if ident, ok := n.(*ast.Ident); ok && tainted[bindings.ObjectOf(ident)] {
			found = ident
			return false
		}
		return true
	})
	return found
}
