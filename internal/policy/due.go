package policy

import (
	"encoding/binary"
	"hash/fnv"
	"math/bits"
	"sort"
	"time"
)

// DueInput is everything Due reads. There is no hidden state and no clock:
// the caller supplies Now.
type DueInput struct {
	// Now is the current time in the device's local zone (Location set); the
	// window and fixed times are evaluated on its wall clock.
	Now time.Time
	// LastSuccess is when the last successful (or tolerated partial) run
	// finished; zero = never.
	LastSuccess time.Time
	// LastAttemptStart is when the last attempt, successful or not, started;
	// zero = never.
	LastAttemptStart time.Time
	// Schedule is the policy's schedule section.
	Schedule Schedule
	// DeviceID feeds the stable per-device offset.
	DeviceID string
}

// Decision is the result of Due.
type Decision struct {
	// Due: a run may start now.
	Due bool
	// Reason when Due: "never", "interval", "catchup", "fixed", "clock_skew".
	// Reason when not Due: "not_yet", "window", "slot_attempted".
	Reason string
	// NotBefore, when !Due, is the earliest time a re-check could say Due:
	// the next interval, the next window opening or the next slot. Zero when
	// unknown (a window that never opens).
	NotBefore time.Time
	// SlotStart is the slot this decision belongs to, for jitter and the
	// journal. See the package documentation for how slots are defined.
	SlotStart time.Time
}

// Reasons of a Decision.
const (
	ReasonNever         = "never"
	ReasonInterval      = "interval"
	ReasonCatchup       = "catchup"
	ReasonFixed         = "fixed"
	ReasonClockSkew     = "clock_skew"
	ReasonNotYet        = "not_yet"
	ReasonWindow        = "window"
	ReasonSlotAttempted = "slot_attempted"
)

// Due computes whether a backup may start. It is pure and total: it reads only
// its input. The rules are documented in the package comment (architecture
// §6.1 "scheduler"); in short, due-ness is recomputed from the last success
// (no stored next-run), N missed intervals collapse into one run, a stable
// per-device offset spreads the fleet, and the window gates the start but
// never cancels a due run. Retry backoff, run-on-wake, startup delay and
// start conditions are the agent's, not Due's.
func Due(in DueInput) Decision {
	var d Decision
	if in.Schedule.Mode == ModeFixed {
		if cands := fixedTimes(in.Schedule.FixedTimes); len(cands) > 0 {
			d = dueFixed(in, cands)
			return gate(in, d)
		}
		// A fixed schedule with no usable times would never run. Fail safe:
		// schedule by interval instead. Validate rejects such a policy.
	}
	d = dueInterval(in)
	return gate(in, d)
}

// gate applies the backup window: a due run outside the window is delayed to
// the next opening, never cancelled.
func gate(in DueInput, d Decision) Decision {
	if !d.Due || in.Schedule.Window == "" {
		return d
	}
	w, err := ParseWindow(in.Schedule.Window)
	if err != nil || w.Contains(in.Now) {
		// An unparsable window never blocks a backup; Validate rejects it.
		return d
	}
	d.Due, d.Reason = false, ReasonWindow
	d.NotBefore, _ = w.NextOpen(in.Now)
	return d
}

func intervalOf(s Schedule) time.Duration {
	if s.IntervalMinutes <= 0 {
		return 24 * time.Hour
	}
	return time.Duration(s.IntervalMinutes) * time.Minute
}

// offsetCap is min(interval/4, 1h): the largest stable offset for an interval.
func offsetCap(interval time.Duration) time.Duration {
	c := interval / 4
	if c > time.Hour {
		c = time.Hour
	}
	return c
}

func dueInterval(in DueInput) Decision {
	now := in.Now
	if in.LastSuccess.IsZero() {
		return Decision{Due: true, Reason: ReasonNever, SlotStart: now}
	}
	if in.LastSuccess.After(now) {
		// The clock went backwards (or LastSuccess was written by a clock that
		// was ahead). Waiting for the clock to catch up could silence backups
		// for days, so run now and say why.
		return Decision{Due: true, Reason: ReasonClockSkew, SlotStart: now}
	}
	interval := intervalOf(in.Schedule)
	offset := StableOffset(in.DeviceID, offsetCap(interval))
	next := in.LastSuccess.Add(interval + offset)
	k := now.Sub(in.LastSuccess) / interval
	slot := in.LastSuccess.Add(k * interval)
	if now.Before(next) {
		return Decision{Reason: ReasonNotYet, NotBefore: next, SlotStart: slot}
	}
	if !in.Schedule.Catchup {
		// Without catch-up the current slot is the only one that may run, and
		// only once: past slots are skipped, never run late.
		if !in.LastAttemptStart.IsZero() && !in.LastAttemptStart.Before(slot) {
			return Decision{Reason: ReasonSlotAttempted, NotBefore: slot.Add(interval + offset), SlotStart: slot}
		}
		return Decision{Due: true, Reason: ReasonInterval, SlotStart: slot}
	}
	reason := ReasonInterval
	if now.Sub(next) >= interval {
		reason = ReasonCatchup
	}
	return Decision{Due: true, Reason: reason, SlotStart: slot}
}

