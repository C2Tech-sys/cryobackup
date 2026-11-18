package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

const adr19IP = "192.0.2.1"

// adr19Owner logs an owner in (enrolling TOTP on the way) and returns the
// session's principal as the gate would load it.
func adr19Owner(t *testing.T, e *env) (*Principal, LoginResult) {
	t.Helper()
	e.addAdmin(t, "ow", "owner", "right horse")
	res := login(t, e, "ow", "right horse")
	p, err := e.m.Authenticate(context.Background(), res.Token)
	if err != nil {
		t.Fatal(err)
	}
	return p, res
}

func adr19Mark(t *testing.T, e *env, token string) *time.Time {
	t.Helper()
	s, err := e.st.GetSessionByHash(context.Background(), hashToken(token))
	if err != nil {
		t.Fatal(err)
	}
	return s.MFAVerifiedAt
}

func adr19Rows(t *testing.T, e *env, result string) []store.AuditEvent {
	t.Helper()
	var out []store.AuditEvent
	for _, ev := range allAudit(t, e.st) {
		if ev.Action == ActionStepUp && ev.Result == result {
			out = append(out, ev)
		}
	}
	return out
}

// A login that passed TOTP is fresh, and the mark is on the session row.
func TestAdr19FreshMFAAfterLogin(t *testing.T) {
	e := newEnv(t)
	p, res := adr19Owner(t, e)
	now := e.clk.Now()
	if !e.m.FreshMFA(p, now) {
		t.Fatal("a session that has just passed TOTP must be fresh")
	}
	if m := adr19Mark(t, e, res.Token); m == nil || !m.Equal(now) {
		t.Fatalf("mfa_verified_at = %v, want the login time %v", m, now)
	}
}

// Fresh up to and including FreshMFAWindow, stale after it; a mark in the future
// (clock moved back) and a principal with no mark are not fresh.
func TestAdr19FreshMFAWindow(t *testing.T) {
	e := newEnv(t)
	p, res := adr19Owner(t, e)
	t0 := e.clk.Now()
	for _, c := range []struct {
		after time.Duration
		want  bool
	}{{0, true}, {4*time.Minute + 59*time.Second, true}, {FreshMFAWindow, true}, {FreshMFAWindow + time.Second, false}, {time.Hour, false}, {-time.Second, false}} {
		if got := e.m.FreshMFA(p, t0.Add(c.after)); got != c.want {
			t.Errorf("after %v: fresh = %v, want %v", c.after, got, c.want)
		}
	}
	if FreshMFAWindow != 5*time.Minute {
		t.Fatalf("the window is 5 minutes (ADR 0019), got %v", FreshMFAWindow)
	}
	// The same through the clock the Manager uses: advance it and load the session again.
	e.clk.Advance(FreshMFAWindow + time.Second)
	again, err := e.m.Authenticate(context.Background(), res.Token)
	if err != nil {
		t.Fatal(err)
	}
	if e.m.FreshMFA(again, e.m.Now()) {
		t.Fatal("a session reloaded after 5 minutes and a second must be stale")
	}
	if e.m.FreshMFA(nil, t0) || e.m.FreshMFA(&Principal{Admin: p.Admin, SessionID: p.SessionID}, t0) {
		t.Fatal("no principal, or one without a mark, is not fresh")
	}
}

// A stale session regains freshness with a valid code; the mark persists on the row,
// moves to the Manager's clock, and one "ok" audit row is written.
func TestAdr19StepUpRefreshes(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p, res := adr19Owner(t, e)
	e.clk.Advance(10 * time.Minute)
	p, err := e.m.Authenticate(ctx, res.Token)
	if err != nil || e.m.FreshMFA(p, e.m.Now()) {
		t.Fatalf("precondition: stale session, err %v", err)
	}
	if err := e.m.StepUp(ctx, p, TOTPCode(e.mfa.secrets["ow"], e.mfa.next()), adr19IP, "UA"); err != nil {
		t.Fatalf("StepUp: %v", err)
	}
	if !e.m.FreshMFA(p, e.m.Now()) {
		t.Fatal("the principal the handler holds must be fresh after StepUp")
	}
	again, _ := e.m.Authenticate(ctx, res.Token)
	if !e.m.FreshMFA(again, e.m.Now()) {
		t.Fatal("the mark must be on the session row, not only on the principal")
	}
	if m := adr19Mark(t, e, res.Token); m == nil || !m.Equal(e.clk.Now()) {
		t.Fatalf("mfa_verified_at = %v, want %v", m, e.clk.Now())
	}
	e.clk.Advance(FreshMFAWindow + time.Second)
	if e.m.FreshMFA(again, e.m.Now()) {
		t.Fatal("the refreshed mark expires after the window too")
	}
	ok := adr19Rows(t, e, "ok")
	if len(ok) != 1 || ok[0].ActorType != "admin" || ok[0].ActorID != p.Admin.ID || ok[0].ObjectType != "admin_session" ||
		ok[0].Severity != "info" || ok[0].SourceIP != adr19IP || strings.Contains(string(ok[0].Details), "UA") {
		t.Fatalf("ok audit rows: %+v", ok)
	}
	if n := len(adr19Rows(t, e, "denied")); n != 0 {
		t.Fatalf("%d failure rows after a good step-up", n)
	}
}

