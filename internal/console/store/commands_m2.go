package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// This file is the M2 command surface (ticket M2-W2C): the audited, validated
// way an operator queues a command for a device (QueueCommandAudited), cancels
// one that has not been delivered yet (CancelCommandAudited), and reads a
// device's command history with the status an operator should see
// (ListDeviceCommands, Command.EffectiveStatus). The M0 EnqueueCommand /
// AgentNextCommands / AgentAckCommand in commands.go are untouched: the agent
// protocol is unchanged.

// Reasons a command is refused, all of them ErrConflict so a caller maps them to
// 409 and may say why.
var (
	// ErrDeviceRetired: a retired device takes no commands.
	ErrDeviceRetired = conflictf("device is retired")
	// ErrDeviceParked: a parked (paused) device is not backed up on demand.
	ErrDeviceParked = conflictf("device is parked")
	// ErrCommandDelivered: the device already has the command; it can no longer be cancelled.
	ErrCommandDelivered = conflictf("command was already delivered to the device")
	// ErrCommandExpired: the command ran out of time before it was delivered.
	ErrCommandExpired = conflictf("command has expired")
)

// Command types an operator may queue in M2. The agent executes exactly these
// (internal/agent/commands); the other types of the schema (restart_service,
// update_agent, uninstall, pause, resume, hold, restore) are queued by their own
// milestones' code paths and are refused here rather than queued to fail.
const (
	CommandBackupNow      = "backup_now"
	CommandCancel         = "cancel"
	CommandRefreshPolicy  = "refresh_policy"
	CommandCheckIn        = "check_in"
	CommandStatusExpired  = "expired"
	CommandStatusCanceled = "cancelled"
)

// OperatorCommandTypes lists the command types QueueCommandAudited accepts.
var OperatorCommandTypes = []string{CommandBackupNow, CommandCancel, CommandRefreshPolicy, CommandCheckIn}

// CommandStatuses are the statuses a command list can be filtered by (the
// effective status, see EffectiveStatus).
var CommandStatuses = []string{"queued", "delivered", "acked", "succeeded", "failed", "expired", "cancelled"}

// Command lifetimes. A command that waits for an offline device is only useful
// for a while: a backup requested yesterday should not start now, and a cancel
// that arrives hours late must never stop a later, unrelated run.
const (
	minCommandTTL = time.Minute
	maxCommandTTL = 7 * 24 * time.Hour
)

// DefaultOperatorTTL is how long a command of the given type waits for the
// device when the caller names no lifetime.
func DefaultOperatorTTL(typ string) time.Duration {
	switch typ {
	case CommandBackupNow, CommandRefreshPolicy:
		return 4 * time.Hour
	default: // cancel, check_in: stale ones are harmless to drop and harmful to run late
		return 15 * time.Minute
	}
}

// EffectiveStatus is the status an operator should see at now. The
// command-expiry job moves queued and delivered commands past their expiry to
// "expired" once a minute; between the deadline and the next tick the stored
// status is stale, so a reader computes it: a command that is still queued or
// delivered when its expiry has passed is expired, never "queued".
func (c Command) EffectiveStatus(now time.Time) string {
	if (c.Status == "queued" || c.Status == "delivered") && !c.ExpiresAt.After(now) {
		return CommandStatusExpired
	}
	return c.Status
}

// Pending reports whether the command is still in flight at now: queued,
// delivered or acknowledged and not past its expiry.
func (c Command) Pending(now time.Time) bool {
	switch c.Status {
	case "queued", "delivered", "acked":
		return c.ExpiresAt.After(now)
	}
	return false
}

// QueueResult is the outcome of QueueCommandAudited.
type QueueResult struct {
	Command Command
	// Created is true when a new command was queued (and one audit row written).
	Created bool
	// Coalesced is true when an equal command was already in flight and was
	// returned instead of queuing a second one. Nothing was written.
	Coalesced bool
}

