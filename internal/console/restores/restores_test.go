package restores

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/console/escrow"
	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

type env struct {
	st     *store.Store
	svc    *Service
	be     *fixtures.Backend
	now    time.Time
	dir    string // the console's artifacts directory (legacy scrub only)
	logs   *bytes.Buffer
	device store.Device
	held   store.Device
}

func clockPtr(t *time.Time) func() time.Time { return func() time.Time { return *t } }

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	e := &env{now: fixtures.Now(), dir: t.TempDir(), logs: &bytes.Buffer{}}
	st, err := store.OpenMemory(ctx, clockPtr(&e.now))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := store.LoadFixtures(ctx, st, e.now); err != nil {
		t.Fatal(err)
	}
	e.st = st
	mk, _ := escrow.NewMasterKey(bytes.Repeat([]byte{3}, 32))
	e.be = &fixtures.Backend{}
	e.svc = &Service{Store: st, Backend: e.be, Master: mk, ArtifactsDir: e.dir, Clock: clockPtr(&e.now), Log: logx.New(e.logs, logx.LevelDebug)}
	find := func(host string) store.Device {
		l, err := st.ListDevices(ctx, store.ScopeAll(), store.ListDevicesOptions{Search: host})
		if err != nil || len(l.Items) != 1 {
			t.Fatalf("fixture device %s: %v", host, err)
		}
		return l.Items[0]
	}
	e.device, e.held = find("DR-OFFICE-PC"), find("LEGAL-PC-01")
	for _, d := range []store.Device{e.device, e.held} {
		if _, _, err := st.AgentReplaceSnapshots(ctx, d.ID, []store.Snapshot{{DeviceID: d.ID, SnapshotID: fixtures.FixtureSnapshotID, TakenAt: e.now.Add(-time.Hour), Hostname: d.Hostname, Paths: []string{`C:\Users`}}}); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

const (
	goodPath = "/C/Users/Public/Documents/Quarterly-Report.txt"
	goodDir  = "/C/Users/Public/Documents"
)

func (e *env) create(t *testing.T, p string) store.RestoreRequest {
	t.Helper()
	r, err := e.svc.Create(context.Background(), Actor{AdminID: "", Username: "tech"}, store.ScopeAll(), e.device.ID, fixtures.FixtureSnapshotID, p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (e *env) get(t *testing.T, id string) store.RestoreRequest {
	t.Helper()
	r, err := e.st.GetRestoreRequest(context.Background(), store.ScopeAll(), id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// files lists every file under dir.
func files(dir string) []string {
	var out []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			out = append(out, p)
		}
		return nil
	})
	return out
}

func readZip(t *testing.T, b []byte) map[string][]byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatalf("not a zip: %v", err)
	}
	out := map[string][]byte{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		out[f.Name], _ = io.ReadAll(rc)
		rc.Close()
	}
	return out
}

// TestCreateIsReadyAndBuildsNothing (ADR 0008): creating a restore validates
// and records it; no dump runs and no file is written.
func TestCreateIsReadyAndBuildsNothing(t *testing.T) {
	e := newEnv(t)
	r := e.create(t, goodPath)
	if r.Status != "ready" || r.Bytes != nil || r.ArtifactRef != "" || r.PurgeAfter != nil || !e.svc.Downloadable(r) {
		t.Fatalf("row: %+v", r)
	}
	if !r.ExpiresAt.Equal(e.now.Add(DefaultTTL)) {
		t.Fatalf("expires_at = %v", r.ExpiresAt)
	}
	if len(r.SelectionWrapped) == 0 || bytes.Contains(r.SelectionWrapped, []byte("Quarterly")) {
		t.Fatal("the path is stored encrypted, never in clear")
	}
	sum := sha256.Sum256([]byte(goodPath))
	if r.SelectionHash != hex.EncodeToString(sum[:]) || r.SelectionCount != 1 {
		t.Fatalf("selection hash/count: %+v", r)
	}
	if n := e.be.Count(adminpath.MethodDump); n != 0 {
		t.Fatalf("Create ran %d dumps", n)
	}
	if f := files(e.dir); len(f) != 0 {
		t.Fatalf("Create wrote files: %v", f)
	}
}

