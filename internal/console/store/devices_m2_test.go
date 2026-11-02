package store

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/policy"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

func hostnames(rows []DeviceRow) string {
	var h []string
	for _, r := range rows {
		h = append(h, r.Hostname)
	}
	return strings.Join(h, ",")
}

func TestListDeviceRowsScopeAndFilters(t *testing.T) {
	w := seed(t)
	w.enroll("tok-a", "A-LAPTOP-2")
	all := must[[]DeviceRow](t)(w.s.ListDeviceRows(w.ctx, ScopeAll(), DeviceFilter{}))
	if hostnames(all) != "A-DESK-1,A-LAPTOP-2,B-DESK-1" {
		t.Fatalf("all: %s", hostnames(all))
	}
	if all[0].CustomerName != "Alpha Dental" || all[2].CustomerName != "Bravo Legal" {
		t.Fatalf("customer names: %+v", all)
	}
	// A's operator never sees B, however the filter asks.
	a := ScopeOf(w.custA.ID)
	if got := must[[]DeviceRow](t)(w.s.ListDeviceRows(w.ctx, a, DeviceFilter{})); hostnames(got) != "A-DESK-1,A-LAPTOP-2" {
		t.Fatalf("scoped: %s", hostnames(got))
	}
	if got := must[[]DeviceRow](t)(w.s.ListDeviceRows(w.ctx, a, DeviceFilter{CustomerID: w.custB.ID})); len(got) != 0 {
		t.Fatalf("filtering by B inside A's scope returned %s", hostnames(got))
	}
	if got := must[[]DeviceRow](t)(w.s.ListDeviceRows(w.ctx, a, DeviceFilter{Search: "bravo"})); len(got) != 0 {
		t.Fatalf("searching B's customer name inside A's scope returned %s", hostnames(got))
	}
	if got := must[[]DeviceRow](t)(w.s.ListDeviceRows(w.ctx, ScopeOf(), DeviceFilter{})); len(got) != 0 {
		t.Fatalf("empty scope: %s", hostnames(got))
	}
	if _, err := w.s.ListDeviceRows(w.ctx, Scope{}, DeviceFilter{}); !errors.Is(err, ErrScope) {
		t.Fatalf("zero scope: %v", err)
	}
	// Filters.
	if got := must[[]DeviceRow](t)(w.s.ListDeviceRows(w.ctx, ScopeAll(), DeviceFilter{CustomerID: w.custB.ID})); hostnames(got) != "B-DESK-1" {
		t.Fatalf("customer filter: %s", hostnames(got))
	}
	if got := must[[]DeviceRow](t)(w.s.ListDeviceRows(w.ctx, ScopeAll(), DeviceFilter{Search: "laptop"})); hostnames(got) != "A-LAPTOP-2" {
		t.Fatalf("search: %s", hostnames(got))
	}
	if got := must[[]DeviceRow](t)(w.s.ListDeviceRows(w.ctx, ScopeAll(), DeviceFilter{Search: "alpha"})); len(got) != 2 {
		t.Fatalf("search by customer name: %s", hostnames(got))
	}
	if got := must[[]DeviceRow](t)(w.s.ListDeviceRows(w.ctx, ScopeAll(), DeviceFilter{Search: "_"})); len(got) != 0 {
		t.Fatalf("a LIKE wildcard in the search must be literal: %s", hostnames(got))
	}
	pilot := "pilot"
	if _, _, err := w.s.UpdateDeviceSettings(w.ctx, ScopeAll(), w.devA.ID, DeviceSettingsPatch{UpdateRing: &pilot}, testActor); err != nil {
		t.Fatal(err)
	}
	if got := must[[]DeviceRow](t)(w.s.ListDeviceRows(w.ctx, ScopeAll(), DeviceFilter{UpdateRing: "pilot"})); hostnames(got) != "A-DESK-1" {
		t.Fatalf("ring filter: %s", hostnames(got))
	}
	// Retired devices are hidden unless asked for.
	must[Device](t)(w.s.UpdateDevice(w.ctx, ScopeAll(), w.devB.ID, DeviceUpdate{Retire: &RetireSpec{Reason: "decommissioned"}}))
	if got := must[[]DeviceRow](t)(w.s.ListDeviceRows(w.ctx, ScopeAll(), DeviceFilter{})); strings.Contains(hostnames(got), "B-DESK-1") {
		t.Fatalf("retired shown by default: %s", hostnames(got))
	}
	if got := must[[]DeviceRow](t)(w.s.ListDeviceRows(w.ctx, ScopeAll(), DeviceFilter{IncludeRetired: true})); len(got) != 3 {
		t.Fatalf("include retired: %s", hostnames(got))
	}
}