// A wrong code is ErrBadCode, leaves the session stale, writes a "denied" row (the schema has no "failed"), and
// counts toward the account lockout (LockAfter 3 in the test env), which then
// refuses even the right code.
func TestAdr19StepUpWrongCodeCountsTowardLockout(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p, res := adr19Owner(t, e)
	e.clk.Advance(10 * time.Minute)
	p, _ = e.m.Authenticate(ctx, res.Token)
	before := adr19Mark(t, e, res.Token)
	for i := 0; i < 3; i++ {
		if err := e.m.StepUp(ctx, p, "000000", adr19IP, ""); !errors.Is(err, ErrBadCode) {
			t.Fatalf("wrong code %d: %v", i, err)
		}
	}
	if rows := adr19Rows(t, e, "denied"); len(rows) != 3 {
		t.Fatalf("denied rows: %d, want 3", len(rows))
	} else if rows[0].Severity != "warning" || !strings.Contains(string(rows[0].Details), "bad_code") && !strings.Contains(string(rows[0].Details), "locked_now") {
		t.Fatalf("denied row: %+v %s", rows[0], rows[0].Details)
	}
	a, _ := e.st.GetAdmin(ctx, p.Admin.ID)
	if a.LockedUntil == nil || !a.LockedUntil.After(e.clk.Now()) {
		t.Fatalf("three wrong step-up codes must lock the account: %+v", a.LockedUntil)
	}
	right := TOTPCode(e.mfa.secrets["ow"], e.mfa.next())
	if err := e.m.StepUp(ctx, p, right, adr19IP, ""); !errors.Is(err, ErrBadCode) {
		t.Fatalf("a locked account must refuse the right code: %v", err)
	}
	if got := adr19Mark(t, e, res.Token); got == nil || !got.Equal(*before) {
		t.Fatalf("a refused step-up moved the mark: %v -> %v", before, got)
	}
	if e.m.FreshMFA(p, e.m.Now()) {
		t.Fatal("still stale")
	}
	if rows := adr19Rows(t, e, "denied"); len(rows) != 4 || countReason(rows, `"reason":"locked"`) != 1 || countReason(rows, "locked_now") != 1 {
		t.Fatalf("want 3 wrong-code rows (the third locked_now) and 1 locked refusal, got %+v", rows)
	}
	for _, ev := range allAudit(t, e.st) {
		if strings.Contains(string(ev.Details), "000000") || strings.Contains(string(ev.Details), right) {
			t.Fatalf("a code reached the audit trail: %s", ev.Details)
		}
	}
}

// A code that already served a step-up is not accepted twice (the replay guard of
// checkTOTP), even inside its window, and the replay is a "denied" row.
func TestAdr19StepUpRefusesReplay(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p, res := adr19Owner(t, e)
	e.clk.Advance(10 * time.Minute)
	p, _ = e.m.Authenticate(ctx, res.Token)
	code := TOTPCode(e.mfa.secrets["ow"], e.mfa.next())
	if err := e.m.StepUp(ctx, p, code, adr19IP, ""); err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(FreshMFAWindow + time.Second)
	again, _ := e.m.Authenticate(ctx, res.Token)
	marked := adr19Mark(t, e, res.Token)
	if err := e.m.StepUp(ctx, again, code, adr19IP, ""); !errors.Is(err, ErrBadCode) {
		t.Fatalf("a replayed code: %v, want ErrBadCode", err)
	}
	if got := adr19Mark(t, e, res.Token); !got.Equal(*marked) || e.m.FreshMFA(again, e.m.Now()) {
		t.Fatal("a replayed code refreshed the session")
	}
	rows := adr19Rows(t, e, "denied")
	if len(rows) != 1 || !strings.Contains(string(rows[0].Details), "replayed_code") {
		t.Fatalf("replay audit: %+v", rows)
	}
}

// A code of the wrong shape, and a recovery code, are not accepted.
func TestAdr19StepUpRefusesNonTOTPCodes(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p, _ := adr19Owner(t, e)
	for _, c := range []string{e.mfa.codes["ow"][0], "12345", "abcdefg"} {
		if err := e.m.StepUp(ctx, p, c, adr19IP, ""); !errors.Is(err, ErrBadCode) {
			t.Errorf("%q: %v", c, err)
		}
	}
}

