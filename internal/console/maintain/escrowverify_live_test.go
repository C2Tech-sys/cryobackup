package maintain

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// P22-H live (ADR 0030): the escrow verifier's proof with the real restic 0.19.1 and rclone 1.75.1
// against a local repository behind the admin remote. The right key is verified; the wrong key is
// recorded as wrong_password. Skipped unless CRYO_RESTIC and CRYO_RCLONE name the binaries:
//
//	CRYO_RESTIC=<restic> CRYO_RCLONE=<rclone> go test -count=1 -run LiveEscrowVerifier -v ./internal/console/maintain
func TestLiveEscrowVerifier(t *testing.T) {
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

	// Right key: the scheduler's pass proves it (the key's proof was 8 days old, so it is due).
	e.onlyDevice(dID)
	e.provenNowExcept(dID)
	e.setProof(dID, e.clk.Now().Add(-8*24*time.Hour), time.Time{})
	rep, err := e.w.RunOnce(e.ctx)
	if err != nil || rep.EscrowVerified != 1 || rep.EscrowUnverified != 0 {
		t.Fatalf("right key: RunOnce = %+v, %v", rep, err)
	}
	esc := e.escrowOf(dID)
	if esc.VerifiedAt == nil || !esc.VerifiedAt.Equal(e.clk.Now()) || esc.LastVerifyErrorClass != "" {
		t.Fatalf("right key recorded %+v", esc)
	}
	t.Logf("live escrow verifier: right key -> verified (verified_at set, class empty)")

	// Wrong key: a week later the recheck runs cat config with a key that does not open the repository.
	e.keys.pw = "not-the-repository-password"
	e.clk.Advance(8 * 24 * time.Hour)
	e.provenNowExcept(dID)
	rep, err = e.w.RunOnce(e.ctx)
	if err != nil || rep.EscrowUnverified != 1 || rep.EscrowVerified != 0 {
		t.Fatalf("wrong key: RunOnce = %+v, %v", rep, err)
	}
	esc = e.escrowOf(dID)
	if esc.LastVerifyErrorClass != adminpath.CodeWrongPassword || esc.LastVerifyAttemptAt == nil || !esc.LastVerifyAttemptAt.Equal(e.clk.Now()) {
		t.Fatalf("wrong key recorded %+v", esc)
	}
	if !e.escrowFlagged(dID) {
		t.Fatal("a wrong key does not raise ESCROW_UNVERIFIED")
	}
	t.Logf("live escrow verifier: wrong key -> %s recorded, device ESCROW_UNVERIFIED", esc.LastVerifyErrorClass)

	// The same proof, asked for on demand, classifies the same way.
	_, cerr := e.w.CatConfig(e.ctx, dev(dID))
	if v, class, skip := ClassifyEscrowCheck(cerr); skip || v.OK || v.ErrorClass != adminpath.CodeWrongPassword || class != adminpath.CodeWrongPassword {
		t.Fatalf("on-demand classification of the wrong key: %+v %q %v", v, class, skip)
	}
	e.keys.pw = testPassword
	_, cerr = e.w.CatConfig(e.ctx, dev(dID))
	if v, _, _ := ClassifyEscrowCheck(cerr); !v.OK {
		t.Fatalf("on-demand classification of the right key: %+v (%v)", v, cerr)
	}
	t.Logf("live escrow verifier: on-demand classification matches (wrong_password, then verified)")
}
