package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/c2tech-sys/cryobackup/internal/ulid"
)

// ErrACLNotApplied is wrapped by a restrict hook (RestrictDir) when it could
// not restrict a path for a benign reason: the process is not elevated (a lab
// run with --state-dir) or the platform has no ACLs. Ensure reports it as "not
// applied" instead of failing; what may then be written is the caller's
// decision (secrets refuse, see package secrets).
var ErrACLNotApplied = errors.New("config: access restriction not applied")

// Errors of the state-directory checks (architecture §6.5). None carries a
// path: the caller knows which state directory it passed.
var (
	// ErrReparsePoint: a layout directory (the root or a child) exists and is a
	// reparse point (junction, symbolic link, mount point). A low-privileged
	// user who created it before the agent could redirect the agent's writes;
	// it is refused, never followed, never repaired.
	ErrReparsePoint = errors.New("config: a state directory is a reparse point")
	// ErrForeignOwner: the state root already exists and is owned by someone
	// other than SYSTEM or BUILTIN\Administrators (ProgramData lets any user
	// create it first); in an unelevated lab run, by someone other than those
	// two or the user running the agent. Refused, never adopted: an elevated
	// --repair-acl quarantines the tree (QuarantineRoot) instead.
	ErrForeignOwner = errors.New("config: the state directory is owned by someone other than SYSTEM or Administrators")
	// ErrStateUntrusted: a layout directory's owner or DACL differs from
	// SDDLStateDir, or cannot be read (VerifyDir also wraps ErrForeignOwner
	// when the owner is the difference).
	ErrStateUntrusted = errors.New("config: the state directory's owner or DACL is not the expected SYSTEM + Administrators descriptor")
	// ErrPrivilege: the process is elevated but SeRestorePrivilege,
	// SeTakeOwnershipPrivilege (or, for a quarantine, SeBackupPrivilege) could
	// not be enabled, so the owner cannot be set.
	ErrPrivilege = errors.New("config: the privileges to set the owner (SeRestorePrivilege, SeTakeOwnershipPrivilege) could not be enabled")
	// ErrQuarantineExists: the quarantine target next to the state root already
	// exists; nothing was moved.
	ErrQuarantineExists = errors.New("config: the quarantine target already exists; nothing was moved")
)

// SDDLStateDir is the security descriptor of every layout directory: owner
// SYSTEM, a protected DACL (no inheritance from ProgramData) granting full
// control to SYSTEM and BUILTIN\Administrators, inherited by children.
const SDDLStateDir = "O:SYG:SYD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"

// stateACEs are the two ACEs of SDDLStateDir, in either order.
const (
	aceSystem = "(A;OICI;FA;;;SY)"
	aceAdmins = "(A;OICI;FA;;;BA)"
)

// CheckStateSDDL compares an SDDL string read back from a directory (owner and
// DACL at least) with SDDLStateDir: the owner must be SYSTEM or BUILTIN\
// Administrators (an administrator-created root that the agent then
// restricted, or SYSTEM), the DACL must be protected and hold exactly the two
// full-control ACEs. It is pure so the rule is tested on every platform.
func CheckStateSDDL(sddl string) error {
	parts := sddlParts(sddl)
	switch parts['O'] {
	case "SY", "BA":
	case "":
		return fmt.Errorf("%w: no owner in the descriptor", ErrStateUntrusted)
	default:
		return fmt.Errorf("%w: owner %s", ErrStateUntrusted, parts['O'])
	}
	dacl, ok := parts['D']
	if !ok {
		return fmt.Errorf("%w: no DACL in the descriptor", ErrStateUntrusted)
	}
	i := strings.IndexByte(dacl, '(')
	if i < 0 {
		return fmt.Errorf("%w: the DACL is empty", ErrStateUntrusted)
	}
	flags, aces := dacl[:i], dacl[i:]
	if !strings.Contains(flags, "P") {
		return fmt.Errorf("%w: the DACL is not protected from inheritance", ErrStateUntrusted)
	}
	if aces != aceSystem+aceAdmins && aces != aceAdmins+aceSystem {
		return fmt.Errorf("%w: the DACL is %s", ErrStateUntrusted, aces)
	}
	return nil
}

