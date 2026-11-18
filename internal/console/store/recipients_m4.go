package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/mail"
	"strings"
	"time"
)

// Mail recipients (M4, ADR 0020 decision 3). A recipient is a global notification rule: an e-mail
// address plus what it receives. Per-customer and per-device rules are M11 (row r20); this file
// reads and writes rows with scope 'global' and channel 'email' only, and every other row of
// notification_rules is invisible to it (ErrNotFound by id).
//
// The three switches map onto the existing columns of notification_rules:
//
//	Alerts      triggers (JSON array of strings) contains "alert"; other entries are kept as they are
//	Recoveries  notify_recovery = 1
//	Digest      digest_schedule = 'daily' (the time of day is the digest_local_time setting); NULL = no digest
//
// name is the operator's label and destination the address. Rows are written with
// min_severity 'critical' (immediate mail is for critical states only; the digest ignores the
// column), config '{}', no nag interval, failure_threshold 1 and enabled 1. A row that someone
// disabled outside the console (enabled = 0) reads with all three switches off, so a disabled
// rule can never receive mail through code that trusts the switches; saving it from here enables
// it again.
//
// The settings are console-wide: every method needs scope.All (ErrScope otherwise). Each
// mutation writes one audit row in the same transaction with ids and switches only, never the
// address or the name (the audit trail is kept for years; an address is personal data).

const (
	maxRecipientName    = 100
	maxMailAddress      = 254 // RFC 5321 path limit
	maxMailLocalPart    = 64
	triggerAlert        = "alert"
	digestScheduleDaily = "daily"
)

// Recipient is one mail recipient.
type Recipient struct {
	ID      string
	Name    string
	Address string
	// Alerts: immediate mail when an alarm opens. Recoveries: one notice when it ends. Digest: the daily digest.
	Alerts, Recoveries, Digest bool
	CreatedAt, UpdatedAt       time.Time
}

// RecipientInput is a recipient's editable fields.
type RecipientInput struct {
	Name    string
	Address string
	// Alerts, Recoveries and Digest are the three switches.
	Alerts, Recoveries, Digest bool
}

// CleanMailAddress trims s and reports whether it is exactly one plain address: no display name,
// no angle brackets, comment or quoting, no space, control character or CR or LF, ASCII only,
// at most 254 characters and a local part of at most 64. It is the check recipients and the test
// mail use, so an address that passes here cannot inject a header or a second recipient.
func CleanMailAddress(s string) (string, bool) {
	if strings.ContainsAny(s, "\r\n") {
		return s, false // never trimmed away: a line break in an address is an attempt, not a typo
	}
	s = strings.TrimSpace(s)
	if s == "" || len(s) > maxMailAddress {
		return s, false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; c <= 0x20 || c >= 0x7f {
			return s, false
		}
	}
	a, err := mail.ParseAddress(s)
	if err != nil || a.Name != "" || a.Address != s {
		return s, false
	}
	local, domain, found := strings.Cut(s, "@")
	if !found || local == "" || domain == "" || len(local) > maxMailLocalPart || strings.Contains(domain, "@") {
		return s, false
	}
	return s, true
}

func (in RecipientInput) clean() (RecipientInput, error) {
	var bad []FieldError
	name, ok := cleanLine(in.Name, maxRecipientName)
	if !ok || name == "" {
		bad = append(bad, fieldf("name", "Give a name of 1 to %d characters on one line.", maxRecipientName))
	}
	addr, ok := CleanMailAddress(in.Address)
	if !ok {
		bad = append(bad, fieldf("address", "Use one plain e-mail address such as name@example.com, without a display name."))
	}
	if err := validation(bad); err != nil {
		return in, err
	}
	in.Name, in.Address = name, addr
	return in, nil
}

const recipientCols = `id, name, destination, triggers, notify_recovery, digest_schedule, enabled, created_at, updated_at`

const recipientWhere = `scope = 'global' AND channel = 'email'`

func scanRecipient(sc scanner) (Recipient, error) {
	var r Recipient
	var triggers string
	var recov, enabled int
	var digest sql.NullString
	var created, updated int64
	if err := sc.Scan(&r.ID, &r.Name, &r.Address, &triggers, &recov, &digest, &enabled, &created, &updated); err != nil {
		return Recipient{}, err
	}
	r.CreatedAt, r.UpdatedAt = timeOf(created), timeOf(updated)
	if enabled == 0 {
		return r, nil
	}
	var names []string
	_ = json.Unmarshal([]byte(triggers), &names) // a row that does not parse has no triggers: no alerts, said by the switch
	for _, n := range names {
		if n == triggerAlert {
			r.Alerts = true
		}
	}
	r.Recoveries = recov != 0
	r.Digest = digest.Valid && digest.String != ""
	return r, nil
}

// triggersWith returns the stored triggers with "alert" present or absent, other entries kept.
func triggersWith(stored string, alerts bool) string {
	var names []string
	_ = json.Unmarshal([]byte(stored), &names)
	out := make([]string, 0, len(names)+1)
	for _, n := range names {
		if n != triggerAlert {
			out = append(out, n)
		}
	}
	if alerts {
		out = append(out, triggerAlert)
	}
	return string(jsonBytes(out))
}

