package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/state"
)

// Acknowledgements and snoozes (M4, ADR 0020 decision 7; matrix row r163). An acknowledgement
// belongs to one subject (a device or a customer) and one alarm type, carries a note, and is
// either plain (in force until cleared), a snooze until a time, or a snooze until the next
// success. It mutes mail and marks the line as acknowledged; it never changes a state, and
// alarm_acks stays "the only persisted alarm datum" of the schema.
//
// An acknowledgement is in force ("active") until it is cleared, its snooze time passes, or, for
// a device snoozed until the next success, a backup succeeds after it. A snooze that ends while
// the alarm persists therefore stops muting, and the alert goes out again.
//
// The schema requires an alarm type on every row (alarm_type is NOT NULL), so there is no "covers
// every alarm" acknowledgement: an operator who wants one names each type.

// MaxSnooze bounds a dated snooze, like MaxPauseSpan: longer is a plain acknowledgement in disguise.
const MaxSnooze = 366 * 24 * time.Hour

const maxAckNote = 500

// ackAlarmTypes are the values of alarm_acks.alarm_type (the schema's CHECK), in display order.
var ackAlarmTypes = []string{"blank", "stuck", "failed", "partial", "unseen", "stale", "first_backup_overdue", "missing_customer",
	"maintenance", "escrow", "identity", "quota", "outdated", "unknown_state"}

// AckAlarmTypes returns the alarm types an acknowledgement can name for the subject
// (EpisodeDevice or EpisodeCustomer); nil for another subject.
func AckAlarmTypes(subject string) []string {
	if subject != EpisodeDevice && subject != EpisodeCustomer {
		return nil
	}
	var out []string
	for _, t := range ackAlarmTypes {
		if (t == "missing_customer" || t == "quota") == (subject == EpisodeCustomer) {
			out = append(out, t)
		}
	}
	return out
}

func validAckType(subject, t string) bool {
	for _, a := range AckAlarmTypes(subject) {
		if a == t {
			return true
		}
	}
	return false
}

// stateAckType maps a state name (the alarm_type of an episode, or a state.State) to the
// alarm_type an acknowledgement uses for it. first_backup_overdue and quota are named by the
// schema but no state maps to them yet, so an acknowledgement of those types mutes nothing.
var stateAckType = map[string]string{
	string(state.StateBlank): "blank", string(state.StateStuck): "stuck", string(state.StateFailed): "failed",
	string(state.StatePartial): "partial", string(state.StateUnseen): "unseen", string(state.StateStale): "stale",
	AlarmMissing: "missing_customer", string(state.StateMaintFailed): "maintenance",
	string(state.StateEscrowUnverified): "escrow", string(state.StateIdentityConflict): "identity",
	string(state.StateOutdated): "outdated", string(state.StateUnknown): "unknown_state",
}

// AckAlarmType returns the acknowledgement alarm type that covers the state (or episode alarm)
// named stateName, and false for a state that has none (OK, PAUSED, DEFERRED, ...).
func AckAlarmType(stateName string) (string, bool) {
	t, ok := stateAckType[stateName]
	return t, ok
}

// Ack is one acknowledgement or snooze.
type Ack struct {
	ID          string
	SubjectType string // EpisodeDevice or EpisodeCustomer
	SubjectID   string // the device id or the customer id
	CustomerID  string // the owning customer
	AlarmType   string // an AckAlarmTypes value
	// AcknowledgedBy is the admin id; empty when that account was deleted.
	AcknowledgedBy string
	AcknowledgedAt time.Time
	Note           string
	// SnoozeUntil is the end of a dated snooze; nil for a plain acknowledgement or a snooze until the next success.
	SnoozeUntil      *time.Time
	UntilNextSuccess bool
	ClearedAt        *time.Time
}

// AckInput is the request to acknowledge or snooze one alarm.
type AckInput struct {
	SubjectType string
	SubjectID   string
	AlarmType   string
	// Note is free text of at most 500 characters. It is stored with the row and never copied to the audit trail.
	Note string
	// SnoozeUntil snoozes until that time (it must be in the future and within MaxSnooze); UntilNextSuccess snoozes
	// until the next successful backup (device) or until the alarm ends (customer). They exclude each other;
	// neither set is a plain acknowledgement.
	SnoozeUntil      *time.Time
	UntilNextSuccess bool
}

const ackCols = `a.id, a.subject_type, a.device_id, a.customer_id, COALESCE(a.customer_id, d.customer_id), a.alarm_type, a.acknowledged_by,
	a.acknowledged_at, a.note, a.snooze_until, a.snooze_until_next_success, a.cleared_at`

const ackFrom = ` FROM alarm_acks a LEFT JOIN devices d ON d.id = a.device_id`

const ackOwner = `COALESCE(a.customer_id, d.customer_id)`

