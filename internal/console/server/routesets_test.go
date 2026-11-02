package server_test

import (
	"sort"
	"strings"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/console/adminapi"
	"github.com/c2tech-sys/cryobackup/internal/console/server"
	"github.com/c2tech-sys/cryobackup/internal/console/ui"
)

// M2-W3D: the route sets the every-mutation tests are derived from
// (adminapi.Patterns() and ui.Patterns(true)) are lists somebody keeps by hand.
// A route registered on the admin router but missing from both lists would be
// served, and escape the audit tests and the OpenAPI diff. This test compares
// the live router with the two lists in both directions.

// serverOwned are the admin-router routes that belong to neither package: the
// liveness probe and each listener's not-found handler.
var serverOwned = []string{"GET /healthz", server.CatchAll}

// routeSetDiff returns what the live router serves that no list names, and what
// the lists name that the router does not serve.
func routeSetDiff(live, listed []string) (unlisted, stale []string) {
	l, n := map[string]bool{}, map[string]bool{}
	for _, p := range live {
		l[p] = true
	}
	for _, p := range listed {
		n[p] = true
	}
	for p := range l {
		if !n[p] {
			unlisted = append(unlisted, p)
		}
	}
	for p := range n {
		if !l[p] {
			stale = append(stale, p)
		}
	}
	sort.Strings(unlisted)
	sort.Strings(stale)
	return unlisted, stale
}

func TestRouteSetDiffSeesAnUnlistedAndAStaleRoute(t *testing.T) {
	unlisted, stale := routeSetDiff([]string{"GET /a", "POST /ui/devices/{id}/wipe"}, []string{"GET /a", "GET /gone"})
	if strings.Join(unlisted, ",") != "POST /ui/devices/{id}/wipe" || strings.Join(stale, ",") != "GET /gone" {
		t.Fatalf("unlisted %v stale %v", unlisted, stale)
	}
}

func TestTheAdminRouterServesExactlyTheListedRoutes(t *testing.T) {
	a := fixturesApp(t, nil)
	listed := append(append(append([]string(nil), adminapi.Patterns()...), ui.Patterns(true)...), serverOwned...)
	unlisted, stale := routeSetDiff(a.Server.AdminPatterns(), listed)
	for _, p := range unlisted {
		t.Errorf("the admin router serves %q, which adminapi.Patterns() and ui.Patterns(true) do not list: list it, so the audit and OpenAPI tests see it", p)
	}
	for _, p := range stale {
		t.Errorf("%q is listed (adminapi.Patterns(), ui.Patterns(true) or serverOwned) but the admin router does not serve it", p)
	}
	if len(a.Server.AdminPatterns()) < 100 {
		t.Fatalf("only %d admin routes registered; the router was not built", len(a.Server.AdminPatterns()))
	}
}
