package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

// Digest settings (M4, ADR 0020 decision 8). digest_local_time and digest_enabled are ordinary
// settings (settings_m2.go: validated, audited, changed through SetSettingsAudited); this file
// reads them in the form the daily-digest job and the C14 banner need.

// ValidLocalTime reports whether s is exactly HH:MM on a 24-hour clock with two digits each
// ("07:00", "23:59"): the form digest_local_time is stored in and internal/state parses.
func ValidLocalTime(s string) bool {
	if len(s) != 5 || s[2] != ':' {
		return false
	}
	h, m := int(s[0]-'0')*10+int(s[1]-'0'), int(s[3]-'0')*10+int(s[4]-'0')
	for _, i := range []int{0, 1, 3, 4} {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return h <= 23 && m <= 59
}

// DigestSettings is the digest's schedule as stored.
type DigestSettings struct {
	// Enabled is digest_enabled; true when the row is absent (the digest is on by default).
	Enabled bool
	// LocalTime is digest_local_time as stored, "" when absent or not a string. It is not
	// validated here: a stored value that is not HH:MM reaches internal/state as it is, and C14b says so.
	LocalTime string
}

// ReadDigestSettings returns the stored digest settings. A digest_enabled row that is not true or
// false is ErrInvalid (fail loud: the caller reports the digest as not checked); the settings are
// console-wide, so the caller needs scope.All.
func (s *Store) ReadDigestSettings(ctx context.Context, scope Scope) (DigestSettings, error) {
	if err := s.needAll(scope); err != nil {
		return DigestSettings{}, err
	}
	out := DigestSettings{Enabled: true}
	err := s.Read(ctx, func(tx *Tx) error {
		var raw string
		switch err := tx.QueryRow(`SELECT value FROM settings WHERE key = ?`, SettingDigestEnabled).Scan(&raw); {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return err
		default:
			switch strings.TrimSpace(raw) {
			case "true":
			case "false":
				out.Enabled = false
			default:
				return invalidf("digest_enabled is not true or false")
			}
		}
		switch err := tx.QueryRow(`SELECT value FROM settings WHERE key = ?`, SettingDigestLocalTime).Scan(&raw); {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return err
		default:
			var v string
			if json.Unmarshal([]byte(raw), &v) == nil {
				out.LocalTime = strings.TrimSpace(v)
			}
		}
		return nil
	})
	return out, err
}
