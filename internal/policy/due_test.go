package policy

import (
	"fmt"
	"testing"
	"time"
)

const fixtureDevice = "01FIXTUREDEVICE0000000000A"

func intervalSchedule(minutes int, catchup bool, window string) Schedule {
	s := Default(ClassUnknown).Schedule
	s.IntervalMinutes = minutes
	s.Catchup = catchup
	s.Window = window
	return s
}

func fixedSchedule(catchup bool, window string, times ...string) Schedule {
	s := Default(ClassUnknown).Schedule
	s.Mode = ModeFixed
	s.FixedTimes = times
	s.Catchup = catchup
	s.Window = window
	return s
}

func TestDueInterval(t *testing.T) {
	chi := chicago(t)
	const interval = 24 * 60
	sched := intervalSchedule(interval, true, "*/0-24")
	offset := StableOffset(fixtureDevice, offsetCap(24*time.Hour))
	last := at(chi, 2026, 6, 1, 3, 0)
	next := last.Add(24*time.Hour + offset)
	if offset == 0 {
		t.Fatal("fixture device must have a non-zero offset for these cases to mean anything")
	}

	tests := []struct {
		name       string
		in         DueInput
		wantDue    bool
		wantReason string
		notBefore  time.Time
		slot       time.Time
	}{
		{"never succeeded", DueInput{Now: last, Schedule: sched, DeviceID: fixtureDevice}, true, "never", time.Time{}, last},
		{"never succeeded, attempted and failed: still due, backoff is the agent's",
			DueInput{Now: last, LastAttemptStart: last.Add(-time.Hour), Schedule: sched, DeviceID: fixtureDevice}, true, "never", time.Time{}, last},
		{"inside the interval", DueInput{Now: last.Add(12 * time.Hour), LastSuccess: last, Schedule: sched, DeviceID: fixtureDevice},
			false, "not_yet", next, last},
		{"one second before next", DueInput{Now: next.Add(-time.Second), LastSuccess: last, Schedule: sched, DeviceID: fixtureDevice},
			false, "not_yet", next, last.Add(24 * time.Hour)},
		{"exactly at next is due (inclusive)", DueInput{Now: next, LastSuccess: last, Schedule: sched, DeviceID: fixtureDevice},
			true, "interval", time.Time{}, last.Add(24 * time.Hour)},
		{"one missed interval collapses to one run", DueInput{Now: last.Add(2*24*time.Hour - time.Minute), LastSuccess: last, Schedule: sched, DeviceID: fixtureDevice},
			true, "interval", time.Time{}, last.Add(24 * time.Hour)},
		{"two intervals late is a catch-up", DueInput{Now: next.Add(24 * time.Hour), LastSuccess: last, Schedule: sched, DeviceID: fixtureDevice},
			true, "catchup", time.Time{}, last.Add(2 * 24 * time.Hour)},
		{"seven missed intervals still one run", DueInput{Now: last.Add(7*24*time.Hour + 5*time.Hour), LastSuccess: last, Schedule: sched, DeviceID: fixtureDevice},
			true, "catchup", time.Time{}, last.Add(7 * 24 * time.Hour)},
		{"clock went backwards: run now and say so", DueInput{Now: last.Add(-3 * 24 * time.Hour), LastSuccess: last, Schedule: sched, DeviceID: fixtureDevice},
			true, "clock_skew", time.Time{}, last.Add(-3 * 24 * time.Hour)},
		{"zero interval falls back to a day", DueInput{Now: last.Add(time.Hour), LastSuccess: last, Schedule: intervalSchedule(0, true, "*/0-24"), DeviceID: fixtureDevice},
			false, "not_yet", next, last},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Due(tc.in)
			if got.Due != tc.wantDue || got.Reason != tc.wantReason {
				t.Fatalf("Due = %v/%q, want %v/%q", got.Due, got.Reason, tc.wantDue, tc.wantReason)
			}
			if !got.NotBefore.Equal(tc.notBefore) {
				t.Errorf("NotBefore = %v, want %v", got.NotBefore, tc.notBefore)
			}
			if !got.SlotStart.Equal(tc.slot) {
				t.Errorf("SlotStart = %v, want %v", got.SlotStart, tc.slot)
			}
		})
	}
}

