package restic

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// ErrInvalidOption is wrapped by every error that a builder or helper returns
// for a bad argument. The message names the field, never its value, so it is
// safe to log.
var ErrInvalidOption = errors.New("restic: invalid option")

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidOption, fmt.Sprintf(format, args...))
}

// Env is the process environment of one restic invocation (DH3). The
// repository location, the repository password and every path restic uses for
// scratch space travel here and never in argv, so nothing secret shows up in a
// process listing.
type Env struct {
	// Repository is RESTIC_REPOSITORY, for example the value that
	// GatewayRepository or AdminRepository returns. It may carry credentials.
	Repository string
	// Password is RESTIC_PASSWORD, the repository password.
	Password string
	// CacheDir is RESTIC_CACHE_DIR. Required unless NoCache is set: restic's
	// default would land in the SYSTEM profile (engine §1.16, architecture
	// §6.3).
	CacheDir string
	// NoCache is the explicit no-cache form (ADR 0008: the console host keeps
	// no repository data on disk). CacheDir must then be empty, and Environ
	// exports no RESTIC_CACHE_DIR at all. The environment cannot switch the
	// cache off by itself: the caller must also put NoCacheFlag on every
	// command line (restic.Common.Extra). The agent never sets it.
	NoCache bool
	// TempDir is exported as TEMP and TMP so restic's pack temp files land on
	// a volume the operator sized for them (engine §1.6). When empty, TEMP and
	// TMP are removed from the result and the platform default applies.
	TempDir string
	// GOMAXPROCS caps the Go scheduler of the child when greater than zero;
	// zero leaves it unset.
	GOMAXPROCS int
}

// NoCacheFlag is restic's global flag that turns the local cache off. A
// caller that sets Env.NoCache passes it on every command line.
const NoCacheFlag = "--no-cache"

// Environ returns base with every RESTIC_*, TEMP, TMP and GOMAXPROCS entry
// removed (matched case-insensitively, because Windows treats names that way),
// followed by this Env's entries. It returns an error when Repository or
// Password is empty, when CacheDir is empty without NoCache (or set with it),
// or when a value contains a NUL byte. The error never contains a value.
// Entries Windows keeps for itself (names that start with '=') are passed
// through untouched.
func (e Env) Environ(base []string) ([]string, error) {
	switch {
	case e.Repository == "":
		return nil, invalid("env: repository is required")
	case e.Password == "":
		return nil, invalid("env: password is required")
	case e.NoCache && e.CacheDir != "":
		return nil, invalid("env: cache dir and no-cache are mutually exclusive")
	case e.CacheDir == "" && !e.NoCache:
		return nil, invalid("env: cache dir is required (never restic's default)")
	case e.GOMAXPROCS < 0:
		return nil, invalid("env: GOMAXPROCS must not be negative")
	}
	for _, v := range []string{e.Repository, e.Password, e.CacheDir, e.TempDir} {
		if strings.ContainsRune(v, 0) {
			return nil, invalid("env: value contains a NUL byte")
		}
	}

	out := make([]string, 0, len(base)+6)
	for _, kv := range base {
		if ownedEnvName(kv) {
			continue
		}
		out = append(out, kv)
	}
	out = append(out,
		"RESTIC_REPOSITORY="+e.Repository,
		"RESTIC_PASSWORD="+e.Password,
	)
	if !e.NoCache {
		out = append(out, "RESTIC_CACHE_DIR="+e.CacheDir)
	}
	if e.TempDir != "" {
		out = append(out, "TEMP="+e.TempDir, "TMP="+e.TempDir)
	}
	if e.GOMAXPROCS > 0 {
		out = append(out, "GOMAXPROCS="+strconv.Itoa(e.GOMAXPROCS))
	}
	return out, nil
}

// ownedEnvName reports whether the KEY=VALUE entry names a variable that
// Environ manages.
func ownedEnvName(kv string) bool {
	if kv == "" || kv[0] == '=' {
		return false
	}
	name := kv
	if i := strings.IndexByte(kv, '='); i >= 0 {
		name = kv[:i]
	}
	name = strings.ToUpper(name)
	return name == "TEMP" || name == "TMP" || name == "GOMAXPROCS" || strings.HasPrefix(name, "RESTIC_")
}

