package adminapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// M3-T3 fix round 4: every API mutation that can change what the fleet
// evaluation reads advances the event stream's change generation and signals
// the hub.
//
// Run time (M3-INT B.e): TestEveryFleetMutationAdvancesTheChangeGeneration
// builds a fresh env per case (about 4 s each under -race on Windows, most of
// it argon2 at sign-in), so its cases run in parallel and share one password
// hash (adminapi_test.go). Even so, the whole adminapi package under
//
//	CGO_ENABLED=1 go test -race -count=5 ./internal/console/adminapi/
//
// takes longer than go test's default 10-minute timeout on Windows: pass
// -timeout 60m (as the M3 gates do).

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
		base := baseRoute(key)
		_, neutral := fleetNeutral[base]
		if !fleetMutationRoutes[base] && !(neutral && key == base) { // a neutral route: its main case only
			continue
		}
		m := cases[key]
		t.Run(key, func(t *testing.T) {
			t.Parallel() // each case has its own env (store, server, hub): B.e, the run time
			e := newEnv(t)
			src := &fakeSource{}
			e.svc.Events, e.svc.sseRefresh, e.svc.sseGap, e.svc.sseShare = src, time.Hour, 10*time.Millisecond, time.Hour
			if base == PatternStorageTargetProbe {
				e.realProber(newProbeBackend(), "fleet-test") // the probe changes probe facts only when a prober runs
			}
			owner := e.login("admin", pw)
			c := m.build(e, owner)()
			var open *sseStream
			if !neutral {
				// A stream open before the call, with its first fleet evaluation shared for an hour and no refresh
				// tick: only the change signal can make it render again.
				open, _ = e.openStream("/api/v1/events?topics=fleet", owner)
				open.next("fleet.state", eventWait)
			}
			g0, fleet0 := e.svc.changeGeneration(), e.fleetDigest()
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
			if base == PatternSystemJobRun {
				jobBackground.Wait() // the job run-now accepted has written what it writes (the only subtest that adds to it)
			}
			src.mu.Lock()
			told := len(src.changed) - told0
			src.mu.Unlock()
			if neutral {
				// The oracle that does not trust either list: a route listed as fleet-neutral must leave
				// every table the fleet evaluation reads (and the fleet routes write) as it was, and signal
				// nothing. A fleet route moved into fleetNeutral fails here.
				if e.fleetDigest() != fleet0 {
					t.Errorf("%s is listed fleet-neutral (%q) but changed fleet data: it must signal", base, fleetNeutral[base])
				}
				if g := e.svc.changeGeneration(); g != g0 || told != 0 {
					t.Errorf("a fleet-neutral route signalled: generation %d -> %d, hub told %d times", g0, g, told)
				}
				return
			}
			if g := e.svc.changeGeneration(); g <= g0 {
				t.Errorf("the change generation stayed at %d: an event stream would keep serving the fleet from before this change", g0)
			}
			if told < 1 {
				t.Error("the hub was not told: open streams would not re-render")
			}
			// The behaviour: the open stream renders again, and a stream opening now gets the fleet after
			// the call (the shared evaluation from before it is not reused).
			open.next("fleet.state", eventWait)
			wantSum := decode[FleetSummaryResponse](t, mustGet(e, "/api/v1/fleet/summary", owner))
			later, _ := e.openStream("/api/v1/events?topics=fleet", owner)
			got := summaryOf(t, later.next("fleet.state", eventWait))
			if !reflect.DeepEqual(got.Devices, wantSum.Devices) || !reflect.DeepEqual(got.CustomerRing, wantSum.CustomerRing) || !reflect.DeepEqual(got.States, wantSum.States) {
				t.Errorf("a stream opened after the call shows devices %+v ring %+v states %v; /fleet/summary says %+v %+v %v",
					got.Devices, got.CustomerRing, got.States, wantSum.Devices, wantSum.CustomerRing, wantSum.States)
			}
		})
	}
}

