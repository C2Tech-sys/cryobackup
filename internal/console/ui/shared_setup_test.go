package ui

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
	removeStaleTempDirs(os.TempDir(), staleTempAge)
	runKeeper = keepLiveTempDirsFresh(liveTouchEvery)
	parseTemplatesFn = clonedTemplates
	code := m.Run()
	runKeeper.stop()
	stopLayoutBrowser()
	if err := checkPageOrigins(); err != nil { // the layout pages' isolation held for the whole run
		fmt.Fprintln(os.Stderr, "FAIL:", err)
		code = 1
	}
	closePageServers()
	removeFixtureTemplate()
	if line := layoutSkipLine(layoutSkips.Load()); line != "" {
		fmt.Fprintln(os.Stderr, line)
	}
	os.Exit(code)
}

// layoutSkipLine is the line TestMain prints when layout tests skipped, or "" when none did. The
// only skip left is a machine with no browser at all (chromeOrSkip); a browser that is found but
// will not start fails its test, with CI set or not (P21-T2 F1, browserWouldNotRun). The line
// shows with -v or a package run; go test ./... drops a passing package's output.
func layoutSkipLine(n int64) string {
	if n <= 0 {
		return ""
	}
	return fmt.Sprintf("%d layout tests skipped because no Chrome or Chromium was found (set CRYO_CHROME; with CI set they fail instead)", n)
}

// tempPrefix starts the name of every temporary directory this package makes outside t.TempDir:
// the fixture template, each env's store copy and the browser profile.
const tempPrefix = "cryo-ui-"

// staleTempAge is how old a leftover directory must be before a run's start-up sweep removes it. A
// directory's modification time does not move while a run merely uses it, so age alone cannot tell
// a live run from a dead one: the margin is a day (no run of this package lasts that long, even
// under -race with -count or in a debugger), and a live run also touches its own directories from
// its start and every liveTouchEvery after (keepLiveTempDirsFresh), so they never look older than
// that (P21-T2 F2).
const staleTempAge = 24 * time.Hour

// liveTouchEvery is how often a running package touches its own temporary directories.
const liveTouchEvery = 10 * time.Minute

// liveTempDirs is the temporary directories this run made outside t.TempDir and has not removed yet.
var liveTempDirs struct {
	mu   sync.Mutex
	dirs map[string]bool
}

// mkLiveTempDir makes a temporary directory named tempPrefix+pattern and registers it as this
// run's, so it is kept fresh until forgetLiveTempDir.
func mkLiveTempDir(pattern string) (string, error) {
	dir, err := os.MkdirTemp("", tempPrefix+pattern)
	if err != nil {
		return "", err
	}
	liveTempDirs.mu.Lock()
	defer liveTempDirs.mu.Unlock()
	if liveTempDirs.dirs == nil {
		liveTempDirs.dirs = map[string]bool{}
	}
	liveTempDirs.dirs[dir] = true
	return dir, nil
}

// forgetLiveTempDir stops keeping dir fresh (it is being removed).
func forgetLiveTempDir(dir string) {
	liveTempDirs.mu.Lock()
	defer liveTempDirs.mu.Unlock()
	delete(liveTempDirs.dirs, dir)
}

// touchLiveTempDirs sets the modification time of every directory this run has made and still
// uses to now, so another run's start-up sweep never takes it for a leftover.
func touchLiveTempDirs() {
	liveTempDirs.mu.Lock()
	defer liveTempDirs.mu.Unlock()
	now := time.Now()
	for d := range liveTempDirs.dirs {
		_ = os.Chtimes(d, now, now)
	}
}

// tempKeeper is the goroutine that keeps this run's temporary directories fresh.
type tempKeeper struct {
	done, finished chan struct{}
	nudge          chan chan struct{} // touchNow asks for a touch over this channel
}

// runKeeper is the keeper TestMain started for this test process (nil outside TestMain's run).
// TestP21T2_F2_TheRunKeepsItsDirectoriesFresh asks it for a touch, so a TestMain that does not start
// it, or a keeper that does not touch, fails a test.
var runKeeper *tempKeeper