// validDeviceID reports whether id matches ^[A-Za-z0-9._-]{1,64}$ and is not a
// dot-only name (it becomes a URL path segment and a bucket path segment).
func validDeviceID(id string) bool {
	if len(id) < 1 || len(id) > 64 || strings.Trim(id, ".") == "" {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// GatewayRepository builds the RESTIC_REPOSITORY value an endpoint uses:
//
//	rest:https://<deviceID>:<password>@<host[:port]>/<deviceID>/
//
// The trailing slash is mandatory (engine §3.1). baseURL must be a scheme and
// host[:port] only: no path (a single "/" is tolerated), query, fragment or
// userinfo. The scheme must be https; http is accepted only when allowInsecure
// is set (lab use). deviceID must match ^[A-Za-z0-9._-]{1,64}$. The password
// is URL-escaped even though gateway passwords are hex. The result contains a
// secret: it belongs in Env.Repository and nowhere else.
func GatewayRepository(baseURL, deviceID, gatewayPassword string, allowInsecure bool) (string, error) {
	if !validDeviceID(deviceID) {
		return "", invalid("gateway repository: device id must match ^[A-Za-z0-9._-]{1,64}$")
	}
	if gatewayPassword == "" {
		return "", invalid("gateway repository: gateway password is required")
	}
	if strings.ContainsAny(baseURL, "?#") {
		return "", invalid("gateway repository: base URL must not carry a query or fragment")
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return "", invalid("gateway repository: base URL does not parse")
	}
	scheme := strings.ToLower(u.Scheme)
	switch {
	case scheme == "https":
	case scheme == "http" && allowInsecure:
	case scheme == "http":
		return "", invalid("gateway repository: http is refused unless insecure mode is allowed")
	default:
		return "", invalid("gateway repository: base URL scheme must be https")
	}
	if u.Opaque != "" || u.Host == "" || u.Hostname() == "" {
		return "", invalid("gateway repository: base URL needs a host")
	}
	if u.User != nil {
		return "", invalid("gateway repository: base URL must not carry userinfo")
	}
	if u.Path != "" && u.Path != "/" || u.RawPath != "" && u.RawPath != "/" {
		return "", invalid("gateway repository: base URL must not carry a path")
	}
	cred := url.UserPassword(deviceID, gatewayPassword).String()
	return "rest:" + scheme + "://" + cred + "@" + u.Host + "/" + deviceID + "/", nil
}

// AdminRepository builds the maintenance-path repository location, which goes
// through restic's rclone backend (architecture §7.3):
//
//	rclone:<remote>:<bucket>/<prefix>/<deviceID>
//
// An empty prefix gives rclone:<remote>:<bucket>/<deviceID>. remote and bucket
// are required; remote must not contain ':'. Leading and trailing '/' on
// bucket and prefix are dropped. No segment may be ".." and none may contain a
// control character.
func AdminRepository(remote, bucket, prefix, deviceID string) (string, error) {
	if !validDeviceID(deviceID) {
		return "", invalid("admin repository: device id must match ^[A-Za-z0-9._-]{1,64}$")
	}
	if remote == "" || strings.ContainsRune(remote, ':') || strings.ContainsRune(remote, '/') || hasControl(remote) {
		return "", invalid("admin repository: remote is required and must not contain ':' or '/'")
	}
	bucket = strings.Trim(bucket, "/")
	prefix = strings.Trim(prefix, "/")
	if bucket == "" {
		return "", invalid("admin repository: bucket is required")
	}
	for _, part := range []string{bucket, prefix} {
		if hasControl(part) {
			return "", invalid("admin repository: bucket and prefix must not contain control characters")
		}
		for _, seg := range strings.Split(part, "/") {
			if seg == ".." {
				return "", invalid("admin repository: bucket and prefix must not contain '..'")
			}
		}
	}
	loc := bucket
	if prefix != "" {
		loc += "/" + prefix
	}
	return "rclone:" + remote + ":" + loc + "/" + deviceID, nil
}

func hasControl(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f {
			return true
		}
	}
	return false
}

// AdminOptions returns the restic -o options that point the rclone backend at
// the admin rclone (architecture §7.3). bypassGovernance appends
// --s3-bypass-governance-retention to rclone's arguments and must be set only
// for the console maintenance worker against a governance-mode bucket
// (architecture §7.3 step 10). The result is meant for Common.Extra.
func AdminOptions(rcloneProgram string, bypassGovernance bool) []string {
	args := "serve restic --stdio"
	if bypassGovernance {
		args += " --s3-bypass-governance-retention"
	}
	return []string{"-o", "rclone.program=" + rcloneProgram, "-o", "rclone.args=" + args}
}