// sddlParts splits an SDDL string into its O:, G:, D: and S: components.
// Section markers are a letter from "OGDS" followed by ':'; neither SID
// strings, aliases nor ACE strings contain a colon.
func sddlParts(s string) map[byte]string {
	out := map[byte]string{}
	var marks []int
	for i := 0; i+1 < len(s); i++ {
		if s[i+1] == ':' && strings.IndexByte("OGDS", s[i]) >= 0 {
			marks = append(marks, i)
		}
	}
	for k, m := range marks {
		end := len(s)
		if k+1 < len(marks) {
			end = marks[k+1]
		}
		out[s[m]] = s[m+2 : end]
	}
	return out
}

// DefaultStateDir is the state directory when --state-dir is absent:
// %ProgramData%\CryoBackup on Windows (architecture §6.5), /var/lib/cryobackup
// elsewhere (CI and development only).
func DefaultStateDir() string {
	if runtime.GOOS == "windows" {
		pd := os.Getenv("ProgramData")
		if pd == "" {
			pd = `C:\ProgramData`
		}
		return filepath.Join(pd, "CryoBackup")
	}
	return "/var/lib/cryobackup"
}

// Layout is the on-disk layout of architecture §6.5 under one state directory.
type Layout struct {
	Root string
}

// Dir and file names under the state directory.
const (
	dirConfig  = "config"
	dirSecrets = "secrets"
	dirPolicy  = "policy"
	dirJournal = "journal"
	dirCache   = "cache"
	dirTmp     = "tmp"
	dirLogs    = "logs"
)

func (l Layout) ConfigDir() string  { return filepath.Join(l.Root, dirConfig) }
func (l Layout) SecretsDir() string { return filepath.Join(l.Root, dirSecrets) }
func (l Layout) PolicyDir() string  { return filepath.Join(l.Root, dirPolicy) }
func (l Layout) JournalDir() string { return filepath.Join(l.Root, dirJournal) }

// CacheDir is RESTIC_CACHE_DIR (never the SYSTEM profile default, engine §1.16).
func (l Layout) CacheDir() string { return filepath.Join(l.Root, dirCache, "restic") }

// TempDir is TEMP for the restic child; emptied at start and after each run.
func (l Layout) TempDir() string { return filepath.Join(l.Root, dirTmp) }

// LogsDir holds agent.log; RunLogsDir holds one jsonl per run. Both may carry
// paths (DH15) and never leave the device.
func (l Layout) LogsDir() string    { return filepath.Join(l.Root, dirLogs) }
func (l Layout) RunLogsDir() string { return filepath.Join(l.Root, dirLogs, "runs") }

func (l Layout) ConfigFile() string  { return filepath.Join(l.ConfigDir(), "agent.json") }
func (l Layout) JournalFile() string { return filepath.Join(l.JournalDir(), "journal.db") }
func (l Layout) AgentLog() string    { return filepath.Join(l.LogsDir(), "agent.log") }
func (l Layout) ExcludeFile() string { return filepath.Join(l.PolicyDir(), "excludes.txt") }
func (l Layout) RootsFile() string   { return filepath.Join(l.PolicyDir(), "roots.txt") }
func (l Layout) PolicyFile() string  { return filepath.Join(l.PolicyDir(), "policy.json") }
func (l Layout) ETagFile() string    { return filepath.Join(l.PolicyDir(), "etag") }

// RunLog is the path of the verbatim local log of one run.
func (l Layout) RunLog(runID string) string {
	return filepath.Join(l.RunLogsDir(), runID+".jsonl")
}

// Children lists every layout directory below the root, parents before
// children. Every one of them gets the owner and the protected DACL of
// SDDLStateDir, not only the ones holding secrets.
func (l Layout) Children() []string {
	return []string{
		l.ConfigDir(), l.SecretsDir(), l.PolicyDir(), l.JournalDir(),
		filepath.Join(l.Root, dirCache), l.CacheDir(), l.TempDir(), l.LogsDir(), l.RunLogsDir(),
	}
}

// EnsureOptions configures Ensure.
type EnsureOptions struct {
	// Restrict applies the owner and protected DACL of SDDLStateDir to one
	// existing directory: RestrictDir in production. It reports
	// ErrACLNotApplied when the process cannot do it (not elevated, no ACLs on
	// the platform). nil creates the layout without restricting it (tests).
	Restrict func(path string) error

	// elevated replaces the platform check (tests of this package only).
	elevated func() bool
	// labOwner replaces checkOwnerLab (tests of this package only).
	labOwner func(path string) error
}

