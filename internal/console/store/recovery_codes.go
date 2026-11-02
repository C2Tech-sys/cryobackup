package store

import "database/sql"

// RecoveryCode is an unused row of admin_recovery_codes: the hash of a
// single-use MFA recovery code (the code itself is shown once and never stored).
type RecoveryCode struct {
	ID       string
	AdminID  string
	CodeHash string
}

// ReplaceRecoveryCodes deletes every recovery code of the account and inserts
// the given hashes as a fresh, unused set (enrollment and regeneration).
func (tx *Tx) ReplaceRecoveryCodes(adminID string, hashes []string) error {
	if len(hashes) == 0 {
		return invalidf("at least one recovery code hash is required")
	}
	if _, err := tx.Exec(`DELETE FROM admin_recovery_codes WHERE admin_id = ?`, adminID); err != nil {
		return err
	}
	now := tx.nowMS()
	for _, h := range hashes {
		if _, err := tx.Exec(`INSERT INTO admin_recovery_codes (id, admin_id, code_hash, used_at, created_at, updated_at) VALUES (?,?,?,NULL,?,?)`,
			tx.id(), adminID, h, now, now); err != nil {
			return err
		}
	}
	return nil
}

// UnusedRecoveryCodes returns the account's unused codes (id and hash), so the
// caller can compare a presented code against every hash in constant time.
func (tx *Tx) UnusedRecoveryCodes(adminID string) ([]RecoveryCode, error) {
	return collect(tx, `SELECT id, admin_id, code_hash FROM admin_recovery_codes WHERE admin_id = ? AND used_at IS NULL ORDER BY id`,
		[]any{adminID}, func(sc scanner) (RecoveryCode, error) {
			var c RecoveryCode
			err := sc.Scan(&c.ID, &c.AdminID, &c.CodeHash)
			return c, err
		})
}

// UseRecoveryCode marks one code used. It reports false when the code was
// already used (a concurrent use won), so a code works exactly once.
func (tx *Tx) UseRecoveryCode(id string) (bool, error) {
	now := tx.nowMS()
	res, err := tx.Exec(`UPDATE admin_recovery_codes SET used_at = ?, updated_at = ? WHERE id = ? AND used_at IS NULL`, now, now, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// RecoveryCodeCounts returns how many of the account's recovery codes are
// unused and how many exist.
func (tx *Tx) RecoveryCodeCounts(adminID string) (unused, total int, err error) {
	var u sql.NullInt64
	err = tx.QueryRow(`SELECT count(*), sum(CASE WHEN used_at IS NULL THEN 1 ELSE 0 END) FROM admin_recovery_codes WHERE admin_id = ?`, adminID).Scan(&total, &u)
	return int(u.Int64), total, err
}
