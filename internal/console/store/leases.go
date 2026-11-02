package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Fleet start leases (ADR 0005, table start_leases, migration 0003).
//
// A scheduled backup asks for a lease before it starts. The console grants
// while fewer than the device's storage target's max_concurrent_backups leases
// are live (lease_until > now). A lease lives LeaseTTL from its grant or last
// renewal; the heartbeats of the running agent renew it and the upload of the
// closed run releases it, so a crashed agent's lease expires on its own.

// LeaseTTL is how long a lease lives from its grant or its last renewal (the
// same value as api.StartLeaseTTL; the store does not import the wire package).
const LeaseTTL = 15 * time.Minute

// Bounds of a refusal's retry-after: the time until the earliest live lease
// expires, clamped to [LeaseRetryMin, LeaseRetryMax] (api.StartLeaseRetryMin/Max).
const (
	LeaseRetryMin = 60 * time.Second
	LeaseRetryMax = 900 * time.Second
)

// LeaseDecision is the answer to AgentStartLease.
type LeaseDecision struct {
	// Granted: the run may start.
	Granted bool
	// Unlimited: granted because the target has no cap (max_concurrent_backups
	// = 0); nothing was stored and LeaseUntil is zero.
	Unlimited bool
	// LeaseUntil is the end of the granted lease (Granted and not Unlimited).
	LeaseUntil time.Time
	// RetryAfter is when to ask again (not Granted): the time until the
	// earliest live lease of the target expires, clamped to
	// [LeaseRetryMin, LeaseRetryMax]. It is never random; the agent adds jitter.
	RetryAfter time.Duration
}

// AgentStartLease decides a start lease for run runID of device deviceID at
// now, in one BEGIN IMMEDIATE transaction (Write), so concurrent requests are
// serialised and a cap of K grants exactly K:
//
//  1. a live lease for the same run id and device is returned unchanged (a
//     retried request gets the same lease); one held by another device is
//     ErrConflict; an expired one is dropped and the request decided afresh;
//  2. a cap of 0 grants without storing anything (Unlimited);
//  3. the device's other leases are released: an agent runs one backup at a
//     time, so a lease it still holds belongs to a run that died with its
//     process, and keeping it would hold a slot for up to LeaseTTL;
//  4. expired leases of the target are deleted, the live ones counted, and the
//     lease is stored when count < max; otherwise the answer is a refusal with
//     RetryAfter.
//
// An unknown device is ErrNotFound.
func (s *Store) AgentStartLease(ctx context.Context, deviceID, runID string, now time.Time) (LeaseDecision, error) {
	if len(runID) != 26 {
		return LeaseDecision{}, invalidf("run id must be a 26-character ULID")
	}
	nowMS := now.UnixMilli()
	if nowMS < minEpochMS {
		return LeaseDecision{}, invalidf("now is not a millisecond timestamp")
	}
	var out LeaseDecision
	err := s.Write(ctx, func(tx *Tx) error {
		var target string
		var max int
		err := tx.QueryRow(`SELECT st.id, st.max_concurrent_backups FROM devices d
			JOIN customers c ON c.id = d.customer_id
			JOIN storage_targets st ON st.id = c.storage_target_id
			WHERE d.id = ?`, deviceID).Scan(&target, &max)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}

		var owner string
		var until int64
		err = tx.QueryRow(`SELECT device_id, lease_until FROM start_leases WHERE run_id = ?`, runID).Scan(&owner, &until)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return err
		case owner != deviceID:
			return conflictf("the run id holds a lease of another device")
		case until > nowMS:
			out = LeaseDecision{Granted: true, LeaseUntil: timeOf(until)}
			return nil
		default:
			if _, err := tx.Exec(`DELETE FROM start_leases WHERE run_id = ?`, runID); err != nil {
				return err
			}
		}

		if max == 0 {
			out = LeaseDecision{Granted: true, Unlimited: true}
			return nil
		}
		if _, err := tx.Exec(`DELETE FROM start_leases WHERE device_id = ? AND run_id <> ?`, deviceID, runID); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM start_leases WHERE storage_target_id = ? AND lease_until <= ?`, target, nowMS); err != nil {
			return err
		}
		var live int
		var earliest sql.NullInt64
		if err := tx.QueryRow(`SELECT count(*), MIN(lease_until) FROM start_leases WHERE storage_target_id = ? AND lease_until > ?`,
			target, nowMS).Scan(&live, &earliest); err != nil {
			return err
		}
		if live >= max {
			wait := LeaseRetryMax
			if earliest.Valid {
				wait = time.Duration(earliest.Int64-nowMS) * time.Millisecond
			}
			out = LeaseDecision{RetryAfter: clampRetry(wait)}
			return nil
		}
		leaseUntil := nowMS + LeaseTTL.Milliseconds()
		if _, err := tx.Exec(`INSERT INTO start_leases (run_id, device_id, storage_target_id, granted_at, lease_until, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, runID, deviceID, target, nowMS, leaseUntil, tx.nowMS(), tx.nowMS()); err != nil {
			return err
		}
		out = LeaseDecision{Granted: true, LeaseUntil: timeOf(leaseUntil)}
		return nil
	})
	if err != nil {
		return LeaseDecision{}, err
	}
	return out, nil
}

