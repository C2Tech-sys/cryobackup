package adminpath

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// sentinel is a PHI-looking string. No error body, error text or log-able value
// may ever contain it.
const sentinel = `C:\fixture\PHI-sentinel`

var fixTime = time.Date(2026, 10, 22, 12, 30, 0, 0, time.UTC)

func fixMeta() ResultMeta {
	return ResultMeta{
		Elapsed:        1500 * time.Millisecond,
		ExitCode:       3,
		Outcome:        restic.OutcomePartial,
		DeferredReason: "",
		ErrorClass:     restic.ClassStorage,
	}
}

func fixSnapshot() restic.Snapshot {
	return restic.Snapshot{
		ID:       strings.Repeat("ab", 32),
		ShortID:  "abababab",
		Time:     fixTime,
		Parent:   strings.Repeat("cd", 32),
		Tree:     strings.Repeat("ef", 32),
		Paths:    []string{"/C/Users/fixture"},
		Hostname: "fixture-host",
		Username: "fixture-user",
		Tags:     []string{"keep"},
	}
}

func fixNode() restic.Node {
	return restic.Node{
		Name: "report.docx", Type: "file", Path: "/C/Users/fixture/report.docx",
		Size: 12345, Mode: 0o644, Permissions: "-rw-r--r--",
		Mtime: fixTime, Atime: fixTime, Ctime: fixTime, UID: 1000, GID: 1001, Inode: 77,
	}
}

func fixLs() LsResult {
	return LsResult{
		ResultMeta: fixMeta(),
		Snapshot: restic.LsSnapshot{
			ID: strings.Repeat("ab", 32), ShortID: "abababab", Time: fixTime,
			Paths: []string{"/C/Users/fixture"}, Hostname: "fixture-host", Tags: []string{"keep"},
		},
		Nodes:     []restic.Node{fixNode()},
		Truncated: true,
	}
}

func fixFind() []restic.FindResult {
	return []restic.FindResult{{
		Hits: 1, Snapshot: strings.Repeat("ab", 32),
		Matches: []restic.FindMatch{{Path: "/C/Users/fixture/report.docx", Name: "report.docx", Type: "file",
			Permissions: "-rw-r--r--", Size: 12345, Mtime: fixTime, Atime: fixTime, Ctime: fixTime}},
	}}
}

func fixForget() ForgetResult {
	s := fixSnapshot()
	return ForgetResult{
		ResultMeta: fixMeta(),
		Groups: []restic.ForgetGroup{{
			Tags: []string{"keep"}, Host: "fixture-host", Paths: []string{"/C/Users/fixture"},
			Keep: []restic.Snapshot{s}, Remove: []restic.Snapshot{s},
			Reasons: []restic.ForgetReason{{Snapshot: s, Matches: []string{"daily snapshot"}}},
		}},
	}
}

// fakeBackend records every request and answers with canned results built from
// internal/restic types. hook, when set, runs first and may block, panic or
// return an error.
type fakeBackend struct {
	mu    sync.Mutex
	calls map[string]int
	reqs  map[string]any
	hook  func(ctx context.Context, method string) error
	// dumpAfter, when set, is the error Dump returns after streaming dumpPayload.
	dumpAfter error
	// dumpBlock, when set, makes Dump stream until its writer fails; it is
	// closed once the first payload went out.
	dumpBlock chan struct{}
}

func newFake() *fakeBackend {
	return &fakeBackend{calls: map[string]int{}, reqs: map[string]any{}}
}

func (f *fakeBackend) enter(ctx context.Context, method string, req any) error {
	f.mu.Lock()
	f.calls[method]++
	f.reqs[method] = req
	hook := f.hook
	f.mu.Unlock()
	if hook != nil {
		return hook(ctx, method)
	}
	return nil
}

func (f *fakeBackend) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		n += c
	}
	return n
}

func (f *fakeBackend) last(method string) any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reqs[method]
}

