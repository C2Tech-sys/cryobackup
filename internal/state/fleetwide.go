package state

import (
	"sort"
	"strconv"
	"time"
)

// Device-derived console conditions: facts about many devices at once that say
// something about the console or the path the agents share, rather than about
// any one device.

// C13 thresholds (§4.6): at least ClockSuspectMinDevices devices, and at least
// ClockSuspectShare of the reporting devices, report clock offsets of the same
// sign beyond the clock-skew limit.
const (
	ClockSuspectMinDevices = 5
	ClockSuspectShare      = 0.2
)

// ClockSkewLimit is the offset beyond which a device carries the clock_skew
// attribute (§4.2 attributes) and counts towards C13.
const ClockSkewLimit = clockSkewLimit

// isReporting is true for a device whose heartbeat, and with it the stored
// clock offset, is current: not retired, facts complete, a parseable last
// heartbeat no older than its unseen_after. The heartbeat handler stores an
// offset with every heartbeat, so a device with a presence row has a real
// offset; a device with none has no LastSeen and does not report.
func isReporting(d *DeviceFacts, now time.Time) bool {
	if d.RetiredAt != 0 || !d.Complete {
		return false
	}
	seen, ok := d.LastSeen.Time()
	return ok && now.Sub(seen) <= normalize(d.Thresholds).UnseenAfter
}

// clockSuspectBanner is C13: the devices agree that the server clock is off.
// One device with a bad clock is only its own clock_skew attribute; agreement
// of many is evidence against the server.
func clockSuspectBanner(reporting []*DeviceFacts) (Banner, bool) {
	ahead, behind := 0, 0
	for _, d := range reporting {
		switch {
		case d.ClockOffset > clockSkewLimit:
			ahead++
		case d.ClockOffset < -clockSkewLimit:
			behind++
		}
	}
	agree, way := ahead, "ahead of"
	if behind > ahead {
		agree, way = behind, "behind"
	}
	if agree < ClockSuspectMinDevices || float64(agree) < ClockSuspectShare*float64(len(reporting)) {
		return Banner{}, false
	}
	return Banner{ID: "C13", Severity: SeverityAlarm, Count: agree,
		Text: "Server clock may be wrong: device offsets agree. " + strconv.Itoa(agree) + " of " +
			strconv.Itoa(len(reporting)) + " reporting devices run more than 2 min " + way + " the server clock."}, true
}

// Collapse thresholds (§4.3): at least CollapseMinDevices active devices, of
// which at least CollapseShare are UNSEEN or DEFERRED for the gateway at once.
const (
	CollapseMinDevices = 5
	CollapseShare      = 0.5
)

// CollapseScope says whether a Collapse is about one customer or the fleet.
type CollapseScope string

const (
	// CollapseCustomer is customer-wide silence: one customer-level row, one notification.
	CollapseCustomer CollapseScope = "customer"
	// CollapseFleet is fleet-wide DEFERRED(gateway): banner C01 instead of N rows.
	CollapseFleet CollapseScope = "fleet"
)

// Collapse is one §4.3 collapse: many devices with one likely cause, shown as
// one row and notified once. The per-device rows stay in FleetView.Devices and
// CustomerState.Devices; the notifier sends one notification per Collapse that
// is not Covered, in place of one per device in DeviceIDs.
type Collapse struct {
	Scope CollapseScope
	// CustomerID and CustomerName are set for a customer collapse.
	CustomerID, CustomerName string
	// Affected devices of Total active devices; Unseen and DeferredGateway split Affected by cause
	// (a device that is both counts as Unseen).
	Affected, Total         int
	Unseen, DeferredGateway int
	// DeviceIDs are the affected devices, sorted.
	DeviceIDs []string
	// Covered is true for a customer collapse whose every affected device is in the fleet-wide
	// collapse: banner C01 already speaks for them, so it adds a row but no notification.
	Covered bool
}

// hasFlag reports whether d is in state s, as Primary or as any flag.
func hasFlag(d *DeviceState, s State) bool {
	if d.Primary == s {
		return true
	}
	for _, f := range d.Flags {
		if f == s {
			return true
		}
	}
	return false
}