// QueueCommandAudited queues a command for a device inside scope on behalf of an
// operator, validated and audited in one transaction (one "command.queue" audit
// row per new command; a replay or a coalesced request writes none).
//
//   - The type must be one of OperatorCommandTypes; params must carry no paths
//     (backup_now, refresh_policy and check_in take none; cancel takes an
//     optional run_id).
//   - A retired device takes no commands (ErrDeviceRetired); backup_now on a
//     parked device is ErrDeviceParked (a parked device is not backed up on
//     demand; resume it first).
//   - The same idempotency key with the same type and params returns the
//     original command; with different content it is ErrConflict.
//   - A command of the same type and params that is still in flight is returned
//     instead of queuing a second (Coalesced), so a double click, two operators
//     or a retry with a fresh key cannot start two backups.
//
// Rejected input is a *ValidationError; a device outside scope is ErrNotFound.
func (s *Store) QueueCommandAudited(ctx context.Context, scope Scope, deviceID string, in CommandInput, actor AuditActor) (QueueResult, error) {
	if err := scope.check(); err != nil {
		return QueueResult{}, err
	}
	var bad []FieldError
	if !oneOf(in.Type, OperatorCommandTypes) {
		bad = append(bad, fieldf("type", "Use backup_now, cancel, refresh_policy or check_in."))
	}
	if n := len(in.IdempotencyKey); n < 1 || n > 128 {
		bad = append(bad, fieldf("idempotency_key", "An idempotency key of 1 to 128 characters is required."))
	}
	if in.TTL != 0 && (in.TTL < minCommandTTL || in.TTL > maxCommandTTL) {
		bad = append(bad, fieldf("ttl", "The lifetime must be between 1 minute and 7 days."))
	}
	params, perr := cleanCommandParams(in.Type, in.Params)
	if perr != nil {
		bad = append(bad, *perr)
	}
	if err := validation(bad); err != nil {
		return QueueResult{}, err
	}
	ttl := in.TTL
	if ttl == 0 {
		ttl = DefaultOperatorTTL(in.Type)
	}
	var out QueueResult
	err := s.Write(ctx, func(tx *Tx) error {
		dev, err := getDevice(tx, scope, deviceID)
		if err != nil {
			return err
		}
		now := tx.nowMS()
		// A replay of the same key is answered before the device state is judged:
		// the first answer stands.
		if existing, err := one(tx, `SELECT `+commandCols+` FROM commands WHERE device_id = ? AND idempotency_key = ?`,
			[]any{deviceID, in.IdempotencyKey}, scanCommand); err == nil {
			if existing.Type != in.Type || !sameJSON(existing.Params, []byte(params)) {
				return conflictf("idempotency key already used for a different command")
			}
			out = QueueResult{Command: existing}
			return nil
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}
		if dev.RetiredAt != nil {
			return ErrDeviceRetired
		}
		if in.Type == CommandBackupNow && dev.PausedAt != nil && (dev.PausedUntil == nil || dev.PausedUntil.UnixMilli() > now) {
			return ErrDeviceParked
		}
		// Coalesce with an equal command still in flight.
		inflight, err := collect(tx, `SELECT `+commandCols+` FROM commands WHERE device_id = ? AND type = ?
			AND status IN ('queued','delivered','acked') AND expires_at > ? ORDER BY created_at, id`, []any{deviceID, in.Type, now}, scanCommand)
		if err != nil {
			return err
		}
		for _, c := range inflight {
			if sameJSON(c.Params, []byte(params)) {
				out = QueueResult{Command: c, Coalesced: true}
				return nil
			}
		}
		id := tx.id()
		if _, err := tx.Exec(`INSERT INTO commands (id, device_id, type, params, idempotency_key, created_by, expires_at, created_at, updated_at)
			VALUES (?,?,?,?,?,?,?,?,?)`, id, deviceID, in.Type, params, in.IdempotencyKey, nullStr(actor.AdminID),
			now+ttl.Milliseconds(), now, now); err != nil {
			return err
		}
		queued := map[string]any{"command_id": id, "type": in.Type, "ttl_s": int64(ttl / time.Second)}
		if in.AuditNote != "" {
			queued["note"] = in.AuditNote
		}
		ev, err := actor.event("command.queue", "device", deviceID, dev.CustomerID, queued)
		if err != nil {
			return err
		}
		if err := audited(tx, ev); err != nil {
			return err
		}
		c, err := one(tx, `SELECT `+commandCols+` FROM commands WHERE id = ?`, []any{id}, scanCommand)
		out = QueueResult{Command: c, Created: true}
		return err
	})
	if err != nil {
		return QueueResult{}, err
	}
	return out, nil
}

