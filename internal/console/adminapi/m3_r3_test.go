package adminapi

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// M3-T3 fix round 3: the event stream after a change, the device-visibility
// cache after a narrowed credential, the per-write deadline, and the probe's
// audit row.

func summaryOf(t *testing.T, ev sseEvent) FleetSummaryResponse {
	t.Helper()
	var s FleetSummaryResponse
	if err := json.Unmarshal([]byte(ev.Data), &s); err != nil {
		t.Fatalf("fleet.state: %v %s", err, ev.Data)
	}
	return s
}

// r3 M1: a stream that connects right after a pause through the API, inside the
// share window of an evaluation made before the pause, gets the fleet after the
// pause, the same counts GET /fleet/summary answers; and a stream already open
// shows the pause on its next fleet.state.
func TestM3EventStreamAfterAPauseServesTheFleetAfterIt(t *testing.T) { // F18, r3 M1
	e := newEnv(t)
	src := &fakeSource{}
	// A share window far longer than the test: only the change generation may invalidate it.
	e.svc.Events, e.svc.sseRefresh, e.svc.sseGap, e.svc.sseShare = src, time.Hour, 10*time.Millisecond, time.Hour
	owner := e.login("admin", pw)
	a, _ := e.openStream("/api/v1/events?topics=fleet", owner)
	before := summaryOf(t, a.next("fleet.state", 5*time.Second))

	e.addAdmin("m3-r3-op", "operator", nil)
	op := e.login("m3-r3-op", pw)
	e.mustReq("POST", "/api/v1/devices/"+e.held.ID+"/pause", op, DevicePauseRequest{Reason: "r3", Indefinite: true}, 200)
	want := decode[FleetSummaryResponse](t, mustGet(e, "/api/v1/fleet/summary", owner))
	if want.Devices.Paused != before.Devices.Paused+1 {
		t.Fatalf("the pause did not show in /fleet/summary: paused %d before, %d after", before.Devices.Paused, want.Devices.Paused)
	}

	b, _ := e.openStream("/api/v1/events?topics=fleet", owner)
	got := summaryOf(t, b.next("fleet.state", 5*time.Second))
	if got.Devices.Paused != want.Devices.Paused || got.States["PAUSED"] != want.States["PAUSED"] || got.States["OK"] != want.States["OK"] {
		t.Errorf("a stream connecting after the pause got paused %d (states %v); /fleet/summary says paused %d (states %v)",
			got.Devices.Paused, got.States, want.Devices.Paused, want.States)
	}
	if ga := summaryOf(t, a.next("fleet.state", 5*time.Second)); ga.Devices.Paused != want.Devices.Paused {
		t.Errorf("the open stream's next fleet.state after the pause has paused %d, want %d", ga.Devices.Paused, want.Devices.Paused)
	}
}

// r3 M1: a change that reaches the console the way an agent's does (written to
// the store, then a hub signal) shows on every open stream of the scope in the
// first fleet.state after the signal, one gap later, even though the streams
// share an evaluation made before the change.
func TestM3EventStreamOpenStreamsConvergeWithinOneGap(t *testing.T) { // F18, r3 M1
	e := newEnv(t)
	src := &fakeSource{}
	e.svc.Events, e.svc.sseRefresh, e.svc.sseGap, e.svc.sseShare = src, time.Hour, 10*time.Millisecond, time.Hour
	owner := e.login("admin", pw)
	s1, _ := e.openStream("/api/v1/events?topics=fleet", owner)
	s2, _ := e.openStream("/api/v1/events?topics=fleet", owner)
	before := summaryOf(t, s1.next("fleet.state", 5*time.Second))
	s2.next("fleet.state", 5*time.Second)

	if _, err := e.st.PauseDeviceAudited(context.Background(), store.ScopeAll(), e.held.ID, store.PauseRequest{Reason: "agent path"},
		store.AuditActor{Username: "r3-test", Method: "api"}); err != nil {
		t.Fatal(err)
	}
	src.send(Signal{Kind: SignalChange, DeviceID: e.held.ID})
	for i, st := range []*sseStream{s1, s2} {
		if got := summaryOf(t, st.next("fleet.state", 2*time.Second)); got.Devices.Paused != before.Devices.Paused+1 {
			t.Errorf("stream %d: the first fleet.state after the signal has paused %d, want %d", i+1, got.Devices.Paused, before.Devices.Paused+1)
		}
	}
}
