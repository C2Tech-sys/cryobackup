package ui

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Tests of the package's own test harness (P21-T1 fix batch 1): the layout pages' isolation and
// the clean-up of directories a killed run left behind.

// Q1 (a): a page server's every answer forbids caching and gives the browser nothing to revalidate
// against, so a later page can never be answered from what an earlier one left in the cache.
func TestLayoutPageServerSendsNoCacheValidators(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "static"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"page.html": "<p>page</p>", "static/app.css": "p { color: red }"} {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(name)), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	p, err := newPageServer(dir)
	if err != nil {
		t.Fatal(err)
	}
	get := func(path string) (*http.Response, string) {
		t.Helper()
		r, _ := http.NewRequest("GET", p.origin+path, nil)
		r.Header.Set("If-Modified-Since", time.Now().Add(time.Hour).UTC().Format(http.TimeFormat))
		r.Header.Set("If-None-Match", `"anything"`)
		res, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res, string(b)
	}
	check := func(path string, wantStatus int, wantBody, wantType string) {
		t.Helper()
		res, body := get(path)
		if res.StatusCode != wantStatus || (wantBody != "" && body != wantBody) {
			t.Errorf("GET %s (conditional): %d %q, want %d %q", path, res.StatusCode, body, wantStatus, wantBody)
		}
		if cc := res.Header.Get("Cache-Control"); cc != "no-store" {
			t.Errorf("GET %s: Cache-Control %q, want no-store", path, cc)
		}
		for _, h := range []string{"Last-Modified", "ETag"} {
			if v := res.Header.Get(h); v != "" {
				t.Errorf("GET %s: sends the validator %s: %q", path, h, v)
			}
		}
		if wantType != "" && res.Header.Get("Content-Type") != wantType {
			t.Errorf("GET %s: Content-Type %q, want %q", path, res.Header.Get("Content-Type"), wantType)
		}
	}
	check("/page.html", 200, "<p>page</p>", "text/html; charset=utf-8")
	check("/static/app.css", 200, "p { color: red }", "text/css; charset=utf-8")
	check("/missing.css", 404, "", "")
	check("/../page.html", 200, "<p>page</p>", "") // a path cannot climb out of the page's directory
	p.retire()
	check("/page.html", 410, "", "") // a retired page serves nothing, from a port it still holds
}

// Q1 (b): no origin is handed out twice in a run. A retired page keeps its listener bound, so the
// operating system cannot give the port to a later page; checkPageOrigins (which TestMain also runs
// over the whole run) finds no duplicate and every listener still bound.
func TestLayoutPageOriginsAreNeverReused(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	seen := map[string]bool{}
	var mine []*pageServer
	for i := 0; i < 64; i++ { // one after another, as measurements come: each retired before the next starts
		p, err := newPageServer(dir)
		if err != nil {
			t.Fatal(err)
		}
		if seen[p.origin] {
			t.Fatalf("origin %s handed out twice", p.origin)
		}
		seen[p.origin] = true
		p.retire()
		mine = append(mine, p)
	}
	for _, p := range mine {
		c, err := net.DialTimeout("tcp", p.ln.Addr().String(), 5*time.Second)
		if err != nil {
			t.Fatalf("retired page %s no longer holds its port: %v", p.origin, err)
		}
		_ = c.Close()
		if l, err := net.Listen("tcp", p.ln.Addr().String()); err == nil {
			_ = l.Close()
			t.Fatalf("the port of retired page %s could be bound again", p.origin)
		}
	}
	if err := checkPageOrigins(); err != nil {
		t.Fatal(err)
	}
}

