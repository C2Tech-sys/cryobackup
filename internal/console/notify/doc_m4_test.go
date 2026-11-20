package notify

import (
	"go/ast"
	"go/doc"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// P20-W2BF D2: the package comment is one comment, from the synopsis to the last section, so `go
// doc` prints all of it. A line without "//" inside it would cut it in two, and only the part
// touching the package clause would remain the package comment.
func TestPackageCommentIsWhole(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "doc.go", nil, parser.ParseComments|parser.PackageClauseOnly)
	if err != nil {
		t.Fatal(err)
	}
	p, err := doc.NewFromFiles(fset, []*ast.File{f}, "github.com/c2tech-sys/cryobackup/internal/console/notify")
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Synopsis(p.Doc); !strings.HasPrefix(got, "Package notify turns alarm episodes into operator mail") {
		t.Errorf("synopsis %q", got)
	}
	for _, want := range []string{
		"# What a message says",
		"A retried alert is the original notification, delayed",
		"One residual class is accepted",
		"# Batches and crash safety",
		"# Failing loudly",
	} {
		if !strings.Contains(p.Doc, want) {
			t.Errorf("the package comment lacks %q", want)
		}
	}
}
