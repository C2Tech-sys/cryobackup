package store

import (
	"context"
	"strconv"
	"time"
)

// LeaveAlertBatch takes the listed episodes out of the alert batch at (their notified_at), in one
// transaction that also reads the batch's delivery rows (dedupe key "alert:<ms of at>:"), so the
// decision and the change cannot be separated by another send. retried is the id of the delivery
// row being re-rendered, whose message leaves the episodes out. Each episode, as it stands at that
// moment:
//
//   - still open (it is muted): its alert mark is taken back (notified_at back to NULL), so it is
//     mailed normally when it becomes mailable again while still open; only when every OTHER row
//     of the batch is final without having been sent (failed for good, or suppressed). While a
//     sibling row is sent, queued or waiting for a retry, that row may still carry the episode, so
//     it stays in the batch and the batch's recovery rule applies to it;
//   - closed meanwhile (also between the caller's read and this call): its recovery is settled with
//     no message (recovery_notified_at = settledAt), when no row of the batch has been sent: no
//     retry renders a closed episode, so no message can name it any more.
//
// An episode no longer marked at at (re-armed and mailed again since), or whose recovery is
// already settled, is left alone. It returns how many episodes were released and how many settled.
// An id that does not exist, or lies outside scope, is ErrNotFound and nothing is changed.
func (s *Store) LeaveAlertBatch(ctx context.Context, scope Scope, ids []string, at, settledAt time.Time, retried string) (released, settled int, err error) {
	if err := scope.check(); err != nil {
		return 0, 0, err
	}
	ms, settleMS := msOf(at), msOf(settledAt)
	ids = uniqueSorted(ids)
	err = s.Write(ctx, func(tx *Tx) error {
		released, settled = 0, 0
		clause, args := scope.clause(episodeOwner)
		for _, id := range ids {
			if _, err := one(tx, episodeSelect+` WHERE e.id = ? AND `+clause, append([]any{id}, args...), scanEpisode); err != nil {
				return err
			}
		}
		prefix := "alert:" + strconv.FormatInt(ms, 10) + ":"
		var sent, othersOpen int
		if err := tx.QueryRow(`SELECT
				COALESCE(SUM(result = 'sent'), 0),
				COALESCE(SUM(id <> ? AND NOT (result = 'suppressed' OR (result = 'failed' AND next_retry_at IS NULL))), 0)
			FROM notification_log WHERE substr(dedupe_key, 1, ?) = ?`, retried, len(prefix), prefix).Scan(&sent, &othersOpen); err != nil {
			return err
		}
		release, settle := sent == 0 && othersOpen == 0, sent == 0
		now := tx.nowMS()
		for _, id := range ids {
			if release {
				res, err := tx.Exec(`UPDATE alarm_episodes SET notified_at = NULL, updated_at = ? WHERE id = ? AND notified_at = ? AND closed_at IS NULL`, now, id, ms)
				if err != nil {
					return err
				}
				n, err := res.RowsAffected()
				if err != nil {
					return err
				}
				released += int(n)
			}
			if !settle {
				continue
			}
			res, err := tx.Exec(`UPDATE alarm_episodes SET recovery_notified_at = ?, updated_at = ?
				WHERE id = ? AND notified_at = ? AND closed_at IS NOT NULL AND recovery_notified_at IS NULL`, settleMS, now, id, ms)
			if err != nil {
				return err
			}
			n, err := res.RowsAffected()
			if err != nil {
				return err
			}
			settled += int(n)
		}
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	return released, settled, nil
}
