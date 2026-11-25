package maintain

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/escrow"
	"github.com/c2tech-sys/cryobackup/internal/logx"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// The bare-host side of the self-backup (ADR 0029, ticket P22-F): list the
// self-backup repository's snapshots and restore one onto a host that has no
// database and no running console, holding only the operator's offline kit
// (the admin rclone file and the master key). `cryo-console self-backup list`
// and `... restore` call this; nothing here opens a database, reads the
// environment or starts a worker.
//
// The restore:
//
//  1. refuses an --out folder that is not empty (or is a symlink, or a file);
//  2. derives the repository password from the master key exactly as the
//     worker does (SelfBackupPasswordFrom, HKDF info SelfBackupKeyInfo);
//  3. lists the snapshots of host "console" and takes the newest, or the one
//     named (a unique prefix of its id);
//  4. restores "<id>:<staging path>" into --out, so console.db, config.json and
//     manifest.json land at its root (spike P22-S1 item 9);
//  5. checks a copy of what it restored exactly as the weekly verify does
//     (Worker.selfCompare: sha256 and size, PRAGMA integrity_check, schema
//     version, row counts, the master key's id, one escrowed key unwrapped),
//     so the restored console.db itself is never opened and stays byte-equal
//     to the backed-up copy.
//
// Any failure after the folder was touched empties it again (and removes it
// when the restore created it): a wrong master key, an unreachable storage or a
// copy that does not match its manifest leaves nothing behind. restic and
// rclone keep their temp files in a 0700 folder beside --out (on the same
// volume, which has room for the copy), removed on every way out. Errors carry
// path-free classes only; no secret is ever returned or logged.

// Classes of the bare-host side, beside the worker's (SelfClass*).
const (
	SelfClassConfig            = "config"             // a required option is missing or malformed
	SelfClassOutNotEmpty       = "out_not_empty"      // --out exists and is not empty
	SelfClassOutDir            = "out_dir"            // --out is a file or a symlink, or cannot be created
	SelfClassResticBinary      = "restic_binary"      // restic is not found, or its sha256 is not the pinned one
	SelfClassResticVersion     = "restic_version"     // restic does not report PinnedResticVersion
	SelfClassSnapshotUnknown   = "snapshot_unknown"   // no snapshot of host console has that id
	SelfClassSnapshotAmbiguous = "snapshot_ambiguous" // the id prefix names more than one snapshot
)

// selfRestoreAfterRestore is a test seam: called with the --out folder just
// after `restic restore` exited 0 (the fake restic writes no files). Nil in
// production.
var selfRestoreAfterRestore func(out string)

// SelfRepo addresses the self-backup repository with the offline kit alone.
type SelfRepo struct {
	// ResticProgram is the restic binary; it must report PinnedResticVersion.
	// ResticSHA256, when set, must be its sha256.
	ResticProgram, ResticSHA256 string
	// RcloneProgram is the rclone restic's rclone: backend starts.
	RcloneProgram string
	// RcloneConfig is the admin rclone file of the offline kit, and Remote the
	// name of its remote (the worker's CRYO_MAINTAIN_REMOTE, usually admin).
	RcloneConfig, Remote string
	// Bucket and Prefix locate the repository: the worker's
	// CRYO_MAINTAIN_BUCKET and CRYO_MAINTAIN_SELF_PREFIX.
	Bucket, Prefix string
	// MasterKey is the console master key of the offline kit; the repository
	// password is derived from it and never leaves this process.
	MasterKey *escrow.MasterKey
}

// SelfRestoreError is a failure of ListSelfSnapshots or RestoreSelfBackup: a
// path-free class, and whether it is the caller's configuration or command
// line (Usage) rather than the backup, the storage or the restore.
type SelfRestoreError struct {
	Class string
	Usage bool
}

func (e *SelfRestoreError) Error() string { return "maintain: self-backup: " + e.Class }

func usageErr(class string) error  { return &SelfRestoreError{Class: class, Usage: true} }
func failedErr(class string) error { return &SelfRestoreError{Class: class} }

// SelfRestoreResult is what RestoreSelfBackup restored and checked.
type SelfRestoreResult struct {
	// Snapshot is the restored snapshot (id, time, paths as restic lists them).
	Snapshot restic.Snapshot
	// Manifest is manifest.json of the snapshot; the restored console.db
	// matched it.
	Manifest SelfBackupManifest
	// ConfigIncluded: config.json was restored beside the database.
	ConfigIncluded bool
	// EscrowChecked: an escrowed key of the copy was unwrapped with the master
	// key; false when the copy has no escrowed device.
	EscrowChecked bool
}

