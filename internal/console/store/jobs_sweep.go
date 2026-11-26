package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/state"
)

// SweepOptions tune SweepAbandonedRuns. Zero values take the defaults below.
type SweepOptions struct {
	// Slack is added to the run's max runtime (the effective policy's sla.max_run_hours) before an open
	// run may be called abandoned.
	Slack time.Duration
	// ActiveWithin is how recent a heartbeat must be for its progress record to count as "the device
	// reports this run active".
	ActiveWithin time.Duration
	// Limit bounds the open runs examined in one pass.
	Limit int
	// ConsoleStartedAt is when this console process started (serve sets it
	// once at start-up); no device restore is closed before ConsoleStartedAt
	// plus DeviceRestoreReconnectGrace. Zero: the sweep works it out itself
	// (restoreSweepListeningSince).
	ConsoleStartedAt time.Time
}

// Defaults of SweepOptions (architecture §5.7 abandoned-run-sweeper).
const (
	DefaultSweepSlack        = time.Hour
	DefaultSweepActiveWithin = 10 * time.Minute
	DefaultSweepLimit        = 1000
)

// SweepResult is what one sweep did, in counts only.
type SweepResult struct {
	// Examined is the number of open agent runs looked at; Abandoned how many were closed.
	Examined, Abandoned int
	// RestoresClosed is how many running device restores the sweep closed as
	// FAILED interrupted (sweepStuckDeviceRestores), and RestoresClosedBy the same
	// count by reason (agent_silent, agent_restarted, no_run_record); nil when
	// none closed.
	RestoresClosed   int
	RestoresClosedBy map[string]int
	// RestoresHeldBack is how many running device restores the sweep would
	// have closed but did not, because the console is inside its reconnect
	// grace (ADR 0032); GraceUntil is when that grace ends (zero outside it).
	RestoresHeldBack int
	GraceUntil       time.Time
}

