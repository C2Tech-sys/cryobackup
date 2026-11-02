package restores

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// TestPurgeExpiresSpentRestores: at expires_at the selection (the encrypted
// path) is cleared and the restore expires; a live one is left alone.
func TestPurgeExpiresSpentRestores(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	live := e.create(t, goodPath)
	e.now = e.now.Add(time.Hour)
	delivered := e.create(t, goodPath)
	if _, err := e.svc.Download(ctx, Actor{}, delivered, io.Discard); err != nil {
		t.Fatal(err)
	}
	if n, err := e.svc.Purge(ctx); err != nil || n != 0 {
		t.Fatalf("nothing is due yet: n=%d err=%v", n, err)
	}
	e.now = live.ExpiresAt // the first one is due, the second one not yet
	if n, err := e.svc.Purge(ctx); err != nil || n != 1 {
		t.Fatalf("first expiry: n=%d err=%v", n, err)
	}
	if r := e.get(t, live.ID); r.Status != "expired" || r.SelectionWrapped != nil || e.svc.Downloadable(r) {
		t.Fatalf("expired row: %+v", r)
	}
	if r := e.get(t, delivered.ID); r.Status != "delivered" || r.SelectionWrapped == nil {
		t.Fatalf("a live delivered restore was touched: %+v", r)
	}
	e.now = e.now.Add(time.Hour)
	if n, _ := e.svc.Purge(ctx); n != 1 {
		t.Fatal("the delivered restore expires at its own expires_at")
	}
	if n, _ := e.svc.Purge(ctx); n != 0 {
		t.Fatal("idempotent")
	}
}

// TestPurgeScrubsLegacyArchives (ADR 0008): archives an earlier version staged
// are deleted at once - the one a row names and any restore-<ULID>.zip or
// .part in the directory - with a warning; other files are never touched, and
// a reference that is not a bare name inside the directory deletes nothing.
func TestPurgeScrubsLegacyArchives(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	staged := e.create(t, goodPath)
	ref := "restore-" + staged.ID + ".zip"
	if _, err := e.st.UpdateRestoreRequest(ctx, store.ScopeAll(), staged.ID, store.RestoreUpdate{ArtifactRef: &ref, PurgeAfter: &staged.ExpiresAt}); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.zip")
	evil := e.create(t, goodPath)
	evilRef := "../" + filepath.Base(outside)
	if _, err := e.st.UpdateRestoreRequest(ctx, store.ScopeAll(), evil.ID, store.RestoreUpdate{ArtifactRef: &evilRef, PurgeAfter: &evil.ExpiresAt}); err != nil {
		t.Fatal(err)
	}
	write := func(p string) {
		if err := os.WriteFile(p, []byte("plaintext client file"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(e.dir, ref))
	write(outside)
	orphans := []string{"restore-01J9ZZ0000000000000000ORPH.zip", "restore-01J9ZZ0000000000000000ORPH.zip.part", "restore-01J9ZZ0000000000000000ORPH.zip.raw.part"}
	for _, o := range orphans {
		write(filepath.Join(e.dir, o))
	}
	foreign := filepath.Join(e.dir, "agent-release-1.4.2.msi")
	write(foreign)

	if _, err := e.svc.Purge(ctx); err != nil {
		t.Fatal(err)
	}
	for _, p := range append([]string{ref}, orphans...) {
		if _, err := os.Stat(filepath.Join(e.dir, p)); !os.IsNotExist(err) {
			t.Errorf("staged archive %s survived the scrub", p)
		}
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Error("a file that is not a restore archive was removed")
	}
	if _, err := os.Stat(outside); err != nil {
		t.Error("a reference outside the directory deleted a file")
	}
	for _, id := range []string{staged.ID, evil.ID} {
		r := e.get(t, id)
		if r.ArtifactRef != "" || r.Status != "ready" || r.SelectionWrapped == nil {
			t.Errorf("row %s after the scrub: ref %q status %s (a live restore stays downloadable)", id, r.ArtifactRef, r.Status)
		}
	}
	if !strings.Contains(e.logs.String(), "removed staged restore archives") || !strings.Contains(e.logs.String(), `"files":5`) {
		t.Fatalf("the scrub is not a loud warning:\n%s", e.logs.String())
	}
	if n, _ := e.svc.Purge(ctx); n != 0 {
		t.Fatal("idempotent")
	}
}
