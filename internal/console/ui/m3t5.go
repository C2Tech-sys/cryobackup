package ui

import (
	"context"
	"net/http"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
	"github.com/c2tech-sys/cryobackup/internal/console/health"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// M3-T5: the sidebar's gateway line and the fixtures session cookies. The board,
// saved views, the pause dialog and the other T5 parts live in m3t5_*.go.

// initM3T5 makes every banner time a 12-hour clock in the operator's display zone: the evaluator words
// the engine's banners in s.Fleet.Zone, and in fixtures mode the lab banners (fixtures.ExtraBannersIn)
// in the same zone. A zone set by the caller is kept.
func (s *Service) initM3T5() {
	if s.Fleet == nil {
		return
	}
	if s.Fleet.Zone == nil {
		s.Fleet.Zone = func(ctx context.Context) *time.Location { return s.loc(ctx) }
	}
	if s.Fixtures && s.Fleet.Extra == nil {
		s.Fleet.Extra = func(now time.Time) []state.Banner { return fixtures.ExtraBannersIn(now, s.display().Loc) }
	}
}

// gatewayItem is the sidebar's Gateway line (G). It is read from the same
// evaluation as the page it sits on, never a constant: the fleet's banners and
// the probe facts the evaluator judged. It follows the rule of the System page's
// gateway row (adminapi probeAttempt/judge) for the cases the page can see:
//
//   - C01 raised: unreachable;
//   - C01b or C00 raised, or no probe facts, or a probe that never ran or is stale: not checked;
//   - the newest run failed: failing;
//   - the newest run passed and is fresh: ok.
//
// "Not checked" is never "ok": a line with no evidence says so.
func (s *Service) gatewayItem(f health.Fleet) SysItem {
	it := SysItem{Label: "Gateway"}
	if f.Computed.IsZero() { // no evaluation at all (the fleet could not be read)
		it.Value, it.Class = "not checked", ClassWarn
		return it
	}
	for _, b := range f.View.Banners {
		switch b.ID {
		case "C01":
			it.Value, it.Class = "unreachable", ClassCrit
			return it
		}
	}
	for _, b := range f.View.Banners {
		if b.ID == "C01b" || b.ID == "C00" {
			it.Value, it.Class = "not checked", ClassWarn
			return it
		}
	}
	it.Value, it.Class = "not checked", ClassWarn
	if s.Fleet == nil || s.Fleet.Probes == nil {
		return it
	}
	pf := s.Fleet.Probes()
	g := pf.Gateway
	if !pf.Wired || g == nil || g.Unconfigured != "" || g.LastRunAt == 0 {
		return it
	}
	iv := pf.ProberInterval
	if iv <= 0 {
		iv = state.DefaultProberInterval
	}
	ran := time.UnixMilli(int64(g.LastRunAt))
	switch {
	case g.ConsecutiveFailures > 0 || g.LastOKAt != g.LastRunAt:
		it.Value, it.Class = "failing", ClassWarn
	case f.Computed.Sub(ran) > time.Duration(state.ProbeStaleIntervals)*iv, ran.After(f.Computed.Add(state.FutureSkewAllowed)):
		// stale or dated in the future: not evidence of now
	default:
		it.Value, it.Class = "ok", ClassOK
	}
	return it
}

// setSessionCookies writes the session and CSRF cookies of a login. In fixtures
// mode the console's clock is frozen in the past, so the cookies' Expires, which
// auth derives from that clock, would be already expired by the browser's clock
// and the browser would drop them: nobody could sign in through the real form.
// Here the same lifetime is counted from the wall clock instead. Outside
// fixtures mode the cookies are exactly auth's.
func (s *Service) setSessionCookies(w http.ResponseWriter, r *http.Request, res auth.LoginResult) {
	if !s.Fixtures {
		s.Auth.SetCookies(w, r, res)
		return
	}
	h := http.Header{}
	s.Auth.SetCookies(headerOnly{h}, r, res)
	life := res.Session.AbsoluteExpiresAt.Sub(s.Clock())
	if life <= 0 {
		life = 12 * time.Hour
	}
	for _, c := range (&http.Response{Header: h}).Cookies() {
		c.Expires = time.Now().Add(life).UTC()
		http.SetCookie(w, c)
	}
}

// headerOnly lets auth.SetCookies write into a bare header map.
type headerOnly struct{ h http.Header }

func (w headerOnly) Header() http.Header         { return w.h }
func (w headerOnly) Write(b []byte) (int, error) { return len(b), nil }
func (w headerOnly) WriteHeader(int)             {}

// agentLabel is the agent version a page prints: empty stays empty (nothing reported yet), anything else
// goes through safeVersion, so a file name or a sentence sent as a label shows as "(withheld)" (F).
func agentLabel(v string) string {
	if v == "" {
		return ""
	}
	return safeVersion(v)
}
