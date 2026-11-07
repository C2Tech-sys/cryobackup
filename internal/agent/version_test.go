package agent

import (
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/agent/internal/fakerestic"
	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/buildinfo"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// M3-HARDEN A3: a path-like build stamp cannot stop a backup. The journal's guard
// refused the run row and restic never started; the console refused the batch. The
// agent enrolls, heartbeats and backs up, and the run carries "unknown". The guards
// themselves still stand for real run data.
func TestAPathLikeBuildStampDoesNotStopABackup(t *testing.T) {
	for name, stamp := range map[string]string{
		"windows":  `C:\builds\agent-1.2.3`,
		"posix":    "/home/builder/agent/1.2.3",
		"unc":      `\\share\builds\agent`,
		"sentence": "built on the build box",
	} {
		t.Run(name, func(t *testing.T) {
			was := buildinfo.Version
			buildinfo.Version = stamp
			t.Cleanup(func() { buildinfo.Version = was })

			fc := newFakeConsole(t)
			fakerestic.Use(t, "backup_ok")
			start(t, fakeOptions(t, enroll(t, fc)))
			waitFor(t, "a heartbeat", 30*time.Second, func() bool { return len(fc.heartbeatList()) >= 1 })
			if got := fc.heartbeatList()[0].AgentVersion; got != api.UnknownVersion {
				t.Errorf("heartbeat agent_version = %q, want %q", got, api.UnknownVersion)
			}
			waitFor(t, "the run to be uploaded", 30*time.Second, func() bool { return len(fc.closedRuns()) >= 1 })
			got := fc.closedRuns()[0]
			if got.Outcome != restic.OutcomeSuccess {
				t.Errorf("outcome = %q, want success", got.Outcome)
			}
			if got.AgentVersion != api.UnknownVersion {
				t.Errorf("run agent_version = %q, want %q", got.AgentVersion, api.UnknownVersion)
			}
		})
	}
}

// A real stamp is left as it is.
func TestARealBuildStampIsReportedUnchanged(t *testing.T) {
	was := buildinfo.Version
	buildinfo.Version = "1.2.3-rc.1"
	t.Cleanup(func() { buildinfo.Version = was })
	if got := versionLabel(); got != "1.2.3-rc.1" {
		t.Fatalf("versionLabel = %q", got)
	}
}
