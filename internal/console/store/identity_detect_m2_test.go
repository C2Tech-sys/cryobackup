package store

import (
	"errors"
	"strings"
	"testing"
	"time"
)

var (
	fpA = strings.Repeat("ab", 16)
	fpB = strings.Repeat("cd", 16)
)

func (w *world) note(dev, fp string) FingerprintOutcome {
	w.t.Helper()
	return must[FingerprintOutcome](w.t)(w.s.AgentNoteFingerprint(w.ctx, dev, fp, "192.0.2.50"))
}

func (w *world) device(dev string) Device {
	w.t.Helper()
	return must[Device](w.t)(w.s.GetDevice(w.ctx, ScopeAll(), dev))
}

func TestAgentNoteFingerprintRaisesOnceOnAChangedFingerprint(t *testing.T) {
	w := seed(t)
	id := w.devA.ID
	// Enrolled without a fingerprint (an import): the first one seen is recorded, not a conflict.
	if got := w.note(id, fpA); got != FingerprintAdopted {
		t.Fatalf("first fingerprint: %s, want adopted", got)
	}
	if d := w.device(id); d.HWFingerprint != fpA || d.IdentityConflictAt != nil {
		t.Fatalf("after adopt: fp=%q conflict=%v", d.HWFingerprint, d.IdentityConflictAt)
	}
	if n := w.cmdAuditCount("device.identity_adopt", id); n != 1 {
		t.Fatalf("adopt audit rows = %d", n)
	}
	// The same fingerprint, and none at all: nothing happens, nothing is written.
	for _, fp := range []string{fpA, ""} {
		if got := w.note(id, fp); got != FingerprintSame {
			t.Fatalf("fingerprint %q: %s, want same", fp, got)
		}
	}
	if w.device(id).IdentityConflictAt != nil || w.cmdAuditCount("device.identity_conflict", id) != 0 {
		t.Fatal("an unchanged fingerprint raised a conflict")
	}
	// A different one raises it, once.
	w.clk.Advance(time.Minute)
	if got := w.note(id, fpB); got != FingerprintConflictRaised {
		t.Fatalf("changed fingerprint: %s, want conflict_raised", got)
	}
	d := w.device(id)
	if d.IdentityConflictAt == nil || d.HWFingerprint != fpA || d.RetiredAt != nil {
		t.Fatalf("after raise: %+v (the recorded fingerprint must stay, the device must not be retired)", d)
	}
	raisedAt := *d.IdentityConflictAt
	w.clk.Advance(time.Minute)
	if got := w.note(id, fpB); got != FingerprintConflictOpen {
		t.Fatalf("repeat: %s, want conflict_open", got)
	}
	if got := w.note(id, fpA); got != FingerprintSame {
		t.Fatalf("the original machine again: %s, want same", got)
	}
	if d2 := w.device(id); d2.IdentityConflictAt == nil || !d2.IdentityConflictAt.Equal(raisedAt) {
		t.Fatalf("a repeat moved the conflict time: %v -> %v", raisedAt, d2.IdentityConflictAt)
	}
	// One audit row: a system event, critical, no fingerprint text anywhere in it.
	l := must[Listing[AuditEvent]](t)(w.s.ListAudit(w.ctx, ScopeAll(), AuditFilter{Action: "device.identity_conflict"}, Page{Limit: 10}))
	if len(l.Items) != 1 {
		t.Fatalf("conflict audit rows = %d, want 1", len(l.Items))
	}
	ev := l.Items[0]
	if ev.ActorType != "system" || ev.Severity != "critical" || ev.ObjectID != id || ev.CustomerID != w.custA.ID || ev.Result != "ok" {
		t.Fatalf("audit row: %+v", ev)
	}
	if blob := string(ev.Details) + ev.ActorLabel + ev.SourceIP; strings.Contains(blob, fpA) || strings.Contains(blob, fpB) {
		t.Fatalf("a fingerprint is in the audit row: %s", blob)
	}
	// The other customer's device is untouched.
	if w.device(w.devB.ID).IdentityConflictAt != nil {
		t.Fatal("another device was flagged")
	}
}

func TestAgentNoteFingerprintKeepAcceptsTheNewFingerprint(t *testing.T) {
	w := seed(t)
	id := w.devA.ID
	w.note(id, fpA)
	w.note(id, fpB)
	d := must[Device](t)(w.s.ResolveIdentityConflict(w.ctx, ScopeAll(), id, IdentityResolution{Action: "keep", Reason: "motherboard replaced"}, wActor))
	if d.IdentityConflictAt != nil || d.HWFingerprint != "" {
		t.Fatalf("after keep: conflict=%v fp=%q", d.IdentityConflictAt, d.HWFingerprint)
	}
	// The machine that heartbeats next becomes the identity; the conflict does not re-raise.
	if got := w.note(id, fpB); got != FingerprintAdopted {
		t.Fatalf("after keep: %s, want adopted", got)
	}
	for i := 0; i < 3; i++ {
		if got := w.note(id, fpB); got != FingerprintSame {
			t.Fatalf("heartbeat %d after keep: %s", i, got)
		}
	}
	if d := w.device(id); d.IdentityConflictAt != nil || d.HWFingerprint != fpB {
		t.Fatalf("after keep and heartbeats: fp=%q conflict=%v", d.HWFingerprint, d.IdentityConflictAt)
	}
	// The old machine, if it is still alive, now is the second machine: raised again, as it should be.
	if got := w.note(id, fpA); got != FingerprintConflictRaised {
		t.Fatalf("the other machine after keep: %s, want conflict_raised", got)
	}
	if n := w.cmdAuditCount("device.identity_conflict", id); n != 2 {
		t.Fatalf("conflict audit rows = %d, want 2 (one per raise)", n)
	}
}

func TestAgentNoteFingerprintRetireAsBeforeAndRetiredIsIgnored(t *testing.T) {
	w := seed(t)
	id := w.devA.ID
	w.note(id, fpA)
	w.note(id, fpB)
	d := must[Device](t)(w.s.ResolveIdentityConflict(w.ctx, ScopeAll(), id, IdentityResolution{Action: "retire", Reason: "clone of a restored image"}, wActor))
	if d.RetiredAt == nil || d.IdentityConflictAt != nil {
		t.Fatalf("after retire: %+v", d)
	}
	// A retired device is not re-flagged (and its credentials are revoked, so the API never reaches here).
	if got := w.note(id, fpB); got != FingerprintSame {
		t.Fatalf("retired device: %s, want same", got)
	}
	if w.device(id).IdentityConflictAt != nil {
		t.Fatal("a retired device was flagged")
	}
}

func TestAgentNoteFingerprintUnknownDeviceAndAuditFailure(t *testing.T) {
	w := seed(t)
	if _, err := w.s.AgentNoteFingerprint(w.ctx, "01J9Y4K3A0B1C2D3E4F5G6H7ZZ", fpA, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown device: %v", err)
	}
	id := w.devA.ID
	w.note(id, fpA)
	refuseAudits(t, w.s)
	// Not audited means not changed: the error comes back and the flag is not set.
	if _, err := w.s.AgentNoteFingerprint(w.ctx, id, fpB, ""); !errors.Is(err, ErrAudit) {
		t.Fatalf("raise without audit: %v, want ErrAudit", err)
	}
	if w.device(id).IdentityConflictAt != nil {
		t.Fatal("the conflict was set although its audit row could not be written")
	}
}