// EnsureResult is what Ensure did.
type EnsureResult struct {
	// Applied: the owner and protected DACL are on the root and every child.
	Applied bool
	// Created: the root did not exist before this call.
	Created bool
}

// Ensure creates the layout and secures it (architecture §6.5). It never
// takes over or resets an existing tree: an untrusted state directory is the
// caller's to refuse or to quarantine (QuarantineRoot), not Ensure's to adopt.
//
//   - an existing layout directory (root or child) that is a reparse point is
//     refused with ErrReparsePoint, whatever the caller's privileges;
//   - when the process can restrict (elevated), a missing root and every
//     missing child are created with SDDLStateDir already set
//     (CreateDirectory with SECURITY_ATTRIBUTES), so no layout directory is
//     open even for a moment, and an existing root must be owned by SYSTEM or
//     BUILTIN\Administrators (ErrForeignOwner otherwise);
//   - when it cannot (an unelevated lab run), an existing root or child whose
//     owner can be read must be owned by SYSTEM, Administrators or the user
//     running the agent (ErrForeignOwner otherwise): the lab flag waives the
//     inability to restrict, never another account's directory. Missing
//     directories are then created plainly; the window between os.Lstat and
//     os.Mkdir exists only on this unprotected lab path;
//   - every layout directory (config, secrets, policy, journal, cache,
//     cache\restic, tmp, logs, logs\runs and the root) then gets opts.Restrict.
//
// Applied is false when the restrict hook reported ErrACLNotApplied (not
// elevated, or no ACLs on the platform): nothing is then protected, and the
// caller decides whether that is acceptable (only with the lab flag
// --insecure-unprotected-state, outside service mode). Any other hook error is
// returned.
func (l Layout) Ensure(o EnsureOptions) (EnsureResult, error) {
	var res EnsureResult
	if l.Root == "" {
		return res, errors.New("config: empty state directory")
	}
	elevated := o.elevated
	if elevated == nil {
		elevated = canRestrict
	}
	labOwner := o.labOwner
	if labOwner == nil {
		labOwner = checkOwnerLab
	}
	secure := o.Restrict != nil && elevated()
	mkdir := func(d string) error {
		if secure {
			return createRestrictedDir(d)
		}
		return os.Mkdir(d, 0o700)
	}

	st, err := os.Lstat(l.Root)
	switch {
	case err == nil:
		if isReparse(st) {
			return res, fmt.Errorf("%w: the state root", ErrReparsePoint)
		}
		if !st.IsDir() {
			return res, errors.New("config: the state root is not a directory")
		}
		if secure {
			err = checkRootOwner(l.Root)
		} else {
			err = labOwner(l.Root)
		}
		if err != nil {
			return res, err
		}
	case errors.Is(err, fs.ErrNotExist):
		if err := os.MkdirAll(filepath.Dir(l.Root), 0o755); err != nil {
			return res, fmt.Errorf("config: create the state directory's parent: %w", err)
		}
		if err := mkdir(l.Root); err != nil {
			return res, fmt.Errorf("config: create the state directory: %w", err)
		}
		res.Created = true
	default:
		return res, fmt.Errorf("config: inspect the state directory: %w", err)
	}

	for _, d := range l.Children() {
		st, err := os.Lstat(d)
		switch {
		case err == nil:
			if isReparse(st) {
				return res, fmt.Errorf("%w: %s", ErrReparsePoint, l.rel(d))
			}
			if !st.IsDir() {
				return res, fmt.Errorf("config: %s is not a directory", l.rel(d))
			}
			if !secure {
				if err := labOwner(d); err != nil {
					return res, fmt.Errorf("%s: %w", l.rel(d), err)
				}
			}
		case errors.Is(err, fs.ErrNotExist):
			if err := mkdir(d); err != nil {
				return res, fmt.Errorf("config: create %s: %w", l.rel(d), err)
			}
		default:
			return res, fmt.Errorf("config: inspect %s: %w", l.rel(d), err)
		}
	}

	if o.Restrict == nil {
		return res, nil
	}
	for _, d := range l.All() {
		if err := o.Restrict(d); err != nil {
			if errors.Is(err, ErrACLNotApplied) {
				return res, nil
			}
			return res, fmt.Errorf("config: restrict %s: %w", l.rel(d), err)
		}
	}
	res.Applied = true
	return res, nil
}