// hm is a parsed "HH:MM" fixed time.
type hm struct{ h, m int }

func parseHM(s string) (hm, bool) {
	if len(s) != 5 || s[2] != ':' || !isDigit(s[0]) || !isDigit(s[1]) || !isDigit(s[3]) || !isDigit(s[4]) {
		return hm{}, false
	}
	h, m := int(s[0]-'0')*10+int(s[1]-'0'), int(s[3]-'0')*10+int(s[4]-'0')
	if h > 23 || m > 59 {
		return hm{}, false
	}
	return hm{h, m}, true
}

// fixedTimes parses and sorts the valid, distinct entries.
func fixedTimes(in []string) []hm {
	var out []hm
	seen := map[hm]bool{}
	for _, s := range in {
		if v, ok := parseHM(s); ok && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].h*60+out[i].m < out[j].h*60+out[j].m
	})
	return out
}

func dueFixed(in DueInput, cands []hm) Decision {
	now := in.Now
	y, mo, day := now.Date()
	var all []time.Time
	for dd := -1; dd <= 1; dd++ {
		for _, c := range cands {
			// time.Date resolves wall-clock times in now's zone, so a 23- or
			// 25-hour day and a nonexistent 02:30 are the zone's business.
			all = append(all, time.Date(y, mo, day+dd, c.h, c.m, 0, 0, now.Location()))
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Before(all[j]) })
	// cands is never empty (the caller checks), so all has candidates on days
	// -1, 0 and +1. Every day -1 candidate is at or before now and every day +1
	// candidate is after it (a zone's offset changes by far less than a day),
	// so the loop always sets both slot and next.
	var slot, next time.Time
	for _, c := range all {
		if c.After(now) {
			next = c
			break
		}
		slot = c
	}
	if !in.LastAttemptStart.IsZero() && !in.LastAttemptStart.Before(slot) {
		return Decision{Reason: ReasonSlotAttempted, NotBefore: next, SlotStart: slot}
	}
	// "Now - SlotStart < interval to the following candidate" holds by
	// construction (slot is the latest candidate <= now); it is kept so that a
	// future change to the candidate set cannot silently run a slot late
	// without catch-up.
	if in.Schedule.Catchup || now.Sub(slot) < next.Sub(slot) {
		return Decision{Due: true, Reason: ReasonFixed, SlotStart: slot}
	}
	return Decision{Reason: ReasonNotYet, NotBefore: next, SlotStart: slot}
}

// StableOffset returns a fixed per-device delay in [0, max): the FNV-1a hash
// of deviceID, avalanche-mixed, mapped evenly onto the range. The same id
// always yields the same offset, so a fleet's due times spread out and stay
// spread; ids that differ only in a trailing counter still spread evenly (plain
// FNV-1a does not do that). max <= 0 gives 0.
func StableOffset(deviceID string, max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(deviceID))
	return scale(mix(h.Sum64()), max)
}

// Jitter returns a deterministic delay in [0, max) for one device and one
// slot: the FNV-1a hash of the device id and the slot's Unix second mapped
// evenly onto the range. The same (device, slot) always yields the same delay,
// different slots yield different delays. Callers apply it after first seeing
// Due for the slot (Decision.SlotStart), with max = jitter_minutes. max <= 0
// gives 0.
func Jitter(deviceID string, slot time.Time, max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(deviceID))
	var b [9]byte
	b[0] = 0xff // separator that no UTF-8 id can contain
	binary.LittleEndian.PutUint64(b[1:], uint64(slot.Unix()))
	_, _ = h.Write(b[:])
	return scale(mix(h.Sum64()), max)
}

// scale maps a uniform 64-bit value onto [0, max) without modulo bias.
func scale(v uint64, max time.Duration) time.Duration {
	hi, _ := bits.Mul64(v, uint64(max))
	return time.Duration(hi)
}

// mix is the splitmix64 finaliser. FNV-1a alone leaves ids that differ only in
// their last bytes clustered in the high bits that scale uses (a test with
// sequential ids showed buckets of 0 and 300 out of 2000).
func mix(x uint64) uint64 {
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	x ^= x >> 31
	return x
}
