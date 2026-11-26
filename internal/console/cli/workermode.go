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
// worker row say that); a worker too old to report the field reads as not in drill.

// Fixed sentences: no host, path or configuration value.
const (
	workerModeDrillConsole = "This console is a DRILL COPY but its maintenance worker is not in drill mode: the worker may run maintenance, escrow checks and the self-backup against production storage. Stop it and set drill mode for the worker too (config.json \"drill\": true, and no CRYO_DRILL=false in its environment)."
	workerModeProdConsole  = "This console is in production mode but its maintenance worker is in drill mode: it serves read methods only and runs no scheduler pass, retention, escrow check or self-backup. Turn drill mode off for the worker (config.json and CRYO_DRILL) and restart it."
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

	mu     sync.Mutex
	at     time.Time
	text   string // the banner text of the newest answer ("" none)
	logged string // the text last logged, so a standing mismatch logs once
}

// Banners is health.Evaluator.WorkerMode: the mismatch banner, or nothing.
func (m *workerMode) Banners(now time.Time) []state.Banner {
	text := m.check()
	if text == "" {
		return nil
	}
	return []state.Banner{{ID: health.BannerWorkerMode, Severity: state.SeverityCritical, Count: 1, Text: text}}
}

// check pings the worker unless a recent answer holds, and returns the mismatch sentence ("" for none).
func (m *workerMode) check() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if now := m.clock(); !m.at.IsZero() && now.Sub(m.at) < workerModeTTL && !now.Before(m.at) {
		return m.text
	}
	m.at = m.clock()
	ctx, cancel := context.WithTimeout(context.Background(), workerModeTimeout)
	defer cancel()
	pr, err := m.ping(ctx)
	m.text = ""
	switch {
	case err != nil: // not a mismatch: C07 and the worker row say the worker is down
	case pr.Drill && !m.drill:
		m.text = workerModeProdConsole
	case !pr.Drill && m.drill:
		m.text = workerModeDrillConsole
	}
	if m.text != "" && m.text != m.logged && m.log != nil {
		m.log.Error("the console and its maintenance worker disagree about drill mode", logx.Class("worker_drill_mismatch"))
	}
	m.logged = m.text
	return m.text
}
