package store

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/state"
)

// Alarm episodes (M4, ADR 0020 decision 6). States are computed on read and nothing remembers
// that a mail went out; an episode is that memory. There is at most one OPEN episode per
// subject (a device or a customer) and alarm type, and an open episode is never mailed twice,
// whatever happens to the state's Since value. When the alarm is gone the episode closes and
// one recovery notice is due.
//
// The alerting jobs use these methods in this order:
//
//	SyncEpisodes                 every evaluator pass, with the alarms that exist right now
//	ListUnnotifiedEpisodes       what still has to be mailed; MarkEpisodesNotified after the mail is queued
//	ListRecoveryDueEpisodes      what has closed since its alert went out; MarkRecoveryNotified after queueing
//
// An episode that opened and closed without an alert ever going out owes no recovery notice,
// so it never appears in ListRecoveryDueEpisodes.

// Episode subjects.
const (
	EpisodeDevice   = "device"
	EpisodeCustomer = "customer"
)

// AlarmMissing is the alarm type of a customer episode: an expected customer with no devices.
const AlarmMissing = "MISSING"

// criticalAlarmTypes are the states that mail immediately (ADR 0020 decision 4); alarmLevelTypes
// are the ones that appear in the digest only. Episodes may exist for either: the vocabulary
// is the state names of internal/state, checked here so a typo cannot open a nameless alarm.
var criticalAlarmTypes = []string{
	string(state.StateFailed), string(state.StateStale), string(state.StateUnseen), string(state.StateBlank),
	string(state.StateStuck), string(state.StateIdentityConflict), string(state.StateUnknown),
}

var alarmLevelTypes = []string{
	string(state.StatePartial), string(state.StateMaintFailed), string(state.StateEscrowUnverified), string(state.StateOutdated),
}

// EpisodeAlarmTypes returns the alarm types an episode of the given subject may carry
// (EpisodeDevice or EpisodeCustomer); nil for another subject. The slice is a copy.
func EpisodeAlarmTypes(subject string) []string {
	switch subject {
	case EpisodeDevice:
		return append(append([]string(nil), criticalAlarmTypes...), alarmLevelTypes...)
	case EpisodeCustomer:
		return []string{AlarmMissing}
	}
	return nil
}

func validEpisodeType(subject, alarm string) bool {
	for _, a := range EpisodeAlarmTypes(subject) {
		if a == alarm {
			return true
		}
	}
	return false
}

// EpisodeKey names one alarm that exists now: the input of SyncEpisodes.
type EpisodeKey struct {
	// SubjectType is EpisodeDevice or EpisodeCustomer; SubjectID the device or customer id.
	SubjectType, SubjectID string
	// AlarmType is the state name (EpisodeAlarmTypes).
	AlarmType string
	// Since is the state engine's "since" for the alarm; zero when it has none. It is kept for
	// display only and never decides whether a mail is due.
	Since time.Time
}

// Episode is one alarm episode.
type Episode struct {
	ID                 string
	SubjectType        string
	SubjectID          string // the device id or the customer id
	CustomerID         string // the owning customer; equals SubjectID for a customer episode
	AlarmType          string
	OpenedAt           time.Time
	StateSince         time.Time
	NotifiedAt         *time.Time // nil = the alert has not gone out
	ClosedAt           *time.Time // nil = still open
	RecoveryNotifiedAt *time.Time // nil = no recovery notice yet
}

// Open reports whether the episode is still open.
func (e Episode) Open() bool { return e.ClosedAt == nil }

const episodeSelect = `SELECT e.id, e.subject_type, e.device_id, e.customer_id, COALESCE(e.customer_id, d.customer_id), e.alarm_type,
	e.opened_at, e.state_since, e.notified_at, e.closed_at, e.recovery_notified_at
	FROM alarm_episodes e LEFT JOIN devices d ON d.id = e.device_id`

const episodeOwner = `COALESCE(e.customer_id, d.customer_id)`

func scanEpisode(sc scanner) (Episode, error) {
	var e Episode
	var dev, cust, owner sql.NullString
	var opened, since int64
	var notified, closed, recovery sql.NullInt64
	if err := sc.Scan(&e.ID, &e.SubjectType, &dev, &cust, &owner, &e.AlarmType, &opened, &since, &notified, &closed, &recovery); err != nil {
		return Episode{}, err
	}
	e.SubjectID = dev.String
	if e.SubjectType == EpisodeCustomer {
		e.SubjectID = cust.String
	}
	e.CustomerID = owner.String
	e.OpenedAt, e.StateSince = timeOf(opened), timeOf(since)
	e.NotifiedAt, e.ClosedAt, e.RecoveryNotifiedAt = ptrTime(notified), ptrTime(closed), ptrTime(recovery)
	return e, nil
}

// episodeRef is the identity of an alarm: what SyncEpisodes compares.
type episodeRef struct{ subjectType, subjectID, alarm string }

func (e Episode) ref() episodeRef { return episodeRef{e.SubjectType, e.SubjectID, e.AlarmType} }

