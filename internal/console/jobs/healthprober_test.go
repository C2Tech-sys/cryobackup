package jobs

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/console/escrow"
	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

type proberRig struct {
	hp     *HealthProber
	st     *store.Store
	be     *fixtures.Backend
	status atomic.Int64 // the fake gateway's answer
	logs   *bytes.Buffer
	now    time.Time
}

// newProberRig is a health-prober over an in-memory store, the fake admin path
// and a TLS gateway answering rig.status. withFleet loads the fixture fleet
// (devices with escrowed keys, so the storage probe has a repository to read).
func newProberRig(t *testing.T, withFleet bool) *proberRig {
	t.Helper()
	r := &proberRig{now: time.Now().UTC().Truncate(time.Second), logs: &bytes.Buffer{}, be: &fixtures.Backend{}}
	clock := func() time.Time { return r.now }
	st, err := store.OpenMemory(context.Background(), clock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if withFleet {
		if err := store.LoadFixtures(context.Background(), st, r.now); err != nil {
			t.Fatal(err)
		}
	}
	r.st = st
	r.status.Store(http.StatusNotFound)
	gw := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if u, _, ok := req.BasicAuth(); !ok || u != "probe" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(int(r.status.Load()))
	}))
	t.Cleanup(gw.Close)
	pool := x509.NewCertPool()
	pool.AddCert(gw.Certificate())
	var k [32]byte
	_, _ = rand.Read(k[:])
	mk, _ := escrow.NewMasterKey(k[:])
	r.hp = &HealthProber{Store: st, Backend: r.be, Master: mk, Clock: clock, Started: r.now.Add(-time.Hour),
		Log: logx.New(r.logs, logx.LevelDebug)}
	r.hp.Gateway.URL, r.hp.Gateway.Roots = gw.URL, pool
	return r
}

