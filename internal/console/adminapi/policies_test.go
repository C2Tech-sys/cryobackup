package adminapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

var ctxBg = context.Background()

// ---- helpers (M2-W2A) ----

func (e *env) adminSession() *session {
	e.t.Helper()
	e.addAdmin("pol-admin", "admin", nil)
	return e.login("pol-admin", pw)
}

func (e *env) createTemplate(s *session, name string, sections map[string]any) PolicyResponse {
	e.t.Helper()
	res, b := e.req("POST", "/api/v1/policies", s, map[string]any{"scope": "template", "name": name, "sections": sections})
	if res.StatusCode != http.StatusCreated {
		e.t.Fatalf("create template: %d %s", res.StatusCode, b)
	}
	return decode[PolicyResponse](e.t, b)
}

func (e *env) impact(s *session, id string, body any) (int, ImpactResponse, []byte) {
	e.t.Helper()
	res, b := e.req("POST", "/api/v1/policies/"+id+"/impact", s, body)
	var out ImpactResponse
	if res.StatusCode == 200 {
		out = decode[ImpactResponse](e.t, b)
	}
	return res.StatusCode, out, b
}

func conflictFields(t *testing.T, b []byte) map[string]string {
	t.Helper()
	var p struct {
		Code      string             `json:"code"`
		Errors    []store.FieldError `json:"errors"`
		Customers []store.ImpactCustomer
	}
	if err := json.Unmarshal(b, &p); err != nil || p.Code != api.CodeConflict {
		t.Fatalf("not a conflict problem: %s", b)
	}
	out := map[string]string{}
	for _, f := range p.Errors {
		out[f.Field] = f.Message
	}
	return out
}

// ---- tests ----

func TestW2APolicyRoleMatrix(t *testing.T) {
	e := newEnv(t)
	order := []string{"read_only", "operator", "admin", "owner"}
	roles := map[string]*session{}
	for _, r := range order {
		e.addAdmin("u-"+r, r, nil)
		roles[r] = e.login("u-"+r, pw)
	}
	dev := e.dev.ID
	override := map[string]any{"patch": map[string]any{"sla": map[string]any{"max_run_hours": 30}}, "reason": "matrix"}
	n := 0
	type call struct {
		name, method, path string
		body               func() any
		min                string
	}
	calls := []call{
		{"list policies", "GET", "/api/v1/policies", nil, "read_only"},
		{"effective policy", "GET", "/api/v1/devices/" + dev + "/effective-policy", nil, "read_only"},
		{"create policy", "POST", "/api/v1/policies", func() any {
			n++
			return map[string]any{"scope": "template", "name": "Matrix " + strings.Repeat("x", n)}
		}, "admin"},
		{"set override", "PUT", "/api/v1/devices/" + dev + "/override", func() any { return override }, "admin"},
		{"get override", "GET", "/api/v1/devices/" + dev + "/override", nil, "read_only"},
		{"clear override", "DELETE", "/api/v1/devices/" + dev + "/override", nil, "admin"},
	}
	for _, c := range calls {
		for i, role := range order {
			var body any
			if c.body != nil {
				body = c.body()
			}
			res, b := e.req(c.method, c.path, roles[role], body)
			allowed := i >= indexOf(order, c.min)
			switch {
			case allowed && res.StatusCode >= 300:
				// the override just cleared answers 404 to the next reader; that is not a role problem
				if !(c.name == "get override" && res.StatusCode == 404) {
					t.Errorf("%s as %s: want success, got %d %s", c.name, role, res.StatusCode, b)
				}
			case !allowed && (res.StatusCode != 403 || problemCode(t, res, b) != api.CodeForbidden):
				t.Errorf("%s as %s: want 403 forbidden, got %d %s", c.name, role, res.StatusCode, b)
			}
		}
	}
	// The write routes that need an existing policy: operators and read-only are refused too.
	ad := roles["admin"]
	tpl := e.createTemplate(ad, "Gate", nil)
	for _, role := range []string{"read_only", "operator"} {
		for _, c := range []struct{ method, path string }{{"PATCH", "/api/v1/policies/" + tpl.ID}, {"DELETE", "/api/v1/policies/" + tpl.ID}, {"POST", "/api/v1/policies/" + tpl.ID + "/impact"}} {
			res, b := e.req(c.method, c.path, roles[role], map[string]any{})
			if res.StatusCode != 403 {
				t.Errorf("%s %s as %s: %d %s", c.method, c.path, role, res.StatusCode, b)
			}
		}
	}
	// A write without the CSRF token changes nothing.
	before := e.audits()
	bad := &session{cookies: ad.cookies, csrf: "wrong"}
	if res, _ := e.req("PUT", "/api/v1/devices/"+dev+"/override", bad, override); res.StatusCode != 403 || e.audits() != before {
		t.Errorf("csrf: %d", res.StatusCode)
	}
}

