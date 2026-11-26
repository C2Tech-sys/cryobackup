package ui

import (
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/maintain"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// P22-R2b B8 and G6: the device page's "Next check" is the verifier's own due time, not a guess.

func TestP22R2bNextCheckIsTheVerifiersDueTime(t *testing.T) {
	t.Parallel()
	c := fmtCtx{Now: time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC), Loc: time.UTC, Zone: "UTC"}
	id := "01JDEVICE00000000000000001"
	var esc store.Escrow
	proven := func(age time.Duration) store.Escrow {
		at := c.Now.Add(-age)
		return store.Escrow{VerifiedAt: &at}
	}
	// The device's real due time is the proof plus a week less its stagger offset (up to a day sooner).
	// Find the offset through the verifier's own function.
	off := 7*24*time.Hour - maintain.EscrowVerifyDueAt(id, c.Now).Sub(c.Now)
	if off < 0 || off >= 24*time.Hour {
		t.Fatalf("offset %v out of [0, 24h)", off)
	}
	dueAge := 7*24*time.Hour - off // the proof age at which the verifier calls the key due

	// Not due: an hour before. The page must not say "Due now" (the old rule said it from day 6).
	esc = proven(dueAge - time.Hour)
	if got := escrowNextCheck(id, esc, c); strings.Contains(got, "Due") || !strings.HasPrefix(got, "By ") {
		t.Errorf("an hour before the verifier's due time: %q", got)
	}
	// Six days and a minute old (the old rule's "Due now") is not due unless the offset is the whole stagger.
	esc = proven(6*24*time.Hour + time.Minute)
	if dueAge > 6*24*time.Hour+time.Minute {
		if got := escrowNextCheck(id, esc, c); strings.Contains(got, "Due") {
			t.Errorf("6 days and a minute old, due at %v: %q", dueAge, got)
		}
	}
	// Due: at the due time exactly, and long after.
	for _, age := range []time.Duration{dueAge, dueAge + time.Second, 30 * 24 * time.Hour} {
		if got := escrowNextCheck(id, proven(age), c); got != "Due now: the next maintenance pass runs it" {
			t.Errorf("proof %v old (due at %v): %q", age, dueAge, got)
		}
	}
	// The date it names is the verifier's.
	esc = proven(time.Hour)
	want := maintain.EscrowVerifyDueAt(id, *esc.VerifiedAt)
	if got := escrowNextCheck(id, esc, c); !strings.Contains(got, c.dayClock(want)) {
		t.Errorf("the date shown is not the due time %v: %q", want, got)
	}
	// A failed key is due as soon as the retry hold is over, whatever the proof's age (B3); a proof in
	// the future proves nothing and is due too (B2).
	esc = proven(time.Hour)
	esc.LastVerifyErrorClass = "repo_missing"
	if got := escrowNextCheck(id, esc, c); got != "Due now: the next maintenance pass runs it" {
		t.Errorf("a failed key with a fresh proof: %q", got)
	}
	future := c.Now.Add(60 * 24 * time.Hour)
	if got := escrowNextCheck(id, store.Escrow{VerifiedAt: &future}, c); got != "Due now: the next maintenance pass runs it" {
		t.Errorf("a proof 60 days ahead: %q", got)
	}
}

// G6: the retry hold is maintain.EscrowVerifyRetry itself (ui no longer keeps a copy): an attempt a
// minute short of it holds the key, one exactly that old does not. Killed mutant: the hold made two hours.
func TestP22R2bNextCheckHoldIsTheVerifiersRetry(t *testing.T) {
	t.Parallel()
	if maintain.EscrowVerifyRetry != time.Hour {
		t.Fatalf("EscrowVerifyRetry = %v", maintain.EscrowVerifyRetry)
	}
	c := fmtCtx{Now: time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC), Loc: time.UTC, Zone: "UTC"}
	id := "01JDEVICE00000000000000001"
	old := c.Now.Add(-30 * 24 * time.Hour)
	at := func(ago time.Duration) store.Escrow {
		a := c.Now.Add(-ago)
		return store.Escrow{VerifiedAt: &old, LastVerifyAttemptAt: &a}
	}
	if got := escrowNextCheck(id, at(maintain.EscrowVerifyRetry-time.Minute), c); !strings.Contains(got, "an hour after the last attempt") {
		t.Errorf("a minute inside the hold: %q", got)
	}
	if got := escrowNextCheck(id, at(maintain.EscrowVerifyRetry), c); strings.Contains(got, "an hour after the last attempt") {
		t.Errorf("exactly at the hold: %q", got)
	}
}
