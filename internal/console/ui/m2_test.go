package ui

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// ---- helpers (M2-W1C) ----

// signInAs creates an account (or reuses it) and signs the test client in as it
// (password, then the MFA step, like every login since M2-W1A).
// It is the one place the M2 tests sign in as someone other than the fixtures
// owner, so a change to the sign-in flow is a one-line fix here.
func (e *env) signInAs(username, role string, scope []string) {
	e.t.Helper()
	hash, _ := auth.HashPassword(testPassword)
	if _, err := e.st.GetAdminByUsername(context.Background(), username); err != nil {
		if _, err := e.st.CreateAdmin(context.Background(), store.Admin{Username: username, Email: username + "@example.invalid", Role: role, PasswordHash: hash, CustomerScope: scope}); err != nil {
			e.t.Fatal(err)
		}
	}
	e.sess, e.csrf = nil, ""
	res, body := e.do("POST", "/login", url.Values{"username": {username}, "password": {testPassword}})
	if res.StatusCode != http.StatusSeeOther {
		e.t.Fatalf("login as %s: %d %s", username, res.StatusCode, body)
	}
	e.adopt(res)
	e.completeMFA(username)
}

func (e *env) customerID(name string) string {
	e.t.Helper()
	cs, err := e.st.ListCustomers(context.Background(), store.ScopeAll())
	if err != nil {
		e.t.Fatal(err)
	}
	for _, c := range cs {
		if c.Name == name {
			return c.ID
		}
	}
	e.t.Fatalf("no fixture customer %q", name)
	return ""
}

func (e *env) page(path string, want int) string {
	e.t.Helper()
	res, body := e.do("GET", path, nil)
	if res.StatusCode != want {
		e.t.Fatalf("GET %s: want %d, got %d\n%s", path, want, res.StatusCode, body[:min(len(body), 300)])
	}
	return body
}

func (e *env) auditRows(action, objectID string) []store.AuditEvent {
	e.t.Helper()
	l, err := e.st.ListAudit(context.Background(), store.ScopeAll(), store.AuditFilter{Action: action, ObjectID: objectID}, store.Page{Limit: 100})
	if err != nil {
		e.t.Fatal(err)
	}
	return l.Items
}

func (e *env) auditTotal() int {
	e.t.Helper()
	l, err := e.st.ListAudit(context.Background(), store.ScopeAll(), store.AuditFilter{}, store.Page{Limit: 1000})
	if err != nil {
		e.t.Fatal(err)
	}
	return len(l.Items)
}

var absoluteURL = regexp.MustCompile(`(?i)(?:href|src|action)="https?://`)

// everyM2Page lists the pages this ticket adds, for the pages that must render
// in fixtures mode with no CDN reference and no path.
func (e *env) everyM2Page() map[string]string {
	summit, blue, lake, north := e.customerID("Summit Eye Care"), e.customerID("Blue Ridge Accounting"), e.customerID("Lakeshore Pediatrics"), e.customerID("Northwind Dental")
	dev := e.dev["FRONTDESK-01"]
	return map[string]string{
		"customers":           "/customers",
		"customers sorted":    "/customers?sort=devices&dir=desc",
		"customers filtered":  "/customers?health=missing&expected=yes",
		"customer overview":   "/customers/" + summit,
		"customer missing":    "/customers/" + blue,
		"customer onboarding": "/customers/" + lake,
		"customer healthy":    "/customers/" + north,
		"customer devices":    "/customers/" + summit + "/devices",
		"customer empty tab":  "/customers/" + blue + "/devices",
		"customer settings":   "/customers/" + summit + "/settings",
		"new customer":        "/ui/customers/new",
		"devices":             "/devices",
		"devices sorted":      "/devices?sort=next_due",
		"devices filtered":    "/devices?state=FAILED&state=UNSEEN&retired=1",
		"devices fragment":    "/ui/devices/table?sort=last_success&dir=desc",
		"device summary":      "/devices/" + dev,
		"device history":      "/devices/" + dev + "/history",
		"device settings":     "/devices/" + dev + "/settings",
		"device audit":        "/devices/" + dev + "/audit",
	}
}

func TestM2PagesRenderInFixturesMode(t *testing.T) {
	e := newEnv(t)
	e.login()
	for name, path := range e.everyM2Page() {
		body := e.page(path, 200)
		if strings.Contains(body, "ZgotmplZ") {
			t.Errorf("%s: html/template rejected a value (ZgotmplZ)", name)
		}
		if absoluteURL.MatchString(body) {
			t.Errorf("%s: an absolute URL reference (no CDN, no third party)", name)
		}
		if m := pathLike.FindString(body); m != "" {
			t.Errorf("%s: shows a path-like string %q", name, m)
		}
		if !strings.HasPrefix(path, "/ui/devices/table") && !strings.Contains(body, `id="freshness"`) {
			t.Errorf("%s: no freshness stamp", name)
		}
	}
	mustContain(t, "customers", e.page("/customers", 200), "Customers", "Summit Eye Care", "Blue Ridge Accounting", "Lakeshore Pediatrics", "MISSING", "ONBOARDING", "EXPECTED", "Add customer",
		`aria-sort="none"`, "Showing 1-6 of 6", `aria-current="page"`, "Default")
	mustContain(t, "devices", e.page("/devices", 200), "FRONTDESK-01", "LAB-RETINA-02", "Showing 1-25 of 25", `id="devresults"`, `hx-get="/ui/devices/table`, `aria-sort="ascending"`,
		"Next due", "Windows", `role="search"`)
	mustContain(t, "fragment", e.page("/ui/devices/table", 200), "FRONTDESK-01", "Showing 1-25 of 25")
}

