package store

import (
	"context"
)

// The health-prober's reads (architecture §4.6, §5.7). None of them returns a
// customer's data: an id, a key generation or a count.

// ActiveEscrowKeyIDs returns the distinct master_key_id values of the active
// escrowed repository keys (banner C12: a loaded master key that is not the one
// some keys were wrapped with).
func (s *Store) ActiveEscrowKeyIDs(ctx context.Context) ([]string, error) {
	var out []string
	err := s.Read(ctx, func(tx *Tx) error {
		var err error
		out, err = collect(tx, `SELECT DISTINCT master_key_id FROM repo_key_escrow
			WHERE retired_at IS NULL AND custody = 'escrowed' AND master_key_id IS NOT NULL ORDER BY master_key_id`, nil,
			func(sc scanner) (string, error) {
				var id string
				return id, sc.Scan(&id)
			})
		return err
	})
	return out, err
}