func (f *fakeBackend) count(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[method]
}

func (f *fakeBackend) setHook(h func(ctx context.Context, method string) error) {
	f.mu.Lock()
	f.hook = h
	f.mu.Unlock()
}

func fixPing() PingResult {
	yes := true
	return PingResult{Version: "v1.2.3", Uptime: 90 * time.Second, MasterKeyLoaded: true, StorageReachable: &yes}
}

func (f *fakeBackend) Ping(ctx context.Context) (PingResult, error) {
	if err := f.enter(ctx, MethodPing, nil); err != nil {
		return PingResult{}, err
	}
	return fixPing(), nil
}

func (f *fakeBackend) Init(ctx context.Context, req InitRequest) (InitResult, error) {
	if err := f.enter(ctx, MethodInit, req); err != nil {
		return InitResult{}, err
	}
	return InitResult{ResultMeta: fixMeta(), RepositoryID: strings.Repeat("12", 32), Created: true}, nil
}

func (f *fakeBackend) CatConfig(ctx context.Context, req DeviceRequest) (restic.Config, error) {
	if err := f.enter(ctx, MethodCatConfig, req); err != nil {
		return restic.Config{}, err
	}
	return restic.Config{Version: 2, ID: strings.Repeat("34", 32), ChunkerPolynomial: "3da3358b4dc173"}, nil
}

func (f *fakeBackend) Ls(ctx context.Context, req LsRequest) (LsResult, error) {
	if err := f.enter(ctx, MethodLs, req); err != nil {
		return LsResult{}, err
	}
	return fixLs(), nil
}

func (f *fakeBackend) Find(ctx context.Context, req FindRequest) ([]restic.FindResult, error) {
	if err := f.enter(ctx, MethodFind, req); err != nil {
		return nil, err
	}
	return fixFind(), nil
}

func (f *fakeBackend) Snapshots(ctx context.Context, req DeviceRequest) ([]restic.Snapshot, error) {
	if err := f.enter(ctx, MethodSnapshots, req); err != nil {
		return nil, err
	}
	return []restic.Snapshot{fixSnapshot()}, nil
}

// dumpPayload is what the fake's Dump streams: larger than one HTTP chunk.
var dumpPayload = bytes.Repeat([]byte("PK\x03\x04 fixture zip stream "), 2000)

func (f *fakeBackend) Dump(ctx context.Context, req DumpRequest, w io.Writer) (DumpResult, error) {
	if err := f.enter(ctx, MethodDump, req); err != nil {
		return DumpResult{}, err
	}
	f.mu.Lock()
	after, block := f.dumpAfter, f.dumpBlock
	f.mu.Unlock()
	if _, err := w.Write(dumpPayload); err != nil {
		return DumpResult{}, err
	}
	if block != nil {
		// Keep writing until the reader goes away (the console disconnected).
		close(block)
		for {
			if _, err := w.Write(dumpPayload); err != nil {
				return DumpResult{}, err
			}
			if ctx.Err() != nil {
				return DumpResult{}, ctx.Err()
			}
		}
	}
	if after != nil {
		return DumpResult{}, after
	}
	// Bytes and SHA256 are the handler's to fill in from what crossed the socket.
	return DumpResult{ResultMeta: fixMeta(), Bytes: 1, SHA256: strings.Repeat("5a", 32)}, nil
}

func (f *fakeBackend) Stats(ctx context.Context, req StatsRequest) (restic.StatsResult, error) {
	if err := f.enter(ctx, MethodStats, req); err != nil {
		return restic.StatsResult{}, err
	}
	return restic.StatsResult{TotalSize: 1 << 30, TotalFileCount: 99, TotalBlobCount: 1234, SnapshotsCount: 7, CompressionRatio: 1.5}, nil
}

