package state

import (
	"fmt"
	"reflect"
	"testing"
	"time"
)

func customer(n int, name string) CustomerFacts {
	return CustomerFacts{ID: custID(n), Name: name, State: "active", KeyCustody: "escrowed"}
}

// deviceOf returns a healthy device n of customer c.
func deviceOf(n, c int, host string) DeviceFacts {
	f := baseFacts()
	f.ID, f.CustomerID, f.Hostname = devID(n), custID(c), host
	return f
}

func worldOf(customers []CustomerFacts, devices []DeviceFacts) World {
	return World{
		Devices:             devices,
		Customers:           customers,
		FleetEverEnrolled:   true,
		PreviousDeviceCount: -1,
		LoadComplete:        true,
		LoadedAt:            t0.Add(-time.Second),
	}
}

// healthyFleet is nCust customers with perCust healthy devices each.
func healthyFleet(nCust, perCust int) World {
	var cs []CustomerFacts
	var ds []DeviceFacts
	n := 0
	for c := 1; c <= nCust; c++ {
		cs = append(cs, customer(c, fmt.Sprintf("Customer %02d", c)))
		for d := 1; d <= perCust; d++ {
			n++
			ds = append(ds, deviceOf(n, c, fmt.Sprintf("ws-%02d-%03d", c, d)))
		}
	}
	return worldOf(cs, ds)
}

func bannerIDs(v FleetView) []string {
	var ids []string
	for _, b := range v.Banners {
		ids = append(ids, b.ID)
	}
	return ids
}

func findBanner(v FleetView, id string) (Banner, bool) {
	for _, b := range v.Banners {
		if b.ID == id {
			return b, true
		}
	}
	return Banner{}, false
}