func TestM2NavHasCustomersAndDevices(t *testing.T) {
	e := newEnv(t)
	e.login()
	b := e.page("/", 200)
	mustContain(t, "board nav", b, `href="/customers"`, `href="/devices"`)
	mustContain(t, "customers nav", e.page("/customers", 200), `href="/customers" aria-current="page"`)
	mustContain(t, "devices nav", e.page("/devices", 200), `href="/devices" aria-current="page"`)
}

func TestM2RoleGating(t *testing.T) {
	e := newEnv(t)
	summit := e.customerID("Summit Eye Care")
	dev := e.dev["FRONTDESK-01"]
	gets := []string{"/customers", "/customers/" + summit, "/customers/" + summit + "/devices", "/customers/" + summit + "/settings", "/devices", "/ui/devices/table",
		"/devices/" + dev, "/devices/" + dev + "/history", "/devices/" + dev + "/settings"}
	form := func(v ...string) url.Values {
		out := url.Values{}
		for i := 0; i+1 < len(v); i += 2 {
			out.Set(v[i], v[i+1])
		}
		return out
	}
	// Anonymous: everything redirects to the sign-in page.
	for _, p := range append(gets, "/ui/customers/new", "/devices/"+dev+"/audit") {
		if res, _ := e.do("GET", p, nil); res.StatusCode != 303 || res.Header.Get("Location") != "/login" {
			t.Errorf("anonymous GET %s: %d", p, res.StatusCode)
		}
	}
	roles := []struct {
		name, role                         string
		newCustomer, audit, deviceSettings bool
	}{{"ro", "read_only", false, false, false}, {"op", "operator", false, false, true}, {"ad", "admin", true, true, true}}
	for _, rl := range roles {
		e.signInAs(rl.name, rl.role, nil)
		for _, p := range gets {
			e.page(p, 200)
		}
		want := func(ok bool) int {
			if ok {
				return 200
			}
			return 403
		}
		e.page("/ui/customers/new", want(rl.newCustomer))
		e.page("/devices/"+dev+"/audit", want(rl.audit))
		before := e.auditTotal()
		res, _ := e.post("/ui/customers", form("name", "Gate Co "+rl.name), false)
		if rl.newCustomer == (res.StatusCode == 403) {
			t.Errorf("%s: POST /ui/customers: %d", rl.role, res.StatusCode)
		}
		res, _ = e.post("/ui/customers/"+summit, form("name", "Summit Eye Care", "notes", rl.name), false)
		if rl.newCustomer == (res.StatusCode == 403) {
			t.Errorf("%s: POST /ui/customers/{id}: %d", rl.role, res.StatusCode)
		}
		res, _ = e.post("/ui/devices/"+dev+"/settings", form("notes", "by "+rl.name, "device_class", "desktop", "update_ring", "stable"), false)
		if rl.deviceSettings == (res.StatusCode == 403) {
			t.Errorf("%s: POST /ui/devices/{id}/settings: %d", rl.role, res.StatusCode)
		}
		if !rl.deviceSettings && e.auditTotal() != before {
			t.Errorf("%s: a denied post wrote an audit row", rl.role)
		}
	}
	// Writes need the CSRF token: a post without one changes nothing.
	e.signInAs("ad", "admin", nil)
	before := e.auditTotal()
	for _, p := range []string{"/ui/customers", "/ui/customers/" + summit, "/ui/devices/" + dev + "/settings"} {
		res, _ := e.do("POST", p, form("name", "No Csrf Co", "notes", "x"), "Origin", e.srv.URL)
		if res.StatusCode != 403 {
			t.Errorf("POST %s without a CSRF token: %d", p, res.StatusCode)
		}
	}
	res, _ := e.do("POST", "/ui/customers", form("name", "Cross Co", "csrf_token", e.csrf), "Origin", "https://evil.example")
	if res.StatusCode != 403 || e.auditTotal() != before {
		t.Errorf("cross-origin post: %d", res.StatusCode)
	}
}

