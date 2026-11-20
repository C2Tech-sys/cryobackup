package fixtures

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// P20-W3B: the mail world of fixtures mode (ADR 0020). With it, /settings/email and the alarm
// acknowledgements show a believable console instead of empty tables: four recipients (one with
// a long name and address), a delivery log that holds every outcome and every suppress reason the
// schema allows, one acknowledged alarm, one snoozed alarm (LoadFixtures' LT-OPTOM-04) and one
// open MISSING alarm nobody has acknowledged yet. Nothing here sends anything: these are rows.
//
// The rows are written with SQL, not through the store's write methods, so their times can lie in
// the past like a log that has been running for a while (the store stamps its own clock, and the
// fixtures clock is frozen). Every value is one the store itself would write: the same columns,
// the same reason words, class words from internal/console/mail.

// M4 fixture names: the tests and the design page refer to them.
const (
	RecipientNightDesk = "Night desk"
	RecipientManager   = "Service manager"
	RecipientOnCall    = "On-call rotation"
	// RecipientLong has a name and an address near their limits, for the layout tests.
	RecipientLong        = "Escalations for after-hours coverage, second-line technicians and the partner NOC"
	RecipientLongAddress = "after-hours.escalations.second-line-technicians@noc-partner.example.test"
	// AckedHost carries the plain acknowledgement; SnoozedHost the snooze LoadFixtures writes.
	AckedHost   = "FRONTDESK-01"
	SnoozedHost = "LT-OPTOM-04"
	// AckNote is the note of the plain acknowledgement: operator text, shown where it was typed.
	AckNote = "Vendor replaces the VSS writer on Thursday; failures are expected until then."
	// MissingCustomer is the expected customer with no devices: its MISSING alarm is open.
	MissingCustomer = "Blue Ridge Accounting"
)

// m4ID makes the n-th M4 fixture id of a kind (26 characters, like every id of the schema).
func m4ID(kind byte, n int) string {
	const alpha = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	b := []byte("01JM4W3BFX00000000000000000")[:26]
	b[10] = kind
	for i := len(b) - 1; n > 0 && i > len(b)-5; i-- {
		b[i] = alpha[n%32]
		n /= 32
	}
	return string(b)
}

