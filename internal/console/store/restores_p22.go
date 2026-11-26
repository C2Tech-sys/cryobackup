package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Restore to the original device (ADR 0027, ticket P22-E): the store half.
//
// A device restore is one restore_requests row and one restore command, created
// together (CreateDeviceRestoreAudited); the agent fetches the selection with its
// device token (AgentDeviceRestore, then AgentMarkRestoreFetched once the console
// could read it); the agent's restore run closes the row when its record arrives
// (closeDeviceRestore, called by AgentIngestRuns in the same transaction); a row
// the device never picked up is closed by the restore-purge job
// (ExpireDeviceRestores); a running row whose agent stopped reporting it is
// closed by the abandoned-run-sweeper (jobs_sweep.go, ADR 0032).
//
// The selection itself never reaches this package in clear: SelectionWrapped is
// the master-key-wrapped blob the restores service writes, and it is cleared the
// moment the row closes. No audit detail, error or constraint message here names
// a path.

// Stored values of a device restore (restore_requests CHECK lists,
// migrations/0001_initial.sql). They are the database's vocabulary, not the
// wire's: the restores service maps api.DeviceRestoreMode device_side to
// RestoreModeDevicePath and api.DeviceOverwriteIfChanged to
// RestoreOverwriteIfChanged.
const (
	RestoreModeDeviceOriginal  = "device_original"
	RestoreModeDevicePath      = "device_path"
	RestoreModeDownloadZip     = "download_zip"
	RestoreOverwriteNever      = "never"
	RestoreOverwriteIfChanged  = "if-changed"
	restoreCommandType         = "restore"
	restoreCommandKeyPrefix    = "restore:"
	maxDeviceRestoreSelections = 100 // api.MaxRestorePaths; restated so the store does not import the wire package
)

// Refusals of a device restore. Each is an ErrConflict (or, for the expiry, a
// value of its own) so a caller can name the rule that applied; ErrDeviceRetired
// (commands_m2.go) is the retired refusal.
var (
	// ErrRestoreCustomerHeld: the device's repository key is held by the customer,
	// so no restore of any kind is offered (DH22).
	ErrRestoreCustomerHeld = conflictf("device key is held by the customer")
	// ErrRestoreDevicePurging: the device is being purged (ADR 0019).
	ErrRestoreDevicePurging = conflictf("device is being purged")
	// ErrDeviceRestoreActive: the device already has a device restore queued or
	// running (ADR 0027: one at a time).
	ErrDeviceRestoreActive = conflictf("a device restore is already active for this device")
	// ErrRestoreExpired: the request waited past its expiry (24 hours, ruling C6).
	ErrRestoreExpired = conflictf("restore request has expired")
)

// IsDeviceRestoreMode reports whether a stored mode is one of the two device modes.
func IsDeviceRestoreMode(mode string) bool {
	return mode == RestoreModeDeviceOriginal || mode == RestoreModeDevicePath
}

// DeviceRestoreInput is a device restore to create. Mode and Overwrite are stored
// values. SelectionWrapped is the wrapped selection; SelectionCount and
// SelectionHash describe it for the audit trail.
type DeviceRestoreInput struct {
	DeviceID         string
	SnapshotID       string
	Mode             string // RestoreModeDeviceOriginal | RestoreModeDevicePath
	Overwrite        string // RestoreOverwriteNever | RestoreOverwriteIfChanged (original only)
	SelectionCount   int
	SelectionHash    string
	SelectionWrapped []byte
	ExpiresAt        time.Time
}

// validate applies the rules of a device restore before the database sees it.
func (in DeviceRestoreInput) validate() error {
	switch {
	case !IsDeviceRestoreMode(in.Mode):
		return invalidf("restore mode is not a device mode")
	case in.Overwrite != RestoreOverwriteNever && in.Overwrite != RestoreOverwriteIfChanged:
		return invalidf("restore overwrite is not never or if-changed")
	case in.Mode == RestoreModeDevicePath && in.Overwrite != RestoreOverwriteNever:
		return invalidf("the side folder is new and empty: overwrite must be never")
	case len(in.SelectionWrapped) == 0:
		return invalidf("a device restore needs its wrapped selection")
	case in.SelectionCount < 1 || in.SelectionCount > maxDeviceRestoreSelections:
		return invalidf("selection count out of range")
	case len(in.SnapshotID) != 64:
		return invalidf("snapshot id must be 64 hex characters")
	case in.ExpiresAt.IsZero():
		return invalidf("expiry is required")
	}
	return nil
}

