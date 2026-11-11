package journal

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
)

// OutboxItem is one run record waiting for delivery. Seq changes every time the
// body is replaced (an open record is replaced by the closed one), so a
// delivery of an old body can never delete a newer one.
type OutboxItem struct {
	RunID    string
	Seq      int64
	Body     api.RunRecord
	Closed   bool
	Attempts int
	NextAt   time.Time
}

// enqueue inserts or replaces the outbox entry of a run.
func enqueue(ctx context.Context, tx *sql.Tx, runID, body string, closed bool, now int64) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO outbox (run_id, seq, body, closed, attempts, next_at, created_at)
		VALUES (?, 1, ?, ?, 0, 0, ?)
		ON CONFLICT(run_id) DO UPDATE SET seq = seq + 1, body = excluded.body, closed = excluded.closed,
			attempts = 0, next_at = 0`, runID, body, boolInt(closed), now)
	return err
}

// OutboxDue returns up to limit entries whose retry time has passed, oldest
// first. Open records come before closed ones of later runs only by age.
func (j *Journal) OutboxDue(ctx context.Context, now time.Time, limit int) ([]OutboxItem, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := j.db.QueryContext(ctx, `SELECT run_id, seq, body, closed, attempts, next_at FROM outbox
		WHERE next_at <= ? ORDER BY created_at, run_id LIMIT ?`, ms(now), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OutboxItem
	for rows.Next() {
		var (
			it     OutboxItem
			body   string
			closed int
			next   int64
		)
		if err := rows.Scan(&it.RunID, &it.Seq, &body, &closed, &it.Attempts, &next); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(body), &it.Body); err != nil {
			return nil, err
		}
		it.Closed = closed != 0
		it.NextAt = fromMS(next)
		out = append(out, it)
	}
	return out, rows.Err()
}

// OutboxNext is the earliest retry time among waiting entries.
func (j *Journal) OutboxNext(ctx context.Context) (time.Time, bool, error) {
	var next sql.NullInt64
	if err := j.db.QueryRowContext(ctx, `SELECT MIN(next_at) FROM outbox`).Scan(&next); err != nil {
		return time.Time{}, false, err
	}
	if !next.Valid {
		return time.Time{}, false, nil
	}
	return fromMS(next.Int64), true, nil
}

// OutboxDepth counts undelivered closed results (the heartbeat's queue_depth).
func (j *Journal) OutboxDepth(ctx context.Context) (int, error) {
	var n int
	err := j.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM outbox WHERE closed = 1`).Scan(&n)
	return n, err
}

// MarkDelivered records a 2xx created, updated or duplicate answer for the body
// with this sequence number. If the body was replaced meanwhile the entry stays
// and the newer body is sent next. A delivered closed record sets
// runs.delivered_at.
func (j *Journal) MarkDelivered(ctx context.Context, runID string, seq int64) error {
	return j.tx(ctx, func(tx *sql.Tx) error {
		var closed int
		err := tx.QueryRowContext(ctx, `SELECT closed FROM outbox WHERE run_id = ? AND seq = ?`, runID, seq).Scan(&closed)
		if err == sql.ErrNoRows {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM outbox WHERE run_id = ? AND seq = ?`, runID, seq); err != nil {
			return err
		}
		if closed != 0 {
			_, err = tx.ExecContext(ctx, `UPDATE runs SET delivered_at = ? WHERE id = ?`, j.now(), runID)
		}
		return err
	})
}

// MarkRejected records a "rejected" answer: the reason is kept on the run row
// (a fixed code from the console, never text) and the entry is removed. A
// rejected record is not retried.
func (j *Journal) MarkRejected(ctx context.Context, runID string, seq int64, reason string) error {
	if len(reason) > 64 {
		reason = reason[:64]
	}
	return j.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM outbox WHERE run_id = ? AND seq = ?`, runID, seq)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return nil
		}
		_, err = tx.ExecContext(ctx, `UPDATE runs SET reject_reason = ? WHERE id = ?`, reason, runID)
		return err
	})
}

// OutboxFailed records a failed delivery attempt: the attempt counter goes up
// and the entry waits until next.
func (j *Journal) OutboxFailed(ctx context.Context, runID string, seq int64, next time.Time) error {
	_, err := j.db.ExecContext(ctx, `UPDATE outbox SET attempts = attempts + 1, next_at = ? WHERE run_id = ? AND seq = ?`,
		ms(next), runID, seq)
	return err
}

