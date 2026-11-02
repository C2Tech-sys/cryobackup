package adminapi

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

const pw = "pw-for-tests-1"

func (e *env) customerNamed(name string) store.Customer {
	e.t.Helper()
	cs, err := e.st.ListCustomers(context.Background(), store.ScopeAll())
	if err != nil {
		e.t.Fatal(err)
	}
	for _, c := range cs {
		if c.Name == name {
			return c
		}
	}
	e.t.Fatalf("no fixture customer %q", name)
	return store.Customer{}
}

func (e *env) auditOf(action, objectID string) []store.AuditEvent {
	e.t.Helper()
	l, err := e.st.ListAudit(context.Background(), store.ScopeAll(), store.AuditFilter{Action: action, ObjectID: objectID}, store.Page{Limit: 100})
	if err != nil {
		e.t.Fatal(err)
	}
	return l.Items
}

func (e *env) audits() int {
	e.t.Helper()
	l, err := e.st.ListAudit(context.Background(), store.ScopeAll(), store.AuditFilter{}, store.Page{Limit: 1000})
	if err != nil {
		e.t.Fatal(err)
	}
	return len(l.Items)
}

func decode[T any](t *testing.T, b []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	return v
}

func (e *env) get(path string, s *session) (int, []byte) {
	e.t.Helper()
	res, b := e.req("GET", path, s, nil)
	return res.StatusCode, b
}

// problemFields returns the field names a 400 problem body lists.
func problemFields(t *testing.T, b []byte) map[string]string {
	t.Helper()
	var p struct {
		Code   string             `json:"code"`
		Errors []store.FieldError `json:"errors"`
	}
	if err := json.Unmarshal(b, &p); err != nil || p.Code != api.CodeInvalidRequest {
		t.Fatalf("not an invalid_request problem: %s", b)
	}
	out := map[string]string{}
	for _, f := range p.Errors {
		out[f.Field] = f.Message
	}
	return out
}

func TestM2RoleMatrix(t *testing.T) {
	e := newEnv(t)
	summit := e.customerNamed("Summit Eye Care")
	roles := map[string]*session{}
	for _, r := range []string{"read_only", "operator", "admin", "owner"} {
		e.addAdmin("u-"+r, r, nil)
		roles[r] = e.login("u-"+r, pw)
	}
	n := 0
	type call struct {
		name   string
		method string
		path   string
		body   func() any
		min    string // lowest role allowed
	}
	order := []string{"read_only", "operator", "admin", "owner"}
	calls := []call{
		{"list customers", "GET", "/api/v1/customers", nil, "read_only"},
		{"get customer", "GET", "/api/v1/customers/" + summit.ID, nil, "read_only"},
		{"customer devices", "GET", "/api/v1/customers/" + summit.ID + "/devices", nil, "read_only"},
		{"device runs", "GET", "/api/v1/devices/" + e.dev.ID + "/runs", nil, "read_only"},
		{"create customer", "POST", "/api/v1/customers", func() any { n++; return map[string]any{"name": "Matrix Co " + strings.Repeat("x", n)} }, "admin"},
		{"patch customer", "PATCH", "/api/v1/customers/" + summit.ID, func() any { return map[string]any{"notes": "matrix"} }, "admin"},
		{"patch device", "PATCH", "/api/v1/devices/" + e.dev.ID, func() any { return map[string]any{"notes": "matrix"} }, "operator"},
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
				t.Errorf("%s as %s: want success, got %d %s", c.name, role, res.StatusCode, b)
			case !allowed && (res.StatusCode != 403 || problemCode(t, res, b) != api.CodeForbidden):
				t.Errorf("%s as %s: want 403 forbidden, got %d %s", c.name, role, res.StatusCode, b)
			}
		}
	}
}

func indexOf(l []string, v string) int {
	for i, s := range l {
		if s == v {
			return i
		}
	}
	return -1
}