// restoreGate reads what decides whether a device restore may be created or
// served: the device row (ErrNotFound when it is not in scope) and the custody of
// its active escrow row. It returns the refusal that applies, in a fixed order:
// purging, retired, customer-held. A device with no active escrow row is
// refused as customer-held: without an escrowed key the console has no business
// restoring it, and failing closed costs nothing.
func restoreGate(tx *Tx, scope Scope, deviceID string) (Device, error) {
	dev, err := getDevice(tx, scope, deviceID)
	if err != nil {
		return Device{}, err
	}
	switch {
	case dev.Purging():
		return dev, ErrRestoreDevicePurging
	case dev.RetiredAt != nil:
		return dev, ErrDeviceRetired
	}
	var custody string
	err = tx.QueryRow(`SELECT custody FROM repo_key_escrow WHERE device_id = ? AND retired_at IS NULL`, deviceID).Scan(&custody)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return dev, ErrRestoreCustomerHeld
	case err != nil:
		return dev, err
	case custody != "escrowed":
		return dev, ErrRestoreCustomerHeld
	}
	return dev, nil
}

// activeDeviceRestore reports whether the device has a device restore that is
// still in flight at now: queued, not past its expiry and with a command that
// was not cancelled, or running (the agent fetched it; its run record closes it,
// or the abandoned-run-sweeper once the agent stops reporting it, ADR 0032).
func activeDeviceRestore(tx *Tx, deviceID string, now int64) (bool, error) {
	var n int
	err := tx.QueryRow(`SELECT count(*) FROM restore_requests r LEFT JOIN commands c ON c.id = r.command_id
		WHERE r.device_id = ? AND r.mode IN ('device_original','device_path')
		  AND ((r.status = 'queued' AND r.expires_at > ? AND COALESCE(c.status, '') <> 'cancelled') OR r.status = 'running')`,
		deviceID, now).Scan(&n)
	return n > 0, err
}

// CreateDeviceRestoreAudited creates a device restore for a device inside scope:
// the restore_requests row (status queued, the selection wrapped, the expiry of
// in.ExpiresAt), the restore command whose params carry only the request id
// ({"restore_request_id": id}, expiring with the row) and the audit row
// restore.create, in one transaction. A failed audit write refuses the whole
// change (ErrAudit) and nothing is created.
//
// Refusals: ErrNotFound (device outside scope), ErrRestoreDevicePurging,
// ErrDeviceRetired, ErrRestoreCustomerHeld, ErrDeviceRestoreActive (one active
// device restore per device), ErrInvalid for input the rules refuse. The
// command is created only here, never through the operator command queue
// (ADR 0010, ADR 0027).
func (s *Store) CreateDeviceRestoreAudited(ctx context.Context, scope Scope, in DeviceRestoreInput, actor AuditActor) (RestoreRequest, Command, error) {
	if err := scope.check(); err != nil {
		return RestoreRequest{}, Command{}, err
	}
	if err := in.validate(); err != nil {
		return RestoreRequest{}, Command{}, err
	}
	var row RestoreRequest
	var cmd Command
	err := s.Write(ctx, func(tx *Tx) error {
		dev, err := restoreGate(tx, scope, in.DeviceID)
		if err != nil {
			return err
		}
		now := tx.nowMS()
		if in.ExpiresAt.UnixMilli() <= now {
			return invalidf("expiry must be in the future")
		}
		active, err := activeDeviceRestore(tx, in.DeviceID, now)
		if err != nil {
			return err
		}
		if active {
			return ErrDeviceRestoreActive
		}
		restoreID, cmdID := tx.id(), tx.id()
		params, err := json.Marshal(map[string]string{"restore_request_id": restoreID})
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO commands (id, device_id, type, params, idempotency_key, created_by, expires_at, created_at, updated_at)
			VALUES (?,?,?,?,?,?,?,?,?)`, cmdID, in.DeviceID, restoreCommandType, string(params), restoreCommandKeyPrefix+restoreID,
			nullStr(actor.AdminID), msOf(in.ExpiresAt), now, now); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO restore_requests (id, device_id, snapshot_id, mode, overwrite, selection_count, selection_hash,
				selection_wrapped, status, requested_by, command_id, expires_at, created_at, updated_at)
			VALUES (?,?,?,?,?,?,?,?,'queued',?,?,?,?,?)`,
			restoreID, in.DeviceID, in.SnapshotID, in.Mode, in.Overwrite, in.SelectionCount, nullStr(in.SelectionHash),
			in.SelectionWrapped, nullStr(actor.AdminID), cmdID, msOf(in.ExpiresAt), now, now); err != nil {
			return err
		}
		ev, err := actor.event("restore.create", "restore", restoreID, dev.CustomerID, map[string]any{
			"mode": in.Mode, "overwrite": in.Overwrite, "selection_count": in.SelectionCount, "command_id": cmdID, "device_id": in.DeviceID})
		if err != nil {
			return err
		}
		if err := audited(tx, ev); err != nil {
			return err
		}
		if row, err = one(tx, `SELECT `+restoreCols+` FROM restore_requests WHERE id = ?`, []any{restoreID}, scanRestore); err != nil {
			return err
		}
		cmd, err = one(tx, `SELECT `+commandCols+` FROM commands WHERE id = ?`, []any{cmdID}, scanCommand)
		return err
	})
	if err != nil {
		return RestoreRequest{}, Command{}, err
	}
	return row, cmd, nil
}

