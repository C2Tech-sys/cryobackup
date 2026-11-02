package store

import (
	"context"
	"database/sql"
	"strings"
	"time"
	"unicode"
)

// Enrollment-token management for the console (M2-W2B; architecture §5.2
// /enrollment-tokens, §5.3 /enroll). The secret itself is never here: the
// handler generates it, passes only its SHA-256 hex, and shows the plaintext
// once. Nothing in this file, and no audit row it writes, holds either the
// secret or its hash.

// Limits on a token.
const (
	// MaxTokenLabel is the longest label, in characters.
	MaxTokenLabel = 100
	// MaxTokenLifetime is the longest a token may live. A token is a bearer
	// credential for enrolling devices; months is not a lifetime that should be
	// reachable from a form.
	MaxTokenLifetime = 90 * 24 * time.Hour
	// MaxTokenUses is the largest use cap.
	MaxTokenUses = 10000
)

// Token statuses, in the order of precedence Status applies them.
const (
	TokenActive  = "active"
	TokenRevoked = "revoked"
	TokenExpired = "expired"
	TokenUsedUp  = "used_up"
)

// TokenStatuses lists the statuses the token list can filter on.
var TokenStatuses = []string{TokenActive, TokenUsedUp, TokenExpired, TokenRevoked}

// Status is where the token stands at now: revoked wins over expired, expired
// over used up, and only a token that is none of those is active.
func (t EnrollmentToken) Status(now time.Time) string {
	switch {
	case t.RevokedAt != nil:
		return TokenRevoked
	case !t.ExpiresAt.After(now):
		return TokenExpired
	case t.MaxUses != nil && t.UsedCount >= *t.MaxUses:
		return TokenUsedUp
	}
	return TokenActive
}

// UsesLeft is the remaining enrollments, or -1 for a token without a cap.
func (t EnrollmentToken) UsesLeft() int {
	if t.MaxUses == nil {
		return -1
	}
	if left := *t.MaxUses - t.UsedCount; left > 0 {
		return left
	}
	return 0
}

// EnrollTokenRow is a token with the names the list shows.
type EnrollTokenRow struct {
	EnrollmentToken
	CustomerName string
	// CreatedByName is the username that created it ("" when the account was
	// removed or the token came from the CLI).
	CreatedByName string
	PolicyName    string
	// Devices counts the devices enrolled with this token (retired ones too).
	Devices int
	// LastUsedAt is the latest enrollment through this token (nil: never used).
	LastUsedAt *time.Time
}

// EnrollTokenFilter selects and pages the token list.
type EnrollTokenFilter struct {
	CustomerID string
	// Status is "" (all) or one of TokenStatuses.
	Status     string
	Page, Size int
	// Offset and Limit (Limit > 0) select a row window instead of a numbered page,
	// for the JSON API whose cursor is a row offset.
	Offset, Limit int
}

// EnrollTokenList is one page of tokens.
type EnrollTokenList struct {
	Rows []EnrollTokenRow
	Page PageInfo
	// All counts every token inside the caller's scope, before the filter; a
	// filtered-empty list and a never-used console look different on the page.
	All int
}

// ValidTokenStatus reports whether s is a status the list filters on.
func ValidTokenStatus(s string) bool { return oneOf(s, TokenStatuses) }

func tokenRowCols() string {
	return prefixCols(enrollTokenCols, "t.") + `, c.name, a.username, p.name,
	(SELECT MAX(d.enrolled_at) FROM devices d WHERE d.enrollment_token_id = t.id),
	(SELECT COUNT(*) FROM devices d WHERE d.enrollment_token_id = t.id)`
}

func tokenRowFrom() string {
	return ` FROM enrollment_tokens t
	JOIN customers c ON c.id = t.customer_id
	LEFT JOIN admin_users a ON a.id = t.created_by
	LEFT JOIN policies p ON p.id = t.policy_id`
}