func TestM2ScopedOperatorNeverSeesAnotherCustomer(t *testing.T) {
	e := newEnv(t)
	north, summit, harbor := e.customerID("Northwind Dental"), e.customerID("Summit Eye Care"), e.customerID("Harbor Family Practice")
	foreign := []string{"FRONTDESK-01", "LAB-RETINA-02", "LEGAL-PC-01", "NEW-EXAM-6"}
	for _, who := range []struct{ name, role string }{{"scoped-op", "operator"}, {"scoped-ro", "read_only"}} {
		e.signInAs(who.name, who.role, []string{north})
		noForeign := func(what, body string) {
			t.Helper()
			for _, h := range foreign {
				if strings.Contains(body, h) {
					t.Errorf("%s/%s shows another customer's device %s", who.name, what, h)
				}
			}
			for _, n := range []string{"Summit Eye Care", "Harbor Family Practice", "Pinecrest Legal", "Blue Ridge Accounting", "Lakeshore Pediatrics"} {
				if strings.Contains(body, n) {
					t.Errorf("%s/%s shows another customer's name %q", who.name, what, n)
				}
			}
		}
		for what, path := range map[string]string{"customers": "/customers", "customers search": "/customers?q=summit", "devices": "/devices", "devices search": "/devices?q=frontdesk",
			"devices by customer": "/devices?customer=" + summit, "fragment": "/ui/devices/table?customer=" + summit, "own customer": "/customers/" + north,
			"own devices tab": "/customers/" + north + "/devices", "own settings": "/customers/" + north + "/settings"} {
			noForeign(what, e.page(path, 200))
		}
		mustContain(t, "scoped customers", e.page("/customers", 200), "Northwind Dental", "Showing 1-1 of 1")
		mustContain(t, "scoped devices", e.page("/devices", 200), "OPS-ND-01", "Showing 1-3 of 3")
		// A customer or device outside scope is a 404, the same as one that does not exist.
		for _, p := range []string{"/customers/" + summit, "/customers/" + harbor + "/devices", "/customers/" + summit + "/settings", "/customers/01J9Y4K3A0B1C2D3E4F5G6H7ZZ",
			"/devices/" + e.dev["FRONTDESK-01"], "/devices/" + e.dev["FRONTDESK-01"] + "/history", "/devices/" + e.dev["FRONTDESK-01"] + "/settings",
			"/ui/devices/table?tab=1&customer=" + summit} {
			body := e.page(p, 404)
			noForeign("404 "+p, body)
		}
	}
	// Posts against another customer's rows are 404 and change nothing.
	e.signInAs("scoped-op", "operator", []string{north})
	before := e.auditTotal()
	res, _ := e.post("/ui/devices/"+e.dev["FRONTDESK-01"]+"/settings", url.Values{"notes": {"hijack"}, "device_class": {"desktop"}, "update_ring": {"stable"}}, false)
	if res.StatusCode != 404 {
		t.Errorf("settings post on an out-of-scope device: %d", res.StatusCode)
	}
	if d, _ := e.st.GetDevice(context.Background(), store.ScopeAll(), e.dev["FRONTDESK-01"]); d.Notes != "" || e.auditTotal() != before {
		t.Error("the out-of-scope device was changed or audited")
	}
	// An admin can see and edit everything.
	e.signInAs("ad", "admin", nil)
	mustContain(t, "admin customers", e.page("/customers", 200), "Summit Eye Care", "Northwind Dental")
}

