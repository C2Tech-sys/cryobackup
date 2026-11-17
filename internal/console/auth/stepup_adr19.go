package auth

// Fresh-MFA step-up (ADR 0019, "fresh MFA arrives with this ADR").
//
// An owner action that cannot be undone (purge of a retired device, delete of a
// customer) needs a TOTP check made on THIS session within the last
// FreshMFAWindow. The mark is sessions.mfa_verified_at: set when the login passes
// its MFA step (newFullSession), refreshed by a re-enrollment that verified a
// current code (ConfirmTOTP) and by StepUp. FreshMFA reads it, RequireFreshMFA
// gates a handler on it, StepUp renews it.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/console/httpx"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

const (
	// FreshMFAWindow is how long a TOTP check keeps a session "fresh". An action is
	// allowed while now minus the mark is at most this (and not negative).
	FreshMFAWindow = 5 * time.Minute

	// ActionStepUp is the audit action of a step-up attempt: result "ok", or "denied"
	// with a reason (the audit_log schema allows ok, denied and error only).
	ActionStepUp = "auth.stepup"

	// CodeStepUpRequired is the problem code RequireFreshMFA answers (HTTP 403): the
	// session is valid but its last TOTP check is older than FreshMFAWindow. The
	// client prompts for a code and calls StepUp (the UI dialog) before retrying.
	CodeStepUpRequired = "mfa_step_up_required"

	stepUpTitle = "A recent MFA check is required. Confirm with a current authenticator code."
)

// FreshMFA reports whether p holds a TOTP check made within FreshMFAWindow of now.
// Only a completed browser session can be fresh: an API token (ViaToken, no
// session) is never fresh, a pending session is not a login at all, and a session
// with no mark (written before MFA existed) or a mark in the future (clock moved
// back) is not fresh. Owner purge is session-only, so a token cannot hold the
// check however long it lives.
func (m *Manager) FreshMFA(p *Principal, now time.Time) bool {
	if p == nil || p.Pending || p.ViaToken() || p.SessionID == "" || p.MFAVerifiedAt == nil {
		return false
	}
	age := now.Sub(*p.MFAVerifiedAt)
	return age >= 0 && age <= FreshMFAWindow
}

// StepUp renews the fresh-MFA mark of p's session after a valid TOTP code (the
// code check is checkTOTP: the replay guard applies, so a code that already
// served a login, a step-up or any other check is refused, and the account
// lockout and the per-IP limiter apply as for every code check). On success the
// session's mark and p.MFAVerifiedAt are m.Now() and one audit row records it
// (ActionStepUp, result "ok", written in the same transaction as the mark: no
// audit row, no mark). A wrong, expired or replayed code is ErrBadCode with one
// audit row (result "denied", reason bad_code, locked_now or replayed_code) that
// counts toward the lockout; a locked account or a rate-limited source is refused
// by the shared preflight (one audit row, result "denied", reason locked or
// rate_limited). An API token is ErrSessionRequired, a pending or missing
// session ErrNoSession, an account without TOTP ErrNotEnrolled. A recovery code
// is not accepted here. userAgent is accepted for symmetry with the login calls
// and is not stored.
func (m *Manager) StepUp(ctx context.Context, p *Principal, code, ip, userAgent string) error {
	if p == nil || p.Pending {
		return ErrNoSession
	}
	if p.ViaToken() || p.SessionID == "" {
		return ErrSessionRequired
	}
	a, err := m.codePreflight(ctx, p, ActionStepUp, "web", ip)
	if err != nil {
		return err
	}
	if a.TOTPEnrolledAt == nil {
		return ErrNotEnrolled
	}
	now := m.Now()
	err = m.Store.Write(ctx, func(tx *store.Tx) error {
		if err := m.checkTOTP(tx, a, code); err != nil {
			return err
		}
		if err := tx.MarkSessionMFAAt(a.ID, p.SessionID, now); err != nil {
			return err
		}
		return auditTx(tx, Actor{Type: "admin", ID: a.ID, Label: a.Username, Method: "web", IP: ip}, ActionStepUp, "admin_session", a.ID, "ok", "info",
			map[string]any{"factor": "totp"})
	})
	switch {
	case isCodeFailure(err):
		m.stepUpFailure(ctx, p, failReason(err, "bad_code"), ip)
		return ErrBadCode
	case errors.Is(err, store.ErrNotFound):
		return ErrNoSession // the session was revoked or deleted under the request
	case err != nil:
		return err
	}
	p.MFAVerifiedAt = &now
	return nil
}

// stepUpFailure records a wrong code: the account's failure counter (which locks
// it after LockAfter, shared with the login) and one audit row, result "denied" (the schema has no "failed").
func (m *Manager) stepUpFailure(ctx context.Context, p *Principal, reason, ip string) {
	_ = m.Store.Write(ctx, func(tx *store.Tx) error {
		upd, err := tx.RecordLoginFailure(p.Admin.ID, m.LockAfter, m.LockFor)
		if err == nil && upd.LockedUntil != nil && upd.LockedUntil.After(m.Now()) && reason == "bad_code" {
			reason = "locked_now"
		}
		return auditTx(tx, Actor{Type: "admin", ID: p.Admin.ID, Label: p.Admin.Username, Method: "web", IP: ip},
			ActionStepUp, "admin_session", p.Admin.ID, "denied", "warning", map[string]any{"reason": reason})
	})
}

// RequireFreshMFA wraps next, which must already sit behind Require or
// RequireSession (they put the Principal in the context). When the principal is
// not fresh (FreshMFA with m.Now()) it answers 403 in the problem format
// (application/problem+json, code CodeStepUpRequired) and next does not run; no
// principal in the context is 401, so a route wired without a gate fails closed.
// The sentence is fixed and carries nothing of the request.
func (m *Manager) RequireFreshMFA(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := FromContext(r.Context())
		if !ok || p == nil {
			httpx.Problem(w, r, api.CodeUnauthorized, 0)
			return
		}
		if !m.FreshMFA(p, m.Now()) {
			writeStepUpRequired(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// writeStepUpRequired writes the 403 problem document. The code is not in the api
// error table (httpx.Problem would title it "Unknown error."), so the document is
// built here with the same shape and headers httpx.ProblemStatus writes.
func writeStepUpRequired(w http.ResponseWriter, r *http.Request) {
	e := &api.Error{Type: api.ErrorTypePrefix + CodeStepUpRequired, Title: stepUpTitle, Status: http.StatusForbidden,
		Code: CodeStepUpRequired, RequestID: httpx.RequestID(r.Context())}
	h := w.Header()
	h.Set("Content-Type", api.ContentTypeProblem)
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(e.Status)
	_ = json.NewEncoder(w).Encode(e)
}
