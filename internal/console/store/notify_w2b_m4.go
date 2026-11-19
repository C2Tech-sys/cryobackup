package store

import (
	"context"
	"time"
)

// UnmarkEpisodesNotified takes back the alert mark of each listed episode that is still open and
// still marked at at (its batch id): notified_at goes back to NULL, in one transaction. The
// notifier's retry job calls it for the episodes a re-rendered alert leaves out (acknowledged,
// snoozed, or of a suspended customer) while no delivery of the batch has been sent: such an
// episode leaves the batch, so it is mailed normally when it becomes mailable again while still
// open, and, if it closes first, it owes no recovery notice (ListRecoveryDueEpisodes needs a
// mark). An episode that is closed, or marked at another time (re-armed and mailed again since), is
// left alone. It returns how many episodes changed. An id that does not exist, or lies outside
// scope, is ErrNotFound and nothing is changed.
func (s *Store) UnmarkEpisodesNotified(ctx context.Context, scope Scope, ids []string, at time.Time) (int, error) {
	if err := scope.check(); err != nil {
		return 0, err
	}
	ms := msOf(at)
	ids = uniqueSorted(ids)
	changed := 0
	err := s.Write(ctx, func(tx *Tx) error {
		changed = 0
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
			changed += int(n)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return changed, nil
}