func (r *proberRig) run(t *testing.T) {
	t.Helper()
	if err := r.hp.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// evaluate judges the prober's facts with every job-derived check healthy, so
// only probe banners show.
func (r *proberRig) evaluate() []string {
	p := r.hp.Facts()
	now := state.Millis(r.now.UnixMilli())
	p.Evaluator = &state.JobFacts{LastFinishedAt: now, Period: time.Minute}
	p.Maintain.Scheduler = &state.JobFacts{LastFinishedAt: now}
	p.Database.SelfBackup = &state.SelfBackupFacts{NotInBuild: true}
	p.Digest, p.Quota, p.Release = &state.DigestFacts{NotInBuild: true}, &state.QuotaFacts{NotInBuild: true}, &state.ReleaseFacts{NotInBuild: true}
	v := state.Evaluate(state.World{Probes: p, LoadComplete: true, PreviousDeviceCount: -1, LoadedAt: r.now}, r.now)
	var ids []string
	for _, b := range v.Banners {
		ids = append(ids, b.ID)
	}
	return ids
}

func TestC06HealthProberFirstRunRecordsEveryProbe(t *testing.T) {
	r := newProberRig(t, false)
	if got := strings.Join(r.evaluate(), ","); !strings.Contains(got, "C01b") || !strings.Contains(got, "C11") {
		t.Fatalf("before the first run the checks must be 'not checked', got %s", got)
	}
	r.run(t)
	p := r.hp.Facts()
	if !p.Wired || p.Gateway.LastOKAt == 0 || p.Gateway.CertNotAfter == 0 || p.Gateway.ConsecutiveFailures != 0 {
		t.Fatalf("gateway %+v", p.Gateway)
	}
	if p.Maintain.RPC.LastOKAt == 0 || !p.MasterKey.Loaded || p.MasterKey.KeyID == "" {
		t.Fatalf("rpc %+v key %+v", p.Maintain.RPC, p.MasterKey)
	}
	if !p.Database.IntegrityOK || p.Database.IntegrityCheckedAt == 0 || p.Database.MeasuredAt == 0 || p.Database.TotalBytes <= 0 {
		t.Fatalf("database %+v", p.Database)
	}
	// An empty console has no repository to probe and no SMTP: both "not checked", with their reasons.
	if p.Storage.Unconfigured == "" || p.SMTP.Probe.Unconfigured != unconfSMTP {
		t.Fatalf("storage %+v smtp %+v", p.Storage, p.SMTP.Probe)
	}
	if got := strings.Join(r.evaluate(), ","); got != "C02b,C03b" {
		t.Fatalf("banners %s, want C02b,C03b", got)
	}
	for row, status := range map[string]string{ProbeRowGateway: "ok", ProbeRowStorage: "skipped", ProbeRowSMTP: "skipped",
		ProbeRowMaintain: "ok", ProbeRowDatabase: "ok", ProbeRowMasterKey: "ok"} {
		j, err := r.st.GetJobState(context.Background(), row)
		if err != nil || j.LastStatus != status {
			t.Errorf("job_state %s: %+v %v, want %s", row, j, err, status)
		}
	}
	if strings.Contains(r.logs.String(), "probe:") || strings.Contains(r.logs.String(), "127.0.0.1") {
		t.Fatalf("logs carry a credential or an address:\n%s", r.logs.String())
	}
}

// §4.8 C06 and C07 end to end: three failed gateway probes in a row raise C01, one does not.
func TestC06C07HealthProberGatewayStreak(t *testing.T) {
	r := newProberRig(t, false)
	r.status.Store(http.StatusBadGateway)
	r.run(t)
	if got := r.evaluate(); slices.Contains(got, "C01") {
		t.Fatalf("one failure raised C01: %v", got)
	}
	r.now = r.now.Add(time.Minute)
	r.run(t)
	r.now = r.now.Add(time.Minute)
	r.run(t)
	p := r.hp.Facts()
	if p.Gateway.ConsecutiveFailures != 3 || p.Gateway.LastErrorClass != "status_502" {
		t.Fatalf("gateway %+v", p.Gateway)
	}
	if got := r.evaluate(); len(got) == 0 || got[0] != "C01" {
		t.Fatalf("three failures: %v, want C01 first", got)
	}
	r.status.Store(http.StatusOK)
	r.now = r.now.Add(time.Minute)
	r.run(t)
	if got := strings.Join(r.evaluate(), ","); strings.Contains(got, "C01") {
		t.Fatalf("recovered gateway still banners: %s", got)
	}
}

// §4.8 C08: the storage probe reads a device repository through the maintain
// RPC; two storage failures raise C02, an answer only the repository could give
// (missing, wrong key) is a pass, and a worker that cannot be reached is not a
// storage failure (C07 says so) but no run either.
func TestC08HealthProberStorage(t *testing.T) {
	r := newProberRig(t, true)
	r.run(t)
	if p := r.hp.Facts(); p.Storage.LastOKAt == 0 || p.Storage.Unconfigured != "" {
		t.Fatalf("storage %+v", p.Storage)
	}
	first, _ := r.st.GetJobState(context.Background(), ProbeRowStorage)

	r.be.Errors = map[string]error{adminpath.MethodCatConfig: adminpath.NewError(adminpath.CodeRepoMissing, "")}
	r.now = r.now.Add(time.Minute)
	r.run(t)
	if p := r.hp.Facts(); p.Storage.ConsecutiveFailures != 0 {
		t.Fatalf("repo_missing counted as a storage failure: %+v", p.Storage)
	}

	r.be.Errors = map[string]error{adminpath.MethodCatConfig: adminpath.NewError(adminpath.CodeStorageUnreachable, "network")}
	for i := 0; i < 2; i++ {
		r.now = r.now.Add(time.Minute)
		r.run(t)
	}
	if got := r.evaluate(); len(got) == 0 || got[0] != "C02" {
		t.Fatalf("two storage failures: %v, want C02", got)
	}

	r.be.Errors = map[string]error{adminpath.MethodCatConfig: adminpath.ErrUnavailable, adminpath.MethodPing: adminpath.ErrUnavailable}
	before := r.hp.Facts().Storage
	r.now = r.now.Add(time.Minute)
	r.run(t)
	p := r.hp.Facts()
	if p.Storage.LastRunAt != before.LastRunAt || p.Storage.ConsecutiveFailures != before.ConsecutiveFailures {
		t.Fatalf("an unreachable worker changed the storage run: %+v -> %+v", before, p.Storage)
	}
	if p.Maintain.RPC.ConsecutiveFailures != 1 {
		t.Fatalf("rpc %+v", p.Maintain.RPC)
	}
	if got := strings.Join(r.evaluate(), ","); !strings.Contains(got, "C07") {
		t.Fatalf("worker down: %s, want C07", got)
	}
	// The probe rotates over the fleet's repositories.
	last, _ := r.st.GetJobState(context.Background(), ProbeRowStorage)
	if last.RunCount <= first.RunCount {
		t.Fatalf("storage row not updated: %d -> %d", first.RunCount, last.RunCount)
	}
}

// The master key: not loaded is C12 and keeps the gateway probe from running
// (its password derives from the key), which is "not checked", never a gateway failure.
func TestC18HealthProberMasterKey(t *testing.T) {
	r := newProberRig(t, true)
	r.hp.Master = nil
	r.run(t)
	p := r.hp.Facts()
	if p.MasterKey.Loaded || p.Gateway.ConsecutiveFailures != 0 || p.Gateway.Unconfigured != unconfMasterKey {
		t.Fatalf("key %+v gateway %+v", p.MasterKey, p.Gateway)
	}
	got := strings.Join(r.evaluate(), ",")
	if !strings.Contains(got, "C12") || !strings.Contains(got, "C01b") {
		t.Fatalf("banners %s", got)
	}
	// The fixture escrows were wrapped under another key generation: a loaded key that differs is C12 too.
	var k [32]byte
	_, _ = rand.Read(k[:])
	r.hp.Master, _ = escrow.NewMasterKey(k[:])
	r.run(t)
	if ids := r.hp.Facts().MasterKey.EscrowKeyIDs; len(ids) == 0 {
		t.Fatal("escrow key ids not read")
	}
	if got := strings.Join(r.evaluate(), ","); !strings.Contains(got, "C12") {
		t.Fatalf("generation mismatch: %s", got)
	}
}

// A gateway that is not configured is "not checked" with its reason before
// and after the first run.
func TestC06HealthProberGatewayNotConfigured(t *testing.T) {
	r := newProberRig(t, false)
	r.hp.Gateway.URL = ""
	if p := r.hp.Facts(); p.Gateway.Unconfigured != unconfGatewayURL {
		t.Fatalf("before the run: %+v", p.Gateway)
	}
	r.run(t)
	if p := r.hp.Facts(); p.Gateway.Unconfigured != unconfGatewayURL || p.Gateway.LastRunAt != 0 {
		t.Fatalf("after the run: %+v", p.Gateway)
	}
}
