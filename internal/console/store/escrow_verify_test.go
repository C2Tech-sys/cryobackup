package store

import (
	"errors"
	"testing"
	"time"
)

// M2-FIX-B1 (lab finding F7): RecordEscrowVerification writes the outcome of a
// cat config with the escrowed key onto the active escrow row, and nothing else.
func TestRecordEscrowVerification(t *testing.T) {
	w := seed(t)
	id := w.devA.ID
	auditBefore := must[Listing[AuditEvent]](t)(w.s.ListAudit(w.ctx, ScopeAll(), AuditFilter{}, Page{Limit: 200}))

	// Could not verify: only the attempt is recorded.
	ok(t, w.s.RecordEscrowVerification(w.ctx, ScopeAll(), id, EscrowVerification{}))
	esc := must[Escrow](t)(w.s.ActiveEscrow(w.ctx, ScopeAll(), id))
	if esc.VerifiedAt != nil || esc.LastVerifyErrorClass != "" || esc.LastVerifyAttemptAt == nil || !esc.LastVerifyAttemptAt.Equal(t0) {
		t.Fatalf("after an attempt: %+v", esc)
	}
	// A verdict on the key.
	w.clk.Advance(time.Minute)
	ok(t, w.s.RecordEscrowVerification(w.ctx, ScopeAll(), id, EscrowVerification{ErrorClass: "wrong_password"}))
	esc = must[Escrow](t)(w.s.ActiveEscrow(w.ctx, ScopeAll(), id))
	if esc.VerifiedAt != nil || esc.LastVerifyErrorClass != "wrong_password" || !esc.LastVerifyAttemptAt.Equal(t0.Add(time.Minute)) {
		t.Fatalf("after a failure: %+v", esc)
	}
	// A later "could not verify" keeps the verdict.
	ok(t, w.s.RecordEscrowVerification(w.ctx, ScopeAll(), id, EscrowVerification{}))
	if esc = must[Escrow](t)(w.s.ActiveEscrow(w.ctx, ScopeAll(), id)); esc.LastVerifyErrorClass != "wrong_password" {
		t.Fatalf("an unverifiable attempt cleared the verdict: %+v", esc)
	}
	// Success sets verified_at and clears the class.
	w.clk.Advance(time.Minute)
	ok(t, w.s.RecordEscrowVerification(w.ctx, ScopeAll(), id, EscrowVerification{OK: true}))
	esc = must[Escrow](t)(w.s.ActiveEscrow(w.ctx, ScopeAll(), id))
	if esc.VerifiedAt == nil || !esc.VerifiedAt.Equal(t0.Add(2*time.Minute)) || esc.LastVerifyErrorClass != "" || !esc.LastVerifyAttemptAt.Equal(t0.Add(2*time.Minute)) {
		t.Fatalf("after a success: %+v", esc)
	}
	// The other device is untouched.
	if other := must[Escrow](t)(w.s.ActiveEscrow(w.ctx, ScopeAll(), w.devB.ID)); other.VerifiedAt != nil || other.LastVerifyAttemptAt != nil {
		t.Fatalf("device B's escrow changed: %+v", other)
	}
	// No audit rows.
	if after := must[Listing[AuditEvent]](t)(w.s.ListAudit(w.ctx, ScopeAll(), AuditFilter{}, Page{Limit: 200})); len(after.Items) != len(auditBefore.Items) {
		t.Fatalf("audit rows %d -> %d", len(auditBefore.Items), len(after.Items))
	}
	// An unknown device, and a scope that does not hold the device, are ErrNotFound.
	if err := w.s.RecordEscrowVerification(w.ctx, ScopeAll(), "01ZZZZZZZZZZZZZZZZZZZZZZZZ", EscrowVerification{OK: true}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown device = %v", err)
	}
	if err := w.s.RecordEscrowVerification(w.ctx, ScopeOf(w.custB.ID), id, EscrowVerification{OK: true}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("out-of-scope device = %v", err)
	}
}
