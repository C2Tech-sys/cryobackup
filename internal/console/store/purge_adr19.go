package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
)

// Purging a retired device and deleting a customer (ADR 0019): the database side.
//
// A purge runs in two parts because storage can fail and the database must not forget a
// repository that still exists:
//
//  1. RequestDevicePurge marks a retired device ("purging") and audits it. Nothing is deleted.
//  2. The device-purge job removes the repository through the maintenance worker, lists
//     ListDevicesPurging, records each failed try with RecordDevicePurgeAttempt, and only when
//     the worker reports the prefix empty calls CompleteDevicePurge, which deletes the row.
//
// There is no other path that deletes a device row. Audit rows have no foreign key and are never
// deleted: after a purge they are the only record the device existed.
//
// Role. The store knows scopes, not roles, so the owner role is re-asserted here by reading the
// acting account: RequestDevicePurge and DeleteCustomer refuse (ErrOwnerRequired) unless
// AuditActor.AdminID names an enabled owner, and refuse (ErrScope) any scope but All. The
// two job steps take no admin: they need scope All (the job's) and a purge that RequestDevicePurge
// recorded, which only an owner can have done.

var (
	// ErrOwnerRequired: the acting account is not an enabled owner, or the scope is not All. It
	// satisfies errors.Is(err, ErrScope), so a caller that maps ErrScope to 403 needs no change.
	ErrOwnerRequired = fmt.Errorf("%w: the owner role is required", ErrScope)
	// ErrDeviceNotRetired: only a retired device can be purged; the caller says to retire it first.
	ErrDeviceNotRetired = conflictf("device is not retired")
	// ErrNotPurging: the device has no purge request, so there is nothing to record or complete.
	ErrNotPurging = conflictf("no purge was requested for this device")
)

// CustomerHasDevicesError is the refusal of DeleteCustomer while any device exists, active or
// retired (devices.customer_id is RESTRICT). errors.Is(err, ErrConflict) holds; the counts let
// the caller say what is left to retire and purge. It carries no device name.
type CustomerHasDevicesError struct{ Active, Retired int }

// Error implements error.
func (e *CustomerHasDevicesError) Error() string {
	return fmt.Sprintf("%v: customer still has devices (%d active, %d retired)", ErrConflict, e.Active, e.Retired)
}

// Unwrap lets errors.Is match ErrConflict.
func (e *CustomerHasDevicesError) Unwrap() error { return ErrConflict }

// ValidatePurgeReason checks the reason of a purge or a customer delete: required, one line, no
// file paths. It returns the cleaned reason.
func ValidatePurgeReason(raw string) (string, []FieldError) { return ValidateRetireReason(raw) }

// errorClassRE is what a recorded failure class may look like: a fixed lower-case word, so no
// message, host name or path can ride into the device row.
var errorClassRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// requireOwner re-asserts, inside tx, that the actor is an enabled owner. The scope check comes
// first and is the caller's: an owner always has the All scope.
func requireOwner(tx *Tx, actor AuditActor) error {
	var role string
	var disabled sql.NullInt64
	err := tx.QueryRow(`SELECT role, disabled_at FROM admin_users WHERE id = ?`, actor.AdminID).Scan(&role, &disabled)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && (role != "owner" || disabled.Valid)) {
		return ErrOwnerRequired
	}
	return err
}

// requireAllScope is the scope half of the owner rule: the zero Scope is ErrScope, and so is any
// scope short of All.
func requireAllScope(scope Scope) error {
	if err := scope.check(); err != nil {
		return err
	}
	if !scope.All {
		return ErrScope
	}
	return nil
}

// PurgeRequestResult is the outcome of RequestDevicePurge.
type PurgeRequestResult struct {
	// Device is the device as it stands after the call.
	Device Device
	// Changed is false when the device was already purging: nothing was written, no audit row
	// exists for this call, and the first request's reason and time stand.
	Changed bool
}

// RequestDevicePurge marks a retired device for purge and writes one "device.purge.request"
// audit row (severity warning) in the same transaction; a failed audit write refuses the change
// (ErrAudit). Nothing is deleted here. The actor must be an enabled owner and the scope All
// (ErrOwnerRequired, ErrScope). A device that does not exist is ErrNotFound; one that is not
// retired is ErrDeviceNotRetired and nothing changes. A device already purging is answered as it
// is with Changed false: no second audit row, the first reason stands. Rejected input is a
// *ValidationError. The audit details carry the reason and no host name.
func (s *Store) RequestDevicePurge(ctx context.Context, scope Scope, deviceID, reason string, actor AuditActor) (PurgeRequestResult, error) {
	if err := requireAllScope(scope); err != nil {
		return PurgeRequestResult{}, err
	}
	var out PurgeRequestResult
	err := s.Write(ctx, func(tx *Tx) error {
		if err := requireOwner(tx, actor); err != nil {
			return err
		}
		cur, err := getDevice(tx, scope, deviceID)
		if err != nil {
			return err
		}
		if cur.Purging() {
			out = PurgeRequestResult{Device: cur}
			return nil
		}
		if cur.RetiredAt == nil {
			return ErrDeviceNotRetired
		}
		clean, bad := ValidatePurgeReason(reason)
		if err := validation(bad); err != nil {
			return err
		}
		now := tx.nowMS()
		if _, err := tx.Exec(`UPDATE devices SET purge_requested_at = ?, purge_requested_by = ?, purge_reason = ?,
			purge_last_error_class = NULL, purge_last_attempt_at = NULL, updated_at = ? WHERE id = ?`,
			now, actor.AdminID, clean, now, deviceID); err != nil {
			return err
		}
		ev, err := actor.event("device.purge.request", "device", deviceID, cur.CustomerID, map[string]any{"reason": clean})
		if err != nil {
			return err
		}
		ev.Severity = "warning"
		if err := audited(tx, ev); err != nil {
			return err
		}
		dev, err := getDevice(tx, scope, deviceID)
		out = PurgeRequestResult{Device: dev, Changed: true}
		return err
	})
	if err != nil {
		return PurgeRequestResult{}, err
	}
	return out, nil
}

