package store

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// Verification fixes of M2-W2A (items 1 to 3, 5 and 6 of the fix batch).

func (w *world) breakPolicy(id string) {
	w.t.Helper()
	ok(w.t, w.s.Write(w.ctx, func(tx *Tx) error {
		_, err := tx.Exec(`UPDATE policies SET schedule = ? WHERE id = ?`, `{"interval_minutes":1}`, id)
		return err
	}))
}

func defaultScheduleWithRetryCap(t *testing.T, capS int) map[string]json.RawMessage {
	t.Helper()
	return sect("schedule", mustSection(t, "schedule", func(m map[string]any) { m["retries"].(map[string]any)["cap_s"] = capS }))
}

// ---- 1. the default edit checks every stored policy ----

func TestW2ADefaultEditChecksPoliciesNobodyUsesYet(t *testing.T) {
	w := seed(t)
	// The verifier's probe: a customer with no devices, a policy that composes today
	// (retry base 1800 under the default cap 3600) and a default edit with cap 900.
	empty := must[Customer](t)(w.s.CreateCustomerAudited(w.ctx, ScopeAll(), CustomerInput{Name: "Empty Co"}, w.actor()))
	custPol := must[PolicyRecord](t)(w.s.CreatePolicyAudited(w.ctx, ScopeAll(), PolicyInput{Scope: "customer", CustomerID: empty.ID, Name: "Slow retries",
		Sections: sect("schedule", `{"retries":{"base_s":1800}}`)}, w.actor()))
	w.assign(empty.ID, custPol.ID)
	// an unassigned template and the policy of an archived customer say the same
	w.template("Template slow", sect("schedule", `{"retries":{"base_s":1800}}`))
	old := must[Customer](t)(w.s.CreateCustomerAudited(w.ctx, ScopeAll(), CustomerInput{Name: "Old Co"}, w.actor()))
	must[PolicyRecord](t)(w.s.CreatePolicyAudited(w.ctx, ScopeAll(), PolicyInput{Scope: "customer", CustomerID: old.ID, Name: "Old policy",
		Sections: sect("schedule", `{"retries":{"base_s":1800}}`)}, w.actor()))
	ok(t, w.s.Write(w.ctx, func(tx *Tx) error {
		_, err := tx.Exec(`UPDATE customers SET state = 'archived', archived_at = ?, updated_at = ? WHERE id = ?`, tx.nowMS(), tx.nowMS(), old.ID)
		return err
	}))

	p := prop(defaultScheduleWithRetryCap(t, 900))
	rep, err := w.s.PreviewPolicyChange(w.ctx, ScopeAll(), w.defaultID(), w.admin.ID, p)
	fe := FieldErrors(err)
	if err == nil || len(fe) != 1 || fe[0].Field != "policy" {
		t.Fatalf("the default edit must be refused at preview: %v", err)
	}
	for _, name := range []string{"Slow retries of Empty Co", "Template slow", "Old policy of Old Co"} {
		if !strings.Contains(fe[0].Message, name) {
			t.Errorf("the refusal must name %q: %s", name, fe[0].Message)
		}
	}
	if rep.Token != "" {
		t.Fatal("a refused preview must not mint a token")
	}
	// ... and at save, even with a token minted for a different, harmless proposal
	good := prop(sect("sla", mustSection(t, "sla", func(m map[string]any) { m["first_backup_grace_hours"] = 80 })))
	tok := must[ImpactReport](t)(w.s.PreviewPolicyChange(w.ctx, ScopeAll(), w.defaultID(), w.admin.ID, good)).Token
	if _, _, err := w.s.UpdatePolicyAudited(w.ctx, ScopeAll(), w.defaultID(), p, tok, w.actor()); err == nil {
		t.Fatal("the save must refuse too")
	}
	if cur := must[PolicyRecord](t)(w.s.DefaultPolicy(w.ctx)); cur.Version != 1 {
		t.Fatal("a refused default edit changed the default")
	}
	// the customer's tab still renders
	if cp := must[CustomerPolicy](t)(w.s.GetCustomerPolicy(w.ctx, ScopeAll(), empty.ID)); cp.Broken {
		t.Fatal("nothing was saved, so nothing may be broken")
	}
	// a scoped reader never gets names outside its scope: the refusal counts them
	// (the default is edited by All scope only; the helper is what the names go through)
	names, hidden, err := newlyBrokenPolicies2(t, w, ScopeOf(w.custA.ID), p)
	if err != nil || len(names) != 1 || names[0] != "Template slow" || hidden != 2 {
		t.Fatalf("scoped view of the refusal: names=%v hidden=%d err=%v", names, hidden, err)
	}
}

