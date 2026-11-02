package ui

import (
	"context"
	"strings"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// M2-W3B: an override with no admin (the enrollment token's policy, or a row
// whose author is unknown) never renders an empty name.
func TestW3BOverrideWithoutAnAdminNamesItsSource(t *testing.T) {
	e := newEnv(t)
	e.signInAs("ad", "admin", nil)
	dev := e.dev["FRONTDESK-01"]
	tab := "/devices/" + dev + "/policy"
	put := func(id, reason string) {
		t.Helper()
		err := e.st.Write(context.Background(), func(tx *store.Tx) error {
			if _, err := tx.Exec(`UPDATE device_policy_overrides SET cleared_at = 1700000000000 WHERE device_id = ? AND cleared_at IS NULL`, dev); err != nil {
				return err
			}
			_, err := tx.Exec(`INSERT INTO device_policy_overrides (id, device_id, patch, reason, created_at, updated_at)
				VALUES (?, ?, '{"sla":{"stale_after_hours":36}}', ?, 1700000000000, 1700000000000)`, id, dev, reason)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	put("01J9Y4K3A0B1C2D3E4F5G6H7A1", "Enrollment token policy: Slow first backup")
	page := e.page(tab, 200)
	mustContain(t, "token override", page, "This device has an override", "Enrollment token policy: Slow first backup", "set by the enrollment token", "this device&#39;s override (from the enrollment token)")
	if strings.Contains(page, "set by  on") || strings.Contains(page, "a removed account") {
		t.Error("the token override banner has an empty or wrong author")
	}

	put("01J9Y4K3A0B1C2D3E4F5G6H7A2", "written by a script")
	page = e.page(tab, 200)
	mustContain(t, "other override without an admin", page, "set by a removed account")
	if strings.Contains(page, "from the enrollment token") || strings.Contains(page, "set by  on") {
		t.Error("a non-token override is labelled as the token's, or has an empty author")
	}
}
