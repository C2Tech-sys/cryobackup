package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/c2tech-sys/cryobackup/internal/state"
)

// EscrowVerification is the outcome of one `restic cat config` through the
// admin path with the device's unwrapped escrowed repository key (M2-FIX-B1,
// ADR 0014; the weekly recheck of ADR 0030).
type EscrowVerification struct {
	// OK means the key opened the repository: verified_at is set and
	// last_verify_error_class cleared.
	OK bool
	// ErrorClass, when OK is false, is the path-free verdict on the key or the
	// repository (wrong_password, repo_missing). It is recorded as
	// last_verify_error_class, which raises ESCROW_UNVERIFIED. An empty class
	// means "could not verify" (storage unreachable, a timeout, a busy worker):
	// only the attempt time is recorded and the previous verdict stands.
	ErrorClass string
}

// RecordEscrowVerification records a verification of the device's active
// escrowed key. It writes no audit row: it is the console's own observation
// of the repository, like presence, and the enrollment that performs the first
// one already writes agent.enroll. It is ErrNotFound when the device is out of
// scope or has no active escrowed key. A retired device's key is recorded like
// any other (ADR 0030: retired devices are verified too).
func (s *Store) RecordEscrowVerification(ctx context.Context, scope Scope, deviceID string, v EscrowVerification) error {
	if err := scope.check(); err != nil {
		return err
	}
	return s.Write(ctx, func(tx *Tx) error {
		return recordEscrowVerificationTx(tx, scope, deviceID, v)
	})
}

// RecordEscrowVerificationAudited is RecordEscrowVerification for a check an
// operator asked for (POST /api/v1/devices/{id}/escrow/verify): the record and
// ev commit together, so a verification whose audit row cannot be written is
// not recorded either. ev must be path-free (Tx.AppendAudit refuses one that
// looks like a path).
func (s *Store) RecordEscrowVerificationAudited(ctx context.Context, scope Scope, deviceID string, v EscrowVerification, ev AuditEvent) error {
	if err := scope.check(); err != nil {
		return err
	}
	return s.Write(ctx, func(tx *Tx) error {
		if err := recordEscrowVerificationTx(tx, scope, deviceID, v); err != nil {
			return err
		}
		_, err := tx.AppendAudit(ev)
		return err
	})
}

func recordEscrowVerificationTx(tx *Tx, scope Scope, deviceID string, v EscrowVerification) error {
	if _, err := scopedDevice(tx, scope, deviceID); err != nil {
		return err
	}
	now := tx.nowMS()
	var q string
	var args []any
	switch {
	case v.OK:
		q, args = `UPDATE repo_key_escrow SET verified_at = ?, last_verify_attempt_at = ?, last_verify_error_class = NULL, updated_at = ?
			WHERE device_id = ? AND retired_at IS NULL AND custody = 'escrowed'`, []any{now, now, now, deviceID}
	case v.ErrorClass != "":
		q, args = `UPDATE repo_key_escrow SET last_verify_attempt_at = ?, last_verify_error_class = ?, updated_at = ?
			WHERE device_id = ? AND retired_at IS NULL AND custody = 'escrowed'`, []any{now, v.ErrorClass, now, deviceID}
	default:
		q, args = `UPDATE repo_key_escrow SET last_verify_attempt_at = ?, updated_at = ?
			WHERE device_id = ? AND retired_at IS NULL AND custody = 'escrowed'`, []any{now, now, deviceID}
	}
	res, err := tx.Exec(q, args...)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return fmt.Errorf("%w: no active escrowed key for the device", ErrNotFound)
	}
	return nil
}

// escrowVerifyKeysSQL is the escrow verifier's candidate set (ADR 0030): every
// device with an active escrowed key (custody escrowed on the key and on its
// customer: the worker refuses a customer-held customer's devices) whose
// enrollment was confirmed (before that there is no repository to open) and
// that is not being purged (its repository is going away; ADR 0019). Retired
// devices are included: retire keeps the repository and the key (ADR 0016), and
// a retired device can still be restored by download. The scope clause is
// appended on the customer id. (A var, not a const, only so a test can break the query and watch
// the cross-check against escrowConfirmedCountSQL fire.)
var escrowVerifyKeysSQL = `
	SELECT d.id, d.retired_at IS NOT NULL, e.custody, e.created_at, e.verified_at, e.last_verify_attempt_at,
	       COALESCE(e.last_verify_error_class, '')
	FROM repo_key_escrow e
	JOIN devices d   ON d.id = e.device_id
	JOIN customers c ON c.id = d.customer_id
	WHERE e.retired_at IS NULL AND e.custody = 'escrowed' AND c.key_custody = 'escrowed'
	  AND d.enroll_confirmed_at IS NOT NULL AND d.purge_requested_at IS NULL
	  AND `

// escrowConfirmedCountSQL counts, by a different route than escrowVerifyKeysSQL, the devices the
// verifier must have a candidate for: confirmed, not purging, an active escrowed key, a customer
// that holds escrowed custody. It exists only to cross-check the candidate query (B4): if that query
// ever returned no rows (or fewer) while these devices exist, the verifier would see nothing to do and
// the fleet would read green, so the mismatch is an error. The scope clause is appended on d.customer_id.
const escrowConfirmedCountSQL = `
	SELECT COUNT(*) FROM devices d
	WHERE d.enroll_confirmed_at IS NOT NULL AND d.purge_requested_at IS NULL
	  AND EXISTS (SELECT 1 FROM repo_key_escrow e WHERE e.device_id = d.id AND e.retired_at IS NULL AND e.custody = 'escrowed')
	  AND EXISTS (SELECT 1 FROM customers c WHERE c.id = d.customer_id AND c.key_custody = 'escrowed')
	  AND `

