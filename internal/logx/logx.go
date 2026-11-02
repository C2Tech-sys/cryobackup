package logx

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
)

// Logger is *slog.Logger. Every console package logs through *logx.Logger and
// never names the log/slog package, which is what the repository test that
// greps for direct slog use relies on.
type Logger = slog.Logger

// Level is slog's level type.
type Level = slog.Level

// Levels.
const (
	LevelDebug = slog.LevelDebug
	LevelInfo  = slog.LevelInfo
	LevelWarn  = slog.LevelWarn
	LevelError = slog.LevelError
)

// Attr is one logging attribute. Build them only with the constructors in this
// package (ID, Class, Count, Hash, Kind, Dur, Int, Str, Err, Bool).
type Attr = slog.Attr

var proc = newRedactor()

// Handler is the path-free slog.Handler. It wraps slog.JSONHandler and, as a
// safety net behind the typed attribute constructors, rewrites every string it
// is handed: a value that looks like a path becomes "<path:hmac8>", and an
// error that carries a path (*fs.PathError, *os.LinkError, *exec.Error) is
// printed as its operation and class only.
type Handler struct {
	slog.Handler
}

// NewHandler returns a Handler that writes JSON lines to w at level or above.
func NewHandler(w io.Writer, level slog.Leveler) *Handler {
	return &Handler{Handler: slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level:       level,
		ReplaceAttr: replaceAttr,
	})}
}

// WithAttrs implements slog.Handler and keeps the redaction in place.
func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &Handler{Handler: h.Handler.WithAttrs(attrs)}
}

// WithGroup implements slog.Handler and keeps the redaction in place.
func (h *Handler) WithGroup(name string) slog.Handler {
	return &Handler{Handler: h.Handler.WithGroup(name)}
}

// Handle implements slog.Handler. The message itself is a string and goes
// through the same net (ReplaceAttr sees it as the "msg" attribute).
func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	return h.Handler.Handle(ctx, r)
}

func replaceAttr(_ []string, a slog.Attr) slog.Attr {
	a.Value = a.Value.Resolve()
	switch a.Value.Kind() {
	case slog.KindString:
		a.Value = slog.StringValue(proc.String(a.Value.String()))
	case slog.KindAny:
		switch v := a.Value.Any().(type) {
		case typed:
			// Built and checked by a typed constructor of this package
			// (ID, Class, Kind, Str, Hash, Host, Err): not free text.
			a.Value = slog.StringValue(string(v))
		case error:
			a.Value = slog.StringValue(proc.ErrorText(v))
		case fmt.Stringer:
			a.Value = slog.StringValue(proc.String(v.String()))
		case nil:
		default:
			a.Value = slog.StringValue(proc.String(fmt.Sprint(v)))
		}
	}
	return a
}

// New returns a Logger that writes path-free JSON to w at level or above.
func New(w io.Writer, level Level) *Logger {
	return slog.New(NewHandler(w, level))
}

var def atomic.Pointer[Logger]

// Install makes a path-free JSON logger on stderr the process default: both
// logx.Default() and slog.Default() (so the standard "log" package and any
// library that logs through slog) go through the Handler from now on.
func Install() *Logger { return InstallTo(os.Stderr, LevelInfo) }

// InstallTo is Install with an explicit writer and level.
func InstallTo(w io.Writer, level Level) *Logger {
	l := New(w, level)
	def.Store(l)
	slog.SetDefault(l)
	return l
}

// Default returns the installed logger, creating a stderr one on first use, so
// calling it never yields a handler that can print a raw path.
func Default() *Logger {
	if l := def.Load(); l != nil {
		return l
	}
	l := New(os.Stderr, LevelInfo)
	if def.CompareAndSwap(nil, l) {
		return l
	}
	return def.Load()
}

// typed is a value a typed constructor built and checked. The handler writes
// it as it is: the free-text rule (redact.go) would turn a ULID, a class such
// as "not_found" or "op:open class:not_exist" into a marker. Only this
// package can make one.
type typed string

func typedAttr(key, v string) Attr { return slog.Any(key, typed(v)) }

// ID is a short identifier (ULID, request id, device id): [A-Za-z0-9_.-], at
// most 64 bytes, a dot followed by a letter or digit only in a version;
// anything else is logged as "invalid".
func ID(key, v string) Attr { return typedAttr(key, safeToken(v)) }

// Class is a path-free classification (an error class, a state name) under the
// key "class".
func Class(v string) Attr { return typedAttr("class", safeToken(v)) }

// Kind is a run kind or event kind under the key "kind".
func Kind(v string) Attr { return typedAttr("kind", safeToken(v)) }

// Str is an allow-listed short token (the alphabet of ID). Free text is not
// accepted: anything else is logged as "invalid".
func Str(key, v string) Attr { return typedAttr(key, safeToken(v)) }

// Hash is a hexadecimal digest (or the first bytes of one); anything else is
// logged as "invalid".
func Hash(key, v string) Attr {
	if len(v) > 128 {
		return typedAttr(key, "invalid")
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return typedAttr(key, "invalid")
		}
	}
	return typedAttr(key, v)
}

// Host is a host name or IP literal the code took from its own configuration
// (a storage endpoint, a gateway URL): an IPv4 or IPv6 literal (brackets
// optional), or a DNS name of letters, digits and hyphens in dot-separated
// labels of at most 63 bytes, 253 in all. Anything else is logged as
// "invalid". It is what keeps a DNS or certificate outage diagnosable while
// free text is opaque. Never pass it text from a user, a request or the OS.
func Host(key, host string) Attr {
	h := strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if ip := net.ParseIP(h); ip != nil {
		return typedAttr(key, ip.String())
	}
	if len(host) > 0 && len(host) <= 253 && dnsHostRE.MatchString(host) {
		return typedAttr(key, strings.ToLower(host))
	}
	return typedAttr(key, "invalid")
}

var dnsHostRE = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?(?:\.[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*$`)

// Code is a numeric code (an exit code, an HTTP status, an OS error number).
func Code(key string, n int) Attr { return slog.Int(key, n) }

// Count is a non-negative tally.
func Count(key string, n int) Attr { return slog.Int(key, n) }

// Int is an integer (status code, exit code, port).
func Int(key string, n int) Attr { return slog.Int(key, n) }

// Bool is a flag.
func Bool(key string, b bool) Attr { return slog.Bool(key, b) }

// Dur is a duration, logged in milliseconds.
func Dur(key string, d time.Duration) Attr { return slog.Int64(key, d.Milliseconds()) }

// Err logs err under "err" as its operation and class when it carries a path,
// else as its text under the free-text rule (unchanged or one marker).
func Err(err error) Attr { return typedAttr("err", proc.ErrorText(err)) }
