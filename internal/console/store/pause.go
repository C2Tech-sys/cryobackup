package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"time"
)

// Console-owned pause and resume (M3-T3, architecture §5.2, DH11, B15). A pause
// is a state-engine input (PAUSED), stored on the device row; the verdict is
// still computed at read time. Pausing:
//
//   - needs a reason (schema rule) and either an end in the future or none
//     (indefinite, which every digest lists with its age);
//   - cancels a running backup (DH11) by queueing a cancel command for it in the
//     same transaction, and queues a policy refresh so the agent learns the
//     pause now instead of at its next poll;
//   - writes exactly one audit row ("device.pause" or "device.resume"); the
//     queued commands are named in its details, not audited separately.

// Pause durations an operator may pick (B15). The API accepts these as hours;
// a pause until a date, or an indefinite one, are the other two forms.
var PauseHours = []int{1, 2, 4, 8, 24}

const (
	maxPauseReason = 200
	// MaxPauseSpan bounds a dated pause: longer than that is an indefinite pause
	// in disguise and should say so.
	MaxPauseSpan = 366 * 24 * time.Hour
)

// PauseRequest parks a device. Until nil is an indefinite pause.
type PauseRequest struct {
	Reason string
	Until  *time.Time
}

// PauseResult is the outcome of PauseDeviceAudited and ResumeDeviceAudited.
type PauseResult struct {
	Device Device
	// Changed is false when the request matched the current state (nothing was
	// written and no audit row exists).
	Changed bool
	// CancelledRunID is the open backup run a cancel command was queued for.
	CancelledRunID string
	// QueuedCommands is how many commands the change queued (cancel, refresh).
	QueuedCommands int
}

