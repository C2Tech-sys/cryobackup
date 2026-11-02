package agentapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/console/enroll"
	"github.com/c2tech-sys/cryobackup/internal/console/escrow"
	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/restic"
	"github.com/c2tech-sys/cryobackup/internal/ulid"
)

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *testClock) Advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

type pub struct {
	mu       sync.Mutex
	changed  map[string]int
	progress map[string]string
}

func (p *pub) DeviceChanged(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.changed[id]++
}
func (p *pub) RunProgress(dev, run string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.progress[dev] = run
}

type harness struct {
	t      *testing.T
	st     *store.Store
	svc    *Service
	be     *fixtures.Backend
	clk    *testClock
	srv    *httptest.Server
	pub    *pub
	keys   *escrow.Keys
	ids    *ulid.Generator
	synced int
	token  string // enrollment token for an escrowed customer
	heldTk string // enrollment token for a customer-held customer
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()
	clk := &testClock{t: fixtures.Now()}
	st, err := store.OpenMemory(ctx, clk.Now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := store.LoadFixtures(ctx, st, clk.Now()); err != nil {
		t.Fatal(err)
	}
	mk, _ := escrow.NewMasterKey(bytes.Repeat([]byte{9}, 32))
	h := &harness{t: t, st: st, clk: clk, be: &fixtures.Backend{}, pub: &pub{changed: map[string]int{}, progress: map[string]string{}},
		keys: &escrow.Keys{Master: mk, Store: st}, ids: ulid.NewGenerator(clk.Now, rand.Reader)}
	h.svc = &Service{Store: st, Keys: h.keys, Backend: h.be, GatewayURL: "https://gateway.example.invalid:9443", Clock: clk.Now,
		Pub: h.pub, HtpasswdSync: func() { h.synced++ }}
	mux := http.NewServeMux()
	h.svc.Register(testRegistrar{mux})
	h.srv = httptest.NewServer(mux)
	t.Cleanup(h.srv.Close)

	mint := func(name string) string {
		cs, _ := st.ListCustomers(ctx, store.ScopeAll())
		for _, c := range cs {
			if c.Name == name {
				plain, _, err := enroll.Mint(ctx, st, store.ScopeAll(), c.ID, enroll.MintOptions{Expires: 24 * time.Hour})
				if err != nil {
					t.Fatal(err)
				}
				return plain
			}
		}
		t.Fatalf("customer %s missing", name)
		return ""
	}
	h.token, h.heldTk = mint("Northwind Dental"), mint("Pinecrest Legal")
	return h
}

type testRegistrar struct{ m *http.ServeMux }

func (r testRegistrar) Handle(p string, h http.Handler) { r.m.Handle(p, h) }

type resp struct {
	code int
	hdr  http.Header
	body []byte
}

func (r resp) json(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.body, v); err != nil {
		t.Fatalf("body %q: %v", r.body, err)
	}
}

func (r resp) problem(t *testing.T) api.Error {
	t.Helper()
	if ct := r.hdr.Get("Content-Type"); ct != api.ContentTypeProblem {
		t.Fatalf("content type %q (status %d, body %s)", ct, r.code, r.body)
	}
	var e api.Error
	r.json(t, &e)
	return e
}

func (h *harness) do(method, path, token string, body any, hdr ...string) resp {
	h.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(b)
	case string:
		rd = strings.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			h.t.Fatal(err)
		}
		rd = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, h.srv.URL+path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return resp{res.StatusCode, res.Header, b}
}

// padded is a valid JSON object larger than n bytes.
func padded(n int) []byte {
	return append(append([]byte(`{"pad":"`), bytes.Repeat([]byte("x"), n)...), []byte(`"}`)...)
}

func nonce(seed byte) string { return strings.Repeat(fmt.Sprintf("%02x", seed), 16) }

func enrollReq(n string) api.EnrollRequest {
	return api.EnrollRequest{Nonce: n, Hostname: "TEST-PC-01", OS: "windows", OSVersion: "Windows 11 Pro", Arch: api.ArchAMD64,
		DeviceClass: api.DeviceClassDesktop, HWFingerprint: strings.Repeat("ab", 16), AgentVersion: "1.4.2", ResticVersion: "0.19.1"}
}

func (h *harness) enroll(n string) (api.EnrollResponse, resp) {
	h.t.Helper()
	r := h.do("POST", api.RouteEnroll, h.token, enrollReq(n))
	var out api.EnrollResponse
	if r.code == 200 {
		r.json(h.t, &out)
	}
	return out, r
}

func hbReq(h *harness) api.HeartbeatRequest {
	return api.HeartbeatRequest{AgentVersion: "1.4.2", ResticVersion: "0.19.1", UptimeS: 3600, DeviceTime: h.clk.Now().Add(3 * time.Minute),
		ServiceState: api.ServiceIdle, PolicyETag: "", HWFingerprint: strings.Repeat("ab", 16)}
}

// ---- enroll ----