func newlyBrokenPolicies2(t *testing.T, w *world, sc Scope, p ProposedPolicy) (names []string, hidden int, err error) {
	t.Helper()
	err = w.s.Read(w.ctx, func(tx *Tx) error {
		def, _, e := loadDefaultSections(tx)
		if e != nil {
			return e
		}
		rec := must[PolicyRecord](t)(w.s.DefaultPolicy(w.ctx))
		res, e := resultOf(rec, p)
		if e != nil {
			return e
		}
		names, hidden, e = newlyBrokenPolicies(tx, sc, def, res.sections)
		return e
	})
	return
}

// ---- 1b. recovery ----

func TestW2AABrokenPolicyCanBeRepairedOrReplaced(t *testing.T) {
	w := seed(t)
	tpl := w.template("Fragile", nil)
	w.assign(w.custA.ID, tpl.ID)
	w.breakPolicy(tpl.ID)

	cp, err := w.s.GetCustomerPolicy(w.ctx, ScopeAll(), w.custA.ID)
	if err != nil || !cp.Broken || cp.Assigned == nil {
		t.Fatalf("the Policy tab must still load for a policy that does not compose: broken=%v err=%v", cp.Broken, err)
	}
	// a harmless default edit is not blocked by a policy that was already broken
	harmless := prop(sect("sla", mustSection(t, "sla", func(m map[string]any) { m["first_backup_grace_hours"] = 80 })))
	if _, err := w.s.PreviewPolicyChange(w.ctx, ScopeAll(), w.defaultID(), w.admin.ID, harmless); err != nil {
		t.Fatalf("an already-broken policy must not lock the default: %v", err)
	}
	// an edit that fixes it is allowed and flagged as a repair
	fix := prop(sect("schedule", `{"interval_minutes":240}`))
	rep, err := w.s.PreviewPolicyChange(w.ctx, ScopeAll(), tpl.ID, w.admin.ID, fix)
	if err != nil || !rep.Repairs || rep.Token == "" {
		t.Fatalf("repair preview: %+v %v", rep, err)
	}
	if _, changed, err := w.s.UpdatePolicyAudited(w.ctx, ScopeAll(), tpl.ID, fix, rep.Token, w.actor()); err != nil || !changed {
		t.Fatalf("repair save: %v changed=%v", err, changed)
	}
	if cp := must[CustomerPolicy](t)(w.s.GetCustomerPolicy(w.ctx, ScopeAll(), w.custA.ID)); cp.Broken {
		t.Fatal("still broken after the repair")
	}
	// reassigning away from a broken policy is allowed too
	w.breakPolicy(tpl.ID)
	arep, err := w.s.PreviewPolicyAssignment(w.ctx, ScopeAll(), w.custA.ID, "", w.admin.ID)
	if err != nil || !arep.Repairs {
		t.Fatalf("reassign preview: %+v %v", arep, err)
	}
	if changed, err := w.s.AssignCustomerPolicyAudited(w.ctx, ScopeAll(), w.custA.ID, "", arep.Token, w.actor()); err != nil || !changed {
		t.Fatalf("reassign: %v changed=%v", err, changed)
	}
}

// ---- 2. pins survive an unchanged save ----

