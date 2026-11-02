package store

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// historyRun builds the n-th run of a scripted history: run n ends n hours
// before t0, every third run fails, every seventh is a restore, the first is open.
func historyRun(n int) RunInput {
	id := fmt.Sprintf("01J9Y4K3A0B1C2D3E4F5G6%04d", n)
	end := t0.Add(-time.Duration(n) * time.Hour)
	if n == 1 {
		return RunInput{ID: id, Kind: "backup", Trigger: "schedule", StartedAt: end.Add(-5 * time.Minute)}
	}
	r := runInput(id, end)
	if n%3 == 0 {
		r.Outcome, r.ErrorClass = "failed", "network"
	}
	if n%7 == 0 {
		r.Kind, r.SnapshotID = "restore", ""
		r.Trigger = "manual"
	}
	return r
}

func ingestHistory(w *world, deviceID string, from, to int) {
	w.t.Helper()
	var runs []RunInput
	for n := from; n <= to; n++ {
		runs = append(runs, historyRun(n))
	}
	res := must[[]IngestResult](w.t)(w.s.AgentIngestRuns(w.ctx, deviceID, runs, t0))
	for _, r := range res {
		if r.Status != IngestCreated {
			w.t.Fatalf("ingest %+v", r)
		}
	}
}

func TestListDeviceRunsFiltersAndPages(t *testing.T) {
	w := seed(t)
	ingestHistory(w, w.devA.ID, 1, 60)
	ingestHistory(w, w.devB.ID, 61, 70)

	p := must[RunPage](t)(w.s.ListDeviceRuns(w.ctx, ScopeAll(), w.devA.ID, RunFilter{}, 1, 25))
	if p.Page.Total != 60 || p.Page.Pages != 3 || len(p.Items) != 25 || p.Page.From != 1 || p.Page.To != 25 {
		t.Fatalf("page 1: %+v items=%d", p.Page, len(p.Items))
	}
	// Newest start first: the open run (n=1) leads, and no run of device B is in A's history.
	if p.Items[0].Outcome != "" || p.Items[0].ID != historyRun(1).ID {
		t.Fatalf("first row: %+v", p.Items[0])
	}
	for _, r := range p.Items {
		if r.DeviceID != w.devA.ID {
			t.Fatalf("another device's run in the history: %+v", r)
		}
	}
	last := must[RunPage](t)(w.s.ListDeviceRuns(w.ctx, ScopeAll(), w.devA.ID, RunFilter{}, 3, 25))
	if len(last.Items) != 10 || last.Page.From != 51 || last.Page.To != 60 {
		t.Fatalf("page 3: %+v items=%d", last.Page, len(last.Items))
	}
	if clamp := must[RunPage](t)(w.s.ListDeviceRuns(w.ctx, ScopeAll(), w.devA.ID, RunFilter{}, 9, 25)); !clamp.Page.Clamped || clamp.Page.Page != 3 {
		t.Fatalf("clamp: %+v", clamp.Page)
	}

	count := func(f RunFilter) int {
		return must[RunPage](t)(w.s.ListDeviceRuns(w.ctx, ScopeAll(), w.devA.ID, f, 1, 200)).Page.Total
	}
	// failed: n%3==0 and n%7!=0 stays failed (the restore keeps its outcome too): 20 multiples of 3 in 1..60.
	if got := count(RunFilter{Outcome: "failed"}); got != 20 {
		t.Errorf("failed: %d", got)
	}
	if got := count(RunFilter{Outcome: "success"}); got != 39 {
		t.Errorf("success: %d", got)
	}
	if got := count(RunFilter{Outcome: RunOutcomeRunning}); got != 1 {
		t.Errorf("running: %d", got)
	}
	if got := count(RunFilter{Kind: "restore"}); got != 8 {
		t.Errorf("restore: %d", got)
	}
	if got := count(RunFilter{Kind: "restore", Outcome: "failed"}); got != 2 { // 21, 42
		t.Errorf("failed restores: %d", got)
	}
	// Range: started within the last 10 hours (inclusive start, exclusive end).
	if got := count(RunFilter{Since: t0.Add(-10*time.Hour - 15*time.Minute)}); got != 10 {
		t.Errorf("since: %d", got)
	}
	if got := count(RunFilter{Since: t0.Add(-30 * time.Hour), Until: t0.Add(-20 * time.Hour)}); got != 10 {
		t.Errorf("range: %d", got)
	}
	// Validation.
	for name, f := range map[string]RunFilter{
		"kind":    {Kind: "format"},
		"outcome": {Outcome: "great"},
		"range":   {Since: t0, Until: t0.Add(-time.Hour)},
	} {
		if _, err := w.s.ListDeviceRuns(w.ctx, ScopeAll(), w.devA.ID, f, 1, 25); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", name, err)
		}
	}
	// A device with no runs is an empty page, not an error.
	empty := must[RunPage](t)(w.s.ListDeviceRuns(w.ctx, ScopeAll(), w.devA.ID, RunFilter{Kind: "unlock"}, 1, 25))
	if empty.Items == nil || len(empty.Items) != 0 || empty.Page.Total != 0 || empty.Page.From != 0 {
		t.Fatalf("empty page: %+v", empty)
	}
}

func TestListDeviceRunsScope(t *testing.T) {
	w := seed(t)
	ingestHistory(w, w.devA.ID, 1, 5)
	ingestHistory(w, w.devB.ID, 61, 65)
	a := ScopeOf(w.custA.ID)
	if got := must[RunPage](t)(w.s.ListDeviceRuns(w.ctx, a, w.devA.ID, RunFilter{}, 1, 25)); got.Page.Total != 5 {
		t.Fatalf("own runs: %+v", got.Page)
	}
	_, eB := w.s.ListDeviceRuns(w.ctx, a, w.devB.ID, RunFilter{}, 1, 25)
	_, eGhost := w.s.ListDeviceRuns(w.ctx, a, "01J9Y4K3A0B1C2D3E4F5G6H7ZZ", RunFilter{}, 1, 25)
	if !errors.Is(eB, ErrNotFound) || !errors.Is(eGhost, ErrNotFound) {
		t.Fatalf("B's device and a ghost must both be not found: %v / %v", eB, eGhost)
	}
	if _, err := w.s.ListDeviceRuns(w.ctx, ScopeOf(), w.devA.ID, RunFilter{}, 1, 25); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty scope: %v", err)
	}
	if _, err := w.s.ListDeviceRuns(w.ctx, Scope{}, w.devA.ID, RunFilter{}, 1, 25); !errors.Is(err, ErrScope) {
		t.Fatalf("zero scope: %v", err)
	}
}
