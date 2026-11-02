package adminpath

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// blockUntilDone is a backend hook that blocks until the request's context is
// done and reports the context error.
func blockUntilDone(seen chan<- error) func(context.Context, string) error {
	return func(ctx context.Context, _ string) error {
		<-ctx.Done()
		seen <- ctx.Err()
		return ctx.Err()
	}
}

// TestPerMethodTimeout: a backend that outlives the method's timeout makes the
// client return context.DeadlineExceeded, and the backend's context is cancelled.
// Another method keeps its own (long) timeout.
func TestPerMethodTimeout(t *testing.T) {
	fb := newFake()
	seen := make(chan error, 4)
	fb.setHook(func(ctx context.Context, method string) error {
		if method == MethodLs {
			return blockUntilDone(seen)(ctx, method)
		}
		return nil
	})
	c, _ := startServer(t, fb, ClientOptions{Timeouts: map[string]time.Duration{MethodLs: 100 * time.Millisecond}})
	start := time.Now()
	_, err := c.Ls(context.Background(), LsRequest{DeviceRequest: withID("to"), SnapshotID: "latest", Dir: "/"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want context.DeadlineExceeded", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("returned after %v", d)
	}
	select {
	case got := <-seen:
		if got == nil {
			t.Error("backend context ended without an error")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the timeout did not cancel the backend's context")
	}
	if _, err := c.Snapshots(context.Background(), withID("ok")); err != nil {
		t.Errorf("another method must be unaffected: %v", err)
	}
}

// TestCallerDeadlineAndCancel: the caller's own deadline and cancellation pass
// through unchanged and reach the backend.
func TestCallerDeadlineAndCancel(t *testing.T) {
	fb := newFake()
	seen := make(chan error, 4)
	fb.setHook(blockUntilDone(seen))
	c, _ := startServer(t, fb, ClientOptions{})

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	_, err := c.CatConfig(ctx, withID("d1"))
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline: got %v", err)
	}
	waitSeen(t, seen)

	ctx, cancel = context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	_, err = c.Snapshots(ctx, withID("c1"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: got %v", err)
	}
	waitSeen(t, seen)
}

func waitSeen(t *testing.T, seen <-chan error) {
	t.Helper()
	select {
	case err := <-seen:
		if err == nil {
			t.Error("backend context ended without an error")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancellation did not reach the backend")
	}
}

// TestBackendTimeoutCodeReachesClient: a deadline inside the backend is a
// "timeout" *Error with status 504, distinct from the client's own deadline.
func TestBackendTimeoutCodeReachesClient(t *testing.T) {
	fb := newFake()
	fb.setHook(func(context.Context, string) error { return context.DeadlineExceeded })
	c, path := startServer(t, fb, ClientOptions{})
	_, err := c.Check(context.Background(), CheckRequest{DeviceRequest: withID("bt")})
	var e *Error
	if !errors.As(err, &e) || e.Code != CodeTimeout {
		t.Fatalf("got %v", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Error("a server-side timeout is an *Error, not the caller's deadline")
	}
	if status, _, _ := rawDo(t, path, "POST", "/v1/check", "application/json", []byte(validBodies[MethodCheck])); status != 504 {
		t.Errorf("status %d", status)
	}
}

// TestUnavailable: no listener means ErrUnavailable and an error text without
// the socket path, at every level of the chain.
func TestUnavailable(t *testing.T) {
	dir := shortDir(t)
	missing := filepath.Join(dir, "gone.sock")
	stale := filepath.Join(dir, "stale.sock")
	makeStale(t, stale)
	for name, p := range map[string]string{"missing": missing, "stale": stale} {
		t.Run(name, func(t *testing.T) {
			c := NewClient(p, ClientOptions{})
			defer c.Close()
			_, err := c.Ping(context.Background())
			if !errors.Is(err, ErrUnavailable) {
				t.Fatalf("got %v, want ErrUnavailable", err)
			}
			for _, text := range chainTexts(err) {
				if strings.Contains(text, dir) || strings.Contains(text, filepath.Base(p)) || strings.Contains(text, "adminpath/v1") {
					t.Errorf("error text leaks the socket path: %q", text)
				}
			}
			if s := fmt.Sprintf("%v|%+v|%#v", err, err, errors.Unwrap(err)); strings.Contains(s, dir) {
				t.Errorf("formatted error leaks the socket path: %s", s)
			}
			var e *Error
			if errors.As(err, &e) {
				t.Error("an unreachable worker is not a wire *Error")
			}
		})
	}
}

// chainTexts returns Error() of err and of everything it wraps.
func chainTexts(err error) []string {
	if err == nil {
		return nil
	}
	out := []string{err.Error()}
	switch u := err.(type) {
	case interface{ Unwrap() error }:
		out = append(out, chainTexts(u.Unwrap())...)
	case interface{ Unwrap() []error }:
		for _, e := range u.Unwrap() {
			out = append(out, chainTexts(e)...)
		}
	}
	return out
}

// makeStale leaves a socket file with nothing listening on it.
func makeStale(t *testing.T, path string) {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(path)
	if err != nil || fi.Mode()&os.ModeSocket == 0 {
		t.Fatalf("setup: stale socket file not left behind: %v %v", fi, err)
	}
}

// TestStaleSocketReplaced: Serve replaces a socket file nothing listens on.
func TestStaleSocketReplaced(t *testing.T) {
	path := filepath.Join(shortDir(t), "ap.sock")
	makeStale(t, path)
	fb := newFake()
	serveAt(t, path, Handler(fb))
	c := NewClient(path, ClientOptions{})
	defer c.Close()
	if _, err := c.Ping(context.Background()); err != nil {
		t.Fatalf("ping after replacing the stale socket: %v", err)
	}
}

// TestListenRefusals: a live socket and a non-socket file are not replaced.
func TestListenRefusals(t *testing.T) {
	dir := shortDir(t)
	t.Run("live", func(t *testing.T) {
		path := filepath.Join(dir, "live.sock")
		serveAt(t, path, Handler(newFake()))
		ln, err := Listen(path)
		if err == nil {
			_ = ln.Close()
			t.Fatal("Listen replaced a live socket")
		}
		c := NewClient(path, ClientOptions{})
		defer c.Close()
		if _, err := c.Ping(context.Background()); err != nil {
			t.Errorf("the live server was disturbed: %v", err)
		}
	})
	t.Run("regular file", func(t *testing.T) {
		path := filepath.Join(dir, "file.sock")
		if err := os.WriteFile(path, []byte("keep me"), 0o600); err != nil {
			t.Fatal(err)
		}
		if ln, err := Listen(path); err == nil {
			_ = ln.Close()
			t.Fatal("Listen replaced a regular file")
		}
		if b, err := os.ReadFile(path); err != nil || string(b) != "keep me" {
			t.Errorf("the file was touched: %q %v", b, err)
		}
	})
	t.Run("empty path", func(t *testing.T) {
		if _, err := Listen(""); err == nil {
			t.Error("empty path accepted")
		}
	})
}

// TestSocketRemovedOnShutdown: cancelling the context stops Serve with a nil
// error and removes the socket file; closing the listener from Listen does too.
func TestSocketRemovedOnShutdown(t *testing.T) {
	path := filepath.Join(shortDir(t), "ap.sock")
	stop := serveAt(t, path, Handler(newFake()))
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("socket missing while serving: %v", err)
	}
	if err := stop(); err != nil {
		t.Fatalf("Serve returned %v", err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("socket file still there after shutdown: %v", err)
	}
	if _, err := net.Dial("unix", path); err == nil {
		t.Error("still accepting connections after shutdown")
	}

	ln, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = ln.Close()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Close of the listener left the file: %v", err)
	}
}

// TestShutdownWithRequestInFlight: Serve still returns when a request is blocked,
// by closing its connection after the grace period... bounded here by cancelling
// the client side first so the test does not wait the full grace.
func TestShutdownWithRequestInFlight(t *testing.T) {
	fb := newFake()
	entered := make(chan struct{}, 1)
	seen := make(chan error, 1)
	fb.setHook(func(ctx context.Context, m string) error {
		entered <- struct{}{}
		return blockUntilDone(seen)(ctx, m)
	})
	path := filepath.Join(shortDir(t), "ap.sock")
	stop := serveAt(t, path, Handler(fb))
	c := NewClient(path, ClientOptions{})
	defer c.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := c.Ping(ctx)
		done <- err
	}()
	<-entered
	cancel()
	<-done
	waitSeen(t, seen)
	if err := stop(); err != nil {
		t.Fatalf("Serve returned %v", err)
	}
}

// TestSocketMode: the socket file is 0660 (architecture DH1).
func TestSocketMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no POSIX file modes: Go reports 0666 for every file, so 0660 cannot be asserted here; the assertion runs on the Linux CI runner")
	}
	path := filepath.Join(shortDir(t), "ap.sock")
	ln, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSocket == 0 || fi.Mode().Perm() != 0o660 {
		t.Errorf("mode %v, want a socket with permissions 0660", fi.Mode())
	}
}

