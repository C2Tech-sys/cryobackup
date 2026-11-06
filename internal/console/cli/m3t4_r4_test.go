package cli

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminapi"
	"github.com/c2tech-sys/cryobackup/internal/console/config"
	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
	"github.com/c2tech-sys/cryobackup/internal/console/jobs"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
)

// M3-T4 fix round 4, on a real App.

// r4Cert writes a self-signed certificate for 127.0.0.1 with the given lifetime.
func r4Cert(t *testing.T, dir, name string, notBefore, notAfter time.Time) (certPath, keyPath string) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(9), Subject: pkix.Name{CommonName: "r4"}, NotBefore: notBefore, NotAfter: notAfter,
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	kb, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	certPath, keyPath = filepath.Join(dir, name+".crt"), filepath.Join(dir, name+".key")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

func r4Row(h adminapi.SystemHealthResponse, id string) adminapi.SystemComponentJSON {
	for _, c := range h.Components {
		if c.ID == id {
			return c
		}
	}
	return adminapi.SystemComponentJSON{}
}

// HIGH-1, on a real TLS listener: the agent listener serves an expired certificate, or one the
// configured CA did not sign. The agent would refuse it, and so does the row.
func TestR4ARefusedAgentCertificateNeverReadsOK(t *testing.T) { // r4 HIGH-1, C05
	now := time.Now()
	for _, tc := range []struct {
		name          string
		notBefore     time.Time
		notAfter      time.Time
		otherCA       bool
		agentStatus   string
		certReason    string
		overallFailed bool
	}{
		{name: "expired", notBefore: now.Add(-48 * time.Hour), notAfter: now.Add(-24 * time.Hour), agentStatus: "failed", certReason: "expired", overallFailed: true},
		{name: "untrusted", notBefore: now.Add(-time.Hour), notAfter: now.Add(365 * 24 * time.Hour), otherCA: true, agentStatus: "warn", certReason: "untrusted", overallFailed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cp, kp := r4Cert(t, dir, "live", tc.notBefore, tc.notAfter)
			ca := cp
			if tc.otherCA {
				ca, _ = r4Cert(t, dir, "other", now.Add(-time.Hour), now.Add(365*24*time.Hour))
			}
			app := r3RealApp(t, func(c *config.Config) { c.TLS = config.TLS{Cert: cp, Key: kp}; c.AgentProbe.CAFile = ca })
			r3MaintainPass(t, app.Store)
			token := r3Token(t, app)
			base := r3Serve(t, app)
			h, settled := r3WaitFor(t, base, token, func(h adminapi.SystemHealthResponse) bool {
				return r4Row(h, "agent_api").Status == tc.agentStatus && r4Row(h, "gateway").Status == "ok"
			})
			if !settled {
				t.Fatalf("never settled: %s", r3Rows(h))
			}
			c := r4Row(h, "certificates")
			if c.Status != "failed" || !strings.Contains(c.Detail, "agent listener") || !strings.Contains(c.Detail, tc.certReason) {
				t.Errorf("certificates = %s %v %q, want failed naming the agent listener and %q", c.Status, c.Banners, c.Detail, tc.certReason)
			}
			if h.Overall != "failed" {
				t.Errorf("overall = %s with a refused certificate", h.Overall)
			}
			// S36 says the same.
			b := r3SignIn(t, app, base)
			_, page := b.do("GET", "/system", nil)
			i := strings.Index(page, `id="comp-certificates"`)
			if i < 0 {
				t.Fatal("no certificates row on S36")
			}
			row := page[i:]
			row = row[:strings.Index(row, "</tr>")]
			if strings.Contains(row, ">OK<") || !strings.Contains(row, "FAILED") || !strings.Contains(row, tc.certReason) {
				t.Errorf("S36 certificates row: %s", row)
			}
		})
	}
}

