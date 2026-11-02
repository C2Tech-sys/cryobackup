package ui

import (
	"context"
	"net/http"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/httpx"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// The live display preferences (M2-W3A). The display zone and the refresh
// interval are settings an owner can change (settings.go); the next request
// after a change uses the new zone with no restart. The fields Loc, Zone and
// Refresh are the boot values and what a Service built without Display
// (tests, tools) runs on.

// display is one consistent reading of the zone, its name and the refresh
// interval, taken live from the shared preferences.
func (s *Service) display() store.DisplaySnapshot {
	if s.Display != nil {
		return s.Display.Snapshot()
	}
	return store.DisplaySnapshot{Zone: s.Zone, Loc: s.Loc, RefreshS: s.Refresh}
}

// snapKey carries the request's display snapshot in its context.
type snapKey struct{}

// withDisplay returns ctx carrying one display snapshot, taken now. A request
// takes exactly one (M2-W3E): every zone, clock and interval it renders then
// comes from the same reading, so a change saved by another request in
// between can never mix an old zone name with a new zone, or one page shell
// with another page's body. Handlers get it from snapRegistrar; the live
// stream and the freshness payload take theirs at the top of the render.
func (s *Service) withDisplay(ctx context.Context) context.Context {
	if _, ok := ctx.Value(snapKey{}).(store.DisplaySnapshot); ok {
		return ctx
	}
	return context.WithValue(ctx, snapKey{}, s.display())
}

// displayOf is the display snapshot of the request ctx belongs to. A ctx with
// none (a caller outside a request) reads the live preferences.
func (s *Service) displayOf(ctx context.Context) store.DisplaySnapshot {
	if d, ok := ctx.Value(snapKey{}).(store.DisplaySnapshot); ok {
		return d
	}
	return s.display()
}

// loc is the operator's display zone for this request.
func (s *Service) loc(ctx context.Context) *time.Location { return s.displayOf(ctx).Loc }

// zone is the IANA name of loc for this request.
func (s *Service) zone(ctx context.Context) string { return s.displayOf(ctx).Zone }

// snapRegistrar wraps a route set so that every handler it registers runs with
// the request's display snapshot in its context.
type snapRegistrar struct {
	httpx.Registrar
	s *Service
}

func (g snapRegistrar) Handle(pattern string, h http.Handler) {
	g.Registrar.Handle(pattern, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r.WithContext(g.s.withDisplay(r.Context())))
	}))
}
