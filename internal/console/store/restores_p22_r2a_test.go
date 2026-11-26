package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// P22-R2a, A1 (ADR 0032): a running device restore whose device has gone silent for
// DeviceRestoreSilenceLimit is closed as FAILED interrupted, with the reason kept and audited and
// no path anywhere; the agent's report arriving after that is kept beside the console's verdict,
// never reopening the row.

const (
	rsQuiet   = "01JD0000000000000000000R11"
	rsExpired = "01JD0000000000000000000R12"
)

func (r *sweepRig) restoreProgress(t *testing.T, id string) DeviceRestoreProgress {
	t.Helper()
	var raw *string
	if err := r.s.Read(context.Background(), func(tx *Tx) error {
		return tx.QueryRow(`SELECT progress FROM restore_requests WHERE id = ?`, id).Scan(&raw)
	}); err != nil {
		t.Fatal(err)
	}
	var p DeviceRestoreProgress
	if raw != nil {
		if err := json.Unmarshal([]byte(*raw), &p); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

func noPathIn(t *testing.T, what string, b []byte) {
	t.Helper()
	if s := string(b); strings.Contains(s, "/") || strings.Contains(s, `\`) || strings.Contains(s, "Users") {
		t.Errorf("%s carries something path-like: %s", what, s)
	}
}

func TestR2A_SweepClosesASilentRestoreAfterTheLimitNotBefore(t *testing.T) {
	r := newSweepRig(t)
	ctx := context.Background()
	r.runningRestore(t, rsQuiet, 2*time.Hour)                     // fetched two hours ago
	r.heartbeat(t, 90*time.Minute, 30*24*time.Hour, "")           // last heard 90 minutes ago; no restart since the fetch
	r.now = r.now.Add(DeviceRestoreSilenceLimit - 91*time.Minute) // silent for the limit less a minute

	res, err := r.s.SweepAbandonedRuns(ctx, SweepOptions{})
	if err != nil || res.RestoresClosed != 0 {
		t.Fatalf("closed before the limit: %+v %v", res, err)
	}
	if st, _, _ := r.restoreState(t, rsQuiet); st != "running" {
		t.Fatalf("before the limit: %s", st)
	}

	r.now = r.now.Add(2 * time.Minute) // a minute past the limit (and well inside the 48 h ceiling)
	res, err = r.s.SweepAbandonedRuns(ctx, SweepOptions{})
	if err != nil || res.RestoresClosed != 1 || res.RestoresClosedBy[RestoreClosedAgentSilent] != 1 {
		t.Fatalf("past the limit: %+v %v", res, err)
	}
	if st, cl, cleared := r.restoreState(t, rsQuiet); st != "failed" || cl != "interrupted" || !cleared {
		t.Fatalf("closed row: %s/%s cleared=%v", st, cl, cleared)
	}
	if p := r.restoreProgress(t, rsQuiet); p.Outcome != "interrupted" || p.ClosedBy != RestoreClosedAgentSilent || p.Late != nil {
		t.Fatalf("progress %+v", p)
	}
	l, err := r.s.ListAudit(ctx, ScopeAll(), AuditFilter{Action: "restore.finish", ObjectID: rsQuiet}, Page{Limit: 5})
	if err != nil || len(l.Items) != 1 || !strings.Contains(string(l.Items[0].Details), `"reason":"agent_silent"`) {
		t.Fatalf("audit %+v %v", l.Items, err)
	}
	noPathIn(t, "restore.finish", l.Items[0].Details)
	if l.Items[0].ActorType != "system" || l.Items[0].Result != "error" {
		t.Errorf("audit row shape %+v", l.Items[0])
	}
	// No longer active: the device may take another device restore.
	var active bool
	if err := r.s.Read(ctx, func(tx *Tx) error {
		var err error
		active, err = activeDeviceRestore(tx, r.device, tx.nowMS())
		return err
	}); err != nil || active {
		t.Fatalf("still active after the close: %v %v", active, err)
	}
}

func TestR2A_LateReportIsKeptBesideTheClosedRow(t *testing.T) {
	r := newSweepRig(t)
	ctx := context.Background()
	r.runningRestore(t, rsQuiet, DeviceRestoreSilenceLimit+time.Hour)
	r.heartbeat(t, DeviceRestoreSilenceLimit+time.Hour, 30*24*time.Hour, "")
	if res, err := r.s.SweepAbandonedRuns(ctx, SweepOptions{}); err != nil || res.RestoresClosedBy[RestoreClosedAgentSilent] != 1 {
		t.Fatalf("sweep %+v %v", res, err)
	}

	// The open record the agent sent at start, arriving late: nothing changes, nothing is audited.
	start := r.now.Add(-DeviceRestoreSilenceLimit - time.Hour)
	if out, err := r.s.AgentIngestRuns(ctx, r.device, []RunInput{{ID: rsQuiet, Kind: "restore", Trigger: "manual", StartedAt: start}}, r.now); err != nil || out[0].Status != IngestCreated {
		t.Fatalf("late open record %+v %v", out, err)
	}
	if p := r.restoreProgress(t, rsQuiet); p.Late != nil {
		t.Fatalf("an open record was taken as a report: %+v", p.Late)
	}

	// The closed record: ingested (the run row closes), the restore row keeps the console's verdict,
	// and the report is kept beside it and audited.
	fin := r.now
	stats := []byte(`{"files_restored":7,"files_skipped":2,"bytes_restored":4096,"restore_result":"","restore_errors":[]}`)
	out, err := r.s.AgentIngestRuns(ctx, r.device, []RunInput{{ID: rsQuiet, Kind: "restore", Trigger: "manual", StartedAt: start,
		FinishedAt: &fin, Outcome: "success", Stats: stats}}, r.now)
	if err != nil || out[0].Status != IngestUpdated {
		t.Fatalf("late closed record %+v %v", out, err)
	}
	if st, cl, _ := r.restoreState(t, rsQuiet); st != "failed" || cl != "interrupted" {
		t.Fatalf("the late report reopened the row: %s/%s", st, cl)
	}
	p := r.restoreProgress(t, rsQuiet)
	if p.ClosedBy != RestoreClosedAgentSilent || p.Late == nil || p.Late.Status != "delivered" || p.Late.Class != "" ||
		p.Late.FilesRestored != 7 || p.Late.FilesSkipped != 2 || p.Late.Outcome != "success" || !p.Late.ReceivedAt.Equal(r.now) {
		t.Fatalf("late report %+v / %+v", p, p.Late)
	}
	l, err := r.s.ListAudit(ctx, ScopeAll(), AuditFilter{Action: "restore.late_report", ObjectID: rsQuiet}, Page{Limit: 5})
	if err != nil || len(l.Items) != 1 {
		t.Fatalf("late audit %+v %v", l.Items, err)
	}
	d := string(l.Items[0].Details)
	for _, want := range []string{`"late_outcome":"success"`, `"late_status":"delivered"`, `"status":"failed"`, `"closed_by":"agent_silent"`, `"files_restored":7`} {
		if !strings.Contains(d, want) {
			t.Errorf("late audit lacks %s: %s", want, d)
		}
	}
	noPathIn(t, "restore.late_report", l.Items[0].Details)
	if l.Items[0].ActorType != "device" || l.Items[0].ActorID != r.device {
		t.Errorf("late audit actor %+v", l.Items[0])
	}

	// A resend is a duplicate: no second report, no second row.
	if out, err := r.s.AgentIngestRuns(ctx, r.device, []RunInput{{ID: rsQuiet, Kind: "restore", Trigger: "manual", StartedAt: start,
		FinishedAt: &fin, Outcome: "failed", Stats: []byte(`{"restore_result":"disk"}`)}}, r.now); err != nil || out[0].Status != IngestDuplicate {
		t.Fatalf("resend %+v %v", out, err)
	}
	if l, _ := r.s.ListAudit(ctx, ScopeAll(), AuditFilter{Action: "restore.late_report", ObjectID: rsQuiet}, Page{Limit: 5}); len(l.Items) != 1 {
		t.Fatalf("late audit rows after a resend: %d", len(l.Items))
	}
	if p := r.restoreProgress(t, rsQuiet); p.Late.Status != "delivered" {
		t.Fatalf("the first late report was replaced: %+v", p.Late)
	}
}

// A row the console expired before the agent fetched it is not one the sweep closed: a run record
// for it changes nothing and writes no late report (as before ADR 0032).
func TestR2A_LateReportOnlyForRowsTheSweepClosed(t *testing.T) {
	r := newSweepRig(t)
	ctx := context.Background()
	st := r.now.Add(-30 * time.Hour).UnixMilli()
	r.exec(t, `INSERT INTO restore_requests (id, device_id, snapshot_id, mode, overwrite, selection_count, status,
			finished_at, expires_at, created_at, updated_at)
		VALUES (?, ?, ?, 'device_path', 'never', 1, 'expired', ?, ?, ?, ?)`,
		rsExpired, r.device, strings.Repeat("ab", 32), st+24*3600*1000, st+24*3600*1000, st, st)
	fin := r.now
	if _, err := r.s.AgentIngestRuns(ctx, r.device, []RunInput{{ID: rsExpired, Kind: "restore", Trigger: "manual", StartedAt: r.now.Add(-time.Hour),
		FinishedAt: &fin, Outcome: "success", Stats: []byte(`{"files_restored":1}`)}}, r.now); err != nil {
		t.Fatal(err)
	}
	if s, _, _ := r.restoreState(t, rsExpired); s != "expired" {
		t.Fatalf("expired row changed: %s", s)
	}
	if l, _ := r.s.ListAudit(ctx, ScopeAll(), AuditFilter{Action: "restore.late_report"}, Page{Limit: 5}); len(l.Items) != 0 {
		t.Fatalf("late report for a row the sweep did not close: %+v", l.Items)
	}
}

// P22-R2a fix F1: a console back from a 30-hour outage closes no device restore until its agents
// have had DeviceRestoreReconnectGrace to reconnect; a device still silent then is closed, and one
// that heartbeats inside the grace stays open.
func TestR2A_NoSilentCloseInTheGraceAfterAConsoleOutage(t *testing.T) {
	for _, tc := range []struct {
		name       string
		heartbeats bool // the device heartbeats 30 minutes after the console is back
	}{{"still silent", false}, {"heartbeat inside the grace", true}} {
		t.Run(tc.name, func(t *testing.T) {
			r := newSweepRig(t)
			ctx := context.Background()
			r.runningRestore(t, rsQuiet, 31*time.Hour)        // fetched before the outage
			r.heartbeat(t, 30*time.Hour, 30*24*time.Hour, "") // its last heartbeat: when the console went down
			down := r.now.Add(-30 * time.Hour)                // the last sweep before the outage
			if err := r.s.RecordJobRun(ctx, abandonedRunSweeperJob, JobRun{Status: "ok", StartedAt: down, FinishedAt: down}); err != nil {
				t.Fatal(err)
			}
			sweep := func(when string, wantClosed bool) {
				t.Helper()
				res, err := r.s.SweepAbandonedRuns(ctx, SweepOptions{})
				if err != nil {
					t.Fatalf("%s: %v", when, err)
				}
				// The job runner records each run, as in production.
				if err := r.s.RecordJobRun(ctx, abandonedRunSweeperJob, JobRun{Status: "ok", StartedAt: r.now, FinishedAt: r.now}); err != nil {
					t.Fatal(err)
				}
				st, _, _ := r.restoreState(t, rsQuiet)
				if closed := st != "running"; closed != wantClosed || (wantClosed && res.RestoresClosedBy[RestoreClosedAgentSilent] != 1) {
					t.Fatalf("%s: status %s, result %+v (want closed %v)", when, st, res, wantClosed)
				}
			}
			r.now = r.now.Add(5 * time.Second) // the first sweep, five seconds after the console started
			sweep("first sweep", false)
			r.now = r.now.Add(5 * time.Minute)
			sweep("second sweep", false)
			if tc.heartbeats {
				r.now = r.now.Add(25 * time.Minute)
				r.heartbeat(t, 0, 30*24*time.Hour, "")
			}
			r.now = r.now.Add(DeviceRestoreReconnectGrace - 30*time.Minute - time.Minute)
			sweep("inside the grace", false)
			r.now = r.now.Add(32 * time.Minute) // the grace has passed
			sweep("after the grace", !tc.heartbeats)
		})
	}
}

// With no outage to see (the previous sweep five minutes ago), the 24-hour rule applies at once.
func TestR2A_NoGraceWithoutAnOutage(t *testing.T) {
	r := newSweepRig(t)
	ctx := context.Background()
	r.runningRestore(t, rsQuiet, 26*time.Hour)
	r.heartbeat(t, 25*time.Hour, 30*24*time.Hour, "")
	prev := r.now.Add(-5 * time.Minute)
	if err := r.s.RecordJobRun(ctx, abandonedRunSweeperJob, JobRun{Status: "ok", StartedAt: prev, FinishedAt: prev}); err != nil {
		t.Fatal(err)
	}
	if res, err := r.s.SweepAbandonedRuns(ctx, SweepOptions{}); err != nil || res.RestoresClosedBy[RestoreClosedAgentSilent] != 1 {
		t.Fatalf("sweep %+v %v", res, err)
	}
	// An explicit start (SweepOptions.ConsoleStartedAt) holds the grace as well.
	r2 := newSweepRig(t)
	r2.runningRestore(t, rsQuiet, 26*time.Hour)
	r2.heartbeat(t, 25*time.Hour, 30*24*time.Hour, "")
	if res, err := r2.s.SweepAbandonedRuns(ctx, SweepOptions{ConsoleStartedAt: r2.now.Add(-time.Hour)}); err != nil || res.RestoresClosed != 0 {
		t.Fatalf("closed inside an explicit grace: %+v %v", res, err)
	}
}
