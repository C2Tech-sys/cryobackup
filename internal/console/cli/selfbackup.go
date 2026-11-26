package cli

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/c2tech-sys/cryobackup/internal/console/escrow"
	"github.com/c2tech-sys/cryobackup/internal/console/maintain"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// `cryo-console self-backup`: the console's own backup (ADR 0029). run and
// verify go through the maintenance worker's own step (`maintain --once`), so
// what they do is exactly what the scheduler does; list and restore need only
// the operator's offline kit (maintain/selfbackup_restore.go) and work on a
// bare host with no database and no running console.

const selfBackupUsage = `usage: cryo-console self-backup <run|verify|list|restore> [flags]

  run      one self-backup now. It makes the db-self-backup job due and runs one maintenance pass
           (as 'cryo-console maintain --once', so the device maintenance that is due runs too), then
           prints the result class. Run it as the worker's user with the worker's environment
           (the CRYO_MAINTAIN_* variables of maintain.env, RCLONE_CONFIG), leaving
           CRYO_MAINTAIN_SCRATCH_DIR unset or different from the running worker's.
             [--config config.json]
  verify   the weekly tested restore now, the same way: restore the newest snapshot into the state
           directory, check it against its manifest (sha256, integrity_check, schema, row counts),
           unwrap one escrowed key, run check --read-data. Prints the verdict and its class.
             [--config config.json]
  list     the snapshots of the self-backup repository: time (UTC), size, id. Never file contents.
             --rclone-config F --remote R --bucket B --prefix P --master-key K
  restore  restore a self-backup onto this host. Needs no database and no running console: only
           the admin rclone file and the master key of the offline kit. Restores the newest snapshot
           (or --snapshot ID) into DIR, which must be empty or not exist yet, checks it against its
           manifest and prints what to copy where. On any failure DIR is left empty.
             --rclone-config F --remote R --bucket B --prefix P --master-key K --out DIR [--snapshot ID]

  list and restore also take --restic BIN, --restic-sha256 HEX and --rclone BIN. Defaults come from the
  worker's variables when set: RCLONE_CONFIG, CRYO_MAINTAIN_REMOTE (else admin), CRYO_MAINTAIN_BUCKET,
  CRYO_MAINTAIN_SELF_PREFIX, CRYO_MASTER_KEY_PATH, CRYO_MAINTAIN_RESTIC (else restic on PATH),
  CRYO_MAINTAIN_RESTIC_SHA256, CRYO_MAINTAIN_RCLONE (else rclone on PATH). restic must be 0.19.1.

  exit codes: 0 ok; 1 the backup, verify, listing or restore failed (or did not run); 2 a usage or
  configuration error (including a self-backup that is not configured or has no restic slot).
  No secret is printed: the repository password is derived from the master key and never shown.
`

// runSelfBackup dispatches `cryo-console self-backup <command>`.
func runSelfBackup(args []string, io IO) int {
	if len(args) == 0 {
		fmt.Fprint(io.Stderr, selfBackupUsage)
		return ExitUsage
	}
	switch args[0] {
	case "run", "verify":
		return selfBackupStep(args[0], args[1:], io)
	case "list":
		return selfBackupList(args[1:], io)
	case "restore":
		return selfBackupRestore(args[1:], io)
	case "help", "-h", "--help":
		fmt.Fprint(io.Stdout, selfBackupUsage)
		return ExitOK
	}
	fmt.Fprintf(io.Stderr, "cryo-console self-backup: unknown command %q\n\n", sanitizeToken(args[0]))
	fmt.Fprint(io.Stderr, selfBackupUsage)
	return ExitUsage
}

// selfBackupContext is the context the commands run under: cancelled by
// SIGINT/SIGTERM (a variable so tests can replace it).
var selfBackupContext = func() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// selfBackupMaintain runs `maintain` for run and verify (tests replace it).
var selfBackupMaintain = func(args []string, stdout, stderr io.Writer) int {
	return maintain.Run(args, stdout, stderr, MaintainKeys)
}

