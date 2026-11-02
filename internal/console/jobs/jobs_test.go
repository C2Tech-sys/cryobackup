package jobs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/enroll"
	"github.com/c2tech-sys/cryobackup/internal/console/escrow"
	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
	"github.com/c2tech-sys/cryobackup/internal/console/gateway"
	"github.com/c2tech-sys/cryobackup/internal/console/restores"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

type env struct {
	st  *store.Store
	now time.Time
	dir string
	mk  *escrow.MasterKey
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{now: fixtures.Now(), dir: t.TempDir()}
	st, err := store.OpenMemory(context.Background(), func() time.Time { return e.now })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := store.LoadFixtures(context.Background(), st, e.now); err != nil {
		t.Fatal(err)
	}
	e.st = st
	e.mk, _ = escrow.NewMasterKey(bytes.Repeat([]byte{5}, 32))
	return e
}

func (e *env) sync() *HtpasswdSync {
	return &HtpasswdSync{Store: e.st, Master: e.mk, Dir: e.dir}
}

func (e *env) enrollOne(t *testing.T, nonce string) store.Device {
	t.Helper()
	ctx := context.Background()
	cs, _ := e.st.ListCustomers(ctx, store.ScopeAll())
	var cust store.Customer
	for _, c := range cs {
		if c.Name == "Northwind Dental" {
			cust = c
		}
	}
	plain, _, err := enroll.Mint(ctx, e.st, store.ScopeAll(), cust.ID, enroll.MintOptions{})
	if err != nil {
		t.Fatal(err)
	}
	hash, err := gateway.HashPassword(strings.Repeat("ab", 32))
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.st.AgentEnroll(ctx, store.EnrollInput{TokenHash: enroll.HashToken(plain), Nonce: nonce, Hostname: "NEW-" + nonce[:4],
		DeviceTokenHash: strings.Repeat("c", 64), GatewayPasswordHash: hash, GatewaySecretWrapped: []byte("w"),
		RepoKeyWrapped: []byte("k"), WrapAlg: escrow.WrapAlg, MasterKeyID: e.mk.KeyID()})
	if err != nil {
		t.Fatal(err)
	}
	return res.Device
}

func readFile(t *testing.T, dir string) (string, []gateway.Login, int64) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, gateway.FileName))
	if err != nil {
		t.Fatal(err)
	}
	gen, _, ls, err := gateway.Parse(b)
	if err != nil {
		t.Fatalf("deployed file must parse: %v", err)
	}
	return string(b), ls, gen
}

func probeLine(ls []gateway.Login) string {
	for _, l := range ls {
		if l.Username == gateway.ProbeUsername {
			return l.Hash
		}
	}
	return ""
}