func TestW2APinEqualToTheDefaultSurvivesAnUnchangedSave(t *testing.T) {
	w := seed(t)
	tpl := w.template("Pinned", sect("sla", `{"stale_after_hours":72}`)) // 72 is the default's value
	defSec := must[PolicyRecord](t)(w.s.DefaultPolicy(w.ctx)).Sections
	def, err := ComposePolicy(defSec, nil)
	ok(t, err)
	target, err := ComposePolicy(defSec, tpl.Sections)
	ok(t, err)
	plain := must[map[string]json.RawMessage](t)(SparseSections(def, target))
	if string(plain["sla"]) != "{}" {
		t.Fatalf("the bug: the plain diff drops the pin: %s", plain["sla"])
	}
	kept := must[map[string]json.RawMessage](t)(SparseWithPins(def, target, tpl.Sections))
	if !strings.Contains(string(kept["sla"]), `"stale_after_hours":72`) {
		t.Fatalf("SparseWithPins must keep the pin: %s", kept["sla"])
	}
	rep := must[ImpactReport](t)(w.s.PreviewPolicyChange(w.ctx, ScopeAll(), tpl.ID, w.admin.ID, ProposedPolicy{Sections: kept}))
	if !rep.Unchanged {
		t.Fatal("saving the form unchanged must be Nothing to save")
	}
	before := w.auditCount("policy.update")
	got, changed, err := w.s.UpdatePolicyAudited(w.ctx, ScopeAll(), tpl.ID, ProposedPolicy{Sections: kept}, rep.Token, w.actor())
	if err != nil || changed || got.Version != tpl.Version || w.auditCount("policy.update") != before {
		t.Fatalf("an unchanged save wrote something: %v changed=%v v%d", err, changed, got.Version)
	}
	// a changed value is still saved, the other pin kept
	target.SLA.UnseenAfterHours = 200
	more := must[map[string]json.RawMessage](t)(SparseWithPins(def, target, tpl.Sections))
	if !strings.Contains(string(more["sla"]), `"stale_after_hours":72`) || !strings.Contains(string(more["sla"]), "200") {
		t.Fatalf("pin and change: %s", more["sla"])
	}
}

// ---- 3. unknown keys ----

func TestW2AUnknownKeysInSectionsAreRefused(t *testing.T) {
	w := seed(t)
	_, err := w.s.CreatePolicyAudited(w.ctx, ScopeAll(), PolicyInput{Scope: "template", Name: "Typo", Sections: sect("sla", `{"stale_after_hourz":5}`)}, w.actor())
	fe := FieldErrors(err)
	if len(fe) != 1 || fe[0].Field != "sla.stale_after_hourz" {
		t.Fatalf("create with a typo: %v", err)
	}
	tpl := w.template("Fine", nil)
	for name, sec := range map[string]map[string]json.RawMessage{
		"unknown nested": sect("schedule", `{"retries":{"bogus":1}}`),
		"unknown field":  sect("sla", `{"stale_after_hourz":5}`),
	} {
		_, err := w.s.PreviewPolicyChange(w.ctx, ScopeAll(), tpl.ID, w.admin.ID, prop(sec))
		if len(FieldErrors(err)) != 1 || !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: preview %v", name, err)
		}
		_, _, err = w.s.UpdatePolicyAudited(w.ctx, ScopeAll(), tpl.ID, prop(sec), "x", w.actor())
		if len(FieldErrors(err)) != 1 {
			t.Errorf("%s: update %v", name, err)
		}
	}
	if cur := must[PolicyRecord](t)(w.s.GetPolicy(w.ctx, ScopeAll(), tpl.ID)); cur.Version != tpl.Version {
		t.Fatal("a refused update changed the policy")
	}
}

// ---- 5. token binding and single use ----