// An API token is never fresh, whatever mark it carries, and cannot step up. A
// pending session and a missing principal cannot either.
func TestAdr19TokenPrincipalIsNeverFresh(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	sess, _ := adr19Owner(t, e)
	tok, _, err := e.m.CreateAPIToken(ctx, sess, "full owner", ScopeFull, 30, "web", "")
	if err != nil {
		t.Fatal(err)
	}
	tp, err := e.m.AuthenticateBearer(ctx, tok)
	if err != nil {
		t.Fatal(err)
	}
	now := e.clk.Now()
	tp.MFAVerifiedAt, tp.SessionID = &now, sess.SessionID // even a forged mark and session id do not make a token fresh
	if e.m.FreshMFA(tp, now) {
		t.Fatal("an API token must never be fresh")
	}
	if err := e.m.StepUp(ctx, tp, TOTPCode(e.mfa.secrets["ow"], e.mfa.next()), adr19IP, ""); !errors.Is(err, ErrSessionRequired) {
		t.Fatalf("StepUp with a token: %v, want ErrSessionRequired", err)
	}
	pend, _ := pendingOf(t, e, "ow", "right horse")
	if e.m.FreshMFA(pend, now) {
		t.Fatal("a pending session is not fresh")
	}
	if err := e.m.StepUp(ctx, pend, TOTPCode(e.mfa.secrets["ow"], e.mfa.next()), adr19IP, ""); !errors.Is(err, ErrNoSession) {
		t.Fatalf("StepUp with a pending session: %v, want ErrNoSession", err)
	}
	if err := e.m.StepUp(ctx, nil, "123456", adr19IP, ""); !errors.Is(err, ErrNoSession) {
		t.Fatalf("StepUp(nil): %v", err)
	}
	if n := len(allStepUpRows(t, e)); n != 0 {
		t.Fatalf("%d step-up audit rows for refused principals, want 0", n)
	}
}

func allStepUpRows(t *testing.T, e *env) []store.AuditEvent {
	t.Helper()
	var out []store.AuditEvent
	for _, ev := range allAudit(t, e.st) {
		if ev.Action == ActionStepUp {
			out = append(out, ev)
		}
	}
	return out
}

