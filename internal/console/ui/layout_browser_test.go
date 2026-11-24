package ui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// The rendered-layout tests share ONE headless browser (P21-T1). Starting Chrome with a fresh
// profile for every measurement cost 2 to 5 s, and now and then a launch stalled for 15 to 55 s;
// the browser tests were about two thirds of the package's run time. Now the first measurement
// starts a browser that stays up until TestMain ends, showing a runner page that asks this process
// for work over loopback HTTP and reports each answer back.
//
// Each measurement keeps its own isolation: its page is served by its own loopback server (its own
// port, so its own origin, storage and cache), in its own frame of exactly the size the separate
// window used to have, removed once it has answered. The page and its probe are those e.layout
// built before; only the way the browser is started and read changed. Nothing leaves the machine:
// every address is 127.0.0.1, and the browser's background networking is switched off.

// layoutJob is one page to lay out: the URL of its wrapper and the size of its frame.
type layoutJob struct {
	ID     string `json:"id"`
	URL    string `json:"url"`
	W      int    `json:"w"`
	H      int    `json:"h"`
	result chan string
}

type layoutBrowser struct {
	ctl     *httptest.Server // the runner page, /next and /done
	cmd     *exec.Cmd
	profile string
	exited  chan struct{} // closed when the browser process ends
	jobs    chan *layoutJob
	quit    chan struct{} // closed by stop: the runner closes the browser
	seq     atomic.Int64
	mu      sync.Mutex
	pending map[string]*layoutJob
}

var (
	browserMu   sync.Mutex
	browser     *layoutBrowser
	browserDone bool // stopLayoutBrowser ran: no new browser is started

	// layoutSkips counts the layout tests that skipped because no browser was found (CI unset).
	// TestMain names the count at the end (layoutSkipLine). A browser that is found but will not
	// run is no skip: its test fails (browserWouldNotRun).
	layoutSkips atomic.Int64
)

// sharedLayoutBrowser returns the package's browser, starting it on first use. A failed start is
// not remembered: the next measurement tries again, so one bad start costs one test, as a failed
// launch did when every measurement started its own browser.
func sharedLayoutBrowser(chrome string) (*layoutBrowser, error) {
	browserMu.Lock()
	defer browserMu.Unlock()
	if browser != nil {
		select {
		case <-browser.exited: // it died: start another
			browser.close()
			browser = nil
		default:
			return browser, nil
		}
	}
	if browserDone {
		return nil, errors.New("the shared browser was already stopped")
	}
	b, err := startLayoutBrowser(chrome)
	if err != nil {
		return nil, err
	}
	browser = b
	return b, nil
}

// stopLayoutBrowser closes the browser (TestMain, after the tests).
func stopLayoutBrowser() {
	browserMu.Lock()
	defer browserMu.Unlock()
	browserDone = true
	if browser != nil {
		browser.close()
		browser = nil
	}
}

// quietBrowserFlags keep a test browser from doing anything but render: no first-run work, no
// extensions, and none of the background requests (updates, sync, safe-browsing lists, metrics) a
// fresh profile otherwise makes. They change nothing about how a page is laid out or painted.
var quietBrowserFlags = []string{"--no-first-run", "--no-default-browser-check", "--disable-background-networking",
	"--disable-component-update", "--disable-sync", "--disable-extensions", "--disable-default-apps", "--metrics-recording-only",
	"--disable-breakpad", "--disable-client-side-phishing-detection", "--disable-domain-reliability"}

const runnerPage = `<!doctype html><html><head><meta charset="utf-8"><title>layout runner</title></head>
<body style="margin:0;background:#888"><script>
var frames = {};
window.addEventListener('message', function (ev) {
  for (var id in frames) {
    if (frames[id].contentWindow !== ev.source) continue;
    var f = frames[id]; delete frames[id];
    fetch('/done?id=' + encodeURIComponent(id), {method: 'POST', body: String(ev.data)}).then(function () { f.remove(); }, function () { f.remove(); });
    return;
  }
});
var misses = 0; // failed asks in a row: about 5 s of them means the test process is gone, so the browser closes itself
function next() {
  fetch('/next').then(function (r) { return r.status === 200 ? r.json() : null; }).then(function (j) {
    misses = 0;
    if (j && j.quit) { window.close(); return; }
    if (j) {
      var f = document.createElement('iframe');
      f.style.cssText = 'position:absolute;left:0;top:0;border:0;width:' + j.w + 'px;height:' + j.h + 'px';
      frames[j.id] = f; f.src = j.url; document.body.appendChild(f);
    }
    next();
  }, function () { if (++misses >= 25) { window.close(); return; } setTimeout(next, 200); });
}
next();
</script></body></html>`

