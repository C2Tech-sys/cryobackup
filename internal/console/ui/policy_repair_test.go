package ui

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/policy"
)

// M2-W3A item 5: a stored policy that does not compose is repaired from its own
// settings, not from the default alone.

func raw(m map[string]string) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	for k, v := range m {
		out[k] = json.RawMessage(v)
	}
	return out
}

func TestPlanRepairKeepsEverythingThatStillComposes(t *testing.T) {
	def, err := policy.Encode(policy.Default(policy.ClassUnknown))
	if err != nil {
		t.Fatal(err)
	}
	stored := raw(map[string]string{
		"schedule":  `{"interval_minutes":1,"jitter_minutes":7}`, // the interval is out of range, the jitter is fine
		"sla":       `{"stale_after_hours":96,"unseen_after_hours":250}`,
		"retention": `{"keep_within":"45d"}`,
	})
	if _, err := store.ComposePolicy(def, stored); err == nil {
		t.Fatal("the stored sections were meant not to compose")
	}
	plan, ok := planRepair(def, stored)
	if !ok {
		t.Fatal("no plan")
	}
	// Kept: the jitter, both SLA fields and the retention window, in the base policy.
	if plan.Base.Schedule.JitterMinutes != 7 || plan.Base.SLA.StaleAfterHours != 96 || plan.Base.SLA.UnseenAfterHours != 250 || plan.Base.Retention.KeepWithin != "45d" {
		t.Fatalf("the base lost settings: jitter %d stale %d unseen %d keep_within %q", plan.Base.Schedule.JitterMinutes, plan.Base.SLA.StaleAfterHours, plan.Base.SLA.UnseenAfterHours, plan.Base.Retention.KeepWithin)
	}
	if plan.Base.Schedule.IntervalMinutes == 1 {
		t.Fatal("the unusable interval was carried")
	}
	// Flagged: exactly the interval, with a reason and the stored value.
	got := map[string]repairItem{}
	for _, it := range plan.Items {
		got[it.Path] = it
	}
	if len(got) != 1 || got["schedule.interval_minutes"].Reason == "" || string(got["schedule.interval_minutes"].Stored) != "1" {
		t.Fatalf("items: %+v", plan.Items)
	}
	if strings.Contains(got["schedule.interval_minutes"].Reason, "\n") {
		t.Fatal("the reason must be one line")
	}
	// The kept sections compose by themselves.
	if _, err := store.ComposePolicy(def, plan.Kept); err != nil {
		t.Fatalf("the kept sections do not compose: %v", err)
	}
	// Items are in section order.
	if plan.Items[0].Path != "schedule.interval_minutes" {
		t.Fatalf("order: %+v", plan.Items)
	}
}

func TestPlanRepairGoesBelowTheSectionAndBelowTheField(t *testing.T) {
	def, _ := policy.Encode(policy.Default(policy.ClassUnknown))
	stored := raw(map[string]string{
		"retention": `{"keep_within":"30d","rules":{"keep_daily":7,"keep_weekly":-5,"keep_monthly":3}}`,
	})
	if _, err := store.ComposePolicy(def, stored); err == nil {
		t.Skip("the engine accepts this retention; nothing to repair")
	}
	plan, ok := planRepair(def, stored)
	if !ok {
		t.Fatal("no plan")
	}
	if plan.Base.Retention.Rules.KeepDaily != 7 || plan.Base.Retention.Rules.KeepMonthly != 3 || plan.Base.Retention.KeepWithin != "30d" {
		t.Fatalf("the other retention rules were lost: %+v", plan.Base.Retention)
	}
	if len(plan.Items) != 1 || !strings.HasPrefix(plan.Items[0].Path, "retention.rules.keep_weekly") {
		t.Fatalf("items: %+v", plan.Items)
	}
}

func TestPlanRepairOnAPolicyThatComposesKeepsItAll(t *testing.T) {
	def, _ := policy.Encode(policy.Default(policy.ClassUnknown))
	stored := raw(map[string]string{"sla": `{"stale_after_hours":96}`})
	plan, ok := planRepair(def, stored)
	// Called on a policy that composes (the caller never does), nothing is lost.
	if !ok || plan.Base.SLA.StaleAfterHours != 96 {
		t.Fatalf("plan: %v %+v", ok, plan)
	}
}

var jitterValue = regexp.MustCompile(`name="f\.schedule\.jitter_minutes"[^>]*value="7"`)
var staleValue = regexp.MustCompile(`name="f\.sla\.stale_after_hours"[^>]*value="96"`)

