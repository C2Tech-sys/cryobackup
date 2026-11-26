package maintain

import (
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// P22-R2b B1 live: the escrow check reads the config with the real restic 0.19.1 and writes nothing to
// the repository. After init, the repository's locks/ folder is made unwritable (an ACL deny on
// Windows, mode 0555 elsewhere): a restic that tried to create its lock object would fail on it, as
// restic cat config without --no-lock does (r1b, restic 0.19.1). The scheduler's escrow phase, the on-demand
// check and enrollment's config read must all succeed and leave locks/ empty. Skipped unless CRYO_RESTIC
// and CRYO_RCLONE name the binaries:
//
//	CRYO_RESTIC=<restic> CRYO_RCLONE=<rclone> go test -count=1 -run LiveEscrowCheckWritesNoLock -v ./internal/console/maintain
func TestLiveEscrowCheckWritesNoLock(t *testing.T) {
	resticBin, rcloneBin := liveBinaries(t)
	remote := t.TempDir()
	conf := filepath.Join(t.TempDir(), "rclone.conf")
	if err := os.WriteFile(conf, []byte("[admin]\ntype = alias\nremote = "+filepath.ToSlash(remote)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, sum, err := hashFile(resticBin)
	if err != nil {
		t.Fatal(err)
	}
	e := newTenv(t, nil, func(c *Config) {
		c.ResticProgram, c.ResticSHA256 = resticBin, sum
		c.RcloneProgram, c.RcloneConfig = rcloneBin, conf
	})
	dID := e.esc
	if res, err := e.w.Init(e.ctx, adminpath.InitRequest{DeviceRequest: dev(dID)}); err != nil || !res.Created || res.Outcome != restic.OutcomeSuccess {
		t.Fatalf("Init = %+v, %v", res, err)
	}
	var locks string
	_ = filepath.WalkDir(remote, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() && d.Name() == "locks" {
			locks = p
			return filepath.SkipAll
		}
		return nil
	})
	if locks == "" {
		t.Fatal("the repository has no locks folder")
	}
	undo := denyWrites(t, locks)
	t.Cleanup(undo)
	// The control: the denial holds, so a lock write would fail here.
	if err := os.WriteFile(filepath.Join(locks, "probe"), []byte("x"), 0o600); err == nil {
		_ = os.Remove(filepath.Join(locks, "probe"))
		t.Skip("locks/ could not be made unwritable for this account (running as an administrator?)")
	}

	e.onlyDevice(dID)
	e.provenNowExcept(dID)
	e.setProof(dID, e.clk.Now().Add(-8*24*time.Hour), time.Time{})
	// The scheduler's escrow phase alone (a whole pass also runs usage and maintenance steps, which do
	// take locks and would wait on the denied folder).
	var rep CycleReport
	e.w.verifyEscrows(e.ctx, &rep)
	if rep.EscrowVerified != 1 || rep.EscrowUnverified != 0 {
		t.Fatalf("scheduler check against a repository whose locks/ is unwritable: %+v", rep)
	}
	if _, err := e.w.CatConfig(e.ctx, dev(dID)); err != nil {
		t.Fatalf("on-demand check against a repository whose locks/ is unwritable: %v", err)
	}
	undo()
	entries, err := os.ReadDir(locks)
	if err != nil || len(entries) != 0 {
		t.Fatalf("locks/ holds %d objects after the checks (%v)", len(entries), err)
	}
	t.Logf("live escrow check: scheduler and on-demand cat config succeed with locks/ write-denied; locks/ stays empty")
}

// denyWrites makes dir refuse new files and folders for this account and returns the undo.
func denyWrites(t *testing.T, dir string) func() {
	t.Helper()
	if runtime.GOOS == "windows" {
		u, err := user.Current()
		if err != nil {
			t.Skipf("no current user: %v", err)
		}
		if out, err := exec.Command("icacls", dir, "/deny", u.Username+":(WD,AD)").CombinedOutput(); err != nil {
			t.Skipf("icacls /deny: %v: %s", err, out)
		}
		return func() { _ = exec.Command("icacls", dir, "/remove:d", u.Username).Run() }
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Skipf("chmod: %v", err)
	}
	return func() { _ = os.Chmod(dir, 0o755) }
}
