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

// DeleteSetting removes one settings row inside the caller's write transaction
// (the caller writes its audit row in the same transaction). It reports whether
// a row was there. It is a Tx method, not a Store one: global configuration,
// used by `cryo-console probe-repo reset` (ADR 0017).
func (tx *Tx) DeleteSetting(key string) (bool, error) {
	res, err := tx.Exec(`DELETE FROM settings WHERE key = ?`, key)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}
