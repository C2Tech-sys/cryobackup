package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/agent/config"
	"github.com/c2tech-sys/cryobackup/internal/agent/diag"
	"github.com/c2tech-sys/cryobackup/internal/agent/internal/fakerestic"
	"github.com/c2tech-sys/cryobackup/internal/agent/journal"
)

// TestStalePlannedStartIsClearedAtStart (M1-HIPAA2 item 7, FIX7 verifier): a
// planned start left in the journal by a process that stopped uncleanly is
// not the running agent's plan. The new process starts a backup at once and
// keeps it running, so its scheduler never writes (or deletes) a plan of its
// own; the stale value must be gone anyway, and "cryo-agent status" must not
// print it as "next due ... (as the running agent plans it)".
func TestStalePlannedStartIsClearedAtStart(t *testing.T) {
	ctx := context.Background()
	fc := newFakeConsole(t)
	dir := enroll(t, fc)

	// The crashed process's leftover: a plan two days out.
	stale := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
	b, _ := json.Marshal(journal.PlannedStart{At: stale, Reason: "startup_delay"})
	jr, err := journal.Open(ctx, journal.Options{Path: config.Layout{Root: dir}.JournalFile()})
	if err != nil {
		t.Fatal(err)
	}
	if err := jr.KVSet(ctx, journal.KVPlannedStart, string(b)); err != nil {
		t.Fatal(err)
	}
	_ = jr.Close()

	fakerestic.Use(t, "hang_graceful") // a backup that runs until it is stopped
	r := start(t, fakeOptions(t, dir))
	waitFor(t, "the backup to be running", 30*time.Second, func() bool {
		r.a.mu.Lock()
		defer r.a.mu.Unlock()
		return r.a.running != nil
	})

	ro := openJournalRO(t, dir)
	if raw, ok, _ := ro.KVGet(ctx, journal.KVPlannedStart); ok {
		t.Fatalf("the previous process's planned start survived the restart: %s", raw)
	}
	rep, err := diag.Status(ctx, dir, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	rep.Render(&out)
	if strings.Contains(out.String(), stale.Format(time.RFC3339)) || strings.Contains(out.String(), "as the running agent plans it") {
		t.Fatalf("status shows a stale planned start:\n%s", out.String())
	}
}