func scanAck(sc scanner) (Ack, error) {
	var a Ack
	var dev, cust, owner, by, note sql.NullString
	var at int64
	var snooze, cleared sql.NullInt64
	var next int
	if err := sc.Scan(&a.ID, &a.SubjectType, &dev, &cust, &owner, &a.AlarmType, &by, &at, &note, &snooze, &next, &cleared); err != nil {
		return Ack{}, err
	}
	a.SubjectID = dev.String
	if a.SubjectType == EpisodeCustomer {
		a.SubjectID = cust.String
	}
	a.CustomerID, a.AcknowledgedBy, a.Note = owner.String, by.String, note.String
	a.AcknowledgedAt, a.SnoozeUntil, a.UntilNextSuccess, a.ClearedAt = timeOf(at), ptrTime(snooze), next != 0, ptrTime(cleared)
	return a, nil
}

func (a Ack) auditDetails(extra map[string]any) json.RawMessage {
	d := map[string]any{"subject_type": a.SubjectType, "alarm_type": a.AlarmType, "until_next_success": a.UntilNextSuccess}
	if a.SubjectType == EpisodeDevice {
		d["device_id"] = a.SubjectID
	} else {
		d["customer_id"] = a.SubjectID
	}
	if a.SnoozeUntil != nil {
		d["snooze_until_ms"] = a.SnoozeUntil.UnixMilli()
	}
	for k, v := range extra {
		d[k] = v
	}
	return jsonBytes(d)
}

