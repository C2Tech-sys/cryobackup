package ui

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// headerBackground is auth.HeaderBackground, spelled out: fresh.js sends these bytes.
const headerBackground = "X-Cryo-Background"

// advance moves the env's clock with an open stream reading it.
func (e *env) advance(d time.Duration) {
	e.nowMu.Lock()
	defer e.nowMu.Unlock()
	e.now = e.now.Add(d)
}

// uiStream is an open /ui/events, read to its end in the background.
type uiStream struct {
	ended chan struct{}
	rest  bytes.Buffer // what arrived after the first ping; read it only after ended
}

// openUIStream opens /ui/events as an EventSource does, with the env's session,
// and reads up to the first ping, so the stream is subscribed before the test acts.
func openUIStream(t *testing.T, e *env) *uiStream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	r, _ := http.NewRequestWithContext(ctx, "GET", e.srv.URL+"/ui/events", nil)
	r.Header.Set("Accept", "text/event-stream")
	for _, c := range e.sess {
		r.AddCookie(c)
	}
	res, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = res.Body.Close() })
	if res.StatusCode != http.StatusOK {
		t.Fatalf("stream answered %d", res.StatusCode)
	}
	br := bufio.NewReader(res.Body)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("stream ended before its first ping: %v", err)
		}
		if strings.HasPrefix(line, "event: ping") {
			break
		}
	}
	st := &uiStream{ended: make(chan struct{})}
	go func() {
		defer close(st.ended)
		_, _ = io.Copy(&st.rest, br)
	}()
	return st
}

func streamStaysOpen(t *testing.T, st *uiStream, what string) {
	t.Helper()
	select {
	case <-st.ended:
		t.Fatalf("the stream ended %s", what)
	case <-time.After(300 * time.Millisecond):
	}
}

func streamEnds(t *testing.T, e *env, st *uiStream, what string) {
	t.Helper()
	e.hub.broadcast(Event{Name: "ping"}) // one ping interval
	select {
	case <-st.ended:
	case <-time.After(5 * time.Second):
		t.Fatalf("the stream was still open one ping after %s", what)
	}
}

// M3-HARDEN A2: a signed-out session loses its stream within one ping interval, and
// the page is told, so it can leave instead of going quietly stale.
func TestUIEventStreamEndsOnePingAfterLogout(t *testing.T) {
	e := newEnv(t)
	e.login()
	st := openUIStream(t, e)
	e.hub.broadcast(Event{Name: "ping"})
	streamStaysOpen(t, st, "on a ping with a live session")
	if res, _ := e.post("/logout", url.Values{}, false); res.StatusCode >= 400 {
		t.Fatalf("logout answered %d", res.StatusCode)
	}
	streamStaysOpen(t, st, "before the next ping") // the check is the ping's, not the logout's
	streamEnds(t, e, st, "logout")
	if !strings.Contains(st.rest.String(), "event: session.ended") {
		t.Errorf("the stream ended without saying so: %q", st.rest.String())
	}
}

// M3-HARDEN A2: the same for a disabled account (sessions deleted with it).
func TestUIEventStreamEndsOnePingAfterTheAccountIsDisabled(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.signInAs("leaving", "admin", nil)
	st := openUIStream(t, e)
	if err := e.st.DisableAdmin(context.Background(), e.adminID("leaving")); err != nil {
		t.Fatal(err)
	}
	streamEnds(t, e, st, "the account was disabled")
}

