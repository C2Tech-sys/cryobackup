package restores

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
)

// TestStreamsBusyIsRefusedNotRecorded: the worker's stream budget is used up;
// the download is refused before any byte with ErrStreamsBusy, nothing at all
// is recorded on the restore (no class, no started_at), it is not audited as a
// failure, and it stays downloadable.
func TestStreamsBusyIsRefusedNotRecorded(t *testing.T) {
	e := newEnv(t)
	e.be.Errors = map[string]error{adminpath.MethodDump: adminpath.NewError(adminpath.CodeBusy, "")}
	r := e.create(t, goodPath)
	var out bytes.Buffer
	if _, err := e.svc.Download(context.Background(), Actor{}, r, &out); !errors.Is(err, ErrStreamsBusy) {
		t.Fatalf("busy worker: %v", err)
	}
	row := e.get(t, r.ID)
	if row.ErrorClass != "" || row.Status != "ready" || row.StartedAt != nil || row.FinishedAt != nil || !e.svc.Downloadable(row) || out.Len() != 0 {
		t.Fatalf("a refused stream left a trace: %+v (%d bytes)", row, out.Len())
	}
	if auditHas(t, e, "restore.failed", r.ID) {
		t.Fatal("a refusal is not a failed download")
	}
	if !strings.Contains(e.logs.String(), "streams_busy") {
		t.Fatalf("the refusal is not logged:\n%s", e.logs.String())
	}
}

// TestRedownloadWindow (M1-HIPAA2 item 2, operator ruling 2026-10-29): a
// delivered restore downloads again, each time with a new single-use token and
// a fresh stream, until expires_at; then every token is refused with
// ErrExpired, and the purge job leaves nothing of the selection but its hash.
func TestRedownloadWindow(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	r := e.create(t, goodPath)
	download := func(sess string) {
		t.Helper()
		cur := e.get(t, r.ID)
		tok, _, err := e.svc.MintToken(cur, sess)
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		if err := e.svc.VerifyToken(tok, cur, sess); err != nil {
			t.Fatalf("verify: %v", err)
		}
		if err := e.svc.VerifyToken(tok, cur, sess); !errors.Is(err, ErrToken) {
			t.Fatalf("a token works once: %v", err)
		}
		if _, err := e.svc.Download(ctx, Actor{}, cur, io.Discard); err != nil {
			t.Fatalf("download: %v", err)
		}
	}
	// (a) first and repeat downloads stream again and count.
	download("s1")
	e.now = e.now.Add(3 * time.Hour)
	download("s1")
	download("s2")
	calls := 0
	for _, c := range e.be.Calls() {
		if c.Method == adminpath.MethodDump {
			calls++
		}
	}
	if row := e.get(t, r.ID); row.Status != "delivered" || row.DownloadCount != 3 || calls != 3 {
		t.Fatalf("after three downloads: count %d status %s, %d dump streams", row.DownloadCount, row.Status, calls)
	}

	// (b) a token minted before expires_at and presented after it is refused,
	// and so is any other token; nothing is minted and nothing streams.
	row := e.get(t, r.ID)
	e.now = row.ExpiresAt.Add(-time.Minute)
	tok, _, err := e.svc.MintToken(row, "s1")
	if err != nil {
		t.Fatal(err)
	}
	e.now = row.ExpiresAt
	for _, presented := range []string{tok, "garbage", ""} {
		if err := e.svc.VerifyToken(presented, row, "s1"); !errors.Is(err, ErrExpired) {
			t.Fatalf("token %q past expires_at: %v", presented, err)
		}
	}
	if _, _, err := e.svc.MintToken(row, "s1"); !errors.Is(err, ErrExpired) {
		t.Fatalf("mint past expires_at: %v", err)
	}
	if _, err := e.svc.Download(ctx, Actor{}, row, io.Discard); !errors.Is(err, ErrExpired) {
		t.Fatalf("download past expires_at: %v", err)
	}

	// (c) the purge job clears the encrypted selection; only the audit hash
	// and count remain, and the expired row still refuses tokens.
	if n, err := e.svc.Purge(ctx); err != nil || n != 1 {
		t.Fatalf("purge: n=%d err=%v", n, err)
	}
	row = e.get(t, r.ID)
	if row.Status != "expired" || row.SelectionWrapped != nil || row.SelectionHash == "" || row.SelectionCount != 1 || row.ArtifactRef != "" {
		t.Fatalf("after the purge: %+v", row)
	}
	if err := e.svc.VerifyToken(tok, row, "s1"); !errors.Is(err, ErrExpired) {
		t.Fatalf("token after the purge: %v", err)
	}
	if _, _, err := e.svc.MintToken(row, "s1"); !errors.Is(err, ErrExpired) {
		t.Fatalf("mint after the purge: %v", err)
	}
	if !errors.Is(ErrExpired, ErrNotReady) {
		t.Fatal("ErrExpired must wrap ErrNotReady")
	}
}
