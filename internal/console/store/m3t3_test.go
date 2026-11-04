package store

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// M3-T3: saved views, pause and resume, the fleet-wide run search and the
// heartbeat ring read.

const matrixViewer = "01J9Y4K3A0B1C2D3E4F5G6H7AB" // an admin id that owns none of the matrix views

// m3Views remembers the fresh view prepared for each target (the matrix target
// struct is shared with other tickets' tests, so the id lives here).
var m3Views = map[string]string{}

// addM3T3Matrix adds the M3-T3 methods to the §3.5 scope matrix (matrix() in
// scope_test.go calls it).
func addM3T3Matrix(m map[string]matrixCase) {
	actor := func(w *world) AuditActor { return AuditActor{AdminID: w.admin.ID, Username: "matrix", Method: "api"} }
	// A shared view owned by the matrix admin that names the target's customer.
	prepView := func(w *world, t *target) {
		v, err := w.s.CreateSavedViewAudited(w.ctx, ScopeAll(), SavedViewInput{Name: uniq("view-" + t.name), Entity: "devices", Shared: true,
			Filter: json.RawMessage(`{"all":[{"customer_id":"` + t.cust + `"}]}`)}, actor(w))
		ok(w.t, err)
		m3Views[t.name] = v.ID
	}
	m["ListSavedViews"] = matrixCase{kind: list, call: func(w *world, sc Scope, t *target) ([]string, error) {
		for _, c := range []Customer{w.custA, w.custB} {
			_, err := w.s.CreateSavedViewAudited(w.ctx, ScopeAll(), SavedViewInput{Name: "list-" + c.ID, Entity: "customers", Shared: true,
				Filter: json.RawMessage(`{"customer_id":"` + c.ID + `"}`)}, actor(w))
			if err != nil && !errors.Is(err, ErrConflict) && !errors.Is(err, ErrScope) {
				return nil, err
			}
		}
		vs, err := w.s.ListSavedViews(w.ctx, sc, matrixViewer, "customers")
		var rows []string
		for _, v := range vs {
			rows = append(rows, viewCustomerRefs(v.Filter)...)
		}
		return rows, err
	}}
	m["GetSavedView"] = matrixCase{kind: byID, prep: prepView, call: func(w *world, sc Scope, t *target) ([]string, error) {
		_, err := w.s.GetSavedView(w.ctx, sc, matrixViewer, m3Views[t.name])
		return nil, err
	}}
	m["CreateSavedViewAudited"] = matrixCase{kind: globalWrite, call: func(w *world, sc Scope, t *target) ([]string, error) {
		_, err := w.s.CreateSavedViewAudited(w.ctx, sc, SavedViewInput{Name: uniq("shared"), Entity: "runs", Shared: true}, actor(w))
		return nil, err
	}}
	m["UpdateSavedViewAudited"] = matrixCase{kind: byID, prep: prepView, call: func(w *world, sc Scope, t *target) ([]string, error) {
		n := uniq("renamed")
		a := actor(w)
		a.AdminID = matrixViewer
		_, _, err := w.s.UpdateSavedViewAudited(w.ctx, sc, m3Views[t.name], SavedViewPatch{Name: &n}, a, true)
		return nil, err
	}}
	m["DeleteSavedViewAudited"] = matrixCase{kind: byID, prep: prepView, call: func(w *world, sc Scope, t *target) ([]string, error) {
		a := actor(w)
		a.AdminID = matrixViewer
		return nil, w.s.DeleteSavedViewAudited(w.ctx, sc, m3Views[t.name], a, true)
	}}
	m["PauseDeviceAudited"] = matrixCase{kind: byID, call: func(w *world, sc Scope, t *target) ([]string, error) {
		_, err := w.s.PauseDeviceAudited(w.ctx, sc, t.dev, PauseRequest{Reason: "matrix"}, actor(w))
		if err == nil { // leave the shared world as found
			_, err = w.s.ResumeDeviceAudited(w.ctx, ScopeAll(), t.dev, actor(w))
		}
		return nil, err
	}}
	m["ResumeDeviceAudited"] = matrixCase{kind: byID, call: func(w *world, sc Scope, t *target) ([]string, error) {
		_, err := w.s.ResumeDeviceAudited(w.ctx, sc, t.dev, actor(w))
		return nil, err
	}}
	m["SearchRunsAt"] = matrixCase{kind: list, call: func(w *world, sc Scope, t *target) ([]string, error) {
		p, err := w.s.SearchRunsAt(w.ctx, sc, RunSearch{}, 0, 200)
		var rows []string
		for _, r := range p.Items {
			rows = append(rows, r.CustomerID)
		}
		return rows, err
	}}
	m["GetRunRow"] = matrixCase{kind: byID, call: func(w *world, sc Scope, t *target) ([]string, error) {
		_, err := w.s.GetRunRow(w.ctx, sc, t.run)
		return nil, err
	}}
	m["ListHeartbeats"] = matrixCase{kind: byID, call: func(w *world, sc Scope, t *target) ([]string, error) {
		rows, _, err := w.s.ListHeartbeats(w.ctx, sc, t.dev, time.Time{}, 0)
		if err == nil && len(rows) == 0 {
			err = errors.New("the seeded heartbeat is missing")
		}
		return nil, err
	}}
}

