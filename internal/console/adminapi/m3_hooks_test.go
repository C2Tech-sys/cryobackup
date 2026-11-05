package adminapi

import (
	"sync/atomic"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/state"
)

// bannerHook is the Evaluator's Extra function in tests. The Evaluator reads
// Extra on every evaluation, from whatever goroutine evaluates: a handler, or an
// event stream between two of the test's own steps. Writing the field while that
// can happen is a data race, so newEnv sets Extra once, before the server starts,
// to banners, and tests swap the function behind it atomically with e.setExtra.
type bannerHook struct {
	fn atomic.Pointer[func(time.Time) []state.Banner]
}

func (h *bannerHook) banners(now time.Time) []state.Banner {
	if f := h.fn.Load(); f != nil {
		return (*f)(now)
	}
	return nil
}

// setExtra replaces the extra banners the next evaluation raises (nil: none).
// It is safe to call while streams and handlers run.
func (e *env) setExtra(f func(time.Time) []state.Banner) {
	if f == nil {
		e.extra.fn.Store(nil)
		return
	}
	e.extra.fn.Store(&f)
}

// setStreamCaps sets MaxEventStreams and MaxEventStreamsPerPrincipal under the
// lock streamSlots reads them under, so it is safe while streams are open.
func (e *env) setStreamCaps(global, perPrincipal int) {
	e.svc.streamMu.Lock()
	defer e.svc.streamMu.Unlock()
	e.svc.MaxEventStreams, e.svc.MaxEventStreamsPerPrincipal = global, perPrincipal
}
