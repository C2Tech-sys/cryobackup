package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/policy"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// LoadOptions tunes the facts loader.
type LoadOptions struct {
	// SystemDrive is the drive letter a profile preset (all_user_profiles,
	// documents_desktop) is assumed to live on when deciding whether a VSS
	// fallback hit a configured root. The agent knows the real one; the loader
	// does not, so the default is "C". fixed_drives presets cover every drive.
	SystemDrive string
}

// ThresholdsFromPolicy maps the sla and schedule sections of an effective policy
// onto the state engine's thresholds (architecture §3.6, §4.1 "thresholds").
//
// Interval is the schedule interval the MISSED rule multiplies: interval_minutes
// in interval mode; in fixed mode 24 h divided by the number of usable HH:MM
// times, falling back to interval_minutes when none parses (the same fallback
// policy.Due makes). Zero means "no schedule" and the engine raises the
// no_schedule attribute. WindowSlack is zero in v0: the MISSED rule gets no
// allowance for closed backup windows yet, so a device whose window is narrow
// may show MISSED slightly early (an early warning, never an alarm).
func ThresholdsFromPolicy(p policy.Policy) state.Thresholds {
	sla := p.SLA
	hours := func(h int) time.Duration { return time.Duration(h) * time.Hour }
	return state.Thresholds{
		StaleAfter:                    hours(sla.StaleAfterHours),
		UnseenAfter:                   hours(sla.UnseenAfterHours),
		FirstBackupGrace:              hours(sla.FirstBackupGraceHours),
		SeedExtension:                 hours(sla.SeedExtensionHours),
		MaxFailedFiles:                sla.PartialTolerance.MaxFailedFiles,
		MaxFailedPct:                  sla.PartialTolerance.MaxFailedPct,
		ConsecutiveFailuresToAlarm:    sla.ConsecutiveFailuresToAlarm,
		MaxRun:                        hours(sla.MaxRunHours),
		InitialMaxRun:                 hours(sla.InitialMaxRunHours),
		StallAfter:                    time.Duration(sla.StallAfterMinutes) * time.Minute,
		MissedFactor:                  sla.MissedFactor,
		EscrowVerifyGrace:             hours(sla.EscrowVerifyGraceHours),
		EscrowRecheck:                 hours(24 * sla.EscrowRecheckDays),
		MaintenanceDeferralAlarmAfter: sla.MaintenanceDeferralAlarmAfter,
		Interval:                      scheduleInterval(p.Schedule),
		WindowSlack:                   0,
		OutdatedIsAlarm:               p.NotificationOverrides.OutdatedAlarm,
	}
}

func scheduleInterval(s policy.Schedule) time.Duration {
	if s.Mode == policy.ModeFixed {
		usable := 0
		for _, t := range s.FixedTimes {
			if _, err := time.Parse("15:04", strings.TrimSpace(t)); err == nil {
				usable++
			}
		}
		if usable > 0 {
			return 24 * time.Hour / time.Duration(usable)
		}
	}
	if s.IntervalMinutes > 0 {
		return time.Duration(s.IntervalMinutes) * time.Minute
	}
	return 0
}

// scopeKey identifies a Scope for the per-scope previous-load device count.
func scopeKey(s Scope) string {
	if s.All {
		return "*"
	}
	ids := append([]string(nil), s.CustomerIDs...)
	sort.Strings(ids)
	return "s:" + strings.Join(ids, ",")
}

func (s *Store) previousCount(key string) int {
	s.prevMu.Lock()
	defer s.prevMu.Unlock()
	if n, ok := s.prev[key]; ok {
		return n
	}
	return -1
}

func (s *Store) rememberCount(key string, n int) {
	s.prevMu.Lock()
	defer s.prevMu.Unlock()
	if s.prev == nil {
		s.prev = map[string]int{}
	}
	s.prev[key] = n
}

