package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"time"
)

// Command is a queued instruction for a device. The queue is pull-based,
// idempotent per (device, idempotency key) and expiring.
type Command struct {
	ID             string
	DeviceID       string
	Type           string // backup_now | cancel | refresh_policy | restart_service | update_agent | pause | resume | hold | restore | check_in | uninstall
	Params         json.RawMessage
	IdempotencyKey string
	Status         string // queued | delivered | acked | succeeded | failed | expired | cancelled
	CreatedBy      string // admin id; "" = system
	ExpiresAt      time.Time
	DeliveredAt    *time.Time
	AckedAt        *time.Time
	CompletedAt    *time.Time
	Result         json.RawMessage
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// CommandInput enqueues a command. Params must carry no paths (a restore
// carries only a restore_request id).
type CommandInput struct {
	Type           string
	Params         json.RawMessage // nil = {}
	IdempotencyKey string          // 1..128 chars; the same key on the same device returns the same command
	TTL            time.Duration   // how long the command may wait; 0 = 24 h
	CreatedBy      string
	// AuditNote, when set, goes in the audit row's details: path-free, validated by the caller.
	AuditNote string
}

const (
	defaultCommandTTL = 24 * time.Hour
	// redeliverAfter is how long a delivered but unacknowledged command waits
	// before AgentNextCommands hands it out again (a lost response must not lose
	// the command; agents deduplicate on the command id).
	redeliverAfter = 2 * time.Minute
)

const commandCols = `id, device_id, type, params, idempotency_key, status, created_by, expires_at, delivered_at, acked_at,
	completed_at, result, created_at, updated_at`

func scanCommand(sc scanner) (Command, error) {
	var c Command
	var by, result sql.NullString
	var del, ack, done sql.NullInt64
	var params string
	var exp, cr, up int64
	if err := sc.Scan(&c.ID, &c.DeviceID, &c.Type, &params, &c.IdempotencyKey, &c.Status, &by, &exp, &del, &ack, &done, &result, &cr, &up); err != nil {
		return Command{}, err
	}
	c.Params, c.CreatedBy, c.ExpiresAt = json.RawMessage(params), by.String, timeOf(exp)
	c.DeliveredAt, c.AckedAt, c.CompletedAt, c.Result = ptrTime(del), ptrTime(ack), ptrTime(done), rawOf(result)
	c.CreatedAt, c.UpdatedAt = timeOf(cr), timeOf(up)
	return c, nil
}

// sameJSON reports whether two JSON documents are equal as values.
func sameJSON(a, b []byte) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}

// EnqueueCommand queues a command for a device inside scope. It is idempotent on
// (device, IdempotencyKey): the same key with the same type and params returns
// the existing command with created=false; the same key with different content
// is ErrConflict (a key must not be reused for a different intent).
func (s *Store) EnqueueCommand(ctx context.Context, scope Scope, deviceID string, in CommandInput) (cmd Command, created bool, err error) {
	if err := scope.check(); err != nil {
		return Command{}, false, err
	}
	if in.IdempotencyKey == "" {
		return Command{}, false, invalidf("idempotency key is required")
	}
	params, err := jsonOr(in.Params, "{}")
	if err != nil {
		return Command{}, false, err
	}
	ttl := in.TTL
	if ttl <= 0 {
		ttl = defaultCommandTTL
	}
	err = s.Write(ctx, func(tx *Tx) error {
		if _, err := scopedDevice(tx, scope, deviceID); err != nil {
			return err
		}
		existing, err := one(tx, `SELECT `+commandCols+` FROM commands WHERE device_id = ? AND idempotency_key = ?`,
			[]any{deviceID, in.IdempotencyKey}, scanCommand)
		switch {
		case err == nil:
			if existing.Type != in.Type || !sameJSON(existing.Params, []byte(params)) {
				return conflictf("idempotency key already used for a different command")
			}
			cmd = existing
			return nil
		case !errors.Is(err, ErrNotFound):
			return err
		}
		id, now := tx.id(), tx.nowMS()
		if _, err := tx.Exec(`INSERT INTO commands (id, device_id, type, params, idempotency_key, created_by, expires_at, created_at, updated_at)
			VALUES (?,?,?,?,?,?,?,?,?)`, id, deviceID, in.Type, params, in.IdempotencyKey, nullStr(in.CreatedBy),
			now+ttl.Milliseconds(), now, now); err != nil {
			return err
		}
		created = true
		cmd, err = one(tx, `SELECT `+commandCols+` FROM commands WHERE id = ?`, []any{id}, scanCommand)
		return err
	})
	return cmd, created, err
}

