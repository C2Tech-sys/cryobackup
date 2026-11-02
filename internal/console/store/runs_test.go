package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

func ingest(t *testing.T, w *world, deviceID string, runs ...RunInput) []IngestResult {
	t.Helper()
	return must[[]IngestResult](t)(w.s.AgentIngestRuns(w.ctx, deviceID, runs, w.clk.Now()))
}

func wantStatus(t *testing.T, got IngestResult, status, reason string) {
	t.Helper()
	if got.Status != status || got.Reason != reason {
		t.Fatalf("run %s: status=%q reason=%q detail=%q; want %q %q", got.ID, got.Status, got.Reason, got.Detail, status, reason)
	}
}

func TestIngestCreatedAndDuplicate(t *testing.T) {
	w := seed(t)
	in := runInput(runID1, t0.Add(-time.Hour))
	wantStatus(t, ingest(t, w, w.devA.ID, in)[0], IngestCreated, "")
	wantStatus(t, ingest(t, w, w.devA.ID, in)[0], IngestDuplicate, "")

	got := must[Run](t)(w.s.GetRun(w.ctx, ScopeAll(), runID1))
	if got.Origin != "agent" || got.Outcome != "success" || got.Attempt != 1 || got.DeviceID != w.devA.ID {
		t.Fatalf("run = %+v", got)
	}
	if got.FinishedAt == nil || !got.FinishedAt.Equal(t0.Add(-time.Hour)) || !got.ReceivedAt.Equal(t0) {
		t.Fatalf("times: finished=%v received=%v", got.FinishedAt, got.ReceivedAt)
	}
	list := must[Listing[Run]](t)(w.s.ListRuns(w.ctx, ScopeAll(), w.devA.ID, Page{}))
	if len(list.Items) != 1 {
		t.Fatalf("ListRuns = %d runs after a duplicate, want 1", len(list.Items))
	}
}

func TestIngestOpenRunIsClosedLater(t *testing.T) {
	w := seed(t)
	open := RunInput{ID: runID1, Kind: "backup", Trigger: "schedule", StartedAt: t0.Add(-30 * time.Minute)}
	wantStatus(t, ingest(t, w, w.devA.ID, open)[0], IngestCreated, "")
	r := must[Run](t)(w.s.GetRun(w.ctx, ScopeAll(), runID1))
	if r.FinishedAt != nil || r.Outcome != "" {
		t.Fatalf("an open run has finished_at=%v outcome=%q", r.FinishedAt, r.Outcome)
	}
	// Resending the open record changes nothing.
	wantStatus(t, ingest(t, w, w.devA.ID, open)[0], IngestDuplicate, "")

	w.clk.Advance(20 * time.Minute)
	closed := runInput(runID1, w.clk.Now().Add(-time.Minute))
	closed.StartedAt = open.StartedAt
	closed.SnapshotID = strings.Repeat("ab", 32)
	wantStatus(t, ingest(t, w, w.devA.ID, closed)[0], IngestUpdated, "")
	r = must[Run](t)(w.s.GetRun(w.ctx, ScopeAll(), runID1))
	if r.Outcome != "success" || r.FinishedAt == nil || r.SnapshotID != closed.SnapshotID {
		t.Fatalf("run after closing = %+v", r)
	}
	if !r.StartedAt.Equal(open.StartedAt) {
		t.Fatalf("closing moved started_at to %v", r.StartedAt)
	}
	// A stale open record arriving after the close must not reopen it.
	wantStatus(t, ingest(t, w, w.devA.ID, open)[0], IngestDuplicate, "")
	if r = must[Run](t)(w.s.GetRun(w.ctx, ScopeAll(), runID1)); r.Outcome != "success" {
		t.Fatalf("a stale open record reopened the run: %+v", r)
	}
}