// LoadWorld reads the facts the state engine needs for every device and customer
// inside scope, in ONE read transaction (a consistent snapshot), using the
// indexed queries of architecture §4.1:
//
//	last success       idx_runs_last_success   (device_id, finished_at DESC) WHERE backup AND success
//	last finished      idx_runs_last_backup_done                             WHERE backup AND finished
//	open run           idx_runs_open                                         WHERE outcome IS NULL
//	maintenance runs   idx_runs_maint_done     (device_id, kind, finished_at DESC)
//	presence           device_presence PK
//	escrow             uq_repo_key_escrow_active
//	maintenance row    maintenance_schedule PK
//	acknowledgement    uq_alarm_acks_active_device
//	expected version   idx_agent_releases_channel
//
// Nothing here is cached or persisted: state is computed from these facts at
// read time (DH7).
//
// It fails loud. A query that errors for one device (or a cancelled context)
// marks that device's facts Complete=false with a LoadErrorClass and adds one
// to World.LoadErrors; the world is returned with LoadComplete=false and the
// engine renders those devices UNKNOWN under banner C09. If the customer or
// device list itself cannot be read, LoadWorld returns the error and a world
// with LoadComplete=false. PreviousDeviceCount is the device count of the last
// complete load for this same Scope in this process (-1 before any). An empty
// result after a non-empty one does not replace it, so C09 keeps firing until
// devices come back.
func (s *Store) LoadWorld(ctx context.Context, scope Scope, opts LoadOptions) (state.World, error) {
	if err := scope.check(); err != nil {
		return state.World{}, err
	}
	key := scopeKey(scope)
	w := state.World{PreviousDeviceCount: s.previousCount(key), LoadedAt: s.Now()}
	err := s.Read(ctx, func(tx *Tx) error {
		l, err := newFactLoader(tx, opts)
		if err != nil {
			return err
		}
		clause, args := scope.clause("id")
		customers, err := collect(tx, `SELECT `+customerCols+` FROM customers WHERE `+clause+` ORDER BY id`, args, scanCustomer)
		if err != nil {
			return err
		}
		clause, args = scope.clause("customer_id")
		devices, err := collect(tx, `SELECT `+deviceCols+` FROM devices WHERE `+clause+` ORDER BY id`, args, scanDevice)
		if err != nil {
			return err
		}
		byID := make(map[string]Customer, len(customers))
		for _, c := range customers {
			byID[c.ID] = c
			w.Customers = append(w.Customers, customerFacts(c))
		}
		for _, d := range devices {
			f := l.deviceFacts(ctx, d, byID[d.CustomerID])
			if !f.Complete {
				w.LoadErrors++
			}
			w.Devices = append(w.Devices, f)
		}
		var ever string
		if err := tx.QueryRow(`SELECT value FROM settings WHERE key = 'fleet_ever_enrolled'`).Scan(&ever); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		w.FleetEverEnrolled = strings.TrimSpace(ever) == "true" || len(devices) > 0
		return nil
	})
	if err != nil {
		w.LoadComplete = false
		return w, err
	}
	w.LoadComplete = w.LoadErrors == 0
	if w.LoadComplete && (len(w.Devices) > 0 || w.PreviousDeviceCount <= 0) {
		s.rememberCount(key, len(w.Devices))
	}
	return w, nil
}

// LoadDeviceFacts is LoadWorld for one device: the same queries, the same
// fail-loud rule (an error sets Complete=false on the returned facts rather
// than failing the call), ErrNotFound when the device is outside scope.
func (s *Store) LoadDeviceFacts(ctx context.Context, scope Scope, deviceID string) (state.DeviceFacts, error) {
	if err := scope.check(); err != nil {
		return state.DeviceFacts{}, err
	}
	var out state.DeviceFacts
	err := s.Read(ctx, func(tx *Tx) error {
		l, err := newFactLoader(tx, LoadOptions{})
		if err != nil {
			return err
		}
		d, err := getDevice(tx, scope, deviceID)
		if err != nil {
			return err
		}
		c, err := one(tx, `SELECT `+customerCols+` FROM customers WHERE id = ?`, []any{d.CustomerID}, scanCustomer)
		if err != nil {
			return err
		}
		out = l.deviceFacts(ctx, d, c)
		return nil
	})
	return out, err
}

func customerFacts(c Customer) state.CustomerFacts {
	f := state.CustomerFacts{ID: c.ID, Name: c.Name, State: c.State, Expected: c.Expected, DisplayTZ: c.DisplayTZ, KeyCustody: c.KeyCustody}
	if c.OnboardingUntil != nil {
		f.OnboardingUntil = state.Millis(c.OnboardingUntil.UnixMilli())
	}
	return f
}

