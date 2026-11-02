package restores

import (
	"context"
	"errors"
	"io"
	"testing"
)

// TestGiveBackNeverRevivesAnOlderToken (M1-HIPAA2 last batch, item 3): A takes
// T1; while A's download is being refused, T2 is minted and already taken by B
// (the table is empty again). A's give-back must not revive T1: a token is
// given back only when nothing was minted for the restore after it.
func TestGiveBackNeverRevivesAnOlderToken(t *testing.T) {
	e := newEnv(t)
	be := &busyOnce{Backend: e.be}
	be.busy.Store(true)
	e.svc.Backend = be
	ctx := context.Background()
	r := e.create(t, goodPath)
	t1, _, err := e.svc.MintToken(r, "s1")
	if err != nil {
		t.Fatal(err)
	}
	// The interleaving, forced: inside A's refused call, B mints T2 and takes it.
	be.hook = func() {
		t2, _, err := e.svc.MintToken(e.get(t, r.ID), "s2")
		if err != nil {
			t.Error(err)
			return
		}
		if err := e.svc.VerifyToken(t2, e.get(t, r.ID), "s2"); err != nil {
			t.Error(err)
		}
	}
	if _, err := e.svc.DownloadWithToken(ctx, Actor{}, e.get(t, r.ID), t1, "s1", io.Discard); !errors.Is(err, ErrStreamsBusy) {
		t.Fatalf("busy: %v", err)
	}
	be.busy.Store(false)
	be.hook = nil
	if _, err := e.svc.DownloadWithToken(ctx, Actor{}, e.get(t, r.ID), t1, "s1", io.Discard); !errors.Is(err, ErrToken) {
		t.Fatalf("T1 was revived after T2 had been minted and taken: %v", err)
	}
	// With nothing minted after it, a refused token is still given back.
	t3, _, _ := e.svc.MintToken(e.get(t, r.ID), "s1")
	be.busy.Store(true)
	if _, err := e.svc.DownloadWithToken(ctx, Actor{}, e.get(t, r.ID), t3, "s1", io.Discard); !errors.Is(err, ErrStreamsBusy) {
		t.Fatalf("busy: %v", err)
	}
	be.busy.Store(false)
	if _, err := e.svc.DownloadWithToken(ctx, Actor{}, e.get(t, r.ID), t3, "s1", io.Discard); err != nil {
		t.Fatalf("a given-back token with nothing minted after it: %v", err)
	}
}
