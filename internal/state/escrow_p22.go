package state

import (
	"strconv"
	"time"
)

// Escrow verification across the fleet (ADR 0030). The maintenance scheduler
// re-proves every escrowed key weekly, retired devices included. An active
// device whose key fails is its own ESCROW_UNVERIFIED (row 12); a retired
// device raises no alarm of its own (row 1 suppresses everything), so its
// failures are counted here instead, into banner C22, and the System page
// reads the same count.

// EscrowKeyFacts is one key the verifier checks: an active escrowed key of a
// device whose enrollment was confirmed and that is not being purged.
type EscrowKeyFacts struct {
	// DeviceID names the device; Retired is set when the device is retired.
	DeviceID string
	Retired  bool
	// Escrow is the key's escrow row (Custody is "escrowed").
	Escrow EscrowFacts
	// LastAttemptAt is the newest verification attempt, whatever its outcome; zero means never.
	LastAttemptAt Millis
}

// EscrowVerifyFacts is the verifier's whole candidate set, as the console's
// loader read it (store.EscrowVerifyKeys).
type EscrowVerifyFacts struct {
	Keys []EscrowKeyFacts
	// NotRead says why the keys could not be read ("" = they were), path-free. The retired
	// devices' verification is then "not checked" (C22b), never judged on empty facts.
	NotRead string
}

// Banner C22 and its "not checked" warning.
const (
	escrowRetiredID          = "C22"
	escrowRetiredUncheckedID = "C22b"
)

// EscrowKeyFailing reports whether one key's verification is failing, by the
// row 12 rule with the recheck on (EscrowRecheckEnforced) and the default
// thresholds: the last verification failed (wrong_password, repo_missing), the
// key was never proven and the grace is gone, or its newest proof is older than
// the recheck interval. A retired device has no policy thresholds loaded (row 1
// returns before the policy is read), so the defaults apply to every key judged
// here; an active device's own state uses its policy's.
func EscrowKeyFailing(k EscrowKeyFacts, now time.Time) (Reason, bool) {
	e := k.Escrow
	if e.Custody == "" {
		e.Custody = custodyEscrowed
	}
	f := DeviceFacts{Escrow: &e}
	reason, _, bad := escrowIssueRecheck(&f, DefaultThresholds(), now.UTC(), time.Time{}, false, EscrowRecheckEnforced)
	return reason, bad
}

// EscrowVerifySummary is what the System page's escrow row says, in counts.
type EscrowVerifySummary struct {
	// Keys is how many keys the verifier checks; Retired how many of them belong to retired devices.
	Keys, Retired int
	// Failing is how many keys have a recorded failed verification (wrong_password, repo_missing).
	Failing int
	// Overdue is how many keys the verifier has not proven in EscrowVerifyMaxAge (never proven
	// once the grace is gone, or proven longer ago): the verifier is not keeping up, or cannot run.
	Overdue int
	// RetiredFailing is how many retired devices' keys fail EscrowKeyFailing: banner C22's count.
	RetiredFailing int
}

// SummarizeEscrowVerification counts f's keys at now. Nil facts are an empty summary.
func SummarizeEscrowVerification(f *EscrowVerifyFacts, now time.Time) EscrowVerifySummary {
	var s EscrowVerifySummary
	if f == nil {
		return s
	}
	now = now.UTC()
	grace := DefaultThresholds().EscrowVerifyGrace
	for _, k := range f.Keys {
		s.Keys++
		if k.Retired {
			s.Retired++
		}
		if k.Escrow.LastVerifyErrorClass != "" {
			s.Failing++
		}
		if escrowProofOverdue(k.Escrow, grace, now) {
			s.Overdue++
		}
		if _, bad := EscrowKeyFailing(k, now); bad && k.Retired {
			s.RetiredFailing++
		}
	}
	return s
}

// escrowProofOverdue: the key's newest proof is older than EscrowVerifyMaxAge, or it was never
// proven and grace has passed since it was escrowed. A garbled time is overdue: it proves nothing.
func escrowProofOverdue(e EscrowFacts, grace time.Duration, now time.Time) bool {
	if e.VerifiedAt != 0 {
		at, ok := recorded(e.VerifiedAt, now)
		return !ok || now.Sub(at) > EscrowVerifyMaxAge
	}
	created, ok := recorded(e.CreatedAt, now)
	return !ok || now.Sub(created) > grace
}

// escrowVerifyBanners is C22: retired devices whose escrowed key fails its
// verification, which RETIRED keeps from alarming on their own rows; C22b when
// the keys could not be read. Nil facts raise nothing: the loader did not
// supply them (World.EscrowVerify).
func escrowVerifyBanners(f *EscrowVerifyFacts, now time.Time) []Banner {
	switch {
	case f == nil:
		return nil
	case f.NotRead != "":
		return []Banner{notChecked(escrowRetiredUncheckedID, "Escrow verification of retired devices",
			"a retired device whose escrowed key no longer opens its repository would go unnoticed", f.NotRead, 0)}
	}
	n := SummarizeEscrowVerification(f, now).RetiredFailing
	if n == 0 {
		return nil
	}
	what := strconv.Itoa(n) + " retired devices"
	if n == 1 {
		what = "1 retired device"
	}
	return []Banner{{ID: escrowRetiredID, Severity: SeverityAlarm, Count: n,
		Text: "Escrow verification failing for " + what + ": the escrowed key may no longer open the repository, so a restore from it may fail."}}
}