// AgentNextCommands hands the authenticated device its pending commands, oldest
// first, up to limit (0 = 10), and marks them delivered. A queued command that
// has not expired is delivered once; one delivered but not acknowledged within
// two minutes is delivered again.
func (s *Store) AgentNextCommands(ctx context.Context, deviceID string, limit int) ([]Command, error) {
	if limit <= 0 {
		limit = 10
	}
	var out []Command
	err := s.Write(ctx, func(tx *Tx) error {
		now := tx.nowMS()
		ids, err := collect(tx, `SELECT id FROM commands WHERE device_id = ? AND expires_at > ?
				AND (status = 'queued' OR (status = 'delivered' AND delivered_at <= ?))
			ORDER BY created_at, id LIMIT ?`, []any{deviceID, now, now - redeliverAfter.Milliseconds(), limit},
			func(sc scanner) (string, error) {
				var id string
				return id, sc.Scan(&id)
			})
		if err != nil {
			return err
		}
		for _, id := range ids {
			if _, err := tx.Exec(`UPDATE commands SET status = 'delivered', delivered_at = ?, updated_at = ? WHERE id = ?`, now, now, id); err != nil {
				return err
			}
			c, err := one(tx, `SELECT `+commandCols+` FROM commands WHERE id = ?`, []any{id}, scanCommand)
			if err != nil {
				return err
			}
			out = append(out, c)
		}
		return nil
	})
	return out, err
}

// CommandAck is a device's report on a command.
type CommandAck struct {
	Status string          // acked | succeeded | failed
	Result json.RawMessage // optional JSON object, path-free
}

// AgentAckCommand records the device's report. It is idempotent: repeating the
// same status is a no-op; moving forward (delivered to acked, acked to
// succeeded or failed) is applied; moving a finished command to a different
// final status, or reporting on a cancelled command or on one that expired
// before it was ever delivered, is ErrConflict. A result for a delivered
// command that has since expired is recorded as a late result and the status
// stays expired (lateAck). A command that belongs to another device is
// ErrNotFound. The agent API answers 2xx for every one of these refusals
// (agentapi handleCommandResult): a deployed agent retries any error forever.
func (s *Store) AgentAckCommand(ctx context.Context, deviceID, id string, ack CommandAck) (Command, error) {
	switch ack.Status {
	case "acked", "succeeded", "failed":
	default:
		return Command{}, invalidf("ack status must be acked, succeeded or failed")
	}
	result, err := nullJSON(ack.Result)
	if err != nil {
		return Command{}, err
	}
	var out Command
	err = s.Write(ctx, func(tx *Tx) error {
		cur, err := one(tx, `SELECT `+commandCols+` FROM commands WHERE id = ? AND device_id = ?`, []any{id, deviceID}, scanCommand)
		if err != nil {
			return err
		}
		if cur.Status == ack.Status {
			out = cur
			return nil
		}
		now := tx.nowMS()
		switch {
		case ack.Status == "acked" && (cur.Status == "queued" || cur.Status == "delivered"):
			_, err = tx.Exec(`UPDATE commands SET status = 'acked', acked_at = ?, updated_at = ? WHERE id = ?`, now, now, id)
		case (ack.Status == "succeeded" || ack.Status == "failed") &&
			(cur.Status == "queued" || cur.Status == "delivered" || cur.Status == "acked"):
			_, err = tx.Exec(`UPDATE commands SET status = ?, acked_at = COALESCE(acked_at, ?), completed_at = ?, result = ?,
				updated_at = ? WHERE id = ?`, ack.Status, now, now, result, now, id)
		case cur.Status == "expired" && cur.DeliveredAt != nil:
			// M2-W2C: the device got the command and answered after the deadline passed.
			// Recorded, never an error (commands_m2.go lateAck).
			err = lateAck(tx, cur, ack, now)
		default:
			return conflictf("command is %s and cannot become %s", cur.Status, ack.Status)
		}
		if err != nil {
			return err
		}
		out, err = one(tx, `SELECT `+commandCols+` FROM commands WHERE id = ?`, []any{id}, scanCommand)
		return err
	})
	return out, err
}

