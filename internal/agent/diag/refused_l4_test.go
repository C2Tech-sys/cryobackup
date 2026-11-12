package diag

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/agent/config"
	"github.com/c2tech-sys/cryobackup/internal/agent/journal"
)

// M3-UPLINK-L4 (ADR 0018 decision 1): "cryo-agent status" says in plain words
// when the console refuses the device token, with what to do, and says nothing
// when it does not.

// A refused token is a block right after the enrollment lines, in plain words,
// with how long it has lasted and what to do; no path, no URL.
func TestRenderTokenRefusedBlock(t *testing.T) {
	since := now.Add(-(3*time.Hour + 12*time.Minute))
	r := Report{
		Enrolled: true, DeviceID: "01JB0000000000000000000001", Server: "https://console.example:8443",
		ReportedAtDevice: now, TokenRefusedSince: since, StateVerified: true, HaveJournal: true,
	}
	out := render(r)
	for _, want := range []string{
		"TOKEN REFUSED since 2026-10-22 08:48 UTC (3 h 12 min)\n",
		"  The console answers 401 to this device's token: the device was retired or\n",
		"  revoked, the console was restored from before this enrollment, or this\n",
		"  agent points at the wrong console. Backups keep running on the last policy\n",
		"  and run records queue locally. The agent asks the console once an hour.\n",
		"  To fix: uninstall CryoBackup, move the state directory aside (the CryoBackup\n",
		"  folder under ProgramData), and install again with a new enrollment token.\n",
		"  If the device is no longer wanted, uninstall it and stop there.\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	// Directly after the console line (the enrollment/server lines), before the
	// state lines.
	iConsole, iBlock, iACL := strings.Index(out, "console:"), strings.Index(out, "TOKEN REFUSED"), strings.Index(out, "state acl:")
	if !(iConsole >= 0 && iConsole < iBlock && iBlock < iACL) {
		t.Errorf("block out of place (console %d, block %d, state acl %d):\n%s", iConsole, iBlock, iACL, out)
	}
	if strings.Count(out, "https://") != 1 { // only the console line
		t.Errorf("the block repeats the console URL:\n%s", out)
	}
}

// Not refused: no block, nothing about a token.
func TestRenderWithoutRefusalHasNoBlock(t *testing.T) {
	r := Report{Enrolled: true, DeviceID: "01JB0000000000000000000001", Server: "https://console.example:8443", ReportedAtDevice: now, StateVerified: true, HaveJournal: true}
	out := render(r)
	for _, bad := range []string{"TOKEN REFUSED", "401", "re-enroll"} {
		if strings.Contains(out, bad) {
			t.Errorf("output carries %q although the token is not refused:\n%s", bad, out)
		}
	}
}

// The duration is in the words an operator reads: minutes, hours and minutes,
// days and hours; a refusal younger than a minute (or a clock behind) is
// "under 1 min".
func TestRefusedForWording(t *testing.T) {
	for _, c := range []struct {
		d    time.Duration
		want string
	}{
		{-time.Minute, "under 1 min"},
		{20 * time.Second, "under 1 min"},
		{90 * time.Second, "2 min"},
		{59 * time.Minute, "59 min"},
		{time.Hour, "1 h 0 min"},
		{3*time.Hour + 12*time.Minute, "3 h 12 min"},
		{25*time.Hour + 30*time.Minute, "1 d 1 h"},
		{49 * time.Hour, "2 d 1 h"},
	} {
		if got := refusedFor(c.d); got != c.want {
			t.Errorf("refusedFor(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

// Status reads the journal key the agent sets, and reports nothing without it
// or with a value that is not a time.
func TestStatusReadsTheRefusalFromTheJournal(t *testing.T) {
	ctx := context.Background()
	stubVerdict(t, config.Verdict{}, false, nil)
	dir, jr := enrolledWithPolicy(t)

	rep, err := Status(ctx, dir, now)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.TokenRefusedSince.IsZero() || strings.Contains(render(rep), "TOKEN REFUSED") {
		t.Fatalf("a journal without the key reads as refused: %v", rep.TokenRefusedSince)
	}

	since := now.Add(-45 * time.Minute)
	if err := jr.KVSet(ctx, journal.KVTokenRefusedSince, since.Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	rep, err = Status(ctx, dir, now)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.TokenRefusedSince.Equal(since) {
		t.Fatalf("TokenRefusedSince = %v, want %v", rep.TokenRefusedSince, since)
	}
	if out := render(rep); !strings.Contains(out, "TOKEN REFUSED since 2026-10-22 11:15 UTC (45 min)\n") {
		t.Errorf("status lacks the block:\n%s", out)
	}

	if err := jr.KVSet(ctx, journal.KVTokenRefusedSince, "not a time"); err != nil {
		t.Fatal(err)
	}
	if rep, err = Status(ctx, dir, now); err != nil || !rep.TokenRefusedSince.IsZero() {
		t.Fatalf("a garbled key reads as refused: %v, %v", rep.TokenRefusedSince, err)
	}
	if err := jr.KVDelete(ctx, journal.KVTokenRefusedSince); err != nil {
		t.Fatal(err)
	}
	if rep, err = Status(ctx, dir, now); err != nil || !rep.TokenRefusedSince.IsZero() {
		t.Fatalf("the deleted key still reads as refused: %v, %v", rep.TokenRefusedSince, err)
	}
}