func TestHtpasswdSyncRendersAndReusesTheProbeLine(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	j := e.sync()
	if err := j.Run(ctx); err != nil {
		t.Fatal(err)
	}
	first, ls, gen := readFile(t, e.dir)
	entries, _ := e.st.ListGatewayLoginsForHtpasswd(ctx)
	if gen != 1 || len(ls) != len(entries)+1 {
		t.Fatalf("generation %d, %d lines for %d logins", gen, len(ls), len(entries))
	}
	probePlain, _ := gateway.ProbePassword(e.mk.Bytes())
	probe := probeLine(ls)
	if !gateway.CheckPassword(probe, probePlain) {
		t.Fatal("the probe line must verify against the derived probe password")
	}
	// A second run changes nothing, byte for byte: the probe hash is reused.
	if err := j.Run(ctx); err != nil {
		t.Fatal(err)
	}
	second, _, gen2 := readFile(t, e.dir)
	if second != first || gen2 != 1 {
		t.Fatalf("a no-op run must leave the file byte-identical (gen %d)", gen2)
	}
	// 30 more runs, still identical (the reconcile never sees drift forever).
	for i := 0; i < 30; i++ {
		if err := j.Run(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if again, _, _ := readFile(t, e.dir); again != first {
		t.Fatal("reconcile churned the file")
	}
	// A new login: file rewritten at the next generation with the SAME probe line.
	dev := e.enrollOne(t, strings.Repeat("ab", 16))
	if err := j.Run(ctx); err != nil {
		t.Fatal(err)
	}
	third, ls3, gen3 := readFile(t, e.dir)
	if gen3 != 2 || probeLine(ls3) != probe || !strings.Contains(third, dev.ID+":") {
		t.Fatalf("new login not synced or probe re-hashed (gen %d)", gen3)
	}
	// Synced stamp set on the login.
	var synced int64
	_ = e.st.Read(ctx, func(tx *store.Tx) error {
		return tx.QueryRow(`SELECT COALESCE(htpasswd_synced_at,0) FROM gateway_logins WHERE device_id = ?`, dev.ID).Scan(&synced)
	})
	if synced == 0 {
		t.Fatal("htpasswd_synced_at not set")
	}
	// Retire: the login leaves the file.
	if _, err := e.st.UpdateDevice(ctx, store.ScopeAll(), dev.ID, store.DeviceUpdate{Retire: &store.RetireSpec{Reason: "test"}}); err != nil {
		t.Fatal(err)
	}
	if err := j.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if fourth, _, gen4 := readFile(t, e.dir); strings.Contains(fourth, dev.ID) || gen4 != 3 {
		t.Fatalf("retired login still deployed (gen %d)", gen4)
	}
}

func TestHtpasswdSyncRepairsDriftAndAuditsIt(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	j := e.sync()
	if err := j.Run(ctx); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(e.dir, gateway.FileName)
	good, _ := os.ReadFile(path)
	if err := os.WriteFile(path, []byte(strings.Replace(string(good), "\n", "\nhacked:$2y$10$"+strings.Repeat("a", 53)+"\n", 1)), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := j.Run(ctx); err != nil {
		t.Fatalf("drift must be repaired: %v", err)
	}
	fixed, _, gen := readFile(t, e.dir)
	if strings.Contains(fixed, "hacked") || gen != 2 {
		t.Fatalf("not repaired (gen %d)", gen)
	}
	evs, _ := e.st.ListAudit(ctx, store.ScopeAll(), store.AuditFilter{Action: "gateway.htpasswd.drift"}, store.Page{Limit: 5})
	if len(evs.Items) != 1 {
		t.Fatalf("expected one drift audit row, got %d", len(evs.Items))
	}
	// An ordinary change (a new login) is not drift.
	e.enrollOne(t, strings.Repeat("cd", 16))
	if err := j.Run(ctx); err != nil {
		t.Fatal(err)
	}
	evs, _ = e.st.ListAudit(ctx, store.ScopeAll(), store.AuditFilter{Action: "gateway.htpasswd.drift"}, store.Page{Limit: 5})
	if len(evs.Items) != 1 {
		t.Fatalf("a normal change must not be audited as drift: %d", len(evs.Items))
	}
	// A missing file is recreated.
	_ = os.Remove(path)
	if err := j.Run(ctx); err != nil {
		t.Fatal(err)
	}
	readFile(t, e.dir) // fails the test unless a parseable file is back
}

func TestHtpasswdSyncRefusesANewerGeneration(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	j := e.sync()
	if err := j.Run(ctx); err != nil {
		t.Fatal(err)
	}
	// Another console deploys generation 99 with different content.
	probe, _ := gateway.HashPassword("x")
	other := gateway.Render([]gateway.Login{{Username: "01J9ZZ0000000000000000OTHR", Hash: "$2y$10$" + strings.Repeat("b", 53)}}, probe, 99)
	if err := gateway.Sync(e.dir, other); err != nil {
		t.Fatal(err)
	}
	err := j.Run(ctx)
	var ce *ClassError
	if !errors.Is(err, gateway.ErrNewerGeneration) || !errors.As(err, &ce) || ce.Class != ClassNewerGen {
		t.Fatalf("want newer-generation error, got %v", err)
	}
	if after, _ := os.ReadFile(filepath.Join(e.dir, gateway.FileName)); !bytes.Equal(after, other) {
		t.Fatal("the newer file must not be overwritten")
	}
	evs, _ := e.st.ListAudit(ctx, store.ScopeAll(), store.AuditFilter{Action: "gateway.htpasswd.newer_generation"}, store.Page{Limit: 5})
	if len(evs.Items) != 1 || evs.Items[0].Severity != "critical" {
		t.Fatalf("expected a critical audit row: %+v", evs.Items)
	}
	if strings.Contains(err.Error(), e.dir) {
		t.Fatal("error text must not carry the path")
	}
}

func TestHtpasswdSyncNeedsTheMasterKeyAndRehashesAfterAKeyChange(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	noKey := &HtpasswdSync{Store: e.st, Dir: e.dir}
	var ce *ClassError
	if err := noKey.Run(ctx); !errors.As(err, &ce) || ce.Class != ClassMasterKey {
		t.Fatalf("no master key: %v", err)
	}
	j := e.sync()
	if err := j.Run(ctx); err != nil {
		t.Fatal(err)
	}
	_, ls, _ := readFile(t, e.dir)
	oldProbe := probeLine(ls)
	e.mk, _ = escrow.NewMasterKey(bytes.Repeat([]byte{6}, 32))
	if err := e.sync().Run(ctx); err != nil {
		t.Fatal(err)
	}
	_, ls, gen := readFile(t, e.dir)
	plain, _ := gateway.ProbePassword(e.mk.Bytes())
	if probeLine(ls) == oldProbe || !gateway.CheckPassword(probeLine(ls), plain) || gen != 2 {
		t.Fatal("a master-key change must re-hash the probe line")
	}
}

func TestRunnerLeasesRecordsAndIsolatesPanics(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	r := NewRunner(e.st, "holder-a", nil)
	var runs atomic.Int32
	r.Add(Job{Name: "ok-job", Every: time.Minute, Run: func(context.Context) error { runs.Add(1); return nil }})
	r.Add(Job{Name: "bad-job", Every: time.Minute, Run: func(context.Context) error { return &ClassError{Class: ClassIO, Err: errors.New("C:\\secret\\path")} }})
	r.Add(Job{Name: "panic-job", Every: time.Minute, Run: func(context.Context) error { panic("boom C:\\x") }})

	if ran, err := r.RunOnce(ctx, "ok-job"); !ran || err != nil || runs.Load() != 1 {
		t.Fatalf("ok-job: %v %v", ran, err)
	}
	st, _ := e.st.GetJobState(ctx, "ok-job")
	if st.LastStatus != "ok" || st.RunCount != 1 || st.NextDueAt == nil || st.LeaseHolder != "" {
		t.Fatalf("job_state: %+v", st)
	}
	if _, err := r.RunOnce(ctx, "bad-job"); err == nil {
		t.Fatal("expected the error back")
	}
	st, _ = e.st.GetJobState(ctx, "bad-job")
	if st.LastStatus != "error" || st.LastErrorClass != ClassIO || strings.Contains(st.LastErrorClass, "secret") {
		t.Fatalf("bad-job state: %+v", st)
	}
	if _, err := r.RunOnce(ctx, "panic-job"); err == nil {
		t.Fatal("panic must surface as an error")
	}
	st, _ = e.st.GetJobState(ctx, "panic-job")
	if st.LastStatus != "error" || st.LastErrorClass != ClassPanic {
		t.Fatalf("panic-job state: %+v", st)
	}
	// Another holder owns the lease: the job is skipped, not run.
	if got, _ := e.st.AcquireJobLease(ctx, "ok-job", "holder-b", time.Minute); !got {
		t.Fatal("holder-b lease")
	}
	if ran, err := r.RunOnce(ctx, "ok-job"); ran || err != nil || runs.Load() != 1 {
		t.Fatalf("leased elsewhere: ran=%v err=%v", ran, err)
	}
	// After the lease expires it runs again.
	e.now = e.now.Add(2 * time.Minute)
	if ran, _ := r.RunOnce(ctx, "ok-job"); !ran || runs.Load() != 2 {
		t.Fatal("expired lease must not block")
	}
	if _, err := r.RunOnce(ctx, "nope"); err == nil {
		t.Fatal("unknown job")
	}
}

func TestRunnerLoopRunsOnScheduleAndOnSignal(t *testing.T) {
	e := newEnv(t)
	r := NewRunner(e.st, "h", nil)
	var sig Signal
	var runs atomic.Int32
	r.Add(Job{Name: "tick", Every: 40 * time.Millisecond, Signal: sig.C(), Run: func(context.Context) error { runs.Add(1); return nil }})
	r.Add(Job{Name: "only-on-signal", Every: time.Hour, Signal: (&Signal{}).C(), Run: func(context.Context) error { return nil }})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.Start(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for runs.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if runs.Load() < 3 {
		t.Fatalf("scheduled runs: %d", runs.Load())
	}
	before := runs.Load()
	sig.Notify()
	sig.Notify()
	for runs.Load() <= before && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if runs.Load() <= before {
		t.Fatal("signal must run the job early")
	}
	r.Stop()
	after := runs.Load()
	time.Sleep(120 * time.Millisecond)
	if runs.Load() != after {
		t.Fatal("Stop must end the loops")
	}
	if got := r.Names(); len(got) != 2 || got[0] != "tick" {
		t.Fatalf("names: %v", got)
	}
}

func TestHeartbeatTrimAndSessionExpireJobs(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	var n0 int
	count := func() int {
		var n int
		_ = e.st.Read(ctx, func(tx *store.Tx) error { return tx.QueryRow(`SELECT count(*) FROM heartbeats`).Scan(&n) })
		return n
	}
	n0 = count()
	if n0 == 0 {
		t.Fatal("fixtures must carry heartbeat rows")
	}
	// Move the clock two days on: every ring row is older than 24 h.
	e.now = e.now.Add(48 * time.Hour)
	if err := (&HeartbeatRingTrim{Store: e.st}).Run(ctx); err != nil {
		t.Fatal(err)
	}
	if count() != 0 {
		t.Fatalf("ring not trimmed: %d of %d", count(), n0)
	}
	if err := (&SessionExpire{Store: e.st}).Job().Run(ctx); err != nil {
		t.Fatal(err)
	}
	if (&HeartbeatRingTrim{}).Job().Name != NameHeartbeatRingTrim || (&SessionExpire{}).Job().Name != NameSessionExpire ||
		(&RestorePurge{Svc: &restores.Service{}}).Job().Name != NameRestorePurge {
		t.Fatal("job names")
	}
}
