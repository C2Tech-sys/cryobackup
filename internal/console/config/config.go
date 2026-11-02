package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// EnvPrefix is the prefix of every environment override.
const EnvPrefix = "CRYO_"

// TLS names a certificate and key file: the agent listener's (tls) or the
// admin listener's (admin.tls).
type TLS struct {
	Cert string `json:"cert"`
	Key  string `json:"key"`
}

// Admin groups the settings of the admin UI listener that are not its bind
// address (admin_listen and admin_allow_public stay flat for compatibility).
// TLS is optional: when set the admin listener serves TLS 1.2+ and the session
// cookie is Secure; when empty it is plain HTTP, which is only acceptable on a
// loopback address or across an encrypted mesh.
type Admin struct {
	TLS TLS `json:"tls"`
}

// Session sets the console session lifetimes in hours.
type Session struct {
	IdleH     int `json:"idle_h"`
	AbsoluteH int `json:"absolute_h"`
}

// RateLimits are the architecture §5.5 numbers.
type RateLimits struct {
	EnrollPerMinPerIP    int `json:"enroll_per_min_per_ip"`
	FailedAuthPerMinIP   int `json:"failed_auth_per_min_per_ip"`
	HeartbeatIntervalS   int `json:"heartbeat_interval_s"`
	RunsPerMin           int `json:"runs_per_min"`
	SnapshotsPerMin      int `json:"snapshots_per_min"`
	PolicyPerMin         int `json:"policy_per_min"`
	LoginPerMinPerIP     int `json:"login_per_min_per_ip"`
	MaxConns             int `json:"max_conns"`
	MaxBlockMinutes      int `json:"max_block_minutes"`
	LoginLockAfter       int `json:"login_lock_after"`
	LoginLockMinutes     int `json:"login_lock_minutes"`
	CommandsLongPollMaxS int `json:"commands_long_poll_max_s"`
}

// RestoreStream is the minimum-rate guard of a technician download (ADR 0008
// streams every restore; M1-HIPAA2): once the first byte went out, a download
// that delivers fewer than MinBytes to the browser in any WindowS seconds is
// aborted and recorded with class too_slow, so a slow reader cannot hold one of
// the maintenance worker's restic slots for hours. Defaults 65536 bytes in 120 s.
// FirstByteS bounds the time from the start of a download to its first byte
// (default 300 s): a restic that hangs before it produces anything is cancelled
// and the restore records class no_data. The number of concurrent downloads is
// the worker's CRYO_MAINTAIN_MAX_STREAMS.
type RestoreStream struct {
	MinBytes   int `json:"min_bytes"`
	WindowS    int `json:"window_s"`
	FirstByteS int `json:"first_byte_s"`
}

// SMTP is accepted and validated in M0 but not used until M4. The password is
// never in this file: password_file names a mounted file.
type SMTP struct {
	Host         string `json:"host"`
	Port         int    `json:"port"`
	Security     string `json:"security"` // starttls | tls | plain
	Username     string `json:"username"`
	PasswordFile string `json:"password_file"`
	From         string `json:"from"`
}

// Config is the console configuration. The file is JSON (the standard library
// has no TOML parser, and a dependency is something the operator patches); every
// key has a CRYO_* environment override named after its path, for example
// tls.cert is CRYO_TLS_CERT and rate_limits.runs_per_min is
// CRYO_RATE_LIMITS_RUNS_PER_MIN.
type Config struct {
	DataDir          string        `json:"data_dir"`
	DBPath           string        `json:"db_path"`
	MasterKeyPath    string        `json:"master_key_path"`
	AgentListen      string        `json:"agent_listen"`
	AdminListen      string        `json:"admin_listen"`
	AdminAllowPublic bool          `json:"admin_allow_public"`
	InsecureHTTPLab  bool          `json:"insecure_http_for_lab"`
	TLS              TLS           `json:"tls"`
	Admin            Admin         `json:"admin"`
	PublicURL        string        `json:"public_url"`
	GatewayURL       string        `json:"gateway_url"`
	GatewayAuthDir   string        `json:"gateway_auth_dir"`
	ArtifactsDir     string        `json:"artifacts_dir"` // where the operator places the agent MSI (shown on /downloads, hashed for the deployment snippets) and agent releases (M11); never restore data (ADR 0008)
	AdminpathSocket  string        `json:"adminpath_socket"`
	DisplayTimezone  string        `json:"display_timezone"`
	RefreshIntervalS int           `json:"refresh_interval_s"`
	Session          Session       `json:"session"`
	RateLimits       RateLimits    `json:"rate_limits"`
	RestoreStream    RestoreStream `json:"restore_stream"`
	SMTP             SMTP          `json:"smtp"`
	Fixtures         bool          `json:"fixtures"`
}

