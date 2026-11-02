package ui

import (
	"net/http"
	"net/url"
	"testing"
)

// TestUIPasswordAloneIsNotALogin uses only the M0 routes and helpers, so it
// also runs against the code before M2-W1A, where it fails: there the
// password alone opened the board.
func TestUIPasswordAloneIsNotALogin(t *testing.T) {
	e := newEnv(t)
	res, body := e.do("POST", "/login", url.Values{"username": {"admin"}, "password": {testPassword}})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("password step: %d %s", res.StatusCode, body)
	}
	e.sess = res.Cookies()
	for _, p := range []string{"/", "/devices/" + e.dev["DR-OFFICE-PC"], "/ui/partials/board"} {
		res, _ := e.do("GET", p, nil)
		if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/login/mfa" {
			t.Errorf("%s with a password-only session: %d %q, want 303 to /login/mfa", p, res.StatusCode, res.Header.Get("Location"))
		}
	}
}