// keepLiveTempDirsFresh touches this run's directories at once and then every interval, from the
// start of the run until stop is called (TestMain). The first touch is done before it returns.
func keepLiveTempDirsFresh(every time.Duration) *tempKeeper {
	k := &tempKeeper{done: make(chan struct{}), finished: make(chan struct{}), nudge: make(chan chan struct{})}
	touchLiveTempDirs()
	go func() {
		defer close(k.finished)
		tick := time.NewTicker(every)
		defer tick.Stop()
		for {
			select {
			case <-tick.C:
				touchLiveTempDirs()
			case ack := <-k.nudge:
				touchLiveTempDirs()
				close(ack)
			case <-k.done:
				return
			}
		}
	}()
	return k
}

// stop ends the keeper and waits for it.
func (k *tempKeeper) stop() { close(k.done); <-k.finished }

// touchNow asks the running keeper for a touch, as its tick would, and waits until it has been
// done. False when the keeper does not answer within 30 seconds (it is not running).
func (k *tempKeeper) touchNow() bool {
	ack := make(chan struct{})
	select {
	case k.nudge <- ack:
	case <-time.After(30 * time.Second):
		return false
	}
	select {
	case <-ack:
		return true
	case <-time.After(30 * time.Second):
		return false
	}
}

// removeStaleTempDirs removes from root (the temp folder) this package's own temporary directories
// left behind by an earlier run that was killed before its cleanups ran (a timeout, a hard kill),
// so they do not pile up there. It touches only directories named tempPrefix* and only those last
// modified more than maxAge ago (staleTempAge at start-up): a younger one may belong to a run of
// this package still going.
func removeStaleTempDirs(root string, maxAge time.Duration) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, de := range entries {
		if !de.IsDir() || !strings.HasPrefix(de.Name(), tempPrefix) {
			continue
		}
		if fi, err := de.Info(); err == nil && time.Since(fi.ModTime()) > maxAge {
			_ = os.RemoveAll(filepath.Join(root, de.Name()))
		}
	}
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
	dir, err := mkLiveTempDir("template-")
	if err != nil {
		return "", err
	}
	now := fixtures.Now()
	st, err := store.Open(ctx, store.Options{Path: filepath.Join(dir, "console.db"), Clock: func() time.Time { return now }})
	if err != nil {
		forgetLiveTempDir(dir)
		_ = os.RemoveAll(dir)
		return "", err
	}
	err = seedFixtureTemplate(ctx, st, now)
	if cerr := st.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		forgetLiveTempDir(dir)
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
		forgetLiveTempDir(templateDir)
		_ = os.RemoveAll(templateDir)
	}
}

// openFixtureStore opens a private copy of the fixture template with clock as the store's clock.
// Like store.OpenMemory, it lives in a fresh temporary directory removed after Close.
func openFixtureStore(t *testing.T, clock func() time.Time) *store.Store {
	st, _ := openFixtureStoreDir(t, clock)
	return st
}

// openFixtureStoreDir is openFixtureStore and the directory of the copy (registered as the run's, so
// kept fresh), for the test that checks the registration.
func openFixtureStoreDir(t *testing.T, clock func() time.Time) (*store.Store, string) {
	t.Helper()
	src, err := fixtureTemplate()
	if err != nil {
		t.Fatalf("building the fixture database: %v", err)
	}
	dir, err := mkLiveTempDir("store-")
	if err != nil {
		t.Fatal(err)
	}
	remove := func() { forgetLiveTempDir(dir); _ = os.RemoveAll(dir) }
	for _, name := range []string{"console.db", "console.db-wal"} {
		if err := copyFile(filepath.Join(src, name), filepath.Join(dir, name)); err != nil && !(errors.Is(err, os.ErrNotExist) && name != "console.db") {
			remove()
			t.Fatal(err)
		}
	}
	st, err := store.Open(context.Background(), store.Options{Path: filepath.Join(dir, "console.db"), Clock: clock})
	if err != nil {
		remove()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close(); remove(); st = nil }) // st = nil: see serveForTest
	return st, dir
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
