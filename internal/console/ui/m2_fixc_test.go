package ui

import (
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/state"
)

// M2-FIX-C (F11): backups continue during an identity conflict (ADR 0011), so
// neither the chip tooltip nor the device banner may tell the operator to
// resolve it "to continue".
func TestFixCIdentityConflictCopyAgreesWithADR0011(t *testing.T) {
	c := fmtCtx{Now: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC), Loc: time.UTC}
	tip := reasonLine(SampleDeviceState(state.StateIdentityConflict, c.Now), c)
	if strings.Contains(tip, "to continue") || !strings.Contains(tip, "Backups continue") {
		t.Errorf("conflict tooltip must say backups continue: %q", tip)
	}
	e := newEnv(t)
	e.login()
	body := e.page("/devices/"+e.dev["FRONTDESK-02"], 200)
	if strings.Contains(body, "to continue") {
		t.Error("device page banner must not say resolve to continue")
	}
	mustContain(t, "banner", body, "Backups continue while this stands.")
}
