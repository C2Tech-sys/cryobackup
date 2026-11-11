package uplink

import (
	"testing"
	"time"
)

// ADR 0018 decision 2: the uplink backoff doubles from one minute and stops at
// five (it stopped at thirty), and the cap never overrides a Retry-After.
func TestBackoffCapsAtFiveMinutes(t *testing.T) {
	if BackoffMax != 5*time.Minute {
		t.Fatalf("BackoffMax = %v, want 5m", BackoffMax)
	}
	want := []time.Duration{1, 2, 4, 5, 5, 5, 5} // minutes, failures 0..6, before jitter
	b := NewBackoff()
	b.Jitter = 0
	for failures, w := range want {
		if got := b.DelayFor(failures); got != w*time.Minute {
			t.Errorf("DelayFor(%d) = %v, want %v", failures, got, w*time.Minute)
		}
	}
	for i, w := range want { // Next walks the same table and counts the failures
		if got := b.Next(); got != w*time.Minute {
			t.Errorf("Next #%d = %v, want %v", i, got, w*time.Minute)
		}
	}
	if b.Failures() != len(want) {
		t.Errorf("Failures = %d, want %d", b.Failures(), len(want))
	}
	// DelayFor does not move the counter.
	b.DelayFor(2)
	if b.Failures() != len(want) {
		t.Errorf("DelayFor changed Failures to %d", b.Failures())
	}
}

// With the standard 20 % jitter the delay never passes the new cap.
func TestBackoffJitterStaysUnderTheNewCap(t *testing.T) {
	b := NewBackoff()
	b.Rand = func() float64 { return 0.9999999 }
	for i := 0; i < 12; i++ {
		if d := b.Next(); d > 5*time.Minute {
			t.Fatalf("step %d = %v, above 5m", i, d)
		}
	}
}

// A Retry-After longer than the cap is honoured exactly: the cap bounds the
// agent's own guess, not the console's instruction.
func TestRetryAfterBeyondTheCapIsHonoured(t *testing.T) {
	b := NewBackoff()
	b.Jitter = 0
	for i := 0; i < 6; i++ {
		b.Next() // deep in the backoff
	}
	err := &Error{Status: 503, RetryAfter: 10 * time.Minute}
	if d := b.Delay(err); d != 10*time.Minute {
		t.Errorf("Delay = %v, want the 10 min Retry-After exactly", d)
	}
	if d := b.Delay(&Error{Status: 503}); d != 5*time.Minute {
		t.Errorf("Delay without Retry-After = %v, want the 5 min cap", d)
	}
}