// Running device restores that stop reporting (ADR 0027; ticket P22-E, fix E3;
// ADR 0032).
//
// A device restore becomes running when the agent fetches its selection, and
// only the agent's run record (run id = restore id) closes it. An agent that
// never sends one would leave the row running and, through the one active device
// restore rule, block every later device restore of that device for good: the
// quiet failure this product exists to prevent. So the abandoned-run-sweeper
// also closes such a row, as FAILED with class interrupted, audited
// restore.finish with the reason, and keeps the reason in the row's progress
// document (DeviceRestoreProgress.ClosedBy), in three cases:
//
//   - agent_silent: nothing from the device (no heartbeat) for
//     DeviceRestoreSilenceLimit, counted from the later of the fetch and its
//     newest heartbeat.
//   - agent_restarted: the agent restarted after the fetch (its newest
//     heartbeat's uptime puts the boot after the fetch) and,
//     deviceRestoreRestartGrace after that boot, no run record of the restore
//     has arrived at all. A restore never resumes after a restart (ADR 0027),
//     and the agent reports an interrupted run from the journal it wrote before
//     restic started; a restart with no record at all means the agent never got
//     that far.
//   - no_run_record: DeviceRestoreRunningBound after the fetch, unless the
//     device reports it active right now (its start lease is live, or its
//     newest heartbeat, no older than SweepOptions.ActiveWithin, carries
//     progress for that run id). The agent sends neither for a restore (its
//     heartbeat leaves the run id and progress out while a restore holds the
//     restic slot), so for a restore this is a ceiling counted from the fetch.
//
// A console that was down heard nothing from anyone, so silence it measures
// across its own outage is not the device's. After every start of the console
// process (and, for a caller that does not say when it started, after an outage
// the sweep sees in its own record) no device restore is closed, for any
// reason, until the console has been listening for DeviceRestoreReconnectGrace
// (restoreSweepListeningSince): every live agent has heartbeated by then, and
// the one that has not is silent while the console listens. A heartbeat inside
// the grace makes the device heard, as always.
//
// A run record that arrives later is still ingested. The row stays as the sweep
// closed it, and the agent's report is kept beside it (recordLateRestoreReport,
// audited restore.late_report): FAILED never turns quietly into RESTORED.
const (
	// DeviceRestoreSilenceLimit is how long a running device restore may go
	// with nothing at all from its device before the console calls it
	// interrupted (ADR 0032). The agent's own bounds (internal/agent): a
	// heartbeat every 60 s (+-10 s); while the console does not answer, a retry
	// backoff clamped at 5 min (uplink.BackoffMax, jitter never above it), or
	// the console's own Retry-After, honoured exactly up to 1 h; with a refused
	// device token one probe an hour (+-20 %), at most 72 min. The restore
	// itself is stopped by the agent after 15 min without movement (restoreIdleBound), a selection
	// fetch is retried for at most 30 min (restoreFetchFor), and a restore that
	// waits for a running backup (up to sla.max_run_hours, 12 h by default)
	// keeps heartbeating meanwhile. So no restore still running on a device
	// that can reach the console goes silent for more than 72 min; 24 h is that
	// bound twenty times over, and the same day a queued restore waits for its
	// device (ruling C6).
	DeviceRestoreSilenceLimit = 24 * time.Hour
	// DeviceRestoreRunningBound is how long a running device restore may go
	// without its run record, heartbeats or not. It is far longer than any
	// plausible pilot restore: the agent stops restic itself when no status
	// line arrives within its watchdog bound (ADR 0027, "A restore that stops
	// moving is stopped"), and a selection of at most 100 paths that has not
	// finished in two days on a working link is not finishing; the operator
	// then sees FAILED, not RUNNING, and a late record is kept beside it.
	DeviceRestoreRunningBound = 48 * time.Hour
	// deviceRestoreRestartGrace gives a restarted agent time to post the run
	// record it journaled before restic started (its outbox sends within
	// minutes of start-up) before the sweep calls the restore lost.
	deviceRestoreRestartGrace = 30 * time.Minute
	// DeviceRestoreReconnectGrace is how long the console listens after an
	// outage before it closes any device restore (ADR 0032). An agent's longest
	// wait before it tries the console again is the Retry-After the console
	// sent, honoured up to 1 h (uplink.Backoff.Delay); its own backoff is
	// clamped at 5 min; the heartbeat that follows takes at most 70 s. Two
	// hours is that 1 h 6 min with the margin of nearly twice over.
	DeviceRestoreReconnectGrace = 2 * time.Hour
	// sweepOutageAfter: without SweepOptions.ConsoleStartedAt, a first sweep
	// that finds the previous sweep finished longer ago than this (the sweeper
	// runs every 5 min) knows the console, or its job runner, was not
	// listening, and starts the reconnect grace.
	sweepOutageAfter = 15 * time.Minute
	// abandonedRunSweeperJob is jobs.NameAbandonedRunSweeper, the job_state
	// row of the sweeper (restated: the store does not import jobs).
	abandonedRunSweeperJob = "abandoned-run-sweeper"
)

// sweepListen is Store.listen: when the sweeps of this process started
// listening (ms; 0 when there is no grace to hold), once set.
type sweepListen struct {
	mu  sync.Mutex
	set bool
	ms  int64
}

// restoreSweepListeningSince settles, once per Store and before the sweep does
// anything that can fail, when the console started listening: o.ConsoleStartedAt
// when the caller says (serve sets it at start-up, so every restart holds the
// grace); else the time of this first sweep when the previous sweep (job_state
// of the sweeper, written by the job runner after every run) finished more than
// sweepOutageAfter before it, or when that record cannot be read (failing safe:
// a grace too many delays a close by two hours, a grace too few closes healthy
// restores); else 0, no grace (a quick restart: the fleet's heartbeats are
// fresh). A later sweep that fails, and the error run the job runner records
// for it, cannot take the grace back.
func (s *Store) restoreSweepListeningSince(ctx context.Context, o SweepOptions) int64 {
	s.listen.mu.Lock()
	defer s.listen.mu.Unlock()
	if s.listen.set {
		return s.listen.ms
	}
	now := msOf(s.Now())
	since := int64(0)
	if !o.ConsoleStartedAt.IsZero() {
		since = msOf(o.ConsoleStartedAt)
	} else {
		js, err := s.GetJobState(ctx, abandonedRunSweeperJob)
		switch {
		case err == nil && js.LastFinishedAt != nil && now-msOf(*js.LastFinishedAt) > sweepOutageAfter.Milliseconds():
			since = now
		case err != nil && !errors.Is(err, ErrNotFound):
			since = now
		}
	}
	s.listen.set, s.listen.ms = true, since
	return since
}