// factLoader carries the caches shared by the devices of one load.
type factLoader struct {
	tx       *Tx
	comp     *composer
	compErr  error
	sysDrive string
	versions map[string]string // update ring -> newest non-withdrawn release version
}

func newFactLoader(tx *Tx, opts LoadOptions) (*factLoader, error) {
	l := &factLoader{tx: tx, sysDrive: strings.ToUpper(strings.TrimSpace(opts.SystemDrive)), versions: map[string]string{}}
	if l.sysDrive == "" {
		l.sysDrive = "C"
	}
	l.comp, l.compErr = newComposer(tx)
	return l, nil
}

// loadClass names a failed step without carrying any text from the error.
func loadClass(step string, err error) string {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, ErrNoDefaultPolicy):
		return "policy_missing"
	case errors.Is(err, ErrInvalid):
		return step + "_invalid"
	}
	return step + "_query"
}

// deviceFacts loads one device's facts. It never fails: the first error sets
// Complete=false and LoadErrorClass and stops the remaining steps.
func (l *factLoader) deviceFacts(ctx context.Context, d Device, c Customer) state.DeviceFacts {
	f := state.DeviceFacts{
		ID: d.ID, CustomerID: d.CustomerID, Hostname: d.Hostname, DeviceClass: d.DeviceClass,
		EnrolledAt: millisOf(&d.EnrolledAt), EnrollConfirmedAt: millisPtr(d.EnrollConfirmedAt),
		FirstBackupDueBy: millisPtr(d.FirstBackupDueBy), RetiredAt: millisPtr(d.RetiredAt),
		PausedAt: millisPtr(d.PausedAt), PausedUntil: millisPtr(d.PausedUntil), PauseReason: d.PauseReason,
		IdentityConflictAt: millisPtr(d.IdentityConflictAt), AgentVersion: d.AgentVersion,
		CustomerKeyCustody: c.KeyCustody, Complete: true,
	}
	if d.PolicyVersionApplied != nil {
		f.PolicyVersionApplied = *d.PolicyVersionApplied
	}
	if d.RetiredAt != nil {
		// RETIRED suppresses everything, even an incomplete load (architecture §4.3): skip the queries.
		return f
	}
	fail := func(step string, err error) state.DeviceFacts {
		f.Complete = false
		f.LoadErrorClass = loadClass(step, err)
		return f
	}
	if err := ctx.Err(); err != nil {
		return fail("context", err)
	}

	// effective policy -> thresholds, forget cadence, VSS roots
	if l.compErr != nil {
		return fail("policy", l.compErr)
	}
	pol, ver, _, err := l.comp.effective(d.ID, c.PolicyID)
	if err != nil {
		return fail("policy", err)
	}
	f.Thresholds = ThresholdsFromPolicy(pol)
	f.PolicyVersion, f.HasSchedule = ver, f.Thresholds.Interval > 0
	roots := l.rootDrives(pol)

	// presence
	var recv, off sql.NullInt64
	var state_, av, progress sql.NullString
	switch err := l.tx.QueryRow(`SELECT received_at, service_state, agent_version, clock_offset_ms, progress
		FROM device_presence WHERE device_id = ?`, d.ID).Scan(&recv, &state_, &av, &off, &progress); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return fail("presence", err)
	default:
		f.LastSeen, f.ServiceState = state.Millis(recv.Int64), state_.String
		if av.Valid && av.String != "" {
			f.AgentVersion = av.String
		}
		f.ClockOffset = time.Duration(off.Int64) * time.Millisecond
		f.ProgressAdvancedAt = progressAdvancedAt(progress)
	}

	// runs: last success, last finished backup, open run, newest attempt, failure streak
	var lastSuccess int64
	switch err := l.tx.QueryRow(lastSuccessSQL, d.ID).Scan(&lastSuccess); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return fail("runs", err)
	default:
		f.LastSuccess = state.Millis(lastSuccess)
	}
	last, err := l.backupRun(d.ID, doneBackupWhere, doneBackupOrder, roots)
	if err != nil {
		return fail("runs", err)
	}
	f.LastBackup = last
	open, err := l.backupRun(d.ID, openBackupWhere, openBackupOrder, roots)
	if err != nil {
		return fail("runs", err)
	}
	f.OpenRun = open
	var attempt sql.NullInt64
	if err := l.tx.QueryRow(`SELECT MAX(started_at) FROM runs INDEXED BY idx_runs_device_started WHERE device_id = ? AND kind = 'backup'`, d.ID).Scan(&attempt); err != nil {
		return fail("runs", err)
	}
	f.LastAttemptStart = state.Millis(attempt.Int64)
	outcomes, err := collect(l.tx, `SELECT outcome FROM runs INDEXED BY idx_runs_last_backup_done WHERE device_id = ? AND kind = 'backup'
		AND outcome IS NOT NULL AND finished_at > ? ORDER BY finished_at DESC LIMIT 1000`, []any{d.ID, lastSuccess},
		func(sc scanner) (string, error) {
			var o string
			return o, sc.Scan(&o)
		})
	if err != nil {
		return fail("runs", err)
	}
	for _, o := range outcomes {
		if state.OutcomeClass(o, false) == state.ClassFailure {
			f.ConsecutiveFailures++
		}
	}
	// "Initial" means no success since (re)enrollment: it widens the stuck limit while progress advances.
	initial := f.LastSuccess == 0 || f.LastSuccess < f.EnrolledAt
	if f.OpenRun != nil {
		f.OpenRun.Initial = initial
	}
	if f.LastBackup != nil {
		f.LastBackup.Initial = initial
	}

	// escrow
	esc, err := one(l.tx, `SELECT `+escrowCols+` FROM repo_key_escrow WHERE device_id = ? AND retired_at IS NULL`, []any{d.ID}, scanEscrow)
	switch {
	case errors.Is(err, ErrNotFound):
	case err != nil:
		return fail("escrow", err)
	default:
		ef := &state.EscrowFacts{Custody: esc.Custody, CreatedAt: millisOf(&esc.CreatedAt),
			VerifiedAt: millisPtr(esc.VerifiedAt), LastVerifyErrorClass: esc.LastVerifyErrorClass}
		f.Escrow = ef
	}

	// maintenance (does not apply to customer-held keys)
	custody := c.KeyCustody
	if f.Escrow != nil && f.Escrow.Custody != "" {
		custody = f.Escrow.Custody
	}
	if custody != "customer_held" {
		m, err := one(l.tx, `SELECT `+maintCols+` FROM maintenance_schedule WHERE device_id = ?`, []any{d.ID}, scanMaint)
		switch {
		case errors.Is(err, ErrNotFound):
			return fail("maintenance_row", ErrInvalid) // an escrowed device without its row is an inconsistent row
		case err != nil:
			return fail("maintenance", err)
		}
		if m.Enabled {
			mf := &state.MaintFacts{DeferredCount: m.DeferredCount,
				ForgetCadence: time.Duration(pol.Maintenance.ForgetEveryDays) * 24 * time.Hour}
			if mf.LastForget, err = l.maintRun(d.ID, "forget"); err != nil {
				return fail("maintenance", err)
			}
			if mf.LastPrune, err = l.maintRun(d.ID, "prune"); err != nil {
				return fail("maintenance", err)
			}
			if mf.LastCheck, err = l.maintRun(d.ID, "check"); err != nil {
				return fail("maintenance", err)
			}
			var fs sql.NullInt64
			if err := l.tx.QueryRow(`SELECT finished_at FROM runs INDEXED BY idx_runs_maint_done WHERE device_id = ? AND kind = 'forget'
				AND kind IN ('forget','prune','check') AND outcome = 'success' ORDER BY finished_at DESC LIMIT 1`, d.ID).Scan(&fs); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return fail("maintenance", err)
			}
			mf.LastForgetSuccessAt = state.Millis(fs.Int64)
			f.Maintenance = mf
		}
	}

	// acknowledgement (v0: the newest active ack covers the device)
	var ackedAt int64
	var snooze sql.NullInt64
	var untilNext int
	switch err := l.tx.QueryRow(`SELECT acknowledged_at, snooze_until, snooze_until_next_success FROM alarm_acks
		WHERE device_id = ? AND cleared_at IS NULL ORDER BY acknowledged_at DESC LIMIT 1`, d.ID).Scan(&ackedAt, &snooze, &untilNext); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return fail("ack", err)
	default:
		a := &state.AckFacts{AckedAt: state.Millis(ackedAt), SnoozeUntil: state.Millis(snooze.Int64), SnoozeUntilNextSuccess: untilNext != 0}
		var at sql.NullInt64
		if err := l.tx.QueryRow(`SELECT MIN(finished_at, received_at) FROM runs INDEXED BY idx_runs_last_success
			WHERE device_id = ? AND kind = 'backup' AND outcome = 'success' AND finished_at <= ? ORDER BY finished_at DESC LIMIT 1`, d.ID, ackedAt).Scan(&at); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fail("ack", err)
		}
		a.AckedAtSuccess = state.Millis(at.Int64)
		f.Ack = a
	}

	// expected agent version: the pin, else the newest non-withdrawn release on the device's ring
	if d.TargetVersion != "" {
		f.ExpectedVersion = d.TargetVersion
	} else {
		v, err := l.ringVersion(d.UpdateRing)
		if err != nil {
			return fail("release", err)
		}
		f.ExpectedVersion = v
	}
	return f
}