func TestW3ABrokenPolicyRepairKeepsItsOtherSettings(t *testing.T) {
	e := newEnv(t)
	e.signInAs("ad", "admin", nil)
	tpl := e.newTemplate("Repair me", map[string]string{"sla": `{"stale_after_hours":96,"unseen_after_hours":250}`, "schedule": `{"interval_minutes":700,"jitter_minutes":7}`})
	// Break one field of one section behind the store's back (an old build, a hand edit).
	if err := e.st.Write(ctx2a, func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE policies SET schedule = ? WHERE id = ?`, `{"interval_minutes":1,"jitter_minutes":7}`, tpl.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	// The editor starts from the policy's own settings and flags the one it could not use.
	ed := e.page("/policies/"+tpl.ID, 200)
	mustContain(t, "broken editor", ed, "This policy does not compose with the default policy.", "1 stored setting could not be used", "1 NOT USABLE",
		"Stored value not usable.", "Everything else this policy carries is kept", `class="banner warn dense repairlist"`, "Stored: 1")
	if !jitterValue.MatchString(ed) || !staleValue.MatchString(ed) {
		t.Fatalf("the editor lost the settings that still work (jitter 7: %v, stale 96: %v)", jitterValue.MatchString(ed), staleValue.MatchString(ed))
	}
	if regexp.MustCompile(`name="f\.schedule\.interval_minutes"[^>]*value="1"`).MatchString(ed) {
		t.Fatal("the unusable interval is shown as if it were a value")
	}

	// The review lists what is dropped and measures the change against what was kept.
	form := e.editorForm(tpl.ID)
	if form.Get("f.schedule.jitter_minutes") != "7" || form.Get("f.sla.stale_after_hours") != "96" {
		t.Fatalf("the form values lost settings: jitter %q stale %q", form.Get("f.schedule.jitter_minutes"), form.Get("f.sla.stale_after_hours"))
	}
	form.Set("f.schedule.interval_minutes", "240")
	res, review := e.post("/ui/policies/"+tpl.ID+"/impact", form, false)
	if res.StatusCode != 200 {
		t.Fatalf("repair preview: %d %.300s", res.StatusCode, review)
	}
	mustContain(t, "repair review", review, "repairs it", "the parts of it that still compose are kept", "Not carried over", "Stored settings that do not work",
		labelOf("schedule.interval_minutes"), ">240<", "Why it is dropped")
	if m := regexp.MustCompile(`(?s)What changes.*?</table>`).FindString(review); strings.Contains(m, labelOf("schedule.jitter_minutes")) || strings.Contains(m, labelOf("sla.stale_after_hours")) {
		t.Errorf("the changes list the kept settings as changes:\n%s", m)
	}
	if res, _ = e.post("/ui/policies/"+tpl.ID, hiddenOfPage(review), false); res.StatusCode != 303 {
		t.Fatalf("repair save: %d", res.StatusCode)
	}

	// Saved: repaired, and nothing else was lost.
	cur, err := e.st.GetPolicy(ctx2a, store.ScopeAll(), tpl.ID)
	if err != nil {
		t.Fatal(err)
	}
	def, _ := e.svc.defaultSections(ctx2a)
	pol, err := store.ComposePolicy(def, cur.Sections)
	if err != nil {
		t.Fatalf("still broken: %v (%s)", err, cur.Sections["schedule"])
	}
	if pol.Schedule.IntervalMinutes != 240 || pol.Schedule.JitterMinutes != 7 || pol.SLA.StaleAfterHours != 96 || pol.SLA.UnseenAfterHours != 250 {
		t.Fatalf("the repaired policy lost settings: interval %d jitter %d stale %d unseen %d", pol.Schedule.IntervalMinutes, pol.Schedule.JitterMinutes, pol.SLA.StaleAfterHours, pol.SLA.UnseenAfterHours)
	}
	if strings.Contains(e.page("/policies/"+tpl.ID, 200), "does not compose with the default policy") {
		t.Fatal("the editor still says it is broken")
	}
}

// A repair that changes nothing about the broken part still drops it, and says so.
func TestW3ARepairReviewListsADropEvenWhenTheFormIsUntouched(t *testing.T) {
	e := newEnv(t)
	e.signInAs("ad", "admin", nil)
	tpl := e.newTemplate("Untouched", map[string]string{"sla": `{"stale_after_hours":96,"unseen_after_hours":250}`})
	if err := e.st.Write(ctx2a, func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE policies SET schedule = ? WHERE id = ?`, `{"interval_minutes":1}`, tpl.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	res, review := e.post("/ui/policies/"+tpl.ID+"/impact", e.editorForm(tpl.ID), false)
	if res.StatusCode != 200 {
		t.Fatalf("preview: %d", res.StatusCode)
	}
	mustContain(t, "untouched repair", review, "Not carried over", labelOf("schedule.interval_minutes"), "the default")
	if res, _ = e.post("/ui/policies/"+tpl.ID, hiddenOfPage(review), false); res.StatusCode != 303 {
		t.Fatalf("save: %d", res.StatusCode)
	}
	cur, _ := e.st.GetPolicy(ctx2a, store.ScopeAll(), tpl.ID)
	def, _ := e.svc.defaultSections(ctx2a)
	pol, err := store.ComposePolicy(def, cur.Sections)
	if err != nil || pol.SLA.StaleAfterHours != 96 {
		t.Fatalf("repaired policy: %v stale %d", err, pol.SLA.StaleAfterHours)
	}
}
