package state

import "testing"

// M2-FIX-B1 (lab finding F7, ADR 0014): until the periodic escrow verification
// job exists (M5), a verification that is only older than escrow_recheck_days
// does not raise ESCROW_UNVERIFIED. When that job lands, set
// EscrowRecheckEnforced to true and flip the first assertion and the B37 case
// in classify_test.go; the rule itself is tested below with the switch on.
func TestEscrowRecheckIsNotEnforcedUntilTheVerifyJobExists(t *testing.T) {
	if EscrowRecheckEnforced {
		t.Fatal("EscrowRecheckEnforced is on: only the change that adds the periodic escrow verification job (M5) may turn it on, and it updates this test and B37")
	}
	th := DefaultThresholds()
	enrolled := t0.Add(-90 * day)
	overdue := func() DeviceFacts {
		f := baseFacts()
		f.Escrow = &EscrowFacts{Custody: "escrowed", CreatedAt: ago(90 * day), VerifiedAt: ago(40 * day)}
		return f
	}

	// Off (today): a 40-day-old verification against a 30-day recheck is fine.
	f := overdue()
	if r, _, bad := escrowIssue(&f, th, t0, enrolled, true); bad {
		t.Fatalf("recheck overdue raised while the rule is off: %s", r)
	}
	if got := Classify(f, t0); got.Primary != StateOK {
		t.Fatalf("Classify = %s, want OK", got.Primary)
	}

	// On (M5): the same facts raise escrow_recheck_overdue, from the verification plus the recheck interval.
	f = overdue()
	r, since, bad := escrowIssueRecheck(&f, th, t0, enrolled, true, true)
	if !bad || r != ReasonEscrowRecheckOverdue || !since.Equal(t0.Add(-40*day).Add(th.EscrowRecheck)) {
		t.Fatalf("with the rule on: %s %v %v", r, since, bad)
	}
	// Inside the recheck interval it stays quiet with the rule on too.
	f.Escrow.VerifiedAt = ago(29 * day)
	if r, _, bad := escrowIssueRecheck(&f, th, t0, enrolled, true, true); bad {
		t.Fatalf("29-day-old verification with the rule on: %s", r)
	}

	// The other three causes are raised with the rule off.
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
		f := overdue()
		c.mod(&f)
		r, _, bad := escrowIssue(&f, th, t0, enrolled, true)
		if !bad || r != c.want {
			t.Errorf("%s: %s %v, want %s", c.name, r, bad, c.want)
		}
		if got := Classify(f, t0); got.Primary != StateEscrowUnverified || got.Reason != c.want {
			t.Errorf("%s: Classify = %s %s", c.name, got.Primary, got.Reason)
		}
	}
}
