package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/ulid"
)

// ---------------------------------------------------------------------------
// enrollment_tokens
// ---------------------------------------------------------------------------

// EnrollmentToken is a customer-scoped, expiring, optionally use-capped token an
// installer presents. Only its SHA-256 hex is stored.
type EnrollmentToken struct {
	ID                 string
	CustomerID         string
	Label              string
	TokenHash          string // lower-case sha256 hex, 64 chars
	PolicyID           string // applied at enrollment as the device override (enroll_policy_m2.go)
	DeviceClassDefault string // laptop | desktop | unknown ("" = unknown)
	AdoptsDeviceID     string // replace-device tokens (F21); not supported by AgentEnroll in v0
	ExpiresAt          time.Time
	MaxUses            *int // nil = unlimited
	UsedCount          int
	ServerFingerprint  string
	CreatedBy          string
	RevokedAt          *time.Time
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

const enrollTokenCols = `id, customer_id, label, token_hash, policy_id, device_class_default, adopts_device_id, expires_at,
	max_uses, used_count, server_fingerprint, created_by, revoked_at, created_at, updated_at`

func scanEnrollToken(sc scanner) (EnrollmentToken, error) {
	var t EnrollmentToken
	var label, pol, adopts, fp, by sql.NullString
	var maxUses, revoked sql.NullInt64
	var exp, c, u int64
	err := sc.Scan(&t.ID, &t.CustomerID, &label, &t.TokenHash, &pol, &t.DeviceClassDefault, &adopts, &exp, &maxUses,
		&t.UsedCount, &fp, &by, &revoked, &c, &u)
	if err != nil {
		return EnrollmentToken{}, err
	}
	t.Label, t.PolicyID, t.AdoptsDeviceID, t.ServerFingerprint, t.CreatedBy = label.String, pol.String, adopts.String, fp.String, by.String
	t.ExpiresAt, t.MaxUses, t.RevokedAt = timeOf(exp), ptrInt(maxUses), ptrTime(revoked)
	t.CreatedAt, t.UpdatedAt = timeOf(c), timeOf(u)
	return t, nil
}

// CreateEnrollmentToken inserts a token for a customer inside scope. The caller
// generates the secret and passes its SHA-256 hex; ExpiresAt is required and
// must lie in the future. AdoptsDeviceID, when set, must name a device inside
// scope (ErrNotFound otherwise) that belongs to t.CustomerID (ErrInvalid
// otherwise); PolicyID, when set, must be a template or a policy owned by
// t.CustomerID (ErrInvalid otherwise).
func (s *Store) CreateEnrollmentToken(ctx context.Context, scope Scope, t EnrollmentToken) (EnrollmentToken, error) {
	if err := scope.check(); err != nil {
		return EnrollmentToken{}, err
	}
	if !scope.Allows(t.CustomerID) {
		return EnrollmentToken{}, ErrNotFound
	}
	if t.DeviceClassDefault == "" {
		t.DeviceClassDefault = "unknown"
	}
	var out EnrollmentToken
	err := s.Write(ctx, func(tx *Tx) error {
		t.ID = tx.id()
		now := tx.nowMS()
		if msOf(t.ExpiresAt) <= now {
			return invalidf("enrollment token must expire in the future")
		}
		// §3.5: no cross-customer reference passes through the store. An adopted
		// device must be inside the caller's scope (else ErrNotFound) and belong
		// to the token's own customer (else ErrInvalid).
		if t.AdoptsDeviceID != "" {
			owner, err := scopedDevice(tx, scope, t.AdoptsDeviceID)
			if err != nil {
				return err
			}
			if owner != t.CustomerID {
				return invalidf("adopted device belongs to a different customer")
			}
		}
		if err := checkCustomerPolicy(tx, t.CustomerID, t.PolicyID); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO enrollment_tokens (`+enrollTokenCols+`) VALUES (?,?,?,?,?,?,?,?,?,0,?,?,?,?,?)`,
			t.ID, t.CustomerID, nullStr(t.Label), t.TokenHash, nullStr(t.PolicyID), t.DeviceClassDefault, nullStr(t.AdoptsDeviceID),
			msOf(t.ExpiresAt), nullInt(t.MaxUses), nullStr(t.ServerFingerprint), nullStr(t.CreatedBy), nullTime(t.RevokedAt), now, now); err != nil {
			return err
		}
		var err error
		out, err = one(tx, `SELECT `+enrollTokenCols+` FROM enrollment_tokens WHERE id = ?`, []any{t.ID}, scanEnrollToken)
		return err
	})
	return out, err
}

// ListEnrollmentTokens returns tokens inside scope, newest first; customerID ""
// means every customer in scope. A deleted token is not listed.
func (s *Store) ListEnrollmentTokens(ctx context.Context, scope Scope, customerID string) ([]EnrollmentToken, error) {
	if err := scope.check(); err != nil {
		return nil, err
	}
	clause, args := scope.clause("customer_id")
	q := `SELECT ` + enrollTokenCols + ` FROM enrollment_tokens WHERE deleted_at IS NULL AND ` + clause
	if customerID != "" {
		q, args = q+` AND customer_id = ?`, append(args, customerID)
	}
	var out []EnrollmentToken
	err := s.Read(ctx, func(tx *Tx) error {
		var err error
		out, err = collect(tx, q+` ORDER BY created_at DESC, id DESC`, args, scanEnrollToken)
		return err
	})
	return out, err
}

// RevokeEnrollmentToken revokes a token inside scope. Already-enrolled devices
// are unaffected. Revoking a revoked token is a no-op.
func (s *Store) RevokeEnrollmentToken(ctx context.Context, scope Scope, id string) error {
	if err := scope.check(); err != nil {
		return err
	}
	clause, args := scope.clause("customer_id")
	return s.Write(ctx, func(tx *Tx) error {
		now := tx.nowMS()
		res, err := tx.Exec(`UPDATE enrollment_tokens SET revoked_at = COALESCE(revoked_at, ?), updated_at = ?
			WHERE id = ? AND `+clause, append([]any{now, now, id}, args...)...)
		if err != nil {
			return err
		}
		return requireRow(res)
	})
}

// GetEnrollmentTokenByHash returns the token with this SHA-256 hex without
// spending a use (ErrNotFound for an unknown hash). It is the agent-side lookup
// of POST /enroll, where the hash is the credential, so it takes no Scope; the
// caller decides what a revoked, expired or used-up token means.
func (s *Store) GetEnrollmentTokenByHash(ctx context.Context, tokenHash string) (EnrollmentToken, error) {
	var out EnrollmentToken
	err := s.Read(ctx, func(tx *Tx) error {
		var err error
		out, err = one(tx, `SELECT `+enrollTokenCols+` FROM enrollment_tokens WHERE token_hash = ?`, []any{tokenHash}, scanEnrollToken)
		return err
	})
	return out, err
}

// EnrollNonceKnown reports whether a device was enrolled with this nonce. It is
// how POST /enroll tells the replay of a used-up token from a stranger's call.
func (s *Store) EnrollNonceKnown(ctx context.Context, nonce string) (bool, error) {
	var n int
	err := s.Read(ctx, func(tx *Tx) error {
		return tx.QueryRow(`SELECT count(*) FROM devices WHERE enroll_nonce = ?`, nonce).Scan(&n)
	})
	return n > 0, err
}

// ConsumeEnrollmentToken atomically spends one use of the token with this hash
// and returns it as it stands after the increment. It is a single guarded
// UPDATE ... RETURNING, so two concurrent consumers of a max_uses=1 token can
// never both win: the loser gets ErrConflict. A hash that matches nothing is
// ErrNotFound; a token that is revoked, expired or used up is ErrConflict.
//
// AgentEnroll spends the token itself inside its own transaction (so a failed
// enrollment does not burn a use); call this directly only for flows that do
// not create a device.
func (s *Store) ConsumeEnrollmentToken(ctx context.Context, tokenHash string) (EnrollmentToken, error) {
	var out EnrollmentToken
	err := s.Write(ctx, func(tx *Tx) error {
		var err error
		out, err = consumeToken(tx, tokenHash)
		return err
	})
	return out, err
}

func consumeToken(tx *Tx, tokenHash string) (EnrollmentToken, error) {
	now := tx.nowMS()
	t, err := one(tx, `UPDATE enrollment_tokens SET used_count = used_count + 1, updated_at = ?
		WHERE token_hash = ? AND revoked_at IS NULL AND deleted_at IS NULL AND expires_at > ? AND (max_uses IS NULL OR used_count < max_uses)
		RETURNING `+enrollTokenCols, []any{now, tokenHash, now}, scanEnrollToken)
	if err == nil {
		return t, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return EnrollmentToken{}, err
	}
	// The guarded UPDATE touched nothing: say why (unknown vs spent), without leaking which token.
	var exists int
	if qerr := tx.QueryRow(`SELECT 1 FROM enrollment_tokens WHERE token_hash = ?`, tokenHash).Scan(&exists); errors.Is(qerr, sql.ErrNoRows) {
		return EnrollmentToken{}, ErrNotFound
	} else if qerr != nil {
		return EnrollmentToken{}, qerr
	}
	return EnrollmentToken{}, conflictf("enrollment token is revoked, expired or used up")
}

// ---------------------------------------------------------------------------
// AgentEnroll (DH17)
// ---------------------------------------------------------------------------

// EnrollInput is everything AgentEnroll needs. The store never generates or
// sees a plaintext secret: the console handler generates the device token, the
// gateway password and the repository password, hashes or wraps them, and
// passes only the results (architecture §8.2).
type EnrollInput struct {
	// TokenHash is the SHA-256 hex of the enrollment token presented (Bearer).
	TokenHash string
	// Nonce is the agent-generated idempotency key.
	Nonce string
	// DeviceID, when set, is the 26-character ULID the new device gets (ErrInvalid
	// when malformed, ErrConflict when a device already has it). The console
	// generates it before it wraps the repository key and the gateway secret, so
	// the wraps are bound to the real id from the start. Empty means the store
	// generates one. A replay of a known nonce ignores it: the stored device wins.
	DeviceID string

	Hostname      string
	OSVersion     string
	Arch          string // amd64 | arm64 | x86 | ""
	DeviceClass   string // laptop | desktop | unknown | "" (falls back to the token's default)
	HWFingerprint string
	AgentVersion  string
	ResticVersion string

	// DeviceTokenHash is the SHA-256 hex of a freshly generated device token. On
	// a replay of an unconfirmed enrollment it replaces the earlier token (the
	// plaintext cannot be recovered from a hash, so a retry is issued a new one).
	DeviceTokenHash string
	// GatewayPasswordHash is the bcrypt hash (htpasswd-compatible) of the gateway
	// password; GatewaySecretWrapped is that password wrapped under the console
	// master key, kept until the agent acknowledges delivery. Both are used only
	// when the enrollment creates the device; a replay re-delivers the stored ones.
	GatewayPasswordHash  string
	GatewaySecretWrapped []byte
	// RepoKeyWrapped, WrapAlg and MasterKeyID describe the escrowed repository
	// password. They are required when the customer's key_custody is escrowed and
	// ignored when it is customer_held.
	RepoKeyWrapped []byte
	WrapAlg        string
	MasterKeyID    string
	// FirstBackupGrace overrides the first-backup grace (devices.first_backup_due_by
	// = now + grace); zero uses the effective policy's sla.first_backup_grace_hours.
	FirstBackupGrace time.Duration
	// SourceIP is recorded in the audit event.
	SourceIP string
}

// EnrollResult is what the enroll response is built from.
type EnrollResult struct {
	Device Device
	// Replay is true when the nonce had been seen: the same device is returned and
	// the earlier secrets are re-delivered (until enrollment is confirmed).
	Replay bool
	// GatewayUsername is the gateway login name; it equals Device.ID.
	GatewayUsername string
	// GatewaySecretWrapped is the still-undelivered gateway password, wrapped.
	GatewaySecretWrapped []byte
	// Custody is the customer's key custody. For escrowed custody the wrapped
	// repository key follows; for customer_held they are empty.
	Custody        string
	RepoKeyWrapped []byte
	WrapAlg        string
	MasterKeyID    string
	// DeviceTokenID is the id of the device token issued by this call.
	DeviceTokenID string
	// TokenPolicy says what became of the token's policy on a new enrollment:
	// "" (none named), "applied", "same" or "skipped". TokenPolicySkipReason is a
	// path-free class when it was skipped; the caller logs it. A replay leaves both empty.
	TokenPolicy           string
	TokenPolicySkipReason string
}

// AgentEnroll creates (or replays) a device enrollment in ONE transaction:
// it spends one use of the enrollment token, creates the device with its repo
// path and first-backup deadline, a device token, a gateway login (username =
// device id) with its pending wrapped secret, the escrow row, the maintenance
// schedule row, flips settings.fleet_ever_enrolled, and appends an audit event.
// Either all of it exists afterwards or none of it does.
//
// Idempotency (DH17): the nonce is the key. A second call with the same nonce
// and the same enrollment token, made before the device's first authenticated
// call (enroll_confirmed_at), returns the SAME device, the SAME pending gateway
// secret and the SAME wrapped repository key, spends no token use, and issues a
// new device token (the old one is revoked: a hash cannot be turned back into
// the plaintext the lost response carried). After confirmation, with a
// different token, after the token expired, or with a hw_fingerprint other than
// the stored one, a reused nonce is ErrConflict, so a leaked nonce cannot be
// used to re-read secrets.
//
// Errors: ErrNotFound (unknown token), ErrConflict (token revoked, expired or
// used up; nonce reused after confirmation or with another token), ErrInvalid
// (bad input, adoption tokens, a missing escrow key for escrowed custody).
func (s *Store) AgentEnroll(ctx context.Context, in EnrollInput) (EnrollResult, error) {
	if in.Nonce == "" || in.TokenHash == "" || in.Hostname == "" || in.DeviceTokenHash == "" {
		return EnrollResult{}, invalidf("token hash, nonce, hostname and device token hash are required")
	}
	if in.DeviceID != "" && !ulid.Valid(in.DeviceID) {
		return EnrollResult{}, invalidf("device id must be a 26-character ULID")
	}
	var out EnrollResult
	err := s.Write(ctx, func(tx *Tx) error {
		existing, err := one(tx, `SELECT `+deviceCols+` FROM devices WHERE enroll_nonce = ?`, []any{in.Nonce}, scanDevice)
		switch {
		case err == nil:
			out, err = replayEnroll(tx, existing, in)
			return err
		case !errors.Is(err, ErrNotFound):
			return err
		}
		out, err = createEnrollment(tx, in)
		return err
	})
	return out, err
}

func replayEnroll(tx *Tx, d Device, in EnrollInput) (EnrollResult, error) {
	if d.EnrollConfirmedAt != nil {
		return EnrollResult{}, conflictf("enrollment nonce already used and confirmed")
	}
	if d.RetiredAt != nil {
		return EnrollResult{}, conflictf("device was retired")
	}
	tok, err := one(tx, `SELECT `+enrollTokenCols+` FROM enrollment_tokens WHERE id = ?`, []any{d.EnrollmentTokenID}, scanEnrollToken)
	if err != nil || tok.TokenHash != in.TokenHash {
		return EnrollResult{}, conflictf("enrollment nonce belongs to a different token")
	}
	if tok.RevokedAt != nil {
		return EnrollResult{}, conflictf("enrollment token was revoked")
	}
	now := tx.nowMS()
	// A replay re-delivers the wrapped secrets, so it is held to the token's
	// lifetime and to the machine that enrolled: an expired token, or a retry
	// from hardware that is not the stored fingerprint, never re-reads them.
	if msOf(tok.ExpiresAt) <= now {
		return EnrollResult{}, conflictf("enrollment token has expired")
	}
	if in.HWFingerprint != d.HWFingerprint {
		return EnrollResult{}, conflictf("enrollment replay does not match the enrolled device")
	}
	// Re-issue the device token: revoke whatever is outstanding, insert the new hash.
	if _, err := tx.Exec(`UPDATE device_tokens SET revoked_at = ?, revoked_reason = 'enrollment replayed', updated_at = ?
		WHERE device_id = ? AND revoked_at IS NULL`, now, now, d.ID); err != nil {
		return EnrollResult{}, err
	}
	tokenID := tx.id()
	if _, err := tx.Exec(`INSERT INTO device_tokens (id, device_id, token_hash, created_at, updated_at) VALUES (?,?,?,?,?)`,
		tokenID, d.ID, in.DeviceTokenHash, now, now); err != nil {
		return EnrollResult{}, err
	}
	res := EnrollResult{Device: d, Replay: true, GatewayUsername: d.ID, DeviceTokenID: tokenID}
	var pending []byte
	if err := tx.QueryRow(`SELECT pending_secret_wrapped FROM gateway_logins WHERE device_id = ? AND revoked_at IS NULL`, d.ID).Scan(&pending); err != nil {
		return EnrollResult{}, err
	}
	res.GatewaySecretWrapped = pending
	esc, err := one(tx, `SELECT `+escrowCols+` FROM repo_key_escrow WHERE device_id = ? AND retired_at IS NULL`, []any{d.ID}, scanEscrow)
	if err != nil {
		return EnrollResult{}, err
	}
	res.Custody, res.RepoKeyWrapped, res.WrapAlg, res.MasterKeyID = esc.Custody, esc.WrappedKey, esc.WrapAlg, esc.MasterKeyID
	return res, nil
}

func createEnrollment(tx *Tx, in EnrollInput) (EnrollResult, error) {
	if in.GatewayPasswordHash == "" {
		return EnrollResult{}, invalidf("gateway password hash is required")
	}
	tok, err := consumeToken(tx, in.TokenHash)
	if err != nil {
		return EnrollResult{}, err
	}
	if tok.AdoptsDeviceID != "" {
		return EnrollResult{}, invalidf("adoption tokens are not supported in this version")
	}
	cust, err := one(tx, `SELECT `+customerCols+` FROM customers WHERE id = ?`, []any{tok.CustomerID}, scanCustomer)
	if err != nil {
		return EnrollResult{}, err
	}
	if cust.State == "archived" {
		return EnrollResult{}, conflictf("customer is archived")
	}
	if cust.DeviceCap != nil {
		var n int
		if err := tx.QueryRow(`SELECT count(*) FROM devices WHERE customer_id = ? AND retired_at IS NULL`, cust.ID).Scan(&n); err != nil {
			return EnrollResult{}, err
		}
		if n >= *cust.DeviceCap {
			return EnrollResult{}, conflictf("customer device cap reached")
		}
	}
	if cust.KeyCustody == "escrowed" && (len(in.RepoKeyWrapped) == 0 || in.WrapAlg == "" || in.MasterKeyID == "") {
		return EnrollResult{}, invalidf("escrowed custody requires a wrapped repository key, wrap_alg and master_key_id")
	}
	var prefix string
	if err := tx.QueryRow(`SELECT prefix FROM storage_targets WHERE id = ?`, cust.StorageTargetID).Scan(&prefix); err != nil {
		return EnrollResult{}, err
	}

	now := tx.nowMS()
	deviceID := in.DeviceID
	if deviceID == "" {
		deviceID = tx.id()
	}
	class := in.DeviceClass
	if class == "" {
		class = tok.DeviceClassDefault
	}
	// The token's policy becomes the device's override layer (enroll_policy_m2.go).
	tokPatch, tokPolName, tokPolStatus, tokPolWhy := tokenPolicyPatch(tx, tok, cust.PolicyID)
	grace := in.FirstBackupGrace
	if grace <= 0 {
		c, err := newComposer(tx)
		if err != nil {
			return EnrollResult{}, err
		}
		hours, err := graceWithPatch(c, cust.PolicyID, tokPatch)
		if err != nil {
			return EnrollResult{}, err
		}
		grace = time.Duration(hours) * time.Hour
	}
	repoPath := strings.Trim(prefix, "/")
	if repoPath != "" {
		repoPath += "/"
	}
	repoPath += deviceID + "/"

	if _, err := tx.Exec(`INSERT INTO devices (id, customer_id, hostname, os_version, arch, device_class, agent_version,
			restic_version, enrolled_at, enrolled_via, enrollment_token_id, enroll_nonce, first_backup_due_by, repo_path,
			hw_fingerprint, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,'token',?,?,?,?,?,?,?)`,
		deviceID, cust.ID, in.Hostname, nullStr(in.OSVersion), nullStr(in.Arch), class, nullStr(in.AgentVersion),
		nullStr(in.ResticVersion), now, tok.ID, in.Nonce, now+grace.Milliseconds(), repoPath, nullStr(in.HWFingerprint), now, now); err != nil {
		return EnrollResult{}, err
	}
	tokenID := tx.id()
	if _, err := tx.Exec(`INSERT INTO device_tokens (id, device_id, token_hash, created_at, updated_at) VALUES (?,?,?,?,?)`,
		tokenID, deviceID, in.DeviceTokenHash, now, now); err != nil {
		return EnrollResult{}, err
	}
	if _, err := tx.Exec(`INSERT INTO gateway_logins (id, device_id, username, password_hash, pending_secret_wrapped, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?)`, tx.id(), deviceID, deviceID, in.GatewayPasswordHash, nullBytes(in.GatewaySecretWrapped), now, now); err != nil {
		return EnrollResult{}, err
	}
	res := EnrollResult{GatewayUsername: deviceID, GatewaySecretWrapped: in.GatewaySecretWrapped, Custody: cust.KeyCustody, DeviceTokenID: tokenID,
		TokenPolicy: tokPolStatus, TokenPolicySkipReason: tokPolWhy}
	if cust.KeyCustody == "escrowed" {
		if _, err := tx.Exec(`INSERT INTO repo_key_escrow (id, device_id, key_version, custody, wrapped_key, wrap_alg, master_key_id,
			created_at, updated_at) VALUES (?,?,1,'escrowed',?,?,?,?,?)`, tx.id(), deviceID, in.RepoKeyWrapped, in.WrapAlg, in.MasterKeyID, now, now); err != nil {
			return EnrollResult{}, err
		}
		res.RepoKeyWrapped, res.WrapAlg, res.MasterKeyID = in.RepoKeyWrapped, in.WrapAlg, in.MasterKeyID
	} else {
		if _, err := tx.Exec(`INSERT INTO repo_key_escrow (id, device_id, key_version, custody, created_at, updated_at)
			VALUES (?,?,1,'customer_held',?,?)`, tx.id(), deviceID, now, now); err != nil {
			return EnrollResult{}, err
		}
	}
	if _, err := tx.Exec(`INSERT INTO maintenance_schedule (device_id, enabled, created_at, updated_at) VALUES (?,?,?,?)`,
		deviceID, boolInt(cust.KeyCustody == "escrowed"), now, now); err != nil {
		return EnrollResult{}, err
	}
	if _, err := tx.Exec(`UPDATE settings SET value = 'true', updated_at = ? WHERE key = 'fleet_ever_enrolled' AND value <> 'true'`, now); err != nil {
		return EnrollResult{}, err
	}
	if tokPolStatus == tokenPolicyApplied {
		if err := insertTokenOverride(tx, deviceID, tokPatch, tokPolName, now); err != nil {
			return EnrollResult{}, err
		}
	}
	var enrollDetails json.RawMessage
	if tokPolStatus != tokenPolicyNone {
		enrollDetails = json.RawMessage(`{"token_policy":"` + tokPolStatus + `"}`)
	}
	if _, err := tx.AppendAudit(AuditEvent{Details: enrollDetails,
		ActorType: "device", ActorID: deviceID, ActorLabel: in.Hostname, Method: "agent", SourceIP: in.SourceIP,
		Action: "device.enroll", ObjectType: "device", ObjectID: deviceID, CustomerID: cust.ID, Result: "ok",
	}); err != nil {
		return EnrollResult{}, err
	}
	res.Device, err = one(tx, `SELECT `+deviceCols+` FROM devices WHERE id = ?`, []any{deviceID}, scanDevice)
	return res, err
}

// ---------------------------------------------------------------------------
// gateway_logins
// ---------------------------------------------------------------------------

// HtpasswdEntry is one line of the gateway's htpasswd file.
type HtpasswdEntry struct {
	Username     string
	PasswordHash string // bcrypt
}

// ListGatewayLoginsForHtpasswd returns the username and bcrypt hash of every
// active login (not revoked, device not retired), ordered by username so the
// rendered file is deterministic (the htpasswd-sync job compares content hashes).
func (s *Store) ListGatewayLoginsForHtpasswd(ctx context.Context) ([]HtpasswdEntry, error) {
	var out []HtpasswdEntry
	err := s.Read(ctx, func(tx *Tx) error {
		var err error
		out, err = collect(tx, `SELECT g.username, g.password_hash FROM gateway_logins g JOIN devices d ON d.id = g.device_id
			WHERE g.revoked_at IS NULL AND d.retired_at IS NULL ORDER BY g.username`, nil,
			func(sc scanner) (HtpasswdEntry, error) {
				var e HtpasswdEntry
				return e, sc.Scan(&e.Username, &e.PasswordHash)
			})
		return err
	})
	return out, err
}

// MarkHtpasswdSynced stamps htpasswd_synced_at = now on the active logins with
// these usernames (called by htpasswd-sync after the file was replaced).
func (s *Store) MarkHtpasswdSynced(ctx context.Context, usernames []string) (int64, error) {
	if len(usernames) == 0 {
		return 0, nil
	}
	args := []any{0, 0}
	for _, u := range usernames {
		args = append(args, u)
	}
	var n int64
	err := s.Write(ctx, func(tx *Tx) error {
		now := tx.nowMS()
		args[0], args[1] = now, now
		res, err := tx.Exec(`UPDATE gateway_logins SET htpasswd_synced_at = ?, updated_at = ?
			WHERE revoked_at IS NULL AND username IN (`+placeholders(len(usernames))+`)`, args...)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	return n, err
}

// AgentPendingGatewaySecret returns the wrapped gateway password awaiting
// delivery to the authenticated device (DH13), or ErrNotFound when none is pending.
func (s *Store) AgentPendingGatewaySecret(ctx context.Context, deviceID string) ([]byte, error) {
	var out []byte
	err := s.Read(ctx, func(tx *Tx) error {
		err := tx.QueryRow(`SELECT pending_secret_wrapped FROM gateway_logins
			WHERE device_id = ? AND revoked_at IS NULL AND pending_secret_wrapped IS NOT NULL`, deviceID).Scan(&out)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	})
	return out, err
}

// AgentAckGatewaySecret records that the device received its gateway password:
// the wrapped copy is erased and delivered_at set. Idempotent; acknowledging
// when nothing is pending is a no-op.
func (s *Store) AgentAckGatewaySecret(ctx context.Context, deviceID string) error {
	return s.Write(ctx, func(tx *Tx) error {
		now := tx.nowMS()
		_, err := tx.Exec(`UPDATE gateway_logins SET pending_secret_wrapped = NULL, delivered_at = ?, updated_at = ?
			WHERE device_id = ? AND revoked_at IS NULL AND pending_secret_wrapped IS NOT NULL`, now, now, deviceID)
		return err
	})
}

// ---------------------------------------------------------------------------
// repo_key_escrow
// ---------------------------------------------------------------------------

// Escrow is the active (or a retired) repository-key escrow row. WrappedKey is
// AES-256-GCM ciphertext under a key derived from the console master key; it is
// useless without that key, and revealing it is gated by fresh MFA in the
// handler layer.
type Escrow struct {
	ID                   string
	DeviceID             string
	KeyVersion           int
	Custody              string // escrowed | customer_held
	WrappedKey           []byte // nil for customer_held
	WrapAlg              string
	MasterKeyID          string
	VerifiedAt           *time.Time
	LastVerifyAttemptAt  *time.Time
	LastVerifyErrorClass string
	RetiredAt            *time.Time
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

const escrowCols = `id, device_id, key_version, custody, wrapped_key, wrap_alg, master_key_id, verified_at, last_verify_attempt_at,
	last_verify_error_class, retired_at, created_at, updated_at`

func scanEscrow(sc scanner) (Escrow, error) {
	var e Escrow
	var key []byte
	var alg, mk, errc sql.NullString
	var ver, att, ret sql.NullInt64
	var c, u int64
	if err := sc.Scan(&e.ID, &e.DeviceID, &e.KeyVersion, &e.Custody, &key, &alg, &mk, &ver, &att, &errc, &ret, &c, &u); err != nil {
		return Escrow{}, err
	}
	e.WrappedKey, e.WrapAlg, e.MasterKeyID, e.LastVerifyErrorClass = key, alg.String, mk.String, errc.String
	e.VerifiedAt, e.LastVerifyAttemptAt, e.RetiredAt = ptrTime(ver), ptrTime(att), ptrTime(ret)
	e.CreatedAt, e.UpdatedAt = timeOf(c), timeOf(u)
	return e, nil
}

// ActiveEscrow returns the device's active escrow row (ErrNotFound when the
// device is outside scope or has none).
func (s *Store) ActiveEscrow(ctx context.Context, scope Scope, deviceID string) (Escrow, error) {
	if err := scope.check(); err != nil {
		return Escrow{}, err
	}
	var out Escrow
	err := s.Read(ctx, func(tx *Tx) error {
		if _, err := scopedDevice(tx, scope, deviceID); err != nil {
			return err
		}
		var err error
		out, err = one(tx, `SELECT `+escrowCols+` FROM repo_key_escrow WHERE device_id = ? AND retired_at IS NULL`,
			[]any{deviceID}, scanEscrow)
		return err
	})
	return out, err
}

// AgentEscrowForDelivery returns the escrow row the enroll response (or its
// replay) carries, and only while the device has not confirmed enrollment
// (DH17): afterwards it is ErrNotFound, so the wrapped key is never handed out
// again through the agent surface.
func (s *Store) AgentEscrowForDelivery(ctx context.Context, deviceID string) (Escrow, error) {
	var out Escrow
	err := s.Read(ctx, func(tx *Tx) error {
		var err error
		out, err = one(tx, `SELECT e.id, e.device_id, e.key_version, e.custody, e.wrapped_key, e.wrap_alg, e.master_key_id,
				e.verified_at, e.last_verify_attempt_at, e.last_verify_error_class, e.retired_at, e.created_at, e.updated_at
			FROM repo_key_escrow e JOIN devices d ON d.id = e.device_id
			WHERE e.device_id = ? AND e.retired_at IS NULL AND d.enroll_confirmed_at IS NULL`, []any{deviceID}, scanEscrow)
		return err
	})
	return out, err
}
