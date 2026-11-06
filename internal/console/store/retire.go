package store

import (
	"context"
	"encoding/json"
	"errors"
)

// Device retire (M3-T6, ADR 0016; architecture §5.2 POST /devices/{id}/retire,
// §7.2 "revoke", DH10). Retiring ends a device's identity for good:
//
//   - every device token and the gateway login are revoked in the one transaction
//     that sets retired_at, so there is no moment at which the device reads RETIRED
//     and can still authenticate;
//   - a pause ends with it (the schema refuses retired and paused together);
//   - commands still queued are cancelled: nothing can collect them any more;
//   - nothing is deleted. The repository, its snapshots, the escrowed key, the run
//     history and the audit trail all stay. Purging is the separate owner action of
//     architecture §5.2 DELETE /devices/{id}.
//
// The htpasswd file is the caller's half: it signals the htpasswd-sync job after
// the commit (adminapi.Service.SyncGateway), and ListGatewayLoginsForHtpasswd has
// already stopped listing the login.

// maxRetireReason bounds the reason of a retire (it is kept on the device row and
// in the audit details).
const maxRetireReason = 500

// RetireRequest is what an admin gives when retiring a device.
type RetireRequest struct {
	Reason string
	// ReplacedBy, when set, is the device that takes over (F08): inside scope, the
	// same customer, not retired, and not this device.
	ReplacedBy string
}

// RetireResult is the outcome of RetireDeviceAudited.
type RetireResult struct {
	Device Device
	// Changed is false when the device was already retired: nothing was written,
	// no audit row exists for this call, and the counts below are zero.
	Changed           bool
	TokensRevoked     int64
	LoginsRevoked     int64
	CommandsCancelled int64
}

// ValidateRetireReason checks the reason of a retire: required, one line, no
// file paths. It returns the cleaned reason.
func ValidateRetireReason(raw string) (string, []FieldError) {
	reason, ok := cleanLine(raw, maxRetireReason)
	switch {
	case reason == "":
		return "", []FieldError{fieldf("reason", "A reason is required.")}
	case !ok:
		return "", []FieldError{fieldf("reason", "The reason must be one line of at most %d characters.", maxRetireReason)}
	}
	if b, err := json.Marshal(reason); err != nil || looksLikePath(string(b)) {
		return "", []FieldError{fieldf("reason", "Do not put file paths in the reason.")}
	}
	return reason, nil
}

// RetireDeviceAudited retires a device inside scope and writes one "device.retire"
// audit row (severity warning) in the same transaction; a failed audit write
// refuses the change and the credentials stay as they were. A device outside
// scope is ErrNotFound. Retiring a retired device changes nothing, answers the
// state it is in with Changed false, and writes no second audit row: the first
// retire's reason and replacement stand. Rejected input is a *ValidationError; a
// ReplacedBy the caller cannot see reads exactly like one of another customer.
func (s *Store) RetireDeviceAudited(ctx context.Context, scope Scope, deviceID string, req RetireRequest, actor AuditActor) (RetireResult, error) {
	if err := scope.check(); err != nil {
		return RetireResult{}, err
	}
	reason, bad := ValidateRetireReason(req.Reason)
	if err := validation(bad); err != nil {
		return RetireResult{}, err
	}
	var out RetireResult
	err := s.Write(ctx, func(tx *Tx) error {
		cur, err := getDevice(tx, scope, deviceID)
		if err != nil {
			return err
		}
		if cur.RetiredAt != nil {
			out = RetireResult{Device: cur}
			return nil
		}
		if err := checkReplacement(tx, scope, cur, req.ReplacedBy); err != nil {
			return err
		}
		now := tx.nowMS()
		if _, err := tx.Exec(`UPDATE devices SET retired_at = ?, retired_reason = ?, replaced_by_device_id = ?,
			paused_at = NULL, paused_until = NULL, pause_reason = NULL, paused_by = NULL, updated_at = ? WHERE id = ?`,
			now, reason, nullStr(req.ReplacedBy), now, deviceID); err != nil {
			return err
		}
		res := RetireResult{Changed: true}
		affected := func(into *int64, q string, args ...any) error {
			r, err := tx.Exec(q, args...)
			if err != nil {
				return err
			}
			*into, err = r.RowsAffected()
			return err
		}
		if err := affected(&res.TokensRevoked, `UPDATE device_tokens SET revoked_at = ?, revoked_reason = 'device retired', updated_at = ?
			WHERE device_id = ? AND revoked_at IS NULL`, now, now, deviceID); err != nil {
			return err
		}
		if err := affected(&res.LoginsRevoked, `UPDATE gateway_logins SET revoked_at = ?, pending_secret_wrapped = NULL, updated_at = ?
			WHERE device_id = ? AND revoked_at IS NULL`, now, now, deviceID); err != nil {
			return err
		}
		if err := affected(&res.CommandsCancelled, `UPDATE commands SET status = 'cancelled', completed_at = ?, updated_at = ?
			WHERE device_id = ? AND status = 'queued'`, now, now, deviceID); err != nil {
			return err
		}
		details := map[string]any{"reason": reason, "tokens_revoked": res.TokensRevoked, "logins_revoked": res.LoginsRevoked,
			"commands_cancelled": res.CommandsCancelled, "was_parked": cur.PausedAt != nil}
		if req.ReplacedBy != "" {
			details["replaced_by"] = req.ReplacedBy
		}
		ev, err := actor.event("device.retire", "device", deviceID, cur.CustomerID, details)
		if err != nil {
			return err
		}
		ev.Severity = "warning"
		if err := audited(tx, ev); err != nil {
			return err
		}
		res.Device, err = getDevice(tx, scope, deviceID)
		out = res
		return err
	})
	if err != nil {
		return RetireResult{}, err
	}
	return out, nil
}

// checkReplacement holds a retire's replacement to §3.5: a device inside scope,
// of the same customer, itself active, and not the device being retired. An
// empty id is no replacement. The message is the same for a device outside scope
// and one of another customer, so the form cannot probe what exists.
func checkReplacement(tx *Tx, scope Scope, cur Device, replacedBy string) error {
	if replacedBy == "" {
		return nil
	}
	refuse := func(msg string) error { return validation([]FieldError{fieldf("replaced_by", "%s", msg)}) }
	if replacedBy == cur.ID {
		return refuse("A device cannot replace itself.")
	}
	repl, err := getDevice(tx, scope, replacedBy)
	if errors.Is(err, ErrNotFound) || (err == nil && repl.CustomerID != cur.CustomerID) {
		return refuse("Choose an active device of the same customer.")
	}
	if err != nil {
		return err
	}
	if repl.RetiredAt != nil {
		return refuse("The replacement device is itself retired.")
	}
	return nil
}
