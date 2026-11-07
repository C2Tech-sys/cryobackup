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
	ctx    context.Context // what the rig's ticks are called with; probes never run under it, so cleanup ends them with Stop
}

// waitBounded runs wait and fails the test with what instead of hanging when it
// does not return within d (M3-T2 delta 2, L4).
func waitBounded(t *testing.T, what string, d time.Duration, wait func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
		t.Errorf("%s did not finish within %v", what, d)
	}
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
	ctx, cancel := context.WithCancel(context.Background())
	r.ctx = ctx
	t.Cleanup(func() { // runs before the store closes (cleanups are LIFO)
		cancel()
		waitBounded(t, "the probes at cleanup", 15*time.Second, r.hp.Stop) // the probes run under the prober's lifetime: Stop ends them
	})
	return r
}

// waitAll waits for every probe, the integrity check included, bounded.
func (r *proberRig) waitAll(t *testing.T) {
	t.Helper()
	waitBounded(t, "the probes", 30*time.Second, r.hp.WaitAll)
	if t.Failed() {
		t.FailNow()
	}
}

// run is one tick and waits for its per-tick probes (not the background integrity check).
func (r *proberRig) run(t *testing.T) {
	t.Helper()
	if err := r.hp.Run(r.ctx); err != nil {
		t.Fatal(err)
	}
	waitBounded(t, "the tick's probes", 30*time.Second, r.hp.Wait)
	if t.Failed() {
		t.FailNow()
	}
}

