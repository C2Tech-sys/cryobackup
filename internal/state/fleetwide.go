package state

import (
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
