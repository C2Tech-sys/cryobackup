package maintain

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/logx"
)

// PinnedResticVersion is the only restic version this worker runs (ADR 0003,
// architecture §6.3). New refuses any other `restic version --json` answer.
const PinnedResticVersion = "0.19.1"

// Default values of Config fields left at their zero value.
const (
	DefaultRemote        = "admin"
	DefaultResticProgram = "restic"
	DefaultRcloneProgram = "rclone"
	DefaultRetryLock     = 30 * time.Second
	DefaultMaxConcurrent = 4
	DefaultMaxStreams    = 2
	DefaultUsageInterval = 6 * time.Hour
	DefaultTickEvery     = time.Minute
)

// Timeouts are the per-method deadlines the worker puts on each restic run.
// They sit a little below the admin-path client defaults
// (adminpath.DefaultTimeouts) so the worker answers with a timeout error
// before the client gives up on the socket. A zero field takes its default.
type Timeouts struct {
	// Version bounds `restic version --json` at start-up (default 30s).
	Version time.Duration
	// Init bounds init plus the cat-config fallback together (default 110s).
	Init time.Duration
	// CatConfig bounds cat config (default 55s).
	CatConfig time.Duration
	// Ls bounds one directory listing (default 110s).
	Ls time.Duration
	// Find bounds one find (default 110s).
	Find time.Duration
	// Snapshots bounds one snapshots listing (default 110s).
	Snapshots time.Duration
	// Dump bounds one dump stream, including the pre-listing that tells a file
	// from a directory (default 11h55m, under adminpath.DumpTimeout: the stream
	// runs at the technician's download speed, ADR 0008).
	Dump time.Duration
	// Stats bounds one stats run (default 110s).
	Stats time.Duration
	// Unlock bounds one unlock (default 55s).
	Unlock time.Duration
}

func (t Timeouts) withDefaults() Timeouts {
	d := func(v *time.Duration, def time.Duration) {
		if *v <= 0 {
			*v = def
		}
	}
	d(&t.Version, 30*time.Second)
	d(&t.Init, 110*time.Second)
	d(&t.CatConfig, 55*time.Second)
	d(&t.Ls, 110*time.Second)
	d(&t.Find, 110*time.Second)
	d(&t.Snapshots, 110*time.Second)
	d(&t.Dump, 11*time.Hour+55*time.Minute)
	d(&t.Stats, 110*time.Second)
	d(&t.Unlock, 55*time.Second)
	return t
}

