package state

import (
	"fmt"
	"math"
	"reflect"
	"testing"
	"time"
)

type devMod func(f *DeviceFacts)

func okDevice(n int) DeviceFacts { return deviceOf(n, 1, fmt.Sprintf("ws-desk-%02d", n)) }

func devWith(n int, mod devMod) DeviceFacts {
	f := okDevice(n)
	if mod != nil {
		mod(&f)
	}
	return f
}

var (
	modStale      devMod = func(f *DeviceFacts) { setSuccess(f, 100*hour) }
	modFailed     devMod = func(f *DeviceFacts) { f.LastBackup = failedRun(30*time.Minute, "network"); f.ConsecutiveFailures = 1 }
	modRetired    devMod = func(f *DeviceFacts) { f.RetiredAt = ago(10 * day) }
	modUnknown    devMod = func(f *DeviceFacts) { f.Complete = false }
	modTimedPause devMod = func(f *DeviceFacts) { f.PausedAt, f.PausedUntil = ago(hour), ahead(5*hour) }
	modIndefPause devMod = func(f *DeviceFacts) { f.PausedAt, f.PausedUntil = ago(hour), 0 }
	modMissed     devMod = func(f *DeviceFacts) { setSuccess(f, 71*hour); f.LastAttemptStart = ago(70 * hour) }
	modDeferred   devMod = func(f *DeviceFacts) { f.ServiceState = "deferred" }
	modOutdated   devMod = func(f *DeviceFacts) { f.AgentVersion = "1.4.0" }
	modPending    devMod = func(f *DeviceFacts) { newlyEnrolled(f, 1*hour) }
)

func customerNamed(name string) CustomerFacts {
	return CustomerFacts{ID: custID(1), Name: name, State: "active"}
}

// rollupOf classifies the facts at t0 and rolls them up for c.
func rollupOf(c CustomerFacts, facts ...DeviceFacts) CustomerState {
	ds := make([]DeviceState, 0, len(facts))
	for _, f := range facts {
		ds = append(ds, Classify(f, t0))
	}
	return Rollup(c, ds, t0)
}

