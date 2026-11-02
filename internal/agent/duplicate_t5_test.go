package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/agent/internal/fakerestic"
	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// M1 acceptance (architecture 10.2): "duplicate-delivery test (same run id
// twice) yields one row". These tests run the agent's real outbox and uplink
// against the real console handler and store; the fake console of the other
// agent tests would answer "duplicate" from a map and prove nothing about the
// console's rows.

// t5Records returns the open and the closed form of one partial run: three
// files failed, so the counts a duplicate delivery could double are non-zero.
func t5Records(startedAgo time.Duration) (open, closed api.RunRecord) {
	now := time.Now().UTC()
	open = api.RunRecord{
		ID: newULID(), Kind: restic.KindBackup, Trigger: api.TriggerRetry, StartedAt: now.Add(-startedAgo),
		Attempt: 2, PolicyVersion: 1, AgentVersion: "t5",
	}
	closed = open
	fin, exit := now, 3
	closed.FinishedAt, closed.Outcome, closed.ExitCode = &fin, restic.OutcomePartial, &exit
	closed.SnapshotID = strings.Repeat("ab", 32)
	closed.ErrorClass = restic.ClassPermission
	closed.ErrorCounts = []restic.ErrorProjection{{During: "archival", Class: restic.ClassPermission, Ext: "docx", Count: 3}}
	closed.Stats = api.RunStats{
		FilesNew: 4, FilesChanged: 2, FilesUnmodified: 114, TotalFilesProcessed: 120, TotalBytesProcessed: 123456,
		DataAdded: 4096, DataAddedPacked: 2048, FailedFiles: 3, TotalDurationS: 12.5,
	}
	return open, closed
}

