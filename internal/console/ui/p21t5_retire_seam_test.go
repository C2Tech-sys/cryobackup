package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/jobs"
)

// P21-T5 fix batch 1. The pages' retire goes through the UI's own in-process admin API, so its wait
// is the UI service's GatewaySyncWait (S1), and a test that needs the wait to end at a chosen read
// cannot rely on a timer: under load the timer ends the wait before the first read has finished.

// postFormEndable posts a form to the pages in process, as the logged-in operator, on a request
// whose context the caller owns, so a test can end a retire's wait without a timer. It returns the
// recorded answer; the redirect is the answer's Location.
func (e *env) postFormEndable(ctx context.Context, path string, form url.Values) *httptest.ResponseRecorder {
	e.t.Helper()
	form.Set("csrf_token", e.csrf)
	req := httptest.NewRequest(http.MethodPost, e.srv.URL+path, strings.NewReader(form.Encode())).WithContext(ctx)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", e.srv.URL)
	for _, c := range e.sess {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	e.srv.Config.Handler.ServeHTTP(rec, req)
	return rec
}

// readsThenEnd makes the in-process API's reads of the gateway's file the first read of the test's
// e.loginFile, then one that ends the wait (end) and never returns while the test runs: it is the
// read in flight when the wait ends. n counts the reads that began. The returned func puts the
// API's own check back; call it once the retire has answered, so the pages read the file as before.
func (e *env) readsThenEnd(end context.CancelFunc, n *atomic.Int32) (restore func()) {
	e.t.Helper()
	release := make(chan struct{})
	e.t.Cleanup(func() { close(release) })
	orig := e.svc.m3.api.CheckGatewayLogins
	e.svc.m3.api.CheckGatewayLogins = func(context.Context) jobs.LoginFileCheck {
		if n.Add(1) == 1 {
			return e.loginFile()
		}
		end()
		<-release
		return jobs.LoginFileCheck{}
	}
	return func() { e.svc.m3.api.CheckGatewayLogins = orig }
}

// S1: ui.Service.GatewaySyncWait reaches the in-process admin API, and unset keeps the default.
func TestGatewaySyncWaitReachesThePagesInProcessAPI(t *testing.T) {
	t.Parallel()
	mk := func(wait time.Duration) *Service {
		e := newEnv(t)
		svc, err := New(&Service{Auth: e.svc.Auth, Store: e.st, Fleet: e.svc.Fleet, Clock: e.svc.Clock, Fixtures: true, GatewaySyncWait: wait})
		if err != nil {
			t.Fatal(err)
		}
		return svc
	}
	if got := mk(37 * time.Second).m3.api.GatewaySyncWait; got != 37*time.Second {
		t.Errorf("set: the in-process API has a wait of %v, want 37s", got)
	}
	// Zero is carried as zero, and the API reads zero as DefaultGatewaySyncWait (pinned in adminapi).
	if got := mk(0).m3.api.GatewaySyncWait; got != 0 {
		t.Errorf("unset: the in-process API has a wait of %v, want 0 (the default)", got)
	}
}