// fleetDigest is a digest of the tables the fleet evaluation reads and the fleet
// routes write (customers, devices, runs and snapshots, policies, revisions and
// overrides, settings, commands, enrollment tokens, storage targets, escrows,
// maintenance schedule, releases, alarm acknowledgements). Columns that move on
// every read or contact are left out.
func (e *env) fleetDigest() string {
	e.t.Helper()
	tables := []string{"agent_releases", "alarm_acks", "commands", "customers", "device_policy_overrides", "devices", "enrollment_tokens",
		"maintenance_schedule", "policies", "policy_revisions", "repo_key_escrow", "runs", "settings", "snapshots", "storage_targets"}
	skipCol := map[string]bool{"last_seen_at": true, "last_used_at": true}
	var b strings.Builder
	err := e.st.Read(context.Background(), func(tx *store.Tx) error {
		for _, tbl := range tables {
			r, err := tx.Query(`SELECT * FROM "` + tbl + `" ORDER BY 1`)
			if err != nil {
				return err
			}
			cols, _ := r.Columns()
			fmt.Fprintf(&b, "== %s\n", tbl)
			for r.Next() {
				vals := make([]any, len(cols))
				ptrs := make([]any, len(cols))
				for i := range vals {
					ptrs[i] = &vals[i]
				}
				if err := r.Scan(ptrs...); err != nil {
					r.Close()
					return err
				}
				for i, c := range cols {
					if !skipCol[c] {
						fmt.Fprintf(&b, "%s=%v;", c, vals[i])
					}
				}
				b.WriteByte('\n')
			}
			r.Close()
		}
		return nil
	})
	if err != nil {
		e.t.Fatalf("fleet digest: %v", err)
	}
	return b.String()
}

// M3-INT B.c, mutant m2: a fleet route that answers 4xx changed nothing and
// signals nothing (no generation step, no hub signal).
func TestFleetRouteThatFailsWith4xxSignalsNothing(t *testing.T) { // M3-INT B.c
	e := newEnv(t)
	src := &fakeSource{}
	e.svc.Events = src
	owner := e.login("admin", pw)
	for _, c := range []struct {
		method, path string
		body         any
		want         int
	}{
		{"PATCH", "/api/v1/policies/no-such-policy", map[string]any{"name": "x"}, 404},
		{"PATCH", "/api/v1/customers/no-such-customer", map[string]any{"expected": true}, 404},
		{"PUT", "/api/v1/settings/no_such_setting", map[string]any{"value": "x"}, 0},
		{"POST", "/api/v1/customers", map[string]any{"name": ""}, 400},
		{"POST", "/api/v1/devices/no-such-device/pause", DevicePauseRequest{Reason: "x", Indefinite: true}, 404},
	} {
		g0 := e.svc.changeGeneration()
		src.mu.Lock()
		told0 := len(src.changed)
		src.mu.Unlock()
		res, b := e.req(c.method, c.path, owner, c.body)
		if res.StatusCode < 400 || res.StatusCode > 499 || (c.want != 0 && res.StatusCode != c.want) {
			t.Fatalf("%s %s = %d %s, want a 4xx (%d)", c.method, c.path, res.StatusCode, b, c.want)
		}
		src.mu.Lock()
		told := len(src.changed) - told0
		src.mu.Unlock()
		if g := e.svc.changeGeneration(); g != g0 || told != 0 {
			t.Errorf("%s %s answered %d and signalled: generation %d -> %d, hub told %d times", c.method, c.path, res.StatusCode, g0, g, told)
		}
	}
}

