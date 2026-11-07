package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

func TestIsBackground(t *testing.T) {
	for _, tc := range []struct {
		method, header, value string
		want                  bool
	}{
		{"GET", HeaderBackground, "1", true},
		{"HEAD", HeaderBackground, "1", true},
		{"GET", "Accept", "text/event-stream", true},
		{"GET", "", "", false},
		{"GET", "Accept", "text/html", false},
		{"GET", "HX-Request", "true", false}, // a filter or a page link is the operator
		{"POST", HeaderBackground, "1", false},
		{"POST", "Accept", "text/event-stream", false},
		{"DELETE", HeaderBackground, "1", false},
	} {
		r := httptest.NewRequest(tc.method, "/x", nil)
		if tc.header != "" {
			r.Header.Set(tc.header, tc.value)
		}
		if got := IsBackground(r); got != tc.want {
			t.Errorf("%s with %s: %q = %v, want %v", tc.method, tc.header, tc.value, got, tc.want)
		}
	}
}

// CheckSession gives Authenticate's verdicts and moves nothing: not last_seen_at, not
// the idle expiry, however late it is asked.
func TestCheckSessionGivesTheSameVerdictsAndWritesNothing(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a := e.addAdmin(t, "ann", "owner", "right horse")
	pending, err := e.m.Login(ctx, "ann", "right horse", "203.0.113.5", "UA")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.CheckSession(ctx, pending.Token); !errors.Is(err, ErrMFAPending) {
		t.Fatalf("a pending session: %v", err)
	}
	if _, err := e.m.CheckSession(ctx, "not-a-session"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("an unknown session: %v", err)
	}
	res := e.mfa.complete(t, pending)
	before, err := e.st.GetSessionByHash(ctx, hashToken(res.Token))
	if err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(11 * time.Hour)
	p, err := e.m.CheckSession(ctx, res.Token)
	if err != nil || p.Admin.ID != a.ID || p.Role != RoleOwner {
		t.Fatalf("a live session: %v %+v", err, p)
	}
	after, err := e.st.GetSessionByHash(ctx, hashToken(res.Token))
	if err != nil {
		t.Fatal(err)
	}
	if !after.ExpiresAt.Equal(before.ExpiresAt) || !after.LastSeenAt.Equal(before.LastSeenAt) {
		t.Fatalf("CheckSession moved the session: expires %v to %v, last seen %v to %v", before.ExpiresAt, after.ExpiresAt, before.LastSeenAt, after.LastSeenAt)
	}
	e.clk.Advance(2 * time.Hour)
	if _, err := e.m.CheckSession(ctx, res.Token); !errors.Is(err, ErrNoSession) {
		t.Fatalf("past the idle timeout: %v", err)
	}

	res = login(t, e, "ann", "right horse")
	if err := e.st.Write(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE admin_users SET disabled_at = ? WHERE id = ?`, e.clk.Now().UnixMilli(), a.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.CheckSession(ctx, res.Token); !errors.Is(err, ErrNoSession) {
		t.Fatalf("a disabled account: %v", err)
	}
}

// The gate: a marked read does not slide, an unmarked one does. After expiry a UI
// partial and an event stream get a refusal that moves the page, and a plain page
// load keeps its redirect.
func TestGateBackgroundReadsDoNotSlideAndTheDeadSessionAnswer(t *testing.T) {
	e := newEnv(t)
	e.addAdmin(t, "ann", "owner", "right horse")
	h := e.m.Require(RoleReadOnly, ModeUI, okHandler())
	do := func(res LoginResult, hdr ...string) *httptest.ResponseRecorder {
		r := req("GET", "/x", &res)
		for i := 0; i+1 < len(hdr); i += 2 {
			r.Header.Set(hdr[i], hdr[i+1])
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	res := login(t, e, "ann", "right horse")
	e.clk.Advance(11 * time.Hour)
	if w := do(res, HeaderBackground, "1"); w.Code != http.StatusOK {
		t.Fatalf("a marked read inside the timeout: %d", w.Code)
	}
	e.clk.Advance(2 * time.Hour)
	if w := do(res, "HX-Request", "true", HeaderBackground, "1"); w.Code != http.StatusUnauthorized || w.Header().Get("HX-Redirect") != SessionEndedPath {
		t.Fatalf("a marked read kept the session alive or got no way out: %d, HX-Redirect %q", w.Code, w.Header().Get("HX-Redirect"))
	}
	if w := do(res, "Accept", "text/event-stream"); w.Code != http.StatusUnauthorized {
		t.Fatalf("an event stream past the timeout: %d", w.Code)
	}
	if w := do(res); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login" {
		t.Fatalf("a page load past the timeout: %d, Location %q", w.Code, w.Header().Get("Location"))
	}

	res = login(t, e, "ann", "right horse")
	e.clk.Advance(11 * time.Hour)
	if w := do(res); w.Code != http.StatusOK {
		t.Fatalf("an unmarked read: %d", w.Code)
	}
	e.clk.Advance(2 * time.Hour)
	if w := do(res); w.Code != http.StatusOK {
		t.Fatalf("an unmarked read did not slide the session: %d", w.Code)
	}
}