func TestUpdateDeviceSettingsAudited(t *testing.T) {
	w := seed(t)
	before := auditTotal(t, w.s)
	name, notes, class, ring := "Front desk", "Behind the counter", "laptop", "pilot"
	d, changed, err := w.s.UpdateDeviceSettings(w.ctx, ScopeAll(), w.devA.ID, DeviceSettingsPatch{FriendlyName: &name, Notes: &notes, DeviceClass: &class, UpdateRing: &ring}, testActor)
	if err != nil || !changed || d.FriendlyName != name || d.Notes != notes || d.DeviceClass != class || d.UpdateRing != ring {
		t.Fatalf("update: %v changed=%v %+v", err, changed, d)
	}
	rows := auditRows(t, w.s, "device.update", w.devA.ID)
	if len(rows) != 1 || auditTotal(t, w.s) != before+1 {
		t.Fatalf("want exactly one device.update row, got %d (total %d, was %d)", len(rows), auditTotal(t, w.s), before)
	}
	ev := rows[0]
	if ev.ObjectType != "device" || ev.CustomerID != w.custA.ID || ev.ActorID != testActor.AdminID || ev.Result != "ok" {
		t.Fatalf("audit row: %+v", ev)
	}
	var det struct {
		Fields      []string          `json:"fields"`
		DeviceClass map[string]string `json:"device_class"`
		UpdateRing  map[string]string `json:"update_ring"`
	}
	if err := json.Unmarshal(ev.Details, &det); err != nil || len(det.Fields) != 4 || det.DeviceClass["from"] != "desktop" || det.DeviceClass["to"] != "laptop" || det.UpdateRing["to"] != "pilot" {
		t.Fatalf("details: %s (%v)", ev.Details, err)
	}
	if strings.Contains(string(ev.Details), "Front desk") || strings.Contains(string(ev.Details), "counter") {
		t.Fatalf("free text must not be copied into the audit row: %s", ev.Details)
	}
	// The same values again change nothing and write nothing.
	if _, changed, err := w.s.UpdateDeviceSettings(w.ctx, ScopeAll(), w.devA.ID, DeviceSettingsPatch{FriendlyName: &name, DeviceClass: &class}, testActor); err != nil || changed {
		t.Fatalf("no-op: %v changed=%v", err, changed)
	}
	if len(auditRows(t, w.s, "device.update", w.devA.ID)) != 1 {
		t.Fatal("a no-op update wrote an audit row")
	}
	// An empty name and empty notes clear them.
	empty := ""
	d, changed, err = w.s.UpdateDeviceSettings(w.ctx, ScopeAll(), w.devA.ID, DeviceSettingsPatch{FriendlyName: &empty, Notes: &empty}, testActor)
	if err != nil || !changed || d.FriendlyName != "" || d.Notes != "" {
		t.Fatalf("clear: %v %+v", err, d)
	}
	if len(auditRows(t, w.s, "device.update", w.devA.ID)) != 2 {
		t.Fatal("clearing is a change and needs its own row")
	}
}

func TestUpdateDeviceSettingsRefusals(t *testing.T) {
	w := seed(t)
	before := auditTotal(t, w.s)
	long := strings.Repeat("x", 121)
	multi := "two\nlines"
	bell := "a\x07b"
	bad, ring := "tablet", "beta"
	for name, tc := range map[string]struct {
		p     DeviceSettingsPatch
		field string
	}{
		"empty patch":    {DeviceSettingsPatch{}, "settings"},
		"long name":      {DeviceSettingsPatch{FriendlyName: &long}, "friendly_name"},
		"multi-line":     {DeviceSettingsPatch{FriendlyName: &multi}, "friendly_name"},
		"control chars":  {DeviceSettingsPatch{Notes: &bell}, "notes"},
		"unknown class":  {DeviceSettingsPatch{DeviceClass: &bad}, "device_class"},
		"unknown ring":   {DeviceSettingsPatch{UpdateRing: &ring}, "update_ring"},
		"huge notes":     {DeviceSettingsPatch{Notes: ptr(strings.Repeat("n", 2001))}, "notes"},
		"class and ring": {DeviceSettingsPatch{DeviceClass: &bad, UpdateRing: &ring}, "update_ring"},
	} {
		_, _, err := w.s.UpdateDeviceSettings(w.ctx, ScopeAll(), w.devA.ID, tc.p, testActor)
		if !errors.Is(err, ErrInvalid) || fieldsOf(err)[tc.field] == "" {
			t.Errorf("%s: want a %q field error, got %v", name, tc.field, err)
		}
	}
	if auditTotal(t, w.s) != before {
		t.Fatal("refused edits wrote audit rows")
	}
	// A retired device is not editable.
	must[Device](t)(w.s.UpdateDevice(w.ctx, ScopeAll(), w.devB.ID, DeviceUpdate{Retire: &RetireSpec{Reason: "decommissioned"}}))
	n := "x"
	if _, _, err := w.s.UpdateDeviceSettings(w.ctx, ScopeAll(), w.devB.ID, DeviceSettingsPatch{FriendlyName: &n}, testActor); !errors.Is(err, ErrConflict) {
		t.Fatalf("retired: want ErrConflict, got %v", err)
	}
}

