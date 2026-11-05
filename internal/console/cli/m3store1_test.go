package cli

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/config"
	"github.com/c2tech-sys/cryobackup/internal/console/jobs"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// M3-STORE1 C1: App.Run does not close the store under a manual job run. It
// cancels the run and waits for it (bounded), so the run's last write lands.
func TestServeShutdownWaitsForAManualJobRun(t *testing.T) { // M3-STORE1 C1
	cfg := config.Default()
	cfg.Resolve()
	cfg.Fixtures = true
	cfg.AdminListen = "127.0.0.1:0"
	app, err := Build(context.Background(), cfg, BuildOptions{FixturesPassword: "fixtures-pass-1"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)

	entered := make(chan struct{})
	var lastWrite atomic.Pointer[error]
	app.Runner.Add(jobs.Job{Name: "test-slow", Every: time.Hour, Lease: time.Hour, Run: func(ctx context.Context) error {
		close(entered)
		<-ctx.Done() // never, on the old code: the run's context outlived shutdown
		time.Sleep(50 * time.Millisecond)
		err := app.Store.Write(context.Background(), func(*store.Tx) error { return nil })
		lastWrite.Store(&err)
		return nil
	}})

	bg := context.Background()
	admin, err := app.Store.GetAdminByUsername(bg, "admin")
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := app.Auth.CreateAPIToken(bg, &auth.Principal{Admin: admin}, "shutdown-test", auth.ScopeFull, 7, "api", "127.0.0.1")
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

	// The server comes up in the background: retry until it answers.
	req, _ := http.NewRequest("POST", "http://"+app.Server.AdminAddr()+"/api/v1/system/jobs/test-slow/run", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	var res *http.Response
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
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
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the job never started")
	}

	stop()
	select {
	case err := <-returned:
		if err != nil {
			t.Fatalf("App.Run = %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("App.Run did not return")
	}
	last := lastWrite.Load()
	if last == nil {
		t.Fatal("App.Run returned, and closed the store, before the manual job run ended")
	}
	if errors.Is(*last, store.ErrClosed) {
		t.Fatal("the manual job run's last write met a closed store")
	}
	if *last != nil {
		t.Fatalf("the manual job run's last write: %v", *last)
	}
}
