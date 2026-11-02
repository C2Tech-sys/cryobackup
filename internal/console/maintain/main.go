package maintain

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
)

// Exit codes of Main.
const (
	// ExitOK: the pass or the loop finished cleanly.
	ExitOK = 0
	// ExitFailed: a runtime failure (a device's usage refresh failed, the
	// admin-path server stopped, the database could not be opened).
	ExitFailed = 1
	// ExitUsage: a bad command line or configuration, or an unpinned restic.
	ExitUsage = 2
)

// KeyUnwrapperFactory builds the production KeyUnwrapper from the master key file
// and the opened store. internal/console/escrow provides the implementation; the
// console's cli package wires it (cli.MaintainKeys, passed to Run).
type KeyUnwrapperFactory func(masterKeyPath string, st *store.Store) (KeyUnwrapper, error)

// DefaultKeyUnwrapper is the factory Main uses. It is nil unless a binary sets
// it (the console calls Run with cli.MaintainKeys instead); Main then refuses to
// start (ExitUsage) rather than run without keys.
var DefaultKeyUnwrapper KeyUnwrapperFactory

// Main is `cryo-console maintain`. args are the arguments after the subcommand
// name. It returns the process exit code.
//
//	--config <file>   console config.json (default $CRYO_CONFIG, else ./config.json)
//	--loop            serve the admin-path socket and run the scheduler until SIGINT/SIGTERM
//	--once            run one scheduler pass and exit
//	--socket <path>   admin-path unix socket (overrides config adminpath_socket)
//
// Exactly one of --loop and --once is required. The keys it reads from the
// config file are the ones the console config schema (owned by P1-T5) defines:
// db_path, master_key_path, adminpath_socket; each is also read from the
// environment as CRYO_<KEY>. The worker's own settings are environment-only, so
// the shared config schema stays P1-T5's:
//
//	CRYO_MAINTAIN_RESTIC, CRYO_MAINTAIN_RESTIC_SHA256   restic program and its pinned SHA-256
//	CRYO_MAINTAIN_RESTIC_SHA256_FILE                     alternative: a file whose first word is that SHA-256 (the image build writes one)
//	CRYO_MAINTAIN_RCLONE                                 rclone program for restic's rclone: backend
//	CRYO_MAINTAIN_REMOTE                                 rclone remote of the admin credential (default admin)
//	CRYO_MAINTAIN_BUCKET, CRYO_MAINTAIN_PREFIX           repository location; default from the default storage target
//	CRYO_MAINTAIN_STORAGE_HOST                           host named in storage-outage log lines; default: the endpoint of the CRYO_MAINTAIN_REMOTE section of RCLONE_CONFIG
//	CRYO_MAINTAIN_SCRATCH_DIR                            in-memory temp space, a tmpfs (default /tmp/cryo-maintain, ADR 0008);
//	                                                     refused when it is a filesystem root, a top-level directory, not owned by the worker's user,
//	                                                     or has a symlink in its path (/var/run/...: use /run/...)
//	CRYO_MAINTAIN_MAX_STREAMS                            concurrent restore download streams, fleet wide (default 2, below the 4 restic slots)
//	RCLONE_CONFIG                                       the mounted rclone.conf holding the admin credential
func Main(args []string) int {
	return Run(args, os.Stdout, os.Stderr, DefaultKeyUnwrapper)
}