// Config configures a Worker. Nothing in it is a secret: the storage admin
// credential lives in the rclone.conf that RcloneConfig names (read by rclone;
// this process scans it only for the Remote's endpoint host, for logs) and the master key is the KeyUnwrapper's business.
type Config struct {
	// SocketPath is the admin-path unix socket Loop serves (architecture DH1,
	// DH18). Required by Loop; unused by the direct Backend calls.
	SocketPath string
	// RcloneProgram is the rclone executable restic starts for the rclone:
	// backend (`-o rclone.program=`). Default "rclone".
	RcloneProgram string
	// ResticProgram is the restic executable. Default "restic".
	ResticProgram string
	// ResticSHA256 is the expected lower-case hex SHA-256 of the restic
	// executable. When set, New refuses a binary that hashes differently
	// (architecture §6.3: pinned and hash-checked). The container image
	// already checks it at build; this is the run-time second look. Empty
	// skips the hash check (the version check always runs).
	ResticSHA256 string
	// Remote is the rclone remote of the admin credential (rclone.conf
	// section name). Default "admin".
	Remote string
	// Bucket and Prefix locate the device repositories:
	// rclone:<Remote>:<Bucket>/<Prefix>/<device id>. Bucket is required.
	Bucket string
	// Prefix is the key prefix inside the bucket; empty means the bucket root.
	Prefix string
	// SelfPrefix is the key prefix of the console's own self-backup repository
	// (CRYO_MAINTAIN_SELF_PREFIX, ADR 0029): rclone:<Remote>:<Bucket>/<SelfPrefix>,
	// written through the admin remote, never through the gateway. Empty: the
	// self-backup does not run and its job row says not_configured (loud: C11
	// then reports that no self-backup was ever written). A value that equals,
	// lies inside or begins with Prefix, contains Prefix, or is set while Prefix
	// is empty is refused (checkSelfPrefix).
	SelfPrefix string
	// StateDir is the console's state directory (config.json data_dir, else the
	// database's directory; default: the directory of the store's file). The
	// self-backup stages its copy in StateDir/self-backup-staging and restores
	// its weekly test into StateDir/self-backup-verify, both 0700, swept at the
	// start of every run and deleted at its end. It must be absolute.
	StateDir string
	// ConfigFile is the console config.json the self-backup copies beside the
	// database. It holds paths and settings, and it can hold one credential: the
	// heartbeat monitor URL (healthcheck_ping.url) often carries its token in the
	// URL. It is copied on purpose: the snapshot is encrypted by restic, and a
	// bare-host restore needs the whole file back. Empty: none is copied, and the
	// manifest says so.
	ConfigFile string
	// StorageHost is the host of the storage endpoint (CRYO_MAINTAIN_STORAGE_HOST,
	// else the endpoint of the Remote section of the rclone.conf the worker runs
	// against; see resolveStorageHost). It is used for logs
	// only: a run that cannot reach the storage logs it through logx.Host with
	// the error class, so a DNS or certificate outage is diagnosable while
	// restic's own text stays out of the log. Empty logs "unconfigured".
	StorageHost string
	// RcloneConfig is the rclone.conf path handed to the children as
	// RCLONE_CONFIG. Empty leaves rclone to its own default location.
	RcloneConfig string
	// ScratchDir is the worker's private scratch space (never restic's default,
	// engine §1.16): restic's and rclone's temp directories (ScratchDir/tmp,
	// TMPDIR) and rclone's cache (ScratchDir/rclone). ADR 0008: the console host
	// keeps no repository data on disk, so it must be a tmpfs; the worker runs
	// restic with --no-cache and warns at start when it can tell the directory
	// is not in memory. New creates it 0700 and empties the two subdirectories.
	// Default DefaultScratchDir(). Nothing else of the worker lives on disk: its
	// bookkeeping (schedules, leases, runs) is in the database, and its retry
	// timers are in memory.
	ScratchDir string
	// BypassGovernance must be false in v0: passing
	// --s3-bypass-governance-retention is M9 work gated on L-GOV-1
	// (architecture §7.3 step 10). New returns an error when it is set.
	BypassGovernance bool
	// Timeouts are the per-method deadlines; the zero value uses the defaults.
	Timeouts Timeouts
	// RetryLock is restic's --retry-lock for repository calls (default 30s).
	RetryLock time.Duration
	// MaxConcurrent caps simultaneous restic processes (default 4), RPC calls and
	// the scheduler's together (prune RAM scales with blob count, engine §2.3).
	MaxConcurrent int
	// MaxStreams caps simultaneous dump streams (technician downloads), fleet
	// wide: default 2, CRYO_MAINTAIN_MAX_STREAMS. It must be below
	// MaxConcurrent, so browse, snapshots, size sampling, prune and check always
	// keep slots while downloads run (a download holds its slot for as long as
	// the browser takes). A stream over the budget is refused at once with
	// adminpath.CodeBusy, never queued. Zero takes the default, lowered to
	// MaxConcurrent-1 when MaxConcurrent is below 3. The self-backup, the other
	// long holder of a slot, keeps the same promise: it runs only while
	// MaxConcurrent-MaxStreams leaves a slot for it and at least one more
	// (selfSlotRoom); with MaxStreams at MaxConcurrent-1 it does not run and its
	// job row says no_slot. That one more is not reserved for anything: the
	// device maintenance shares it, first come, with browse, snapshots, the
	// storage probe, escrow verify and purge. The rule promises only that
	// downloads and the self-backup together never hold every slot.
	MaxStreams int
	// UsageInterval is how often step 8 (usage refresh) samples a device
	// (default 6h). usage_samples keeps one row per hour bucket.
	UsageInterval time.Duration
	// TickEvery is the scheduler period (default 1 minute, architecture §7.3).
	TickEvery time.Duration
	// Version is the build version reported by Ping. Default buildinfo.Version.
	Version string
	// Logger receives the worker's log lines. The worker's own call sites use
	// typed logx attributes only (device id, request id, kind, exit code, class,
	// counts, elapsed); the logger's handler is the path-free safety net behind
	// them. Nil logs to logx.Default().
	Logger *logx.Logger
}

