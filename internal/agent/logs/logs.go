// Package logs holds the device-local logging plumbing: a size-rotating file
// writer for logs\agent.log (10 MiB x 5, architecture §6.5) behind a log/slog
// JSON handler, and the retention sweep of logs\runs (30 days).
//
// Everything written through this package is path-bearing and local-only
// (DH15): it lives under the state directory with the SYSTEM + Administrators
// ACL and is never uploaded, never part of a diagnostics bundle. The shared
// path-free internal/logx boundary is for output that leaves the device and is
// not needed on the endpoint.
package logs

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Default rotation of agent.log (architecture §6.5).
const (
	DefaultMaxBytes = 10 << 20
	DefaultKeep     = 5
	// RunLogRetention is how long logs\runs\<id>.jsonl are kept.
	RunLogRetention = 30 * 24 * time.Hour
)

// Rotating is an io.WriteCloser that rotates path to path.1 ... path.<keep-1>
// when a write would take it past MaxBytes. It is safe for concurrent use.
type Rotating struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	keep     int
	f        *os.File
	size     int64
}

// OpenRotating opens (appending to) the log file at path.
func OpenRotating(path string, maxBytes int64, keep int) (*Rotating, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	if keep < 1 {
		keep = DefaultKeep
	}
	r := &Rotating{path: path, maxBytes: maxBytes, keep: keep}
	if err := r.open(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *Rotating) open() error {
	if err := os.MkdirAll(filepath.Dir(r.path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // path from the agent's own state directory or configuration
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	r.f, r.size = f, st.Size()
	return nil
}

// Write implements io.Writer. Every write is flushed to disk (FlushFileBuffers
// on Windows) before it returns. Without that, NTFS leaves the size in the
// directory entry of a file that existed before this process opened it at its
// old value for as long as the handle stays open: dir, Explorer and an RMM's
// file check showed the service's agent.log as 0 bytes for hours while every
// line was in it (L-LOG-1). The flush also keeps the lines of a process that
// dies with the machine. The log is a few lines per backup, so the cost is
// negligible.
func (r *Rotating) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return 0, os.ErrClosed
	}
	if r.size > 0 && r.size+int64(len(p)) > r.maxBytes {
		if err := r.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	if err == nil {
		_ = r.f.Sync() // the line is written; a failed flush only delays it
	}
	return n, err
}

func (r *Rotating) rotate() error {
	_ = r.f.Close()
	r.f = nil
	// path.<keep-1> is dropped, the rest shift up by one.
	_ = os.Remove(r.path + "." + strconv.Itoa(r.keep-1))
	for i := r.keep - 2; i >= 1; i-- {
		_ = os.Rename(r.path+"."+strconv.Itoa(i), r.path+"."+strconv.Itoa(i+1))
	}
	if r.keep > 1 {
		_ = os.Rename(r.path, r.path+".1")
	} else {
		_ = os.Remove(r.path)
	}
	return r.open()
}

// Close closes the file.
func (r *Rotating) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return nil
	}
	err := r.f.Close()
	r.f = nil
	return err
}

// NewLogger returns a JSON slog logger writing to w at the given level.
func NewLogger(w io.Writer, level slog.Level) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level}))
}

// Discard is a logger that drops everything (tests).
func Discard() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

// PruneRunLogs removes run logs (*.jsonl) in dir whose modification time is
// older than maxAge, and returns how many it removed.
func PruneRunLogs(dir string, now time.Time, maxAge time.Duration) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	removed := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) > maxAge {
			if os.Remove(filepath.Join(dir, e.Name())) == nil {
				removed++
			}
		}
	}
	return removed, nil
}
