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

// TestP22R2bEscrowRecheckZeroIsOff: policy sla.escrow_recheck_days 0 is "recheck off" (policy
// validation allows it): a proven key's age never raises ESCROW_UNVERIFIED, while a recorded failure and a
// never-verified key still do. Killed mutant: the EscrowRecheck > 0 guard dropped (every key overdue at once).
func TestP22R2bEscrowRecheckZeroIsOff(t *testing.T) {
	th := DefaultThresholds()
	th.EscrowRecheck = 0
	check := func(e EscrowFacts) (Reason, bool) {
		e.Custody, e.CreatedAt = "escrowed", ago(200*day)
		f := DeviceFacts{Escrow: &e}
		r, _, bad := escrowIssueRecheck(&f, th, t0, time.Time{}, false, true)
		return r, bad
	}
	if r, bad := check(EscrowFacts{VerifiedAt: ago(365 * day)}); bad {
		t.Errorf("a year-old proof with the recheck off: %q", r)
	}
	if r, bad := check(EscrowFacts{VerifiedAt: ago(day), LastVerifyErrorClass: "wrong_password"}); !bad || r != ReasonEscrowVerifyFailed {
		t.Errorf("a recorded failure with the recheck off: %q %v", r, bad)
	}
	if r, bad := check(EscrowFacts{}); !bad || r != ReasonEscrowNeverVerified {
		t.Errorf("a never-verified key with the recheck off: %q %v", r, bad)
	}
	th.EscrowRecheck = 30 * 24 * time.Hour
	if r, bad := check(EscrowFacts{VerifiedAt: ago(31 * day)}); !bad || r != ReasonEscrowRecheckOverdue {
		t.Errorf("a 31-day-old proof with a 30-day recheck: %q %v", r, bad)
	}
}
