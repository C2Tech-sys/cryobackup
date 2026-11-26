package cli

import (
	"context"
	"sync"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/console/health"
	"github.com/c2tech-sys/cryobackup/internal/logx"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// Drill mode is decided twice, independently: serve reads config.json (or CRYO_DRILL) and so does
// maintain, each from its own process environment. A unit's EnvironmentFile can override one and
// not the other (CRYO_DRILL=false in maintain.env beats "drill": true in the file), and then a
// drill console sits in front of a worker that runs the scheduler, the escrow verifier and the
// self-backup against production storage, or a production console in front of a worker that
// refuses everything. The worker reports its mode in its ping (adminpath.PingResult.Drill); this
// compares it with the console's and, when they differ, raises a critical banner on every page and
// logs an error. Unreachable or unanswered is not a mismatch (banner C07 and the System page's
// worker row say that). A worker too old to report the field is "unknown": a drill console treats it
// as a mismatch (it cannot be shown to be in drill mode), a production console shows a warning.

// Fixed sentences: no host, path or configuration value.
const (
	workerModeDrillConsole = "This console is a DRILL COPY but its maintenance worker is not in drill mode: the worker may run maintenance, escrow checks and the self-backup against production storage. Stop it and set drill mode for the worker too (config.json \"drill\": true, and no CRYO_DRILL=false in its environment)."
	workerModeProdConsole  = "This console is in production mode but its maintenance worker is in drill mode: it serves read methods only and runs no scheduler pass, retention, escrow check or self-backup. Turn drill mode off for the worker (config.json and CRYO_DRILL) and restart it."
	workerModeUnknown      = "Worker mode unknown (older worker): the maintenance worker does not report whether it is in drill mode, so the console cannot check it against its own. Update the worker."
)

// workerModeTTL is how long one ping answers for: the banner is asked on every evaluation.
const workerModeTTL = 30 * time.Second

// workerModeTimeout bounds one ping (the socket is local).
const workerModeTimeout = 2 * time.Second

// workerMode compares the console's drill mode with the worker's.
type workerMode struct {
	ping  func(ctx context.Context) (adminpath.PingResult, error)
	drill bool // this console's mode
	clock func() time.Time
	log   *logx.Logger

	// mu guards the cached answer only. It is never held across the ping (up to workerModeTimeout): two
	// evaluations that find the answer stale may both ping, which is harmless (a local read-only call),
	// whereas a held lock would queue every page render behind a slow worker.
	mu     sync.Mutex
	at     time.Time
	banner state.Banner // the newest answer; the zero value is "no banner"
	logged string       // the text last logged, so a standing mismatch logs once
}

// Banners is health.Evaluator.WorkerMode: the mismatch (critical) or unknown-mode (warning) banner, or nothing.
func (m *workerMode) Banners(now time.Time) []state.Banner {
	b := m.check()
	if b.ID == "" {
		return nil
	}
	return []state.Banner{b}
}

// check pings the worker unless a recent answer holds, and returns the banner it justifies.
func (m *workerMode) check() state.Banner {
	m.mu.Lock()
	if now := m.clock(); !m.at.IsZero() && now.Sub(m.at) < workerModeTTL && !now.Before(m.at) {
		b := m.banner
		m.mu.Unlock()
		return b
	}
	m.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), workerModeTimeout)
	pr, err := m.ping(ctx)
	cancel()
	var b state.Banner
	switch {
	case err != nil: // not a mismatch: C07 and the worker row say the worker is down
	case pr.Drill == nil && m.drill: // cannot be shown to be in drill mode
		b = state.Banner{ID: health.BannerWorkerMode, Severity: state.SeverityCritical, Count: 1, Text: workerModeDrillConsole}
	case pr.Drill == nil:
		b = state.Banner{ID: health.BannerWorkerModeUnknown, Severity: state.SeverityAlarm, Count: 1, Text: workerModeUnknown}
	case *pr.Drill && !m.drill:
		b = state.Banner{ID: health.BannerWorkerMode, Severity: state.SeverityCritical, Count: 1, Text: workerModeProdConsole}
	case !*pr.Drill && m.drill:
		b = state.Banner{ID: health.BannerWorkerMode, Severity: state.SeverityCritical, Count: 1, Text: workerModeDrillConsole}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.at, m.banner = m.clock(), b
	if b.Text != "" && b.Text != m.logged && m.log != nil {
		if b.Severity >= state.SeverityCritical {
			m.log.Error("the console and its maintenance worker disagree about drill mode", logx.Class("worker_drill_mismatch"))
		} else {
			m.log.Warn("the maintenance worker does not report its drill mode", logx.Class("worker_drill_unknown"))
		}
	}
	m.logged = b.Text
	return b
}
