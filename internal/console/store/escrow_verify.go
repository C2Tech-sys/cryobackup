package store

import (
	"context"
	"fmt"
)

// EscrowVerification is the outcome of one `restic cat config` through the
// admin path with the device's unwrapped escrowed repository key (M2-FIX-B1,
// ADR 0014).
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
// scope or has no active escrowed key.
func (s *Store) RecordEscrowVerification(ctx context.Context, scope Scope, deviceID string, v EscrowVerification) error {
	if err := scope.check(); err != nil {
		return err
	}
	return s.Write(ctx, func(tx *Tx) error {
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
	})
}