func TestIngestLateReportOverwritesAbandoned(t *testing.T) {
	w := seed(t)
	open := RunInput{ID: runID1, Kind: "backup", Trigger: "schedule", StartedAt: t0.Add(-20 * time.Hour)}
	ingest(t, w, w.devA.ID, open)
	// The sweeper abandons it (it marks the row directly).
	ok(t, w.s.Write(w.ctx, func(tx *Tx) error {
		_, err := tx.Exec(`UPDATE runs SET outcome = 'abandoned', finished_at = started_at + 60000 WHERE id = ?`, runID1)
		return err
	}))
	late := runInput(runID1, t0.Add(-19*time.Hour))
	late.StartedAt = open.StartedAt
	wantStatus(t, ingest(t, w, w.devA.ID, late)[0], IngestUpdated, "")
	if r := must[Run](t)(w.s.GetRun(w.ctx, ScopeAll(), runID1)); r.Outcome != "success" {
		t.Fatalf("outcome = %q, want the late success to overwrite abandoned", r.Outcome)
	}
}

func TestIngestRejections(t *testing.T) {
	w := seed(t)
	good := runInput(runID1, t0.Add(-time.Hour))
	ingest(t, w, w.devA.ID, good)

	// A run id already owned by another device.
	wantStatus(t, ingest(t, w, w.devB.ID, good)[0], IngestRejected, ReasonWrongDevice)
	if r := must[Run](t)(w.s.GetRun(w.ctx, ScopeAll(), runID1)); r.DeviceID != w.devA.ID {
		t.Fatal("a rejected run changed the owner")
	}

	fin := func(d time.Duration) *time.Time { x := t0.Add(d); return &x }
	cases := []struct {
		name   string
		mutate func(r *RunInput)
		reason string
	}{
		{"seconds-epoch started_at", func(r *RunInput) { r.StartedAt = time.UnixMilli(1_700_000_000) }, ReasonInvalidTimestamp}, // a seconds value read as milliseconds is 1970
		{"seconds-epoch finished_at", func(r *RunInput) {
			r.FinishedAt = func() *time.Time { x := time.UnixMilli(1_800_000_000); return &x }()
		}, ReasonInvalidTimestamp},
		{"outcome without finished_at", func(r *RunInput) { r.FinishedAt = nil }, ReasonOutcomeFinishedMismatch},
		{"finished_at without outcome", func(r *RunInput) { r.Outcome = "" }, ReasonOutcomeFinishedMismatch},
		{"finished before started", func(r *RunInput) { r.FinishedAt = fin(-3 * time.Hour); r.StartedAt = t0.Add(-time.Hour) }, ReasonFinishedBeforeStarted},
		{"deferred without a reason", func(r *RunInput) { r.Outcome = "deferred" }, ReasonDeferredReasonRequired},
		{"deferred reason on a success", func(r *RunInput) { r.DeferredReason = "lock" }, ReasonDeferredReasonUnexpected},
		{"snapshot on a non-backup run", func(r *RunInput) { r.Kind = "check"; r.SnapshotID = strings.Repeat("a", 64) }, ReasonSnapshotOnNonBackup},
		{"malformed id", func(r *RunInput) { r.ID = "short" }, ReasonInvalidID},
		{"stats not an object", func(r *RunInput) { r.Stats = []byte(`[1,2]`) }, ReasonInvalidStats},
		{"unknown kind (database CHECK)", func(r *RunInput) { r.Kind = "defrag" }, ReasonInvalidValue},
		{"bad snapshot id (database CHECK)", func(r *RunInput) { r.SnapshotID = "xyz" }, ReasonInvalidValue},
		{"unknown outcome (database CHECK)", func(r *RunInput) { r.Outcome = "great" }, ReasonInvalidValue},
	}
	for i, c := range cases {
		r := runInput("01J9Y4K3A0B1C2D3E4F5G6H7K"+string(rune('0'+i%10)), t0.Add(-2*time.Hour))
		if i >= 10 {
			r.ID = "01J9Y4K3A0B1C2D3E4F5G6H7M" + string(rune('0'+i%10))
		}
		c.mutate(&r)
		res := ingest(t, w, w.devA.ID, r)[0]
		wantStatus(t, res, IngestRejected, c.reason)
		if c.reason == ReasonInvalidValue && res.Detail == "" {
			t.Errorf("%s: invalid_value must carry the constraint detail", c.name)
		}
	}
	// Nothing but the one good run was stored.
	if n := must[Listing[Run]](t)(w.s.ListRuns(w.ctx, ScopeAll(), "", Page{})); len(n.Items) != 1 {
		t.Fatalf("%d runs stored, want only the good one", len(n.Items))
	}
}