// Q3: the start-up sweep removes only this package's own leftovers, and only old ones.
// P21-T2 F2: and never a live run's. The start-up age is a day, and a running package touches its
// own directories from its start, so one it is still using is never old, however long it has run.
func TestStaleTempDirsOnlyOldOwnOnesAreRemoved(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	old := time.Now().Add(-2 * time.Hour)
	mk := func(name string, mod time.Time, dir bool) string {
		t.Helper()
		p := filepath.Join(root, name)
		var err error
		if dir {
			err = os.MkdirAll(filepath.Join(p, "inner"), 0o755)
		} else {
			err = os.WriteFile(p, []byte("x"), 0o644)
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mod, mod); err != nil {
			t.Fatal(err)
		}
		return p
	}
	gone := mk(tempPrefix+"store-old", old, true)
	keep := []string{
		mk(tempPrefix+"store-young", time.Now(), true), // may belong to a run still going
		mk("cryo-store-old", old, true),                // another package's
		mk("someone-else", old, true),
		mk(tempPrefix+"a-file", old, false), // not a directory
	}
	removeStaleTempDirs(root, time.Hour)
	if _, err := os.Stat(gone); !os.IsNotExist(err) {
		t.Errorf("%s is still there (%v)", gone, err)
	}
	for _, p := range keep {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s was removed: %v", p, err)
		}
	}

	// F2: the start-up sweep's age is a day. A directory of a run alive for hours (race with -count,
	// a debugger) stays; one older than a day goes.
	if staleTempAge != 24*time.Hour {
		t.Fatalf("the start-up sweep age is %v, want 24h", staleTempAge)
	}
	hoursOld := mk(tempPrefix+"store-hours", time.Now().Add(-5*time.Hour), true)
	dayOld := mk(tempPrefix+"store-day", time.Now().Add(-25*time.Hour), true)
	// A live run's directory whose own mtime never moved (only its files changed): the run touches it.
	live := mk(tempPrefix+"store-live", time.Now().Add(-48*time.Hour), true)
	liveTempDirs.mu.Lock()
	if liveTempDirs.dirs == nil {
		liveTempDirs.dirs = map[string]bool{}
	}
	liveTempDirs.dirs[live] = true
	liveTempDirs.mu.Unlock()
	t.Cleanup(func() { forgetLiveTempDir(live) })
	k := keepLiveTempDirsFresh(time.Hour) // touches at once, at the start of the run
	k.stop()
	if fi, err := os.Stat(live); err != nil || time.Since(fi.ModTime()) > time.Minute {
		t.Fatalf("the live directory was not touched at the start of the run (%v)", err)
	}
	removeStaleTempDirs(root, staleTempAge)
	for _, p := range []string{hoursOld, live} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s was removed by the start-up sweep: %v", p, err)
		}
	}
	if _, err := os.Stat(dayOld); !os.IsNotExist(err) {
		t.Errorf("%s, older than a day, is still there (%v)", dayOld, err)
	}
	// The touch is what kept it: with no touch, the same directory at the same age is swept.
	// The directory is forgotten BEFORE it is aged: the process's own keeper (TestMain's) may tick at
	// any moment, and a tick between the two would touch a directory this test means to leave alone.
	forgetLiveTempDir(live)
	old48 := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(live, old48, old48); err != nil {
		t.Fatal(err)
	}
	touchLiveTempDirs()
	removeStaleTempDirs(root, staleTempAge)
	if _, err := os.Stat(live); !os.IsNotExist(err) {
		t.Errorf("an untouched directory two days old was kept (%v): the test does not see the touch", err)
	}
	// The package's own directories are registered: the fixture template is one of them.
	tpl, err := fixtureTemplate()
	if err != nil {
		t.Fatal(err)
	}
	liveTempDirs.mu.Lock()
	registered := liveTempDirs.dirs[tpl]
	liveTempDirs.mu.Unlock()
	if !registered {
		t.Errorf("the fixture template %s is not among the directories the run keeps fresh", tpl)
	}
}

// tbRecorder is a testing.TB that records how a helper ended the test instead of ending this one:
// Fatal* and Skip* stop the calling goroutine, as the real ones do.
type tbRecorder struct {
	testing.TB
	failed, skipped bool
	msg             string
}

func (r *tbRecorder) Helper()                   {}
func (r *tbRecorder) Logf(string, ...any)       {}
func (r *tbRecorder) Errorf(f string, a ...any) { r.failed, r.msg = true, fmt.Sprintf(f, a...) }
func (r *tbRecorder) Error(a ...any)            { r.failed, r.msg = true, fmt.Sprint(a...) }
func (r *tbRecorder) Fail()                     { r.failed = true }
func (r *tbRecorder) FailNow()                  { r.failed = true; runtime.Goexit() }
func (r *tbRecorder) Fatalf(f string, a ...any) { r.Errorf(f, a...); runtime.Goexit() }
func (r *tbRecorder) Fatal(a ...any)            { r.Error(a...); runtime.Goexit() }
func (r *tbRecorder) SkipNow()                  { r.skipped = true; runtime.Goexit() }
func (r *tbRecorder) Skipf(f string, a ...any) {
	r.skipped, r.msg = true, fmt.Sprintf(f, a...)
	runtime.Goexit()
}
func (r *tbRecorder) Skip(a ...any) { r.skipped, r.msg = true, fmt.Sprint(a...); runtime.Goexit() }
func (r *tbRecorder) run(f func(testing.TB)) *tbRecorder {
	done := make(chan struct{})
	go func() { defer close(done); f(r) }()
	<-done
	return r
}