func TestM2CreateCustomerThroughTheUI(t *testing.T) {
	e := newEnv(t)
	e.signInAs("ad", "admin", nil)
	form := e.page("/ui/customers/new", 200)
	mustContain(t, "form", form, `name="csrf_token"`, `name="name"`, `name="expected"`, `name="onboarding_until"`, `name="display_tz"`, `name="device_cap"`, `name="policy_id"`,
		`name="contact_name"`, "Default policy", "Name (required)", "Expected to have devices", "America/Chicago")
	before := e.auditTotal()
	res, body := e.post("/ui/customers", url.Values{"name": {"Delta Vision"}, "expected": {"1"}, "display_tz": {"America/Chicago"}, "device_cap": {"12"},
		"onboarding_until": {"2026-12-01T17:00"}, "contact_name": {"Lee Park", ""}, "contact_email": {"lee@delta.example", ""}, "contact_phone": {"555-0100", ""}, "contact_role": {"owner", ""},
		"notes": {"Pilot customer"}}, false)
	if res.StatusCode != 303 {
		t.Fatalf("create: %d %s", res.StatusCode, body)
	}
	loc := res.Header.Get("Location")
	if !strings.HasPrefix(loc, "/customers/") || !strings.HasSuffix(loc, "?saved=created") {
		t.Fatalf("redirect: %s", loc)
	}
	id := strings.TrimSuffix(strings.TrimPrefix(loc, "/customers/"), "?saved=created")
	rows := e.auditRows("customer.create", id)
	if len(rows) != 1 || e.auditTotal() != before+1 || rows[0].Method != "web" || rows[0].ActorLabel != "ad" || rows[0].CustomerID != id {
		t.Fatalf("want exactly one customer.create audit row: %+v (total %d, was %d)", rows, e.auditTotal(), before)
	}
	c, err := e.st.GetCustomer(context.Background(), store.ScopeAll(), id)
	if err != nil || !c.Expected || c.DeviceCap == nil || *c.DeviceCap != 12 || c.DisplayTZ != "America/Chicago" || len(store.ParseContacts(c.Contacts)) != 1 {
		t.Fatalf("stored customer: %v %+v", err, c)
	}
	// 5:00 PM in Chicago on 1 Dec is 23:00 UTC: stored UTC, shown in the display zone.
	if c.OnboardingUntil == nil || c.OnboardingUntil.UTC().Format("2006-01-02T15:04") != "2026-12-01T23:00" {
		t.Fatalf("onboarding_until: %v", c.OnboardingUntil)
	}
	page := e.page(loc, 200)
	mustContain(t, "created page", page, "Customer created.", "Delta Vision", "Lee Park", "delta.example", "Onboarding until", "5:00 PM", "Expected", "12")
	// The new expected customer has no devices yet: onboarding, not a quiet green and not yet MISSING.
	mustContain(t, "onboarding", page, "ONBOARDING")
	mustContain(t, "settings form", e.page("/customers/"+id+"/settings", 200), `value="Delta Vision"`, `value="2026-12-01T17:00"`, `value="12"`, `value="lee@delta.example"`)

	// Refusals re-render the form with every value kept and the reason beside the field.
	before = e.auditTotal()
	res, body = e.post("/ui/customers", url.Values{"name": {"Delta Two"}, "display_tz": {"Mars/Base"}, "device_cap": {"zero"}, "notes": {"Keep this text"}, "contact_name": {"Kept Person", ""}}, false)
	if res.StatusCode != 400 {
		t.Fatalf("refused create: %d", res.StatusCode)
	}
	mustContain(t, "refusal", body, "The customer was not created.", "Fix 2 fields", "Use an IANA zone name", "Use a whole number", `value="Delta Two"`, `value="Mars/Base"`,
		"Keep this text", `value="Kept Person"`, `aria-invalid="true"`, `role="alert"`)
	res, body = e.post("/ui/customers", url.Values{"name": {"delta vision"}}, false)
	if res.StatusCode != 400 {
		t.Fatalf("duplicate create: %d", res.StatusCode)
	}
	mustContain(t, "duplicate", body, "Another customer already has this name.", `value="delta vision"`)
	if e.auditTotal() != before {
		t.Fatal("a refused create wrote an audit row")
	}
}

func TestM2EditCustomerThroughTheUI(t *testing.T) {
	e := newEnv(t)
	e.signInAs("ad", "admin", nil)
	harbor := e.customerID("Harbor Family Practice")
	settings := e.page("/customers/"+harbor+"/settings", 200)
	mustContain(t, "settings tab", settings, `value="Harbor Family Practice"`, "Marcus Bell", "Save changes", `aria-current="page">Settings`)
	mustContain(t, "tabs", settings, `href="/customers/`+harbor+`">Overview`, `/devices">Devices`)
	// M2-W2A added the Policy tab (S17), shown to admins; this account is one.
	mustContain(t, "policy tab", settings, `href="/customers/`+harbor+`/policy">Policy`)
	before := e.auditTotal()
	// Mark the customer expected and give it a window.
	post := func(extra url.Values) (*http.Response, string) {
		v := url.Values{"name": {"Harbor Family Practice"}, "slug": {"harbor-family-practice"}, "contact_name": {"Marcus Bell", ""}, "contact_email": {"marcus@harbor-family.example", ""},
			"contact_phone": {"", ""}, "contact_role": {"practice administrator", ""}}
		for k, vs := range extra {
			v[k] = vs
		}
		return e.post("/ui/customers/"+harbor, v, false)
	}
	res, body := post(url.Values{"expected": {"1"}, "onboarding_until": {"2026-12-01T17:00"}})
	if res.StatusCode != 303 || res.Header.Get("Location") != "/customers/"+harbor+"/settings?saved=1" {
		t.Fatalf("edit: %d %s %s", res.StatusCode, res.Header.Get("Location"), body)
	}
	if rows := e.auditRows("customer.update", harbor); len(rows) != 1 || e.auditTotal() != before+1 || !strings.Contains(string(rows[0].Details), `"expected":{"from":false,"to":true}`) {
		t.Fatalf("audit: %+v", rows)
	}
	mustContain(t, "saved", e.page("/customers/"+harbor+"/settings?saved=1", 200), "Changes saved.", `name="expected" value="1" checked`)
	// Submitting the same values again is not a change: no new audit row.
	post(url.Values{"expected": {"1"}, "onboarding_until": {"2026-12-01T17:00"}})
	if len(e.auditRows("customer.update", harbor)) != 1 {
		t.Fatal("an unchanged save wrote an audit row")
	}
	// Not expected again, window cleared.
	post(url.Values{})
	if len(e.auditRows("customer.update", harbor)) != 2 {
		t.Fatal("clearing the flag is a change")
	}
	c, _ := e.st.GetCustomer(context.Background(), store.ScopeAll(), harbor)
	if c.Expected || c.OnboardingUntil != nil {
		t.Fatalf("cleared: %+v", c)
	}
	// A refusal keeps the values and says why.
	res, body = post(url.Values{"name": {"summit eye care"}, "notes": {"Kept notes"}})
	if res.StatusCode != 400 {
		t.Fatalf("rename onto another customer: %d", res.StatusCode)
	}
	mustContain(t, "refused edit", body, "The changes were not saved.", "Another customer already has this name.", `value="summit eye care"`, "Kept notes")
	if c, _ := e.st.GetCustomer(context.Background(), store.ScopeAll(), harbor); c.Name != "Harbor Family Practice" {
		t.Fatal("a refused edit renamed the customer")
	}
}