// ListDevicesPurging lists the devices inside scope that have a purge requested and not finished,
// oldest request first: the device-purge job's work list (it passes ScopeAll). The rows carry the
// host name, so the caller must not log them.
func (s *Store) ListDevicesPurging(ctx context.Context, scope Scope) ([]Device, error) {
	if err := scope.check(); err != nil {
		return nil, err
	}
	var out []Device
	err := s.Read(ctx, func(tx *Tx) error {
		clause, args := scope.clause("customer_id")
		var err error
		out, err = collect(tx, `SELECT `+deviceCols+` FROM devices WHERE purge_requested_at IS NOT NULL AND `+clause+
			` ORDER BY purge_requested_at, id`, args, scanDevice)
		return err
	})
	return out, err
}

// RecordDevicePurgeAttempt notes that the job tried to remove a purging device's repository: it
// sets purge_last_attempt_at to now and purge_last_error_class to errorClass. An empty class
// clears a failure that was outstanding (the attempt itself is still recorded). A class is a
// fixed lower-case word (ErrInvalid otherwise): never a message. The scope must be All; a device
// that does not exist is ErrNotFound and one with no purge request is ErrNotPurging. No audit row
// is written: this is the job's own bookkeeping, repeated on every retry.
func (s *Store) RecordDevicePurgeAttempt(ctx context.Context, scope Scope, deviceID, errorClass string) error {
	if err := requireAllScope(scope); err != nil {
		return err
	}
	if errorClass != "" && !errorClassRE.MatchString(errorClass) {
		return invalidf("a purge error class is a short lower-case word")
	}
	return s.Write(ctx, func(tx *Tx) error {
		cur, err := getDevice(tx, scope, deviceID)
		if err != nil {
			return err
		}
		if !cur.Purging() {
			return ErrNotPurging
		}
		now := tx.nowMS()
		_, err = tx.Exec(`UPDATE devices SET purge_last_error_class = ?, purge_last_attempt_at = ?, updated_at = ? WHERE id = ?`,
			nullStr(errorClass), now, now, deviceID)
		return err
	})
}

// CompleteDevicePurge is the last step of a purge: the worker reported the repository gone, having
// removed objects objects and bytes bytes. In one transaction it writes one "device.purge.done"
// audit row (system actor, severity warning; the device id, the customer id, the counts, and no
// host name) and deletes the device row, which the foreign keys carry through to everything the
// device owns: runs, snapshot cache, presence and heartbeats, tokens, gateway login, escrowed key,
// commands, restores, acknowledgements, notification rules, usage samples, maintenance schedule,
// start leases, policy overrides and the enrollment tokens that adopt it. Other devices' rows are
// untouched, except that a device this one replaced loses the link (SET NULL). A failed audit
// write refuses the whole change (ErrAudit) and the row stays. The scope must be All; a device
// that does not exist is ErrNotFound; one with no purge request is ErrNotPurging, so no other path
// deletes a device. Negative counts are ErrInvalid.
func (s *Store) CompleteDevicePurge(ctx context.Context, scope Scope, deviceID string, objects, bytes int64) error {
	if err := requireAllScope(scope); err != nil {
		return err
	}
	if objects < 0 || bytes < 0 {
		return invalidf("a purge removes a non-negative number of objects and bytes")
	}
	return s.Write(ctx, func(tx *Tx) error {
		cur, err := getDevice(tx, scope, deviceID)
		if err != nil {
			return err
		}
		if !cur.Purging() {
			return ErrNotPurging
		}
		details, err := json.Marshal(map[string]any{"device_id": deviceID, "customer_id": cur.CustomerID, "objects": objects, "bytes": bytes})
		if err != nil {
			return err
		}
		ev := AuditEvent{ActorType: "system", Method: "system", ActorLabel: "device-purge", Action: "device.purge.done",
			ObjectType: "device", ObjectID: deviceID, CustomerID: cur.CustomerID, Result: "ok", Severity: "warning", Details: details}
		if err := audited(tx, ev); err != nil {
			return err
		}
		res, err := tx.Exec(`DELETE FROM devices WHERE id = ? AND purge_requested_at IS NOT NULL`, deviceID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return fmt.Errorf("store: purge of a device deleted %d rows", n)
		}
		return nil
	})
}

