package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/console/agentapi"
	consolecfg "github.com/c2tech-sys/cryobackup/internal/console/config"
	consoleenroll "github.com/c2tech-sys/cryobackup/internal/console/enroll"
	"github.com/c2tech-sys/cryobackup/internal/console/escrow"
	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
)

// realConsole is the real console half of the agent API (internal/console/
// agentapi on a real SQLite store in a temp directory, behind httptest), the
// opposite of fakeConsole: the M1-T5 tests use it where what matters is what
// the console stores. A recording layer in front of the handler keeps every
// request the agent sent and every response the console gave, and can lose a
// response after the console has committed.
type realConsole struct {
	t      *testing.T
	st     *store.Store
	srv    *httptest.Server
	dbPath string
	logs   *syncBuffer // the console's own log output
	token  string      // an enrollment token of the fixtures customer
	custID string

	// dropRuns is the number of POST /runs responses to lose: the console
	// handles the request to the end (the store is written), then the
	// connection is cut before a byte of the answer is sent.
	dropRuns atomic.Int32

	mu   sync.Mutex
	reqs []wireCall
}

// wireCall is one request the agent made and, unless it was lost, the answer.
type wireCall struct {
	Method, Path string
	Body         []byte // the request body, byte for byte
	Status       int    // 0 when the response was lost
	Resp         []byte
}

// syncBuffer is a bytes.Buffer safe for the logger's goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func newRealConsole(t *testing.T) *realConsole {
	t.Helper()
	ctx := context.Background()
	c := &realConsole{t: t, logs: &syncBuffer{}, dbPath: filepath.Join(t.TempDir(), "console.db")}
	st, err := store.Open(ctx, store.Options{Path: c.dbPath})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	c.st = st
	if _, err := st.CreateStorageTarget(ctx, store.StorageTarget{Name: "Lab storage", Provider: "s3_compatible", Endpoint: "s3.example.invalid",
		Bucket: "bkt", Prefix: "fleet", GatewayURL: "https://gateway.example.invalid:9443", CredentialRef: "CRYO_TEST_S3", IsDefault: true}); err != nil {
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
	c.custID = cust.ID
	c.token, _, err = consoleenroll.Mint(ctx, st, store.ScopeAll(), cust.ID, consoleenroll.MintOptions{Expires: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	mk, err := escrow.NewMasterKey(bytes.Repeat([]byte{7}, escrow.MasterKeyLen))
	if err != nil {
		t.Fatal(err)
	}
	limits := consolecfg.Default().RateLimits
	limits.HeartbeatIntervalS = 1 // the agents of these tests heartbeat about as often
	svc := &agentapi.Service{
		Store: st, Keys: &escrow.Keys{Master: mk, Store: st}, Backend: &fixtures.Backend{},
		GatewayURL: "https://gateway.example.invalid:9443", Clock: time.Now, Log: logx.New(c.logs, logx.LevelDebug),
		Limits: limits, HtpasswdSync: func() {},
	}
	mux := http.NewServeMux()
	svc.Register(muxRegistrar{mux})
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { c.serve(mux, w, r) }))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *realConsole) URL() string { return c.srv.URL }

// serve records the request and, for POST /runs, lets the console commit and
// then (while dropRuns lasts) cuts the connection instead of answering.
func (c *realConsole) serve(next http.Handler, w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(body))
	call := wireCall{Method: r.Method, Path: r.URL.Path, Body: body}

	if r.URL.Path == api.RouteRuns && r.Method == http.MethodPost {
		rec := httptest.NewRecorder()
		next.ServeHTTP(rec, r)
		lose := false
		for {
			n := c.dropRuns.Load()
			if n <= 0 {
				break
			}
			if c.dropRuns.CompareAndSwap(n, n-1) {
				lose = true
				break
			}
		}
		if lose {
			c.record(call)
			if hj, ok := w.(http.Hijacker); ok {
				if conn, _, err := hj.Hijack(); err == nil {
					_ = conn.Close()
				}
			}
			return
		}
		call.Status, call.Resp = rec.Code, rec.Body.Bytes()
		c.record(call)
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
		return
	}
	c.record(call)
	next.ServeHTTP(w, r)
}

func (c *realConsole) record(call wireCall) {
	c.mu.Lock()
	c.reqs = append(c.reqs, call)
	c.mu.Unlock()
}

// calls returns a copy of everything the agent sent so far.
func (c *realConsole) calls() []wireCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]wireCall(nil), c.reqs...)
}

// runPosts returns the POST /runs calls that carried the run id, in order.
func (c *realConsole) runPosts(id string) []wireCall {
	var out []wireCall
	for _, call := range c.calls() {
		if call.Path != api.RouteRuns {
			continue
		}
		var req api.RunsRequest
		if json.Unmarshal(call.Body, &req) != nil {
			continue
		}
		for _, r := range req.Runs {
			if r.ID == id {
				out = append(out, call)
				break
			}
		}
	}
	return out
}

// enrollAgent enrolls a fresh state directory with the real console.
func (c *realConsole) enrollAgent() (stateDir, deviceID string) {
	c.t.Helper()
	dir := filepath.Join(c.t.TempDir(), "state")
	res, err := Enroll(context.Background(), EnrollOptions{
		StateDir: dir, ServerURL: c.URL(), Token: c.token, InsecureLab: true, AllowUnprotectedState: true,
		InsecurePlaintextSecrets: plainSecrets(),
	})
	if err != nil {
		c.t.Fatalf("enroll with the real console: %v", err)
	}
	return dir, res.DeviceID
}

// runRow returns the console's stored row of a run.
func (c *realConsole) runRow(id string) (store.Run, error) {
	return c.st.GetRun(context.Background(), store.ScopeAll(), id)
}

// deviceRuns lists every stored run of the device.
func (c *realConsole) deviceRuns(deviceID string) []store.Run {
	c.t.Helper()
	l, err := c.st.ListRuns(context.Background(), store.ScopeAll(), deviceID, store.Page{Limit: 200})
	if err != nil {
		c.t.Fatal(err)
	}
	return l.Items
}
