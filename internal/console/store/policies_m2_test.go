package store

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/policy"
)

// ---- helpers (M2-W2A) ----

func (w *world) actor() AuditActor {
	return AuditActor{AdminID: w.admin.ID, Username: "w2a", Method: "web"}
}

func sect(kv ...string) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	for i := 0; i+1 < len(kv); i += 2 {
		out[kv[i]] = json.RawMessage(kv[i+1])
	}
	return out
}

func prop(sections map[string]json.RawMessage) ProposedPolicy {
	return ProposedPolicy{Sections: sections}
}

func (w *world) auditCount(action string) int {
	w.t.Helper()
	l := must[Listing[AuditEvent]](w.t)(w.s.ListAudit(w.ctx, ScopeAll(), AuditFilter{Action: action}, Page{Limit: 1000}))
	return len(l.Items)
}

func (w *world) defaultID() string {
	return must[PolicyRecord](w.t)(w.s.DefaultPolicy(w.ctx)).ID
}

// template creates a template policy through the audited path.
func (w *world) template(name string, sections map[string]json.RawMessage) PolicyRecord {
	w.t.Helper()
	return must[PolicyRecord](w.t)(w.s.CreatePolicyAudited(w.ctx, ScopeAll(), PolicyInput{Scope: "template", Name: name, Sections: sections}, w.actor()))
}

func (w *world) previewAndUpdate(scope Scope, id string, p ProposedPolicy) (PolicyRecord, bool, error) {
	w.t.Helper()
	rep, err := w.s.PreviewPolicyChange(w.ctx, scope, id, w.admin.ID, p)
	if err != nil {
		return PolicyRecord{}, false, err
	}
	return w.s.UpdatePolicyAudited(w.ctx, scope, id, p, rep.Token, w.actor())
}

func (w *world) assign(custID, polID string) {
	w.t.Helper()
	rep := must[ImpactReport](w.t)(w.s.PreviewPolicyAssignment(w.ctx, ScopeAll(), custID, polID, w.admin.ID))
	must[bool](w.t)(w.s.AssignCustomerPolicyAudited(w.ctx, ScopeAll(), custID, polID, rep.Token, w.actor()))
}

func agentETag(w *world, dev Device) string {
	w.t.Helper()
	_, _, etag, err := w.s.AgentEffectivePolicy(w.ctx, dev.ID)
	ok(w.t, err)
	return etag
}

// ---- blast radius ----

func TestW2APreviewCountsDevicesAndNamesTheChangedFields(t *testing.T) {
	w := seed(t)
	tpl := w.template("Workstation", sect("sla", `{"stale_after_hours":48}`))
	w.assign(w.custA.ID, tpl.ID)

	rep := must[ImpactReport](t)(w.s.PreviewPolicyChange(w.ctx, ScopeAll(), tpl.ID, w.admin.ID, prop(sect("sla", `{"stale_after_hours":36}`))))
	if rep.Population != 1 || rep.Devices != 1 || len(rep.Customers) != 1 || rep.Customers[0].ID != w.custA.ID {
		t.Fatalf("the template is used by Alpha's one device only: %+v", rep)
	}
	if len(rep.Changes) != 1 || rep.Changes[0].Path != "sla.stale_after_hours" || rep.Changes[0].Old != "48" || rep.Changes[0].New != "36" {
		t.Fatalf("changed fields (old -> new): %+v", rep.Changes)
	}
	if rep.Token == "" || rep.Unchanged || rep.Version != tpl.Version {
		t.Fatalf("token/unchanged/version: %+v", rep)
	}

	// The default policy reaches every device of every customer; Bravo (default) and
	// Alpha (template, which sets only stale_after_hours) both change when the
	// default's unseen window changes.
	defRep := must[ImpactReport](t)(w.s.PreviewPolicyChange(w.ctx, ScopeAll(), w.defaultID(), w.admin.ID, prop(sect("sla", mustSection(t, "sla", func(m map[string]any) { m["unseen_after_hours"] = 200 })))))
	if defRep.Population != 2 || defRep.Devices != 2 {
		t.Fatalf("default edit reaches both customers: %+v", defRep)
	}
	// A device whose own override covers the changed field keeps it.
	ov := json.RawMessage(`{"sla":{"unseen_after_hours":300}}`)
	must[DeviceOverride](t)(overrideOf(w.s.SetDeviceOverride(w.ctx, ScopeAll(), w.devB.ID, OverrideInput{Patch: ov, Reason: "pinned"}, w.actor())))
	defRep = must[ImpactReport](t)(w.s.PreviewPolicyChange(w.ctx, ScopeAll(), w.defaultID(), w.admin.ID, prop(sect("sla", mustSection(t, "sla", func(m map[string]any) { m["unseen_after_hours"] = 200 })))))
	if defRep.Population != 2 || defRep.Devices != 1 {
		t.Fatalf("Bravo's override keeps its value, so 1 of 2 devices changes: %+v", defRep)
	}
}