// restoreGraceUntil is the end of the reconnect grace, or 0 when none holds.
// Before the first sweep of this process it assumes the grace starts now.
func (s *Store) restoreGraceUntil() int64 {
	s.listen.mu.Lock()
	set, ms := s.listen.set, s.listen.ms
	s.listen.mu.Unlock()
	if !set {
		ms = msOf(s.Now())
	}
	if ms == 0 {
		return 0
	}
	return ms + DeviceRestoreReconnectGrace.Milliseconds()
}

// DeviceRestoreCloseAt is about when the sweep closes a running device restore
// fetched at started whose device was last heard from at lastHeard (zero:
// never) if nothing more arrives (ADR 0032): DeviceRestoreSilenceLimit after
// the later of the two, or DeviceRestoreRunningBound after the fetch if that
// is sooner, and never before the reconnect grace of this process ends. The
// status page shows it; the sweep runs every five minutes, so "about". A
// function, not a method: it reads no customer data, and the scope matrix
// (scope_test.go) classifies every Store method.
func DeviceRestoreCloseAt(s *Store, started, lastHeard time.Time) time.Time {
	heard := started
	if lastHeard.After(heard) {
		heard = lastHeard
	}
	at := heard.Add(DeviceRestoreSilenceLimit)
	if c := started.Add(DeviceRestoreRunningBound); c.Before(at) {
		at = c
	}
	if g := s.restoreGraceUntil(); g > 0 && time.UnixMilli(g).After(at) {
		at = time.UnixMilli(g).UTC()
	}
	return at.UTC()
}

// stuckRestore is one running device restore as the sweep reads it.
type stuckRestore struct {
	id, deviceID, cust string
	started            int64
	runExists          bool
	progress           sql.NullString
	presenceAt, uptime sql.NullInt64
	leaseUntil         sql.NullInt64
}

// stuckReason says why a running device restore must be closed now, or "".
func (r stuckRestore) stuckReason(now int64, activeWithin time.Duration) string {
	heard := r.started
	if r.presenceAt.Valid && r.presenceAt.Int64 > heard {
		heard = r.presenceAt.Int64
	}
	if now-heard >= DeviceRestoreSilenceLimit.Milliseconds() {
		return RestoreClosedAgentSilent
	}
	if !r.runExists && r.presenceAt.Valid && r.uptime.Valid {
		boot := r.presenceAt.Int64 - r.uptime.Int64*1000
		if boot > r.started && now-boot >= deviceRestoreRestartGrace.Milliseconds() {
			return RestoreClosedAgentRestarted
		}
	}
	progressFor, _ := progressRun(r.progress)
	active := (r.leaseUntil.Valid && r.leaseUntil.Int64 > now) ||
		(progressFor == r.id && r.presenceAt.Valid && now-r.presenceAt.Int64 <= activeWithin.Milliseconds())
	if now-r.started >= DeviceRestoreRunningBound.Milliseconds() && !active {
		return RestoreClosedNoRunRecord
	}
	return ""
}