// TestDownloadStreamsAFileAndRecordsTheDelivery: the zip is valid, its content
// hashes match the original, nothing lands on disk during or after, and the
// size and SHA-256 are recorded only when the stream completed.
func TestDownloadStreamsAFileAndRecordsTheDelivery(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	r := e.create(t, goodPath)
	var during []string
	out := &hookWriter{first: func() { during = files(e.dir) }}
	res, err := e.svc.Download(ctx, Actor{Username: "tech"}, r, out)
	if err != nil {
		t.Fatal(err)
	}
	got := readZip(t, out.Bytes())
	want := fixtures.DefaultFiles()[0].Content
	if len(got) != 1 || sha256.Sum256(got["Quarterly-Report.txt"]) != sha256.Sum256(want) {
		t.Fatalf("zip entries %d; restored content differs from the original", len(got))
	}
	sum := sha256.Sum256(out.Bytes())
	if res.Bytes != int64(out.Len()) || res.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("result %+v for %d bytes", res, out.Len())
	}
	row := e.get(t, r.ID)
	var pr struct {
		Bytes  int64  `json:"bytes"`
		SHA256 string `json:"sha256"`
	}
	_ = json.Unmarshal(row.Progress, &pr)
	if row.Status != "delivered" || row.DownloadCount != 1 || row.Bytes == nil || *row.Bytes != res.Bytes || pr.SHA256 != res.SHA256 || pr.Bytes != res.Bytes || row.ErrorClass != "" {
		t.Fatalf("row after delivery: %+v progress %s", row, row.Progress)
	}
	if calls := e.be.Calls(); len(calls) != 1 || calls[0].Path != goodPath || calls[0].DeviceID != e.device.ID {
		t.Fatalf("the worker got %+v", calls)
	}
	if len(during) != 0 || len(files(e.dir)) != 0 {
		t.Fatalf("files on disk: during %v, after %v", during, files(e.dir))
	}
	// A delivered restore streams again (a new download, from the beginning).
	var again bytes.Buffer
	if _, err := e.svc.Download(ctx, Actor{}, row, &again); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(again.Bytes(), out.Bytes()) || e.get(t, r.ID).DownloadCount != 2 {
		t.Fatal("the second download differs or was not counted")
	}
	if strings.Contains(e.logs.String(), "Quarterly") || strings.Contains(e.logs.String(), "/C/Users") {
		t.Fatalf("the path reached a log line:\n%s", e.logs.String())
	}
}

// TestDownloadStreamsADirectory: a directory restore is one zip of its files.
func TestDownloadStreamsADirectory(t *testing.T) {
	e := newEnv(t)
	r := e.create(t, goodDir)
	var out bytes.Buffer
	if _, err := e.svc.Download(context.Background(), Actor{}, r, &out); err != nil {
		t.Fatal(err)
	}
	got := readZip(t, out.Bytes())
	if len(got) != 2 || sha256.Sum256(got["Quarterly-Report.txt"]) != sha256.Sum256(fixtures.DefaultFiles()[0].Content) ||
		sha256.Sum256(got["Budget-2026.csv"]) != sha256.Sum256(fixtures.DefaultFiles()[1].Content) {
		t.Fatalf("directory zip entries: %d", len(got))
	}
	if len(files(e.dir)) != 0 {
		t.Fatal("a directory restore wrote a file")
	}
}

type hookWriter struct {
	bytes.Buffer
	first func()
	done  bool
}

func (h *hookWriter) Write(p []byte) (int, error) {
	if !h.done && h.first != nil {
		h.done = true
		h.first()
	}
	return h.Buffer.Write(p)
}