func TestM2WritesNeedCSRFAndASession(t *testing.T) {
	e := newEnv(t)
	s := e.login("admin", pw)
	bad := &session{cookies: s.cookies, csrf: "wrong"}
	before := e.audits()
	summit := e.customerNamed("Summit Eye Care")
	for _, tc := range []struct{ method, path string }{{"POST", "/api/v1/customers"}, {"PATCH", "/api/v1/customers/" + summit.ID}, {"PATCH", "/api/v1/devices/" + e.dev.ID}} {
		res, b := e.req(tc.method, tc.path, bad, map[string]any{"name": "Csrf Co", "notes": "x"})
		if res.StatusCode != 403 || problemCode(t, res, b) != api.CodeForbidden {
			t.Errorf("%s %s with a bad CSRF token: %d", tc.method, tc.path, res.StatusCode)
		}
	}
	if e.audits() != before {
		t.Fatal("a CSRF failure changed something")
	}
}

func TestM2CustomerListFiltersSortsAndPages(t *testing.T) {
	e := newEnv(t)
	s := e.login("admin", pw)
	list := func(q string) CustomerListResponse {
		code, b := e.get("/api/v1/customers"+q, s)
		if code != 200 {
			t.Fatalf("%s: %d %s", q, code, b)
		}
		return decode[CustomerListResponse](t, b)
	}
	all := list("")
	if all.Total != 6 || all.Of != 6 || len(all.Customers) != 6 || !all.Complete || all.ComputedAt.IsZero() || all.Truncated {
		t.Fatalf("all: total=%d of=%d len=%d", all.Total, all.Of, len(all.Customers))
	}
	// The engine's order: the expected customer with no devices comes first.
	if all.Customers[0].Name != "Blue Ridge Accounting" || all.Customers[0].Rollup == nil || all.Customers[0].Rollup.Status != "missing" {
		t.Fatalf("first row: %+v", all.Customers[0])
	}
	var summit CustomerJSON
	for _, c := range all.Customers {
		if c.Name == "Summit Eye Care" {
			summit = c
		}
	}
	if summit.Devices < 10 || summit.Rollup == nil || summit.Rollup.Status != "alarm" || summit.Rollup.Counts["FAILED"] != 1 || len(summit.Contacts) != 1 || summit.PolicyName == "" && summit.PolicyID != "" {
		t.Fatalf("Summit rollup: %+v", summit)
	}
	if n := list("?expected=true").Total; n != 2 {
		t.Errorf("expected=true: %d", n)
	}
	if n := list("?expected=false").Total; n != 4 {
		t.Errorf("expected=false: %d", n)
	}
	if c := list("?health=missing"); c.Total != 1 || c.Of != 6 || c.Customers[0].Name != "Blue Ridge Accounting" {
		t.Errorf("health=missing: %+v", c)
	}
	if c := list("?q=family"); c.Total != 1 || c.Customers[0].Name != "Harbor Family Practice" {
		t.Errorf("q: %+v", c)
	}
	if c := list("?sort=name"); c.Customers[0].Name != "Blue Ridge Accounting" || c.Customers[5].Name != "Summit Eye Care" {
		t.Errorf("sort=name: %v / %v", c.Customers[0].Name, c.Customers[5].Name)
	}
	if c := list("?sort=name&dir=desc"); c.Customers[0].Name != "Summit Eye Care" {
		t.Errorf("sort=name desc: %v", c.Customers[0].Name)
	}
	p1 := list("?limit=4&sort=name")
	if len(p1.Customers) != 4 || !p1.Truncated || p1.NextCursor != "o4" {
		t.Fatalf("page 1: %+v", p1)
	}
	if p2 := list("?limit=4&sort=name&cursor=" + p1.NextCursor); len(p2.Customers) != 2 || p2.Truncated || p2.Customers[0].Name != "Pinecrest Legal" {
		t.Fatalf("page 2: %d first=%s", len(p2.Customers), p2.Customers[0].Name)
	}
	for _, q := range []string{"?limit=0", "?limit=501", "?cursor=zz", "?expected=maybe", "?health=bogus", "?sort=bogus", "?dir=up", "?state=bogus"} {
		res, b := e.req("GET", "/api/v1/customers"+q, s, nil)
		if res.StatusCode != 400 || problemCode(t, res, b) != api.CodeInvalidRequest {
			t.Errorf("%s: %d", q, res.StatusCode)
		}
	}
}