func TestM2CustomerPageStatesAndEvidence(t *testing.T) {
	e := newEnv(t)
	e.login()
	// An expected customer with no devices is a critical alarm, never a neutral empty state.
	blue := e.page("/customers/"+e.customerID("Blue Ridge Accounting"), 200)
	mustContain(t, "missing overview", blue, "MISSING", "No devices are enrolled for an expected customer", `role="alert"`, "Yes, and it has no devices", "every machine there is unmonitored")
	tab := e.page("/customers/"+e.customerID("Blue Ridge Accounting")+"/devices", 200)
	mustContain(t, "missing devices tab", tab, "No devices are enrolled for an expected customer", `class="panel-state alarm"`)
	// Inside its onboarding window the same absence is neutral and says until when.
	lake := e.page("/customers/"+e.customerID("Lakeshore Pediatrics")+"/devices", 200)
	mustContain(t, "onboarding tab", lake, "Onboarding: no devices yet", `class="panel-state neutral"`, "(America/Chicago)")
	// A customer with failures lists what needs attention, worst first, and links to the device.
	summit := e.page("/customers/"+e.customerID("Summit Eye Care"), 200)
	mustContain(t, "attention", summit, "Needs attention", "FRONTDESK-01", "FAILED", "LAB-RETINA-02", `href="/devices/`+e.dev["FRONTDESK-01"]+`"`, "Dana Whitfield", "Health")
	// A healthy customer prints its evidence instead of an empty box.
	mustContain(t, "healthy", e.page("/customers/"+e.customerID("Northwind Dental"), 200), "Nothing needs attention.", "checked at 1:42:10 PM (America/Chicago)")
	// A customer that does not exist is a 404 page, not an empty one.
	mustContain(t, "unknown", e.page("/customers/01J9Y4K3A0B1C2D3E4F5G6H7ZZ", 404), "Not found")
}

