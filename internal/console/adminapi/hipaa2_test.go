package adminapi

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
)

const hipaa2Path = "/C/Users/Public/Documents/Quarterly-Report.txt"

func (e *env) auditCount(action, id string) int {
	e.t.Helper()
	l, err := e.st.ListAudit(context.Background(), store.ScopeAll(), store.AuditFilter{Action: action, ObjectID: id}, store.Page{Limit: 50})
	if err != nil {
		e.t.Fatal(err)
	}
	return len(l.Items)
}

// TestRedownloadUntilExpiry (M1-HIPAA2 item 2, operator ruling 2026-10-29):
// (a) a delivered restore streams again with a new token and counts every
// download; (d) every download, first or repeat, writes its restore.download
// audit row; (b) past expires_at every token - one minted before expiry
// included - and every mint is refused with 410 Gone; (c) the purge job
// clears the encrypted selection, and the expired restore still answers 410.
func TestRedownloadUntilExpiry(t *testing.T) {
	e := newEnv(t)
	s := e.login("admin", "pw-for-tests-1")
	rj := e.newRestore(s, hipaa2Path)
	var first []byte
	for i := 1; i <= 3; i++ {
		u, _ := e.mint(s, rj.ID)
		res, body := e.req("GET", u, s, nil)
		if res.StatusCode != 200 || len(body) == 0 {
			t.Fatalf("download %d: %d", i, res.StatusCode)
		}
		if i == 1 {
			first = body
		} else if !bytes.Equal(body, first) {
			t.Fatalf("download %d streamed different bytes", i)
		}
		if res, _ := e.req("GET", u, s, nil); res.StatusCode != 403 {
			t.Fatalf("download %d: a used token must not work again: %d", i, res.StatusCode)
		}
		cur, _ := e.st.GetRestoreRequest(context.Background(), store.ScopeAll(), rj.ID)
		if cur.Status != "delivered" || cur.DownloadCount != i {
			t.Fatalf("after download %d: %+v", i, cur)
		}
		if n := e.auditCount("restore.download", rj.ID); n != i {
			t.Fatalf("after download %d: %d restore.download audit rows", i, n)
		}
		e.clk.Advance(time.Hour)
	}
	if n := e.be.Count(adminpath.MethodDump); n != 3 {
		t.Fatalf("%d dump streams for three downloads: each download streams again", n)
	}

	// (b) A token minted a minute before expires_at, presented after it.
	cur, _ := e.st.GetRestoreRequest(context.Background(), store.ScopeAll(), rj.ID)
	e.clk.Advance(cur.ExpiresAt.Sub(e.clk.Now()) - time.Minute)
	s = e.login("admin", "pw-for-tests-1") // the old session aged with the clock
	late, _ := e.mint(s, rj.ID)
	if late == "" {
		t.Fatal("a token is minted until expires_at")
	}
	e.clk.Advance(time.Minute)
	gone := func(what string, res *http.Response, b []byte) {
		t.Helper()
		if res.StatusCode != http.StatusGone || problemCode(t, res, b) != api.CodeNotFound {
			t.Fatalf("%s past expires_at: %d %s", what, res.StatusCode, b)
		}
	}
	res, b := e.req("GET", late, s, nil)
	gone("a token minted before expiry", res, b)
	res, b = e.req("GET", "/api/v1/restores/"+rj.ID+"/download?token=garbage", s, nil)
	gone("a garbage token", res, b)
	res, b = e.req("POST", "/api/v1/restores/"+rj.ID+"/download", s, nil)
	gone("a mint", res, b)
	if n := e.be.Count(adminpath.MethodDump); n != 3 {
		t.Fatalf("a refused download streamed: %d dump calls", n)
	}

	// (c) The purge job leaves only the hash of the selection.
	if _, err := e.rest.Purge(context.Background()); err != nil {
		t.Fatal(err)
	}
	cur, _ = e.st.GetRestoreRequest(context.Background(), store.ScopeAll(), rj.ID)
	if cur.Status != "expired" || cur.SelectionWrapped != nil || cur.SelectionHash == "" {
		t.Fatalf("after the purge: %+v", cur)
	}
	res, b = e.req("POST", "/api/v1/restores/"+rj.ID+"/download", s, nil)
	gone("a mint after the purge", res, b)
	if n := e.auditCount("restore.download", rj.ID); n != 3 {
		t.Fatalf("%d restore.download audit rows, want one per completed download", n)
	}
}

