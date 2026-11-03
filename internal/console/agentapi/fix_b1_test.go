package agentapi

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/console/enroll"
	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// M2-FIX-B1: concurrent enrollments on one token (F4), heartbeats of a cloned
// pair (F9) and the escrow verification recorded at enrollment (F7).

// slowBackend is the fixtures admin path with a slow repository init, so that
// concurrent enrollments really overlap inside the handler.
type slowBackend struct {
	*fixtures.Backend
	delay time.Duration
}

func (b slowBackend) Init(ctx context.Context, req adminpath.InitRequest) (adminpath.InitResult, error) {
	time.Sleep(b.delay)
	return b.Backend.Init(ctx, req)
}

func (h *harness) mintUses(n int) (string, store.EnrollmentToken) {
	h.t.Helper()
	ctx := context.Background()
	cs, _ := h.st.ListCustomers(ctx, store.ScopeAll())
	for _, c := range cs {
		if c.Name == "Northwind Dental" {
			plain, tok, err := enroll.Mint(ctx, h.st, store.ScopeAll(), c.ID, enroll.MintOptions{Expires: 24 * time.Hour, MaxUses: &n})
			if err != nil {
				h.t.Fatal(err)
			}
			return plain, tok
		}
	}
	h.t.Fatal("customer missing")
	return "", store.EnrollmentToken{}
}

func machineReq(i int) api.EnrollRequest {
	r := enrollReq(nonce(byte(100 + i)))
	r.Hostname = fmt.Sprintf("PUSH-PC-%02d", i)
	r.HWFingerprint = strings.Repeat(fmt.Sprintf("%02x", 0x40+i), 16)
	return r
}

// F4: an RMM push of five machines on one five-use token, all at once.
func TestFixB1ConcurrentEnrollmentsOnOneTokenAllSucceed(t *testing.T) {
	h := newHarness(t)
	h.svc.Backend = slowBackend{Backend: h.be, delay: 150 * time.Millisecond}
	// This test is about serialising, not about the bound (the next test covers
	// the bound): a long wait keeps it stable on a loaded CI machine under -race.
	h.svc.enrollWait = 5 * time.Minute
	plain, tok := h.mintUses(5)

	const n = 5
	codes := make([]int, n)
	ids := make([]string, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			r := h.do("POST", api.RouteEnroll, plain, machineReq(i))
			codes[i] = r.code
			if r.code == 200 {
				var out api.EnrollResponse
				if err := json.Unmarshal(r.body, &out); err == nil {
					ids[i] = out.DeviceID
				}
			}
		}()
	}
	close(start)
	wg.Wait()

	seen := map[string]bool{}
	for i := 0; i < n; i++ {
		if codes[i] != 200 || ids[i] == "" {
			t.Fatalf("enrollment %d: status %d (all: %v)", i, codes[i], codes)
		}
		if seen[ids[i]] {
			t.Fatalf("two enrollments returned device %s", ids[i])
		}
		seen[ids[i]] = true
	}
	// One device each, token used 5 of 5.
	l, _ := h.st.ListDevices(context.Background(), store.ScopeAll(), store.ListDevicesOptions{Search: "PUSH-PC-"})
	if len(l.Items) != n {
		t.Fatalf("devices = %d, want %d", len(l.Items), n)
	}
	got, err := h.st.GetEnrollmentTokenByHash(context.Background(), tok.TokenHash)
	if err != nil || got.UsedCount != n {
		t.Fatalf("token used %d (%v), want %d", got.UsedCount, err, n)
	}
	if h.svc.enrollQueue.held() != 0 {
		t.Fatalf("the per-token queue still tracks %d keys", h.svc.enrollQueue.held())
	}
	// A sixth machine is refused: the token is exhausted.
	r := h.do("POST", api.RouteEnroll, plain, machineReq(6))
	if r.code != 401 || r.problem(t).Code != api.CodeUnauthorized {
		t.Fatalf("sixth enrollment: %d %s", r.code, r.body)
	}
	if l, _ := h.st.ListDevices(context.Background(), store.ScopeAll(), store.ListDevicesOptions{Search: "PUSH-PC-"}); len(l.Items) != n {
		t.Fatalf("the sixth enrollment created a device: %d", len(l.Items))
	}
}