// HIGH-1, the gateway half: its certificate is refused by the real probe.
func TestR4ARefusedGatewayCertificateNeverReadsOK(t *testing.T) { // r4 HIGH-1, C05
	dir := t.TempDir()
	ca, _ := r4Cert(t, dir, "other", time.Now().Add(-time.Hour), time.Now().Add(365*24*time.Hour))
	app := r3RealApp(t, func(c *config.Config) { c.HealthProber.GatewayCAFile = ca })
	r3MaintainPass(t, app.Store)
	token := r3Token(t, app)
	base := r3Serve(t, app)
	h, settled := r3WaitFor(t, base, token, func(h adminapi.SystemHealthResponse) bool {
		return r4Row(h, "gateway").Status == "warn" && r4Row(h, "agent_api").Status == "ok"
	})
	if !settled {
		t.Fatalf("never settled: %s", r3Rows(h))
	}
	if c := r4Row(h, "certificates"); c.Status != "failed" || !strings.Contains(c.Detail, "gateway") || !strings.Contains(c.Detail, "untrusted") {
		t.Errorf("certificates = %+v", c)
	}
}

// MEDIUM-1: remove healthcheck_ping.url and restart. The job's old row must not hold overall off ok.
func TestR4ALeftoverJobRowDoesNotHoldOverallOffOK(t *testing.T) { // r4 M1
	app := r3RealApp(t, nil)
	r3MaintainPass(t, app.Store)
	last := time.Now().UTC().Add(-3 * time.Hour)
	due := last.Add(5 * time.Minute)
	if err := app.Store.RecordJobRun(context.Background(), jobs.NameHealthcheckPing, store.JobRun{Status: "error", ErrorClass: "evaluator_unhealthy", StartedAt: last, FinishedAt: last, NextDueAt: &due}); err != nil {
		t.Fatal(err)
	}
	token := r3Token(t, app)
	base := r3Serve(t, app)
	h, ok := r3WaitFor(t, base, token, func(h adminapi.SystemHealthResponse) bool { return h.Overall == "ok" })
	if !ok {
		t.Fatalf("a row of a job this process does not run holds overall at %s: %s", h.Overall, r3Rows(h))
	}
	b := r3SignIn(t, app, base)
	_, page := b.do("GET", "/system", nil)
	i := strings.Index(page, `id="job-`+jobs.NameHealthcheckPing+`"`)
	if i < 0 {
		t.Fatal("the leftover row is not in the job table")
	}
	row := page[i:]
	row = row[:strings.Index(row, "</tr>")]
	if !strings.Contains(row, "NO LONGER SCHEDULED") || strings.Contains(row, "OVERDUE") || strings.Contains(row, "Run now") || strings.Contains(row, ">ERROR<") {
		t.Errorf("the leftover row on S36: %s", row)
	}
}

