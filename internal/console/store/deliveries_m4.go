package store

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strings"
	"time"
)

// The delivery log (M4, ADR 0020 decision 9): every message the console sends, or decides not to
// send, is one notification_log row. The table is used as designed:
//
//	dedupe_key       names the message ("digest:<local-date>:<rule id>"); one row per key
//	kind, subject_*  what it is about; subject_type 'console' (no subject id) for a digest or a test
//	result           queued -> sent, or queued -> failed -> sent / failed; 'suppressed' rows record a
//	                 decision not to send (suppress_reason says why) and are never sent
//	error_class      a class word of the newest failure, never the relay's own text
//	attempt          the number of the newest attempt: 1 while queued, +1 for each retry that follows a
//	                 failure, so a row that gave up after three retries reads 4
//	next_retry_at    when the next attempt is due; NULL once the row gave up
//	sent_at          the time of the newest attempt or decision (the table's own definition)
//
// The log holds no recipient address, no subject line and no body; the rule id leads to the
// recipient while the rule exists. The log is console-wide and names no customer, so every
// method here needs scope.All (ErrScope otherwise).

// Delivery kinds (the schema's kind CHECK).
const (
	DeliveryAlert    = "alert"
	DeliveryRecovery = "recovery"
	DeliveryNag      = "nag"
	DeliveryDigest   = "digest"
	DeliveryReport   = "report"
	DeliveryTest     = "test"
	DeliveryEvent    = "event"
)

// Delivery results.
const (
	DeliveryQueued     = "queued"
	DeliverySent       = "sent"
	DeliveryFailed     = "failed"
	DeliverySuppressed = "suppressed"
)

// Suppress reasons (the schema's CHECK).
var deliverySuppressReasons = []string{"acknowledged", "snoozed", "paused", "deduped", "customer_suspended", "below_threshold"}

// DeliveryRetryDelays is the schedule of ADR 0020 decision 9: a failed send is retried up to three
// times, after 1, 5 and 15 minutes. DeliveryRetryDelay(attempt) reads it.
var DeliveryRetryDelays = []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute}

// DeliveryRetryDelay returns how long after the failure of attempt n (1 is the first send) the next
// attempt is due, and false when that was the last one.
func DeliveryRetryDelay(attempt int) (time.Duration, bool) {
	if attempt < 1 || attempt > len(DeliveryRetryDelays) {
		return 0, false
	}
	return DeliveryRetryDelays[attempt-1], true
}

// QueuedStaleAfter is how long a row may stay 'queued' before ListDueRetries returns it: a sender
// that crashed between queueing and sending leaves one behind, and nothing else would ever pick it up.
const QueuedStaleAfter = 5 * time.Minute

var (
	errClassRE  = regexp.MustCompile(`^[a-z0-9_]{1,40}$`)
	dedupeKeyRE = regexp.MustCompile(`^[\x21-\x7e]{1,200}$`)
)

// Delivery is one notification_log row.
type Delivery struct {
	ID             string
	RuleID         string // empty when the rule was deleted or the message has no rule
	DedupeKey      string
	Kind           string
	SubjectType    string // device, customer or console
	SubjectID      string
	Result         string
	SuppressReason string
	ErrorClass     string
	Attempt        int
	NextRetryAt    *time.Time
	// SentAt is the time of the newest attempt or decision.
	SentAt    time.Time
	CreatedAt time.Time
}

// DeliveryInput is a message to record.
type DeliveryInput struct {
	Kind string
	// RuleID is the recipient rule the message is for; empty for a message with none (a test to an address typed by hand).
	RuleID    string
	DedupeKey string
	// SubjectType is device, customer or console; SubjectID must be set for the first two.
	SubjectType string
	SubjectID   string
	// SuppressReason, when set, records a decision not to send: the row is 'suppressed' and never queued.
	SuppressReason string
}

const deliveryCols = `id, rule_id, dedupe_key, kind, subject_type, subject_id, result, suppress_reason, error_class, attempt, next_retry_at, sent_at, created_at`

func scanDelivery(sc scanner) (Delivery, error) {
	var d Delivery
	var rule, subj, reason, class sql.NullString
	var next sql.NullInt64
	var sent, created int64
	if err := sc.Scan(&d.ID, &rule, &d.DedupeKey, &d.Kind, &d.SubjectType, &subj, &d.Result, &reason, &class, &d.Attempt, &next, &sent, &created); err != nil {
		return Delivery{}, err
	}
	d.RuleID, d.SubjectID, d.SuppressReason, d.ErrorClass = rule.String, subj.String, reason.String, class.String
	d.NextRetryAt, d.SentAt, d.CreatedAt = ptrTime(next), timeOf(sent), timeOf(created)
	return d, nil
}

