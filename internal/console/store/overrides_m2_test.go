package store

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func fieldOf(t *testing.T, view DeviceEffective, path string) EffectiveField {
	t.Helper()
	for _, f := range view.Fields {
		if f.Path == path {
			return f
		}
	}
	t.Fatalf("effective view has no field %s", path)
	return EffectiveField{}
}

func TestW2AOverrideSetShowsSourcesPerField(t *testing.T) {
	w := seed(t)
	tpl := w.template("Workstation", sect("sla", `{"stale_after_hours":48}`, "retention", `{"rules":{"keep_daily":10},"keep_within":"7d"}`))
	w.assign(w.custA.ID, tpl.ID)
	etag := agentETag(w, w.devA)
	before := w.auditCount("device.override_set")

	o, changed, err := w.s.SetDeviceOverride(w.ctx, ScopeAll(), w.devA.ID, OverrideInput{
		Patch:  json.RawMessage(`{"schedule":{"interval_minutes":240},"sla":{"stale_after_hours":24}}`),
		Reason: "files server, back up more often"}, w.actor())
	if err != nil || !changed || o.Reason == "" || o.SetBy != w.admin.ID {
		t.Fatalf("set: %v changed=%v %+v", err, changed, o)
	}
	if w.auditCount("device.override_set") != before+1 {
		t.Fatal("exactly one audit row per override")
	}
	ev := must[Listing[AuditEvent]](t)(w.s.ListAudit(w.ctx, ScopeAll(), AuditFilter{Action: "device.override_set"}, Page{Limit: 5})).Items[0]
	if strings.Contains(string(ev.Details), "files server") || strings.Contains(string(ev.Details), "240") || !strings.Contains(string(ev.Details), "schedule.interval_minutes") {
		t.Fatalf("the audit row names fields, never values or the reason: %s", ev.Details)
	}
	if agentETag(w, w.devA) == etag {
		t.Fatal("the agent's effective policy did not change after the override")
	}

	v := must[DeviceEffective](t)(w.s.GetDeviceEffective(w.ctx, ScopeAll(), w.devA.ID))
	iv := fieldOf(t, v, "schedule.interval_minutes")
	if iv.Source != SourceOverride || iv.Value != "240" || iv.Inherited != "1440" {
		t.Fatalf("overridden schedule: %+v", iv)
	}
	sl := fieldOf(t, v, "sla.stale_after_hours")
	if sl.Source != SourceOverride || sl.Value != "24" || sl.Inherited != "48" {
		t.Fatalf("override over the template: %+v", sl)
	}
	if f := fieldOf(t, v, "retention.rules.keep_daily"); f.Source != SourcePolicy || f.Value != "10" {
		t.Fatalf("a rule the template created comes from the template: %+v", f)
	}
	if f := fieldOf(t, v, "retention.keep_within"); f.Source != SourcePolicy || f.Value != `"7d"` {
		t.Fatalf("keep_within: %+v", f)
	}
	if f := fieldOf(t, v, "conditions.metered"); f.Source != SourceDefault {
		t.Fatalf("untouched field: %+v", f)
	}
	if v.PolicyName != "Workstation" || v.PolicyScope != "template" || v.Override == nil || v.ETag == "" {
		t.Fatalf("view header: %+v", v)
	}

	// Setting the same override again is not a change: no audit row, no history row.
	_, changed, err = w.s.SetDeviceOverride(w.ctx, ScopeAll(), w.devA.ID, OverrideInput{
		Patch: json.RawMessage(`{ "sla": {"stale_after_hours": 24}, "schedule": {"interval_minutes": 240} }`), Reason: "files server, back up more often"}, w.actor())
	if err != nil || changed || w.auditCount("device.override_set") != before+1 {
		t.Fatalf("idempotent set: %v changed=%v", err, changed)
	}
	// Replacing it clears the old row (history is kept) and keeps one active.
	if _, changed, err = w.s.SetDeviceOverride(w.ctx, ScopeAll(), w.devA.ID, OverrideInput{Patch: json.RawMessage(`{"sla":{"stale_after_hours":30}}`), Reason: "relaxed"}, w.actor()); err != nil || !changed {
		t.Fatalf("replace: %v", err)
	}
	var total, active int
	ok(t, w.s.Read(w.ctx, func(tx *Tx) error {
		if err := tx.QueryRow(`SELECT COUNT(*), COALESCE(SUM(cleared_at IS NULL), 0) FROM device_policy_overrides WHERE device_id = ?`, w.devA.ID).Scan(&total, &active); err != nil {
			return err
		}
		return nil
	}))
	if total != 2 || active != 1 {
		t.Fatalf("history kept, one active: %d rows %d active", total, active)
	}
}