func TestM2ScopedOperatorNeverSeesAnotherCustomer(t *testing.T) {
	e := newEnv(t)
	harbor, summit, nw := e.customerNamed("Harbor Family Practice"), e.customerNamed("Summit Eye Care"), e.customerNamed("Northwind Dental")
	e.addAdmin("scoped", "operator", []string{nw.ID})
	e.addAdmin("scoped-ro", "read_only", []string{nw.ID})
	for _, who := range []string{"scoped", "scoped-ro"} {
		sc := e.login(who, pw)
		code, b := e.get("/api/v1/customers", sc)
		list := decode[CustomerListResponse](t, b)
		if code != 200 || list.Total != 1 || list.Of != 1 || list.Customers[0].ID != nw.ID {
			t.Fatalf("%s list: %d %s", who, code, b)
		}
		// Searching for another customer's name finds nothing.
		if code, b := e.get("/api/v1/customers?q=summit", sc); code != 200 || decode[CustomerListResponse](t, b).Total != 0 {
			t.Fatalf("%s searched out-of-scope: %d %s", who, code, b)
		}
		for _, id := range []string{summit.ID, harbor.ID, "01J9Y4K3A0B1C2D3E4F5G6H7ZZ"} {
			for _, suffix := range []string{"", "/devices"} {
				res, body := e.req("GET", "/api/v1/customers/"+id+suffix, sc, nil)
				if res.StatusCode != 404 || problemCode(t, res, body) != api.CodeNotFound {
					t.Errorf("%s GET customers/%s%s: want 404, got %d", who, id[:4], suffix, res.StatusCode)
				}
			}
		}
		if code, b := e.get("/api/v1/customers/"+nw.ID+"/devices", sc); code != 200 || decode[CustomerDevicesResponse](t, b).Total != 3 {
			t.Fatalf("%s own devices: %d %s", who, code, b)
		}
		// Another customer's device: runs and settings are 404 as well.
		res, body := e.req("GET", "/api/v1/devices/"+e.dev.ID+"/runs", sc, nil)
		if res.StatusCode != 404 {
			t.Errorf("%s runs of an out-of-scope device: %d %s", who, res.StatusCode, body)
		}
	}
	sc := e.login("scoped", pw)
	before := e.audits()
	res, b := e.req("PATCH", "/api/v1/devices/"+e.dev.ID, sc, map[string]any{"notes": "hijack"})
	if res.StatusCode != 404 || problemCode(t, res, b) != api.CodeNotFound {
		t.Fatalf("patch of an out-of-scope device: %d %s", res.StatusCode, b)
	}
	d, _ := e.st.GetDevice(context.Background(), store.ScopeAll(), e.dev.ID)
	if d.Notes != "" || e.audits() != before {
		t.Fatal("the out-of-scope device was changed or audited")
	}
	// Customer create and edit are admin-only; the scoped operator is refused before the store.
	res, _ = e.req("POST", "/api/v1/customers", sc, map[string]any{"name": "Scoped Co"})
	if res.StatusCode != 403 {
		t.Fatalf("scoped operator creating a customer: %d", res.StatusCode)
	}
}