func TestW2ATokenGoesStaleWhenThePopulationMoves(t *testing.T) {
	w := seed(t)
	tpl := w.template("Shared", sect("sla", `{"stale_after_hours":48}`))
	w.assign(w.custA.ID, tpl.ID)
	p := prop(sect("sla", `{"stale_after_hours":36}`))
	rep := must[ImpactReport](t)(w.s.PreviewPolicyChange(w.ctx, ScopeAll(), tpl.ID, w.admin.ID, p))
	w.assign(w.custB.ID, tpl.ID) // another customer joins the template after the preview
	if _, _, err := w.s.UpdatePolicyAudited(w.ctx, ScopeAll(), tpl.ID, p, rep.Token, w.actor()); !errors.Is(err, ErrImpactStale) {
		t.Fatalf("the preview measured one customer, the save would reach two: %v", err)
	}
	rep = must[ImpactReport](t)(w.s.PreviewPolicyChange(w.ctx, ScopeAll(), tpl.ID, w.admin.ID, p))
	if rep.Population != 2 {
		t.Fatalf("a fresh preview sees both customers: %+v", rep)
	}
	if _, _, err := w.s.UpdatePolicyAudited(w.ctx, ScopeAll(), tpl.ID, p, rep.Token, w.actor()); err != nil {
		t.Fatalf("fresh token: %v", err)
	}
}

func TestW2ADefaultVersionMoveIsStaleNotMismatch(t *testing.T) {
	w := seed(t)
	tpl := w.template("Edge", nil)
	// an assignment token goes stale when the default changes
	arep := must[ImpactReport](t)(w.s.PreviewPolicyAssignment(w.ctx, ScopeAll(), w.custA.ID, tpl.ID, w.admin.ID))
	harmless := prop(sect("sla", mustSection(t, "sla", func(m map[string]any) { m["first_backup_grace_hours"] = 80 })))
	if _, _, err := w.previewAndUpdate(ScopeAll(), w.defaultID(), harmless); err != nil {
		t.Fatal(err)
	}
	if _, err := w.s.AssignCustomerPolicyAudited(w.ctx, ScopeAll(), w.custA.ID, tpl.ID, arep.Token, w.actor()); !errors.Is(err, ErrImpactStale) || errors.Is(err, ErrImpactMismatch) {
		t.Fatalf("assignment after a default change: want stale, got %v", err)
	}
	// so does a template edit
	edit := prop(sect("sla", `{"stale_after_hours":40}`))
	rep := must[ImpactReport](t)(w.s.PreviewPolicyChange(w.ctx, ScopeAll(), tpl.ID, w.admin.ID, edit))
	again := prop(sect("sla", mustSection(t, "sla", func(m map[string]any) { m["first_backup_grace_hours"] = 90 })))
	if _, _, err := w.previewAndUpdate(ScopeAll(), w.defaultID(), again); err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.s.UpdatePolicyAudited(w.ctx, ScopeAll(), tpl.ID, edit, rep.Token, w.actor()); !errors.Is(err, ErrImpactStale) || errors.Is(err, ErrImpactMismatch) {
		t.Fatalf("edit after a default change: want stale, got %v", err)
	}
	// a default edit goes stale when a new stored policy appears
	drep := must[ImpactReport](t)(w.s.PreviewPolicyChange(w.ctx, ScopeAll(), w.defaultID(), w.admin.ID, harmless2(t)))
	w.template("Latecomer", nil)
	if _, _, err := w.s.UpdatePolicyAudited(w.ctx, ScopeAll(), w.defaultID(), harmless2(t), drep.Token, w.actor()); !errors.Is(err, ErrImpactStale) {
		t.Fatalf("default edit after a new template: %v", err)
	}
}

func harmless2(t *testing.T) ProposedPolicy {
	return prop(sect("sla", mustSection(t, "sla", func(m map[string]any) { m["first_backup_grace_hours"] = 100 })))
}