// TestBusyStreamsAnswerTryAgain (M1-HIPAA2 item 1): when the worker's stream
// budget is used up, the download is a 503 problem with Retry-After, not a
// queued wait; nothing is recorded on the restore; and the token is not spent:
// the same URL streams once a slot is free, exactly once. A browser navigation
// is sent back to the restore page instead, which says so in plain words.
func TestBusyStreamsAnswerTryAgain(t *testing.T) {
	e := newEnv(t)
	s := e.login("admin", "pw-for-tests-1")
	e.be.Errors = map[string]error{adminpath.MethodDump: adminpath.NewError(adminpath.CodeBusy, "")}
	rj := e.newRestore(s, hipaa2Path)
	u, _ := e.mint(s, rj.ID)
	res, b := e.req("GET", u, s, nil)
	if res.StatusCode != http.StatusServiceUnavailable || problemCode(t, res, b) != api.CodeUnavailable || res.Header.Get(api.HeaderRetryAfter) == "" {
		t.Fatalf("busy: %d %v %s", res.StatusCode, res.Header, b)
	}
	cur, _ := e.st.GetRestoreRequest(context.Background(), store.ScopeAll(), rj.ID)
	if cur.ErrorClass != "" || cur.Status != "ready" || cur.StartedAt != nil || e.auditCount("restore.failed", rj.ID) != 0 {
		t.Fatalf("a busy refusal was recorded: %+v", cur)
	}
	// A browser navigation is sent to the restore page, token still unspent.
	nav, _ := http.NewRequest("GET", e.srv.URL+u, nil)
	for _, c := range s.cookies {
		nav.AddCookie(c)
	}
	nav.Header.Set("Accept", "text/html,application/xhtml+xml")
	nav.Header.Set("Sec-Fetch-Mode", "navigate")
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	nres, err := noFollow.Do(nav)
	if err != nil {
		t.Fatal(err)
	}
	nres.Body.Close()
	if want := "/restores/" + rj.ID + "?notice=" + BusyNotice + "&at=" + strconv.FormatInt(e.clk.Now().Unix(), 10); nres.StatusCode != http.StatusSeeOther || nres.Header.Get("Location") != want {
		t.Fatalf("busy navigation: %d %q", nres.StatusCode, nres.Header.Get("Location"))
	}
	// Try again with the same URL once a slot is free: it streams, once.
	e.be.Errors = nil
	if res, body := e.req("GET", u, s, nil); res.StatusCode != 200 || len(body) == 0 {
		t.Fatalf("the same token after busy: %d", res.StatusCode)
	}
	if res, _ := e.req("GET", u, s, nil); res.StatusCode != http.StatusForbidden {
		t.Fatalf("a used token must not work again: %d", res.StatusCode)
	}
	if n := e.auditCount("restore.download", rj.ID); n != 1 {
		t.Fatalf("%d restore.download audit rows, want 1", n)
	}
}

// logWatch is a log sink that signals when a line containing want is written.
type logWatch struct {
	want string
	hit  chan struct{}
	once sync.Once
}

func (l *logWatch) Write(p []byte) (int, error) {
	if strings.Contains(string(p), l.want) {
		l.once.Do(func() { close(l.hit) })
	}
	return len(p), nil
}

// watchLog sends the restore service's log through a logWatch for want.
func (e *env) watchLog(want string) <-chan struct{} {
	l := &logWatch{want: want, hit: make(chan struct{})}
	e.rest.Log = logx.New(l, logx.LevelDebug)
	return l.hit
}

// stuckBackend never writes: restic that hangs before its first byte.
type stuckBackend struct{ *fixtures.Backend }

func (b *stuckBackend) Dump(ctx context.Context, _ adminpath.DumpRequest, _ io.Writer) (adminpath.DumpResult, error) {
	<-ctx.Done()
	return adminpath.DumpResult{}, ctx.Err()
}

