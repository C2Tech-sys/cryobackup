package store

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// tokenFor mints a token for customer A that names policyID ("" = none).
func (w *world) tokenFor(secret, policyID string) {
	w.t.Helper()
	must[EnrollmentToken](w.t)(w.s.CreateEnrollmentToken(w.ctx, ScopeAll(), EnrollmentToken{
		CustomerID: w.custA.ID, TokenHash: sha256Hex(secret), PolicyID: policyID, ExpiresAt: w.clk.Now().Add(24 * time.Hour)}))
}

func (w *world) setCustomerPolicy(policyID string) {
	w.t.Helper()
	c := must[Customer](w.t)(w.s.GetCustomer(w.ctx, ScopeAll(), w.custA.ID))
	c.PolicyID = policyID
	must[Customer](w.t)(w.s.UpdateCustomer(w.ctx, ScopeAll(), w.custA.ID, c))
}

func (w *world) graceHours(dev Device) int {
	w.t.Helper()
	e := must[EffectivePolicy](w.t)(w.s.EffectivePolicyDoc(w.ctx, ScopeAll(), dev.ID))
	return e.Policy.SLA.FirstBackupGraceHours
}

func (w *world) enrollDetails(devID string) string {
	w.t.Helper()
	l := must[Listing[AuditEvent]](w.t)(w.s.ListAudit(w.ctx, ScopeAll(), AuditFilter{Action: "device.enroll", ObjectID: devID}, Page{Limit: 5}))
	if len(l.Items) != 1 {
		w.t.Fatalf("device.enroll audit rows = %d, want 1", len(l.Items))
	}
	return string(l.Items[0].Details)
}

func TestEnrollmentAppliesTheTokensPolicyAsTheDeviceOverride(t *testing.T) {
	w := seed(t)
	custPol := must[PolicyRecord](t)(w.s.CreatePolicy(w.ctx, ScopeAll(), PolicyInput{Scope: "customer", CustomerID: w.custA.ID, Name: "Alpha policy",
		Sections: sect("sla", `{"stale_after_hours":60}`)}))
	w.setCustomerPolicy(custPol.ID)
	tpl := must[PolicyRecord](t)(w.s.CreatePolicy(w.ctx, ScopeAll(), PolicyInput{Scope: "template", Name: "Slow first backup",
		Sections: sect("sla", `{"first_backup_grace_hours":200}`)}))

	// Without a token policy: the customer's policy, no override, no token_policy audit detail.
	w.tokenFor("plain", "")
	plain := w.enroll("plain", "PLAIN-1").Device
	base := w.graceHours(plain)
	if o := must[*DeviceOverride](t)(w.s.GetDeviceOverride(w.ctx, ScopeAll(), plain.ID)); o != nil {
		t.Fatalf("a token without a policy created an override: %+v", o)
	}
	if d := w.enrollDetails(plain.ID); strings.Contains(d, "token_policy") {
		t.Fatalf("enroll audit details without a token policy = %q", d)
	}

	// With policy P: the device runs default + customer + P.
	w.tokenFor("with-p", tpl.ID)
	dev := w.enroll("with-p", "WITHP-1").Device
	o := must[*DeviceOverride](t)(w.s.GetDeviceOverride(w.ctx, ScopeAll(), dev.ID))
	if o == nil || o.SetBy != "" || !strings.Contains(o.Reason, "Slow first backup") || !strings.Contains(string(o.Patch), "200") {
		t.Fatalf("override: %+v", o)
	}
	e := must[EffectivePolicy](t)(w.s.EffectivePolicyDoc(w.ctx, ScopeAll(), dev.ID))
	if e.Policy.SLA.FirstBackupGraceHours != 200 {
		t.Fatalf("grace hours = %d, want 200 from the token's policy", e.Policy.SLA.FirstBackupGraceHours)
	}
	if e.Policy.SLA.StaleAfterHours != 60 {
		t.Fatalf("stale_after_hours = %d, want 60: the customer's policy still applies to fields P does not set", e.Policy.SLA.StaleAfterHours)
	}
	if base == 200 || w.graceHours(plain) != base {
		t.Fatalf("the other device's policy moved: %d", w.graceHours(plain))
	}
	if e.PolicyID != custPol.ID || e.Version != custPol.Version {
		t.Fatalf("the override changed the policy document identity: %+v", e)
	}
	// The first-backup deadline uses the composed grace.
	if dev.FirstBackupDueBy == nil || dev.FirstBackupDueBy.Sub(dev.EnrolledAt) != 200*time.Hour {
		t.Fatalf("first_backup_due_by %v from enrolled_at %v: want a 200 h grace", dev.FirstBackupDueBy, dev.EnrolledAt)
	}
	if d := w.enrollDetails(dev.ID); d != `{"token_policy":"applied"}` {
		t.Fatalf("enroll audit details = %q", d)
	}
	// The template moving later does not move the enrolled device (a snapshot).
	must[PolicyRecord](t)(w.s.UpdatePolicy(w.ctx, ScopeAll(), tpl.ID, PolicyUpdate{Sections: sect("sla", `{"first_backup_grace_hours":24}`)}))
	if got := w.graceHours(dev); got != 200 {
		t.Fatalf("grace after the template changed = %d, want the enrolled 200", got)
	}
}

