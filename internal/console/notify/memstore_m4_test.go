package notify

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// P20-W2BF D1: a fresh store per test, at a tenth of the cost under the race detector. Applying
// every migration to an empty file (store.OpenMemory) parses and runs the whole schema, which the
// race detector and checkptr make take seconds (the SQLite engine is pure Go); this package opens
// hundreds of stores. Instead, the first store at a clock instant is migrated once into a template
// file, closed (which checkpoints it into the one file), and each test opens its own copy with
// store.Open: the same schema and migration rows, applied at the same clock instant, found already
// applied. The store package's own tests cover migrating from empty.

var templates struct {
	mu   sync.Mutex
	dir  string
	path map[int64]string // clock instant (unix ms) -> template file
}

// TestMain removes the template files when the tests are done.
func TestMain(m *testing.M) {
	code := m.Run()
	templates.mu.Lock()
	if templates.dir != "" {
		_ = os.RemoveAll(templates.dir)
	}
	templates.mu.Unlock()
	os.Exit(code)
}

// templateAt is the path of a migrated, closed store whose migrations were applied at instant at.
func templateAt(at time.Time) (string, error) {
	templates.mu.Lock()
	defer templates.mu.Unlock()
	if p, ok := templates.path[at.UnixMilli()]; ok {
		return p, nil
	}
	if templates.dir == "" {
		dir, err := os.MkdirTemp("", "cryo-notify-tpl-")
		if err != nil {
			return "", err
		}
		templates.dir, templates.path = dir, map[int64]string{}
	}
	p := filepath.Join(templates.dir, fmt.Sprintf("tpl-%d.db", at.UnixMilli()))
	st, err := store.Open(context.Background(), store.Options{Path: p, Clock: func() time.Time { return at }})
	if err != nil {
		return "", err
	}
	if err := st.Close(); err != nil {
		return "", err
	}
	if fi, err := os.Stat(p + "-wal"); err == nil && fi.Size() > 0 {
		return "", fmt.Errorf("template %s left a non-empty WAL: a copy of the main file alone would lack it", filepath.Base(p))
	}
	templates.path[at.UnixMilli()] = p
	return p, nil
}

// copyTemplate opens, for t, a store on a fresh copy of the template migrated at instant at.
func copyTemplate(t *testing.T, at time.Time, clock func() time.Time) *store.Store {
	t.Helper()
	tpl, err := templateAt(at)
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "console.db")
	if err := copyFileTo(tpl, dst); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), store.Options{Path: dst, Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func copyFileTo(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