func TestRefusals(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	cases := []struct {
		name string
		dev  string
		snap string
		path string
		want error
	}{
		{"customer-held", e.held.ID, fixtures.FixtureSnapshotID, goodPath, ErrCustomerHeld},
		{"snapshot not in the cache", e.device.ID, strings.Repeat("a", 64), goodPath, ErrInvalid},
		{"malformed snapshot", e.device.ID, "latest", goodPath, ErrInvalid},
		{"relative path", e.device.ID, fixtures.FixtureSnapshotID, "C/x", ErrInvalid},
		{"control character", e.device.ID, fixtures.FixtureSnapshotID, "/C/x\n", ErrInvalid},
		{"empty path", e.device.ID, fixtures.FixtureSnapshotID, "", ErrInvalid},
		{"unknown device", "01J9ZZ0000000000000000ZZZZ", fixtures.FixtureSnapshotID, goodPath, store.ErrNotFound},
	}
	for _, c := range cases {
		if _, err := e.svc.Create(ctx, Actor{}, store.ScopeAll(), c.dev, c.snap, c.path); !errors.Is(err, c.want) {
			t.Errorf("%s: want %v, got %v", c.name, c.want, err)
		}
	}
	// A scope that does not include the device sees it as not found.
	if _, err := e.svc.Create(ctx, Actor{}, store.ScopeOf("other"), e.device.ID, fixtures.FixtureSnapshotID, goodPath); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("out of scope: %v", err)
	}
	noKey := &Service{Store: e.st, Backend: e.be, Clock: e.svc.Clock}
	if _, err := noKey.Create(ctx, Actor{}, store.ScopeAll(), e.device.ID, fixtures.FixtureSnapshotID, goodPath); !errors.Is(err, ErrMasterKey) {
		t.Errorf("no master key: %v", err)
	}
	r := e.create(t, goodPath)
	if _, err := noKey.Download(ctx, Actor{}, r, io.Discard); !errors.Is(err, ErrMasterKey) {
		t.Errorf("download without the master key: %v", err)
	}
	if e.be.Count(adminpath.MethodDump) != 0 {
		t.Error("no dump may run for a refused request")
	}
}

// TestFailureBeforeTheFirstByte: the class is recorded, logged and audited, the
// row stays downloadable (a retry starts over), and nothing was written.
func TestFailureBeforeTheFirstByte(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	r := e.create(t, goodPath)
	e.be.Errors = map[string]error{adminpath.MethodDump: adminpath.NewError(adminpath.CodeStorageUnreachable, restic.ClassNetwork)}
	var out bytes.Buffer
	_, err := e.svc.Download(ctx, Actor{}, r, &out)
	var fe *FailedError
	if !errors.As(err, &fe) || fe.Class != "network" || fe.Code != adminpath.CodeStorageUnreachable || out.Len() != 0 {
		t.Fatalf("got %v, %d bytes", err, out.Len())
	}
	row := e.get(t, r.ID)
	if row.Status != "ready" || row.ErrorClass != "network" || row.SelectionWrapped == nil || row.DownloadCount != 0 || !e.svc.Downloadable(row) {
		t.Fatalf("row after a failed download: %+v", row)
	}
	if !strings.Contains(e.logs.String(), `"class":"network"`) || !strings.Contains(e.logs.String(), "restore: download failed") {
		t.Fatalf("the failure is not logged with its class:\n%s", e.logs.String())
	}
	if !auditHas(t, e, "restore.failed", r.ID) {
		t.Fatal("the failure is not audited")
	}
	// The worker is unreachable: class unavailable.
	e.be.Errors = map[string]error{adminpath.MethodDump: adminpath.ErrUnavailable}
	if _, err := e.svc.Download(ctx, Actor{}, row, io.Discard); !errors.As(err, &fe) || fe.Class != ClassUnavailable {
		t.Fatalf("socket down: %v", err)
	}
	// A retry that works clears the class.
	e.be.Errors = nil
	if _, err := e.svc.Download(ctx, Actor{}, e.get(t, r.ID), io.Discard); err != nil {
		t.Fatal(err)
	}
	if row := e.get(t, r.ID); row.Status != "delivered" || row.ErrorClass != "" {
		t.Fatalf("after a good retry: %+v", row)
	}
}

