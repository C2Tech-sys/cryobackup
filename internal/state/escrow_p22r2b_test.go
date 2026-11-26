package state

import (
	"testing"
	"time"
)

// P22-R2b B2, G2, G4: a future proof time never reads green; the cadence is the literal week; C22 is
// an alarm.

// TestP22R2bFutureEscrowProofIsNotGreen: a proof time later than now + FutureSkewAllowed (a console
// clock that ran ahead when the key was verified) proves nothing: the device's own row 12, the System
// summary and banner C22 for a retired device all treat it as unverified, as every other recorded
// check time is treated (recorded()). Killed mutant: escrowIssueRecheck back on VerifiedAt.Time().
func TestP22R2bFutureEscrowProofIsNotGreen(t *testing.T) {
	future := EscrowFacts{Custody: "escrowed", CreatedAt: ago(200 * day), VerifiedAt: ahead(60 * day)}

	// Row 12 on an active device.
	f := baseFacts()
	f.Escrow = &future
	got := Classify(f, t0)
	if !hasState(got.Flags, StateEscrowUnverified) {
		t.Fatalf("a proof 60 days ahead: flags %v, want ESCROW_UNVERIFIED", got.Flags)
	}
	// Inside the skew it is still a proof.
	near := future
	near.VerifiedAt = ahead(time.Minute)
	f.Escrow = &near
	if hasState(Classify(f, t0).Flags, StateEscrowUnverified) {
		t.Fatal("a proof a minute ahead (inside the allowed skew) raised ESCROW_UNVERIFIED")
	}

	// The System row's summary counts it overdue (it already did, through recorded()), and the engine's
	// failing rule agrees with it.
	k := EscrowKeyFacts{DeviceID: "d", Retired: true, Escrow: future}
	if s := SummarizeEscrowVerification(&EscrowVerifyFacts{Keys: []EscrowKeyFacts{k}}, t0); s.Overdue != 1 || s.RetiredFailing != 1 {
		t.Fatalf("summary = %+v, want overdue 1 and retired failing 1", s)
	}
	if r, bad := EscrowKeyFailing(k, t0); !bad || r != ReasonEscrowNeverVerified {
		t.Fatalf("EscrowKeyFailing = %q %v", r, bad)
	}

	// Banner C22 counts the retired device whose proof is in the future.
	w := healthyFleet(1, 3)
	w.Devices[2].RetiredAt = ago(10 * day)
	w.EscrowVerify = &EscrowVerifyFacts{Keys: []EscrowKeyFacts{escrowKey(devID(3), true, EscrowFacts{VerifiedAt: ahead(60 * day)})}}
	if b, ok := findBanner(Evaluate(w, t0), escrowRetiredID); !ok || b.Count != 1 {
		t.Fatalf("C22 for a retired device proven 60 days ahead: %+v %v", b, ok)
	}
}

// TestP22R2bEscrowCadenceIsAWeek pins the verifier's cadence to its literal (the due time, the
// System page's max age and the device page all derive from it). Killed mutants: M2 (6 days) and
// M2b (8 days).
func TestP22R2bEscrowCadenceIsAWeek(t *testing.T) {
	if EscrowVerifyEvery != 7*24*time.Hour {
		t.Fatalf("EscrowVerifyEvery = %v, want 7 days", EscrowVerifyEvery)
	}
	if EscrowVerifyMaxAge != 8*24*time.Hour {
		t.Fatalf("EscrowVerifyMaxAge = %v, want 8 days (a cadence and a day)", EscrowVerifyMaxAge)
	}
}

// TestP22R2bRetiredEscrowBannerIsAnAlarm: ADR 0030 makes C22 the substitute for the retired device's
// own ESCROW_UNVERIFIED alarm, so it is an alarm, not a warning (state/doc.go says so). Killed mutant:
// SeverityWarning on C22.
func TestP22R2bRetiredEscrowBannerIsAnAlarm(t *testing.T) {
	w := healthyFleet(1, 3)
	w.Devices[2].RetiredAt = ago(10 * day)
	w.EscrowVerify = &EscrowVerifyFacts{Keys: []EscrowKeyFacts{escrowKey(devID(3), true, EscrowFacts{VerifiedAt: ago(day), LastVerifyErrorClass: "wrong_password"})}}
	b, ok := findBanner(Evaluate(w, t0), escrowRetiredID)
	if !ok || b.Severity != SeverityAlarm {
		t.Fatalf("C22 = %+v (%v), want severity alarm", b, ok)
	}
}

// TestP22R2bEscrowRecheckZeroKeepsItsMeaning: policy sla.escrow_recheck_days 0 is a recheck interval of
// zero days (ADR 0033), which is what it has been since the recheck became enforced: every proven key is
// overdue at once. It is never "off": ESCROW_UNVERIFIED stays raised. Killed mutant: a guard that skips the
// recheck when the interval is 0.
func TestP22R2bEscrowRecheckZeroKeepsItsMeaning(t *testing.T) {
	th := DefaultThresholds()
	th.EscrowRecheck = 0
	e := EscrowFacts{Custody: "escrowed", CreatedAt: ago(200 * day), VerifiedAt: ago(time.Minute)}
	f := DeviceFacts{Escrow: &e}
	if r, _, bad := escrowIssueRecheck(&f, th, t0, time.Time{}, false, true); !bad || r != ReasonEscrowRecheckOverdue {
		t.Errorf("a key proven a minute ago with a recheck of 0 days: %q %v, want overdue", r, bad)
	}
	th.EscrowRecheck = 30 * 24 * time.Hour
	if r, _, bad := escrowIssueRecheck(&f, th, t0, time.Time{}, false, true); bad {
		t.Errorf("the same key with 30 days: %q", r)
	}
}