// sweepStuckDeviceRestores closes the running device restores stuckReason names,
// one transaction per row with its audit row; a failed audit write leaves that
// row running for the next sweep and is returned. Inside the reconnect grace
// (listening, restoreSweepListeningSince) it closes none and counts them in
// res.RestoresHeldBack instead.
func (s *Store) sweepStuckDeviceRestores(ctx context.Context, activeWithin time.Duration, listening int64, res *SweepResult) error {
	var grace int64
	if listening > 0 && msOf(s.Now()) < listening+DeviceRestoreReconnectGrace.Milliseconds() {
		grace = listening + DeviceRestoreReconnectGrace.Milliseconds() // the agents may still be reconnecting
	}
	var rows []stuckRestore
	err := s.Read(ctx, func(tx *Tx) error {
		var err error
		rows, err = collect(tx, `SELECT r.id, r.device_id, d.customer_id, COALESCE(r.started_at, r.updated_at),
				EXISTS (SELECT 1 FROM runs x WHERE x.id = r.id AND x.device_id = r.device_id),
				p.progress, p.received_at, p.uptime_s, l.lease_until
			FROM restore_requests r JOIN devices d ON d.id = r.device_id
			LEFT JOIN device_presence p ON p.device_id = r.device_id
			LEFT JOIN start_leases l ON l.run_id = r.id
			WHERE r.mode IN ('device_original','device_path') AND r.status = 'running'
			ORDER BY r.created_at, r.id`, nil, func(sc scanner) (stuckRestore, error) {
			var r stuckRestore
			return r, sc.Scan(&r.id, &r.deviceID, &r.cust, &r.started, &r.runExists, &r.progress, &r.presenceAt, &r.uptime, &r.leaseUntil)
		})
		return err
	})
	if err != nil {
		return err
	}
	closed := 0
	var by map[string]int
	var firstErr error
	for _, r := range rows {
		reason := r.stuckReason(msOf(s.Now()), activeWithin)
		if reason == "" {
			continue
		}
		if grace > 0 {
			res.RestoresHeldBack++
			res.GraceUntil = time.UnixMilli(grace).UTC()
			continue
		}
		did := false
		err := s.Write(ctx, func(tx *Tx) error {
			did = false
			now := tx.nowMS()
			prog, _ := json.Marshal(DeviceRestoreProgress{Outcome: "interrupted", ClosedBy: reason})
			res, err := tx.Exec(`UPDATE restore_requests SET status = 'failed', error_class = 'interrupted', progress = ?, selection_wrapped = NULL,
				finished_at = ?, updated_at = ? WHERE id = ? AND status = 'running'`, string(prog), now, now, r.id)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return nil // its run record closed it meanwhile
			}
			details, _ := json.Marshal(map[string]any{"status": "failed", "error_class": "interrupted", "reason": reason})
			if err := audited(tx, AuditEvent{ActorType: "system", Method: "system", Action: "restore.finish", ObjectType: "restore",
				ObjectID: r.id, CustomerID: r.cust, Result: "error", Severity: "warning", Details: details}); err != nil {
				return err
			}
			did = true
			return nil
		})
		switch {
		case err != nil && firstErr == nil:
			firstErr = err
		case err == nil && did:
			closed++
			if by == nil {
				by = map[string]int{}
			}
			by[reason]++
		}
	}
	res.RestoresClosed, res.RestoresClosedBy = closed, by
	return firstErr
}

// openRun is one open agent run as the sweeper reads it.
type openRun struct {
	id, deviceID, policyID string
	started                int64
	progress               sql.NullString
	presenceAt             sql.NullInt64
	leaseUntil             sql.NullInt64
}