func TestEnrollCreatesTheDeviceAndDeliversSecretsOnce(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	out, r := h.enroll(nonce(1))
	if r.code != 200 {
		t.Fatalf("enroll: %d %s", r.code, r.body)
	}
	if !enroll.WellFormed(string(out.DeviceToken)) || len(out.RepoPassword) != 64 || len(out.Gateway.Password) != 64 {
		t.Fatalf("secret shapes: %q %d %d", out.DeviceToken, len(out.RepoPassword), len(out.Gateway.Password))
	}
	if out.Gateway.Username != out.DeviceID || out.Gateway.URL != "https://gateway.example.invalid:9443" || out.Custody != api.CustodyEscrowed {
		t.Fatalf("gateway/custody: %+v", out)
	}
	if out.Policy.ETag == "" || out.Policy.Version < 1 || r.hdr.Get("Cache-Control") != "no-store" {
		t.Fatalf("policy/cache: %+v", out.Policy)
	}
	if h.synced != 1 || h.be.Count(adminpath.MethodInit) != 1 || h.be.Count(adminpath.MethodCatConfig) != 1 {
		t.Fatalf("htpasswd signal %d, init %d, cat config %d", h.synced, h.be.Count(adminpath.MethodInit), h.be.Count(adminpath.MethodCatConfig))
	}
	// The store holds hashes and wrapped blobs, never the secrets.
	dev, err := h.st.GetDevice(ctx, store.ScopeAll(), out.DeviceID)
	if err != nil || dev.Hostname != "TEST-PC-01" || dev.EnrollConfirmedAt != nil {
		t.Fatalf("device: %+v %v", dev, err)
	}
	if _, _, err := h.st.AgentLookupDeviceToken(ctx, enroll.HashToken(string(out.DeviceToken))); err != nil {
		t.Fatalf("device token must resolve by hash: %v", err)
	}
	esc, _ := h.st.ActiveEscrow(ctx, store.ScopeAll(), out.DeviceID)
	if bytes.Contains(esc.WrappedKey, []byte(out.RepoPassword)) || esc.WrapAlg != escrow.WrapAlg || esc.MasterKeyID != h.keys.Master.KeyID() {
		t.Fatalf("escrow row: %+v", esc)
	}
	if pt, err := h.keys.Master.UnwrapRepoKey(out.DeviceID, esc.KeyVersion, esc.WrappedKey); err != nil || string(pt) != string(out.RepoPassword) {
		t.Fatalf("escrow must be wrapped under the device id: %v", err)
	}
	// The audit trail carries agent.enroll and no secret.
	evs, _ := h.st.ListAudit(ctx, store.ScopeAll(), store.AuditFilter{ObjectID: out.DeviceID}, store.Page{Limit: 20})
	var seen bool
	for _, e := range evs.Items {
		seen = seen || e.Action == "agent.enroll"
		if strings.Contains(string(e.Details), string(out.RepoPassword)) || strings.Contains(string(e.Details), string(out.DeviceToken)) {
			t.Fatal("secret in audit")
		}
	}
	if !seen {
		t.Fatal("agent.enroll audit row missing")
	}
	if h.pub.changed[out.DeviceID] == 0 {
		t.Fatal("publisher not told")
	}
}

func TestEnrollReplayIsIdempotentUntilTheFirstHeartbeat(t *testing.T) {
	h := newHarness(t)
	first, r := h.enroll(nonce(2))
	if r.code != 200 {
		t.Fatalf("%d %s", r.code, r.body)
	}
	second, r := h.enroll(nonce(2))
	if r.code != 200 {
		t.Fatalf("replay: %d %s", r.code, r.body)
	}
	if second.DeviceID != first.DeviceID || second.RepoPassword != first.RepoPassword || second.Gateway.Password != first.Gateway.Password {
		t.Fatal("a replay must return the same device, repository password and gateway password")
	}
	if second.DeviceToken == first.DeviceToken {
		t.Fatal("a replay must issue a fresh device token")
	}
	// The first token was revoked by the replay; the second works.
	if c := h.do("GET", api.RoutePolicy, string(first.DeviceToken), nil).code; c != 401 {
		t.Fatalf("old token after replay: %d", c)
	}
	if c := h.do("GET", api.RoutePolicy, string(second.DeviceToken), nil).code; c != 200 {
		t.Fatalf("new token: %d", c)
	}
	// One device, one token use.
	cs, _ := h.st.ListDevices(context.Background(), store.ScopeAll(), store.ListDevicesOptions{Search: "TEST-PC-01"})
	if len(cs.Items) != 1 {
		t.Fatalf("devices: %d", len(cs.Items))
	}
	// After the first heartbeat the nonce is spent: 409 conflict.
	if r := h.do("POST", api.RouteHeartbeat, string(second.DeviceToken), hbReq(h)); r.code != 200 {
		t.Fatalf("heartbeat: %d %s", r.code, r.body)
	}
	if _, r := h.enroll(nonce(2)); r.code != 409 || r.problem(t).Code != api.CodeConflict {
		t.Fatalf("replay after confirmation: %d %s", r.code, r.body)
	}
	// A different nonce is a new device.
	third, r := h.enroll(nonce(3))
	if r.code != 200 || third.DeviceID == first.DeviceID {
		t.Fatalf("second device: %d", r.code)
	}
}

