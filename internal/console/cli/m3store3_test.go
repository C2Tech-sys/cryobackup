package cli

import (
	"context"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/c2tech-sys/cryobackup/internal/console/config"
	"github.com/c2tech-sys/cryobackup/internal/console/server"
	"github.com/c2tech-sys/cryobackup/internal/logx"
)

// adminListener reaches the server's bound admin listener, to break it from outside
// as a failing network stack would. The server has no API for that, and should not.
func adminListener(t *testing.T, s *server.Server) net.Listener {
	t.Helper()
	f := reflect.ValueOf(s).Elem().FieldByName("adminLn")
	if !f.IsValid() {
		t.Fatal("server.Server has no field adminLn: update this test")
	}
	ln, ok := reflect.NewAt(f.Type(), unsafe.Pointer(f.UnsafeAddr())).Elem().Interface().(net.Listener)
	if !ok || ln == nil {
		t.Fatal("the admin listener is not bound")
	}
	return ln
}

// M3-STORE3 1: a listener that fails must not leave the console alive and silent,
// serving nothing and running no jobs. Run ends with no help from its context, logs
// the failure by class, and answers an error, so serve exits non-zero.
func TestServeListenerFailureEndsRunWithAnError(t *testing.T) { // M3-STORE3 1
	cfg := config.Default()
	cfg.Resolve()
	cfg.Fixtures = true
	cfg.AdminListen = "127.0.0.1:0"
	logs := &lockedBuf{}
	app, err := Build(context.Background(), cfg, BuildOptions{FixturesPassword: "fixtures-pass-1", Log: logx.New(logs, logx.LevelDebug)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	if err := app.Server.Listen(); err != nil {
		t.Fatal(err)
	}
	ln := adminListener(t, app.Server)

	ctx, stop := context.WithCancel(context.Background()) // stays live: only the listener fails
	returned := make(chan error, 1)
	runEnded := make(chan struct{})
	go func() { defer close(runEnded); returned <- app.Run(ctx) }()
	t.Cleanup(func() { stop(); <-runEnded }) // before app.Close (cleanups are LIFO)

	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	bound := shutdownWorstCase + 20*time.Second
	select {
	case err := <-returned:
		if err == nil {
			t.Fatal("App.Run = nil after a listener failed; serve would exit 0 and nothing would restart it")
		}
		if ctx.Err() != nil {
			t.Fatalf("the test's context ended: %v", ctx.Err())
		}
	case <-time.After(bound):
		t.Fatalf("App.Run still blocked %v after a listener failed; log:\n%s", bound, logs.String())
	}
	if got := logs.String(); !strings.Contains(got, `"class":"listener_failed"`) {
		t.Fatalf("no listener_failed line in the log:\n%s", got)
	}
}