func (in DeliveryInput) validate() error {
	var bad []FieldError
	switch in.Kind {
	case DeliveryAlert, DeliveryRecovery, DeliveryNag, DeliveryDigest, DeliveryReport, DeliveryTest, DeliveryEvent:
	default:
		bad = append(bad, fieldf("kind", "Use alert, recovery, nag, digest, report, test or event."))
	}
	if !dedupeKeyRE.MatchString(in.DedupeKey) {
		bad = append(bad, fieldf("dedupe_key", "Use 1 to 200 printable characters without spaces."))
	}
	switch in.SubjectType {
	case "device", "customer":
		if in.SubjectID == "" || len(in.SubjectID) > 40 {
			bad = append(bad, fieldf("subject_id", "Name the device or customer."))
		}
	case "console":
		if in.SubjectID != "" {
			bad = append(bad, fieldf("subject_id", "A message about the console names no subject."))
		}
	default:
		bad = append(bad, fieldf("subject_type", "Use device, customer or console."))
	}
	if in.SuppressReason != "" {
		found := false
		for _, r := range deliverySuppressReasons {
			found = found || r == in.SuppressReason
		}
		if !found {
			bad = append(bad, fieldf("suppress_reason", "Use a known suppress reason."))
		}
	}
	return validation(bad)
}

// EnqueueDelivery records a message as queued (or as suppressed when in.SuppressReason is set),
// with attempt 1 and sent_at set to now. If a row with the same dedupe key exists, whatever its
// result, nothing is written and that row is returned with created false: the message was already
// decided. A rule id that does not exist is ErrNotFound.
func (s *Store) EnqueueDelivery(ctx context.Context, scope Scope, in DeliveryInput) (d Delivery, created bool, err error) {
	if err := s.needAll(scope); err != nil {
		return Delivery{}, false, err
	}
	if err := in.validate(); err != nil {
		return Delivery{}, false, err
	}
	err = s.Write(ctx, func(tx *Tx) error {
		d, created = Delivery{}, false
		existing, err := one(tx, `SELECT `+deliveryCols+` FROM notification_log WHERE dedupe_key = ? ORDER BY sent_at DESC, id DESC LIMIT 1`, []any{in.DedupeKey}, scanDelivery)
		switch {
		case err == nil:
			d = existing
			return nil
		case !errors.Is(err, ErrNotFound):
			return err
		}
		if in.RuleID != "" {
			var found int
			if err := tx.QueryRow(`SELECT 1 FROM notification_rules WHERE id = ?`, in.RuleID).Scan(&found); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return ErrNotFound
				}
				return err
			}
		}
		now := tx.nowMS()
		id := tx.id()
		result := DeliveryQueued
		if in.SuppressReason != "" {
			result = DeliverySuppressed
		}
		if _, err := tx.Exec(`INSERT INTO notification_log (id, rule_id, dedupe_key, kind, subject_type, subject_id, channel, result, suppress_reason, attempt, sent_at, created_at, updated_at)
			VALUES (?,?,?,?,?,?,'email',?,?,1,?,?,?)`, id, nullStr(in.RuleID), in.DedupeKey, in.Kind, in.SubjectType, nullStr(in.SubjectID), result, nullStr(in.SuppressReason), now, now, now); err != nil {
			return err
		}
		d, err = one(tx, `SELECT `+deliveryCols+` FROM notification_log WHERE id = ?`, []any{id}, scanDelivery)
		created = err == nil
		return err
	})
	if err != nil {
		return Delivery{}, false, err
	}
	return d, created, nil
}

// MarkDeliverySent records that the message was delivered: result 'sent', sent_at now, no error
// class, no retry. A row that failed before counts the retry (attempt + 1). A row that is already
// sent is returned unchanged; a suppressed row is ErrInvalid (it was never to be sent).
func (s *Store) MarkDeliverySent(ctx context.Context, scope Scope, id string) (Delivery, error) {
	return s.finishDelivery(ctx, scope, id, DeliverySent, "", nil)
}

// MarkDeliveryFailed records a failed attempt: result 'failed', the class word of the failure
// (lower-case letters, digits and underscores, at most 40; never the server's text) and when the
// next attempt is due, or nil when the row gives up. A row that failed before counts the retry
// (attempt + 1). A row that is already sent is ErrConflict; a suppressed row is ErrInvalid.
func (s *Store) MarkDeliveryFailed(ctx context.Context, scope Scope, id, errorClass string, nextRetryAt *time.Time) (Delivery, error) {
	if !errClassRE.MatchString(errorClass) {
		return Delivery{}, validation([]FieldError{fieldf("error_class", "Use a class word of lower-case letters, digits and underscores.")})
	}
	return s.finishDelivery(ctx, scope, id, DeliveryFailed, errorClass, nextRetryAt)
}