func TestDueNoCatchup(t *testing.T) {
	chi := chicago(t)
	sched := intervalSchedule(24*60, false, "*/0-24")
	offset := StableOffset(fixtureDevice, offsetCap(24*time.Hour))
	last := at(chi, 2026, 6, 1, 3, 0)
	slot1 := last.Add(24 * time.Hour)
	slot7 := last.Add(7 * 24 * time.Hour)

	tests := []struct {
		name       string
		in         DueInput
		wantDue    bool
		wantReason string
		notBefore  time.Time
		slot       time.Time
	}{
		{"first slot not attempted", DueInput{Now: slot1.Add(offset), LastSuccess: last, LastAttemptStart: last.Add(-time.Minute), Schedule: sched, DeviceID: fixtureDevice},
			true, "interval", time.Time{}, slot1},
		{"first slot attempted and failed", DueInput{Now: slot1.Add(offset + time.Hour), LastSuccess: last, LastAttemptStart: slot1.Add(offset), Schedule: sched, DeviceID: fixtureDevice},
			false, "slot_attempted", slot1.Add(24*time.Hour + offset), slot1},
		{"a week asleep: only the current slot may run, once", DueInput{Now: slot7.Add(time.Hour), LastSuccess: last, LastAttemptStart: last.Add(-time.Minute), Schedule: sched, DeviceID: fixtureDevice},
			true, "interval", time.Time{}, slot7},
		{"an attempt in an old slot does not block the current one", DueInput{Now: slot7.Add(time.Hour), LastSuccess: last, LastAttemptStart: slot1.Add(offset), Schedule: sched, DeviceID: fixtureDevice},
			true, "interval", time.Time{}, slot7},
		{"current slot already attempted after a week asleep", DueInput{Now: slot7.Add(time.Hour), LastSuccess: last, LastAttemptStart: slot7.Add(10 * time.Minute), Schedule: sched, DeviceID: fixtureDevice},
			false, "slot_attempted", slot7.Add(24*time.Hour + offset), slot7},
		{"before the first slot opens", DueInput{Now: slot1.Add(offset - time.Second), LastSuccess: last, Schedule: sched, DeviceID: fixtureDevice},
			false, "not_yet", slot1.Add(offset), slot1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Due(tc.in)
			if got.Due != tc.wantDue || got.Reason != tc.wantReason {
				t.Fatalf("Due = %v/%q, want %v/%q", got.Due, got.Reason, tc.wantDue, tc.wantReason)
			}
			if !got.NotBefore.Equal(tc.notBefore) {
				t.Errorf("NotBefore = %v, want %v", got.NotBefore, tc.notBefore)
			}
			if !got.SlotStart.Equal(tc.slot) {
				t.Errorf("SlotStart = %v, want %v", got.SlotStart, tc.slot)
			}
		})
	}
}

