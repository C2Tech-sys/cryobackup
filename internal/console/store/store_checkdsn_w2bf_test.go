package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// P20-W2BF C2: the check store opens exactly the file Open opens for the same path. A "%41" in the
// path is not "A", a space is a space; "?" and "#" are refused by both, as Open always has.
func TestCheckStoreOpensTheSameFileAsOpen(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	for _, name := range []string{"pct%41.db", "with space.db", "pct%2525 and space%3F.db"} {
		path := filepath.Join(dir, name)
		st, err := Open(ctx, Options{Path: path})
		if err != nil {
			t.Fatalf("%s: Open: %v", name, err)
		}
		want, err := st.SchemaVersion(ctx)
		if err != nil {
			t.Fatal(err)
		}
		_ = st.Close()
		chk, pending, err := OpenNoMigrate(ctx, Options{Path: path})
		if err != nil {
			t.Fatalf("%s: OpenNoMigrate: %v", name, err)
		}
		got, err := chk.SchemaVersion(ctx)
		_ = chk.Close()
		if err != nil || got != want || len(pending) != 0 {
			t.Fatalf("%s: check store reads schema %d (%v) pending %v, Open %d", name, got, err, pending, want)
		}
		if _, err := os.Stat(filepath.Join(dir, "pctA.db")); err == nil {
			t.Fatalf("%s: a decoded name was opened", name)
		}
	}
	for _, name := range []string{"hash#mark.db", "what?.db"} {
		path := filepath.Join(dir, name)
		if _, err := Open(ctx, Options{Path: path}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s: Open: %v", name, err)
		}
		if _, _, err := OpenNoMigrate(ctx, Options{Path: path}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s: OpenNoMigrate: %v", name, err)
		}
	}
}

// checkURI: drive letters, UNC paths, relative and POSIX paths, and the characters a URI path
// must escape.
func TestCheckURIPathForms(t *testing.T) {
	for in, want := range map[string]string{
		`/var/lib/cryo/console.db`:    "file:///var/lib/cryo/console.db",
		`data/console.db`:             "file:data/console.db",
		`/srv/pct%41 x/console.db`:    "file:///srv/pct%2541%20x/console.db",
		`/srv/caf` + "\u00e9" + `.db`: "file:///srv/caf%C3%A9.db",
	} {
		if got := checkURIPath(in, false); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		`C:\ProgramData\Cryo\console.db`: "file:///C:/ProgramData/Cryo/console.db",
		`\\server\share\cryo\console.db`: "file:////server/share/cryo/console.db",
		`D:\data dir\pct%41\console.db`:  "file:///D:/data%20dir/pct%2541/console.db",
		`data\console.db`:                "file:data/console.db",
	} {
		if got := checkURIPath(in, true); got != want {
			t.Errorf("windows %q: %q, want %q", in, got, want)
		}
	}
}