func TestW2AAssignmentTokenIsSingleUse(t *testing.T) {
	w := seed(t)
	a := w.template("A", nil)
	b := w.template("B", nil)
	t1 := must[ImpactReport](t)(w.s.PreviewPolicyAssignment(w.ctx, ScopeAll(), w.custA.ID, a.ID, w.admin.ID))
	if changed, err := w.s.AssignCustomerPolicyAudited(w.ctx, ScopeAll(), w.custA.ID, a.ID, t1.Token, w.actor()); err != nil || !changed {
		t.Fatalf("first use: %v changed=%v", err, changed)
	}
	w.assign(w.custA.ID, b.ID)
	w.assign(w.custA.ID, "") // back to the state the token was measured on
	before := w.auditCount("customer.update")
	if _, err := w.s.AssignCustomerPolicyAudited(w.ctx, ScopeAll(), w.custA.ID, a.ID, t1.Token, w.actor()); !errors.Is(err, ErrImpactStale) {
		t.Fatalf("a used token must not apply again after A to B to A: %v", err)
	}
	if c := must[Customer](t)(w.s.GetCustomer(w.ctx, ScopeAll(), w.custA.ID)); c.PolicyID != "" || w.auditCount("customer.update") != before {
		t.Fatal("the replayed token changed the customer")
	}
	// the same holds for an update token
	tpl := w.template("Once", sect("sla", `{"stale_after_hours":48}`))
	p := prop(sect("sla", `{"stale_after_hours":40}`))
	rep := must[ImpactReport](t)(w.s.PreviewPolicyChange(w.ctx, ScopeAll(), tpl.ID, w.admin.ID, p))
	if _, _, err := w.s.UpdatePolicyAudited(w.ctx, ScopeAll(), tpl.ID, p, rep.Token, w.actor()); err != nil {
		t.Fatal(err)
	}
	back := prop(sect("sla", `{"stale_after_hours":48}`))
	w.previewAndUpdate(ScopeAll(), tpl.ID, back)
	if _, _, err := w.s.UpdatePolicyAudited(w.ctx, ScopeAll(), tpl.ID, p, rep.Token, w.actor()); !errors.Is(err, ErrImpactStale) {
		t.Fatalf("an update token is single use too: %v", err)
	}
}

// ---- 6. nulls ----

func TestW2ANullInASparseSectionRemovesThePinAndIsNotStored(t *testing.T) {
	w := seed(t)
	tpl := must[PolicyRecord](t)(w.s.CreatePolicyAudited(w.ctx, ScopeAll(), PolicyInput{Scope: "template", Name: "Nully",
		Sections: sect("sla", `{"stale_after_hours":null,"unseen_after_hours":200}`)}, w.actor()))
	if strings.Contains(string(tpl.Sections["sla"]), "null") || !strings.Contains(string(tpl.Sections["sla"]), "200") {
		t.Fatalf("a null must not be stored: %s", tpl.Sections["sla"])
	}
	w.assign(w.custA.ID, tpl.ID)
	v := must[DeviceEffective](t)(w.s.GetDeviceEffective(w.ctx, ScopeAll(), w.devA.ID))
	if f := fieldOf(t, v, "sla.stale_after_hours"); f.Source != SourceDefault || f.Value != "72" {
		t.Fatalf("a removed pin shows the default: %+v", f)
	}
	if f := fieldOf(t, v, "sla.unseen_after_hours"); f.Source != SourcePolicy {
		t.Fatalf("the real pin shows the policy: %+v", f)
	}
	// removing an existing pin through an update
	pinned := w.template("Pinned2", sect("sla", `{"stale_after_hours":48,"unseen_after_hours":200}`))
	got, _, err := w.previewAndUpdate(ScopeAll(), pinned.ID, prop(sect("sla", `{"stale_after_hours":null,"unseen_after_hours":200}`)))
	if err != nil || strings.Contains(string(got.Sections["sla"]), "stale_after_hours") || strings.Contains(string(got.Sections["sla"]), "null") {
		t.Fatalf("update with a null: %v %s", err, got.Sections["sla"])
	}
}