func TestM2DevicesTableFiltersSortsAndPages(t *testing.T) {
	e := newEnv(t)
	e.login()
	hostsOf := func(body string) []string {
		var out []string
		for _, m := range regexp.MustCompile(`class="hostname" href="/devices/[0-9A-Z]+">([^<]+)<`).FindAllStringSubmatch(body, -1) {
			out = append(out, m[1])
		}
		return out
	}
	all := hostsOf(e.page("/devices", 200))
	if len(all) != 25 {
		t.Fatalf("default list: %d rows", len(all))
	}
	// Default order is exceptions first, never the alphabet.
	if all[0] == "BILLING-H3" {
		t.Fatalf("not exception-first: %v", all[:3])
	}
	byHost := hostsOf(e.page("/devices?sort=host", 200))
	for i := 1; i < len(byHost); i++ {
		if strings.ToLower(byHost[i-1]) > strings.ToLower(byHost[i]) {
			t.Fatalf("sort=host out of order at %d: %v", i, byHost)
		}
	}
	desc := hostsOf(e.page("/devices?sort=host&dir=desc", 200))
	if desc[0] != byHost[len(byHost)-1] {
		t.Fatalf("sort=host desc starts with %s", desc[0])
	}
	if b := e.page("/devices?sort=host", 200); !strings.Contains(b, `aria-sort="ascending"`) || !strings.Contains(b, `href="/devices?dir=desc&amp;sort=host"`) {
		t.Fatal("the active header must say ascending and offer the descending link")
	}
	for _, k := range []string{"customer", "state", "last_success", "next_due", "agent", "os"} {
		if got := hostsOf(e.page("/devices?sort="+k, 200)); len(got) != 25 {
			t.Errorf("sort=%s: %d rows", k, len(got))
		}
	}
	if got := hostsOf(e.page("/devices?state=FAILED", 200)); len(got) != 1 || got[0] != "FRONTDESK-01" {
		t.Fatalf("state filter: %v", got)
	}
	b := e.page("/devices?state=FAILED&state=UNSEEN", 200)
	if got := hostsOf(b); len(got) != 2 {
		t.Fatalf("two states: %v", got)
	}
	mustContain(t, "chips", b, "Remove filter: State: FAILED", "Remove filter: State: UNSEEN", "Clear all", "Showing 1-2 of 2")
	if got := hostsOf(e.page("/devices?q=legal", 200)); len(got) != 3 {
		t.Fatalf("search by customer name: %v", got)
	}
	if got := hostsOf(e.page("/devices?customer="+e.customerID("Pinecrest Legal"), 200)); len(got) != 3 {
		t.Fatalf("customer filter: %v", got)
	}
	// Retired devices are hidden unless asked for; that is one more row, and page 2 at 25 per page.
	r1 := e.page("/devices?retired=1&size=25", 200)
	mustContain(t, "paged", r1, "Showing 1-25 of 26", `aria-label="Next page"`, `href="/devices?page=2&amp;retired=1&amp;size=25"`, `aria-label="25 per page"`, "Including retired")
	r2 := e.page("/devices?retired=1&size=25&page=2", 200)
	if got := hostsOf(r2); len(got) != 1 {
		t.Fatalf("page 2: %v", got)
	}
	mustContain(t, "page 2", r2, "Showing 26-26 of 26", `aria-current="page"`, `aria-label="Previous page"`)
	mustContain(t, "retired row", e.page("/devices?retired=1&q=OLD-RECEPTION", 200), "RETIRED", "OLD-RECEPTION-PC")
	// A page past the end shows the last page and says so.
	mustContain(t, "clamped", e.page("/devices?retired=1&size=25&page=9", 200), "past the end", "Showing 26-26 of 26")
	// Filter-caused empty: neutral, with the denominator and a way out.
	none := e.page("/devices?q=no-such-host", 200)
	mustContain(t, "no match", none, "No devices match these filters", "0 of 25 shown.", "Clear filters", `class="panel-state neutral"`)
	// Unknown filters and sorts are reported, never silently ignored.
	mustContain(t, "bad state", e.page("/devices?state=HEALTHY", 400), "Filters not valid", "Unknown device state.")
	mustContain(t, "bad sort", e.page("/devices?sort=colour", 400), "Filters not valid")
	// The customer page's Devices tab is the same table for one customer.
	tab := e.page("/customers/"+e.customerID("Harbor Family Practice")+"/devices?sort=host", 200)
	if got := hostsOf(tab); len(got) != 6 || strings.Contains(tab, `href="/customers/`+e.customerID("Harbor Family Practice")+`">Harbor`) {
		t.Fatalf("customer tab: %v", got)
	}
	mustContain(t, "tab", tab, "NEW-EXAM-6", `aria-current="page">Devices`, `hx-vals='{"customer":"`)
	if strings.Contains(tab, `<th scope="col" aria-sort="none"><a class="sortlink" href="/customers/`+e.customerID("Harbor Family Practice")+`/devices?sort=customer`) {
		t.Fatal("the customer column belongs to /devices only")
	}
}

func TestM2DevicesFragmentPushesTheCanonicalURL(t *testing.T) {
	e := newEnv(t)
	e.login()
	res, body := e.do("GET", "/ui/devices/table?state=FAILED&sort=host", nil, "HX-Request", "true")
	if res.StatusCode != 200 || res.Header.Get("HX-Push-Url") != "/devices?sort=host&state=FAILED" {
		t.Fatalf("fragment: %d push=%q", res.StatusCode, res.Header.Get("HX-Push-Url"))
	}
	if strings.Contains(body, "<html") || strings.Contains(body, "sidebar") || !strings.Contains(body, "FRONTDESK-01") {
		t.Fatal("the fragment must be the results region only")
	}
	harbor := e.customerID("Harbor Family Practice")
	res, _ = e.do("GET", "/ui/devices/table?tab=1&customer="+harbor+"&state=OK", nil)
	if res.Header.Get("HX-Push-Url") != "/customers/"+harbor+"/devices?state=OK" {
		t.Fatalf("customer tab push: %q", res.Header.Get("HX-Push-Url"))
	}
	if res, _ := e.do("GET", "/ui/devices/table?sort=colour", nil); res.StatusCode != 400 {
		t.Fatalf("bad sort fragment: %d", res.StatusCode)
	}
}

func TestM2NoEnrolledDevicesIsAnAlarm(t *testing.T) {
	e := newEnv(t)
	// A scoped account whose only customer has no devices: "no data" where data must exist.
	blue := e.customerID("Blue Ridge Accounting")
	e.signInAs("blue-op", "operator", []string{blue})
	body := e.page("/devices", 200)
	mustContain(t, "alarm", body, "No devices are enrolled", `class="panel-state alarm"`, `role="alert"`)
	if strings.Contains(body, "No devices match") {
		t.Fatal("an empty fleet is not a filter result")
	}
}

