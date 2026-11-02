package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// M2-W3D: the scope matrix is generated from the source, not from a list that
// someone has to remember to extend.
//
// TestScopeMatrixCoversEveryExportedMethod (scope_test.go) already walks the
// methods of *Store by reflection. Reflection cannot see three things, and these
// tests close them with go/ast over the package source:
//
//   - a Scope that is not the parameter right after ctx;
//   - an exported function or a method of another type that takes a Scope;
//   - a matrix entry whose closure calls a different method than its name says,
//     which would test the wrong code and still pass.
//
// Adding an exported store method that takes a Scope therefore fails the build
// until it has a matrix entry, and a matrix entry that does not call its method
// fails too.

// scopeParamExempt lists the exported functions and methods outside *Store that
// take a Scope on purpose, with the reason. It is empty today: the only exported
// things that take a Scope are Store methods.
var scopeParamExempt = map[string]string{}

// parsePackage parses every .go file of the package directory.
func parsePackage(t *testing.T, includeTests bool) map[string]*ast.File {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || (!includeTests && strings.HasSuffix(name, "_test.go")) {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(".", name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files[name] = f
	}
	if len(files) == 0 {
		t.Fatal("no Go files found in the store package directory")
	}
	return files
}

// isScopeType reports whether a parameter type is Scope, *Scope, ...Scope or a
// slice or map of Scope.
func isScopeType(e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name == "Scope"
	case *ast.StarExpr:
		return isScopeType(x.X)
	case *ast.Ellipsis:
		return isScopeType(x.Elt)
	case *ast.ArrayType:
		return isScopeType(x.Elt)
	case *ast.MapType:
		return isScopeType(x.Value)
	}
	return false
}

func receiverName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return ""
	}
	e := fd.Recv.List[0].Type
	if s, ok := e.(*ast.StarExpr); ok {
		e = s.X
	}
	if id, ok := e.(*ast.Ident); ok {
		return id.Name
	}
	return "?"
}

// scopeParamIndex returns the position of the first Scope parameter among the
// declared parameters (the receiver is not counted), or -1.
func scopeParamIndex(fd *ast.FuncDecl) int {
	i := 0
	for _, f := range fd.Type.Params.List {
		n := len(f.Names)
		if n == 0 {
			n = 1
		}
		if isScopeType(f.Type) {
			return i
		}
		i += n
	}
	return -1
}

func TestEveryExportedFunctionThatTakesAScopeIsInTheMatrix(t *testing.T) {
	cases := matrix()
	var scoped []string
	for file, f := range parsePackage(t, false) {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || !fd.Name.IsExported() {
				continue
			}
			idx := scopeParamIndex(fd)
			if idx < 0 {
				continue
			}
			recv := receiverName(fd)
			label := fd.Name.Name
			if recv != "" {
				label = recv + "." + fd.Name.Name
			}
			switch {
			case recv == "Scope":
				// Methods of Scope itself have no Scope parameter; this branch cannot trigger, kept explicit.
			case recv != "Store":
				if _, ok := scopeParamExempt[label]; !ok {
					t.Errorf("%s (%s) takes a Scope but is not a *Store method: add a scope matrix entry for its callers' path, or list it in scopeParamExempt with the reason", label, file)
				}
			default:
				scoped = append(scoped, fd.Name.Name)
				if idx != 1 { // ctx is parameter 0
					t.Errorf("%s (%s): the Scope must be the parameter right after ctx (found at position %d)", label, file, idx)
				}
				if _, ok := cases[fd.Name.Name]; !ok {
					t.Errorf("%s (%s) takes a Scope but has no scope matrix entry: add one (matrix() in scope_test.go or an addXMatrix hook)", label, file)
				}
			}
		}
	}
	// The generated list and the hand-written matrix must name the same methods, in both directions.
	have := map[string]bool{}
	for _, n := range scoped {
		have[n] = true
	}
	var stale []string
	for name := range cases {
		if !have[name] {
			stale = append(stale, name)
		}
	}
	sort.Strings(stale)
	for _, n := range stale {
		t.Errorf("the scope matrix has an entry for %s, but no exported *Store method of that name takes a Scope", n)
	}
	if len(scoped) < 30 {
		t.Errorf("found only %d scoped *Store methods in the source; the scan is broken", len(scoped))
	}
}

// keyedClosures finds `"Name": {... call: func(...) {...} ...}` entries in the
// test files' matrix literals and the add*Matrix hooks, which assign
// `m["Name"] = matrixCase{...}` or put the key in a composite literal.
func matrixClosures(t *testing.T) map[string]*ast.FuncLit {
	t.Helper()
	out := map[string]*ast.FuncLit{}
	record := func(key ast.Expr, val ast.Expr) {
		bl, ok := key.(*ast.BasicLit)
		if !ok || bl.Kind != token.STRING {
			return
		}
		name := strings.Trim(bl.Value, `"`)
		var lit *ast.CompositeLit
		switch v := val.(type) {
		case *ast.CompositeLit:
			lit = v
		case *ast.UnaryExpr:
			lit, _ = v.X.(*ast.CompositeLit)
		}
		if lit == nil {
			return
		}
		for _, el := range lit.Elts {
			kv, ok := el.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "call" {
				if fl, ok := kv.Value.(*ast.FuncLit); ok {
					out[name] = fl
				}
			}
		}
	}
	for _, f := range parsePackage(t, true) {
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.KeyValueExpr:
				record(x.Key, x.Value)
			case *ast.AssignStmt:
				if len(x.Lhs) == 1 && len(x.Rhs) == 1 {
					if ix, ok := x.Lhs[0].(*ast.IndexExpr); ok {
						record(ix.Index, x.Rhs[0])
					}
				}
			}
			return true
		})
	}
	return out
}

func TestEveryMatrixEntryCallsTheMethodItIsNamedFor(t *testing.T) {
	closures := matrixClosures(t)
	cases := matrix()
	for name := range cases {
		fl, ok := closures[name]
		if !ok {
			t.Errorf("matrix entry %s: could not find its call closure in the test source (keep entries as `\"Name\": {kind: ..., call: func ...}`)", name)
			continue
		}
		called := false
		ast.Inspect(fl.Body, func(n ast.Node) bool {
			if ce, ok := n.(*ast.CallExpr); ok {
				if se, ok := ce.Fun.(*ast.SelectorExpr); ok && se.Sel.Name == name {
					called = true
				}
			}
			return !called
		})
		if !called {
			t.Errorf("matrix entry %s: its closure never calls %s, so the case would test some other method", name, name)
		}
	}
}