func TestRollupMissingEmptyOnboarding(t *testing.T) {
	expected := func(name string) CustomerFacts { c := customerNamed(name); c.Expected = true; return c }

	t.Run("D01_expected_zero_devices_missing", func(t *testing.T) {
		cs := rollupOf(expected("Northwind Dental"))
		if cs.Status != CustomerMissing || cs.Alarm != SeverityCritical || cs.SortKey != 0 || cs.HealthKnown {
			t.Errorf("status %s alarm %s sortKey %d healthKnown %v", cs.Status, cs.Alarm, cs.SortKey, cs.HealthKnown)
		}
	})
	t.Run("D02_expected_all_retired_missing", func(t *testing.T) {
		cs := rollupOf(expected("Northwind Dental"),
			devWith(1, modRetired), devWith(2, modRetired), devWith(3, modRetired))
		if cs.Status != CustomerMissing || cs.Retired != 3 || cs.Active != 0 || len(cs.Counts) != 0 {
			t.Errorf("status %s retired %d active %d counts %v", cs.Status, cs.Retired, cs.Active, cs.Counts)
		}
	})
	t.Run("D03_expected_with_device_not_missing", func(t *testing.T) {
		cs := rollupOf(expected("Northwind Dental"), okDevice(1))
		if cs.Status != CustomerOK || cs.Alarm != SeverityNone {
			t.Errorf("status %s alarm %s, want ok/none", cs.Status, cs.Alarm)
		}
	})
	t.Run("D03_expected_with_only_paused_device_not_missing", func(t *testing.T) {
		cs := rollupOf(expected("Northwind Dental"), devWith(1, modTimedPause))
		if cs.Status == CustomerMissing {
			t.Errorf("a parked device is a device: status %s", cs.Status)
		}
	})
	t.Run("D04_not_expected_zero_devices_empty", func(t *testing.T) {
		cs := rollupOf(customerNamed("Contoso Law"))
		if cs.Status != CustomerEmpty || cs.HealthKnown || cs.HealthPct != 0 || cs.Alarm != SeverityNone || cs.SortKey != 6 {
			t.Errorf("status %s health %v/%v alarm %s sortKey %d", cs.Status, cs.HealthPct, cs.HealthKnown, cs.Alarm, cs.SortKey)
		}
		if cs.Status == CustomerOK {
			t.Errorf("an empty customer must never be green")
		}
	})
	t.Run("D04_not_expected_only_retired_devices_empty", func(t *testing.T) {
		cs := rollupOf(customerNamed("Contoso Law"), devWith(1, modRetired))
		if cs.Status != CustomerEmpty || cs.Retired != 1 {
			t.Errorf("status %s retired %d", cs.Status, cs.Retired)
		}
	})
	t.Run("D05_onboarding_grace_suppresses_missing", func(t *testing.T) {
		c := expected("Fabrikam Auto")
		c.OnboardingUntil = ahead(24 * hour)
		cs := rollupOf(c)
		if cs.Status != CustomerOnboarding || cs.Alarm != SeverityNone || cs.SortKey != 5 {
			t.Errorf("status %s alarm %s sortKey %d", cs.Status, cs.Alarm, cs.SortKey)
		}
	})
	t.Run("D06_onboarding_expired_missing", func(t *testing.T) {
		c := expected("Fabrikam Auto")
		c.OnboardingUntil = ago(1 * time.Millisecond)
		if cs := rollupOf(c); cs.Status != CustomerMissing || cs.Alarm != SeverityCritical {
			t.Errorf("status %s alarm %s", cs.Status, cs.Alarm)
		}
		c.OnboardingUntil = ms(t0) // until == now is over
		if cs := rollupOf(c); cs.Status != CustomerMissing {
			t.Errorf("onboarding_until == now: status %s, want missing", cs.Status)
		}
		c.OnboardingUntil = -3 // garbled never excuses a missing customer
		if cs := rollupOf(c); cs.Status != CustomerMissing {
			t.Errorf("garbled onboarding_until: status %s, want missing", cs.Status)
		}
	})
	t.Run("D08_archived_never_missing", func(t *testing.T) {
		c := expected("Wingtip Plumbing")
		c.State = "archived"
		if cs := rollupOf(c); cs.Status != CustomerEmpty {
			t.Errorf("archived with no devices: status %s, want empty (never MISSING)", cs.Status)
		}
	})
}