// fetchable judges a device restore the authenticated device asks for, inside
// tx at now. Another device's restore, a download, a finished or purged row and
// an unknown id are all ErrNotFound, alike, so a device token learns nothing
// about a restore that is not its own. Then: the device being purged
// (ErrRestoreDevicePurging), retired (ErrDeviceRetired) or customer-held
// (ErrRestoreCustomerHeld); a request past its expiry, or expired by the job,
// ErrRestoreExpired.
func fetchable(tx *Tx, deviceID, id string, now int64) (RestoreRequest, error) {
	r, err := one(tx, `SELECT `+restoreCols+` FROM restore_requests WHERE id = ? AND device_id = ?`, []any{id, deviceID}, scanRestore)
	if err != nil {
		return RestoreRequest{}, err
	}
	if !IsDeviceRestoreMode(r.Mode) {
		return RestoreRequest{}, ErrNotFound
	}
	if _, err := restoreGate(tx, ScopeAll(), deviceID); err != nil {
		return RestoreRequest{}, err
	}
	switch {
	case r.Status == "expired":
		return RestoreRequest{}, ErrRestoreExpired
	case r.Status != "queued" && r.Status != "running":
		return RestoreRequest{}, ErrNotFound
	case r.ExpiresAt.UnixMilli() <= now:
		return RestoreRequest{}, ErrRestoreExpired
	case len(r.SelectionWrapped) == 0:
		return RestoreRequest{}, ErrNotFound
	}
	return r, nil
}

// AgentDeviceRestore returns the device restore id of the authenticated device,
// with its wrapped selection, when the device may fetch it now (see fetchable
// for every refusal). It changes nothing: the console first proves it can read
// the selection, then records the fetch with AgentMarkRestoreFetched.
func (s *Store) AgentDeviceRestore(ctx context.Context, deviceID, id string) (RestoreRequest, error) {
	var out RestoreRequest
	err := s.Read(ctx, func(tx *Tx) error {
		var err error
		out, err = fetchable(tx, deviceID, id, tx.nowMS())
		return err
	})
	return out, err
}

// AgentMarkRestoreFetched records that the authenticated device fetched its
// restore: the row is judged again (the same refusals as AgentDeviceRestore),
// becomes running (started_at the first fetch's time) and the audit row
// restore.fetch is written, path-free, in the same transaction; a failed audit
// write refuses (ErrAudit) and nothing changes. A repeated fetch of a running
// restore (a lost answer) is recorded again.
func (s *Store) AgentMarkRestoreFetched(ctx context.Context, deviceID, id, sourceIP string) (RestoreRequest, error) {
	var out RestoreRequest
	err := s.Write(ctx, func(tx *Tx) error {
		r, err := fetchable(tx, deviceID, id, tx.nowMS())
		if err != nil {
			return err
		}
		now := tx.nowMS()
		if _, err := tx.Exec(`UPDATE restore_requests SET status = 'running', started_at = COALESCE(started_at, ?), updated_at = ? WHERE id = ?`,
			now, now, id); err != nil {
			return err
		}
		var cust string
		if err := tx.QueryRow(`SELECT customer_id FROM devices WHERE id = ?`, deviceID).Scan(&cust); err != nil {
			return err
		}
		details, _ := json.Marshal(map[string]any{"mode": r.Mode, "status_before": r.Status, "selection_count": r.SelectionCount})
		if err := audited(tx, AuditEvent{ActorType: "device", ActorID: deviceID, Method: "agent", SourceIP: sourceIP, Action: "restore.fetch",
			ObjectType: "restore", ObjectID: id, CustomerID: cust, Result: "ok", Details: details}); err != nil {
			return err
		}
		out, err = one(tx, `SELECT `+restoreCols+` FROM restore_requests WHERE id = ?`, []any{id}, scanRestore)
		return err
	})
	return out, err
}