// CustomerDeleteResult says what DeleteCustomer removed besides the customer.
type CustomerDeleteResult struct {
	CustomerID string
	// Name is the customer's name as it was; the audit row keeps it because nothing else will.
	Name string
	// Rows deleted with the customer, by table (the foreign keys cascade; see DeleteCustomer).
	TokensDeleted, PoliciesDeleted, AlarmAcksDeleted, NotificationRulesDeleted, ReportsDeleted, EnrollmentRequestsDeleted int64
}

// DeleteCustomer deletes a customer that has no devices at all, active or retired, and writes one
// "customer.delete" audit row (severity warning) in the same transaction; a failed audit write
// refuses the change (ErrAudit). The actor must be an enabled owner and the scope All
// (ErrOwnerRequired, ErrScope). A customer that does not exist is ErrNotFound; one with any device
// is a *CustomerHasDevicesError (errors.Is ErrConflict) carrying the active and retired counts, and
// nothing changes. Rejected input is a *ValidationError.
//
// What the rest of the schema does with the customer's row (every foreign key that points at it):
//
//	devices.customer_id               RESTRICT: why a device refuses the delete (checked first, with counts)
//	enrollment_tokens.customer_id     CASCADE: deleted here explicitly and counted; the audit rows that name them stay
//	policies.customer_id              CASCADE: the customer's own policies go, and their revisions. customers.policy_id is
//	                                  RESTRICT, but a customer pointing at its own policy does not block its own delete
//	                                  (the test sets exactly that)
//	alarm_acks.customer_id            CASCADE: acknowledgements on the customer go
//	notification_rules.customer_id    CASCADE: the customer's notification rules go (their log rows keep, rule_id SET NULL)
//	reports.customer_id               CASCADE: the customer's reports go
//	enrollment_requests.customer_id   SET NULL by the schema, which an approved request's CHECK refuses; so the
//	                                  requests naming the customer are deleted first (they hold host names and addresses)
//
// admin_users.customer_scope is JSON, not a key: a deleted customer's id simply drops out.
func (s *Store) DeleteCustomer(ctx context.Context, scope Scope, customerID, reason string, actor AuditActor) (CustomerDeleteResult, error) {
	if err := requireAllScope(scope); err != nil {
		return CustomerDeleteResult{}, err
	}
	var out CustomerDeleteResult
	err := s.Write(ctx, func(tx *Tx) error {
		if err := requireOwner(tx, actor); err != nil {
			return err
		}
		var name string
		if err := tx.QueryRow(`SELECT name FROM customers WHERE id = ?`, customerID).Scan(&name); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		var active, retired int
		if err := tx.QueryRow(`SELECT COALESCE(SUM(retired_at IS NULL), 0), COALESCE(SUM(retired_at IS NOT NULL), 0) FROM devices WHERE customer_id = ?`,
			customerID).Scan(&active, &retired); err != nil {
			return err
		}
		if active+retired > 0 {
			return &CustomerHasDevicesError{Active: active, Retired: retired}
		}
		clean, bad := ValidatePurgeReason(reason)
		if err := validation(bad); err != nil {
			return err
		}
		res := CustomerDeleteResult{CustomerID: customerID, Name: name}
		count := func(into *int64, table string) error {
			return tx.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE customer_id = ?`, customerID).Scan(into)
		}
		for _, c := range []struct {
			into  *int64
			table string
		}{{&res.TokensDeleted, "enrollment_tokens"}, {&res.PoliciesDeleted, "policies"}, {&res.AlarmAcksDeleted, "alarm_acks"},
			{&res.NotificationRulesDeleted, "notification_rules"}, {&res.ReportsDeleted, "reports"},
			{&res.EnrollmentRequestsDeleted, "enrollment_requests"}} {
			if err := count(c.into, c.table); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`DELETE FROM enrollment_requests WHERE customer_id = ?`, customerID); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM enrollment_tokens WHERE customer_id = ?`, customerID); err != nil {
			return err
		}
		del, err := tx.Exec(`DELETE FROM customers WHERE id = ?`, customerID)
		if err != nil {
			return mapErr(err)
		}
		if n, _ := del.RowsAffected(); n != 1 {
			return fmt.Errorf("store: delete of a customer deleted %d rows", n)
		}
		ev, err := actor.event("customer.delete", "customer", customerID, customerID, map[string]any{
			"reason": clean, "name": name, "tokens_deleted": res.TokensDeleted, "policies_deleted": res.PoliciesDeleted,
			"alarm_acks_deleted": res.AlarmAcksDeleted, "notification_rules_deleted": res.NotificationRulesDeleted,
			"reports_deleted": res.ReportsDeleted, "enrollment_requests_deleted": res.EnrollmentRequestsDeleted})
		if err != nil {
			return err
		}
		ev.Severity = "warning"
		if err := audited(tx, ev); err != nil {
			return err
		}
		out = res
		return nil
	})
	if err != nil {
		return CustomerDeleteResult{}, err
	}
	return out, nil
}