// ExpireCommands marks queued or delivered commands past their expiry as
// expired (the command-expiry job) and returns how many.
func (s *Store) ExpireCommands(ctx context.Context) (int64, error) {
	var n int64
	err := s.Write(ctx, func(tx *Tx) error {
		now := tx.nowMS()
		res, err := tx.Exec(`UPDATE commands SET status = 'expired', updated_at = ?
			WHERE status IN ('queued','delivered') AND expires_at < ?`, now, now)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	return n, err
}

// ListCommands returns a device's commands newest first.
func (s *Store) ListCommands(ctx context.Context, scope Scope, deviceID string, page Page) (Listing[Command], error) {
	if err := scope.check(); err != nil {
		return Listing[Command]{}, err
	}
	limit, offset, err := page.window()
	if err != nil {
		return Listing[Command]{}, err
	}
	var items []Command
	err = s.Read(ctx, func(tx *Tx) error {
		if _, err := scopedDevice(tx, scope, deviceID); err != nil {
			return err
		}
		var err error
		items, err = collect(tx, `SELECT `+commandCols+` FROM commands WHERE device_id = ?
			ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?`, []any{deviceID, limit + 1, offset}, scanCommand)
		return err
	})
	if err != nil {
		return Listing[Command]{}, err
	}
	return finish(items, limit, offset), nil
}

// ---------------------------------------------------------------------------
// restore_requests
// ---------------------------------------------------------------------------

// RestoreRequest is a technician or device restore. The selection (paths) exists
// only as SelectionWrapped, an encrypted blob the console writes and later
// purges (nulls) when the restore is spent (a technician download: at
// ExpiresAt); the count and a hash persist for audit. This package only stores
// and returns the blob; it never decrypts it.
type RestoreRequest struct {
	ID               string
	DeviceID         string
	SnapshotID       string // 64 hex
	TargetDeviceID   string // relay restores only: the replacement device that pulls the artifact
	Mode             string // device_original | device_path | device_relay | download_zip | download_tar
	Overwrite        string // always | if-changed | if-newer | never ("" = if-changed)
	Verify           bool
	Simulate         bool
	SelectionCount   int
	SelectionHash    string
	SelectionWrapped []byte
	Status           string // queued | running | ready | delivered | failed | cancelled | expired | purged
	RequestedBy      string
	CommandID        string
	ArtifactRef      string // legacy: an archive an earlier version staged; technician downloads stream and never set it (ADR 0008)
	Bytes            *int64
	Progress         json.RawMessage
	ErrorClass       string
	StartedAt        *time.Time
	FinishedAt       *time.Time
	ExpiresAt        time.Time
	PurgeAfter       *time.Time
	DownloadCount    int
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

const restoreCols = `id, device_id, snapshot_id, target_device_id, mode, overwrite, verify, simulate, selection_count, selection_hash,
	selection_wrapped, status, requested_by, command_id, artifact_ref, bytes, progress, error_class, started_at, finished_at,
	expires_at, purge_after, download_count, created_at, updated_at`

func scanRestore(sc scanner) (RestoreRequest, error) {
	var r RestoreRequest
	var target, hash, by, cmd, art, progress, errc sql.NullString
	var wrapped []byte
	var verify, simulate int
	var bytesN, started, finished, purge sql.NullInt64
	var exp, c, u int64
	err := sc.Scan(&r.ID, &r.DeviceID, &r.SnapshotID, &target, &r.Mode, &r.Overwrite, &verify, &simulate, &r.SelectionCount,
		&hash, &wrapped, &r.Status, &by, &cmd, &art, &bytesN, &progress, &errc, &started, &finished, &exp, &purge,
		&r.DownloadCount, &c, &u)
	if err != nil {
		return RestoreRequest{}, err
	}
	r.TargetDeviceID, r.SelectionHash, r.SelectionWrapped = target.String, hash.String, wrapped
	r.Verify, r.Simulate = verify != 0, simulate != 0
	r.RequestedBy, r.CommandID, r.ArtifactRef, r.ErrorClass = by.String, cmd.String, art.String, errc.String
	r.Bytes, r.Progress, r.StartedAt, r.FinishedAt = ptrInt64(bytesN), rawOf(progress), ptrTime(started), ptrTime(finished)
	r.ExpiresAt, r.PurgeAfter = timeOf(exp), ptrTime(purge)
	r.CreatedAt, r.UpdatedAt = timeOf(c), timeOf(u)
	return r, nil
}

// CreateRestoreRequest inserts a restore for a device inside scope. For a relay
// restore the target device must be inside scope too and belong to the SAME
// customer (data never crosses customers). The schema's rules (verify excludes
// simulate, an artifact needs purge_after and a download or relay mode, relay
// iff a target different from the source) surface as ErrInvalid.
func (s *Store) CreateRestoreRequest(ctx context.Context, scope Scope, r RestoreRequest) (RestoreRequest, error) {
	return s.createRestoreRequest(ctx, scope, r, nil)
}

// CreateRestoreRequestAudited creates the restore request and writes its audit
// row (restore.request) in the same transaction, so the two commit together or
// not at all: ErrAudit when the row cannot be written, and no restore exists
// then. Nothing about the selection (path, hash) goes into the row.
func (s *Store) CreateRestoreRequestAudited(ctx context.Context, scope Scope, r RestoreRequest, actor AuditActor) (RestoreRequest, error) {
	return s.createRestoreRequest(ctx, scope, r, &actor)
}

func (s *Store) createRestoreRequest(ctx context.Context, scope Scope, r RestoreRequest, actor *AuditActor) (RestoreRequest, error) {
	if err := scope.check(); err != nil {
		return RestoreRequest{}, err
	}
	if r.Overwrite == "" {
		r.Overwrite = "if-changed"
	}
	if r.Status == "" {
		r.Status = "queued"
	}
	progress, err := nullJSON(r.Progress)
	if err != nil {
		return RestoreRequest{}, err
	}
	var out RestoreRequest
	err = s.Write(ctx, func(tx *Tx) error {
		cust, err := scopedDevice(tx, scope, r.DeviceID)
		if err != nil {
			return err
		}
		if r.TargetDeviceID != "" {
			tc, err := scopedDevice(tx, scope, r.TargetDeviceID)
			if err != nil {
				return err
			}
			if tc != cust {
				return invalidf("relay target belongs to a different customer")
			}
		}
		r.ID = tx.id()
		now := tx.nowMS()
		if _, err := tx.Exec(`INSERT INTO restore_requests (`+restoreCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,0,?,?)`,
			r.ID, r.DeviceID, r.SnapshotID, nullStr(r.TargetDeviceID), r.Mode, r.Overwrite, boolInt(r.Verify), boolInt(r.Simulate),
			r.SelectionCount, nullStr(r.SelectionHash), nullBytes(r.SelectionWrapped), r.Status, nullStr(r.RequestedBy),
			nullStr(r.CommandID), nullStr(r.ArtifactRef), nullInt64(r.Bytes), progress, nullStr(r.ErrorClass),
			nullTime(r.StartedAt), nullTime(r.FinishedAt), msOf(r.ExpiresAt), nullTime(r.PurgeAfter), now, now); err != nil {
			return err
		}
		if actor != nil {
			ev, err := actor.event("restore.request", "restore", r.ID, cust, map[string]any{"mode": r.Mode, "selection_count": r.SelectionCount})
			if err != nil {
				return err
			}
			if err := audited(tx, ev); err != nil {
				return err
			}
		}
		out, err = one(tx, `SELECT `+restoreCols+` FROM restore_requests WHERE id = ?`, []any{r.ID}, scanRestore)
		return err
	})
	return out, err
}

// restoreScoped selects a restore whose source device is inside scope.
func restoreScoped(tx *Tx, scope Scope, id string) (RestoreRequest, error) {
	clause, args := scope.clause("d.customer_id")
	return one(tx, `SELECT `+prefixCols(restoreCols, "r.")+` FROM restore_requests r JOIN devices d ON d.id = r.device_id
		WHERE r.id = ? AND `+clause, append([]any{id}, args...), scanRestore)
}

// GetRestoreRequest returns a restore whose source device is inside scope.
func (s *Store) GetRestoreRequest(ctx context.Context, scope Scope, id string) (RestoreRequest, error) {
	if err := scope.check(); err != nil {
		return RestoreRequest{}, err
	}
	var out RestoreRequest
	err := s.Read(ctx, func(tx *Tx) error {
		var err error
		out, err = restoreScoped(tx, scope, id)
		return err
	})
	return out, err
}

// ListRestoreRequests returns a device's restores newest first.
func (s *Store) ListRestoreRequests(ctx context.Context, scope Scope, deviceID string, page Page) (Listing[RestoreRequest], error) {
	if err := scope.check(); err != nil {
		return Listing[RestoreRequest]{}, err
	}
	limit, offset, err := page.window()
	if err != nil {
		return Listing[RestoreRequest]{}, err
	}
	var items []RestoreRequest
	err = s.Read(ctx, func(tx *Tx) error {
		if _, err := scopedDevice(tx, scope, deviceID); err != nil {
			return err
		}
		var err error
		items, err = collect(tx, `SELECT `+restoreCols+` FROM restore_requests WHERE device_id = ?
			ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?`, []any{deviceID, limit + 1, offset}, scanRestore)
		return err
	})
	if err != nil {
		return Listing[RestoreRequest]{}, err
	}
	return finish(items, limit, offset), nil
}

// ListExpiredRestores returns the technician downloads (mode download_zip) the
// restore-purge job must act on at now (ADR 0008: restores stream, nothing is
// staged):
//
//   - every row that still names a server-side archive (artifact_ref set): an
//     earlier version staged it, and the job deletes the file at once, whatever
//     the row's status or purge date;
//   - every row that still holds its encrypted selection and is spent: past
//     expires_at, or in a status a download can no longer start from (anything
//     but ready and delivered).
//
// The job clears artifact_ref and the selection (and expires the row) through
// UpdateRestoreRequest, so a handled restore is not listed again. It is a job
// method: no Scope.
func (s *Store) ListExpiredRestores(ctx context.Context, now time.Time) ([]RestoreRequest, error) {
	var out []RestoreRequest
	err := s.Read(ctx, func(tx *Tx) error {
		var err error
		out, err = collect(tx, `SELECT `+restoreCols+` FROM restore_requests
			WHERE mode = 'download_zip'
			  AND (artifact_ref IS NOT NULL
			       OR (selection_wrapped IS NOT NULL AND (expires_at <= ? OR status NOT IN ('ready','delivered'))))
			ORDER BY created_at, id`, []any{msOf(now)}, scanRestore)
		return err
	})
	return out, err
}

// RestoreUpdate changes a restore's progress fields; nil fields are left alone.
type RestoreUpdate struct {
	Status         *string
	ArtifactRef    *string // "" clears
	PurgeAfter     *time.Time
	Bytes          *int64
	Progress       json.RawMessage
	ErrorClass     *string
	StartedAt      *time.Time
	FinishedAt     *time.Time
	CommandID      *string
	CountDownload  bool // increments download_count
	ClearSelection bool // nulls selection_wrapped (the purge)
	SelectionHash  *string
	SelectionCount *int
}

// UpdateRestoreRequest applies u to a restore whose source device is inside scope.
func (s *Store) UpdateRestoreRequest(ctx context.Context, scope Scope, id string, u RestoreUpdate) (RestoreRequest, error) {
	if err := scope.check(); err != nil {
		return RestoreRequest{}, err
	}
	progress, err := nullJSON(u.Progress)
	if err != nil {
		return RestoreRequest{}, err
	}
	var out RestoreRequest
	err = s.Write(ctx, func(tx *Tx) error {
		if _, err := restoreScoped(tx, scope, id); err != nil {
			return err
		}
		now := tx.nowMS()
		sets, args := []string{"updated_at = ?"}, []any{now}
		add := func(col string, v any) { sets, args = append(sets, col+" = ?"), append(args, v) }
		if u.Status != nil {
			add("status", *u.Status)
		}
		if u.ArtifactRef != nil {
			add("artifact_ref", nullStr(*u.ArtifactRef))
		}
		if u.PurgeAfter != nil {
			add("purge_after", msOf(*u.PurgeAfter))
		}
		if u.Bytes != nil {
			add("bytes", *u.Bytes)
		}
		if progress != nil {
			add("progress", progress)
		}
		if u.ErrorClass != nil {
			add("error_class", nullStr(*u.ErrorClass))
		}
		if u.StartedAt != nil {
			add("started_at", msOf(*u.StartedAt))
		}
		if u.FinishedAt != nil {
			add("finished_at", msOf(*u.FinishedAt))
		}
		if u.CommandID != nil {
			add("command_id", nullStr(*u.CommandID))
		}
		if u.SelectionHash != nil {
			add("selection_hash", nullStr(*u.SelectionHash))
		}
		if u.SelectionCount != nil {
			add("selection_count", *u.SelectionCount)
		}
		if u.ClearSelection {
			add("selection_wrapped", nil)
		}
		q := `UPDATE restore_requests SET ` + strings.Join(sets, ", ")
		if u.CountDownload {
			q += `, download_count = download_count + 1`
		}
		if _, err := tx.Exec(q+` WHERE id = ?`, append(args, id)...); err != nil {
			return err
		}
		var err error
		out, err = one(tx, `SELECT `+restoreCols+` FROM restore_requests WHERE id = ?`, []any{id}, scanRestore)
		return err
	})
	return out, err
}

// AgentGetRestoreRequest returns a restore the authenticated device may act on:
// the device that is restored (device_id) or, for a relay, the replacement
// device that pulls the artifact (target_device_id). Only a live restore is
// served: status queued, running or ready, expires_at in the future and
// purge_after unset or in the future. Anything else (delivered, failed,
// cancelled, expired, purged, past its expiry or purge date, another device's)
// is ErrNotFound, so a spent restore's selection blob and artifact reference
// are never handed out again.
func (s *Store) AgentGetRestoreRequest(ctx context.Context, deviceID, id string) (RestoreRequest, error) {
	var out RestoreRequest
	err := s.Read(ctx, func(tx *Tx) error {
		now := tx.nowMS()
		var err error
		out, err = one(tx, `SELECT `+restoreCols+` FROM restore_requests
			WHERE id = ? AND (device_id = ? OR target_device_id = ?)
			  AND status IN ('queued','running','ready') AND expires_at > ? AND (purge_after IS NULL OR purge_after > ?)`,
			[]any{id, deviceID, deviceID, now, now}, scanRestore)
		return err
	})
	return out, err
}