func TestEnrollmentTokenPolicySameAsCustomersOrUnusableDoesNotBlockEnrollment(t *testing.T) {
	w := seed(t)
	custPol := must[PolicyRecord](t)(w.s.CreatePolicy(w.ctx, ScopeAll(), PolicyInput{Scope: "customer", CustomerID: w.custA.ID, Name: "Alpha policy",
		Sections: sect("sla", `{"stale_after_hours":60}`)}))
	w.setCustomerPolicy(custPol.ID)

	// The customer's own policy named on the token adds nothing.
	w.tokenFor("same", custPol.ID)
	same := w.enroll("same", "SAME-1").Device
	if o := must[*DeviceOverride](t)(w.s.GetDeviceOverride(w.ctx, ScopeAll(), same.ID)); o != nil {
		t.Fatalf("override for the customer's own policy: %+v", o)
	}
	if d := w.enrollDetails(same.ID); d != `{"token_policy":"same"}` {
		t.Fatalf("details = %q", d)
	}

	// An empty template adds nothing either.
	empty := must[PolicyRecord](t)(w.s.CreatePolicy(w.ctx, ScopeAll(), PolicyInput{Scope: "template", Name: "Empty"}))
	w.tokenFor("empty", empty.ID)
	e := w.enroll("empty", "EMPTY-1").Device
	if o := must[*DeviceOverride](t)(w.s.GetDeviceOverride(w.ctx, ScopeAll(), e.ID)); o != nil {
		t.Fatalf("override for an empty template: %+v", o)
	}

	// A template that no longer composes to a valid policy: the device still enrolls, under the
	// customer's policy, and the audit row says the token's policy was skipped.
	bad := must[PolicyRecord](t)(w.s.CreatePolicy(w.ctx, ScopeAll(), PolicyInput{Scope: "template", Name: "Bad", Sections: sect("sla", `{"first_backup_grace_hours":24}`)}))
	w.tokenFor("bad", bad.ID)
	ok(t, w.s.Write(w.ctx, func(tx *Tx) error {
		_, err := tx.Exec(`UPDATE policies SET sla = '{"stale_after_hours":-5}' WHERE id = ?`, bad.ID)
		return err
	}))
	b := w.enroll("bad", "BAD-1").Device
	if o := must[*DeviceOverride](t)(w.s.GetDeviceOverride(w.ctx, ScopeAll(), b.ID)); o != nil {
		t.Fatalf("an invalid token policy was applied: %+v", o)
	}
	if got := w.graceHours(b); got != w.graceHours(same) {
		t.Fatalf("grace = %d, want the customer's %d", got, w.graceHours(same))
	}
	if d := w.enrollDetails(b.ID); d != `{"token_policy":"skipped"}` {
		t.Fatalf("details = %q", d)
	}
}

