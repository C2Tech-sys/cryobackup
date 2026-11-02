package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"time"
)

// Device is the unit of isolation: one restic repository, one gateway login,
// one key. There is deliberately no status field: health is computed at read
// time by internal/state from facts (ADR D10, DH7); paused_at and retired_at
// here are lifecycle facts that are inputs to that function.
type Device struct {
	ID                   string
	CustomerID           string
	SourceKind           string // windows_endpoint (the only kind implemented in v1)
	Hostname             string
	FriendlyName         string
	OSVersion            string
	Arch                 string // amd64 | arm64 | x86 | ""
	DeviceClass          string // laptop | desktop | unknown
	AgentVersion         string
	ResticVersion        string
	UpdateRing           string // stable | pilot
	TargetVersion        string
	EnrolledAt           time.Time
	EnrolledVia          string // token | lobby | adoption | import
	EnrollmentTokenID    string
	EnrollNonce          string
	EnrollConfirmedAt    *time.Time
	FirstBackupDueBy     *time.Time
	PolicyVersionApplied *int
	PausedAt             *time.Time
	PausedUntil          *time.Time
	PauseReason          string
	PausedBy             string
	RetiredAt            *time.Time
	RetiredReason        string
	ReplacedByDeviceID   string
	RepoPath             string // <prefix>/<device_id>/ relative to the storage target
	HWFingerprint        string
	IdentityConflictAt   *time.Time
	Notes                string
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

const deviceCols = `id, customer_id, source_kind, hostname, friendly_name, os_version, arch, device_class, agent_version,
	restic_version, update_ring, target_version, enrolled_at, enrolled_via, enrollment_token_id, enroll_nonce,
	enroll_confirmed_at, first_backup_due_by, policy_version_applied, paused_at, paused_until, pause_reason, paused_by,
	retired_at, retired_reason, replaced_by_device_id, repo_path, hw_fingerprint, identity_conflict_at, notes,
	created_at, updated_at`

func scanDevice(sc scanner) (Device, error) {
	var d Device
	var friendly, osv, arch, av, rv, tv, tok, nonce, reason, by, rreason, repl, hw, notes sql.NullString
	var confirmed, due, pva, paused, until, retired, ident sql.NullInt64
	var enrolled, c, u int64
	err := sc.Scan(&d.ID, &d.CustomerID, &d.SourceKind, &d.Hostname, &friendly, &osv, &arch, &d.DeviceClass, &av, &rv,
		&d.UpdateRing, &tv, &enrolled, &d.EnrolledVia, &tok, &nonce, &confirmed, &due, &pva, &paused, &until, &reason, &by,
		&retired, &rreason, &repl, &d.RepoPath, &hw, &ident, &notes, &c, &u)
	if err != nil {
		return Device{}, err
	}
	d.FriendlyName, d.OSVersion, d.Arch, d.AgentVersion = friendly.String, osv.String, arch.String, av.String
	d.ResticVersion, d.TargetVersion, d.EnrollmentTokenID, d.EnrollNonce = rv.String, tv.String, tok.String, nonce.String
	d.PauseReason, d.PausedBy, d.RetiredReason, d.ReplacedByDeviceID = reason.String, by.String, rreason.String, repl.String
	d.HWFingerprint, d.Notes = hw.String, notes.String
	d.EnrolledAt = timeOf(enrolled)
	d.EnrollConfirmedAt, d.FirstBackupDueBy, d.PolicyVersionApplied = ptrTime(confirmed), ptrTime(due), ptrInt(pva)
	d.PausedAt, d.PausedUntil, d.RetiredAt, d.IdentityConflictAt = ptrTime(paused), ptrTime(until), ptrTime(retired), ptrTime(ident)
	d.CreatedAt, d.UpdatedAt = timeOf(c), timeOf(u)
	return d, nil
}

// GetDevice returns a device inside scope (ErrNotFound otherwise).
func (s *Store) GetDevice(ctx context.Context, scope Scope, id string) (Device, error) {
	if err := scope.check(); err != nil {
		return Device{}, err
	}
	var out Device
	err := s.Read(ctx, func(tx *Tx) error {
		var err error
		out, err = getDevice(tx, scope, id)
		return err
	})
	return out, err
}

func getDevice(tx *Tx, scope Scope, id string) (Device, error) {
	clause, args := scope.clause("customer_id")
	return one(tx, `SELECT `+deviceCols+` FROM devices WHERE id = ? AND `+clause, append([]any{id}, args...), scanDevice)
}

// ListDevicesOptions filters and orders ListDevices. Only lifecycle and
// identity columns can be filtered: computed health state is not stored, so a
// state filter is applied by the caller over state.Evaluate's result.
type ListDevicesOptions struct {
	CustomerID     string // "" = every customer in scope
	Search         string // case-insensitive substring of hostname or friendly name
	DeviceClass    string
	UpdateRing     string
	IncludeRetired bool
	Sort           string // hostname (default) | enrolled | customer
	Page           Page
}

// ListDevices returns the devices inside scope that match o, one page at a time.
func (s *Store) ListDevices(ctx context.Context, scope Scope, o ListDevicesOptions) (Listing[Device], error) {
	if err := scope.check(); err != nil {
		return Listing[Device]{}, err
	}
	limit, offset, err := o.Page.window()
	if err != nil {
		return Listing[Device]{}, err
	}
	clause, args := scope.clause("customer_id")
	where := []string{clause}
	if o.CustomerID != "" {
		where, args = append(where, "customer_id = ?"), append(args, o.CustomerID)
	}
	if o.Search != "" {
		pat := "%" + escapeLike(o.Search) + "%"
		where = append(where, `(hostname LIKE ? ESCAPE '\' OR friendly_name LIKE ? ESCAPE '\')`)
		args = append(args, pat, pat)
	}
	if o.DeviceClass != "" {
		where, args = append(where, "device_class = ?"), append(args, o.DeviceClass)
	}
	if o.UpdateRing != "" {
		where, args = append(where, "update_ring = ?"), append(args, o.UpdateRing)
	}
	if !o.IncludeRetired {
		where = append(where, "retired_at IS NULL")
	}
	var order string
	switch o.Sort {
	case "", "hostname":
		order = "hostname COLLATE NOCASE, id"
	case "enrolled":
		order = "enrolled_at DESC, id"
	case "customer":
		order = "customer_id, hostname COLLATE NOCASE, id"
	default:
		return Listing[Device]{}, invalidf("unknown sort %q", o.Sort)
	}
	var items []Device
	err = s.Read(ctx, func(tx *Tx) error {
		var err error
		items, err = collect(tx, `SELECT `+deviceCols+` FROM devices WHERE `+strings.Join(where, " AND ")+
			` ORDER BY `+order+` LIMIT ? OFFSET ?`, append(args, limit+1, offset), scanDevice)
		return err
	})
	if err != nil {
		return Listing[Device]{}, err
	}
	return finish(items, limit, offset), nil
}

func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// PauseSpec parks a device. Reason is required (the schema rejects an empty
// one). Until nil means an indefinite pause, which the digest lists with its age.
type PauseSpec struct {
	Reason string
	Until  *time.Time
}

// RetireSpec retires a device for good (RETIRED is terminal).
type RetireSpec struct {
	Reason             string
	ReplacedByDeviceID string
}

// DeviceUpdate is a set of optional changes to a device; nil/false fields are left alone.
type DeviceUpdate struct {
	FriendlyName          *string
	Notes                 *string
	DeviceClass           *string
	UpdateRing            *string
	TargetVersion         *string // "" clears the pin
	Pause                 *PauseSpec
	Unpause               bool
	Retire                *RetireSpec
	ClearIdentityConflict bool
	By                    string // admin id recorded as paused_by
}

// UpdateDevice applies u to a device inside scope. The schema's lifecycle rules
// surface as ErrInvalid with the constraint named: pausing without a reason, a
// pause that ends before it starts, and retiring a device that is still paused
// (unpause first). Retiring also revokes the device's tokens and gateway login
// in the same transaction, so a retired machine can no longer authenticate or
// reach the gateway. A Retire.ReplacedByDeviceID must name a device inside scope
// (ErrNotFound otherwise) of the same customer and not itself retired
// (ErrInvalid otherwise).
func (s *Store) UpdateDevice(ctx context.Context, scope Scope, id string, u DeviceUpdate) (Device, error) {
	if err := scope.check(); err != nil {
		return Device{}, err
	}
	if u.Pause != nil && u.Unpause {
		return Device{}, invalidf("pause and unpause are exclusive")
	}
	var out Device
	err := s.Write(ctx, func(tx *Tx) error {
		cur, err := getDevice(tx, scope, id)
		if err != nil {
			return err
		}
		now := tx.nowMS()
		sets, args := []string{"updated_at = ?"}, []any{now}
		add := func(col string, v any) { sets, args = append(sets, col+" = ?"), append(args, v) }
		if u.FriendlyName != nil {
			add("friendly_name", nullStr(*u.FriendlyName))
		}
		if u.Notes != nil {
			add("notes", nullStr(*u.Notes))
		}
		if u.DeviceClass != nil {
			add("device_class", *u.DeviceClass)
		}
		if u.UpdateRing != nil {
			add("update_ring", *u.UpdateRing)
		}
		if u.TargetVersion != nil {
			add("target_version", nullStr(*u.TargetVersion))
		}
		if u.Pause != nil {
			add("paused_at", now)
			add("paused_until", nullTime(u.Pause.Until))
			add("pause_reason", nullStr(u.Pause.Reason))
			add("paused_by", nullStr(u.By))
		}
		if u.Unpause {
			add("paused_at", nil)
			add("paused_until", nil)
			add("pause_reason", nil)
			add("paused_by", nil)
		}
		if u.Retire != nil {
			if u.Retire.ReplacedByDeviceID != "" {
				// §3.5: the replacement must be inside the caller's scope (ErrNotFound)
				// and belong to the same customer as the retired device (ErrInvalid).
				owner, err := scopedDevice(tx, scope, u.Retire.ReplacedByDeviceID)
				if err != nil {
					return err
				}
				if owner != cur.CustomerID {
					return invalidf("replacement device belongs to a different customer")
				}
				var replRetired sql.NullInt64
				if err := tx.QueryRow(`SELECT retired_at FROM devices WHERE id = ?`, u.Retire.ReplacedByDeviceID).Scan(&replRetired); err != nil {
					return err
				}
				if replRetired.Valid {
					return invalidf("replacement device is itself retired")
				}
			}
			add("retired_at", now)
			add("retired_reason", nullStr(u.Retire.Reason))
			add("replaced_by_device_id", nullStr(u.Retire.ReplacedByDeviceID))
		}
		if u.ClearIdentityConflict {
			add("identity_conflict_at", nil)
			add("hw_fingerprint", nil) // as ResolveKeep: the next heartbeat records the live machine's fingerprint
		}
		if _, err := tx.Exec(`UPDATE devices SET `+strings.Join(sets, ", ")+` WHERE id = ?`, append(args, cur.ID)...); err != nil {
			return err
		}
		if u.Retire != nil && cur.RetiredAt == nil {
			if _, err := tx.Exec(`UPDATE device_tokens SET revoked_at = ?, revoked_reason = 'device retired', updated_at = ?
				WHERE device_id = ? AND revoked_at IS NULL`, now, now, cur.ID); err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE gateway_logins SET revoked_at = ?, pending_secret_wrapped = NULL, updated_at = ?
				WHERE device_id = ? AND revoked_at IS NULL`, now, now, cur.ID); err != nil {
				return err
			}
		}
		out, err = getDevice(tx, scope, id)
		return err
	})
	return out, err
}

// AgentConfirmEnrollment stamps enroll_confirmed_at on the first authenticated
// call after enrollment (idempotent). After it, the escrowed key and pending
// gateway secret are no longer delivered (DH17).
func (s *Store) AgentConfirmEnrollment(ctx context.Context, deviceID string) error {
	return s.Write(ctx, func(tx *Tx) error {
		now := tx.nowMS()
		res, err := tx.Exec(`UPDATE devices SET enroll_confirmed_at = COALESCE(enroll_confirmed_at, ?),
			updated_at = CASE WHEN enroll_confirmed_at IS NULL THEN ? ELSE updated_at END WHERE id = ?`, now, now, deviceID)
		if err != nil {
			return err
		}
		return requireRow(res)
	})
}

// ---------------------------------------------------------------------------
// device_presence and heartbeats (DH2)
// ---------------------------------------------------------------------------

// Presence is one heartbeat: the latest-row view (device_presence, the recon's
// last_seen_at) and, identically, a row of the 24-hour ring (heartbeats).
type Presence struct {
	DeviceID string
	// ReceivedAt is SERVER time when the heartbeat arrived; zero means the store clock.
	ReceivedAt           time.Time
	AgentVersion         string
	ResticVersion        string // also copied to devices.restic_version
	UptimeS              *int64
	OnAC                 *bool
	BatteryPct           *int
	Metered              *bool
	VPN                  *bool
	QueueDepth           *int
	JournalDepth         *int
	ClockOffsetMS        *int64 // device clock minus server clock
	ServiceState         string // idle | running | paused | deferred
	LastErrorClass       string
	PolicyVersionApplied *int // copied to devices.policy_version_applied
	// Progress is live run progress, counts only, never paths. The store adds an
	// "advanced_at" key (server time) that moves only when the counts change,
	// which is what the engine reads as ProgressAdvancedAt.
	Progress  json.RawMessage
	SourceIP  string
	UpdatedAt time.Time
}

const presenceCols = `device_id, received_at, agent_version, uptime_s, on_ac, battery_pct, metered, vpn, queue_depth,
	journal_depth, clock_offset_ms, service_state, last_error_class, progress, source_ip, updated_at`

func scanPresence(sc scanner) (Presence, error) {
	var p Presence
	var av, lec, progress, ip sql.NullString
	var up, ac, bat, met, vpn, qd, jd, off sql.NullInt64
	var recv, u int64
	err := sc.Scan(&p.DeviceID, &recv, &av, &up, &ac, &bat, &met, &vpn, &qd, &jd, &off, &p.ServiceState, &lec, &progress, &ip, &u)
	if err != nil {
		return Presence{}, err
	}
	p.ReceivedAt, p.AgentVersion, p.LastErrorClass, p.SourceIP = timeOf(recv), av.String, lec.String, ip.String
	p.UptimeS, p.OnAC, p.BatteryPct, p.Metered, p.VPN = ptrInt64(up), ptrBool(ac), ptrInt(bat), ptrBool(met), ptrBool(vpn)
	p.QueueDepth, p.JournalDepth, p.ClockOffsetMS = ptrInt(qd), ptrInt(jd), ptrInt64(off)
	p.Progress, p.UpdatedAt = rawOf(progress), timeOf(u)
	return p, nil
}

// advanceProgress returns the progress document to store: next, carrying
// "advanced_at" forward when nothing but advanced_at differs from prev, and
// stamping now otherwise (including the first progress seen).
func advanceProgress(prev sql.NullString, next json.RawMessage, now int64) (any, error) {
	if len(next) == 0 {
		return nil, nil
	}
	var cur map[string]any
	if err := json.Unmarshal(next, &cur); err != nil || cur == nil {
		return nil, invalidf("progress must be a JSON object")
	}
	delete(cur, "advanced_at")
	advanced := now
	if prev.Valid {
		var old map[string]any
		if json.Unmarshal([]byte(prev.String), &old) == nil && old != nil {
			if at, ok := old["advanced_at"].(float64); ok {
				delete(old, "advanced_at")
				if reflect.DeepEqual(old, cur) {
					advanced = int64(at)
				}
			}
		}
	}
	cur["advanced_at"] = advanced
	b, err := json.Marshal(cur)
	return string(b), err
}

// AgentUpsertPresence records a heartbeat for the authenticated device in one
// transaction: it upserts device_presence, appends to the heartbeat ring, and
// copies the facts that live on the device row (agent and restic versions,
// policy_version_applied) while stamping enroll_confirmed_at on the first call
// after enrollment (architecture §5.1). ErrNotFound when the device does not
// exist.
func (s *Store) AgentUpsertPresence(ctx context.Context, deviceID string, p Presence) error {
	if p.ServiceState == "" {
		return invalidf("service_state is required")
	}
	return s.Write(ctx, func(tx *Tx) error {
		now := tx.nowMS()
		recv := now
		if !p.ReceivedAt.IsZero() {
			recv = msOf(p.ReceivedAt)
		}
		var exists int
		if err := tx.QueryRow(`SELECT 1 FROM devices WHERE id = ?`, deviceID).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		var prev sql.NullString
		if err := tx.QueryRow(`SELECT progress FROM device_presence WHERE device_id = ?`, deviceID).Scan(&prev); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		progress, err := advanceProgress(prev, p.Progress, now)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO device_presence (`+presenceCols[:strings.Index(presenceCols, ", updated_at")]+`, created_at, updated_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(device_id) DO UPDATE SET received_at = excluded.received_at, agent_version = excluded.agent_version,
				uptime_s = excluded.uptime_s, on_ac = excluded.on_ac, battery_pct = excluded.battery_pct,
				metered = excluded.metered, vpn = excluded.vpn, queue_depth = excluded.queue_depth,
				journal_depth = excluded.journal_depth, clock_offset_ms = excluded.clock_offset_ms,
				service_state = excluded.service_state, last_error_class = excluded.last_error_class,
				progress = excluded.progress, source_ip = excluded.source_ip, updated_at = excluded.updated_at`,
			deviceID, recv, nullStr(p.AgentVersion), nullInt64(p.UptimeS), nullBool(p.OnAC), nullInt(p.BatteryPct),
			nullBool(p.Metered), nullBool(p.VPN), nullInt(p.QueueDepth), nullInt(p.JournalDepth), nullInt64(p.ClockOffsetMS),
			p.ServiceState, nullStr(p.LastErrorClass), progress, nullStr(p.SourceIP), now, now); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO heartbeats (id, device_id, received_at, agent_version, uptime_s, on_ac, battery_pct,
				metered, vpn, queue_depth, journal_depth, clock_offset_ms, service_state, last_error_class, created_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			tx.id(), deviceID, recv, nullStr(p.AgentVersion), nullInt64(p.UptimeS), nullBool(p.OnAC), nullInt(p.BatteryPct),
			nullBool(p.Metered), nullBool(p.VPN), nullInt(p.QueueDepth), nullInt(p.JournalDepth), nullInt64(p.ClockOffsetMS),
			p.ServiceState, nullStr(p.LastErrorClass), now); err != nil {
			return err
		}
		_, err = tx.Exec(`UPDATE devices SET
				enroll_confirmed_at = COALESCE(enroll_confirmed_at, ?),
				agent_version = COALESCE(?, agent_version),
				restic_version = COALESCE(?, restic_version),
				policy_version_applied = COALESCE(?, policy_version_applied),
				updated_at = ?
			WHERE id = ? AND (enroll_confirmed_at IS NULL
				OR agent_version IS NOT COALESCE(?, agent_version)
				OR restic_version IS NOT COALESCE(?, restic_version)
				OR policy_version_applied IS NOT COALESCE(?, policy_version_applied))`,
			now, nullStr(p.AgentVersion), nullStr(p.ResticVersion), nullInt(p.PolicyVersionApplied), now, deviceID,
			nullStr(p.AgentVersion), nullStr(p.ResticVersion), nullInt(p.PolicyVersionApplied))
		return err // the device row is only written when a fact on it changed: a quiet heartbeat touches presence and the ring only
	})
}

// GetPresence returns a device's latest heartbeat (ErrNotFound when the device
// is outside scope or has never reported).
func (s *Store) GetPresence(ctx context.Context, scope Scope, deviceID string) (Presence, error) {
	if err := scope.check(); err != nil {
		return Presence{}, err
	}
	var out Presence
	err := s.Read(ctx, func(tx *Tx) error {
		if _, err := scopedDevice(tx, scope, deviceID); err != nil {
			return err
		}
		var err error
		out, err = one(tx, `SELECT `+presenceCols+` FROM device_presence WHERE device_id = ?`, []any{deviceID}, scanPresence)
		return err
	})
	return out, err
}

// TrimHeartbeats deletes ring rows received before olderThan, batch rows per
// transaction (0 means 5000), and returns the total deleted (the
// heartbeat-ring-trim job). Safe to interrupt and to run twice.
func (s *Store) TrimHeartbeats(ctx context.Context, olderThan time.Time, batch int) (int64, error) {
	if batch <= 0 {
		batch = 5000
	}
	var total int64
	for {
		var n int64
		err := s.Write(ctx, func(tx *Tx) error {
			res, err := tx.Exec(`DELETE FROM heartbeats WHERE id IN
				(SELECT id FROM heartbeats WHERE received_at < ? LIMIT ?)`, msOf(olderThan), batch)
			if err != nil {
				return err
			}
			n, err = res.RowsAffected()
			return err
		})
		if err != nil {
			return total, err
		}
		total += n
		if n < int64(batch) {
			return total, nil
		}
	}
}

// ---------------------------------------------------------------------------
// device_tokens
// ---------------------------------------------------------------------------

// DeviceToken is a hashed bearer token for the agent API. One token per device
// is current; a superseded token stays valid until GraceUntil so a lost
// rotation response cannot lock an agent out.
type DeviceToken struct {
	ID            string
	DeviceID      string
	TokenHash     string
	SupersededAt  *time.Time
	GraceUntil    *time.Time
	ExpiresAt     *time.Time
	LastUsedAt    *time.Time
	LastUsedIP    string
	RevokedAt     *time.Time
	RevokedReason string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

const tokenCols = `id, device_id, token_hash, superseded_at, grace_until, expires_at, last_used_at, last_used_ip, revoked_at,
	revoked_reason, created_at, updated_at`

func scanDeviceToken(sc scanner) (DeviceToken, error) {
	var t DeviceToken
	var sup, grace, exp, used, rev sql.NullInt64
	var ip, reason sql.NullString
	var c, u int64
	if err := sc.Scan(&t.ID, &t.DeviceID, &t.TokenHash, &sup, &grace, &exp, &used, &ip, &rev, &reason, &c, &u); err != nil {
		return DeviceToken{}, err
	}
	t.SupersededAt, t.GraceUntil, t.ExpiresAt, t.LastUsedAt = ptrTime(sup), ptrTime(grace), ptrTime(exp), ptrTime(used)
	t.LastUsedIP, t.RevokedAt, t.RevokedReason = ip.String, ptrTime(rev), reason.String
	t.CreatedAt, t.UpdatedAt = timeOf(c), timeOf(u)
	return t, nil
}

// AgentLookupDeviceToken resolves a bearer token hash to its device. The token
// must not be revoked or expired, and a superseded token only counts until its
// grace window ends; the device must not be retired. Anything else is
// ErrNotFound (the API answers 401 without saying which). It writes nothing:
// use AgentTouchDeviceToken for last_used.
func (s *Store) AgentLookupDeviceToken(ctx context.Context, tokenHash string) (string, DeviceToken, error) {
	var tok DeviceToken
	err := s.Read(ctx, func(tx *Tx) error {
		now := tx.nowMS()
		var err error
		tok, err = one(tx, `SELECT t.id, t.device_id, t.token_hash, t.superseded_at, t.grace_until, t.expires_at, t.last_used_at,
				t.last_used_ip, t.revoked_at, t.revoked_reason, t.created_at, t.updated_at
			FROM device_tokens t JOIN devices d ON d.id = t.device_id
			WHERE t.token_hash = ? AND t.revoked_at IS NULL AND d.retired_at IS NULL
			  AND (t.expires_at IS NULL OR t.expires_at > ?)
			  AND (t.superseded_at IS NULL OR t.grace_until > ?)`, []any{tokenHash, now, now}, scanDeviceToken)
		return err
	})
	return tok.DeviceID, tok, err
}

// AgentTouchDeviceToken records last_used_at and the source IP. Callers throttle
// it (once a few minutes per token) because it is a write on the agent hot path.
func (s *Store) AgentTouchDeviceToken(ctx context.Context, tokenID, ip string) error {
	return s.Write(ctx, func(tx *Tx) error {
		now := tx.nowMS()
		_, err := tx.Exec(`UPDATE device_tokens SET last_used_at = ?, last_used_ip = ?, updated_at = ? WHERE id = ?`,
			now, nullStr(ip), now, tokenID)
		return err
	})
}

// AgentRotateDeviceToken replaces the device's current token with newHash. The
// old token is superseded, not revoked: it keeps working until now+grace so a
// rotation response lost in transit cannot lock the agent out.
//
// currentTokenID is the id of the token that authenticated the call (from
// AgentLookupDeviceToken). The rotation is refused with ErrConflict unless that
// token is the device's current one (not superseded, not revoked, not expired): a token that
// is merely inside its grace window cannot mint a successor. A successful
// rotation revokes the device's earlier superseded tokens, so at most one
// grace-window token exists at a time.
func (s *Store) AgentRotateDeviceToken(ctx context.Context, deviceID, currentTokenID, newHash string, grace time.Duration) (DeviceToken, error) {
	if grace <= 0 {
		return DeviceToken{}, invalidf("grace must be positive")
	}
	var out DeviceToken
	err := s.Write(ctx, func(tx *Tx) error {
		var exists int
		if err := tx.QueryRow(`SELECT 1 FROM devices WHERE id = ? AND retired_at IS NULL`, deviceID).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		now := tx.nowMS()
		var current int
		// Current means not superseded, not revoked and not past its expiry: the
		// hash lookup refuses an expired token, and so must the rotation.
		if err := tx.QueryRow(`SELECT 1 FROM device_tokens WHERE id = ? AND device_id = ? AND superseded_at IS NULL AND revoked_at IS NULL
			AND (expires_at IS NULL OR expires_at > ?)`,
			currentTokenID, deviceID, now).Scan(&current); errors.Is(err, sql.ErrNoRows) {
			return conflictf("token is not the device's current token")
		} else if err != nil {
			return err
		}
		// Only one grace-window token at a time: retire the earlier superseded ones first.
		if _, err := tx.Exec(`UPDATE device_tokens SET revoked_at = ?, revoked_reason = 'superseded by a later rotation', updated_at = ?
			WHERE device_id = ? AND revoked_at IS NULL AND superseded_at IS NOT NULL`, now, now, deviceID); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE device_tokens SET superseded_at = ?, grace_until = ?, updated_at = ?
			WHERE id = ?`, now, now+grace.Milliseconds(), now, currentTokenID); err != nil {
			return err
		}
		id := tx.id()
		if _, err := tx.Exec(`INSERT INTO device_tokens (id, device_id, token_hash, created_at, updated_at) VALUES (?,?,?,?,?)`,
			id, deviceID, newHash, now, now); err != nil {
			return err
		}
		var err error
		out, err = one(tx, `SELECT `+tokenCols+` FROM device_tokens WHERE id = ?`, []any{id}, scanDeviceToken)
		return err
	})
	return out, err
}
