package jobs

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/console/escrow"
	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
	"github.com/c2tech-sys/cryobackup/internal/console/gateway"
	"github.com/c2tech-sys/cryobackup/internal/console/probe"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
	"github.com/c2tech-sys/cryobackup/internal/restic"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// syncBuf is a log sink safe for the prober's concurrent probes.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// testClock is a settable clock safe for the concurrent probes.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *testClock) Advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

type proberRig struct {
	hp     *HealthProber
	st     *store.Store
	be     *fixtures.Backend
	mk     *escrow.MasterKey
	status atomic.Int64 // the fake gateway's answer
	logs   *syncBuf
	clk    *testClock
}

// newProberRig is a health-prober over an in-memory store, the fake admin path
// and a TLS gateway answering rig.status. withFleet loads the fixture fleet.
func newProberRig(t *testing.T, withFleet bool) *proberRig {
	t.Helper()
	r := &proberRig{clk: &testClock{t: time.Now().UTC().Truncate(time.Second)}, logs: &syncBuf{}, be: &fixtures.Backend{}}
	st, err := store.OpenMemory(context.Background(), r.clk.Now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if withFleet {
		if err := store.LoadFixtures(context.Background(), st, r.clk.Now()); err != nil {
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
	r.mk, _ = escrow.NewMasterKey(k[:])
	r.hp = &HealthProber{Store: st, Backend: r.be, Master: r.mk, Clock: r.clk.Now, Started: r.clk.Now().Add(-time.Hour),
		Log: logx.New(r.logs, logx.LevelDebug)}
	r.hp.Gateway.URL, r.hp.Gateway.Roots = gw.URL, pool
	t.Cleanup(r.hp.WaitAll)
	return r
}

// run is one tick and waits for its per-tick probes (not the background integrity check).
func (r *proberRig) run(t *testing.T) {
	t.Helper()
	if err := r.hp.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.hp.Wait()
}

// evaluate judges the prober's facts with every job-derived check healthy, so
// only probe banners show.
func (r *proberRig) evaluate() []string {
	p := r.hp.Facts()
	now := state.Millis(r.clk.Now().UnixMilli())
	p.Evaluator = &state.JobFacts{LastFinishedAt: now, Period: time.Minute}
	p.Maintain.Scheduler = &state.JobFacts{LastFinishedAt: now}
	p.Database.SelfBackup = &state.SelfBackupFacts{NotInBuild: true}
	p.Digest, p.Quota, p.Release = &state.DigestFacts{NotInBuild: true}, &state.QuotaFacts{NotInBuild: true}, &state.ReleaseFacts{NotInBuild: true}
	v := state.Evaluate(state.World{Probes: p, LoadComplete: true, PreviousDeviceCount: -1, LoadedAt: r.clk.Now()}, r.clk.Now())
	var ids []string
	for _, b := range v.Banners {
		ids = append(ids, b.ID)
	}
	return ids
}

func (r *proberRig) row(t *testing.T, name string) store.JobState {
	t.Helper()
	j, err := r.st.GetJobState(context.Background(), name)
	if err != nil {
		t.Fatalf("job_state %s: %v", name, err)
	}
	return j
}

func TestC06HealthProberFirstRunRecordsEveryProbe(t *testing.T) {
	r := newProberRig(t, false)
	if got := strings.Join(r.evaluate(), ","); !strings.Contains(got, "C01b") || !strings.Contains(got, "C11") || !strings.Contains(got, "C12b") {
		t.Fatalf("before the first run the checks must be 'not checked', got %s", got)
	}
	r.run(t)
	r.hp.WaitAll()
	p := r.hp.Facts()
	if !p.Wired || p.Gateway.LastOKAt == 0 || p.Gateway.CertNotAfter == 0 || p.Gateway.ConsecutiveFailures != 0 {
		t.Fatalf("gateway %+v", p.Gateway)
	}
	if p.Maintain.RPC.LastOKAt == 0 || p.MasterKey == nil || !p.MasterKey.Loaded || p.MasterKey.KeyID == "" {
		t.Fatalf("rpc %+v key %+v", p.Maintain.RPC, p.MasterKey)
	}
	if !p.Database.IntegrityOK || p.Database.IntegrityCheckedAt == 0 || p.Database.MeasuredAt == 0 || p.Database.TotalBytes <= 0 {
		t.Fatalf("database %+v", p.Database)
	}
	// ADR 0017 / M3-T2 H1(c): a console with no device reaches a healthy storage probe through the probe repository.
	if p.Storage.LastOKAt == 0 || p.Storage.Unconfigured != "" {
		t.Fatalf("storage %+v", p.Storage)
	}
	calls := r.be.Calls()
	if !slices.ContainsFunc(calls, func(c fixtures.Call) bool {
		return c.Method == adminpath.MethodCatConfig && c.DeviceID == adminpath.ProbeRepoID
	}) {
		t.Fatalf("storage probe did not address the probe repository: %+v", calls)
	}
	if got := strings.Join(r.evaluate(), ","); got != "C03b" {
		t.Fatalf("banners %s, want C03b (SMTP not configured)", got)
	}
	for row, status := range map[string]string{ProbeRowGateway: "ok", ProbeRowStorage: "ok", ProbeRowSMTP: "skipped",
		ProbeRowMaintain: "ok", ProbeRowDatabase: "ok", ProbeRowIntegrity: "ok", ProbeRowMasterKey: "ok"} {
		if j := r.row(t, row); j.LastStatus != status {
			t.Errorf("job_state %s: %+v, want %s", row, j, status)
		}
	}
}

// M3 m7, m7b: the derived probe password (64 hex characters) never reaches a
// log line or a job_state row, through a passing and a refused gateway probe.
func TestC06ProbePasswordNeverLoggedOrStored(t *testing.T) {
	r := newProberRig(t, true)
	pw, err := gateway.ProbePassword(r.mk.Bytes())
	if err != nil || len(pw) != 64 {
		t.Fatalf("probe password %d chars, %v", len(pw), err)
	}
	r.run(t)
	r.status.Store(http.StatusUnauthorized)
	r.clk.Advance(time.Minute)
	r.run(t)
	r.hp.WaitAll()
	if strings.Contains(r.logs.String(), pw) || strings.Contains(r.logs.String(), pw[:16]) {
		t.Fatal("the probe password reached the log")
	}
	rows, err := r.st.ListJobStates(context.Background())
	if err != nil || len(rows) == 0 {
		t.Fatalf("rows %v %v", rows, err)
	}
	for _, j := range rows {
		for _, v := range []string{j.Name, j.LastStatus, j.LastErrorClass, j.LeaseHolder} {
			if strings.Contains(v, pw[:16]) {
				t.Fatalf("the probe password reached job_state row %s", j.Name)
			}
		}
	}
	if j := r.row(t, ProbeRowGateway); j.LastErrorClass != "unauthorized" {
		t.Fatalf("refused probe row %+v", j)
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
	r.clk.Advance(time.Minute)
	r.run(t)
	r.clk.Advance(time.Minute)
	r.run(t)
	p := r.hp.Facts()
	if p.Gateway.ConsecutiveFailures != 3 || p.Gateway.LastErrorClass != "status_502" {
		t.Fatalf("gateway %+v", p.Gateway)
	}
	if got := r.evaluate(); len(got) == 0 || got[0] != "C01" {
		t.Fatalf("three failures: %v, want C01 first", got)
	}
	r.status.Store(http.StatusOK)
	r.clk.Advance(time.Minute)
	r.run(t)
	if got := strings.Join(r.evaluate(), ","); strings.Contains(got, "C01") {
		t.Fatalf("recovered gateway still banners: %s", got)
	}
}

// §4.8 C08 (M3-T2 H1a): the storage probe passes only on a clean list + cat
// config of the probe repository. repo_missing is a failure with its own class
// (the probe repository was created once and is gone), two in a row raise C02;
// a worker that cannot be reached is not a storage failure but no run either.
func TestC08HealthProberStorage(t *testing.T) {
	r := newProberRig(t, true)
	r.run(t)
	if p := r.hp.Facts(); p.Storage.LastOKAt == 0 {
		t.Fatalf("storage %+v", p.Storage)
	}
	r.be.Errors = map[string]error{adminpath.MethodCatConfig: adminpath.NewError(adminpath.CodeRepoMissing, restic.ClassRepoMissing)}
	r.clk.Advance(time.Minute)
	r.run(t)
	if p := r.hp.Facts(); p.Storage.ConsecutiveFailures != 1 || p.Storage.LastErrorClass != "repo_missing" {
		t.Fatalf("repo_missing must be a storage failure: %+v", p.Storage)
	}
	r.clk.Advance(time.Minute)
	r.run(t)
	if got := r.evaluate(); len(got) == 0 || got[0] != "C02" {
		t.Fatalf("two repo_missing probes: %v, want C02", got)
	}
	if j := r.row(t, ProbeRowStorage); j.LastStatus != "error" || j.LastErrorClass != "repo_missing" {
		t.Fatalf("row %+v", j)
	}

	r.be.Errors = map[string]error{adminpath.MethodCatConfig: adminpath.NewError(adminpath.CodeStorageUnreachable, restic.ClassNetwork)}
	r.clk.Advance(time.Minute)
	r.run(t)
	if p := r.hp.Facts(); p.Storage.LastErrorClass != string(restic.ClassNetwork) || p.Storage.ConsecutiveFailures != 3 {
		t.Fatalf("unreachable storage %+v", p.Storage)
	}

	r.be.Errors = map[string]error{adminpath.MethodCatConfig: adminpath.ErrUnavailable, adminpath.MethodPing: adminpath.ErrUnavailable}
	before := r.hp.Facts().Storage
	r.clk.Advance(time.Minute)
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
}

// blockingBackend's CatConfig never answers until its context ends.
type blockingBackend struct{ *fixtures.Backend }

func (b blockingBackend) CatConfig(ctx context.Context, _ adminpath.DeviceRequest) (restic.Config, error) {
	<-ctx.Done()
	return restic.Config{}, ctx.Err()
}

// M3 m2b: the storage probe is bounded; a worker that never answers is a
// timeout failure, not a probe that hangs.
func TestC08StorageProbeIsBounded(t *testing.T) {
	r := newProberRig(t, false)
	r.hp.Backend = blockingBackend{r.be}
	r.hp.StorageTimeout = 200 * time.Millisecond
	done := make(chan struct{})
	go func() { r.run(t); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the storage probe outlived its bound")
	}
	if p := r.hp.Facts(); p.Storage.ConsecutiveFailures != 1 || p.Storage.LastErrorClass != "timeout" {
		t.Fatalf("storage %+v", p.Storage)
	}
}

// fakeRelay is an SMTP relay that greets and answers EHLO and QUIT; any other
// command (MAIL, RCPT, DATA, AUTH, ...) is a protocol violation by the probe.
func fakeRelay(t *testing.T, violations *atomic.Int64) probe.SMTPSettings {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				rd := bufio.NewReader(c)
				_, _ = c.Write([]byte("220 relay\r\n"))
				for {
					line, err := rd.ReadString('\n')
					if err != nil {
						return
					}
					switch strings.ToUpper(strings.Fields(line + " x")[0]) {
					case "EHLO", "HELO":
						_, _ = c.Write([]byte("250 relay\r\n"))
					case "QUIT":
						_, _ = c.Write([]byte("221 bye\r\n"))
						return
					default:
						violations.Add(1)
						_, _ = c.Write([]byte("502 not for a probe\r\n"))
					}
				}
			}(c)
		}
	}()
	h, p, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(p)
	return probe.SMTPSettings{Host: h, Port: port, Security: "plain", Timeout: 2 * time.Second}
}

// M3-T2 M1: each probe has its own cadence. While integrity_check hangs for
// longer than three prober intervals, the gateway, storage, SMTP and maintain
// RPC facts stay fresh and no "not checked" banner appears; the hung check is
// not started a second time.
func TestC17IntegrityHangDoesNotStaleTheOtherProbes(t *testing.T) {
	r := newProberRig(t, false)
	var violations atomic.Int64
	r.hp.SMTP = fakeRelay(t, &violations)
	var calls atomic.Int64
	r.hp.IntegrityCheck = func(ctx context.Context) (string, error) {
		calls.Add(1)
		<-ctx.Done()
		return "", ctx.Err()
	}
	r.hp.IntegrityTimeout = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for i := 0; i < 5; i++ { // five ticks, four intervals
		if i > 0 {
			r.clk.Advance(time.Minute)
		}
		done := make(chan struct{})
		go func() { _ = r.hp.Run(ctx); r.hp.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("a tick waited for the hung integrity check")
		}
	}
	got := r.evaluate()
	for _, b := range got {
		if strings.HasSuffix(b, "b") {
			t.Fatalf("a not-checked banner while integrity hangs: %v", got)
		}
	}
	p := r.hp.Facts()
	now := state.Millis(r.clk.Now().UnixMilli())
	for name, run := range map[string]state.ProbeRun{"gateway": *p.Gateway, "storage": *p.Storage, "smtp": p.SMTP.Probe, "rpc": p.Maintain.RPC} {
		if run.LastRunAt != now {
			t.Errorf("%s not fresh: last run %d, now %d", name, run.LastRunAt, now)
		}
	}
	if calls.Load() != 1 || violations.Load() != 0 {
		t.Fatalf("integrity started %d times (want 1); SMTP violations %d", calls.Load(), violations.Load())
	}
	cancel()
	r.hp.WaitAll()
}

// M3-T2 M1: an integrity check that times out backs off for a day instead of
// running again on the next tick.
func TestC17IntegrityTimeoutBacksOffADay(t *testing.T) {
	r := newProberRig(t, false)
	var calls atomic.Int64
	r.hp.IntegrityCheck = func(ctx context.Context) (string, error) {
		calls.Add(1)
		<-ctx.Done()
		return "", ctx.Err()
	}
	r.hp.IntegrityTimeout = 50 * time.Millisecond
	r.run(t)
	r.hp.WaitAll()
	if j := r.row(t, ProbeRowIntegrity); j.LastStatus != "error" || j.LastErrorClass != ClassTimeout {
		t.Fatalf("row %+v", j)
	}
	for i := 0; i < 3; i++ {
		r.clk.Advance(time.Minute)
		r.run(t)
		r.hp.WaitAll()
	}
	if calls.Load() != 1 {
		t.Fatalf("retried %d times within the day", calls.Load()-1)
	}
	r.clk.Advance(IntegrityRetryAfter)
	r.run(t)
	r.hp.WaitAll()
	if calls.Load() != 2 {
		t.Fatalf("not retried after the back-off: %d calls", calls.Load())
	}
}

// M3 m10: an integrity result other than "ok" is a failed check: C11 critical.
func TestC17IntegrityProblemIsAFailedCheck(t *testing.T) {
	r := newProberRig(t, false)
	r.hp.IntegrityCheck = func(context.Context) (string, error) { return "row 3 missing from index idx_x", nil }
	r.run(t)
	r.hp.WaitAll()
	if p := r.hp.Facts(); p.Database.IntegrityOK || p.Database.IntegrityCheckedAt == 0 {
		t.Fatalf("database %+v", p.Database)
	}
	if j := r.row(t, ProbeRowIntegrity); j.LastStatus != "error" || j.LastErrorClass != "integrity_failed" {
		t.Fatalf("row %+v", j)
	}
	p := r.hp.Facts()
	v := state.Evaluate(state.World{Probes: state.ProbeFacts{Wired: true, Database: p.Database}, LoadComplete: true, PreviousDeviceCount: -1,
		LoadedAt: r.clk.Now()}, r.clk.Now())
	for _, b := range v.Banners {
		if b.ID == "C11" {
			if b.Severity != state.SeverityCritical || !strings.Contains(b.Text, "integrity check failed") {
				t.Fatalf("C11 %+v", b)
			}
			return
		}
	}
	t.Fatal("no C11 for a failed integrity check")
}

// M3-T2 M2: the generation comparison counts only when the escrow key ids were
// read: a failed read, or a result older than one interval (plus jitter), is
// C12b, never "loaded, no mismatch"; a key that is not loaded is C12.
func TestC18MasterKeyComparisonMustBeCurrent(t *testing.T) {
	r := newProberRig(t, true)
	fail := atomic.Bool{}
	r.hp.EscrowKeyIDs = func(ctx context.Context) ([]string, error) {
		if fail.Load() {
			return nil, errors.New("database is locked")
		}
		return []string{r.mk.KeyID()}, nil
	}
	r.run(t)
	if p := r.hp.Facts(); p.MasterKey == nil || !p.MasterKey.Loaded {
		t.Fatalf("after a read: %+v", p.MasterKey)
	}
	if got := r.evaluate(); slices.Contains(got, "C12") || slices.Contains(got, "C12b") {
		t.Fatalf("matching generation: %v", got)
	}
	fail.Store(true)
	r.clk.Advance(time.Minute)
	r.run(t)
	if p := r.hp.Facts(); p.MasterKey != nil {
		t.Fatalf("a failed read left a comparison standing: %+v", p.MasterKey)
	}
	if got := r.evaluate(); !slices.Contains(got, "C12b") {
		t.Fatalf("failed read: %v, want C12b", got)
	}
	if j := r.row(t, ProbeRowMasterKey); j.LastStatus != "error" {
		t.Fatalf("row %+v", j)
	}
	// A good read, then no read for two intervals: stale, C12b.
	fail.Store(false)
	r.clk.Advance(time.Minute)
	r.run(t)
	r.clk.Advance(2 * time.Minute)
	if got := r.evaluate(); !slices.Contains(got, "C12b") {
		t.Fatalf("stale comparison: %v, want C12b", got)
	}
	r.hp.Master = nil
	if got := r.evaluate(); !slices.Contains(got, "C12") {
		t.Fatalf("no key: %v, want C12", got)
	}
}

// M3 m14: the master-key row records a generation mismatch, and C12 shows it.
// The fixture escrows were wrapped under another key generation than the rig's key.
func TestC18MasterKeyRowRecordsAGenerationMismatch(t *testing.T) {
	r := newProberRig(t, true)
	r.run(t)
	if ids := r.hp.Facts().MasterKey.EscrowKeyIDs; len(ids) == 0 || ids[0] == r.mk.KeyID() {
		t.Fatalf("fixture escrow ids %v", ids)
	}
	if j := r.row(t, ProbeRowMasterKey); j.LastStatus != "error" || j.LastErrorClass != "generation_mismatch" {
		t.Fatalf("row %+v", j)
	}
	if got := r.evaluate(); !slices.Contains(got, "C12") {
		t.Fatalf("banners %v, want C12", got)
	}
}

// The master key not loaded is C12 and keeps the gateway probe from running
// (its password derives from the key): "not checked", never a gateway failure.
func TestC18HealthProberMasterKeyNotLoaded(t *testing.T) {
	r := newProberRig(t, true)
	r.hp.Master = nil
	r.run(t)
	p := r.hp.Facts()
	if p.MasterKey == nil || p.MasterKey.Loaded || p.Gateway.ConsecutiveFailures != 0 || p.Gateway.Unconfigured != unconfMasterKey {
		t.Fatalf("key %+v gateway %+v", p.MasterKey, p.Gateway)
	}
	got := strings.Join(r.evaluate(), ",")
	if !strings.Contains(got, "C12") || !strings.Contains(got, "C01b") {
		t.Fatalf("banners %s", got)
	}
	if j := r.row(t, ProbeRowMasterKey); j.LastErrorClass != ClassMasterKey {
		t.Fatalf("row %+v", j)
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