func overrideOf(o DeviceOverride, _ bool, err error) (DeviceOverride, error) { return o, err }

// mustSection returns the default policy's stored section with edit applied.
func mustSection(t *testing.T, name string, edit func(map[string]any)) string {
	t.Helper()
	sec := must[map[string]json.RawMessage](t)(policy.Encode(policy.Default(policy.ClassUnknown)))
	var m map[string]any
	ok(t, json.Unmarshal(sec[name], &m))
	edit(m)
	b, err := json.Marshal(m)
	ok(t, err)
	return string(b)
}

func TestW2AUpdateNeedsThePreviewTokenOfExactlyThisChange(t *testing.T) {
	w := seed(t)
	tpl := w.template("Workstation", sect("sla", `{"stale_after_hours":48}`))
	w.assign(w.custA.ID, tpl.ID)
	p := prop(sect("sla", `{"stale_after_hours":36}`))
	rep := must[ImpactReport](t)(w.s.PreviewPolicyChange(w.ctx, ScopeAll(), tpl.ID, w.admin.ID, p))
	before := w.auditCount("policy.update")

	// no token
	_, _, err := w.s.UpdatePolicyAudited(w.ctx, ScopeAll(), tpl.ID, p, "", w.actor())
	if !errors.Is(err, ErrInvalid) || len(FieldErrors(err)) != 1 || FieldErrors(err)[0].Field != "impact_token" {
		t.Fatalf("missing token: %v", err)
	}
	// a token for another proposal
	_, _, err = w.s.UpdatePolicyAudited(w.ctx, ScopeAll(), tpl.ID, prop(sect("sla", `{"stale_after_hours":12}`)), rep.Token, w.actor())
	if !errors.Is(err, ErrImpactMismatch) || !errors.Is(err, ErrConflict) {
		t.Fatalf("another proposal: %v", err)
	}
	// another administrator cannot use it
	other := AuditActor{AdminID: "01J9Y4K3A0B1C2D3E4F5G6H7BB", Username: "other"}
	_, _, err = w.s.UpdatePolicyAudited(w.ctx, ScopeAll(), tpl.ID, p, rep.Token, other)
	if !errors.Is(err, ErrImpactMismatch) {
		t.Fatalf("another admin: %v", err)
	}
	// garbage
	if _, _, err = w.s.UpdatePolicyAudited(w.ctx, ScopeAll(), tpl.ID, p, rep.Token+"x", w.actor()); !errors.Is(err, ErrImpactMismatch) {
		t.Fatalf("forged token: %v", err)
	}
	if got := must[PolicyRecord](t)(w.s.GetPolicy(w.ctx, ScopeAll(), tpl.ID)); got.Version != tpl.Version || w.auditCount("policy.update") != before {
		t.Fatal("a refused update changed the policy or wrote an audit row")
	}

	// the right token applies exactly what was previewed
	etag := agentETag(w, w.devA)
	got, changed, err := w.s.UpdatePolicyAudited(w.ctx, ScopeAll(), tpl.ID, p, rep.Token, w.actor())
	if err != nil || !changed || got.Version != tpl.Version+1 {
		t.Fatalf("update: %v changed=%v v%d", err, changed, got.Version)
	}
	if !strings.Contains(string(got.Sections["sla"]), "36") {
		t.Fatalf("stored section: %s", got.Sections["sla"])
	}
	if w.auditCount("policy.update") != before+1 {
		t.Fatalf("want exactly one policy.update audit row, have %d (was %d)", w.auditCount("policy.update"), before)
	}
	revs := must[[]PolicyRevisionView](t)(w.s.ListPolicyRevisionViews(w.ctx, ScopeAll(), tpl.ID, 10))
	if len(revs) != 2 || revs[0].Version != 2 || revs[0].ChangedBy != w.admin.ID {
		t.Fatalf("revisions: %+v", revs)
	}
	if agentETag(w, w.devA) == etag {
		t.Fatal("the agent's effective policy did not change after the save")
	}
	// the token cannot be replayed: the version moved
	if _, _, err = w.s.UpdatePolicyAudited(w.ctx, ScopeAll(), tpl.ID, p, rep.Token, w.actor()); !errors.Is(err, ErrImpactStale) {
		t.Fatalf("replay: %v", err)
	}
}