// DefaultScratchDir is <os.TempDir()>/cryo-maintain: /tmp/cryo-maintain in the
// container, whose /tmp is a tmpfs (deploy/compose/docker-compose.yml).
func DefaultScratchDir() string { return filepath.Join(os.TempDir(), "cryo-maintain") }

// errConfig is wrapped by every validation error; it never carries a value.
var errConfig = errors.New("maintain: invalid config")

func cfgErr(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errConfig, fmt.Sprintf(format, args...))
}

// withDefaults fills the zero fields and validates the result.
func (c Config) withDefaults() (Config, error) {
	if c.RcloneProgram == "" {
		c.RcloneProgram = DefaultRcloneProgram
	}
	// restic splits the rclone.program option like a shell command line (verified
	// with restic 0.19.1: a backslash is an escape, a space starts an argument),
	// so the path is written with forward slashes and may hold neither whitespace
	// nor quotes.
	c.RcloneProgram = filepath.ToSlash(c.RcloneProgram)
	if c.ResticProgram == "" {
		c.ResticProgram = DefaultResticProgram
	}
	if c.Remote == "" {
		c.Remote = DefaultRemote
	}
	if c.RetryLock <= 0 {
		c.RetryLock = DefaultRetryLock
	}
	if c.MaxConcurrent <= 0 {
		c.MaxConcurrent = DefaultMaxConcurrent
	}
	if c.MaxStreams <= 0 {
		c.MaxStreams = min(DefaultMaxStreams, c.MaxConcurrent-1)
	}
	if c.UsageInterval <= 0 {
		c.UsageInterval = DefaultUsageInterval
	}
	if c.TickEvery <= 0 {
		c.TickEvery = DefaultTickEvery
	}
	c.Timeouts = c.Timeouts.withDefaults()
	if c.ScratchDir == "" {
		c.ScratchDir = DefaultScratchDir()
	}
	c.ResticSHA256 = strings.ToLower(strings.TrimSpace(c.ResticSHA256))

	switch {
	case c.BypassGovernance:
		return c, cfgErr("BypassGovernance is not supported in v0 (architecture §7.3 step 10, gated on L-GOV-1)")
	case strings.ContainsAny(c.RcloneProgram, " \t\r\n\"'\\"):
		return c, cfgErr("RcloneProgram must not contain whitespace, quotes or backslashes (restic splits rclone.program)")
	case strings.Contains(c.Remote, ":") || strings.Contains(c.Remote, "/"):
		return c, cfgErr("Remote must be a bare rclone remote name")
	case c.Bucket == "":
		return c, cfgErr("Bucket is required")
	case c.MaxStreams < 1 || c.MaxStreams >= c.MaxConcurrent:
		return c, cfgErr("MaxStreams (CRYO_MAINTAIN_MAX_STREAMS) must be at least 1 and below MaxConcurrent (%d), so maintenance always keeps a slot", c.MaxConcurrent)
	case !filepath.IsAbs(c.ScratchDir):
		return c, cfgErr("ScratchDir must be an absolute path")
	case topLevel(c.ScratchDir):
		// The worker empties <scratch>/tmp and <scratch>/rclone at start: on "/"
		// or "/var" that would delete a system directory's contents.
		return c, cfgErr("ScratchDir (CRYO_MAINTAIN_SCRATCH_DIR) must not be a filesystem root or a top-level directory: the worker empties its tmp and rclone subdirectories; use a directory of its own such as /run/cryo-maintain")
	}
	if c.ResticSHA256 != "" && !isHex(c.ResticSHA256, 64) {
		return c, cfgErr("ResticSHA256 must be 64 hex digits")
	}
	if c.StateDir != "" && !filepath.IsAbs(c.StateDir) {
		return c, fmt.Errorf("%w: %w", errConfig, errStateDir)
	}
	c.SelfPrefix = strings.Trim(c.SelfPrefix, "/")
	if c.SelfPrefix != "" {
		if err := checkSelfPrefix(c.Prefix, c.SelfPrefix); err != nil {
			return c, err
		}
	}
	return c, nil
}

