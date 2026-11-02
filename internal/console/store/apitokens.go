package store

import (
	"database/sql"
	"encoding/json"
	"time"
)

// APIToken is a personal API token (architecture §5.2 /me/api-tokens, feature
// row r101). Only the SHA-256 of the secret is stored; TokenPrefix is its
// first characters, for display. Scopes is the scope list in words ("read" or
// "full"); the token never acts beyond its account's role and customer scope.
type APIToken struct {
	ID          string
	AdminID     string
	Name        string
	Scopes      []string
	TokenPrefix string
	TokenHash   string
	LastUsedAt  *time.Time
	ExpiresAt   *time.Time
	RevokedAt   *time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

const apiTokenCols = `id, admin_id, name, scopes, token_prefix, token_hash, last_used_at, expires_at, revoked_at, created_at, updated_at`

func scanAPIToken(sc scanner) (APIToken, error) {
	var t APIToken
	var scopes string
	var last, exp, rev sql.NullInt64
	var c, u int64
	if err := sc.Scan(&t.ID, &t.AdminID, &t.Name, &scopes, &t.TokenPrefix, &t.TokenHash, &last, &exp, &rev, &c, &u); err != nil {
		return APIToken{}, err
	}
	if err := json.Unmarshal([]byte(scopes), &t.Scopes); err != nil {
		return APIToken{}, invalidf("api token %s has malformed scopes", t.ID)
	}
	t.LastUsedAt, t.ExpiresAt, t.RevokedAt = ptrTime(last), ptrTime(exp), ptrTime(rev)
	t.CreatedAt, t.UpdatedAt = timeOf(c), timeOf(u)
	return t, nil
}

// CreateAPIToken inserts a token; ID and timestamps are assigned. A second
// active token with the same name for the same account is ErrConflict.
func (tx *Tx) CreateAPIToken(t APIToken) (APIToken, error) {
	if t.AdminID == "" || t.Name == "" || t.TokenHash == "" || t.TokenPrefix == "" || len(t.Scopes) == 0 {
		return APIToken{}, invalidf("api token needs an account, a name, a hash, a prefix and a scope")
	}
	b, err := json.Marshal(t.Scopes)
	if err != nil {
		return APIToken{}, err
	}
	t.ID = tx.id()
	now := tx.nowMS()
	if _, err := tx.Exec(`INSERT INTO api_tokens (`+apiTokenCols+`) VALUES (?,?,?,?,?,?,NULL,?,NULL,?,?)`,
		t.ID, t.AdminID, t.Name, string(b), t.TokenPrefix, t.TokenHash, nullTime(t.ExpiresAt), now, now); err != nil {
		return APIToken{}, err
	}
	return one(tx, `SELECT `+apiTokenCols+` FROM api_tokens WHERE id = ?`, []any{t.ID}, scanAPIToken)
}

// ListAPITokens returns the account's tokens, newest first, revoked ones included.
func (tx *Tx) ListAPITokens(adminID string) ([]APIToken, error) {
	return collect(tx, `SELECT `+apiTokenCols+` FROM api_tokens WHERE admin_id = ? ORDER BY created_at DESC, id DESC`, []any{adminID}, scanAPIToken)
}

// GetLiveAPITokenByHash returns the token behind a secret's hash when it is
// neither revoked nor expired; anything else is ErrNotFound, so a caller cannot
// forget a check.
func (tx *Tx) GetLiveAPITokenByHash(hash string) (APIToken, error) {
	now := tx.nowMS()
	return one(tx, `SELECT `+apiTokenCols+` FROM api_tokens WHERE token_hash = ? AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > ?)`,
		[]any{hash, now}, scanAPIToken)
}

// RevokeAPIToken revokes one of the account's tokens. A token of another
// account, an unknown id and an already revoked token are ErrNotFound.
func (tx *Tx) RevokeAPIToken(adminID, id string) (APIToken, error) {
	now := tx.nowMS()
	res, err := tx.Exec(`UPDATE api_tokens SET revoked_at = ?, updated_at = ? WHERE id = ? AND admin_id = ? AND revoked_at IS NULL`, now, now, id, adminID)
	if err != nil {
		return APIToken{}, err
	}
	if err := requireRow(res); err != nil {
		return APIToken{}, err
	}
	return one(tx, `SELECT `+apiTokenCols+` FROM api_tokens WHERE id = ?`, []any{id}, scanAPIToken)
}

// RevokeAdminAPITokens revokes every active token of the account (disable).
func (tx *Tx) RevokeAdminAPITokens(adminID string) (int64, error) {
	now := tx.nowMS()
	res, err := tx.Exec(`UPDATE api_tokens SET revoked_at = ?, updated_at = ? WHERE admin_id = ? AND revoked_at IS NULL`, now, now, adminID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// TouchAPIToken records a use (last_used_at = now).
func (tx *Tx) TouchAPIToken(id string) error {
	now := tx.nowMS()
	_, err := tx.Exec(`UPDATE api_tokens SET last_used_at = ?, updated_at = ? WHERE id = ?`, now, now, id)
	return err
}
