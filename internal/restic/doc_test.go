package restic

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// TestDocListsTheClassifierTable keeps doc.go and the table in code in step:
// every class and every keyword of every row appears in the documentation.
func TestDocListsTheClassifierTable(t *testing.T) {
	raw, err := os.ReadFile("doc.go")
	if err != nil {
		t.Fatal(err)
	}
	doc := strings.ToLower(string(raw))
	for i, r := range Rules() {
		if !strings.Contains(doc, string(r.Class)) {
			t.Errorf("rule %d: doc.go does not mention class %s", i+1, r.Class)
		}
		for _, kw := range append(append(append([]string(nil), r.Any...), r.Words...), r.All...) {
			if !strings.Contains(doc, `"`+strings.ToLower(kw)+`"`) {
				t.Errorf("rule %d (%s): doc.go does not list %q", i+1, r.Class, kw)
			}
		}
	}
	for _, f := range ForbiddenFlags() {
		if !strings.Contains(doc, f) {
			t.Errorf("doc.go does not mention the forbidden flag %s", f)
		}
	}
	for _, name := range []string{"LsSnapshot", "Node", "Snapshot", "FindResult", "FindMatch", "ForgetGroup", "ForgetReason"} {
		if !strings.Contains(string(raw), name) {
			t.Errorf("doc.go does not list the path-bearing type %s", name)
		}
	}
}

// TestEveryExportedIdentifierIsDocumented parses the package sources and fails
// on an exported identifier without a doc comment.
func TestEveryExportedIdentifierIsDocumented(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool { return !strings.HasSuffix(fi.Name(), "_test.go") }, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	pkg := pkgs["restic"]
	if pkg == nil {
		t.Fatal("package restic not found")
	}
	missing := func(name, kind string, pos token.Pos) {
		t.Errorf("%s %s (%s) has no doc comment", kind, name, fset.Position(pos))
	}
	for _, f := range pkg.Files {
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if !d.Name.IsExported() {
					continue
				}
				if d.Recv != nil && len(d.Recv.List) > 0 {
					// methods on unexported types need no doc
					if id := recvName(d.Recv.List[0].Type); id != "" && !ast.IsExported(id) {
						continue
					}
				}
				if d.Doc == nil {
					missing(d.Name.Name, "func", d.Pos())
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.TypeSpec:
						if s.Name.IsExported() && d.Doc == nil && s.Doc == nil {
							missing(s.Name.Name, "type", s.Pos())
						}
					case *ast.ValueSpec:
						for _, n := range s.Names {
							// a documented group (or trailing comment) covers its members
							if n.IsExported() && d.Doc == nil && s.Doc == nil && s.Comment == nil {
								missing(n.Name, "value", n.Pos())
							}
						}
					}
				}
			}
		}
	}
}

func recvName(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.StarExpr:
		return recvName(v.X)
	}
	return ""
}