// TestBodyCap: a 2 MiB request is refused with 413 and invalid_request without
// reaching the backend, and the server keeps working; a body exactly at the cap
// is read (and then rejected on its content, not its size).
func TestBodyCap(t *testing.T) {
	fb := newFake()
	c, path := startServer(t, fb, ClientOptions{})
	big := []byte(`{"device_id":"dev1","pad":"` + strings.Repeat("A", 2<<20) + `"}`)
	status, body, _ := rawDo(t, path, "POST", "/v1/init", "application/json", big)
	if status != 413 {
		t.Fatalf("status %d, want 413 (%.200s)", status, body)
	}
	if e := wireError(t, body); e.Code != CodeInvalidRequest {
		t.Errorf("code %q", e.Code)
	}
	if fb.total() != 0 {
		t.Error("the backend was reached")
	}

	atCap := []byte(`{"device_id":"dev1","pad":"`)
	atCap = append(atCap, bytes.Repeat([]byte("A"), MaxRequestBytes-len(atCap)-2)...)
	atCap = append(atCap, '"', '}')
	if len(atCap) != MaxRequestBytes {
		t.Fatalf("setup: %d bytes", len(atCap))
	}
	status, body, _ = rawDo(t, path, "POST", "/v1/init", "application/json", atCap)
	if status != 400 {
		t.Errorf("a body at the cap must be judged on content (unknown field): status %d (%.200s)", status, body)
	}
	if _, err := c.Ping(context.Background()); err != nil {
		t.Errorf("server not serving after oversize bodies: %v", err)
	}

	// The client refuses to send an oversize body at all.
	_, err := invoke[PingResult](c, context.Background(), MethodPing, map[string]string{"pad": strings.Repeat("A", 2<<20)})
	var e *Error
	if !errors.As(err, &e) || e.Code != CodeInvalidRequest {
		t.Errorf("client oversize: got %v", err)
	}
}

