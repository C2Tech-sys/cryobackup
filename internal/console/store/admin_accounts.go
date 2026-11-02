package store

import (
	"encoding/json"
	"time"
)

// Account administration and MFA state (ticket M2-W1A). Every method here is an
// in-transaction (*Tx) method on purpose: the caller makes the change and
// writes its audit row in the same Write, so a change whose audit row cannot be
// written is rolled back (the "NOT AUDITED" rule). Admin accounts are
// console-wide data, not customer-owned, so nothing here takes a Scope.

// ErrLastOwner is returned (wrapped in ErrConflict) when a change would leave
// the console without an enabled owner account.
var ErrLastOwner = conflictf("the console must keep at least one enabled owner")

// ListAdmins is the in-transaction form of Store.ListAdmins.
func (tx *Tx) ListAdmins() ([]Admin, error) {
	return collect(tx, `SELECT `+adminCols+` FROM admin_users ORDER BY username COLLATE NOCASE`, nil, scanAdmin)
}

// GetAdminByUsername is the in-transaction form of Store.GetAdminByUsername.
func (tx *Tx) GetAdminByUsername(username string) (Admin, error) {
	return one(tx, `SELECT `+adminCols+` FROM admin_users WHERE username = ?`, []any{username}, scanAdmin)
}

// countEnabledOwners counts owner accounts that are not disabled, leaving out
// exceptID ("" leaves out nothing).
func (tx *Tx) countEnabledOwners(exceptID string) (int, error) {
	var n int
	err := tx.QueryRow(`SELECT count(*) FROM admin_users WHERE role = 'owner' AND disabled_at IS NULL AND id <> ?`, exceptID).Scan(&n)
	return n, err
}

// guardLastOwner refuses a change that removes a (currently enabled) owner
// when it is the only one.
func (tx *Tx) guardLastOwner(a Admin) error {
	if a.Role != "owner" || a.DisabledAt != nil {
		return nil
	}
	n, err := tx.countEnabledOwners(a.ID)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrLastOwner
	}
	return nil
}

// UpdateAdminRoleScope sets the role and customer scope of an account (nil
// scope = all customers; only operator and read_only may be scoped, which the
// schema enforces as ErrInvalid). Demoting the last enabled owner is
// ErrLastOwner. It returns the account before and after.
func (tx *Tx) UpdateAdminRoleScope(id, role string, scope []string) (before, after Admin, err error) {
	before, err = tx.GetAdmin(id)
	if err != nil {
		return before, after, err
	}
	if role != "owner" {
		if err := tx.guardLastOwner(before); err != nil {
			return before, after, err
		}
	}
	var scopeArg any
	if scope != nil {
		b, merr := json.Marshal(scope)
		if merr != nil {
			return before, after, merr
		}
		scopeArg = string(b)
	}
	res, err := tx.Exec(`UPDATE admin_users SET role = ?, customer_scope = ?, updated_at = ? WHERE id = ?`, role, scopeArg, tx.nowMS(), id)
	if err != nil {
		return before, after, err
	}
	if err := requireRow(res); err != nil {
		return before, after, err
	}
	after, err = tx.GetAdmin(id)
	return before, after, err
}

// UpdateAdminEmail sets the e-mail address of an account.
func (tx *Tx) UpdateAdminEmail(id, email string) error {
	if email == "" {
		return invalidf("e-mail is required")
	}
	res, err := tx.Exec(`UPDATE admin_users SET email = ?, updated_at = ? WHERE id = ?`, email, tx.nowMS(), id)
	if err != nil {
		return err
	}
	return requireRow(res)
}