// selfBackupStep is run and verify: make the job due, run one `maintain
// --once`, and judge the self-backup line it prints.
func selfBackupStep(what string, args []string, io IO) int {
	fs := newFlags("self-backup "+what, io)
	cfgPath := fs.String("config", "", "console config.json (default $CRYO_CONFIG, else ./config.json)")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(io.Stderr, "cryo-console self-backup %s: unexpected arguments\n", what)
		return ExitUsage
	}
	path := *cfgPath
	if path == "" {
		path = os.Getenv("CRYO_CONFIG")
	}
	if path == "" {
		path = "config.json"
	}
	job := maintain.SelfBackupJob
	if what == "verify" {
		job = maintain.SelfBackupVerifyJob
	}
	if code := makeJobDue(path, job, io); code != ExitOK {
		return code
	}
	var out bytes.Buffer
	code := selfBackupMaintain([]string{"--config", path, "--once"}, &out, io.Stderr)
	_, _ = io.Stdout.Write(out.Bytes())
	if code == maintain.ExitUsage {
		fmt.Fprintf(io.Stderr, "cryo-console self-backup %s: the maintenance worker refused its configuration (above)\n", what)
		return ExitUsage
	}
	msg, rc := selfStepOutcome(what, out.String())
	if rc == ExitOK {
		fmt.Fprintln(io.Stdout, msg)
	} else {
		fmt.Fprintln(io.Stderr, msg)
	}
	return rc
}

// makeJobDue clears next_due_at of job, so the worker's next step (this
// command's, or the running worker's next tick when that one holds the lease)
// finds it due. Nothing else of the row changes: its history stays.
func makeJobDue(cfgPath, job string, io IO) int {
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		fmt.Fprintln(io.Stderr, "cryo-console self-backup:", sanitize(err))
		return ExitUsage
	}
	ctx := context.Background()
	st, err := store.Open(ctx, store.Options{Path: cfg.DBPath})
	if err != nil {
		fmt.Fprintln(io.Stderr, "cryo-console self-backup: cannot open the database:", sanitize(err))
		return ExitFailed
	}
	defer st.Close()
	err = st.Write(ctx, func(tx *store.Tx) error {
		// A job with no row is due already (maintain: selfJobDue).
		_, err := tx.Exec(`UPDATE job_state SET next_due_at = NULL, updated_at = ? WHERE name = ?`, tx.Now().UnixMilli(), job)
		return err
	})
	if err != nil {
		fmt.Fprintln(io.Stderr, "cryo-console self-backup: the job could not be made due:", sanitize(err))
		return ExitFailed
	}
	return ExitOK
}

// selfStepLinePrefix starts the line `maintain --once` prints for its
// self-backup step (maintain.selfBackupLine).
const selfStepLinePrefix = "maintain: self-backup "

// selfStepOutcome judges the self-backup line of `maintain --once` for run
// (the backup) or verify (the verification) and returns the sentence to print
// and the exit code.
func selfStepOutcome(what, out string) (string, int) {
	line := ""
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, selfStepLinePrefix) {
			line = strings.TrimSpace(strings.TrimPrefix(l, selfStepLinePrefix))
		}
	}
	head := "self-backup " + what + ": "
	switch line {
	case "":
		return head + "the maintenance pass stopped before the self-backup step (see above)", ExitFailed
	case "skipped":
		return head + "not run: another process holds the self-backup's lease (the running worker is doing a step now); the job stays due, try again later", ExitFailed
	case "nothing_due":
		return head + "not run: nothing was due", ExitFailed
	case "not_configured":
		return head + "not run, class not_configured: set CRYO_MAINTAIN_SELF_PREFIX (ADR 0029)", ExitUsage
	case "no_slot":
		return head + "not run, class no_slot: CRYO_MAINTAIN_MAX_STREAMS leaves the self-backup no restic slot; keep it at 2", ExitUsage
	}
	fields := map[string]string{}
	leaseLost := false
	for _, f := range strings.Fields(line) {
		if k, v, ok := strings.Cut(f, "="); ok {
			fields[k] = v
		} else if f == "lease_lost" {
			leaseLost = true
		}
	}
	if leaseLost {
		return head + "stopped: another process took over the self-backup's lease mid-step", ExitFailed
	}
	if what == "run" {
		switch b := fields["backup"]; {
		case b == "ok":
			return head + "ok: a new snapshot was written (class none)", ExitOK
		case strings.HasPrefix(b, "error:"):
			return head + "FAILED, class " + strings.TrimPrefix(b, "error:"), ExitFailed
		}
		return head + "the backup did not run", ExitFailed
	}
	switch v := fields["verify"]; {
	case v == "ok":
		return head + "verdict ok: the newest snapshot restored intact and matches its manifest (class none)", ExitOK
	case strings.HasPrefix(v, "failed:"):
		return head + "verdict FAILED, class " + strings.TrimPrefix(v, "failed:"), ExitFailed
	case strings.HasPrefix(v, "inconclusive:"):
		return head + "no verdict, class " + strings.TrimPrefix(v, "inconclusive:"), ExitFailed
	}
	return head + "no verification ran: no self-backup is recorded yet, or the step stopped before it", ExitFailed
}