// cleanCommandParams normalises and checks the params of an operator command and
// returns them as canonical JSON text.
func cleanCommandParams(typ string, raw json.RawMessage) (string, *FieldError) {
	if len(strings.TrimSpace(string(raw))) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return "{}", nil
	}
	var m map[string]json.RawMessage
	if len(raw) > 1024 || json.Unmarshal(raw, &m) != nil {
		fe := fieldf("params", "Params must be a small JSON object.")
		return "", &fe
	}
	allowed := map[string]bool{}
	if typ == CommandCancel {
		allowed["run_id"] = true
	}
	for k, v := range m {
		if !allowed[k] {
			fe := fieldf("params", "This command takes no parameter %q.", k)
			return "", &fe
		}
		var id string
		if json.Unmarshal(v, &id) != nil || !plainID(id) {
			fe := fieldf("params", "run_id must be a run id.")
			return "", &fe
		}
	}
	b, err := json.Marshal(m)
	if err != nil {
		fe := fieldf("params", "Params must be a small JSON object.")
		return "", &fe
	}
	return string(b), nil
}

// plainID reports whether s is a 26-character identifier (a ULID): letters and
// digits only, so it can be neither a path nor free text.
func plainID(s string) bool {
	if len(s) != 26 {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z') {
			return false
		}
	}
	return true
}

// CancelCommandAudited cancels a command of a device inside scope that has not
// been delivered yet, and writes one "command.cancel" audit row. It is
// idempotent: cancelling an already cancelled command succeeds with changed
// false and writes nothing. A command the device already received is
// ErrCommandDelivered (queue a cancel command to stop a running backup
// instead), one past its expiry ErrCommandExpired. A device or command outside
// scope, or a command that belongs to another device, is ErrNotFound.
func (s *Store) CancelCommandAudited(ctx context.Context, scope Scope, deviceID, commandID string, actor AuditActor) (cmd Command, changed bool, err error) {
	if err := scope.check(); err != nil {
		return Command{}, false, err
	}
	err = s.Write(ctx, func(tx *Tx) error {
		dev, err := getDevice(tx, scope, deviceID)
		if err != nil {
			return err
		}
		cur, err := one(tx, `SELECT `+commandCols+` FROM commands WHERE id = ? AND device_id = ?`, []any{commandID, deviceID}, scanCommand)
		if err != nil {
			return err
		}
		now := tx.nowMS()
		switch {
		case cur.Status == CommandStatusCanceled:
			cmd = cur
			return nil
		case cur.Status == "expired" || (cur.Status == "queued" && cur.ExpiresAt.UnixMilli() <= now):
			return ErrCommandExpired
		case cur.Status != "queued":
			return ErrCommandDelivered
		}
		if _, err := tx.Exec(`UPDATE commands SET status = 'cancelled', completed_at = ?, updated_at = ? WHERE id = ? AND status = 'queued'`,
			now, now, commandID); err != nil {
			return err
		}
		ev, err := actor.event("command.cancel", "device", deviceID, dev.CustomerID, map[string]any{"command_id": commandID, "type": cur.Type})
		if err != nil {
			return err
		}
		if err := audited(tx, ev); err != nil {
			return err
		}
		cmd, err = one(tx, `SELECT `+commandCols+` FROM commands WHERE id = ?`, []any{commandID}, scanCommand)
		changed = true
		return err
	})
	if err != nil {
		return Command{}, false, err
	}
	return cmd, changed, nil
}