func TestW2APolicyEditNeedsThePreviewAndAppliesExactlyIt(t *testing.T) {
	e := newEnv(t)
	ad := e.adminSession()
	tpl := e.createTemplate(ad, "Workstation", map[string]any{"sla": map[string]any{"stale_after_hours": 48}})
	if tpl.Version != 1 || tpl.Scope != "template" || len(e.auditOf("policy.create", tpl.ID)) != 1 {
		t.Fatalf("created: %+v", tpl)
	}
	// assign it to the customer of the test device so the preview has a population
	dev, err := e.st.GetDevice(ctxBg, store.ScopeAll(), e.dev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res, b := e.assignPolicy(ad, dev.CustomerID, tpl.ID); res.StatusCode != 200 {
		t.Fatalf("assign: %d %s", res.StatusCode, b)
	}
	change := map[string]any{"sections": map[string]any{"sla": map[string]any{"stale_after_hours": 36}}}

	// 1. no token: refused, field named
	res, b := e.req("PATCH", "/api/v1/policies/"+tpl.ID, ad, change)
	if res.StatusCode != 400 || problemFields(t, b)["impact_token"] == "" {
		t.Fatalf("patch without a token: %d %s", res.StatusCode, b)
	}
	// 2. preview: devices and fields before saving
	code, imp, raw := e.impact(ad, tpl.ID, change)
	if code != 200 || imp.Population < 1 || imp.Devices != imp.Population || imp.Token == "" || len(imp.Changes) != 1 ||
		imp.Changes[0].Path != "sla.stale_after_hours" || imp.Changes[0].Old != "48" || imp.Changes[0].New != "36" {
		t.Fatalf("impact: %d %s", code, raw)
	}
	// 3. the token of another change is refused (409 says why) and nothing is saved
	other := map[string]any{"sections": map[string]any{"sla": map[string]any{"stale_after_hours": 12}}, "impact_token": imp.Token}
	res, b = e.req("PATCH", "/api/v1/policies/"+tpl.ID, ad, other)
	if res.StatusCode != 409 || conflictFields(t, b)["impact_token"] == "" {
		t.Fatalf("foreign token: %d %s", res.StatusCode, b)
	}
	if len(e.auditOf("policy.update", tpl.ID)) != 0 {
		t.Fatal("a refused patch wrote an audit row")
	}
	// 4. the previewed change applies: one version, one audit row
	change["impact_token"] = imp.Token
	res, b = e.req("PATCH", "/api/v1/policies/"+tpl.ID, ad, change)
	got := decode[PolicyResponse](t, b)
	if res.StatusCode != 200 || got.Version != 2 || got.Changed == nil || !*got.Changed || len(e.auditOf("policy.update", tpl.ID)) != 1 {
		t.Fatalf("patch: %d %s", res.StatusCode, b)
	}
	// 5. the token cannot be replayed once the policy moved
	res, b = e.req("PATCH", "/api/v1/policies/"+tpl.ID, ad, change)
	if res.StatusCode != 409 || conflictFields(t, b)["impact_token"] == "" || len(e.auditOf("policy.update", tpl.ID)) != 1 {
		t.Fatalf("replay: %d %s", res.StatusCode, b)
	}
	// 6. invalid documents come back keyed by path, from the preview as well
	code, _, raw = e.impact(ad, tpl.ID, map[string]any{"sections": map[string]any{"schedule": map[string]any{"window": "Mon-Fri/18-8x"}}})
	if code != 400 || problemFields(t, raw)["schedule.window"] == "" {
		t.Fatalf("invalid window: %d %s", code, raw)
	}
}

func TestW2APolicyDeleteIsRefusedWhileAssigned(t *testing.T) {
	e := newEnv(t)
	ad := e.adminSession()
	tpl := e.createTemplate(ad, "Doomed", nil)
	dev, _ := e.st.GetDevice(ctxBg, store.ScopeAll(), e.dev.ID)
	if res, b := e.assignPolicy(ad, dev.CustomerID, tpl.ID); res.StatusCode != 200 {
		t.Fatalf("assign: %d %s", res.StatusCode, b)
	}
	res, b := e.req("DELETE", "/api/v1/policies/"+tpl.ID, ad, nil)
	var p struct {
		Customers []store.ImpactCustomer `json:"customers"`
	}
	_ = json.Unmarshal(b, &p)
	if res.StatusCode != 409 || len(p.Customers) != 1 || p.Customers[0].ID != dev.CustomerID {
		t.Fatalf("delete while assigned: %d %s", res.StatusCode, b)
	}
	if code, _, raw := e.impact(ad, tpl.ID, map[string]any{"delete": true}); code != 409 {
		t.Fatalf("delete preview while assigned: %d %s", code, raw)
	}
	if res, b := e.assignPolicy(ad, dev.CustomerID, ""); res.StatusCode != 200 {
		t.Fatalf("unassign: %d %s", res.StatusCode, b)
	}
	if res, _ := e.req("DELETE", "/api/v1/policies/"+tpl.ID, ad, nil); res.StatusCode != 400 {
		t.Fatalf("delete without a preview token: %d", res.StatusCode)
	}
	code, imp, raw := e.impact(ad, tpl.ID, map[string]any{"delete": true})
	if code != 200 || imp.Token == "" || imp.Population != 0 {
		t.Fatalf("delete preview: %d %s", code, raw)
	}
	if res, b := e.req("DELETE", "/api/v1/policies/"+tpl.ID+"?impact_token="+url.QueryEscape(imp.Token), ad, nil); res.StatusCode != 204 {
		t.Fatalf("delete: %d %s", res.StatusCode, b)
	}
	if res, _ := e.req("GET", "/api/v1/policies/"+tpl.ID, ad, nil); res.StatusCode != 404 || len(e.auditOf("policy.delete", tpl.ID)) != 1 {
		t.Fatalf("after delete: %d", res.StatusCode)
	}
	// the default policy is never deletable
	list := decode[PolicyListResponse](t, func() []byte { _, b := e.get("/api/v1/policies?scope=default", ad); return b }())
	if len(list.Policies) != 1 {
		t.Fatalf("default policy list: %+v", list)
	}
	if res, b := e.req("POST", "/api/v1/policies/"+list.Policies[0].ID+"/impact", ad, map[string]any{"delete": true}); res.StatusCode != 409 {
		t.Fatalf("default delete preview: %d %s", res.StatusCode, b)
	}
}

func TestW2AOverrideEndToEndWithSources(t *testing.T) {
	e := newEnv(t)
	ad := e.adminSession()
	dev := e.held.ID
	if res, _ := e.req("GET", "/api/v1/devices/"+dev+"/override", ad, nil); res.StatusCode != 404 {
		t.Fatalf("no override yet: %d", res.StatusCode)
	}
	for name, c := range map[string]struct {
		body  map[string]any
		field string
	}{
		"no reason":     {map[string]any{"patch": map[string]any{"sla": map[string]any{"max_run_hours": 30}}}, "reason"},
		"unknown field": {map[string]any{"patch": map[string]any{"sla": map[string]any{"colour": 1}}, "reason": "x"}, "patch"},
		"bad value":     {map[string]any{"patch": map[string]any{"schedule": map[string]any{"interval_minutes": 1}}, "reason": "x"}, "schedule.interval_minutes"},
	} {
		res, b := e.req("PUT", "/api/v1/devices/"+dev+"/override", ad, c.body)
		if res.StatusCode != 400 || problemFields(t, b)[c.field] == "" {
			t.Errorf("%s: %d %s", name, res.StatusCode, b)
		}
	}
	if len(e.auditOf("device.override_set", dev)) != 0 {
		t.Fatal("a refused override wrote an audit row")
	}
	res, b := e.req("PUT", "/api/v1/devices/"+dev+"/override", ad, map[string]any{"patch": map[string]any{"schedule": map[string]any{"interval_minutes": 360}}, "reason": "busy server"})
	o := decode[OverrideResponse](t, b)
	if res.StatusCode != 200 || o.Changed == nil || !*o.Changed || o.Reason != "busy server" || len(e.auditOf("device.override_set", dev)) != 1 {
		t.Fatalf("set: %d %s", res.StatusCode, b)
	}
	if res, b := e.req("GET", "/api/v1/devices/"+dev+"/override", ad, nil); res.StatusCode != 200 || decode[OverrideResponse](t, b).Reason != "busy server" {
		t.Fatalf("get: %d %s", res.StatusCode, b)
	}
	_, b = e.get("/api/v1/devices/"+dev+"/effective-policy", ad)
	eff := decode[EffectivePolicyResponse](t, b)
	if eff.ETag == "" || eff.Override == nil || len(eff.Policy) == 0 {
		t.Fatalf("effective: %s", b)
	}
	sources := map[string]EffectiveFieldJSON{}
	for _, f := range eff.Fields {
		sources[f.Path] = f
	}
	if f := sources["schedule.interval_minutes"]; f.Source != "override" || string(f.Value) != "360" || string(f.Inherited) != "1440" {
		t.Fatalf("overridden field: %+v", f)
	}
	if f := sources["conditions.metered"]; f.Source != "default" && f.Source != "policy" {
		t.Fatalf("other field: %+v", f)
	}
	if res, _ := e.req("DELETE", "/api/v1/devices/"+dev+"/override", ad, nil); res.StatusCode != 204 || len(e.auditOf("device.override_clear", dev)) != 1 {
		t.Fatalf("clear: %d", res.StatusCode)
	}
	_, b = e.get("/api/v1/devices/"+dev+"/effective-policy", ad)
	for _, f := range decode[EffectivePolicyResponse](t, b).Fields {
		if f.Path == "schedule.interval_minutes" && f.Source == "override" {
			t.Fatal("the field is still labelled as overridden after the override was cleared")
		}
	}
}

func TestW2AScopedAccountsSeeOnlyTheirOwnPolicies(t *testing.T) {
	e := newEnv(t)
	north := e.customerNamed("Northwind Dental")
	summit := e.customerNamed("Summit Eye Care")
	e.addAdmin("scoped-ro", "read_only", []string{north.ID})
	sc := e.login("scoped-ro", pw)
	ad := e.adminSession()

	// Summit's customer policy exists, but this account may not see it or count its devices.
	list := decode[PolicyListResponse](t, func() []byte { _, b := e.get("/api/v1/policies", sc); return b }())
	var summitPolicy string
	for _, p := range e.policiesOf(ad) {
		if p.CustomerID == summit.ID {
			summitPolicy = p.ID
		}
	}
	if summitPolicy == "" {
		t.Fatal("fixtures changed: Summit has no customer policy")
	}
	for _, p := range list.Policies {
		if p.CustomerID == summit.ID || p.ID == summitPolicy || strings.Contains(p.Name, "Summit") {
			t.Errorf("scoped account sees another customer's policy: %+v", p)
		}
		if p.Scope == "default" && p.Devices != nil && *p.Devices > northDevices(e, north.ID) {
			t.Errorf("the default policy's device count leaks other customers' devices: %d", *p.Devices)
		}
	}
	if code, _ := e.get("/api/v1/policies/"+summitPolicy, sc); code != 404 {
		t.Errorf("another customer's policy by id: %d", code)
	}
	// Devices of another customer: effective policy and override are 404, and no write reaches them.
	other := e.deviceOf(summit.ID)
	if code, _ := e.get("/api/v1/devices/"+other+"/effective-policy", sc); code != 404 {
		t.Errorf("effective policy outside scope: %d", code)
	}
	if code, _ := e.get("/api/v1/devices/"+other+"/override", sc); code != 404 {
		t.Errorf("override outside scope: %d", code)
	}
	// An admin sees both.
	if code, b := e.get("/api/v1/policies/"+summitPolicy, ad); code != 200 {
		t.Errorf("admin: %d %s", code, b)
	}
}

// assignPolicy assigns a policy ("" = the default) the way the API now requires:
// preview with assign_customer, then PATCH the customer with the token.
func (e *env) assignPolicy(s *session, customerID, policyID string) (*http.Response, []byte) {
	e.t.Helper()
	pathID := policyID
	var value any = policyID
	if policyID == "" {
		def, err := e.st.DefaultPolicy(ctxBg)
		if err != nil {
			e.t.Fatal(err)
		}
		pathID, value = def.ID, nil
	}
	code, imp, raw := e.impact(s, pathID, map[string]any{"assign_customer": customerID})
	if code != 200 || imp.Token == "" {
		e.t.Fatalf("assignment preview: %d %s", code, raw)
	}
	return e.req("PATCH", "/api/v1/customers/"+customerID, s, map[string]any{"policy_id": value, "impact_token": imp.Token})
}

func TestW2AAssigningAPolicyThroughTheAPINeedsItsPreview(t *testing.T) {
	e := newEnv(t)
	ad := e.adminSession()
	tpl := e.createTemplate(ad, "Strict", map[string]any{"sla": map[string]any{"stale_after_hours": 24, "unseen_after_hours": 100}})
	dev, _ := e.st.GetDevice(ctxBg, store.ScopeAll(), e.dev.ID)
	cust := dev.CustomerID
	current, _ := e.st.GetCustomer(ctxBg, store.ScopeAll(), cust)
	origPolicy := current.PolicyID // the fixture customer already has its own policy
	other := e.createTemplate(ad, "Other", nil)
	path := "/api/v1/customers/" + cust
	before := e.audits()

	// 1. the bypass is closed: no token, no assignment, and the fields say why
	res, b := e.req("PATCH", path, ad, map[string]any{"policy_id": tpl.ID})
	f := problemFields(t, b)
	if res.StatusCode != 400 || f["impact_token"] == "" || f["policy_id"] == "" {
		t.Fatalf("no token: %d %s", res.StatusCode, b)
	}
	// 2. the preview reports devices and changed fields
	code, imp, raw := e.impact(ad, tpl.ID, map[string]any{"assign_customer": cust})
	if code != 200 || imp.Population < 1 || imp.Devices < 1 || imp.Token == "" || len(imp.Changes) == 0 {
		t.Fatalf("assignment preview: %d %s", code, raw)
	}
	// 3. a token cannot ride along with other changes, nor be used for another policy
	res, b = e.req("PATCH", path, ad, map[string]any{"policy_id": tpl.ID, "notes": "x", "impact_token": imp.Token})
	if res.StatusCode != 400 || problemFields(t, b)["policy_id"] == "" {
		t.Fatalf("policy with other fields: %d %s", res.StatusCode, b)
	}
	res, b = e.req("PATCH", path, ad, map[string]any{"policy_id": other.ID, "impact_token": imp.Token})
	if res.StatusCode != 409 || conflictFields(t, b)["impact_token"] == "" {
		t.Fatalf("token of another assignment: %d %s", res.StatusCode, b)
	}
	if c, _ := e.st.GetCustomer(ctxBg, store.ScopeAll(), cust); c.PolicyID != origPolicy || e.audits() != before {
		t.Fatal("a refused assignment changed the customer or wrote an audit row")
	}
	// 4. the previewed assignment applies, with exactly one audit row
	res, b = e.req("PATCH", path, ad, map[string]any{"policy_id": tpl.ID, "impact_token": imp.Token})
	if res.StatusCode != 200 || decode[CustomerResponse](t, b).PolicyID != tpl.ID || e.audits() != before+1 {
		t.Fatalf("assign: %d %s", res.StatusCode, b)
	}
	// 5. replaying the token (the assignment moved) is a 409; an unchanged policy_id is accepted without one
	if res, b = e.req("PATCH", path, ad, map[string]any{"policy_id": nil, "impact_token": imp.Token}); res.StatusCode != 409 {
		t.Fatalf("replay: %d %s", res.StatusCode, b)
	}
	if res, b = e.req("PATCH", path, ad, map[string]any{"policy_id": tpl.ID, "notes": "same policy, new note"}); res.StatusCode != 200 {
		t.Fatalf("unchanged policy_id with other fields: %d %s", res.StatusCode, b)
	}
	// 6. an out-of-scope customer is a 404 on the preview (an admin is never scoped; the store enforces it, see the matrix)
	if code, _, _ := e.impact(ad, tpl.ID, map[string]any{"assign_customer": "01J9Y4K3A0B1C2D3E4F5G6H7ZZ"}); code != 404 {
		t.Fatalf("unknown customer: %d", code)
	}
	// POST /customers with an initial policy stays allowed (a new customer has no devices)
	res, b = e.req("POST", "/api/v1/customers", ad, map[string]any{"name": "Fresh Co", "policy_id": tpl.ID})
	if res.StatusCode != 201 || decode[CustomerResponse](t, b).PolicyID != tpl.ID {
		t.Fatalf("create with a policy: %d %s", res.StatusCode, b)
	}
}

func TestW2AFixBatchThroughTheAPI(t *testing.T) {
	e := newEnv(t)
	ad := e.adminSession()
	// unknown keys are refused on create and on patch
	res, b := e.req("POST", "/api/v1/policies", ad, map[string]any{"scope": "template", "name": "Typo", "sections": map[string]any{"sla": map[string]any{"stale_after_hourz": 5}}})
	if res.StatusCode != 400 || problemFields(t, b)["sla.stale_after_hourz"] == "" {
		t.Fatalf("create with a typo: %d %s", res.StatusCode, b)
	}
	tpl := e.createTemplate(ad, "Slow retries", map[string]any{"schedule": map[string]any{"retries": map[string]any{"base_s": 1800}}})
	res, b = e.req("PATCH", "/api/v1/policies/"+tpl.ID, ad, map[string]any{"sections": map[string]any{"sla": map[string]any{"stale_after_hourz": 5}}, "impact_token": "x"})
	if res.StatusCode != 400 || problemFields(t, b)["sla.stale_after_hourz"] == "" {
		t.Fatalf("patch with a typo: %d %s", res.StatusCode, b)
	}
	// the default edit is judged against every stored policy, assigned or not
	def, err := e.st.DefaultPolicy(ctxBg)
	if err != nil {
		t.Fatal(err)
	}
	var sched map[string]any
	if err := json.Unmarshal(def.Sections["schedule"], &sched); err != nil {
		t.Fatal(err)
	}
	sched["retries"].(map[string]any)["cap_s"] = 900
	code, _, raw := e.impact(ad, def.ID, map[string]any{"sections": map[string]any{"schedule": sched}})
	f := problemFields(t, raw)
	if code != 400 || !strings.Contains(f["policy"], "Slow retries") {
		t.Fatalf("default edit that breaks an unused template: %d %s", code, raw)
	}
	// the generic customer update cannot change the policy even with a stray token
	dev, _ := e.st.GetDevice(ctxBg, store.ScopeAll(), e.dev.ID)
	res, b = e.req("PATCH", "/api/v1/customers/"+dev.CustomerID, ad, map[string]any{"policy_id": tpl.ID, "impact_token": "forged"})
	if res.StatusCode != 409 && res.StatusCode != 400 {
		t.Fatalf("forged token: %d %s", res.StatusCode, b)
	}
	if c, _ := e.st.GetCustomer(ctxBg, store.ScopeAll(), dev.CustomerID); c.PolicyID == tpl.ID {
		t.Fatal("a forged token assigned the policy")
	}
}

func (e *env) policiesOf(s *session) []PolicyJSON {
	e.t.Helper()
	_, b := e.get("/api/v1/policies", s)
	return decode[PolicyListResponse](e.t, b).Policies
}

func (e *env) deviceOf(customerID string) string {
	e.t.Helper()
	l, err := e.st.ListDevices(ctxBg, store.ScopeOf(customerID), store.ListDevicesOptions{})
	if err != nil || len(l.Items) == 0 {
		e.t.Fatalf("no device for customer: %v", err)
	}
	return l.Items[0].ID
}

func northDevices(e *env, customerID string) int {
	l, _ := e.st.ListDevices(ctxBg, store.ScopeOf(customerID), store.ListDevicesOptions{})
	return len(l.Items)
}
