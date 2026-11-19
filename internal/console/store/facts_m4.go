package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/state"
)

// Mail facts (M4, ADR 0020 decision 9): the loaders of the send-side facts of banners C03 and C14,
// which no code filled before. They are read from the delivery log and the digest settings, never
// inferred from a job's heartbeat.

// TestResult is the newest test send recorded in the delivery log.
type TestResult struct {
	At time.Time
	OK bool
	// ErrorClass is the class word of a failed test; empty when it passed.
	ErrorClass string
}

// SendFacts is what the delivery log says about sending.
//
// Test sends are not counted among the real ones: a test to a mistyped address says nothing about
// whether alerts are delivered. They are reported on their own in LastTest.
type SendFacts struct {
	// ConsecutiveFailures is the number of failed send attempts since the last delivered message
	// (test sends excluded), whichever messages the attempts belonged to. The log keeps one row per
	// message and cannot say how failures and deliveries of different messages interleaved, so the
	// count is kept where it happens: MarkDeliveryFailed adds one, MarkDeliverySent resets it
	// (SettingSMTPConsecutiveFailures).
	ConsecutiveFailures int
	// LastSentAt is when a message was last delivered; zero means never.
	LastSentAt time.Time
	// PendingAlerts counts the alert, recovery and nag messages still waiting for delivery (queued, or
	// failed with a retry scheduled); OldestPendingAt is when the oldest of them was queued (zero when
	// none). A message that gave up is not pending: it is in ConsecutiveFailures.
	PendingAlerts   int
	OldestPendingAt time.Time
	// LastTest is the newest test send; nil when none was ever sent.
	LastTest *TestResult
	// LastDigestSentAt is when a digest was last delivered; zero means never.
	LastDigestSentAt time.Time
}

// readFailureCount reads the consecutive-failure counter: zero when the row is absent, ErrInvalid
// when it is not a non-negative integer (the caller reports the facts as not read, never as zero).
func readFailureCount(tx *Tx) (int, error) {
	var raw string
	switch err := tx.QueryRow(`SELECT value FROM settings WHERE key = ?`, SettingSMTPConsecutiveFailures).Scan(&raw); {
	case errors.Is(err, sql.ErrNoRows):
		return 0, nil
	case err != nil:
		return 0, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < 0 {
		return 0, invalidf("the failure counter is not a non-negative integer")
	}
	return n, nil
}

// LoadSendFacts reads the send facts in one read transaction. An error means the delivery log
// could not be read; the caller must not read the zero value as "nothing is wrong" (MailFacts
// turns it into the banners' "not checked"). The log is console-wide: scope.All is required.
func (s *Store) LoadSendFacts(ctx context.Context, scope Scope) (SendFacts, error) {
	if err := s.needAll(scope); err != nil {
		return SendFacts{}, err
	}
	var f SendFacts
	err := s.Read(ctx, func(tx *Tx) error {
		f = SendFacts{}
		n, err := readFailureCount(tx)
		if err != nil {
			return err
		}
		f.ConsecutiveFailures = n
		var last, oldest, digest sql.NullInt64
		if err := tx.QueryRow(`SELECT MAX(sent_at) FROM notification_log WHERE kind <> 'test' AND result = 'sent'`).Scan(&last); err != nil {
			return err
		}
		f.LastSentAt = ptrTimeOrZero(last)
		if err := tx.QueryRow(`SELECT count(*), MIN(created_at) FROM notification_log WHERE kind NOT IN ('digest','test')
			AND (result = 'queued' OR (result = 'failed' AND next_retry_at IS NOT NULL))`).Scan(&f.PendingAlerts, &oldest); err != nil {
			return err
		}
		f.OldestPendingAt = ptrTimeOrZero(oldest)
		if err := tx.QueryRow(`SELECT MAX(sent_at) FROM notification_log WHERE kind = 'digest' AND result = 'sent'`).Scan(&digest); err != nil {
			return err
		}
		f.LastDigestSentAt = ptrTimeOrZero(digest)
		var at int64
		var res string
		var class sql.NullString
		switch err := tx.QueryRow(`SELECT sent_at, result, error_class FROM notification_log WHERE kind = 'test' AND result IN ('sent','failed')
			ORDER BY sent_at DESC, id DESC LIMIT 1`).Scan(&at, &res, &class); {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return err
		default:
			f.LastTest = &TestResult{At: timeOf(at), OK: res == DeliverySent, ErrorClass: class.String}
		}
		return nil
	})
	if err != nil {
		return SendFacts{}, err
	}
	return f, nil
}

func ptrTimeOrZero(v sql.NullInt64) time.Time {
	if !v.Valid {
		return time.Time{}
	}
	return timeOf(v.Int64)
}

func millisOrZero(t time.Time) state.Millis {
	if t.IsZero() {
		return 0
	}
	return state.Millis(t.UnixMilli())
}

// Reasons MailFacts gives for a "not checked" banner: fixed, path-free sentences.
const (
	notReadDeliveryLog    = "the delivery log could not be read"
	notReadDigestSettings = "the digest settings could not be read"
)

// MailFacts fills the two mail checks of state.ProbeFacts from real rows: the alert e-mail path
// (C03) from the delivery log and the connect probe the caller passes in, and the daily digest
// (C14) from the digest settings and the delivery log. It never fails: what cannot be read is
// said in the facts' NotRead field, which the banners turn into "not checked" (C03b, C14b).
//
//   - smtpProbe is the health prober's connect and EHLO result; it is carried into the SMTP facts as it is.
//   - The digest facts have NotInBuild false: the caller sets that marker only while no daily-digest
//     job exists, and replaces these facts with its own marker until then.
//
// The facts are console-wide: scope.All is required (ErrScope otherwise), and both returned values
// are then non-nil.
func (s *Store) MailFacts(ctx context.Context, scope Scope, smtpProbe state.ProbeRun) (*state.SMTPFacts, *state.DigestFacts, error) {
	if err := s.needAll(scope); err != nil {
		return nil, nil, err
	}
	smtp := &state.SMTPFacts{Probe: smtpProbe}
	digest := &state.DigestFacts{}
	sf, serr := s.LoadSendFacts(ctx, scope)
	if serr != nil {
		smtp.NotRead, digest.NotRead = notReadDeliveryLog, notReadDeliveryLog
	} else {
		smtp.ConsecutiveSendFailures = sf.ConsecutiveFailures
		smtp.LastSendOKAt = millisOrZero(sf.LastSentAt)
		smtp.PendingAlerts = sf.PendingAlerts
		smtp.LastTestFailed = sf.LastTest != nil && !sf.LastTest.OK
		digest.LastSentAt = millisOrZero(sf.LastDigestSentAt)
	}
	ds, derr := s.ReadDigestSettings(ctx, scope)
	if derr != nil {
		digest.NotRead = notReadDigestSettings
	} else {
		digest.Disabled, digest.LocalTime = !ds.Enabled, ds.LocalTime
	}
	return smtp, digest, nil
}