func millisOf(t *time.Time) state.Millis {
	if t == nil {
		return 0
	}
	return state.Millis(t.UnixMilli())
}

func millisPtr(t *time.Time) state.Millis { return millisOf(t) }

// progressAdvancedAt reads the "advanced_at" key AgentUpsertPresence maintains.
func progressAdvancedAt(raw sql.NullString) state.Millis {
	if !raw.Valid {
		return 0
	}
	var m map[string]any
	if json.Unmarshal([]byte(raw.String), &m) != nil {
		return 0
	}
	if at, ok := m["advanced_at"].(float64); ok {
		return state.Millis(int64(at))
	}
	return 0
}

// ringVersion returns the newest non-withdrawn release version on a ring, "" when none.
func (l *factLoader) ringVersion(ring string) (string, error) {
	if v, ok := l.versions[ring]; ok {
		return v, nil
	}
	var v string
	err := l.tx.QueryRow(`SELECT version FROM agent_releases WHERE channel = ? AND withdrawn_at IS NULL
		ORDER BY released_at DESC LIMIT 1`, ring).Scan(&v)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	l.versions[ring] = v
	return v, nil
}

// driveSet is the set of drive letters that hold a configured backup root.
type driveSet struct {
	all     bool
	letters map[string]bool
}

// rootDrives derives the drive letters of the effective policy's roots: explicit
// drive-absolute roots by their letter, profile presets by the system drive,
// fixed_drives as every drive.
func (l *factLoader) rootDrives(p policy.Policy) driveSet {
	ds := driveSet{letters: map[string]bool{}}
	for _, r := range p.Selection.Roots {
		if len(r) >= 2 && r[1] == ':' {
			ds.letters[strings.ToUpper(r[:1])] = true
		}
	}
	for _, pr := range p.Selection.Presets {
		switch pr {
		case policy.PresetFixedDrives:
			ds.all = true
		case policy.PresetAllUserProfiles, policy.PresetDocumentsDesktop:
			ds.letters[l.sysDrive] = true
		}
	}
	return ds
}

