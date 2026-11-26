package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// P22-R2b H1 (ADR 0033): sla.escrow_recheck_days. READ paths never fail on a stored value of 1 to 7 (they use
// 8); WRITE paths refuse it, naming the field; 0 stays accepted.

// lowRecheck stores days as the default policy's sla.escrow_recheck_days by raw SQL, as a policy saved before
// the floor existed would be stored.
func (w *world) lowRecheck(days int) {
	w.t.Helper()
	ok(w.t, w.s.Write(w.ctx, func(tx *Tx) error {
		_, err := tx.Exec(`UPDATE policies SET sla = json_set(sla, '$.escrow_recheck_days', ?) WHERE scope = 'default'`, days)
		return err
	}))
}

// Killed mutants: the clamp dropped from effectiveDoc (AgentEffectivePolicy fails invalid or keeps 3),
// from graceWithPatch (enrollment fails), from composeLayers; Validate refusing the stored value.
func TestP22R2bStoredLowRecheckNeverFailsAReadPath(t *testing.T) {
	w := seed(t)
	w.lowRecheck(3)

	// AgentEffectivePolicy: ok, and the policy runs with 8.
	p, _, etag, err := w.s.AgentEffectivePolicy(w.ctx, w.devA.ID)
	if err != nil || p.SLA.EscrowRecheckDays != 8 || etag == "" {
		t.Fatalf("AgentEffectivePolicy with a stored 3 = %d days, %v", p.SLA.EscrowRecheckDays, err)
	}
	// The operator-side compose of the same device.
	if e, err := w.s.EffectivePolicyDoc(w.ctx, ScopeAll(), w.devA.ID); err != nil || e.Policy.SLA.EscrowRecheckDays != 8 {
		t.Fatalf("EffectivePolicyDoc = %d days, %v", e.Policy.SLA.EscrowRecheckDays, err)
	}
	// AgentEnroll (the first-backup grace is read from the composed policy).
	res, err := w.s.AgentEnroll(w.ctx, enrollInput("tok-a", "A-DESK-9"))
	if err != nil || res.Device.ID == "" {
		t.Fatalf("AgentEnroll with a stored 3: %+v, %v", res.Device.ID, err)
	}
	// LoadWorld: no device load error, and the thresholds carry 8 days.
	world, err := w.s.LoadWorld(w.ctx, ScopeAll(), LoadOptions{})
	if err != nil || !world.LoadComplete {
		t.Fatalf("LoadWorld: complete=%v, %v", world.LoadComplete, err)
	}
	for _, d := range world.Devices {
		if !d.Complete || d.LoadErrorClass != "" {
			t.Fatalf("device %s: Complete=%v class=%q", d.Hostname, d.Complete, d.LoadErrorClass)
		}
		if d.Thresholds.EscrowRecheck != 8*24*time.Hour {
			t.Fatalf("device %s: recheck %v, want 8 days", d.Hostname, d.Thresholds.EscrowRecheck)
		}
	}
	// 0 is not clamped: it keeps its meaning (a zero-day recheck).
	w.lowRecheck(0)
	if p, _, _, err := w.s.AgentEffectivePolicy(w.ctx, w.devA.ID); err != nil || p.SLA.EscrowRecheckDays != 0 {
		t.Fatalf("a stored 0 = %d days, %v", p.SLA.EscrowRecheckDays, err)
	}
}

func fieldNamed(err error, field string) bool {
	for _, f := range FieldErrors(err) {
		if f.Field == field {
			return true
		}
	}
	return false
}

// Killed mutants: the input check dropped from validateComposed, validateProposal or the override write;
// the floor off by one (7 accepted, 8 refused).
func TestP22R2bWritesBelowTheFloorAreRefused(t *testing.T) {
	w := seed(t)
	sla := func(d int) map[string]json.RawMessage {
		return map[string]json.RawMessage{"sla": json.RawMessage(fmt.Sprintf(`{"escrow_recheck_days":%d}`, d))}
	}
	n := 0
	for _, c := range []struct {
		days int
		ok   bool
	}{{3, false}, {7, false}, {8, true}, {0, true}} {
		n++
		name := fmt.Sprintf("Template %d", n)
		// CreatePolicy (validateComposed).
		_, err := w.s.CreatePolicy(w.ctx, ScopeAll(), PolicyInput{Scope: "template", Name: name, Sections: sla(c.days)})
		switch {
		case c.ok && err != nil:
			t.Errorf("CreatePolicy %d: %v", c.days, err)
		case !c.ok && (!errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "sla.escrow_recheck_days")):
			t.Errorf("CreatePolicy %d: %v, want ErrInvalid naming sla.escrow_recheck_days", c.days, err)
		}
		// CreatePolicyAudited (validateProposal).
		_, err = w.s.CreatePolicyAudited(w.ctx, ScopeAll(), PolicyInput{Scope: "template", Name: name + " audited", Sections: sla(c.days)}, AuditActor{AdminID: w.admin.ID, Username: "root", Method: "web"})
		switch {
		case c.ok && err != nil:
			t.Errorf("CreatePolicyAudited %d: %v", c.days, err)
		case !c.ok && !fieldNamed(err, "sla.escrow_recheck_days"):
			t.Errorf("CreatePolicyAudited %d: %v, want a field error on sla.escrow_recheck_days", c.days, err)
		}
		// A device override (the patch composed over the device policy).
		_, _, err = w.s.SetDeviceOverride(w.ctx, ScopeAll(), w.devA.ID, OverrideInput{Patch: json.RawMessage(fmt.Sprintf(`{"sla":{"escrow_recheck_days":%d}}`, c.days)), Reason: "test"},
			AuditActor{AdminID: w.admin.ID, Username: "root", Method: "web"})
		switch {
		case c.ok && err != nil:
			t.Errorf("SetDeviceOverride %d: %v", c.days, err)
		case !c.ok && !fieldNamed(err, "sla.escrow_recheck_days"):
			t.Errorf("SetDeviceOverride %d: %v, want a field error on sla.escrow_recheck_days", c.days, err)
		}
	}
	// A stored 3 does not block a write that does not carry it: another field of a customer policy saves.
	w.lowRecheck(3)
	if _, err := w.s.CreatePolicy(w.ctx, ScopeAll(), PolicyInput{Scope: "template", Name: "Unrelated edit",
		Sections: map[string]json.RawMessage{"sla": json.RawMessage(`{"stale_after_hours":60,"unseen_after_hours":120}`)}}); err != nil {
		t.Errorf("a write that does not carry escrow_recheck_days, over a stored 3: %v", err)
	}
}

// The operator-side compose of a stored policy (ComposePolicy, the editor view) shows the value that runs: 8.
// Killed mutant: the clamp dropped from composeLayers.
func TestP22R2bComposeClamps(t *testing.T) {
	w := seed(t)
	w.lowRecheck(3)
	defRec := must[PolicyRecord](t)(w.s.DefaultPolicy(w.ctx))
	p, err := ComposePolicy(defRec.Sections, nil)
	if err != nil || p.SLA.EscrowRecheckDays != 8 {
		t.Fatalf("ComposePolicy over a stored 3 = %d days, %v", p.SLA.EscrowRecheckDays, err)
	}
}
