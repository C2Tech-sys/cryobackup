package state

import (
	"fmt"
	"reflect"
	"testing"
	"time"
)

// t0 is the frozen "now" every classification test runs at (the reference
// suite's frozen-now approach). Nothing in this package's tests reads a clock.
var t0 = time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

const (
	hour = time.Hour
	day  = 24 * time.Hour
)

func ms(t time.Time) Millis { return Millis(t.UnixMilli()) }

// ago is the raw timestamp d before t0; ahead is d after it.
func ago(d time.Duration) Millis   { return ms(t0.Add(-d)) }
func ahead(d time.Duration) Millis { return ms(t0.Add(d)) }

// devID returns a fixed 26-character device id.
func devID(n int) string { return fmt.Sprintf("DEV%023d", n) }

// custID returns a fixed 26-character customer id.
func custID(n int) string { return fmt.Sprintf("CUS%023d", n) }

func successRun(finishedAgo time.Duration) *RunFacts {
	return &RunFacts{
		Outcome:     "success",
		StartedAt:   ago(finishedAgo + 10*time.Minute),
		FinishedAt:  ago(finishedAgo),
		TotalFiles:  90000,
		CountsKnown: true,
	}
}

func failedRun(finishedAgo time.Duration, class string) *RunFacts {
	return &RunFacts{
		Outcome:     "failed",
		StartedAt:   ago(finishedAgo + 5*time.Minute),
		FinishedAt:  ago(finishedAgo),
		ErrorClass:  class,
		CountsKnown: true,
	}
}

// baseFacts is a completely healthy workstation at t0: success 2 h ago, seen
// 30 s ago, escrow verified, maintenance fresh, versions equal, defaults.
func baseFacts() DeviceFacts {
	return DeviceFacts{
		ID:                 devID(1),
		CustomerID:         custID(1),
		Hostname:           "ws-reception-01",
		DeviceClass:        "workstation",
		EnrolledAt:         ago(30 * day),
		EnrollConfirmedAt:  ago(30*day - time.Hour),
		FirstBackupDueBy:   ago(27 * day),
		LastSeen:           ago(30 * time.Second),
		ServiceState:       "idle",
		LastSuccess:        ago(2 * hour),
		LastBackup:         successRun(2 * hour),
		LastAttemptStart:   ago(2*hour + 10*time.Minute),
		AgentVersion:       "1.5.0",
		ExpectedVersion:    "1.5.0",
		Escrow:             &EscrowFacts{Custody: "escrowed", CreatedAt: ago(30 * day), VerifiedAt: ago(1 * day)},
		CustomerKeyCustody: "escrowed",
		Maintenance: &MaintFacts{
			LastForget:          successRun(1 * day),
			LastPrune:           successRun(1 * day),
			LastCheck:           successRun(1 * day),
			LastForgetSuccessAt: ago(1 * day),
			ForgetCadence:       7 * day,
		},
		PolicyVersionApplied: 3,
		PolicyVersion:        3,
		HasSchedule:          true,
		Thresholds:           DefaultThresholds(),
		Complete:             true,
	}
}

// newlyEnrolled turns f into a device enrolled enrolledAgo ago that has never
// succeeded and whose first-backup deadline is 72 h after enrollment.
func newlyEnrolled(f *DeviceFacts, enrolledAgo time.Duration) {
	f.EnrolledAt = ago(enrolledAgo)
	f.EnrollConfirmedAt = ago(enrolledAgo - 5*time.Minute)
	f.FirstBackupDueBy = ms(t0.Add(-enrolledAgo + 72*hour))
	f.LastSuccess = 0
	f.LastBackup = nil
	f.LastAttemptStart = 0
	f.Escrow = &EscrowFacts{Custody: "escrowed", CreatedAt: ago(enrolledAgo - time.Minute), VerifiedAt: ago(enrolledAgo / 2)}
}

// graceExpired makes f a device that never succeeded and whose grace ended long ago.
func graceExpired(f *DeviceFacts) {
	newlyEnrolled(f, 10*day)
	f.FirstBackupDueBy = ago(7 * day)
}

// expectation is the declarative half of a classification row.
type expectation struct {
	primary State
	flags   []State // exact, in order; nil means "do not check"
	alarm   string  // "none", "alarm", "critical"; "" means "do not check"
	reason  Reason  // "" means "do not check"
	attrs   []Attr  // must all be present
	noAttrs []Attr  // must all be absent
	exact   []Attr  // when non-nil, the exact attribute list
	check   func(t *testing.T, got DeviceState)
}

func (w expectation) verify(t *testing.T, got DeviceState) {
	t.Helper()
	if got.Primary != w.primary {
		t.Errorf("primary = %s, want %s (flags %v)", got.Primary, w.primary, got.Flags)
	}
	if w.flags != nil && !reflect.DeepEqual(got.Flags, w.flags) {
		t.Errorf("flags = %v, want %v", got.Flags, w.flags)
	}
	if len(got.Flags) == 0 || got.Flags[0] != got.Primary {
		t.Errorf("flags %v do not start with primary %s", got.Flags, got.Primary)
	}
	if w.alarm != "" && got.Alarm.String() != w.alarm {
		t.Errorf("alarm = %s, want %s", got.Alarm, w.alarm)
	}
	if w.reason != "" && got.Reason != w.reason {
		t.Errorf("reason = %q, want %q", got.Reason, w.reason)
	}
	for _, a := range w.attrs {
		if !hasAttr(got.Attrs, a) {
			t.Errorf("attrs %v missing %s", got.Attrs, a)
		}
	}
	for _, a := range w.noAttrs {
		if hasAttr(got.Attrs, a) {
			t.Errorf("attrs %v should not contain %s", got.Attrs, a)
		}
	}
	if w.exact != nil && !reflect.DeepEqual(got.Attrs, w.exact) {
		t.Errorf("attrs = %v, want exactly %v", got.Attrs, w.exact)
	}
	if w.check != nil {
		w.check(t, got)
	}
}

func hasState(flags []State, s State) bool {
	for _, f := range flags {
		if f == s {
			return true
		}
	}
	return false
}

// classifyCase is one row of a table-driven classification test.
type classifyCase struct {
	name string
	mod  func(f *DeviceFacts)
	now  time.Time // zero means t0
	want expectation
}

func runClassifyCases(t *testing.T, cases []classifyCase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := baseFacts()
			if c.mod != nil {
				c.mod(&f)
			}
			now := c.now
			if now.IsZero() {
				now = t0
			}
			c.want.verify(t, Classify(f, now))
		})
	}
}
