package adminpath

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// TestDumpStreamsTheArchiveAsTheBody: the archive is the HTTP body
// (application/zip, chunked, no Content-Length) and the result is a trailer.
func TestDumpStreamsTheArchiveAsTheBody(t *testing.T) {
	fb := newFake()
	_, path := startServer(t, fb, ClientOptions{})
	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}}}
	resp, err := hc.Post("http://x/v1/dump", "application/json", strings.NewReader(validBodies[MethodDump]))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/zip" || resp.ContentLength != -1 {
		t.Fatalf("status %d, type %q, length %d", resp.StatusCode, resp.Header.Get("Content-Type"), resp.ContentLength)
	}
	if !bytes.Equal(body, dumpPayload) {
		t.Fatal("the body is not the streamed archive")
	}
	tr := resp.Trailer.Get(TrailerDumpResult)
	if !strings.Contains(tr, `"result"`) || !strings.Contains(tr, `"sha256"`) || strings.Contains(tr, `"error"`) {
		t.Fatalf("trailer: %q", tr)
	}
}

// TestDumpFailureBeforeTheFirstByte is an ordinary JSON error.
func TestDumpFailureBeforeTheFirstByte(t *testing.T) {
	fb := newFake()
	c, _ := startServer(t, fb, ClientOptions{})
	fb.setHook(func(context.Context, string) error { return NewError(CodeWrongPassword, restic.ClassWrongPassword) })
	var buf bytes.Buffer
	_, err := c.Dump(context.Background(), DumpRequest{DeviceRequest: withID("d1"), SnapshotID: "latest", Path: "/a"}, &buf)
	var e *Error
	if !errors.As(err, &e) || e.Code != CodeWrongPassword || e.Class != restic.ClassWrongPassword {
		t.Fatalf("got %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("%d bytes reached the destination", buf.Len())
	}
}

// TestDumpFailureMidStream: the error arrives in the trailer and Client returns
// it; the partial bytes are never reported as a result.
func TestDumpFailureMidStream(t *testing.T) {
	fb := newFake()
	c, _ := startServer(t, fb, ClientOptions{})
	fb.mu.Lock()
	fb.dumpAfter = NewError(CodeStorageUnreachable, restic.ClassNetwork)
	fb.mu.Unlock()
	var buf bytes.Buffer
	res, err := c.Dump(context.Background(), DumpRequest{DeviceRequest: withID("d2"), SnapshotID: "latest", Path: "/a"}, &buf)
	var e *Error
	if !errors.As(err, &e) || e.Code != CodeStorageUnreachable || e.Class != restic.ClassNetwork {
		t.Fatalf("got %+v %v", res, err)
	}
	if buf.Len() != len(dumpPayload) {
		t.Fatalf("the partial archive was not passed through: %d bytes", buf.Len())
	}
}

// TestDumpWithoutTrailerIsAFailure: a body that ends without the result trailer
// (the worker died, a proxy cut it) is never a complete archive.
func TestDumpWithoutTrailerIsAFailure(t *testing.T) {
	path := filepath.Join(shortDir(t), "ap.sock")
	ln, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write(dumpPayload)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	c := NewClient(path, ClientOptions{})
	defer c.Close()
	_, err = c.Dump(context.Background(), DumpRequest{DeviceRequest: withID("d3"), SnapshotID: "latest", Path: "/a"}, io.Discard)
	var e *Error
	if !errors.As(err, &e) || e.Code != CodeInternal {
		t.Fatalf("got %v, want internal", err)
	}
}

// failingWriter accepts n bytes, then fails.
type failingWriter struct{ left int }

func (f *failingWriter) Write(p []byte) (int, error) {
	if len(p) > f.left {
		n := f.left
		f.left = 0
		return n, errors.New("client went away 203.0.113.9")
	}
	f.left -= len(p)
	return len(p), nil
}

// TestDumpDestinationFailureCancelsTheWorker: when the destination stops
// accepting data, Client returns ErrStreamWrite (with no text from the
// destination) and the worker's request context is cancelled, so restic stops.
func TestDumpDestinationFailureCancelsTheWorker(t *testing.T) {
	fb := newFake()
	c, _ := startServer(t, fb, ClientOptions{})
	block := make(chan struct{})
	cancelled := make(chan struct{})
	fb.mu.Lock()
	fb.dumpBlock = block
	fb.mu.Unlock()
	fb.setHook(func(ctx context.Context, _ string) error {
		go func() { <-ctx.Done(); close(cancelled) }()
		return nil
	})
	_, err := c.Dump(context.Background(), DumpRequest{DeviceRequest: withID("d4"), SnapshotID: "latest", Path: "/a"}, &failingWriter{left: 1000})
	if !errors.Is(err, ErrStreamWrite) || strings.Contains(err.Error(), "203.0.113.9") {
		t.Fatalf("got %v, want ErrStreamWrite without the destination's text", err)
	}
	select {
	case <-cancelled:
	case <-time.After(10 * time.Second):
		t.Fatal("the worker's request was not cancelled when the destination failed")
	}
}
