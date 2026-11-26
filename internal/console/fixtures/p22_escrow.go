package fixtures

import (
	"context"
	"errors"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// P22-I: the repository-key states of the device page's escrow panel (ADR 0030). The fictional
// fleet already holds most of them: every device is verified 5 days ago, TECH-BENCH-07 never,
// LEGAL-PC-01 is customer-held, OLD-RECEPTION-PC is retired. SeedP22Escrow moves four more devices
// into the states the panel words (a proof past the weekly cadence, a failed check of each class, a
// check that reached no verdict) and re-times the others, so the panel, its tests and its
// screenshots have one device per state. Nothing here is a real name or key.

// P22EscrowState is one panel state of the fictional fleet.
type P22EscrowState struct {
	// Key names the state in the tests and the screenshot slugs.
	Key string
	// Title says what the panel shows.
	Title string
	// Host is the device that shows it.
	Host string
	// Verified and Attempt are how long ago the key was last proven and last checked (zero: never).
	Verified, Attempt time.Duration
	// ErrorClass is the recorded failure class ("" for none).
	ErrorClass string
	// Held, Unconfirmed and Purging mark the states the fleet itself is in (a customer-held key, an
	// enrollment never confirmed, a device being purged): SeedP22Escrow writes nothing for them.
	// Retired marks a retired device, whose key is re-timed like any other.
	Held, Unconfirmed, Purging, Retired bool
}

// P22EscrowStates lists the panel states SeedP22Escrow writes.
func P22EscrowStates() []P22EscrowState {
	h, d := time.Hour, 24*time.Hour
	return []P22EscrowState{
		{Key: "verified", Title: "Escrowed, verified 2 days ago", Host: "FRONTDESK-01", Verified: 2 * d, Attempt: 2 * d},
		{Key: "stale", Title: "Escrowed, last proof 10 days old (past the weekly check)", Host: "PRINT-SRV-01", Verified: 10 * d, Attempt: 3 * h},
		{Key: "failed", Title: "Escrowed, the last check failed: wrong password", Host: "LT-BILLING-1", Verified: 12 * d, Attempt: 40 * time.Minute, ErrorClass: "wrong_password"},
		{Key: "failed-repo", Title: "Escrowed, the last check failed: repository missing", Host: "WAITING-RM-PC", Verified: 3 * d, Attempt: 2 * h, ErrorClass: "repo_missing"},
		{Key: "never", Title: "Escrowed, never verified", Host: "TECH-BENCH-07", Attempt: 5 * h},
		{Key: "held", Title: "Customer-held key", Host: "LEGAL-PC-01", Held: true},
		{Key: "unconfirmed", Title: "Enrollment never confirmed: no repository yet", Host: "KIOSK-WAIT-01", Unconfirmed: true},
		{Key: "purging", Title: "Retired and being purged: the key is no longer checked", Host: "SPARE-PC-09", Purging: true, Retired: true},
		{Key: "retired", Title: "Retired device, key verified 3 days ago", Host: "OLD-RECEPTION-PC", Verified: 3 * d, Attempt: 3 * d, Retired: true},
	}
}

// P22EscrowStateByKey returns the state with key k.
func P22EscrowStateByKey(k string) (P22EscrowState, bool) {
	for _, s := range P22EscrowStates() {
		if s.Key == k {
			return s, true
		}
	}
	return P22EscrowState{}, false
}

// SeedP22Escrow writes the states of P22EscrowStates into a database LoadFixtures filled. A
// state the fleet is already in (customer-held, unconfirmed, purging) writes nothing; every other
// state sets the active escrow row's proof, last attempt and failure class, times relative to now.
func SeedP22Escrow(ctx context.Context, st *store.Store, now time.Time) error {
	now = now.UTC()
	devs := map[string]string{}
	l, err := st.ListDevices(ctx, store.ScopeAll(), store.ListDevicesOptions{IncludeRetired: true, Page: store.Page{Limit: 500}})
	if err != nil {
		return err
	}
	for _, d := range l.Items {
		devs[d.Hostname] = d.ID
	}
	return st.Write(ctx, func(tx *store.Tx) error {
		for _, s := range P22EscrowStates() {
			id, ok := devs[s.Host]
			if !ok {
				return errors.New("fixtures: no device " + s.Host)
			}
			if s.Held || s.Unconfirmed || s.Purging {
				continue
			}
			ago := func(d time.Duration) any {
				if d == 0 {
					return nil
				}
				return now.Add(-d).UnixMilli()
			}
			var class any
			if s.ErrorClass != "" {
				class = s.ErrorClass
			}
			res, err := tx.Exec(`UPDATE repo_key_escrow SET verified_at = ?, last_verify_attempt_at = ?, last_verify_error_class = ?
				WHERE device_id = ? AND retired_at IS NULL AND custody = 'escrowed'`, ago(s.Verified), ago(s.Attempt), class, id)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return errors.New("fixtures: " + s.Host + " has no active escrowed key")
			}
		}
		return nil
	})
}