// lateAck records a result that arrived for a delivered command after its
// expiry. The status stays expired (the operator already saw it expire and may
// have asked again); the outcome is kept in the result as "late_status" beside
// whatever the device reported, and completed_at says when it arrived. A late
// "acked" carries no outcome and changes nothing; a repeat of the same late
// status is a no-op; a different late status than the one recorded is ignored.
func lateAck(tx *Tx, cur Command, ack CommandAck, now int64) error {
	if ack.Status != "succeeded" && ack.Status != "failed" {
		return nil
	}
	obj := map[string]json.RawMessage{}
	if len(cur.Result) > 0 {
		_ = json.Unmarshal(cur.Result, &obj)
	}
	if _, done := obj["late_status"]; done {
		return nil
	}
	if len(ack.Result) > 0 {
		var in map[string]json.RawMessage
		if json.Unmarshal(ack.Result, &in) == nil {
			for k, v := range in {
				obj[k] = v
			}
		}
	}
	obj["late_status"], _ = json.Marshal(ack.Status)
	b, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`UPDATE commands SET completed_at = ?, result = ?, updated_at = ? WHERE id = ?`, now, string(b), now, cur.ID)
	return err
}

// CommandFilter narrows ListDeviceCommands. Status is an effective status (see
// Command.EffectiveStatus); Type a command type. Zero fields do not filter.
type CommandFilter struct {
	Status string
	Type   string
}

// CommandPage is one numbered page of a device's commands, newest first.
type CommandPage struct {
	Items []Command
	Page  PageInfo
}

// ListDeviceCommands returns one numbered page of a device's commands that match
// f, newest first. now decides the effective status filter (a queued command
// past its expiry counts as expired). The device must lie inside scope
// (ErrNotFound otherwise). Rejected filter values are a *ValidationError.
// page and size are normalised as Paginate does; when limit > 0 the window is
// the rows [offset, offset+limit) instead (the JSON API's cursor).
func (s *Store) ListDeviceCommands(ctx context.Context, scope Scope, deviceID string, f CommandFilter, now time.Time, page, size, offset, limit int) (CommandPage, error) {
	if err := scope.check(); err != nil {
		return CommandPage{}, err
	}
	var bad []FieldError
	if f.Status != "" && !oneOf(f.Status, CommandStatuses) {
		bad = append(bad, fieldf("status", "Unknown command status."))
	}
	if f.Type != "" && !oneOf(f.Type, commandTypesAll) {
		bad = append(bad, fieldf("type", "Unknown command type."))
	}
	if err := validation(bad); err != nil {
		return CommandPage{}, err
	}
	where, args := []string{"device_id = ?"}, []any{deviceID}
	nowMS := msOf(now)
	switch f.Status {
	case "":
	case "queued", "delivered":
		where, args = append(where, "status = ? AND expires_at > ?"), append(args, f.Status, nowMS)
	case "expired":
		where, args = append(where, "(status = 'expired' OR (status IN ('queued','delivered') AND expires_at <= ?))"), append(args, nowMS)
	default:
		where, args = append(where, "status = ?"), append(args, f.Status)
	}
	if f.Type != "" {
		where, args = append(where, "type = ?"), append(args, f.Type)
	}
	cond := strings.Join(where, " AND ")
	var out CommandPage
	err := s.Read(ctx, func(tx *Tx) error {
		if _, err := scopedDevice(tx, scope, deviceID); err != nil {
			return err
		}
		var total int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM commands WHERE `+cond, args...).Scan(&total); err != nil {
			return err
		}
		from, to, info := windowOf(total, page, size, offset, limit)
		items, err := collect(tx, `SELECT `+commandCols+` FROM commands WHERE `+cond+` ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?`,
			append(append([]any(nil), args...), to-from, from), scanCommand)
		if err != nil {
			return err
		}
		if items == nil {
			items = []Command{}
		}
		out = CommandPage{Items: items, Page: info}
		return nil
	})
	return out, err
}

// commandTypesAll is every command type of the schema, for filtering a history
// that may hold commands other milestones queued.
var commandTypesAll = []string{"backup_now", "cancel", "refresh_policy", "restart_service", "update_agent", "pause", "resume", "hold", "restore", "check_in", "uninstall"}
