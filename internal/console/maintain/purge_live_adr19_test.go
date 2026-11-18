package maintain

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// TestLivePurgeRepoLocalADR19 runs purge_repo with the real rclone against a
// throwaway local directory (an alias remote in a temporary rclone.conf, the
// same arrangement as TestLiveAdminPathLocal; no S3, no network, never the
// operator's rclone configuration). It is skipped unless CRYO_RESTIC and
// CRYO_RCLONE are both set (liveBinaries).
//
// It creates a real restic repository for a retired device and one for an
// active device, plus decoys next to the retired one (a sibling whose name
// starts with the device id, the probe repository, a file at the prefix
// root), purges the retired device, and proves that exactly its directory is
// gone, that everything else is untouched, and that a second purge is success
// with zeros.
func TestLivePurgeRepoLocalADR19(t *testing.T) {
	resticBin, rcloneBin := liveBinaries(t)

	root := t.TempDir()
	conf := filepath.Join(t.TempDir(), "rclone.conf")
	if err := os.WriteFile(conf, []byte("[admin]\ntype = alias\nremote = "+filepath.ToSlash(root)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, sum, err := hashFile(resticBin)
	if err != nil {
		t.Fatal(err)
	}
	e := newTenv(t, nil, func(c *Config) {
		c.ResticProgram, c.ResticSHA256 = resticBin, sum
		c.RcloneProgram, c.RcloneConfig = rcloneBin, conf
		c.Prefix = "fleet"
	})
	retired, active := e.retiredID(), e.esc
	ctx := e.ctx

	for _, id := range []string{retired, active} {
		res, err := e.w.Init(ctx, adminpath.InitRequest{DeviceRequest: dev(id)})
		if err != nil || !res.Created {
			t.Fatalf("Init(%s) = %+v, %v", id, res, err)
		}
	}
	prefixDir := filepath.Join(root, e.cfg.Bucket, "fleet")
	decoys := []string{
		filepath.Join(prefixDir, retired+"-sibling", "keep.txt"),
		filepath.Join(prefixDir, adminpath.ProbeRepoID, "config"),
		filepath.Join(prefixDir, "root-file.txt"),
	}
	for _, p := range decoys {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("decoy\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	target := filepath.Join(prefixDir, retired)
	if _, err := os.Stat(filepath.Join(target, "config")); err != nil {
		t.Fatalf("the retired device's repository was not created where expected: %v", err)
	}

	// A wrong bucket (ticket P19-F1): the device prefix is "not found" there,
	// which must not read as "nothing left". The parent listing finds no
	// <bucket>/<prefix> and the purge stops with a classified error.
	bucket := e.w.cfg.Bucket
	e.w.cfg.Bucket = "no-such-bucket"
	_, err = e.w.PurgeRepo(ctx, purgeReq(retired))
	wantCode(t, err, adminpath.CodeStorageUnreachable, "storage")
	e.w.cfg.Bucket = bucket
	if _, err := os.Stat(filepath.Join(target, "config")); err != nil {
		t.Fatalf("the repository changed under a purge with a wrong bucket: %v", err)
	}

	// The right location without the storage probe repository's directory
	// (the decoy above stands in for it): no positive evidence that this is
	// the fleet's location, so the purge stops the same way and deletes nothing.
	probeDir := filepath.Join(prefixDir, adminpath.ProbeRepoID)
	if err := os.Rename(probeDir, probeDir+"-away"); err != nil {
		t.Fatal(err)
	}
	_, err = e.w.PurgeRepo(ctx, purgeReq(retired))
	wantCode(t, err, adminpath.CodeStorageUnreachable, "storage")
	if err := os.Rename(probeDir+"-away", probeDir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(target, "config")); err != nil {
		t.Fatalf("the repository changed under a purge without the probe directory: %v", err)
	}

	res, err := e.w.PurgeRepo(ctx, purgeReq(retired))
	if err != nil {
		t.Fatalf("PurgeRepo: %v", err)
	}
	if res.ObjectsRemoved == 0 || res.BytesRemoved == 0 || res.RemainingObjects != 0 || res.ErrorClass != "" {
		t.Fatalf("PurgeRepo = %+v, want objects and bytes removed and none left", res)
	}
	t.Logf("live: purge removed %d objects, %d bytes, %d left", res.ObjectsRemoved, res.BytesRemoved, res.RemainingObjects)
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("the purged repository is still there: %v", err)
	}
	for _, p := range decoys {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("decoy %s was removed: %v", filepath.Base(filepath.Dir(p)), err)
		}
	}
	if _, err := e.w.CatConfig(ctx, dev(active)); err != nil {
		t.Errorf("the active device's repository was damaged: %v", err)
	}

	// Idempotent: the prefix is gone, so the second call is success with zeros
	// (and `rclone purge`, which exits 1 on a missing directory, is not run).
	again, err := e.w.PurgeRepo(ctx, purgeReq(retired))
	if err != nil || again.ObjectsRemoved != 0 || again.BytesRemoved != 0 || again.RemainingObjects != 0 {
		t.Fatalf("second PurgeRepo = %+v, %v; want success with zeros", again, err)
	}

	// The active device is refused, and its repository stays.
	if _, err := e.w.PurgeRepo(ctx, purgeReq(active)); err == nil {
		t.Fatal("an active device was purged")
	}
	if _, err := os.Stat(filepath.Join(prefixDir, active, "config")); err != nil {
		t.Errorf("the active device's repository is gone: %v", err)
	}
	var holder *string
	if err := e.st.Read(ctx, func(tx *store.Tx) error {
		return tx.QueryRow(`SELECT lease_holder FROM maintenance_schedule WHERE device_id = ?`, retired).Scan(&holder)
	}); err != nil || holder != nil {
		t.Errorf("lease after the live purge = %v, %v", holder, err)
	}
	e.noLeak(t, nil)
}
