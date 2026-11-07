package ui

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/health"
	"github.com/c2tech-sys/cryobackup/internal/console/hubsig"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
)

// Event is one server-sent event. Data is a server-rendered HTML fragment (or
// the JSON of the freshness event); fresh.js swaps fragments into the page by
// element id.
type Event struct {
	Name string
	Data string
}

// Sub is one SSE subscriber: who it is (scope and role), what the page shows
// (a board filter or one device) and its outbox.
type Sub struct {
	Scope store.Scope
	Role  auth.Role
	// Viewer is the account the stream is for: a saved view on the board is resolved as it.
	Viewer string
	// Filter is the board's filter as a query string (boardQuery.Encode).
	Filter   string
	DeviceID string
	// Recheck, when set, is asked after every ping; false ends the stream (the
	// credential check of streamRecheck, m3h_stream.go).
	Recheck func(ctx context.Context) bool

	ch     chan Event
	closed bool
}

func (s *Sub) key() string {
	scope := "all"
	if !s.Scope.All {
		ids := append([]string(nil), s.Scope.CustomerIDs...)
		sort.Strings(ids)
		scope = strings.Join(ids, ",")
	}
	viewerKey := "" // only a saved view differs by account
	if strings.Contains(s.Filter, "view=") {
		viewerKey = s.Viewer
	}
	return scope + "|" + viewerKey + "|" + s.Filter + "|" + s.DeviceID
}

// Ticker makes a channel that fires every d and a function that stops it. The
// default is time.NewTicker; tests inject their own.
type Ticker func(d time.Duration) (<-chan time.Time, func())

func realTicker(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTicker(d)
	return t.C, t.Stop
}

// Hub fans changes out to SSE subscribers (architecture §13.1): `fleet.state`
// (the re-rendered board, or the device summary), `run.progress`,
// `banner.update`, `freshness` and a `ping` every 15 s. Changes arrive through
// DeviceChanged and RunProgress (the agent API calls them) and are coalesced:
// at most one re-render per Coalesce interval per distinct view. A time-based
// refresh re-renders everything every Refresh as well, because states change
// with the clock (a device turns STALE with no event at all).
type Hub struct {
	// Tick makes tickers (default time.NewTicker).
	Tick Ticker
	// Ping is the ping cadence (15 s), Refresh the periodic re-render (the
	// console refresh interval) and Coalesce the minimum gap between change renders.
	Ping, Refresh, Coalesce time.Duration
	Log                     *logx.Logger
	// RefreshNow, when set, is asked on every ping for the current refresh
	// interval; a changed value restarts the refresh ticker (M2-W3A: the
	// interval is a setting).
	RefreshNow func() time.Duration

	render    func(ctx context.Context, sub *Sub) ([]Event, error)
	freshness func(f health.Fleet) string

	mu       sync.Mutex
	subs     map[*Sub]struct{}
	dirty    bool
	progress map[string]string // run id -> device id, drained by the next flush
	// watchers are the admin API's event streams (hub_watch.go, M3-T3).
	watchers map[chan hubsig.Signal]struct{}
	// gen counts the signals passed to watchers (Generation, hub_watch.go).
	gen atomic.Uint64
}

// NewHub returns a Hub with the architecture's cadences.
func NewHub(refresh time.Duration, log *logx.Logger) *Hub {
	if log == nil {
		log = logx.Default()
	}
	return &Hub{Tick: realTicker, Ping: 15 * time.Second, Refresh: refresh, Coalesce: 2 * time.Second, Log: log,
		subs: map[*Sub]struct{}{}, progress: map[string]string{}}
}

// Subscribe registers sub and returns its channel and the cancel function.
func (h *Hub) Subscribe(sub *Sub) (<-chan Event, func()) {
	sub.ch = make(chan Event, 32)
	h.mu.Lock()
	h.subs[sub] = struct{}{}
	h.mu.Unlock()
	return sub.ch, func() { h.unsubscribe(sub) }
}

func (h *Hub) unsubscribe(sub *Sub) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.subs[sub]; ok {
		delete(h.subs, sub)
		if !sub.closed {
			sub.closed = true
			close(sub.ch)
		}
	}
}

// Subscribers returns how many streams are open (tests, /healthz).
func (h *Hub) Subscribers() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// DeviceChanged marks the fleet dirty; the next coalesced tick re-renders.
func (h *Hub) DeviceChanged(deviceID string) {
	h.mu.Lock()
	h.dirty = true
	h.mu.Unlock()
	h.notify(hubsig.Signal{Kind: hubsig.Change, DeviceID: deviceID})
}