func TestRollupStatusAndHealth(t *testing.T) {
	c := customerNamed("Northwind Dental")

	t.Run("D07_suspended_mutes_notifications_not_state", func(t *testing.T) {
		sc := c
		sc.State = "suspended"
		cs := rollupOf(sc, okDevice(1), devWith(2, modFailed))
		if cs.Status != CustomerAlarm || !cs.Muted || cs.Alarm != SeverityCritical || cs.Counts[StateFailed] != 1 {
			t.Errorf("status %s muted %v alarm %s counts %v", cs.Status, cs.Muted, cs.Alarm, cs.Counts)
		}
		for _, d := range cs.Devices {
			if d.Hostname == "ws-desk-02" && (d.Primary != StateFailed || d.Alarm != SeverityCritical) {
				t.Errorf("device state changed by suspension: %s %s", d.Primary, d.Alarm)
			}
		}
		if active := rollupOf(c, okDevice(1), devWith(2, modFailed)); active.Muted {
			t.Errorf("an active customer is not muted")
		}
	})

	t.Run("D09_rollup_worst_state_wins", func(t *testing.T) {
		cs := rollupOf(c, okDevice(1), devWith(2, modStale), devWith(3, modMissed))
		if cs.Status != CustomerAlarm || cs.Alarm != SeverityCritical || cs.SortKey != 2 {
			t.Errorf("status %s alarm %s", cs.Status, cs.Alarm)
		}
		if cs.Counts[StateOK] != 1 || cs.Counts[StateStale] != 1 || cs.Counts[StateMissed] != 1 {
			t.Errorf("counts = %v", cs.Counts)
		}
	})
	t.Run("D09_alarm_severity_without_critical", func(t *testing.T) {
		cs := rollupOf(c, devWith(1, func(f *DeviceFacts) { f.Escrow = nil }))
		if cs.Status != CustomerAlarm || cs.Alarm != SeverityAlarm {
			t.Errorf("status %s alarm %s, want alarm/alarm", cs.Status, cs.Alarm)
		}
	})

	t.Run("D10_health_percent_excludes_paused_retired", func(t *testing.T) {
		facts := []DeviceFacts{
			devWith(1, nil), devWith(2, nil), devWith(3, nil), devWith(4, nil), devWith(5, nil), devWith(6, nil),
			devWith(7, modStale),
			devWith(8, modTimedPause), devWith(9, modIndefPause),
			devWith(10, modRetired),
		}
		cs := rollupOf(c, facts...)
		if cs.Active != 7 || cs.Paused != 2 || cs.Retired != 1 {
			t.Errorf("active %d paused %d retired %d, want 7/2/1", cs.Active, cs.Paused, cs.Retired)
		}
		if !cs.HealthKnown || math.Abs(cs.HealthPct-100.0*6/7) > 1e-9 {
			t.Errorf("health = %v known %v, want 6/7", cs.HealthPct, cs.HealthKnown)
		}
	})
	t.Run("D10_pending_first_is_in_the_denominator_not_the_numerator", func(t *testing.T) {
		cs := rollupOf(c, okDevice(1), devWith(2, modPending))
		if !cs.HealthKnown || cs.HealthPct != 50 || cs.Status != CustomerOK {
			t.Errorf("health %v status %s", cs.HealthPct, cs.Status)
		}
	})
	t.Run("D11_health_percent_zero_denominator_na", func(t *testing.T) {
		cs := rollupOf(c, devWith(1, modTimedPause), devWith(2, modIndefPause))
		if cs.HealthKnown || cs.HealthPct != 0 {
			t.Errorf("health = %v known %v, want n/a", cs.HealthPct, cs.HealthKnown)
		}
		if empty := rollupOf(c); empty.HealthKnown {
			t.Errorf("no devices: health known")
		}
	})
	t.Run("D13_paused_only_customer_not_green", func(t *testing.T) {
		cs := rollupOf(c, devWith(1, modTimedPause), devWith(2, modTimedPause))
		if cs.Status != CustomerWarn || cs.Alarm != SeverityNone || cs.SortKey != 3 || cs.Paused != 2 {
			t.Errorf("status %s alarm %s sortKey %d paused %d, want warn", cs.Status, cs.Alarm, cs.SortKey, cs.Paused)
		}
	})
	t.Run("D13_paused_device_with_alarm_still_alarms", func(t *testing.T) {
		cs := rollupOf(c, devWith(1, func(f *DeviceFacts) { modIndefPause(f); f.Escrow = nil }))
		if cs.Status != CustomerAlarm || cs.Alarm != SeverityAlarm {
			t.Errorf("status %s alarm %s", cs.Status, cs.Alarm)
		}
	})
}