func scanTokenRow(sc scanner) (EnrollTokenRow, error) {
	var r EnrollTokenRow
	var label, pol, adopts, fp, by sql.NullString
	var maxUses, revoked, last sql.NullInt64
	var exp, c, u int64
	var custName string
	var creator, policyName sql.NullString
	err := sc.Scan(&r.ID, &r.CustomerID, &label, &r.TokenHash, &pol, &r.DeviceClassDefault, &adopts, &exp, &maxUses,
		&r.UsedCount, &fp, &by, &revoked, &c, &u, &custName, &creator, &policyName, &last, &r.Devices)
	if err != nil {
		return EnrollTokenRow{}, err
	}
	r.Label, r.PolicyID, r.AdoptsDeviceID, r.ServerFingerprint, r.CreatedBy = label.String, pol.String, adopts.String, fp.String, by.String
	r.ExpiresAt, r.MaxUses, r.RevokedAt = timeOf(exp), ptrInt(maxUses), ptrTime(revoked)
	r.CreatedAt, r.UpdatedAt = timeOf(c), timeOf(u)
	r.CustomerName, r.CreatedByName, r.PolicyName, r.LastUsedAt = custName, creator.String, policyName.String, ptrTime(last)
	return r, nil
}

// statusClause is the SQL form of Status for a token alias t at time now.
func statusClause(status string, now int64) (string, []any) {
	switch status {
	case TokenRevoked:
		return `t.revoked_at IS NOT NULL`, nil
	case TokenExpired:
		return `t.revoked_at IS NULL AND t.expires_at <= ?`, []any{now}
	case TokenUsedUp:
		return `t.revoked_at IS NULL AND t.expires_at > ? AND t.max_uses IS NOT NULL AND t.used_count >= t.max_uses`, []any{now}
	case TokenActive:
		return `t.revoked_at IS NULL AND t.expires_at > ? AND (t.max_uses IS NULL OR t.used_count < t.max_uses)`, []any{now}
	}
	return `1 = 1`, nil
}

// ListEnrollmentTokenRows returns one page of the tokens inside scope, newest
// first, with customer, creator and policy names and the last-use time. A
// filter naming a customer outside scope returns nothing (not an error: the
// list only ever holds rows the caller may see).
func (s *Store) ListEnrollmentTokenRows(ctx context.Context, scope Scope, f EnrollTokenFilter) (EnrollTokenList, error) {
	if err := scope.check(); err != nil {
		return EnrollTokenList{}, err
	}
	if f.Status != "" && !ValidTokenStatus(f.Status) {
		return EnrollTokenList{}, validation([]FieldError{fieldf("status", "Use active, used_up, expired or revoked.")})
	}
	clause, args := scope.clause("t.customer_id")
	where, wargs := clause, args
	if f.CustomerID != "" {
		where, wargs = where+` AND t.customer_id = ?`, append(append([]any(nil), wargs...), f.CustomerID)
	}
	var out EnrollTokenList
	err := s.Read(ctx, func(tx *Tx) error {
		if err := tx.QueryRow(`SELECT count(*) FROM enrollment_tokens t WHERE `+clause, args...).Scan(&out.All); err != nil {
			return err
		}
		sc, sargs := statusClause(f.Status, tx.nowMS())
		full, fargs := where+` AND `+sc, append(append([]any(nil), wargs...), sargs...)
		var total int
		if err := tx.QueryRow(`SELECT count(*) FROM enrollment_tokens t WHERE `+full, fargs...).Scan(&total); err != nil {
			return err
		}
		from, to, info := windowOf(total, f.Page, f.Size, f.Offset, f.Limit)
		out.Page = info
		var err error
		out.Rows, err = collect(tx, `SELECT `+tokenRowCols()+tokenRowFrom()+` WHERE `+full+
			` ORDER BY t.created_at DESC, t.id DESC LIMIT ? OFFSET ?`, append(fargs, to-from, from), scanTokenRow)
		return err
	})
	return out, err
}

// GetEnrollmentTokenRow returns one token inside scope (ErrNotFound outside it).
func (s *Store) GetEnrollmentTokenRow(ctx context.Context, scope Scope, id string) (EnrollTokenRow, error) {
	if err := scope.check(); err != nil {
		return EnrollTokenRow{}, err
	}
	clause, args := scope.clause("t.customer_id")
	var out EnrollTokenRow
	err := s.Read(ctx, func(tx *Tx) error {
		var err error
		out, err = one(tx, `SELECT `+tokenRowCols()+tokenRowFrom()+` WHERE t.id = ? AND `+clause, append([]any{id}, args...), scanTokenRow)
		return err
	})
	return out, err
}

