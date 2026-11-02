package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// ---------------------------------------------------------------------------
// settings
// ---------------------------------------------------------------------------

// Setting is one global key/value row. Secrets never live here: a value that
// needs a secret holds the NAME of an environment variable or mounted file.
type Setting struct {
	Key       string
	Value     json.RawMessage
	UpdatedBy string // admin id; "" when system or deleted admin
	CreatedAt time.Time
	UpdatedAt time.Time
}

const settingCols = `key, value, updated_by, created_at, updated_at`

func scanSetting(sc scanner) (Setting, error) {
	var s Setting
	var by sql.NullString
	var c, u int64
	var v string
	if err := sc.Scan(&s.Key, &v, &by, &c, &u); err != nil {
		return Setting{}, err
	}
	s.Value, s.UpdatedBy = json.RawMessage(v), by.String
	s.CreatedAt, s.UpdatedAt = timeOf(c), timeOf(u)
	return s, nil
}

// GetSetting returns one setting or ErrNotFound.
func (s *Store) GetSetting(ctx context.Context, key string) (Setting, error) {
	var out Setting
	err := s.Read(ctx, func(tx *Tx) error {
		var err error
		out, err = one(tx, `SELECT `+settingCols+` FROM settings WHERE key = ?`, []any{key}, scanSetting)
		return err
	})
	return out, err
}

// ListSettings returns every setting ordered by key.
func (s *Store) ListSettings(ctx context.Context) ([]Setting, error) {
	var out []Setting
	err := s.Read(ctx, func(tx *Tx) error {
		var err error
		out, err = collect(tx, `SELECT `+settingCols+` FROM settings ORDER BY key`, nil, scanSetting)
		return err
	})
	return out, err
}

// PutSetting creates or replaces a setting. value must be valid JSON; updatedBy
// is the admin id ("" for the system) and must exist when given.
func (s *Store) PutSetting(ctx context.Context, key string, value json.RawMessage, updatedBy string) error {
	if len(value) == 0 {
		return invalidf("setting value is required")
	}
	if !json.Valid(value) {
		return invalidf("setting value is not valid JSON")
	}
	return s.Write(ctx, func(tx *Tx) error {
		now := tx.nowMS()
		_, err := tx.Exec(`INSERT INTO settings (key, value, updated_by, created_at, updated_at) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_by = excluded.updated_by, updated_at = excluded.updated_at`,
			key, string(value), nullStr(updatedBy), now, now)
		return err
	})
}

// ---------------------------------------------------------------------------
// storage_targets
// ---------------------------------------------------------------------------