func TestDueWindowGatesStartsOnly(t *testing.T) {
	chi := chicago(t)
	sched := intervalSchedule(24*60, true, "Mon-Fri/8-18")
	offset := StableOffset(fixtureDevice, offsetCap(24*time.Hour))
	last := at(chi, 2026, 6, 5, 9, 0) // Friday
	next := last.Add(24*time.Hour + offset)

	t.Run("due on Saturday morning, window closed until Monday", func(t *testing.T) {
		now := next // Saturday
		got := Due(DueInput{Now: now, LastSuccess: last, Schedule: sched, DeviceID: fixtureDevice})
		if got.Due || got.Reason != "window" {
			t.Fatalf("got %+v, want not due, reason window", got)
		}
		if want := at(chi, 2026, 6, 8, 8, 0); !got.NotBefore.Equal(want) {
			t.Fatalf("NotBefore = %v, want %v", got.NotBefore, want)
		}
		again := Due(DueInput{Now: got.NotBefore, LastSuccess: last, Schedule: sched, DeviceID: fixtureDevice})
		if !again.Due || again.Reason != "catchup" && again.Reason != "interval" {
			t.Fatalf("at NotBefore the run must be due, got %+v", again)
		}
	})
	t.Run("never succeeded, window closed", func(t *testing.T) {
		now := at(chi, 2026, 6, 6, 12, 0)
		got := Due(DueInput{Now: now, Schedule: sched, DeviceID: fixtureDevice})
		if got.Due || got.Reason != "window" || !got.NotBefore.Equal(at(chi, 2026, 6, 8, 8, 0)) {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("not due: the window never turns a not-yet into a decision", func(t *testing.T) {
		got := Due(DueInput{Now: last.Add(time.Hour), LastSuccess: last, Schedule: sched, DeviceID: fixtureDevice})
		if got.Due || got.Reason != "not_yet" {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("an unparsable window never blocks a backup", func(t *testing.T) {
		s := sched
		s.Window = "garbage"
		got := Due(DueInput{Now: next, LastSuccess: last, Schedule: s, DeviceID: fixtureDevice})
		if !got.Due {
			t.Fatalf("an unparsable window must never block a backup, got %+v", got)
		}
	})
	t.Run("empty window means ungated", func(t *testing.T) {
		s := sched
		s.Window = ""
		got := Due(DueInput{Now: next, LastSuccess: last, Schedule: s, DeviceID: fixtureDevice})
		if !got.Due {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("a window never cancels a no-catchup slot: the next slot runs when open", func(t *testing.T) {
		s := intervalSchedule(24*60, false, "Mon-Fri/8-18")
		now := at(chi, 2026, 6, 8, 9, 0) // Monday morning, two slots after Friday's success
		got := Due(DueInput{Now: now, LastSuccess: last, LastAttemptStart: last.Add(-time.Minute), Schedule: s, DeviceID: fixtureDevice})
		if !got.Due {
			t.Fatalf("got %+v", got)
		}
	})
}

func TestDueDSTLocalTime(t *testing.T) {
	chi := chicago(t)
	offset := StableOffset(fixtureDevice, offsetCap(24*time.Hour))

	t.Run("daily interval across spring forward is 24 real hours: the local clock shifts an hour", func(t *testing.T) {
		sched := intervalSchedule(24*60, true, "*/0-24")
		last := at(chi, 2026, 3, 7, 8, 0) // Saturday 08:00 CST
		next := last.Add(24*time.Hour + offset)
		if next.Hour() != 9 {
			t.Fatalf("next = %v: want local 09:xx (24h after 08:00 CST is 09:00 CDT, plus an offset under 1h)", next)
		}
		if got := Due(DueInput{Now: next.Add(-time.Second), LastSuccess: last, Schedule: sched, DeviceID: fixtureDevice}); got.Due {
			t.Fatalf("due one second early: %+v", got)
		}
		if got := Due(DueInput{Now: next, LastSuccess: last, Schedule: sched, DeviceID: fixtureDevice}); !got.Due {
			t.Fatalf("not due at next: %+v", got)
		}
	})
	t.Run("window closed across the spring-forward night: opens 11 real hours later", func(t *testing.T) {
		sched := intervalSchedule(24*60, true, "*/8-9")
		now := at(chi, 2026, 3, 7, 20, 0) // Saturday 20:00 CST
		got := Due(DueInput{Now: now, LastSuccess: at(chi, 2026, 3, 6, 10, 0), Schedule: sched, DeviceID: fixtureDevice})
		if got.Due || got.Reason != "window" {
			t.Fatalf("got %+v", got)
		}
		want := at(chi, 2026, 3, 8, 8, 0) // 08:00 CDT
		if !got.NotBefore.Equal(want) {
			t.Fatalf("NotBefore = %v, want %v", got.NotBefore, want)
		}
		if d := got.NotBefore.Sub(now); d != 11*time.Hour {
			t.Fatalf("real distance = %v, want 11h (the night is one hour shorter)", d)
		}
	})
	t.Run("window closed across the fall-back night: opens 13 real hours later", func(t *testing.T) {
		sched := intervalSchedule(24*60, true, "*/8-9")
		now := at(chi, 2026, 10, 31, 20, 0)
		got := Due(DueInput{Now: now, LastSuccess: at(chi, 2026, 10, 30, 10, 0), Schedule: sched, DeviceID: fixtureDevice})
		if got.Due || got.Reason != "window" {
			t.Fatalf("got %+v", got)
		}
		if d := got.NotBefore.Sub(now); d != 13*time.Hour || got.NotBefore.Hour() != 8 {
			t.Fatalf("NotBefore = %v (%v later), want 08:00 local, 13h later", got.NotBefore, d)
		}
	})
}

func TestDueFixed(t *testing.T) {
	chi := chicago(t)
	s := fixedSchedule(true, "*/0-24", "20:00", "08:00") // unsorted on purpose
	day := func(h, m int) time.Time { return at(chi, 2026, 6, 10, h, m) }

	tests := []struct {
		name       string
		now        time.Time
		lastAttem  time.Time
		sched      Schedule
		wantDue    bool
		wantReason string
		notBefore  time.Time
		slot       time.Time
	}{
		{"after the morning time, not yet attempted", day(8, 5), day(0, 0).Add(-4 * time.Hour), s, true, "fixed", time.Time{}, day(8, 0)},
		{"morning slot attempted", day(9, 0), day(8, 1), s, false, "slot_attempted", day(20, 0), day(8, 0)},
		{"afternoon: morning slot attempted, evening not yet", day(15, 0), day(8, 1), s, false, "slot_attempted", day(20, 0), day(8, 0)},
		{"evening slot due", day(20, 1), day(8, 1), s, true, "fixed", time.Time{}, day(20, 0)},
		{"a missed evening runs once the next morning (catch-up), slot is the latest one", at(chi, 2026, 6, 11, 9, 0), day(8, 1), s, true, "fixed", time.Time{}, at(chi, 2026, 6, 11, 8, 0)},
		{"after midnight the slot is yesterday evening", at(chi, 2026, 6, 11, 1, 0), day(8, 1), s, true, "fixed", time.Time{}, day(20, 0)},
		{"exactly at the time is due (inclusive)", day(20, 0), day(8, 1), s, true, "fixed", time.Time{}, day(20, 0)},
		{"one minute before the time is the previous slot", day(19, 59), day(8, 1), s, false, "slot_attempted", day(20, 0), day(8, 0)},
		{"never attempted is due at once", day(12, 0), time.Time{}, s, true, "fixed", time.Time{}, day(8, 0)},
		{"no catch-up behaves identically for the latest slot", day(9, 0), day(0, 5), fixedSchedule(false, "*/0-24", "20:00", "08:00"), true, "fixed", time.Time{}, day(8, 0)},
		{"window closed delays the fixed run", day(9, 0), day(0, 5), fixedSchedule(true, "*/20-22", "08:00"), false, "window", day(20, 0), day(8, 0)},
		{"single time: slot is yesterday's before the time", day(7, 0), at(chi, 2026, 6, 8, 8, 0), fixedSchedule(true, "*/0-24", "08:00"), true, "fixed", time.Time{}, at(chi, 2026, 6, 9, 8, 0)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Due(DueInput{Now: tc.now, LastAttemptStart: tc.lastAttem, Schedule: tc.sched, DeviceID: fixtureDevice})
			if got.Due != tc.wantDue || got.Reason != tc.wantReason {
				t.Fatalf("Due = %v/%q, want %v/%q", got.Due, got.Reason, tc.wantDue, tc.wantReason)
			}
			if !got.NotBefore.Equal(tc.notBefore) {
				t.Errorf("NotBefore = %v, want %v", got.NotBefore, tc.notBefore)
			}
			if !got.SlotStart.Equal(tc.slot) {
				t.Errorf("SlotStart = %v, want %v", got.SlotStart, tc.slot)
			}
		})
	}

	t.Run("fixed mode without usable times schedules by interval instead of going silent", func(t *testing.T) {
		bad := fixedSchedule(true, "*/0-24", "8am", "25:00")
		got := Due(DueInput{Now: day(12, 0), Schedule: bad, DeviceID: fixtureDevice})
		if !got.Due || got.Reason != "never" {
			t.Fatalf("got %+v", got)
		}
	})
}

func TestDueFixedAcrossDST(t *testing.T) {
	chi := chicago(t)
	s := fixedSchedule(true, "*/0-24", "01:30", "03:30")

	t.Run("spring forward: 03:00 CDT is after 01:30 CST and before 03:30", func(t *testing.T) {
		now := at(chi, 2026, 3, 8, 3, 0)
		if now.Hour() != 3 {
			t.Fatalf("fixture broken: %v", now)
		}
		got := Due(DueInput{Now: now, LastAttemptStart: at(chi, 2026, 3, 7, 3, 30), Schedule: s, DeviceID: fixtureDevice})
		wantSlot := at(chi, 2026, 3, 8, 1, 30)
		if !got.Due || got.Reason != "fixed" || !got.SlotStart.Equal(wantSlot) {
			t.Fatalf("got %+v, want due slot %v", got, wantSlot)
		}
		got = Due(DueInput{Now: now, LastAttemptStart: now, Schedule: s, DeviceID: fixtureDevice})
		if got.Due || got.Reason != "slot_attempted" || !got.NotBefore.Equal(at(chi, 2026, 3, 8, 3, 30)) {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("fall back: the 25-hour day puts 08:00 CST 8.5 real hours after 00:30 CDT", func(t *testing.T) {
		s := fixedSchedule(true, "*/0-24", "00:30", "08:00")
		now := at(chi, 2026, 11, 1, 7, 0)
		got := Due(DueInput{Now: now, LastAttemptStart: at(chi, 2026, 10, 31, 8, 1), Schedule: s, DeviceID: fixtureDevice})
		if !got.Due || !got.SlotStart.Equal(at(chi, 2026, 11, 1, 0, 30)) {
			t.Fatalf("got %+v", got)
		}
		got = Due(DueInput{Now: now, LastAttemptStart: at(chi, 2026, 11, 1, 0, 31), Schedule: s, DeviceID: fixtureDevice})
		if got.Due || got.Reason != "slot_attempted" || !got.NotBefore.Equal(at(chi, 2026, 11, 1, 8, 0)) {
			t.Fatalf("got %+v", got)
		}
		if d := got.NotBefore.Sub(at(chi, 2026, 11, 1, 0, 30)); d != 8*time.Hour+30*time.Minute {
			t.Fatalf("00:30 CDT to 08:00 CST is %v, want 8h30m", d)
		}
	})
}

func TestStableOffsetAndJitter(t *testing.T) {
	const max = time.Hour
	ids := make([]string, 2000)
	for i := range ids {
		ids[i] = fmt.Sprintf("01FIXTURE%017d", i)
	}

	t.Run("offset is bounded, deterministic and spread", func(t *testing.T) {
		var buckets [10]int
		for _, id := range ids {
			o := StableOffset(id, max)
			if o < 0 || o >= max {
				t.Fatalf("offset %v out of [0,%v)", o, max)
			}
			if o != StableOffset(id, max) {
				t.Fatalf("offset for %q is not deterministic", id)
			}
			buckets[int(o*10/max)]++
		}
		for i, n := range buckets {
			if n < 140 || n > 260 {
				t.Errorf("bucket %d has %d of %d ids; want about 200", i, n, len(ids))
			}
		}
	})
	t.Run("jitter is bounded, deterministic, differs per slot and per device, and is spread", func(t *testing.T) {
		slot := time.Date(2026, 6, 10, 8, 0, 0, 0, time.UTC)
		var buckets [10]int
		for _, id := range ids {
			j := Jitter(id, slot, 30*time.Minute)
			if j < 0 || j >= 30*time.Minute {
				t.Fatalf("jitter %v out of range", j)
			}
			if j != Jitter(id, slot, 30*time.Minute) {
				t.Fatalf("jitter for %q is not deterministic", id)
			}
			buckets[int(j*10/(30*time.Minute))]++
		}
		for i, n := range buckets {
			if n < 140 || n > 260 {
				t.Errorf("bucket %d has %d of %d; want about 200", i, n, len(ids))
			}
		}
		same := 0
		for i := 0; i < 200; i++ {
			s2 := slot.Add(time.Duration(i+1) * 24 * time.Hour)
			if Jitter(ids[0], s2, 30*time.Minute) == Jitter(ids[0], slot, 30*time.Minute) {
				same++
			}
		}
		if same > 5 {
			t.Errorf("jitter repeated for %d of 200 different slots", same)
		}
		if Jitter(ids[0], slot, 30*time.Minute) != Jitter(ids[0], slot.In(time.FixedZone("x", 3600)), 30*time.Minute) {
			t.Error("jitter must depend on the instant of the slot, not its zone")
		}
	})
	t.Run("non-positive max gives zero", func(t *testing.T) {
		if StableOffset("x", 0) != 0 || StableOffset("x", -time.Hour) != 0 || Jitter("x", time.Time{}, 0) != 0 {
			t.Fatal("want 0")
		}
	})
	t.Run("offset cap is min(interval/4, 1h)", func(t *testing.T) {
		for _, tc := range []struct{ interval, want time.Duration }{
			{15 * time.Minute, 225 * time.Second}, {time.Hour, 15 * time.Minute}, {4 * time.Hour, time.Hour}, {24 * time.Hour, time.Hour}, {7 * 24 * time.Hour, time.Hour},
		} {
			if got := offsetCap(tc.interval); got != tc.want {
				t.Errorf("offsetCap(%v) = %v, want %v", tc.interval, got, tc.want)
			}
		}
		// A 15-minute schedule keeps every device's offset under a quarter interval.
		for _, id := range ids[:200] {
			if o := StableOffset(id, offsetCap(15*time.Minute)); o >= 15*time.Minute/4 {
				t.Fatalf("offset %v is not under interval/4", o)
			}
		}
	})
}