func actorOf(w *world) AuditActor {
	return AuditActor{AdminID: w.admin.ID, Username: "root", Method: "api", IP: "192.0.2.1"}
}

func openRun(id string, start time.Time) RunInput {
	return RunInput{ID: id, Kind: "backup", Trigger: "schedule", StartedAt: start}
}

func TestPauseCancelsTheRunningBackupAndWritesOneAuditRow(t *testing.T) { // M3-T3: DH11
	w := seed(t)
	w.heartbeat(w.devA.ID)
	ingest(t, w, w.devA.ID, openRun(runID1, t0.Add(-5*time.Minute)))
	until := t0.Add(4 * time.Hour)

	res, err := w.s.PauseDeviceAudited(w.ctx, ScopeAll(), w.devA.ID, PauseRequest{Reason: "replacing the disk", Until: &until}, actorOf(w))
	ok(t, err)
	if !res.Changed || res.CancelledRunID != runID1 || res.QueuedCommands != 2 {
		t.Fatalf("pause = %+v, want changed, run %s cancelled, 2 commands (cancel + refresh)", res, runID1)
	}
	if res.Device.PausedAt == nil || res.Device.PausedUntil == nil || !res.Device.PausedUntil.Equal(until) || res.Device.PauseReason != "replacing the disk" || res.Device.PausedBy != w.admin.ID {
		t.Fatalf("device after pause: %+v", res.Device)
	}
	if n := auditCount(t, w, "device.pause"); n != 1 {
		t.Fatalf("%d device.pause audit rows, want exactly 1", n)
	}
	cmds := must[Listing[Command]](t)(w.s.ListCommands(w.ctx, ScopeAll(), w.devA.ID, Page{Limit: 50}))
	var cancel, refresh int
	for _, c := range cmds.Items {
		switch c.Type {
		case CommandCancel:
			cancel++
			if !strings.Contains(string(c.Params), runID1) {
				t.Errorf("cancel params %s do not name the running run", c.Params)
			}
		case CommandRefreshPolicy:
			refresh++
		}
	}
	if cancel != 1 || refresh != 1 {
		t.Fatalf("queued cancel=%d refresh=%d, want 1 and 1", cancel, refresh)
	}
	if n := auditCount(t, w, "command.queue"); n != 0 {
		t.Fatalf("the queued commands wrote %d audit rows of their own; the pause row must be the only one", n)
	}

	// The same request again changes nothing and writes nothing.
	again, err := w.s.PauseDeviceAudited(w.ctx, ScopeAll(), w.devA.ID, PauseRequest{Reason: "replacing the disk", Until: &until}, actorOf(w))
	ok(t, err)
	if again.Changed || auditCount(t, w, "device.pause") != 1 {
		t.Fatalf("a repeated pause = %+v, audit rows %d", again, auditCount(t, w, "device.pause"))
	}
}