// restoreRunStats is the restore block of a run's stats JSON (api.RunStats,
// ADR 0027), read here without the wire package: counts and fixed class words,
// never a path.
type restoreRunStats struct {
	FilesRestored int64  `json:"files_restored"`
	FilesSkipped  int64  `json:"files_skipped"`
	BytesRestored uint64 `json:"bytes_restored"`
	BytesSkipped  uint64 `json:"bytes_skipped"`
	RestoreResult string `json:"restore_result"`
	RestoreErrors []struct {
		Class string `json:"class"`
		Count int64  `json:"count"`
	} `json:"restore_errors"`
}

// DeviceRestoreProgress is the progress document a closed device restore keeps
// (restore_requests.progress): the run's outcome and counts, and the error
// counts by class. It carries no path and no message.
type DeviceRestoreProgress struct {
	Outcome       string                `json:"outcome"`
	FilesRestored int64                 `json:"files_restored"`
	FilesSkipped  int64                 `json:"files_skipped"`
	BytesRestored uint64                `json:"bytes_restored"`
	BytesSkipped  uint64                `json:"bytes_skipped"`
	Errors        []RestoreClassCounted `json:"errors,omitempty"`
	// ClosedBy is set when the console, not the agent's run record, closed the
	// restore (the abandoned-run-sweeper, jobs_sweep.go, ADR 0032): the reason
	// word (RestoreClosedAgentSilent, RestoreClosedAgentRestarted,
	// RestoreClosedNoRunRecord). Empty for a row its run record closed.
	ClosedBy string `json:"closed_by,omitempty"`
	// Late is the agent's own report of a restore the console had already
	// closed (ADR 0032): recorded beside the console's verdict, never reopening
	// the row. Nil until such a report arrives.
	Late *DeviceRestoreLate `json:"late,omitempty"`
}

// Reasons the console closes a running device restore whose agent stopped
// reporting it (DeviceRestoreProgress.ClosedBy and the audit row's "reason").
const (
	RestoreClosedAgentSilent    = "agent_silent"
	RestoreClosedAgentRestarted = "agent_restarted"
	RestoreClosedNoRunRecord    = "no_run_record"
)

// DeviceRestoreLate is what the agent reported for a restore after the console
// had closed it (ADR 0032): the status and class its run record would have
// closed the row with, and its counts. No path, no message.
type DeviceRestoreLate struct {
	Outcome       string                `json:"outcome"`
	Status        string                `json:"status"`
	Class         string                `json:"class,omitempty"`
	FilesRestored int64                 `json:"files_restored"`
	FilesSkipped  int64                 `json:"files_skipped"`
	BytesRestored uint64                `json:"bytes_restored"`
	Errors        []RestoreClassCounted `json:"errors,omitempty"`
	ReceivedAt    time.Time             `json:"received_at"`
}

// RestoreClassCounted is one class and how many files (or, for
// nothing_matched, restic runs) ended in it.
type RestoreClassCounted struct {
	Class string `json:"class"`
	Count int64  `json:"count"`
}

// RestoreClassPartial is the error class of a restore that placed files and
// had errors (ADR 0027: RESTORED WITH ERRORS).
const RestoreClassPartial = "partial"

// restoreClose is how a run outcome closes a device restore: the row's status
// and error class.
func restoreClose(outcome, result, runClass string) (status, class string) {
	switch outcome {
	case "success":
		return "delivered", ""
	case "partial":
		return "delivered", RestoreClassPartial
	case "cancelled":
		return "cancelled", "cancelled"
	case "interrupted":
		return "failed", "interrupted"
	}
	switch {
	case result != "":
		return "failed", result
	case runClass != "":
		return "failed", runClass
	case outcome != "failed" && outcome != "":
		return "failed", outcome // skipped, deferred, abandoned: never delivered
	}
	return "failed", "unknown"
}

