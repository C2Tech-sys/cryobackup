package adminapi

import (
	"encoding/json"
	"sort"
	"testing"
	"time"
)

// M3-T3 fix round 4: every API mutation that can change what the fleet
// evaluation reads advances the event stream's change generation and signals
// the hub.

// fleetMutationRoutes are the mutating routes that change something the fleet
// evaluation reads (customers, devices, policies and assignments, overrides,
// settings, runs and commands, enrollment, restores, the storage probe's facts).
// Every mutating route is either here or in fleetNeutral (fleetchange.go), never
// both and never neither, so a new route is a decision.
var fleetMutationRoutes = map[string]bool{
	PatternCustomerCreate:       true,
	PatternCustomerPatch:        true,
	PatternDevicePatch:          true,
	PatternDevicePause:          true,
	PatternDeviceResume:         true,
	PatternDeviceCommandCreate:  true,
	PatternDeviceCommandCancel:  true,
	PatternRunCancel:            true,
	PatternDeviceIdentityResolv: true,
	PatternPolicyCreate:         true,
	PatternPolicyPatch:          true,
	PatternPolicyDelete:         true,
	PatternOverrideSet:          true,
	PatternOverrideClear:        true,
	PatternEnrollTokenCreate:    true,
	PatternEnrollTokenRevoke:    true,
	PatternSettingPut:           true,
	PatternRestoreCreate:        true,
	PatternStorageTargetProbe:   true,
}

func TestEveryFleetMutationAdvancesTheChangeGeneration(t *testing.T) { // r4 MEDIUM-1
	cases := auditMutationCases()
	for p := range mutatingRoutes() {
		_, neutral := fleetNeutral[p]
		switch {
		case neutral && fleetMutationRoutes[p]:
			t.Errorf("%q is both a fleet mutation and fleet-neutral: pick one", p)
		case !neutral && !fleetMutationRoutes[p]:
			t.Errorf("mutating route %q is in neither fleetMutationRoutes (this test) nor fleetNeutral (fleetchange.go): decide whether it changes what the fleet evaluation reads", p)
		}
	}
	for p := range fleetMutationRoutes {
		if !mutatingRoutes()[p] {
			t.Errorf("fleetMutationRoutes lists %q, which is not a mutating admin route", p)
		}
	}
	for p, why := range fleetNeutral {
		if !mutatingRoutes()[p] {
			t.Errorf("fleetNeutral lists %q, which is not a mutating admin route", p)
		}
		if why == "" {
			t.Errorf("fleetNeutral lists %q without saying why it cannot change the fleet", p)
		}
	}
	if t.Failed() {
		return
	}
	names := make([]string, 0, len(cases))
	for p := range cases {
		names = append(names, p)
	}
	sort.Strings(names)
	for _, key := range names {
		if !fleetMutationRoutes[baseRoute(key)] {
			continue
		}
		m := cases[key]
		t.Run(key, func(t *testing.T) {
			e := newEnv(t)
			src := &fakeSource{}
			e.svc.Events = src
			owner := e.login("admin", pw)
			c := m.build(e, owner)()
			g0 := e.svc.changeGeneration()
			src.mu.Lock()
			told0 := len(src.changed)
			src.mu.Unlock()
			res, body := e.do(c, owner)
			want := m.status
			if want == 0 {
				want = 200
			}
			if res.StatusCode != want {
				t.Fatalf("status %d, want %d: %s", res.StatusCode, want, body)
			}
			if g := e.svc.changeGeneration(); g <= g0 {
				t.Errorf("the change generation stayed at %d: an event stream would keep serving the fleet from before this change", g0)
			}
			src.mu.Lock()
			told := len(src.changed) - told0
			src.mu.Unlock()
			if told < 1 {
				t.Error("the hub was not told: open streams would not re-render")
			}
		})
	}
}

// r4 MEDIUM-1, the reported case: an empty customer marked expected becomes
// "missing"; a stream connecting right after the PATCH shows it, and the open
// stream re-renders.
func TestM3EventStreamAfterACustomerPatchServesTheFleetAfterIt(t *testing.T) { // F18, r4 MEDIUM-1
	e := newEnv(t)
	src := &fakeSource{}
	e.svc.Events, e.svc.sseRefresh, e.svc.sseGap, e.svc.sseShare = src, time.Hour, 10*time.Millisecond, time.Hour
	owner := e.login("admin", pw)
	var cust struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(e.mustReq("POST", "/api/v1/customers", owner, map[string]any{"name": "Round Four Empty Co"}, 201), &cust); err != nil || cust.ID == "" {
		t.Fatalf("create customer: %v", err)
	}
	a, _ := e.openStream("/api/v1/events?topics=fleet", owner)
	before := summaryOf(t, a.next("fleet.state", 5*time.Second))

	e.mustReq("PATCH", "/api/v1/customers/"+cust.ID, owner, map[string]any{"expected": true}, 200)
	want := decode[FleetSummaryResponse](t, mustGet(e, "/api/v1/fleet/summary", owner))
	if want.CustomerRing.Missing != before.CustomerRing.Missing+1 {
		t.Fatalf("fixture: /fleet/summary missing %d before, %d after", before.CustomerRing.Missing, want.CustomerRing.Missing)
	}
	b, _ := e.openStream("/api/v1/events?topics=fleet", owner)
	if got := summaryOf(t, b.next("fleet.state", 5*time.Second)); got.CustomerRing.Missing != want.CustomerRing.Missing {
		t.Errorf("a stream connecting after the PATCH shows customer_ring.missing %d; /fleet/summary says %d", got.CustomerRing.Missing, want.CustomerRing.Missing)
	}
	if got := summaryOf(t, a.next("fleet.state", time.Second)); got.CustomerRing.Missing != want.CustomerRing.Missing {
		t.Errorf("the open stream's next fleet.state shows missing %d, want %d", got.CustomerRing.Missing, want.CustomerRing.Missing)
	}
}