func TestM2DevicePanelTabs(t *testing.T) {
	e := newEnv(t)
	e.signInAs("ad", "admin", nil)
	id := e.dev["FRONTDESK-01"]
	sum := e.page("/devices/"+id, 200)
	mustContain(t, "summary", sum, `aria-current="page">Summary`, `href="/devices/`+id+`/history">History`, `href="/devices/`+id+`/settings">Settings`, `href="/devices/`+id+`/audit">Audit`,
		"Last ten runs", "Facts", `id="device-live"`, `data-events="/ui/events?device=`, `href="/customers/`, "Summit Eye Care")
	hist := e.page("/devices/"+id+"/history", 200)
	mustContain(t, "history", hist, `aria-current="page">History`, `name="kind"`, `name="outcome"`, `name="range"`, "Run history, newest first", "Showing 1-", `<th scope="col">Started</th>`, "Apply")
	if strings.Contains(hist, `id="device-live"`) || strings.Contains(hist, "data-events=\"/ui/events?device=") {
		t.Fatal("only the summary tab is live-swapped")
	}
	// Times are 12-hour in the display zone.
	if !regexp.MustCompile(`<time datetime="[^"]+">\w{3} \d{1,2} \w{3}, \d{1,2}:\d\d (AM|PM)</time>`).MatchString(hist) {
		t.Fatal("run times must read 'Fri 30 Oct, 1:42 PM'")
	}
	if regexp.MustCompile(`<time datetime="[^"]+">[^<]*\b(1[3-9]|2[0-3]):\d\d\b`).MatchString(hist) {
		t.Fatal("a 24-hour time on the history tab")
	}
	// Filters.
	fail := e.page("/devices/"+id+"/history?outcome=failed", 200)
	mustContain(t, "failed runs", fail, "Remove filter: Outcome: failed", "FAILED")
	none := e.page("/devices/"+id+"/history?kind=unlock", 200)
	mustContain(t, "no unlock runs", none, "No runs match these filters", "Clear filters")
	mustContain(t, "range", e.page("/devices/"+id+"/history?range=24h", 200), "Last 24 hours")
	mustContain(t, "bad kind", e.page("/devices/"+id+"/history?kind=format", 400), "Filters not valid")
	mustContain(t, "unknown tab", e.page("/devices/"+id+"/keys", 404), "Not found")
	mustContain(t, "summary alias", e.page("/devices/"+id+"/summary", 200), "Last ten runs")
	mustContain(t, "restore still routes", e.page("/devices/"+e.dev["DR-OFFICE-PC"]+"/restore", 200), "Which file do you need back")

	// Settings: edit, audited once, shown in the audit tab.
	mustContain(t, "settings", e.page("/devices/"+id+"/settings", 200), `name="friendly_name"`, `name="device_class"`, `name="update_ring"`, `name="notes"`, "Save settings", "Device facts", `name="csrf_token"`)
	before := e.auditTotal()
	res, body := e.post("/ui/devices/"+id+"/settings", url.Values{"friendly_name": {"Front desk PC"}, "notes": {"Behind the counter"}, "device_class": {"laptop"}, "update_ring": {"pilot"}}, false)
	if res.StatusCode != 303 || res.Header.Get("Location") != "/devices/"+id+"/settings?saved=1" {
		t.Fatalf("save: %d %s %s", res.StatusCode, res.Header.Get("Location"), body)
	}
	rows := e.auditRows("device.update", id)
	if len(rows) != 1 || e.auditTotal() != before+1 || rows[0].Method != "web" || rows[0].ActorLabel != "ad" {
		t.Fatalf("want one device.update row: %+v", rows)
	}
	saved := e.page("/devices/"+id+"/settings?saved=1", 200)
	mustContain(t, "saved", saved, "Changes saved.", `value="Front desk PC"`, "Behind the counter", `value="pilot" selected`, `value="laptop" selected`)
	mustContain(t, "renamed in the table", e.page("/devices?q=front+desk+pc", 200), "FRONTDESK-01", "Front desk PC")
	// Saving the same values writes nothing.
	e.post("/ui/devices/"+id+"/settings", url.Values{"friendly_name": {"Front desk PC"}, "notes": {"Behind the counter"}, "device_class": {"laptop"}, "update_ring": {"pilot"}}, false)
	if len(e.auditRows("device.update", id)) != 1 {
		t.Fatal("an unchanged save wrote an audit row")
	}
	aud := e.page("/devices/"+id+"/audit", 200)
	mustContain(t, "audit", aud, `aria-current="page">Audit`, "Settings changed", "Changed friendly name, notes, device class, update ring", "class desktop to laptop", "ring stable to pilot", "ad", "Read-only", "OK")
	if strings.Contains(aud, "Behind the counter") || strings.Contains(aud, "Front desk PC") || strings.Contains(aud, `<form class="formcard"`) {
		t.Fatal("the audit tab must be read-only and must not repeat free text")
	}
	// A refusal keeps what was typed and says why; nothing is written.
	before = e.auditTotal()
	res, body = e.post("/ui/devices/"+id+"/settings", url.Values{"friendly_name": {strings.Repeat("x", 121)}, "notes": {"Kept text"}, "device_class": {"tablet"}, "update_ring": {"beta"}}, false)
	if res.StatusCode != 400 {
		t.Fatalf("refused: %d", res.StatusCode)
	}
	mustContain(t, "refusal", body, "The settings were not saved.", "at most 120 characters", "Use unknown, laptop or desktop.", "Use stable or pilot.", "Kept text", `role="alert"`, `aria-invalid="true"`)
	if e.auditTotal() != before {
		t.Fatal("a refused save wrote an audit row")
	}
}

