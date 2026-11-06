package state

import (
	"sort"
	"strconv"
	"strings"
	"time"
)

// loadErrorOrphan is the LoadErrorClass Evaluate gives a device whose customer
// is not in the loaded customer set: an inconsistent row, so UNKNOWN.
const loadErrorOrphan = "orphan_device"

// Evaluate classifies every device, rolls up every customer, and derives the
// console-level banners of architecture §4.6: C08 (empty fleet) and C09 (empty
// poll or partial data) from the load, the probe-driven conditions from
// World.Probes (banners.go), and the device-derived ones (fleetwide.go). C04,
// C04b and the agent-listener half of C05 are not raised here (ADR 0013: they
// come from the self-probe in internal/console/health). The result is stamped
// with now (C23).
//
// Evaluate never makes missing data look healthy. A failed or partial load,
// an empty poll after a non-empty one, a device with incomplete facts, or a
// device whose customer was not loaded each produce a banner and make
// Complete false, so FleetView.AllClear can never be true.
//
// Archived customers and their devices are left out of the view (D08); their
// devices still count towards the C09 incompleteness check, because a failed
// load is a failed load. Output order is fully determined by the facts, not by
// the order of World.Devices or World.Customers.
func Evaluate(w World, now time.Time) FleetView {
	now = now.UTC()

	known := make(map[string]bool, len(w.Customers))
	archived := make(map[string]bool)
	for _, c := range w.Customers {
		known[c.ID] = true
		if c.State == customerArchived {
			archived[c.ID] = true
		}
	}

	incomplete := 0
	states := make([]DeviceState, 0, len(w.Devices))
	for _, df := range w.Devices {
		d := df
		if !known[d.CustomerID] {
			d.Complete = false
			d.LoadErrorClass = loadErrorOrphan
		}
		if !d.Complete {
			incomplete++
		}
		if archived[d.CustomerID] {
			continue
		}
		states = append(states, Classify(d, now))
	}

	byCustomer := make(map[string][]DeviceState, len(w.Customers))
	for _, ds := range states {
		byCustomer[ds.CustomerID] = append(byCustomer[ds.CustomerID], ds)
	}
	customers := make([]CustomerState, 0, len(w.Customers))
	sortKeyOf := make(map[string]int, len(w.Customers))
	for _, c := range w.Customers {
		if c.State == customerArchived {
			continue
		}
		cs := Rollup(c, byCustomer[c.ID], now)
		sortKeyOf[c.ID] = cs.SortKey
		customers = append(customers, cs)
	}
	sort.SliceStable(customers, func(i, j int) bool {
		a, b := &customers[i], &customers[j]
		if a.SortKey != b.SortKey {
			return a.SortKey < b.SortKey
		}
		if la, lb := strings.ToLower(a.Name), strings.ToLower(b.Name); la != lb {
			return la < lb
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.ID < b.ID
	})

	unknownTier := CustomerUnknown.sortKey()
	keyFor := func(ds *DeviceState) int {
		if k, ok := sortKeyOf[ds.CustomerID]; ok {
			return k
		}
		return unknownTier // orphan devices are UNKNOWN: they sort with UNKNOWN-containing customers
	}
	sort.SliceStable(states, func(i, j int) bool {
		a, b := &states[i], &states[j]
		if ka, kb := keyFor(a), keyFor(b); ka != kb {
			return ka < kb
		}
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

	active := 0
	for i := range states {
		if states[i].Primary != StateRetired {
			active++
		}
	}

	emptyPoll := w.PreviousDeviceCount > 0 && len(w.Devices) == 0 && w.LoadComplete
	partial := !w.LoadComplete || w.LoadErrors > 0 || incomplete > 0 || emptyPoll

	var banners []Banner
	// C08: an enrolled fleet with nothing active. Not raised when the load itself failed:
	// zero rows are then a symptom, and C09 says so.
	if (w.FleetEverEnrolled || w.PreviousDeviceCount > 0) && active == 0 && w.LoadComplete {
		banners = append(banners, Banner{
			ID:       "C08",
			Severity: SeverityCritical,
			Text:     "No active devices enrolled.",
		})
	}
	if partial {
		banners = append(banners, partialBanner(w, incomplete, emptyPoll))
	}
	banners = append(banners, probeBanners(w, now)...)
	if b, ok := clockSuspectBanner(ReportingDevices(w, now)); ok {
		banners = append(banners, b)
	}
	collapsed, gatewayMass := fleetCollapses(states, customers)
	if gatewayMass {
		// C19: one C01 for the whole cluster. A failing gateway probe already raised C01 and
		// says more (its last OK time); the device-derived one is added only without it.
		if _, probed := findBannerID(banners, "C01"); !probed {
			banners = append(banners, gatewayDerivedBanner(collapsed[0]))
		}
	}
	sort.SliceStable(banners, func(i, j int) bool {
		if banners[i].Severity != banners[j].Severity {
			return banners[i].Severity > banners[j].Severity
		}
		return banners[i].ID < banners[j].ID
	})

	return FleetView{
		Banners:    banners,
		Devices:    states,
		Customers:  customers,
		Collapses:  collapsed,
		ComputedAt: now,
		LoadedAt:   w.LoadedAt,
		Complete:   !partial,
		FirstRun:   !w.FleetEverEnrolled && len(w.Devices) == 0 && !emptyPoll,
	}
}

// findBannerID returns the index of the banner with id, if any.
func findBannerID(bs []Banner, id string) (int, bool) {
	for i := range bs {
		if bs[i].ID == id {
			return i, true
		}
	}
	return -1, false
}

// partialBanner builds C09 (§4.6): the facts loader returned nothing where it
// previously returned devices, failed to load some devices, or did not complete.
func partialBanner(w World, incomplete int, emptyPoll bool) Banner {
	b := Banner{ID: "C09", Severity: SeverityCritical}
	switch {
	case emptyPoll:
		b.Count = w.PreviousDeviceCount
		b.Text = "Partial data: the device list came back empty after " +
			strconv.Itoa(w.PreviousDeviceCount) + " devices on the previous load"
	case incomplete > 0:
		b.Count = incomplete
		b.Text = "Partial data: " + strconv.Itoa(incomplete) + " devices could not be classified"
		if w.LoadErrors > 0 {
			b.Text += " (" + strconv.Itoa(w.LoadErrors) + " load errors)"
		}
	case w.LoadErrors > 0:
		b.Count = w.LoadErrors
		b.Text = "Partial data: " + strconv.Itoa(w.LoadErrors) + " load errors"
	default:
		b.Text = "Partial data: the facts load did not complete"
	}
	return b
}
