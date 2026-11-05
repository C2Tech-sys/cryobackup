package cli

import (
	"bytes"
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/config"
	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
	"github.com/c2tech-sys/cryobackup/internal/console/jobs"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
)

// lockedBuf is a log sink safe for the app's goroutines.
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// M3-STORE2 1, end to end on a real serve App with its prober. A manual job is
// stuck inside a store Write and a scheduled job is queued behind it, neither
// honouring its context. App.Run still returns inside shutdownWorstCase, having
// logged what it abandoned by class. The reviewer's run was "still blocked after 1m0s".
func TestServeShutdownIsBoundedBehindAStuckWrite(t *testing.T) { // M3-STORE2 1
	dir := t.TempDir()
	if code, out, errb := run(t, "", initArgs(dir, "--master-key-copy-confirmed")...); code != 0 {
		t.Fatalf("init: %d\n%s\n%s", code, out, errb)
	}
	cfg, err := config.Read(filepath.Join(dir, "config.json"), func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	cfg.InsecureHTTPLab, cfg.AgentListen, cfg.AdminListen = true, "127.0.0.1:0", "127.0.0.1:0"
	cfg.AdminpathSocket = filepath.Join(dir, "a.sock")
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	logs := &lockedBuf{}
	app, err := Build(context.Background(), cfg, BuildOptions{Backend: &fixtures.Backend{}, Log: logx.New(logs, logx.LevelDebug)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	holding, queued := make(chan struct{}), make(chan struct{})
	bg := context.Background()
	app.Runner.Add(jobs.Job{Name: "test-stuck-manual", Every: 24 * time.Hour, Lease: time.Hour, Run: func(context.Context) error {
		return app.Store.Write(bg, func(*store.Tx) error { close(holding); <-release; return nil })
	}})
	app.Runner.Add(jobs.Job{Name: "test-stuck-scheduled", Every: time.Second, Lease: time.Hour, Run: func(context.Context) error {
		close(queued) // it has its lease, and it runs once: it never gets past the Write below
		<-holding     // only once the writer is stuck, so the run-now request can write its audit row first
		return app.Store.Write(bg, func(*store.Tx) error { return nil })
	}})

	owner, err := app.Store.GetAdminByUsername(bg, "owner")
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := app.Auth.CreateAPIToken(bg, &auth.Principal{Admin: owner}, "shutdown-test", auth.ScopeFull, 7, "api", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Server.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(bg)
	defer stop()
	returned := make(chan error, 1)
	go func() { returned <- app.Run(ctx) }()

	select {
	case <-queued:
	case <-time.After(30 * time.Second):
		t.Fatal("the scheduled job never started")
	}
	if app.Server.AdminTLSEnabled() {
		t.Fatal("the test expects a plain listener")
	}
	req, _ := http.NewRequest("POST", "http://"+app.Server.AdminAddr()+"/api/v1/system/jobs/test-stuck-manual/run", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	var res *http.Response
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if res, err = http.DefaultClient.Do(req); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
	}
	res.Body.Close()
	if res.StatusCode != 202 {
		t.Fatalf("run-now = %d, want 202", res.StatusCode)
	}
	select {
	case <-holding:
	case <-time.After(30 * time.Second):
		t.Fatal("the manual job's Write never started")
	}

	began := time.Now()
	stop()
	bound := shutdownWorstCase + 20*time.Second // the worst case, and room for a loaded machine
	select {
	case err := <-returned:
		if err != nil {
			t.Fatalf("App.Run = %v", err)
		}
	case <-time.After(bound):
		t.Fatalf("App.Run still blocked after %v behind a stuck Write", bound)
	}
	t.Logf("App.Run returned %v after its context ended (worst case %v)", time.Since(began).Round(time.Second), shutdownWorstCase)

	got := logs.String()
	for _, want := range []string{"a job did not stop at shutdown", "background work abandoned at shutdown", `"class":"store_close_abandoned"`} {
		if !strings.Contains(got, want) {
			t.Errorf("no %q in the shutdown log:\n%s", want, got)
		}
	}
}