// TestNoFirstByteIsAProblemAnswer (M1-HIPAA2 follow-up 2): when no byte comes
// within the first-byte bound the stream is cancelled, and since no header
// went out the browser gets a 503 problem with Retry-After, not a cut
// transfer; the restore records no_data and is audited.
func TestNoFirstByteIsAProblemAnswer(t *testing.T) {
	e := newEnv(t)
	s := e.login("admin", "pw-for-tests-1")
	e.rest.Backend = &stuckBackend{Backend: e.be}
	e.rest.FirstByteTimeout = 200 * time.Millisecond
	rj := e.newRestore(s, hipaa2Path)
	u, _ := e.mint(s, rj.ID)
	req, _ := http.NewRequest("GET", e.srv.URL+u, nil)
	for _, c := range s.cookies {
		req.AddCookie(c)
	}
	// Bounded: without the first-byte bound the request would never end.
	res, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("no first byte: no answer: %v", err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable || problemCode(t, res, b) != api.CodeUnavailable || res.Header.Get(api.HeaderRetryAfter) == "" {
		t.Fatalf("no first byte: %d %s", res.StatusCode, b)
	}
	cur, _ := e.st.GetRestoreRequest(context.Background(), store.ScopeAll(), rj.ID)
	if cur.ErrorClass != "no_data" || cur.Status != "ready" || e.auditCount("restore.failed", rj.ID) != 1 {
		t.Fatalf("after no first byte: %+v", cur)
	}
}

// floodBackend writes as fast as the destination takes it until a write fails
// or ctx ends.
type floodBackend struct{ *fixtures.Backend }

func (b *floodBackend) Dump(ctx context.Context, _ adminpath.DumpRequest, w io.Writer) (adminpath.DumpResult, error) {
	buf := bytes.Repeat([]byte{'z'}, 64<<10)
	for ctx.Err() == nil {
		if _, err := w.Write(buf); err != nil {
			return adminpath.DumpResult{}, adminpath.ErrStreamWrite
		}
	}
	return adminpath.DumpResult{}, ctx.Err()
}

// TestBrowserThatStopsReadingIsCutAsTooSlow (M1-HIPAA2 item 1): a browser
// that stops reading blocks the stream's write; the minimum-rate guard aborts
// it within its window (not after the two-minute per-write stall), the
// response is cut and the restore records too_slow with a restore.failed
// audit row.
func TestBrowserThatStopsReadingIsCutAsTooSlow(t *testing.T) {
	e := newEnv(t)
	s := e.login("admin", "pw-for-tests-1")
	e.rest.Backend = &floodBackend{Backend: e.be}
	e.rest.MinRateBytes, e.rest.MinRateWindow = 1<<40, 300*time.Millisecond // nothing meets it: every stream trips
	failedLog := e.watchLog("restore: download failed")
	rj := e.newRestore(s, "/C/Users/Public/Documents")
	u, _ := e.mint(s, rj.ID)
	req, _ := http.NewRequest("GET", e.srv.URL+u, nil)
	for _, c := range s.cookies {
		req.AddCookie(c)
	}
	res, err := e.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	// Do not read: the server's writes block once the socket buffers are full.
	// The signal is the service's "download failed" line, logged after the
	// audit row and the row update.
	select {
	case <-failedLog:
	case <-time.After(60 * time.Second):
		t.Fatal("the stalled stream was not cut within 60 s")
	}
	cur, _ := e.st.GetRestoreRequest(context.Background(), store.ScopeAll(), rj.ID)
	if cur.ErrorClass != "too_slow" || cur.Status != "ready" || cur.DownloadCount != 0 {
		t.Fatalf("a stalled browser: %+v", cur)
	}
	if e.auditCount("restore.failed", rj.ID) != 1 {
		t.Fatal("no restore.failed audit row")
	}
	if _, err := io.Copy(io.Discard, res.Body); err == nil {
		t.Fatal("an aborted stream ended like a complete download")
	}
	if !strings.Contains(res.Header.Get("Content-Type"), "zip") {
		t.Fatalf("the stream had started: %v", res.Header)
	}
}
