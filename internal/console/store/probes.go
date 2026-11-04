package store

import (
	"context"
	"database/sql"
	"errors"
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

// NextStorageProbeDevice returns the device whose repository the storage probe
// (banner C02) reads next: the smallest id greater than after among non-retired
// devices with an active escrowed key, wrapping around to the smallest. It
// returns "" when no such device exists (nothing to probe yet). Rotating spreads
// the probe over the fleet's repositories instead of hammering one.
func (s *Store) NextStorageProbeDevice(ctx context.Context, after string) (string, error) {
	const q = `SELECT d.id FROM devices d JOIN repo_key_escrow e ON e.device_id = d.id AND e.retired_at IS NULL AND e.custody = 'escrowed'
		WHERE d.retired_at IS NULL AND d.id > ? ORDER BY d.id LIMIT 1`
	var id string
	err := s.Read(ctx, func(tx *Tx) error {
		err := tx.QueryRow(q, after).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) && after != "" {
			err = tx.QueryRow(q, "").Scan(&id)
		}
		if errors.Is(err, sql.ErrNoRows) {
			id = ""
			return nil
		}
		return err
	})
	return id, err
}
