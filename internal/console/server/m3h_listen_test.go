package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/config"
	"github.com/c2tech-sys/cryobackup/internal/console/httpx"
	"github.com/c2tech-sys/cryobackup/internal/logx"
)

// syncBuf is a log sink the test reads while the server writes to it.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// fullRig is a Server at a low max_conns whose every slot on the named listeners
// is held by an open stream: a request whose handler blocks until the test ends,
// which is what an SSE stream or a long poll is to the listener.
type fullRig struct {
	s       *Server
	logs    *syncBuf
	release chan struct{}
}

const rigMaxConns = 2

func newFullRig(t *testing.T) *fullRig {
	t.Helper()
	cfg := config.Default()
	cfg.Resolve()
	cfg.AdminListen, cfg.AgentListen, cfg.InsecureHTTPLab = "127.0.0.1:0", "127.0.0.1:0", true
	cfg.RateLimits.MaxConns = rigMaxConns
	rig := &fullRig{logs: &syncBuf{}, release: make(chan struct{})}
	t.Cleanup(func() { close(rig.release) })
	hold := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "open\n")
		w.(http.Flusher).Flush()
		<-rig.release // deliberately deaf to the request context: the worst stream there is
	})
	s, err := New(cfg, Options{
		Log:           logx.New(rig.logs, logx.LevelDebug),
		RegisterAdmin: func(r httpx.Registrar) { r.Handle("GET /hold", hold) },
		RegisterAgent: func(r httpx.Registrar) { r.Handle("GET /hold", hold) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Listen(); err != nil {
		t.Fatal(err)
	}
	rig.s = s
	return rig
}

// fill opens max_conns streams on addr and returns once they all answered and the
// listener's accept loop has taken its next attempt at a slot.
func (r *fullRig) fill(t *testing.T, addr string, ln net.Listener) {
	t.Helper()
	for i := 0; i < rigMaxConns; i++ {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		if _, err := fmt.Fprintf(c, "GET /hold HTTP/1.1\r\nHost: t\r\n\r\n"); err != nil {
			t.Fatal(err)
		}
		_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
		buf := make([]byte, 64)
		if _, err := c.Read(buf); err != nil {
			t.Fatalf("stream %d did not open: %v", i, err)
		}
	}
	lim, ok := ln.(*limitListener)
	if !ok {
		t.Fatalf("listener is a %T, not the limiting listener", ln)
	}
	if got := len(lim.sem); got != rigMaxConns {
		t.Fatalf("slots held = %d, want %d", got, rigMaxConns)
	}
	time.Sleep(100 * time.Millisecond) // the accept loop reaches its wait for a slot
}

// waitServe bounds Serve by what Run's shutdownWorstCase counts for it.
func waitServe(t *testing.T, done <-chan error, logs *syncBuf) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(ShutdownGrace + 10*time.Second):
		t.Fatalf("Serve still blocked past ShutdownGrace with every slot held; log:\n%s", logs.String())
		return nil
	}
}

// M3-HARDEN A1: a clean cancel with every slot held by an open stream. Accept sits
// waiting for a slot; if closing the listener does not end that wait, Serve hangs
// on its WaitGroup with nothing in the log and the shutdown budget in cli/app.go is
// false.
func TestServeEndsOnCancelWithEverySlotHeld(t *testing.T) {
	t.Parallel()
	rig := newFullRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- rig.s.Serve(ctx) }()
	rig.fill(t, rig.s.AdminAddr(), rig.s.adminLn)
	rig.fill(t, rig.s.AgentAddr(), rig.s.agentLn)
	cancel()
	if err := waitServe(t, done, rig.logs); err != nil {
		t.Fatalf("a clean cancel returned %v", err)
	}
}

// M3-HARDEN A1, B (m06): either listener failing while the other is full ends Serve
// with an error that names it, in the error and in the log.
func TestServeEndsOnListenerFailureWithTheOtherFull(t *testing.T) {
	t.Parallel()
	for _, failing := range []string{"admin", "agent"} {
		t.Run(failing, func(t *testing.T) {
			t.Parallel()
			rig := newFullRig(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- rig.s.Serve(ctx) }()
			var victim net.Listener
			if failing == "admin" {
				victim = rig.s.adminLn
				rig.fill(t, rig.s.AgentAddr(), rig.s.agentLn)
			} else {
				victim = rig.s.agentLn
				rig.fill(t, rig.s.AdminAddr(), rig.s.adminLn)
			}
			if err := victim.Close(); err != nil {
				t.Fatal(err)
			}
			err := waitServe(t, done, rig.logs)
			if err == nil {
				t.Fatal("Serve returned nil after a listener failed")
			}
			if !strings.Contains(err.Error(), "the "+failing+" listener") {
				t.Errorf("error does not name the %s listener: %v", failing, err)
			}
			got := rig.logs.String()
			if !strings.Contains(got, `"class":"listener_failed"`) || !strings.Contains(got, `"listener":"`+failing+`"`) {
				t.Errorf("no listener_failed line naming %s:\n%s", failing, got)
			}
			other := "agent"
			if failing == "agent" {
				other = "admin"
			}
			if strings.Contains(got, `"listener":"`+other+`"`) {
				t.Errorf("the healthy %s listener is named as failed:\n%s", other, got)
			}
		})
	}
}

// fakeAcceptErr fails Accept the way a dead network stack does.
type fakeAcceptErr struct {
	net.Listener
	err error
}

func (f fakeAcceptErr) Accept() (net.Conn, error) { return nil, f.err }

func TestLimitListenerAcceptEndsOnCloseAndGivesTheSlotBack(t *testing.T) {
	t.Parallel()
	base, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln := newLimitListener(base, 1)
	lim := ln.(*limitListener)
	lim.sem <- struct{}{} // the only slot is taken
	ended := make(chan error, 1)
	go func() {
		_, err := ln.Accept()
		ended <- err
	}()
	time.Sleep(50 * time.Millisecond)
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-ended:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("Accept = %v, want net.ErrClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Accept stayed blocked on a slot after Close")
	}
	// Serve closes the listeners again after Shutdown closed them: that stays harmless.
	_ = ln.Close()
	<-lim.sem

	// A failing Accept gives its slot back.
	boom := errors.New("boom")
	failing := newLimitListener(fakeAcceptErr{Listener: base, err: boom}, 1).(*limitListener)
	if _, err := failing.Accept(); !errors.Is(err, boom) {
		t.Fatalf("Accept = %v", err)
	}
	if len(failing.sem) != 0 {
		t.Fatal("a failed Accept kept its slot")
	}
}