func recipientDetails(r Recipient) json.RawMessage {
	return jsonBytes(map[string]any{"alerts": r.Alerts, "recoveries": r.Recoveries, "digest": r.Digest})
}

func (s *Store) needAll(scope Scope) error {
	if err := scope.check(); err != nil {
		return err
	}
	if !scope.All {
		return ErrScope
	}
	return nil
}

// duplicateAddress reports whether another global e-mail rule (not exceptID) has the address,
// compared without regard to case.
func duplicateAddress(tx *Tx, addr, exceptID string) (bool, error) {
	var n int
	err := tx.QueryRow(`SELECT count(*) FROM notification_rules WHERE `+recipientWhere+` AND destination = ? COLLATE NOCASE AND id <> ?`, addr, exceptID).Scan(&n)
	return n > 0, err
}

// CreateRecipient adds a recipient. An address that another recipient already has is ErrConflict;
// an invalid name or address is a *ValidationError.
func (s *Store) CreateRecipient(ctx context.Context, scope Scope, in RecipientInput, actor AuditActor) (Recipient, error) {
	if err := s.needAll(scope); err != nil {
		return Recipient{}, err
	}
	in, err := in.clean()
	if err != nil {
		return Recipient{}, err
	}
	var out Recipient
	err = s.Write(ctx, func(tx *Tx) error {
		if dup, err := duplicateAddress(tx, in.Address, ""); err != nil {
			return err
		} else if dup {
			return conflictf("a recipient with this address already exists")
		}
		now := tx.nowMS()
		id := tx.id()
		var digest any
		if in.Digest {
			digest = digestScheduleDaily
		}
		if _, err := tx.Exec(`INSERT INTO notification_rules (id, scope, name, channel, destination, config, triggers, min_severity, digest_schedule,
			notify_recovery, enabled, created_at, updated_at) VALUES (?, 'global', ?, 'email', ?, '{}', ?, 'critical', ?, ?, 1, ?, ?)`,
			id, in.Name, in.Address, triggersWith("[]", in.Alerts), digest, boolInt(in.Recoveries), now, now); err != nil {
			return err
		}
		out, err = one(tx, `SELECT `+recipientCols+` FROM notification_rules WHERE id = ?`, []any{id}, scanRecipient)
		if err != nil {
			return err
		}
		ev, err := actor.event("recipient.create", "notification_rule", id, "", nil)
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

// GetRecipient returns one recipient; ErrNotFound when no global e-mail rule has the id.
func (s *Store) GetRecipient(ctx context.Context, scope Scope, id string) (Recipient, error) {
	if err := s.needAll(scope); err != nil {
		return Recipient{}, err
	}
	var out Recipient
	err := s.Read(ctx, func(tx *Tx) error {
		var err error
		out, err = one(tx, `SELECT `+recipientCols+` FROM notification_rules WHERE id = ? AND `+recipientWhere, []any{id}, scanRecipient)
		return err
	})
	return out, err
}

// ListRecipients returns every recipient, by name.
func (s *Store) ListRecipients(ctx context.Context, scope Scope) ([]Recipient, error) {
	if err := s.needAll(scope); err != nil {
		return nil, err
	}
	out := []Recipient{}
	err := s.Read(ctx, func(tx *Tx) error {
		rows, err := collect(tx, `SELECT `+recipientCols+` FROM notification_rules WHERE `+recipientWhere+` ORDER BY name COLLATE NOCASE, id`, nil, scanRecipient)
		out = append(out, rows...)
		return err
	})
	return out, err
}

// UpdateRecipient replaces a recipient's name, address and switches. Saving what is already stored
// writes nothing and no audit row. ErrNotFound for an unknown id, ErrConflict for an address
// another recipient has.
func (s *Store) UpdateRecipient(ctx context.Context, scope Scope, id string, in RecipientInput, actor AuditActor) (Recipient, error) {
	if err := s.needAll(scope); err != nil {
		return Recipient{}, err
	}
	in, err := in.clean()
	if err != nil {
		return Recipient{}, err
	}
	var out Recipient
	err = s.Write(ctx, func(tx *Tx) error {
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

// DeleteRecipient removes a recipient. Its delivery-log rows stay (the log outlives the rule).
// ErrNotFound for an unknown id.
func (s *Store) DeleteRecipient(ctx context.Context, scope Scope, id string, actor AuditActor) error {
	if err := s.needAll(scope); err != nil {
		return err
	}
	return s.Write(ctx, func(tx *Tx) error {
		cur, err := one(tx, `SELECT `+recipientCols+` FROM notification_rules WHERE id = ? AND `+recipientWhere, []any{id}, scanRecipient)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM notification_rules WHERE id = ?`, id); err != nil {
			return err
		}
		ev, err := actor.event("recipient.delete", "notification_rule", id, "", nil)
		if err != nil {
			return err
		}
		ev.Details = recipientDetails(cur)
		return audited(tx, ev)
	})
}