func isUnseen(d *DeviceState) bool { return hasFlag(d, StateUnseen) }

// isDeferredGateway is true for a device that cannot reach the gateway: it is
// DEFERRED for the gateway, or its newest backup run was deferred for the
// gateway even though the DEFERRED row no longer applies. The second case
// matters: once the outage outlives stale_after the device is STALE, which
// suppresses DEFERRED, and the gateway evidence must not vanish as the outage
// gets worse.
func isDeferredGateway(d *DeviceState) bool {
	gateway := deferredReason(d.Detail.DeferredReason) == ReasonDeferredGateway
	if hasFlag(d, StateDeferred) && (d.Reason == ReasonDeferredGateway || gateway) {
		return true
	}
	return d.Detail.LastOutcome == outcomeDeferred && gateway
}

// collapses reports whether affected of total meets the §4.3 rule.
func collapses(affected, total int) bool {
	return total >= CollapseMinDevices && float64(affected) >= CollapseShare*float64(total)
}

// CustomerCollapse applies the §4.3 customer-wide collapse to one rolled-up
// customer: at least 5 active (non-retired, non-paused) devices, at least half
// of them UNSEEN or DEFERRED for the gateway. ok is false when it does not apply;
// the counts are filled either way.
func CustomerCollapse(c CustomerState) (col Collapse, ok bool) {
	col = Collapse{Scope: CollapseCustomer, CustomerID: c.ID, CustomerName: c.Name}
	for i := range c.Devices {
		d := &c.Devices[i]
		if d.Primary == StateRetired || d.Primary == StatePaused {
			continue
		}
		col.Total++
		switch {
		case isUnseen(d):
			col.Unseen++
		case isDeferredGateway(d):
			col.DeferredGateway++
		default:
			continue
		}
		col.Affected++
		col.DeviceIDs = append(col.DeviceIDs, d.ID)
	}
	sort.Strings(col.DeviceIDs)
	return col, collapses(col.Affected, col.Total)
}

// fleetGatewayCollapse is the fleet-wide half of §4.3 and the derived half of
// C01: of the reporting devices (active, facts known, heartbeat current), at
// least 5 and at least half are DEFERRED for the gateway. Devices that cannot
// report (UNSEEN, UNKNOWN) say nothing about the gateway and are left out.
func fleetGatewayCollapse(states []DeviceState) (Collapse, bool) {
	col := Collapse{Scope: CollapseFleet}
	for i := range states {
		d := &states[i]
		if d.Primary == StateRetired || d.Primary == StatePaused || d.Primary == StateUnknown || isUnseen(d) {
			continue
		}
		col.Total++
		if isDeferredGateway(d) {
			col.Affected++
			col.DeferredGateway++
			col.DeviceIDs = append(col.DeviceIDs, d.ID)
		}
	}
	sort.Strings(col.DeviceIDs)
	return col, collapses(col.Affected, col.Total)
}

// gatewayDerivedBanner is C01 raised from the devices (§4.3, §4.6, C19).
func gatewayDerivedBanner(col Collapse) Banner {
	return Banner{ID: "C01", Severity: SeverityCritical, Count: col.Affected,
		Text: "Gateway unreachable: no agent can back up. " + strconv.Itoa(col.Affected) + " of " + strconv.Itoa(col.Total) +
			" reporting devices are deferring their backups because they cannot reach it."}
}

// fleetCollapses lists the §4.3 collapses of a view: the fleet-wide one first,
// then customer ones in customer order.
func fleetCollapses(states []DeviceState, customers []CustomerState) (out []Collapse, fleet bool) {
	fc, fleet := fleetGatewayCollapse(states)
	inFleet := map[string]bool{}
	if fleet {
		out = append(out, fc)
		for _, id := range fc.DeviceIDs {
			inFleet[id] = true
		}
	}
	for i := range customers {
		cc, ok := CustomerCollapse(customers[i])
		if !ok {
			continue
		}
		cc.Covered = fleet
		for _, id := range cc.DeviceIDs {
			if !inFleet[id] {
				cc.Covered = false
				break
			}
		}
		out = append(out, cc)
	}
	return out, fleet
}