// selfSession is one use of the repository: the runner, the environment with
// the derived password, and the 0700 temp folder.
type selfSession struct {
	run      *runner
	env      restic.Env
	base     []string
	common   restic.Common
	tmp      string // the session folder: tmp/ (restic, rclone temp), rclone/ (rclone cache), check/
	timeouts Timeouts
}

// openSelfRepo checks the options and the restic binary and prepares a session
// whose temp folder is created under parent ("" for the system temp folder).
func openSelfRepo(ctx context.Context, r SelfRepo, parent string) (*selfSession, error) {
	switch {
	case r.ResticProgram == "", r.RcloneProgram == "", r.RcloneConfig == "", r.Remote == "", r.Bucket == "", strings.Trim(r.Prefix, "/") == "":
		return nil, usageErr(SelfClassConfig)
	case r.MasterKey == nil || !r.MasterKey.Loaded():
		return nil, usageErr(SelfClassNoKey)
	}
	// The children run in the session folder, so every path they are given is
	// absolute.
	rcloneConfig, err := filepath.Abs(r.RcloneConfig)
	if err != nil {
		return nil, usageErr(SelfClassConfig)
	}
	if fi, err := os.Stat(rcloneConfig); err != nil || !fi.Mode().IsRegular() {
		return nil, usageErr(SelfClassConfig)
	}
	rcloneProgram := r.RcloneProgram
	if strings.ContainsAny(rcloneProgram, `/\`) {
		if rcloneProgram, err = filepath.Abs(rcloneProgram); err != nil {
			return nil, usageErr(SelfClassConfig)
		}
	}
	// restic splits rclone.program like a shell command line, as for the
	// worker (Config.withDefaults): forward slashes, no whitespace or quotes.
	rcloneProgram = filepath.ToSlash(rcloneProgram)
	if strings.ContainsAny(rcloneProgram, " \t\r\n\"'\\") {
		return nil, usageErr(SelfClassConfig)
	}
	pw, err := SelfBackupPasswordFrom(r.MasterKey.Bytes())
	if err != nil {
		return nil, usageErr(SelfClassNoKey)
	}
	parentDir, last := path.Split(strings.Trim(r.Prefix, "/"))
	repo, err := restic.AdminRepository(r.Remote, r.Bucket, strings.Trim(parentDir, "/"), last)
	if err != nil {
		return nil, usageErr(SelfClassConfig)
	}
	resolved, err := exec.LookPath(r.ResticProgram)
	if err == nil {
		resolved, err = filepath.Abs(resolved)
	}
	if err != nil {
		return nil, usageErr(SelfClassResticBinary)
	}
	if r.ResticSHA256 != "" {
		if _, got, err := hashFile(resolved); err != nil || got != strings.ToLower(r.ResticSHA256) {
			return nil, usageErr(SelfClassResticBinary)
		}
	}
	tmp, err := os.MkdirTemp(parent, ".cryo-self-backup-")
	if err != nil {
		return nil, usageErr(SelfClassOutDir)
	}
	s := &selfSession{tmp: tmp, timeouts: Timeouts{}.withDefaults()}
	for _, d := range []string{"tmp", "rclone", "check"} {
		if err := os.Mkdir(filepath.Join(tmp, d), 0o700); err != nil {
			s.close()
			return nil, failedErr(SelfClassStaging)
		}
	}
	_ = os.Chmod(tmp, 0o700)
	s.run = &runner{program: resolved, dir: filepath.Join(tmp, "tmp")}
	s.base = []string{"PATH=" + os.Getenv("PATH")}
	if runtime.GOOS == "windows" {
		if v := os.Getenv("SystemRoot"); v != "" {
			s.base = append(s.base, "SystemRoot="+v)
		}
	}
	s.base = append(s.base, "RCLONE_CONFIG="+rcloneConfig, "TMPDIR="+filepath.Join(tmp, "tmp"),
		"RCLONE_TEMP_DIR="+filepath.Join(tmp, "tmp"), "RCLONE_CACHE_DIR="+filepath.Join(tmp, "rclone"))
	s.env = restic.Env{Repository: repo, Password: pw, NoCache: true, TempDir: filepath.Join(tmp, "tmp")}
	s.common = restic.Common{RetryLock: DefaultRetryLock,
		Extra: append([]string{restic.NoCacheFlag}, restic.AdminOptions(rcloneProgram, false)...)}

	// The pinned engine only (ADR 0003), as the worker checks at start.
	var info restic.VersionInfo
	args := append(append([]string(nil), restic.Version().Args...), restic.NoCacheFlag)
	res, err := s.run.run(ctx, s.timeouts.Version, args, s.base, func(r io.Reader) error {
		var derr error
		info, derr = restic.DecodeVersion(r)
		return derr
	})
	if err != nil || res.Exit != 0 || res.StdoutErr != nil || info.Version != PinnedResticVersion {
		s.close()
		if ctx.Err() != nil {
			return nil, failedErr(SelfClassInterrupted)
		}
		return nil, usageErr(SelfClassResticVersion)
	}
	return s, nil
}

// close forgets the password and removes the session folder.
func (s *selfSession) close() {
	s.env.Password, s.env.Repository = "", ""
	_ = os.RemoveAll(s.tmp)
}

// exec runs one restic command and returns the run and its class ("" for
// success), as Worker.selfExec does without the worker's slots.
func (s *selfSession) exec(ctx context.Context, cmd restic.Command, timeout time.Duration, onStdout func(io.Reader) error) (execResult, string) {
	environ, err := s.env.Environ(s.base)
	if err != nil {
		return execResult{}, string(restic.ClassInternal)
	}
	pr, err := s.run.run(ctx, timeout, cmd.Args, environ, onStdout)
	if err != nil {
		if ctx.Err() != nil {
			return execResult{}, SelfClassInterrupted
		}
		return execResult{}, string(restic.ClassInternal)
	}
	res := execResult{procResult: pr, Result: restic.Map(cmd.Kind, pr.Exit, restic.Evidence{Stderr: pr.Stderr})}
	if pr.Canceled {
		res.Result = restic.Result{Outcome: restic.OutcomeInterrupted}
	}
	return res, selfClass(res)
}

// snapshots lists the snapshots of host console, newest first.
func (s *selfSession) snapshots(ctx context.Context) ([]restic.Snapshot, error) {
	cmd, err := restic.Snapshots(SelfBackupHost, s.common)
	if err != nil {
		return nil, failedErr(string(restic.ClassInternal))
	}
	var snaps []restic.Snapshot
	if _, class := s.exec(ctx, cmd, s.timeouts.Snapshots, func(r io.Reader) error {
		var derr error
		snaps, derr = restic.DecodeSnapshots(r)
		return derr
	}); class != "" {
		return nil, failedErr(class)
	}
	sort.SliceStable(snaps, func(i, j int) bool { return snaps[i].Time.After(snaps[j].Time) })
	return snaps, nil
}

// ListSelfSnapshots lists the self-backup repository's snapshots of host
// console, newest first. Nothing is written but a temp folder, removed again.
func ListSelfSnapshots(ctx context.Context, r SelfRepo) ([]restic.Snapshot, error) {
	s, err := openSelfRepo(ctx, r, "")
	if err != nil {
		return nil, err
	}
	defer s.close()
	return s.snapshots(ctx)
}

// ValidSnapshotPrefix reports whether id can name a snapshot: 4 to 64
// lower-case hex digits (restic's short id is 8).
func ValidSnapshotPrefix(id string) bool {
	if len(id) < 4 || len(id) > 64 {
		return false
	}
	return isHex(id, len(id))
}

// pickSnapshot is the newest snapshot (snaps newest first), or the one whose id
// begins with id.
func pickSnapshot(snaps []restic.Snapshot, id string) (restic.Snapshot, error) {
	if len(snaps) == 0 {
		return restic.Snapshot{}, failedErr(SelfClassNoSnapshot)
	}
	if id == "" {
		return snaps[0], nil
	}
	var hit []restic.Snapshot
	for _, sn := range snaps {
		if strings.HasPrefix(sn.ID, id) {
			hit = append(hit, sn)
		}
	}
	switch len(hit) {
	case 0:
		return restic.Snapshot{}, failedErr(SelfClassSnapshotUnknown)
	case 1:
		return hit[0], nil
	}
	return restic.Snapshot{}, usageErr(SelfClassSnapshotAmbiguous)
}

// outState is what RestoreSelfBackup found at --out.
type outState struct {
	dir     string
	created bool
}

// checkOut refuses an --out that is not an empty real folder. A missing one
// is fine (its parent must exist): it is created only once the repository has
// answered.
func checkOut(out string) (outState, error) {
	if out == "" || strings.ContainsRune(out, 0) {
		return outState{}, usageErr(SelfClassConfig)
	}
	abs, err := filepath.Abs(out)
	if err != nil {
		return outState{}, usageErr(SelfClassOutDir)
	}
	fi, err := os.Lstat(abs)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if pfi, perr := os.Stat(filepath.Dir(abs)); perr != nil || !pfi.IsDir() {
			return outState{}, usageErr(SelfClassOutDir)
		}
		return outState{dir: abs, created: true}, nil
	case err != nil, fi.Mode()&os.ModeSymlink != 0, !fi.IsDir():
		return outState{}, usageErr(SelfClassOutDir)
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return outState{}, usageErr(SelfClassOutDir)
	}
	if len(entries) != 0 {
		return outState{}, usageErr(SelfClassOutNotEmpty)
	}
	return outState{dir: abs}, nil
}

// wipe empties the folder again after a failure, and removes it when the
// restore created it.
func (o outState) wipe() {
	if o.created {
		_ = os.RemoveAll(o.dir)
		return
	}
	entries, err := os.ReadDir(o.dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		_ = os.RemoveAll(filepath.Join(o.dir, e.Name()))
	}
}

// RestoreSelfBackup restores the newest self-backup (or the snapshot whose id
// begins with snapshotID) into out and checks it against its manifest; see the
// top of this file. On any error out is as it was before (empty or absent).
func RestoreSelfBackup(ctx context.Context, r SelfRepo, snapshotID, out string) (SelfRestoreResult, error) {
	var res SelfRestoreResult
	if snapshotID != "" && !ValidSnapshotPrefix(snapshotID) {
		return res, usageErr(SelfClassConfig)
	}
	o, err := checkOut(out)
	if err != nil {
		return res, err
	}
	s, err := openSelfRepo(ctx, r, filepath.Dir(o.dir))
	if err != nil {
		return res, err
	}
	defer s.close()

	snaps, err := s.snapshots(ctx)
	if err != nil {
		return res, err
	}
	snap, err := pickSnapshot(snaps, snapshotID)
	if err != nil {
		return res, err
	}
	if len(snap.Paths) != 1 || snap.ID == "" {
		return res, failedErr(SelfClassManifest)
	}
	res.Snapshot = snap

	// Room for the restored files and the copy that is checked (same volume).
	if snap.Summary != nil {
		need := 2*snap.Summary.TotalBytesProcessed + SelfBackupDiskMargin
		if free, known, err := freeDiskBytes(s.tmp); err == nil && known && free < need {
			return res, failedErr(SelfClassDisk)
		}
	}

	if o.created {
		if err := os.Mkdir(o.dir, 0o700); err != nil {
			return res, usageErr(SelfClassOutDir)
		}
	}
	ok := false
	defer func() {
		if !ok {
			o.wipe()
		}
	}()

	rcmd, err := restic.Restore(restic.RestoreOptions{Common: s.common, SnapshotID: snap.ID + ":" + snapshotTreePath(snap.Paths[0]), Target: o.dir})
	if err != nil {
		return res, failedErr(SelfClassManifest)
	}
	if _, class := s.exec(ctx, rcmd, selfRestoreTimeout, nil); class != "" {
		if inconclusiveClass(class) || class == string(restic.ClassRepoMissing) || class == string(restic.ClassWrongPassword) {
			return res, failedErr(class)
		}
		return res, failedErr(SelfClassRestore)
	}

	if selfRestoreAfterRestore != nil {
		selfRestoreAfterRestore(o.dir)
	}

	// Check a copy, so the restored console.db is never opened (opening it
	// would change it) and stays byte-equal to the backed-up copy.
	check := filepath.Join(s.tmp, "check")
	for _, f := range []string{SelfBackupDBFile, SelfBackupManifestFile} {
		if err := copyFile(filepath.Join(o.dir, f), filepath.Join(check, f)); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return res, failedErr(SelfClassManifest)
			}
			return res, failedErr(SelfClassStaging)
		}
	}
	vw := &Worker{selfKeys: NewSelfBackupKeysFrom(r.MasterKey), log: logx.New(io.Discard, logx.LevelError), clock: time.Now}
	v := vw.selfCompare(ctx, check, snap.ID)
	if v.class != "" {
		return res, failedErr(v.class)
	}
	m, err := readManifest(filepath.Join(o.dir, SelfBackupManifestFile))
	if err != nil {
		return res, failedErr(SelfClassManifest)
	}
	res.Manifest, res.EscrowChecked = m, v.escrow
	if fi, err := os.Lstat(filepath.Join(o.dir, SelfBackupConfigFile)); err == nil && fi.Mode().IsRegular() {
		res.ConfigIncluded = true
	}
	if m.ConfigIncluded != res.ConfigIncluded {
		return res, failedErr(SelfClassManifest)
	}
	ok = true
	return res, nil
}

// readManifest reads manifest.json of a restored snapshot.
func readManifest(p string) (SelfBackupManifest, error) {
	var m SelfBackupManifest
	raw, err := os.ReadFile(p)
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return m, err
	}
	if m.Format != SelfBackupManifestFormat {
		return m, errors.New("maintain: not a self-backup manifest")
	}
	return m, nil
}
