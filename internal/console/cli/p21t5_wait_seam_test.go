package cli

import (
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminapi"
)

// P21-T5 fix batch 1 (S1, S3): BuildOptions.GatewaySyncWait reaches both the admin API and the pages'
// service (which copies it into its in-process API: pinned in the ui package), and unset leaves both on
// the default, which serve relies on.
func TestBuildOptionsGatewaySyncWaitReachesAdminAndUI(t *testing.T) {
	set, _ := buildFromInitWith(t, "America/Chicago", "America/Chicago", BuildOptions{GatewaySyncWait: 41 * time.Second})
	if set.Admin.GatewaySyncWait != 41*time.Second || set.UI.GatewaySyncWait != 41*time.Second {
		t.Errorf("set: admin %v, ui %v, want 41s for both", set.Admin.GatewaySyncWait, set.UI.GatewaySyncWait)
	}
	unset, _ := buildFromInit(t, "America/Chicago", "America/Chicago")
	if unset.Admin.GatewaySyncWait != 0 || unset.UI.GatewaySyncWait != 0 {
		t.Errorf("unset: admin %v, ui %v, want 0 (adminapi.DefaultGatewaySyncWait = %v)", unset.Admin.GatewaySyncWait, unset.UI.GatewaySyncWait, adminapi.DefaultGatewaySyncWait)
	}
}