// r4ClockedApp is a real non-fixtures App on a clock the test moves, with every optional job registered.
func r4ClockedApp(t *testing.T, clock *atomic.Pointer[time.Time], pingURL string) *App {
	t.Helper()
	dir := t.TempDir()
	if code, out, errb := run(t, "", initArgs(dir, "--master-key-copy-confirmed", "--timezone", "America/Chicago")...); code != 0 {
		t.Fatalf("init: %d\n%s\n%s", code, out, errb)
	}
	cfg, err := config.Read(filepath.Join(dir, "config.json"), func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	cfg.InsecureHTTPLab, cfg.AgentListen, cfg.AdminListen = true, "127.0.0.1:0", "127.0.0.1:0"
	cfg.AdminpathSocket = filepath.Join(dir, "a.sock")
	cfg.HealthcheckPing.URL = pingURL
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	app, err := Build(context.Background(), cfg, BuildOptions{Backend: &fixtures.Backend{}, Log: logx.New(&logs, logx.LevelDebug), Clock: func() time.Time { return *clock.Load() }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	return app
}

// MEDIUM-2: overdue allows for the job's real jitter, for every job serve registers: each row's own schedule
// (next due minus finish, as jobs.Runner.RunOnce records it), the runner's plus or minus 10 %, and a minute.
func TestR4NoRegisteredJobIsOverdueInsideItsJitter(t *testing.T) { // r4 M2
	var clock atomic.Pointer[time.Time]
	start := time.Now().UTC()
	clock.Store(&start)
	app := r4ClockedApp(t, &clock, "http://127.0.0.1:1/ping")
	srv := httptest.NewServer(app.Server.AdminRouter)
	t.Cleanup(srv.Close)
	token := r3Token(t, app)
	ctx := context.Background()
	names := app.Runner.Names()
	if len(names) < 8 {
		t.Fatalf("registered jobs: %v", names)
	}
	for _, n := range names {
		if _, err := app.Runner.RunOnce(ctx, n); err != nil && !strings.Contains(err.Error(), "job failed") {
			t.Logf("%s: %v", n, err)
		}
	}
	app.Prober.WaitAll()
	jobRow := func(name string) adminapi.JobJSON {
		t.Helper()
		req, _ := http.NewRequest("GET", srv.URL+"/api/v1/system/jobs", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var jl adminapi.JobListResponse
		if err := json.NewDecoder(res.Body).Decode(&jl); err != nil {
			t.Fatal(err)
		}
		for _, j := range jl.Jobs {
			if j.Name == name {
				return j
			}
		}
		t.Fatalf("job %s is not listed", name)
		return adminapi.JobJSON{}
	}
	sawHourly := false
	for _, n := range names {
		j := jobRow(n)
		if j.NextDueAt == nil || j.LastFinishedAt == nil {
			t.Errorf("%s has no schedule on its row: %+v", n, j)
			continue
		}
		every := j.NextDueAt.Sub(*j.LastFinishedAt)
		sawHourly = sawHourly || every == time.Hour
		slack := every/10 + time.Minute
		at := j.NextDueAt.Add(slack - time.Second)
		clock.Store(&at)
		if got := jobRow(n); got.Overdue {
			t.Errorf("%s (every %v): overdue %v after it was due, inside the runner's jitter", n, every, slack-time.Second)
		}
		at2 := j.NextDueAt.Add(slack + time.Second)
		clock.Store(&at2)
		if got := jobRow(n); !got.Overdue {
			t.Errorf("%s (every %v): not overdue %v after it was due, past the jitter", n, every, slack+time.Second)
		}
		clock.Store(&start)
	}
	if !sawHourly {
		t.Error("session-expire's hourly schedule was not among them")
	}
}

// LOW 2: the first ping of a fresh console, before the first evaluation, is skipped: no ping is sent and no failure is recorded.
func TestR4AFreshConsolesPingSkipsUntilAnEvaluationHasFinished(t *testing.T) { // r4 low 2
	var hits atomic.Int32
	monitor := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	t.Cleanup(monitor.Close)
	var clock atomic.Pointer[time.Time]
	start := time.Now().UTC()
	clock.Store(&start)
	app := r4ClockedApp(t, &clock, monitor.URL+"/ping")
	ctx := context.Background()
	if ran, err := app.Runner.RunOnce(ctx, jobs.NameHealthcheckPing); !ran || err != nil {
		t.Fatalf("the first ping on a fresh console: ran %v err %v", ran, err)
	}
	row, err := app.Store.GetJobState(ctx, jobs.NameHealthcheckPing)
	if err != nil || row.LastStatus != "skipped" || row.LastErrorClass != "" || hits.Load() != 0 {
		t.Fatalf("row %+v err %v hits %d", row, err, hits.Load())
	}
	// Once an evaluation has finished, it pings.
	if ran, err := app.Runner.RunOnce(ctx, jobs.NameAlarmEvaluator); !ran || err != nil {
		t.Fatalf("evaluator: %v %v", ran, err)
	}
	if ran, err := app.Runner.RunOnce(ctx, jobs.NameHealthcheckPing); !ran || err != nil || hits.Load() != 1 {
		t.Fatalf("ping after an evaluation: ran %v err %v hits %d", ran, err, hits.Load())
	}
}