func TestRollupWarnOkUnknown(t *testing.T) {
	c := customerNamed("Northwind Dental")
	cases := []struct {
		name  string
		facts []DeviceFacts
		want  CustomerStatus
	}{
		{"every_device_ok", []DeviceFacts{okDevice(1), okDevice(2)}, CustomerOK},
		{"pending_first_counts_as_ok", []DeviceFacts{okDevice(1), devWith(2, modPending)}, CustomerOK},
		{"timed_pause_among_ok_devices_stays_ok", []DeviceFacts{okDevice(1), devWith(2, modTimedPause)}, CustomerOK},
		{"indefinite_pause_warns", []DeviceFacts{okDevice(1), devWith(2, modIndefPause)}, CustomerWarn},
		{"missed_warns", []DeviceFacts{okDevice(1), devWith(2, modMissed)}, CustomerWarn},
		{"deferred_warns", []DeviceFacts{okDevice(1), devWith(2, modDeferred)}, CustomerWarn},
		{"outdated_warns", []DeviceFacts{okDevice(1), devWith(2, modOutdated)}, CustomerWarn},
		{"outdated_alarm_override_alarms", []DeviceFacts{devWith(1, func(f *DeviceFacts) { modOutdated(f); f.Thresholds.OutdatedIsAlarm = true })}, CustomerAlarm},
		{"unknown_device_makes_customer_unknown", []DeviceFacts{okDevice(1), devWith(2, modUnknown)}, CustomerUnknown},
		{"unknown_beats_alarm", []DeviceFacts{devWith(1, modFailed), devWith(2, modUnknown)}, CustomerUnknown},
		{"paused_device_with_incomplete_facts_is_unknown", []DeviceFacts{devWith(1, func(f *DeviceFacts) { modTimedPause(f); f.Complete = false })}, CustomerUnknown},
		{"retired_device_does_not_matter", []DeviceFacts{okDevice(1), devWith(2, func(f *DeviceFacts) { modRetired(f); modStale(f) })}, CustomerOK},
	}
	for _, tc := range cases {
		t.Run("D09_"+tc.name, func(t *testing.T) {
			cs := rollupOf(c, tc.facts...)
			if cs.Status != tc.want {
				t.Errorf("status = %s, want %s (counts %v)", cs.Status, tc.want, cs.Counts)
			}
			if tc.want == CustomerUnknown && cs.Alarm != SeverityCritical {
				t.Errorf("an UNKNOWN-containing customer must alarm critical, got %s", cs.Alarm)
			}
		})
	}

	t.Run("D09_rollup_is_independent_of_input_order_and_does_not_mutate_it", func(t *testing.T) {
		facts := []DeviceFacts{devWith(3, modStale), okDevice(1), devWith(2, modMissed), devWith(4, modFailed)}
		var ds []DeviceState
		for _, f := range facts {
			ds = append(ds, Classify(f, t0))
		}
		snapshot := append([]DeviceState(nil), ds...)
		a := Rollup(c, ds, t0)
		rev := make([]DeviceState, len(ds))
		for i := range ds {
			rev[len(ds)-1-i] = ds[i]
		}
		b := Rollup(c, rev, t0)
		if !reflect.DeepEqual(a, b) {
			t.Errorf("order of input changed the rollup:\n%+v\n%+v", a, b)
		}
		if !reflect.DeepEqual(ds, snapshot) {
			t.Errorf("Rollup reordered or modified its input")
		}
	})
}

