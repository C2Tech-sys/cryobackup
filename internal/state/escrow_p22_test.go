package state

import (
	"strings"
	"testing"
	"time"
)

// P22-H (ADR 0030): a retired device's failing escrow verification raises no
// alarm on its own row (row 1) and is counted into banner C22 instead.

func escrowKey(id string, retired bool, e EscrowFacts) EscrowKeyFacts {
	if e.Custody == "" {
		e.Custody = "escrowed"
	}
	if e.CreatedAt == 0 {
		e.CreatedAt = ago(200 * day)
	}
	return EscrowKeyFacts{DeviceID: id, Retired: retired, Escrow: e}
}

func TestP22HRetiredEscrowFailureIsSuppressedOnTheDevice(t *testing.T) {
	f := baseFacts()
	f.RetiredAt = ago(10 * day)
	for name, e := range map[string]*EscrowFacts{
		"wrong_password": {Custody: "escrowed", CreatedAt: ago(200 * day), VerifiedAt: ago(20 * day), LastVerifyErrorClass: "wrong_password"},
		"overdue":        {Custody: "escrowed", CreatedAt: ago(200 * day), VerifiedAt: ago(90 * day)},
		"never":          {Custody: "escrowed", CreatedAt: ago(200 * day)},
	} {
		f.Escrow = e
		got := Classify(f, t0)
		if got.Primary != StateRetired || got.Alarm != SeverityNone || len(got.AlarmFlags) != 0 || hasState(got.Flags, StateEscrowUnverified) {
			t.Errorf("%s: a retired device alarms on its own row: %+v", name, got)
		}
	}
}

func TestP22HEscrowKeyFailingClasses(t *testing.T) {
	for _, c := range []struct {
		name string
		e    EscrowFacts
		want Reason // "" = not failing
	}{
		{"proven yesterday", EscrowFacts{VerifiedAt: ago(day)}, ""},
		{"proven 29 days ago", EscrowFacts{VerifiedAt: ago(29 * day)}, ""},
		{"proven 31 days ago (the verifier stopped)", EscrowFacts{VerifiedAt: ago(31 * day)}, ReasonEscrowRecheckOverdue},
		{"wrong_password", EscrowFacts{VerifiedAt: ago(day), LastVerifyErrorClass: "wrong_password"}, ReasonEscrowVerifyFailed},
		{"repo_missing", EscrowFacts{VerifiedAt: ago(day), LastVerifyErrorClass: "repo_missing"}, ReasonEscrowVerifyFailed},
		{"never proven, inside the grace", EscrowFacts{CreatedAt: ago(2 * hour)}, ""},
		{"never proven, past the grace", EscrowFacts{CreatedAt: ago(30 * hour)}, ReasonEscrowNeverVerified},
		{"garbled proof time", EscrowFacts{VerifiedAt: 12345}, ReasonEscrowNeverVerified},
		{"proof 60 days in the future (P22-R2b B2)", EscrowFacts{VerifiedAt: ahead(60 * day)}, ReasonEscrowNeverVerified},
		{"proof past the allowed skew", EscrowFacts{VerifiedAt: ahead(FutureSkewAllowed + time.Minute)}, ReasonEscrowNeverVerified},
		{"proof inside the allowed skew", EscrowFacts{VerifiedAt: ahead(time.Minute)}, ""},
	} {
		r, bad := EscrowKeyFailing(escrowKey("d", true, c.e), t0)
		if bad != (c.want != "") || r != c.want {
			t.Errorf("%s: %q %v, want %q", c.name, r, bad, c.want)
		}
	}
}

