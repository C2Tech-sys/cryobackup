package agent

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/agent/internal/fakerestic"
	"github.com/c2tech-sys/cryobackup/internal/console/agentapi"
	consoleenroll "github.com/c2tech-sys/cryobackup/internal/console/enroll"
	"github.com/c2tech-sys/cryobackup/internal/console/escrow"
	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

type muxRegistrar struct{ m *http.ServeMux }

func (r muxRegistrar) Handle(p string, h http.Handler) { r.m.Handle(p, h) }

// TestFleetCapEndToEnd runs the real console agent API (store, agentapi) and
// two real agents in process (ADR 0005): the storage target's cap is 1 and
// both devices are due at once (their first backup). One runs; the other is
// refused, journals deferred/fleet_cap and runs once the first run's closed
// upload has released the lease. The two backups never overlap.
func TestFleetCapEndToEnd(t *testing.T) {
	ctx := context.Background()
	st, err := store.OpenMemory(ctx, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	target, err := st.CreateStorageTarget(ctx, store.StorageTarget{Name: "Lab storage", Provider: "s3_compatible", Endpoint: "s3.example.invalid",
		Bucket: "bkt", Prefix: "fleet", GatewayURL: "https://gateway.example.invalid:9443", CredentialRef: "CRYO_TEST_S3", IsDefault: true,
		MaxConcurrentBackups: 1})
	if err != nil {
		t.Fatal(err)
	}
	sections, err := store.PolicySections(testPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreatePolicy(ctx, store.ScopeAll(), store.PolicyInput{Scope: "default", Name: "Default", Sections: sections}); err != nil {
		t.Fatal(err)
	}
	cust, err := st.CreateCustomer(ctx, store.ScopeAll(), store.Customer{Name: "Example Clinic", KeyCustody: "escrowed", Expected: true})
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := consoleenroll.Mint(ctx, st, store.ScopeAll(), cust.ID, consoleenroll.MintOptions{Expires: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	mk, err := escrow.NewMasterKey(bytes.Repeat([]byte{7}, escrow.MasterKeyLen))
	if err != nil {
		t.Fatal(err)
	}
	svc := &agentapi.Service{Store: st, Keys: &escrow.Keys{Master: mk, Store: st}, Backend: &fixtures.Backend{},
		GatewayURL: "https://gateway.example.invalid:9443", Clock: time.Now}
	mux := http.NewServeMux()
	svc.Register(muxRegistrar{mux})
	console := httptest.NewServer(mux)
	t.Cleanup(console.Close)

	fakerestic.Use(t, "slow") // about half a second per backup
	var dirs []string
	for i := 0; i < 2; i++ {
		dir := filepath.Join(t.TempDir(), "state")
		if _, err := Enroll(ctx, EnrollOptions{StateDir: dir, ServerURL: console.URL, Token: token, InsecureLab: true,
			AllowUnprotectedState: true, InsecurePlaintextSecrets: plainSecrets()}); err != nil {
			t.Fatalf("enroll %d: %v", i, err)
		}
		dirs = append(dirs, dir)
	}
	for _, dir := range dirs {
		o := fakeOptions(t, dir)
		o.leaseWait = func(time.Duration) time.Duration { return 700 * time.Millisecond }
		start(t, o)
	}

	type result struct {
		success  store.Run
		deferred int
	}
	devices := func() map[string]*result {
		out := map[string]*result{}
		l, err := st.ListRuns(ctx, store.ScopeAll(), "", store.Page{Limit: 200})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range l.Items {
			res := out[r.DeviceID]
			if res == nil {
				res = &result{}
				out[r.DeviceID] = res
			}
			switch {
			case r.Outcome == "success":
				res.success = r
			case r.Outcome == "deferred" && r.DeferredReason == "fleet_cap":
				res.deferred++
			}
		}
		return out
	}
	waitFor(t, "a successful backup on both devices", 60*time.Second, func() bool {
		d := devices()
		n := 0
		for _, r := range d {
			if r.success.ID != "" {
				n++
			}
		}
		return n == 2
	})
	got := devices()
	var runs []store.Run
	deferrals := 0
	for _, r := range got {
		runs = append(runs, r.success)
		deferrals += r.deferred
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].StartedAt.Before(runs[j].StartedAt) })
	first, second := runs[0], runs[1]
	if first.FinishedAt == nil || second.StartedAt.Before(*first.FinishedAt) {
		t.Fatalf("the two backups overlapped with a cap of 1: first %v-%v, second from %v", first.StartedAt, first.FinishedAt, second.StartedAt)
	}
	if got[second.DeviceID].deferred == 0 || got[first.DeviceID].deferred != 0 {
		t.Fatalf("deferrals: the first device %d, the second %d; want only the second deferred", got[first.DeviceID].deferred, got[second.DeviceID].deferred)
	}
	if deferrals < 1 {
		t.Fatal("no fleet_cap deferral reached the console")
	}
	for _, r := range runs {
		if r.Attempt != 1 {
			t.Errorf("device %s ran attempt %d: the deferral moved the retry ladder", r.DeviceID, r.Attempt)
		}
	}
	// Both closed uploads released their leases.
	if leases, err := st.ListStartLeases(ctx, target.ID, time.Now()); err != nil || len(leases) != 0 {
		t.Fatalf("leases left after both runs: %+v %v", leases, err)
	}
}
