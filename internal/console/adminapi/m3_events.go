package adminapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/health"
	"github.com/c2tech-sys/cryobackup/internal/console/httpx"
	"github.com/c2tech-sys/cryobackup/internal/console/hubsig"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
)

// The admin event stream (architecture §5.2 GET /api/v1/events, §13.1). It is the
// JSON twin of the UI's HTML stream: the same hub signals, read-only, and the
// same Evaluator.Fleet view behind every payload. Nothing here writes.
//
// Events:
//
//	fleet.state     the fleet summary (same body as GET /fleet/summary), on connect,
//	                on every device change (coalesced) and every refresh interval,
//	                because states change with the clock
//	banner.update   the console banners, on connect and whenever the set changes
//	run.progress    {device_id, run_id}: a run of a device in the caller's scope made progress
//	stream.error    the fleet could not be evaluated; the last state is stale
//	ping            every 15 seconds
//
// topics= narrows the stream to some of fleet, banners and runs (comma list;
// default all). The stream is scoped like every other read: a caller sees the
// fleet of its own customers and progress of its own devices only.

// Signal is what the event source tells the stream (see hubsig).
type Signal = hubsig.Signal

// Signal kinds.
const (
	SignalChange   = hubsig.Change
	SignalProgress = hubsig.Progress
	SignalRefresh  = hubsig.Refresh
)

// EventSource is the hub the stream listens to (ui.Hub satisfies it). Watch
// returns a channel of signals and the function that stops watching; a slow
// reader loses signals, never blocks the hub. DeviceChanged tells the hub that
// a device changed (an API write), so every open stream re-renders. Generation
// counts the hub's signals and grows before each one is delivered (a lost signal
// still counts), so a watcher that reads it after a signal sees that signal
// counted.
type EventSource interface {
	Watch() (<-chan Signal, func())
	DeviceChanged(deviceID string)
	Generation() uint64
}

// DefaultMaxEventStreams is how many event streams may be open at once when
// Service.MaxEventStreams is zero. Each stream holds a connection and, on a
// change, shares one fleet evaluation with the others of its scope; the cap
// keeps a runaway client (or a hundred browser tabs) from holding the
// connections of the admin listener. A negative MaxEventStreams removes the cap.
const DefaultMaxEventStreams = 32

// DefaultMaxEventStreamsPerPrincipal is how many event streams one account (or one
// API token) may hold open when Service.MaxEventStreamsPerPrincipal is zero. A
// handful of browser tabs fit; a script that opens streams in a loop does not
// get to take the global slots of everyone else.
const DefaultMaxEventStreamsPerPrincipal = 4

// DefaultEventWriteTimeout is the write deadline of every write to a stream
// (events and pings): a reader that stalls longer than this is dropped and its
// slot freed.
const DefaultEventWriteTimeout = 30 * time.Second

// The stream's credential is checked again once per ping interval (15 s, the
// same Authenticate calls the gate makes): a revoked or expired session or
// token, or a disabled account, loses its stream within one interval. A role or
// customer-scope change takes effect on the next evaluation. Note that this
// check counts as activity for the session's sliding idle expiry, as any
// request would; the absolute session lifetime still ends the stream.

// sseShareDefault is how long one fleet evaluation may serve every stream of the
// same scope: a change that fans out to N streams costs one evaluation, not N.
// The window only bounds the reuse; a change ends it at once (changeGeneration).
const sseShareDefault = time.Second

const (
	ssePing        = 15 * time.Second
	sseDefaultGap  = 2 * time.Second
	sseMinRefresh  = 5 * time.Second
	sseTopicFleet  = "fleet"
	sseTopicBanner = "banners"
	sseTopicRuns   = "runs"
)

func (s *Service) sseTimings() (refresh, gap time.Duration) {
	refresh, gap = s.sseRefresh, s.sseGap
	if refresh <= 0 {
		refresh = time.Duration(s.display().RefreshS) * time.Second
		if refresh < sseMinRefresh {
			refresh = sseMinRefresh
		}
	}
	if gap <= 0 {
		gap = sseDefaultGap
	}
	return refresh, gap
}

// sharedEval is the newest evaluation of one scope, for every stream of it.
// gen is the change generation read before the evaluation started.
type sharedEval struct {
	mu    sync.Mutex
	at    time.Time
	gen   uint64
	fleet health.Fleet
	ok    bool
}