func TestM2CreateCustomerWritesOneAuditRow(t *testing.T) {
	e := newEnv(t)
	s := e.login("admin", pw)
	before := e.audits()
	res, b := e.req("POST", "/api/v1/customers", s, CreateCustomerRequest{Name: "Delta Vision", Expected: true, DisplayTZ: "America/Chicago",
		Contacts: []ContactJSON{{Name: "Lee", Email: "lee@delta.example", Role: "owner"}}})
	if res.StatusCode != 201 {
		t.Fatalf("create: %d %s", res.StatusCode, b)
	}
	c := decode[CustomerResponse](t, b)
	if c.ID == "" || c.Slug != "delta-vision" || !c.Expected || c.State != "active" || c.ComputedAt.IsZero() || len(c.Contacts) != 1 ||
		res.Header.Get("Location") != "/api/v1/customers/"+c.ID {
		t.Fatalf("created: %+v (%s)", c, res.Header.Get("Location"))
	}
	// An expected customer with no devices is MISSING at once: no quiet green.
	if c.Rollup == nil || c.Rollup.Status != "missing" {
		t.Fatalf("rollup: %+v", c.Rollup)
	}
	rows := e.auditOf("customer.create", c.ID)
	if len(rows) != 1 || e.audits() != before+1 {
		t.Fatalf("want exactly one customer.create row: %d (total %d, was %d)", len(rows), e.audits(), before)
	}
	if ev := rows[0]; ev.Method != "api" || ev.ActorLabel != "admin" || ev.CustomerID != c.ID || ev.Result != "ok" {
		t.Fatalf("audit row: %+v", ev)
	}
	// Refusals: field errors listed, nothing written.
	before = e.audits()
	for name, tc := range map[string]struct {
		body   any
		status int
		field  string
	}{
		"duplicate":    {CreateCustomerRequest{Name: "delta vision"}, 400, "name"},
		"no name":      {CreateCustomerRequest{}, 400, "name"},
		"bad zone":     {CreateCustomerRequest{Name: "Echo", DisplayTZ: "Nope/Nope"}, 400, "display_tz"},
		"bad cap":      {map[string]any{"name": "Echo", "device_cap": 0}, 400, "device_cap"},
		"bad policy":   {map[string]any{"name": "Echo", "policy_id": "01J9Y4K3A0B1C2D3E4F5G6H7ZZ"}, 400, "policy_id"},
		"bad contact":  {CreateCustomerRequest{Name: "Echo", Contacts: []ContactJSON{{Name: "", Email: "x"}}}, 400, "contacts"},
		"unknown key":  {`{"name":"Echo","admin":true}`, 400, ""},
		"not an array": {`{"name":"Echo","contacts":"lee"}`, 400, ""},
		"trailing":     {`{"name":"Echo"} {}`, 400, ""},
	} {
		res, b := e.req("POST", "/api/v1/customers", s, tc.body)
		if res.StatusCode != tc.status {
			t.Errorf("%s: %d %s", name, res.StatusCode, b)
			continue
		}
		if tc.field != "" && problemFields(t, b)[tc.field] == "" {
			t.Errorf("%s: field %q not listed: %s", name, tc.field, b)
		}
		if tc.field == "" {
			problemCode(t, res, b)
		}
	}
	if e.audits() != before {
		t.Fatal("refused creates wrote audit rows")
	}
}

