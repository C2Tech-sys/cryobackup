package state

import (
	"sort"
	"strings"
	"time"
)

// The daily digest's content (§5.7 daily-digest, §4.8 C22). Rendering and
// sending are M4; this is the exceptions-first list the digest is built from,
// computed from the same FleetView as every page so the two cannot disagree.

// DigestKind is one section of the digest, in the order the digest lists them.
type DigestKind string

// Digest sections, exceptions first (§5.7: MISSING, UNKNOWN, unseen, blank,
// failed, stale, partial, escrow, maintenance, parked, deferred > 24 h). The
// critical states §5.7 does not name (identity conflict, stuck) sit with their
// precedence neighbours rather than being left out.
const (
	DigestMissing     DigestKind = "missing"
	DigestUnknown     DigestKind = "unknown"
	DigestIdentity    DigestKind = "identity_conflict"
	DigestUnseen      DigestKind = "unseen"
	DigestBlank       DigestKind = "blank"
	DigestStuck       DigestKind = "stuck"
	DigestFailed      DigestKind = "failed"
	DigestStale       DigestKind = "stale"
	DigestPartial     DigestKind = "partial"
	DigestEscrow      DigestKind = "escrow"
	DigestMaintenance DigestKind = "maintenance"
	DigestParked      DigestKind = "parked"
	DigestDeferred    DigestKind = "deferred_24h"
	// DigestOutdated holds devices whose OUTDATED flag alarms (notification_overrides.outdated_alarm).
	DigestOutdated DigestKind = "outdated"
)

var digestOrder = [...]DigestKind{
	DigestMissing, DigestUnknown, DigestIdentity, DigestUnseen, DigestBlank, DigestStuck, DigestFailed,
	DigestStale, DigestPartial, DigestEscrow, DigestMaintenance, DigestParked, DigestDeferred, DigestOutdated,
}

func (k DigestKind) rank() int {
	for i, x := range digestOrder {
		if x == k {
			return i
		}
	}
	return len(digestOrder)
}

// DigestDeferredAfter is how long a device must have been DEFERRED to be listed.
const DigestDeferredAfter = 24 * time.Hour

// DigestLine is one listed customer (MISSING) or device.
type DigestLine struct {
	Kind                     DigestKind
	CustomerID, CustomerName string
	// DeviceID and Hostname are empty for a customer line.
	DeviceID, Hostname string
	State              State
	Since              time.Time
	// Alarm and Alarms are the device's severity and every alarming flag, whichever state is primary:
	// a parked device with an unverified escrow is listed as parked and names ESCROW_UNVERIFIED.
	Alarm  Severity
	Alarms []State
	// Attrs carries the device's badges (paused_indefinite flags an open-ended pause).
	Attrs []Attr
	// Muted marks a suspended customer: listed and labelled, notifications muted (D07).
	Muted bool
}

// DigestView is what the digest says, banner state first.
type DigestView struct {
	// Banners are the console conditions, most severe first.
	Banners []Banner
	// Lines are the exceptions in DigestKind order, then customer name, then hostname.
	Lines []DigestLine
	// AllClear is FleetView.AllClear: the digest says "all clear" only on evidence.
	AllClear   bool
	ComputedAt time.Time
}

// digestKindOf maps a device to its section: by its primary state, or, when the
// primary is a state the digest does not list (OK, PENDING_FIRST, MISSED,
// OUTDATED, RETIRED, DEFERRED for less than a day) but the device alarms, by
// its first alarming flag, so no alarm is left out of the digest.
func digestKindOf(d *DeviceState, now time.Time) (DigestKind, bool) {
	if k, ok := digestKindOfState(d.Primary, d, now); ok {
		return k, true
	}
	for _, s := range d.AlarmFlags {
		if k, ok := digestKindOfState(s, d, now); ok {
			return k, true
		}
		if s == StateOutdated {
			return DigestOutdated, true
		}
	}
	return "", false
}

// digestKindOfState is the section of one state.
func digestKindOfState(s State, d *DeviceState, now time.Time) (DigestKind, bool) {
	switch s {
	case StateUnknown:
		return DigestUnknown, true
	case StateIdentityConflict:
		return DigestIdentity, true
	case StateUnseen:
		return DigestUnseen, true
	case StateBlank:
		return DigestBlank, true
	case StateStuck:
		return DigestStuck, true
	case StateFailed:
		return DigestFailed, true
	case StateStale:
		return DigestStale, true
	case StatePartial:
		return DigestPartial, true
	case StateEscrowUnverified:
		return DigestEscrow, true
	case StateMaintFailed:
		return DigestMaintenance, true
	case StatePaused:
		return DigestParked, true
	case StateDeferred:
		// A deferral whose start is unknown cannot be shown to be short: list it.
		if d.Since.IsZero() || now.Sub(d.Since) > DigestDeferredAfter {
			return DigestDeferred, true
		}
	}
	return "", false
}

// DigestOf lists the exceptions of v for the daily digest: every MISSING
// customer and every device in a listed state, even when everything else is
// green (C22). Devices are taken from v.Devices, so a device whose customer was
// not loaded (UNKNOWN, orphan) is still listed.
func DigestOf(v FleetView, now time.Time) DigestView {
	now = now.UTC()
	out := DigestView{Banners: append([]Banner(nil), v.Banners...), AllClear: v.AllClear(), ComputedAt: v.ComputedAt}
	names := make(map[string]string, len(v.Customers))
	muted := make(map[string]bool, len(v.Customers))
	for i := range v.Customers {
		c := &v.Customers[i]
		names[c.ID], muted[c.ID] = c.Name, c.Muted
		if c.Status == CustomerMissing {
			out.Lines = append(out.Lines, DigestLine{Kind: DigestMissing, CustomerID: c.ID, CustomerName: c.Name, Muted: c.Muted})
		}
	}
	for i := range v.Devices {
		d := &v.Devices[i]
		k, ok := digestKindOf(d, now)
		if !ok {
			continue
		}
		out.Lines = append(out.Lines, DigestLine{Kind: k, CustomerID: d.CustomerID, CustomerName: names[d.CustomerID],
			DeviceID: d.ID, Hostname: d.Hostname, State: d.Primary, Since: d.Since,
			Alarm: d.Alarm, Alarms: append([]State(nil), d.AlarmFlags...),
			Attrs: append([]Attr(nil), d.Attrs...), Muted: muted[d.CustomerID]})
	}
	sort.SliceStable(out.Lines, func(i, j int) bool {
		a, b := &out.Lines[i], &out.Lines[j]
		if ra, rb := a.Kind.rank(), b.Kind.rank(); ra != rb {
			return ra < rb
		}
		if la, lb := strings.ToLower(a.CustomerName), strings.ToLower(b.CustomerName); la != lb {
			return la < lb
		}
		if la, lb := strings.ToLower(a.Hostname), strings.ToLower(b.Hostname); la != lb {
			return la < lb
		}
		return a.DeviceID < b.DeviceID
	})
	return out
}