// changeGeneration counts the changes the stream can know of: every hub signal
// (Events.Generation) and every change made through this API that tells the hub
// (deviceChanged bumps apiGen before it tells anyone). Both counters only grow,
// so their sum only grows, and it has grown before any stream can hear of the
// change. An evaluation is tagged with the value read before it started; it is
// reused only while that value is still current, so an evaluation that may
// predate a change is never served after the change.
func (s *Service) changeGeneration() uint64 {
	g := s.apiGen.Load()
	if s.Events != nil {
		g += s.Events.Generation()
	}
	return g
}

// scopeKey names a scope the way the hub does, so equal scopes share.
func scopeKey(sc store.Scope) string {
	if sc.All {
		return "all"
	}
	ids := append([]string(nil), sc.CustomerIDs...)
	sort.Strings(ids)
	return "ids:" + strings.Join(ids, ",")
}

// sharedFleet evaluates the fleet for scope, reusing the newest successful
// evaluation of the same scope when it is younger than the share window and no
// change has happened since it started (its generation is current). Concurrent
// callers of one scope wait for the evaluation in flight instead of starting
// their own. A failed evaluation is returned, never kept: the next caller tries
// again. gen is the generation of the answer: no change counted at or below it
// is missing from it.
func (s *Service) sharedFleet(ctx context.Context, scope store.Scope) (f health.Fleet, gen uint64, err error) {
	key := scopeKey(scope)
	s.evalMu.Lock()
	if s.evals == nil {
		s.evals = map[string]*sharedEval{}
	}
	ev := s.evals[key]
	if ev == nil {
		ev = &sharedEval{}
		s.evals[key] = ev
	}
	s.evalMu.Unlock()
	window := s.sseShare
	if window == 0 {
		window = sseShareDefault
	}
	ev.mu.Lock()
	defer ev.mu.Unlock()
	gen = s.changeGeneration() // read before evaluating: a change during the evaluation makes it stale
	if ev.ok && ev.gen == gen && time.Since(ev.at) < window {
		return ev.fleet, ev.gen, nil
	}
	f, err = s.Fleet.Fleet(ctx, scope)
	if err != nil {
		ev.ok = false
		return health.Fleet{}, 0, err
	}
	ev.fleet, ev.at, ev.gen, ev.ok = f, time.Now(), gen, true
	return f, gen, nil
}

// streamSlots takes one slot for the principal and one global slot, in that
// order, and returns the function that gives both back. status is 0 on success,
// 429 when the principal is over its limit, 503 when the console is.
//
// The two limits are read under streamMu, the lock the slot counts already take,
// so a change of them (tests do this while streams are open) is ordered with every
// admission.
func (s *Service) streamSlots(principal string) (release func(), status int) {
	s.streamMu.Lock()
	defer s.streamMu.Unlock()
	perLimit := s.MaxEventStreamsPerPrincipal
	if perLimit == 0 {
		perLimit = DefaultMaxEventStreamsPerPrincipal
	}
	limit := s.MaxEventStreams
	if limit == 0 {
		limit = DefaultMaxEventStreams
	}
	if perLimit > 0 && s.streamsOf[principal] >= perLimit {
		return nil, http.StatusTooManyRequests
	}
	if limit > 0 && s.streamsOpen >= limit {
		return nil, http.StatusServiceUnavailable
	}
	if s.streamsOf == nil {
		s.streamsOf = map[string]int{}
	}
	s.streamsOf[principal]++
	s.streamsOpen++
	return func() {
		s.streamMu.Lock()
		defer s.streamMu.Unlock()
		if s.streamsOf[principal]--; s.streamsOf[principal] <= 0 {
			delete(s.streamsOf, principal)
		}
		s.streamsOpen--
	}, 0
}

// openStreams reports how many streams are open (tests, diagnostics).
func (s *Service) openStreams() int {
	s.streamMu.Lock()
	defer s.streamMu.Unlock()
	return s.streamsOpen
}

// principalKey names who a stream belongs to: the token for a token caller, else the account.
func principalKey(p *auth.Principal) string {
	if p.TokenID != "" {
		return "token:" + p.TokenID
	}
	return "admin:" + p.Admin.ID
}

// streamCredential is the credential the request authenticated with, kept so the
// stream can check it again: the bearer token if the request carried one (the
// gate prefers it), else the session cookie.
func streamCredential(r *http.Request) (bearer, cookie string) {
	if h := r.Header.Get(api.HeaderAuthorization); len(h) >= len(api.BearerPrefix) && strings.EqualFold(h[:len(api.BearerPrefix)], api.BearerPrefix) {
		return strings.TrimSpace(h[len(api.BearerPrefix):]), ""
	}
	if c, err := r.Cookie(auth.SessionCookie); err == nil {
		return "", c.Value
	}
	return "", ""
}

