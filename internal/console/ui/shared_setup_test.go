package ui

import (
	"context"
	"errors"
	"html/template"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// Set-up shared by the whole package (P21-T1). Building the fixture world is the same work for
// every test: migrate an empty database, hash the test password (argon2id at the production
// parameters) and seed the fixtures. It is done once per test binary; each test then opens its OWN
// copy of the resulting database file, so no test sees another's writes and the order the tests
// run in changes nothing. The page templates are parsed once and every Service gets its own clone,
// and the browser of the rendered-layout tests is shared too (layout_browser_test.go). TestMain
// removes what was built once.
//
// Most tests call t.Parallel(): each builds its own env (database copy, Service, test server). The
// few that change process-wide state (a fixtures switch, a package variable) stay serial and say so
// in their first line; Go runs every serial test before it releases the parallel ones.

func TestMain(m *testing.M) {
	parseTemplatesFn = clonedTemplates
	code := m.Run()
	stopLayoutBrowser()
	removeFixtureTemplate()
	os.Exit(code)
}

// parsedTemplates is the page templates parsed once, never executed: only clones of it are.
var parsedTemplates = sync.OnceValues(func() (map[string]*template.Template, error) {
	return parseTemplates((&Service{}).funcs()) // the functions are only named here; each clone gets its own Service's
})

// clonedTemplates is what parseTemplates returns, built from one parse: each page template is cloned
// (html/template copies the parse trees, so escaping one clone touches no other) and given fm, the
// calling Service's functions, in place of the placeholder ones it was parsed with.
func clonedTemplates(fm template.FuncMap) (map[string]*template.Template, error) {
	base, err := parsedTemplates()
	if err != nil {
		return nil, err
	}
	out := make(map[string]*template.Template, len(base))
	for name, t := range base {
		c, err := t.Clone()
		if err != nil {
			return nil, err
		}
		out[name] = c.Funcs(fm)
	}
	return out, nil
}

// testPasswordHash is the argon2id PHC string of testPassword, computed once. A fresh salt per
// account protects nothing in a test; the verification on login still runs in full.
var testPasswordHash = sync.OnceValue(func() string {
	h, err := auth.HashPassword(testPassword)
	if err != nil {
		panic(err)
	}
	return h
})

// The fixture template: a closed, checkpointed database holding exactly what newEnv used to build
// in every test (the fixtures with the admin's password hash, plus a snapshot each for
// DR-OFFICE-PC and LEGAL-PC-01, written at the fixture clock).
var (
	templateOnce sync.Once
	templateDir  string
	templateErr  error
)

func fixtureTemplate() (string, error) {
	templateOnce.Do(func() {
		templateDir, templateErr = buildFixtureTemplate()
	})
	return templateDir, templateErr
}

func buildFixtureTemplate() (string, error) {
	ctx := context.Background()
	dir, err := os.MkdirTemp("", "cryo-ui-template-")
	if err != nil {
		return "", err
	}
	now := fixtures.Now()
	st, err := store.Open(ctx, store.Options{Path: filepath.Join(dir, "console.db"), Clock: func() time.Time { return now }})
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	err = seedFixtureTemplate(ctx, st, now)
	if cerr := st.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}

func seedFixtureTemplate(ctx context.Context, st *store.Store, now time.Time) error {
	if err := store.LoadFixtures(ctx, st, now, store.WithAdminPasswordHash(testPasswordHash())); err != nil {
		return err
	}
	l, err := st.ListDevices(ctx, store.ScopeAll(), store.ListDevicesOptions{IncludeRetired: true, Page: store.Page{Limit: 100}})
	if err != nil {
		return err
	}
	ids := map[string]string{}
	for _, d := range l.Items {
		ids[d.Hostname] = d.ID
	}
	for _, d := range []string{"DR-OFFICE-PC", "LEGAL-PC-01"} {
		if ids[d] == "" {
			return errors.New("fixture template: no device " + d)
		}
		if _, _, err := st.AgentReplaceSnapshots(ctx, ids[d], []store.Snapshot{{DeviceID: ids[d], SnapshotID: fixtures.FixtureSnapshotID, TakenAt: now.Add(-time.Hour), Hostname: d, Paths: []string{`C:\Users`}}}); err != nil {
			return err
		}
	}
	return nil
}

func removeFixtureTemplate() {
	if templateDir != "" {
		_ = os.RemoveAll(templateDir)
	}
}

// openFixtureStore opens a private copy of the fixture template with clock as the store's clock.
// Like store.OpenMemory, it lives in a fresh temporary directory removed after Close.
func openFixtureStore(t *testing.T, clock func() time.Time) *store.Store {
	t.Helper()
	src, err := fixtureTemplate()
	if err != nil {
		t.Fatalf("building the fixture database: %v", err)
	}
	dir, err := os.MkdirTemp("", "cryo-ui-store-")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"console.db", "console.db-wal"} {
		if err := copyFile(filepath.Join(src, name), filepath.Join(dir, name)); err != nil && !(errors.Is(err, os.ErrNotExist) && name != "console.db") {
			_ = os.RemoveAll(dir)
			t.Fatal(err)
		}
	}
	st, err := store.Open(context.Background(), store.Options{Path: filepath.Join(dir, "console.db"), Clock: clock})
	if err != nil {
		_ = os.RemoveAll(dir)
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close(); _ = os.RemoveAll(dir); st = nil }) // st = nil: see serveForTest
	return st
}

// serveForTest starts h on a loopback test server that is closed when t ends.
//
// The cleanup forgets the server once it has closed it, and openFixtureStore's forgets the store,
// because a finished parallel test is not freed at once: its *testing.T stays reachable until every
// parallel test of the package has ended, and the spent cleanup closures stay reachable with it. A
// closure still holding the server (its handler is the Service: templates, store, clock and env) kept
// about 40 MB per test alive, gigabytes over a run.
func serveForTest(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(func() { srv.Close(); srv = nil })
	return srv
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