// Default returns the defaults: loopback admin listener, public agent listener
// on 8443, America/Chicago display zone, 30 s refresh, 12 h idle and 7 d
// absolute sessions and the architecture §5.5 limits. Path keys default to
// locations under DataDir (resolved by Resolve).
func Default() Config {
	return Config{
		DataDir:          "./data",
		AgentListen:      ":8443",
		AdminListen:      "127.0.0.1:8080",
		DisplayTimezone:  "America/Chicago",
		RefreshIntervalS: 30,
		Session:          Session{IdleH: 12, AbsoluteH: 168},
		RateLimits: RateLimits{
			EnrollPerMinPerIP:    10,
			FailedAuthPerMinIP:   30,
			HeartbeatIntervalS:   20,
			RunsPerMin:           30,
			SnapshotsPerMin:      6,
			PolicyPerMin:         6,
			LoginPerMinPerIP:     10,
			MaxConns:             1024,
			MaxBlockMinutes:      15,
			LoginLockAfter:       5,
			LoginLockMinutes:     15,
			CommandsLongPollMaxS: 30,
		},
		RestoreStream: RestoreStream{MinBytes: 64 << 10, WindowS: 120, FirstByteS: 300},
		SMTP:          SMTP{Security: "starttls"},
	}
}

// Resolve fills the path keys that were left empty from DataDir: db_path,
// master_key_path, artifacts_dir, adminpath_socket and gateway_auth_dir.
func (c *Config) Resolve() {
	j := func(name string) string { return filepath.Join(c.DataDir, name) }
	if c.DBPath == "" {
		c.DBPath = j("console.db")
	}
	if c.MasterKeyPath == "" {
		c.MasterKeyPath = j("master.key")
	}
	if c.ArtifactsDir == "" {
		c.ArtifactsDir = j("artifacts")
	}
	if c.AdminpathSocket == "" {
		c.AdminpathSocket = j("adminpath.sock")
	}
	if c.GatewayAuthDir == "" {
		c.GatewayAuthDir = j("gateway-auth")
	}
}

// Load reads path (empty means "no file": defaults plus environment), strictly
// (unknown keys are an error: a typo must not silently fall back to a default),
// applies the CRYO_* overrides from getenv (nil means os.Getenv), resolves the
// path defaults and validates the result.
func Load(path string, getenv func(string) string) (Config, error) {
	c, err := Read(path, getenv)
	if err != nil {
		return Config{}, err
	}
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// Read is Load without Validate: the file (strictly), the CRYO_* overrides and
// the path defaults. The CLI uses it so command-line flags can still adjust the
// configuration before it is validated.
func Read(path string, getenv func(string) string) (Config, error) {
	c := Default()
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return Config{}, fmt.Errorf("config: reading the config file: %w", classify(err))
		}
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			return Config{}, fmt.Errorf("config: parsing the config file: %w", err)
		}
	}
	if getenv == nil {
		getenv = os.Getenv
	}
	if err := ApplyEnv(&c, getenv); err != nil {
		return Config{}, err
	}
	c.Resolve()
	return c, nil
}

// classify strips the path out of a *fs.PathError so the text can be printed.
func classify(err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}

// EnvName returns the environment variable that overrides the key at the given
// JSON path, for example EnvName("tls", "cert") is "CRYO_TLS_CERT".
func EnvName(path ...string) string {
	return EnvPrefix + strings.ToUpper(strings.Join(path, "_"))
}

// EnvKeys lists every environment override with the JSON key it sets, in field
// order. A test uses it to prove that every key in config.example.json has one.
func EnvKeys() map[string]string {
	out := map[string]string{}
	walk(reflect.TypeOf(Config{}), nil, func(path []string, _ reflect.StructField) {
		out[EnvName(path...)] = strings.Join(path, ".")
	})
	return out
}