// StorageTarget is an S3-compatible bucket the gateway fronts. Storage targets
// are global configuration, not customer-owned data, so these methods take no
// Scope. CredentialRef is the NAME of the environment variable or mounted
// secret on the admin-path host; the credential itself never touches the database.
type StorageTarget struct {
	ID                      string
	Name                    string
	Provider                string // wasabi | backblaze_b2 | idrive_e2 | aws_s3 | s3_compatible
	Endpoint                string
	Region                  string
	Bucket                  string
	Prefix                  string
	GatewayURL              string
	Versioning              bool
	ObjectLockMode          string // none | governance ("" = none)
	ObjectLockDays          *int
	MinStorageDays          int
	DefaultPruneCadenceDays int // 0 = 30
	CredentialRef           string
	SoftQuotaBytes          *int64
	IsDefault               bool
	ArchivedAt              *time.Time
	// MaxConcurrentBackups is the fleet cap (ADR 0005): how many start leases
	// may be live for this target at once. 0 = unlimited, the default.
	MaxConcurrentBackups int
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

const targetCols = `id, name, provider, endpoint, region, bucket, prefix, gateway_url, versioning, object_lock_mode,
	object_lock_days, min_storage_days, default_prune_cadence_days, credential_ref, soft_quota_bytes, is_default,
	archived_at, max_concurrent_backups, created_at, updated_at`

func scanTarget(sc scanner) (StorageTarget, error) {
	var t StorageTarget
	var region sql.NullString
	var versioning, def int
	var lockDays, quota, archived sql.NullInt64
	var c, u int64
	err := sc.Scan(&t.ID, &t.Name, &t.Provider, &t.Endpoint, &region, &t.Bucket, &t.Prefix, &t.GatewayURL, &versioning,
		&t.ObjectLockMode, &lockDays, &t.MinStorageDays, &t.DefaultPruneCadenceDays, &t.CredentialRef, &quota, &def,
		&archived, &t.MaxConcurrentBackups, &c, &u)
	if err != nil {
		return StorageTarget{}, err
	}
	t.Region, t.Versioning, t.IsDefault = region.String, versioning != 0, def != 0
	t.ObjectLockDays, t.SoftQuotaBytes, t.ArchivedAt = ptrInt(lockDays), ptrInt64(quota), ptrTime(archived)
	t.CreatedAt, t.UpdatedAt = timeOf(c), timeOf(u)
	return t, nil
}

// CreateStorageTarget inserts a target; ID and timestamps are assigned. At most
// one target may be the default (ErrConflict naming uq_storage_targets_default).
// A negative MaxConcurrentBackups is ErrInvalid.
func (s *Store) CreateStorageTarget(ctx context.Context, t StorageTarget) (StorageTarget, error) {
	if t.MaxConcurrentBackups < 0 {
		return StorageTarget{}, invalidf("max_concurrent_backups must not be negative")
	}
	if t.ObjectLockMode == "" {
		t.ObjectLockMode = "none"
	}
	if t.DefaultPruneCadenceDays == 0 {
		t.DefaultPruneCadenceDays = 30
	}
	var out StorageTarget
	err := s.Write(ctx, func(tx *Tx) error {
		now := tx.nowMS()
		t.ID = tx.id()
		_, err := tx.Exec(`INSERT INTO storage_targets (`+targetCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			t.ID, t.Name, t.Provider, t.Endpoint, nullStr(t.Region), t.Bucket, t.Prefix, t.GatewayURL, boolInt(t.Versioning),
			t.ObjectLockMode, nullInt(t.ObjectLockDays), t.MinStorageDays, t.DefaultPruneCadenceDays, t.CredentialRef,
			nullInt64(t.SoftQuotaBytes), boolInt(t.IsDefault), nullTime(t.ArchivedAt), t.MaxConcurrentBackups, now, now)
		if err != nil {
			return err
		}
		out, err = one(tx, `SELECT `+targetCols+` FROM storage_targets WHERE id = ?`, []any{t.ID}, scanTarget)
		return err
	})
	return out, err
}

// DefaultStorageTarget returns the default target or ErrNotFound.
func (s *Store) DefaultStorageTarget(ctx context.Context) (StorageTarget, error) {
	var out StorageTarget
	err := s.Read(ctx, func(tx *Tx) error {
		var err error
		out, err = one(tx, `SELECT `+targetCols+` FROM storage_targets WHERE is_default = 1`, nil, scanTarget)
		return err
	})
	return out, err
}

// GetStorageTarget returns one target or ErrNotFound.
func (s *Store) GetStorageTarget(ctx context.Context, id string) (StorageTarget, error) {
	var out StorageTarget
	err := s.Read(ctx, func(tx *Tx) error {
		var err error
		out, err = one(tx, `SELECT `+targetCols+` FROM storage_targets WHERE id = ?`, []any{id}, scanTarget)
		return err
	})
	return out, err
}

// SetStorageTargetMaxConcurrentBackups sets the fleet cap of a target (ADR
// 0005): n live start leases at most, 0 = unlimited. It returns the updated
// target, ErrNotFound for an unknown id and ErrInvalid for a negative n. Leases
// already granted are not revoked by a lower cap; they run out (released by
// the run upload or expired) and new ones are counted against the new value.
func (s *Store) SetStorageTargetMaxConcurrentBackups(ctx context.Context, id string, n int) (StorageTarget, error) {
	var out StorageTarget
	err := s.Write(ctx, func(tx *Tx) error {
		var err error
		_, out, err = tx.SetStorageTargetMaxConcurrentBackups(id, n)
		return err
	})
	return out, err
}

// SetStorageTargetMaxConcurrentBackups is the in-transaction form: it also
// returns the cap before the change, so a caller can audit the change in the
// same transaction (cryo-console target set).
func (tx *Tx) SetStorageTargetMaxConcurrentBackups(id string, n int) (before int, after StorageTarget, err error) {
	if n < 0 {
		return 0, StorageTarget{}, invalidf("max_concurrent_backups must not be negative")
	}
	prev, err := one(tx, `SELECT `+targetCols+` FROM storage_targets WHERE id = ?`, []any{id}, scanTarget)
	if err != nil {
		return 0, StorageTarget{}, err
	}
	if _, err := tx.Exec(`UPDATE storage_targets SET max_concurrent_backups = ?, updated_at = ? WHERE id = ?`, n, tx.nowMS(), id); err != nil {
		return 0, StorageTarget{}, err
	}
	after, err = one(tx, `SELECT `+targetCols+` FROM storage_targets WHERE id = ?`, []any{id}, scanTarget)
	return prev.MaxConcurrentBackups, after, err
}

// ListStorageTargets returns every target ordered by name.
func (s *Store) ListStorageTargets(ctx context.Context) ([]StorageTarget, error) {
	var out []StorageTarget
	err := s.Read(ctx, func(tx *Tx) error {
		var err error
		out, err = collect(tx, `SELECT `+targetCols+` FROM storage_targets ORDER BY name COLLATE NOCASE`, nil, scanTarget)
		return err
	})
	return out, err
}

// ---------------------------------------------------------------------------
// job_state (leases and system health, architecture §5.7)
// ---------------------------------------------------------------------------

// JobState is one job's last run and lease. A lease keeps two processes from
// running the same job at once (architecture §9: two consoles on one file).
type JobState struct {
	Name           string
	LastStartedAt  *time.Time
	LastFinishedAt *time.Time
	LastStatus     string // running | ok | error | skipped
	LastErrorClass string
	LastDurationMS *int64
	RunCount       int
	LeaseHolder    string
	LeaseUntil     *time.Time
	NextDueAt      *time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// JobRun is what RecordJobRun stores about one execution.
type JobRun struct {
	Status     string // running | ok | error | skipped
	ErrorClass string // path-free class when Status is error
	StartedAt  time.Time
	FinishedAt time.Time
	NextDueAt  *time.Time
}

const jobCols = `name, last_started_at, last_finished_at, last_status, last_error_class, last_duration_ms, run_count,
	lease_holder, lease_until, next_due_at, created_at, updated_at`

func scanJob(sc scanner) (JobState, error) {
	var j JobState
	var st, fin, dur, until, next sql.NullInt64
	var status, errc, holder sql.NullString
	var c, u int64
	if err := sc.Scan(&j.Name, &st, &fin, &status, &errc, &dur, &j.RunCount, &holder, &until, &next, &c, &u); err != nil {
		return JobState{}, err
	}
	j.LastStartedAt, j.LastFinishedAt, j.LastDurationMS = ptrTime(st), ptrTime(fin), ptrInt64(dur)
	j.LastStatus, j.LastErrorClass, j.LeaseHolder = status.String, errc.String, holder.String
	j.LeaseUntil, j.NextDueAt = ptrTime(until), ptrTime(next)
	j.CreatedAt, j.UpdatedAt = timeOf(c), timeOf(u)
	return j, nil
}

// AcquireJobLease takes the lease on a job for ttl and reports whether it did.
// It succeeds when nobody holds the lease, the previous lease expired, or the
// caller already holds it (a renewal). It never blocks.
func (s *Store) AcquireJobLease(ctx context.Context, name, holder string, ttl time.Duration) (bool, error) {
	if name == "" || holder == "" || ttl <= 0 {
		return false, invalidf("job name, holder and a positive ttl are required")
	}
	var got bool
	err := s.Write(ctx, func(tx *Tx) error {
		now := tx.nowMS()
		res, err := tx.Exec(`INSERT INTO job_state (name, lease_holder, lease_until, created_at, updated_at) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(name) DO UPDATE SET lease_holder = excluded.lease_holder, lease_until = excluded.lease_until,
				updated_at = excluded.updated_at
			WHERE job_state.lease_holder IS NULL OR job_state.lease_until <= ? OR job_state.lease_holder = excluded.lease_holder`,
			name, holder, now+ttl.Milliseconds(), now, now, now)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		got = n == 1
		return err
	})
	return got, err
}

// ReleaseJobLease releases the lease if holder still holds it; releasing a lease
// that expired or was taken over is a no-op, not an error.
func (s *Store) ReleaseJobLease(ctx context.Context, name, holder string) error {
	return s.Write(ctx, func(tx *Tx) error {
		_, err := tx.Exec(`UPDATE job_state SET lease_holder = NULL, lease_until = NULL, updated_at = ?
			WHERE name = ? AND lease_holder = ?`, tx.nowMS(), name, holder)
		return err
	})
}

// RecordJobRun stores the outcome of one execution and increments run_count.
func (s *Store) RecordJobRun(ctx context.Context, name string, r JobRun) error {
	dur := r.FinishedAt.Sub(r.StartedAt).Milliseconds()
	if dur < 0 {
		return invalidf("finished before started")
	}
	return s.Write(ctx, func(tx *Tx) error {
		now := tx.nowMS()
		_, err := tx.Exec(`INSERT INTO job_state (name, last_started_at, last_finished_at, last_status, last_error_class,
				last_duration_ms, run_count, next_due_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?, ?)
			ON CONFLICT(name) DO UPDATE SET last_started_at = excluded.last_started_at, last_finished_at = excluded.last_finished_at,
				last_status = excluded.last_status, last_error_class = excluded.last_error_class,
				last_duration_ms = excluded.last_duration_ms, run_count = job_state.run_count + 1,
				next_due_at = excluded.next_due_at, updated_at = excluded.updated_at`,
			name, msOf(r.StartedAt), msOf(r.FinishedAt), r.Status, nullStr(r.ErrorClass), dur, nullTime(r.NextDueAt), now, now)
		return err
	})
}

// GetJobState returns one job's state or ErrNotFound (a job that never ran has no row).
func (s *Store) GetJobState(ctx context.Context, name string) (JobState, error) {
	var out JobState
	err := s.Read(ctx, func(tx *Tx) error {
		var err error
		out, err = one(tx, `SELECT `+jobCols+` FROM job_state WHERE name = ?`, []any{name}, scanJob)
		return err
	})
	return out, err
}

// ListJobStates returns every job ordered by name.
func (s *Store) ListJobStates(ctx context.Context) ([]JobState, error) {
	var out []JobState
	err := s.Read(ctx, func(tx *Tx) error {
		var err error
		out, err = collect(tx, `SELECT `+jobCols+` FROM job_state ORDER BY name`, nil, scanJob)
		return err
	})
	return out, err
}

// ---------------------------------------------------------------------------
// maintenance_schedule
// ---------------------------------------------------------------------------

// MaintenanceSchedule is a device's forget/prune/check cadence and lease.
type MaintenanceSchedule struct {
	DeviceID            string
	Enabled             bool
	LastForgetAt        *time.Time
	LastPruneAt         *time.Time
	LastCheckAt         *time.Time
	NextForgetAfter     *time.Time
	NextPruneAfter      *time.Time
	NextCheckAfter      *time.Time
	DeferredCount       int
	LastDeferredAt      *time.Time
	ConsecutiveFailures int
	LeaseHolder         string
	LeaseUntil          *time.Time
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

const maintCols = `device_id, enabled, last_forget_at, last_prune_at, last_check_at, next_forget_after, next_prune_after,
	next_check_after, deferred_count, last_deferred_at, consecutive_failures, lease_holder, lease_until, created_at, updated_at`

func scanMaint(sc scanner) (MaintenanceSchedule, error) {
	var m MaintenanceSchedule
	var enabled int
	var lf, lp, lc, nf, np, nc, ld, lu sql.NullInt64
	var holder sql.NullString
	var c, u int64
	if err := sc.Scan(&m.DeviceID, &enabled, &lf, &lp, &lc, &nf, &np, &nc, &m.DeferredCount, &ld,
		&m.ConsecutiveFailures, &holder, &lu, &c, &u); err != nil {
		return MaintenanceSchedule{}, err
	}
	m.Enabled = enabled != 0
	m.LastForgetAt, m.LastPruneAt, m.LastCheckAt = ptrTime(lf), ptrTime(lp), ptrTime(lc)
	m.NextForgetAfter, m.NextPruneAfter, m.NextCheckAfter = ptrTime(nf), ptrTime(np), ptrTime(nc)
	m.LastDeferredAt, m.LeaseHolder, m.LeaseUntil = ptrTime(ld), holder.String, ptrTime(lu)
	m.CreatedAt, m.UpdatedAt = timeOf(c), timeOf(u)
	return m, nil
}

// GetMaintenanceSchedule returns a device's maintenance row (created by AgentEnroll).
func (s *Store) GetMaintenanceSchedule(ctx context.Context, scope Scope, deviceID string) (MaintenanceSchedule, error) {
	if err := scope.check(); err != nil {
		return MaintenanceSchedule{}, err
	}
	var out MaintenanceSchedule
	err := s.Read(ctx, func(tx *Tx) error {
		if _, err := scopedDevice(tx, scope, deviceID); err != nil {
			return err
		}
		var err error
		out, err = one(tx, `SELECT `+maintCols+` FROM maintenance_schedule WHERE device_id = ?`, []any{deviceID}, scanMaint)
		return err
	})
	return out, err
}

// scopedDevice returns the customer id of deviceID, or ErrNotFound when the
// device does not exist or lies outside scope (indistinguishable by design).
func scopedDevice(tx *Tx, scope Scope, deviceID string) (customerID string, err error) {
	if err := scope.check(); err != nil {
		return "", err
	}
	err = tx.QueryRow(`SELECT customer_id FROM devices WHERE id = ?`, deviceID).Scan(&customerID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if !scope.Allows(customerID) {
		return "", ErrNotFound
	}
	return customerID, nil
}