// F4: a wait that runs out is a 429 with Retry-After and exactly one audit row.
func TestFixB1EnrollmentWaitTimeoutIs429WithOneAuditRow(t *testing.T) {
	h := newHarness(t)
	h.svc.Backend = slowBackend{Backend: h.be, delay: 600 * time.Millisecond}
	h.svc.enrollWait = 50 * time.Millisecond
	plain, _ := h.mintUses(5)

	first := make(chan resp, 1)
	go func() { first <- h.do("POST", api.RouteEnroll, plain, machineReq(1)) }()
	// Wait until the first enrollment holds the token (it is inside the slow init).
	deadline := time.Now().Add(5 * time.Second)
	for h.be.Count(adminpath.MethodInit) == 0 && h.svc.enrollQueue.held() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the first enrollment never started")
		}
		time.Sleep(5 * time.Millisecond)
	}
	r := h.do("POST", api.RouteEnroll, plain, machineReq(2))
	if r.code != 429 || r.hdr.Get("Retry-After") == "" || r.problem(t).Code != api.CodeRateLimited {
		t.Fatalf("second enrollment during a long first one: %d %s", r.code, r.body)
	}
	if f := <-first; f.code != 200 {
		t.Fatalf("first enrollment: %d %s", f.code, f.body)
	}
	evs, _ := h.st.ListAudit(context.Background(), store.ScopeAll(), store.AuditFilter{Action: "agent.enroll.denied"}, store.Page{Limit: 50})
	busy := 0
	for _, e := range evs.Items {
		if strings.Contains(string(e.Details), `"token_busy"`) {
			busy++
			if e.Result != "denied" || e.ActorType != "system" {
				t.Fatalf("audit row: %+v", e)
			}
		}
	}
	if busy != 1 {
		t.Fatalf("token_busy audit rows = %d, want 1 (%d denied rows)", busy, len(evs.Items))
	}
}

// F4 (report only, the default is unchanged): one NAT address pushing five
// machines at once stays inside the per-IP bucket of 10 per minute.
func TestFixB1FiveMachinesBehindOneNATFitThePerIPBucket(t *testing.T) {
	h := newHarness(t)
	plain, _ := h.mintUses(5)
	for i := 0; i < 5; i++ {
		if r := h.do("POST", api.RouteEnroll, plain, machineReq(i)); r.code != 200 {
			t.Fatalf("machine %d from one address: %d %s", i, r.code, r.body)
		}
	}
}

// F9: two machines heartbeating with one device token at the normal cadence
// while the identity conflict stands are never refused.
func TestFixB1ClonedPairHeartbeatsAreNotRateLimited(t *testing.T) {
	h := newHarness(t)
	e := h.enrolled()
	tok := string(e.DeviceToken)
	orig, clone := strings.Repeat("ab", 16), strings.Repeat("cd", 16)
	if r := h.beat(tok, clone); r.code != 200 {
		t.Fatalf("raising heartbeat: %d", r.code)
	}
	dev, _ := h.st.GetDevice(context.Background(), store.ScopeAll(), e.DeviceID)
	if dev.IdentityConflictAt == nil {
		t.Fatal("the conflict was not raised")
	}
	send := func(fp string) resp {
		hb := hbReq(h)
		hb.HWFingerprint = fp
		return h.do("POST", api.RouteHeartbeat, tok, hb)
	}
	// The agent's cadence is 60 s with jitter; the two machines are out of phase
	// by anything from 0 to 60 s. Worst case first: one second apart.
	for _, gap := range []time.Duration{time.Second, 0, 5 * time.Second, 19 * time.Second, 30 * time.Second} {
		for round := 0; round < 4; round++ {
			h.clk.Advance(60*time.Second - gap)
			if r := send(orig); r.code != 200 {
				t.Fatalf("gap %v round %d, original: %d %s", gap, round, r.code, r.body)
			}
			h.clk.Advance(gap)
			if r := send(clone); r.code != 200 {
				t.Fatalf("gap %v round %d, clone: %d %s", gap, round, r.code, r.body)
			}
		}
	}
	// The pair keeps showing the conflict.
	if got := h.deviceState(e.DeviceID).Primary; got != state.StateIdentityConflict {
		t.Fatalf("board state = %s", got)
	}
}