// M3-HARDEN A2: a narrowed account does not keep reading what it could before: the
// stream ends quietly and the browser's reconnect subscribes with the current scope.
func TestUIEventStreamEndsWhenTheScopeNarrows(t *testing.T) {
	e := newEnv(t)
	e.login()
	cust := e.customerID("Summit Eye Care")
	e.signInAs("narrowed", "operator", nil)
	st := openUIStream(t, e)
	e.hub.broadcast(Event{Name: "ping"})
	streamStaysOpen(t, st, "on a ping with nothing changed")
	id := e.adminID("narrowed")
	err := e.st.Write(context.Background(), func(tx *store.Tx) error {
		_, _, err := tx.UpdateAdminRoleScope(id, "operator", []string{cust})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	streamEnds(t, e, st, "the scope narrowed")
	if strings.Contains(st.rest.String(), "session.ended") {
		t.Errorf("a live session was told it ended: %q", st.rest.String())
	}
}

// M3-HARDEN A2: a database that cannot answer is no verdict on the credential.
func TestUIEventStreamStaysWhenTheCheckCannotRun(t *testing.T) {
	e := newEnv(t)
	e.login()
	st := openUIStream(t, e)
	if err := e.st.Close(); err != nil {
		t.Fatal(err)
	}
	e.hub.broadcast(Event{Name: "ping"})
	streamStaysOpen(t, st, "because the check could not run")
}

// M3-HARDEN A2 (security review): the stream's own check does not count as activity.
// An open tab with nothing but its stream reaches the idle timeout: pings keep coming,
// the session expires on time, the stream ends and the page is told.
func TestUIEventStreamDoesNotKeepAnIdleSessionAlive(t *testing.T) {
	e := newEnv(t)
	e.login()
	st := openUIStream(t, e)
	step := e.svc.Auth.Idle / 4
	for i := 0; i < 3; i++ {
		e.advance(step)
		e.hub.broadcast(Event{Name: "ping"})
		streamStaysOpen(t, st, "before the idle timeout")
	}
	e.advance(step + time.Minute)
	streamEnds(t, e, st, "the idle timeout passed")
	if !strings.Contains(st.rest.String(), "event: session.ended") {
		t.Errorf("the stream ended without saying so: %q", st.rest.String())
	}
	res, _ := e.do("GET", "/", nil)
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/login" {
		t.Fatalf("the session outlived its idle timeout: GET / answered %d, Location %q", res.StatusCode, res.Header.Get("Location"))
	}
}

// M3-HARDEN A2 (security review): reconnecting a stream does not slide the session
// either: an EventSource retries on its own for as long as the page is open.
func TestUIEventStreamReconnectDoesNotKeepAnIdleSessionAlive(t *testing.T) {
	e := newEnv(t)
	e.login()
	step := e.svc.Auth.Idle / 4
	for i := 0; i < 3; i++ {
		e.advance(step)
		openUIStream(t, e)
	}
	e.advance(step + time.Minute)
	r, _ := e.do("GET", "/ui/events", nil, "Accept", "text/event-stream")
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a stream reconnecting past the idle timeout answered %d, want 401", r.StatusCode)
	}
}

// M3-HARDEN (security review): the board's background refresh does not slide the
// session, and what comes back after the idle timeout moves the page to the sign-in
// page, which says why, instead of swapping that page into the board.
func TestABackgroundRefreshDoesNotKeepAnIdleSessionAlive(t *testing.T) {
	e := newEnv(t)
	e.login()
	idle := e.svc.Auth.Idle
	poll := func() *http.Response {
		res, _ := e.do("GET", "/ui/partials/board", nil, "HX-Request", "true", headerBackground, "1")
		return res
	}
	e.advance(idle - time.Minute)
	if res := poll(); res.StatusCode != http.StatusOK {
		t.Fatalf("a poll inside the idle timeout answered %d", res.StatusCode)
	}
	e.advance(2 * time.Minute)
	res := poll()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a poll past the idle timeout answered %d, want 401: polls keep the session alive", res.StatusCode)
	}
	if got := res.Header.Get("HX-Redirect"); got != "/login?ended=1" {
		t.Fatalf("HX-Redirect = %q, want the sign-in page with its notice", got)
	}
	if res.Header.Get("Location") != "" {
		t.Error("htmx would follow a redirect unseen and swap the sign-in page into the board")
	}
	_, body := e.do("GET", "/login?ended=1", nil)
	mustContain(t, "sign-in page after a session ended", body, `id="signin-ended"`, "You were signed out.")
}

// The control: what the operator does still counts. A request without the marker
// slides the session.
func TestAnOperatorRequestStillKeepsTheSessionAlive(t *testing.T) {
	e := newEnv(t)
	e.login()
	idle := e.svc.Auth.Idle
	e.advance(idle - time.Minute)
	if res, _ := e.do("GET", "/ui/partials/board", nil, "HX-Request", "true"); res.StatusCode != http.StatusOK {
		t.Fatalf("an unmarked request answered %d", res.StatusCode)
	}
	e.advance(2 * time.Minute)
	if res, _ := e.do("GET", "/ui/partials/board", nil, "HX-Request", "true"); res.StatusCode != http.StatusOK {
		t.Fatalf("the session did not slide on an unmarked request: %d", res.StatusCode)
	}
}

// The notice is there only when asked for and repeats nothing from the query.
func TestSignInPageSaysWhyOnlyAfterASessionEnded(t *testing.T) {
	e := newEnv(t)
	_, plain := e.do("GET", "/login", nil)
	if strings.Contains(plain, "signin-ended") {
		t.Error("the plain sign-in page says a session ended")
	}
	_, other := e.do("GET", "/login?ended=%3Cscript%3E", nil)
	if strings.Contains(other, "signin-ended") || strings.Contains(other, "<script>alert") {
		t.Error("the notice answers to any value")
	}
}
