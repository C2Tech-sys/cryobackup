package agent

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/agent/supervisor"
)

// Gateway watchdog (M1-FIX4, foreman ruling 2026-10-24; architecture §6.2
// "gateway down mid-run is deferred, never failed").
//
// restic 0.19.1 does not exit promptly when the gateway goes away in the middle
// of a backup: every backend operation retries for up to 15 minutes
// (internal/global/global.go: retry.New(be, 15*time.Minute, ...)) and the lock
// refresh keeps retrying after that, so the run would stay "running" until
// max_runtime (12 h by default) and then read "cancelled". The watchdog ends
// that wait:
//
//   - while a backup runs, it probes the gateway every gatewayProbeEvery with a
//     plain TCP connect to the repository URL's host:port (no credentials, no
//     restic call, nothing sent), and it follows restic's progress
//     (bytes_done and files_done of the status stream, through the
//     supervisor's samples);
//   - when there has been no progress AND the gateway has been unreachable for
//     throttle.gateway_down_grace_minutes (default 15), it stops restic through
//     the supervisor's cancel path (interrupt, then terminate) and the run is
//     journaled deferred, reason gateway, class network; never cancelled, never
//     failed. The scheduler then retries it on the normal backoff;
//   - progress alone resets the clock: a slow upload that still moves is never
//     stopped, whatever the probe says. A gateway that answers while nothing
//     moves is the stall path (sla.stall_after_minutes), unchanged;
//   - while the gateway cannot be reached and the backup does not move, the
//     heartbeat says so with the fields it already has: progress.stalled is
//     true and last_error_class is network (buildHeartbeat). The console's own
//     reachability (the last heartbeat's answer) is logged with the decision.
//
// Limit: the probe connects directly. A deployment whose restic reaches the
// gateway only through an HTTP proxy sees every probe fail, and the watchdog
// then rests on progress alone (a backup that stops moving for the grace is
// deferred and retried instead of waiting for the stall alarm).

// errGatewayDown is the cancel cause the watchdog stops restic with.
var errGatewayDown = errors.New("agent: the gateway could not be reached and the backup made no progress for the grace")

// gatewayProbeEvery is how often the watchdog probes the gateway.
const gatewayProbeEvery = 30 * time.Second

// gatewayDialTimeout bounds one probe.
const gatewayDialTimeout = 5 * time.Second

// gatewayProbeAddr returns the host:port the watchdog probes for a restic
// repository string: the host of a rest: repository (the gateway; userinfo is
// dropped and never logged), with the scheme's default port. A repository that
// is not a REST URL (a local directory in a lab) has nothing to probe.
func gatewayProbeAddr(repo string) (string, bool) {
	raw, ok := strings.CutPrefix(repo, "rest:")
	if !ok {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return "", false
	}
	port := u.Port()
	switch strings.ToLower(u.Scheme) {
	case "https":
		if port == "" {
			port = "443"
		}
	case "http":
		if port == "" {
			port = "80"
		}
	default:
		return "", false
	}
	return net.JoinHostPort(u.Hostname(), port), true
}

// gatewayWatch is the watchdog's state for one run. It is pure: times come in
// from the caller (the agent's clock), so the rules are tested without waiting.
type gatewayWatch struct {
	grace time.Duration // no progress and no gateway for this long: stop restic
	shown time.Duration // no progress for this long while unreachable: the heartbeat says stalled

	mu        sync.Mutex
	moved     time.Time // the last progress (or the start of the run)
	seen      bool
	bytes     uint64
	files     uint64
	downSince time.Time // the first failed probe of the current outage; zero while reachable
}

func newGatewayWatch(grace, shown time.Duration, start time.Time) *gatewayWatch {
	return &gatewayWatch{grace: grace, shown: shown, moved: start}
}

// progress records a supervisor sample: a change of bytes_done or files_done
// (or the first sample) is progress.
func (w *gatewayWatch) progress(p supervisor.Progress, now time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.seen || p.BytesDone != w.bytes || p.FilesDone != w.files {
		w.moved = now
	}
	w.seen, w.bytes, w.files = true, p.BytesDone, p.FilesDone
}

