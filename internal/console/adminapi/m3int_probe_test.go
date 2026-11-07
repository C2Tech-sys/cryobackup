package adminapi

import (
	"bytes"
	"context"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/escrow"
	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
	"github.com/c2tech-sys/cryobackup/internal/console/jobs"
	"github.com/c2tech-sys/cryobackup/internal/console/probe"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// M3-INT item A: POST /api/v1/storage-targets/{id}/probe runs the real
// health-prober's probes now and answers only from the facts they produce.

// proberHook is the Service's Prober in tests: set once before the server
// serves, and swapped atomically by tests, so no test writes a Service field
// while a handler may read it.
type proberHook struct {
	p atomic.Pointer[HealthProbe]
}

func (h *proberHook) set(p HealthProbe) { h.p.Store(&p) }

func (h *proberHook) ProbeNow(ctx context.Context) (bool, error) {
	return (*h.p.Load()).ProbeNow(ctx)
}

// failingProber is a prober that cannot run (its store failed).
type failingProber struct{}

func (failingProber) ProbeNow(context.Context) (bool, error) { return false, store.ErrBusy }

// probeBackend is the fixtures admin path whose probe-repository cat_config
// (the storage probe, ADR 0017) can be held until release is closed, and
// answers busy while busy is set. It counts the storage probes it served.
type probeBackend struct {
	*fixtures.Backend
	hold    atomic.Bool
	busy    atomic.Bool
	release chan struct{}
	entered chan struct{}
	calls   atomic.Int64
}

func newProbeBackend() *probeBackend {
	return &probeBackend{Backend: &fixtures.Backend{}, release: make(chan struct{}), entered: make(chan struct{}, 16)}
}

func (b *probeBackend) CatConfig(ctx context.Context, req adminpath.DeviceRequest) (restic.Config, error) {
	if req.DeviceID != adminpath.ProbeRepoID {
		return b.Backend.CatConfig(ctx, req)
	}
	b.calls.Add(1)
	if b.busy.Load() {
		return restic.Config{}, adminpath.NewError(adminpath.CodeBusy, "")
	}
	if b.hold.Load() {
		b.entered <- struct{}{}
		select {
		case <-b.release:
		case <-ctx.Done():
			return restic.Config{}, ctx.Err()
		}
	}
	return b.Backend.CatConfig(ctx, req)
}

// realProber wires a real jobs.HealthProber over the env's store and clock, be
// as its admin path and a TLS gateway that answers the probe login, into the
// Service (through a proberHook) and the Evaluator's probe source. It must be
// called before the first request of the test. Stop runs at cleanup, before
// the store closes.
func (e *env) realProber(be adminpath.Backend, holder string) (*jobs.HealthProber, *proberHook) {
	e.t.Helper()
	gw := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, _, ok := r.BasicAuth(); !ok || u != "probe" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusNotFound) // no probe repository yet: healthy for C01 (ADR 0017)
	}))
	e.t.Cleanup(gw.Close)
	pool := x509.NewCertPool()
	pool.AddCert(gw.Certificate())
	mk, _ := escrow.NewMasterKey(bytes.Repeat([]byte{7}, 32))
	hp := &jobs.HealthProber{Store: e.st, Backend: be, Master: mk, Clock: e.clk.Now, Started: e.clk.Now().Add(-time.Hour), Holder: holder}
	hp.Gateway.URL, hp.Gateway.Roots = gw.URL, pool
	logins := &jobs.HtpasswdSync{Store: e.st, Master: mk, Dir: e.t.TempDir()} // the fixture fleet has a retired device: C17 needs a file to read
	if err := logins.Run(context.Background()); err != nil {
		e.t.Fatal(err)
	}
	hp.Logins = logins
	e.t.Cleanup(hp.Stop)
	hook := &proberHook{}
	hook.set(hp)
	e.svc.Prober = hook
	e.svc.Fleet.Probes = hp.Facts
	if e.svc.Fleet.Endpoint == nil { // the certificates check reads both listeners, like the row
		e.svc.Fleet.Endpoint = func() probe.Status {
			now := e.clk.Now()
			return probe.Status{Runs: true, Known: true, Interval: time.Minute, Started: now, LastOK: now, Last: probe.Result{At: now, OK: true, NotAfter: now.Add(365 * 24 * time.Hour)}}
		}
	}
	return hp, hook
}