func TestEnrollFailuresAreProblemDetails(t *testing.T) {
	h := newHarness(t)
	check := func(name string, r resp, code int, apiCode string) {
		t.Helper()
		if r.code != code {
			t.Errorf("%s: status %d (%s)", name, r.code, r.body)
			return
		}
		if e := r.problem(t); e.Code != apiCode || e.Status != code {
			t.Errorf("%s: %+v", name, e)
		}
	}
	check("no token", h.do("POST", api.RouteEnroll, "", enrollReq(nonce(4))), 401, api.CodeUnauthorized)
	check("malformed token", h.do("POST", api.RouteEnroll, "short", enrollReq(nonce(4))), 401, api.CodeUnauthorized)
	check("unknown token", h.do("POST", api.RouteEnroll, strings.Repeat("A", 43), enrollReq(nonce(4))), 401, api.CodeUnauthorized)
	bad := enrollReq("zz")
	check("invalid body", h.do("POST", api.RouteEnroll, h.token, bad), 400, api.CodeInvalidRequest)
	check("not json", h.do("POST", api.RouteEnroll, h.token, "{nope"), 400, api.CodeInvalidRequest)
	check("trailing data", h.do("POST", api.RouteEnroll, h.token, `{"nonce":"`+nonce(4)+`"} {}`), 400, api.CodeInvalidRequest)
	check("body too large", h.do("POST", api.RouteEnroll, h.token, padded(api.MaxEnrollBody+10)), 413, api.CodePayloadTooLarge)
	leak := enrollReq(nonce(4))
	leak.OSVersion = `Windows at C:\Users\jdoe\AppData`
	check("path in body", h.do("POST", api.RouteEnroll, h.token, leak), 400, api.CodeInvalidRequest)
	check("customer-held custody", h.do("POST", api.RouteEnroll, h.heldTk, enrollReq(nonce(5))), 501, api.CodeCustodyCustomerHeld)
	if got := h.do("POST", api.RouteEnroll, h.heldTk, enrollReq(nonce(5))).problem(t); got.Status != 501 {
		t.Fatalf("problem body status: %+v", got)
	}
	// None of that created a device, burned a use or touched the repository.
	l, _ := h.st.ListDevices(context.Background(), store.ScopeAll(), store.ListDevicesOptions{Search: "TEST-PC-01"})
	if len(l.Items) != 0 || h.be.Count(adminpath.MethodInit) != 0 {
		t.Fatalf("failed enrollments left state behind: %d devices, %d inits", len(l.Items), h.be.Count(adminpath.MethodInit))
	}
}