// probed records a probe and returns what it means: stalled (the heartbeat
// reports the condition) and stop (the grace has run out for both clocks).
func (w *gatewayWatch) probed(reachable bool, now time.Time) (stalled, stop bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	switch {
	case reachable:
		w.downSince = time.Time{}
		return false, false
	case w.downSince.IsZero():
		w.downSince = now
	}
	still := now.Sub(w.moved)
	return still >= w.shown, still >= w.grace && now.Sub(w.downSince) >= w.grace
}

// probeGateway reports whether a TCP connection to addr can be opened.
func probeGateway(ctx context.Context, addr string, timeout time.Duration) bool {
	d := net.Dialer{Timeout: timeout}
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// runWatchdog is one run's watchdog: start it before restic, feed it the
// progress samples, and call end once the supervisor has returned.
type runWatchdog struct {
	w     *gatewayWatch // nil: nothing to probe (no network repository)
	quit  chan struct{}
	done  chan struct{}
	fired func() bool
}

// startGatewayWatch starts the watchdog of run id on rctx; stop is the cancel
// of the context restic runs on. repo is the restic repository string.
func (a *Agent) startGatewayWatch(rctx context.Context, stop context.CancelCauseFunc, id, repo string, grace time.Duration) *runWatchdog {
	rw := &runWatchdog{quit: make(chan struct{}), done: make(chan struct{}),
		fired: func() bool { return errors.Is(context.Cause(rctx), errGatewayDown) }}
	addr := a.o.gatewayAddr
	if addr == "" {
		var ok bool
		if addr, ok = gatewayProbeAddr(repo); !ok {
			a.log.Info("the repository is not a gateway URL; the gateway watchdog is off for this run", "run", id)
			close(rw.done)
			return rw
		}
	}
	every := a.o.gatewayProbeEvery
	if every <= 0 {
		every = gatewayProbeEvery
	}
	// "Not moving" for the heartbeat: a probe period without a new sample,
	// and never less than two sampling periods (samples arrive every
	// progress interval even while restic moves).
	sample := a.o.progressEvery
	if sample <= 0 {
		sample = supervisor.DefaultProgressInterval
	}
	rw.w = newGatewayWatch(grace, max(every, 2*sample), a.clock())
	go func() {
		defer close(rw.done)
		defer a.setGatewayStalled(false)
		t := time.NewTicker(every)
		defer t.Stop()
		shown := false
		for {
			select {
			case <-rw.quit:
				return
			case <-rctx.Done():
				return
			case <-t.C:
			}
			pctx, cancel := context.WithTimeout(rctx, gatewayDialTimeout)
			reachable := probeGateway(pctx, addr, gatewayDialTimeout)
			cancel()
			stalled, fire := rw.w.probed(reachable, a.clock())
			a.setGatewayStalled(stalled)
			switch {
			case stalled && !shown:
				a.log.Warn("the gateway cannot be reached and the backup is not moving; the run is deferred if this lasts the grace",
					"run", id, "grace_s", int(grace/time.Second), "console_reachable", a.consoleReachable())
			case !stalled && shown:
				a.log.Info("the backup is moving again or the gateway answers", "run", id, "gateway_reachable", reachable)
			}
			shown = stalled
			if fire {
				a.log.Warn("the gateway could not be reached and the backup made no progress for the grace: restic is stopped and the run deferred",
					"run", id, "grace_s", int(grace/time.Second), "console_reachable", a.consoleReachable())
				stop(errGatewayDown)
				return
			}
		}
	}()
	return rw
}

// progress feeds one supervisor sample to the watchdog.
func (rw *runWatchdog) progress(p supervisor.Progress, now time.Time) {
	if rw.w != nil {
		rw.w.progress(p, now)
	}
}

// end stops the watchdog, waits for it, and reports whether it stopped restic.
func (rw *runWatchdog) end() bool {
	fired := rw.fired()
	select {
	case <-rw.quit:
	default:
		close(rw.quit)
	}
	<-rw.done
	return fired
}

// setGatewayStalled records whether the watchdog sees the gateway away and the
// backup not moving (the heartbeat reports it).
func (a *Agent) setGatewayStalled(v bool) {
	a.mu.Lock()
	a.gwStalled = v
	a.mu.Unlock()
}

// consoleReachable reports whether the last heartbeat was answered.
func (a *Agent) consoleReachable() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return !a.hbOKAt.IsZero() && !a.hbOKAt.Before(a.hbLast)
}
