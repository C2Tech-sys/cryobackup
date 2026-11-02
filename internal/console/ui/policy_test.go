package ui

import (
	"context"
	"encoding/json"
	"errors"
	"html"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// ---- helpers (M2-W2A) ----

var ctx2a = context.Background()

func (e *env) policyByName(name string) store.PolicyRecord {
	e.t.Helper()
	l, err := e.st.ListPolicies(ctx2a, store.ScopeAll())
	if err != nil {
		e.t.Fatal(err)
	}
	for _, p := range l {
		if p.Name == name {
			return p
		}
	}
	e.t.Fatalf("no policy %q", name)
	return store.PolicyRecord{}
}

// editorForm is what the editor would post for a stored policy, unchanged.
func (e *env) editorForm(id string) url.Values {
	e.t.Helper()
	rec, err := e.st.GetPolicy(ctx2a, store.ScopeAll(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	base, _, _, err := e.svc.policyBase(ctx2a, rec)
	if err != nil && !errors.Is(err, errPolicyBroken) {
		e.t.Fatal(err)
	}
	tree, err := treeOf(base)
	if err != nil {
		e.t.Fatal(err)
	}
	v := formFromDoc(tree)
	v.Set("name", rec.Name)
	v.Set("description", rec.Description)
	return v
}

var hiddenInput = regexp.MustCompile(`<input type="hidden" name="([^"]*)" value="([^"]*)">`)

// hiddenOfPage reads the hidden inputs of a rendered page, as a browser would post them.
func hiddenOfPage(body string) url.Values {
	v := url.Values{}
	for _, m := range hiddenInput.FindAllStringSubmatch(body, -1) {
		if m[1] == "csrf_token" {
			continue
		}
		v.Add(html.UnescapeString(m[1]), html.UnescapeString(m[2]))
	}
	return v
}

var helpSpan = regexp.MustCompile(`(?s)<span class="help"[^>]*>.*?</span>`)

// pathFree strips the help sentences, which give example paths, before the path check.
func pathFree(body string) string { return helpSpan.ReplaceAllString(body, "") }

func (e *env) policyIDs() (def, summit store.PolicyRecord) {
	return e.policyByName("Default"), e.policyByName("Summit Workstation")
}

func (e *env) newTemplate(name string, sec map[string]string) store.PolicyRecord {
	e.t.Helper()
	in := store.PolicyInput{Scope: "template", Name: name, Sections: map[string]json.RawMessage{}}
	for k, v := range sec {
		in.Sections[k] = json.RawMessage(v)
	}
	admin, err := e.st.GetAdminByUsername(ctx2a, "admin")
	if err != nil {
		e.t.Fatal(err)
	}
	rec, err := e.st.CreatePolicyAudited(ctx2a, store.ScopeAll(), in, store.AuditActor{AdminID: admin.ID, Username: "admin", Method: "web"})
	if err != nil {
		e.t.Fatal(err)
	}
	return rec
}

// ---- tests ----

func TestW2APagesRenderInFixturesMode(t *testing.T) {
	e := newEnv(t)
	e.login()
	def, summit := e.policyIDs()
	tpl := e.newTemplate("Laptops", map[string]string{"conditions": `{"battery_floor_pct":30}`})
	pages := map[string]string{
		"policies":             "/policies",
		"default editor":       "/policies/" + def.ID,
		"customer policy":      "/policies/" + summit.ID,
		"template editor":      "/policies/" + tpl.ID,
		"new policy":           "/policies/new",
		"new customer policy":  "/policies/new?customer=" + e.customerID("Northwind Dental"),
		"delete default":       "/policies/" + def.ID + "/delete",
		"delete assigned":      "/policies/" + summit.ID + "/delete",
		"delete unassigned":    "/policies/" + tpl.ID + "/delete",
		"customer policy tab":  "/customers/" + e.customerID("Summit Eye Care") + "/policy",
		"default customer tab": "/customers/" + e.customerID("Northwind Dental") + "/policy",
		"empty customer tab":   "/customers/" + e.customerID("Blue Ridge Accounting") + "/policy",
		"overridden device":    "/devices/" + e.dev["DR-OFFICE-PC"] + "/policy",
		"plain device":         "/devices/" + e.dev["FRONTDESK-01"] + "/policy",
		"device via tab route": "/devices/" + e.dev["FRONTDESK-01"] + "/policy?saved=override",
		"policies after saves": "/policies?saved=deleted",
	}
	for name, path := range pages {
		status := 200
		body := e.page(path, status)
		if strings.Contains(body, "ZgotmplZ") {
			t.Errorf("%s: html/template rejected a value (ZgotmplZ)", name)
		}
		if absoluteURL.MatchString(body) {
			t.Errorf("%s: an absolute URL reference (no CDN, no third party)", name)
		}
		if m := pathLike.FindString(pathFree(body)); m != "" {
			t.Errorf("%s: shows a path-like string %q", name, m)
		}
		if !strings.Contains(body, `id="freshness"`) {
			t.Errorf("%s: no freshness stamp", name)
		}
	}
	mustContain(t, "policy list", e.page("/policies", 200), "Policies", "Default", "Summit Workstation", "DEFAULT", "CUSTOMER", "v1", `href="/policies/new"`, `href="/policies" aria-current="page"`)
	ed := e.page("/policies/"+def.ID, 200)
	mustContain(t, "default editor", ed, `name="f.schedule.interval_minutes"`, `name="f.schedule.window"`, `hx-post="/ui/policy/window/validate"`, "Published exclusion baseline", "READ-ONLY",
		"Recycle.Bin", "hiberfil.sys", `name="f.retention.mode"`, `name="f.retention.keep_within"`, `name="f.sla.stale_after_hours"`, `name="f.selection.excludes.custom"`,
		"Review changes", "Revisions", "*/0-24 (always)", "cannot be deleted")
	mustContain(t, "customer policy editor", e.page("/policies/"+summit.ID, 200), "Summit Eye Care", "Mon-Fri/7-19;Sat/8-14", "only settings that differ are stored", "hours per week allowed")
	// a customer's Policy tab and a device's, with their sources
	tab := e.page("/customers/"+e.customerID("Summit Eye Care")+"/policy", 200)
	mustContain(t, "customer tab", tab, "Summit Workstation", "Assigned policy", "Change the assignment", "Review the change", "New policy for this customer", "POLICY", "DEFAULT",
		"1 device has its own override", `aria-current="page">Policy`)
	dev := e.page("/devices/"+e.dev["DR-OFFICE-PC"]+"/policy", 200)
	mustContain(t, "device tab", dev, "Summit Workstation", "This device has an override", "large initial dataset", "OVERRIDE", "POLICY", "DEFAULT", "Override settings for this device",
		`name="o.sla.max_run_hours"`, `name="reason"`, "Longest normal run", `aria-current="page">Policy`)
}

func TestW2AEditorGrammarErrorsSurfaceOnTheForm(t *testing.T) {
	e := newEnv(t)
	e.signInAs("ad", "admin", nil)
	_, summit := e.policyIDs()
	before := e.auditTotal()
	form := e.editorForm(summit.ID)
	form.Set("f.schedule.window", "Mon-Fri/18-8x")
	form.Set("f.schedule.interval_minutes", "1")
	form.Set("f.throttle.windows", "10@Mon-Fri/8-18\nnonsense")
	form.Set("f.retention.keep_within", "")
	form.Set("f.throttle.upload_kibps", "50%")
	form.Set("f.selection.roots", "relative\\path")
	form.Set("name", "Summit renamed")
	res, body := e.post("/ui/policies/"+summit.ID+"/impact", form, false)
	if res.StatusCode != 400 {
		t.Fatalf("invalid editor: %d", res.StatusCode)
	}
	mustContain(t, "errors", body,
		"Not a valid window, at character", // the parser's own error, with its position
		"must be between 15 and 10080",     // the engine's own error for the interval
		"Line 2: write it as rate@window",  // the throttle form
		"Keep within is required",
		"Percent rates are not supported", // ParseRate's own words
		"must be an absolute Windows path",
		`aria-invalid="true"`, `role="alert"`, "Fix ", "Nothing was saved.",
		`value="Mon-Fri/18-8x"`, `value="Summit renamed"`) // every entered value is kept
	if strings.Contains(body, `name="impact_token"`) {
		t.Fatal("a refused form must not carry a preview token")
	}
	if e.auditTotal() != before {
		t.Fatal("a refused form wrote an audit row")
	}
	if cur, _ := e.st.GetPolicy(ctx2a, store.ScopeAll(), summit.ID); cur.Version != summit.Version {
		t.Fatal("a refused form changed the policy")
	}
}

func TestW2APreviewShowsTheBlastRadiusThenSavesExactlyIt(t *testing.T) {
	e := newEnv(t)
	e.signInAs("ad", "admin", nil)
	_, summit := e.policyIDs()
	before := e.auditTotal()
	form := e.editorForm(summit.ID)
	form.Set("f.schedule.window", "Mon-Fri/6-20")
	form.Set("f.sla.stale_after_hours", "60")
	form.Set("note", "longer days")
	res, review := e.post("/ui/policies/"+summit.ID+"/impact", form, false)
	if res.StatusCode != 200 {
		t.Fatalf("preview: %d %s", res.StatusCode, review[:min(len(review), 400)])
	}
	// the number of devices and the fields, old to new, before anything is saved
	mustContain(t, "review", review, "This change affects", "device", "Summit Eye Care", "What changes", "Backup window", "Mon-Fri/7-19;Sat/8-14", "Mon-Fri/6-20",
		"Stale after", "72", "60", `name="impact_token"`, "Edit again", "Save Summit Workstation (version 2)", "Nothing is saved until you confirm")
	if cur, _ := e.st.GetPolicy(ctx2a, store.ScopeAll(), summit.ID); cur.Version != summit.Version || e.auditTotal() != before {
		t.Fatal("a preview must not change the policy or write an audit row")
	}
	// confirm with exactly what the review page carries
	conf := hiddenOfPage(review)
	if conf.Get("impact_token") == "" || conf.Get("f.schedule.window") != "Mon-Fri/6-20" {
		t.Fatalf("hidden fields: %v", conf)
	}

	// a tampered field is refused: the token binds what was previewed
	bad := url.Values{}
	for k, v := range conf {
		bad[k] = append([]string(nil), v...)
	}
	bad.Set("f.sla.stale_after_hours", "36")
	res, body := e.post("/ui/policies/"+summit.ID, bad, false)
	if res.StatusCode != 409 {
		t.Fatalf("tampered confirm: %d", res.StatusCode)
	}
	mustContain(t, "tampered", body, "Preview again", "not the ones that were previewed", `value="36"`)
	if cur, _ := e.st.GetPolicy(ctx2a, store.ScopeAll(), summit.ID); cur.Version != summit.Version {
		t.Fatal("a tampered confirm saved")
	}

	// somebody else saves first: the preview is stale and the confirm is refused
	other := e.editorForm(summit.ID)
	other.Set("f.sla.stale_after_hours", "50")
	_, rv2 := e.post("/ui/policies/"+summit.ID+"/impact", other, false)
	if res, _ := e.post("/ui/policies/"+summit.ID, hiddenOfPage(rv2), false); res.StatusCode != 303 {
		t.Fatalf("the first confirm: %d", res.StatusCode)
	}
	res, body = e.post("/ui/policies/"+summit.ID, conf, false)
	if res.StatusCode != 409 {
		t.Fatalf("stale confirm: %d", res.StatusCode)
	}
	mustContain(t, "stale", body, "Preview again", "changed")
	if cur, _ := e.st.GetPolicy(ctx2a, store.ScopeAll(), summit.ID); cur.Version != summit.Version+1 || strings.Contains(string(cur.Sections["schedule"]), "6-20") {
		t.Fatalf("the stale confirm must not apply: v%d %s", cur.Version, cur.Sections["schedule"])
	}

	// a fresh preview of the original change applies
	_, rv3 := e.post("/ui/policies/"+summit.ID+"/impact", form, false)
	totalBefore := e.auditTotal()
	res, _ = e.post("/ui/policies/"+summit.ID, hiddenOfPage(rv3), false)
	if res.StatusCode != 303 || res.Header.Get("Location") != "/policies/"+summit.ID+"?saved=1" {
		t.Fatalf("confirm: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	cur, _ := e.st.GetPolicy(ctx2a, store.ScopeAll(), summit.ID)
	if cur.Version != summit.Version+2 || !strings.Contains(string(cur.Sections["schedule"]), "Mon-Fri/6-20") || !strings.Contains(string(cur.Sections["sla"]), "60") {
		t.Fatalf("saved policy: v%d %s %s", cur.Version, cur.Sections["schedule"], cur.Sections["sla"])
	}
	if rows := e.auditRows("policy.update", summit.ID); len(rows) != 2 || e.auditTotal() != totalBefore+1 {
		t.Fatalf("want exactly one policy.update row per save: %d rows, total %d -> %d", len(rows), totalBefore, e.auditTotal())
	}
	page := e.page("/policies/"+summit.ID+"?saved=1", 200)
	mustContain(t, "saved", page, "Policy saved.", "ad", "longer days", "v3", "CURRENT")
}

func TestW2ADefaultEditPreviewReachesEveryCustomer(t *testing.T) {
	e := newEnv(t)
	e.signInAs("ad", "admin", nil)
	def, _ := e.policyIDs()
	form := e.editorForm(def.ID)
	form.Set("f.sla.first_backup_grace_hours", "96")
	res, review := e.post("/ui/policies/"+def.ID+"/impact", form, false)
	if res.StatusCode != 200 {
		t.Fatalf("preview: %d", res.StatusCode)
	}
	mustContain(t, "default review", review, "This change affects", "inherit from the default policy", "First-backup grace", "72", "96", "Northwind Dental", "Summit Eye Care")
	// nothing changed: nothing to save
	_, same := e.post("/ui/policies/"+def.ID+"/impact", e.editorForm(def.ID), false)
	mustContain(t, "unchanged", same, "Nothing to save")
	if strings.Contains(same, "Save Default") {
		t.Fatal("an unchanged form must not offer a save")
	}
}

func TestW2ANewPolicyAndDelete(t *testing.T) {
	e := newEnv(t)
	e.signInAs("ad", "admin", nil)
	north := e.customerID("Northwind Dental")
	form := e.editorForm(e.policyByName("Default").ID)
	form.Set("name", "Laptops")
	form.Set("description", "Battery aware")
	form.Set("f.conditions.battery_floor_pct", "30")
	before := e.auditTotal()
	res, body := e.post("/ui/policies", form, false)
	if res.StatusCode != 303 || !strings.HasSuffix(res.Header.Get("Location"), "?saved=created") {
		t.Fatalf("create: %d %s", res.StatusCode, body[:min(len(body), 300)])
	}
	tpl := e.policyByName("Laptops")
	if tpl.Scope != "template" || !strings.Contains(string(tpl.Sections["conditions"]), "30") || string(tpl.Sections["maintenance"]) != "{}" {
		t.Fatalf("stored template: %s %s", tpl.Scope, tpl.Sections["conditions"])
	}
	if len(e.auditRows("policy.create", tpl.ID)) != 1 || e.auditTotal() != before+1 {
		t.Fatal("create must write exactly one audit row")
	}
	// the duplicate name is a field error and keeps the form
	res, body = e.post("/ui/policies", form, false)
	if res.StatusCode != 400 {
		t.Fatalf("duplicate: %d", res.StatusCode)
	}
	mustContain(t, "duplicate", body, "Another policy already has this name.", `value="Laptops"`)

	// assigned: the delete page says where, and refuses
	e.assignThroughUI(north, tpl.ID)
	blocked := e.page("/policies/"+tpl.ID+"/delete", 200)
	mustContain(t, "blocked", blocked, "still in use", "Northwind Dental", `href="/customers/`+north+`/policy"`, "Policy tab")
	if strings.Contains(blocked, "danger-solid") {
		t.Fatal("a blocked delete must not offer the button")
	}
	res, _ = e.post("/ui/policies/"+tpl.ID+"/delete", url.Values{"impact_token": {"forged"}}, false)
	if res.StatusCode != 409 {
		t.Fatalf("delete while assigned: %d", res.StatusCode)
	}
	if _, err := e.st.GetPolicy(ctx2a, store.ScopeAll(), tpl.ID); err != nil {
		t.Fatal("a refused delete deleted the policy")
	}
	e.assignThroughUI(north, "")
	page := e.page("/policies/"+tpl.ID+"/delete", 200)
	mustContain(t, "delete page", page, "This delete affects no devices", "Delete Laptops", "danger-solid")
	tok := hiddenOfPage(page)
	if res, _ = e.post("/ui/policies/"+tpl.ID+"/delete", url.Values{"impact_token": {"forged"}}, false); res.StatusCode != 409 {
		t.Fatalf("forged token: %d", res.StatusCode)
	}
	res, _ = e.post("/ui/policies/"+tpl.ID+"/delete", tok, false)
	if res.StatusCode != 303 || res.Header.Get("Location") != "/policies?saved=deleted" {
		t.Fatalf("delete: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	if len(e.auditRows("policy.delete", tpl.ID)) != 1 {
		t.Fatal("delete must write exactly one audit row")
	}
}

// assignThroughUI assigns a policy to a customer the way the Policy tab does: preview, then confirm.
func (e *env) assignThroughUI(customerID, policyID string) {
	e.t.Helper()
	res, review := e.post("/ui/customers/"+customerID+"/policy/impact", url.Values{"policy_id": {policyID}}, false)
	if res.StatusCode != 200 {
		e.t.Fatalf("assignment preview: %d %s", res.StatusCode, review[:min(len(review), 300)])
	}
	conf := hiddenOfPage(review)
	if conf.Get("impact_token") == "" {
		// an unchanged assignment has nothing to confirm
		return
	}
	res, body := e.post("/ui/customers/"+customerID+"/policy", conf, false)
	if res.StatusCode != 303 {
		e.t.Fatalf("assignment: %d %s", res.StatusCode, body[:min(len(body), 300)])
	}
}

func TestW2ACustomerPolicyTabAssignsWithTheBlastRadiusFirst(t *testing.T) {
	e := newEnv(t)
	e.signInAs("ad", "admin", nil)
	north := e.customerID("Northwind Dental")
	tpl := e.newTemplate("Strict", map[string]string{"sla": `{"stale_after_hours":24,"unseen_after_hours":100}`})
	before := e.auditTotal()
	res, review := e.post("/ui/customers/"+north+"/policy/impact", url.Values{"policy_id": {tpl.ID}}, false)
	if res.StatusCode != 200 {
		t.Fatalf("preview: %d", res.StatusCode)
	}
	mustContain(t, "assign review", review, "This assignment affects", "devices of Northwind Dental move to Strict", "Stale after", "72", "24", "Assign Strict", `name="impact_token"`)
	if e.auditTotal() != before {
		t.Fatal("a preview wrote an audit row")
	}
	if c, _ := e.st.GetCustomer(ctx2a, store.ScopeAll(), north); c.PolicyID != "" {
		t.Fatal("a preview assigned the policy")
	}
	// confirm without the token: nothing changes
	if res, body := e.post("/ui/customers/"+north+"/policy", url.Values{"policy_id": {tpl.ID}}, false); res.StatusCode != 409 {
		t.Fatalf("no token: %d %s", res.StatusCode, body[:min(len(body), 200)])
	}
	res, _ = e.post("/ui/customers/"+north+"/policy", hiddenOfPage(review), false)
	if res.StatusCode != 303 || res.Header.Get("Location") != "/customers/"+north+"/policy?saved=assigned" {
		t.Fatalf("assign: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	if c, _ := e.st.GetCustomer(ctx2a, store.ScopeAll(), north); c.PolicyID != tpl.ID {
		t.Fatal("the confirmed assignment did not stick")
	}
	if rows := e.auditRows("customer.update", north); len(rows) != 1 || e.auditTotal() != before+1 {
		t.Fatalf("assign must write exactly one audit row: %d, total %d -> %d", len(rows), before, e.auditTotal())
	}
	mustContain(t, "after", e.page("/customers/"+north+"/policy?saved=assigned", 200), "Policy assigned.", "Strict", "Template · version 1")
	// another customer's own policy cannot be assigned, and says so plainly
	_, summit := e.policyIDs()
	res, body := e.post("/ui/customers/"+north+"/policy/impact", url.Values{"policy_id": {summit.ID}}, false)
	if res.StatusCode != 400 {
		t.Fatalf("foreign policy: %d", res.StatusCode)
	}
	mustContain(t, "foreign", body, "Choose one of the listed policies")
}

func TestW2AOversizedPostsAreRefusedBeforeAnythingIsParsed(t *testing.T) {
	e := newEnv(t)
	e.signInAs("ad", "admin", nil)
	before := e.auditTotal()
	big := url.Values{"f.schedule.window": {strings.Repeat("a", 20<<10)}} // over the 16 KiB cap of the validator
	res, _ := e.post("/ui/policy/window/validate", big, true)
	if res.StatusCode != 413 {
		t.Fatalf("oversized window check: %d", res.StatusCode)
	}
	// 413 comes first: no CSRF token and an oversized body is a 413, not a 403 after a parse
	res, _ = e.do("POST", "/ui/policy/window/validate", big, "Origin", e.srv.URL)
	if res.StatusCode != 413 {
		t.Fatalf("oversized post without a token: %d", res.StatusCode)
	}
	// every console write has the global cap (1 MiB)
	huge := url.Values{"name": {strings.Repeat("n", 2<<20)}}
	if res, _ = e.post("/ui/customers", huge, false); res.StatusCode != 413 {
		t.Fatalf("2 MiB customer post: %d", res.StatusCode)
	}
	def, _ := e.policyIDs()
	policyBig := e.editorForm(def.ID)
	policyBig.Set("description", strings.Repeat("d", 300<<10)) // over the 256 KiB policy form cap
	if res, _ = e.post("/ui/policies/"+def.ID+"/impact", policyBig, false); res.StatusCode != 413 {
		t.Fatalf("oversized policy form: %d", res.StatusCode)
	}
	if e.auditTotal() != before {
		t.Fatal("a refused post wrote an audit row")
	}
	// a normal post still works
	if res, _ = e.post("/ui/policy/window/validate", url.Values{"f.schedule.window": {"*/0-24"}}, true); res.StatusCode != 200 {
		t.Fatalf("normal window check: %d", res.StatusCode)
	}
}

func TestW2AWindowValidatorIsRateLimitedPerSession(t *testing.T) {
	e := newEnv(t)
	e.signInAs("ad", "admin", nil)
	limited := 0
	for i := 0; i < 60; i++ { // the test clock does not move, so the burst of 40 is all there is
		res, _ := e.post("/ui/policy/window/validate", url.Values{"f.schedule.window": {"*/0-24"}}, true)
		if res.StatusCode == 429 {
			limited++
			if res.Header.Get("Retry-After") == "" {
				t.Fatal("429 without Retry-After")
			}
		}
	}
	if limited < 15 {
		t.Fatalf("only %d of 60 checks were limited", limited)
	}
	// another session has its own budget
	e.signInAs("ad2", "admin", nil)
	if res, _ := e.post("/ui/policy/window/validate", url.Values{"f.schedule.window": {"*/0-24"}}, true); res.StatusCode != 200 {
		t.Fatalf("a second session: %d", res.StatusCode)
	}
}

func TestW2AUnchangedFormIsNothingToSaveAndKeepsPins(t *testing.T) {
	e := newEnv(t)
	e.signInAs("ad", "admin", nil)
	tpl := e.newTemplate("Pinned", map[string]string{"sla": `{"stale_after_hours":72}`}) // equal to the default's own value
	before := e.auditTotal()
	res, review := e.post("/ui/policies/"+tpl.ID+"/impact", e.editorForm(tpl.ID), false)
	if res.StatusCode != 200 {
		t.Fatalf("preview: %d", res.StatusCode)
	}
	mustContain(t, "unchanged", review, "Nothing to save")
	if strings.Contains(review, "Save Pinned") {
		t.Fatal("an unchanged form must not offer a save")
	}
	res, _ = e.post("/ui/policies/"+tpl.ID, hiddenOfPage(review), false)
	cur, _ := e.st.GetPolicy(ctx2a, store.ScopeAll(), tpl.ID)
	if res.StatusCode != 303 || cur.Version != tpl.Version || !strings.Contains(string(cur.Sections["sla"]), "72") || e.auditTotal() != before {
		t.Fatalf("an unchanged confirm wrote something: %d v%d %s audit %d/%d", res.StatusCode, cur.Version, cur.Sections["sla"], e.auditTotal(), before)
	}
	// changing something else keeps the pin
	form := e.editorForm(tpl.ID)
	form.Set("f.sla.unseen_after_hours", "200")
	_, rv := e.post("/ui/policies/"+tpl.ID+"/impact", form, false)
	if res, _ = e.post("/ui/policies/"+tpl.ID, hiddenOfPage(rv), false); res.StatusCode != 303 {
		t.Fatalf("save: %d", res.StatusCode)
	}
	cur, _ = e.st.GetPolicy(ctx2a, store.ScopeAll(), tpl.ID)
	if !strings.Contains(string(cur.Sections["sla"]), `"stale_after_hours":72`) || !strings.Contains(string(cur.Sections["sla"]), "200") {
		t.Fatalf("the pin was dropped: %s", cur.Sections["sla"])
	}
}

func TestW2ABrokenPolicyIsAnAlarmNotADeadEnd(t *testing.T) {
	e := newEnv(t)
	e.signInAs("ad", "admin", nil)
	_, summit := e.policyIDs()
	custSummit := e.customerID("Summit Eye Care")
	if err := e.st.Write(ctx2a, func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE policies SET schedule = ? WHERE id = ?`, `{"interval_minutes":1}`, summit.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	tab := e.page("/customers/"+custSummit+"/policy", 200)
	mustContain(t, "broken tab", tab, "does not compose with the default policy", "Change the assignment", "Review the change")
	ed := e.page("/policies/"+summit.ID, 200)
	mustContain(t, "broken editor", ed, "This policy does not compose with the default policy.", `name="f.schedule.interval_minutes"`)
	// the repair: the form starts from the default; saving it reviews as a repair and applies
	form := e.editorForm(summit.ID) // composes? it falls back to the default
	form.Set("f.schedule.interval_minutes", "240")
	res, review := e.post("/ui/policies/"+summit.ID+"/impact", form, false)
	if res.StatusCode != 200 {
		t.Fatalf("repair preview: %d %s", res.StatusCode, review[:min(len(review), 300)])
	}
	mustContain(t, "repair", review, "repairs it")
	if res, _ = e.post("/ui/policies/"+summit.ID, hiddenOfPage(review), false); res.StatusCode != 303 {
		t.Fatalf("repair save: %d", res.StatusCode)
	}
	if strings.Contains(e.page("/customers/"+custSummit+"/policy", 200), "does not compose with the default policy") {
		t.Fatal("still broken after the repair")
	}
}

func TestW2ACustomerSettingsFormCannotAssignAPolicy(t *testing.T) {
	e := newEnv(t)
	e.signInAs("ad", "admin", nil)
	harbor := e.customerID("Harbor Family Practice")
	tpl := e.newTemplate("Bypass", nil)
	settings := e.page("/customers/"+harbor+"/settings", 200)
	if strings.Contains(settings, `name="policy_id"`) {
		t.Fatal("the edit form still has a policy select")
	}
	mustContain(t, "settings", settings, `href="/customers/`+harbor+`/policy">Policy tab`, "Default policy", "Name (required)")
	// a post that still carries policy_id (an old page, a script) saves the rest and ignores it
	before := e.auditTotal()
	res, _ := e.post("/ui/customers/"+harbor, url.Values{"name": {"Harbor Family Practice"}, "slug": {"harbor-family-practice"}, "notes": {"bypass attempt"}, "policy_id": {tpl.ID}}, false)
	if res.StatusCode != 303 {
		t.Fatalf("settings post: %d", res.StatusCode)
	}
	c, _ := e.st.GetCustomer(ctx2a, store.ScopeAll(), harbor)
	if c.PolicyID != "" || c.Notes != "bypass attempt" {
		t.Fatalf("policy_id %q notes %q: the policy must be untouched and the note saved", c.PolicyID, c.Notes)
	}
	if rows := e.auditRows("customer.update", harbor); len(rows) != 1 || e.auditTotal() != before+1 || strings.Contains(string(rows[0].Details), "policy_id") {
		t.Fatalf("audit: %+v", rows)
	}
	// the create form keeps its initial policy: a new customer has no devices to preview
	mustContain(t, "create form", e.page("/ui/customers/new", 200), `name="policy_id"`)
	res, _ = e.post("/ui/customers", url.Values{"name": {"Initial Co"}, "policy_id": {tpl.ID}}, false)
	if res.StatusCode != 303 {
		t.Fatalf("create: %d", res.StatusCode)
	}
	// a read-only reader of the form sees the policy but is not pointed at the admin tab
	e.signInAs("op", "operator", nil)
	op := e.page("/customers/"+harbor+"/settings", 200)
	if strings.Contains(op, `name="policy_id"`) || strings.Contains(op, "/policy\">Policy tab") {
		t.Fatal("an operator is offered a policy control or the admin tab")
	}
}

func TestW2ADeviceOverrideThroughTheUI(t *testing.T) {
	e := newEnv(t)
	e.signInAs("ad", "admin", nil)
	dev := e.dev["FRONTDESK-01"]
	tab := "/devices/" + dev + "/policy"
	before := e.auditTotal()
	// no reason: refused, nothing written, values kept
	form := url.Values{"o.sla.stale_after_hours": {"1"}, "f.sla.stale_after_hours": {"36"}, "reason": {""}}
	res, body := e.post("/ui/devices/"+dev+"/override", form, false)
	if res.StatusCode != 400 {
		t.Fatalf("no reason: %d", res.StatusCode)
	}
	mustContain(t, "no reason", body, "Say why this device differs", `value="36"`, `aria-invalid="true"`)
	// nothing ticked: refused
	if res, body = e.post("/ui/devices/"+dev+"/override", url.Values{"reason": {"x"}}, false); res.StatusCode != 400 {
		t.Fatalf("nothing ticked: %d", res.StatusCode)
	}
	mustContain(t, "nothing ticked", body, "Choose at least one setting")
	// a value the engine rejects is shown on its field
	bad := url.Values{"o.schedule.window": {"1"}, "f.schedule.window": {"Mon/9-5x"}, "reason": {"x"}}
	if res, body = e.post("/ui/devices/"+dev+"/override", bad, false); res.StatusCode != 400 {
		t.Fatalf("bad window: %d", res.StatusCode)
	}
	mustContain(t, "bad window", body, "Not a valid window, at character", `value="Mon/9-5x"`)
	if e.auditTotal() != before {
		t.Fatal("a refused override wrote an audit row")
	}

	form = url.Values{"o.sla.stale_after_hours": {"1"}, "f.sla.stale_after_hours": {"36"}, "o.schedule.interval_minutes": {"1"}, "f.schedule.interval_minutes": {"360"},
		"f.sla.unseen_after_hours": {"999"}, // not ticked: ignored
		"reason":                   {"busy file server"}, "expires_at": {"2027-01-05T17:00"}}
	res, _ = e.post("/ui/devices/"+dev+"/override", form, false)
	if res.StatusCode != 303 || res.Header.Get("Location") != tab+"?saved=override" {
		t.Fatalf("set: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	ov, err := e.st.GetDeviceOverride(ctx2a, store.ScopeAll(), dev)
	if err != nil || ov == nil || ov.Reason != "busy file server" || ov.ExpiresAt == nil {
		t.Fatalf("stored override: %v %+v", err, ov)
	}
	// 5:00 PM in Chicago on 5 Jan is 23:00 UTC: stored UTC, shown in the display zone
	if ov.ExpiresAt.UTC().Format("2006-01-02T15:04") != "2027-01-05T23:00" {
		t.Fatalf("expiry stored as %v", ov.ExpiresAt)
	}
	if strings.Contains(string(ov.Patch), "unseen_after_hours") || !strings.Contains(string(ov.Patch), "36") || !strings.Contains(string(ov.Patch), "360") {
		t.Fatalf("only the ticked settings are stored: %s", ov.Patch)
	}
	if len(e.auditRows("device.override_set", dev)) != 1 || e.auditTotal() != before+1 {
		t.Fatal("setting an override must write exactly one audit row")
	}
	page := e.page(tab+"?saved=override", 200)
	mustContain(t, "after set", page, "Override saved.", "This device has an override", "busy file server", "OVERRIDE", "was 72", "was 1440", "Stale after", `name="o.sla.stale_after_hours" value="1" checked`,
		`value="2027-01-05T17:00"`, "Clear override")
	// clear
	res, _ = e.post("/ui/devices/"+dev+"/override/clear", url.Values{}, false)
	if res.StatusCode != 303 || res.Header.Get("Location") != tab+"?saved=cleared" {
		t.Fatalf("clear: %d", res.StatusCode)
	}
	if o, _ := e.st.GetDeviceOverride(ctx2a, store.ScopeAll(), dev); o != nil {
		t.Fatal("override still active after clear")
	}
	if len(e.auditRows("device.override_clear", dev)) != 1 {
		t.Fatal("clearing must write exactly one audit row")
	}
	mustContain(t, "after clear", e.page(tab+"?saved=cleared", 200), "Override cleared.", "No override: every value below comes from the policy.")
}

func TestW2ARoleGating(t *testing.T) {
	e := newEnv(t)
	def, summit := e.policyIDs()
	custSummit := e.customerID("Summit Eye Care")
	dev := e.dev["FRONTDESK-01"]
	form := func(kv ...string) url.Values {
		out := url.Values{}
		for i := 0; i+1 < len(kv); i += 2 {
			out.Set(kv[i], kv[i+1])
		}
		return out
	}
	adminGets := []string{"/policies/" + def.ID, "/policies/new", "/policies/" + summit.ID + "/delete", "/customers/" + custSummit + "/policy"}
	allGets := []string{"/policies", "/devices/" + dev + "/policy"}
	// anonymous: everything redirects to sign-in
	for _, p := range append(append([]string{}, adminGets...), allGets...) {
		if res, _ := e.do("GET", p, nil); res.StatusCode != 303 || res.Header.Get("Location") != "/login" {
			t.Errorf("anonymous GET %s: %d", p, res.StatusCode)
		}
	}
	for _, rl := range []struct {
		name, role string
		admin      bool
	}{{"ro", "read_only", false}, {"op", "operator", false}, {"ad", "admin", true}} {
		e.signInAs(rl.name, rl.role, nil)
		for _, p := range allGets {
			e.page(p, 200)
		}
		for _, p := range adminGets {
			want := 403
			if rl.admin {
				want = 200
			}
			e.page(p, want)
		}
		before := e.auditTotal()
		posts := map[string]url.Values{
			"/ui/policies":                                   e.editorForm(def.ID),
			"/ui/policies/" + def.ID + "/impact":             e.editorForm(def.ID),
			"/ui/policies/" + def.ID + "/edit":               e.editorForm(def.ID),
			"/ui/policies/" + summit.ID:                      e.editorForm(summit.ID),
			"/ui/policies/" + summit.ID + "/delete":          form("impact_token", "x"),
			"/ui/policy/window/validate":                     form("f.schedule.window", "*/0-24"),
			"/ui/customers/" + custSummit + "/policy/impact": form("policy_id", ""),
			"/ui/customers/" + custSummit + "/policy":        form("policy_id", "", "impact_token", "x"),
			"/ui/devices/" + dev + "/override":               form("o.sla.stale_after_hours", "1", "f.sla.stale_after_hours", "36", "reason", "x"),
			"/ui/devices/" + dev + "/override/clear":         form(),
		}
		for path, v := range posts {
			res, _ := e.post(path, v, false)
			if rl.admin == (res.StatusCode == 403) {
				t.Errorf("%s: POST %s: %d", rl.role, path, res.StatusCode)
			}
			if !rl.admin && res.StatusCode != 403 {
				t.Errorf("%s: POST %s must be refused, got %d", rl.role, path, res.StatusCode)
			}
		}
		if !rl.admin && e.auditTotal() != before {
			t.Errorf("%s: a denied post wrote an audit row", rl.role)
		}
		// the customer page shows the Policy tab to admins only
		tabs := e.page("/customers/"+custSummit, 200)
		if rl.admin != strings.Contains(tabs, ">Policy</a>") {
			t.Errorf("%s: customer Policy tab visibility wrong", rl.role)
		}
		// the editor controls on the device tab are admin-only
		dtab := e.page("/devices/"+dev+"/policy", 200)
		if rl.admin != strings.Contains(dtab, "Override settings for this device") {
			t.Errorf("%s: device override form visibility wrong", rl.role)
		}
	}
	// writes need the CSRF token
	e.signInAs("ad", "admin", nil)
	before := e.auditTotal()
	for _, p := range []string{"/ui/policies/" + summit.ID, "/ui/policies/" + summit.ID + "/impact", "/ui/devices/" + dev + "/override", "/ui/customers/" + custSummit + "/policy"} {
		res, _ := e.do("POST", p, form("name", "x"), "Origin", e.srv.URL)
		if res.StatusCode != 403 {
			t.Errorf("POST %s without a CSRF token: %d", p, res.StatusCode)
		}
	}
	if e.auditTotal() != before {
		t.Error("a CSRF failure wrote an audit row")
	}
}

func TestW2AScopedAccountsNeverSeeOrChangeAnotherCustomersPolicy(t *testing.T) {
	e := newEnv(t)
	north, summit := e.customerID("Northwind Dental"), e.customerID("Summit Eye Care")
	_, summitPol := e.policyIDs()
	e.signInAs("scoped-ro", "read_only", []string{north})
	list := e.page("/policies", 200)
	for _, leak := range []string{"Summit Workstation", "Summit Eye Care"} {
		if strings.Contains(list, leak) {
			t.Errorf("the scoped policy list shows %q", leak)
		}
	}
	mustContain(t, "scoped list", list, "Default")
	// the default's device and customer counts cover only the scope
	sums, err := e.st.ListPolicySummaries(ctx2a, store.ScopeOf(north))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range sums {
		if s.Scope == "default" && !strings.Contains(list, plural(s.Devices, "device", "devices")) {
			t.Errorf("the default row should show the in-scope count %q", plural(s.Devices, "device", "devices"))
		}
	}
	all, _ := e.st.ListPolicySummaries(ctx2a, store.ScopeAll())
	for _, s := range all {
		if s.Scope == "default" && s.Devices == sums[0].Devices && s.Devices != 0 {
			t.Error("the scoped count equals the fleet-wide count: the scope is not applied")
		}
	}
	// another customer's device: the policy tab is a 404, not a leak
	foreign := e.dev["FRONTDESK-01"]
	body := e.page("/devices/"+foreign+"/policy", 404)
	for _, h := range []string{"FRONTDESK-01", "Summit"} {
		if strings.Contains(body, h) {
			t.Errorf("404 page leaks %q", h)
		}
	}
	// scoped accounts cannot reach the admin pages at all
	e.page("/policies/"+summitPol.ID, 403)
	e.page("/customers/"+summit+"/policy", 403)
	before := e.auditTotal()
	if res, _ := e.post("/ui/devices/"+foreign+"/override", url.Values{"o.sla.stale_after_hours": {"1"}, "f.sla.stale_after_hours": {"36"}, "reason": {"x"}}, false); res.StatusCode != 403 {
		t.Errorf("override by a scoped read-only account: %d", res.StatusCode)
	}
	if e.auditTotal() != before {
		t.Error("a denied post wrote an audit row")
	}
	// an operator scoped to Northwind sees the same restrictions
	e.signInAs("scoped-op", "operator", []string{north})
	e.page("/devices/"+foreign+"/policy", 404)
	e.page("/devices/"+e.dev["OPS-ND-01"]+"/policy", 200)
}

func TestW2AWindowValidatorReturnsTheParsersReading(t *testing.T) {
	e := newEnv(t)
	e.signInAs("ad", "admin", nil)
	res, body := e.post("/ui/policy/window/validate", url.Values{"f.schedule.window": {"Mon-Fri/8-18;Sat,Sun/0-24"}}, true)
	if res.StatusCode != 200 {
		t.Fatalf("valid window: %d", res.StatusCode)
	}
	mustContain(t, "valid", body, "hours per week allowed", "Mon-Fri/8-18;Sat,Sun/0-24", `class="wg"`, `class="on"`)
	res, body = e.post("/ui/policy/window/validate", url.Values{"f.schedule.window": {"Mon-Fri/18-8x"}}, true)
	if res.StatusCode != 200 {
		t.Fatalf("invalid window: %d", res.StatusCode)
	}
	mustContain(t, "invalid", body, "Not a valid window, at character", "caretline", "^")
	_, body = e.post("/ui/policy/window/validate", url.Values{"f.maintenance.window_hint": {""}, "optional": {"1"}}, true)
	mustContain(t, "optional blank", body, "No window set.")
	if res, _ = e.post("/ui/policy/window/validate", url.Values{}, true); res.StatusCode != 400 {
		t.Fatalf("no field: %d", res.StatusCode)
	}
}

func TestW2AEffectiveViewLabelsTheSourceOfEveryField(t *testing.T) {
	e := newEnv(t)
	e.signInAs("ad", "admin", nil)
	page := e.page("/devices/"+e.dev["DR-OFFICE-PC"]+"/policy", 200)
	// DR-OFFICE-PC: window from Summit's customer policy, max run hours from its override, everything else the default
	rowOf := func(label string) string {
		i := strings.Index(page, ">"+label+"</td>")
		if i < 0 {
			t.Fatalf("no row %q", label)
		}
		j := strings.Index(page[i:], "</tr>")
		return page[i : i+j]
	}
	if r := rowOf("Backup window"); !strings.Contains(r, "POLICY") || !strings.Contains(r, "Summit Workstation") || !strings.Contains(r, "Mon-Fri/7-19;Sat/8-14") {
		t.Errorf("window row: %s", r)
	}
	if r := rowOf("Longest normal run"); !strings.Contains(r, "OVERRIDE") || !strings.Contains(r, "24") || !strings.Contains(r, "was 12") {
		t.Errorf("run hours row: %s", r)
	}
	if r := rowOf("Stale after"); !strings.Contains(r, "DEFAULT") || !strings.Contains(r, "72") {
		t.Errorf("stale row: %s", r)
	}
}

func TestW2ANavHasPolicies(t *testing.T) {
	e := newEnv(t)
	e.login()
	mustContain(t, "board nav", e.page("/", 200), `href="/policies"`)
	mustContain(t, "policies nav", e.page("/policies", 200), `href="/policies" aria-current="page"`)
}