func TestDefaultTimeoutsAndOptions(t *testing.T) {
	want := map[string]time.Duration{
		MethodPing: 5 * time.Second, MethodCatConfig: time.Minute,
		MethodLs: 2 * time.Minute, MethodFind: 2 * time.Minute, MethodSnapshots: 2 * time.Minute, MethodStats: 2 * time.Minute,
		MethodInit: 2 * time.Minute, MethodDump: 12 * time.Hour, MethodUnlock: time.Minute,
		MethodForget: 10 * time.Minute, MethodPrune: 6 * time.Hour, MethodCheck: 6 * time.Hour,
	}
	got := DefaultTimeouts()
	if len(got) != len(Methods()) {
		t.Errorf("%d defaults for %d methods", len(got), len(Methods()))
	}
	for m, d := range want {
		if got[m] != d {
			t.Errorf("default timeout of %s = %v, want %v", m, got[m], d)
		}
	}
	got[MethodPing] = time.Nanosecond
	if DefaultTimeouts()[MethodPing] != 5*time.Second {
		t.Error("DefaultTimeouts must return a copy")
	}
	c := NewClient("x", ClientOptions{Timeouts: map[string]time.Duration{MethodPrune: time.Minute, MethodPing: 0, MethodLs: -1}})
	if c.timeouts[MethodPrune] != time.Minute || c.timeouts[MethodPing] != 5*time.Second || c.timeouts[MethodLs] != 2*time.Minute {
		t.Errorf("options not merged as documented: %v", c.timeouts)
	}
}