// One bad run never fails the batch: the good ones around it are stored.
func TestIngestBatchWithABadRunKeepsTheRest(t *testing.T) {
	w := seed(t)
	bad := runInput(runID2, t0.Add(-time.Hour))
	bad.StartedAt = time.UnixMilli(1_700_000_000) // seconds, not milliseconds
	res := ingest(t, w, w.devA.ID, runInput(runID1, t0.Add(-3*time.Hour)), bad, runInput(runID3, t0.Add(-time.Hour)))
	wantStatus(t, res[0], IngestCreated, "")
	wantStatus(t, res[1], IngestRejected, ReasonInvalidTimestamp)
	wantStatus(t, res[2], IngestCreated, "")
	if n := must[Listing[Run]](t)(w.s.ListRuns(w.ctx, ScopeAll(), w.devA.ID, Page{})); len(n.Items) != 2 {
		t.Fatalf("%d runs stored, want 2", len(n.Items))
	}
}

// DH5: a device clock three hours ahead cannot make a backup look fresh.
func TestIngestServerTimeCorrectionAndClamp(t *testing.T) {
	w := seed(t)
	offset := int64(3 * time.Hour / time.Millisecond)
	ok(t, w.s.AgentUpsertPresence(w.ctx, w.devA.ID, Presence{ReceivedAt: t0, ServiceState: "idle", ClockOffsetMS: &offset}))

	// The device says it ran from 13:32 to 13:41 by ITS clock, which is 3 h ahead of the server.
	devStart, devEnd := t0.Add(3*time.Hour-10*time.Minute), t0.Add(3*time.Hour-time.Minute)
	r := runInput(runID1, devEnd)
	r.StartedAt = devStart
	wantStatus(t, ingest(t, w, w.devA.ID, r)[0], IngestCreated, "")
	got := must[Run](t)(w.s.GetRun(w.ctx, ScopeAll(), runID1))
	if !got.StartedAt.Equal(t0.Add(-10*time.Minute)) || !got.FinishedAt.Equal(t0.Add(-time.Minute)) {
		t.Fatalf("corrected times = %v .. %v, want %v .. %v", got.StartedAt, got.FinishedAt, t0.Add(-10*time.Minute), t0.Add(-time.Minute))
	}
	if got.ClockOffsetMS != offset {
		t.Fatalf("clock_offset_ms = %d, want the correction applied (%d)", got.ClockOffsetMS, offset)
	}

	// A finish time in the console's future (device clock further ahead than its reported offset) is clamped to received_at.
	r2 := runInput(runID2, t0.Add(3*time.Hour+30*time.Minute))
	r2.StartedAt = t0.Add(3*time.Hour - time.Minute)
	wantStatus(t, ingest(t, w, w.devA.ID, r2)[0], IngestCreated, "")
	got = must[Run](t)(w.s.GetRun(w.ctx, ScopeAll(), runID2))
	if !got.FinishedAt.Equal(t0) || got.StartedAt.After(t0) {
		t.Fatalf("clamped times = %v .. %v, want finished == received_at %v", got.StartedAt, got.FinishedAt, t0)
	}

	// A negative offset (device behind) moves times forward, still clamped to received_at.
	neg := int64(-2 * time.Hour / time.Millisecond)
	ok(t, w.s.AgentUpsertPresence(w.ctx, w.devB.ID, Presence{ReceivedAt: t0, ServiceState: "idle", ClockOffsetMS: &neg}))
	r3 := runInput(runID3, t0.Add(-2*time.Hour-5*time.Minute)) // device clock 2 h behind: server time is 5 min ago
	wantStatus(t, ingest(t, w, w.devB.ID, r3)[0], IngestCreated, "")
	got = must[Run](t)(w.s.GetRun(w.ctx, ScopeAll(), runID3))
	if !got.FinishedAt.Equal(t0.Add(-5 * time.Minute)) {
		t.Fatalf("finished = %v, want %v", got.FinishedAt, t0.Add(-5*time.Minute))
	}
}