// P21-T2 F1: a browser that is FOUND but will not start fails the test, with CI unset as with CI
// set; it is no longer a skip, and it adds nothing to the skip count. (No browser installed at all
// stays a skip: chromeOrSkip, unchanged.)
func TestP21T2_F1_AFoundBrowserThatWillNotStartIsAFailure(t *testing.T) {
	// Serial (P21-T2): it sets CI, a process-wide variable, so it never runs beside the parallel tests.
	bogus := filepath.Join(t.TempDir(), "chrome.exe")
	if err := os.WriteFile(bogus, []byte("not a browser"), 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := startLayoutBrowser(bogus) // found (the file is there), and it cannot start
	if err == nil {
		b.close()
		t.Fatal("a file that is no browser started as one")
	}
	before := layoutSkips.Load()
	for _, ci := range []string{"", "true"} {
		t.Setenv("CI", ci)
		r := (&tbRecorder{}).run(func(tb testing.TB) { browserWouldNotRun(tb, err) })
		if !r.failed || r.skipped {
			t.Errorf("CI=%q: a found browser that would not start: failed %v, skipped %v (%s)", ci, r.failed, r.skipped, r.msg)
		}
		if !strings.Contains(r.msg, "would not run") || !strings.Contains(r.msg, err.Error()) {
			t.Errorf("CI=%q: the failure does not say what happened: %q", ci, r.msg)
		}
	}
	if layoutSkips.Load() != before {
		t.Error("a found browser that would not start was counted as a skip")
	}
	// An error the page or the harness caused is not the browser's: the layout helper reports those
	// itself (errNoAnswer, errPageServer); they are still told apart from a browser that will not run.
	if errors.Is(err, errNoAnswer) || errors.Is(err, errPageServer) {
		t.Errorf("the start failure %v reads as a page or harness fault", err)
	}
}

// P21-T2 F1: the count line names the one skip left, a machine with no browser at all, and is not
// printed when nothing skipped.
func TestP21T2_F1_TheSkipCountLineNamesNoBrowserFound(t *testing.T) {
	t.Parallel()
	if l := layoutSkipLine(0); l != "" {
		t.Errorf("no skips printed %q", l)
	}
	l := layoutSkipLine(3)
	if !strings.HasPrefix(l, "3 layout tests skipped") || !strings.Contains(l, "no Chrome or Chromium was found") || !strings.Contains(l, "CRYO_CHROME") {
		t.Errorf("the count line is %q", l)
	}
	if strings.Contains(l, "did not start") || strings.Contains(l, "would not run") {
		t.Errorf("the count line still counts a browser that would not start as a skip: %q", l)
	}
}

// P21-T2 fix batch 1 R4 (b), (c): the run keeps its directories fresh. TestMain started the keeper
// (runKeeper) and it touches what the run registered: a directory aged two days is fresh after the
// keeper's touch, for a directory registered by hand and for the copy of the fixture store every env
// opens (openFixtureStore). Without the keeper started, or with the store copies not registered, a
// long run's directories would look like leftovers to another run's sweep.
func TestP21T2_F2_TheRunKeepsItsDirectoriesFresh(t *testing.T) {
	t.Parallel()
	if runKeeper == nil {
		t.Fatal("TestMain did not start the keep-fresh goroutine (runKeeper is nil)")
	}
	by, err := mkLiveTempDir("keep-")
	if err != nil {
		t.Fatal(err)
	}
	_, storeDir := openFixtureStoreDir(t, time.Now)
	t.Cleanup(func() { forgetLiveTempDir(by); _ = os.RemoveAll(by) })
	dirs := map[string]string{"a directory registered with mkLiveTempDir": by, "the copy of the fixture store": storeDir}
	aged := time.Now().Add(-48 * time.Hour)
	for what, d := range dirs {
		if err := os.Chtimes(d, aged, aged); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	if !runKeeper.touchNow() {
		t.Fatal("the keep-fresh goroutine started by TestMain does not answer")
	}
	for what, d := range dirs {
		if fi, err := os.Stat(d); err != nil || time.Since(fi.ModTime()) > time.Minute {
			t.Errorf("%s was not touched by the run's keeper (%v)", what, err)
		}
	}
}

// P21-T2 fix batch 1 R4 (a): the F1 rule holds where layout ends the test, not only in the helper
// browserWouldNotRun. layout is given a browser that is FOUND (CRYO_CHROME names a file) and will not
// start, with CI unset and set; it must fail the test, and must not skip it or count a skip. Putting
// the old t.Skipf back at layout's call site turns this red, as the helper's own test cannot.
func TestP21T2_F1_LayoutFailsWhenTheFoundBrowserWillNotStart(t *testing.T) {
	// Serial (P21-T2): it sets CI and CRYO_CHROME, process-wide variables.
	bogus := filepath.Join(t.TempDir(), "chrome.exe")
	if err := os.WriteFile(bogus, []byte("not a browser"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRYO_CHROME", bogus)
	e := newEnv(t)
	before := layoutSkips.Load()
	for _, ci := range []string{"", "true"} {
		t.Setenv("CI", ci)
		r := &tbRecorder{TB: t}
		r.run(func(tb testing.TB) {
			e.layout(tb, layoutOpts{HTML: "<!doctype html><html><body><p>x</p></body></html>", Probe: "return {iw: innerWidth};",
				Start: startLayoutBrowser}) // not the shared browser: a running one would answer
		})
		if !r.failed || r.skipped {
			t.Errorf("CI=%q: layout with a found browser that would not start: failed %v, skipped %v (%s)", ci, r.failed, r.skipped, r.msg)
		}
		if !strings.Contains(r.msg, "would not run") {
			t.Errorf("CI=%q: the failure does not say the browser would not run: %q", ci, r.msg)
		}
	}
	if layoutSkips.Load() != before {
		t.Error("a found browser that would not start was counted as a skip")
	}
}
