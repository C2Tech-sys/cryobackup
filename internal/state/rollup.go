package state

import (
	"sort"
	"strings"
	"time"
)

// Customer lifecycle states (schema: customers.state).
const (
	customerSuspended = "suspended"
	customerArchived  = "archived"
)

// sortDeviceStates orders states by (primary precedence, hostname, id) in place.
func sortDeviceStates(ds []DeviceState) {
	sort.SliceStable(ds, func(i, j int) bool {
		a, b := &ds[i], &ds[j]
		if ra, rb := a.Primary.rank(), b.Primary.rank(); ra != rb {
			return ra < rb
		}
		if la, lb := strings.ToLower(a.Hostname), strings.ToLower(b.Hostname); la != lb {
			return la < lb
		}
		if a.Hostname != b.Hostname {
			return a.Hostname < b.Hostname
		}
		return a.ID < b.ID
	})
}

// Rollup folds one customer's classified devices into the §4.5 customer state.
// ds must be the customer's devices as returned by Classify; Rollup does not
// look at any facts of its own, so it is as pure as Classify.
//
// Order of decision: MISSING or ONBOARDING or EMPTY when there is no
// non-retired device; then UNKNOWN-containing; then ALARM; then WARN; then OK.
// A customer with no devices is never green, and a customer whose only devices
// are parked is WARN, not OK. Retired devices are excluded from every count.
// A suspended customer keeps its states and alarms and is only Muted (D07). An
// archived customer is never MISSING or ONBOARDING; Evaluate leaves it out.
func Rollup(c CustomerFacts, ds []DeviceState, now time.Time) CustomerState {
	now = now.UTC()
	cs := CustomerState{
		ID:     c.ID,
		Name:   c.Name,
		Counts: map[State]int{},
		Muted:  c.State == customerSuspended,
	}
	archived := c.State == customerArchived

	devices := make([]DeviceState, len(ds))
	copy(devices, ds)
	sortDeviceStates(devices)
	cs.Devices = devices

	healthy := 0
	containsUnknown, needsWarn := false, false
	var worst Severity
	for i := range devices {
		d := &devices[i]
		if d.Primary == StateRetired {
			cs.Retired++
			continue
		}
		cs.Counts[d.Primary]++
		if d.Primary == StatePaused {
			cs.Paused++
		} else {
			cs.Active++
			// A fresh success and no alarm of any kind. STALE is critical, so a stale device
			// is already excluded by its alarm.
			if d.Alarm == SeverityNone && !d.Detail.LastSuccess.IsZero() {
				healthy++
			}
		}
		if d.Alarm > worst {
			worst = d.Alarm
		}
		for _, s := range d.Flags {
			switch s {
			case StateUnknown:
				containsUnknown = true
			case StateMissed, StateDeferred, StateOutdated:
				needsWarn = true
			}
		}
		if d.Primary == StatePaused && hasAttr(d.Attrs, AttrPausedIndefinite) {
			needsWarn = true
		}
	}
	nonRetired := cs.Active + cs.Paused

	switch {
	case nonRetired == 0 && c.Expected && !archived:
		if until, ok := c.OnboardingUntil.Time(); ok && until.After(now) {
			cs.Status = CustomerOnboarding
		} else {
			// An absent, past or unparseable onboarding window does not excuse a missing customer.
			cs.Status = CustomerMissing
			cs.Alarm = SeverityCritical
		}
	case nonRetired == 0:
		cs.Status = CustomerEmpty
	case containsUnknown:
		cs.Status = CustomerUnknown
		cs.Alarm = SeverityCritical
	case worst > SeverityNone:
		cs.Status = CustomerAlarm
		cs.Alarm = worst
	case needsWarn || cs.Active == 0:
		cs.Status = CustomerWarn
	default:
		cs.Status = CustomerOK
	}
	cs.SortKey = cs.Status.sortKey()

	if cs.Active > 0 {
		cs.HealthKnown = true
		cs.HealthPct = 100 * float64(healthy) / float64(cs.Active)
	}
	return cs
}

func hasAttr(attrs []Attr, a Attr) bool {
	for _, x := range attrs {
		if x == a {
			return true
		}
	}
	return false
}