// checkSelfPrefix is the rule for the self-backup prefix (ticket P22-C, spike
// P22-S1 item 8.2). The gateway credential's IAM policy is scoped to the device
// prefix, and a policy resource written "<bucket>/<prefix>*" (no slash) matches
// every key that merely begins with the prefix, "<prefix>-console/..." included.
// So with device prefix "cb" the self prefix is refused when it is "cb", inside
// it ("cb/x"), or a string extension of it ("cb-console", "cbx"): the gateway
// could then read or overwrite the console's own backup. With an empty device
// prefix (bucket root) every self prefix is inside the device area: refused.
// A self prefix that contains the device prefix ("a" with device "a/b") is
// refused too: the device repositories would sit inside the self-backup
// repository. Each segment must be letters, digits, '.', '_' or '-' (the
// alphabet restic.AdminRepository accepts), never "." or "..".
// Both prefixes are compared without their leading and trailing slashes.
func checkSelfPrefix(devicePrefix, selfPrefix string) error {
	d := strings.Trim(devicePrefix, "/")
	s := strings.Trim(selfPrefix, "/")
	var why error
	for _, seg := range strings.Split(s, "/") {
		if !validPrefixSegment(seg) {
			why = errSelfPrefixForm
		}
	}
	switch {
	case why != nil:
	case d == "":
		why = errSelfPrefixRoot
	case strings.HasPrefix(s, d):
		why = errSelfPrefixGateway
	case strings.HasPrefix(d, s+"/"):
		why = errSelfPrefixContains
	default:
		return nil
	}
	return fmt.Errorf("%w: %w", errConfig, why)
}

// The configuration errors of the self-backup settings. Each is a fixed
// sentence (no value, no path) that Run prints as it is (configText): the
// prefix refusals name the IAM reason, so the operator knows why.
var (
	errSelfPrefixGateway = errors.New("CRYO_MAINTAIN_SELF_PREFIX must not equal, lie inside or begin with CRYO_MAINTAIN_PREFIX: " +
		"the gateway credential's IAM policy covers <prefix>/* and, when written <prefix>* without the slash, also <prefix>-console, " +
		"so the gateway could read or overwrite the console's self-backup; choose a prefix that does not begin with the device prefix")
	errSelfPrefixRoot = errors.New("CRYO_MAINTAIN_SELF_PREFIX is set while CRYO_MAINTAIN_PREFIX is empty: with the device repositories at the bucket root " +
		"the gateway credential's IAM policy covers the whole bucket and every self-backup prefix is inside it; " +
		"device repositories must live under a prefix for self-backup to be safe")
	errSelfPrefixContains = errors.New("CRYO_MAINTAIN_SELF_PREFIX must not contain CRYO_MAINTAIN_PREFIX: the device repositories would sit inside the self-backup repository")
	errSelfPrefixForm     = errors.New("CRYO_MAINTAIN_SELF_PREFIX must be one or more segments of letters, digits, '.', '_' or '-' separated by '/' (no '.' or '..' segment)")
	errStateDir           = errors.New("the console state directory (config.json data_dir or CRYO_DATA_DIR) must be an absolute path")
)

// configText is the line Run prints for a configuration error: the
// self-backup sentence when it is one of those, classText otherwise.
func configText(err error) string {
	for _, e := range []error{errSelfPrefixGateway, errSelfPrefixRoot, errSelfPrefixContains, errSelfPrefixForm, errStateDir} {
		if errors.Is(err, e) {
			return "maintain: invalid configuration: " + e.Error()
		}
	}
	return classText(err)
}

// validPrefixSegment: 1 to 64 of [A-Za-z0-9._-], not "." or "..".
func validPrefixSegment(seg string) bool {
	if seg == "" || seg == "." || seg == ".." || len(seg) > 64 {
		return false
	}
	for i := 0; i < len(seg); i++ {
		c := seg[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// topLevel reports whether the absolute path p is a filesystem root ("/",
// `C:\`) or a directory directly under one ("/tmp", "/var", `C:\Windows`).
func topLevel(p string) bool {
	p = filepath.Clean(p)
	parent := filepath.Dir(p)
	return parent == p || filepath.Dir(parent) == parent
}

func isHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