// The loader's run queries. The predicates are literals on purpose: SQLite uses
// a partial index only when the query's WHERE visibly implies the index's, and
// a bound parameter does not. A test runs EXPLAIN QUERY PLAN over these exact
// strings and fails if the intended index is not chosen.
const (
	lastSuccessSQL  = `SELECT MIN(finished_at, received_at) FROM runs INDEXED BY idx_runs_last_success WHERE device_id = ? AND kind = 'backup' AND outcome = 'success' ORDER BY finished_at DESC LIMIT 1`
	doneBackupWhere = `kind = 'backup' AND outcome IS NOT NULL` // idx_runs_last_backup_done
	doneBackupOrder = `finished_at DESC`
	openBackupWhere = `kind = 'backup' AND outcome IS NULL` // idx_runs_open
	openBackupOrder = `started_at DESC`
	// The redundant IN (...) term restates the partial index predicate so idx_runs_maint_done is
	// eligible; kind = ? then uses the index's second column.
	maintRunSQL = `SELECT outcome, started_at, finished_at, received_at, stats, error_counts, error_class, deferred_reason
		FROM runs INDEXED BY idx_runs_maint_done WHERE device_id = ? AND kind IN ('forget','prune','check') AND kind = ? AND outcome IS NOT NULL
		ORDER BY finished_at DESC LIMIT 1`
)