func TestW2APreviewGoesStaleWhenThePolicyMovesOrTheTokenExpires(t *testing.T) {
	w := seed(t)
	tpl := w.template("Workstation", sect("sla", `{"stale_after_hours":48}`))
	p := prop(sect("sla", `{"stale_after_hours":36}`))
	rep := must[ImpactReport](t)(w.s.PreviewPolicyChange(w.ctx, ScopeAll(), tpl.ID, w.admin.ID, p))
	// somebody else saves first
	if _, _, err := w.previewAndUpdate(ScopeAll(), tpl.ID, prop(sect("sla", `{"stale_after_hours":40}`))); err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.s.UpdatePolicyAudited(w.ctx, ScopeAll(), tpl.ID, p, rep.Token, w.actor()); !errors.Is(err, ErrImpactStale) {
		t.Fatalf("policy moved in between: %v", err)
	}
	rep = must[ImpactReport](t)(w.s.PreviewPolicyChange(w.ctx, ScopeAll(), tpl.ID, w.admin.ID, p))
	w.clk.Advance(impactTTL + time.Minute)
	if _, _, err := w.s.UpdatePolicyAudited(w.ctx, ScopeAll(), tpl.ID, p, rep.Token, w.actor()); !errors.Is(err, ErrImpactStale) {
		t.Fatalf("expired token: %v", err)
	}
}

func TestW2AAnUnchangedProposalWritesNothing(t *testing.T) {
	w := seed(t)
	tpl := w.template("Workstation", sect("sla", `{"stale_after_hours":48}`))
	rep := must[ImpactReport](t)(w.s.PreviewPolicyChange(w.ctx, ScopeAll(), tpl.ID, w.admin.ID, prop(sect("sla", `{ "stale_after_hours": 48 }`))))
	if !rep.Unchanged || len(rep.Changes) != 0 {
		t.Fatalf("whitespace is not a change: %+v", rep)
	}
	before := w.auditCount("policy.update")
	got, changed, err := w.s.UpdatePolicyAudited(w.ctx, ScopeAll(), tpl.ID, prop(sect("sla", `{ "stale_after_hours": 48 }`)), rep.Token, w.actor())
	if err != nil || changed || got.Version != tpl.Version || w.auditCount("policy.update") != before {
		t.Fatalf("no-op: %v changed=%v v%d audit %d/%d", err, changed, got.Version, w.auditCount("policy.update"), before)
	}
}

