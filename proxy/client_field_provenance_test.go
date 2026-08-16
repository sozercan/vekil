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

// Three separate leaks of client data into error_param have come from the same
// shape: a name obtained from the CLIENT's own JSON object, handed to
// newChatInvalidRequest, which cannot tell where its param came from. Each was
// fixed by hand and the next one was written anyway, so this is the tripwire
// rather than a fourth round of care.
//
// The rule: a value derived from a client-supplied map key must go through
// newChatInvalidRequestClientField -- which keeps the full path for the client
// and records only the trusted parent.
//
// An earlier version of this test tracked only DIRECT use of the range variable
// (a bare identifier, or one inside a `+`). That missed the pattern the
// validators actually use, which launders the key through a slice:
//
//	for field := range root {            // field is the client's key
//	    unknown = append(unknown, field) // taint moves to the slice
//	}
//	return newChatInvalidRequest(unknown[0], ...) // and back out by index
//
// Reverting a call site to exactly that left the old test green. Taint is now
// propagated to a fixpoint through assignment and append, and a call is an
// offence if ANY tainted name appears anywhere in its param expression --
// index, slice, concatenation or Sprintf argument alike.
func TestClientKeysDoNotReachNewChatInvalidRequest(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
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
			fn, ok := n.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				return true
			}
			tainted := clientTaintedNames(fn)
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
				if ident := taintedIdentIn(call.Args[0], tainted); ident != "" {
					offenders = append(offenders, fset.Position(call.Pos()).String()+
						": param derives from client-supplied key via "+ident)
				}
				return true
			})
			return true
		})
	}

	// Without this the test passes trivially the day someone renames the constructor.
	if checked == 0 {
		t.Fatal("scanned no newChatInvalidRequest call in a function handling client keys; this test no longer checks anything")
	}
	for _, offender := range offenders {
		t.Errorf("%s: use newChatInvalidRequestClientField so only the trusted parent is logged", offender)
	}
}

// clientTaintedNames returns every local name in fn that can hold a client-chosen
// map key, propagated to a fixpoint.
//
// The seed is the KEY of a key-only range over anything not provably a slice.
// That is fail-closed on purpose: proving map-ness instead missed
// `object, err := decodeChatJSONObject(raw, param)`, where the map arrives from
// a call and no literal names it. The slice exemption is what keeps
// `fmt.Sprintf("messages[%d]", i)` quiet -- an integer index is vekil's own
// structure, not the client's spelling, and flagging it produced 40 false
// positives in an earlier version. Measured at zero false positives here.
func clientTaintedNames(fn *ast.FuncDecl) map[string]bool {
	slices := sliceTypedNames(fn)
	tainted := map[string]bool{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		rng, ok := n.(*ast.RangeStmt)
		if !ok {
			return true
		}
		// Key-only range. Over a map that key is the client's field name; over a
		// slice it is an integer index. FAIL CLOSED: taint unless the operand is
		// provably a slice. Requiring proof of map-ness instead is what let the
		// `object, err := decodeChatJSONObject(raw, param)` shape through -- the
		// map arrives from a call, so no literal or make() names it.
		if rng.Value != nil {
			return true
		}
		if src, ok := rng.X.(*ast.Ident); ok && slices[src.Name] {
			return true
		}
		if _, ok := rng.X.(*ast.CompositeLit); ok {
			return true
		}
		if key, ok := rng.Key.(*ast.Ident); ok && key.Name != "_" {
			tainted[key.Name] = true
		}
		return true
	})
	if len(tainted) == 0 {
		return nil
	}
	// Fixpoint: any assignment whose right-hand side mentions a tainted name
	// taints its left-hand side. That covers `unknown = append(unknown, field)`,
	// `field := unknown[0]`, and any chain built from those.
	for changed := true; changed; {
		changed = false
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			assign, ok := n.(*ast.AssignStmt)
			if !ok {
				return true
			}
			carries := false
			for _, rhs := range assign.Rhs {
				if taintedIdentIn(rhs, tainted) != "" {
					carries = true
					break
				}
			}
			if !carries {
				return true
			}
			for _, lhs := range assign.Lhs {
				if ident, ok := lhs.(*ast.Ident); ok && ident.Name != "_" && !tainted[ident.Name] {
					tainted[ident.Name] = true
					changed = true
				}
			}
			return true
		})
	}
	return tainted
}

// sliceTypedNames returns the names in fn provably holding a slice or array:
// variadic and slice parameters, `make([]T, ...)`, slice composite literals, and
// `var x []T`. Anything not on this list is treated as possibly a map, because a
// missed map key is a leak while a spurious integer index is only noise a
// reviewer resolves once.
func sliceTypedNames(fn *ast.FuncDecl) map[string]bool {
	names := map[string]bool{}
	markType := func(idents []*ast.Ident, typ ast.Expr) {
		switch typ.(type) {
		case *ast.ArrayType, *ast.Ellipsis:
		default:
			return
		}
		for _, ident := range idents {
			names[ident.Name] = true
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
				if ident, ok := stmt.Lhs[i].(*ast.Ident); ok {
					names[ident.Name] = true
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

// taintedIdentIn reports the first tainted name appearing anywhere in expr, or "".
// Whole-expression search is the point: `unknown[0]`, `unknown[i]`, `p + unknown[0]`
// and `fmt.Sprintf("%s", unknown[0])` are the same leak wearing different syntax.
func taintedIdentIn(expr ast.Expr, tainted map[string]bool) string {
	found := ""
	ast.Inspect(expr, func(n ast.Node) bool {
		if found != "" {
			return false
		}
		if ident, ok := n.(*ast.Ident); ok && tainted[ident.Name] {
			found = ident.Name
			return false
		}
		return true
	})
	return found
}
