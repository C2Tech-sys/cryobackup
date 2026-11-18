package restores

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// ADR 0019 (P19-F2): a restore created before a device was retired and purged must not stream the
// device's backups once the purge has been requested. A device that is only retired keeps ADR 0016's
// behaviour: its existing restores still download until they expire.

func (e *env) retireDevice(t *testing.T, id string) store.AuditActor {
	t.Helper()
	ctx := context.Background()
	a, err := e.st.GetAdminByUsername(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	actor := store.AuditActor{AdminID: a.ID, Username: a.Username, Method: "web"}
	if _, err := e.st.RetireDeviceAudited(ctx, store.ScopeAll(), id, store.RetireRequest{Reason: "decommissioned"}, actor); err != nil {
		t.Fatal(err)
	}
	return actor
}

func TestAdr19DownloadRefusedForAPurgingDevice(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	r := e.create(t, goodPath)
	tok, _, err := e.svc.MintToken(r, "session-A")
	if err != nil {
		t.Fatal(err)
	}
	actor := e.retireDevice(t, e.device.ID)
	if _, err := e.st.RequestDevicePurge(ctx, store.ScopeAll(), e.device.ID, "lab machine gone", actor); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if _, err := e.svc.Download(ctx, Actor{Username: "tech"}, r, &out); !errors.Is(err, ErrNotReady) {
		t.Fatalf("Download for a purging device: %v, want ErrNotReady (the conflict problem)", err)
	}
	if _, err := e.svc.DownloadWithToken(ctx, Actor{Username: "tech"}, r, tok, "session-A", &out); !errors.Is(err, ErrNotReady) {
		t.Fatalf("DownloadWithToken for a purging device: %v, want ErrNotReady", err)
	}
	if out.Len() != 0 || e.be.Count(adminpath.MethodDump) != 0 {
		t.Fatalf("a purging device's restore streamed: %d bytes, %d dumps", out.Len(), e.be.Count(adminpath.MethodDump))
	}
	if got := e.get(t, r.ID); got.Status != "ready" || got.DownloadCount != 0 {
		t.Fatalf("a refused download changed the restore: %+v", got)
	}
}

// A device that is only retired (ADR 0016) still lets an existing restore download.
func TestAdr19DownloadStillWorksForAMerelyRetiredDevice(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	r := e.create(t, goodPath)
	e.retireDevice(t, e.device.ID)
	var out bytes.Buffer
	if _, err := e.svc.Download(ctx, Actor{Username: "tech"}, r, &out); err != nil {
		t.Fatalf("Download for a retired device: %v", err)
	}
	if out.Len() == 0 || e.be.Count(adminpath.MethodDump) != 1 {
		t.Fatalf("retired device: %d bytes, %d dumps", out.Len(), e.be.Count(adminpath.MethodDump))
	}
}
