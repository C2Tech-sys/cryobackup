package state

import (
	"testing"
	"time"
)

// M3-T4 r4 M3: one count of the reporting devices, used by C13 and by the system health page.

func TestReportingDevicesIsWhatC13Judges(t *testing.T) { // C13
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	limit := normalize(Thresholds{}).UnseenAfter
	at := func(d time.Duration) Millis { return Millis(now.Add(-d).UnixMilli()) }
	dev := func(id, cust string, mod func(*DeviceFacts)) DeviceFacts {
		d := DeviceFacts{ID: id, CustomerID: cust, Complete: true, LastSeen: at(time.Minute), ClockOffset: time.Hour}
		if mod != nil {
			mod(&d)
		}
		return d
	}
	w := World{Customers: []CustomerFacts{{ID: "c", State: "active"}, {ID: "gone", State: customerArchived}}, Devices: []DeviceFacts{
		dev("current", "c", nil),
		dev("paused, heartbeat current", "c", func(d *DeviceFacts) { d.PausedAt = at(time.Hour) }),
		dev("exactly at its limit", "c", func(d *DeviceFacts) { d.LastSeen = at(limit) }),
		dev("just past its limit", "c", func(d *DeviceFacts) { d.LastSeen = at(limit + time.Second) }),
		dev("retired", "c", func(d *DeviceFacts) { d.RetiredAt = at(time.Hour) }),
		dev("facts incomplete", "c", func(d *DeviceFacts) { d.Complete = false }),
		dev("never heard from", "c", func(d *DeviceFacts) { d.LastSeen = 0 }),
		dev("garbled heartbeat", "c", func(d *DeviceFacts) { d.LastSeen = 5 }),
		dev("customer archived", "gone", nil),
		dev("customer not loaded", "orphan", nil),
	}}
	got := map[string]bool{}
	for _, d := range ReportingDevices(w, now) {
		got[d.ID] = true
	}
	want := []string{"current", "paused, heartbeat current", "exactly at its limit"}
	if len(got) != len(want) {
		t.Errorf("reporting: %v, want %v", got, want)
	}
	for _, id := range want {
		if !got[id] {
			t.Errorf("%s does not report", id)
		}
	}
}

// The boundary of C13, through Evaluate: 4 devices that all agree raise nothing, 5 do.
func TestC13RaisedAtExactlyFiveReportingDevices(t *testing.T) { // C13
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	for n, want := range map[int]bool{4: false, 5: true} {
		w := World{LoadComplete: true, FleetEverEnrolled: true, Customers: []CustomerFacts{{ID: "c", State: "active"}}, LoadedAt: now}
		for i := 0; i < n; i++ {
			w.Devices = append(w.Devices, DeviceFacts{ID: string(rune('a' + i)), CustomerID: "c", Complete: true, EnrolledAt: Millis(now.Add(-time.Hour).UnixMilli()),
				LastSeen: Millis(now.Add(-time.Minute).UnixMilli()), ClockOffset: time.Hour})
		}
		raised := false
		for _, b := range Evaluate(w, now).Banners {
			raised = raised || b.ID == "C13"
		}
		if raised != want {
			t.Errorf("%d reporting devices that agree: C13 %v, want %v", n, raised, want)
		}
		if got := len(ReportingDevices(w, now)); got != n {
			t.Errorf("%d devices: ReportingDevices says %d", n, got)
		}
	}
}
