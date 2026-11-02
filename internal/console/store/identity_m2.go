package store

import (
	"context"
	"encoding/json"
)

// This file is the identity-conflict resolution of M2-W2C (architecture §4.2
// row 4, §5.2 POST /devices/{id}/identity/resolve). identity_conflict_at is set
// when a second machine presents a device's identity (a cloned VM, a restored
// disk image); the device is then IDENTITY_CONFLICT, a critical alarm, until an
// admin resolves it.

// ErrNoIdentityConflict: the device has no unresolved identity conflict.
var ErrNoIdentityConflict = conflictf("device has no identity conflict to resolve")

// Resolutions of an identity conflict.
const (
	// ResolveKeep says the identity stays with this device: the second sighting
	// was a legitimate hardware change or a machine that has since been removed.
	// The conflict is cleared and the alarm ends. Nothing is rotated.
	ResolveKeep = "keep"
	// ResolveRetire says the identity can no longer be trusted: the device is
	// retired and its device token and gateway login are revoked, so neither
	// machine can reach the console or the repository. The real machine is
	// enrolled again as a new device. Existing backups follow retention.
	ResolveRetire = "retire"
)

// IdentityResolutions lists the actions ResolveIdentityConflict accepts.
var IdentityResolutions = []string{ResolveKeep, ResolveRetire}

// IdentityResolution is the admin's decision.
type IdentityResolution struct {
	Action string // keep | retire
	// Reason is required and audited: one printable line of at most 500
	// characters, with no file path.
	Reason string
}

// ResolveIdentityConflict resolves the identity conflict of a device inside
// scope and writes one "device.identity_resolve" audit row (severity warning)
// in the same transaction; a failed audit write refuses the change. The device
// must have an unresolved conflict (ErrNoIdentityConflict otherwise) and must
// not be retired. A device outside scope is ErrNotFound. Rejected input is a
// *ValidationError.
func (s *Store) ResolveIdentityConflict(ctx context.Context, scope Scope, deviceID string, in IdentityResolution, actor AuditActor) (Device, error) {
	if err := scope.check(); err != nil {
		return Device{}, err
	}
	var bad []FieldError
	if !oneOf(in.Action, IdentityResolutions) {
		bad = append(bad, fieldf("action", "Use keep or retire."))
	}
	reason, ok := cleanLine(in.Reason, 500)
	switch {
	case reason == "":
		bad = append(bad, fieldf("reason", "A reason is required."))
	case !ok:
		bad = append(bad, fieldf("reason", "The reason must be one line of at most 500 characters."))
	default:
		if b, err := json.Marshal(reason); err != nil || looksLikePath(string(b)) {
			bad = append(bad, fieldf("reason", "Do not put file paths in the reason."))
		}
	}
	if err := validation(bad); err != nil {
		return Device{}, err
	}
	var out Device
	err := s.Write(ctx, func(tx *Tx) error {
		cur, err := getDevice(tx, scope, deviceID)
		if err != nil {
			return err
		}
		if cur.RetiredAt != nil {
			return ErrDeviceRetired
		}
		if cur.IdentityConflictAt == nil {
			return ErrNoIdentityConflict
		}
		now := tx.nowMS()
		switch in.Action {
		case ResolveKeep:
			// The recorded fingerprint is cleared with the flag (M2-W3B): the next
			// heartbeat then records whichever machine is alive as the device's
			// fingerprint (AgentNoteFingerprint), so keeping a legitimate hardware
			// change does not raise the same conflict again.
			_, err = tx.Exec(`UPDATE devices SET identity_conflict_at = NULL, hw_fingerprint = NULL, updated_at = ? WHERE id = ?`, now, deviceID)
		default:
			// A retired device cannot also be parked (schema rule): the pause ends with it.
			_, err = tx.Exec(`UPDATE devices SET identity_conflict_at = NULL, retired_at = ?, retired_reason = ?,
				paused_at = NULL, paused_until = NULL, pause_reason = NULL, paused_by = NULL, updated_at = ? WHERE id = ?`,
				now, "identity conflict", now, deviceID)
			if err == nil {
				_, err = tx.Exec(`UPDATE device_tokens SET revoked_at = ?, revoked_reason = 'device retired', updated_at = ?
					WHERE device_id = ? AND revoked_at IS NULL`, now, now, deviceID)
			}
			if err == nil {
				_, err = tx.Exec(`UPDATE gateway_logins SET revoked_at = ?, pending_secret_wrapped = NULL, updated_at = ?
					WHERE device_id = ? AND revoked_at IS NULL`, now, now, deviceID)
			}
		}
		if err != nil {
			return err
		}
		ev, err := actor.event("device.identity_resolve", "device", deviceID, cur.CustomerID, map[string]any{"resolution": in.Action, "reason": reason})
		if err != nil {
			return err
		}
		ev.Severity = "warning"
		if err := audited(tx, ev); err != nil {
			return err
		}
		out, err = getDevice(tx, scope, deviceID)
		return err
	})
	if err != nil {
		return Device{}, err
	}
	return out, nil
}