func startLayoutBrowser(chrome string) (*layoutBrowser, error) {
	profile, err := mkLiveTempDir("browser-")
	if err != nil {
		return nil, err
	}
	b := &layoutBrowser{profile: profile, exited: make(chan struct{}), jobs: make(chan *layoutJob), quit: make(chan struct{}), pending: map[string]*layoutJob{}}
	polled := make(chan struct{})
	var once sync.Once
	mux := http.NewServeMux()
	mux.HandleFunc("GET /runner", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, runnerPage)
	})
	mux.HandleFunc("GET /next", func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(polled) })
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		select {
		case j := <-b.jobs:
			if err := json.NewEncoder(w).Encode(j); err != nil {
				go func() { b.jobs <- j }() // the runner did not get it: hand it out again
			}
		case <-b.quit:
			_, _ = io.WriteString(w, `{"quit":true}`)
		case <-time.After(20 * time.Second):
			w.WriteHeader(http.StatusNoContent)
		case <-r.Context().Done():
		}
	})
	mux.HandleFunc("POST /done", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		b.mu.Lock()
		j := b.pending[r.URL.Query().Get("id")]
		delete(b.pending, r.URL.Query().Get("id"))
		b.mu.Unlock()
		if j != nil {
			j.result <- string(body)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	b.ctl = httptest.NewServer(mux)
	args := append([]string{"--headless=new", "--disable-gpu", "--no-sandbox", "--hide-scrollbars", "--window-size=2000,1400", "--user-data-dir=" + profile,
		"--disable-background-timer-throttling", "--disable-renderer-backgrounding", "--disable-backgrounding-occluded-windows"}, quietBrowserFlags...)
	b.cmd = exec.Command(chrome, append(args, b.ctl.URL+"/runner")...)
	if err := b.cmd.Start(); err != nil {
		b.ctl.Close()
		forgetLiveTempDir(profile)
		_ = os.RemoveAll(profile)
		return nil, err
	}
	go func() { _ = b.cmd.Wait(); close(b.exited) }()
	select {
	case <-polled:
		return b, nil
	case <-b.exited:
		b.close()
		return nil, errors.New("the browser exited before its runner page asked for work")
	case <-time.After(90 * time.Second):
		b.close()
		return nil, errors.New("the browser's runner page did not ask for work within 90 s")
	}
}

// run lays out the wrapper page in dir (page.html and its static/ files) in a frame of w x h and
// returns what the wrapper reported. It gives up after timeout, or when the browser ends.
func (b *layoutBrowser) run(dir string, w, h int, timeout time.Duration) (string, error) {
	pages, err := newPageServer(dir)
	if err != nil {
		return "", fmt.Errorf("%w: %v", errPageServer, err)
	}
	defer pages.retire()
	j := &layoutJob{ID: strconv.FormatInt(b.seq.Add(1), 10), URL: pages.origin + "/page.html", W: w, H: h, result: make(chan string, 1)}
	b.mu.Lock()
	b.pending[j.ID] = j
	b.mu.Unlock()
	defer func() { b.mu.Lock(); delete(b.pending, j.ID); b.mu.Unlock() }()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	select {
	case b.jobs <- j:
	case <-b.exited:
		return "", errors.New("the browser exited")
	case <-ctx.Done():
		return "", fmt.Errorf("the browser took no work for %v", timeout)
	}
	select {
	case r := <-j.result:
		return r, nil
	case <-b.exited:
		return "", errors.New("the browser exited")
	case <-ctx.Done():
		return "", fmt.Errorf("%w within %v", errNoAnswer, timeout)
	}
}

// errNoAnswer is a running browser that took the page and never reported on it: the page or its
// probe is at fault, not the machine, so the test fails rather than skips.
var errNoAnswer = errors.New("no answer from the page")

// errPageServer is a page server that could not be set up: the harness is at fault, so the test fails.
var errPageServer = errors.New("the page server could not start")

// ---- the per-page servers ----
//
// Each measured page is served from its own origin, so nothing one page leaves in the browser
// (cache entries, sessionStorage, which fresh.js uses) can reach another. Two rules make that hold:
//
//   - nothing is cached or revalidated: every response says Cache-Control: no-store and carries no
//     Last-Modified or ETag, and conditional request headers are ignored;
//   - an origin is never handed out twice in a run: a page's listener stays bound, answering 410
//     to everything, until TestMain ends, so the operating system cannot give its port to a later
//     page. (Linux picks ephemeral ports at random; a reused port would be a reused origin.)
//
// checkPageOrigins, run by TestMain at the end, verifies both halves of the second rule for the
// whole run, and TestLayoutPageServer* test them directly.

type pageServer struct {
	origin string
	ln     net.Listener
	srv    *http.Server
	dir    atomic.Pointer[string] // nil once retired: the server answers 410 Gone
}

var pageServers struct {
	mu      sync.Mutex
	all     []*pageServer
	origins map[string]bool
	dupes   []string // origins handed out twice (checkPageOrigins fails the run on any)
}

// newPageServer serves dir from a new loopback origin, never one handed out before in this run.
func newPageServer(dir string) (*pageServer, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	p := &pageServer{origin: "http://" + ln.Addr().String(), ln: ln}
	p.dir.Store(&dir)
	p.srv = &http.Server{Handler: pageHandler(p.dir.Load), ReadHeaderTimeout: time.Minute}
	pageServers.mu.Lock()
	if pageServers.origins == nil {
		pageServers.origins = map[string]bool{}
	}
	if pageServers.origins[p.origin] {
		pageServers.dupes = append(pageServers.dupes, p.origin)
	}
	pageServers.origins[p.origin] = true
	pageServers.all = append(pageServers.all, p)
	pageServers.mu.Unlock()
	go func() { _ = p.srv.Serve(ln) }()
	return p, nil
}

// retire stops serving the page and keeps the port: from now on every request is answered 410.
func (p *pageServer) retire() {
	p.dir.Store(nil)
	p.srv.SetKeepAlivesEnabled(false)
}

// pageHandler serves the files of the directory dir returns, with no caching and no validators,
// or 410 Gone for everything once dir returns nil.
func pageHandler(dir func() *string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		d := dir()
		if d == nil {
			http.Error(w, "this page's measurement is over", http.StatusGone)
			return
		}
		rel := path.Clean("/" + r.URL.Path)
		if r.Method != http.MethodGet && r.Method != http.MethodHead || rel == "/" || strings.ContainsAny(rel, `\:`) {
			http.NotFound(w, r)
			return
		}
		b, err := os.ReadFile(filepath.Join(*d, filepath.FromSlash(rel[1:])))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		if ct := mime.TypeByExtension(path.Ext(rel)); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		// A .br file is a raw Brotli stream, sent as Content-Encoding: br so the browser decodes it
		// (P21-T3: the glyph coverage test reads WOFF2 font data this way; Go has no Brotli decoder).
		if path.Ext(rel) == ".br" {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Encoding", "br")
		}
		_, _ = w.Write(b)
	})
}