func TestFleetUnreachableAndPartialData(t *testing.T) {
	t.Run("C01_facts_loader_error_all_devices_unknown", func(t *testing.T) {
		w := healthyFleet(2, 3)
		w.LoadComplete = false
		w.LoadErrors = len(w.Devices)
		for i := range w.Devices {
			w.Devices[i].Complete = false
			w.Devices[i].LoadErrorClass = "timeout"
		}
		v := Evaluate(w, t0)
		if len(v.Devices) != 6 {
			t.Fatalf("devices = %d, want 6", len(v.Devices))
		}
		for _, d := range v.Devices {
			if d.Primary != StateUnknown || d.Alarm != SeverityCritical || d.Reason != ReasonFactsUnavailable {
				t.Errorf("%s: %s alarm %s reason %s, want UNKNOWN critical", d.Hostname, d.Primary, d.Alarm, d.Reason)
			}
			if hasState(d.Flags, StateOK) || d.Detail.LoadErrorClass != "timeout" {
				t.Errorf("%s: flags %v detail %+v", d.Hostname, d.Flags, d.Detail)
			}
		}
		for _, c := range v.Customers {
			if c.Status != CustomerUnknown || c.Alarm != SeverityCritical {
				t.Errorf("customer %s status %s: want unknown/critical", c.Name, c.Status)
			}
		}
		b, ok := findBanner(v, "C09")
		if !ok || b.Severity != SeverityCritical || b.Count != 6 {
			t.Errorf("banner C09 = %+v ok=%v", b, ok)
		}
		if v.AllClear() || v.Complete {
			t.Errorf("AllClear=%v Complete=%v, want both false", v.AllClear(), v.Complete)
		}
	})

	t.Run("C02_facts_partial_some_devices_unknown", func(t *testing.T) {
		w := healthyFleet(10, 20) // 200 devices
		for _, i := range []int{7, 91, 150} {
			w.Devices[i].Complete = false
			w.Devices[i].LoadErrorClass = "inconsistent_row"
		}
		v := Evaluate(w, t0)
		unknown, ok := 0, 0
		for _, d := range v.Devices {
			switch d.Primary {
			case StateUnknown:
				unknown++
				if d.Alarm != SeverityCritical {
					t.Errorf("UNKNOWN device %s alarm = %s", d.Hostname, d.Alarm)
				}
			case StateOK:
				ok++
			}
		}
		if unknown != 3 || ok != 197 {
			t.Errorf("unknown %d ok %d, want 3 and 197", unknown, ok)
		}
		b, found := findBanner(v, "C09")
		if !found || b.Count != 3 || b.Text != "Partial data: 3 devices could not be classified" {
			t.Errorf("banner = %+v found=%v", b, found)
		}
		if v.AllClear() {
			t.Errorf("AllClear with 3 unknown devices")
		}
		// the three UNKNOWN devices sort first, before 197 OK ones
		for i := 0; i < 3; i++ {
			if v.Devices[i].Primary != StateUnknown {
				t.Errorf("devices[%d] = %s, want UNKNOWN first", i, v.Devices[i].Primary)
			}
		}
		unknownCustomers := 0
		for _, c := range v.Customers {
			if c.Status == CustomerUnknown {
				unknownCustomers++
			}
		}
		if unknownCustomers == 0 || unknownCustomers > 3 {
			t.Errorf("unknown customers = %d", unknownCustomers)
		}
	})

	t.Run("C02_load_errors_without_any_device_row_still_banner", func(t *testing.T) {
		w := healthyFleet(1, 2)
		w.LoadErrors = 2
		v := Evaluate(w, t0)
		b, ok := findBanner(v, "C09")
		if !ok || b.Count != 2 || v.AllClear() || v.Complete {
			t.Errorf("banner %+v ok=%v allclear=%v complete=%v", b, ok, v.AllClear(), v.Complete)
		}
	})

	t.Run("C03_empty_poll_after_nonempty", func(t *testing.T) {
		w := worldOf([]CustomerFacts{customer(1, "Northwind Dental"), customer(2, "Contoso Law")}, nil)
		w.PreviousDeviceCount = 200
		v := Evaluate(w, t0)
		if !reflect.DeepEqual(bannerIDs(v), []string{"C08", "C09"}) {
			t.Errorf("banners = %v, want [C08 C09]", bannerIDs(v))
		}
		for _, b := range v.Banners {
			if b.Severity != SeverityCritical {
				t.Errorf("banner %s severity %s", b.ID, b.Severity)
			}
		}
		if v.FirstRun {
			t.Errorf("an empty poll after a non-empty one is not a first run")
		}
		if v.Complete || v.AllClear() {
			t.Errorf("Complete=%v AllClear=%v, want false", v.Complete, v.AllClear())
		}
	})

	t.Run("C03_empty_poll_banner_even_if_the_ever_enrolled_flag_was_lost", func(t *testing.T) {
		w := worldOf(nil, nil)
		w.FleetEverEnrolled = false
		w.PreviousDeviceCount = 5
		v := Evaluate(w, t0)
		if !reflect.DeepEqual(bannerIDs(v), []string{"C08", "C09"}) || v.FirstRun || v.AllClear() {
			t.Errorf("banners %v firstRun %v allClear %v", bannerIDs(v), v.FirstRun, v.AllClear())
		}
	})

	t.Run("C04_first_run_empty_fleet_is_info", func(t *testing.T) {
		w := worldOf(nil, nil)
		w.FleetEverEnrolled = false
		v := Evaluate(w, t0)
		if len(v.Banners) != 0 || !v.FirstRun || !v.Complete {
			t.Errorf("banners %v firstRun %v complete %v, want none/true/true", v.Banners, v.FirstRun, v.Complete)
		}
		if !v.AllClear() {
			t.Errorf("a first-run empty fleet is neutral, not an alarm: AllClear should be true")
		}
	})

	t.Run("C04_first_run_but_load_failed_is_not_clear", func(t *testing.T) {
		w := worldOf(nil, nil)
		w.FleetEverEnrolled = false
		w.LoadComplete = false
		v := Evaluate(w, t0)
		if _, ok := findBanner(v, "C09"); !ok || v.AllClear() {
			t.Errorf("banners %v allClear %v", bannerIDs(v), v.AllClear())
		}
	})

	t.Run("C05_empty_after_ever_enrolled_is_alarm", func(t *testing.T) {
		w := worldOf([]CustomerFacts{customer(1, "Northwind Dental")}, nil)
		v := Evaluate(w, t0)
		b, ok := findBanner(v, "C08")
		if !ok || b.Severity != SeverityCritical || b.Text != "No active devices enrolled." {
			t.Errorf("banner = %+v ok=%v", b, ok)
		}
		if _, dup := findBanner(v, "C09"); dup {
			t.Errorf("C09 raised for a complete, consistent load")
		}
		if v.FirstRun || v.AllClear() {
			t.Errorf("firstRun %v allClear %v", v.FirstRun, v.AllClear())
		}
	})

	t.Run("C05_all_devices_retired_is_empty_fleet", func(t *testing.T) {
		w := healthyFleet(1, 2)
		for i := range w.Devices {
			w.Devices[i].RetiredAt = ago(day)
		}
		v := Evaluate(w, t0)
		if _, ok := findBanner(v, "C08"); !ok || v.AllClear() {
			t.Errorf("banners %v allClear %v", bannerIDs(v), v.AllClear())
		}
	})

	t.Run("C05_empty_world_with_failed_load_says_partial_not_empty", func(t *testing.T) {
		w := worldOf(nil, nil)
		w.LoadComplete = false
		w.LoadErrors = 1
		v := Evaluate(w, t0)
		if !reflect.DeepEqual(bannerIDs(v), []string{"C09"}) {
			t.Errorf("banners = %v, want [C09] only", bannerIDs(v))
		}
	})

	t.Run("C02_orphan_device_is_unknown", func(t *testing.T) {
		w := healthyFleet(1, 2)
		w.Devices = append(w.Devices, deviceOf(99, 42, "ws-orphan-01")) // customer 42 was not loaded
		v := Evaluate(w, t0)
		var orphan *DeviceState
		for i := range v.Devices {
			if v.Devices[i].ID == devID(99) {
				orphan = &v.Devices[i]
			}
		}
		if orphan == nil || orphan.Primary != StateUnknown || orphan.Detail.LoadErrorClass != "orphan_device" {
			t.Fatalf("orphan = %+v", orphan)
		}
		if b, ok := findBanner(v, "C09"); !ok || b.Count != 1 || v.AllClear() {
			t.Errorf("banner %+v ok=%v allClear=%v", b, ok, v.AllClear())
		}
	})
}