// revalidate checks the stream's credential again. alive false means it is
// revoked, expired or its account disabled and the stream must end. A database
// error is not a verdict: the stream stays and the check is repeated on the next
// interval (logged without identifiers).
func (s *Service) revalidate(ctx context.Context, bearer, cookie string, cur *auth.Principal) (*auth.Principal, bool) {
	var p *auth.Principal
	var err error
	switch {
	case bearer != "":
		p, err = s.Auth.AuthenticateBearer(ctx, bearer)
	case cookie != "":
		p, err = s.Auth.Authenticate(ctx, cookie)
	default:
		return cur, false
	}
	switch {
	case err == nil:
		if p.Role < auth.RoleReadOnly {
			return cur, false
		}
		return p, true
	case errors.Is(err, auth.ErrNoSession):
		return cur, false
	}
	s.log().Warn("event stream: credential not re-checked", logx.Class("auth"))
	return cur, true
}

func (s *Service) writeTimeout() time.Duration {
	if s.sseWriteTimeout > 0 {
		return s.sseWriteTimeout
	}
	return DefaultEventWriteTimeout
}

// writeSSE writes one event under the write deadline: a reader that does not
// take it in time fails the write, which ends the stream.
func (s *Service) writeSSE(w http.ResponseWriter, rc *http.ResponseController, fl http.Flusher, name string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_ = rc.SetWriteDeadline(time.Now().Add(s.writeTimeout()))
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, b); err != nil {
		return err
	}
	fl.Flush()
	return nil
}

// RunProgressJSON is the payload of run.progress.
type RunProgressJSON struct {
	DeviceID string `json:"device_id"`
	RunID    string `json:"run_id,omitempty"`
}

// BannerUpdateJSON is the payload of banner.update.
type BannerUpdateJSON struct {
	StampJSON
	Banners []BannerJSON `json:"banners"`
}

// StreamErrorJSON is the payload of stream.error.
type StreamErrorJSON struct {
	ComputedAt time.Time `json:"computed_at"`
	Class      string    `json:"class"`
}