func ptr[T any](v T) *T { return &v }

func TestUpdateDeviceSettingsScope(t *testing.T) {
	w := seed(t)
	n := "Renamed"
	a := ScopeOf(w.custA.ID)
	if _, changed, err := w.s.UpdateDeviceSettings(w.ctx, a, w.devA.ID, DeviceSettingsPatch{FriendlyName: &n}, testActor); err != nil || !changed {
		t.Fatalf("own device: %v %v", err, changed)
	}
	_, _, eB := w.s.UpdateDeviceSettings(w.ctx, a, w.devB.ID, DeviceSettingsPatch{FriendlyName: &n}, testActor)
	_, _, eGhost := w.s.UpdateDeviceSettings(w.ctx, a, "01J9Y4K3A0B1C2D3E4F5G6H7ZZ", DeviceSettingsPatch{FriendlyName: &n}, testActor)
	if !errors.Is(eB, ErrNotFound) || !errors.Is(eGhost, ErrNotFound) {
		t.Fatalf("B's device and a ghost must both be not found: %v / %v", eB, eGhost)
	}
	if got := must[Device](t)(w.s.GetDevice(w.ctx, ScopeAll(), w.devB.ID)); got.FriendlyName != "" {
		t.Fatal("B's device was renamed through A's scope")
	}
	if len(auditRows(t, w.s, "device.update", w.devB.ID)) != 0 {
		t.Fatal("a denied edit wrote an audit row")
	}
	if _, _, err := w.s.UpdateDeviceSettings(w.ctx, Scope{}, w.devA.ID, DeviceSettingsPatch{FriendlyName: &n}, testActor); !errors.Is(err, ErrScope) {
		t.Fatalf("zero scope: %v", err)
	}
}

func TestUpdateDeviceSettingsRefusedWhenItCannotBeAudited(t *testing.T) {
	w := seed(t)
	refuseAudits(t, w.s)
	n := "Renamed"
	if _, _, err := w.s.UpdateDeviceSettings(w.ctx, ScopeAll(), w.devA.ID, DeviceSettingsPatch{FriendlyName: &n}, testActor); !errors.Is(err, ErrAudit) {
		t.Fatalf("want ErrAudit, got %v", err)
	}
	if got := must[Device](t)(w.s.GetDevice(w.ctx, ScopeAll(), w.devA.ID)); got.FriendlyName != "" {
		t.Fatal("the rename survived although its audit row could not be written")
	}
}