func TestP22HRetiredEscrowBannerCountsRetiredFailuresOnly(t *testing.T) {
	w := healthyFleet(1, 3)
	w.Devices[2].RetiredAt = ago(10 * day)
	keys := []EscrowKeyFacts{
		escrowKey(devID(1), false, EscrowFacts{VerifiedAt: ago(day), LastVerifyErrorClass: "wrong_password"}), // active: its own row
		escrowKey(devID(2), false, EscrowFacts{VerifiedAt: ago(day)}),
		escrowKey(devID(3), true, EscrowFacts{VerifiedAt: ago(day), LastVerifyErrorClass: "repo_missing"}),
		escrowKey("retired-old", true, EscrowFacts{VerifiedAt: ago(40 * day)}),
		escrowKey("retired-ok", true, EscrowFacts{VerifiedAt: ago(3 * day)}),
	}

	// Not supplied: nothing, and the view is as it was.
	if _, ok := findBanner(Evaluate(w, t0), escrowRetiredID); ok {
		t.Fatal("C22 raised without escrow facts")
	}

	w.EscrowVerify = &EscrowVerifyFacts{Keys: keys}
	v := Evaluate(w, t0)
	b, ok := findBanner(v, escrowRetiredID)
	if !ok || b.Count != 2 || b.Severity != SeverityAlarm || !strings.HasPrefix(b.Text, "Escrow verification failing for 2 retired devices") {
		t.Fatalf("C22 = %+v (%v), want an alarm counting 2 retired devices; banners %v", b, ok, bannerIDs(v))
	}
	if strings.Contains(b.Text, "retired-old") || strings.Contains(b.Text, devID(3)) {
		t.Errorf("the banner names a device: %q", b.Text)
	}
	if !hasRetired(v) {
		t.Error("the retired device is not RETIRED")
	}
	for _, d := range v.Devices {
		if d.ID == devID(3) && d.Alarm != SeverityNone {
			t.Errorf("the retired device alarms: %+v", d)
		}
	}

	// One: singular wording.
	w.EscrowVerify = &EscrowVerifyFacts{Keys: keys[3:]}
	if b, _ := findBanner(Evaluate(w, t0), escrowRetiredID); b.Count != 1 || !strings.HasPrefix(b.Text, "Escrow verification failing for 1 retired device:") {
		t.Errorf("one failing: %+v", b)
	}
	// None failing: no banner, and the view can read all clear.
	w.EscrowVerify = &EscrowVerifyFacts{Keys: keys[1:2]}
	if v := Evaluate(w, t0); len(v.Banners) != 0 || !v.AllClear() {
		t.Errorf("no failing retired key: banners %v", bannerIDs(v))
	}
	// Unread: the not-checked warning, never quiet and never all clear.
	w.EscrowVerify = &EscrowVerifyFacts{NotRead: "the escrow rows could not be read"}
	v = Evaluate(w, t0)
	if b, ok := findBanner(v, escrowRetiredUncheckedID); !ok || b.Severity != SeverityAlarm || !strings.Contains(b.Text, "not checked: the escrow rows could not be read") || v.AllClear() {
		t.Errorf("unread keys: %+v %v (all clear %v)", b, ok, v.AllClear())
	}
}

func hasRetired(v FleetView) bool {
	for _, d := range v.Devices {
		if d.Primary == StateRetired {
			return true
		}
	}
	return false
}

func TestP22HEscrowSummaryNeverGreenWhenStale(t *testing.T) {
	now := t0
	f := &EscrowVerifyFacts{Keys: []EscrowKeyFacts{
		escrowKey("fresh", false, EscrowFacts{VerifiedAt: ago(2 * day)}),
		escrowKey("lagging", false, EscrowFacts{VerifiedAt: ago(EscrowVerifyMaxAge + time.Hour)}),
		escrowKey("never-new", false, EscrowFacts{CreatedAt: ago(time.Hour)}),
		escrowKey("never-old", true, EscrowFacts{CreatedAt: ago(3 * day)}),
		escrowKey("failed", true, EscrowFacts{VerifiedAt: ago(2 * day), LastVerifyErrorClass: "wrong_password"}),
		escrowKey("garbled", false, EscrowFacts{VerifiedAt: ahead(time.Hour)}),
	}}
	got := SummarizeEscrowVerification(f, now)
	want := EscrowVerifySummary{Keys: 6, Retired: 2, Failing: 1, Overdue: 3, RetiredFailing: 2}
	if got != want {
		t.Fatalf("summary = %+v, want %+v", got, want)
	}
	if s := SummarizeEscrowVerification(nil, now); s != (EscrowVerifySummary{}) {
		t.Errorf("nil facts: %+v", s)
	}
}