// evaluate judges the prober's facts with every job-derived check healthy, so
// only probe banners show.
func (r *proberRig) evaluate() []string {
	p := r.hp.Facts()
	now := state.Millis(r.clk.Now().UnixMilli())
	p.Evaluator = &state.JobFacts{LastFinishedAt: now, Period: time.Minute}
	p.Maintain.Scheduler = &state.JobFacts{LastFinishedAt: now}
	p.Database.SelfBackup = &state.SelfBackupFacts{NotInBuild: true}
	p.LoginSync = &state.LoginSyncFacts{} // the evaluator supplies it; nothing is retired in this rig
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
	r.waitAll(t)
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
	r.waitAll(t)
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
	if err := r.hp.Run(r.ctx); err != nil {
		t.Fatal(err)
	}
	// L4: a probe that ignores its bound fails here with a message; the cleanup's
	// cancel then releases it, so the package never hangs to its timeout.
	waitBounded(t, "the storage probe (its 200 ms bound)", 5*time.Second, r.hp.Wait)
	if t.Failed() {
		t.FailNow()
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
	// M3-INT2: the probes run under the prober's own lifetime, not a tick's context, so it is Stop
	// (serve's shutdown) that cancels the hung check; it ends at once, and nothing is abandoned.
	cancel()
	waitBounded(t, "Stop with the hung integrity check", 10*time.Second, r.hp.Stop)
	waitBounded(t, "the cancelled integrity check", 2*time.Second, r.hp.WaitAll)
	if strings.Contains(r.logs.String(), "abandoned") {
		t.Fatal("Stop abandoned an integrity check that honours its context")
	}
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
	r.waitAll(t)
	if j := r.row(t, ProbeRowIntegrity); j.LastStatus != "error" || j.LastErrorClass != ClassTimeout {
		t.Fatalf("row %+v", j)
	}
	for i := 0; i < 3; i++ {
		r.clk.Advance(time.Minute)
		r.run(t)
		r.waitAll(t)
	}
	if calls.Load() != 1 {
		t.Fatalf("retried %d times within the day", calls.Load()-1)
	}
	r.clk.Advance(IntegrityRetryAfter)
	r.run(t)
	r.waitAll(t)
	if calls.Load() != 2 {
		t.Fatalf("not retried after the back-off: %d calls", calls.Load())
	}
}

// M3 m10: an integrity result other than "ok" is a failed check: C11 critical.
func TestC17IntegrityProblemIsAFailedCheck(t *testing.T) {
	r := newProberRig(t, false)
	r.hp.IntegrityCheck = func(context.Context) (string, error) { return "row 3 missing from index idx_x", nil }
	r.run(t)
	r.waitAll(t)
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

// countingBackend blocks every CatConfig until release is closed (or its
// context ends) and records how many ran at once.
type countingBackend struct {
	*fixtures.Backend
	release      chan struct{}
	active, peak atomic.Int64
	total        atomic.Int64
}

func (b *countingBackend) CatConfig(ctx context.Context, _ adminpath.DeviceRequest) (restic.Config, error) {
	n := b.active.Add(1)
	b.total.Add(1)
	defer b.active.Add(-1)
	for {
		p := b.peak.Load()
		if n <= p || b.peak.CompareAndSwap(p, n) {
			break
		}
	}
	select {
	case <-b.release:
		return restic.Config{}, nil
	case <-ctx.Done():
		return restic.Config{}, ctx.Err()
	}
}

// M3-T2 delta 2, M-1: two consoles (two runners, two probers) on one database.
// The holder of the probe lease probes; the other console's tick runs (its
// runner lease is free) but starts no probe, so storage probes never overlap,
// and it shows the not-checked warnings.
func TestC08TwoConsolesNeverProbeAtOnce(t *testing.T) {
	r := newProberRig(t, false)
	be := &countingBackend{Backend: r.be, release: make(chan struct{})}
	r.hp.Backend, r.hp.Holder = be, "console-a"
	second := &HealthProber{Store: r.st, Backend: be, Master: r.mk, Clock: r.clk.Now, Started: r.clk.Now(), Holder: "console-b",
		Log: logx.New(r.logs, logx.LevelDebug), Gateway: r.hp.Gateway}
	t.Cleanup(func() { waitBounded(t, "the second prober", 10*time.Second, second.WaitAll) })
	runA, runB := NewRunner(r.st, "console-a", nil), NewRunner(r.st, "console-b", nil)
	runA.Add(r.hp.Job())
	runB.Add(second.Job())
	if ran, err := runA.RunOnce(r.ctx, NameHealthProber); !ran || err != nil {
		t.Fatalf("first console: ran %v err %v", ran, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for be.active.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	ran, err := runB.RunOnce(r.ctx, NameHealthProber)
	if err != nil || !ran {
		t.Fatalf("second console's tick: ran %v err %v (its runner lease is free while the first console's probes run)", ran, err)
	}
	time.Sleep(200 * time.Millisecond)
	if p := be.peak.Load(); p != 1 || be.total.Load() != 1 {
		t.Fatalf("peak concurrent storage probes %d, total %d: want 1 and 1", p, be.total.Load())
	}
	second.Wait()
	if p := second.Facts(); p.Gateway.LastRunAt != 0 || p.Maintain.RPC.LastRunAt != 0 || p.Storage.LastRunAt != 0 {
		t.Fatalf("the console without the lease probed: gateway %+v rpc %+v", p.Gateway, p.Maintain.RPC)
	}
	close(be.release)
	r.waitAll(t)
	if p := r.hp.Facts(); p.Storage.LastOKAt == 0 {
		t.Fatalf("the holder's storage probe did not complete: %+v", p.Storage)
	}
}

// M3-T2 delta 2, M-1: the lease is renewed while a probe is in flight, and
// losing it cancels every probe in flight; nothing is recorded for them.
func TestC08ProbeLeaseLostCancelsProbesInFlight(t *testing.T) {
	r := newProberRig(t, false)
	be := &countingBackend{Backend: r.be, release: make(chan struct{})}
	defer close(be.release)
	r.hp.Backend, r.hp.Holder, r.hp.renewEvery = be, "console-a", 20*time.Millisecond
	if err := r.hp.Run(r.ctx); err != nil {
		t.Fatal(err)
	}
	waitActive(t, be)
	// The renewer keeps the lease while the probe runs: the lease is ours and its expiry moves on.
	first := r.row(t, ProbeLease)
	r.clk.Advance(time.Minute)
	waitLeaseRenewedAfter(t, r, "console-a", *first.LeaseUntil)
	// Another console takes it.
	if err := r.st.Write(r.ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE job_state SET lease_holder = 'console-b' WHERE name = ?`, ProbeLease)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	waitBounded(t, "the probes after the lease was lost", 30*time.Second, r.hp.WaitAll)
	if be.active.Load() != 0 {
		t.Fatal("a storage probe kept running without the lease")
	}
	if p := r.hp.Facts(); p.Storage.LastRunAt != 0 {
		t.Fatalf("a cancelled probe was recorded: %+v", p.Storage)
	}
}

// panickingBackend panics in CatConfig.
type panickingBackend struct{ *fixtures.Backend }

func (panickingBackend) CatConfig(context.Context, adminpath.DeviceRequest) (restic.Config, error) {
	panic("client 0123 /home/someone/secret.docx")
}

// M3-T2 delta 2, L2: a panic in one probe is that probe's internal_panic row;
// the console and the other probes carry on, and the log carries no panic value.
func TestC08ProbePanicIsContained(t *testing.T) {
	r := newProberRig(t, false)
	r.hp.Backend = panickingBackend{r.be}
	r.run(t)
	if j := r.row(t, ProbeRowStorage); j.LastStatus != "error" || j.LastErrorClass != ClassPanic {
		t.Fatalf("storage row %+v", j)
	}
	if j := r.row(t, ProbeRowGateway); j.LastStatus != "ok" {
		t.Fatalf("gateway row %+v: the other probes must run", j)
	}
	if strings.Contains(r.logs.String(), "secret.docx") || strings.Contains(r.logs.String(), "someone") {
		t.Fatal("the panic value reached the log")
	}
	r.clk.Advance(time.Minute)
	r.run(t) // the prober is still alive and the storage probe is not stuck in flight
	if j := r.row(t, ProbeRowStorage); j.RunCount != 2 {
		t.Fatalf("storage probe ran %d times, want 2", j.RunCount)
	}
}

// M3-T2 delta 2, M-2: a worker whose slots are all taken answers busy; that is
// "not checked" (C02b once the last result ages), never C02.
func TestC08BusyWorkerIsNotCheckedNeverC02(t *testing.T) {
	r := newProberRig(t, false)
	r.run(t) // a passing probe first
	r.be.Errors = map[string]error{adminpath.MethodCatConfig: adminpath.NewError(adminpath.CodeBusy, "")}
	for i := 0; i < 4; i++ {
		r.clk.Advance(time.Minute)
		r.run(t)
	}
	got := r.evaluate()
	if slices.Contains(got, "C02") || !slices.Contains(got, "C02b") {
		t.Fatalf("a busy worker for four intervals: %v, want C02b and no C02", got)
	}
	if p := r.hp.Facts(); p.Storage.ConsecutiveFailures != 0 {
		t.Fatalf("busy counted as a storage failure: %+v", p.Storage)
	}
	if j := r.row(t, ProbeRowStorage); j.LastStatus != "skipped" || j.LastErrorClass != adminpath.CodeBusy {
		t.Fatalf("row %+v", j)
	}
}