// SeedM4 adds the mail world to a database LoadFixtures filled. Call it once; a second call fails
// on the ids.
func SeedM4(ctx context.Context, st *store.Store, now time.Time) error {
	now = now.UTC()
	ms := func(d time.Duration) int64 { return now.Add(-d).UnixMilli() }
	return st.Write(ctx, func(tx *store.Tx) error {
		one := func(q string, args ...any) (string, error) {
			var v string
			err := tx.QueryRow(q, args...).Scan(&v)
			if errors.Is(err, sql.ErrNoRows) {
				return "", errors.New("fixtures: the mail world needs LoadFixtures first")
			}
			return v, err
		}
		admin, err := one(`SELECT id FROM admin_users WHERE username = ?`, store.FixtureAdminUsername)
		if err != nil {
			return err
		}
		acked, err := one(`SELECT id FROM devices WHERE hostname = ?`, AckedHost)
		if err != nil {
			return err
		}
		snoozed, err := one(`SELECT id FROM devices WHERE hostname = ?`, SnoozedHost)
		if err != nil {
			return err
		}
		paused, err := one(`SELECT id FROM devices WHERE hostname = 'XRAY-OLD-01'`)
		if err != nil {
			return err
		}
		missing, err := one(`SELECT id FROM customers WHERE name = ?`, MissingCustomer)
		if err != nil {
			return err
		}

		// ---- recipients ----
		type rcpt struct {
			name, addr                 string
			alerts, recoveries, digest bool
			age                        time.Duration
		}
		rs := []rcpt{
			{RecipientNightDesk, "night-desk@example.test", true, true, true, 40 * 24 * time.Hour},
			{RecipientManager, "service.manager@example.test", false, false, true, 30 * 24 * time.Hour},
			{RecipientOnCall, "oncall@example.test", true, false, false, 12 * 24 * time.Hour},
			{RecipientLong, RecipientLongAddress, true, true, false, 3 * 24 * time.Hour},
		}
		ids := make([]string, len(rs))
		for i, r := range rs {
			ids[i] = m4ID('R', i+1)
			triggers := "[]"
			if r.alerts {
				triggers = `["alert"]`
			}
			var digest any
			if r.digest {
				digest = "daily"
			}
			recov := 0
			if r.recoveries {
				recov = 1
			}
			if _, err := tx.Exec(`INSERT INTO notification_rules (id, scope, name, channel, destination, config, triggers, min_severity, digest_schedule,
				notify_recovery, enabled, created_at, updated_at) VALUES (?, 'global', ?, 'email', ?, '{}', ?, 'critical', ?, ?, 1, ?, ?)`,
				ids[i], r.name, r.addr, triggers, digest, recov, ms(r.age), ms(r.age)); err != nil {
				return err
			}
		}
		night, manager, oncall, long := ids[0], ids[1], ids[2], ids[3]

		// ---- the delivery log: every outcome and every suppress reason ----
		type row struct {
			rule                    any
			kind, subjType, subject string
			result, reason, class   string
			attempt                 int
			retryIn                 time.Duration // > 0: a retry is due this far ahead
			ago                     time.Duration
		}
		loc := Zone()
		today := now.In(loc)
		digestAt := time.Date(today.Year(), today.Month(), today.Day(), 7, 0, 0, 0, loc)
		if digestAt.After(now) {
			digestAt = digestAt.AddDate(0, 0, -1)
		}
		rows := []row{
			{oncall, "alert", "device", acked, "queued", "", "", 1, 0, 20 * time.Second},
			{long, "alert", "device", acked, "failed", "", "timeout", 2, 4 * time.Minute, time.Minute},
			{night, "alert", "device", acked, "suppressed", "acknowledged", "", 1, 0, 35 * time.Minute},
			{night, "alert", "device", snoozed, "suppressed", "snoozed", "", 1, 0, 80 * time.Minute},
			{night, "alert", "device", acked, "sent", "", "", 1, 0, 2 * time.Hour},
			{oncall, "alert", "device", acked, "sent", "", "", 1, 0, 2 * time.Hour},
			{nil, "alert", "device", acked, "failed", "", "recipient_rejected", 4, 0, 2*time.Hour + 21*time.Minute},
			{night, "alert", "device", paused, "suppressed", "paused", "", 1, 0, 4 * time.Hour},
			{night, "recovery", "device", paused, "sent", "", "", 1, 0, 5 * time.Hour},
			{oncall, "alert", "customer", missing, "suppressed", "deduped", "", 1, 0, 6 * time.Hour},
			{night, "alert", "customer", missing, "suppressed", "customer_suspended", "", 1, 0, 8 * time.Hour},
			{night, "recovery", "device", acked, "suppressed", "below_threshold", "", 1, 0, 10 * time.Hour},
			{nil, "test", "console", "", "failed", "", "auth_rejected", 1, 0, 26 * time.Hour},
			{nil, "test", "console", "", "sent", "", "", 1, 0, 25 * time.Hour},
		}
		// the digest of today and of the nine days before it, to both digest recipients: a log long enough to page
		for d := 0; d < 10; d++ {
			at := now.Sub(digestAt.AddDate(0, 0, -d))
			rows = append(rows, row{night, "digest", "console", "", "sent", "", "", 1, 0, at}, row{manager, "digest", "console", "", "sent", "", "", 1, 0, at})
		}
		for i, r := range rows {
			id := m4ID('D', i+1)
			key := "fixture:" + id
			if r.kind == "digest" {
				key = state.DigestKey(now.Add(-r.ago), loc, r.rule.(string))
			}
			var subj, reason, class, next any
			if r.subject != "" {
				subj = r.subject
			}
			if r.reason != "" {
				reason = r.reason
			}
			if r.class != "" {
				class = r.class
			}
			if r.retryIn > 0 {
				next = now.Add(r.retryIn).UnixMilli()
			}
			created := ms(r.ago + time.Duration(r.attempt-1)*time.Minute)
			if _, err := tx.Exec(`INSERT INTO notification_log (id, rule_id, dedupe_key, kind, subject_type, subject_id, channel, result, suppress_reason,
				error_class, attempt, next_retry_at, sent_at, created_at, updated_at) VALUES (?,?,?,?,?,?, 'email', ?,?,?,?,?,?,?,?)`,
				id, r.rule, key, r.kind, r.subjType, subj, r.result, reason, class, r.attempt, next, ms(r.ago), created, ms(r.ago)); err != nil {
				return err
			}
		}

		// ---- alarms: one acknowledged, one open and not acknowledged (the snoozed one is LoadFixtures') ----
		if _, err := tx.Exec(`INSERT INTO alarm_episodes (id, subject_type, device_id, alarm_type, opened_at, state_since, notified_at, created_at, updated_at)
			VALUES (?, 'device', ?, ?, ?, ?, ?, ?, ?)`, m4ID('E', 1), acked, string(state.StateFailed), ms(2*time.Hour), ms(50*time.Hour), ms(2*time.Hour), ms(2*time.Hour), ms(2*time.Hour)); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO alarm_acks (id, subject_type, device_id, alarm_type, acknowledged_by, acknowledged_at, note, created_at, updated_at)
			VALUES (?, 'device', ?, 'failed', ?, ?, ?, ?, ?)`, m4ID('A', 1), acked, admin, ms(40*time.Minute), AckNote, ms(40*time.Minute), ms(40*time.Minute)); err != nil {
			return err
		}
		_, err = tx.Exec(`INSERT INTO alarm_episodes (id, subject_type, customer_id, alarm_type, opened_at, state_since, notified_at, created_at, updated_at)
			VALUES (?, 'customer', ?, ?, ?, ?, ?, ?, ?)`, m4ID('E', 2), missing, store.AlarmMissing, ms(72*time.Hour), ms(72*time.Hour), ms(72*time.Hour), ms(72*time.Hour), ms(72*time.Hour))
		return err
	})
}