func (f *fakeBackend) Unlock(ctx context.Context, req UnlockRequest) (UnlockResult, error) {
	if err := f.enter(ctx, MethodUnlock, req); err != nil {
		return UnlockResult{}, err
	}
	return UnlockResult{ResultMeta: ResultMeta{Elapsed: time.Second, Outcome: restic.OutcomeSuccess}}, nil
}

func (f *fakeBackend) Forget(ctx context.Context, req ForgetRequest) (ForgetResult, error) {
	if err := f.enter(ctx, MethodForget, req); err != nil {
		return ForgetResult{}, err
	}
	return fixForget(), nil
}

func (f *fakeBackend) Prune(ctx context.Context, req PruneRequest) (PruneResult, error) {
	if err := f.enter(ctx, MethodPrune, req); err != nil {
		return PruneResult{}, err
	}
	return PruneResult{
		ResultMeta: ResultMeta{Elapsed: 2 * time.Minute, ExitCode: 11, Outcome: restic.OutcomeDeferred, DeferredReason: restic.DeferredLock, ErrorClass: restic.ClassLock},
		Stats:      restic.PruneStats{RepackBlobs: 5, RepackBytes: 6, RemoveBlobs: 7, RemoveBytes: 8, UnusedPercentAfter: 4.5},
	}, nil
}

func (f *fakeBackend) Check(ctx context.Context, req CheckRequest) (CheckResult, error) {
	if err := f.enter(ctx, MethodCheck, req); err != nil {
		return CheckResult{}, err
	}
	return CheckResult{
		ResultMeta: ResultMeta{Elapsed: time.Minute, ExitCode: 1, Outcome: restic.OutcomeFailed, ErrorClass: restic.ClassStorage},
		Summary:    restic.CheckSummary{NumErrors: 2, BrokenPacks: 1, SuggestRepairIndex: true, SuggestPrune: true},
	}, nil
}

// shortDir returns a short temp directory: unix socket paths are limited to
// about 100 bytes, and t.TempDir includes the test name.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ap")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func waitDial(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.Dial("unix", path)
		if err == nil {
			_ = conn.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("server did not start listening: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// serveAt starts Serve for h on path and returns a stop function that cancels it
// and returns Serve's result.
func serveAt(t *testing.T, path string, h http.Handler) (stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- Serve(ctx, path, h) }()
	waitDial(t, path)
	var once sync.Once
	var result error
	stop = func() error {
		once.Do(func() {
			cancel()
			select {
			case result = <-errc:
			case <-time.After(20 * time.Second):
				t.Error("Serve did not return after its context was cancelled")
			}
		})
		return result
	}
	t.Cleanup(func() { _ = stop() })
	return stop
}

// startServer serves b on a fresh socket and returns a client for it and the
// socket path.
func startServer(t *testing.T, b Backend, opts ClientOptions) (*Client, string) {
	t.Helper()
	path := filepath.Join(shortDir(t), "ap.sock")
	serveAt(t, path, Handler(b))
	c := NewClient(path, opts)
	t.Cleanup(c.Close)
	return c, path
}

// rawDo sends one raw HTTP request over the socket and returns the status, the
// body and the headers.
func rawDo(t *testing.T, path, verb, route, ctype string, body []byte) (int, []byte, http.Header) {
	t.Helper()
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		},
	}
	defer tr.CloseIdleConnections()
	req, err := http.NewRequest(verb, "http://adminpath"+route, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := (&http.Client{Transport: tr, Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("raw %s %s: %v", verb, route, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("raw %s %s: reading body: %v", verb, route, err)
	}
	return resp.StatusCode, data, resp.Header
}

// wireError decodes {"error": {...}} from a raw response body.
func wireError(t *testing.T, data []byte) Error {
	t.Helper()
	var env struct {
		Error *Error `json:"error"`
	}
	if err := json.Unmarshal(data, &env); err != nil || env.Error == nil {
		t.Fatalf("response is not an error envelope: %q", data)
	}
	return *env.Error
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// validator is implemented by every request type.
type validator interface{ Validate() error }
