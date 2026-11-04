package adminapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/httpx"
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

// Signal is what the event source tells the stream: Kind is "change" (some
// device changed), "progress" (a run of DeviceID made progress) or "refresh"
// (the periodic re-evaluation tick).
type Signal struct {
	Kind     string
	DeviceID string
	RunID    string
}

// Signal kinds.
const (
	SignalChange   = "change"
	SignalProgress = "progress"
	SignalRefresh  = "refresh"
)

// EventSource is the hub the stream listens to (ui.Hub satisfies it). Watch
// returns a channel of signals and the function that stops watching; a slow
// reader loses signals, never blocks the hub. DeviceChanged tells the hub that
// a device changed (an API write), so every open stream re-renders.
type EventSource interface {
	Watch() (<-chan Signal, func())
	DeviceChanged(deviceID string)
}

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

func writeSSE(w http.ResponseWriter, fl http.Flusher, name string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
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
	// Watch before the first byte: a client that has seen the headers has a
	// registered listener, so no signal after that point is lost.
	var sigC <-chan Signal
	stop := func() {}
	if s.Events != nil {
		sigC, stop = s.Events.Watch()
	}
	defer stop()
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{}) // the stream outlives the server's write timeout
	_ = rc.SetReadDeadline(time.Time{})
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w, "retry: 3000\n\n")
	fl.Flush()

	refresh, gap := s.sseTimings()
	refreshT, gapT, pingT := time.NewTicker(refresh), time.NewTicker(gap), time.NewTicker(ssePing)
	defer refreshT.Stop()
	defer gapT.Stop()
	defer pingT.Stop()

	var lastBanners string
	emit := func(ctx context.Context) bool {
		f, err := s.Fleet.Fleet(ctx, p.Scope)
		if err != nil {
			s.log().Warn("event stream: fleet not evaluated", logx.Class("fleet"))
			return writeSSE(w, fl, "stream.error", StreamErrorJSON{ComputedAt: s.now(), Class: "fleet_unavailable"}) == nil
		}
		fd := fleetData{Fleet: f, names: map[string]string{}, Devices: f.View.Devices, Customers: f.View.Customers, Collapses: f.View.Collapses}
		if topics[sseTopicFleet] {
			if writeSSE(w, fl, "fleet.state", buildSummary(fd)) != nil {
				return false
			}
		}
		if topics[sseTopicBanner] {
			bj := bannersJSON(f.View.Banners)
			key, _ := json.Marshal(bj)
			if string(key) != lastBanners {
				lastBanners = string(key)
				if writeSSE(w, fl, "banner.update", BannerUpdateJSON{StampJSON: fd.stamp(), Banners: bj}) != nil {
					return false
				}
			}
		}
		return true
	}
	if !emit(r.Context()) {
		return
	}

	inScope := map[string]bool{} // device id -> may this caller see it
	visible := func(id string) bool {
		v, known := inScope[id]
		if !known {
			_, err := s.Store.GetDevice(r.Context(), p.Scope, id)
			v = err == nil
			inScope[id] = v
		}
		return v
	}
	dirty := false
	for {
		select {
		case <-r.Context().Done():
			return
		case sg, open := <-sigC:
			if !open {
				sigC = nil // the source went away; the refresh tick keeps the stream alive
				continue
			}
			switch sg.Kind {
			case SignalProgress:
				if topics[sseTopicRuns] && visible(sg.DeviceID) {
					if writeSSE(w, fl, "run.progress", RunProgressJSON{DeviceID: sg.DeviceID, RunID: sg.RunID}) != nil {
						return
					}
				}
				dirty = true
			case SignalChange, SignalRefresh:
				dirty = true
			}
		case <-gapT.C:
			if dirty {
				dirty = false
				if !emit(r.Context()) {
					return
				}
			}
		case <-refreshT.C:
			dirty = false
			if !emit(r.Context()) {
				return
			}
		case <-pingT.C:
			if _, err := fmt.Fprint(w, "event: ping\ndata: {}\n\n"); err != nil {
				return
			}
			fl.Flush()
		}
	}
}
