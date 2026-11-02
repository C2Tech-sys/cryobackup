package adminpath

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// TestDocMentionsEverything keeps doc.go in step with the code: every method,
// every error code with its status, every default timeout.
func TestDocMentionsEverything(t *testing.T) {
	raw, err := os.ReadFile("doc.go")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	for _, m := range Methods() {
		if !strings.Contains(doc, "//\t"+m+" ") {
			t.Errorf("doc.go has no table row for method %s", m)
		}
	}
	for _, code := range Codes() {
		row := false
		for _, line := range strings.Split(doc, "\n") {
			f := strings.Fields(strings.TrimPrefix(line, "//"))
			if len(f) >= 2 && f[0] == code && f[1] == strconv.Itoa(HTTPStatus(code)) {
				row = true
			}
		}
		if !row {
			t.Errorf("doc.go has no row for %s with status %d", code, HTTPStatus(code))
		}
	}
	for m, d := range DefaultTimeouts() {
		found := false
		for _, line := range strings.Split(doc, "\n") {
			f := strings.Fields(strings.TrimPrefix(line, "//"))
			if len(f) >= 2 && f[0] == m && f[1] == shortDuration(d.String()) {
				found = true
			}
		}
		if !found {
			t.Errorf("doc.go does not give the %v timeout of %s", d, m)
		}
	}
	for _, name := range []string{"DH1", "DH17", "DH22", "DH27", "PATH-BEARING", "go test -count=1"} {
		if !strings.Contains(doc, name) {
			t.Errorf("doc.go does not mention %s", name)
		}
	}
}

// shortDuration renders 2m0s as 120s, 1h0m0s as 1h, 10m0s as 10m, 5s as 5s, the
// way the table in doc.go does.
func shortDuration(s string) string {
	switch s {
	case "1m0s":
		return "60s"
	case "2m0s":
		return "120s"
	case "1h0m0s":
		return "1h"
	case "10m0s":
		return "10m"
	case "6h0m0s":
		return "6h"
	case "12h0m0s":
		return "12h"
	}
	return s
}

// TestEveryExportedIdentifierIsDocumented parses the package's own source.
func TestEveryExportedIdentifierIsDocumented(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool { return !strings.HasSuffix(fi.Name(), "_test.go") }, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	for _, pkg := range pkgs {
		for fname, f := range pkg.Files {
			for _, decl := range f.Decls {
				switch d := decl.(type) {
				case *ast.FuncDecl:
					if d.Name.IsExported() && d.Doc == nil && exportedReceiver(d) {
						t.Errorf("%s: func %s has no doc comment", fname, d.Name.Name)
					}
				case *ast.GenDecl:
					for _, spec := range d.Specs {
						switch s := spec.(type) {
						case *ast.TypeSpec:
							if s.Name.IsExported() && d.Doc == nil && s.Doc == nil {
								t.Errorf("%s: type %s has no doc comment", fname, s.Name.Name)
							}
						case *ast.ValueSpec:
							for _, n := range s.Names {
								if n.IsExported() && d.Doc == nil && s.Doc == nil && s.Comment == nil {
									t.Errorf("%s: %s has no doc comment", fname, n.Name)
								}
							}
						}
					}
				}
			}
		}
	}
}

func exportedReceiver(d *ast.FuncDecl) bool {
	if d.Recv == nil || len(d.Recv.List) == 0 {
		return true
	}
	t := d.Recv.List[0].Type
	if star, ok := t.(*ast.StarExpr); ok {
		t = star.X
	}
	id, ok := t.(*ast.Ident)
	return ok && id.IsExported()
}

// TestImportsAndConventions: the package uses the standard library and
// internal/restic only, writes no log line and reads no clock.
func TestImportsAndConventions(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, parser.ImportsOnly|parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			first, _, _ := strings.Cut(path, "/")
			std := !strings.Contains(first, ".")
			if !std && path != "github.com/c2tech-sys/cryobackup/internal/restic" {
				t.Errorf("%s imports %s: only the standard library and internal/restic are allowed", name, path)
			}
			if path == "log" || path == "log/slog" {
				t.Errorf("%s imports %s: this package does not log", name, path)
			}
		}
		code := string(src)
		for _, bad := range []string{"time.Now(", "fmt.Print", "os.Stdout", "os.Stderr"} {
			if strings.Contains(code, bad) {
				t.Errorf("%s uses %s", name, bad)
			}
		}
	}
}
