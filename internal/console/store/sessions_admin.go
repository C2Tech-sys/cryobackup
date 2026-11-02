package store

// Session management for the account screens and the MFA step (ticket
// M2-W1A). In-transaction forms, so a session change and its audit row commit
// together.

// InsertSession is the in-transaction form of Store.CreateSession.
func (tx *Tx) InsertSession(x Session) (Session, error) {
	x.ID = tx.id()
	now := tx.nowMS()
	seen := now
	if !x.LastSeenAt.IsZero() {
		seen = msOf(x.LastSeenAt)
	}
	if _, err := tx.Exec(`INSERT INTO sessions (`+sessionCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		x.ID, x.AdminID, x.TokenHash, nullTime(x.MFAVerifiedAt), nullStr(x.SourceIP), nullStr(x.UserAgentHash), seen,
		msOf(x.ExpiresAt), msOf(x.AbsoluteExpiresAt), nullTime(x.RevokedAt), now, now); err != nil {
		return Session{}, err
	}
	return one(tx, `SELECT `+sessionCols+` FROM sessions WHERE id = ?`, []any{x.ID}, scanSession)
}

// DeleteSession removes one session by id (missing is not an error).
func (tx *Tx) DeleteSession(id string) error {
	_, err := tx.Exec(`DELETE FROM sessions WHERE id = ?`, id)
	return err
}

// ListAdminSessions returns the account's live, MFA-complete sessions, the most
// recently seen first. Pending (password-only) sessions are not listed: they
// cannot do anything but the MFA step and expire within minutes.
func (tx *Tx) ListAdminSessions(adminID string) ([]Session, error) {
	now := tx.nowMS()
	return collect(tx, `SELECT `+sessionCols+` FROM sessions
		WHERE admin_id = ? AND revoked_at IS NULL AND mfa_verified_at IS NOT NULL AND expires_at > ? AND absolute_expires_at > ?
		ORDER BY last_seen_at DESC, id DESC`, []any{adminID, now, now}, scanSession)
}

// RevokeAdminSession deletes one session of the account. A session of another
// account or an unknown id is ErrNotFound.
func (tx *Tx) RevokeAdminSession(adminID, id string) error {
	res, err := tx.Exec(`DELETE FROM sessions WHERE id = ? AND admin_id = ?`, id, adminID)
	if err != nil {
		return err
	}
	return requireRow(res)
}

// DeleteAdminSessions deletes every session of the account except keepID ("" keeps none).
func (tx *Tx) DeleteAdminSessions(adminID, keepID string) (int64, error) {
	res, err := tx.Exec(`DELETE FROM sessions WHERE admin_id = ? AND id <> ?`, adminID, keepID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
