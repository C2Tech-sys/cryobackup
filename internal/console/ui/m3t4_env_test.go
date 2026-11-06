package ui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminapi"
	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// M3-T4 test helpers. The pages read the admin API in process; for the tests
// that compare a page with GET /api/v1/*, the same admin API is also mounted on
// the test server, exactly as serve mounts it beside the UI.

// fakeJobs is a job runner that records what it was asked to run.
type fakeJobs struct {
	mu    sync.Mutex
	names []string
	ran   []string
	done  chan string
}

func newFakeJobs(names ...string) *fakeJobs {
	return &fakeJobs{names: names, done: make(chan string, 16)}
}

func (f *fakeJobs) Names() []string { return append([]string(nil), f.names...) }

func (f *fakeJobs) RunOnce(ctx context.Context, name string) (bool, error) {
	f.mu.Lock()
	f.ran = append(f.ran, name)
	f.mu.Unlock()
	f.done <- name
	return true, nil
}

func (f *fakeJobs) runs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ran...)
}

// waitRun waits for the runner to be asked once.
func (f *fakeJobs) waitRun(t *testing.T) string {
	t.Helper()
	select {
	case n := <-f.done:
		return n
	case <-time.After(5 * time.Second):
		t.Fatal("the job runner was never asked to run the job")
		return ""
	}
}

// newM3Env is newEnv with a fake job runner on the UI and the admin API mounted
// beside the UI on the test server.
func newM3Env(t *testing.T) (*env, *fakeJobs) {
	t.Helper()
	e := newEnv(t)
	jobs := newFakeJobs("alarm-evaluator", "session-expire", "htpasswd-sync")
	e.svc.Jobs = jobs
	pub := &adminapi.Service{Auth: e.svc.Auth, Store: e.st, Fleet: e.svc.Fleet, Backend: e.be, Clock: e.svc.Clock, Display: e.svc.Display, Jobs: jobs}
	mux := http.NewServeMux()
	e.svc.Register(muxReg{mux})
	pub.Register(muxReg{mux})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	e.srv = srv
	return e, jobs
}

// apiGet reads an admin API route as the signed-in session of e.
func (e *env) apiGet(path string, out any) {
	e.t.Helper()
	res, body := e.do("GET", path, nil)
	if res.StatusCode != 200 {
		e.t.Fatalf("GET %s: %d %.300s", path, res.StatusCode, body)
	}
	if err := json.Unmarshal([]byte(body), out); err != nil {
		e.t.Fatalf("GET %s: %v", path, err)
	}
}

// allBanners is every condition the console can raise, for the pages that must
// show them all: C01 to C16 of the fixtures, C00, every "b" not-checked
// warning and C11 at its critical severity.
func allBanners(now time.Time) []state.Banner {
	out := fixtures.Banners(now)
	for i := range out {
		if out[i].ID == "C11" {
			out[i].Severity = state.SeverityCritical
			out[i].Text = "Console database needs attention: the maintenance worker cannot read its database."
		}
	}
	warn := state.SeverityAlarm
	out = append(out, state.Banner{ID: "C00", Severity: warn, Text: "Console self-checks are not wired: the rows below cannot be called OK."})
	for _, b := range []struct{ id, what string }{{"C01b", "Gateway"}, {"C02b", "Storage"}, {"C03b", "Alert e-mail"}, {"C04b", "Agent API"}, {"C05b", "Certificate"},
		{"C06b", "Alerting"}, {"C07b", "Maintenance worker"}, {"C12b", "Master key"}, {"C14b", "Digest"}, {"C15b", "Storage quota"}, {"C16b", "Release manifest"}} {
		out = append(out, state.Banner{ID: b.id, Severity: warn, Text: b.what + " has not been checked."})
	}
	return out
}

// ingest adds runs to a device through the agent ingest path.
func (e *env) ingest(host string, runs ...store.RunInput) {
	e.t.Helper()
	res, err := e.st.AgentIngestRuns(context.Background(), e.dev[host], runs, e.now)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, r := range res {
		if r.Status != store.IngestCreated {
			e.t.Fatalf("ingest %s: %+v", host, r)
		}
	}
}

// ulid makes a 26-character run id from n (the agent's idempotency key).
func ulid(n int) string {
	const alpha = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	b := []byte("01J9Y4K3A0B1C2D3E4F5G6H7K0")
	for i := 25; n > 0 && i > 20; i-- {
		b[i] = alpha[n%32]
		n /= 32
	}
	return string(b)
}

func has(s string, subs ...string) bool {
	for _, x := range subs {
		if !strings.Contains(s, x) {
			return false
		}
	}
	return true
}