// SweepAbandonedRuns is the abandoned-run-sweeper (architecture §5.7): an open
// agent run (outcome NULL) whose start is older than its max runtime plus
// Slack, and which the device no longer reports active, is closed as
// "abandoned" with finished_at at its last known activity. The device reports
// a run active while its start lease is live (heartbeats renew it, ADR 0005)
// or while its newest heartbeat, no older than ActiveWithin, carries progress
// for that run id. Max runtime is the device's effective policy
// sla.max_run_hours (the §3.6 default when the policy sets none).
//
// It is idempotent: the UPDATE matches only rows still open (WHERE outcome IS
// NULL), so a run the agent closed meanwhile is untouched, and a later report of
// the run with a real outcome overwrites "abandoned" (AgentIngestRuns). Console
// runs (origin console, the maintenance worker's) are not the agent's to
// report and are left alone.
//
// After the runs it closes the running device restores whose agent stopped
// reporting them (sweepStuckDeviceRestores, ADR 0027), counted in
// SweepResult.RestoresClosed; an error there is returned after the run sweep
// committed.
func (s *Store) SweepAbandonedRuns(ctx context.Context, o SweepOptions) (SweepResult, error) {
	if o.Slack <= 0 {
		o.Slack = DefaultSweepSlack
	}
	if o.ActiveWithin <= 0 {
		o.ActiveWithin = DefaultSweepActiveWithin
	}
	if o.Limit <= 0 {
		o.Limit = DefaultSweepLimit
	}
	var res SweepResult
	// First, before anything here can fail: when this process started listening.
	listening := s.restoreSweepListeningSince(ctx, o)
	err := s.Write(ctx, func(tx *Tx) error {
		now := tx.nowMS()
		runs, err := collect(tx, `SELECT r.id, r.device_id, COALESCE(c.policy_id, ''), r.started_at, p.progress, p.received_at, l.lease_until
			FROM runs r INDEXED BY idx_runs_open
			JOIN devices d ON d.id = r.device_id
			JOIN customers c ON c.id = d.customer_id
			LEFT JOIN device_presence p ON p.device_id = r.device_id
			LEFT JOIN start_leases l ON l.run_id = r.id
			WHERE r.outcome IS NULL AND r.origin = 'agent'
			ORDER BY r.started_at LIMIT ?`, []any{o.Limit}, func(sc scanner) (openRun, error) {
			var r openRun
			return r, sc.Scan(&r.id, &r.deviceID, &r.policyID, &r.started, &r.progress, &r.presenceAt, &r.leaseUntil)
		})
		if err != nil {
			return err
		}
		comp, err := newComposer(tx)
		if err != nil {
			return err
		}
		for _, r := range runs {
			res.Examined++
			maxRun := state.DefaultThresholds().MaxRun
			if pol, _, _, perr := comp.effective(r.deviceID, r.policyID); perr == nil {
				if m := ThresholdsFromPolicy(pol).MaxRun; m > 0 {
					maxRun = m
				}
			}
			if now-r.started <= (maxRun + o.Slack).Milliseconds() {
				continue
			}
			if r.leaseUntil.Valid && r.leaseUntil.Int64 > now {
				continue // a heartbeat renewed its start lease: the agent says it is running
			}
			progressFor, advancedAt := progressRun(r.progress)
			if progressFor == r.id && r.presenceAt.Valid && now-r.presenceAt.Int64 <= o.ActiveWithin.Milliseconds() {
				continue // the newest heartbeat reports this run's progress
			}
			// Last known activity: the progress of this run when the device reported it, else its start.
			last := r.started
			if progressFor == r.id && advancedAt > last && advancedAt <= now {
				last = advancedAt
			}
			out, err := tx.Exec(`UPDATE runs SET outcome = 'abandoned', finished_at = ?, updated_at = ?
				WHERE id = ? AND outcome IS NULL`, last, now, r.id)
			if err != nil {
				return err
			}
			if n, _ := out.RowsAffected(); n > 0 {
				res.Abandoned++
			}
		}
		return nil
	})
	if err != nil {
		return res, err
	}
	err = s.sweepStuckDeviceRestores(ctx, o.ActiveWithin, listening, &res)
	return res, err
}

// progressRun reads the run id and the "advanced_at" stamp of a stored progress
// document; zero values when there is none or it does not parse.
func progressRun(raw sql.NullString) (runID string, advancedAt int64) {
	if !raw.Valid {
		return "", 0
	}
	var m struct {
		RunID      string  `json:"run_id"`
		AdvancedAt float64 `json:"advanced_at"`
	}
	if json.Unmarshal([]byte(raw.String), &m) != nil {
		return "", 0
	}
	return m.RunID, int64(m.AdvancedAt)
}
