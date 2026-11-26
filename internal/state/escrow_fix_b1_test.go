package state

import "testing"

// M2-FIX-B1 (lab finding F7, ADR 0014) kept the recheck off until a periodic
// escrow verification existed. ADR 0030 adds it (the maintenance scheduler
// re-proves every escrowed key weekly) and turns the rule on: a verification
// that is only older than escrow_recheck_days raises ESCROW_UNVERIFIED, reason
// escrow_recheck_overdue. The rule is still tested both ways through
// escrowIssueRecheck.
func TestEscrowRecheckIsEnforcedOnceTheVerifyJobExists(t *testing.T) {
	if !EscrowRecheckEnforced {
		t.Fatal("EscrowRecheckEnforced is off: ADR 0030 turned it on with the weekly verifier; turning it off again needs a new ADR")
	}
	if EscrowVerifyEvery >= DefaultThresholds().EscrowRecheck {
		t.Fatalf("the verifier's cadence %v must be shorter than the default recheck %v, or a healthy key alarms between checks", EscrowVerifyEvery, DefaultThresholds().EscrowRecheck)
	}
	th := DefaultThresholds()
	enrolled := t0.Add(-90 * day)
	overdue := func() DeviceFacts {
		f := baseFacts()
		f.Escrow = &EscrowFacts{Custody: "escrowed", CreatedAt: ago(90 * day), VerifiedAt: ago(40 * day)}
		return f
	}

	// On (the default now): a 40-day-old verification against a 30-day recheck is overdue,
	// from the verification plus the recheck interval.
	f := overdue()
	r, since, bad := escrowIssue(&f, th, t0, enrolled, true)
	if !bad || r != ReasonEscrowRecheckOverdue || !since.Equal(t0.Add(-40*day).Add(th.EscrowRecheck)) {
		t.Fatalf("with the rule on: %s %v %v", r, since, bad)
	}
	if got := Classify(f, t0); got.Primary != StateEscrowUnverified || got.Reason != ReasonEscrowRecheckOverdue {
		t.Fatalf("Classify = %s %s, want ESCROW_UNVERIFIED escrow_recheck_overdue", got.Primary, got.Reason)
	}
	// Inside the recheck interval it stays quiet.
	f.Escrow.VerifiedAt = ago(29 * day)
	if r, _, bad := escrowIssue(&f, th, t0, enrolled, true); bad {
		t.Fatalf("29-day-old verification: %s", r)
	}
	// A key the weekly verifier keeps proving never comes near the recheck.
	f.Escrow.VerifiedAt = ago(EscrowVerifyMaxAge)
	if r, _, bad := escrowIssue(&f, th, t0, enrolled, true); bad {
		t.Fatalf("a key proven one cadence and a day ago: %s", r)
	}

	// Off (the explicit switch, as ADR 0014 had it): the same facts are fine.
	f = overdue()
	if r, _, bad := escrowIssueRecheck(&f, th, t0, enrolled, true, false); bad {
		t.Fatalf("recheck overdue raised with the rule off: %s", r)
	}

	// The other three causes are raised either way.
	cases := []struct {
		name string
		mod  func(*DeviceFacts)
		want Reason
	}{
		{"missing", func(f *DeviceFacts) { f.Escrow = nil }, ReasonEscrowMissing},
		{"verify_failed", func(f *DeviceFacts) { f.Escrow.LastVerifyErrorClass = "wrong_password" }, ReasonEscrowVerifyFailed},
		{"never_verified_after_grace", func(f *DeviceFacts) { f.Escrow.VerifiedAt = 0 }, ReasonEscrowNeverVerified},
	}
	for _, c := range cases {
		for _, on := range []bool{true, false} {
			f := overdue()
			c.mod(&f)
			r, _, bad := escrowIssueRecheck(&f, th, t0, enrolled, true, on)
			if !bad || r != c.want {
				t.Errorf("%s (recheck %v): %s %v, want %s", c.name, on, r, bad, c.want)
			}
		}
		f := overdue()
		c.mod(&f)
		if got := Classify(f, t0); got.Primary != StateEscrowUnverified || got.Reason != c.want {
			t.Errorf("%s: Classify = %s %s", c.name, got.Primary, got.Reason)
		}
	}
}