func walk(t reflect.Type, prefix []string, fn func(path []string, f reflect.StructField)) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name := strings.Split(f.Tag.Get("json"), ",")[0]
		if name == "" || name == "-" {
			continue
		}
		path := append(append([]string(nil), prefix...), name)
		if f.Type.Kind() == reflect.Struct {
			walk(f.Type, path, fn)
			continue
		}
		fn(path, f)
	}
}

// ApplyEnv applies every CRYO_* override found through getenv.
func ApplyEnv(c *Config, getenv func(string) string) error {
	return applyEnv(reflect.ValueOf(c).Elem(), nil, getenv)
}

func applyEnv(v reflect.Value, prefix []string, getenv func(string) string) error {
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name := strings.Split(f.Tag.Get("json"), ",")[0]
		if name == "" || name == "-" {
			continue
		}
		path := append(append([]string(nil), prefix...), name)
		fv := v.Field(i)
		if f.Type.Kind() == reflect.Struct {
			if err := applyEnv(fv, path, getenv); err != nil {
				return err
			}
			continue
		}
		key := EnvName(path...)
		raw, ok := lookup(getenv, key)
		if !ok {
			continue
		}
		switch f.Type.Kind() {
		case reflect.String:
			fv.SetString(raw)
		case reflect.Int:
			n, err := strconv.Atoi(strings.TrimSpace(raw))
			if err != nil {
				return fmt.Errorf("config: %s must be an integer", key)
			}
			fv.SetInt(int64(n))
		case reflect.Bool:
			b, err := strconv.ParseBool(strings.TrimSpace(raw))
			if err != nil {
				return fmt.Errorf("config: %s must be true or false", key)
			}
			fv.SetBool(b)
		default:
			return fmt.Errorf("config: %s has an unsupported type", key)
		}
	}
	return nil
}

func lookup(getenv func(string) string, key string) (string, bool) {
	v := getenv(key)
	return v, v != ""
}