// CreateAck acknowledges or snoozes one alarm of a device or customer inside scope. An active
// acknowledgement of the same subject and alarm type is replaced (cleared, then the new one
// created, in one transaction) and the audit row says so. A subject outside scope or one that
// does not exist is ErrNotFound; an alarm type that does not fit the subject, a snooze that is
// not in the future or lasts longer than MaxSnooze, both snooze forms together, or a note that is
// too long is a *ValidationError. One alarm_ack.create audit row is written in the same
// transaction (ids and switches; never the note); a change whose audit row cannot be written is
// refused (ErrAudit).
func (s *Store) CreateAck(ctx context.Context, scope Scope, in AckInput, actor AuditActor) (Ack, error) {
	if err := scope.check(); err != nil {
		return Ack{}, err
	}
	var bad []FieldError
	switch in.SubjectType {
	case EpisodeDevice, EpisodeCustomer:
		if !validAckType(in.SubjectType, in.AlarmType) {
			bad = append(bad, fieldf("alarm_type", "Choose an alarm type that applies to a %s.", in.SubjectType))
		}
	default:
		bad = append(bad, fieldf("subject_type", "Use device or customer."))
	}
	if in.SubjectID == "" || len(in.SubjectID) > 40 {
		bad = append(bad, fieldf("subject_id", "Name the device or customer."))
	}
	note, ok := cleanText(in.Note, maxAckNote)
	if !ok {
		bad = append(bad, fieldf("note", "Use at most %d characters of plain text.", maxAckNote))
	}
	if in.SnoozeUntil != nil && in.UntilNextSuccess {
		bad = append(bad, fieldf("snooze_until", "Choose a snooze date or snooze until the next success, not both."))
	}
	if err := validation(bad); err != nil {
		return Ack{}, err
	}
	var out Ack
	err := s.Write(ctx, func(tx *Tx) error {
		now := tx.nowMS()
		customerID := in.SubjectID
		if in.SubjectType == EpisodeDevice {
			d, err := getDevice(tx, scope, in.SubjectID)
			if err != nil {
				return err
			}
			customerID = d.CustomerID
		} else {
			if !scope.Allows(in.SubjectID) {
				return ErrNotFound
			}
			var one int
			if err := tx.QueryRow(`SELECT 1 FROM customers WHERE id = ?`, in.SubjectID).Scan(&one); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return ErrNotFound
				}
				return err
			}
		}
		var snooze any
		if in.SnoozeUntil != nil {
			u := in.SnoozeUntil.UnixMilli()
			switch {
			case u <= now:
				return validation([]FieldError{fieldf("snooze_until", "The snooze must end in the future.")})
			case u > now+MaxSnooze.Milliseconds():
				return validation([]FieldError{fieldf("snooze_until", "A snooze can last at most a year; acknowledge without a date instead.")})
			}
			snooze = u
		}
		col := "device_id"
		if in.SubjectType == EpisodeCustomer {
			col = "customer_id"
		}
		res, err := tx.Exec(`UPDATE alarm_acks SET cleared_at = ?, updated_at = ? WHERE `+col+` = ? AND alarm_type = ? AND cleared_at IS NULL`,
			now, now, in.SubjectID, in.AlarmType)
		if err != nil {
			return err
		}
		replaced, err := res.RowsAffected()
		if err != nil {
			return err
		}
		id := tx.id()
		dev, cust := any(nil), any(nil)
		if in.SubjectType == EpisodeDevice {
			dev = in.SubjectID
		} else {
			cust = in.SubjectID
		}
		if _, err := tx.Exec(`INSERT INTO alarm_acks (id, subject_type, device_id, customer_id, alarm_type, acknowledged_by, acknowledged_at, note,
			snooze_until, snooze_until_next_success, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
			id, in.SubjectType, dev, cust, in.AlarmType, nullStr(actor.AdminID), now, nullStr(note), snooze, boolInt(in.UntilNextSuccess), now, now); err != nil {
			return err
		}
		out, err = one(tx, `SELECT `+ackCols+ackFrom+` WHERE a.id = ?`, []any{id}, scanAck)
		if err != nil {
			return err
		}
		ev, err := actor.event("alarm_ack.create", "alarm_ack", id, customerID, nil)
		if err != nil {
			return err
		}
		ev.Details = out.auditDetails(map[string]any{"replaced": replaced > 0})
		return audited(tx, ev)
	})
	if err != nil {
		return Ack{}, err
	}
	return out, nil
}

// ClearAck ends an acknowledgement or snooze inside scope. An acknowledgement that does not exist
// or lies outside scope is ErrNotFound; one that is already cleared is returned unchanged and
// writes no audit row. One alarm_ack.clear audit row is written in the same transaction.
func (s *Store) ClearAck(ctx context.Context, scope Scope, id string, actor AuditActor) (Ack, error) {
	if err := scope.check(); err != nil {
		return Ack{}, err
	}
	clause, args := scope.clause(ackOwner)
	var out Ack
	err := s.Write(ctx, func(tx *Tx) error {
		a, err := one(tx, `SELECT `+ackCols+ackFrom+` WHERE a.id = ? AND `+clause, append([]any{id}, args...), scanAck)
		if err != nil {
			return err
		}
		out = a
		if a.ClearedAt != nil {
			return nil
		}
		now := tx.nowMS()
		if _, err := tx.Exec(`UPDATE alarm_acks SET cleared_at = ?, updated_at = ? WHERE id = ?`, now, now, id); err != nil {
			return err
		}
		c := timeOf(now)
		out.ClearedAt = &c
		ev, err := actor.event("alarm_ack.clear", "alarm_ack", id, a.CustomerID, nil)
		if err != nil {
			return err
		}
		ev.Details = a.auditDetails(map[string]any{"reason": "operator"})
		return audited(tx, ev)
	})
	if err != nil {
		return Ack{}, err
	}
	return out, nil
}

// ListActiveAcks returns the acknowledgements and snoozes in force inside scope, newest first:
// not cleared, not past their snooze time, and (for a device snoozed until the next success) with
// no successful backup since. Lapsed ones are not listed; the evaluator reads that as "the alert
// is due again".
func (s *Store) ListActiveAcks(ctx context.Context, scope Scope) ([]Ack, error) {
	if err := scope.check(); err != nil {
		return nil, err
	}
	clause, args := scope.clause(ackOwner)
	out := []Ack{}
	err := s.Read(ctx, func(tx *Tx) error {
		now := tx.nowMS()
		rows, err := collect(tx, `SELECT `+ackCols+ackFrom+` WHERE a.cleared_at IS NULL AND `+clause+` ORDER BY a.acknowledged_at DESC, a.id DESC`, args, scanAck)
		if err != nil {
			return err
		}
		out = out[:0]
		for _, a := range rows {
			active, err := ackInForce(tx, a, now)
			if err != nil {
				return err
			}
			if active {
				out = append(out, a)
			}
		}
		return nil
	})
	return out, err
}

// ackInForce applies the three ways an uncleared acknowledgement stops muting. It mirrors the
// state engine's ackView (internal/state), which reads the same rows when it renders a device.
func ackInForce(tx *Tx, a Ack, now int64) (bool, error) {
	if a.SnoozeUntil == nil && !a.UntilNextSuccess {
		return true, nil // plain: in force until cleared
	}
	if a.SnoozeUntil != nil && a.SnoozeUntil.UnixMilli() > now {
		return true, nil
	}
	if !a.UntilNextSuccess {
		return false, nil // a dated snooze that ended
	}
	if a.SubjectType == EpisodeCustomer {
		return true, nil // ends when the alarm does (SyncEpisodes clears it)
	}
	ackedAt := msOf(a.AcknowledgedAt)
	var base, latest sql.NullInt64
	if err := tx.QueryRow(`SELECT MIN(finished_at, received_at) FROM runs INDEXED BY idx_runs_last_success
		WHERE device_id = ? AND kind = 'backup' AND outcome = 'success' AND finished_at <= ? ORDER BY finished_at DESC LIMIT 1`, a.SubjectID, ackedAt).Scan(&base); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if err := tx.QueryRow(lastSuccessSQL, a.SubjectID).Scan(&latest); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	// No success at all, or none newer than the one at acknowledgement time: still snoozed.
	return !latest.Valid || latest.Int64 <= base.Int64, nil
}
