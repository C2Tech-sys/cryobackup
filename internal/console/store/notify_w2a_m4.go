package store

import (
	"context"
	"time"
)

// What the notifier (internal/console/notify, P20-W2A) needs from the store beyond the episode
// and delivery-log methods: the episodes of one batch, the re-raise of a lapsed snooze, and a
// decision not to retry.
//
// A batch is the set of episodes one evaluator pass mailed together. Its id is the time the pass
// marked them (notified_at, or recovery_notified_at for a recovery notice), in epoch
// milliseconds, and the delivery rows of the batch carry it in their dedupe key
// ("alert:<ms>:<rule id>"). The retry job finds the episodes of a row again from that id alone;
// the log itself names no device and no customer.

// ListEpisodesNotifiedAt returns the episodes inside scope whose alert was marked at at (the
// batch id), open or closed, oldest first.
func (s *Store) ListEpisodesNotifiedAt(ctx context.Context, scope Scope, at time.Time) ([]Episode, error) {
	return s.listEpisodesAt(ctx, scope, "e.notified_at", at)
}

// ListEpisodesRecoveryNotifiedAt returns the episodes inside scope whose recovery notice was marked
// at at (the batch id), oldest first.
func (s *Store) ListEpisodesRecoveryNotifiedAt(ctx context.Context, scope Scope, at time.Time) ([]Episode, error) {
	return s.listEpisodesAt(ctx, scope, "e.recovery_notified_at", at)
}

func (s *Store) listEpisodesAt(ctx context.Context, scope Scope, column string, at time.Time) ([]Episode, error) {
	if err := scope.check(); err != nil {
		return nil, err
	}
	clause, args := scope.clause(episodeOwner)
	out := []Episode{}
	err := s.Read(ctx, func(tx *Tx) error {
		rows, err := collect(tx, episodeSelect+` WHERE `+column+` = ? AND `+clause+` ORDER BY e.opened_at, e.id`,
			append([]any{msOf(at)}, args...), scanEpisode)
		out = append(out[:0], rows...)
		return err
	})
	return out, err
}

// ackClearSnoozeEnded is the audit reason of an acknowledgement RearmLapsedSnoozes clears.
const ackClearSnoozeEnded = "snooze_ended"

// RearmLapsedSnoozes raises the alert again for every alarm whose snooze ended while the alarm
// persists (ADR 0020 decision 7). A snooze that lapsed (its time passed, or a backup succeeded
// after a snooze until the next success; ackInForce) and still covers an open episode is cleared
// (one alarm_ack.clear audit row, actor system, reason snooze_ended) and that episode's
// notified_at is reset, in one transaction: the next alert batch mails it once more, exactly
// once, because the cleared row cannot lapse a second time. A plain acknowledgement never
// lapses and is left alone; so is a snooze still in force. It returns how many alarms were
// re-armed. The notifier calls it only when an alert can actually go out (mail configured and a
// recipient with the alerts switch), so an alarm is not re-armed into a queue nobody reads.
func (s *Store) RearmLapsedSnoozes(ctx context.Context, scope Scope) (int, error) {
	if err := scope.check(); err != nil {
		return 0, err
	}
	clause, args := scope.clause(ackOwner)
	n := 0
	err := s.Write(ctx, func(tx *Tx) error {
		n = 0
		now := tx.nowMS()
		rows, err := collect(tx, `SELECT `+ackCols+ackFrom+` WHERE a.cleared_at IS NULL AND `+ackHasOpenEpisode+` AND `+clause+
			` ORDER BY a.acknowledged_at, a.id`, args, scanAck)
		if err != nil {
			return err
		}
		for _, a := range rows {
			active, err := ackInForce(tx, a, now)
			if err != nil {
				return err
			}
			if active {
				continue
			}
			if _, err := tx.Exec(`UPDATE alarm_acks SET cleared_at = ?, updated_at = ? WHERE id = ? AND cleared_at IS NULL`, now, now, a.ID); err != nil {
				return err
			}
			ev := AuditEvent{ActorType: "system", Method: "system", Action: "alarm_ack.clear", ObjectType: "alarm_ack", ObjectID: a.ID,
				CustomerID: a.CustomerID, Details: a.auditDetails(map[string]any{"reason": ackClearSnoozeEnded})}
			if err := audited(tx, ev); err != nil {
				return err
			}
			alarm, _ := EpisodeAlarmOfAck(a.AlarmType) // ackHasOpenEpisode matched, so the type maps to a state
			col := "device_id"
			if a.SubjectType == EpisodeCustomer {
				col = "customer_id"
			}
			if _, err := tx.Exec(`UPDATE alarm_episodes SET notified_at = NULL, updated_at = ? WHERE `+col+` = ? AND alarm_type = ? AND closed_at IS NULL`,
				now, a.SubjectID, alarm); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

// SuppressDelivery records a decision not to send a message that is still waiting (queued, or
// failed with a retry scheduled): result 'suppressed' with the reason, no next retry, sent_at now
// (the time of the decision). The error class of an earlier failure is kept as history. The
// consecutive-failure counter is not touched: no attempt was made. A row that is already
// suppressed is returned unchanged; a final row (delivered, or failed with no retry left) is
// ErrConflict, like MarkDeliveryFailed; an unknown reason is a *ValidationError; an unknown id is
// ErrNotFound. The log is console-wide: scope.All is required.
func (s *Store) SuppressDelivery(ctx context.Context, scope Scope, id, reason string) (Delivery, error) {
	if err := s.needAll(scope); err != nil {
		return Delivery{}, err
	}
	known := false
	for _, r := range deliverySuppressReasons {
		known = known || r == reason
	}
	if !known {
		return Delivery{}, validation([]FieldError{fieldf("suppress_reason", "Use a known suppress reason.")})
	}
	var out Delivery
	err := s.Write(ctx, func(tx *Tx) error {
		cur, err := one(tx, `SELECT `+deliveryCols+` FROM notification_log WHERE id = ?`, []any{id}, scanDelivery)
		if err != nil {
			return err
		}
		switch {
		case cur.Result == DeliverySuppressed:
			out = cur
			return nil
		case cur.Result == DeliverySent:
			return conflictf("the message was already delivered")
		case cur.Result == DeliveryFailed && cur.NextRetryAt == nil:
			return conflictf("the message gave up and accepts no further decision")
		}
		now := tx.nowMS()
		if _, err := tx.Exec(`UPDATE notification_log SET result = 'suppressed', suppress_reason = ?, next_retry_at = NULL, sent_at = ?, updated_at = ? WHERE id = ?`,
			reason, now, now, id); err != nil {
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