// queueSystemCommand inserts a command for a device inside tx on behalf of the
// admin, skipping it when the idempotency key already exists. It reports
// whether a row was written.
func queueSystemCommand(tx *Tx, deviceID, typ, params, key, adminID string, ttl time.Duration, now int64) (bool, error) {
	res, err := tx.Exec(`INSERT OR IGNORE INTO commands (id, device_id, type, params, idempotency_key, created_by, expires_at, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?)`, tx.id(), deviceID, typ, params, key, nullStr(adminID), now+ttl.Milliseconds(), now, now)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// PauseDeviceAudited parks a device inside scope. A retired device cannot be
// paused (ErrDeviceRetired); a device outside scope is ErrNotFound; a missing
// reason, a reason that looks like a path, or an end that is not in the future
// (or more than MaxPauseSpan away) is a *ValidationError. Pausing an already
// paused device with the same end and reason is a no-op; a different end or
// reason replaces the pause and is audited.
func (s *Store) PauseDeviceAudited(ctx context.Context, scope Scope, deviceID string, req PauseRequest, actor AuditActor) (PauseResult, error) {
	if err := scope.check(); err != nil {
		return PauseResult{}, err
	}
	reason, ok := cleanLine(req.Reason, maxPauseReason)
	var bad []FieldError
	switch {
	case !ok || reason == "":
		bad = append(bad, fieldf("reason", "Give a reason of 1 to %d characters on one line.", maxPauseReason))
	case looksLikePath(reason):
		bad = append(bad, fieldf("reason", "Describe the reason in words, without a file path."))
	}
	var out PauseResult
	err := s.Write(ctx, func(tx *Tx) error {
		dev, err := getDevice(tx, scope, deviceID)
		if err != nil {
			return err
		}
		now := tx.nowMS()
		if req.Until != nil {
			switch u := req.Until.UnixMilli(); {
			case u <= now+int64(time.Minute/time.Millisecond):
				bad = append(bad, fieldf("until", "The pause must end at least a minute from now."))
			case u > now+MaxPauseSpan.Milliseconds():
				bad = append(bad, fieldf("until", "A dated pause can last at most a year; leave the end out to pause indefinitely."))
			}
		}
		if err := validation(bad); err != nil {
			return err
		}
		if dev.RetiredAt != nil {
			return ErrDeviceRetired
		}
		var until any
		if req.Until != nil {
			until = req.Until.UnixMilli()
		}
		inForce := dev.PausedAt != nil && (dev.PausedUntil == nil || dev.PausedUntil.UnixMilli() > now)
		if inForce && dev.PauseReason == reason && sameTimePtr(dev.PausedUntil, req.Until) {
			out = PauseResult{Device: dev}
			return nil
		}
		pausedAt := now
		if inForce {
			pausedAt = dev.PausedAt.UnixMilli() // a replaced pause keeps its start (paused_until must stay after it)
		}
		if req.Until != nil && req.Until.UnixMilli() <= pausedAt {
			return validation([]FieldError{fieldf("until", "The pause must end after it began.")})
		}
		if _, err := tx.Exec(`UPDATE devices SET paused_at = ?, paused_until = ?, pause_reason = ?, paused_by = ?, updated_at = ? WHERE id = ?`,
			pausedAt, until, reason, nullStr(actor.AdminID), now, deviceID); err != nil {
			return err
		}
		queued := 0
		var runID string
		err = tx.QueryRow(`SELECT id FROM runs WHERE device_id = ? AND kind = 'backup' AND outcome IS NULL ORDER BY started_at DESC, id DESC LIMIT 1`, deviceID).Scan(&runID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if runID != "" {
			made, err := queueSystemCommand(tx, deviceID, CommandCancel, `{"run_id":"`+runID+`"}`, "pause-cancel:"+runID, actor.AdminID, DefaultOperatorTTL(CommandCancel), now)
			if err != nil {
				return err
			}
			if made {
				queued++
			}
		}
		made, err := queueSystemCommand(tx, deviceID, CommandRefreshPolicy, `{}`, "pause-refresh:"+deviceID+":"+strconv.FormatInt(now, 10), actor.AdminID, DefaultOperatorTTL(CommandRefreshPolicy), now)
		if err != nil {
			return err
		}
		if made {
			queued++
		}
		details := map[string]any{"reason": reason, "indefinite": req.Until == nil, "commands": queued}
		if req.Until != nil {
			details["until"] = req.Until.UTC().Format(time.RFC3339)
		}
		if runID != "" {
			details["cancelled_run"] = runID
		}
		ev, err := actor.event("device.pause", "device", deviceID, dev.CustomerID, details)
		if err != nil {
			return err
		}
		if err := audited(tx, ev); err != nil {
			return err
		}
		d, err := getDevice(tx, scope, deviceID)
		out = PauseResult{Device: d, Changed: true, CancelledRunID: runID, QueuedCommands: queued}
		return err
	})
	if err != nil {
		return PauseResult{}, err
	}
	return out, nil
}

// ResumeDeviceAudited ends a pause early. A device that is not paused (or whose
// pause already lapsed and was cleared) is a no-op with Changed false. A lapsed
// pause that is still recorded on the row is cleared and audited. A device
// outside scope is ErrNotFound.
func (s *Store) ResumeDeviceAudited(ctx context.Context, scope Scope, deviceID string, actor AuditActor) (PauseResult, error) {
	if err := scope.check(); err != nil {
		return PauseResult{}, err
	}
	var out PauseResult
	err := s.Write(ctx, func(tx *Tx) error {
		dev, err := getDevice(tx, scope, deviceID)
		if err != nil {
			return err
		}
		if dev.PausedAt == nil {
			out = PauseResult{Device: dev}
			return nil
		}
		now := tx.nowMS()
		if _, err := tx.Exec(`UPDATE devices SET paused_at = NULL, paused_until = NULL, pause_reason = NULL, paused_by = NULL, updated_at = ? WHERE id = ?`,
			now, deviceID); err != nil {
			return err
		}
		queued := 0
		made, err := queueSystemCommand(tx, deviceID, CommandRefreshPolicy, `{}`, "resume-refresh:"+deviceID+":"+strconv.FormatInt(now, 10), actor.AdminID, DefaultOperatorTTL(CommandRefreshPolicy), now)
		if err != nil {
			return err
		}
		if made {
			queued++
		}
		ev, err := actor.event("device.resume", "device", deviceID, dev.CustomerID, map[string]any{"was_indefinite": dev.PausedUntil == nil, "commands": queued})
		if err != nil {
			return err
		}
		if err := audited(tx, ev); err != nil {
			return err
		}
		d, err := getDevice(tx, scope, deviceID)
		out = PauseResult{Device: d, Changed: true, QueuedCommands: queued}
		return err
	})
	if err != nil {
		return PauseResult{}, err
	}
	return out, nil
}

func sameTimePtr(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.UnixMilli() == b.UnixMilli()
}