func TestW2AProposalProblemsComeBackKeyedByDocumentPath(t *testing.T) {
	w := seed(t)
	tpl := w.template("Workstation", nil)
	_, err := w.s.PreviewPolicyChange(w.ctx, ScopeAll(), tpl.ID, w.admin.ID, prop(sect("schedule", `{"window":"Mon-Fri/18-8x"}`, "sla", `{"stale_after_hours":0}`)))
	fe := FieldErrors(err)
	if !errors.Is(err, ErrInvalid) || len(fe) < 2 {
		t.Fatalf("want field errors, got %v", err)
	}
	byField := map[string]string{}
	for _, f := range fe {
		byField[f.Field] = f.Message
	}
	if !strings.Contains(byField["schedule.window"], "invalid window") || byField["sla.stale_after_hours"] == "" {
		t.Fatalf("problems by path: %+v", byField)
	}
}

func TestW2AADefaultEditThatBreaksACustomerPolicyIsRefused(t *testing.T) {
	w := seed(t)
	tpl := w.template("Tight", sect("sla", `{"unseen_after_hours":100}`))
	w.assign(w.custA.ID, tpl.ID)
	// stale 110 is fine for the default alone (unseen 168) but not under Tight (unseen 100).
	p := prop(sect("sla", mustSection(t, "sla", func(m map[string]any) { m["stale_after_hours"] = 110 })))
	rep, err := w.s.PreviewPolicyChange(w.ctx, ScopeAll(), w.defaultID(), w.admin.ID, p)
	if err == nil || len(FieldErrors(err)) == 0 || !strings.Contains(FieldErrors(err)[0].Message, "Tight") {
		t.Fatalf("want the preview to refuse naming the policy: %+v %v", rep, err)
	}

	if rep.Token != "" {
		t.Fatal("a refused preview must not mint a token")
	}
}

// ---- create, delete, assign ----

func TestW2ACreatePolicyIsAuditedAndValidated(t *testing.T) {
	w := seed(t)
	before := w.auditCount("policy.create")
	tpl := w.template("Laptops", sect("conditions", `{"battery_floor_pct":30}`))
	if w.auditCount("policy.create") != before+1 || tpl.Version != 1 {
		t.Fatal("create must write exactly one audit row and revision 1")
	}
	// name clash is a field error, case-insensitively, and writes nothing
	_, err := w.s.CreatePolicyAudited(w.ctx, ScopeAll(), PolicyInput{Scope: "template", Name: "LAPTOPS"}, w.actor())
	if fe := FieldErrors(err); len(fe) != 1 || fe[0].Field != "name" {
		t.Fatalf("duplicate name: %v", err)
	}
	// a document problem is a field error on its path
	_, err = w.s.CreatePolicyAudited(w.ctx, ScopeAll(), PolicyInput{Scope: "template", Name: "Bad", Sections: sect("schedule", `{"interval_minutes":1}`)}, w.actor())
	if fe := FieldErrors(err); len(fe) != 1 || fe[0].Field != "schedule.interval_minutes" {
		t.Fatalf("invalid document: %v", err)
	}
	if w.auditCount("policy.create") != before+1 {
		t.Fatal("a refused create wrote an audit row")
	}
	// a scoped caller cannot mint a template
	if _, err = w.s.CreatePolicyAudited(w.ctx, ScopeOf(w.custA.ID), PolicyInput{Scope: "template", Name: "Nope"}, w.actor()); !errors.Is(err, ErrScope) {
		t.Fatalf("scoped template create: %v", err)
	}
}