// DisableAdmin disables an account, deletes its sessions and revokes its API
// tokens. Disabling a disabled account keeps its original disabled_at.
// Disabling the last enabled owner is ErrLastOwner.
func (tx *Tx) DisableAdmin(id string) error {
	a, err := tx.GetAdmin(id)
	if err != nil {
		return err
	}
	if err := tx.guardLastOwner(a); err != nil {
		return err
	}
	now := tx.nowMS()
	if _, err := tx.Exec(`UPDATE admin_users SET disabled_at = COALESCE(disabled_at, ?), updated_at = ? WHERE id = ?`, now, now, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM sessions WHERE admin_id = ?`, id); err != nil {
		return err
	}
	_, err = tx.RevokeAdminAPITokens(id)
	return err
}

// EnableAdmin clears disabled_at and any login lock. ErrNotFound for an unknown id.
func (tx *Tx) EnableAdmin(id string) error {
	res, err := tx.Exec(`UPDATE admin_users SET disabled_at = NULL, failed_login_count = 0, locked_until = NULL, updated_at = ? WHERE id = ?`, tx.nowMS(), id)
	if err != nil {
		return err
	}
	return requireRow(res)
}

// DeleteAdmin removes an account (its sessions, recovery codes and API tokens
// go with it by ON DELETE CASCADE; rows elsewhere that name it fall back to
// NULL). Deleting the last enabled owner is ErrLastOwner.
func (tx *Tx) DeleteAdmin(id string) (Admin, error) {
	a, err := tx.GetAdmin(id)
	if err != nil {
		return a, err
	}
	if err := tx.guardLastOwner(a); err != nil {
		return a, err
	}
	res, err := tx.Exec(`DELETE FROM admin_users WHERE id = ?`, id)
	if err != nil {
		return a, err
	}
	return a, requireRow(res)
}

// SetAdminTOTP stores a wrapped TOTP secret as the account's enrollment, with
// enrolled_at now and the replay guard at lastStep (the step of the code that
// confirmed the enrollment).
func (tx *Tx) SetAdminTOTP(id string, wrapped []byte, lastStep int64) error {
	if len(wrapped) == 0 {
		return invalidf("wrapped secret is required")
	}
	now := tx.nowMS()
	res, err := tx.Exec(`UPDATE admin_users SET totp_secret_wrapped = ?, totp_enrolled_at = ?, totp_last_step = ?, updated_at = ? WHERE id = ?`,
		wrapped, now, lastStep, now, id)
	if err != nil {
		return err
	}
	return requireRow(res)
}

// ClearAdminMFA is the break-glass reset: it removes the TOTP enrollment and
// every recovery code and deletes the account's sessions, so the next login
// goes through enrollment again.
func (tx *Tx) ClearAdminMFA(id string) error {
	res, err := tx.Exec(`UPDATE admin_users SET totp_secret_wrapped = NULL, totp_enrolled_at = NULL, totp_last_step = NULL, updated_at = ? WHERE id = ?`,
		tx.nowMS(), id)
	if err != nil {
		return err
	}
	if err := requireRow(res); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM admin_recovery_codes WHERE admin_id = ?`, id); err != nil {
		return err
	}
	_, err = tx.Exec(`DELETE FROM sessions WHERE admin_id = ?`, id)
	return err
}

// AdvanceTOTPStep is the replay guard: it records step as the last used TOTP
// step and reports true only when step is later than the one recorded. One
// UPDATE does the check and the write, so two requests with the same code
// cannot both succeed.
func (tx *Tx) AdvanceTOTPStep(id string, step int64) (bool, error) {
	res, err := tx.Exec(`UPDATE admin_users SET totp_last_step = ?, updated_at = ?
		WHERE id = ? AND totp_enrolled_at IS NOT NULL AND (totp_last_step IS NULL OR totp_last_step < ?)`, step, tx.nowMS(), id, step)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// RecordLoginFailure is the in-transaction form of Store.RecordLoginFailure.
func (tx *Tx) RecordLoginFailure(id string, lockAfter int, lockFor time.Duration) (Admin, error) {
	now := tx.nowMS()
	res, err := tx.Exec(`UPDATE admin_users SET failed_login_count = failed_login_count + 1,
		locked_until = CASE WHEN ? > 0 AND failed_login_count + 1 >= ? THEN ? ELSE locked_until END,
		updated_at = ? WHERE id = ?`, lockAfter, lockAfter, now+lockFor.Milliseconds(), now, id)
	if err != nil {
		return Admin{}, err
	}
	if err := requireRow(res); err != nil {
		return Admin{}, err
	}
	return tx.GetAdmin(id)
}

// ClearLoginFailures is the in-transaction form of Store.ClearLoginFailures.
func (tx *Tx) ClearLoginFailures(id, ip string) error {
	now := tx.nowMS()
	res, err := tx.Exec(`UPDATE admin_users SET failed_login_count = 0, locked_until = NULL, last_login_at = ?,
		last_login_ip = ?, updated_at = ? WHERE id = ?`, now, nullStr(ip), now, id)
	if err != nil {
		return err
	}
	return requireRow(res)
}