// checkPageOrigins reports an origin handed out twice in this run, or a page listener that is no
// longer bound (so its port could have gone to a later page). TestMain runs it before closing them.
func checkPageOrigins() error {
	pageServers.mu.Lock()
	defer pageServers.mu.Unlock()
	if len(pageServers.dupes) > 0 {
		return fmt.Errorf("layout page origins handed out twice: %v", pageServers.dupes)
	}
	for _, p := range pageServers.all {
		c, err := net.DialTimeout("tcp", p.ln.Addr().String(), 5*time.Second)
		if err != nil {
			return fmt.Errorf("layout page listener %s is no longer bound: %v", p.origin, err)
		}
		_ = c.Close()
	}
	return nil
}

// closePageServers closes every page listener (TestMain, after checkPageOrigins).
func closePageServers() {
	pageServers.mu.Lock()
	defer pageServers.mu.Unlock()
	for _, p := range pageServers.all {
		_ = p.srv.Close()
	}
	pageServers.all = nil
}

// close asks the runner to close the browser, kills it if it has not gone within a few seconds,
// and removes its profile.
func (b *layoutBrowser) close() {
	select {
	case <-b.quit:
	default:
		close(b.quit)
	}
	select {
	case <-b.exited:
	case <-time.After(5 * time.Second):
		_ = b.cmd.Process.Kill()
		select {
		case <-b.exited:
		case <-time.After(5 * time.Second):
		}
	}
	b.ctl.CloseClientConnections()
	b.ctl.Close()
	forgetLiveTempDir(b.profile)
	for i := 0; i < 20; i++ { // the browser's helper processes can hold profile files a moment longer
		if err := os.RemoveAll(b.profile); err == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}
