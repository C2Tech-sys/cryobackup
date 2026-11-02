package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestPasswordAloneIsNotALogin uses only the M0 API surface (Login,
// Authenticate, Require), so it also runs against the code before M2-W1A,
// where it fails: there the password alone was a complete login.
func TestPasswordAloneIsNotALogin(t *testing.T) {
	e := newEnv(t)
	e.addAdmin(t, "pw", "owner", "right horse")
	res, err := e.m.Login(context.Background(), "pw", "right horse", "192.0.2.1", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.Authenticate(context.Background(), res.Token); err == nil {
		t.Fatal("the password step alone authenticated a session")
	}
	w := httptest.NewRecorder()
	e.m.Require(RoleReadOnly, ModeAPI, okHandler()).ServeHTTP(w, req("GET", "/api/v1/version", &res))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("an API route answered %d to a password-only session, want 401", w.Code)
	}
}