// Validate checks the configuration: required values, listener addresses, the
// rule that plain HTTP on the agent listener (insecure_http_for_lab) is allowed
// only on a loopback or private bind, the display zone, session and rate-limit
// ranges and, when present, the SMTP block. It does not touch the filesystem.
func (c Config) Validate() error {
	var problems []string
	add := func(format string, a ...any) { problems = append(problems, fmt.Sprintf(format, a...)) }

	if c.DataDir == "" {
		add("data_dir is required")
	}
	if !c.Fixtures {
		if c.DBPath == "" {
			add("db_path is required")
		}
		if c.MasterKeyPath == "" {
			add("master_key_path is required")
		}
	}
	if err := checkListen("agent_listen", c.AgentListen); err != nil {
		add("%v", err)
	}
	if err := checkListen("admin_listen", c.AdminListen); err != nil {
		add("%v", err)
	}
	if !c.AdminAllowPublic && !IsLoopbackOrPrivate(c.AdminListen) {
		add("admin_listen %q is not a loopback or private address: bind it to 127.0.0.1 or a mesh/private IP, or set admin_allow_public", c.AdminListen)
	}
	if !c.Fixtures {
		hasCert, hasKey := c.TLS.Cert != "", c.TLS.Key != ""
		switch {
		case hasCert != hasKey:
			add("tls.cert and tls.key must be set together")
		case !hasCert && !c.InsecureHTTPLab:
			add("tls.cert and tls.key are required for the agent listener (or run with insecure_http_for_lab on a loopback or private bind)")
		}
	}
	if (c.Admin.TLS.Cert != "") != (c.Admin.TLS.Key != "") {
		add("admin.tls.cert and admin.tls.key must be set together")
	}
	if c.InsecureHTTPLab && !IsLoopbackOrPrivate(c.AgentListen) {
		add("insecure_http_for_lab is allowed only when agent_listen is a loopback or private address, not %q", c.AgentListen)
	}
	if c.PublicURL != "" {
		if _, err := ParsePublicURL(c.PublicURL); err != nil {
			add("%s", err.Error())
		}
	}
	if c.GatewayURL != "" {
		if u, err := url.Parse(c.GatewayURL); err != nil || u.Scheme != "https" || u.Host == "" {
			add("gateway_url must be an https URL")
		}
	}
	if _, err := time.LoadLocation(c.DisplayTimezone); err != nil || c.DisplayTimezone == "" {
		add("display_timezone %q is not an IANA zone", c.DisplayTimezone)
	}
	if c.RefreshIntervalS < 5 || c.RefreshIntervalS > 3600 {
		add("refresh_interval_s must be between 5 and 3600")
	}
	if c.Session.IdleH < 1 || c.Session.AbsoluteH < c.Session.IdleH || c.Session.AbsoluteH > 24*90 {
		add("session.idle_h must be at least 1 and no more than session.absolute_h (at most 2160)")
	}
	rl := c.RateLimits
	for name, v := range map[string]int{
		"rate_limits.enroll_per_min_per_ip": rl.EnrollPerMinPerIP, "rate_limits.failed_auth_per_min_per_ip": rl.FailedAuthPerMinIP,
		"rate_limits.heartbeat_interval_s": rl.HeartbeatIntervalS, "rate_limits.runs_per_min": rl.RunsPerMin,
		"rate_limits.snapshots_per_min": rl.SnapshotsPerMin, "rate_limits.policy_per_min": rl.PolicyPerMin,
		"rate_limits.login_per_min_per_ip": rl.LoginPerMinPerIP, "rate_limits.max_conns": rl.MaxConns,
		"rate_limits.max_block_minutes": rl.MaxBlockMinutes, "rate_limits.login_lock_after": rl.LoginLockAfter,
		"rate_limits.login_lock_minutes": rl.LoginLockMinutes, "rate_limits.commands_long_poll_max_s": rl.CommandsLongPollMaxS,
	} {
		if v < 1 {
			add("%s must be at least 1", name)
		}
	}
	if rl.CommandsLongPollMaxS > 30 {
		add("rate_limits.commands_long_poll_max_s must not exceed 30")
	}
	if c.RestoreStream.MinBytes < 1 {
		add("restore_stream.min_bytes must be at least 1")
	}
	if c.RestoreStream.WindowS < 10 || c.RestoreStream.WindowS > 3600 {
		add("restore_stream.window_s must be between 10 and 3600")
	}
	if c.RestoreStream.FirstByteS < 10 || c.RestoreStream.FirstByteS > 3600 {
		add("restore_stream.first_byte_s must be between 10 and 3600")
	}
	if c.SMTP.Host != "" {
		if c.SMTP.Port < 1 || c.SMTP.Port > 65535 {
			add("smtp.port must be between 1 and 65535")
		}
		switch c.SMTP.Security {
		case "starttls", "tls", "plain":
		default:
			add("smtp.security must be starttls, tls or plain")
		}
		if c.SMTP.From == "" || !strings.Contains(c.SMTP.From, "@") {
			add("smtp.from must be an e-mail address")
		}
	}
	if len(problems) > 0 {
		return &Error{Problems: problems}
	}
	return nil
}

// Error lists every problem Validate found.
type Error struct{ Problems []string }

// Error implements error.
func (e *Error) Error() string {
	return "config: invalid configuration: " + strings.Join(e.Problems, "; ")
}

func checkListen(key, addr string) error {
	if addr == "" {
		return fmt.Errorf("%s is required", key)
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%s %q must be host:port", key, addr)
	}
	_ = host
	if n, err := strconv.Atoi(port); err != nil || n < 0 || n > 65535 {
		return fmt.Errorf("%s %q has an invalid port", key, addr)
	}
	return nil
}

// cgnat is the shared address space of RFC 6598, 100.64.0.0/10, which
// Tailscale and Headscale meshes assign to their nodes. net.IP.IsPrivate does
// not cover it (it is not RFC 1918), so it is named here.
var cgnat = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

// IsLoopbackOrPrivate reports whether the host of a host:port address (a bind
// address or a peer address) is a loopback, private (RFC 1918, RFC 4193),
// link-local or shared-address-space (RFC 6598, 100.64.0.0/10, the
// Tailscale/Headscale mesh range) IP literal, or "localhost". An empty host
// (all interfaces) and any other name are public.
func IsLoopbackOrPrivate(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || cgnat.Contains(ip)
}