// backupRunSQL pins the partial index with INDEXED BY: if the index were ever
// renamed or dropped the query fails (the device becomes UNKNOWN, loudly)
// instead of silently scanning the runs table.
func backupRunSQL(where, order string) string {
	index := "idx_runs_last_backup_done"
	if where == openBackupWhere {
		index = "idx_runs_open"
	}
	return `SELECT outcome, started_at, finished_at, received_at, stats, error_counts, error_class, deferred_reason
		FROM runs INDEXED BY ` + index + ` WHERE device_id = ? AND ` + where + ` ORDER BY ` + order + ` LIMIT 1`
}

// backupRun reads the newest run matching where, ordered by order, as RunFacts.
// The literal predicates are what lets SQLite use the partial indexes.
func (l *factLoader) backupRun(deviceID, where, order string, roots driveSet) (*state.RunFacts, error) {
	var outcome, errClass, deferred sql.NullString
	var started, recv int64
	var fin sql.NullInt64
	var stats, counts string
	err := l.tx.QueryRow(backupRunSQL(where, order), deviceID).
		Scan(&outcome, &started, &fin, &recv, &stats, &counts, &errClass, &deferred)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return runFacts(outcome.String, started, fin, recv, stats, counts, errClass.String, deferred.String, roots), nil
}

// maintRun reads the newest finished run of one maintenance operation. The
// redundant IN (...) term restates the partial index predicate so idx_runs_maint_done
// is eligible; kind = ? then uses the index's second column.
func (l *factLoader) maintRun(deviceID, kind string) (*state.RunFacts, error) {
	var outcome, errClass, deferred sql.NullString
	var started, recv int64
	var fin sql.NullInt64
	var stats, counts string
	err := l.tx.QueryRow(maintRunSQL, deviceID, kind).
		Scan(&outcome, &started, &fin, &recv, &stats, &counts, &errClass, &deferred)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return runFacts(outcome.String, started, fin, recv, stats, counts, errClass.String, deferred.String, driveSet{}), nil
}

// runFacts maps a runs row onto state.RunFacts. FinishedAt is clamped to
// received_at (B40: a finish time can never be in the console's future).
// CountsKnown is true only when both total_files_processed and failed_files are
// present and non-negative numbers in the stats document.
func runFacts(outcome string, started int64, fin sql.NullInt64, recv int64, stats, counts, errClass, deferred string, roots driveSet) *state.RunFacts {
	rf := &state.RunFacts{
		Outcome: outcome, StartedAt: state.Millis(started), ErrorClass: errClass, DeferredReason: deferred,
	}
	if fin.Valid {
		f := fin.Int64
		if recv > 0 && f > recv {
			f = recv
		}
		rf.FinishedAt = state.Millis(f)
	}
	var st map[string]json.RawMessage
	if json.Unmarshal([]byte(stats), &st) == nil && st != nil {
		var total, failed float64
		_, okT := number(st["total_files_processed"], &total)
		_, okF := number(st["failed_files"], &failed)
		if okT && okF && total >= 0 && failed >= 0 {
			rf.CountsKnown = true
			rf.TotalFiles, rf.FailedFiles = int64(total), int64(failed)
		}
		var vols []string
		if json.Unmarshal(st["vss_fallback_volumes"], &vols) == nil {
			for _, v := range vols {
				letter := strings.ToUpper(strings.TrimRight(strings.TrimSpace(v), `:\/`))
				if len(letter) != 1 {
					continue
				}
				rf.VSSFallback = append(rf.VSSFallback, letter)
				if roots.all || roots.letters[letter] {
					rf.VSSFallbackOnRoot = true
				}
			}
		}
	}
	var ec map[string]int
	if json.Unmarshal([]byte(counts), &ec) == nil && len(ec) > 0 {
		rf.ErrorCounts = ec
	}
	return rf
}

func number(raw json.RawMessage, out *float64) (float64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return 0, false
	}
	return *out, true
}
