package store

import (
	"context"
	"database/sql"
	"encoding/json"
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
