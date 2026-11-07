package adminapi

import (
	"testing"
	"time"
)

// M3-HARDEN A2 (security review): the admin stream's credential check does not count
// as activity for a cookie session. With checks running, the session expires at its
// idle timeout, the stream ends and the next request answers 401.
func TestM3EventStreamDoesNotKeepAnIdleSessionAlive(t *testing.T) {
	e := newEnv(t)
	e.svc.sseRefresh, e.svc.sseRevalidate = time.Hour, 20*time.Millisecond
	s := e.login("admin", pw)
	st, res := e.openStream("/api/v1/events?topics=fleet", s, "Accept", "text/event-stream")
	if res.StatusCode != 200 {
		t.Fatalf("stream answered %d", res.StatusCode)
	}
	st.next("fleet.state", 5*time.Second)
	step := e.svc.Auth.Idle / 4
	for i := 0; i < 3; i++ {
		e.clk.Advance(step)
		if st.closed(200 * time.Millisecond) {
			t.Fatal("the stream ended before the idle timeout")
		}
	}
	e.clk.Advance(step + time.Minute)
	if !st.closed(5 * time.Second) {
		t.Fatal("the stream outlived the session's idle timeout: its own check keeps the session alive")
	}
	if res, b := e.req("GET", "/api/v1/fleet/summary", s, nil); res.StatusCode != 401 {
		t.Fatalf("after the idle timeout the session answered %d: %s", res.StatusCode, b)
	}
}