// RunProgress records live progress; it also dirties the fleet so device pages refresh.
func (h *Hub) RunProgress(deviceID, runID string) {
	h.mu.Lock()
	h.dirty = true
	h.progress[runID] = deviceID
	h.mu.Unlock()
	h.notify(hubsig.Signal{Kind: hubsig.Progress, DeviceID: deviceID, RunID: runID})
}

// send delivers to one subscriber; a subscriber that cannot keep up is dropped
// (its stream closes, the browser reconnects and refetches). The non-blocking
// send happens under h.mu, the same lock unsubscribe closes the channel under,
// so a send can never race the close (or hit a closed channel).
func (h *Hub) send(sub *Sub, evs ...Event) {
	h.mu.Lock()
	dropped := false
	if !sub.closed {
		for _, ev := range evs {
			select {
			case sub.ch <- ev:
				continue
			default:
				dropped = true
			}
			break
		}
	}
	h.mu.Unlock()
	if dropped {
		h.unsubscribe(sub)
	}
}

func (h *Hub) broadcast(evs ...Event) {
	h.mu.Lock()
	subs := make([]*Sub, 0, len(h.subs))
	for s := range h.subs {
		subs = append(subs, s)
	}
	h.mu.Unlock()
	for _, s := range subs {
		h.send(s, evs...)
	}
}

// flush re-renders once per distinct view and delivers to its subscribers.
func (h *Hub) flush(ctx context.Context) {
	if h.render == nil {
		return
	}
	h.mu.Lock()
	groups := map[string][]*Sub{}
	for s := range h.subs {
		groups[s.key()] = append(groups[s.key()], s)
	}
	h.dirty = false
	h.progress = map[string]string{}
	h.mu.Unlock()
	for _, subs := range groups {
		evs, err := h.render(ctx, subs[0])
		if err != nil {
			h.Log.Warn("sse render failed", logx.Class("render"))
			continue
		}
		for _, s := range subs {
			h.send(s, evs...)
		}
	}
}

// Run drives the hub until ctx is cancelled.
func (h *Hub) Run(ctx context.Context) {
	if h.Tick == nil {
		h.Tick = realTicker
	}
	pingC, stopPing := h.Tick(h.Ping)
	defer stopPing()
	refreshEvery := h.Refresh
	refreshC, stopRefresh := h.Tick(refreshEvery)
	defer func() { stopRefresh() }()
	coalesceC, stopCoalesce := h.Tick(h.Coalesce)
	defer stopCoalesce()
	for {
		select {
		case <-ctx.Done():
			return
		case <-pingC:
			h.broadcast(Event{Name: "ping"})
			if h.RefreshNow != nil {
				if d := h.RefreshNow(); d > 0 && d != refreshEvery {
					stopRefresh()
					refreshEvery = d
					refreshC, stopRefresh = h.Tick(refreshEvery)
				}
			}
		case <-refreshC:
			h.notify(hubsig.Signal{Kind: hubsig.Refresh})
			h.flush(ctx)
		case <-coalesceC:
			h.mu.Lock()
			d := h.dirty
			h.mu.Unlock()
			if d {
				h.flush(ctx)
			}
		}
	}
}

// ServeSSE streams events to one browser until it goes away.
func (h *Hub) ServeSSE(w http.ResponseWriter, r *http.Request, sub *Sub) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{}) // the stream outlives the server's write timeout
	_ = rc.SetReadDeadline(time.Time{})
	hd := w.Header()
	hd.Set("Content-Type", "text/event-stream")
	hd.Set("Cache-Control", "no-store")
	hd.Set("X-Accel-Buffering", "no")
	hd.Set("Connection", "keep-alive")
	ch, cancel := h.Subscribe(sub)
	defer cancel()
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w, "retry: 3000\n\n")
	fl.Flush()
	// First paint of the stream: the stamp, banners and board as of now.
	if h.render != nil {
		if evs, err := h.render(r.Context(), sub); err == nil {
			h.send(sub, evs...)
		}
	}
	h.send(sub, Event{Name: "ping"})
	for {
		select {
		case <-r.Context().Done():
			return
		case ev, open := <-ch:
			if !open {
				return
			}
			if err := writeEvent(w, ev); err != nil {
				return
			}
			fl.Flush()
			if ev.Name == "ping" && sub.Recheck != nil && !sub.Recheck(r.Context()) {
				return
			}
		}
	}
}

func writeEvent(w http.ResponseWriter, ev Event) error {
	var b strings.Builder
	b.WriteString("event: " + ev.Name + "\n")
	data := ev.Data
	if data == "" {
		b.WriteString("data: \n")
	}
	for _, line := range strings.Split(strings.ReplaceAll(data, "\r", ""), "\n") {
		if data == "" {
			break
		}
		b.WriteString("data: " + line + "\n")
	}
	b.WriteString("\n")
	_, err := fmt.Fprint(w, b.String())
	return err
}
