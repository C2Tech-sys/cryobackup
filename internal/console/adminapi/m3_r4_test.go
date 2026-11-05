package adminapi

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
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

// blockingProber probes until the request that started it is cancelled (the
// client went away mid-probe).
type blockingProber struct{ started chan struct{} }

func (b blockingProber) ProbeNow(ctx context.Context) (bool, error) {
	close(b.started)
	<-ctx.Done()
	return true, ctx.Err()
}

// r4 LOW-1: a client that disconnects mid-probe still leaves exactly one audit
// row: the row is written after the run, so it must not use the request's
// (now cancelled) context.
func TestM3StorageProbeAuditedWhenTheClientDisconnects(t *testing.T) { // F19, r4 LOW-1
	e := newEnv(t)
	owner := e.login("admin", pw)
	target := e.firstStorageTarget()
	pr := blockingProber{started: make(chan struct{})}
	e.svc.Prober = pr
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "POST", e.srv.URL+"/api/v1/storage-targets/"+target+"/probe", nil)
	for _, c := range owner.cookies {
		req.AddCookie(c)
	}
	req.Header.Set("Origin", e.srv.URL)
	req.Header.Set(auth.CSRFHeader, owner.csrf)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if res, err := http.DefaultClient.Do(req); err == nil {
			res.Body.Close()
		}
	}()
	select {
	case <-pr.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the probe never started")
	}
	cancel() // the client goes away while the prober runs
	<-done
	rows := func() int { return e.auditCount("storage_target.probe", target) }
	deadline := time.Now().Add(5 * time.Second)
	for rows() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond) // and no second row follows
	if n := rows(); n != 1 {
		t.Fatalf("%d audit rows for a probe whose client disconnected, want exactly 1", n)
	}
}

// r4 LOW-2: without a hub (Events nil) the API's own counter is the whole change
// generation. A stream that connects right after a pause, inside the share
// window of an evaluation from before it, still gets the fleet after the pause.
func TestM3EventStreamWithoutAHubSeesAnAPIChange(t *testing.T) { // F18, r4 LOW-2
	e := newEnv(t)
	e.svc.Events, e.svc.sseRefresh, e.svc.sseGap, e.svc.sseShare = nil, time.Hour, 10*time.Millisecond, time.Hour
	owner := e.login("admin", pw)
	a, _ := e.openStream("/api/v1/events?topics=fleet", owner)
	before := summaryOf(t, a.next("fleet.state", 5*time.Second))
	e.mustReq("POST", "/api/v1/devices/"+e.held.ID+"/pause", owner, DevicePauseRequest{Reason: "no hub", Indefinite: true}, 200)
	b, _ := e.openStream("/api/v1/events?topics=fleet", owner)
	if got := summaryOf(t, b.next("fleet.state", 5*time.Second)); got.Devices.Paused != before.Devices.Paused+1 {
		t.Errorf("without a hub, a stream connecting after the pause shows paused %d, want %d", got.Devices.Paused, before.Devices.Paused+1)
	}
}

// r4 LOW-4 through the API: an owner who loses the customer a shared view is for
// gets 404 renaming or deleting it; an all-customer admin can still delete it.
func TestM3SharedViewOwnerWhoLostTheAudienceGets404(t *testing.T) { // F06, r4 LOW-4
	e := newEnv(t)
	admin := e.login("admin", pw)
	nw, summit := e.customerNamed("Northwind Dental"), e.customerNamed("Summit Eye Care")
	e.addAdmin("m3-r4-owner", "read_only", []string{nw.ID, summit.ID})
	sess := e.login("m3-r4-owner", pw)
	v := decode[ViewResponse](t, e.mustReq("POST", "/api/v1/views", sess, CreateViewRequest{Name: "For Summit", Entity: "devices", Shared: true,
		Audience: &ViewAudienceJSON{CustomerIDs: []string{summit.ID}}}, 201))
	acct, err := e.st.GetAdminByUsername(context.Background(), "m3-r4-owner")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.Write(context.Background(), func(tx *store.Tx) error {
		_, _, err := tx.UpdateAdminRoleScope(acct.ID, "read_only", []string{nw.ID})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	n := "pushed to Summit"
	if res, b := e.req("PATCH", "/api/v1/views/"+v.ID, sess, PatchViewRequest{Name: &n}); res.StatusCode != 404 {
		t.Errorf("rename after losing Summit = %d %s, want 404", res.StatusCode, b)
	}
	if res, b := e.req("DELETE", "/api/v1/views/"+v.ID, sess, nil); res.StatusCode != 404 {
		t.Errorf("delete after losing Summit = %d %s, want 404", res.StatusCode, b)
	}
	if res, b := e.req("DELETE", "/api/v1/views/"+v.ID, admin, nil); res.StatusCode != 204 {
		t.Errorf("an all-customer admin deleting it = %d %s, want 204", res.StatusCode, b)
	}
}