func TestPauseRefusesWhatTheSchemaAndTheOperatorCannotMean(t *testing.T) {
	w := seed(t)
	past, soon, far := t0.Add(-time.Hour), t0.Add(30*time.Second), t0.Add(MaxPauseSpan+24*time.Hour)
	for name, req := range map[string]PauseRequest{
		"no reason":       {},
		"a blank reason":  {Reason: "   "},
		"a path":          {Reason: `disk at C:\Users\someone\Documents is failing`},
		"a newline":       {Reason: "line one\nline two"},
		"an end in past":  {Reason: "x", Until: &past},
		"under a minute":  {Reason: "x", Until: &soon},
		"beyond the span": {Reason: "x", Until: &far},
	} {
		_, err := w.s.PauseDeviceAudited(w.ctx, ScopeAll(), w.devA.ID, req, actorOf(w))
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
	if n := auditCount(t, w, "device.pause"); n != 0 {
		t.Fatalf("refused pauses wrote %d audit rows", n)
	}
	dev := must[Device](t)(w.s.GetDevice(w.ctx, ScopeAll(), w.devA.ID))
	if dev.PausedAt != nil {
		t.Fatal("a refused pause parked the device")
	}
	// A retired device cannot be paused (ErrConflict, a clear reason), a device outside scope is not found.
	ok(t, errOf(w.s.UpdateDevice(w.ctx, ScopeAll(), w.devB.ID, DeviceUpdate{Retire: &RetireSpec{Reason: "gone"}})))
	if _, err := w.s.PauseDeviceAudited(w.ctx, ScopeAll(), w.devB.ID, PauseRequest{Reason: "x"}, actorOf(w)); !errors.Is(err, ErrDeviceRetired) {
		t.Fatalf("pausing a retired device = %v", err)
	}
	if _, err := w.s.PauseDeviceAudited(w.ctx, ScopeOf(w.custB.ID), w.devA.ID, PauseRequest{Reason: "x"}, actorOf(w)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("pausing another customer's device = %v", err)
	}
}

func errOf[T any](_ T, err error) error { return err }

func TestResumeEndsTheParkingAndIsIdempotent(t *testing.T) {
	w := seed(t)
	// Indefinite pause: no end.
	res := must[PauseResult](t)(w.s.PauseDeviceAudited(w.ctx, ScopeAll(), w.devA.ID, PauseRequest{Reason: "customer is closed for renovation"}, actorOf(w)))
	if res.Device.PausedUntil != nil || res.CancelledRunID != "" || res.QueuedCommands != 1 {
		t.Fatalf("indefinite pause = %+v", res)
	}
	r1 := must[PauseResult](t)(w.s.ResumeDeviceAudited(w.ctx, ScopeAll(), w.devA.ID, actorOf(w)))
	if !r1.Changed || r1.Device.PausedAt != nil || r1.Device.PauseReason != "" {
		t.Fatalf("resume = %+v", r1)
	}
	r2 := must[PauseResult](t)(w.s.ResumeDeviceAudited(w.ctx, ScopeAll(), w.devA.ID, actorOf(w)))
	if r2.Changed {
		t.Fatalf("a second resume reports a change: %+v", r2)
	}
	if n := auditCount(t, w, "device.resume"); n != 1 {
		t.Fatalf("%d device.resume audit rows, want exactly 1", n)
	}
}

func TestPauseIsRefusedWhenItCannotBeAudited(t *testing.T) {
	w := seed(t)
	ok(t, w.s.Write(w.ctx, func(tx *Tx) error {
		_, err := tx.Exec(`CREATE TRIGGER refuse_audits BEFORE INSERT ON audit_events BEGIN SELECT RAISE(ABORT, 'no'); END`)
		return err
	}))
	_, err := w.s.PauseDeviceAudited(w.ctx, ScopeAll(), w.devA.ID, PauseRequest{Reason: "x"}, actorOf(w))
	if !errors.Is(err, ErrAudit) {
		t.Fatalf("err = %v, want ErrAudit", err)
	}
	if dev := must[Device](t)(w.s.GetDevice(w.ctx, ScopeAll(), w.devA.ID)); dev.PausedAt != nil {
		t.Fatal("the device was paused although its audit row could not be written")
	}
}

func TestSavedViewsOwnSharedAndScope(t *testing.T) {
	w := seed(t)
	owner, other := actorOf(w), AuditActor{AdminID: matrixViewer, Username: "other", Method: "api"}
	mine := must[SavedView](t)(w.s.CreateSavedViewAudited(w.ctx, ScopeAll(), SavedViewInput{Name: "Mine", Entity: "devices"}, owner))
	shared := must[SavedView](t)(w.s.CreateSavedViewAudited(w.ctx, ScopeAll(), SavedViewInput{Name: "Everyone", Entity: "devices", Shared: true,
		Filter: json.RawMessage(`{"state":"STALE"}`), Columns: json.RawMessage(`["hostname","state"]`)}, owner))
	if string(mine.Filter) != "{}" || string(mine.Columns) != "[]" || string(mine.Sort) != "[]" || mine.Shared {
		t.Fatalf("defaults: %+v", mine)
	}

	names := func(vs []SavedView, err error) string {
		t.Helper()
		ok(t, err)
		var n []string
		for _, v := range vs {
			n = append(n, v.Name)
		}
		return strings.Join(n, ",")
	}
	if got := names(w.s.ListSavedViews(w.ctx, ScopeAll(), w.admin.ID, "")); got != "Everyone,Mine" {
		t.Fatalf("owner sees %q", got)
	}
	if got := names(w.s.ListSavedViews(w.ctx, ScopeAll(), other.AdminID, "")); got != "Everyone" {
		t.Fatalf("another admin sees %q, want only the shared view", got)
	}
	if got := names(w.s.ListSavedViews(w.ctx, ScopeAll(), w.admin.ID, "runs")); got != "" {
		t.Fatalf("entity filter: %q", got)
	}
	if _, err := w.s.ListSavedViews(w.ctx, ScopeAll(), w.admin.ID, "reports"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown entity = %v", err)
	}
	if _, err := w.s.GetSavedView(w.ctx, ScopeAll(), other.AdminID, mine.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another admin's private view = %v, want ErrNotFound", err)
	}
	// A scoped caller cannot share (ErrScope), and a duplicate name is a conflict.
	if _, err := w.s.CreateSavedViewAudited(w.ctx, ScopeOf(w.custA.ID), SavedViewInput{Name: "S", Entity: "runs", Shared: true}, other); !errors.Is(err, ErrScope) {
		t.Fatalf("scoped share = %v", err)
	}
	if _, err := w.s.CreateSavedViewAudited(w.ctx, ScopeAll(), SavedViewInput{Name: "Mine", Entity: "devices"}, owner); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate = %v", err)
	}
	// Only the owner edits; an admin may; the shared view is read-only to everyone else.
	n := "Renamed"
	if _, _, err := w.s.UpdateSavedViewAudited(w.ctx, ScopeAll(), shared.ID, SavedViewPatch{Name: &n}, other, false); !errors.Is(err, ErrScope) {
		t.Fatalf("a non-owner editing a shared view = %v, want ErrScope", err)
	}
	v, changed, err := w.s.UpdateSavedViewAudited(w.ctx, ScopeAll(), shared.ID, SavedViewPatch{Name: &n}, other, true)
	ok(t, err)
	if !changed || v.Name != "Renamed" {
		t.Fatalf("admin edit: %+v changed=%v", v, changed)
	}
	if _, changed, err := w.s.UpdateSavedViewAudited(w.ctx, ScopeAll(), shared.ID, SavedViewPatch{Name: &n}, other, true); err != nil || changed {
		t.Fatalf("a no-op edit: changed=%v err=%v", changed, err)
	}
	bad := json.RawMessage(`[1]`)
	if _, _, err := w.s.UpdateSavedViewAudited(w.ctx, ScopeAll(), mine.ID, SavedViewPatch{Filter: &bad}, owner, false); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a filter that is an array = %v", err)
	}
	ok(t, w.s.DeleteSavedViewAudited(w.ctx, ScopeAll(), mine.ID, owner, false))
	if _, err := w.s.GetSavedView(w.ctx, ScopeAll(), w.admin.ID, mine.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted view = %v", err)
	}
	for action, want := range map[string]int{"view.create": 2, "view.update": 1, "view.delete": 1} {
		if got := auditCount(t, w, action); got != want {
			t.Errorf("%s audit rows = %d, want %d", action, got, want)
		}
	}
}

