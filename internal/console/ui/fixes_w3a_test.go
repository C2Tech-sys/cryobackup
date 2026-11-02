package ui

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// Blind-verification fixes for M2-W3A.

// An action that starts with a dot has no family; the page must still render.
func TestW3AAuditFormSurvivesOddActionNames(t *testing.T) {
	e := newEnv(t)
	e.login()
	for _, act := range []string{".leading", "trailing.", "ünï.code", "single"} {
		e.appendAudit(store.AuditEvent{ActorType: "system", Action: act})
	}
	b := e.page("/audit", 200)
	mustContain(t, "odd actions", b, ".leading", "trailing.", "single")
	// The family list never offers a filter the store would refuse.
	if strings.Contains(b, `value="*.`) || strings.Contains(b, `value=".*"`) {
		t.Error("an empty family is offered as a filter")
	}
	mustContain(t, "family", b, `value="ünï.*"`, "Ünï (all)") // rune-safe capitalisation
}

// The settings page names a config.json value that the saved one overrides.
func TestW3ASettingsSaysWhenTheSavedValueOverridesConfig(t *testing.T) {
	e := newEnv(t)
	e.login()
	if strings.Contains(e.page("/settings", 200), "The saved value wins.") {
		t.Fatal("a note before anything was saved")
	}
	res, _ := e.post("/ui/settings", url.Values{"display_timezone": {"Asia/Tokyo"}, "refresh_interval_s": {"30"}}, false)
	if res.StatusCode != 303 {
		t.Fatalf("save: %d", res.StatusCode)
	}
	b := e.page("/settings", 200)
	mustContain(t, "override note", b, "The saved value wins.", "config.json says America/Chicago as the display time zone", "Asia/Tokyo")
}

// If what a repair drops cannot be worked out, the review is refused and nothing is saved.
func TestW3ARepairIsRefusedWhenItsDropsCannotBeListed(t *testing.T) {
	e := newEnv(t)
	e.signInAs("ad", "admin", nil)
	tpl := e.newTemplate("Cannot list", map[string]string{"sla": `{"stale_after_hours":96,"unseen_after_hours":250}`})
	if err := e.st.Write(ctx2a, func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE policies SET schedule = ? WHERE id = ?`, `{"interval_minutes":1}`, tpl.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	form := e.editorForm(tpl.ID)
	form.Set("f.schedule.interval_minutes", "240")
	// Healthy: the review lists the drop.
	_, review := e.post("/ui/policies/"+tpl.ID+"/impact", form, false)
	mustContain(t, "healthy review", review, "Not carried over")
	token := hiddenOfPage(review)

	orig := repairReviewImpl
	repairReviewImpl = func(*Service, context.Context, store.PolicyRecord, store.ProposedPolicy) ([]PolDropped, []PolChangeRow, bool) {
		return nil, nil, false
	}
	defer func() { repairReviewImpl = orig }()

	before := e.auditTotal()
	res, body := e.post("/ui/policies/"+tpl.ID+"/impact", form, false)
	if res.StatusCode != 500 || !strings.Contains(body, "Repair cannot be previewed") || strings.Contains(body, "Save ") {
		t.Fatalf("review with unknown drops: %d %.300s", res.StatusCode, body)
	}
	// A save carrying a valid preview token is refused as well.
	res, body = e.post("/ui/policies/"+tpl.ID, token, false)
	if res.StatusCode != 500 || !strings.Contains(body, "Repair cannot be saved") {
		t.Fatalf("save with unknown drops: %d %.300s", res.StatusCode, body)
	}
	cur, _ := e.st.GetPolicy(ctx2a, store.ScopeAll(), tpl.ID)
	if string(cur.Sections["schedule"]) != `{"interval_minutes":1}` || e.auditTotal() != before {
		t.Fatalf("something was saved: %s (audit %d -> %d)", cur.Sections["schedule"], before, e.auditTotal())
	}
}