// EnrolledDevice is a device that enrolled through a token.
type EnrolledDevice struct {
	ID         string
	Hostname   string
	EnrolledAt time.Time
	Retired    bool
}

// ListEnrollmentTokenDevices returns the devices enrolled through a token
// inside scope, newest first (ErrNotFound when the token is outside scope).
func (s *Store) ListEnrollmentTokenDevices(ctx context.Context, scope Scope, tokenID string) ([]EnrolledDevice, error) {
	if err := scope.check(); err != nil {
		return nil, err
	}
	clause, args := scope.clause("t.customer_id")
	var out []EnrolledDevice
	err := s.Read(ctx, func(tx *Tx) error {
		var found int
		if err := tx.QueryRow(`SELECT 1 FROM enrollment_tokens t WHERE t.id = ? AND `+clause, append([]any{tokenID}, args...)...).Scan(&found); err != nil {
			if err == sql.ErrNoRows {
				return ErrNotFound
			}
			return err
		}
		var err error
		out, err = collect(tx, `SELECT d.id, d.hostname, d.enrolled_at, d.retired_at IS NOT NULL FROM devices d
			WHERE d.enrollment_token_id = ? ORDER BY d.enrolled_at DESC, d.id DESC LIMIT 500`, []any{tokenID}, func(sc scanner) (EnrolledDevice, error) {
			var d EnrolledDevice
			var at int64
			err := sc.Scan(&d.ID, &d.Hostname, &at, &d.Retired)
			d.EnrolledAt = timeOf(at)
			return d, err
		})
		return err
	})
	return out, err
}

// EnrollTokenInput is what the console form (or the API) asks for. The caller
// supplies the SHA-256 hex of a secret it generated; the plaintext never reaches
// the store.
type EnrollTokenInput struct {
	CustomerID  string
	Label       string
	PolicyID    string // "" = the customer's own policy (or the default)
	DeviceClass string // laptop | desktop | unknown | ""
	ExpiresAt   time.Time
	MaxUses     *int // nil = unlimited
	TokenHash   string
}

// ValidateEnrollTokenInput checks what can be checked without the database, as
// one error per field (fixed sentences that never echo the input).
func ValidateEnrollTokenInput(in EnrollTokenInput, now time.Time) []FieldError {
	var errs []FieldError
	if strings.TrimSpace(in.CustomerID) == "" {
		errs = append(errs, fieldf("customer_id", "Choose a customer."))
	}
	if n := len([]rune(in.Label)); n > MaxTokenLabel {
		errs = append(errs, fieldf("label", "Use at most %d characters.", MaxTokenLabel))
	}
	for _, r := range in.Label {
		if unicode.IsControl(r) {
			errs = append(errs, fieldf("label", "Use plain text on one line."))
			break
		}
	}
	switch {
	case in.ExpiresAt.IsZero() || !in.ExpiresAt.After(now):
		errs = append(errs, fieldf("expires", "Choose an expiry in the future."))
	case in.ExpiresAt.After(now.Add(MaxTokenLifetime)):
		errs = append(errs, fieldf("expires", "A token can live at most %d days.", int(MaxTokenLifetime/(24*time.Hour))))
	}
	if in.MaxUses != nil && (*in.MaxUses < 1 || *in.MaxUses > MaxTokenUses) {
		errs = append(errs, fieldf("max_uses", "Use a number from 1 to %d, or unlimited.", MaxTokenUses))
	}
	if in.DeviceClass != "" && !oneOf(in.DeviceClass, []string{"laptop", "desktop", "unknown"}) {
		errs = append(errs, fieldf("device_class", "Use laptop, desktop or unknown."))
	}
	return errs
}