func TestW2AOverrideIsValidatedAgainstTheEngine(t *testing.T) {
	w := seed(t)
	set := func(patch, reason string) error {
		_, _, err := w.s.SetDeviceOverride(w.ctx, ScopeAll(), w.devA.ID, OverrideInput{Patch: json.RawMessage(patch), Reason: reason}, w.actor())
		return err
	}
	before := w.auditCount("device.override_set")
	for name, c := range map[string]struct{ patch, reason, field string }{
		"no reason":      {`{"sla":{"stale_after_hours":24}}`, "  ", "reason"},
		"unknown field":  {`{"sla":{"colour":24}}`, "x", "patch"},
		"unknown":        {`{"nothing":{"a":1}}`, "x", "patch"},
		"empty":          {`{}`, "x", "patch"},
		"not an object":  {`[1]`, "x", "patch"},
		"bad value":      {`{"schedule":{"interval_minutes":1}}`, "x", "schedule.interval_minutes"},
		"bad window":     {`{"schedule":{"window":"Mon-Fri/18-8x"}}`, "x", "schedule.window"},
		"wrong type":     {`{"schedule":{"interval_minutes":"often"}}`, "x", "patch"},
		"section scalar": {`{"sla":5}`, "x", "patch"},
	} {
		err := set(c.patch, c.reason)
		found := false
		for _, f := range FieldErrors(err) {
			found = found || f.Field == c.field
		}
		if !errors.Is(err, ErrInvalid) || !found {
			t.Errorf("%s: want a %s field error, got %v", name, c.field, err)
		}
	}
	if w.auditCount("device.override_set") != before {
		t.Fatal("a refused override wrote an audit row")
	}
	// An override that is valid alone but not over the customer's policy is refused too.
	tpl := w.template("Tight", sect("sla", `{"unseen_after_hours":100}`))
	w.assign(w.custA.ID, tpl.ID)
	err := set(`{"sla":{"stale_after_hours":110}}`, "x")
	if fe := FieldErrors(err); len(fe) != 1 || fe[0].Field != "sla.unseen_after_hours" {
		t.Fatalf("override over the template: %v", err)
	}
	// An expiry in the past is refused.
	past := t0.Add(-time.Hour)
	_, _, err = w.s.SetDeviceOverride(w.ctx, ScopeAll(), w.devA.ID, OverrideInput{Patch: json.RawMessage(`{"sla":{"stale_after_hours":24}}`), Reason: "x", ExpiresAt: &past}, w.actor())
	if fe := FieldErrors(err); len(fe) != 1 || fe[0].Field != "expires_at" {
		t.Fatalf("past expiry: %v", err)
	}
}

func TestW2AOverrideClearAndExpiry(t *testing.T) {
	w := seed(t)
	exp := t0.Add(2 * time.Hour)
	must[DeviceOverride](t)(overrideOf(w.s.SetDeviceOverride(w.ctx, ScopeAll(), w.devA.ID,
		OverrideInput{Patch: json.RawMessage(`{"sla":{"stale_after_hours":24}}`), Reason: "temporary", ExpiresAt: &exp}, w.actor())))
	base := agentETag(w, w.devB) // Bravo has no override and the same default policy
	if agentETag(w, w.devA) == base {
		t.Fatal("the override must change Alpha's effective policy")
	}
	// After the expiry the agent no longer sees it, and the view says so.
	w.clk.Advance(3 * time.Hour)
	if agentETag(w, w.devA) != base {
		t.Fatal("an expired override must not reach the agent")
	}
	v := must[DeviceEffective](t)(w.s.GetDeviceEffective(w.ctx, ScopeAll(), w.devA.ID))
	if v.Override == nil || !v.Override.Expired || fieldOf(t, v, "sla.stale_after_hours").Source != SourceDefault {
		t.Fatalf("expired override: %+v", v.Override)
	}
	// An expired, uncleared row does not block a new override (one active row is a database rule).
	must[DeviceOverride](t)(overrideOf(w.s.SetDeviceOverride(w.ctx, ScopeAll(), w.devA.ID,
		OverrideInput{Patch: json.RawMessage(`{"sla":{"stale_after_hours":30}}`), Reason: "again"}, w.actor())))

	before := w.auditCount("device.override_clear")
	changed, err := w.s.ClearDeviceOverride(w.ctx, ScopeAll(), w.devA.ID, w.actor())
	if err != nil || !changed || w.auditCount("device.override_clear") != before+1 {
		t.Fatalf("clear: %v changed=%v", err, changed)
	}
	if agentETag(w, w.devA) != base {
		t.Fatal("clearing the override must restore the policy")
	}
	if o := must[*DeviceOverride](t)(w.s.GetDeviceOverride(w.ctx, ScopeAll(), w.devA.ID)); o != nil {
		t.Fatalf("cleared override still active: %+v", o)
	}
	changed, err = w.s.ClearDeviceOverride(w.ctx, ScopeAll(), w.devA.ID, w.actor())
	if err != nil || changed || w.auditCount("device.override_clear") != before+1 {
		t.Fatalf("clearing nothing must be a quiet no-op: %v changed=%v", err, changed)
	}
}