func (s *Service) events(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	topics := map[string]bool{sseTopicFleet: true, sseTopicBanner: true, sseTopicRuns: true}
	if v := strings.TrimSpace(r.URL.Query().Get("topics")); v != "" {
		topics = map[string]bool{}
		for _, t := range splitList([]string{v}) {
			if t != sseTopicFleet && t != sseTopicBanner && t != sseTopicRuns {
				httpx.Problem(w, r, api.CodeInvalidRequest, 0)
				return
			}
			topics[t] = true
		}
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		httpx.Problem(w, r, api.CodeInternal, 0)
		return
	}
	// Over a limit: refused, with Retry-After, before anything is held for the stream.
	// The principal's own limit is checked first, so one account cannot use up the global slots.
	release, status := s.streamSlots(principalKey(p))
	switch status {
	case http.StatusTooManyRequests:
		httpx.Problem(w, r, api.CodeRateLimited, 30*time.Second)
		return
	case http.StatusServiceUnavailable:
		httpx.Problem(w, r, api.CodeUnavailable, 30*time.Second)
		return
	}
	defer release()
	bearer, cookie := streamCredential(r)
	// Watch before the first byte: a client that has seen the headers has a
	// registered listener, so no signal after that point is lost.
	var sigC <-chan Signal
	stop := func() {}
	if s.Events != nil {
		sigC, stop = s.Events.Watch()
	}
	defer stop()
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Time{}) // the stream outlives the server's timeouts; every write sets its own deadline
	_ = rc.SetWriteDeadline(time.Now().Add(s.writeTimeout()))
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w, "retry: 3000\n\n")
	fl.Flush()

	refresh, gap := s.sseTimings()
	// With a hub, its refresh tick drives the stream (a SignalRefresh marks it dirty) and the
	// stream's own tick is only a watchdog at three intervals, so one tick is one evaluation, not
	// two. Without a hub, or once it goes away, the own tick is the refresh.
	period := func() time.Duration {
		if sigC != nil {
			return 3 * refresh
		}
		return refresh
	}
	pingEvery := s.sseRevalidate
	if pingEvery <= 0 {
		pingEvery = ssePing
	}
	refreshT, gapT, pingT := time.NewTicker(period()), time.NewTicker(gap), time.NewTicker(pingEvery)
	defer refreshT.Stop()
	defer gapT.Stop()
	defer pingT.Stop()

	var lastBanners string
	// emit sends the fleet of the caller's scope. alive is false when the stream
	// must end; gen is the change generation the fleet sent is current for (the
	// maximum on a stream.error: the client has been told the state is stale, and
	// the next signal tries again).
	emit := func(ctx context.Context) (alive bool, gen uint64) {
		refreshT.Reset(period()) // an emit of any cause restarts the watchdog
		f, gen, err := s.sharedFleet(ctx, p.Scope)
		if err != nil {
			s.log().Warn("event stream: fleet not evaluated", logx.Class("fleet"))
			return s.writeSSE(w, rc, fl, "stream.error", StreamErrorJSON{ComputedAt: s.now(), Class: "fleet_unavailable"}) == nil, ^uint64(0)
		}
		fd := fleetData{Fleet: f, names: map[string]string{}, Devices: f.View.Devices, Customers: f.View.Customers, Collapses: f.View.Collapses}
		if topics[sseTopicFleet] {
			if s.writeSSE(w, rc, fl, "fleet.state", buildSummary(fd)) != nil {
				return false, gen
			}
		}
		if topics[sseTopicBanner] {
			bj := bannersJSON(f.View.Banners)
			key, _ := json.Marshal(bj)
			if string(key) != lastBanners {
				lastBanners = string(key)
				if s.writeSSE(w, rc, fl, "banner.update", BannerUpdateJSON{StampJSON: fd.stamp(), Banners: bj}) != nil {
					return false, gen
				}
			}
		}
		return true, gen
	}
	if alive, _ := emit(r.Context()); !alive {
		return
	}

	inScope := map[string]bool{} // device id -> may this caller see it
	visible := func(id string) bool {
		if v, known := inScope[id]; known {
			return v
		}
		_, err := s.Store.GetDevice(r.Context(), p.Scope, id)
		switch {
		case err == nil:
			inScope[id] = true
		case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrScope):
			inScope[id] = false // a real answer: outside the caller's scope, or gone
		default:
			// A database error says nothing about visibility. Do not remember it as "not
			// visible": this event is dropped, the next one asks again. Nothing identifying is logged.
			s.log().Warn("event stream: device scope not checked", logx.Class("device_lookup"))
			return false
		}
		return inScope[id]
	}
	// dirty is set by a signal; dirtyGen is the change generation read when it was
	// received (it already counts that signal). The flag is cleared only by an emit
	// whose fleet is at least that current, never by an older (cached) one.
	dirty, dirtyGen := false, uint64(0)
	markDirty := func() {
		dirty = true
		if g := s.changeGeneration(); g > dirtyGen {
			dirtyGen = g
		}
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case sg, open := <-sigC:
			if !open {
				sigC = nil // the source went away; the own tick keeps the stream alive, at the refresh interval
				refreshT.Reset(period())
				continue
			}
			switch sg.Kind {
			case SignalProgress:
				if topics[sseTopicRuns] && visible(sg.DeviceID) {
					if s.writeSSE(w, rc, fl, "run.progress", RunProgressJSON{DeviceID: sg.DeviceID, RunID: sg.RunID}) != nil {
						return
					}
				}
				markDirty()
			case SignalChange, SignalRefresh:
				markDirty()
			}
		case <-gapT.C:
			if dirty {
				alive, gen := emit(r.Context())
				if !alive {
					return
				}
				if gen >= dirtyGen {
					dirty = false
				}
			}
		case <-refreshT.C:
			alive, gen := emit(r.Context())
			if !alive {
				return
			}
			if gen >= dirtyGen {
				dirty = false
			}
		case <-pingT.C:
			_ = rc.SetWriteDeadline(time.Now().Add(s.writeTimeout()))
			if _, err := fmt.Fprint(w, "event: ping\ndata: {}\n\n"); err != nil {
				return
			}
			fl.Flush()
			// The credential is checked again once per interval; a revoked principal loses the stream here.
			np, alive := s.revalidate(r.Context(), bearer, cookie, p)
			if !alive {
				return
			}
			// A changed role or scope (a narrowed account) forgets every visibility
			// answer: a device seen under the old scope is asked about again.
			if np.Role != p.Role || scopeKey(np.Scope) != scopeKey(p.Scope) {
				inScope = map[string]bool{}
			}
			p = np
		}
	}
}