func TestEnrollTokenStatesAndMasterKey(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	toks, _ := h.st.ListEnrollmentTokens(ctx, store.ScopeAll(), "")
	for _, tk := range toks {
		if tk.TokenHash == enroll.HashToken(h.token) {
			if err := h.st.RevokeEnrollmentToken(ctx, store.ScopeAll(), tk.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, r := h.enroll(nonce(6)); r.code != 401 {
		t.Fatalf("revoked token: %d %s", r.code, r.body)
	}
	// Expired.
	cs, _ := h.st.ListCustomers(ctx, store.ScopeAll())
	plain, _, _ := enroll.Mint(ctx, h.st, store.ScopeAll(), cs[0].ID, enroll.MintOptions{Expires: time.Hour})
	h.clk.Advance(2 * time.Hour)
	if r := h.do("POST", api.RouteEnroll, plain, enrollReq(nonce(7))); r.code != 401 {
		t.Fatalf("expired token: %d", r.code)
	}
	// No master key: 503 master_key_unavailable, and nothing was stored.
	h.svc.Keys = &escrow.Keys{Store: h.st}
	h.clk.Advance(-2 * time.Hour)
	p2, _, _ := enroll.Mint(ctx, h.st, store.ScopeAll(), cs[0].ID, enroll.MintOptions{Expires: time.Hour})
	r := h.do("POST", api.RouteEnroll, p2, enrollReq(nonce(8)))
	if r.code != 503 || r.problem(t).Code != api.CodeMasterKeyUnavailable {
		t.Fatalf("no master key: %d %s", r.code, r.body)
	}
	l, _ := h.st.ListDevices(ctx, store.ScopeAll(), store.ListDevicesOptions{Search: "TEST-PC-01"})
	if len(l.Items) != 0 {
		t.Fatal("an unwrapped key must never be stored")
	}
}

func TestEnrollRetriesAfterTheAdminPathFails(t *testing.T) {
	h := newHarness(t)
	h.be.Errors = map[string]error{adminpath.MethodInit: adminpath.ErrUnavailable}
	_, r := h.enroll(nonce(9))
	if r.code != 503 || r.problem(t).Code != api.CodeUnavailable || r.hdr.Get("Retry-After") == "" {
		t.Fatalf("init failure: %d %s", r.code, r.body)
	}
	l, _ := h.st.ListDevices(context.Background(), store.ScopeAll(), store.ListDevicesOptions{Search: "TEST-PC-01"})
	if len(l.Items) != 1 {
		t.Fatalf("the device must stay in place for the retry: %d", len(l.Items))
	}
	h.be.Errors = nil
	out, r := h.enroll(nonce(9))
	if r.code != 200 || out.DeviceID != l.Items[0].ID {
		t.Fatalf("retry with the same nonce: %d %s", r.code, r.body)
	}
	// A result-style failure (an existing repository, the wrong key) is treated like an error.
	h.be.InitResult = &adminpath.InitResult{ResultMeta: adminpath.ResultMeta{Outcome: restic.OutcomeFailed, ErrorClass: restic.ClassWrongPassword, ExitCode: 12}}
	if _, r := h.enroll(nonce(9)); r.code != 503 || r.hdr.Get("Retry-After") == "" {
		t.Fatalf("a failed Init result must be a 503: %d", r.code)
	}
	h.be.InitResult = nil
	h.be.Errors = map[string]error{adminpath.MethodCatConfig: adminpath.NewError(adminpath.CodeWrongPassword, restic.ClassWrongPassword)}
	if _, r := h.enroll(nonce(9)); r.code != 503 {
		t.Fatalf("a key that does not open the repository must not enroll: %d", r.code)
	}
}

func TestEnrollRateLimitPerIP(t *testing.T) {
	h := newHarness(t)
	var last resp
	for i := 0; i < 12; i++ {
		last = h.do("POST", api.RouteEnroll, strings.Repeat("B", 43), enrollReq(nonce(10)))
	}
	if last.code != 429 || last.hdr.Get("Retry-After") == "" || last.problem(t).Code != api.CodeRateLimited {
		t.Fatalf("expected 429 with Retry-After: %d %s", last.code, last.body)
	}
	evs, _ := h.st.ListAudit(context.Background(), store.ScopeAll(), store.AuditFilter{Action: "agent.enroll.denied"}, store.Page{Limit: 50})
	if len(evs.Items) == 0 {
		t.Fatal("agent.enroll.denied audit rows missing")
	}
}

// ---- policy ----

func (h *harness) enrolled() api.EnrollResponse {
	h.t.Helper()
	out, r := h.enroll(nonce(20))
	if r.code != 200 {
		h.t.Fatalf("enroll: %d %s", r.code, r.body)
	}
	return out
}

func TestPolicyETagAnd304(t *testing.T) {
	h := newHarness(t)
	e := h.enrolled()
	tok := string(e.DeviceToken)
	r := h.do("GET", api.RoutePolicy, tok, nil)
	if r.code != 200 {
		t.Fatalf("%d %s", r.code, r.body)
	}
	var p api.PolicyResponse
	r.json(t, &p)
	etag := r.hdr.Get("ETag")
	if etag == "" || etag != p.ETag || p.Version < 1 || p.Paused {
		t.Fatalf("etag %q body %q v%d", etag, p.ETag, p.Version)
	}
	if p.PendingGatewaySecret == nil || string(p.PendingGatewaySecret.Password) != string(e.Gateway.Password) {
		t.Fatal("until acknowledged the policy carries the pending gateway secret (DH13)")
	}
	// With a secret pending, If-None-Match does not produce a 304.
	h.clk.Advance(time.Minute)
	if r := h.do("GET", api.RoutePolicy, tok, nil, "If-None-Match", etag); r.code != 200 {
		t.Fatalf("pending secret must force 200: %d", r.code)
	}
	// Heartbeat with the acknowledgement erases it.
	hb := hbReq(h)
	hb.GatewaySecretAcked = true
	h.clk.Advance(time.Minute)
	if r := h.do("POST", api.RouteHeartbeat, tok, hb); r.code != 200 {
		t.Fatalf("heartbeat: %d %s", r.code, r.body)
	}
	h.clk.Advance(time.Minute)
	r = h.do("GET", api.RoutePolicy, tok, nil, "If-None-Match", etag)
	if r.code != 304 || len(r.body) != 0 || r.hdr.Get("ETag") != etag {
		t.Fatalf("expected 304: %d %q", r.code, r.body)
	}
	h.clk.Advance(time.Minute)
	if r := h.do("GET", api.RoutePolicy, tok, nil, "If-None-Match", `"stale"`); r.code != 200 {
		t.Fatalf("stale etag: %d", r.code)
	}
	// Pausing changes the ETag and shows in the body, so a 304 can never hide it.
	until := h.clk.Now().Add(4 * time.Hour)
	if _, err := h.st.UpdateDevice(context.Background(), store.ScopeAll(), e.DeviceID, store.DeviceUpdate{Pause: &store.PauseSpec{Reason: "test", Until: &until}}); err != nil {
		t.Fatal(err)
	}
	h.clk.Advance(time.Minute)
	r = h.do("GET", api.RoutePolicy, tok, nil, "If-None-Match", etag)
	var paused api.PolicyResponse
	r.json(t, &paused)
	if r.code != 200 || !paused.Paused || paused.PausedUntil == nil || paused.ETag == etag {
		t.Fatalf("paused policy: %d %+v", r.code, paused)
	}
	// Rate limit: 6 per minute per device.
	var last resp
	for i := 0; i < 8; i++ {
		last = h.do("GET", api.RoutePolicy, tok, nil)
	}
	if last.code != 429 {
		t.Fatalf("policy limiter: %d", last.code)
	}
}

func TestDeviceAuthRejectsBadAndForeignTokens(t *testing.T) {
	h := newHarness(t)
	e := h.enrolled()
	for name, tok := range map[string]string{"none": "", "garbage": "xyz", "unknown": strings.Repeat("C", 43), "enrollment token as device token": h.token} {
		r := h.do("GET", api.RoutePolicy, tok, nil)
		if r.code != 401 || r.problem(t).Code != api.CodeUnauthorized {
			t.Errorf("%s: %d %s", name, r.code, r.body)
		}
	}
	// Retired devices lose access at once.
	if _, err := h.st.UpdateDevice(context.Background(), store.ScopeAll(), e.DeviceID, store.DeviceUpdate{Retire: &store.RetireSpec{Reason: "r"}}); err != nil {
		t.Fatal(err)
	}
	if r := h.do("GET", api.RoutePolicy, string(e.DeviceToken), nil); r.code != 401 {
		t.Fatalf("retired: %d", r.code)
	}
}

func TestFailedAuthBlocksTheIPWithOneAuditRow(t *testing.T) {
	h := newHarness(t)
	var last resp
	for i := 0; i < 40; i++ {
		last = h.do("GET", api.RoutePolicy, strings.Repeat("D", 43), nil)
	}
	if last.code != 429 || last.hdr.Get("Retry-After") == "" {
		t.Fatalf("expected a block: %d", last.code)
	}
	evs, _ := h.st.ListAudit(context.Background(), store.ScopeAll(), store.AuditFilter{Action: "agent.auth.blocked"}, store.Page{Limit: 10})
	if len(evs.Items) != 1 {
		t.Fatalf("one audit row per block, got %d", len(evs.Items))
	}
	// A valid device is blocked too while the block lasts (it shares the source IP).
	h.clk.Advance(61 * time.Second)
	if r := h.do("GET", api.RoutePolicy, strings.Repeat("D", 43), nil); r.code != 401 {
		t.Fatalf("block must lapse: %d", r.code)
	}
}

// ---- heartbeat ----

func TestHeartbeatRecordsPresenceOffsetAndConfirms(t *testing.T) {
	h := newHarness(t)
	e := h.enrolled()
	tok := string(e.DeviceToken)
	ctx := context.Background()
	hb := hbReq(h) // device clock 3 minutes ahead
	on, bat := true, 61
	hb.OnAC, hb.BatteryPct = &on, &bat
	hb.Progress = &api.Progress{RunID: h.mustID(), Kind: restic.KindBackup, PercentDone: 0.5, BytesDone: 10, FilesDone: 5, TotalFiles: 10, UpdatedAt: h.clk.Now()}
	r := h.do("POST", api.RouteHeartbeat, tok, hb)
	if r.code != 200 {
		t.Fatalf("%d %s", r.code, r.body)
	}
	var out api.HeartbeatResponse
	r.json(t, &out)
	if out.ClockOffsetMS != 3*60*1000 || !out.ServerTime.Equal(h.clk.Now()) || out.PolicyETag == "" || out.CommandsPending {
		t.Fatalf("response: %+v", out)
	}
	p, err := h.st.GetPresence(ctx, store.ScopeAll(), e.DeviceID)
	if err != nil || p.ClockOffsetMS == nil || *p.ClockOffsetMS != 180000 || p.ServiceState != "idle" || p.BatteryPct == nil || *p.BatteryPct != 61 {
		t.Fatalf("presence: %+v %v", p, err)
	}
	if !strings.Contains(string(p.Progress), `"percent_done":0.5`) || strings.Contains(string(p.Progress), "updated_at") {
		t.Fatalf("progress document: %s", p.Progress)
	}
	dev, _ := h.st.GetDevice(ctx, store.ScopeAll(), e.DeviceID)
	if dev.EnrollConfirmedAt == nil || dev.AgentVersion != "1.4.2" {
		t.Fatalf("first heartbeat must confirm the enrollment: %+v", dev)
	}
	if h.pub.progress[e.DeviceID] == "" {
		t.Fatal("progress must be published")
	}
	// Faster than 20 s: 429 with Retry-After; the agent honours it.
	h.clk.Advance(5 * time.Second)
	r = h.do("POST", api.RouteHeartbeat, tok, hbReq(h))
	if r.code != 429 || r.hdr.Get("Retry-After") == "" || r.problem(t).Code != api.CodeRateLimited {
		t.Fatalf("limiter: %d %s", r.code, r.body)
	}
	h.clk.Advance(20 * time.Second)
	if r := h.do("POST", api.RouteHeartbeat, tok, hbReq(h)); r.code != 200 {
		t.Fatalf("after the interval: %d", r.code)
	}
	// Commands pending shows up in the response.
	if _, _, err := h.st.EnqueueCommand(ctx, store.ScopeAll(), e.DeviceID, store.CommandInput{Type: "backup_now", IdempotencyKey: "k1"}); err != nil {
		t.Fatal(err)
	}
	h.clk.Advance(21 * time.Second)
	r = h.do("POST", api.RouteHeartbeat, tok, hbReq(h))
	r.json(t, &out)
	if !out.CommandsPending {
		t.Fatalf("commands_pending: %+v", out)
	}
}

func TestHeartbeatValidationAndPathFree(t *testing.T) {
	h := newHarness(t)
	e := h.enrolled()
	tok := string(e.DeviceToken)
	bad := hbReq(h)
	bad.ServiceState = "weird"
	if r := h.do("POST", api.RouteHeartbeat, tok, bad); r.code != 400 {
		t.Fatalf("invalid enum: %d", r.code)
	}
	h.clk.Advance(30 * time.Second)
	leak := hbReq(h)
	leak.AgentVersion = `1.4.2 C:\Users\jdoe\agent.exe`
	if r := h.do("POST", api.RouteHeartbeat, tok, leak); r.code != 400 {
		t.Fatalf("path in heartbeat: %d", r.code)
	}
}

// ---- runs ----

func (h *harness) mustID() string {
	id, err := h.ids.New()
	if err != nil {
		h.t.Fatal(err)
	}
	return id
}

func (h *harness) run(outcome restic.Outcome, finishedAgo time.Duration) api.RunRecord {
	start := h.clk.Now().Add(-finishedAgo - 10*time.Minute)
	rec := api.RunRecord{ID: h.mustID(), Kind: restic.KindBackup, Trigger: api.TriggerSchedule, StartedAt: start, Attempt: 1, PolicyVersion: 1, AgentVersion: "1.4.2",
		Stats: api.RunStats{TotalFilesProcessed: 1000, FailedFiles: 0, DataAdded: 4096}}
	if outcome != "" {
		fin := h.clk.Now().Add(-finishedAgo)
		code := 0
		rec.FinishedAt, rec.Outcome, rec.ExitCode = &fin, outcome, &code
		rec.SnapshotID = strings.Repeat("ab", 32)
	}
	return rec
}

func TestRunsIngestStatusesAndStatsKeys(t *testing.T) {
	h := newHarness(t)
	e := h.enrolled()
	tok := string(e.DeviceToken)
	ctx := context.Background()
	good := h.run(restic.OutcomeSuccess, 5*time.Minute)
	good.Stats.VSSFallbackVolumes = []string{"D"}
	open := h.run("", 0)
	partial := h.run(restic.OutcomePartial, time.Hour)
	partial.Stats.FailedFiles = 7
	partial.ErrorCounts = []restic.ErrorProjection{{During: "scan", Class: restic.ClassPermission, Ext: "docx", Count: 4}, {During: "read", Class: restic.ClassPermission, Count: 3}}
	partial.ErrorClass = restic.ClassPermission
	invalid := h.run(restic.OutcomeSuccess, time.Hour)
	invalid.Kind = "nonsense"

	r := h.do("POST", api.RouteRuns, tok, api.RunsRequest{Runs: []api.RunRecord{good, open, partial, invalid}})
	if r.code != 200 {
		t.Fatalf("%d %s", r.code, r.body)
	}
	var out api.RunsResponse
	r.json(t, &out)
	want := []api.RunStatus{api.RunCreated, api.RunCreated, api.RunCreated, api.RunRejected}
	for i, res := range out.Results {
		if res.ID != []string{good.ID, open.ID, partial.ID, invalid.ID}[i] || res.Status != want[i] {
			t.Errorf("result %d: %+v", i, res)
		}
	}
	if out.Results[3].Reason != api.ReasonInvalid {
		t.Errorf("invalid reason: %+v", out.Results[3])
	}
	// Resending the batch is idempotent: duplicates.
	h.clk.Advance(3 * time.Second)
	r = h.do("POST", api.RouteRuns, tok, api.RunsRequest{Runs: []api.RunRecord{good, open}})
	r.json(t, &out)
	if out.Results[0].Status != api.RunDuplicate || out.Results[1].Status != api.RunDuplicate {
		t.Fatalf("resend: %+v", out.Results)
	}
	// Closing the open run is an update.
	closed := open
	fin := h.clk.Now().Add(-time.Minute)
	code := 0
	closed.FinishedAt, closed.Outcome, closed.ExitCode = &fin, restic.OutcomeSuccess, &code
	closed.SnapshotID = strings.Repeat("cd", 32)
	r = h.do("POST", api.RouteRuns, tok, api.RunsRequest{Runs: []api.RunRecord{closed}})
	r.json(t, &out)
	if out.Results[0].Status != api.RunUpdated {
		t.Fatalf("close: %+v", out.Results)
	}
	// The stored stats carry exactly the keys the facts loader reads.
	run, err := h.st.GetRun(ctx, store.ScopeAll(), good.ID)
	if err != nil {
		t.Fatal(err)
	}
	var stats map[string]any
	_ = json.Unmarshal(run.Stats, &stats)
	if stats["total_files_processed"] != float64(1000) || stats["failed_files"] != float64(0) {
		t.Fatalf("stats keys: %s", run.Stats)
	}
	if v, _ := stats["vss_fallback_volumes"].([]any); len(v) != 1 || v[0] != "D" {
		t.Fatalf("vss_fallback_volumes: %s", run.Stats)
	}
	pr, _ := h.st.GetRun(ctx, store.ScopeAll(), partial.ID)
	if pr.ErrorCounts["permission"] != 7 || pr.Outcome != "partial" || pr.ErrorClass != "permission" {
		t.Fatalf("error counts must be summed by class: %+v", pr.ErrorCounts)
	}
	// And the fleet view agrees: the device has a success now.
	f, err := h.st.LoadDeviceFacts(ctx, store.ScopeAll(), e.DeviceID)
	if err != nil || f.LastSuccess == 0 {
		t.Fatalf("facts: %+v %v", f.LastSuccess, err)
	}
}

func TestRunsWithAnotherDevicesIDAreRejected(t *testing.T) {
	h := newHarness(t)
	a := h.enrolled()
	b, r := h.enroll(nonce(21))
	if r.code != 200 {
		t.Fatal(r.code)
	}
	rec := h.run(restic.OutcomeSuccess, time.Minute)
	if r := h.do("POST", api.RouteRuns, string(a.DeviceToken), api.RunsRequest{Runs: []api.RunRecord{rec}}); r.code != 200 {
		t.Fatal(r.code)
	}
	r = h.do("POST", api.RouteRuns, string(b.DeviceToken), api.RunsRequest{Runs: []api.RunRecord{rec}})
	var out api.RunsResponse
	r.json(t, &out)
	if out.Results[0].Status != api.RunRejected || out.Results[0].Reason != api.ReasonConflict {
		t.Fatalf("another device's run id: %+v", out.Results)
	}
}

func TestRunsLeakingAPathAreRejectedWith400(t *testing.T) {
	h := newHarness(t)
	e := h.enrolled()
	rec := h.run(restic.OutcomeFailed, time.Minute)
	rec.ErrorCounts = []restic.ErrorProjection{{During: "scan", Class: restic.ClassPermission, Ext: "docx", Count: 1}}
	// ErrorProjection has no free-text field; the leak is in a field that does accept text.
	rec.AgentVersion = `1.4.2 \\fileserver\share\patients`
	r := h.do("POST", api.RouteRuns, string(e.DeviceToken), api.RunsRequest{Runs: []api.RunRecord{rec}})
	if r.code != 400 || r.problem(t).Code != api.CodeInvalidRequest {
		t.Fatalf("%d %s", r.code, r.body)
	}
	if _, err := h.st.GetRun(context.Background(), store.ScopeAll(), rec.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("a leaking batch must not reach the store")
	}
	// Empty and oversized batches.
	h.clk.Advance(time.Minute)
	if r := h.do("POST", api.RouteRuns, string(e.DeviceToken), `{"runs":[]}`); r.code != 400 {
		t.Fatalf("empty batch: %d", r.code)
	}
}

func TestRunsLimiterAndBodyCap(t *testing.T) {
	h := newHarness(t)
	e := h.enrolled()
	tok := string(e.DeviceToken)
	var last resp
	for i := 0; i < 32; i++ {
		last = h.do("POST", api.RouteRuns, tok, api.RunsRequest{Runs: []api.RunRecord{h.run(restic.OutcomeSuccess, time.Minute)}})
	}
	if last.code != 429 {
		t.Fatalf("30/min per device: %d", last.code)
	}
	h.clk.Advance(2 * time.Minute)
	big := padded(api.MaxRunsBody + 1)
	if r := h.do("POST", api.RouteRuns, tok, big); r.code != 413 {
		t.Fatalf("body cap: %d", r.code)
	}
}

// ---- snapshots ----

func TestSnapshotsReplaceTheCache(t *testing.T) {
	h := newHarness(t)
	e := h.enrolled()
	tok := string(e.DeviceToken)
	snap := func(seed string) restic.Snapshot {
		return restic.Snapshot{ID: strings.Repeat(seed, 32), ShortID: strings.Repeat(seed, 4), Time: h.clk.Now().Add(-time.Hour), Paths: []string{`C:\Users`},
			Hostname: "TEST-PC-01", Tags: []string{"scheduled"}, ProgramVersion: "restic 0.19.1"}
	}
	r := h.do("POST", api.RouteSnapshots, tok, api.SnapshotsRequest{Snapshots: []restic.Snapshot{snap("ab"), snap("cd")}})
	var out api.SnapshotsResponse
	r.json(t, &out)
	if r.code != 200 || out.Upserted != 2 {
		t.Fatalf("%d %+v", r.code, out)
	}
	list, _ := h.st.ListSnapshots(context.Background(), store.ScopeAll(), e.DeviceID)
	if len(list) != 2 || list[0].Paths[0] != `C:\Users` {
		t.Fatalf("cache: %+v", list)
	}
	r = h.do("POST", api.RouteSnapshots, tok, api.SnapshotsRequest{Snapshots: []restic.Snapshot{snap("cd")}})
	r.json(t, &out)
	if out.Removed != 1 {
		t.Fatalf("forgotten snapshots leave the cache: %+v", out)
	}
	// A username on the wire is rejected; configured roots are the allowed path-bearing field.
	bad := snap("ef")
	bad.Username = "jdoe"
	if r := h.do("POST", api.RouteSnapshots, tok, api.SnapshotsRequest{Snapshots: []restic.Snapshot{bad}}); r.code != 400 {
		t.Fatalf("username: %d", r.code)
	}
	bad = snap("ef")
	bad.Tags = []string{`C:\Users\jdoe`}
	h.clk.Advance(time.Minute)
	if r := h.do("POST", api.RouteSnapshots, tok, api.SnapshotsRequest{Snapshots: []restic.Snapshot{bad}}); r.code != 400 {
		t.Fatalf("path in a tag: %d", r.code)
	}
}

// ---- commands ----

func TestCommandsLongPollWakeSingleFlightAndResult(t *testing.T) {
	h := newHarness(t)
	e := h.enrolled()
	tok := string(e.DeviceToken)
	ctx := context.Background()

	// Nothing queued, wait=0: an immediate empty answer.
	var out api.CommandsResponse
	r := h.do("GET", api.RouteCommands, tok, nil)
	r.json(t, &out)
	if r.code != 200 || len(out.Commands) != 0 {
		t.Fatalf("%d %s", r.code, r.body)
	}

	// A held poll is woken by a notification, and a second poller gets 409.
	type result struct {
		r   resp
		dur time.Duration
	}
	first := make(chan result, 1)
	start := time.Now()
	go func() {
		first <- result{h.do("GET", api.RouteCommands+"?wait=20", tok, nil), 0}
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if rel, ok := h.svc.pollFlight.TryAcquire(e.DeviceID); ok {
			rel() // not in flight yet
		} else {
			break // the first poll holds the flight
		}
		if time.Now().After(deadline) {
			t.Fatal("the long poll never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	second := h.do("GET", api.RouteCommands+"?wait=1", tok, nil)
	if second.code != 409 || second.problem(t).Code != api.CodeConflict {
		t.Fatalf("second poller: %d %s", second.code, second.body)
	}
	cmd, created, err := h.st.EnqueueCommand(ctx, store.ScopeAll(), e.DeviceID, store.CommandInput{Type: "backup_now", IdempotencyKey: "bn-1"})
	if err != nil || !created {
		t.Fatal(err)
	}
	h.svc.Notifier.Notify(e.DeviceID)
	select {
	case res := <-first:
		took := time.Since(start)
		res.r.json(t, &out)
		if res.r.code != 200 || len(out.Commands) != 1 || out.Commands[0].ID != cmd.ID || out.Commands[0].Type != api.CommandBackupNow {
			t.Fatalf("woken poll: %d %s", res.r.code, res.r.body)
		}
		if took > 10*time.Second {
			t.Fatalf("the poll was not woken: %v", took)
		}
		if res.r.hdr.Get(api.HeaderServerTime) == "" {
			t.Fatal("server time on the long-poll answer")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("long poll did not return after the notify")
	}

	// Result: acked, then succeeded; repeating is idempotent; an unknown id is 404.
	post := func(id string, status api.CommandStatus) resp {
		return h.do("POST", api.RouteCommandResult(id), tok, api.CommandResultRequest{Status: status})
	}
	if r := post(cmd.ID, api.CommandAcked); r.code != 204 {
		t.Fatalf("ack: %d %s", r.code, r.body)
	}
	if r := post(cmd.ID, api.CommandSucceeded); r.code != 204 {
		t.Fatalf("succeeded: %d", r.code)
	}
	if r := post(cmd.ID, api.CommandSucceeded); r.code != 204 {
		t.Fatalf("repeat: %d", r.code)
	}
	if r := post(cmd.ID, api.CommandFailed); r.code != 204 { // ignored, not refused: a deployed agent retries any error forever
		t.Fatalf("changing a final status: %d, want 204 (ignored)", r.code)
	}
	if r := post(h.mustID(), api.CommandSucceeded); r.code != 204 { // unknown id: ignored with a log line
		t.Fatalf("unknown command: %d", r.code)
	}
	if r := h.do("POST", api.RouteCommandResult(cmd.ID), tok, `{"status":"bogus"}`); r.code != 400 {
		t.Fatalf("bad status: %d", r.code)
	}
	// Another device cannot report on this command.
	other, _ := h.enroll(nonce(22))
	if r := h.do("POST", api.RouteCommandResult(cmd.ID), string(other.DeviceToken), api.CommandResultRequest{Status: api.CommandSucceeded}); r.code != 204 {
		t.Fatalf("foreign command: %d, want 204 (ignored)", r.code)
	}
	if got := h.command(e.DeviceID, cmd.ID); got.Status != "succeeded" {
		t.Fatalf("an ignored result changed the command to %s", got.Status)
	}
	// An existing command is delivered at once even with a wait.
	_, _, _ = h.st.EnqueueCommand(ctx, store.ScopeAll(), e.DeviceID, store.CommandInput{Type: "check_in", IdempotencyKey: "ci-1"})
	t0 := time.Now()
	r = h.do("GET", api.RouteCommands+"?wait=20", tok, nil)
	r.json(t, &out)
	if len(out.Commands) != 1 || time.Since(t0) > 5*time.Second {
		t.Fatalf("queued command must be returned immediately: %+v", out)
	}
	// wait is clamped and validated.
	if r := h.do("GET", api.RouteCommands+"?wait=abc", tok, nil); r.code != 400 {
		t.Fatalf("bad wait: %d", r.code)
	}
}

func TestCommandsWaitTimesOutEmpty(t *testing.T) {
	h := newHarness(t)
	e := h.enrolled()
	t0 := time.Now()
	r := h.do("GET", api.RouteCommands+"?wait=1", string(e.DeviceToken), nil)
	var out api.CommandsResponse
	r.json(t, &out)
	if r.code != 200 || len(out.Commands) != 0 || time.Since(t0) < 900*time.Millisecond {
		t.Fatalf("%d after %v", r.code, time.Since(t0))
	}
}

func TestUnmatchedAgentRoutesAreNotServedHere(t *testing.T) {
	h := newHarness(t)
	e := h.enrolled()
	// Only registered patterns are handled; the milestone routes are not (the server adds the problem+json catch-all).
	if r := h.do("POST", api.RouteTokenRotate, string(e.DeviceToken), nil); r.code != 404 {
		t.Fatalf("unregistered route: %d", r.code)
	}
	if r := h.do("DELETE", api.RoutePolicy, string(e.DeviceToken), nil); r.code != 405 {
		t.Fatalf("wrong method: %d", r.code)
	}
}