// clampRetry rounds a wait up to whole seconds and clamps it to
// [LeaseRetryMin, LeaseRetryMax].
func clampRetry(d time.Duration) time.Duration {
	if r := d % time.Second; r != 0 {
		d += time.Second - r
	}
	if d < LeaseRetryMin {
		return LeaseRetryMin
	}
	if d > LeaseRetryMax {
		return LeaseRetryMax
	}
	return d
}

// AgentRenewStartLease extends the live lease of run runID held by device
// deviceID to now + LeaseTTL (a heartbeat of the running agent). It reports
// whether a lease was renewed. An expired lease is not revived: its slot may
// already be someone else's; the run, already started, simply goes on
// uncounted (ADR 0005 fails open).
func (s *Store) AgentRenewStartLease(ctx context.Context, deviceID, runID string, now time.Time) (bool, error) {
	var renewed bool
	err := s.Write(ctx, func(tx *Tx) error {
		nowMS := now.UnixMilli()
		res, err := tx.Exec(`UPDATE start_leases SET lease_until = ?, updated_at = ? WHERE run_id = ? AND device_id = ? AND lease_until > ?`,
			nowMS+LeaseTTL.Milliseconds(), tx.nowMS(), runID, deviceID, nowMS)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		renewed = n > 0
		return err
	})
	return renewed, err
}

// releaseStartLease deletes the lease of a run of the device (the upload of
// the closed run). It runs inside AgentIngestRuns' transaction.
func releaseStartLease(tx *Tx, deviceID, runID string) error {
	_, err := tx.Exec(`DELETE FROM start_leases WHERE run_id = ? AND device_id = ?`, runID, deviceID)
	return err
}

// DeleteExpiredStartLeases deletes every lease whose lease_until is at or
// before now and returns how many (the heartbeat ring trim job; expired leases
// are also dropped whenever a lease is asked for).
func (s *Store) DeleteExpiredStartLeases(ctx context.Context, now time.Time) (int64, error) {
	var n int64
	err := s.Write(ctx, func(tx *Tx) error {
		res, err := tx.Exec(`DELETE FROM start_leases WHERE lease_until <= ?`, now.UnixMilli())
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	return n, err
}

// StartLease is one row of start_leases.
type StartLease struct {
	RunID           string
	DeviceID        string
	StorageTargetID string
	GrantedAt       time.Time
	LeaseUntil      time.Time
}

// ListStartLeases returns the leases of a storage target that are live at
// now, earliest expiry first (doctor output, tests).
func (s *Store) ListStartLeases(ctx context.Context, storageTargetID string, now time.Time) ([]StartLease, error) {
	var out []StartLease
	err := s.Read(ctx, func(tx *Tx) error {
		var err error
		out, err = collect(tx, `SELECT run_id, device_id, storage_target_id, granted_at, lease_until FROM start_leases
			WHERE storage_target_id = ? AND lease_until > ? ORDER BY lease_until, run_id`, []any{storageTargetID, now.UnixMilli()},
			func(sc scanner) (StartLease, error) {
				var l StartLease
				var g, u int64
				if err := sc.Scan(&l.RunID, &l.DeviceID, &l.StorageTargetID, &g, &u); err != nil {
					return StartLease{}, err
				}
				l.GrantedAt, l.LeaseUntil = timeOf(g), timeOf(u)
				return l, nil
			})
		return err
	})
	return out, err
}
