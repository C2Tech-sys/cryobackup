package server

import (
	stdlog "log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/logx"
)

// The http.Server ErrorLog of both listeners.
//
// net/http reports a failed TLS handshake as free text through
// http.Server.ErrorLog: "http: TLS handshake error from <ip:port>: <reason>".
// An internet scanner hitting the public agent listener produces one such line
// per probe. Left at the default, the text went through the path guard of the
// process logger and came out as a bare "<path:...>" message (the reason text
// holds slashes), which says nothing and still carries a peer address.
//
// errorLog replaces that with typed, path-free records:
//
//   - class tls_handshake_failed: counted always, logged at DEBUG per event and
//     at WARN as one summary line per interval carrying the count since the
//     last summary. Neither the remote address nor the reason is ever logged.
//   - class http_server_error: every other message net/http reports, reduced
//     to a fixed kind (accept, panic, write_header, http2, other) and rate
//     limited the same way. The text is dropped for the same reason.
const (
	// ClassTLSHandshakeFailed is the log class of a failed TLS handshake.
	ClassTLSHandshakeFailed = "tls_handshake_failed"
	// ClassHTTPServerError is the log class of every other net/http server error.
	ClassHTTPServerError = "http_server_error"

	// errorLogInterval is the least time between two WARN summaries of a class.
	errorLogInterval = time.Minute
)

type errorLog struct {
	log      *logx.Logger
	listener string // "agent" or "admin"
	now      func() time.Time

	tlsFailed atomic.Int64
	other     atomic.Int64

	mu   sync.Mutex
	last map[string]time.Time // class -> time of the last WARN summary
	owed map[string]int       // class -> events since that summary
}

func newErrorLog(log *logx.Logger, listener string, now func() time.Time) *errorLog {
	if log == nil {
		log = logx.Default()
	}
	if now == nil {
		now = time.Now
	}
	return &errorLog{log: log, listener: listener, now: now, last: map[string]time.Time{}, owed: map[string]int{}}
}

// Logger is the *log.Logger to set as http.Server.ErrorLog.
func (e *errorLog) Logger() *stdlog.Logger { return stdlog.New(e, "", 0) }

// TLSHandshakeFailures is the number of failed handshakes seen so far.
func (e *errorLog) TLSHandshakeFailures() int64 { return e.tlsFailed.Load() }

// ServerErrors is the number of other net/http server errors seen so far.
func (e *errorLog) ServerErrors() int64 { return e.other.Load() }

// Write implements io.Writer for the standard logger: one call per message.
func (e *errorLog) Write(p []byte) (int, error) {
	class, kind := classify(string(p))
	if class == ClassTLSHandshakeFailed {
		e.tlsFailed.Add(1)
	} else {
		e.other.Add(1)
	}
	e.log.Debug("http server event", logx.Str("listener", e.listener), logx.Class(class), logx.Str("kind", kind))
	e.summarise(class, kind)
	return len(p), nil
}

// summarise logs the WARN line of a class and kind when its interval has passed.
// Each (class, kind) has its own interval, so a rare certificate problem is not
// hidden behind a flood of scanner noise.
func (e *errorLog) summarise(class, kind string) {
	key := class + "/" + kind
	now := e.now()
	e.mu.Lock()
	e.owed[key]++
	if last, ok := e.last[key]; ok && now.Sub(last) < errorLogInterval {
		e.mu.Unlock()
		return
	}
	n := e.owed[key]
	e.owed[key], e.last[key] = 0, now
	e.mu.Unlock()
	e.log.Warn("http server error", logx.Str("listener", e.listener), logx.Class(class), logx.Str("kind", kind), logx.Count("count", n),
		logx.Dur("window", errorLogInterval))
}