// closeDeviceRestore closes the device restore whose id is the run's id when the
// agent's restore run arrives (ADR 0027: the run id is the restore id). It runs
// inside AgentIngestRuns' transaction, so the run and the row commit together.
// An open run marks a queued row running; a closed run moves a queued or running
// row to delivered, failed or cancelled with its class and counts, clears the
// wrapped selection and writes the audit row restore.finish (a failed audit
// write fails the batch, and the agent resends it). A row of another device and
// a download are left alone. A row already closed is left as it is; when the
// console closed it (the sweep, ADR 0032) the agent's closed record is kept
// beside it (recordLateRestoreReport).
func closeDeviceRestore(tx *Tx, deviceID string, r RunInput) error {
	cur, err := one(tx, `SELECT `+restoreCols+` FROM restore_requests WHERE id = ? AND device_id = ?`, []any{r.ID, deviceID}, scanRestore)
	switch {
	case errors.Is(err, ErrNotFound):
		return nil
	case err != nil:
		return err
	}
	if !IsDeviceRestoreMode(cur.Mode) {
		return nil
	}
	if cur.Status != "queued" && cur.Status != "running" {
		return recordLateRestoreReport(tx, deviceID, cur, r)
	}
	now := tx.nowMS()
	if r.Outcome == "" {
		if cur.Status == "queued" {
			_, err := tx.Exec(`UPDATE restore_requests SET status = 'running', started_at = COALESCE(started_at, ?), updated_at = ? WHERE id = ?`, now, now, r.ID)
			return err
		}
		return nil
	}
	status, class, prog := restoreVerdictOf(r)
	raw, err := json.Marshal(prog)
	if err != nil {
		return err
	}
	bytes := int64(prog.BytesRestored)
	if _, err := tx.Exec(`UPDATE restore_requests SET status = ?, error_class = ?, progress = ?, bytes = ?, selection_wrapped = NULL,
			started_at = COALESCE(started_at, ?), finished_at = ?, updated_at = ? WHERE id = ?`,
		status, nullStr(class), string(raw), bytes, now, now, now, r.ID); err != nil {
		return err
	}
	var cust string
	if err := tx.QueryRow(`SELECT customer_id FROM devices WHERE id = ?`, deviceID).Scan(&cust); err != nil {
		return err
	}
	details, _ := json.Marshal(map[string]any{"status": status, "error_class": class, "outcome": r.Outcome,
		"files_restored": prog.FilesRestored, "files_skipped": prog.FilesSkipped, "errors": prog.Errors})
	result, sev := "ok", "info"
	if status != "delivered" || class != "" {
		result, sev = "error", "warning"
	}
	return audited(tx, AuditEvent{ActorType: "device", ActorID: deviceID, Method: "agent", Action: "restore.finish",
		ObjectType: "restore", ObjectID: r.ID, CustomerID: cust, Result: result, Severity: sev, Details: details})
}

// restoreVerdictOf reads a closed restore run record: the status and class it
// closes the row with, and the progress document it leaves (counts and classes,
// never a path).
func restoreVerdictOf(r RunInput) (status, class string, prog DeviceRestoreProgress) {
	var st restoreRunStats
	if len(r.Stats) > 0 {
		_ = json.Unmarshal(r.Stats, &st) // validated by the agent API; an unreadable block closes with zero counts
	}
	status, class = restoreClose(r.Outcome, st.RestoreResult, r.ErrorClass)
	prog = DeviceRestoreProgress{Outcome: r.Outcome, FilesRestored: st.FilesRestored, FilesSkipped: st.FilesSkipped,
		BytesRestored: st.BytesRestored, BytesSkipped: st.BytesSkipped}
	for _, e := range st.RestoreErrors {
		prog.Errors = append(prog.Errors, RestoreClassCounted{Class: e.Class, Count: e.Count})
	}
	return status, class, prog
}