// An account without TOTP cannot step up (it cannot have a full session either, but
// the answer must be the specific one).
func TestAdr19StepUpNeedsEnrollment(t *testing.T) {
	e := newEnv(t)
	p, _ := adr19Owner(t, e)
	p.Admin.TOTPEnrolledAt = nil
	if err := e.st.Write(context.Background(), func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE admin_users SET totp_enrolled_at = NULL, totp_secret_wrapped = NULL WHERE id = ?`, p.Admin.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.m.StepUp(context.Background(), p, "123456", adr19IP, ""); !errors.Is(err, ErrNotEnrolled) {
		t.Fatalf("StepUp without enrollment: %v", err)
	}
}

// A re-enrollment confirmed with a current code is a verified code on a full session
// and refreshes the mark.
func TestAdr19ReenrollRefreshesTheMark(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p, res := adr19Owner(t, e)
	e.clk.Advance(10 * time.Minute)
	p, _ = e.m.Authenticate(ctx, res.Token)
	if e.m.FreshMFA(p, e.m.Now()) {
		t.Fatal("precondition: stale")
	}
	en, err := e.m.BeginTOTP(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	current := TOTPCode(e.mfa.secrets["ow"], e.mfa.next())
	if _, _, err := e.m.ConfirmTOTP(ctx, p, TOTPCode(secretOf(t, en.Secret), e.mfa.now()), current, "web", adr19IP, ""); err != nil {
		t.Fatalf("re-enroll: %v", err)
	}
	again, _ := e.m.Authenticate(ctx, res.Token)
	if !e.m.FreshMFA(again, e.m.Now()) {
		t.Fatal("a re-enrollment that verified a current code must refresh the session's mark")
	}
}

// The store stamp refreshes a completed session of the account and nothing else: not a
// pending session, not another account's session, not an unknown id.
func TestAdr19MarkSessionMFAAtGuards(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p, res := adr19Owner(t, e)
	other := e.addAdmin(t, "op", "operator", "right horse")
	pend, pres := pendingOf(t, e, "op", "right horse")
	at := e.clk.Now().Add(time.Minute)
	mark := func(adminID, id string) error {
		return e.st.Write(ctx, func(tx *store.Tx) error { return tx.MarkSessionMFAAt(adminID, id, at) })
	}
	if err := mark(other.ID, pend.SessionID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("pending session: %v, want ErrNotFound", err)
	}
	if adr19Mark(t, e, pres.Token) != nil {
		t.Error("a pending session got a mark")
	}
	if err := mark(other.ID, p.SessionID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("another account's session: %v, want ErrNotFound", err)
	}
	if err := mark(p.Admin.ID, "no-such-session"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown session: %v, want ErrNotFound", err)
	}
	if err := mark(p.Admin.ID, p.SessionID); err != nil {
		t.Fatal(err)
	}
	if m := adr19Mark(t, e, res.Token); m == nil || !m.Equal(at) {
		t.Fatalf("mark = %v, want %v", m, at)
	}
}

type adr19Problem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Code   string `json:"code"`
}

// RequireFreshMFA lets a fresh session through, answers 403 mfa_step_up_required (problem
// format) for a stale one and for an API token, and runs the handler again after a StepUp.
func TestAdr19RequireFreshMFA(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p, res := adr19Owner(t, e)
	tok, _, err := e.m.CreateAPIToken(ctx, p, "full owner", ScopeFull, 30, "web", "")
	if err != nil {
		t.Fatal(err)
	}
	h := e.m.Require(RoleOwner, ModeAPI, e.m.RequireFreshMFA(okHandler()))
	do := func(bearer string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		var r *http.Request
		if bearer != "" {
			r = req("GET", "/api/v1/x", nil)
			r.Header.Set("Authorization", "Bearer "+bearer)
		} else {
			r = req("GET", "/api/v1/x", &res)
		}
		h.ServeHTTP(w, r)
		return w
	}
	if w := do(""); w.Code != 200 || w.Body.String() != "hello ow" {
		t.Fatalf("fresh session: %d %s", w.Code, w.Body)
	}
	stale := func(name string, w *httptest.ResponseRecorder) {
		t.Helper()
		var pr adr19Problem
		if w.Code != 403 || w.Header().Get("Content-Type") != "application/problem+json" || w.Header().Get("Cache-Control") != "no-store" ||
			json.Unmarshal(w.Body.Bytes(), &pr) != nil || pr.Code != "mfa_step_up_required" || pr.Status != 403 ||
			pr.Type != "urn:cryobackup:error:mfa_step_up_required" || pr.Title == "" || strings.Contains(w.Body.String(), "hello") {
			t.Fatalf("%s: %d %s %s", name, w.Code, w.Header().Get("Content-Type"), w.Body)
		}
	}
	stale("api token", do(tok))
	e.clk.Advance(FreshMFAWindow + time.Second)
	stale("stale session", do(""))
	sp, _ := e.m.Authenticate(ctx, res.Token)
	if err := e.m.StepUp(ctx, sp, TOTPCode(e.mfa.secrets["ow"], e.mfa.next()), adr19IP, ""); err != nil {
		t.Fatal(err)
	}
	if w := do(""); w.Code != 200 {
		t.Fatalf("after step-up: %d %s", w.Code, w.Body)
	}
	// Wired without a gate in front: no principal in the context, 401, handler not run.
	w := httptest.NewRecorder()
	e.m.RequireFreshMFA(okHandler()).ServeHTTP(w, req("GET", "/api/v1/x", nil))
	if w.Code != 401 {
		t.Fatalf("no principal: %d", w.Code)
	}
}

func countReason(rows []store.AuditEvent, reason string) int {
	n := 0
	for _, r := range rows {
		if strings.Contains(string(r.Details), reason) {
			n++
		}
	}
	return n
}

// A step-up whose audit row cannot be written changes nothing (the transaction
// rolls back, mark and TOTP step included) and is NOT a wrong code: the caller
// must see a store failure, never ErrBadCode, and the right code stays usable.
func TestAdr19StepUpAuditFailureIsNotABadCode(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p, res := adr19Owner(t, e)
	e.clk.Advance(10 * time.Minute)
	p, _ = e.m.Authenticate(ctx, res.Token)
	before := adr19Mark(t, e, res.Token)
	if err := e.st.Write(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`CREATE TRIGGER refuse_audits BEFORE INSERT ON audit_events BEGIN SELECT RAISE(ABORT, 'audit refused by the test'); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	code := TOTPCode(e.mfa.secrets["ow"], e.mfa.next())
	err := e.m.StepUp(ctx, p, code, adr19IP, "")
	if err == nil || errors.Is(err, ErrBadCode) {
		t.Fatalf("audit failure: got %v, want a store failure that is not ErrBadCode", err)
	}
	if !errors.Is(err, store.ErrAudit) {
		t.Fatalf("audit failure must be store.ErrAudit, got %v", err)
	}
	if got := adr19Mark(t, e, res.Token); got == nil || !got.Equal(*before) || e.m.FreshMFA(p, e.m.Now()) {
		t.Fatalf("a refused step-up moved the mark: %v -> %v", before, got)
	}
	// Nothing was consumed: with the trigger gone the same code works.
	if err := e.st.Write(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`DROP TRIGGER refuse_audits`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.m.StepUp(ctx, p, code, adr19IP, ""); err != nil {
		t.Fatalf("the code must still be good after a rolled-back step-up: %v", err)
	}
}