// EpisodeSync is the outcome of SyncEpisodes.
type EpisodeSync struct {
	// Opened are the episodes that began in this call, Closed those that ended in it (both with their
	// final values), in the order they opened.
	Opened, Closed []Episode
	// ClearedAcks counts acknowledgements and snoozes cleared because the alarm they covered ended.
	ClearedAcks int
}

// SyncEpisodes makes the open episodes inside scope match current, the alarms that exist right
// now: it opens an episode for each key that has none open and closes every open episode whose
// key is absent, in one transaction. Calling it again with the same input changes nothing
// (empty result). now is when the evaluator pass ran; zero means the store clock.
//
// An acknowledgement or snooze that covers an episode this call closes is cleared in the same
// transaction (one alarm_ack.clear audit row, actor system): the alarm it muted is over, and an
// ack left behind would silently mute the next, unrelated failure of the same kind.
//
// The caller must pass the complete set of alarms inside scope. A pass that could not load the
// fleet must not call SyncEpisodes with what it did load: every episode missing from the input
// closes and sends a recovery notice.
//
// A key that names a subject outside scope or one that does not exist is ErrNotFound and nothing
// is changed; an unknown alarm type or subject type is a *ValidationError.
func (s *Store) SyncEpisodes(ctx context.Context, scope Scope, current []EpisodeKey, now time.Time) (EpisodeSync, error) {
	if err := scope.check(); err != nil {
		return EpisodeSync{}, err
	}
	if now.IsZero() {
		now = s.Now()
	}
	at := msOf(now)
	var bad []FieldError
	want := map[episodeRef]EpisodeKey{}
	var order []episodeRef
	for _, k := range current {
		switch {
		case k.SubjectID == "" || len(k.SubjectID) > 40:
			bad = append(bad, fieldf("subject_id", "Name the device or customer the alarm belongs to."))
		case !validEpisodeType(k.SubjectType, k.AlarmType):
			bad = append(bad, fieldf("alarm_type", "Use a known alarm type for a %s episode.", k.SubjectType))
		default:
			r := episodeRef{k.SubjectType, k.SubjectID, k.AlarmType}
			if _, dup := want[r]; !dup {
				want[r] = k
				order = append(order, r)
			}
		}
	}
	if err := validation(bad); err != nil {
		return EpisodeSync{}, err
	}
	var out EpisodeSync
	err := s.Write(ctx, func(tx *Tx) error {
		out = EpisodeSync{}
		owners := map[episodeRef]string{}
		for _, r := range order {
			switch r.subjectType {
			case EpisodeDevice:
				d, err := getDevice(tx, scope, r.subjectID)
				if err != nil {
					return err
				}
				owners[r] = d.CustomerID
			default:
				if !scope.Allows(r.subjectID) {
					return ErrNotFound
				}
				var one int
				if err := tx.QueryRow(`SELECT 1 FROM customers WHERE id = ?`, r.subjectID).Scan(&one); err != nil {
					if errors.Is(err, sql.ErrNoRows) {
						return ErrNotFound
					}
					return err
				}
				owners[r] = r.subjectID
			}
		}
		clause, args := scope.clause(episodeOwner)
		open, err := collect(tx, episodeSelect+` WHERE e.closed_at IS NULL AND `+clause+` ORDER BY e.opened_at, e.id`, args, scanEpisode)
		if err != nil {
			return err
		}
		have := map[episodeRef]bool{}
		for _, e := range open {
			have[e.ref()] = true
		}
		for _, r := range order {
			if have[r] {
				continue
			}
			k := want[r]
			since := at
			if !k.Since.IsZero() && msOf(k.Since) >= 1000000000000 {
				since = msOf(k.Since)
			}
			id := tx.id()
			dev, cust := any(nil), any(nil)
			if r.subjectType == EpisodeDevice {
				dev = r.subjectID
			} else {
				cust = r.subjectID
			}
			if _, err := tx.Exec(`INSERT INTO alarm_episodes (id, subject_type, device_id, customer_id, alarm_type, opened_at, state_since, created_at, updated_at)
				VALUES (?,?,?,?,?,?,?,?,?)`, id, r.subjectType, dev, cust, r.alarm, at, since, at, at); err != nil {
				return err
			}
			out.Opened = append(out.Opened, Episode{ID: id, SubjectType: r.subjectType, SubjectID: r.subjectID, CustomerID: owners[r],
				AlarmType: r.alarm, OpenedAt: timeOf(at), StateSince: timeOf(since)})
		}
		for _, e := range open {
			if _, still := want[e.ref()]; still {
				continue
			}
			if _, err := tx.Exec(`UPDATE alarm_episodes SET closed_at = ?, updated_at = ? WHERE id = ?`, at, at, e.ID); err != nil {
				return err
			}
			c := timeOf(at)
			e.ClosedAt = &c
			out.Closed = append(out.Closed, e)
			n, err := clearAcksOfEpisode(tx, e, at)
			if err != nil {
				return err
			}
			out.ClearedAcks += n
		}
		return nil
	})
	if err != nil {
		return EpisodeSync{}, err
	}
	return out, nil
}