// Run is Main with its outputs and key factory injected, for tests.
func Run(args []string, stdout, stderr io.Writer, keys KeyUnwrapperFactory) int {
	fs := flag.NewFlagSet("maintain", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "console config.json")
	loop := fs.Bool("loop", false, "serve the admin path and run the scheduler")
	once := fs.Bool("once", false, "run one scheduler pass and exit")
	socket := fs.String("socket", "", "admin-path unix socket")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if fs.NArg() != 0 || *loop == *once {
		fmt.Fprintln(stderr, "maintain: exactly one of --loop and --once is required")
		return ExitUsage
	}

	fc, err := loadFileConfig(*cfgPath)
	if err != nil {
		fmt.Fprintln(stderr, classText(err))
		return ExitUsage
	}
	if *socket != "" {
		fc.AdminpathSocket = *socket
	}
	switch {
	case fc.DBPath == "":
		fmt.Fprintln(stderr, "maintain: db_path (or CRYO_DB_PATH) is required")
		return ExitUsage
	case fc.MasterKeyPath == "":
		fmt.Fprintln(stderr, "maintain: master_key_path (or CRYO_MASTER_KEY_PATH) is required")
		return ExitUsage
	case *loop && fc.AdminpathSocket == "":
		fmt.Fprintln(stderr, "maintain: adminpath_socket (or --socket) is required with --loop")
		return ExitUsage
	case keys == nil:
		fmt.Fprintln(stderr, "maintain: no key unwrapper is wired into this binary")
		return ExitUsage
	}
	if os.Getenv("CRYO_MAINTAIN_STATE_DIR") != "" {
		// ADR 0008: the worker keeps nothing on a persistent volume any more.
		fmt.Fprintln(stderr, "maintain: CRYO_MAINTAIN_STATE_DIR is ignored (ADR 0008: no repository data on disk); temp files go to CRYO_MAINTAIN_SCRATCH_DIR, a tmpfs")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	lg := logx.New(stderr, logx.LevelInfo)

	st, err := store.Open(ctx, store.Options{Path: fc.DBPath})
	if err != nil {
		fmt.Fprintln(stderr, "maintain: the database could not be opened")
		return ExitFailed
	}
	defer st.Close()
	kw, err := keys(fc.MasterKeyPath, st)
	if err != nil {
		fmt.Fprintln(stderr, "maintain: the master key could not be loaded")
		return ExitFailed
	}

	resticSum, err := resticChecksum()
	if err != nil {
		fmt.Fprintln(stderr, classText(err))
		return ExitUsage
	}
	maxStreams := 0
	if v := os.Getenv("CRYO_MAINTAIN_MAX_STREAMS"); v != "" {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil || n < 1 {
			fmt.Fprintln(stderr, "maintain: CRYO_MAINTAIN_MAX_STREAMS must be a whole number of at least 1")
			return ExitUsage
		}
		maxStreams = n
	}
	cfg := Config{
		SocketPath:    fc.AdminpathSocket,
		ResticProgram: os.Getenv("CRYO_MAINTAIN_RESTIC"),
		ResticSHA256:  resticSum,
		RcloneProgram: os.Getenv("CRYO_MAINTAIN_RCLONE"),
		Remote:        os.Getenv("CRYO_MAINTAIN_REMOTE"),
		Bucket:        os.Getenv("CRYO_MAINTAIN_BUCKET"),
		Prefix:        os.Getenv("CRYO_MAINTAIN_PREFIX"),
		StorageHost:   os.Getenv("CRYO_MAINTAIN_STORAGE_HOST"),
		RcloneConfig:  os.Getenv("RCLONE_CONFIG"),
		ScratchDir:    os.Getenv("CRYO_MAINTAIN_SCRATCH_DIR"),
		MaxStreams:    maxStreams,
		Logger:        lg,
	}
	if cfg.Bucket == "" {
		tgt, err := st.DefaultStorageTarget(ctx)
		if err != nil {
			fmt.Fprintln(stderr, "maintain: no bucket: set CRYO_MAINTAIN_BUCKET or define a default storage target")
			return ExitUsage
		}
		cfg.Bucket, cfg.Prefix = tgt.Bucket, tgt.Prefix
	}
	// For logs only (Config.StorageHost): the endpoint of the rclone remote this
	// worker runs against, not the database's default storage target.
	cfg.StorageHost = resolveStorageHost(cfg, cfg.StorageHost)
	w, err := New(cfg, st, kw, nil)
	switch {
	case errors.Is(err, errConfig):
		fmt.Fprintln(stderr, classText(err))
		return ExitUsage
	case errors.Is(err, ErrResticBinary), errors.Is(err, ErrResticVersion):
		fmt.Fprintln(stderr, classText(err))
		return ExitUsage
	case err != nil:
		fmt.Fprintln(stderr, "maintain: the worker could not start")
		return ExitFailed
	}

	if *once {
		rep, err := w.RunOnce(ctx)
		fmt.Fprintf(stdout, "maintain: considered=%d sampled=%d preflight=%d deferred=%d unlocked=%d failed=%d m5_due=%d skipped=%t\n",
			rep.Considered, rep.Sampled, rep.Preflight, rep.Deferred, rep.Unlocked, rep.Failed, rep.M5Due, rep.Skipped)
		if err != nil || rep.Failed > 0 {
			return ExitFailed
		}
		return ExitOK
	}
	if err := w.Loop(ctx); err != nil {
		fmt.Fprintln(stderr, "maintain: the admin-path server stopped")
		return ExitFailed
	}
	return ExitOK
}

// resticChecksum returns the pinned restic SHA-256 from CRYO_MAINTAIN_RESTIC_SHA256
// or, when that is unset, from the first word of the file named by
// CRYO_MAINTAIN_RESTIC_SHA256_FILE. Neither set means no hash check (the version
// check still runs). A named file that cannot be read is an error: a pin the
// operator asked for must not be silently skipped.
func resticChecksum() (string, error) {
	if v := os.Getenv("CRYO_MAINTAIN_RESTIC_SHA256"); v != "" {
		return v, nil
	}
	path := os.Getenv("CRYO_MAINTAIN_RESTIC_SHA256_FILE")
	if path == "" {
		return "", nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", errChecksumRead
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return "", errChecksumNone
	}
	return fields[0], nil
}

// fileConfig is the part of the console config.json maintain reads. Unknown keys
// are ignored: the schema belongs to the console config package (artifacts_dir
// and data_dir are no longer the worker's business: ADR 0008).
type fileConfig struct {
	DBPath          string `json:"db_path"`
	MasterKeyPath   string `json:"master_key_path"`
	AdminpathSocket string `json:"adminpath_socket"`
}

// loadFileConfig reads the config file (when it exists, or when --config named
// it) and applies the CRYO_<KEY> environment overrides. Errors are fixed
// sentences: no path, no file content.
func loadFileConfig(path string) (fileConfig, error) {
	explicit := path != ""
	if path == "" {
		path = os.Getenv("CRYO_CONFIG")
		explicit = path != ""
	}
	if path == "" {
		path = "config.json"
	}
	var fc fileConfig
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(data, &fc); err != nil {
			return fc, errConfigJSON
		}
	case errors.Is(err, os.ErrNotExist) && !explicit:
		// no file: the environment may carry everything
	default:
		return fc, errConfigRead
	}
	override := func(dst *string, name string) {
		if v := os.Getenv(name); v != "" {
			*dst = v
		}
	}
	override(&fc.DBPath, "CRYO_DB_PATH")
	override(&fc.MasterKeyPath, "CRYO_MASTER_KEY_PATH")
	override(&fc.AdminpathSocket, "CRYO_ADMINPATH_SOCKET")
	return fc, nil
}
