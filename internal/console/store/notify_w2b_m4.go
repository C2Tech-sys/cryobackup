package store

import (
	"context"
	"strconv"
	"time"
)

// SettleClosedLeftOut settles, with no message, the recovery of the listed episodes of the alert
// batch at (their notified_at) that are closed: the episodes a retried alert row (retried, its
// delivery id) leaves out because they closed. A retried alert carries every episode of its batch
// that is still open, so closing is the only reason one is left out, and closing is permanent.
//
// It reads the batch's delivery rows (dedupe key "alert:<ms of at>:") in the same transaction and
// settles nothing when another row of the batch is sent, or is queued and never attempted (a send
// still in flight from the pass that formed the batch, which rendered these episodes): then the
// episodes stay as they are and the batch's recovery rule decides. A sibling that failed with a
// retry scheduled cannot carry them (its retry leaves them out too) and does not count. An episode
// that is open, no longer marked at at, or already settled is left alone. It returns how many
// episodes were settled. An id that does not exist, or lies outside scope, is ErrNotFound and
// nothing is changed.
func (s *Store) SettleClosedLeftOut(ctx context.Context, scope Scope, ids []string, at, settledAt time.Time, retried string) (int, error) {
	if err := scope.check(); err != nil {
		return 0, err
	}
	ms, settleMS := msOf(at), msOf(settledAt)
	ids = uniqueSorted(ids)
	settled := 0
	err := s.Write(ctx, func(tx *Tx) error {
		settled = 0
		clause, args := scope.clause(episodeOwner)
		for _, id := range ids {
			if _, err := one(tx, episodeSelect+` WHERE e.id = ? AND `+clause, append([]any{id}, args...), scanEpisode); err != nil {
				return err
			}
		}
		prefix := "alert:" + strconv.FormatInt(ms, 10) + ":"
		var carriers int
		if err := tx.QueryRow(`SELECT count(*) FROM notification_log
			WHERE substr(dedupe_key, 1, ?) = ? AND id <> ? AND result IN ('sent', 'queued')`, len(prefix), prefix, retried).Scan(&carriers); err != nil {
			return err
		}
		if carriers > 0 {
			return nil
		}
		now := tx.nowMS()
		for _, id := range ids {
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
		return 0, err
	}
	return settled, nil
}
