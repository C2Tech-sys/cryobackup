package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/state"
	"github.com/c2tech-sys/cryobackup/internal/ulid"
)

// minEpochMS mirrors the schema's CHECK (>= 1e12): anything smaller is a
// seconds-epoch value or garbage, never a millisecond timestamp.
const minEpochMS = 1_000_000_000_000

// Run is one backup, restore or maintenance run. Agent runs (origin "agent")
// arrive through AgentIngestRuns; maintenance runs (origin "console") are
// written by the maintain worker. StartedAt and FinishedAt are SERVER time:
// the device's time corrected by its clock offset and clamped to ReceivedAt
// (DH5). A nil FinishedAt means the run is open (no outcome yet).
type Run struct {
	ID             string
	DeviceID       string
	Origin         string // agent | console
	Kind           string // backup | restore | forget | prune | check | unlock | maintenance
	Trigger        string // schedule | catchup | wake | boot | manual | retry
	StartedAt      time.Time
	FinishedAt     *time.Time
	ReceivedAt     time.Time
	ClockOffsetMS  int64
	Outcome        string // "" while open
	ResticExitCode *int
	SnapshotID     string
	Stats          json.RawMessage
	DataAddedBytes *int64
	ErrorCounts    map[string]int // {error class: n}; never paths or messages
	ErrorClass     string
	Attempt        int
	DeferredReason string
	PolicyVersion  *int
	AgentVersion   string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// RunInput is one run as the agent reports it. Times are DEVICE time; the store
// converts them. Nothing here carries a path: ErrorCounts holds class names and
// counts, Stats holds counters (see the stats keys below).
//
// Stats keys the facts loader reads (all optional; absent means unknown and a
// partial run with unknown counts is over tolerance, the engine's fail-loud
// rule): "total_files_processed" (number), "failed_files" (number),
// "vss_fallback_volumes" (array of drive letters).
type RunInput struct {
	ID             string // 26-char ULID, agent-generated: the idempotency key
	Kind           string
	Trigger        string
	StartedAt      time.Time  // device time
	FinishedAt     *time.Time // device time; nil for an open run
	Outcome        string     // "" for an open run
	ResticExitCode *int
	SnapshotID     string // 64 hex, backup runs only
	Stats          json.RawMessage
	DataAddedBytes *int64
	ErrorCounts    map[string]int
	ErrorClass     string
	Attempt        int // 0 = 1
	DeferredReason string
	PolicyVersion  int // 0 = unknown
	AgentVersion   string
}

// Ingest statuses, one per submitted run.
const (
	IngestCreated   = "created"
	IngestUpdated   = "updated"
	IngestDuplicate = "duplicate"
	IngestRejected  = "rejected"
)

// Reason codes for a rejected run. They are fixed strings, never free text.
const (
	ReasonWrongDevice              = "wrong_device"
	ReasonInvalidID                = "invalid_id"
	ReasonInvalidTimestamp         = "invalid_timestamp"
	ReasonFinishedBeforeStarted    = "finished_before_started"
	ReasonOutcomeFinishedMismatch  = "outcome_finished_mismatch"
	ReasonDeferredReasonRequired   = "deferred_reason_required"
	ReasonDeferredReasonUnexpected = "deferred_reason_unexpected"
	ReasonSnapshotOnNonBackup      = "snapshot_on_non_backup"
	ReasonInvalidStats             = "invalid_stats"
	ReasonInvalidValue             = "invalid_value"
)

// IngestResult is the verdict on one submitted run. Detail is set for
// ReasonInvalidValue only: SQLite's own constraint text (table, column and
// expression names; never row content).
type IngestResult struct {
	ID     string
	Status string
	Reason string
	Detail string
}

// runReason maps a CHECK violation message to a fixed reason code.
func runReason(msg string) string {
	switch {
	case strings.Contains(msg, ">= 1000000000000"):
		return ReasonInvalidTimestamp
	case strings.Contains(msg, "finished_at >= started_at"):
		return ReasonFinishedBeforeStarted
	case strings.Contains(msg, "outcome IS NULL) = (finished_at IS NULL"):
		return ReasonOutcomeFinishedMismatch
	case strings.Contains(msg, "outcome <> 'deferred'"):
		return ReasonDeferredReasonRequired
	case strings.Contains(msg, "deferred_reason IS NULL OR"):
		return ReasonDeferredReasonUnexpected
	case strings.Contains(msg, "snapshot_id IS NULL OR kind"):
		return ReasonSnapshotOnNonBackup
	}
	return ReasonInvalidValue
}

// validateRunInput applies the schema's couplings in Go first so the reason is
// exact and deterministic; the database CHECKs remain the backstop.
func validateRunInput(r *RunInput) string {
	if len(r.ID) != ulid.Len || !ulid.Valid(r.ID) {
		return ReasonInvalidID
	}
	if r.StartedAt.IsZero() || r.StartedAt.UnixMilli() < minEpochMS {
		return ReasonInvalidTimestamp
	}
	if r.FinishedAt != nil && r.FinishedAt.UnixMilli() < minEpochMS {
		return ReasonInvalidTimestamp
	}
	if (r.Outcome == "") != (r.FinishedAt == nil) {
		return ReasonOutcomeFinishedMismatch
	}
	if r.FinishedAt != nil && r.FinishedAt.Before(r.StartedAt) {
		return ReasonFinishedBeforeStarted
	}
	if r.Outcome == "deferred" && r.DeferredReason == "" {
		return ReasonDeferredReasonRequired
	}
	if r.DeferredReason != "" && r.Outcome != "deferred" {
		return ReasonDeferredReasonUnexpected
	}
	if r.SnapshotID != "" && r.Kind != "backup" {
		return ReasonSnapshotOnNonBackup
	}
	if len(r.Stats) > 0 {
		var obj map[string]json.RawMessage
		if json.Unmarshal(r.Stats, &obj) != nil || obj == nil {
			return ReasonInvalidStats
		}
	}
	return ""
}

// correct converts a device timestamp to server time (device minus offset) and
// clamps it to receivedAt (DH5): a device clock running ahead can never make a
// backup look fresher than the moment the console heard about it.
func correct(t time.Time, offsetMS, receivedMS int64) int64 {
	ms := t.UnixMilli() - offsetMS
	if ms > receivedMS {
		return receivedMS
	}
	return ms
}

// AgentIngestRuns stores a batch of run results from the authenticated device.
//
// Idempotency (the run id is the key, architecture §5.1 and §3.4):
//   - unknown id: created.
//   - known id, same device, stored run open and incoming run finished: updated
//     (an open run may be sent at start and closed later). The same holds when
//     the stored outcome is "abandoned" (set by the sweeper) and the incoming
//     outcome is real: the late report overwrites abandoned.
//   - known id, same device, anything else (a resend, a stale open record after
//     the run closed): duplicate; nothing changes.
//   - known id owned by another device, or a console-origin run of this device:
//     rejected (wrong_device), and nothing about the other row is revealed.
//   - a run that violates the schema's rules (seconds-epoch timestamps, an
//     outcome without finished_at, a deferred run without a reason, a snapshot
//     on a non-backup run, malformed id or stats, an unknown enum value):
//     rejected with a fixed reason code. One bad run never fails the batch.
//
// Time (DH5): started_at and finished_at are stored as device time minus the
// device's current clock offset (device_presence.clock_offset_ms, 0 when it has
// never reported), clamped to receivedAt; the offset used is kept in
// runs.clock_offset_ms. A closing update never finishes before the stored start.
//
// Start lease (ADR 0005): every closed record of this device that is not
// rejected (created, updated or duplicate) releases the run's start lease, in
// the same transaction; an open record leaves it held.
//
// A database error that is not a per-run constraint (busy, cancelled context)
// aborts the whole batch and is returned; the agent resends, which is safe.
func (s *Store) AgentIngestRuns(ctx context.Context, deviceID string, runs []RunInput, receivedAt time.Time) ([]IngestResult, error) {
	if receivedAt.IsZero() {
		return nil, invalidf("receivedAt is required")
	}
	recv := receivedAt.UnixMilli()
	if recv < minEpochMS {
		return nil, invalidf("receivedAt is not a millisecond timestamp")
	}
	results := make([]IngestResult, len(runs))
	err := s.Write(ctx, func(tx *Tx) error {
		var exists int
		if err := tx.QueryRow(`SELECT 1 FROM devices WHERE id = ?`, deviceID).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		var off sql.NullInt64
		if err := tx.QueryRow(`SELECT clock_offset_ms FROM device_presence WHERE device_id = ?`, deviceID).Scan(&off); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		for i := range runs {
			res, err := ingestOne(tx, deviceID, runs[i], recv, off.Int64)
			if err != nil {
				return err
			}
			results[i] = res
			// The closed record releases the run's start lease (ADR 0005); the
			// open record sent at start does not.
			if runs[i].FinishedAt != nil && res.Status != IngestRejected {
				if err := releaseStartLease(tx, deviceID, runs[i].ID); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return results, nil
}

func ingestOne(tx *Tx, deviceID string, r RunInput, recv, offset int64) (IngestResult, error) {
	res := IngestResult{ID: r.ID}
	reject := func(reason, detail string) (IngestResult, error) {
		res.Status, res.Reason, res.Detail = IngestRejected, reason, detail
		return res, nil
	}
	if reason := validateRunInput(&r); reason != "" {
		return reject(reason, "")
	}
	attempt := r.Attempt
	if attempt == 0 {
		attempt = 1
	}
	stats := "{}"
	if len(r.Stats) > 0 {
		stats = string(r.Stats)
	}
	counts, err := json.Marshal(nonNilCounts(r.ErrorCounts))
	if err != nil {
		return reject(ReasonInvalidValue, "error_counts")
	}
	var policyVer any
	if r.PolicyVersion > 0 {
		policyVer = r.PolicyVersion
	}

	started := correct(r.StartedAt, offset, recv)
	var finished any
	var finishedMS int64
	if r.FinishedAt != nil {
		finishedMS = correct(*r.FinishedAt, offset, recv)
	}

	var owner, origin string
	var storedOutcome sql.NullString
	var storedStart int64
	err = tx.QueryRow(`SELECT device_id, origin, outcome, started_at FROM runs WHERE id = ?`, r.ID).Scan(&owner, &origin, &storedOutcome, &storedStart)
	exists := true
	if errors.Is(err, sql.ErrNoRows) {
		exists = false
	} else if err != nil {
		return res, err
	}

	if !exists {
		if r.FinishedAt != nil {
			if finishedMS < started {
				finishedMS = started
			}
			finished = finishedMS
		}
		if _, err := tx.Exec(`SAVEPOINT ingest_run`); err != nil {
			return res, err
		}
		_, err := tx.Exec(`INSERT INTO runs (id, device_id, origin, kind, trigger_kind, started_at, finished_at, received_at,
				clock_offset_ms, outcome, restic_exit_code, snapshot_id, stats, data_added_bytes, error_counts, error_class,
				attempt, deferred_reason, policy_version, agent_version, created_at, updated_at)
			VALUES (?,?,'agent',?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			r.ID, deviceID, r.Kind, r.Trigger, started, finished, recv, offset, nullStr(r.Outcome), nullInt(r.ResticExitCode),
			nullStr(r.SnapshotID), stats, nullInt64(r.DataAddedBytes), string(counts), nullStr(r.ErrorClass), attempt,
			nullStr(r.DeferredReason), policyVer, nullStr(r.AgentVersion), tx.nowMS(), tx.nowMS())
		return finishSavepoint(tx, res, err, IngestCreated)
	}

	// A console-origin run (maintenance the console started) is never the agent's
	// to close or reopen, even on the same device: same reason, nothing revealed.
	if owner != deviceID || origin != "agent" {
		return reject(ReasonWrongDevice, "")
	}
	reopensAbandoned := storedOutcome.Valid && storedOutcome.String == "abandoned" && r.Outcome != "" && r.Outcome != "abandoned"
	closes := !storedOutcome.Valid && r.FinishedAt != nil
	if !(closes || reopensAbandoned) {
		res.Status = IngestDuplicate
		return res, nil
	}
	if finishedMS < storedStart {
		finishedMS = storedStart
	}
	if _, err := tx.Exec(`SAVEPOINT ingest_run`); err != nil {
		return res, err
	}
	_, err = tx.Exec(`UPDATE runs SET finished_at = ?, outcome = ?, restic_exit_code = ?, snapshot_id = ?, stats = ?,
			data_added_bytes = ?, error_counts = ?, error_class = ?, attempt = ?, deferred_reason = ?,
			policy_version = COALESCE(?, policy_version), agent_version = COALESCE(?, agent_version),
			received_at = ?, clock_offset_ms = ?, updated_at = ? WHERE id = ? AND origin = 'agent'`,
		finishedMS, r.Outcome, nullInt(r.ResticExitCode), nullStr(r.SnapshotID), stats, nullInt64(r.DataAddedBytes),
		string(counts), nullStr(r.ErrorClass), attempt, nullStr(r.DeferredReason), policyVer, nullStr(r.AgentVersion),
		recv, offset, tx.nowMS(), r.ID)
	return finishSavepoint(tx, res, err, IngestUpdated)
}

// finishSavepoint releases the savepoint on success and rolls back to it on a
// constraint violation, turning that into a rejection; any other error aborts.
func finishSavepoint(tx *Tx, res IngestResult, execErr error, okStatus string) (IngestResult, error) {
	if execErr == nil {
		if _, err := tx.Exec(`RELEASE ingest_run`); err != nil {
			return res, err
		}
		res.Status = okStatus
		return res, nil
	}
	mapped := mapErr(execErr)
	var ce *ConstraintError
	if !errors.As(mapped, &ce) {
		return res, execErr
	}
	if _, err := tx.Exec(`ROLLBACK TO ingest_run`); err != nil {
		return res, err
	}
	if _, err := tx.Exec(`RELEASE ingest_run`); err != nil {
		return res, err
	}
	res.Status, res.Reason = IngestRejected, runReason(ce.Message)
	if res.Reason == ReasonInvalidValue {
		res.Detail = ce.Message
	}
	return res, nil
}

func nonNilCounts(m map[string]int) map[string]int {
	if m == nil {
		return map[string]int{}
	}
	return m
}

const runCols = `id, device_id, origin, kind, trigger_kind, started_at, finished_at, received_at, clock_offset_ms, outcome,
	restic_exit_code, snapshot_id, stats, data_added_bytes, error_counts, error_class, attempt, deferred_reason,
	policy_version, agent_version, created_at, updated_at`

func scanRun(sc scanner) (Run, error) {
	var r Run
	var fin, exit, added, polVer sql.NullInt64
	var outcome, snap, errClass, deferred, av sql.NullString
	var stats, counts string
	var started, recv, c, u int64
	err := sc.Scan(&r.ID, &r.DeviceID, &r.Origin, &r.Kind, &r.Trigger, &started, &fin, &recv, &r.ClockOffsetMS, &outcome,
		&exit, &snap, &stats, &added, &counts, &errClass, &r.Attempt, &deferred, &polVer, &av, &c, &u)
	if err != nil {
		return Run{}, err
	}
	r.StartedAt, r.FinishedAt, r.ReceivedAt = timeOf(started), ptrTime(fin), timeOf(recv)
	r.Outcome, r.ResticExitCode, r.SnapshotID = outcome.String, ptrInt(exit), snap.String
	r.Stats, r.DataAddedBytes = json.RawMessage(stats), ptrInt64(added)
	r.ErrorClass, r.DeferredReason, r.PolicyVersion, r.AgentVersion = errClass.String, deferred.String, ptrInt(polVer), av.String
	r.CreatedAt, r.UpdatedAt = timeOf(c), timeOf(u)
	if err := json.Unmarshal([]byte(counts), &r.ErrorCounts); err != nil {
		return Run{}, invalidf("run %s has malformed error_counts", r.ID)
	}
	return r, nil
}

var qualifiedRunCols = prefixCols(runCols, "r.")

// GetRun returns a run whose device lies inside scope (ErrNotFound otherwise).
func (s *Store) GetRun(ctx context.Context, scope Scope, id string) (Run, error) {
	if err := scope.check(); err != nil {
		return Run{}, err
	}
	clause, args := scope.clause("d.customer_id")
	var out Run
	err := s.Read(ctx, func(tx *Tx) error {
		var err error
		out, err = one(tx, `SELECT `+qualifiedRunCols+` FROM runs r JOIN devices d ON d.id = r.device_id
			WHERE r.id = ? AND `+clause, append([]any{id}, args...), scanRun)
		return err
	})
	return out, err
}

// HourCount is the finished agent backup runs of one device in one UTC hour,
// split by what the fleet state machine makes of their outcome: Success counts
// success and partial-within-tolerance, Failure counts failed, abandoned and any
// outcome the console does not know. Neutral outcomes (cancelled, skipped,
// interrupted, deferred) are not counted.
type HourCount struct {
	DeviceID string
	Hour     time.Time // start of the UTC hour
	Success  int
	Failure  int
}

// TenDayCounts returns per-device, per-UTC-hour counts of finished agent backup
// runs with from <= finished_at < to (a zero from or to leaves that end open),
// ordered by device and hour; it is what the ten-day strip is bucketed from.
// deviceID "" covers every non-retired device inside scope; a deviceID outside
// scope is ErrNotFound.
func (s *Store) TenDayCounts(ctx context.Context, scope Scope, deviceID string, from, to time.Time) ([]HourCount, error) {
	if err := scope.check(); err != nil {
		return nil, err
	}
	clause, args := scope.clause("d.customer_id")
	q := `SELECT r.device_id, r.finished_at / 3600000, r.outcome, count(*) FROM runs r JOIN devices d ON d.id = r.device_id
		WHERE r.kind = 'backup' AND r.origin = 'agent' AND r.outcome IS NOT NULL AND ` + clause
	if deviceID != "" {
		q, args = q+` AND r.device_id = ?`, append(args, deviceID)
	} else {
		q += ` AND d.retired_at IS NULL`
	}
	if !from.IsZero() {
		q, args = q+` AND r.finished_at >= ?`, append(args, msOf(from))
	}
	if !to.IsZero() {
		q, args = q+` AND r.finished_at < ?`, append(args, msOf(to))
	}
	q += ` GROUP BY r.device_id, r.finished_at / 3600000, r.outcome ORDER BY r.device_id, r.finished_at / 3600000`
	type key struct {
		dev string
		hr  int64
	}
	var order []key
	sums := map[key]*HourCount{}
	err := s.Read(ctx, func(tx *Tx) error {
		if deviceID != "" {
			if _, err := scopedDevice(tx, scope, deviceID); err != nil {
				return err
			}
		}
		rows, err := tx.Query(q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var dev, outcome string
			var hr int64
			var n int
			if err := rows.Scan(&dev, &hr, &outcome, &n); err != nil {
				return err
			}
			k := key{dev, hr}
			c := sums[k]
			if c == nil {
				c = &HourCount{DeviceID: dev, Hour: time.UnixMilli(hr * 3600000).UTC()}
				sums[k] = c
				order = append(order, k)
			}
			switch state.OutcomeClass(outcome, true) {
			case state.ClassSuccess:
				c.Success += n
			case state.ClassFailure:
				c.Failure += n
			}
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	out := make([]HourCount, 0, len(order))
	for _, k := range order {
		out = append(out, *sums[k])
	}
	return out, nil
}

// ListRuns returns runs newest first (by start). deviceID "" lists across every
// device inside scope; a deviceID outside scope is ErrNotFound.
func (s *Store) ListRuns(ctx context.Context, scope Scope, deviceID string, page Page) (Listing[Run], error) {
	if err := scope.check(); err != nil {
		return Listing[Run]{}, err
	}
	limit, offset, err := page.window()
	if err != nil {
		return Listing[Run]{}, err
	}
	clause, args := scope.clause("d.customer_id")
	q := `SELECT ` + qualifiedRunCols + ` FROM runs r JOIN devices d ON d.id = r.device_id WHERE ` + clause
	if deviceID != "" {
		q, args = q+` AND r.device_id = ?`, append(args, deviceID)
	}
	q += ` ORDER BY r.started_at DESC, r.id DESC LIMIT ? OFFSET ?`
	var items []Run
	err = s.Read(ctx, func(tx *Tx) error {
		if deviceID != "" {
			if _, err := scopedDevice(tx, scope, deviceID); err != nil {
				return err
			}
		}
		var err error
		items, err = collect(tx, q, append(args, limit+1, offset), scanRun)
		return err
	})
	if err != nil {
		return Listing[Run]{}, err
	}
	return finish(items, limit, offset), nil
}

// ConsoleRunInput starts a console-originated run (a maintenance operation).
type ConsoleRunInput struct {
	DeviceID string
	Kind     string // forget | prune | check | unlock | maintenance | restore
	Trigger  string // "" = schedule
}

// CreateConsoleRun opens a run with origin "console" starting now. The
// maintain worker passes ScopeAll; the scope check exists so no code path
// reaches a device without one.
func (s *Store) CreateConsoleRun(ctx context.Context, scope Scope, in ConsoleRunInput) (Run, error) {
	if err := scope.check(); err != nil {
		return Run{}, err
	}
	if in.Trigger == "" {
		in.Trigger = "schedule"
	}
	var out Run
	err := s.Write(ctx, func(tx *Tx) error {
		if _, err := scopedDevice(tx, scope, in.DeviceID); err != nil {
			return err
		}
		id, now := tx.id(), tx.nowMS()
		if _, err := tx.Exec(`INSERT INTO runs (id, device_id, origin, kind, trigger_kind, started_at, received_at, created_at, updated_at)
			VALUES (?,?,'console',?,?,?,?,?,?)`, id, in.DeviceID, in.Kind, in.Trigger, now, now, now, now); err != nil {
			return err
		}
		var err error
		out, err = one(tx, `SELECT `+runCols+` FROM runs WHERE id = ?`, []any{id}, scanRun)
		return err
	})
	return out, err
}

// ConsoleRunResult closes a console run.
type ConsoleRunResult struct {
	Outcome        string // success | partial | failed | cancelled | skipped | deferred | interrupted | abandoned
	ResticExitCode *int
	Stats          json.RawMessage
	ErrorCounts    map[string]int
	ErrorClass     string
	DeferredReason string
	DataAddedBytes *int64
}

// FinishConsoleRun closes an open console run at the store clock's now. Closing
// an agent run, or one already closed, is ErrConflict.
func (s *Store) FinishConsoleRun(ctx context.Context, scope Scope, runID string, f ConsoleRunResult) (Run, error) {
	if err := scope.check(); err != nil {
		return Run{}, err
	}
	if f.Outcome == "" {
		return Run{}, invalidf("outcome is required")
	}
	stats, err := jsonOr(f.Stats, "{}")
	if err != nil {
		return Run{}, err
	}
	counts, err := json.Marshal(nonNilCounts(f.ErrorCounts))
	if err != nil {
		return Run{}, err
	}
	var out Run
	err = s.Write(ctx, func(tx *Tx) error {
		clause, cargs := scope.clause("d.customer_id")
		cur, err := one(tx, `SELECT `+qualifiedRunCols+` FROM runs r JOIN devices d ON d.id = r.device_id
			WHERE r.id = ? AND `+clause, append([]any{runID}, cargs...), scanRun)
		if err != nil {
			return err
		}
		if cur.Origin != "console" || cur.FinishedAt != nil {
			return conflictf("run %s is not an open console run", runID)
		}
		now := tx.nowMS()
		fin := now
		if fin < cur.StartedAt.UnixMilli() {
			fin = cur.StartedAt.UnixMilli()
		}
		if _, err := tx.Exec(`UPDATE runs SET finished_at = ?, outcome = ?, restic_exit_code = ?, stats = ?, error_counts = ?,
				error_class = ?, deferred_reason = ?, data_added_bytes = ?, received_at = ?, updated_at = ? WHERE id = ?`,
			fin, f.Outcome, nullInt(f.ResticExitCode), stats, string(counts), nullStr(f.ErrorClass), nullStr(f.DeferredReason),
			nullInt64(f.DataAddedBytes), now, now, runID); err != nil {
			return err
		}
		out, err = one(tx, `SELECT `+runCols+` FROM runs WHERE id = ?`, []any{runID}, scanRun)
		return err
	})
	return out, err
}

// ---------------------------------------------------------------------------
// snapshots
// ---------------------------------------------------------------------------

// Snapshot is a cache row of `restic snapshots --json`; the repository is
// authoritative. Paths are the backup ROOTS passed to restic (policy-defined),
// never file-level paths.
type Snapshot struct {
	DeviceID       string
	SnapshotID     string // 64 hex
	TakenAt        time.Time
	ParentID       string
	Hostname       string
	Paths          []string
	Tags           []string
	ProgramVersion string
	Summary        json.RawMessage
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

const snapCols = `device_id, snapshot_id, taken_at, parent_id, hostname, paths, tags, program_version, summary, created_at, updated_at`

func scanSnapshot(sc scanner) (Snapshot, error) {
	var x Snapshot
	var parent, host, prog sql.NullString
	var paths, tags, summary string
	var taken, c, u int64
	if err := sc.Scan(&x.DeviceID, &x.SnapshotID, &taken, &parent, &host, &paths, &tags, &prog, &summary, &c, &u); err != nil {
		return Snapshot{}, err
	}
	x.TakenAt, x.ParentID, x.Hostname, x.ProgramVersion = timeOf(taken), parent.String, host.String, prog.String
	x.Summary, x.CreatedAt, x.UpdatedAt = json.RawMessage(summary), timeOf(c), timeOf(u)
	if err := json.Unmarshal([]byte(paths), &x.Paths); err != nil {
		return Snapshot{}, invalidf("snapshot %s has malformed paths", x.SnapshotID)
	}
	if err := json.Unmarshal([]byte(tags), &x.Tags); err != nil {
		return Snapshot{}, invalidf("snapshot %s has malformed tags", x.SnapshotID)
	}
	return x, nil
}

// AgentReplaceSnapshots makes the device's snapshot cache equal to list, in one
// transaction: present snapshots are upserted, rows no longer present are
// deleted (forget pruned them). It returns how many were upserted and removed.
// An invalid snapshot (id not 64 hex, taken_at not milliseconds) fails the whole
// call with ErrInvalid and changes nothing.
func (s *Store) AgentReplaceSnapshots(ctx context.Context, deviceID string, list []Snapshot) (upserted, removed int, err error) {
	err = s.Write(ctx, func(tx *Tx) error {
		var exists int
		if err := tx.QueryRow(`SELECT 1 FROM devices WHERE id = ?`, deviceID).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		now := tx.nowMS()
		keep := make(map[string]bool, len(list))
		for _, x := range list {
			paths, err := json.Marshal(nonNilStrings(x.Paths))
			if err != nil {
				return err
			}
			tags, err := json.Marshal(nonNilStrings(x.Tags))
			if err != nil {
				return err
			}
			summary, err := jsonOr(x.Summary, "{}")
			if err != nil {
				return err
			}
			if _, err := tx.Exec(`INSERT INTO snapshots (device_id, snapshot_id, taken_at, parent_id, hostname, paths, tags,
					program_version, summary, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)
				ON CONFLICT(device_id, snapshot_id) DO UPDATE SET taken_at = excluded.taken_at, parent_id = excluded.parent_id,
					hostname = excluded.hostname, paths = excluded.paths, tags = excluded.tags,
					program_version = excluded.program_version, summary = excluded.summary, updated_at = excluded.updated_at`,
				deviceID, x.SnapshotID, msOf(x.TakenAt), nullStr(x.ParentID), nullStr(x.Hostname), string(paths), string(tags),
				nullStr(x.ProgramVersion), summary, now, now); err != nil {
				return err
			}
			keep[x.SnapshotID] = true
			upserted++
		}
		rows, err := collect(tx, `SELECT snapshot_id FROM snapshots WHERE device_id = ?`, []any{deviceID},
			func(sc scanner) (string, error) {
				var id string
				return id, sc.Scan(&id)
			})
		if err != nil {
			return err
		}
		for _, id := range rows {
			if keep[id] {
				continue
			}
			if _, err := tx.Exec(`DELETE FROM snapshots WHERE device_id = ? AND snapshot_id = ?`, deviceID, id); err != nil {
				return err
			}
			removed++
		}
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	return upserted, removed, nil
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// ListSnapshots returns a device's cached snapshots, newest first.
func (s *Store) ListSnapshots(ctx context.Context, scope Scope, deviceID string) ([]Snapshot, error) {
	if err := scope.check(); err != nil {
		return nil, err
	}
	var out []Snapshot
	err := s.Read(ctx, func(tx *Tx) error {
		if _, err := scopedDevice(tx, scope, deviceID); err != nil {
			return err
		}
		var err error
		out, err = collect(tx, `SELECT `+snapCols+` FROM snapshots WHERE device_id = ? ORDER BY taken_at DESC, snapshot_id`,
			[]any{deviceID}, scanSnapshot)
		return err
	})
	return out, err
}