func TestSavedViewNamingAnotherCustomerIsHiddenFromAScopedCaller(t *testing.T) {
	w := seed(t)
	own := must[SavedView](t)(w.s.CreateSavedViewAudited(w.ctx, ScopeAll(), SavedViewInput{Name: "A only", Entity: "devices", Shared: true,
		Filter: json.RawMessage(`{"all":[{"customer_id":"` + w.custA.ID + `"}]}`)}, actorOf(w)))
	both := must[SavedView](t)(w.s.CreateSavedViewAudited(w.ctx, ScopeAll(), SavedViewInput{Name: "A and B", Entity: "devices", Shared: true,
		Filter: json.RawMessage(`{"customer_ids":["` + w.custA.ID + `","` + w.custB.ID + `"]}`)}, actorOf(w)))
	plain := must[SavedView](t)(w.s.CreateSavedViewAudited(w.ctx, ScopeAll(), SavedViewInput{Name: "Plain", Entity: "devices", Shared: true,
		Filter: json.RawMessage(`{"state":"OK"}`)}, actorOf(w)))
	vs := must[[]SavedView](t)(w.s.ListSavedViews(w.ctx, ScopeOf(w.custA.ID), matrixViewer, "devices"))
	got := map[string]bool{}
	for _, v := range vs {
		got[v.ID] = true
	}
	if !got[own.ID] || !got[plain.ID] || got[both.ID] {
		t.Fatalf("scoped to A sees %v; want the A-only and the plain view, not the one that names B", got)
	}
	if _, err := w.s.GetSavedView(w.ctx, ScopeOf(w.custA.ID), matrixViewer, both.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get of a view naming B, scoped to A = %v", err)
	}
	if vs := must[[]SavedView](t)(w.s.ListSavedViews(w.ctx, ScopeOf(), matrixViewer, "devices")); len(vs) != 1 || vs[0].ID != plain.ID {
		t.Fatalf("an empty scope sees %d views, want only the plain one", len(vs))
	}
}

