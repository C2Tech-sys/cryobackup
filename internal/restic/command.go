package restic

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Command is one restic invocation: which operation it is and the argv after
// the program name. Args carries no secrets (the repository and its password
// travel in Env) but it may carry paths: roots, the exclude file, a restore
// target, a snapshot subfolder. Never log it.
type Command struct {
	Kind Kind
	Args []string
}

// Common holds the options every command accepts. The zero value adds nothing.
type Common struct {
	// RetryLock renders --retry-lock when greater than zero: how long restic
	// keeps retrying a locked repository before exiting 11.
	RetryLock time.Duration
	// LimitUpload renders --limit-upload (KiB/s) when greater than zero.
	LimitUpload int
	// LimitDownload renders --limit-download (KiB/s) when greater than zero.
	LimitDownload int
	// Extra is appended verbatim after the other common flags, for example
	// AdminOptions(...). It is checked against the forbidden-flag list.
	Extra []string
}

// forbiddenFlags is the list of flags that no builder ever emits and that
// Common.Extra may not smuggle in (architecture §6.3, engine §1.4-§1.5,
// ticket P1-T1 constraints). "--remove-all" is forbidden here because the only
// legitimate source is Unlock(removeAll=true), which renders it itself.
var forbiddenFlags = []string{
	"--one-file-system",
	"--no-extra-verify",
	"--ignore-inode",
	"--ignore-ctime",
	"--unsafe-allow-remove-all",
	"--delete",
	"--prune",
	"--remove-all",
	"--recursive",
	"--insecure-no-password",
	"--password-file",
	"--password-command",
	"--repo",
	"-r",
	"--repository-file",
	"-p",
}

// ForbiddenFlags returns a copy of the flags that builders never emit.
func ForbiddenFlags() []string {
	return append([]string(nil), forbiddenFlags...)
}

// forbiddenArg reports whether arg is (or begins with) a forbidden flag. A flag
// matches as the whole element, as "flag=value", and, for the one-letter
// flags, as "-rVALUE" / "-pVALUE".
func forbiddenArg(arg string) bool {
	for _, f := range forbiddenFlags {
		if arg == f || strings.HasPrefix(arg, f+"=") {
			return true
		}
		if len(f) == 2 && strings.HasPrefix(arg, f) && !strings.HasPrefix(arg, "--") {
			return true
		}
	}
	return false
}

// validate checks the common options.
func (c Common) validate() error {
	if c.RetryLock < 0 {
		return invalid("retry lock must not be negative")
	}
	if c.LimitUpload < 0 || c.LimitDownload < 0 {
		return invalid("bandwidth limits must not be negative")
	}
	for _, a := range c.Extra {
		if a == "--" {
			return invalid("extra arguments must not contain the -- separator")
		}
		if forbiddenArg(a) {
			return invalid("extra arguments contain a forbidden flag")
		}
	}
	return nil
}

// limits appends --limit-upload and --limit-download.
func (c Common) limits(args []string) []string {
	if c.LimitUpload > 0 {
		args = append(args, "--limit-upload", strconv.Itoa(c.LimitUpload))
	}
	if c.LimitDownload > 0 {
		args = append(args, "--limit-download", strconv.Itoa(c.LimitDownload))
	}
	return args
}

// retry appends --retry-lock.
func (c Common) retry(args []string) []string {
	if c.RetryLock > 0 {
		args = append(args, "--retry-lock", formatDuration(c.RetryLock))
	}
	return args
}

// tail appends the common flags in their standard place: after the
// command-specific flags, in the order --limit-upload, --limit-download,
// --retry-lock, Extra (ticket §1).
func (c Common) tail(args []string) []string {
	args = c.limits(args)
	args = c.retry(args)
	return append(args, c.Extra...)
}

// formatDuration renders d the way the architecture writes it ("<n>s") when it
// is a whole number of seconds, and in Go syntax otherwise. Both forms are
// accepted by restic (it uses time.ParseDuration).
func formatDuration(d time.Duration) string {
	if d%time.Second == 0 {
		return strconv.FormatInt(int64(d/time.Second), 10) + "s"
	}
	return d.String()
}

// hasControlOrEmpty reports whether s is empty or holds a control character.
func hasControlOrEmpty(s string) bool {
	return s == "" || hasControl(s)
}

// validSnapshotRef accepts "latest" or a hex snapshot id of 8 to 64 digits,
// and, when allowSub is set, a trailing ":<subfolder>" (engine §1.12). Because
// the first character is never '-', the value cannot be read as a flag.
func validSnapshotRef(s string, allowSub bool) bool {
	id, sub, hasSub := strings.Cut(s, ":")
	if hasSub {
		if !allowSub || sub == "" || hasControl(sub) {
			return false
		}
	}
	if id == "latest" {
		return true
	}
	if len(id) < 8 || len(id) > 64 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

var (
	keepWithinRE = regexp.MustCompile(`^([0-9]+y)?([0-9]+m)?([0-9]+d)?([0-9]+h)?$`)
	// MaxUnused: "unlimited", a percentage, or a byte count with an optional k/m/g/t suffix.
	maxUnusedRE = regexp.MustCompile(`^(unlimited|[0-9]+(\.[0-9]+)?%|[0-9]+[kKmMgGtT]?)$`)
	sizeRE      = regexp.MustCompile(`^[0-9]+[kKmMgGtT]?$`)
	// ReadDataSubset: "n/t", "x%" or a size such as "10G".
	subsetFracRE = regexp.MustCompile(`^([0-9]+)/([0-9]+)$`)
	subsetPctRE  = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?%$`)
)
