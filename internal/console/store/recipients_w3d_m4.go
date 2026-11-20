package store

import (
	"context"
	"database/sql"
	"errors"
)

// A recipient edit (PATCH /api/v1/notification-rules/{id}) names some fields and leaves the rest.
// PatchRecipient reads the row, applies the named fields and writes it back in one write
// transaction, so two edits at the same moment that name different fields both stand: the second
// reads what the first committed. (Reading the row in the handler and writing every field back
// with UpdateRecipient lost one of two simultaneous edits.)

// RecipientPatch is a recipient edit: a nil field keeps its stored value.
type RecipientPatch struct {
	Name    *string
	Address *string
	// Alerts, Recoveries and Digest are the three switches (see recipients_m4.go).
	Alerts, Recoveries, Digest *bool
}

// apply returns cur with the fields p names replaced.
func (p RecipientPatch) apply(cur Recipient) RecipientInput {
	in := RecipientInput{Name: cur.Name, Address: cur.Address, Alerts: cur.Alerts, Recoveries: cur.Recoveries, Digest: cur.Digest}
	if p.Name != nil {
		in.Name = *p.Name
	}
	if p.Address != nil {
		in.Address = *p.Address
	}
	if p.Alerts != nil {
		in.Alerts = *p.Alerts
	}
	if p.Recoveries != nil {
		in.Recoveries = *p.Recoveries
	}
	if p.Digest != nil {
		in.Digest = *p.Digest
	}
	return in
}

// PatchRecipient applies the fields p names to a recipient, read and written in one transaction,
// and returns the row as stored. An edit that changes nothing writes nothing and no audit row (as
// UpdateRecipient). A row disabled outside the console reads with its switches off (scanRecipient)
// and is enabled again by any edit that changes it. ErrNotFound for an unknown id, ErrConflict for
// an address another recipient has, a *ValidationError for an invalid name or address (including a
// stored one the edit leaves as it is).
func (s *Store) PatchRecipient(ctx context.Context, scope Scope, id string, p RecipientPatch, actor AuditActor) (Recipient, error) {
	if err := s.needAll(scope); err != nil {
		return Recipient{}, err
	}
	var out Recipient
	err := s.Write(ctx, func(tx *Tx) error {
		var triggers string
		var enabled int
		if err := tx.QueryRow(`SELECT triggers, enabled FROM notification_rules WHERE id = ? AND `+recipientWhere, id).Scan(&triggers, &enabled); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		cur, err := one(tx, `SELECT `+recipientCols+` FROM notification_rules WHERE id = ?`, []any{id}, scanRecipient)
		if err != nil {
			return err
		}
		in, err := p.apply(cur).clean()
		if err != nil {
			return err
		}
		if enabled != 0 && cur.Name == in.Name && cur.Address == in.Address && cur.Alerts == in.Alerts && cur.Recoveries == in.Recoveries && cur.Digest == in.Digest {
			out = cur
			return nil
		}
		if dup, err := duplicateAddress(tx, in.Address, id); err != nil {
			return err
		} else if dup {
			return conflictf("a recipient with this address already exists")
		}
		var digest any
		if in.Digest {
			digest = digestScheduleDaily
		}
		if _, err := tx.Exec(`UPDATE notification_rules SET name = ?, destination = ?, triggers = ?, digest_schedule = ?, notify_recovery = ?, enabled = 1, updated_at = ? WHERE id = ?`,
			in.Name, in.Address, triggersWith(triggers, in.Alerts), digest, boolInt(in.Recoveries), tx.nowMS(), id); err != nil {
			return err
		}
		out, err = one(tx, `SELECT `+recipientCols+` FROM notification_rules WHERE id = ?`, []any{id}, scanRecipient)
		if err != nil {
			return err
		}
		ev, err := actor.event("recipient.update", "notification_rule", id, "", nil)
		if err != nil {
			return err
		}
		ev.Details = recipientDetails(out)
		return audited(tx, ev)
	})
	if err != nil {
		return Recipient{}, err
	}
	return out, nil
}