// clearAcksOfEpisode clears the active acknowledgement that covers the closed episode e, if any,
// and audits it as the system.
func clearAcksOfEpisode(tx *Tx, e Episode, at int64) (int, error) {
	kind, ok := AckAlarmType(e.AlarmType)
	if !ok {
		return 0, nil
	}
	col := "device_id"
	if e.SubjectType == EpisodeCustomer {
		col = "customer_id"
	}
	var id string
	switch err := tx.QueryRow(`SELECT id FROM alarm_acks WHERE `+col+` = ? AND alarm_type = ? AND cleared_at IS NULL`, e.SubjectID, kind).Scan(&id); {
	case errors.Is(err, sql.ErrNoRows):
		return 0, nil
	case err != nil:
		return 0, err
	}
	if _, err := tx.Exec(`UPDATE alarm_acks SET cleared_at = ?, updated_at = ? WHERE id = ?`, at, at, id); err != nil {
		return 0, err
	}
	ev := AuditEvent{ActorType: "system", Method: "system", Action: "alarm_ack.clear", ObjectType: "alarm_ack", ObjectID: id, CustomerID: e.CustomerID,
		Details: jsonBytes(map[string]any{"reason": "alarm_resolved", "alarm_type": kind, "episode_id": e.ID})}
	if err := audited(tx, ev); err != nil {
		return 0, err
	}
	return 1, nil
}

func (s *Store) listEpisodes(ctx context.Context, scope Scope, where string) ([]Episode, error) {
	if err := scope.check(); err != nil {
		return nil, err
	}
	clause, args := scope.clause(episodeOwner)
	var out []Episode
	err := s.Read(ctx, func(tx *Tx) error {
		var err error
		out, err = collect(tx, episodeSelect+` WHERE `+where+` AND `+clause+` ORDER BY e.opened_at, e.id`, args, scanEpisode)
		return err
	})
	if out == nil {
		out = []Episode{}
	}
	return out, err
}

// ListOpenEpisodes returns the open episodes inside scope, oldest first.
func (s *Store) ListOpenEpisodes(ctx context.Context, scope Scope) ([]Episode, error) {
	return s.listEpisodes(ctx, scope, `e.closed_at IS NULL`)
}

// ListUnnotifiedEpisodes returns the open episodes whose alert has not gone out, oldest first.
func (s *Store) ListUnnotifiedEpisodes(ctx context.Context, scope Scope) ([]Episode, error) {
	return s.listEpisodes(ctx, scope, `e.closed_at IS NULL AND e.notified_at IS NULL`)
}

// ListRecoveryDueEpisodes returns the closed episodes whose alert went out and whose recovery
// notice has not, oldest first. A closed episode that was never alerted owes none.
func (s *Store) ListRecoveryDueEpisodes(ctx context.Context, scope Scope) ([]Episode, error) {
	return s.listEpisodes(ctx, scope, `e.closed_at IS NOT NULL AND e.notified_at IS NOT NULL AND e.recovery_notified_at IS NULL`)
}

// MarkEpisodesNotified records that the alert for each listed episode went out (or was queued)
// at at (zero = the store clock). An episode already marked keeps its first time. It returns how
// many episodes changed. An id that does not exist, or lies outside scope, is ErrNotFound and
// nothing is changed.
func (s *Store) MarkEpisodesNotified(ctx context.Context, scope Scope, ids []string, at time.Time) (int, error) {
	return s.markEpisodes(ctx, scope, ids, at, "notified_at", false)
}

// MarkRecoveryNotified records that the recovery notice of each listed closed episode went out at
// at (zero = the store clock). An episode that is still open is ErrInvalid; otherwise it behaves
// like MarkEpisodesNotified.
func (s *Store) MarkRecoveryNotified(ctx context.Context, scope Scope, ids []string, at time.Time) (int, error) {
	return s.markEpisodes(ctx, scope, ids, at, "recovery_notified_at", true)
}

func (s *Store) markEpisodes(ctx context.Context, scope Scope, ids []string, at time.Time, column string, needClosed bool) (int, error) {
	if err := scope.check(); err != nil {
		return 0, err
	}
	if at.IsZero() {
		at = s.Now()
	}
	ms := msOf(at)
	ids = uniqueSorted(ids)
	changed := 0
	err := s.Write(ctx, func(tx *Tx) error {
		changed = 0
		clause, args := scope.clause(episodeOwner)
		eps := make([]Episode, 0, len(ids))
		for _, id := range ids {
			e, err := one(tx, episodeSelect+` WHERE e.id = ? AND `+clause, append([]any{id}, args...), scanEpisode)
			if err != nil {
				return err
			}
			if needClosed && e.Open() {
				return invalidf("a recovery notice follows a closed episode")
			}
			eps = append(eps, e)
		}
		for _, e := range eps {
			res, err := tx.Exec(`UPDATE alarm_episodes SET `+column+` = ?, updated_at = ? WHERE id = ? AND `+column+` IS NULL`, ms, ms, e.ID)
			if err != nil {
				return err
			}
			n, err := res.RowsAffected()
			if err != nil {
				return err
			}
			changed += int(n)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return changed, nil
}

func uniqueSorted(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}