func TestW2ADeleteIsRefusedWhileAssignedAndNamesWhere(t *testing.T) {
	w := seed(t)
	tpl := w.template("Workstation", nil)
	w.assign(w.custA.ID, tpl.ID)
	rep, err := w.s.PreviewPolicyDelete(w.ctx, ScopeAll(), tpl.ID, w.admin.ID)
	var inUse *PolicyInUseError
	if !errors.As(err, &inUse) || !errors.Is(err, ErrConflict) || len(inUse.Customers) != 1 || inUse.Customers[0].ID != w.custA.ID || inUse.Customers[0].Devices != 1 {
		t.Fatalf("assigned policy: %v", err)
	}
	if rep.Token != "" || len(rep.Blockers) != 1 {
		t.Fatalf("a blocked delete has no token and lists the customers: %+v", rep)
	}
	if err := w.s.DeletePolicyAudited(w.ctx, ScopeAll(), tpl.ID, "anything", w.actor()); !errors.As(err, &inUse) {
		t.Fatalf("delete while assigned: %v", err)
	}
	if _, err := w.s.PreviewPolicyDelete(w.ctx, ScopeAll(), w.defaultID(), w.admin.ID); !errors.Is(err, ErrDefaultPolicy) {
		t.Fatalf("default policy: %v", err)
	}

	w.assign(w.custA.ID, "") // back to the default
	before := w.auditCount("policy.delete")
	if err := w.s.DeletePolicyAudited(w.ctx, ScopeAll(), tpl.ID, "", w.actor()); len(FieldErrors(err)) != 1 {
		t.Fatalf("delete without a token: %v", err)
	}
	rep = must[ImpactReport](t)(w.s.PreviewPolicyDelete(w.ctx, ScopeAll(), tpl.ID, w.admin.ID))
	if rep.Population != 0 || rep.Token == "" {
		t.Fatalf("an unassigned policy affects nobody: %+v", rep)
	}
	other := w.template("Other", nil)
	if err := w.s.DeletePolicyAudited(w.ctx, ScopeAll(), other.ID, rep.Token, w.actor()); !errors.Is(err, ErrImpactMismatch) {
		t.Fatalf("a token for another policy: %v", err)
	}
	ok(t, w.s.DeletePolicyAudited(w.ctx, ScopeAll(), tpl.ID, rep.Token, w.actor()))
	if _, err := w.s.GetPolicy(w.ctx, ScopeAll(), tpl.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted policy still readable: %v", err)
	}
	if w.auditCount("policy.delete") != before+1 {
		t.Fatal("delete must write exactly one audit row")
	}
}

func TestW2AAssigningAPolicyShowsDevicesAndFieldsFirst(t *testing.T) {
	w := seed(t)
	tpl := w.template("Workstation", sect("sla", `{"stale_after_hours":36}`))
	rep := must[ImpactReport](t)(w.s.PreviewPolicyAssignment(w.ctx, ScopeAll(), w.custA.ID, tpl.ID, w.admin.ID))
	if rep.Population != 1 || rep.Devices != 1 || len(rep.Changes) != 1 || rep.Changes[0].Path != "sla.stale_after_hours" || rep.Changes[0].Old != "72" || rep.Changes[0].New != "36" {
		t.Fatalf("assignment preview: %+v", rep)
	}
	before := w.auditCount("customer.update")
	if _, err := w.s.AssignCustomerPolicyAudited(w.ctx, ScopeAll(), w.custA.ID, tpl.ID, "", w.actor()); len(FieldErrors(err)) != 1 {
		t.Fatalf("assign without a token: %v", err)
	}
	if _, err := w.s.AssignCustomerPolicyAudited(w.ctx, ScopeAll(), w.custA.ID, "", rep.Token, w.actor()); !errors.Is(err, ErrImpactMismatch) {
		t.Fatalf("a token for another assignment: %v", err)
	}
	etag := agentETag(w, w.devA)
	changed, err := w.s.AssignCustomerPolicyAudited(w.ctx, ScopeAll(), w.custA.ID, tpl.ID, rep.Token, w.actor())
	if err != nil || !changed || w.auditCount("customer.update") != before+1 {
		t.Fatalf("assign: %v changed=%v audit=%d", err, changed, w.auditCount("customer.update"))
	}
	if agentETag(w, w.devA) == etag {
		t.Fatal("the agent's effective policy did not change after the assignment")
	}
	// another customer's own policy is not assignable, with the same answer as an unknown id
	pol := must[PolicyRecord](t)(w.s.CreatePolicyAudited(w.ctx, ScopeAll(), PolicyInput{Scope: "customer", CustomerID: w.custB.ID, Name: "Bravo only"}, w.actor()))
	_, err = w.s.PreviewPolicyAssignment(w.ctx, ScopeAll(), w.custA.ID, pol.ID, w.admin.ID)
	_, err2 := w.s.PreviewPolicyAssignment(w.ctx, ScopeAll(), w.custA.ID, "01J9Y4K3A0B1C2D3E4F5G6H7ZZ", w.admin.ID)
	if fe := FieldErrors(err); len(fe) != 1 || fe[0].Field != "policy_id" || err.Error() != err2.Error() {
		t.Fatalf("foreign policy: %v / %v", err, err2)
	}
}

