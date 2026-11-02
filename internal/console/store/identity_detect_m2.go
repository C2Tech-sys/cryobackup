package store

import (
	"context"
	"database/sql"
	"errors"
)

// This file is the detection half of identity conflicts (M2-W3B; the resolution
// half is identity_m2.go). A heartbeat carries the hardware fingerprint of the
// machine that sent it (api.HeartbeatRequest.HWFingerprint). A fingerprint that
// differs from the one recorded for the device means a second machine is using
// the device's identity: a cloned VM, a restored disk image. Architecture §10.2
// and §14 ("a mismatch ... sets identity_conflict_at"). Only the mismatch rule is
// implemented; "two simultaneous sessions" is not detected here.
//
// The conflict never blocks the device: the heartbeat that raised it is still
// answered 2xx and later ones are too (the board shows a critical alarm and an
// admin chooses keep or retire). The fingerprint is an opaque hash; it is never
// written to a log or to an audit row.

// FingerprintOutcome says what AgentNoteFingerprint did.
type FingerprintOutcome string

const (
	// FingerprintSame: nothing to do (same fingerprint, none sent, or the device
	// is retired). The hot path: one read, no write.
	FingerprintSame FingerprintOutcome = "same"
	// FingerprintAdopted: the device had no recorded fingerprint (an import, or
	// an identity-conflict "keep") and this one is now recorded.
	FingerprintAdopted FingerprintOutcome = "adopted"
	// FingerprintConflictRaised: the fingerprint differs from the recorded one and
	// identity_conflict_at has just been set.
	FingerprintConflictRaised FingerprintOutcome = "conflict_raised"
	// FingerprintConflictOpen: it differs, and the conflict was already raised.
	FingerprintConflictOpen FingerprintOutcome = "conflict_open"
)

// AgentNoteFingerprint compares the fingerprint a device's heartbeat carried
// with the recorded one. It takes the authenticated device id (never an id from
// a request body) and touches only that device. It is idempotent: the conflict
// is raised once and a repeat changes nothing and writes no audit row.
//
//   - fp == "": the agent did not send one (older agent); nothing is compared.
//   - recorded fingerprint empty: fp is adopted and one "device.identity_adopt"
//     audit row is written. This is also how "keep" takes effect: it clears the
//     recorded fingerprint, so the machine that heartbeats next becomes the
//     device's identity and the conflict does not re-raise for it.
//   - fp differs: identity_conflict_at is set to now and one
//     "device.identity_conflict" audit row (actor system, severity critical,
//     no fingerprint text) is written in the same transaction. A failed audit
//     write refuses the change; the caller logs it and still answers the agent.
//
// A retired device is ignored.
func (s *Store) AgentNoteFingerprint(ctx context.Context, deviceID, fp, sourceIP string) (FingerprintOutcome, error) {
	if fp == "" {
		return FingerprintSame, nil
	}
	var recorded string
	var conflict, retired sql.NullInt64
	if err := s.Read(ctx, func(tx *Tx) error {
		return readFingerprintState(tx, deviceID, &recorded, &conflict, &retired)
	}); err != nil {
		return FingerprintSame, err
	}
	switch {
	case retired.Valid, recorded == fp:
		return FingerprintSame, nil
	case recorded != "" && conflict.Valid:
		return FingerprintConflictOpen, nil
	}
	out := FingerprintSame
	err := s.Write(ctx, func(tx *Tx) error {
		// Re-read inside the write transaction: two heartbeats may have raced here.
		recorded, conflict, retired = "", sql.NullInt64{}, sql.NullInt64{}
		if err := readFingerprintState(tx, deviceID, &recorded, &conflict, &retired); err != nil {
			return err
		}
		var host, cust string
		if err := tx.QueryRow(`SELECT hostname, customer_id FROM devices WHERE id = ?`, deviceID).Scan(&host, &cust); err != nil {
			return err
		}
		now := tx.nowMS()
		switch {
		case retired.Valid, recorded == fp:
			return nil
		case recorded == "":
			if _, err := tx.Exec(`UPDATE devices SET hw_fingerprint = ?, updated_at = ? WHERE id = ?`, fp, now, deviceID); err != nil {
				return err
			}
			out = FingerprintAdopted
			return audited(tx, AuditEvent{ActorType: "system", ActorID: deviceID, ActorLabel: host, Method: "agent", SourceIP: sourceIP,
				Action: "device.identity_adopt", ObjectType: "device", ObjectID: deviceID, CustomerID: cust, Result: "ok", Severity: "info"})
		case conflict.Valid:
			out = FingerprintConflictOpen
			return nil
		}
		if _, err := tx.Exec(`UPDATE devices SET identity_conflict_at = ?, updated_at = ? WHERE id = ?`, now, now, deviceID); err != nil {
			return err
		}
		out = FingerprintConflictRaised
		return audited(tx, AuditEvent{ActorType: "system", ActorID: deviceID, ActorLabel: host, Method: "agent", SourceIP: sourceIP,
			Action: "device.identity_conflict", ObjectType: "device", ObjectID: deviceID, CustomerID: cust, Result: "ok", Severity: "critical",
			Details: []byte(`{"cause":"fingerprint_changed"}`)})
	})
	if err != nil {
		return FingerprintSame, err
	}
	return out, nil
}

func readFingerprintState(tx *Tx, deviceID string, recorded *string, conflict, retired *sql.NullInt64) error {
	var fp sql.NullString
	err := tx.QueryRow(`SELECT hw_fingerprint, identity_conflict_at, retired_at FROM devices WHERE id = ?`, deviceID).Scan(&fp, conflict, retired)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	*recorded = fp.String
	return err
}