// F9: the larger allowance of a flagged device still stops one hammering client.
func TestFixB1FlaggedDeviceStillLimitsAHammeringClient(t *testing.T) {
	h := newHarness(t)
	e := h.enrolled()
	tok := string(e.DeviceToken)
	if r := h.beat(tok, strings.Repeat("cd", 16)); r.code != 200 {
		t.Fatalf("raising heartbeat: %d", r.code)
	}
	h.clk.Advance(21 * time.Second)
	ok, limited := 0, 0
	for i := 0; i < 12; i++ {
		hb := hbReq(h)
		hb.HWFingerprint = strings.Repeat("cd", 16)
		switch r := h.do("POST", api.RouteHeartbeat, tok, hb); r.code {
		case 200:
			ok++
		case 429:
			limited++
			if r.hdr.Get("Retry-After") == "" {
				t.Fatal("429 without Retry-After")
			}
		default:
			t.Fatalf("heartbeat %d: %d %s", i, r.code, r.body)
		}
	}
	if ok != conflictHeartbeatMachines || limited != 12-conflictHeartbeatMachines {
		t.Fatalf("12 heartbeats in one instant: %d accepted, %d refused; want %d accepted", ok, limited, conflictHeartbeatMachines)
	}
	// An unflagged device keeps the one-per-interval limit.
	other, r := h.enroll(nonce(77))
	if r.code != 200 {
		t.Fatalf("enroll: %d", r.code)
	}
	if r := h.do("POST", api.RouteHeartbeat, string(other.DeviceToken), hbReq(h)); r.code != 200 {
		t.Fatalf("first heartbeat: %d", r.code)
	}
	if r := h.do("POST", api.RouteHeartbeat, string(other.DeviceToken), hbReq(h)); r.code != 429 {
		t.Fatalf("an unflagged device's second heartbeat in the same instant: %d", r.code)
	}
}

// F7: enrollment's successful cat config is the escrow verification.
func TestFixB1EnrollmentRecordsTheEscrowVerification(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	out, r := h.enroll(nonce(31))
	if r.code != 200 {
		t.Fatalf("enroll: %d %s", r.code, r.body)
	}
	esc, err := h.st.ActiveEscrow(ctx, store.ScopeAll(), out.DeviceID)
	if err != nil || esc.VerifiedAt == nil || !esc.VerifiedAt.Equal(h.clk.Now()) || esc.LastVerifyErrorClass != "" {
		t.Fatalf("escrow after enrollment: %+v %v", esc, err)
	}
	// No audit noise: the enrollment writes device.enroll and agent.enroll, nothing else.
	evs, _ := h.st.ListAudit(ctx, store.ScopeAll(), store.AuditFilter{ObjectID: out.DeviceID}, store.Page{Limit: 20})
	var actions []string
	for _, e := range evs.Items {
		actions = append(actions, e.Action)
	}
	if len(actions) != 2 {
		t.Fatalf("audit rows for the enrollment: %v", actions)
	}
	// A failed cat config at enrollment records nothing (the enrollment is refused and retried).
	h.be.Errors = map[string]error{adminpath.MethodCatConfig: adminpath.NewError(adminpath.CodeStorageUnreachable, "")}
	if _, r := h.enroll(nonce(32)); r.code != 503 {
		t.Fatalf("enroll with a failing cat config: %d %s", r.code, r.body)
	}
	h.be.Errors = nil
	l, _ := h.st.ListDevices(ctx, store.ScopeAll(), store.ListDevicesOptions{Search: "TEST-PC-01"})
	if len(l.Items) != 2 {
		t.Fatalf("devices after the refused enrollment: %d, want 2 (it stays in place for the retry)", len(l.Items))
	}
	for _, d := range l.Items {
		if d.ID == out.DeviceID {
			continue
		}
		if esc, err := h.st.ActiveEscrow(ctx, store.ScopeAll(), d.ID); err != nil || esc.VerifiedAt != nil {
			t.Fatalf("an enrollment whose cat config failed was recorded as verified: %+v %v", esc, err)
		}
	}
	// Confirmed, heartbeating, and past the 24 h grace: no ESCROW_UNVERIFIED.
	tok := string(out.DeviceToken)
	if r := h.do("POST", api.RouteHeartbeat, tok, hbReq(h)); r.code != 200 {
		t.Fatalf("heartbeat: %d", r.code)
	}
	h.clk.Advance(25 * time.Hour)
	if r := h.do("POST", api.RouteHeartbeat, tok, hbReq(h)); r.code != 200 {
		t.Fatalf("heartbeat: %d", r.code)
	}
	ds := h.deviceState(out.DeviceID)
	for _, f := range ds.Flags {
		if f == state.StateEscrowUnverified {
			t.Fatalf("ESCROW_UNVERIFIED 25 h after a verified enrollment: %+v", ds)
		}
	}
}