// ---- list ----

func TestW2APolicyListCountsOnlyInsideTheScope(t *testing.T) {
	w := seed(t)
	tpl := w.template("Workstation", nil)
	w.assign(w.custA.ID, tpl.ID)
	all := must[[]PolicySummary](t)(w.s.ListPolicySummaries(w.ctx, ScopeAll()))
	count := func(l []PolicySummary, id string) (int, int) {
		for _, p := range l {
			if p.ID == id {
				return p.Customers, p.Devices
			}
		}
		t.Fatalf("policy %s missing from the list", id)
		return 0, 0
	}
	if c, d := count(all, tpl.ID); c != 1 || d != 1 {
		t.Fatalf("template usage (all): %d customers %d devices", c, d)
	}
	if c, d := count(all, w.defaultID()); c != 1 || d != 1 {
		t.Fatalf("default usage (all): %d customers %d devices", c, d)
	}
	onlyA := must[[]PolicySummary](t)(w.s.ListPolicySummaries(w.ctx, ScopeOf(w.custA.ID)))
	if c, d := count(onlyA, tpl.ID); c != 1 || d != 1 {
		t.Fatalf("template usage (A): %d %d", c, d)
	}
	if c, d := count(onlyA, w.defaultID()); c != 0 || d != 0 {
		t.Fatalf("Alpha's scope must not count Bravo's device on the default policy: %d customers %d devices", c, d)
	}
	onlyB := must[[]PolicySummary](t)(w.s.ListPolicySummaries(w.ctx, ScopeOf(w.custB.ID)))
	if c, d := count(onlyB, tpl.ID); c != 0 || d != 0 {
		t.Fatalf("Bravo's scope must not learn that the template has a device: %d %d", c, d)
	}
}

// ---- sparse form ----

func TestW2ASparseSectionsRoundTrip(t *testing.T) {
	def := policy.Default(policy.ClassUnknown)
	target := def
	target.Schedule.IntervalMinutes = 240
	target.Schedule.Window = "Mon-Fri/8-18"
	target.SLA.StaleAfterHours = 24
	target.Selection.Excludes = []string{"@vm_images", "*.tmp"}
	target.Retention = policy.Retention{Rules: &policy.KeepCounts{KeepDaily: 14}, KeepWithin: "7d", KeepTag: []string{}}
	sp := must[map[string]json.RawMessage](t)(SparseSections(def, target))
	if string(sp["maintenance"]) != "{}" || string(sp["agent_permissions"]) != "{}" {
		t.Fatalf("untouched sections must be empty: %s %s", sp["maintenance"], sp["agent_permissions"])
	}
	if !strings.Contains(string(sp["retention"]), `"preset":null`) || strings.Contains(string(sp["schedule"]), "jitter") {
		t.Fatalf("sparse form: retention=%s schedule=%s", sp["retention"], sp["schedule"])
	}
	defSec := must[map[string]json.RawMessage](t)(policy.Encode(def))
	got := must[policy.Policy](t)(ComposePolicy(defSec, sp))
	a, _ := policy.Canonical(got)
	b, _ := policy.Canonical(target)
	if string(a) != string(b) {
		t.Fatalf("round trip differs:\n%s\n%s", a, b)
	}
}