func TestRollupSortOrderAndArchived(t *testing.T) {
	// One customer of every status. Names are chosen so alphabetical order is the reverse of the required order.
	var cust []CustomerFacts
	var devs []DeviceFacts
	n := 0
	add := func(name string, expected bool, onboarding Millis, mods ...devMod) {
		id := len(cust) + 1
		cust = append(cust, CustomerFacts{ID: custID(id), Name: name, State: "active", Expected: expected, OnboardingUntil: onboarding})
		for _, m := range mods {
			n++
			d := deviceOf(n, id, fmt.Sprintf("host-%02d", n))
			if m != nil {
				m(&d)
			}
			devs = append(devs, d)
		}
	}
	add("Alpha Empty", false, 0)
	add("Bravo Onboarding", true, ahead(24*hour))
	add("Charlie Ok", false, 0, nil)
	add("Delta Warn", false, 0, modMissed)
	add("Echo Alarm", false, 0, modFailed)
	add("Foxtrot Unknown", false, 0, modUnknown)
	add("Golf Missing", true, 0)

	t.Run("D12_sort_order", func(t *testing.T) {
		// archived customers are not part of the order
		cust = append(cust, CustomerFacts{ID: custID(50), Name: "Aardvark Archived", State: "archived"})
		w := worldOf(cust, devs)
		v := Evaluate(w, t0)
		var got []string
		var statuses []CustomerStatus
		for _, c := range v.Customers {
			got = append(got, c.Name)
			statuses = append(statuses, c.Status)
		}
		want := []string{"Golf Missing", "Foxtrot Unknown", "Echo Alarm", "Delta Warn", "Charlie Ok", "Bravo Onboarding", "Alpha Empty"}
		wantStatus := []CustomerStatus{CustomerMissing, CustomerUnknown, CustomerAlarm, CustomerWarn, CustomerOK, CustomerOnboarding, CustomerEmpty}
		if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(statuses, wantStatus) {
			t.Errorf("order = %v (%v), want %v", got, statuses, want)
		}
		for i := 1; i < len(v.Customers); i++ {
			if v.Customers[i-1].SortKey > v.Customers[i].SortKey {
				t.Errorf("sort keys not ascending at %d", i)
			}
		}
		// devices follow their customers' order: the UNKNOWN device (customer tier 1) precedes the FAILED one (tier 2)
		var hosts []string
		for _, d := range v.Devices {
			hosts = append(hosts, d.Hostname)
		}
		if want := []string{"host-04", "host-03", "host-02", "host-01"}; !reflect.DeepEqual(hosts, want) {
			t.Errorf("device order = %v, want %v", hosts, want)
		}
	})

	t.Run("D12_same_status_sorts_by_name_then_id", func(t *testing.T) {
		cs := []CustomerFacts{
			{ID: custID(3), Name: "zulu dental", State: "active"},
			{ID: custID(2), Name: "Alpha Dental", State: "active"},
			{ID: custID(1), Name: "alpha dental", State: "active"},
		}
		v := Evaluate(worldOf(cs, nil), t0)
		var ids []string
		for _, c := range v.Customers {
			ids = append(ids, c.ID)
		}
		// "Alpha Dental" < "alpha dental" as raw strings; case-insensitive names tie, then raw name, then id
		if want := []string{custID(2), custID(1), custID(3)}; !reflect.DeepEqual(ids, want) {
			t.Errorf("ids = %v, want %v", ids, want)
		}
	})

	t.Run("D08_archived_excluded", func(t *testing.T) {
		cs := []CustomerFacts{
			{ID: custID(1), Name: "Northwind Dental", State: "active"},
			{ID: custID(2), Name: "Wingtip Plumbing", State: "archived"},
		}
		ds := []DeviceFacts{deviceOf(1, 1, "ws-front-01"), deviceOf(2, 2, "ws-shop-01")}
		ds[1].LastSuccess = ago(100 * hour) // would be STALE, but the customer is archived
		ds[1].LastBackup = successRun(100 * hour)
		v := Evaluate(worldOf(cs, ds), t0)
		if len(v.Customers) != 1 || v.Customers[0].Name != "Northwind Dental" {
			t.Errorf("customers = %+v", v.Customers)
		}
		if len(v.Devices) != 1 || v.Devices[0].Hostname != "ws-front-01" {
			t.Errorf("devices = %+v", v.Devices)
		}
		if len(v.Banners) != 0 || !v.AllClear() {
			t.Errorf("banners %v allClear %v: an archived customer must not raise MISSING or alarms", v.Banners, v.AllClear())
		}
		// but a failed load is a failed load, archived or not
		ds[1].Complete = false
		if v2 := Evaluate(worldOf(cs, ds), t0); v2.AllClear() || v2.Complete {
			t.Errorf("incomplete facts of an archived customer's device went unnoticed")
		}
	})
}