// selfRepoFlags are the flags of list and restore, with the worker's variables
// as defaults.
type selfRepoFlags struct {
	rcloneConfig, remote, bucket, prefix, masterKey, restic, resticSHA, rclone *string
	drill                                                                      *bool
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func addSelfRepoFlags(fs *flag.FlagSet) selfRepoFlags {
	return selfRepoFlags{
		rcloneConfig: fs.String("rclone-config", os.Getenv("RCLONE_CONFIG"), "the admin rclone file of the offline kit (default $RCLONE_CONFIG)"),
		remote:       fs.String("remote", envOr("CRYO_MAINTAIN_REMOTE", "admin"), "its remote name (default $CRYO_MAINTAIN_REMOTE, else admin)"),
		bucket:       fs.String("bucket", os.Getenv("CRYO_MAINTAIN_BUCKET"), "the bucket (default $CRYO_MAINTAIN_BUCKET)"),
		prefix:       fs.String("prefix", os.Getenv("CRYO_MAINTAIN_SELF_PREFIX"), "the self-backup prefix (default $CRYO_MAINTAIN_SELF_PREFIX)"),
		masterKey:    fs.String("master-key", os.Getenv("CRYO_MASTER_KEY_PATH"), "the master key file of the offline kit (default $CRYO_MASTER_KEY_PATH)"),
		restic:       fs.String("restic", envOr("CRYO_MAINTAIN_RESTIC", "restic"), "restic 0.19.1 (default $CRYO_MAINTAIN_RESTIC, else restic on PATH)"),
		resticSHA:    fs.String("restic-sha256", os.Getenv("CRYO_MAINTAIN_RESTIC_SHA256"), "the restic binary's pinned sha256 (default $CRYO_MAINTAIN_RESTIC_SHA256)"),
		rclone:       fs.String("rclone", envOr("CRYO_MAINTAIN_RCLONE", "rclone"), "rclone (default $CRYO_MAINTAIN_RCLONE, else rclone on PATH)"),
		drill:        fs.Bool("drill", envBool("CRYO_DRILL"), "a drill run: read the live repository with --no-lock, leaving no lock object in it (default $CRYO_DRILL)"),
	}
}

// envBool is the boolean value of the variable; unset, empty or not a boolean is false.
func envBool(name string) bool {
	b, err := strconv.ParseBool(os.Getenv(name))
	return err == nil && b
}

// repo checks the flags and loads the master key. A missing flag or a key
// file that cannot be read is a usage error; the message names the flag, never
// a path or a secret.
func (f selfRepoFlags) repo(cmd string, io IO) (maintain.SelfRepo, bool) {
	if v := os.Getenv("CRYO_DRILL"); v != "" {
		if _, err := strconv.ParseBool(v); err != nil { // as the worker: a malformed value is refused, not guessed
			fmt.Fprintf(io.Stderr, "cryo-console self-backup %s: CRYO_DRILL must be true or false\n", cmd)
			return maintain.SelfRepo{}, false
		}
	}
	for _, req := range []struct{ name, env, val string }{
		{"--rclone-config", "RCLONE_CONFIG", *f.rcloneConfig}, {"--remote", "CRYO_MAINTAIN_REMOTE", *f.remote},
		{"--bucket", "CRYO_MAINTAIN_BUCKET", *f.bucket}, {"--prefix", "CRYO_MAINTAIN_SELF_PREFIX", *f.prefix},
		{"--master-key", "CRYO_MASTER_KEY_PATH", *f.masterKey},
	} {
		if strings.TrimSpace(req.val) == "" {
			fmt.Fprintf(io.Stderr, "cryo-console self-backup %s: %s is required (or set %s)\n", cmd, req.name, req.env)
			return maintain.SelfRepo{}, false
		}
	}
	mk, err := escrow.LoadMasterKey(*f.masterKey)
	if err != nil {
		fmt.Fprintf(io.Stderr, "cryo-console self-backup %s: the --master-key file could not be read or is not a console master key\n", cmd)
		return maintain.SelfRepo{}, false
	}
	return maintain.SelfRepo{
		ResticProgram: *f.restic, ResticSHA256: *f.resticSHA, RcloneProgram: *f.rclone,
		RcloneConfig: *f.rcloneConfig, Remote: *f.remote, Bucket: *f.bucket, Prefix: *f.prefix, MasterKey: mk,
		Drill: *f.drill,
	}, true
}

// selfBackupList prints the snapshots of the self-backup repository.
func selfBackupList(args []string, io IO) int {
	fs := newFlags("self-backup list", io)
	flags := addSelfRepoFlags(fs)
	if code, ok := parse(fs, args); !ok {
		return code
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(io.Stderr, "cryo-console self-backup list: unexpected arguments")
		return ExitUsage
	}
	repo, ok := flags.repo("list", io)
	if !ok {
		return ExitUsage
	}
	ctx, stop := selfBackupContext()
	defer stop()
	snaps, err := maintain.ListSelfSnapshots(ctx, repo)
	if err != nil {
		return selfRestoreFailure("list", err, io)
	}
	if len(snaps) == 0 {
		fmt.Fprintln(io.Stdout, "no self-backup snapshot in this repository (host console)")
		return ExitOK
	}
	fmt.Fprintf(io.Stdout, "%-20s  %10s  %s\n", "TIME (UTC)", "SIZE", "ID")
	for _, s := range snaps {
		size := "-"
		if s.Summary != nil {
			size = humanBytes(s.Summary.TotalBytesProcessed)
		}
		fmt.Fprintf(io.Stdout, "%-20s  %10s  %s\n", s.Time.UTC().Format("2006-01-02T15:04:05Z"), size, s.ID)
	}
	fmt.Fprintf(io.Stdout, "%d snapshot(s), newest first; restore one with --snapshot ID (a unique prefix is enough)\n", len(snaps))
	return ExitOK
}

// selfBackupRestore restores a self-backup with the offline kit alone.
func selfBackupRestore(args []string, io IO) int {
	fs := newFlags("self-backup restore", io)
	flags := addSelfRepoFlags(fs)
	out := fs.String("out", "", "the folder to restore into: empty, or not there yet (required)")
	snapshot := fs.String("snapshot", "", "the snapshot id (or a unique prefix of it); default the newest")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(io.Stderr, "cryo-console self-backup restore: unexpected arguments")
		return ExitUsage
	}
	if strings.TrimSpace(*out) == "" {
		fmt.Fprintln(io.Stderr, "cryo-console self-backup restore: --out is required")
		return ExitUsage
	}
	if *snapshot != "" && !maintain.ValidSnapshotPrefix(*snapshot) {
		fmt.Fprintln(io.Stderr, "cryo-console self-backup restore: --snapshot must be 4 to 64 lower-case hex digits (an id from 'self-backup list')")
		return ExitUsage
	}
	repo, ok := flags.repo("restore", io)
	if !ok {
		return ExitUsage
	}
	ctx, stop := selfBackupContext()
	defer stop()
	res, err := maintain.RestoreSelfBackup(ctx, repo, *snapshot, *out)
	if err != nil {
		return selfRestoreFailure("restore", err, io)
	}
	m := res.Manifest
	w := io.Stdout
	fmt.Fprintln(w, "self-backup restore: ok")
	fmt.Fprintf(w, "  snapshot  %s, written %s (host %s)\n", res.Snapshot.ID, m.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"), maintain.SelfBackupHost)
	fmt.Fprintf(w, "  checked   console.db: %d bytes, sha256 %s, equal to its manifest; integrity_check ok; schema %d; row counts of %d tables match\n",
		m.Database.Bytes, m.Database.SHA256, m.SchemaVersion, len(m.Tables))
	if res.EscrowChecked {
		fmt.Fprintln(w, "  escrow    one escrowed repository key of the copy unwrapped with this master key")
	} else {
		fmt.Fprintln(w, "  escrow    the copy holds no escrowed device key: nothing to unwrap")
	}
	fmt.Fprintf(w, "  manifest  build %s, master key id %s, bucket %s, self-backup prefix %s, device prefix %s\n",
		orDash(m.Build), orDash(m.MasterKeyID), orDash(m.Bucket), orDash(m.Prefix), orDash(m.DevicePrefix))
	files := "console.db, manifest.json"
	if res.ConfigIncluded {
		files = "console.db, config.json, manifest.json"
	}
	fmt.Fprintln(w, "  in --out  "+files)
	fmt.Fprint(w, selfRestoreNext(res.ConfigIncluded))
	return ExitOK
}

// selfRestoreNext is what to copy where after a restore, on the standard
// layout of deploy/systemd (the paths in your config.json win).
func selfRestoreNext(config bool) string {
	cfgStep := "  3. config.json was not in this snapshot: write /etc/cryobackup/config.json from your records (deploy/systemd/README.md).\n"
	if config {
		cfgStep = "  3. config.json -> /etc/cryobackup/config.json (root:cryobackup, 0640). Check its host names and TLS paths for the new host.\n"
	}
	return "\nnext, on the new host (standard layout of deploy/systemd; the paths in config.json win):\n" +
		"  1. systemctl stop cryo-console cryo-maintain rclone-gateway (if they run yet).\n" +
		"  2. console.db -> the db_path of config.json (standard /var/lib/cryobackup/console.db), owner cryobackup, mode 0640;\n" +
		"     delete any console.db-wal and console.db-shm beside it first.\n" +
		cfgStep +
		"  4. from the offline kit: master.key -> /etc/cryobackup/master.key (0400); rclone-admin.conf and rclone-gateway.conf,\n" +
		"     maintain.env and gateway.env -> /etc/cryobackup/; the mail password file at the path smtp.password_file names.\n" +
		"     TLS keys are not kept: issue them again (deploy/rclone/tls.md).\n" +
		"  5. systemctl start cryo-console cryo-maintain rclone-gateway; cryo-console doctor --config /etc/cryobackup/config.json.\n" +
		"  Lost (ADR 0029): everything after this snapshot. Devices enrolled since are refused and must enroll again.\n" +
		"  manifest.json is for your records; the console does not read it. Runbook: docs/runbooks/console-disaster-recovery.md\n"
}

// selfRestoreFailure prints a failure of list or restore and returns its exit
// code: 2 for the caller's configuration, 1 otherwise.
func selfRestoreFailure(cmd string, err error, io IO) int {
	var se *maintain.SelfRestoreError
	if !errors.As(err, &se) {
		fmt.Fprintf(io.Stderr, "cryo-console self-backup %s: failed\n", cmd)
		return ExitFailed
	}
	fmt.Fprintf(io.Stderr, "cryo-console self-backup %s: FAILED, class %s: %s\n", cmd, se.Class, selfClassText(se.Class))
	if cmd == "restore" && !se.Usage {
		fmt.Fprintln(io.Stderr, "nothing was left in --out.")
	}
	if se.Usage {
		return ExitUsage
	}
	return ExitFailed
}

// selfClassText says what a class means for the operator holding the kit.
func selfClassText(class string) string {
	switch class {
	case "wrong_password":
		return "the master key does not open this repository (a different master key, or the wrong bucket or prefix)"
	case "repo_missing":
		return "there is no self-backup repository at that bucket and prefix"
	case "network", "tls", "storage", "auth":
		return "the storage could not be reached, or refused the admin credential of --rclone-config"
	case maintain.SelfClassOutNotEmpty:
		return "--out must be an empty folder, or one that does not exist yet"
	case maintain.SelfClassOutDir:
		return "--out is a file or a symlink, its parent folder does not exist, or it cannot be created"
	case maintain.SelfClassConfig:
		return "an option is missing or malformed, or the --rclone-config file is not there"
	case maintain.SelfClassNoKey:
		return "the master key is not usable"
	case maintain.SelfClassResticBinary:
		return "restic was not found, or its sha256 is not the pinned one"
	case maintain.SelfClassResticVersion:
		return "restic is not version " + maintain.PinnedResticVersion
	case maintain.SelfClassNoSnapshot:
		return "the repository holds no snapshot of host console"
	case maintain.SelfClassSnapshotUnknown:
		return "no snapshot has that id ('self-backup list' shows them)"
	case maintain.SelfClassSnapshotAmbiguous:
		return "that id prefix names more than one snapshot: give more of it"
	case maintain.SelfClassManifest:
		return "the restored copy does not match its manifest; try an older snapshot (--snapshot)"
	case maintain.SelfClassIntegrity:
		return "the restored database fails PRAGMA integrity_check; try an older snapshot (--snapshot)"
	case maintain.SelfClassMasterKey:
		return "the copy was made under another master key"
	case maintain.SelfClassEscrow:
		return "an escrowed key of the copy does not unwrap with this master key"
	case maintain.SelfClassDisk:
		return "not enough free disk beside --out for the restored files and the copy that is checked"
	case maintain.SelfClassInterrupted:
		return "stopped before it finished"
	case maintain.SelfClassTimeout:
		return "restic hit its deadline"
	}
	return "see the class"
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// humanBytes is n in B, KiB, MiB or GiB, one decimal.
func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatUint(n, 10) + " B"
	}
	v, suffix := float64(n), []string{"KiB", "MiB", "GiB", "TiB"}
	i := -1
	for v >= unit && i < len(suffix)-1 {
		v /= unit
		i++
	}
	return strconv.FormatFloat(v, 'f', 1, 64) + " " + suffix[i]
}