// ErrEscrowCandidates is the candidate set disagreeing with the devices it should cover: fewer
// candidates than confirmed devices with an escrowed key. It is an ErrInvalid so the facts loaders
// report it as a fixed class ("escrow_invalid"), never as an empty candidate list that reads healthy.
var ErrEscrowCandidates = fmt.Errorf("%w: the escrow verifier's candidates do not cover the confirmed devices with an escrowed key", ErrInvalid)

// EscrowVerifyKeys lists the escrow verifier's candidate keys inside scope, by
// device id: what the scheduler checks (maintain/escrowverify.go), what banner
// C22 and the System page's escrow row judge (state.EscrowVerifyFacts). Times
// are the raw milliseconds the engine parses itself. It is ErrEscrowCandidates when
// fewer candidates come back than confirmed devices with an escrowed key exist.
func (s *Store) EscrowVerifyKeys(ctx context.Context, scope Scope) ([]state.EscrowKeyFacts, error) {
	if err := scope.check(); err != nil {
		return nil, err
	}
	var out []state.EscrowKeyFacts
	err := s.Read(ctx, func(tx *Tx) error {
		var err error
		out, err = escrowVerifyKeysTx(tx, scope)
		return err
	})
	return out, err
}

// escrowVerifyKeysTx is EscrowVerifyKeys inside an open transaction (for a
// facts load that reads it beside the devices, in the same snapshot).
func escrowVerifyKeysTx(tx *Tx, scope Scope) ([]state.EscrowKeyFacts, error) {
	out, err := escrowVerifyKeysRows(tx, scope)
	if err != nil {
		return nil, err
	}
	clause, args := scope.clause("d.customer_id")
	var want int
	if err := tx.QueryRow(escrowConfirmedCountSQL+clause, args...).Scan(&want); err != nil {
		return nil, err
	}
	if len(out) < want {
		return nil, ErrEscrowCandidates
	}
	return out, nil
}

func escrowVerifyKeysRows(tx *Tx, scope Scope) ([]state.EscrowKeyFacts, error) {
	clause, args := scope.clause("d.customer_id")
	rows, err := tx.Query(escrowVerifyKeysSQL+clause+` ORDER BY d.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []state.EscrowKeyFacts
	for rows.Next() {
		var k state.EscrowKeyFacts
		var created int64
		var verified, attempt sql.NullInt64
		if err := rows.Scan(&k.DeviceID, &k.Retired, &k.Escrow.Custody, &created, &verified, &attempt, &k.Escrow.LastVerifyErrorClass); err != nil {
			return nil, err
		}
		k.Escrow.CreatedAt, k.Escrow.VerifiedAt, k.LastAttemptAt = state.Millis(created), state.Millis(verified.Int64), state.Millis(attempt.Int64)
		out = append(out, k)
	}
	return out, rows.Err()
}

// EscrowVerifyFactsOf is the escrow verifier's candidate keys as the engine's
// facts (World.EscrowVerify, the System page's escrow row). The error is only
// an invalid scope (a programming error, ErrScope). A read that fails is
// reported inside the facts as NotRead with a fixed class, never as an empty
// list that would read healthy.
func (s *Store) EscrowVerifyFactsOf(ctx context.Context, scope Scope) (*state.EscrowVerifyFacts, error) {
	if err := scope.check(); err != nil {
		return nil, err
	}
	keys, err := s.EscrowVerifyKeys(ctx, scope)
	if err != nil {
		return &state.EscrowVerifyFacts{NotRead: "the escrowed keys could not be read (" + loadClass("escrow", err) + ")"}, nil
	}
	return &state.EscrowVerifyFacts{Keys: keys}, nil
}

// EscrowVerifyTarget is what an on-demand verification needs to know about a
// device before it asks the worker (adminapi escrow_p22.go).
type EscrowVerifyTarget struct {
	DeviceID, CustomerID string
	// Custody is the effective custody: customer_held when the customer or the active key says
	// so, escrowed for an active escrowed key, "" when the device has no active key at all.
	Custody string
	// Retired, Purging and Confirmed are the device's lifecycle: retired_at, purge_requested_at
	// and enroll_confirmed_at set.
	Retired, Purging, Confirmed bool
}

// GetEscrowVerifyTarget reads the device's EscrowVerifyTarget. It is ErrNotFound
// when the device does not exist or is outside scope.
func (s *Store) GetEscrowVerifyTarget(ctx context.Context, scope Scope, deviceID string) (EscrowVerifyTarget, error) {
	if err := scope.check(); err != nil {
		return EscrowVerifyTarget{}, err
	}
	t := EscrowVerifyTarget{DeviceID: deviceID}
	err := s.Read(ctx, func(tx *Tx) error {
		cust, err := scopedDevice(tx, scope, deviceID)
		if err != nil {
			return err
		}
		t.CustomerID = cust
		var customerCustody string
		if err := tx.QueryRow(`SELECT d.retired_at IS NOT NULL, d.purge_requested_at IS NOT NULL, d.enroll_confirmed_at IS NOT NULL, c.key_custody
			FROM devices d JOIN customers c ON c.id = d.customer_id WHERE d.id = ?`, deviceID).Scan(&t.Retired, &t.Purging, &t.Confirmed, &customerCustody); err != nil {
			return err
		}
		var keyCustody string
		switch err := tx.QueryRow(`SELECT custody FROM repo_key_escrow WHERE device_id = ? AND retired_at IS NULL`, deviceID).Scan(&keyCustody); {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return err
		}
		switch {
		case customerCustody == "customer_held" || keyCustody == "customer_held":
			t.Custody = "customer_held"
		case keyCustody == "escrowed":
			t.Custody = "escrowed"
		}
		return nil
	})
	return t, err
}