func TestIngestUnknownDeviceAndBadReceivedAt(t *testing.T) {
	w := seed(t)
	if _, err := w.s.AgentIngestRuns(w.ctx, "01J9Y4K3A0B1C2D3E4F5G6H7ZZ", []RunInput{runInput(runID1, t0)}, t0); err == nil {
		t.Fatal("ingest for an unknown device succeeded")
	}
	if _, err := w.s.AgentIngestRuns(w.ctx, w.devA.ID, nil, time.Time{}); err == nil {
		t.Fatal("zero receivedAt accepted")
	}
}

func TestConsoleRunLifecycle(t *testing.T) {
	w := seed(t)
	r := must[Run](t)(w.s.CreateConsoleRun(w.ctx, ScopeAll(), ConsoleRunInput{DeviceID: w.devA.ID, Kind: "prune"}))
	if r.Origin != "console" || r.FinishedAt != nil || r.Trigger != "schedule" {
		t.Fatalf("console run = %+v", r)
	}
	w.clk.Advance(5 * time.Minute)
	done := must[Run](t)(w.s.FinishConsoleRun(w.ctx, ScopeAll(), r.ID, ConsoleRunResult{Outcome: "success"}))
	if done.Outcome != "success" || done.FinishedAt == nil || !done.FinishedAt.Equal(t0.Add(5*time.Minute)) {
		t.Fatalf("finished = %+v", done)
	}
	if _, err := w.s.FinishConsoleRun(w.ctx, ScopeAll(), r.ID, ConsoleRunResult{Outcome: "failed"}); err == nil {
		t.Fatal("closing a closed console run succeeded")
	}
	// An agent run cannot be closed through the console path.
	ingest(t, w, w.devA.ID, RunInput{ID: runID1, Kind: "backup", Trigger: "manual", StartedAt: t0})
	if _, err := w.s.FinishConsoleRun(w.ctx, ScopeAll(), runID1, ConsoleRunResult{Outcome: "success"}); err == nil {
		t.Fatal("FinishConsoleRun closed an agent run")
	}
	if _, err := w.s.FinishConsoleRun(w.ctx, ScopeAll(), r.ID, ConsoleRunResult{}); err == nil {
		t.Fatal("an empty outcome was accepted")
	}
}

func TestListRunsPaging(t *testing.T) {
	w := seed(t)
	var batch []RunInput
	for i := 0; i < 7; i++ {
		id := "01J9Y4K3A0B1C2D3E4F5G6H7P" + string(rune('0'+i))
		batch = append(batch, runInput(id, t0.Add(-time.Duration(i+1)*time.Hour)))
	}
	ingest(t, w, w.devA.ID, batch...)
	var seen []string
	cursor := ""
	for pages := 0; pages < 10; pages++ {
		l := must[Listing[Run]](t)(w.s.ListRuns(w.ctx, ScopeAll(), w.devA.ID, Page{Limit: 3, Cursor: cursor}))
		for _, r := range l.Items {
			seen = append(seen, r.ID)
		}
		if l.NextCursor == "" {
			break
		}
		cursor = l.NextCursor
	}
	if len(seen) != 7 {
		t.Fatalf("paged through %d runs, want 7: %v", len(seen), seen)
	}
	if _, err := w.s.ListRuns(w.ctx, ScopeAll(), w.devA.ID, Page{Cursor: "garbage"}); err == nil {
		t.Fatal("a garbage cursor was accepted")
	}
}

