package ui

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// openUIStream opens /ui/events with the env's session and reads up to the first
// ping, so the stream is subscribed before the test acts on it. ended reports the
// body's end; it stays open while the stream does.
func openUIStream(t *testing.T, e *env) (ended <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	r, _ := http.NewRequestWithContext(ctx, "GET", e.srv.URL+"/ui/events", nil)
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
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(io.Discard, br)
	}()
	return done
}

func streamStaysOpen(t *testing.T, ended <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ended:
		t.Fatalf("the stream ended %s", what)
	case <-time.After(300 * time.Millisecond):
	}
}

func streamEnds(t *testing.T, e *env, ended <-chan struct{}, what string) {
	t.Helper()
	e.hub.broadcast(Event{Name: "ping"}) // one ping interval
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatalf("the stream was still open one ping after %s", what)
	}
}

// M3-HARDEN A2: a signed-out session loses its stream within one ping interval.
func TestUIEventStreamEndsOnePingAfterLogout(t *testing.T) {
	e := newEnv(t)
	e.login()
	ended := openUIStream(t, e)
	e.hub.broadcast(Event{Name: "ping"})
	streamStaysOpen(t, ended, "on a ping with a live session")
	if res, _ := e.post("/logout", url.Values{}, false); res.StatusCode >= 400 {
		t.Fatalf("logout answered %d", res.StatusCode)
	}
	streamStaysOpen(t, ended, "before the next ping") // the check is the ping's, not the logout's
	streamEnds(t, e, ended, "logout")
}

// M3-HARDEN A2: the same for a disabled account (sessions deleted with it).
func TestUIEventStreamEndsOnePingAfterTheAccountIsDisabled(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.signInAs("leaving", "admin", nil)
	ended := openUIStream(t, e)
	if err := e.st.DisableAdmin(context.Background(), e.adminID("leaving")); err != nil {
		t.Fatal(err)
	}
	streamEnds(t, e, ended, "the account was disabled")
}

// M3-HARDEN A2: a narrowed account does not keep reading what it could before: the
// stream ends and the browser's reconnect subscribes with the current scope.
func TestUIEventStreamEndsWhenTheScopeNarrows(t *testing.T) {
	e := newEnv(t)
	e.login()
	cust := e.customerID("Summit Eye Care")
	e.signInAs("narrowed", "operator", nil)
	ended := openUIStream(t, e)
	e.hub.broadcast(Event{Name: "ping"})
	streamStaysOpen(t, ended, "on a ping with nothing changed")
	id := e.adminID("narrowed")
	err := e.st.Write(context.Background(), func(tx *store.Tx) error {
		_, _, err := tx.UpdateAdminRoleScope(id, "operator", []string{cust})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	streamEnds(t, e, ended, "the scope narrowed")
}

// M3-HARDEN A2: a database that cannot answer is no verdict on the credential.
func TestUIEventStreamStaysWhenTheCheckCannotRun(t *testing.T) {
	e := newEnv(t)
	e.login()
	ended := openUIStream(t, e)
	if err := e.st.Close(); err != nil {
		t.Fatal(err)
	}
	e.hub.broadcast(Event{Name: "ping"})
	streamStaysOpen(t, ended, "because the check could not run")
}