// KVGet reads one key.
func (j *Journal) KVGet(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := j.db.QueryRowContext(ctx, `SELECT v FROM kv WHERE k = ?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	return v, err == nil, err
}

// KVSet writes one key. Secrets are never stored here: the kv table holds
// etags, offsets, flags and the last-known-good policy.
func (j *Journal) KVSet(ctx context.Context, key, value string) error {
	_, err := j.db.ExecContext(ctx, `INSERT INTO kv (k, v, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(k) DO UPDATE SET v = excluded.v, updated_at = excluded.updated_at`, key, value, j.now())
	return err
}

// KVDelete removes one key.
func (j *Journal) KVDelete(ctx context.Context, key string) error {
	_, err := j.db.ExecContext(ctx, `DELETE FROM kv WHERE k = ?`, key)
	return err
}

// Well-known kv keys.
const (
	KVPolicyETag      = "policy_etag"
	KVPolicyJSON      = "policy_json" // last-known-good api.PolicyResponse without secrets
	KVClockOffsetMS   = "clock_offset_ms"
	KVLastHeartbeat   = "last_heartbeat" // RFC 3339 device time of the last successful heartbeat
	KVGatewayAckPend  = "gateway_ack_pending"
	KVStopMarker      = "stop_marker" // set on a clean stop request, read on the next start
	KVLastErrorClass  = "last_error_class"
	KVServiceState    = "service_state"
	KVHeartbeatServer = "last_heartbeat_server_time"
	// KVInsecureState is set (to the warning, no path) when the last start ran
	// with --insecure-unprotected-state, and deleted on a start without it.
	KVInsecureState = "insecure_unprotected_state"
	// KVPlannedStart is the running scheduler's next planned start, a
	// PlannedStart in JSON (L-STATUS-1): set while it waits (startup delay,
	// jitter, backoff, window, the next slot), deleted while a backup runs,
	// when it cannot schedule, and on a clean stop.
	KVPlannedStart = "planned_start"
	// KVTokenRefusedSince is set to the time of the first of the three 401s
	// (RFC 3339 UTC) while the console refuses the device token (ADR 0018), and
	// deleted when a heartbeat is accepted again and on every agent start.
	KVTokenRefusedSince = "token_refused_since"
)

// PlannedStart is the value of KVPlannedStart: when the scheduler plans to
// look again (and start, if nothing changes) and why it waits until then (a
// scheduler reason: startup_delay, jitter, retry_wait, window, lease_wait,
// hold or one of policy.Due's). At is UTC.
type PlannedStart struct {
	At     time.Time `json:"at"`
	Reason string    `json:"reason"`
}

// CommandRow is one entry of the command log.
type CommandRow struct {
	ID         string
	Type       string
	Params     string
	ReceivedAt time.Time
	Status     string // "received" until finished, then acked|succeeded|failed
	ErrorClass string
	Result     string
	Posted     bool // the console acknowledged the result (Refused is false)
	// Refused: the console answered the result with a final 4xx; the result
	// was dropped and is never re-posted (MarkCommandRefused).
	Refused bool
}

// Status of a command that has been received but not finished.
const CommandReceived = "received"

// RecordCommand inserts a command into the command log. created is false when
// the id was already logged (the command is not executed twice) and row is the
// existing entry.
func (j *Journal) RecordCommand(ctx context.Context, id, typ string, params []byte) (row CommandRow, created bool, err error) {
	res, err := j.db.ExecContext(ctx, `INSERT OR IGNORE INTO commands (id, type, params, received_at, status)
		VALUES (?, ?, ?, ?, ?)`, id, typ, string(params), j.now(), CommandReceived)
	if err != nil {
		return CommandRow{}, false, err
	}
	n, _ := res.RowsAffected()
	row, ok, err := j.Command(ctx, id)
	if err != nil || !ok {
		return CommandRow{}, false, err
	}
	return row, n == 1, nil
}

// Command returns one command log entry.
func (j *Journal) Command(ctx context.Context, id string) (CommandRow, bool, error) {
	rows, err := j.queryCommands(ctx, `SELECT id, type, params, received_at, status, error_class, result, posted
		FROM commands WHERE id = ?`, id)
	if err != nil || len(rows) == 0 {
		return CommandRow{}, false, err
	}
	return rows[0], true, nil
}

// FinishCommand stores the outcome of a command.
func (j *Journal) FinishCommand(ctx context.Context, id, status, errorClass string, result []byte) error {
	_, err := j.db.ExecContext(ctx, `UPDATE commands SET status = ?, error_class = ?, result = ?, finished_at = ? WHERE id = ?`,
		status, errorClass, string(result), j.now(), id)
	return err
}

// MarkCommandPosted records that the console acknowledged the result.
func (j *Journal) MarkCommandPosted(ctx context.Context, id string) error {
	_, err := j.db.ExecContext(ctx, `UPDATE commands SET posted = ? WHERE id = ?`, commandPosted, id)
	return err
}

// posted column values: 0 not yet, 1 acknowledged, 2 refused by the console.
const (
	commandPosted  = 1
	commandRefused = 2
)

// MarkCommandRefused records that the console refused the result with a final
// 4xx. The result is dropped: UnpostedCommands no longer returns it, so it
// cannot hold up the command loop. The row stays in the log for the device
// support bundle.
func (j *Journal) MarkCommandRefused(ctx context.Context, id string) error {
	_, err := j.db.ExecContext(ctx, `UPDATE commands SET posted = ? WHERE id = ?`, commandRefused, id)
	return err
}

// UnpostedCommands returns finished commands whose result the console has not
// acknowledged yet, oldest first (re-posted after a restart or an outage).
func (j *Journal) UnpostedCommands(ctx context.Context) ([]CommandRow, error) {
	return j.queryCommands(ctx, `SELECT id, type, params, received_at, status, error_class, result, posted
		FROM commands WHERE posted = 0 AND status != ? ORDER BY received_at`, CommandReceived)
}

// RecentCommands returns the newest limit entries.
func (j *Journal) RecentCommands(ctx context.Context, limit int) ([]CommandRow, error) {
	return j.queryCommands(ctx, `SELECT id, type, params, received_at, status, error_class, result, posted
		FROM commands ORDER BY received_at DESC, id DESC LIMIT ?`, limit)
}

func (j *Journal) queryCommands(ctx context.Context, q string, args ...any) ([]CommandRow, error) {
	rows, err := j.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CommandRow
	for rows.Next() {
		var (
			r      CommandRow
			rec    int64
			posted int
		)
		if err := rows.Scan(&r.ID, &r.Type, &r.Params, &rec, &r.Status, &r.ErrorClass, &r.Result, &posted); err != nil {
			return nil, err
		}
		r.ReceivedAt = fromMS(rec)
		r.Posted = posted == commandPosted
		r.Refused = posted == commandRefused
		out = append(out, r)
	}
	return out, rows.Err()
}