func TestSnapshotsReplace(t *testing.T) {
	w := seed(t)
	ctx := context.Background()
	snap := func(c string, at time.Time) Snapshot {
		return Snapshot{SnapshotID: strings.Repeat(c, 64), TakenAt: at, Hostname: w.devA.ID, Paths: []string{`C:\Users`}, Tags: []string{"cryo"}, ProgramVersion: "0.19.1"}
	}
	up, rm := must2(t)(w.s.AgentReplaceSnapshots(ctx, w.devA.ID, []Snapshot{snap("a", t0.Add(-3*time.Hour)), snap("b", t0.Add(-2*time.Hour)), snap("c", t0.Add(-time.Hour))}))
	if up != 3 || rm != 0 {
		t.Fatalf("first replace = %d upserted, %d removed", up, rm)
	}
	// "a" was forgotten, "d" is new, "b" and "c" remain: upsert present, delete absent.
	up, rm = must2(t)(w.s.AgentReplaceSnapshots(ctx, w.devA.ID, []Snapshot{snap("b", t0.Add(-2*time.Hour)), snap("c", t0.Add(-time.Hour)), snap("d", t0)}))
	if up != 3 || rm != 1 {
		t.Fatalf("second replace = %d upserted, %d removed", up, rm)
	}
	list := must[[]Snapshot](t)(w.s.ListSnapshots(ctx, ScopeAll(), w.devA.ID))
	if len(list) != 3 || list[0].SnapshotID != strings.Repeat("d", 64) || list[2].SnapshotID != strings.Repeat("b", 64) {
		t.Fatalf("snapshots = %+v", list)
	}
	if list[0].Paths[0] != `C:\Users` || list[0].Tags[0] != "cryo" {
		t.Fatalf("paths/tags did not round-trip: %+v", list[0])
	}
	// A bad snapshot id fails the whole call and changes nothing.
	bad := snap("e", t0)
	bad.SnapshotID = "not-hex"
	if _, _, err := w.s.AgentReplaceSnapshots(ctx, w.devA.ID, []Snapshot{bad}); err == nil {
		t.Fatal("a bad snapshot id was accepted")
	}
	if list = must[[]Snapshot](t)(w.s.ListSnapshots(ctx, ScopeAll(), w.devA.ID)); len(list) != 3 {
		t.Fatalf("a failed replace changed the cache: %d snapshots", len(list))
	}
	// Another device's cache is untouched.
	if other := must[[]Snapshot](t)(w.s.ListSnapshots(ctx, ScopeAll(), w.devB.ID)); len(other) != 0 {
		t.Fatalf("device B has %d snapshots", len(other))
	}
}

// must2 adapts a (int, int, error) call.
func must2(t *testing.T) func(int, int, error) (int, int) {
	return func(a, b int, err error) (int, int) {
		t.Helper()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		return a, b
	}
}

// Change 5 (P1-FIX3): a console-origin run with the same id, on the same device,
// can be neither closed nor reopened by the agent.
func TestIngestCannotTouchConsoleOriginRuns(t *testing.T) {
	w := seed(t)
	open := must[Run](t)(w.s.CreateConsoleRun(w.ctx, ScopeAll(), ConsoleRunInput{DeviceID: w.devA.ID, Kind: "check"}))

	// Closing: the agent presents the console run's id with a finished outcome.
	closing := runInput(open.ID, t0.Add(-time.Minute))
	closing.Kind = "check"
	wantStatus(t, ingest(t, w, w.devA.ID, closing)[0], IngestRejected, ReasonWrongDevice)
	got := must[Run](t)(w.s.GetRun(w.ctx, ScopeAll(), open.ID))
	if got.Origin != "console" || got.FinishedAt != nil || got.Outcome != "" {
		t.Fatalf("the agent closed a console run: %+v", got)
	}

	// Reopening: a console run abandoned by the sweeper must not be overwritten by an agent "late outcome".
	ok(t, w.s.Write(w.ctx, func(tx *Tx) error {
		_, err := tx.Exec(`UPDATE runs SET finished_at = started_at, outcome = 'abandoned' WHERE id = ?`, open.ID)
		return err
	}))
	wantStatus(t, ingest(t, w, w.devA.ID, closing)[0], IngestRejected, ReasonWrongDevice)
	got = must[Run](t)(w.s.GetRun(w.ctx, ScopeAll(), open.ID))
	if got.Outcome != "abandoned" || got.Origin != "console" {
		t.Fatalf("the agent reopened a console run: %+v", got)
	}

	// The agent's own runs still close normally.
	openAgent := runInput(runID2, t0.Add(-time.Hour))
	openAgent.FinishedAt, openAgent.Outcome = nil, ""
	wantStatus(t, ingest(t, w, w.devA.ID, openAgent)[0], IngestCreated, "")
	wantStatus(t, ingest(t, w, w.devA.ID, runInput(runID2, t0.Add(-time.Hour)))[0], IngestUpdated, "")
}