// recordLateRestoreReport keeps the agent's closed record of a restore the
// console had already closed because the agent stopped reporting it (ADR 0032).
// The row is not reopened: its status, class and finish time stay the
// console's verdict, so the operator never sees a FAILED restore turn quietly
// into RESTORED. What the agent reported is added to the progress document
// (DeviceRestoreProgress.Late) and written as the audit row restore.late_report,
// path-free, in the ingest transaction (a failed audit write fails the batch and
// the agent resends). Only the first late closed record is kept; an open record,
// a row the agent itself closed and a row the console expired or cancelled
// before the agent fetched it are left alone, as before.
func recordLateRestoreReport(tx *Tx, deviceID string, cur RestoreRequest, r RunInput) error {
	if r.Outcome == "" || len(cur.Progress) == 0 {
		return nil
	}
	var prog DeviceRestoreProgress
	if json.Unmarshal(cur.Progress, &prog) != nil || prog.ClosedBy == "" || prog.Late != nil {
		return nil
	}
	status, class, got := restoreVerdictOf(r)
	prog.Late = &DeviceRestoreLate{Outcome: r.Outcome, Status: status, Class: class, FilesRestored: got.FilesRestored,
		FilesSkipped: got.FilesSkipped, BytesRestored: got.BytesRestored, Errors: got.Errors, ReceivedAt: time.UnixMilli(tx.nowMS()).UTC()}
	raw, err := json.Marshal(prog)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE restore_requests SET progress = ?, updated_at = ? WHERE id = ?`, string(raw), tx.nowMS(), cur.ID); err != nil {
		return err
	}
	var cust string
	if err := tx.QueryRow(`SELECT customer_id FROM devices WHERE id = ?`, deviceID).Scan(&cust); err != nil {
		return err
	}
	details, _ := json.Marshal(map[string]any{"status": cur.Status, "error_class": cur.ErrorClass, "closed_by": prog.ClosedBy,
		"late_outcome": r.Outcome, "late_status": status, "late_error_class": class,
		"files_restored": got.FilesRestored, "files_skipped": got.FilesSkipped, "errors": got.Errors})
	return audited(tx, AuditEvent{ActorType: "device", ActorID: deviceID, Method: "agent", Action: "restore.late_report",
		ObjectType: "restore", ObjectID: cur.ID, CustomerID: cust, Result: "ok", Severity: "warning", Details: details})
}

// ExpireDeviceRestores closes the device restores the device never picked up
// (the restore-purge job, ADR 0027 ruling C6): a queued row whose command was
// cancelled becomes cancelled; a queued row at or past its expiry becomes
// expired. Each closed row loses its wrapped selection and gets one audit row
// (restore.expire or restore.finish), in one transaction per row; a failed audit
// write leaves that row as it was for the next run. A running row is the
// agent's: its run record closes it. It returns how many rows it closed. A job
// method: no Scope.
func (s *Store) ExpireDeviceRestores(ctx context.Context, now time.Time) (int, error) {
	type due struct {
		id, cust, cmdStatus string
		expires             int64
	}
	var rows []due
	err := s.Read(ctx, func(tx *Tx) error {
		var err error
		rows, err = collect(tx, `SELECT r.id, d.customer_id, COALESCE(c.status, ''), r.expires_at FROM restore_requests r
				JOIN devices d ON d.id = r.device_id LEFT JOIN commands c ON c.id = r.command_id
			WHERE r.mode IN ('device_original','device_path') AND r.status = 'queued'
			  AND (r.expires_at <= ? OR c.status = 'cancelled') ORDER BY r.created_at, r.id`, []any{msOf(now)},
			func(sc scanner) (due, error) {
				var d due
				return d, sc.Scan(&d.id, &d.cust, &d.cmdStatus, &d.expires)
			})
		return err
	})
	if err != nil {
		return 0, err
	}
	closed := 0
	var firstErr error
	for _, d := range rows {
		status, action, class := "expired", "restore.expire", ""
		if d.cmdStatus == "cancelled" {
			status, action, class = "cancelled", "restore.finish", "cancelled"
		}
		did := false
		err := s.Write(ctx, func(tx *Tx) error {
			did = false
			t := tx.nowMS()
			res, err := tx.Exec(`UPDATE restore_requests SET status = ?, error_class = ?, selection_wrapped = NULL, finished_at = ?, updated_at = ?
				WHERE id = ? AND status = 'queued'`, status, nullStr(class), t, t, d.id)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return nil // closed meanwhile (the agent fetched it, or another run)
			}
			details, _ := json.Marshal(map[string]any{"status": status})
			if err := audited(tx, AuditEvent{ActorType: "system", Method: "system", Action: action, ObjectType: "restore", ObjectID: d.id,
				CustomerID: d.cust, Result: "ok", Severity: "warning", Details: details}); err != nil {
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
		}
	}
	return closed, firstErr
}
