package ui

import (
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
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
}