func TestC21AllClearRequiresEverything(t *testing.T) {
	type row struct {
		name  string
		world func() World
		want  bool
	}
	rows := []row{
		{"C21_all_clear_true_only_when_nothing_is_wrong", func() World { return healthyFleet(3, 3) }, true},
		{"C21_first_run_empty_fleet_is_clear", func() World {
			w := worldOf(nil, nil)
			w.FleetEverEnrolled = false
			return w
		}, true},
		{"C21_false_with_banner_c08", func() World { return worldOf([]CustomerFacts{customer(1, "Northwind Dental")}, nil) }, false},
		{"C21_false_with_banner_c09_load_errors", func() World { w := healthyFleet(2, 2); w.LoadErrors = 1; return w }, false},
		{"C21_false_when_load_incomplete", func() World { w := healthyFleet(2, 2); w.LoadComplete = false; return w }, false},
		{"C21_false_with_unknown_device", func() World { w := healthyFleet(2, 2); w.Devices[0].Complete = false; return w }, false},
		{"C21_false_with_failed_device", func() World {
			w := healthyFleet(2, 2)
			w.Devices[1].LastBackup = failedRun(30*time.Minute, "network")
			w.Devices[1].ConsecutiveFailures = 1
			return w
		}, false},
		{"C21_false_with_alarm_only_device_partial", func() World {
			w := healthyFleet(1, 2)
			w.Devices[0].LastBackup = partialRun(1000, 99000, map[string]int{"permission": 1000})
			return w
		}, false},
		{"C21_false_with_acknowledged_alarm_still_wrong", func() World {
			w := healthyFleet(1, 2)
			w.Devices[0].LastBackup = failedRun(30*time.Minute, "network")
			w.Devices[0].ConsecutiveFailures = 1
			w.Devices[0].Ack = &AckFacts{AckedAt: ago(hour)}
			return w
		}, false},
		{"C21_false_with_missing_customer", func() World {
			w := healthyFleet(2, 2)
			w.Customers = append(w.Customers, CustomerFacts{ID: custID(9), Name: "Fabrikam Auto", State: "active", Expected: true})
			return w
		}, false},
		{"C21_false_with_suspended_customer_alarm_still_wrong", func() World {
			w := healthyFleet(2, 2)
			w.Customers[0].State = "suspended"
			w.Devices[0].LastSuccess = ago(100 * hour)
			w.Devices[0].LastBackup = successRun(100 * hour)
			return w
		}, false},
		{"C21_false_when_fleet_empty_after_nonempty_load", func() World {
			w := worldOf(nil, nil)
			w.PreviousDeviceCount = 3
			return w
		}, false},
		{"C21_false_when_never_enrolled_flag_false_but_devices_unknown", func() World {
			w := healthyFleet(1, 1)
			w.FleetEverEnrolled = false
			w.Devices[0].Complete = false
			return w
		}, false},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			if got := Evaluate(r.world(), t0).AllClear(); got != r.want {
				t.Errorf("AllClear = %v, want %v", got, r.want)
			}
		})
	}

	t.Run("C21_all_clear_method_cases_on_hand_built_views", func(t *testing.T) {
		clear := FleetView{Complete: true, Devices: []DeviceState{{Primary: StateOK}}, ComputedAt: t0}
		if !clear.AllClear() {
			t.Fatal("a complete, non-empty, quiet view must be clear")
		}
		for name, mutate := range map[string]func(*FleetView){
			"banner":        func(v *FleetView) { v.Banners = []Banner{{ID: "C05", Severity: SeverityNone}} },
			"incomplete":    func(v *FleetView) { v.Complete = false },
			"empty":         func(v *FleetView) { v.Devices = nil },
			"device alarm":  func(v *FleetView) { v.Devices = []DeviceState{{Primary: StateOK, Alarm: SeverityAlarm}} },
			"unknown":       func(v *FleetView) { v.Devices = []DeviceState{{Primary: StateUnknown}} },
			"customer":      func(v *FleetView) { v.Customers = []CustomerState{{Alarm: SeverityCritical}} },
			"missing":       func(v *FleetView) { v.Customers = []CustomerState{{Status: CustomerMissing}} },
			"unknown cust.": func(v *FleetView) { v.Customers = []CustomerState{{Status: CustomerUnknown}} },
		} {
			v := clear
			mutate(&v)
			if v.AllClear() {
				t.Errorf("%s: AllClear true", name)
			}
		}
		if (FleetView{}).AllClear() {
			t.Errorf("the zero FleetView must not be clear")
		}
		if !(FleetView{Complete: true, FirstRun: true}).AllClear() {
			t.Errorf("first run, complete, nothing wrong: clear")
		}
	})
}

