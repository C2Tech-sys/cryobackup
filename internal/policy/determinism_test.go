package policy

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// nonTestSources returns the package's non-test .go files and their contents.
func nonTestSources(t *testing.T) map[string]string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		out[f] = string(b)
	}
	if len(out) < 5 {
		t.Fatalf("found only %d non-test source files; the glob is wrong", len(out))
	}
	return out
}

// TestNoClockReads: this package is pure, so the clock is an input. Only the
// callers (agent scheduler, console) read it.
func TestNoClockReads(t *testing.T) {
	for name, src := range nonTestSources(t) {
		for i, line := range strings.Split(src, "\n") {
			if strings.Contains(line, "time.Now(") {
				t.Errorf("%s:%d reads the clock: %s", name, i+1, strings.TrimSpace(line))
			}
		}
	}
}

// TestStandardLibraryOnlyAndNoSideEffects: no module imports, and none of the
// packages that would let this one touch the machine or write logs.
func TestStandardLibraryOnlyAndNoSideEffects(t *testing.T) {
	forbidden := map[string]bool{"os": true, "os/exec": true, "log": true, "log/slog": true, "net": true, "net/http": true, "io/ioutil": true, "syscall": true, "unsafe": true}
	fset := token.NewFileSet()
	for name, src := range nonTestSources(t) {
		f, err := parser.ParseFile(fset, name, src, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			if first, _, _ := strings.Cut(path, "/"); strings.Contains(first, ".") {
				t.Errorf("%s imports %q: standard library only", name, path)
			}
			if forbidden[path] {
				t.Errorf("%s imports %q: not allowed in this package", name, path)
			}
		}
		for i, line := range strings.Split(src, "\n") {
			if strings.Contains(line, "fmt.Print") {
				t.Errorf("%s:%d prints: %s", name, i+1, strings.TrimSpace(line))
			}
		}
		if strings.Contains(src, "time/tzdata") {
			t.Errorf("%s imports time/tzdata; that belongs in test files only", name)
		}
	}
}
