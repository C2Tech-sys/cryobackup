package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/state"
)

var testActor = AuditActor{AdminID: "01J9Y4K3A0B1C2D3E4F5G6H7AA", Username: "tester", IP: "192.0.2.10", Method: "web"}

// auditRows returns the audit rows of one action about one object, newest first.
func auditRows(t *testing.T, s *Store, action, objectID string) []AuditEvent {
	t.Helper()
	l, err := s.ListAudit(context.Background(), ScopeAll(), AuditFilter{Action: action, ObjectID: objectID}, Page{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	return l.Items
}

func auditTotal(t *testing.T, s *Store) int {
	t.Helper()
	l, err := s.ListAudit(context.Background(), ScopeAll(), AuditFilter{}, Page{Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	return len(l.Items)
}

// refuseAudits makes every later audit insert fail, the way a full disk or a
// broken table would; the change that tried to write one must not survive.
func refuseAudits(t *testing.T, s *Store) {
	t.Helper()
	err := s.Write(context.Background(), func(tx *Tx) error {
		_, err := tx.Exec(`CREATE TRIGGER refuse_audits BEFORE INSERT ON audit_events BEGIN SELECT RAISE(ABORT, 'audit refused by the test'); END`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func fieldsOf(err error) map[string]string {
	out := map[string]string{}
	for _, f := range FieldErrors(err) {
		out[f.Field] = f.Message
	}
	return out
}

func TestCreateCustomerAuditedWritesOneAuditRow(t *testing.T) {
	w := seed(t)
	before := auditTotal(t, w.s)
	cap := 25
	until := t0.Add(14 * 24 * time.Hour)
	c, err := w.s.CreateCustomerAudited(w.ctx, ScopeAll(), CustomerInput{
		Name: "  Charlie Optical ", Expected: true, OnboardingUntil: &until, DisplayTZ: "America/Chicago", DeviceCap: &cap,
		Contacts: []Contact{{Name: "Pat Jones", Email: "pat@charlie.example", Role: "owner"}}, Notes: "Pilot site",
	}, testActor)
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "Charlie Optical" || c.Slug != "charlie-optical" || !c.Expected || c.State != "active" || c.KeyCustody != "escrowed" ||
		c.DisplayTZ != "America/Chicago" || c.DeviceCap == nil || *c.DeviceCap != 25 || c.OnboardingUntil == nil || !c.OnboardingUntil.Equal(until) {
		t.Fatalf("created customer: %+v", c)
	}
	if got := ParseContacts(c.Contacts); len(got) != 1 || got[0].Email != "pat@charlie.example" {
		t.Fatalf("contacts: %s", c.Contacts)
	}
	rows := auditRows(t, w.s, "customer.create", c.ID)
	if len(rows) != 1 || auditTotal(t, w.s) != before+1 {
		t.Fatalf("want exactly one customer.create audit row, got %d (total %d, was %d)", len(rows), auditTotal(t, w.s), before)
	}
	ev := rows[0]
	if ev.ActorType != "admin" || ev.ActorID != testActor.AdminID || ev.ActorLabel != "tester" || ev.Method != "web" || ev.CustomerID != c.ID ||
		ev.ObjectType != "customer" || ev.Result != "ok" || ev.SourceIP != "192.0.2.10" {
		t.Fatalf("audit row: %+v", ev)
	}
	if strings.Contains(string(ev.Details), "Charlie") || strings.Contains(string(ev.Details), "pat@") {
		t.Fatalf("the audit row must not carry names or contacts: %s", ev.Details)
	}
}

func TestCreateCustomerAuditedRefusals(t *testing.T) {
	w := seed(t)
	before := auditTotal(t, w.s)
	zero := 0
	for name, tc := range map[string]struct {
		in     CustomerInput
		fields []string
	}{
		"empty name":      {CustomerInput{Name: "  "}, []string{"name"}},
		"long name":       {CustomerInput{Name: strings.Repeat("n", 201)}, []string{"name"}},
		"bad slug":        {CustomerInput{Name: "Delta", Slug: "Not A Slug"}, []string{"slug"}},
		"bad zone":        {CustomerInput{Name: "Delta", DisplayTZ: "Mars/Olympus"}, []string{"display_tz"}},
		"zero cap":        {CustomerInput{Name: "Delta", DeviceCap: &zero}, []string{"device_cap"}},
		"nameless person": {CustomerInput{Name: "Delta", Contacts: []Contact{{Email: "x@y.example"}}}, []string{"contacts"}},
		"bad email":       {CustomerInput{Name: "Delta", Contacts: []Contact{{Name: "X", Email: "not-an-address"}}}, []string{"contacts"}},
		"too many people": {CustomerInput{Name: "Delta", Contacts: make([]Contact, MaxContacts+1)}, []string{"contacts"}},
		"bad custody":     {CustomerInput{Name: "Delta", KeyCustody: "shared"}, []string{"key_custody"}},
		"unknown target":  {CustomerInput{Name: "Delta", StorageTargetID: "01J9Y4K3A0B1C2D3E4F5G6H7ZZ"}, []string{"storage_target_id"}},
		"duplicate name":  {CustomerInput{Name: "alpha dental"}, []string{"name"}},
		"duplicate slug":  {CustomerInput{Name: "Another", Slug: w.custA.Slug}, []string{"slug"}},
		"unknown policy":  {CustomerInput{Name: "Delta", PolicyID: "01J9Y4K3A0B1C2D3E4F5G6H7ZZ"}, []string{"policy_id"}},
	} {
		_, err := w.s.CreateCustomerAudited(w.ctx, ScopeAll(), tc.in, testActor)
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", name, err)
			continue
		}
		got := fieldsOf(err)
		for _, f := range tc.fields {
			if got[f] == "" {
				t.Errorf("%s: field %q not reported: %v", name, f, got)
			}
		}
	}
	if auditTotal(t, w.s) != before {
		t.Fatal("a refused create must write no audit row")
	}
	if rows, _ := w.s.ListCustomerRows(w.ctx, ScopeAll(), CustomerListQuery{}); len(rows) != 2 {
		t.Fatalf("a refused create left %d customers", len(rows))
	}
}

func TestCreateCustomerNeedsScopeAll(t *testing.T) {
	w := seed(t)
	for name, sc := range map[string]Scope{"scoped": ScopeOf(w.custA.ID), "empty": ScopeOf(), "zero": {}} {
		if _, err := w.s.CreateCustomerAudited(w.ctx, sc, CustomerInput{Name: "Echo"}, testActor); !errors.Is(err, ErrScope) {
			t.Errorf("%s scope: want ErrScope, got %v", name, err)
		}
	}
	if len(auditRows(t, w.s, "customer.create", "")) != 0 {
		t.Fatal("a denied create wrote an audit row")
	}
}

func TestCreateCustomerRefusedWhenItCannotBeAudited(t *testing.T) {
	w := seed(t)
	refuseAudits(t, w.s)
	_, err := w.s.CreateCustomerAudited(w.ctx, ScopeAll(), CustomerInput{Name: "Foxtrot"}, testActor)
	if !errors.Is(err, ErrAudit) {
		t.Fatalf("want ErrAudit, got %v", err)
	}
	rows, _ := w.s.ListCustomerRows(w.ctx, ScopeAll(), CustomerListQuery{Search: "Foxtrot"})
	if len(rows) != 0 {
		t.Fatal("the customer exists although its audit row could not be written")
	}
}

func TestUpdateCustomerAudited(t *testing.T) {
	w := seed(t)
	yes := true
	until := t0.Add(7 * 24 * time.Hour)
	c, changed, err := w.s.UpdateCustomerAudited(w.ctx, ScopeAll(), w.custA.ID, CustomerPatch{Expected: &yes, OnboardingUntil: &until}, testActor)
	if err != nil || !changed || !c.Expected || c.OnboardingUntil == nil || !c.OnboardingUntil.Equal(until) {
		t.Fatalf("update: %v changed=%v %+v", err, changed, c)
	}
	rows := auditRows(t, w.s, "customer.update", w.custA.ID)
	if len(rows) != 1 {
		t.Fatalf("want one customer.update row, got %d", len(rows))
	}
	var d struct {
		Fields   []string        `json:"fields"`
		Expected map[string]bool `json:"expected"`
	}
	if err := json.Unmarshal(rows[0].Details, &d); err != nil || len(d.Fields) != 2 || d.Fields[0] != "expected" || d.Fields[1] != "onboarding_until" ||
		d.Expected["from"] || !d.Expected["to"] {
		t.Fatalf("audit details: %s (%v)", rows[0].Details, err)
	}
	if rows[0].CustomerID != w.custA.ID || rows[0].ActorLabel != "tester" {
		t.Fatalf("audit row: %+v", rows[0])
	}

	// The same patch again changes nothing: no second audit row.
	_, changed, err = w.s.UpdateCustomerAudited(w.ctx, ScopeAll(), w.custA.ID, CustomerPatch{Expected: &yes, OnboardingUntil: &until}, testActor)
	if err != nil || changed || len(auditRows(t, w.s, "customer.update", w.custA.ID)) != 1 {
		t.Fatalf("a no-op update: err=%v changed=%v rows=%d", err, changed, len(auditRows(t, w.s, "customer.update", w.custA.ID)))
	}

	// Clearing the window and the cap; contacts and notes.
	cap := 10
	if _, _, err := w.s.UpdateCustomerAudited(w.ctx, ScopeAll(), w.custA.ID, CustomerPatch{DeviceCap: &cap}, testActor); err != nil {
		t.Fatal(err)
	}
	notes := "Front desk only"
	contacts := []Contact{{Name: "Sam", Phone: "555-0100"}}
	c, changed, err = w.s.UpdateCustomerAudited(w.ctx, ScopeAll(), w.custA.ID, CustomerPatch{ClearOnboarding: true, ClearDeviceCap: true, Notes: &notes, Contacts: &contacts}, testActor)
	if err != nil || !changed || c.OnboardingUntil != nil || c.DeviceCap != nil || c.Notes != notes || len(ParseContacts(c.Contacts)) != 1 {
		t.Fatalf("clear: %v %+v", err, c)
	}
	if n := len(auditRows(t, w.s, "customer.update", w.custA.ID)); n != 3 {
		t.Fatalf("three real changes, %d audit rows", n)
	}
}

func TestUpdateCustomerAuditedValidationAndPolicy(t *testing.T) {
	w := seed(t)
	empty, badTZ := " ", "Nowhere/Land"
	_, _, err := w.s.UpdateCustomerAudited(w.ctx, ScopeAll(), w.custA.ID, CustomerPatch{Name: &empty, DisplayTZ: &badTZ}, testActor)
	got := fieldsOf(err)
	if !errors.Is(err, ErrInvalid) || got["name"] == "" || got["display_tz"] == "" {
		t.Fatalf("want name and display_tz field errors, got %v (%v)", got, err)
	}
	dup := "bravo legal"
	if _, _, err := w.s.UpdateCustomerAudited(w.ctx, ScopeAll(), w.custA.ID, CustomerPatch{Name: &dup}, testActor); fieldsOf(err)["name"] == "" {
		t.Fatalf("renaming onto another customer must be a name error, got %v", err)
	}
	// A policy that does not exist and another customer's policy read the same.
	other := must[PolicyRecord](t)(w.s.CreatePolicy(w.ctx, ScopeAll(), PolicyInput{Scope: "customer", CustomerID: w.custB.ID, Name: "B only", Sections: map[string]json.RawMessage{}}))
	ghost, theirs := "01J9Y4K3A0B1C2D3E4F5G6H7ZZ", other.ID
	_, _, e1 := w.s.UpdateCustomerAudited(w.ctx, ScopeAll(), w.custA.ID, CustomerPatch{PolicyID: &ghost}, testActor)
	_, _, e2 := w.s.UpdateCustomerAudited(w.ctx, ScopeAll(), w.custA.ID, CustomerPatch{PolicyID: &theirs}, testActor)
	if fieldsOf(e1)["policy_id"] == "" || fieldsOf(e1)["policy_id"] != fieldsOf(e2)["policy_id"] {
		t.Fatalf("an unknown policy and another customer's must read the same: %v / %v", e1, e2)
	}
	if len(auditRows(t, w.s, "customer.update", w.custA.ID)) != 0 {
		t.Fatal("refused updates wrote audit rows")
	}
	// A template is allowed, and "" returns to the default policy.
	tpl := must[PolicyRecord](t)(w.s.CreatePolicy(w.ctx, ScopeAll(), PolicyInput{Scope: "template", Name: "Laptop fleet", TemplateDeviceClass: "laptop", Sections: map[string]json.RawMessage{}}))
	// M2-W2A: the audited update no longer changes a customer's policy at all (only
	// AssignCustomerPolicyAudited does, behind an impact token), so the guard is
	// not handler-only; naming the current policy is not a change.
	_, _, err = w.s.UpdateCustomerAudited(w.ctx, ScopeAll(), w.custA.ID, CustomerPatch{PolicyID: &tpl.ID}, testActor)
	if fieldsOf(err)["policy_id"] == "" {
		t.Fatalf("a policy change through the generic update must be refused: %v", err)
	}
	if cur := must[Customer](t)(w.s.GetCustomer(w.ctx, ScopeAll(), w.custA.ID)); cur.PolicyID != "" {
		t.Fatal("the refused update assigned the policy")
	}
	none := ""
	notes := "same policy"
	c, changed, err := w.s.UpdateCustomerAudited(w.ctx, ScopeAll(), w.custA.ID, CustomerPatch{PolicyID: &none, Notes: &notes}, testActor)
	if err != nil || !changed || c.PolicyID != "" || c.Notes != notes {
		t.Fatalf("an unchanged policy_id beside another change: %v %+v", err, c)
	}
}

func TestUpdateCustomerAuditedScope(t *testing.T) {
	w := seed(t)
	yes := true
	// An operator scoped to A edits A, never B, and cannot tell B from a customer that does not exist.
	sc := ScopeOf(w.custA.ID)
	if _, changed, err := w.s.UpdateCustomerAudited(w.ctx, sc, w.custA.ID, CustomerPatch{Expected: &yes}, testActor); err != nil || !changed {
		t.Fatalf("scoped edit of own customer: %v %v", err, changed)
	}
	_, _, eB := w.s.UpdateCustomerAudited(w.ctx, sc, w.custB.ID, CustomerPatch{Expected: &yes}, testActor)
	_, _, eGhost := w.s.UpdateCustomerAudited(w.ctx, sc, "01J9Y4K3A0B1C2D3E4F5G6H7ZZ", CustomerPatch{Expected: &yes}, testActor)
	if !errors.Is(eB, ErrNotFound) || !errors.Is(eGhost, ErrNotFound) {
		t.Fatalf("B and a ghost must both be not found: %v / %v", eB, eGhost)
	}
	if got := must[Customer](t)(w.s.GetCustomer(w.ctx, ScopeAll(), w.custB.ID)); got.Expected {
		t.Fatal("B was changed through A's scope")
	}
	if len(auditRows(t, w.s, "customer.update", w.custB.ID)) != 0 {
		t.Fatal("a denied update wrote an audit row about B")
	}
	if _, _, err := w.s.UpdateCustomerAudited(w.ctx, Scope{}, w.custA.ID, CustomerPatch{Expected: &yes}, testActor); !errors.Is(err, ErrScope) {
		t.Fatalf("zero scope: %v", err)
	}
}

// A scoped caller must not learn which names and slugs other customers use:
// renaming is refused outright (ErrScope, no field message either way), while
// an All-scope caller gets the collision reported on the field.
func TestRenameCollisionIsNoExistenceOracleForScopedCallers(t *testing.T) {
	w := seed(t)
	sc := ScopeOf(w.custA.ID)
	taken := w.custB.Name // exists, out of scope
	free := "Totally Unused Name"
	for _, n := range []string{taken, free} {
		n := n
		_, _, err := w.s.UpdateCustomerAudited(w.ctx, sc, w.custA.ID, CustomerPatch{Name: &n}, testActor)
		if !errors.Is(err, ErrScope) || len(FieldErrors(err)) != 0 {
			t.Fatalf("scoped rename to %q: want a bare ErrScope, got %v", n, err)
		}
	}
	slug := w.custB.Slug
	if _, _, err := w.s.UpdateCustomerAudited(w.ctx, sc, w.custA.ID, CustomerPatch{Slug: &slug}, testActor); !errors.Is(err, ErrScope) {
		t.Fatalf("scoped slug change: %v", err)
	}
	if got := must[Customer](t)(w.s.GetCustomer(w.ctx, ScopeAll(), w.custA.ID)); got.Name != w.custA.Name || got.Slug != w.custA.Slug {
		t.Fatal("a refused rename changed the customer")
	}
	// Re-sending its own name is not a rename; other fields still edit.
	own, notes := w.custA.Name, "ok"
	if _, _, err := w.s.UpdateCustomerAudited(w.ctx, sc, w.custA.ID, CustomerPatch{Name: &own, Notes: &notes}, testActor); err != nil {
		t.Fatalf("scoped edit of other fields: %v", err)
	}
	// All scope: the collision with another customer is reported on the field (in-scope path).
	_, _, err := w.s.UpdateCustomerAudited(w.ctx, ScopeAll(), w.custA.ID, CustomerPatch{Name: &taken}, testActor)
	if fieldsOf(err)["name"] == "" {
		t.Fatalf("All-scope collision: %v", err)
	}
	// Create is All-only too (covered in TestCreateCustomerNeedsScopeAll): a scoped caller never reaches the name check.
	if _, err := w.s.CreateCustomerAudited(w.ctx, sc, CustomerInput{Name: taken}, testActor); !errors.Is(err, ErrScope) || len(FieldErrors(err)) != 0 {
		t.Fatalf("scoped create with a taken name: %v", err)
	}
}

func TestUpdateCustomerAuditedRefusedWhenItCannotBeAudited(t *testing.T) {
	w := seed(t)
	refuseAudits(t, w.s)
	yes := true
	if _, _, err := w.s.UpdateCustomerAudited(w.ctx, ScopeAll(), w.custA.ID, CustomerPatch{Expected: &yes}, testActor); !errors.Is(err, ErrAudit) {
		t.Fatalf("want ErrAudit, got %v", err)
	}
	if got := must[Customer](t)(w.s.GetCustomer(w.ctx, ScopeAll(), w.custA.ID)); got.Expected {
		t.Fatal("the change survived although its audit row could not be written")
	}
}

func TestUpdateCustomerAuditedRefusesAnArchivedCustomer(t *testing.T) {
	w := seed(t)
	arch := must[Customer](t)(w.s.CreateCustomer(w.ctx, ScopeAll(), Customer{Name: "Gone Co", State: "archived"}))
	yes := true
	if _, _, err := w.s.UpdateCustomerAudited(w.ctx, ScopeAll(), arch.ID, CustomerPatch{Expected: &yes}, testActor); !errors.Is(err, ErrConflict) {
		t.Fatalf("want ErrConflict, got %v", err)
	}
}

func TestListCustomerRowsFiltersScopeAndRollup(t *testing.T) {
	w := seed(t)
	arch := must[Customer](t)(w.s.CreateCustomer(w.ctx, ScopeAll(), Customer{Name: "Gone Co", State: "archived"}))
	exp := must[Customer](t)(w.s.CreateCustomer(w.ctx, ScopeAll(), Customer{Name: "Expected Co", Expected: true}))
	_ = arch
	w.heartbeat(w.devA.ID)
	res, err := w.s.AgentIngestRuns(w.ctx, w.devA.ID, []RunInput{runInput(runID1, t0.Add(-2*time.Hour))}, t0)
	if err != nil || res[0].Status != IngestCreated {
		t.Fatalf("ingest: %v %+v", err, res)
	}

	rows := must[[]CustomerRow](t)(w.s.ListCustomerRows(w.ctx, ScopeAll(), CustomerListQuery{}))
	if len(rows) != 3 { // archived hidden by default
		t.Fatalf("default list has %d rows", len(rows))
	}
	byName := map[string]CustomerRow{}
	for _, r := range rows {
		byName[r.Name] = r
	}
	a := byName["Alpha Dental"]
	if a.Devices != 1 || a.Retired != 0 || a.LastSuccessAt == nil || !a.LastSuccessAt.Equal(t0.Add(-2*time.Hour)) {
		t.Fatalf("Alpha rollup: %+v", a)
	}
	if b := byName["Bravo Legal"]; b.Devices != 1 || b.LastSuccessAt != nil {
		t.Fatalf("Bravo rollup: %+v", b)
	}
	if e := byName["Expected Co"]; e.Devices != 0 || !e.Expected || e.ID != exp.ID {
		t.Fatalf("Expected Co: %+v", e)
	}
	if got := must[[]CustomerRow](t)(w.s.ListCustomerRows(w.ctx, ScopeAll(), CustomerListQuery{IncludeArchived: true})); len(got) != 4 {
		t.Fatalf("with archived: %d", len(got))
	}
	if got := must[[]CustomerRow](t)(w.s.ListCustomerRows(w.ctx, ScopeAll(), CustomerListQuery{Lifecycle: "archived"})); len(got) != 1 || got[0].Name != "Gone Co" {
		t.Fatalf("archived only: %+v", got)
	}
	yes, no := true, false
	if got := must[[]CustomerRow](t)(w.s.ListCustomerRows(w.ctx, ScopeAll(), CustomerListQuery{Expected: &yes})); len(got) != 1 || got[0].Name != "Expected Co" {
		t.Fatalf("expected only: %+v", got)
	}
	if got := must[[]CustomerRow](t)(w.s.ListCustomerRows(w.ctx, ScopeAll(), CustomerListQuery{Expected: &no})); len(got) != 2 {
		t.Fatalf("not expected: %d", len(got))
	}
	if got := must[[]CustomerRow](t)(w.s.ListCustomerRows(w.ctx, ScopeAll(), CustomerListQuery{Search: "LEGAL"})); len(got) != 1 || got[0].Name != "Bravo Legal" {
		t.Fatalf("search: %+v", got)
	}
	if got := must[[]CustomerRow](t)(w.s.ListCustomerRows(w.ctx, ScopeAll(), CustomerListQuery{Search: "100%"})); len(got) != 0 {
		t.Fatalf("a LIKE wildcard in the search must be literal: %+v", got)
	}
	if _, err := w.s.ListCustomerRows(w.ctx, ScopeAll(), CustomerListQuery{Lifecycle: "bogus"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad lifecycle: %v", err)
	}
	// Scope: A's operator sees A only, an empty scope nothing, the zero scope is refused.
	if got := must[[]CustomerRow](t)(w.s.ListCustomerRows(w.ctx, ScopeOf(w.custA.ID), CustomerListQuery{})); len(got) != 1 || got[0].ID != w.custA.ID {
		t.Fatalf("scoped list: %+v", got)
	}
	if got := must[[]CustomerRow](t)(w.s.ListCustomerRows(w.ctx, ScopeOf(), CustomerListQuery{})); len(got) != 0 {
		t.Fatalf("empty scope: %+v", got)
	}
	if _, err := w.s.ListCustomerRows(w.ctx, Scope{}, CustomerListQuery{}); !errors.Is(err, ErrScope) {
		t.Fatalf("zero scope: %v", err)
	}
	if _, err := w.s.GetCustomerRow(w.ctx, ScopeOf(w.custA.ID), w.custB.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetCustomerRow outside scope: %v", err)
	}
	if r, err := w.s.GetCustomerRow(w.ctx, ScopeOf(w.custA.ID), w.custA.ID); err != nil || r.Devices != 1 {
		t.Fatalf("GetCustomerRow: %v %+v", err, r)
	}
}

func TestPolicyChoicesNeverIncludeAnotherCustomersPolicy(t *testing.T) {
	w := seed(t)
	tpl := must[PolicyRecord](t)(w.s.CreatePolicy(w.ctx, ScopeAll(), PolicyInput{Scope: "template", Name: "Desktops", TemplateDeviceClass: "desktop", Sections: map[string]json.RawMessage{}}))
	pa := must[PolicyRecord](t)(w.s.CreatePolicy(w.ctx, ScopeAll(), PolicyInput{Scope: "customer", CustomerID: w.custA.ID, Name: "A own", Sections: map[string]json.RawMessage{}}))
	must[PolicyRecord](t)(w.s.CreatePolicy(w.ctx, ScopeAll(), PolicyInput{Scope: "customer", CustomerID: w.custB.ID, Name: "B own", Sections: map[string]json.RawMessage{}}))
	got := must[[]PolicyChoice](t)(w.s.PolicyChoicesFor(w.ctx, ScopeAll(), w.custA.ID))
	if len(got) != 2 || got[0].ID != tpl.ID || got[1].ID != pa.ID {
		t.Fatalf("choices for A: %+v", got)
	}
	got = must[[]PolicyChoice](t)(w.s.PolicyChoicesFor(w.ctx, ScopeAll(), ""))
	if len(got) != 1 || got[0].ID != tpl.ID {
		t.Fatalf("choices for a new customer: %+v", got)
	}
}

func TestBuildCustomerTable(t *testing.T) {
	mk := func(name string, devices int, expected bool) CustomerRow {
		id := Slugify(name)
		return CustomerRow{Customer: Customer{ID: id, Name: name, Expected: expected}, Devices: devices}
	}
	rows := []CustomerRow{mk("Zulu", 3, false), mk("Alpha", 0, true), mk("Mike", 9, false), mk("Quebec", 1, false), mk("Gone", 0, false)}
	view := []state.CustomerState{
		{ID: "zulu", Name: "Zulu", Status: state.CustomerOK, SortKey: 4},
		{ID: "alpha", Name: "Alpha", Status: state.CustomerMissing, SortKey: 0},
		{ID: "mike", Name: "Mike", Status: state.CustomerAlarm, SortKey: 2},
		{ID: "quebec", Name: "Quebec", Status: state.CustomerOK, SortKey: 4},
	}
	names := func(tb CustomerTable) string {
		var n []string
		for _, r := range tb.Rows {
			n = append(n, r.Name)
		}
		return strings.Join(n, ",")
	}
	tb, err := BuildCustomerTable(rows, view, CustomerTableQuery{})
	if err != nil || names(tb) != "Alpha,Mike,Quebec,Zulu,Gone" || tb.Total != 5 {
		t.Fatalf("default order is the engine's (missing, alarm, ok, then those it does not list): %v %s", err, names(tb))
	}
	if tb, _ = BuildCustomerTable(rows, view, CustomerTableQuery{Health: "ok"}); names(tb) != "Quebec,Zulu" || tb.Total != 5 || tb.Page.Total != 2 {
		t.Fatalf("health filter: %s total=%d page.total=%d", names(tb), tb.Total, tb.Page.Total)
	}
	if tb, _ = BuildCustomerTable(rows, view, CustomerTableQuery{Sort: "devices", Desc: true}); names(tb) != "Mike,Zulu,Quebec,Gone,Alpha" {
		t.Fatalf("devices desc: %s", names(tb))
	}
	if tb, _ = BuildCustomerTable(rows, view, CustomerTableQuery{Sort: "name"}); names(tb) != "Alpha,Gone,Mike,Quebec,Zulu" {
		t.Fatalf("name: %s", names(tb))
	}
	if tb, _ = BuildCustomerTable(rows, view, CustomerTableQuery{Sort: "expected"}); tb.Rows[0].Name != "Alpha" {
		t.Fatalf("expected first: %s", names(tb))
	}
	if tb, _ = BuildCustomerTable(rows, view, CustomerTableQuery{Size: 25, Page: 9}); !tb.Page.Clamped || tb.Page.Page != 1 || len(tb.Rows) != 5 {
		t.Fatalf("page past the end: %+v", tb.Page)
	}
	for _, q := range []CustomerTableQuery{{Health: "bogus"}, {Sort: "bogus"}} {
		if _, err := BuildCustomerTable(rows, view, q); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v: want ErrInvalid, got %v", q, err)
		}
	}
	if tb, err = BuildCustomerTable(nil, nil, CustomerTableQuery{}); err != nil || tb.Rows == nil || len(tb.Rows) != 0 || tb.Page.Pages != 1 || tb.Page.From != 0 {
		t.Fatalf("empty: %v %+v", err, tb)
	}
}

func TestPaginate(t *testing.T) {
	for _, tc := range []struct {
		total, page, size int
		off, lim          int
		info              PageInfo
	}{
		{0, 1, 50, 0, 50, PageInfo{Total: 0, Page: 1, Pages: 1, Size: 50}},
		{212, 1, 50, 0, 50, PageInfo{Total: 212, Page: 1, Pages: 5, Size: 50, From: 1, To: 50}},
		{212, 5, 50, 200, 50, PageInfo{Total: 212, Page: 5, Pages: 5, Size: 50, From: 201, To: 212}},
		{212, 99, 50, 200, 50, PageInfo{Total: 212, Page: 5, Pages: 5, Size: 50, From: 201, To: 212, Clamped: true}},
		{212, 0, 7, 0, 50, PageInfo{Total: 212, Page: 1, Pages: 5, Size: 50, From: 1, To: 50}},
		{30, -3, 25, 0, 25, PageInfo{Total: 30, Page: 1, Pages: 2, Size: 25, From: 1, To: 25}},
	} {
		off, lim, info := Paginate(tc.total, tc.page, tc.size)
		if off != tc.off || lim != tc.lim || info != tc.info {
			t.Errorf("Paginate(%d,%d,%d) = %d,%d,%+v; want %d,%d,%+v", tc.total, tc.page, tc.size, off, lim, info, tc.off, tc.lim, tc.info)
		}
	}
	if OffsetCursor(0) != "" || OffsetCursor(50) != "o50" {
		t.Fatal("OffsetCursor")
	}
}