func TestM2PatchCustomer(t *testing.T) {
	e := newEnv(t)
	s := e.login("admin", pw)
	harbor := e.customerNamed("Harbor Family Practice")
	path := "/api/v1/customers/" + harbor.ID
	before := e.audits()
	res, b := e.req("PATCH", path, s, `{"expected":true,"onboarding_until":"2026-12-01T00:00:00Z","device_cap":40,"notes":"Mark expected"}`)
	if res.StatusCode != 200 {
		t.Fatalf("patch: %d %s", res.StatusCode, b)
	}
	c := decode[CustomerResponse](t, b)
	if !c.Expected || c.OnboardingUntil == nil || c.DeviceCap == nil || *c.DeviceCap != 40 || c.Notes != "Mark expected" {
		t.Fatalf("patched: %+v", c)
	}
	if rows := e.auditOf("customer.update", harbor.ID); len(rows) != 1 || e.audits() != before+1 || !strings.Contains(string(rows[0].Details), `"expected":{"from":false,"to":true}`) {
		t.Fatalf("audit: %d rows", len(rows))
	}
	// Same again: nothing changes, nothing is audited. null clears.
	if res, _ := e.req("PATCH", path, s, `{"expected":true}`); res.StatusCode != 200 || len(e.auditOf("customer.update", harbor.ID)) != 1 {
		t.Fatal("a no-op patch must succeed without an audit row")
	}
	res, b = e.req("PATCH", path, s, `{"onboarding_until":null,"device_cap":null,"notes":null,"expected":false}`)
	c = decode[CustomerResponse](t, b)
	if res.StatusCode != 200 || c.Expected || c.OnboardingUntil != nil || c.DeviceCap != nil || c.Notes != "" {
		t.Fatalf("cleared: %d %+v", res.StatusCode, c)
	}
	// Refusals.
	for name, tc := range map[string]struct {
		body   string
		status int
		field  string
	}{
		"empty name":      {`{"name":""}`, 400, "name"},
		"taken name":      {`{"name":"summit eye care"}`, 400, "name"},
		"bad zone":        {`{"display_tz":"Mars/Base"}`, 400, "display_tz"},
		"expected null":   {`{"expected":null}`, 400, ""},
		"unknown field":   {`{"state":"archived"}`, 400, ""},
		"bad policy":      {`{"policy_id":"01J9Y4K3A0B1C2D3E4F5G6H7ZZ"}`, 400, "policy_id"},
		"wrong type":      {`{"expected":"yes"}`, 400, ""},
		"not json object": {`[1]`, 400, ""},
	} {
		res, b := e.req("PATCH", path, s, tc.body)
		if res.StatusCode != tc.status {
			t.Errorf("%s: %d %s", name, res.StatusCode, b)
		} else if tc.field != "" && problemFields(t, b)[tc.field] == "" {
			t.Errorf("%s: field %q not listed: %s", name, tc.field, b)
		}
	}
	res, b = e.req("PATCH", "/api/v1/customers/01J9Y4K3A0B1C2D3E4F5G6H7ZZ", s, `{"notes":"x"}`)
	if res.StatusCode != 404 || problemCode(t, res, b) != api.CodeNotFound {
		t.Fatalf("unknown customer: %d", res.StatusCode)
	}
}