// M3-INT B.a: a fleet route whose store write commits and whose follow-up then
// fails (500) still signals, because the change is in the store. The customer
// PATCH commits, then its response evaluates the fleet, which fails here.
func TestFleetRouteSignalsOnCommitEvenWhenTheAnswerIs500(t *testing.T) { // M3-INT B.a
	e := newEnv(t)
	src := &fakeSource{}
	e.svc.Events = src
	owner := e.login("admin", pw)
	var cust struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(e.mustReq("POST", "/api/v1/customers", owner, map[string]any{"name": "Commit Then Fail Co"}, 201), &cust); err != nil || cust.ID == "" {
		t.Fatalf("create customer: %v", err)
	}
	// The fleet evaluation's world load reads the settings table and fails without it (store.LoadWorld,
	// fleet_ever_enrolled); the customer write does not touch it.
	if err := e.st.Write(context.Background(), func(tx *store.Tx) error {
		_, err := tx.Exec(`DROP TABLE settings`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	g0 := e.svc.changeGeneration()
	src.mu.Lock()
	told0 := len(src.changed)
	src.mu.Unlock()
	res, b := e.req("PATCH", "/api/v1/customers/"+cust.ID, owner, map[string]any{"expected": true})
	if res.StatusCode != 500 {
		t.Fatalf("PATCH with a failing follow-up evaluation = %d %s, want 500", res.StatusCode, b)
	}
	c, err := e.st.GetCustomer(context.Background(), store.ScopeAll(), cust.ID)
	if err != nil || !c.Expected {
		t.Fatalf("the PATCH did not commit (expected %v, err %v): the test needs commit-then-fail", c.Expected, err)
	}
	src.mu.Lock()
	told := len(src.changed) - told0
	src.mu.Unlock()
	if g := e.svc.changeGeneration(); g <= g0 || told < 1 {
		t.Errorf("a committed change answered 500 and did not signal: generation %d -> %d, hub told %d times", g0, g, told)
	}
}

// M3-INT B.a: the probe ran (its facts changed), then its audit row could not be
// written (500): it still signals.
func TestStorageProbeSignalsWhenItRanEvenIfTheAnswerIs500(t *testing.T) { // M3-INT B.a
	e := newEnv(t)
	src := &fakeSource{}
	e.svc.Events = src
	owner := e.login("admin", pw)
	target := e.firstStorageTarget()
	e.realProber(newProbeBackend(), "console-a")
	e.setAuditRefusal(true)
	g0 := e.svc.changeGeneration()
	res, b := e.req("POST", "/api/v1/storage-targets/"+target+"/probe", owner, nil)
	e.setAuditRefusal(false)
	if res.StatusCode != 500 {
		t.Fatalf("probe whose audit row is refused = %d %s, want 500", res.StatusCode, b)
	}
	src.mu.Lock()
	told := len(src.changed)
	src.mu.Unlock()
	if g := e.svc.changeGeneration(); g <= g0 || told < 1 {
		t.Errorf("a probe that ran answered 500 and did not signal: generation %d -> %d, hub told %d times", g0, g, told)
	}
}

// orderSource is a hub that, when told of a change, reads the customer the test
// changed, to prove the signal comes after the store write has committed.
type orderSource struct {
	fakeSource
	e        *env
	id       string
	armed    atomic.Bool
	seen     atomic.Int32 // 1: the change was visible at the signal; 2: it was not
	checking sync.Mutex
}

func (o *orderSource) DeviceChanged(id string) {
	if o.armed.Load() {
		o.checking.Lock()
		c, err := o.e.st.GetCustomer(context.Background(), store.ScopeAll(), o.id)
		if err == nil && c.Expected {
			o.seen.CompareAndSwap(0, 1)
		} else {
			o.seen.CompareAndSwap(0, 2)
		}
		o.checking.Unlock()
	}
	o.fakeSource.DeviceChanged(id)
}

// M3-INT B.b, mutant m1: the fleet signal fires after the store write, never
// before it: at the signal the change is already readable.
func TestFleetSignalFiresAfterTheStoreWrite(t *testing.T) { // M3-INT B.b
	e := newEnv(t)
	src := &orderSource{e: e}
	e.svc.Events = src
	owner := e.login("admin", pw)
	var cust struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(e.mustReq("POST", "/api/v1/customers", owner, map[string]any{"name": "Signal Order Co"}, 201), &cust); err != nil || cust.ID == "" {
		t.Fatalf("create customer: %v", err)
	}
	src.id = cust.ID
	src.armed.Store(true)
	e.mustReq("PATCH", "/api/v1/customers/"+cust.ID, owner, map[string]any{"expected": true}, 200)
	switch src.seen.Load() {
	case 0:
		t.Fatal("the PATCH signalled nothing")
	case 2:
		t.Fatal("the signal fired before the store write: a stream re-rendering on it would read the fleet from before the change")
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
	before := summaryOf(t, a.next("fleet.state", eventWait))

	e.mustReq("PATCH", "/api/v1/customers/"+cust.ID, owner, map[string]any{"expected": true}, 200)
	want := decode[FleetSummaryResponse](t, mustGet(e, "/api/v1/fleet/summary", owner))
	if want.CustomerRing.Missing != before.CustomerRing.Missing+1 {
		t.Fatalf("fixture: /fleet/summary missing %d before, %d after", before.CustomerRing.Missing, want.CustomerRing.Missing)
	}
	b, _ := e.openStream("/api/v1/events?topics=fleet", owner)
	if got := summaryOf(t, b.next("fleet.state", eventWait)); got.CustomerRing.Missing != want.CustomerRing.Missing {
		t.Errorf("a stream connecting after the PATCH shows customer_ring.missing %d; /fleet/summary says %d", got.CustomerRing.Missing, want.CustomerRing.Missing)
	}
	if got := summaryOf(t, a.next("fleet.state", eventWait)); got.CustomerRing.Missing != want.CustomerRing.Missing {
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
	before := summaryOf(t, a.next("fleet.state", eventWait))
	e.mustReq("POST", "/api/v1/devices/"+e.held.ID+"/pause", owner, DevicePauseRequest{Reason: "no hub", Indefinite: true}, 200)
	b, _ := e.openStream("/api/v1/events?topics=fleet", owner)
	if got := summaryOf(t, b.next("fleet.state", eventWait)); got.Devices.Paused != before.Devices.Paused+1 {
		t.Errorf("without a hub, a stream connecting after the pause shows paused %d, want %d", got.Devices.Paused, before.Devices.Paused+1)
	}
}

// r4 LOW-4 through the API, as amended by M3-INT B.d: an owner who loses the
// customer a shared view is for gets 404 renaming it, but sees it in GET /views
// marked locked, and may make it private or delete it; an all-customer admin can
// still delete such a view.
func TestM3SharedViewOwnerWhoLostTheAudienceGets404(t *testing.T) { // F06, r4 LOW-4, M3-INT B.d
	e := newEnv(t)
	admin := e.login("admin", pw)
	nw, summit := e.customerNamed("Northwind Dental"), e.customerNamed("Summit Eye Care")
	e.addAdmin("m3-r4-owner", "read_only", []string{nw.ID, summit.ID})
	sess := e.login("m3-r4-owner", pw)
	v := decode[ViewResponse](t, e.mustReq("POST", "/api/v1/views", sess, CreateViewRequest{Name: "For Summit", Entity: "devices", Shared: true,
		Audience: &ViewAudienceJSON{CustomerIDs: []string{summit.ID}}}, 201))
	v2 := decode[ViewResponse](t, e.mustReq("POST", "/api/v1/views", sess, CreateViewRequest{Name: "Also for Summit", Entity: "devices", Shared: true,
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
	f := json.RawMessage(`{"q":"pushed"}`)
	if res, b := e.req("PATCH", "/api/v1/views/"+v.ID, sess, PatchViewRequest{Filter: &f}); res.StatusCode != 404 {
		t.Errorf("filter change after losing Summit = %d %s, want 404", res.StatusCode, b)
	}
	// The list and the actions agree: the view is listed as the owner's, locked.
	listed := func(s *session, id string) (SavedViewJSON, bool) {
		for _, lv := range decode[ViewListResponse](t, mustGet(e, "/api/v1/views", s)).Views {
			if lv.ID == id {
				return lv, true
			}
		}
		return SavedViewJSON{}, false
	}
	if lv, ok := listed(sess, v.ID); !ok || !lv.Mine || !lv.Locked {
		t.Errorf("GET /views after losing Summit: %+v listed=%v, want the owner's view marked locked", lv, ok)
	}
	// Both actions that only reduce exposure are allowed.
	no := false
	if res, b := e.req("PATCH", "/api/v1/views/"+v.ID, sess, PatchViewRequest{Shared: &no}); res.StatusCode != 200 {
		t.Errorf("making it private after losing Summit = %d %s, want 200", res.StatusCode, b)
	}
	if lv, ok := listed(sess, v.ID); !ok || lv.Shared || lv.Locked {
		t.Errorf("after making it private: %+v listed=%v, want private and no longer locked", lv, ok)
	}
	if res, b := e.req("DELETE", "/api/v1/views/"+v.ID, sess, nil); res.StatusCode != 204 {
		t.Errorf("delete after losing Summit = %d %s, want 204", res.StatusCode, b)
	}
	// The owner's other view for Summit: an all-customer admin can still delete it.
	if res, b := e.req("DELETE", "/api/v1/views/"+v2.ID, admin, nil); res.StatusCode != 204 {
		t.Errorf("an all-customer admin deleting it = %d %s, want 204", res.StatusCode, b)
	}
}