func TestM2DeviceSettingsReadOnlyBelowOperator(t *testing.T) {
	e := newEnv(t)
	e.signInAs("ro", "read_only", nil)
	b := e.page("/devices/"+e.dev["FRONTDESK-01"]+"/settings", 200)
	mustContain(t, "read-only", b, "needs the operator role", "<fieldset class=\"plain stack\" disabled>")
	if strings.Contains(b, "Save settings") {
		t.Fatal("a read-only account is offered a save button")
	}
	if strings.Contains(e.page("/devices/"+e.dev["FRONTDESK-01"], 200), `/audit">Audit`) {
		t.Fatal("the audit tab is for admins")
	}
	// A retired device cannot be edited either.
	e.signInAs("op", "operator", nil)
	mustContain(t, "retired", e.page("/devices/"+e.dev["OLD-RECEPTION-PC"]+"/settings", 200), "This device is retired")
	res, _ := e.post("/ui/devices/"+e.dev["OLD-RECEPTION-PC"]+"/settings", url.Values{"notes": {"x"}, "device_class": {"desktop"}, "update_ring": {"stable"}}, false)
	if res.StatusCode != 409 {
		t.Fatalf("settings post on a retired device: %d", res.StatusCode)
	}
}

func TestM2ChangesAreRefusedLoudlyWhenTheyCannotBeAudited(t *testing.T) {
	e := newEnv(t)
	e.signInAs("ad", "admin", nil)
	if err := e.st.Write(context.Background(), func(tx *store.Tx) error {
		_, err := tx.Exec(`CREATE TRIGGER refuse_audits BEFORE INSERT ON audit_events BEGIN SELECT RAISE(ABORT, 'audit refused by the test'); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	id := e.dev["FRONTDESK-01"]
	harbor := e.customerID("Harbor Family Practice")
	for name, tc := range map[string]struct {
		path string
		form url.Values
	}{
		"device settings": {"/ui/devices/" + id + "/settings", url.Values{"notes": {"unauditable"}, "device_class": {"desktop"}, "update_ring": {"stable"}}},
		"customer create": {"/ui/customers", url.Values{"name": {"Unauditable Co"}}},
		"customer edit":   {"/ui/customers/" + harbor, url.Values{"name": {"Harbor Family Practice"}, "slug": {"harbor-family-practice"}, "expected": {"1"}}},
	} {
		res, body := e.post(tc.path, tc.form, false)
		if res.StatusCode != 500 {
			t.Errorf("%s: want 500, got %d", name, res.StatusCode)
			continue
		}
		mustContain(t, name, body, "NOT AUDITED", "Nothing was changed")
		if strings.Contains(body, "audit refused by the test") {
			t.Errorf("%s: the page leaks the cause", name)
		}
	}
	if d, _ := e.st.GetDevice(context.Background(), store.ScopeAll(), id); d.Notes != "" {
		t.Error("the device changed although its audit row could not be written")
	}
	if c, _ := e.st.GetCustomer(context.Background(), store.ScopeAll(), harbor); c.Expected {
		t.Error("the customer changed although its audit row could not be written")
	}
	if rows, _ := e.st.ListCustomerRows(context.Background(), store.ScopeAll(), store.CustomerListQuery{Search: "Unauditable"}); len(rows) != 0 {
		t.Error("the customer was created although its audit row could not be written")
	}
}

func TestM2SettingsPostChangesTheBoardsFreshness(t *testing.T) {
	e := newEnv(t)
	e.signInAs("ad", "admin", nil)
	// A change marks the fleet dirty so open boards refresh at the next coalesced tick.
	sub, cancel := e.hub.Subscribe(&Sub{Scope: store.ScopeAll(), Role: auth.RoleAdmin})
	defer cancel()
	_ = sub
	e.hub.mu.Lock()
	e.hub.dirty = false
	e.hub.mu.Unlock()
	e.post("/ui/devices/"+e.dev["FRONTDESK-01"]+"/settings", url.Values{"notes": {"hub"}, "device_class": {"desktop"}, "update_ring": {"stable"}}, false)
	e.hub.mu.Lock()
	dirty := e.hub.dirty
	e.hub.mu.Unlock()
	if !dirty {
		t.Fatal("a saved change must mark the fleet dirty")
	}
}