func TestM2ChangesAreRefusedWhenTheyCannotBeAudited(t *testing.T) {
	e := newEnv(t)
	s := e.login("admin", pw)
	harbor := e.customerNamed("Harbor Family Practice")
	if err := e.st.Write(context.Background(), func(tx *store.Tx) error {
		_, err := tx.Exec(`CREATE TRIGGER refuse_audits BEFORE INSERT ON audit_events BEGIN SELECT RAISE(ABORT, 'audit refused by the test'); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ method, path, body string }{
		{"POST", "/api/v1/customers", `{"name":"Unauditable Co"}`},
		{"PATCH", "/api/v1/customers/" + harbor.ID, `{"expected":true}`},
		{"PATCH", "/api/v1/devices/" + e.dev.ID, `{"notes":"unauditable"}`},
	} {
		res, b := e.req(tc.method, tc.path, s, tc.body)
		if res.StatusCode != 500 || problemCode(t, res, b) != api.CodeInternal {
			t.Errorf("%s %s: %d %s", tc.method, tc.path, res.StatusCode, b)
		}
		if strings.Contains(string(b), "audit refused") {
			t.Errorf("the problem body leaks the cause: %s", b)
		}
	}
	if c := e.customerNamed("Harbor Family Practice"); c.Expected {
		t.Fatal("the customer changed although its audit row could not be written")
	}
	if d, _ := e.st.GetDevice(context.Background(), store.ScopeAll(), e.dev.ID); d.Notes != "" {
		t.Fatal("the device changed although its audit row could not be written")
	}
}

func TestM2CustomerDevices(t *testing.T) {
	e := newEnv(t)
	s := e.login("admin", pw)
	summit := e.customerNamed("Summit Eye Care")
	list := func(q string) CustomerDevicesResponse {
		code, b := e.get("/api/v1/customers/"+summit.ID+"/devices"+q, s)
		if code != 200 {
			t.Fatalf("%s: %d %s", q, code, b)
		}
		return decode[CustomerDevicesResponse](t, b)
	}
	all := list("")
	if all.Total != 13 || all.Of != 13 || !all.Complete || all.Truncated { // 14 fixture devices, one retired
		t.Fatalf("all: total=%d of=%d", all.Total, all.Of)
	}
	for _, d := range all.Devices {
		if d.Customer != "Summit Eye Care" || d.NextDue == nil || d.NextDue.Kind == "" || d.UpdateRing == "" {
			t.Fatalf("row: %+v", d)
		}
		if d.State == "RETIRED" {
			t.Fatal("retired devices are hidden by default")
		}
	}
	// Exception-first by default: critical states lead and the healthy device is not first.
	if all.Devices[0].Severity != "critical" || all.Devices[0].State == "OK" {
		t.Fatalf("first row: %+v", all.Devices[0])
	}
	if f := list("?state=failed,stale"); f.Total != 2 || f.Of != 13 {
		t.Errorf("state filter: %d of %d", f.Total, f.Of)
	}
	if f := list("?state=FAILED&state=UNSEEN"); f.Total != 2 {
		t.Errorf("repeated state: %d", f.Total)
	}
	if f := list("?q=frontdesk"); f.Total != 2 {
		t.Errorf("q: %d", f.Total)
	}
	if f := list("?include_retired=true"); f.Total != 14 {
		t.Errorf("include_retired: %d", f.Total)
	}
	if f := list("?sort=host"); f.Devices[0].Hostname != "DR-LAPTOP-03" {
		t.Errorf("sort=host first: %s", f.Devices[0].Hostname)
	}
	if f := list("?sort=host&dir=desc"); f.Devices[0].Hostname != "TECH-BENCH-07" {
		t.Errorf("sort=host desc first: %s", f.Devices[0].Hostname)
	}
	for _, key := range []string{"customer", "state", "last_success", "next_due", "agent", "os"} {
		if f := list("?sort=" + key); len(f.Devices) != 13 {
			t.Errorf("sort=%s: %d rows", key, len(f.Devices))
		}
	}
	p1 := list("?sort=host&limit=5")
	p2 := list("?sort=host&limit=5&cursor=" + p1.NextCursor)
	if len(p1.Devices) != 5 || !p1.Truncated || p1.NextCursor != "o5" || p2.Devices[0].ID == p1.Devices[0].ID || p1.Total != 13 {
		t.Fatalf("paging: %+v / %+v", p1.NextCursor, p2.Devices[0])
	}
	for _, q := range []string{"?state=HEALTHY", "?sort=colour", "?limit=0", "?cursor=x", "?include_retired=maybe", "?dir=sideways"} {
		res, b := e.req("GET", "/api/v1/customers/"+summit.ID+"/devices"+q, s, nil)
		if res.StatusCode != 400 || problemCode(t, res, b) != api.CodeInvalidRequest {
			t.Errorf("%s: %d", q, res.StatusCode)
		}
	}
	if code, _ := e.get("/api/v1/customers/01J9Y4K3A0B1C2D3E4F5G6H7ZZ/devices", s); code != 404 {
		t.Errorf("unknown customer: %d", code)
	}
}

func TestM2PatchDevice(t *testing.T) {
	e := newEnv(t)
	s := e.login("admin", pw)
	path := "/api/v1/devices/" + e.dev.ID
	before := e.audits()
	res, b := e.req("PATCH", path, s, `{"friendly_name":"Doctor's PC","notes":"Room 4","device_class":"laptop","update_ring":"pilot"}`)
	if res.StatusCode != 200 {
		t.Fatalf("patch: %d %s", res.StatusCode, b)
	}
	d := decode[DeviceSettingsResponse](t, b)
	if d.FriendlyName != "Doctor's PC" || d.Notes != "Room 4" || d.DeviceClass != "laptop" || d.UpdateRing != "pilot" || !d.Changed || d.ID != e.dev.ID {
		t.Fatalf("patched: %+v", d)
	}
	if rows := e.auditOf("device.update", e.dev.ID); len(rows) != 1 || e.audits() != before+1 || rows[0].Method != "api" || rows[0].CustomerID != e.dev.CustomerID {
		t.Fatalf("audit: %d rows (total %d, was %d)", len(rows), e.audits(), before)
	}
	// The change shows in the device detail and the customer's table.
	code, body := e.get(path, s)
	if code != 200 || decode[DeviceDetailJSON](t, body).DeviceClass != "laptop" {
		t.Fatalf("detail after patch: %d", code)
	}
	if res, b := e.req("PATCH", path, s, `{"friendly_name":null,"notes":null}`); res.StatusCode != 200 || decode[DeviceSettingsResponse](t, b).FriendlyName != "" {
		t.Fatalf("clear: %d %s", res.StatusCode, b)
	}
	if res, b := e.req("PATCH", path, s, `{"device_class":"laptop"}`); res.StatusCode != 200 || decode[DeviceSettingsResponse](t, b).Changed {
		t.Fatalf("no-op: %d %s", res.StatusCode, b)
	}
	if n := len(e.auditOf("device.update", e.dev.ID)); n != 2 {
		t.Fatalf("two real changes, %d audit rows", n)
	}
	for name, tc := range map[string]struct {
		body   string
		status int
		field  string
	}{
		"empty":        {`{}`, 400, "settings"},
		"bad class":    {`{"device_class":"tablet"}`, 400, "device_class"},
		"bad ring":     {`{"update_ring":"beta"}`, 400, "update_ring"},
		"class null":   {`{"device_class":null}`, 400, ""},
		"long name":    {`{"friendly_name":"` + strings.Repeat("x", 121) + `"}`, 400, "friendly_name"},
		"unknown key":  {`{"hostname":"other"}`, 400, ""},
		"repo path":    {`{"repo_path":"x"}`, 400, ""},
		"wrong type":   {`{"notes":5}`, 400, ""},
		"newline name": {`{"friendly_name":"a\nb"}`, 400, "friendly_name"},
	} {
		res, b := e.req("PATCH", path, s, tc.body)
		if res.StatusCode != tc.status {
			t.Errorf("%s: %d %s", name, res.StatusCode, b)
		} else if tc.field != "" && problemFields(t, b)[tc.field] == "" {
			t.Errorf("%s: field %q not listed: %s", name, tc.field, b)
		}
	}
	// A retired device cannot be edited.
	var retired store.Device
	l, _ := e.st.ListDevices(context.Background(), store.ScopeAll(), store.ListDevicesOptions{Search: "OLD-RECEPTION-PC", IncludeRetired: true})
	retired = l.Items[0]
	if res, b := e.req("PATCH", "/api/v1/devices/"+retired.ID, s, `{"notes":"x"}`); res.StatusCode != 409 || problemCode(t, res, b) != api.CodeConflict {
		t.Fatalf("retired: %d %s", res.StatusCode, b)
	}
	if res, _ := e.req("PATCH", "/api/v1/devices/01J9Y4K3A0B1C2D3E4F5G6H7ZZ", s, `{"notes":"x"}`); res.StatusCode != 404 {
		t.Fatalf("unknown device: %d", res.StatusCode)
	}
}

func TestM2DeviceRuns(t *testing.T) {
	e := newEnv(t)
	s := e.login("admin", pw)
	path := "/api/v1/devices/" + e.dev.ID + "/runs"
	list := func(q string) DeviceRunsResponse {
		code, b := e.get(path+q, s)
		if code != 200 {
			t.Fatalf("%s: %d %s", q, code, b)
		}
		return decode[DeviceRunsResponse](t, b)
	}
	all := list("?limit=200")
	if all.Total < 10 || len(all.Runs) != all.Total || all.ComputedAt.IsZero() {
		t.Fatalf("all: total=%d len=%d", all.Total, len(all.Runs))
	}
	for i := 1; i < len(all.Runs); i++ {
		if all.Runs[i].StartedAt.After(all.Runs[i-1].StartedAt) {
			t.Fatal("runs must be newest first")
		}
	}
	if bk := list("?kind=backup&limit=200"); bk.Total == 0 || bk.Total > all.Total {
		t.Errorf("kind=backup: %d of %d", bk.Total, all.Total)
	}
	if none := list("?kind=restore"); none.Total != 0 || none.Runs == nil || len(none.Runs) != 0 {
		t.Errorf("kind=restore: %+v", none)
	}
	if ok := list("?outcome=success&limit=200"); ok.Total == 0 || ok.Total > all.Total {
		t.Errorf("outcome=success: %d", ok.Total)
	}
	p1 := list("?limit=3")
	p2 := list("?limit=3&cursor=" + p1.NextCursor)
	if len(p1.Runs) != 3 || !p1.Truncated || p1.NextCursor != "o3" || p2.Runs[0].ID != all.Runs[3].ID {
		t.Fatalf("paging: %+v", p1.NextCursor)
	}
	since := all.Runs[2].StartedAt.UTC().Format("2006-01-02T15:04:05Z")
	if r := list("?since=" + since + "&limit=200"); r.Total != 3 {
		t.Errorf("since: %d", r.Total)
	}
	until := all.Runs[2].StartedAt.UTC().Format("2006-01-02T15:04:05Z")
	if r := list("?until=" + until + "&limit=200"); r.Total != all.Total-3 {
		t.Errorf("until (exclusive): %d of %d", r.Total, all.Total)
	}
	for _, q := range []string{"?kind=format", "?outcome=great", "?since=yesterday", "?limit=0", "?limit=201", "?cursor=x", "?since=2026-10-02T00:00:00Z&until=2026-10-01T00:00:00Z"} {
		res, b := e.req("GET", path+q, s, nil)
		if res.StatusCode != 400 || problemCode(t, res, b) != api.CodeInvalidRequest {
			t.Errorf("%s: %d", q, res.StatusCode)
		}
	}
	if code, _ := e.get("/api/v1/devices/01J9Y4K3A0B1C2D3E4F5G6H7ZZ/runs", s); code != 404 {
		t.Errorf("unknown device: %d", code)
	}
}

func TestM2ResponsesAreCaseAndPathFree(t *testing.T) {
	e := newEnv(t)
	s := e.login("admin", pw)
	summit := e.customerNamed("Summit Eye Care")
	for _, p := range []string{"/api/v1/customers", "/api/v1/customers/" + summit.ID, "/api/v1/customers/" + summit.ID + "/devices", "/api/v1/devices/" + e.dev.ID + "/runs?limit=200"} {
		code, b := e.get(p, s)
		if code != 200 {
			t.Fatalf("%s: %d", p, code)
		}
		var v any
		if err := json.Unmarshal(b, &v); err != nil {
			t.Fatal(err)
		}
		if err := api.AssertPathFree(v); err != nil {
			t.Errorf("%s: %v", p, err)
		}
		if res, _ := e.req("GET", p, s, nil); res.Header.Get("Cache-Control") != "no-store" || res.Header.Get("Content-Type") != api.ContentTypeJSON {
			t.Errorf("%s headers: %v", p, res.Header)
		}
	}
}