// rel names a layout directory relative to the root (for errors: the state
// directory itself may name a user's profile in a lab run).
func (l Layout) rel(d string) string {
	if r, err := filepath.Rel(l.Root, d); err == nil {
		return r
	}
	return filepath.Base(d)
}

// Config is agent.json: connection facts, never a secret (architecture §6.5).
type Config struct {
	// ServerURL is the console's agent listener base URL, https://host[:port].
	ServerURL string `json:"server_url"`
	// DeviceID is the 26-character ULID the console issued at enrollment.
	DeviceID string `json:"device_id"`
	// CustomerID is the 26-character ULID of the customer the device belongs
	// to (api.EnrollResponse.CustomerID): the snapshot tag cb.customer. An
	// identifier, not a secret.
	CustomerID string `json:"customer_id,omitempty"`
	// GatewayURL is the base URL of the restic gateway (no userinfo, no path).
	GatewayURL string `json:"gateway_url,omitempty"`
	// Custody is "escrowed" or "customer_held".
	Custody string `json:"custody,omitempty"`
	// SPKIPin is the optional base64 SHA-256 of the console's public key info.
	SPKIPin string `json:"spki_pin,omitempty"`
	// UpdateRing is the release channel (later milestone; carried only).
	UpdateRing string `json:"update_ring,omitempty"`
	// Proxy is an optional http(s) proxy URL for the uplink.
	Proxy string `json:"proxy,omitempty"`
	// InsecureLab allows an http:// console and gateway and skips TLS
	// verification of the console. It exists for lab loops and is printed by
	// "cryo-agent status".
	InsecureLab bool `json:"insecure_lab,omitempty"`
	// EnrollNonce is the idempotency key of /enroll, persisted before the first
	// attempt so a retry after a lost response reuses it (DH17).
	EnrollNonce string `json:"enroll_nonce,omitempty"`
}

// Enrolled reports whether enrollment completed (a device id is stored).
func (c Config) Enrolled() bool { return c.DeviceID != "" }

// Validate checks the connection facts.
func (c Config) Validate() error {
	if c.ServerURL != "" {
		if err := ValidateBaseURL(c.ServerURL, c.InsecureLab); err != nil {
			return fmt.Errorf("config: server_url: %w", err)
		}
	}
	if c.GatewayURL != "" {
		if err := ValidateBaseURL(c.GatewayURL, c.InsecureLab); err != nil {
			return fmt.Errorf("config: gateway_url: %w", err)
		}
	}
	if c.Proxy != "" {
		u, err := url.Parse(c.Proxy)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return errors.New("config: proxy must be an http or https URL")
		}
	}
	switch c.Custody {
	case "", "escrowed", "customer_held":
	default:
		return errors.New("config: unknown custody")
	}
	if c.CustomerID != "" && !ulid.Valid(c.CustomerID) {
		return errors.New("config: customer_id is not a ULID")
	}
	return nil
}

// ValidateBaseURL accepts https://host[:port] (and http:// only when insecure).
func ValidateBaseURL(raw string, insecure bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Hostname() == "" {
		return errors.New("must be a URL with a host")
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
	case "http":
		if !insecure {
			return errors.New("http is refused unless insecure lab mode is set")
		}
	default:
		return errors.New("scheme must be https")
	}
	if u.User != nil {
		return errors.New("must not carry userinfo")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return errors.New("must not carry a query or fragment")
	}
	return nil
}

// ErrNotFound is returned by Load when agent.json does not exist.
var ErrNotFound = errors.New("config: agent.json not found")

// Load reads and validates agent.json. Unknown fields are tolerated so a newer
// agent's file does not stop an older one from starting.
func Load(path string) (Config, error) {
	b, err := os.ReadFile(path) //nolint:gosec // path from the agent's own state directory or configuration
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, ErrNotFound
	}
	if err != nil {
		return Config{}, fmt.Errorf("config: read: %w", err)
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return Config{}, errors.New("config: agent.json is not valid JSON")
	}
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// Save writes agent.json atomically (temp file in the same directory, then
// rename) with mode 0600. The directory ACL is the real protection on Windows.
func Save(path string, c Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(path, append(b, '\n'), 0o600)
}

// WriteFileAtomic writes data to a temporary file next to path and renames it
// over path.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	cleanup := func() { _ = os.Remove(tmp) }
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		cleanup()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		cleanup()
		return err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Chmod(tmp, perm); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		cleanup()
		return err
	}
	return nil
}