// An already-invalid customer policy refuses the enrollment before anything is
// written: ErrInvalid, no device row, no token use spent (not a 500 after the fact).
func TestEnrollmentRefusesAnInvalidCustomerPolicyBeforeWriting(t *testing.T) {
	w := seed(t)
	custPol := must[PolicyRecord](t)(w.s.CreatePolicy(w.ctx, ScopeAll(), PolicyInput{Scope: "customer", CustomerID: w.custA.ID, Name: "Alpha policy",
		Sections: sect("sla", `{"stale_after_hours":60}`)}))
	w.setCustomerPolicy(custPol.ID)
	ok(t, w.s.Write(w.ctx, func(tx *Tx) error {
		_, err := tx.Exec(`UPDATE policies SET sla = '{"stale_after_hours":-5}' WHERE id = ?`, custPol.ID)
		return err
	}))
	tpl := must[PolicyRecord](t)(w.s.CreatePolicy(w.ctx, ScopeAll(), PolicyInput{Scope: "template", Name: "T", Sections: sect("sla", `{"first_backup_grace_hours":200}`)}))
	countDevices := func() int {
		n := 0
		ok(t, w.s.Read(w.ctx, func(tx *Tx) error { return tx.QueryRow(`SELECT count(*) FROM devices`).Scan(&n) }))
		return n
	}
	usedOf := func(secret string) int {
		for _, tk := range must[[]EnrollmentToken](t)(w.s.ListEnrollmentTokens(w.ctx, ScopeAll(), w.custA.ID)) {
			if tk.TokenHash == sha256Hex(secret) {
				return tk.UsedCount
			}
		}
		t.Fatalf("token %s not listed", secret)
		return 0
	}
	for _, c := range []struct{ secret, policy string }{{"inv-plain", ""}, {"inv-with-token-policy", tpl.ID}} {
		w.tokenFor(c.secret, c.policy)
		devices, used := countDevices(), usedOf(c.secret)
		_, err := w.s.AgentEnroll(w.ctx, enrollInput(c.secret, "INVALID-"+c.secret))
		if !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s: enroll with an invalid customer policy = %v, want ErrInvalid", c.secret, err)
		}
		if countDevices() != devices || usedOf(c.secret) != used {
			t.Fatalf("%s: the refused enrollment left a device or spent a use", c.secret)
		}
	}
}

func TestEnrollResultReportsASkippedTokenPolicyWithAClass(t *testing.T) {
	w := seed(t)
	bad := must[PolicyRecord](t)(w.s.CreatePolicy(w.ctx, ScopeAll(), PolicyInput{Scope: "template", Name: "Bad", Sections: sect("sla", `{"first_backup_grace_hours":24}`)}))
	ok(t, w.s.Write(w.ctx, func(tx *Tx) error {
		_, err := tx.Exec(`UPDATE policies SET sla = '{"stale_after_hours":-5}' WHERE id = ?`, bad.ID)
		return err
	}))
	w.tokenFor("skip", bad.ID)
	res := w.enroll("skip", "SKIP-1")
	if res.TokenPolicy != "skipped" || res.TokenPolicySkipReason != "does_not_compose" {
		t.Fatalf("result: %q / %q", res.TokenPolicy, res.TokenPolicySkipReason)
	}
	if again := must[EnrollResult](t)(w.s.AgentEnroll(w.ctx, func() EnrollInput {
		in := enrollInput("skip", "SKIP-1")
		in.DeviceTokenHash = sha256Hex("skip-replay")
		return in
	}())); again.TokenPolicy != "" {
		t.Fatalf("a replay reports the token policy again: %q", again.TokenPolicy)
	}
}

func TestEnrollmentReplayDoesNotApplyTheTokenPolicyTwice(t *testing.T) {
	w := seed(t)
	tpl := must[PolicyRecord](t)(w.s.CreatePolicy(w.ctx, ScopeAll(), PolicyInput{Scope: "template", Name: "T", Sections: sect("sla", `{"first_backup_grace_hours":200}`)}))
	w.tokenFor("replay", tpl.ID)
	first := w.enroll("replay", "REPLAY-1")
	in := enrollInput("replay", "REPLAY-1")
	in.DeviceTokenHash = sha256Hex("device-token-replayed")
	again := must[EnrollResult](t)(w.s.AgentEnroll(w.ctx, in))
	if !again.Replay || again.Device.ID != first.Device.ID {
		t.Fatalf("replay: %+v", again)
	}
	n := 0
	ok(t, w.s.Read(w.ctx, func(tx *Tx) error {
		return tx.QueryRow(`SELECT count(*) FROM device_policy_overrides WHERE device_id = ?`, first.Device.ID).Scan(&n)
	}))
	if n != 1 {
		t.Fatalf("override rows after a replay = %d, want 1", n)
	}
}