// t5RunsResult decodes the console's answer to a POST /runs.
func t5RunsResult(t *testing.T, call wireCall, id string) api.RunResult {
	t.Helper()
	var resp api.RunsResponse
	if err := json.Unmarshal(call.Resp, &resp); err != nil {
		t.Fatalf("answer %q: %v", call.Resp, err)
	}
	for _, r := range resp.Results {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("no result for the run in %s", call.Resp)
	return api.RunResult{}
}

// deliverUntilEmpty runs the agent's outbox delivery until nothing is left,
// as its loop would after the backoff; it returns the errors it met.
func deliverUntilEmpty(t *testing.T, a *Agent) []error {
	t.Helper()
	ctx := context.Background()
	var errs []error
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if err := a.deliverOutbox(ctx); err != nil {
			errs = append(errs, err)
		}
		if n, _ := a.jr.OutboxDepth(ctx); n == 0 {
			if due, _ := a.jr.OutboxDue(ctx, time.Now().Add(time.Hour), 10); len(due) == 0 {
				return errs
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the outbox did not drain")
	return errs
}

// TestT5DuplicateDeliveryAfterALostResponse: the console commits a closed run
// and the answer is lost; the agent must not call it delivered, must send it
// again, and the console must end with one row, the right outcome and counts
// (not doubled), while the agent's journal marks it delivered.
func TestT5DuplicateDeliveryAfterALostResponse(t *testing.T) {
	ctx := context.Background()
	c := newRealConsole(t)
	dir, devID := c.enrollAgent()
	fakerestic.Use(t, "backup_ok")
	a, err := New(ctx, fakeOptions(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	open, closed := t5Records(2 * time.Minute)
	if err := a.jr.StartRun(ctx, open); err != nil {
		t.Fatal(err)
	}
	if err := a.jr.FinishRun(ctx, closed); err != nil {
		t.Fatal(err)
	}

	// First delivery: the console writes the row, the agent hears nothing.
	c.dropRuns.Store(1)
	if err := a.deliverOutbox(ctx); err == nil {
		t.Fatal("deliverOutbox reported success although no answer arrived")
	}
	stored, err := c.runRow(closed.ID)
	if err != nil {
		t.Fatalf("the console did not commit the run before the answer was lost: %v", err)
	}
	if stored.Outcome != string(restic.OutcomePartial) {
		t.Fatalf("committed row = %+v", stored)
	}
	row, ok, _ := a.jr.Run(ctx, closed.ID)
	if !ok || row.DeliveredAt != nil {
		t.Fatalf("the journal calls a run delivered that the agent never got an answer for: %+v", row)
	}
	if n, _ := a.jr.OutboxDepth(ctx); n != 1 {
		t.Fatalf("outbox depth after the lost answer = %d, want 1", n)
	}

	// Second delivery: the same run id again, now answered.
	deliverUntilEmpty(t, a)

	posts := c.runPosts(closed.ID)
	if len(posts) < 2 {
		t.Fatalf("the console saw %d deliveries of the run, want the lost one and the resend", len(posts))
	}
	if posts[0].Status != 0 {
		t.Errorf("the first answer was meant to be lost, status %d", posts[0].Status)
	}
	last := posts[len(posts)-1]
	if res := t5RunsResult(t, last, closed.ID); last.Status != 200 || res.Status != api.RunDuplicate {
		t.Errorf("resend answered %d / %+v, want 200 duplicate", last.Status, res)
	}

	// One row, the right outcome, counts not doubled.
	n := 0
	for _, r := range c.deviceRuns(devID) {
		if r.ID == closed.ID {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("the console holds %d rows for the run id, want 1", n)
	}
	got, err := c.runRow(closed.ID)
	if err != nil {
		t.Fatal(err)
	}
	var stats map[string]any
	_ = json.Unmarshal(got.Stats, &stats)
	if got.Outcome != "partial" || got.ErrorClass != "permission" || got.SnapshotID != closed.SnapshotID || got.Attempt != 2 ||
		got.ResticExitCode == nil || *got.ResticExitCode != 3 || got.Trigger != "retry" || got.FinishedAt == nil {
		t.Errorf("stored row = %+v", got)
	}
	if got.ErrorCounts["permission"] != 3 || len(got.ErrorCounts) != 1 {
		t.Errorf("error counts = %v, want permission:3 once", got.ErrorCounts)
	}
	if stats["failed_files"] != float64(3) || stats["total_files_processed"] != float64(120) || stats["files_new"] != float64(4) {
		t.Errorf("stored stats = %s", got.Stats)
	}
	if got.DataAddedBytes == nil || *got.DataAddedBytes != 4096 {
		t.Errorf("data added = %v", got.DataAddedBytes)
	}

	// The agent's side: delivered, nothing queued, nothing rejected.
	row, _, _ = a.jr.Run(ctx, closed.ID)
	if row.DeliveredAt == nil || row.RejectReason != "" {
		t.Errorf("journal row = delivered %v, reject %q", row.DeliveredAt, row.RejectReason)
	}
	if d, _ := a.jr.OutboxDepth(ctx); d != 0 {
		t.Errorf("outbox depth = %d", d)
	}
	before := len(c.runPosts(closed.ID))
	if err := a.deliverOutbox(ctx); err != nil || len(c.runPosts(closed.ID)) != before {
		t.Errorf("a delivered run was sent again (err %v)", err)
	}
}

// TestT5LostResponseOnTheOpenRecord: the open record reaches the console and
// its answer is lost, then the run closes. The console must end with the one
// row closed (an update, not a second row), and a late replay of the open
// record must neither reopen it nor change it.
func TestT5LostResponseOnTheOpenRecord(t *testing.T) {
	ctx := context.Background()
	c := newRealConsole(t)
	dir, devID := c.enrollAgent()
	fakerestic.Use(t, "backup_ok")
	a, err := New(ctx, fakeOptions(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	open, closed := t5Records(5 * time.Minute)
	if err := a.jr.StartRun(ctx, open); err != nil {
		t.Fatal(err)
	}
	c.dropRuns.Store(1)
	if err := a.deliverOutbox(ctx); err == nil {
		t.Fatal("deliverOutbox reported success although no answer arrived")
	}
	if r, err := c.runRow(open.ID); err != nil || r.Outcome != "" || r.FinishedAt != nil {
		t.Fatalf("the open run is not stored open: %+v %v", r, err)
	}

	// The run closes; the closed body replaces the open one in the outbox.
	if err := a.jr.FinishRun(ctx, closed); err != nil {
		t.Fatal(err)
	}
	deliverUntilEmpty(t, a)

	posts := c.runPosts(open.ID)
	last := posts[len(posts)-1]
	if res := t5RunsResult(t, last, open.ID); res.Status != api.RunUpdated && res.Status != api.RunDuplicate {
		t.Errorf("close answered %+v, want updated (or duplicate after a retry)", res)
	}
	assertOneClosedRow := func(when string) {
		t.Helper()
		n := 0
		for _, r := range c.deviceRuns(devID) {
			if r.ID == open.ID {
				n++
			}
		}
		got, err := c.runRow(open.ID)
		if err != nil || n != 1 {
			t.Fatalf("%s: %d rows for the run id, err %v", when, n, err)
		}
		if got.Outcome != "partial" || got.FinishedAt == nil || got.ErrorCounts["permission"] != 3 || got.SnapshotID != closed.SnapshotID {
			t.Errorf("%s: stored row = %+v", when, got)
		}
	}
	assertOneClosedRow("after the close")
	if row, _, _ := a.jr.Run(ctx, open.ID); row.DeliveredAt == nil {
		t.Error("the journal does not mark the closed run delivered")
	}

	// A stale open record arriving after the close (a retried request from
	// before it): the real client, the real handler.
	resp, err := a.up.UploadRuns(ctx, api.RunsRequest{Runs: []api.RunRecord{open}})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) != 1 || resp.Results[0].Status != api.RunDuplicate {
		t.Errorf("late open record answered %+v, want duplicate", resp.Results)
	}
	assertOneClosedRow("after a late replay of the open record")
}
