package store

import (
	"context"
	"time"
)

// LeaveAlertBatch takes the listed episodes out of the alert batch at (their notified_at), in one
// transaction, whichever way each stands at that moment:
//
//   - still open: its alert mark is taken back (notified_at back to NULL), so it is mailed
//     normally when it becomes mailable again while still open;
//   - closed meanwhile (also between the caller's read and this call): its recovery is settled
//     with no message (recovery_notified_at = settledAt), since it was in no delivered alert.
//
// The notifier's retry job calls it for the episodes a re-rendered alert leaves out while no
// delivery of the batch has been sent. An episode no longer marked at at (re-armed and mailed
// again since), or whose recovery is already settled, is left alone. It returns how many episodes
// were released and how many settled. An id that does not exist, or lies outside scope, is
// ErrNotFound and nothing is changed.
func (s *Store) LeaveAlertBatch(ctx context.Context, scope Scope, ids []string, at, settledAt time.Time) (released, settled int, err error) {
	if err := scope.check(); err != nil {
		return 0, 0, err
	}
	ms, settle := msOf(at), msOf(settledAt)
	ids = uniqueSorted(ids)
	err = s.Write(ctx, func(tx *Tx) error {
		released, settled = 0, 0
		clause, args := scope.clause(episodeOwner)
		for _, id := range ids {
			if _, err := one(tx, episodeSelect+` WHERE e.id = ? AND `+clause, append([]any{id}, args...), scanEpisode); err != nil {
				return err
			}
		}
		now := tx.nowMS()
		for _, id := range ids {
			res, err := tx.Exec(`UPDATE alarm_episodes SET notified_at = NULL, updated_at = ? WHERE id = ? AND notified_at = ? AND closed_at IS NULL`, now, id, ms)
			if err != nil {
				return err
			}
			n, err := res.RowsAffected()
			if err != nil {
				return err
			}
			released += int(n)
			res, err = tx.Exec(`UPDATE alarm_episodes SET recovery_notified_at = ?, updated_at = ?
				WHERE id = ? AND notified_at = ? AND closed_at IS NOT NULL AND recovery_notified_at IS NULL`, settle, now, id, ms)
			if err != nil {
				return err
			}
			if n, err = res.RowsAffected(); err != nil {
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