// CreateEnrollmentTokenAudited creates a token for a customer inside scope and
// writes its audit row (enrollment_token.create) in the same transaction: a
// token whose audit row cannot be written is not created (ErrAudit). The row
// holds the expiry, use cap and whether a policy was named; never the secret, its
// hash or the label (free text).
//
// A customer outside scope is ErrNotFound; an archived customer is a field
// error on customer_id (a token for it would enroll devices into a customer
// nobody looks at).
func (s *Store) CreateEnrollmentTokenAudited(ctx context.Context, scope Scope, in EnrollTokenInput, actor AuditActor) (EnrollmentToken, error) {
	if err := scope.check(); err != nil {
		return EnrollmentToken{}, err
	}
	if !scope.Allows(in.CustomerID) {
		return EnrollmentToken{}, ErrNotFound
	}
	in.Label = strings.TrimSpace(in.Label)
	if errs := ValidateEnrollTokenInput(in, s.Now()); len(errs) > 0 {
		return EnrollmentToken{}, validation(errs)
	}
	if len(in.TokenHash) != 64 {
		return EnrollmentToken{}, invalidf("enrollment token hash is not a SHA-256 hex digest")
	}
	var out EnrollmentToken
	err := s.Write(ctx, func(tx *Tx) error {
		var state string
		if err := tx.QueryRow(`SELECT state FROM customers WHERE id = ?`, in.CustomerID).Scan(&state); err != nil {
			if err == sql.ErrNoRows {
				return ErrNotFound
			}
			return err
		}
		if state == "archived" {
			return validation([]FieldError{fieldf("customer_id", "This customer is archived. Restore it before enrolling devices.")})
		}
		if err := checkCustomerPolicy(tx, in.CustomerID, in.PolicyID); err != nil {
			return policyFieldError(err)
		}
		id, now := tx.id(), tx.nowMS()
		class := in.DeviceClass
		if class == "" {
			class = "unknown"
		}
		// created_by names the account only when it exists (an API token's owner
		// that was deleted meanwhile leaves it empty; the audit row still names the actor).
		if _, err := tx.Exec(`INSERT INTO enrollment_tokens (`+enrollTokenCols+`) VALUES (?,?,?,?,?,?,?,?,?,0,?,(SELECT id FROM admin_users WHERE id = ?),?,?,?)`,
			id, in.CustomerID, nullStr(in.Label), in.TokenHash, nullStr(in.PolicyID), class, nil, msOf(in.ExpiresAt), nullInt(in.MaxUses),
			nil, actor.AdminID, nil, now, now); err != nil {
			return mapErr(err)
		}
		details := map[string]any{"expires_at": in.ExpiresAt.UTC().Format(time.RFC3339), "policy_named": in.PolicyID != "", "device_class": class}
		if in.MaxUses != nil {
			details["max_uses"] = *in.MaxUses
		} else {
			details["max_uses"] = nil
		}
		ev, err := actor.event("enrollment_token.create", "enrollment_token", id, in.CustomerID, details)
		if err != nil {
			return err
		}
		if err := audited(tx, ev); err != nil {
			return err
		}
		out, err = one(tx, `SELECT `+enrollTokenCols+` FROM enrollment_tokens WHERE id = ?`, []any{id}, scanEnrollToken)
		return err
	})
	return out, err
}

// RevokeEnrollmentTokenAudited revokes a token inside scope and writes its audit
// row (enrollment_token.revoke) in the same transaction (ErrAudit when it cannot
// be written; the token then stays valid). It reports whether anything changed:
// revoking a revoked token is a no-op with no audit row. Devices that already
// enrolled are unaffected; the token stops enrolling at once, because the agent
// enroll path reads revoked_at on every call.
func (s *Store) RevokeEnrollmentTokenAudited(ctx context.Context, scope Scope, id string, actor AuditActor) (bool, error) {
	if err := scope.check(); err != nil {
		return false, err
	}
	clause, args := scope.clause("customer_id")
	changed := false
	err := s.Write(ctx, func(tx *Tx) error {
		var cust string
		var revoked sql.NullInt64
		var used int
		if err := tx.QueryRow(`SELECT customer_id, revoked_at, used_count FROM enrollment_tokens WHERE id = ? AND `+clause, append([]any{id}, args...)...).
			Scan(&cust, &revoked, &used); err != nil {
			if err == sql.ErrNoRows {
				return ErrNotFound
			}
			return err
		}
		if revoked.Valid {
			return nil
		}
		now := tx.nowMS()
		if _, err := tx.Exec(`UPDATE enrollment_tokens SET revoked_at = ?, updated_at = ? WHERE id = ? AND revoked_at IS NULL`, now, now, id); err != nil {
			return err
		}
		ev, err := actor.event("enrollment_token.revoke", "enrollment_token", id, cust, map[string]any{"used_count": used})
		if err != nil {
			return err
		}
		if err := audited(tx, ev); err != nil {
			return err
		}
		changed = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return changed, nil
}