// postProbe sends the probe request in the background and returns the channel
// its answer arrives on.
func (e *env) postProbe(s *session, target string) <-chan probeAnswer {
	ch := make(chan probeAnswer, 1)
	go func() {
		req, _ := http.NewRequest("POST", e.srv.URL+"/api/v1/storage-targets/"+target+"/probe", nil)
		for _, c := range s.cookies {
			req.AddCookie(c)
		}
		req.Header.Set("Origin", e.srv.URL)
		req.Header.Set(auth.CSRFHeader, s.csrf)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			ch <- probeAnswer{err: err}
			return
		}
		defer res.Body.Close()
		var b bytes.Buffer
		_, _ = b.ReadFrom(res.Body)
		ch <- probeAnswer{status: res.StatusCode, body: b.Bytes()}
	}()
	return ch
}

type probeAnswer struct {
	status int
	body   []byte
	err    error
}

func checkOf(t *testing.T, pr ProbeResponse, id string) ProbeCheckJSON {
	t.Helper()
	for _, c := range pr.Checks {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("no %s check in %+v", id, pr.Checks)
	return ProbeCheckJSON{}
}

// The handler waits for the probe it asked for: while the storage probe is
// held, no answer arrives; once it ends, the answer carries its result. Red on
// the RunOnce path, which answered at once from the facts before the probe.
func TestM3INTStorageProbeWaitsForTheProbeItRan(t *testing.T) { // F19, M3-INT A
	e := newEnv(t)
	owner := e.login("admin", pw)
	target := e.firstStorageTarget()
	be := newProbeBackend()
	be.hold.Store(true)
	e.realProber(be, "console-a")

	answer := e.postProbe(owner, target)
	select {
	case <-be.entered:
	case a := <-answer:
		t.Fatalf("answered (%d %s) before the storage probe even started", a.status, a.body)
	case <-time.After(10 * time.Second):
		t.Fatal("the storage probe never started")
	}
	select {
	case a := <-answer:
		t.Fatalf("answered (%d %s) while the storage probe it asked for was still running", a.status, a.body)
	case <-time.After(300 * time.Millisecond):
	}
	close(be.release)
	var a probeAnswer
	select {
	case a = <-answer:
	case <-time.After(10 * time.Second):
		t.Fatal("no answer after the storage probe ended")
	}
	if a.err != nil || a.status != 200 {
		t.Fatalf("probe = %d %s (%v)", a.status, a.body, a.err)
	}
	pr := decode[ProbeResponse](t, a.body)
	if !pr.Ran || pr.Job == nil || pr.Job.Status != "ok" {
		t.Fatalf("probe that ran: %+v", pr)
	}
	for _, id := range []string{"gateway", "storage", "certificates"} {
		if c := checkOf(t, pr, id); c.Status != statusOK {
			t.Errorf("%s = %+v after a passing probe, want ok", id, c)
		}
	}
	if n := be.calls.Load(); n != 1 {
		t.Errorf("%d storage probes for one request, want 1", n)
	}
	if n := e.auditCount("storage_target.probe", target); n != 1 {
		t.Errorf("%d audit rows, want 1", n)
	}
}

// A storage probe already in flight from a tick is waited for, not doubled.
func TestM3INTStorageProbeWaitsForTheProbeInFlight(t *testing.T) { // F19, M3-INT A
	e := newEnv(t)
	owner := e.login("admin", pw)
	target := e.firstStorageTarget()
	be := newProbeBackend()
	be.hold.Store(true)
	hp, _ := e.realProber(be, "console-a")
	if err := hp.Run(context.Background()); err != nil { // a tick: its storage probe is now held in flight
		t.Fatal(err)
	}
	select {
	case <-be.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the tick's storage probe never started")
	}
	e.clk.Advance(time.Second) // the request starts after the tick's probe did
	answer := e.postProbe(owner, target)
	select {
	case a := <-answer:
		t.Fatalf("answered (%d %s) while the storage probe in flight had not ended", a.status, a.body)
	case <-time.After(300 * time.Millisecond):
	}
	close(be.release)
	a := <-answer
	if a.status != 200 {
		t.Fatalf("probe = %d %s", a.status, a.body)
	}
	pr := decode[ProbeResponse](t, a.body)
	if c := checkOf(t, pr, "storage"); c.Status != statusOK {
		t.Errorf("storage = %+v once the probe in flight passed, want ok", c)
	}
	if n := be.calls.Load(); n != 1 {
		t.Errorf("%d storage probes: the one in flight was doubled", n)
	}
}

// Another console holds the probe lease: nothing is probed here, the answer is
// 409 and the one audit row says conflict. Red on the RunOnce path, which took
// the runner's free lease, probed nothing and answered ran=true.
func TestM3INTStorageProbeLeaseHeldElsewhereIs409(t *testing.T) { // F19, M3-INT A
	e := newEnv(t)
	owner := e.login("admin", pw)
	target := e.firstStorageTarget()
	be := newProbeBackend()
	e.realProber(be, "console-a")
	if ok, err := e.st.AcquireJobLease(context.Background(), jobs.ProbeLease, "console-b", time.Hour); err != nil || !ok {
		t.Fatalf("console-b takes the probe lease: %v %v", ok, err)
	}
	res, b := e.req("POST", "/api/v1/storage-targets/"+target+"/probe", owner, nil)
	if res.StatusCode != 409 {
		t.Fatalf("probe while another console holds the probe lease = %d %s, want 409", res.StatusCode, b)
	}
	if strings.Contains(string(b), `"ran":true`) {
		t.Errorf("the answer says ran=true: %s", b)
	}
	if n := be.calls.Load(); n != 0 {
		t.Errorf("%d storage probes ran without the lease", n)
	}
	l, err := e.st.ListAudit(context.Background(), store.ScopeAll(), store.AuditFilter{Action: "storage_target.probe", ObjectID: target}, store.Page{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Items) != 1 || l.Items[0].Result != "error" || !strings.Contains(string(l.Items[0].Details), `"conflict"`) {
		t.Fatalf("audit rows %+v, want exactly one error/conflict", l.Items)
	}
}

// Only facts produced at or after the request started count: a storage result
// from before it (the worker is busy now, so the probe records nothing) is
// not_checked even though it passed; the gateway, probed now, is ok.
func TestM3INTStorageProbeReadsOnlyFactsFromThisRequest(t *testing.T) { // F19, M3-INT A
	e := newEnv(t)
	owner := e.login("admin", pw)
	target := e.firstStorageTarget()
	be := newProbeBackend()
	hp, _ := e.realProber(be, "console-a")
	if err := hp.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	hp.Wait()
	if f := hp.Facts(); f.Storage.LastOKAt == 0 || f.Gateway.LastOKAt == 0 {
		t.Fatalf("the earlier tick did not pass: storage %+v gateway %+v", f.Storage, f.Gateway)
	}
	e.clk.Advance(time.Minute)
	be.busy.Store(true)
	pr := decode[ProbeResponse](t, e.mustReq("POST", "/api/v1/storage-targets/"+target+"/probe", owner, nil, 200))
	if !pr.Ran {
		t.Fatalf("probe: %+v", pr)
	}
	if c := checkOf(t, pr, "storage"); c.Status != statusNotChecked || c.Detail == "" {
		t.Errorf("storage = %+v from a result older than the request, want not_checked with a reason", c)
	}
	if c := checkOf(t, pr, "gateway"); c.Status != statusOK {
		t.Errorf("gateway = %+v, probed during the request, want ok", c)
	}
	// A storage probe that fails now is a warning at once, as on the health page, before its streak raises C02.
	be.busy.Store(false)
	be.Errors = map[string]error{adminpath.MethodCatConfig: adminpath.NewError(adminpath.CodeStorageUnreachable, "")}
	e.clk.Advance(time.Minute)
	pr = decode[ProbeResponse](t, e.mustReq("POST", "/api/v1/storage-targets/"+target+"/probe", owner, nil, 200))
	if c := checkOf(t, pr, "storage"); c.Status != statusWarn {
		t.Errorf("storage = %+v after a failed probe just now, want warn", c)
	}
}