func TestListDeviceAuditIsScopedToTheDevice(t *testing.T) {
	w := seed(t)
	n := "Renamed"
	must[Device](t)(func() (Device, error) {
		d, _, err := w.s.UpdateDeviceSettings(w.ctx, ScopeAll(), w.devA.ID, DeviceSettingsPatch{FriendlyName: &n}, testActor)
		return d, err
	}())
	m := "Other"
	if _, _, err := w.s.UpdateDeviceSettings(w.ctx, ScopeAll(), w.devB.ID, DeviceSettingsPatch{FriendlyName: &m}, testActor); err != nil {
		t.Fatal(err)
	}
	l := must[Listing[AuditEvent]](t)(w.s.ListDeviceAudit(w.ctx, ScopeAll(), w.devA.ID, Page{Limit: 50}))
	var actions []string
	for _, e := range l.Items {
		if e.ObjectID != w.devA.ID {
			t.Fatalf("another object's row in A's audit: %+v", e)
		}
		actions = append(actions, e.Action)
	}
	if !contains(actions, "device.update") || !contains(actions, "device.enroll") || len(actions) != 2 {
		t.Fatalf("actions: %v", actions)
	}
	// A's operator may read A's rows, not B's, and B's device id is simply not found.
	if got := must[Listing[AuditEvent]](t)(w.s.ListDeviceAudit(w.ctx, ScopeOf(w.custA.ID), w.devA.ID, Page{})); len(got.Items) == 0 {
		t.Fatal("scoped operator sees no rows of its own device")
	}
	if _, err := w.s.ListDeviceAudit(w.ctx, ScopeOf(w.custA.ID), w.devB.ID, Page{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("B's device audit through A's scope: %v", err)
	}
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

func TestDeviceNextDue(t *testing.T) {
	w := seed(t)
	ids := []string{w.devA.ID, w.devB.ID}
	// A device that never succeeded is due now (unless a window holds it back).
	got := must[map[string]NextDue](t)(w.s.DeviceNextDue(w.ctx, ScopeAll(), ids, nil, t0, time.UTC))
	pol, _, _, err := w.s.EffectivePolicyFor(w.ctx, ScopeAll(), w.devA.ID)
	if err != nil {
		t.Fatal(err)
	}
	dec := policy.Due(policy.DueInput{Now: t0, Schedule: pol.Schedule, DeviceID: w.devA.ID})
	if want := map[bool]string{true: DueNow, false: DueScheduled}[dec.Due]; got[w.devA.ID].Kind != want {
		t.Fatalf("never-succeeded device: kind %q, the policy says due=%v", got[w.devA.ID].Kind, dec.Due)
	}
	// With a recent success the next start lies in the future.
	last := map[string]time.Time{w.devA.ID: t0.Add(-10 * time.Minute)}
	got = must[map[string]NextDue](t)(w.s.DeviceNextDue(w.ctx, ScopeAll(), ids, last, t0, time.UTC))
	if nd := got[w.devA.ID]; nd.Kind != DueScheduled || !nd.At.After(t0) {
		t.Fatalf("recent success: %+v", nd)
	}
	// Paused and retired devices have no next due.
	must[Device](t)(w.s.UpdateDevice(w.ctx, ScopeAll(), w.devA.ID, DeviceUpdate{Pause: &PauseSpec{Reason: "site closed"}}))
	must[Device](t)(w.s.UpdateDevice(w.ctx, ScopeAll(), w.devB.ID, DeviceUpdate{Retire: &RetireSpec{Reason: "decommissioned"}}))
	got = must[map[string]NextDue](t)(w.s.DeviceNextDue(w.ctx, ScopeAll(), ids, last, t0, time.UTC))
	if got[w.devA.ID].Kind != DuePaused || got[w.devB.ID].Kind != DueRetired {
		t.Fatalf("paused/retired: %+v", got)
	}
	// Scope: a device outside it has no entry at all.
	got = must[map[string]NextDue](t)(w.s.DeviceNextDue(w.ctx, ScopeOf(w.custA.ID), ids, last, t0, nil))
	if _, leaked := got[w.devB.ID]; leaked || len(got) != 1 {
		t.Fatalf("scoped next due: %+v", got)
	}
}

func fakeRow(host, customer string) DeviceRow {
	return DeviceRow{Device: Device{ID: "id-" + host, Hostname: host, CustomerID: customer, AgentVersion: "1.2.0", OSVersion: "Windows 11 Pro"}, CustomerName: customer}
}

func fakeState(host string, st state.State, sev state.Severity, lastSuccess time.Time) state.DeviceState {
	return state.DeviceState{ID: "id-" + host, Hostname: host, Primary: st, Alarm: sev, Detail: state.Detail{LastSuccess: lastSuccess}}
}

func tableHosts(tb DeviceTable) string {
	var h []string
	for _, r := range tb.Rows {
		h = append(h, r.Hostname)
	}
	return strings.Join(h, ",")
}

func TestBuildDeviceTable(t *testing.T) {
	rows := []DeviceRow{fakeRow("ALPHA", "Zed Co"), fakeRow("BRAVO", "Acme"), fakeRow("CHARLIE", "Acme"), fakeRow("DELTA", "Mid"), fakeRow("ECHO", "Mid")}
	rows[1].AgentVersion, rows[2].AgentVersion = "1.10.0", "1.9.0"
	rows[3].OSVersion = "Windows 10 Pro"
	view := []state.DeviceState{
		fakeState("ALPHA", state.StateOK, state.SeverityNone, t0.Add(-1*time.Hour)),
		fakeState("BRAVO", state.StateFailed, state.SeverityCritical, t0.Add(-5*time.Hour)),
		fakeState("CHARLIE", state.StateStale, state.SeverityCritical, t0.Add(-100*time.Hour)),
		fakeState("DELTA", state.StatePartial, state.SeverityAlarm, t0.Add(-2*time.Hour)),
		// ECHO has no verdict at all.
	}
	due := map[string]NextDue{
		"id-ALPHA":   {Kind: DueScheduled, At: t0.Add(3 * time.Hour)},
		"id-BRAVO":   {Kind: DueNow, At: t0},
		"id-CHARLIE": {Kind: DuePaused},
		"id-DELTA":   {Kind: DueScheduled, At: t0.Add(1 * time.Hour)},
	}
	build := func(q DeviceTableQuery) DeviceTable {
		t.Helper()
		tb, err := BuildDeviceTable(rows, view, due, q)
		if err != nil {
			t.Fatal(err)
		}
		return tb
	}
	// Default: loudest first; the device the engine has no verdict for counts as
	// critical and UNKNOWN, never as healthy.
	tb := build(DeviceTableQuery{})
	if tableHosts(tb) != "ECHO,BRAVO,CHARLIE,DELTA,ALPHA" {
		t.Fatalf("default order: %s", tableHosts(tb))
	}
	if echo := tb.Rows[0]; echo.InView || echo.Primary() != state.StateUnknown {
		t.Fatalf("a device with no verdict must read UNKNOWN: %+v", echo)
	}
	for sortKey, want := range map[string]string{
		"host": "ALPHA,BRAVO,CHARLIE,DELTA,ECHO", "customer": "BRAVO,CHARLIE,DELTA,ECHO,ALPHA", "last_success": "ECHO,CHARLIE,BRAVO,DELTA,ALPHA",
		"next_due": "BRAVO,DELTA,ALPHA,CHARLIE,ECHO", "agent": "ALPHA,DELTA,ECHO,CHARLIE,BRAVO", "os": "DELTA,ALPHA,BRAVO,CHARLIE,ECHO",
	} {
		if got := tableHosts(build(DeviceTableQuery{Sort: sortKey})); got != want {
			t.Errorf("sort %s: %s, want %s", sortKey, got, want)
		}
	}
	if got := tableHosts(build(DeviceTableQuery{Sort: "host", Desc: true})); got != "ECHO,DELTA,CHARLIE,BRAVO,ALPHA" {
		t.Errorf("host desc: %s", got)
	}
	// State filter, including UNKNOWN for the device with no verdict.
	if tb := build(DeviceTableQuery{States: []string{"failed", "STALE"}}); tableHosts(tb) != "BRAVO,CHARLIE" || tb.Total != 5 || tb.Page.Total != 2 {
		t.Errorf("state filter: %s total=%d", tableHosts(tb), tb.Total)
	}
	if tb := build(DeviceTableQuery{States: []string{"UNKNOWN"}}); tableHosts(tb) != "ECHO" {
		t.Errorf("unknown filter: %s", tableHosts(tb))
	}
	// Paging: 25 is the smallest page size, so build a long list.
	var many []DeviceRow
	for i := 0; i < 60; i++ {
		many = append(many, fakeRow("H"+string(rune('A'+i/26))+string(rune('A'+i%26)), "Acme"))
	}
	pg, err := BuildDeviceTable(many, nil, nil, DeviceTableQuery{Sort: "host", Page: 3, Size: 25})
	if err != nil || len(pg.Rows) != 10 || pg.Page.From != 51 || pg.Page.To != 60 || pg.Page.Pages != 3 {
		t.Fatalf("page 3: %v %+v rows=%d", err, pg.Page, len(pg.Rows))
	}
	if pg, _ = BuildDeviceTable(many, nil, nil, DeviceTableQuery{Page: 99, Size: 25}); !pg.Page.Clamped || pg.Page.Page != 3 {
		t.Fatalf("clamp: %+v", pg.Page)
	}
	for _, q := range []DeviceTableQuery{{States: []string{"HEALTHY"}}, {Sort: "colour"}} {
		if _, err := BuildDeviceTable(rows, view, due, q); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v: want ErrInvalid, got %v", q, err)
		}
	}
	if tb, err := BuildDeviceTable(nil, nil, nil, DeviceTableQuery{}); err != nil || tb.Rows == nil || tb.Page.Pages != 1 || tb.Page.From != 0 {
		t.Fatalf("empty: %v %+v", err, tb)
	}
}
