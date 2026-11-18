package auth

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"
)

// A recovery code never makes a session fresh (ticket P19-F1, ADR 0019: a TOTP
// check within FreshMFAWindow; StepUp refuses recovery codes).

// adr19RecoveryLogin logs the owner in again with a recovery code and returns the
// new session's principal as the gate would load it.
func adr19RecoveryLogin(t *testing.T, e *env, code string) (*Principal, LoginResult) {
	t.Helper()
	ctx := context.Background()
	pend, _ := pendingOf(t, e, "ow", "right horse")
	res, _, err := e.m.VerifyRecoveryLogin(ctx, pend, code, "web", adr19IP, "")
	if err != nil {
		t.Fatalf("recovery login: %v", err)
	}
	p, err := e.m.Authenticate(ctx, res.Token)
	if err != nil || p.Pending {
		t.Fatalf("a recovery login must be a completed login: %+v, %v", p, err)
	}
	return p, res
}

// A recovery-code login is a completed session (Authenticate, the session list
// and StepUp all accept it) but not fresh, on the principal and on the row, and
// RequireFreshMFA answers 403; a StepUp with a TOTP code then makes it fresh.
func TestAdr19RecoveryLoginIsNotFresh(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	adr19Owner(t, e)
	p, res := adr19RecoveryLogin(t, e, e.mfa.codes["ow"][0])

	if e.m.FreshMFA(p, e.m.Now()) {
		t.Fatal("a session that passed MFA with a recovery code must not be fresh")
	}
	if m := adr19Mark(t, e, res.Token); m == nil || !m.Equal(mfaMarkNotFresh) {
		t.Fatalf("mfa_verified_at = %v, want the not-fresh mark %v (present, never fresh)", m, mfaMarkNotFresh)
	}
	sessions, err := e.m.ListSessions(ctx, p.Admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	listed := false
	for _, s := range sessions {
		listed = listed || s.ID == p.SessionID
	}
	if !listed {
		t.Fatal("the recovery login is missing from the account's session list")
	}

	h := e.m.Require(RoleOwner, ModeAPI, e.m.RequireFreshMFA(okHandler()))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req("GET", "/api/v1/x", &res))
	if w.Code != 403 {
		t.Fatalf("a recovery login behind RequireFreshMFA: %d %s, want 403", w.Code, w.Body)
	}
	// Not fresh at any time a working clock could show, one moved back a year included.
	for _, at := range []time.Time{e.m.Now(), e.m.Now().Add(-365 * 24 * time.Hour), time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)} {
		if e.m.FreshMFA(p, at) {
			t.Errorf("fresh at %v", at)
		}
	}

	if err := e.m.StepUp(ctx, p, TOTPCode(e.mfa.secrets["ow"], e.mfa.next()), adr19IP, ""); err != nil {
		t.Fatalf("StepUp after a recovery login: %v", err)
	}
	again, _ := e.m.Authenticate(ctx, res.Token)
	if !e.m.FreshMFA(again, e.m.Now()) {
		t.Fatal("a TOTP step-up must make the recovery-code session fresh")
	}
	if m := adr19Mark(t, e, res.Token); m == nil || !m.Equal(e.clk.Now()) {
		t.Fatalf("mfa_verified_at after the step-up = %v, want %v", m, e.clk.Now())
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req("GET", "/api/v1/x", &res))
	if w.Code != 200 {
		t.Fatalf("after the step-up: %d %s", w.Code, w.Body)
	}
}

// A re-enrollment authorised by a recovery code: the recovery code alone leaves
// the session stale (a confirm whose new-secret code is wrong changes nothing);
// the confirm verifies a code of the new secret, a TOTP check, so the session is
// fresh only once that confirm succeeds.
func TestAdr19RecoveryReenrollIsFreshOnlyAfterConfirm(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	adr19Owner(t, e)
	p, res := adr19RecoveryLogin(t, e, e.mfa.codes["ow"][0])
	recovery := e.mfa.codes["ow"][1]

	en, err := e.m.BeginTOTP(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := e.m.Authenticate(ctx, res.Token); e.m.FreshMFA(again, e.m.Now()) {
		t.Fatal("beginning a re-enrollment made the session fresh")
	}
	if _, _, err := e.m.ConfirmTOTP(ctx, p, "000000", recovery, "web", adr19IP, ""); !errors.Is(err, ErrBadCode) {
		t.Fatalf("a confirm with a wrong new-secret code: %v, want ErrBadCode", err)
	}
	if again, _ := e.m.Authenticate(ctx, res.Token); e.m.FreshMFA(again, e.m.Now()) {
		t.Fatal("a recovery code with a failed confirm made the session fresh")
	}
	if m := adr19Mark(t, e, res.Token); m == nil || !m.Equal(mfaMarkNotFresh) {
		t.Fatalf("mark after a failed confirm = %v", m)
	}

	if _, _, err := e.m.ConfirmTOTP(ctx, p, TOTPCode(secretOf(t, en.Secret), e.mfa.next()), recovery, "web", adr19IP, ""); err != nil {
		t.Fatalf("re-enroll authorised by a recovery code: %v", err)
	}
	again, _ := e.m.Authenticate(ctx, res.Token)
	if !e.m.FreshMFA(again, e.m.Now()) {
		t.Fatal("the confirm verified a TOTP code of the new secret: the session must be fresh after it")
	}
	if m := adr19Mark(t, e, res.Token); m == nil || !m.Equal(e.clk.Now()) {
		t.Fatalf("mark after the confirm = %v, want %v", m, e.clk.Now())
	}
}

// A TOTP login is still fresh at once (the not-fresh mark is the recovery path's
// only).
func TestAdr19TOTPLoginStaysFresh(t *testing.T) {
	e := newEnv(t)
	adr19Owner(t, e)
	pend, _ := pendingOf(t, e, "ow", "right horse")
	res, err := e.m.VerifyTOTPLogin(context.Background(), pend, TOTPCode(e.mfa.secrets["ow"], e.mfa.next()), "web", adr19IP, "")
	if err != nil {
		t.Fatal(err)
	}
	p, err := e.m.Authenticate(context.Background(), res.Token)
	if err != nil || !e.m.FreshMFA(p, e.m.Now()) {
		t.Fatalf("a TOTP login must be fresh: %v", err)
	}
	if m := adr19Mark(t, e, res.Token); m == nil || !m.Equal(e.clk.Now()) {
		t.Fatalf("mark = %v, want the login time", m)
	}
}