func TestW2AOverridesStayInsideTheScope(t *testing.T) {
	w := seed(t)
	onlyB := ScopeOf(w.custB.ID)
	in := OverrideInput{Patch: json.RawMessage(`{"sla":{"stale_after_hours":24}}`), Reason: "x"}
	if _, _, err := w.s.SetDeviceOverride(w.ctx, onlyB, w.devA.ID, in, w.actor()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("set outside scope: %v", err)
	}
	// even with an input that would be refused, a device outside scope is a 404, not a field error
	if _, _, err := w.s.SetDeviceOverride(w.ctx, onlyB, w.devA.ID, OverrideInput{Reason: ""}, w.actor()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bad input outside scope: %v", err)
	}
	if _, err := w.s.GetDeviceOverride(w.ctx, onlyB, w.devA.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get outside scope: %v", err)
	}
	if _, err := w.s.ClearDeviceOverride(w.ctx, onlyB, w.devA.ID, w.actor()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("clear outside scope: %v", err)
	}
	if _, err := w.s.GetDeviceEffective(w.ctx, onlyB, w.devA.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("effective outside scope: %v", err)
	}
	if _, _, err := w.s.SetDeviceOverride(w.ctx, onlyB, w.devB.ID, in, w.actor()); err != nil {
		t.Fatalf("set inside scope: %v", err)
	}
}

func TestW2ACustomerPolicyViewAndSources(t *testing.T) {
	w := seed(t)
	tpl := w.template("Workstation", sect("sla", `{"stale_after_hours":48}`))
	w.assign(w.custA.ID, tpl.ID)
	must[DeviceOverride](t)(overrideOf(w.s.SetDeviceOverride(w.ctx, ScopeAll(), w.devA.ID, OverrideInput{Patch: json.RawMessage(`{"sla":{"stale_after_hours":24}}`), Reason: "x"}, w.actor())))
	v := must[CustomerPolicy](t)(w.s.GetCustomerPolicy(w.ctx, ScopeAll(), w.custA.ID))
	if v.Assigned == nil || v.Assigned.ID != tpl.ID || v.Devices != 1 || v.Overridden != 1 || len(v.Choices) != 1 || v.Choices[0].ID != tpl.ID {
		t.Fatalf("customer policy: %+v", v)
	}
	found := false
	for _, f := range v.Effective.Fields {
		if f.Path == "sla.stale_after_hours" {
			found = f.Source == SourcePolicy && f.Value == "48" && f.Inherited == "72"
		}
	}
	if !found {
		t.Fatal("the summary must show the template as the source of stale_after_hours")
	}
	b := must[CustomerPolicy](t)(w.s.GetCustomerPolicy(w.ctx, ScopeAll(), w.custB.ID))
	if b.Assigned != nil || b.Effective.PolicyScope != "default" || b.Overridden != 0 {
		t.Fatalf("default customer: %+v", b)
	}
	if _, err := w.s.GetCustomerPolicy(w.ctx, ScopeOf(w.custB.ID), w.custA.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("customer outside scope: %v", err)
	}
}
