package cli

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminapi"
	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/config"
	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
)

// M3-T4 fix round 5, on a real App with real probes and real TLS listeners. The clock is the
// test's, so the 5-minute grace of C04 and the prober's weekly integrity check can be passed.

type r5Options struct {
	gatewayNotAfter time.Time
	gatewayStatus   int // default 404: the probe login's repository does not exist yet, which is healthy
	agentNotAfter   time.Time
	agentOtherCA    bool
}

type r5Rig struct {
	t     *testing.T
	app   *App
	clock *atomic.Pointer[time.Time]
	base  string
	token string
}

// newR5Rig builds the App. The public agent address is a real TLS listener that answers 502 (a
// proxy in front of a stopped agent API): the certificate is read, the responder stage fails.
func newR5Rig(t *testing.T, o r5Options) *r5Rig {
	t.Helper()
	var clock atomic.Pointer[time.Time]
	start := time.Now().UTC()
	clock.Store(&start)
	if o.gatewayNotAfter.IsZero() {
		o.gatewayNotAfter = start.Add(365 * 24 * time.Hour)
	}
	if o.agentNotAfter.IsZero() {
		o.agentNotAfter = start.Add(365 * 24 * time.Hour)
	}
	if o.gatewayStatus == 0 {
		o.gatewayStatus = http.StatusNotFound
	}
	dir := t.TempDir()
	if code, out, errb := run(t, "", initArgs(dir, "--master-key-copy-confirmed", "--timezone", "America/Chicago")...); code != 0 {
		t.Fatalf("init: %d\n%s\n%s", code, out, errb)
	}
	cfg, err := config.Read(filepath.Join(dir, "config.json"), func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	listener := func(name string, notAfter time.Time, status int) (url, caPath string) {
		cp, kp := r4Cert(t, dir, name, start.Add(-48*time.Hour), notAfter)
		pair, err := tls.LoadX509KeyPair(cp, kp)
		if err != nil {
			t.Fatal(err)
		}
		s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }))
		s.TLS = &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}
		s.StartTLS()
		t.Cleanup(s.Close)
		return s.URL, cp
	}
	gwURL, gwCA := listener("gateway", o.gatewayNotAfter, o.gatewayStatus)
	agentURL, agentCA := listener("agent", o.agentNotAfter, http.StatusBadGateway)
	if o.agentOtherCA {
		agentCA, _ = r4Cert(t, dir, "other", start.Add(-time.Hour), start.Add(365*24*time.Hour))
	}
	own, ownKey := r4Cert(t, dir, "own", start.Add(-time.Hour), start.Add(365*24*time.Hour))
	smtpHost, smtpPort := r3SMTP(t)
	cfg.AgentListen, cfg.AdminListen = "127.0.0.1:0", "127.0.0.1:0"
	cfg.TLS = config.TLS{Cert: own, Key: ownKey}
	cfg.PublicURL, cfg.AgentProbe.CAFile = agentURL, agentCA
	cfg.GatewayURL, cfg.HealthProber.GatewayCAFile = gwURL, gwCA
	cfg.SMTP = config.SMTP{Host: smtpHost, Port: smtpPort, Security: "plain", From: "cryo@example.invalid"}
	cfg.AdminpathSocket = filepath.Join(dir, "a.sock")
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	app, err := Build(context.Background(), cfg, BuildOptions{Backend: &fixtures.Backend{}, Log: logx.New(&logs, logx.LevelDebug), Clock: func() time.Time { return *clock.Load() }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	t.Cleanup(app.Prober.Stop)
	srv := httptest.NewServer(app.Server.AdminRouter)
	t.Cleanup(srv.Close)
	// A token that outlives the weeks the clock will be moved by: r3Token's lasts 7 days.
	admin, err := app.Store.GetAdminByUsername(context.Background(), "owner")
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := app.Auth.CreateAPIToken(context.Background(), &auth.Principal{Admin: admin}, "r5", auth.ScopeFull, 90, "api", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	return &r5Rig{t: t, app: app, clock: &clock, base: srv.URL, token: token}
}

// tick moves the clock to d after the start and runs one real round of every probe.
func (r *r5Rig) tick(start time.Time, d time.Duration) adminapi.SystemHealthResponse {
	r.t.Helper()
	at := start.Add(d)
	r.clock.Store(&at)
	ctx := context.Background()
	if err := r.app.Prober.Run(ctx); err != nil {
		r.t.Fatal(err)
	}
	r.app.Prober.WaitAll()
	r.app.Endpoint.Once(ctx)
	return r3Health(r.t, r.base, r.token)
}

// post is POST /api/v1/storage-targets/{id}/probe on the real App.
func (r *r5Rig) post() adminapi.ProbeResponse {
	r.t.Helper()
	ts, err := r.app.Store.ListStorageTargets(context.Background())
	if err != nil || len(ts) == 0 {
		r.t.Fatalf("storage targets: %v", err)
	}
	req, _ := http.NewRequest("POST", r.base+"/api/v1/storage-targets/"+ts[0].ID+"/probe", nil)
	req.Header.Set("Authorization", "Bearer "+r.token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer res.Body.Close()
	var pr adminapi.ProbeResponse
	if err := json.NewDecoder(res.Body).Decode(&pr); err != nil {
		r.t.Fatal(err)
	}
	return pr
}

func r5Check(pr adminapi.ProbeResponse, id string) adminapi.ProbeCheckJSON {
	for _, c := range pr.Checks {
		if c.ID == id {
			return c
		}
	}
	return adminapi.ProbeCheckJSON{}
}

// F1: a certificate 3 days from expiry, behind an outage that outlasts the 5-minute grace. C04 returns
// before C05 for the agent listener, so the row has to judge the days left itself.
func TestR5AgentCertificateNearExpiryOutlastsTheGrace(t *testing.T) { // r5 F1, C05
	start := time.Now().UTC()
	r := newR5Rig(t, r5Options{agentNotAfter: start.Add(3*24*time.Hour + time.Hour)})
	h := r.tick(start, 0)
	if c := r4Row(h, "certificates"); c.Status != "failed" {
		t.Errorf("first failure: certificates = %s %v %q", c.Status, c.Banners, c.Detail)
	}
	h = r.tick(start, 6*time.Minute)
	if c := r4Row(h, "agent_api"); c.Status != "failed" || len(c.Banners) == 0 || c.Banners[0] != "C04" {
		t.Fatalf("the control is past the grace, under C04: agent_api = %+v", c)
	}
	c := r4Row(h, "certificates")
	if c.Status != "failed" || !strings.Contains(c.Detail, "agent listener") || !strings.Contains(c.Detail, "expires in 3 days") {
		t.Errorf("past the grace: certificates = %s %v %q, want failed saying 3 days", c.Status, c.Banners, c.Detail)
	}
}

// F1: the gateway half agrees, on a gateway that answers 502 past C01's streak.
func TestR5GatewayCertificateNearExpiryOutlastsTheGrace(t *testing.T) { // r5 F1, C05
	start := time.Now().UTC()
	r := newR5Rig(t, r5Options{gatewayNotAfter: start.Add(3*24*time.Hour + time.Hour), gatewayStatus: http.StatusBadGateway})
	var h adminapi.SystemHealthResponse
	for _, d := range []time.Duration{0, time.Minute, 2 * time.Minute, 3 * time.Minute, 4 * time.Minute, 6 * time.Minute} {
		h = r.tick(start, d)
		c := r4Row(h, "certificates")
		if c.Status != "failed" {
			t.Errorf("%v in: certificates = %s %v %q, want failed", d, c.Status, c.Banners, c.Detail)
		}
	}
	if c := r4Row(h, "gateway"); len(c.Banners) == 0 || c.Banners[0] != "C01" {
		t.Fatalf("the control is under C01: gateway = %+v", c)
	}
	if c := r4Row(h, "certificates"); !strings.Contains(c.Detail, "gateway") || !strings.Contains(c.Detail, "expires in 3 days") {
		t.Errorf("past C01: certificates detail = %q", c.Detail)
	}
}

// F2: a real HealthProber whose +7-day integrity check errors. The prober keeps the old good
// result, and only its job_state row says the attempt failed. That must reach the row.
func TestR5ADatabaseIntegrityAttemptThatErrorsReachesTheRow(t *testing.T) { // r5 F2, C11
	start := time.Now().UTC()
	r := newR5Rig(t, r5Options{})
	r.app.Prober.IntegrityCheck = func(context.Context) (string, error) { return "ok", nil }
	h := r.tick(start, 0)
	if c := r4Row(h, "database"); c.Status != "ok" {
		t.Fatalf("control: %+v", c)
	}
	r.app.Prober.IntegrityCheck = func(context.Context) (string, error) { return "", context.DeadlineExceeded }
	h = r.tick(start, 7*24*time.Hour+time.Minute)
	c := r4Row(h, "database")
	if c.Status != "warn" || !strings.Contains(c.Detail, "integrity") || !strings.Contains(c.Detail, "timeout") {
		t.Errorf("the integrity attempt errored: database = %s %v %q, want warn saying so", c.Status, c.Banners, c.Detail)
	}
	if h.Overall == "ok" {
		t.Errorf("overall = ok with the newest integrity attempt failed")
	}
	// A check that really reports damage is failed, and a later good one clears it.
	r.app.Prober.IntegrityCheck = func(context.Context) (string, error) { return "*** page 7 is corrupt", nil }
	h = r.tick(start, 8*24*time.Hour+2*time.Minute)
	if c := r4Row(h, "database"); c.Status != "failed" {
		t.Errorf("a damaged database: %s %v %q", c.Status, c.Banners, c.Detail)
	}
	r.app.Prober.IntegrityCheck = func(context.Context) (string, error) { return "ok", nil }
	h = r.tick(start, 9*24*time.Hour+3*time.Minute)
	if c := r4Row(h, "database"); c.Status != "ok" {
		t.Errorf("after a good check: %s %v %q", c.Status, c.Banners, c.Detail)
	}
}

// F3: POST says what S36 says, for every check, on a refused agent certificate the gateway run knows nothing about.
func TestR5PostProbeAndS36CannotDisagree(t *testing.T) { // r5 F3
	start := time.Now().UTC()
	r := newR5Rig(t, r5Options{agentOtherCA: true})
	r.tick(start, 0)
	pr := r.post()
	h := r3Health(t, r.base, r.token)
	if c := r5Check(pr, "certificates"); c.Status != "failed" || !strings.Contains(c.Detail, "agent listener") {
		t.Errorf("POST certificates = %+v, want failed: S36 says %+v", c, r4Row(h, "certificates"))
	}
	for _, id := range []string{"gateway", "storage", "certificates"} {
		if a, b := r5Check(pr, id).Status, r4Row(h, id).Status; a != b {
			t.Errorf("%s: POST %s, S36 %s", id, a, b)
		}
	}
}

// F3 again, while the gateway answers 502: the check that failed just now reads the same on both.
func TestR5PostProbeAndS36AgreeOnAFailingGateway(t *testing.T) { // r5 F3
	start := time.Now().UTC()
	r := newR5Rig(t, r5Options{gatewayStatus: http.StatusBadGateway})
	r.tick(start, 0)
	pr := r.post()
	h := r3Health(t, r.base, r.token)
	for _, id := range []string{"gateway", "storage", "certificates"} {
		if a, b := r5Check(pr, id).Status, r4Row(h, id).Status; a != b {
			t.Errorf("%s: POST %s, S36 %s", id, a, b)
		}
	}
}

// F4: a real expired gateway certificate was read and refused: failed, not "no certificate was read".
func TestR5PostProbeOnAnExpiredGatewayCertificateSaysFailed(t *testing.T) { // r5 F4
	start := time.Now().UTC()
	r := newR5Rig(t, r5Options{gatewayNotAfter: start.Add(-24 * time.Hour)})
	r.tick(start, 0)
	pr := r.post()
	c := r5Check(pr, "certificates")
	if c.Status != "failed" || strings.Contains(c.Detail, "no certificate was read") {
		t.Errorf("POST certificates on an expired gateway = %+v", c)
	}
	if h := r3Health(t, r.base, r.token); r4Row(h, "certificates").Status != "failed" {
		t.Errorf("S36 certificates = %+v", r4Row(h, "certificates"))
	}
}

// M1 on a real prober: pass, fail, then the worker is busy. One failure is below C02's streak, so no banner speaks.
func TestR6ARealProbersFailureSurvivesASkip(t *testing.T) { // r6 M1, C02
	start := time.Now().UTC()
	r := newR5Rig(t, r5Options{})
	be := r.app.Backend.(*fixtures.Backend)
	h := r.tick(start, 0)
	if c := r4Row(h, "storage"); c.Status != "ok" {
		t.Fatalf("pass: %+v", c)
	}
	be.Errors = map[string]error{adminpath.MethodCatConfig: adminpath.NewError(adminpath.CodeStorageUnreachable, "")}
	h = r.tick(start, time.Minute)
	if c := r4Row(h, "storage"); c.Status != "warn" {
		t.Fatalf("fail: %+v", c)
	}
	be.Errors = map[string]error{adminpath.MethodCatConfig: adminpath.NewError(adminpath.CodeBusy, "")}
	h = r.tick(start, 2*time.Minute)
	c := r4Row(h, "storage")
	if c.Status != "warn" {
		t.Errorf("fail, then the worker is busy: storage = %s %v %q, want warn", c.Status, c.Banners, c.Detail)
	}
	if len(c.Banners) != 0 {
		t.Errorf("the control is below C02's streak: %+v", c)
	}
}

// L3 on a real prober: the escrow read fails, and its row says error store.
func TestR6AFailedEscrowReadWarnsOnTheMasterKeyRow(t *testing.T) { // r6 L3, C12
	start := time.Now().UTC()
	r := newR5Rig(t, r5Options{})
	r.app.Prober.EscrowKeyIDs = func(context.Context) ([]string, error) { return nil, store.ErrBusy }
	h := r.tick(start, 0)
	c := r4Row(h, "master_key")
	if c.Status != "warn" || !strings.Contains(c.Detail, "store") {
		t.Errorf("a failed escrow read: master_key = %s %v %q, want warn saying store", c.Status, c.Banners, c.Detail)
	}
}