func (s *Store) finishDelivery(ctx context.Context, scope Scope, id, result, class string, next *time.Time) (Delivery, error) {
	if err := s.needAll(scope); err != nil {
		return Delivery{}, err
	}
	var out Delivery
	err := s.Write(ctx, func(tx *Tx) error {
		cur, err := one(tx, `SELECT `+deliveryCols+` FROM notification_log WHERE id = ?`, []any{id}, scanDelivery)
		if err != nil {
			return err
		}
		switch {
		case cur.Result == DeliverySuppressed:
			return invalidf("a suppressed message is never sent")
		case cur.Result == DeliverySent && result == DeliverySent:
			out = cur
			return nil
		case cur.Result == DeliverySent:
			return conflictf("the message was already delivered")
		}
		attempt := cur.Attempt
		if cur.Result == DeliveryFailed {
			attempt++ // this outcome belongs to a retry
		}
		now := tx.nowMS()
		if _, err := tx.Exec(`UPDATE notification_log SET result = ?, error_class = ?, attempt = ?, next_retry_at = ?, sent_at = ?, updated_at = ? WHERE id = ?`,
			result, nullStr(class), attempt, nullTime(next), now, now, id); err != nil {
			return err
		}
		out, err = one(tx, `SELECT `+deliveryCols+` FROM notification_log WHERE id = ?`, []any{id}, scanDelivery)
		return err
	})
	if err != nil {
		return Delivery{}, err
	}
	return out, nil
}

// ListDueRetries returns the messages that need another attempt at now: failed rows whose
// next_retry_at has come, and rows left 'queued' for QueuedStaleAfter or longer (a sender that died
// before it sent). Oldest due first; limit is clamped to 1 to 1000 (0 means 100).
func (s *Store) ListDueRetries(ctx context.Context, scope Scope, now time.Time, limit int) ([]Delivery, error) {
	if err := s.needAll(scope); err != nil {
		return nil, err
	}
	switch {
	case limit <= 0:
		limit = defaultPageLimit
	case limit > maxPageLimit:
		limit = maxPageLimit
	}
	at := msOf(now)
	out := []Delivery{}
	err := s.Read(ctx, func(tx *Tx) error {
		rows, err := collect(tx, `SELECT `+deliveryCols+` FROM notification_log
			WHERE (result = 'failed' AND next_retry_at IS NOT NULL AND next_retry_at <= ?)
			   OR (result = 'queued' AND created_at <= ?)
			ORDER BY COALESCE(next_retry_at, created_at), id LIMIT ?`, []any{at, at - QueuedStaleAfter.Milliseconds(), limit}, scanDelivery)
		out = append(out, rows...)
		return err
	})
	return out, err
}

// DeliveryFilter narrows ListRecentDeliveries; empty fields do not filter.
type DeliveryFilter struct {
	Kind   string
	Result string
}

// ListRecentDeliveries returns the log, newest attempt first, one page at a time (the cursor is
// Listing.NextCursor of the previous page). An unknown kind or result is a *ValidationError.
func (s *Store) ListRecentDeliveries(ctx context.Context, scope Scope, f DeliveryFilter, page Page) (Listing[Delivery], error) {
	if err := s.needAll(scope); err != nil {
		return Listing[Delivery]{}, err
	}
	limit, offset, err := page.window()
	if err != nil {
		return Listing[Delivery]{}, err
	}
	var where []string
	var args []any
	var bad []FieldError
	if f.Kind != "" {
		if (DeliveryInput{Kind: f.Kind, DedupeKey: "k", SubjectType: "console"}).validate() != nil {
			bad = append(bad, fieldf("kind", "Use alert, recovery, nag, digest, report, test or event."))
		}
		where, args = append(where, "kind = ?"), append(args, f.Kind)
	}
	if f.Result != "" {
		switch f.Result {
		case DeliveryQueued, DeliverySent, DeliveryFailed, DeliverySuppressed:
		default:
			bad = append(bad, fieldf("result", "Use queued, sent, failed or suppressed."))
		}
		where, args = append(where, "result = ?"), append(args, f.Result)
	}
	if err := validation(bad); err != nil {
		return Listing[Delivery]{}, err
	}
	clause := "1 = 1"
	if len(where) > 0 {
		clause = strings.Join(where, " AND ")
	}
	var items []Delivery
	err = s.Read(ctx, func(tx *Tx) error {
		var err error
		items, err = collect(tx, `SELECT `+deliveryCols+` FROM notification_log WHERE `+clause+` ORDER BY sent_at DESC, id DESC LIMIT ? OFFSET ?`,
			append(args, limit+1, offset), scanDelivery)
		return err
	})
	if err != nil {
		return Listing[Delivery]{}, err
	}
	return finish(items, limit, offset), nil
}

// HasDelivered reports whether a message with this dedupe key was delivered (result 'sent'). A
// queued, failed or suppressed row does not count.
func (s *Store) HasDelivered(ctx context.Context, scope Scope, dedupeKey string) (bool, error) {
	if err := s.needAll(scope); err != nil {
		return false, err
	}
	var n int
	err := s.Read(ctx, func(tx *Tx) error {
		return tx.QueryRow(`SELECT count(*) FROM notification_log WHERE dedupe_key = ? AND result = 'sent'`, dedupeKey).Scan(&n)
	})
	return n > 0, err
}
