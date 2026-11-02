package journal

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// Run states stored in runs.state (architecture §6.2). The console only ever
// sees the outcome vocabulary; the state is local.
const (
	StateStarting = "starting"
	StateRunning  = "running"
	StateDone     = "done"
)

// RunRow is a journaled run: the wire record plus the local-only columns.
type RunRow struct {
	api.RunRecord
	State        string
	DeliveredAt  *time.Time
	RejectReason string
	UpdatedAt    time.Time
}

// StartRun writes the run row before the child process starts (§6.2) and queues
// the open record for upload. rec must be open (no outcome, no finish time).
// The record is checked with api.AssertPathFree before it is queued.
func (j *Journal) StartRun(ctx context.Context, rec api.RunRecord) error {
	if rec.Outcome != "" || rec.FinishedAt != nil {
		return errors.New("journal: StartRun needs an open record")
	}
	body, err := encodeBody(rec)
	if err != nil {
		return err
	}
	return j.tx(ctx, func(tx *sql.Tx) error {
		now := j.now()
		if _, err := tx.ExecContext(ctx, `INSERT INTO runs
			(id, kind, trigger, state, started_at, attempt, policy_version, initial, agent_version, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			rec.ID, string(rec.Kind), string(rec.Trigger), StateStarting, ms(rec.StartedAt),
			rec.Attempt, rec.PolicyVersion, boolInt(rec.Initial), rec.AgentVersion, now); err != nil {
			return err
		}
		return enqueue(ctx, tx, rec.ID, body, false, now)
	})
}

// MarkRunning moves a run from starting to running (restic is spawned and in
// the job object).
func (j *Journal) MarkRunning(ctx context.Context, id string) error {
	_, err := j.db.ExecContext(ctx, `UPDATE runs SET state = ?, updated_at = ? WHERE id = ? AND finished_at IS NULL`,
		StateRunning, j.now(), id)
	return err
}

// Touch records journal activity for an open run. "abandoned" runs are closed
// with the time of the last touch.
func (j *Journal) Touch(ctx context.Context, id string) error {
	_, err := j.db.ExecContext(ctx, `UPDATE runs SET updated_at = ? WHERE id = ? AND finished_at IS NULL`, j.now(), id)
	return err
}

// TouchProgress is Touch that also stores st, the run's last progress values
// (files and bytes done, elapsed time), as the open row's stats: a run the
// next process closes with OpenRunsOnStart (a reboot or a crash killed it)
// then reports how far it got instead of zeros (L-STATS-1). FinishRun
// replaces them with the final stats. st holds counts only, never a path.
func (j *Journal) TouchProgress(ctx context.Context, id string, st api.RunStats) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	_, err = j.db.ExecContext(ctx, `UPDATE runs SET updated_at = ?, stats = ? WHERE id = ? AND finished_at IS NULL`, j.now(), string(b), id)
	return err
}

// FinishRun closes a run and queues the closed record, replacing the open one
// if it is still waiting in the outbox. Both happen in one transaction.
func (j *Journal) FinishRun(ctx context.Context, rec api.RunRecord) error {
	if rec.Outcome == "" || rec.FinishedAt == nil {
		return errors.New("journal: FinishRun needs a closed record")
	}
	body, err := encodeBody(rec)
	if err != nil {
		return err
	}
	stats, err := json.Marshal(rec.Stats)
	if err != nil {
		return err
	}
	counts, err := json.Marshal(nonNilCounts(rec.ErrorCounts))
	if err != nil {
		return err
	}
	return j.tx(ctx, func(tx *sql.Tx) error {
		now := j.now()
		var exit any
		if rec.ExitCode != nil {
			exit = *rec.ExitCode
		}
		res, err := tx.ExecContext(ctx, `UPDATE runs SET state = ?, finished_at = ?, outcome = ?, exit_code = ?,
			snapshot_id = ?, stats = ?, error_counts = ?, error_class = ?, attempt = ?, deferred_reason = ?,
			policy_version = ?, initial = ?, updated_at = ?
			WHERE id = ? AND finished_at IS NULL`,
			StateDone, ms(*rec.FinishedAt), string(rec.Outcome), exit,
			rec.SnapshotID, string(stats), string(counts), string(rec.ErrorClass), rec.Attempt, string(rec.DeferredReason),
			rec.PolicyVersion, boolInt(rec.Initial), now, rec.ID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return fmt.Errorf("journal: run is not open")
		}
		return enqueue(ctx, tx, rec.ID, body, true, now)
	})
}

// InsertFinished journals a run that never ran a child (a skipped run, a
// refused start): the row is created closed and queued in one step.
func (j *Journal) InsertFinished(ctx context.Context, rec api.RunRecord) error {
	body, err := encodeBody(rec)
	if err != nil {
		return err
	}
	if rec.Outcome == "" || rec.FinishedAt == nil {
		return errors.New("journal: InsertFinished needs a closed record")
	}
	stats, _ := json.Marshal(rec.Stats)
	counts, _ := json.Marshal(nonNilCounts(rec.ErrorCounts))
	return j.tx(ctx, func(tx *sql.Tx) error {
		now := j.now()
		var exit any
		if rec.ExitCode != nil {
			exit = *rec.ExitCode
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO runs
			(id, kind, trigger, state, started_at, finished_at, outcome, exit_code, snapshot_id, stats, error_counts,
			 error_class, attempt, deferred_reason, policy_version, initial, agent_version, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			rec.ID, string(rec.Kind), string(rec.Trigger), StateDone, ms(rec.StartedAt), ms(*rec.FinishedAt),
			string(rec.Outcome), exit, rec.SnapshotID, string(stats), string(counts),
			string(rec.ErrorClass), rec.Attempt, string(rec.DeferredReason), rec.PolicyVersion,
			boolInt(rec.Initial), rec.AgentVersion, now); err != nil {
			return err
		}
		return enqueue(ctx, tx, rec.ID, body, true, now)
	})
}

// OpenRunsOnStart closes rows left open by a previous process: as interrupted
// when the previous process recorded a stop (interrupted is true), otherwise as
// abandoned (§6.2: "no record of why"). The finish time is the last journal
// activity of the row, never earlier than its start. The closed records are
// queued for upload and returned.
func (j *Journal) OpenRunsOnStart(ctx context.Context, interrupted bool, agentVersion string) ([]api.RunRecord, error) {
	outcome := restic.OutcomeAbandoned
	if interrupted {
		outcome = restic.OutcomeInterrupted
	}
	rows, err := j.openRows(ctx)
	if err != nil {
		return nil, err
	}
	var closed []api.RunRecord
	for _, r := range rows {
		finished := r.UpdatedAt
		if finished.Before(r.StartedAt) {
			finished = r.StartedAt
		}
		rec := r.RunRecord
		rec.Outcome = outcome
		rec.FinishedAt = &finished
		if rec.AgentVersion == "" {
			rec.AgentVersion = agentVersion
		}
		if err := j.FinishRun(ctx, rec); err != nil {
			return closed, err
		}
		closed = append(closed, rec)
	}
	return closed, nil
}

func (j *Journal) openRows(ctx context.Context) ([]RunRow, error) {
	return j.queryRuns(ctx, `SELECT `+runColumns+` FROM runs WHERE finished_at IS NULL ORDER BY started_at`)
}

// OpenRunCount is the number of runs without an outcome (heartbeat journal_depth).
func (j *Journal) OpenRunCount(ctx context.Context) (int, error) {
	var n int
	err := j.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs WHERE finished_at IS NULL`).Scan(&n)
	return n, err
}

// Run returns one run by id.
func (j *Journal) Run(ctx context.Context, id string) (RunRow, bool, error) {
	rows, err := j.queryRuns(ctx, `SELECT `+runColumns+` FROM runs WHERE id = ?`, id)
	if err != nil || len(rows) == 0 {
		return RunRow{}, false, err
	}
	return rows[0], true, nil
}

// RecentRuns returns up to limit runs of a kind, newest start first.
func (j *Journal) RecentRuns(ctx context.Context, kind restic.Kind, limit int) ([]RunRow, error) {
	if limit <= 0 {
		limit = 50
	}
	return j.queryRuns(ctx, `SELECT `+runColumns+` FROM runs WHERE kind = ? ORDER BY started_at DESC, id DESC LIMIT ?`,
		string(kind), limit)
}

// HasSnapshot reports whether any backup run ever produced a snapshot: false
// means the next backup is the device's first (RunRecord.Initial).
func (j *Journal) HasSnapshot(ctx context.Context) (bool, error) {
	var n int
	err := j.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs WHERE kind = ? AND snapshot_id != ''`,
		string(restic.KindBackup)).Scan(&n)
	return n > 0, err
}

const runColumns = `id, kind, trigger, state, started_at, finished_at, outcome, exit_code, snapshot_id, stats,
	error_counts, error_class, attempt, deferred_reason, policy_version, initial, agent_version,
	delivered_at, reject_reason, updated_at`

func (j *Journal) queryRuns(ctx context.Context, q string, args ...any) ([]RunRow, error) {
	rows, err := j.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RunRow
	for rows.Next() {
		var (
			r                         RunRow
			kind, trig, state         string
			started                   int64
			finished, delivered       sql.NullInt64
			exit                      sql.NullInt64
			outcome, snap, stats      string
			counts, class, deferred   string
			attempt, policyV, initial int
			agent, reject             string
			updated                   int64
		)
		if err := rows.Scan(&r.ID, &kind, &trig, &state, &started, &finished, &outcome, &exit, &snap, &stats,
			&counts, &class, &attempt, &deferred, &policyV, &initial, &agent, &delivered, &reject, &updated); err != nil {
			return nil, err
		}
		r.Kind = restic.Kind(kind)
		r.Trigger = api.Trigger(trig)
		r.State = state
		r.StartedAt = fromMS(started)
		if finished.Valid {
			t := fromMS(finished.Int64)
			r.FinishedAt = &t
		}
		r.Outcome = restic.Outcome(outcome)
		if exit.Valid {
			v := int(exit.Int64)
			r.ExitCode = &v
		}
		r.SnapshotID = snap
		_ = json.Unmarshal([]byte(stats), &r.Stats)
		_ = json.Unmarshal([]byte(counts), &r.ErrorCounts)
		r.ErrorClass = restic.ErrorClass(class)
		r.Attempt = attempt
		r.DeferredReason = restic.DeferredReason(deferred)
		r.PolicyVersion = policyV
		r.Initial = initial != 0
		r.AgentVersion = agent
		if delivered.Valid {
			t := fromMS(delivered.Int64)
			r.DeliveredAt = &t
		}
		r.RejectReason = reject
		r.UpdatedAt = fromMS(updated)
		out = append(out, r)
	}
	return out, rows.Err()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nonNilCounts(c []restic.ErrorProjection) []restic.ErrorProjection {
	if c == nil {
		return []restic.ErrorProjection{}
	}
	return c
}

// encodeBody renders the outbox body of a record, refusing anything the
// path-free guard flags (architecture §8.4 item 8).
func encodeBody(rec api.RunRecord) (string, error) {
	if err := api.AssertPathFree(rec); err != nil {
		return "", fmt.Errorf("journal: run record is not path-free: %w", err)
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