func TestC23FreshnessStampInEveryFleetView(t *testing.T) {
	worlds := map[string]World{
		"empty":      {},
		"first run":  func() World { w := worldOf(nil, nil); w.FleetEverEnrolled = false; return w }(),
		"healthy":    healthyFleet(2, 2),
		"incomplete": func() World { w := healthyFleet(1, 1); w.LoadComplete = false; return w }(),
		"empty poll": func() World { w := worldOf(nil, nil); w.PreviousDeviceCount = 9; return w }(),
	}
	est := time.FixedZone("EST", -5*3600)
	for name, w := range worlds {
		t.Run("C23_freshness_stamp_in_every_fleetview_"+name, func(t *testing.T) {
			v := Evaluate(w, t0.In(est))
			if v.ComputedAt.IsZero() || !v.ComputedAt.Equal(t0) || v.ComputedAt.Location() != time.UTC {
				t.Errorf("ComputedAt = %v (%v), want %v in UTC", v.ComputedAt, v.ComputedAt.Location(), t0)
			}
			if !v.LoadedAt.Equal(w.LoadedAt) {
				t.Errorf("LoadedAt = %v, want %v", v.LoadedAt, w.LoadedAt)
			}
		})
	}
}

// TestFleetProbeCasesPendingM3 keeps the ids of the probe-driven cases of
// architecture 4.8 visible. They need World.Probes, which arrives at M3/M4.
func TestFleetProbeCasesPendingM3(t *testing.T) {
	for _, name := range []string{
		"C06_gateway_probe_failing_banner",
		"C07_gateway_single_probe_failure_no_banner",
		"C08_storage_probe_failed_banner",
		"C09_smtp_failing_banner",
		"C10_smtp_recovers_clears_banner",
		"C11_agent_api_silent_banner",
		"C12_agent_api_silent_not_when_all_paused_or_empty",
		"C13_evaluator_stalled_banner",
		"C14_maintenance_worker_down_banner",
		"C15_cert_expiring_warn_and_critical",
		"C16_stale_view_freshness_banner",
		"C17_db_degraded_banner",
		"C18_master_key_unavailable_banner",
		"C19_many_deferred_gateway_collapse_to_one_banner",
		"C20_customer_wide_silence_collapses",
		"C22_digest_lists_unknown_and_missing_even_when_rest_green",
		"C24_clock_suspect_banner",
		"C25_digest_not_sent_banner",
		"C26_quota_thresholds",
		"C27_quota_growth_projection",
		"C28_release_manifest_invalid_banner",
	} {
		t.Run(name, func(t *testing.T) { t.Skip("M3") })
	}
}