func auditHas(t *testing.T, e *env, action, id string) bool {
	t.Helper()
	l, err := e.st.ListAudit(context.Background(), store.ScopeAll(), store.AuditFilter{Action: action, ObjectID: id}, store.Page{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	return len(l.Items) > 0
}

// TestFailureMidStream: a run that breaks after part of the archive went out is
// a failure, never a delivery.
func TestFailureMidStream(t *testing.T) {
	e := newEnv(t)
	e.be.FailDumpAfter = 40
	r := e.create(t, goodDir)
	var out bytes.Buffer
	_, err := e.svc.Download(context.Background(), Actor{}, r, &out)
	var fe *FailedError
	if !errors.As(err, &fe) || fe.Class != "network" || out.Len() != 40 {
		t.Fatalf("got %v after %d bytes", err, out.Len())
	}
	if row := e.get(t, r.ID); row.Status != "ready" || row.Bytes != nil || row.DownloadCount != 0 {
		t.Fatalf("a cut stream was recorded as a delivery: %+v", row)
	}
}

type deadBrowser struct{}

func (deadBrowser) Write([]byte) (int, error) { return 0, errors.New("broken pipe 198.51.100.7") }

// TestClientDisconnect records class disconnected, with no text from the
// connection.
func TestClientDisconnect(t *testing.T) {
	e := newEnv(t)
	r := e.create(t, goodPath)
	_, err := e.svc.Download(context.Background(), Actor{}, r, deadBrowser{})
	var fe *FailedError
	if !errors.As(err, &fe) || fe.Class != ClassDisconnected {
		t.Fatalf("got %v", err)
	}
	if row := e.get(t, r.ID); row.ErrorClass != ClassDisconnected || row.Status != "ready" {
		t.Fatalf("row: %+v", row)
	}
	if strings.Contains(e.logs.String(), "198.51.100.7") {
		t.Fatal("the connection's error text reached the log")
	}
}

// blockingBackend holds Dump until released.
type blockingBackend struct {
	*fixtures.Backend
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingBackend) Dump(ctx context.Context, req adminpath.DumpRequest, w io.Writer) (adminpath.DumpResult, error) {
	b.once.Do(func() { close(b.entered) })
	<-b.release
	return b.Backend.Dump(ctx, req, w)
}

// TestOneStreamPerRestore: a second download of the same restore while one
// streams is refused (ErrBusy); another restore is not affected.
func TestOneStreamPerRestore(t *testing.T) {
	e := newEnv(t)
	bb := &blockingBackend{Backend: e.be, entered: make(chan struct{}), release: make(chan struct{})}
	e.svc.Backend = bb
	r := e.create(t, goodPath)
	done := make(chan error, 1)
	go func() { _, err := e.svc.Download(context.Background(), Actor{}, r, io.Discard); done <- err }()
	<-bb.entered
	if _, err := e.svc.Download(context.Background(), Actor{}, r, io.Discard); !errors.Is(err, ErrBusy) {
		t.Fatalf("a concurrent download of the same restore: %v", err)
	}
	close(bb.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestTokenIsBoundSingleUseAndExpiring(t *testing.T) {
	e := newEnv(t)
	r := e.create(t, goodPath)
	tok, exp, err := e.svc.MintToken(r, "session-A")
	if err != nil {
		t.Fatal(err)
	}
	if !exp.Equal(e.now.Add(2 * time.Minute)) {
		t.Fatalf("a token lives 2 minutes: %v", exp)
	}
	// Bound to the minting session and the restore: a wrong session or restore
	// does not consume it.
	if err := e.svc.VerifyToken(tok, r, "session-B"); !errors.Is(err, ErrToken) {
		t.Fatalf("token from another session: %v", err)
	}
	if err := e.svc.VerifyToken(tok, r, ""); !errors.Is(err, ErrToken) {
		t.Fatalf("token without a session: %v", err)
	}
	other := e.create(t, goodPath)
	if err := e.svc.VerifyToken(tok, other, "session-A"); !errors.Is(err, ErrToken) {
		t.Fatalf("a token must not open another restore: %v", err)
	}
	for _, bad := range []string{"", "garbage", tok + "x", tok[:len(tok)-1]} {
		if err := e.svc.VerifyToken(bad, r, "session-A"); !errors.Is(err, ErrToken) {
			t.Fatalf("%q: %v", bad, err)
		}
	}
	if err := e.svc.VerifyToken(tok, r, "session-A"); err != nil {
		t.Fatalf("fresh token: %v", err)
	}
	// Single use: there is no resume, so the same token never works twice.
	if err := e.svc.VerifyToken(tok, r, "session-A"); !errors.Is(err, ErrToken) {
		t.Fatalf("a used token must not work again: %v", err)
	}
	// Minting again invalidates the previous token.
	tok2, _, _ := e.svc.MintToken(r, "session-A")
	tok3, _, _ := e.svc.MintToken(r, "session-A")
	if tok2 == tok3 {
		t.Fatal("tokens must be fresh")
	}
	if err := e.svc.VerifyToken(tok2, r, "session-A"); !errors.Is(err, ErrToken) {
		t.Fatalf("the previous token must stop working: %v", err)
	}
	// Expiry.
	e.now = e.now.Add(TokenTTL + time.Second)
	if err := e.svc.VerifyToken(tok3, r, "session-A"); !errors.Is(err, ErrToken) {
		t.Fatalf("expired token: %v", err)
	}
	// A delivered restore mints again (a second download re-streams).
	if _, err := e.svc.Download(context.Background(), Actor{}, r, io.Discard); err != nil {
		t.Fatal(err)
	}
	after := e.get(t, r.ID)
	tok4, _, err := e.svc.MintToken(after, "session-A")
	if err != nil {
		t.Fatalf("a delivered restore must mint a new token: %v", err)
	}
	if err := e.svc.VerifyToken(tok4, after, "session-A"); err != nil {
		t.Fatal(err)
	}
	// An expired restore mints nothing.
	e.now = after.ExpiresAt
	if _, _, err := e.svc.MintToken(after, "session-A"); !errors.Is(err, ErrNotReady) {
		t.Fatalf("no token past expires_at: %v", err)
	}
	if _, err := e.svc.Download(context.Background(), Actor{}, after, io.Discard); !errors.Is(err, ErrNotReady) {
		t.Fatalf("no download past expires_at: %v", err)
	}
	if _, _, err := e.svc.MintToken(r, ""); !errors.Is(err, ErrNotReady) && !errors.Is(err, ErrToken) {
		t.Fatalf("a token needs a session to bind to: %v", err)
	}
}

func TestTokenNeedsASession(t *testing.T) {
	e := newEnv(t)
	r := e.create(t, goodPath)
	if _, _, err := e.svc.MintToken(r, ""); !errors.Is(err, ErrToken) {
		t.Fatalf("a token needs a session to bind to: %v", err)
	}
}

func TestTokenIsStoredOnlyAsAHash(t *testing.T) {
	e := newEnv(t)
	r := e.create(t, goodPath)
	tok, _, _ := e.svc.MintToken(r, "s")
	tb := e.svc.table()
	tb.mu.Lock()
	defer tb.mu.Unlock()
	d := tb.m[r.ID]
	if bytes.Contains(d.hash[:], []byte(tok)) || len(tb.m) != 1 {
		t.Fatal("only the hash is kept")
	}
}