func TestSearchRunsAppliesEveryClauseThroughScope(t *testing.T) {
	w := seed(t)
	w.heartbeat(w.devA.ID)
	w.heartbeat(w.devB.ID)
	fail := runInput(runID2, t0.Add(-2*time.Hour))
	fail.Outcome, fail.ErrorClass = "failed", "locked"
	bRun := runInput("01J9Y4K3A0B1C2D3E4F5G6H7J4", t0.Add(-3*time.Hour))
	for _, res := range append(ingest(t, w, w.devA.ID, runInput(runID1, t0.Add(-time.Hour)), fail, openRun(runID3, t0.Add(-5*time.Minute))), ingest(t, w, w.devB.ID, bRun)...) {
		if res.Status != IngestCreated {
			t.Fatalf("seed run %+v", res)
		}
	}

	count := func(sc Scope, f RunSearch) (int, int) {
		t.Helper()
		p, err := w.s.SearchRunsAt(w.ctx, sc, f, 0, 100)
		ok(t, err)
		return len(p.Items), p.Page.Total
	}
	if n, _ := count(ScopeAll(), RunSearch{}); n != 4 {
		t.Fatalf("all runs = %d, want 4", n)
	}
	if n, _ := count(ScopeOf(w.custA.ID), RunSearch{}); n != 3 {
		t.Fatalf("scoped to A = %d, want 3", n)
	}
	if n, _ := count(ScopeOf(w.custA.ID), RunSearch{CustomerIDs: []string{w.custB.ID}}); n != 0 {
		t.Fatalf("A asking for B's runs sees %d", n)
	}
	if n, _ := count(ScopeAll(), RunSearch{Outcomes: []string{"failed"}}); n != 1 {
		t.Fatalf("failed = %d", n)
	}
	if n, _ := count(ScopeAll(), RunSearch{Outcomes: []string{RunOutcomeRunning}}); n != 1 {
		t.Fatalf("running = %d", n)
	}
	if n, _ := count(ScopeAll(), RunSearch{Outcomes: []string{"failed", RunOutcomeRunning}}); n != 2 {
		t.Fatalf("failed or running = %d", n)
	}
	if n, _ := count(ScopeAll(), RunSearch{DeviceIDs: []string{w.devB.ID}}); n != 1 {
		t.Fatalf("device B = %d", n)
	}
	if n, _ := count(ScopeAll(), RunSearch{Kinds: []string{"check"}}); n != 0 {
		t.Fatalf("kind check = %d", n)
	}
	if n, _ := count(ScopeAll(), RunSearch{Since: t0.Add(-150 * time.Minute), Until: t0.Add(-30 * time.Minute)}); n != 2 {
		t.Fatalf("date window = %d, want the runs started 2h-70min ago", n)
	}
	p := must[RunSearchPage](t)(w.s.SearchRunsAt(w.ctx, ScopeOf(w.custA.ID), RunSearch{Outcomes: []string{"failed"}}, 0, 100))
	if p.Of != 3 || p.Page.Total != 1 || p.Items[0].Hostname != "A-DESK-1" || p.Items[0].CustomerName != "Alpha Dental" {
		t.Fatalf("page = %+v", p)
	}
	// Paging by row offset, newest first.
	p1 := must[RunSearchPage](t)(w.s.SearchRunsAt(w.ctx, ScopeAll(), RunSearch{}, 0, 2))
	p2 := must[RunSearchPage](t)(w.s.SearchRunsAt(w.ctx, ScopeAll(), RunSearch{}, 2, 2))
	if len(p1.Items) != 2 || len(p2.Items) != 2 || p1.Page.Total != 4 || !p1.Items[0].StartedAt.After(p2.Items[1].StartedAt) {
		t.Fatalf("paging: %d/%d items, total %d", len(p1.Items), len(p2.Items), p1.Page.Total)
	}
	// Rejected clauses.
	for name, f := range map[string]RunSearch{
		"status":  {Outcomes: []string{"exploded"}},
		"kind":    {Kinds: []string{"nap"}},
		"range":   {Since: t0, Until: t0.Add(-time.Hour)},
		"newline": {CustomerIDs: []string{"a\nb"}},
	} {
		if _, err := w.s.SearchRunsAt(w.ctx, ScopeAll(), f, 0, 10); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
	if _, err := w.s.GetRunRow(w.ctx, ScopeOf(w.custA.ID), "01J9Y4K3A0B1C2D3E4F5G6H7J4"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("B's run read as A = %v", err)
	}
}

func TestListHeartbeatsIsNewestFirstWindowedAndFlagsTruncation(t *testing.T) {
	w := seed(t)
	for i := 0; i < 5; i++ {
		w.clk.Advance(time.Minute)
		w.heartbeat(w.devA.ID)
	}
	rows, truncated, err := w.s.ListHeartbeats(w.ctx, ScopeAll(), w.devA.ID, time.Time{}, 0)
	ok(t, err)
	if len(rows) != 5 || truncated || !rows[0].ReceivedAt.After(rows[4].ReceivedAt) {
		t.Fatalf("rows=%d truncated=%v", len(rows), truncated)
	}
	rows, truncated, err = w.s.ListHeartbeats(w.ctx, ScopeAll(), w.devA.ID, time.Time{}, 3)
	ok(t, err)
	if len(rows) != 3 || !truncated {
		t.Fatalf("limit 3: rows=%d truncated=%v", len(rows), truncated)
	}
	rows, _, err = w.s.ListHeartbeats(w.ctx, ScopeAll(), w.devA.ID, w.clk.Now().Add(-90*time.Second), 0)
	ok(t, err)
	if len(rows) != 2 {
		t.Fatalf("since window: %d rows, want 2", len(rows))
	}
	if _, _, err := w.s.ListHeartbeats(w.ctx, ScopeOf(w.custB.ID), w.devA.ID, time.Time{}, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another customer's ring = %v", err)
	}
}
