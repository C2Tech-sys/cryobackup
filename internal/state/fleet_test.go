package state

import (
	"fmt"
	"reflect"
	"strings"
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
		Probes:              healthyProbes(), // every check wired and passing at t0
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

// Group C cases that live outside this package: C11
// (agent_endpoint_probe_failing_banner, ADR 0013) is the self-probe of
// internal/console/health (endpoint.go, endpoint_test.go). C12 is here, as the
// engine-side half: an offline fleet raises no console banner.

func TestC22DigestListsUnknownAndMissing(t *testing.T) {
	t.Run("C22_digest_lists_unknown_and_missing_even_when_rest_green", func(t *testing.T) {
		w := healthyFleet(3, 4)
		w.Customers = append(w.Customers, CustomerFacts{ID: custID(9), Name: "Fabrikam Auto", State: "active", Expected: true})
		w.Devices[5].Complete, w.Devices[5].LoadErrorClass = false, "timeout"
		v := Evaluate(w, t0)
		d := DigestOf(v, t0)
		if len(d.Lines) != 2 {
			t.Fatalf("lines = %+v, want the MISSING customer and the UNKNOWN device", d.Lines)
		}
		if l := d.Lines[0]; l.Kind != DigestMissing || l.CustomerName != "Fabrikam Auto" || l.DeviceID != "" {
			t.Errorf("first line %+v, want MISSING Fabrikam Auto", l)
		}
		if l := d.Lines[1]; l.Kind != DigestUnknown || l.DeviceID != devID(6) || l.State != StateUnknown || l.CustomerName != "Customer 02" {
			t.Errorf("second line %+v, want the UNKNOWN device", l)
		}
		if d.AllClear || len(d.Banners) == 0 || d.Banners[0].ID != "C09" || !d.ComputedAt.Equal(t0) {
			t.Errorf("allClear %v banners %v computed %v", d.AllClear, d.Banners, d.ComputedAt)
		}
	})
	t.Run("C22_digest_of_a_green_fleet_is_empty_and_clear", func(t *testing.T) {
		d := DigestOf(Evaluate(healthyFleet(2, 3), t0), t0)
		if len(d.Lines) != 0 || len(d.Banners) != 0 || !d.AllClear {
			t.Errorf("digest %+v", d)
		}
	})
	t.Run("C22_digest_sections_in_order", func(t *testing.T) {
		w := healthyFleet(1, 9)
		w.Customers = append(w.Customers, CustomerFacts{ID: custID(8), Name: "Alpine Clinic", State: "active", Expected: true})
		w.Devices = append(w.Devices, deviceOf(99, 7, "ws-orphan-01")) // customer 7 not loaded: UNKNOWN, still listed
		w.Devices[0].PausedAt = ago(day)                               // parked, indefinite
		w.Devices[1].LastSuccess, w.Devices[1].LastBackup = ago(100*hour), successRun(100*hour)
		w.Devices[2].LastBackup, w.Devices[2].ConsecutiveFailures = failedRun(hour, "network"), 1
		w.Devices[3].LastSeen = ago(200 * hour)
		deferGateway(&w.Devices[4]) // deferred 1 h: not listed
		deferGateway(&w.Devices[5])
		w.Devices[5].LastBackup.StartedAt, w.Devices[5].LastBackup.FinishedAt = ago(30*hour), ago(30*hour)
		w.Devices[5].LastSuccess = ago(40 * hour)
		v := Evaluate(w, t0)
		var got []DigestKind
		for _, l := range DigestOf(v, t0).Lines {
			got = append(got, l.Kind)
		}
		want := []DigestKind{DigestMissing, DigestUnknown, DigestUnseen, DigestFailed, DigestStale, DigestParked, DigestDeferred}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("sections = %v, want %v", got, want)
		}
		for _, l := range DigestOf(v, t0).Lines {
			if l.Kind == DigestParked && !hasAttr(l.Attrs, AttrPausedIndefinite) {
				t.Errorf("parked line does not flag the indefinite pause: %+v", l)
			}
		}
	})
	t.Run("C22_digest_names_every_alarm_flag_not_only_the_primary", func(t *testing.T) {
		w := healthyFleet(1, 3)
		w.Devices[0].PausedAt = ago(day) // parked, and its escrow was never verified
		w.Devices[0].Escrow = &EscrowFacts{Custody: "escrowed", CreatedAt: ago(30 * day)}
		newlyEnrolled(&w.Devices[1], 30*hour) // pending first backup, escrow never verified past the grace
		w.Devices[1].Escrow = &EscrowFacts{Custody: "escrowed", CreatedAt: ago(30 * hour)}
		w.Devices[2].ExpectedVersion, w.Devices[2].Thresholds.OutdatedIsAlarm = "9.0.0", true // OUTDATED as an alarm
		lines := DigestOf(Evaluate(w, t0), t0).Lines
		byHost := map[string]DigestLine{}
		for _, l := range lines {
			byHost[l.Hostname] = l
		}
		if l := byHost[w.Devices[0].Hostname]; l.Kind != DigestParked || l.State != StatePaused ||
			!reflect.DeepEqual(l.Alarms, []State{StateEscrowUnverified}) || l.Alarm != SeverityAlarm {
			t.Errorf("parked device line %+v, want parked naming ESCROW_UNVERIFIED", l)
		}
		if l := byHost[w.Devices[1].Hostname]; l.Kind != DigestEscrow || l.State != StatePendingFirst ||
			!reflect.DeepEqual(l.Alarms, []State{StateEscrowUnverified}) {
			t.Errorf("pending device line %+v, want listed under escrow although its primary is not listed", l)
		}
		if l := byHost[w.Devices[2].Hostname]; l.Kind != DigestOutdated || !reflect.DeepEqual(l.Alarms, []State{StateOutdated}) {
			t.Errorf("outdated-alarm device line %+v", l)
		}
		if len(lines) != 3 {
			t.Errorf("lines %+v, want the three alarming devices", lines)
		}
	})
	t.Run("C22_digest_suspended_customer_is_listed_and_labelled", func(t *testing.T) {
		w := healthyFleet(1, 2)
		w.Customers[0].State = "suspended"
		w.Devices[0].LastBackup, w.Devices[0].ConsecutiveFailures = failedRun(hour, "network"), 1
		d := DigestOf(Evaluate(w, t0), t0)
		if len(d.Lines) != 1 || !d.Lines[0].Muted || d.Lines[0].Kind != DigestFailed {
			t.Errorf("lines %+v", d.Lines)
		}
	})
}

// ---- probe-driven banners (architecture 4.6; 4.8 C06-C10, C13-C18, C25-C28) ----------

func okProbe() ProbeRun {
	return ProbeRun{LastRunAt: ago(30 * time.Second), LastOKAt: ago(30 * time.Second), CertNotAfter: ahead(90 * day)}
}

// healthyProbes is every check wired and passing at t0 (display zone UTC).
func healthyProbes() ProbeFacts {
	gw, st := okProbe(), okProbe()
	return ProbeFacts{
		Wired:          true,
		StartedAt:      ago(2 * day),
		ProberInterval: time.Minute,
		Gateway:        &gw,
		Storage:        &st,
		SMTP:           &SMTPFacts{Probe: okProbe(), LastSendOKAt: ago(hour)},
		Evaluator:      &JobFacts{LastFinishedAt: ago(20 * time.Second), Period: time.Minute},
		Maintain:       &MaintainFacts{RPC: okProbe(), Scheduler: &JobFacts{LastFinishedAt: ago(4 * time.Minute), Period: 5 * time.Minute}},
		View:           &ViewFacts{RefreshInterval: 30 * time.Second},
		Database: &DatabaseFacts{IntegrityCheckedAt: ago(day), IntegrityOK: true, MeasuredAt: ago(time.Minute),
			FreeBytes: 60 << 30, TotalBytes: 100 << 30, WALBytes: 4 << 20, SelfBackup: &SelfBackupFacts{LastAt: ago(10 * hour)}},
		MasterKey: &MasterKeyFacts{Loaded: true, KeyID: "mk-1", EscrowKeyIDs: []string{"mk-1"}},
		Digest:    &DigestFacts{LocalTime: "07:00", LastSentAt: ago(5 * hour)},
		Quota: &QuotaFacts{Targets: []TargetUsage{{ID: "st-1", Name: "primary", SoftQuotaBytes: 10 << 40, UsedBytes: 5 << 40,
			MeasuredAt: ago(hour), Samples: []UsageSample{{ago(7 * day), 5 << 40}, {ago(hour), 5 << 40}}}}},
		Release: &ReleaseFacts{CheckedAt: ago(10 * time.Minute)},
	}
}

// probeCase is one row of a probe-banner table: mod changes a healthy, fully
// probed fleet; want is the exact banner list as "ID/severity" strings.
type probeCase struct {
	name     string
	mod      func(p *ProbeFacts, w *World)
	now      time.Time // zero means t0
	want     []string
	contains string // when set, the first wanted banner's text must contain it
}

func runProbeCases(t *testing.T, cases []probeCase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := healthyFleet(2, 3)
			w.Probes = healthyProbes()
			if c.mod != nil {
				c.mod(&w.Probes, &w)
			}
			now := c.now
			if now.IsZero() {
				now = t0
			}
			v := Evaluate(w, now)
			var got []string
			for _, b := range v.Banners {
				got = append(got, b.ID+"/"+b.Severity.String())
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("banners = %v, want %v (%+v)", got, c.want, v.Banners)
			}
			if len(c.want) > 0 {
				if v.AllClear() {
					t.Errorf("AllClear true with banners %v", got)
				}
				if c.contains != "" && !strings.Contains(v.Banners[0].Text, c.contains) {
					t.Errorf("text %q does not contain %q", v.Banners[0].Text, c.contains)
				}
			} else if !v.AllClear() {
				t.Errorf("AllClear false with no banners")
			}
		})
	}
}

func TestProbeBannersAllHealthyIsClear(t *testing.T) {
	runProbeCases(t, []probeCase{{name: "C21_all_clear_with_every_probe_wired_and_passing"}})
}

func TestC06C07GatewayProbe(t *testing.T) {
	runProbeCases(t, []probeCase{
		{name: "C06_gateway_probe_failing_banner", mod: func(p *ProbeFacts, _ *World) {
			p.Gateway.ConsecutiveFailures, p.Gateway.LastOKAt = 3, ago(4*time.Minute)
		}, want: []string{"C01/critical"}, contains: "Gateway unreachable: no agent can back up. Last OK 11:56 AM."},
		{name: "C06_gateway_failing_last_ok_in_display_zone", mod: func(p *ProbeFacts, w *World) {
			p.Gateway.ConsecutiveFailures, p.Gateway.LastOKAt = 5, ago(4*time.Minute)
			w.DisplayZone = time.FixedZone("CDT", -5*3600)
		}, want: []string{"C01/critical"}, contains: "Last OK 6:56 AM."},
		{name: "C06_gateway_never_answered", mod: func(p *ProbeFacts, _ *World) {
			p.Gateway.ConsecutiveFailures, p.Gateway.LastOKAt = 3, 0
		}, want: []string{"C01/critical"}, contains: "It has never answered a probe."},
		{name: "C06_gateway_failing_streak_outlives_a_stopped_prober", mod: func(p *ProbeFacts, _ *World) {
			p.Gateway.ConsecutiveFailures, p.Gateway.LastRunAt = 4, ago(hour)
		}, want: []string{"C01/critical"}},
		{name: "C07_gateway_single_probe_failure_no_banner", mod: func(p *ProbeFacts, _ *World) {
			p.Gateway.ConsecutiveFailures = 1
		}},
		{name: "C07_gateway_two_probe_failures_no_banner", mod: func(p *ProbeFacts, _ *World) {
			p.Gateway.ConsecutiveFailures = 2
		}},
		{name: "C06_gateway_never_probed_is_not_checked", mod: func(p *ProbeFacts, _ *World) {
			*p.Gateway = ProbeRun{}
		}, want: []string{"C01b/alarm"}, contains: "Gateway not checked: the health prober has not completed a probe of it yet"},
		{name: "C06_gateway_probe_stopped_is_not_checked", mod: func(p *ProbeFacts, _ *World) {
			p.Gateway.LastRunAt = ago(10 * time.Minute)
		}, want: []string{"C01b/alarm"}, contains: "no probe of it has completed for 10 min"},
		{name: "C06_gateway_probe_garbled_time_is_not_checked", mod: func(p *ProbeFacts, _ *World) {
			p.Gateway.LastRunAt = 1_700_000_000 // seconds, not millis
		}, want: []string{"C01b/alarm"}},
		{name: "C06_gateway_three_intervals_boundary_is_still_checked", mod: func(p *ProbeFacts, _ *World) {
			p.Gateway.LastRunAt = ago(3 * time.Minute)
		}},
		{name: "C06_zero_prober_interval_means_sixty_seconds", mod: func(p *ProbeFacts, _ *World) {
			p.ProberInterval, p.Gateway.LastRunAt = 0, ago(181*time.Second)
		}, want: []string{"C01b/alarm"}},
	})
}

func TestC08StorageProbe(t *testing.T) {
	runProbeCases(t, []probeCase{
		{name: "C08_storage_probe_failed_banner", mod: func(p *ProbeFacts, _ *World) {
			p.Storage.ConsecutiveFailures, p.Storage.LastOKAt = 2, ago(14*time.Minute)
		}, want: []string{"C02/critical"}, contains: "Storage unreachable from the admin path: retention, restore and usage are impaired. Last OK 11:46 AM."},
		{name: "C08_storage_single_failure_no_banner", mod: func(p *ProbeFacts, _ *World) { p.Storage.ConsecutiveFailures = 1 }},
		{name: "C08_storage_never_probed_is_not_checked", mod: func(p *ProbeFacts, _ *World) { *p.Storage = ProbeRun{} },
			want: []string{"C02b/alarm"}, contains: "Storage not checked"},
	})
}

func TestC09C10SMTP(t *testing.T) {
	runProbeCases(t, []probeCase{
		{name: "C09_smtp_failing_banner", mod: func(p *ProbeFacts, _ *World) {
			p.SMTP.ConsecutiveSendFailures, p.SMTP.LastSendOKAt = 3, ago(26*hour)
		}, want: []string{"C03/critical"}, contains: "Alert e-mail cannot be delivered: alarms may go unnoticed. Last send Wed 21 Oct 10:00 AM."},
		{name: "C09_smtp_two_failed_sends_no_banner", mod: func(p *ProbeFacts, _ *World) { p.SMTP.ConsecutiveSendFailures = 2 }},
		{name: "C09_smtp_test_send_failed", mod: func(p *ProbeFacts, _ *World) { p.SMTP.LastTestFailed = true },
			want: []string{"C03/critical"}, contains: "Last send 11:00 AM."},
		{name: "C09_smtp_alerts_pending_no_send_for_a_day", mod: func(p *ProbeFacts, _ *World) {
			p.SMTP.PendingAlerts, p.SMTP.LastSendOKAt = 4, ago(25*hour)
		}, want: []string{"C03/critical"}},
		{name: "C09_smtp_alerts_pending_never_delivered", mod: func(p *ProbeFacts, _ *World) {
			p.SMTP.PendingAlerts, p.SMTP.LastSendOKAt = 1, 0
		}, want: []string{"C03/critical"}, contains: "No alert e-mail has been delivered yet."},
		{name: "C09_smtp_alerts_pending_recent_send_no_banner", mod: func(p *ProbeFacts, _ *World) { p.SMTP.PendingAlerts = 3 }},
		{name: "C09_smtp_probe_failing_three_times", mod: func(p *ProbeFacts, _ *World) { p.SMTP.Probe.ConsecutiveFailures = 3 },
			want: []string{"C03/critical"}},
		{name: "C09_smtp_never_probed_never_sent_is_not_checked", mod: func(p *ProbeFacts, _ *World) { *p.SMTP = SMTPFacts{} },
			want: []string{"C03b/alarm"}, contains: "Alert e-mail not checked"},
		{name: "C09_smtp_never_probed_but_delivered_today_is_evidence", mod: func(p *ProbeFacts, _ *World) { p.SMTP.Probe = ProbeRun{} }},
		{name: "C10_smtp_recovers_clears_banner", mod: func(p *ProbeFacts, _ *World) {
			p.SMTP.ConsecutiveSendFailures, p.SMTP.LastSendOKAt, p.SMTP.PendingAlerts = 0, ago(time.Minute), 0
		}},
	})
	t.Run("C10_smtp_recovers_clears_banner_transition", func(t *testing.T) {
		w := healthyFleet(1, 2)
		w.Probes = healthyProbes()
		w.Probes.SMTP.ConsecutiveSendFailures, w.Probes.SMTP.LastSendOKAt = 3, ago(2*day)
		if _, ok := findBanner(Evaluate(w, t0), "C03"); !ok {
			t.Fatal("no C03 while failing")
		}
		w.Probes.SMTP.ConsecutiveSendFailures, w.Probes.SMTP.LastSendOKAt = 0, ago(time.Minute)
		if v := Evaluate(w, t0); len(v.Banners) != 0 || !v.AllClear() {
			t.Fatalf("after recovery: %v", bannerIDs(v))
		}
	})
}

func TestC13EvaluatorStalled(t *testing.T) {
	runProbeCases(t, []probeCase{
		{name: "C13_evaluator_stalled_banner", mod: func(p *ProbeFacts, _ *World) {
			p.Evaluator.LastFinishedAt = ago(10 * time.Minute)
		}, want: []string{"C06/critical"}, contains: "Alerting is stalled: this view and notifications may be stale. Last evaluation 11:50 AM."},
		{name: "C13_evaluator_within_three_periods_no_banner", mod: func(p *ProbeFacts, _ *World) {
			p.Evaluator.LastFinishedAt = ago(3 * time.Minute)
		}},
		{name: "C13_evaluator_zero_period_means_sixty_seconds", mod: func(p *ProbeFacts, _ *World) {
			p.Evaluator.Period, p.Evaluator.LastFinishedAt = 0, ago(181*time.Second)
		}, want: []string{"C06/critical"}},
		{name: "C13_evaluator_never_finished_just_started_is_not_checked", mod: func(p *ProbeFacts, _ *World) {
			p.StartedAt, p.Evaluator.LastFinishedAt = ago(time.Minute), 0
		}, want: []string{"C06b/alarm"}, contains: "Alerting has not run yet"},
		{name: "C13_evaluator_never_finished_long_after_start_is_stalled", mod: func(p *ProbeFacts, _ *World) {
			p.StartedAt, p.Evaluator.LastFinishedAt = ago(hour), 0
		}, want: []string{"C06/critical"}, contains: "No evaluation has finished since the console started."},
		{name: "C13_evaluator_never_finished_unknown_start_is_stalled", mod: func(p *ProbeFacts, _ *World) {
			p.StartedAt, p.Evaluator.LastFinishedAt = 0, 0
		}, want: []string{"C06/critical"}},
	})
}

func TestC14MaintenanceWorker(t *testing.T) {
	runProbeCases(t, []probeCase{
		{name: "C14_maintenance_worker_down_banner", mod: func(p *ProbeFacts, _ *World) {
			p.Maintain.RPC.ConsecutiveFailures = 1
		}, want: []string{"C07/critical"}, contains: "Maintenance worker is not running: retention, restore browsing and usage sampling are off."},
		{name: "C14_maintenance_scheduler_stale_30_min", mod: func(p *ProbeFacts, _ *World) {
			p.Maintain.Scheduler.LastFinishedAt = ago(45 * time.Minute)
		}, want: []string{"C07/critical"}, contains: "The maintenance scheduler last finished at 11:15 AM."},
		{name: "C14_maintenance_rpc_down_and_scheduler_stale_is_one_banner", mod: func(p *ProbeFacts, _ *World) {
			p.Maintain.RPC.ConsecutiveFailures, p.Maintain.Scheduler.LastFinishedAt = 2, ago(2*hour)
		}, want: []string{"C07/critical"}},
		{name: "C14_maintenance_rpc_never_probed_is_not_checked", mod: func(p *ProbeFacts, _ *World) {
			p.Maintain.RPC = ProbeRun{}
		}, want: []string{"C07b/alarm"}, contains: "Maintenance worker not checked"},
		{name: "C14_maintenance_scheduler_first_run_pending_is_not_checked", mod: func(p *ProbeFacts, _ *World) {
			p.StartedAt, p.Maintain.Scheduler.LastFinishedAt = ago(10*time.Minute), 0
		}, want: []string{"C07b/alarm"}, contains: "has not finished its first run"},
		{name: "C14_maintenance_scheduler_never_ran_long_after_start", mod: func(p *ProbeFacts, _ *World) {
			p.StartedAt, p.Maintain.Scheduler.LastFinishedAt = ago(2*hour), 0
		}, want: []string{"C07/critical"}},
		{name: "C14_maintenance_scheduler_not_in_this_build", mod: func(p *ProbeFacts, _ *World) {
			p.Maintain.Scheduler = nil
		}},
	})
}

func TestC15GatewayCertificate(t *testing.T) {
	runProbeCases(t, []probeCase{
		{name: "C15_cert_expiring_warn_and_critical_20_days_warn", mod: func(p *ProbeFacts, _ *World) {
			p.Gateway.CertNotAfter = ahead(20*day + hour)
		}, want: []string{"C05/alarm"}, contains: "Gateway certificate expires in 20 days."},
		{name: "C15_cert_expiring_warn_and_critical_6_days_critical", mod: func(p *ProbeFacts, _ *World) {
			p.Gateway.CertNotAfter = ahead(6*day + hour)
		}, want: []string{"C05/critical"}, contains: "Gateway certificate expires in 6 days."},
		{name: "C15_cert_21_days_no_banner", mod: func(p *ProbeFacts, _ *World) { p.Gateway.CertNotAfter = ahead(21 * day) }},
		{name: "C15_cert_one_day", mod: func(p *ProbeFacts, _ *World) { p.Gateway.CertNotAfter = ahead(day + hour) },
			want: []string{"C05/critical"}, contains: "expires in 1 day."},
		{name: "C15_cert_hours_left", mod: func(p *ProbeFacts, _ *World) { p.Gateway.CertNotAfter = ahead(5 * hour) },
			want: []string{"C05/critical"}, contains: "expires in less than a day."},
		{name: "C15_cert_expired", mod: func(p *ProbeFacts, _ *World) { p.Gateway.CertNotAfter = ago(hour) },
			want: []string{"C05/critical"}, contains: "Gateway certificate has expired"},
		{name: "C15_cert_not_recorded_is_not_checked", mod: func(p *ProbeFacts, _ *World) { p.Gateway.CertNotAfter = 0 },
			want: []string{"C05b/alarm"}, contains: "Gateway certificate not checked"},
		{name: "C15_cert_not_judged_while_gateway_fails", mod: func(p *ProbeFacts, _ *World) {
			p.Gateway.ConsecutiveFailures, p.Gateway.CertNotAfter = 3, ahead(2*day)
		}, want: []string{"C01/critical"}},
	})
}

func TestC16StaleView(t *testing.T) {
	runProbeCases(t, []probeCase{
		{name: "C16_stale_view_freshness_banner", mod: func(_ *ProbeFacts, w *World) {
			w.LoadedAt = t0.Add(-61 * time.Second)
		}, want: []string{"C10/alarm"}, contains: "Live updates disconnected: data as of 11:58 AM."},
		{name: "C16_two_intervals_boundary_not_stale", mod: func(_ *ProbeFacts, w *World) { w.LoadedAt = t0.Add(-60 * time.Second) }},
		{name: "C16_stream_disconnected", mod: func(p *ProbeFacts, _ *World) { p.View.StreamDisconnected = true },
			want: []string{"C10/alarm"}},
		{name: "C16_longer_refresh_interval_moves_the_threshold", mod: func(p *ProbeFacts, w *World) {
			p.View.RefreshInterval, w.LoadedAt = 2*time.Minute, t0.Add(-3*time.Minute)
		}},
		{name: "C16_zero_refresh_interval_means_default", mod: func(p *ProbeFacts, w *World) {
			p.View.RefreshInterval, w.LoadedAt = 0, t0.Add(-61*time.Second)
		}, want: []string{"C10/alarm"}},
		{name: "C16_unknown_snapshot_time_is_stale", mod: func(_ *ProbeFacts, w *World) { w.LoadedAt = time.Time{} },
			want: []string{"C10/alarm"}, contains: "the age of this data is unknown"},
	})
}

func TestC17DatabaseDegraded(t *testing.T) {
	runProbeCases(t, []probeCase{
		{name: "C17_db_degraded_banner_integrity_failed", mod: func(p *ProbeFacts, _ *World) { p.Database.IntegrityOK = false },
			want: []string{"C11/critical"}, contains: "Console database needs attention: integrity check failed."},
		{name: "C17_db_degraded_banner_disk_low", mod: func(p *ProbeFacts, _ *World) { p.Database.FreeBytes = 7 << 30 },
			want: []string{"C11/alarm"}, contains: "disk below 10% free (7%)"},
		{name: "C17_db_wal_over_1_gib", mod: func(p *ProbeFacts, _ *World) { p.Database.WALBytes = 2 << 30 },
			want: []string{"C11/alarm"}, contains: "WAL over 1 GiB"},
		{name: "C17_db_self_backup_older_than_26h", mod: func(p *ProbeFacts, _ *World) { p.Database.SelfBackup.LastAt = ago(27 * hour) },
			want: []string{"C11/alarm"}, contains: "self-backup older than 26 h"},
		{name: "C17_db_self_backup_verify_failed", mod: func(p *ProbeFacts, _ *World) { p.Database.SelfBackup.VerifyFailed = true },
			want: []string{"C11/critical"}},
		{name: "C17_db_self_backup_never_written", mod: func(p *ProbeFacts, _ *World) { p.Database.SelfBackup.LastAt = 0 },
			want: []string{"C11/alarm"}, contains: "no self-backup has been written"},
		{name: "C17_db_integrity_never_checked", mod: func(p *ProbeFacts, _ *World) { p.Database.IntegrityCheckedAt = 0 },
			want: []string{"C11/alarm"}, contains: "integrity check has never run"},
		{name: "C17_db_never_measured", mod: func(p *ProbeFacts, _ *World) { p.Database.MeasuredAt, p.Database.TotalBytes = 0, 0 },
			want: []string{"C11/alarm"}, contains: "free disk and WAL size not measured"},
		{name: "C17_db_causes_join_and_worst_severity_wins", mod: func(p *ProbeFacts, _ *World) {
			p.Database.IntegrityOK, p.Database.WALBytes = false, 3<<30
		}, want: []string{"C11/critical"}, contains: "integrity check failed; WAL over 1 GiB."},
		{name: "C17_db_self_backup_not_in_this_build", mod: func(p *ProbeFacts, _ *World) { p.Database.SelfBackup = nil }},
	})
}

func TestC18MasterKey(t *testing.T) {
	runProbeCases(t, []probeCase{
		{name: "C18_master_key_unavailable_banner", mod: func(p *ProbeFacts, _ *World) { p.MasterKey.Loaded = false },
			want: []string{"C12/critical"}, contains: "Master key unavailable: enrollment, escrow reveal and server-side retention are disabled."},
		{name: "C18_master_key_zero_value_is_unavailable", mod: func(p *ProbeFacts, _ *World) { *p.MasterKey = MasterKeyFacts{} },
			want: []string{"C12/critical"}},
		{name: "C18_master_key_generation_mismatch", mod: func(p *ProbeFacts, _ *World) {
			p.MasterKey.EscrowKeyIDs = []string{"mk-1", "mk-0"}
		}, want: []string{"C12/critical"}, contains: "is not the one some escrowed repository keys were wrapped with"},
	})
	t.Run("C18_master_key_unavailable_enrollment_refused", func(t *testing.T) {
		if (MasterKeyFacts{}).EnrollmentAllowed() || (MasterKeyFacts{KeyID: "mk-1"}).EnrollmentAllowed() {
			t.Error("enrollment allowed without a loaded master key")
		}
		if !(MasterKeyFacts{Loaded: true, KeyID: "mk-1"}).EnrollmentAllowed() {
			t.Error("enrollment refused with a loaded master key")
		}
	})
}

func TestC25DigestNotSent(t *testing.T) {
	// Every case runs at t0 (12:00 UTC) so the other probes stay current; the digest time moves instead.
	at := func(h, m int) time.Time { return time.Date(2026, 10, 22, h, m, 0, 0, time.UTC) }
	digest := func(local string, sent Millis) func(p *ProbeFacts, _ *World) {
		return func(p *ProbeFacts, _ *World) { p.Digest.LocalTime, p.Digest.LastSentAt = local, sent }
	}
	runProbeCases(t, []probeCase{
		{name: "C25_digest_not_sent_banner", mod: digest("11:15", ms(at(11, 15).Add(-day))),
			want: []string{"C14/critical"}, contains: "Daily digest was not delivered. It was due by 11:45 AM."},
		// 11:40 is due, but its grace runs to 12:10: yesterday's digest is the one judged.
		{name: "C25_digest_inside_grace_yesterday_sent_no_banner", mod: digest("11:40", ms(at(11, 41).Add(-day)))},
		{name: "C25_digest_inside_grace_yesterday_missing", mod: digest("11:40", ms(at(11, 41).Add(-2*day))),
			want: []string{"C14/critical"}, contains: "due by Wed 21 Oct 12:10 PM."},
		{name: "C25_digest_sent_today_no_banner", mod: digest("11:15", ms(at(11, 16)))},
		{name: "C25_digest_manual_send_earlier_same_day_counts", mod: digest("11:15", ms(at(6, 0)))},
		{name: "C25_digest_never_sent", mod: digest("11:15", 0), want: []string{"C14/critical"}},
		{name: "C25_digest_send_time_in_the_future_is_not_checked", mod: digest("11:15", ahead(10*time.Minute)),
			want: []string{"C14b/alarm"}, contains: "the newest digest send time is garbled or in the future"},
		{name: "C25_digest_send_time_slightly_ahead_is_tolerated", mod: digest("11:15", ahead(4*time.Minute))},
		{name: "C25_digest_disabled", mod: func(p *ProbeFacts, _ *World) { p.Digest.Disabled, p.Digest.LastSentAt = true, 0 }},
		{name: "C25_digest_time_not_set_is_not_checked", mod: func(p *ProbeFacts, _ *World) { p.Digest.LocalTime = "" },
			want: []string{"C14b/alarm"}, contains: "Daily digest not checked"},
		{name: "C25_digest_time_in_display_zone", mod: func(p *ProbeFacts, w *World) {
			w.DisplayZone = time.FixedZone("CDT", -5*3600) // 06:00 CDT is 11:00 UTC; t0 is 12:00 UTC
			p.Digest.LocalTime = "06:00"
			p.Digest.LastSentAt = ms(time.Date(2026, 10, 21, 11, 0, 0, 0, time.UTC))
		}, want: []string{"C14/critical"}, contains: "due by 6:30 AM."},
		{name: "C25_digest_time_in_display_zone_not_yet_due", mod: func(p *ProbeFacts, w *World) {
			w.DisplayZone = time.FixedZone("CDT", -5*3600) // 07:00 CDT is 12:00 UTC: inside the grace at t0
			p.Digest.LastSentAt = ms(time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC))
		}},
	})
	t.Run("C25_digest_local_time_parsing", func(t *testing.T) {
		for in, ok := range map[string]bool{"07:00": true, "23:59": true, "0:05": true, "24:00": false, "7": false, "07:7": false, "ab:cd": false, "": false} {
			if _, _, got := parseLocalTime(in); got != ok {
				t.Errorf("parseLocalTime(%q) ok = %v, want %v", in, got, ok)
			}
		}
	})
}

func TestC26C27Quota(t *testing.T) {
	const tib = int64(1) << 40
	setPct := func(pct int64) func(p *ProbeFacts, _ *World) {
		return func(p *ProbeFacts, _ *World) {
			tg := &p.Quota.Targets[0]
			tg.SoftQuotaBytes, tg.UsedBytes = 100*tib, pct*tib
			tg.Samples = nil
		}
	}
	runProbeCases(t, []probeCase{
		{name: "C26_quota_thresholds_79", mod: setPct(79)},
		{name: "C26_quota_thresholds_80", mod: setPct(80), want: []string{"C15/alarm"}, contains: "Storage at 80% of quota on primary."},
		{name: "C26_quota_thresholds_90", mod: setPct(90), want: []string{"C15/critical"}},
		{name: "C26_quota_thresholds_100", mod: setPct(100), want: []string{"C15/critical"}, contains: "Storage at 100% of quota"},
		{name: "C26_quota_fraction_below_threshold_shows_floor", mod: func(p *ProbeFacts, _ *World) {
			tg := &p.Quota.Targets[0]
			tg.SoftQuotaBytes, tg.UsedBytes, tg.Samples = 1000, 899, nil
		}, want: []string{"C15/alarm"}, contains: "Storage at 89% of quota"},
		{name: "C26_quota_worst_target_wins", mod: func(p *ProbeFacts, _ *World) {
			p.Quota.Targets = []TargetUsage{
				{ID: "a", Name: "archive", SoftQuotaBytes: 100, UsedBytes: 85, MeasuredAt: ago(hour)},
				{ID: "b", Name: "secondary", SoftQuotaBytes: 100, UsedBytes: 93, MeasuredAt: ago(hour)},
				{ID: "c", Name: "spare", SoftQuotaBytes: 100, UsedBytes: 91, MeasuredAt: ago(hour)},
			}
		}, want: []string{"C15/critical"}, contains: "Storage at 93% of quota on secondary."},
		{name: "C26_quota_unset_raises_nothing", mod: func(p *ProbeFacts, _ *World) {
			p.Quota.Targets[0].SoftQuotaBytes, p.Quota.Targets[0].UsedBytes = 0, 900*tib
		}},
		{name: "C26_quota_usage_never_measured_is_not_checked", mod: func(p *ProbeFacts, _ *World) {
			p.Quota.Targets[0].MeasuredAt = 0
		}, want: []string{"C15b/alarm"}, contains: "Storage usage of primary not checked"},
		{name: "C27_quota_growth_projection", mod: func(p *ProbeFacts, _ *World) {
			// 50 TiB of 100 used, growing 5 TiB a day: full in 10 days.
			tg := &p.Quota.Targets[0]
			tg.SoftQuotaBytes, tg.UsedBytes = 100*tib, 50*tib
			tg.Samples = []UsageSample{{ago(4 * day), 30 * tib}, {ago(2 * day), 40 * tib}, {ago(0), 50 * tib}}
		}, want: []string{"C15/critical"}, contains: "Storage at 50% of quota on primary. Projected full in about 10 days."},
		{name: "C27_quota_growth_slow_no_banner", mod: func(p *ProbeFacts, _ *World) {
			// 1 TiB a day from 50 %: 50 days left.
			tg := &p.Quota.Targets[0]
			tg.SoftQuotaBytes, tg.UsedBytes = 100*tib, 50*tib
			tg.Samples = []UsageSample{{ago(4 * day), 46 * tib}, {ago(0), 50 * tib}}
		}},
		{name: "C27_quota_shrinking_no_projection", mod: func(p *ProbeFacts, _ *World) {
			tg := &p.Quota.Targets[0]
			tg.Samples = []UsageSample{{ago(4 * day), 9 * tib}, {ago(0), 5 * tib}}
		}},
		{name: "C27_quota_one_sample_no_projection", mod: func(p *ProbeFacts, _ *World) {
			tg := &p.Quota.Targets[0]
			tg.Samples = []UsageSample{{ago(0), 5 * tib}}
		}},
		{name: "C27_quota_projection_under_a_day", mod: func(p *ProbeFacts, _ *World) {
			tg := &p.Quota.Targets[0]
			tg.SoftQuotaBytes, tg.UsedBytes = 100*tib, 70*tib
			tg.Samples = []UsageSample{{ago(day), 20 * tib}, {ago(0), 70 * tib}}
		}, want: []string{"C15/critical"}, contains: "Projected full in less than a day."},
	})
}

func TestC28ReleaseManifest(t *testing.T) {
	runProbeCases(t, []probeCase{
		{name: "C28_release_manifest_invalid_banner", mod: func(p *ProbeFacts, _ *World) { p.Release.Rejected = true },
			want: []string{"C16/alarm"}, contains: "Release manifest rejected."},
		{name: "C28_release_check_never_ran_is_not_checked", mod: func(p *ProbeFacts, _ *World) { p.Release.CheckedAt = 0 },
			want: []string{"C16b/alarm"}},
	})
}

// TestStaleAndFutureCheckTimes: a measurement older than its named maximum age,
// and any check time more than FutureSkewAllowed ahead of now, is "not checked"
// (or a C11 cause), never healthy.
func TestStaleAndFutureCheckTimes(t *testing.T) {
	future, nearFuture := ahead(10*time.Minute), ahead(4*time.Minute)
	runProbeCases(t, []probeCase{
		// C11
		{name: "C17_db_integrity_older_than_max_age", mod: func(p *ProbeFacts, _ *World) { p.Database.IntegrityCheckedAt = ago(9 * day) },
			want: []string{"C11/alarm"}, contains: "integrity check not run for 9 days"},
		{name: "C17_db_integrity_within_max_age", mod: func(p *ProbeFacts, _ *World) { p.Database.IntegrityCheckedAt = ago(7 * day) }},
		{name: "C17_db_integrity_time_in_the_future", mod: func(p *ProbeFacts, _ *World) { p.Database.IntegrityCheckedAt = future },
			want: []string{"C11/alarm"}, contains: "integrity check time is garbled or in the future"},
		{name: "C17_db_measurement_stale", mod: func(p *ProbeFacts, _ *World) { p.Database.MeasuredAt = ago(20 * time.Minute) },
			want: []string{"C11/alarm"}, contains: "free disk and WAL size not measured for 20 min"},
		{name: "C17_db_measurement_stale_hides_the_old_disk_figure", mod: func(p *ProbeFacts, _ *World) {
			p.Database.MeasuredAt, p.Database.FreeBytes = ago(time.Hour), 1<<30 // 1 % free an hour ago: not current evidence
		}, want: []string{"C11/alarm"}, contains: "not measured for 60 min."},
		{name: "C17_db_measurement_in_the_future", mod: func(p *ProbeFacts, _ *World) { p.Database.MeasuredAt = future },
			want: []string{"C11/alarm"}, contains: "disk measurement time is garbled or in the future"},
		{name: "C17_db_self_backup_in_the_future", mod: func(p *ProbeFacts, _ *World) { p.Database.SelfBackup.LastAt = future },
			want: []string{"C11/alarm"}, contains: "self-backup time is garbled or in the future"},
		// C15
		{name: "C26_quota_measurement_stale", mod: func(p *ProbeFacts, _ *World) { p.Quota.Targets[0].MeasuredAt = ago(4 * hour) },
			want: []string{"C15b/alarm"}, contains: "Storage usage of primary not checked: usage has not been measured for 4 h"},
		{name: "C26_quota_measurement_within_max_age", mod: func(p *ProbeFacts, _ *World) { p.Quota.Targets[0].MeasuredAt = ago(2 * hour) }},
		{name: "C26_quota_measurement_in_the_future", mod: func(p *ProbeFacts, _ *World) { p.Quota.Targets[0].MeasuredAt = future },
			want: []string{"C15b/alarm"}, contains: "the newest usage measurement time is garbled or in the future"},
		{name: "C27_quota_future_sample_is_ignored", mod: func(p *ProbeFacts, _ *World) {
			tg := &p.Quota.Targets[0] // flat at 50 %; a future sample at 99 % would project full within days
			tg.Samples = append(tg.Samples, UsageSample{At: ahead(2 * day), Bytes: tg.SoftQuotaBytes * 99 / 100})
		}},
		// C16
		{name: "C28_release_check_stale", mod: func(p *ProbeFacts, _ *World) { p.Release.CheckedAt = ago(50 * time.Minute) },
			want: []string{"C16b/alarm"}, contains: "release-check has not run for 50 min"},
		{name: "C28_release_check_in_the_future", mod: func(p *ProbeFacts, _ *World) { p.Release.CheckedAt = future },
			want: []string{"C16b/alarm"}, contains: "the newest release-check time is garbled or in the future"},
		{name: "C28_rejected_manifest_shows_even_when_stale", mod: func(p *ProbeFacts, _ *World) {
			p.Release.CheckedAt, p.Release.Rejected = ago(3*hour), true
		}, want: []string{"C16/alarm"}},
		// probes and jobs
		{name: "C06_gateway_probe_time_in_the_future", mod: func(p *ProbeFacts, _ *World) { p.Gateway.LastRunAt = future },
			want: []string{"C01b/alarm"}, contains: "its newest probe time is garbled or in the future"},
		{name: "C06_gateway_probe_time_slightly_ahead_is_tolerated", mod: func(p *ProbeFacts, _ *World) { p.Gateway.LastRunAt = nearFuture }},
		{name: "C08_storage_probe_time_in_the_future", mod: func(p *ProbeFacts, _ *World) { p.Storage.LastRunAt = future },
			want: []string{"C02b/alarm"}},
		{name: "C09_smtp_future_delivery_is_not_evidence", mod: func(p *ProbeFacts, _ *World) {
			p.SMTP.Probe.LastRunAt, p.SMTP.LastSendOKAt = future, future
		}, want: []string{"C03b/alarm"}},
		{name: "C09_smtp_pending_with_only_a_future_delivery_is_failing", mod: func(p *ProbeFacts, _ *World) {
			p.SMTP.LastSendOKAt, p.SMTP.PendingAlerts = future, 2
		}, want: []string{"C03/critical"}, contains: "No alert e-mail has been delivered yet."},
		{name: "C13_evaluator_finish_in_the_future", mod: func(p *ProbeFacts, _ *World) { p.Evaluator.LastFinishedAt = future },
			want: []string{"C06b/alarm"}, contains: "the evaluator's newest finish time is garbled or in the future"},
		{name: "C13_evaluator_never_finished_start_in_the_future_is_stalled", mod: func(p *ProbeFacts, _ *World) {
			p.StartedAt, p.Evaluator.LastFinishedAt = future, 0
		}, want: []string{"C06/critical"}},
		{name: "C14_maintenance_scheduler_finish_in_the_future", mod: func(p *ProbeFacts, _ *World) { p.Maintain.Scheduler.LastFinishedAt = future },
			want: []string{"C07b/alarm"}, contains: "the maintenance scheduler's newest finish time is garbled"},
		{name: "C14_maintenance_rpc_probe_time_in_the_future", mod: func(p *ProbeFacts, _ *World) { p.Maintain.RPC.LastRunAt = future },
			want: []string{"C07b/alarm"}},
		{name: "C16_snapshot_time_in_the_future_is_unknown_age", mod: func(_ *ProbeFacts, w *World) { w.LoadedAt = t0.Add(10 * time.Minute) },
			want: []string{"C10/alarm"}, contains: "the age of this data is unknown"},
	})
}

func TestProbeBannersNilAndZeroChecks(t *testing.T) {
	t.Run("C21_unwired_self_checks_raise_c00_and_are_never_clear", func(t *testing.T) {
		w := healthyFleet(1, 2)
		w.Probes = ProbeFacts{StartedAt: ago(day)}
		v := Evaluate(w, t0)
		b, ok := findBanner(v, "C00")
		if !reflect.DeepEqual(bannerIDs(v), []string{"C00"}) || !ok || b.Severity != SeverityAlarm ||
			b.Text != "Console self-checks are not wired: gateway, storage, e-mail and job health are not being monitored." {
			t.Fatalf("banners %+v", v.Banners)
		}
		if v.AllClear() {
			t.Fatal("AllClear with the self-checks not wired")
		}
	})
	t.Run("C21_unwired_still_evaluates_the_checks_it_has", func(t *testing.T) {
		w := healthyFleet(1, 2)
		w.Probes = ProbeFacts{Gateway: &ProbeRun{ConsecutiveFailures: 3}}
		if got := bannerIDs(Evaluate(w, t0)); !reflect.DeepEqual(got, []string{"C01", "C00"}) {
			t.Fatalf("banners %v", got)
		}
	})
	t.Run("C21_wired_every_nil_check_is_not_checked", func(t *testing.T) {
		w := healthyFleet(1, 2)
		w.Probes = ProbeFacts{Wired: true, StartedAt: ago(day)}
		v := Evaluate(w, t0)
		want := []string{"C01b", "C02b", "C03b", "C06b", "C07b", "C11", "C12b", "C14b", "C15b", "C16b"}
		if !reflect.DeepEqual(bannerIDs(v), want) {
			t.Fatalf("banners = %v, want %v", bannerIDs(v), want)
		}
		for _, b := range v.Banners {
			if b.Severity != SeverityAlarm || !strings.Contains(b.Text, "not") {
				t.Errorf("%s: %s %q", b.ID, b.Severity, b.Text)
			}
		}
		if v.AllClear() {
			t.Fatal("AllClear with every check missing")
		}
	})
	t.Run("C21_wired_one_forgotten_pointer_is_loud", func(t *testing.T) {
		w := healthyFleet(1, 2)
		w.Probes.Storage = nil
		v := Evaluate(w, t0)
		if !reflect.DeepEqual(bannerIDs(v), []string{"C02b"}) || v.AllClear() {
			t.Fatalf("banners %v allClear %v", bannerIDs(v), v.AllClear())
		}
	})
	t.Run("C21_no_view_is_quiet_for_callers_without_a_page", func(t *testing.T) {
		w := healthyFleet(1, 2)
		w.Probes.View = nil
		if v := Evaluate(w, t0); len(v.Banners) != 0 || !v.AllClear() {
			t.Fatalf("banners %v", bannerIDs(v))
		}
	})
	t.Run("C21_every_zero_check_is_loud", func(t *testing.T) {
		w := healthyFleet(1, 2)
		w.Probes = ProbeFacts{
			Wired:   true,
			Gateway: &ProbeRun{}, Storage: &ProbeRun{}, SMTP: &SMTPFacts{}, Evaluator: &JobFacts{},
			Maintain: &MaintainFacts{Scheduler: &JobFacts{}}, View: &ViewFacts{}, Database: &DatabaseFacts{SelfBackup: &SelfBackupFacts{}},
			MasterKey: &MasterKeyFacts{}, Digest: &DigestFacts{}, Quota: &QuotaFacts{Targets: []TargetUsage{{SoftQuotaBytes: 1}}},
			Release: &ReleaseFacts{},
		}
		w.LoadedAt = time.Time{}
		v := Evaluate(w, t0)
		want := []string{"C06", "C07", "C12", "C01b", "C02b", "C03b", "C10", "C11", "C14b", "C15b", "C16b"}
		if !reflect.DeepEqual(bannerIDs(v), want) {
			t.Fatalf("banners = %v, want %v", bannerIDs(v), want)
		}
		if v.AllClear() {
			t.Fatal("AllClear with every check at its zero value")
		}
	})
}

// ---- clock (architecture 4.2 attribute clock skew, 4.6 C13; 4.8 C24) -----------------

func TestC24ClockSuspect(t *testing.T) {
	// fleet is 10 healthy devices; skew sets the offset of the first n of them.
	fleet := func(n int, off time.Duration) World {
		w := healthyFleet(2, 5)
		for i := 0; i < n; i++ {
			w.Devices[i].ClockOffset = off
		}
		return w
	}
	cases := []struct {
		name string
		w    World
		want bool
		text string
	}{
		{"C24_clock_suspect_banner", func() World {
			w := healthyFleet(4, 5) // 20 devices, 6 (30 %) at +5 min
			for i := 0; i < 6; i++ {
				w.Devices[i].ClockOffset = 5 * time.Minute
			}
			return w
		}(), true, "Server clock may be wrong: device offsets agree. 6 of 20 reporting devices run more than 2 min ahead of the server clock."},
		{"C24_clock_suspect_behind", fleet(5, -10*time.Minute), true, "5 of 10 reporting devices run more than 2 min behind the server clock."},
		{"C24_clock_suspect_needs_five_devices", fleet(4, 5*time.Minute), false, ""},
		{"C24_clock_suspect_needs_twenty_percent", func() World {
			w := healthyFleet(6, 5) // 30 devices, 5 skewed: 16.7 %
			for i := 0; i < 5; i++ {
				w.Devices[i].ClockOffset = 5 * time.Minute
			}
			return w
		}(), false, ""},
		{"C24_clock_suspect_exactly_twenty_percent", func() World {
			w := healthyFleet(5, 5) // 25 devices, 5 skewed: 20 %
			for i := 0; i < 5; i++ {
				w.Devices[i].ClockOffset = 3 * time.Minute
			}
			return w
		}(), true, "5 of 25"},
		{"C24_clock_suspect_mixed_signs_do_not_agree", func() World {
			w := fleet(4, 5*time.Minute)
			for i := 4; i < 8; i++ {
				w.Devices[i].ClockOffset = -5 * time.Minute
			}
			return w
		}(), false, ""},
		{"C24_clock_suspect_two_minutes_is_not_beyond", fleet(8, 2*time.Minute), false, ""},
		{"C24_clock_suspect_ignores_unseen_retired_and_unknown", func() World {
			w := fleet(8, 5*time.Minute)
			w.Devices[0].LastSeen = ago(200 * hour) // unseen: offset is not current
			w.Devices[1].RetiredAt = ago(day)
			w.Devices[2].Complete = false
			w.Devices[3].LastSeen = 0 // never heartbeat: falls back to enrolled_at, has no offset
			return w
		}(), false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := Evaluate(c.w, t0)
			b, ok := findBanner(v, "C13")
			if ok != c.want {
				t.Fatalf("C13 raised = %v, want %v (banners %v)", ok, c.want, bannerIDs(v))
			}
			if !ok {
				return
			}
			if b.Severity != SeverityAlarm || !strings.Contains(b.Text, c.text) || v.AllClear() {
				t.Errorf("banner %+v allClear %v", b, v.AllClear())
			}
		})
	}
	t.Run("C24_clock_skew_is_a_device_attribute_never_the_primary", func(t *testing.T) {
		v := Evaluate(fleet(6, 3*time.Hour), t0)
		skewed := 0
		for _, d := range v.Devices {
			if hasAttr(d.Attrs, AttrClockSkew) {
				skewed++
				if d.Primary != StateOK || d.Detail.ClockOffset != 3*time.Hour {
					t.Errorf("%s: primary %s offset %v", d.Hostname, d.Primary, d.Detail.ClockOffset)
				}
			}
		}
		if skewed != 6 {
			t.Errorf("skewed devices = %d, want 6", skewed)
		}
	})
}

// ---- collapse (architecture 4.3; 4.8 C12, C19, C20) ----------------------------------

func deferGateway(f *DeviceFacts) {
	f.ServiceState = "deferred"
	f.LastBackup = &RunFacts{Outcome: "deferred", DeferredReason: "gateway", StartedAt: ago(hour), FinishedAt: ago(hour), CountsKnown: true}
}

func goSilent(f *DeviceFacts, quiet time.Duration) { f.LastSeen = ago(quiet) }

func TestC20CustomerWideSilence(t *testing.T) {
	// customerOf10 is customer 1 with 10 devices and customer 2 with 3 healthy ones.
	customerOf10 := func(mod func(ds []DeviceFacts)) World {
		w := worldOf([]CustomerFacts{customer(1, "Northwind Dental"), customer(2, "Contoso Law")}, nil)
		for i := 1; i <= 10; i++ {
			w.Devices = append(w.Devices, deviceOf(i, 1, fmt.Sprintf("nd-%02d", i)))
		}
		for i := 11; i <= 13; i++ {
			w.Devices = append(w.Devices, deviceOf(i, 2, fmt.Sprintf("cl-%02d", i)))
		}
		mod(w.Devices[:10])
		return w
	}
	type want struct {
		collapse                bool
		affected, total, unseen int
	}
	cases := []struct {
		name string
		mod  func(ds []DeviceFacts)
		want want
	}{
		{"C20_customer_wide_silence_collapses", func(ds []DeviceFacts) {
			for i := 0; i < 8; i++ {
				goSilent(&ds[i], 200*hour)
			}
		}, want{true, 8, 10, 8}},
		{"C20_half_is_enough", func(ds []DeviceFacts) {
			for i := 0; i < 5; i++ {
				goSilent(&ds[i], 200*hour)
			}
		}, want{true, 5, 10, 5}},
		{"C20_under_half_no_collapse", func(ds []DeviceFacts) {
			for i := 0; i < 4; i++ {
				goSilent(&ds[i], 200*hour)
			}
		}, want{false, 4, 10, 4}},
		{"C20_gateway_deferral_counts_as_silence", func(ds []DeviceFacts) {
			for i := 0; i < 3; i++ {
				goSilent(&ds[i], 200*hour)
			}
			for i := 3; i < 6; i++ {
				deferGateway(&ds[i])
			}
		}, want{true, 6, 10, 3}},
		{"C20_other_deferral_reasons_do_not_count", func(ds []DeviceFacts) {
			for i := 0; i < 8; i++ {
				deferGateway(&ds[i])
				ds[i].LastBackup.DeferredReason = "battery"
			}
		}, want{false, 0, 10, 0}},
		{"C20_paused_devices_count_in_the_denominator", func(ds []DeviceFacts) {
			for i := 0; i < 3; i++ {
				goSilent(&ds[i], 200*hour)
			}
			for i := 3; i < 6; i++ {
				ds[i].PausedAt = ago(day) // paused: still an active device of the customer
			}
			for i := 6; i < 8; i++ {
				ds[i].RetiredAt = ago(day) // retired: out of the fleet
			}
		}, want{false, 3, 8, 3}}, // 3 silent of the 8 non-retired: 37.5 %
		{"C20_paused_half_and_silent_half_collapse", func(ds []DeviceFacts) {
			for i := 0; i < 5; i++ {
				goSilent(&ds[i], 200*hour)
			}
			for i := 5; i < 10; i++ {
				ds[i].PausedAt = ago(day)
			}
		}, want{true, 5, 10, 5}},
		{"C20_fewer_than_five_active_never_collapse", func(ds []DeviceFacts) {
			for i := 0; i < 4; i++ {
				goSilent(&ds[i], 200*hour)
			}
			for i := 4; i < 10; i++ {
				ds[i].RetiredAt = ago(day)
			}
		}, want{false, 4, 4, 4}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := Evaluate(customerOf10(c.mod), t0)
			var nd *CustomerState
			for i := range v.Customers {
				if v.Customers[i].ID == custID(1) {
					nd = &v.Customers[i]
				}
			}
			col, ok := CustomerCollapse(*nd)
			if ok != c.want.collapse || col.Affected != c.want.affected || col.Total != c.want.total || col.Unseen != c.want.unseen {
				t.Fatalf("collapse %v %+v, want %+v", ok, col, c.want)
			}
			var rows []Collapse
			for _, x := range v.Collapses {
				if x.Scope == CollapseCustomer {
					rows = append(rows, x)
				}
			}
			if !c.want.collapse {
				if len(rows) != 0 {
					t.Fatalf("collapses %+v, want none", rows)
				}
				return
			}
			// one customer row and one notification; the device rows remain listed.
			if len(rows) != 1 || rows[0].CustomerID != custID(1) || rows[0].CustomerName != "Northwind Dental" ||
				rows[0].Covered || len(rows[0].DeviceIDs) != c.want.affected {
				t.Fatalf("collapses %+v", v.Collapses)
			}
			listed := 0
			for _, d := range v.Devices {
				if d.CustomerID == custID(1) {
					listed++
				}
			}
			if listed != 10 {
				t.Errorf("device rows of the customer = %d, want all 10 kept", listed)
			}
			// customer-wide silence is an entity row, never a console banner (ADR 0013)
			if len(v.Banners) != 0 {
				t.Errorf("customer-wide silence raised console banners %v", bannerIDs(v))
			}
		})
	}
}

func TestC19GatewayDeferralCollapse(t *testing.T) {
	t.Run("C19_many_deferred_gateway_collapse_to_one_banner", func(t *testing.T) {
		w := healthyFleet(16, 10) // 160 devices, 150 deferred(gateway)
		for i := 0; i < 150; i++ {
			deferGateway(&w.Devices[i])
		}
		v := Evaluate(w, t0)
		var c01 []Banner
		for _, b := range v.Banners {
			if b.ID == "C01" {
				c01 = append(c01, b)
			}
		}
		if len(c01) != 1 || c01[0].Severity != SeverityCritical || c01[0].Count != 150 ||
			c01[0].Text != "Gateway unreachable: no agent can back up. 150 of 160 reporting devices are deferring their backups because they cannot reach it." {
			t.Fatalf("C01 banners %+v", c01)
		}
		if len(v.Collapses) == 0 || v.Collapses[0].Scope != CollapseFleet || len(v.Collapses[0].DeviceIDs) != 150 {
			t.Fatalf("fleet collapse %+v", v.Collapses)
		}
		// rows not individually notified: every customer collapse is covered by the fleet one.
		for _, c := range v.Collapses[1:] {
			if c.Scope != CollapseCustomer || !c.Covered {
				t.Errorf("customer collapse not covered: %+v", c)
			}
		}
		if v.AllClear() {
			t.Error("AllClear with C01")
		}
	})
	rows := []struct {
		name    string
		n, of   int
		want    bool
		probe   bool
		wantTxt string
	}{
		{"C19_half_of_reporting_devices_raises_c01", 5, 10, true, false, "5 of 10 reporting devices"},
		{"C19_under_half_no_banner", 4, 10, false, false, ""},
		{"C19_four_devices_never_raise", 4, 4, false, false, ""},
		{"C19_failing_probe_and_mass_deferral_are_one_c01", 8, 10, true, true, "Last OK"},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			w := healthyFleet(1, r.of)
			for i := 0; i < r.n; i++ {
				deferGateway(&w.Devices[i])
			}
			if r.probe {
				w.Probes = healthyProbes()
				w.Probes.Gateway.ConsecutiveFailures, w.Probes.Gateway.LastOKAt = 3, ago(5*time.Minute)
			}
			v := Evaluate(w, t0)
			n := 0
			var b Banner
			for _, x := range v.Banners {
				if x.ID == "C01" {
					n++
					b = x
				}
			}
			if (n == 1) != r.want || n > 1 {
				t.Fatalf("C01 count %d, want raised=%v (banners %v)", n, r.want, bannerIDs(v))
			}
			if r.want && !strings.Contains(b.Text, r.wantTxt) {
				t.Errorf("text %q, want %q", b.Text, r.wantTxt)
			}
		})
	}
	t.Run("C19_gateway_outage_past_stale_after_still_collapses", func(t *testing.T) {
		// The outage outlived stale_after: every device is STALE, which suppresses DEFERRED,
		// but the newest run of each was still deferred for the gateway.
		w := healthyFleet(1, 10)
		for i := range w.Devices {
			deferGateway(&w.Devices[i])
			w.Devices[i].LastSuccess = ago(80 * hour)
		}
		v := Evaluate(w, t0)
		for _, d := range v.Devices {
			if d.Primary != StateStale || hasState(d.Flags, StateDeferred) {
				t.Fatalf("%s: %s %v, want STALE without DEFERRED", d.Hostname, d.Primary, d.Flags)
			}
		}
		b, ok := findBanner(v, "C01")
		if !ok || b.Count != 10 || !strings.Contains(b.Text, "10 of 10 reporting devices") {
			t.Fatalf("C01 = %+v ok=%v", b, ok)
		}
		if len(v.Collapses) != 2 || v.Collapses[0].Scope != CollapseFleet || v.Collapses[0].Affected != 10 ||
			v.Collapses[1].Scope != CollapseCustomer || !v.Collapses[1].Covered {
			t.Fatalf("collapses %+v", v.Collapses)
		}
	})
	t.Run("C19_stale_device_whose_newest_run_succeeded_is_not_gateway_evidence", func(t *testing.T) {
		w := healthyFleet(1, 10)
		for i := range w.Devices {
			w.Devices[i].LastSuccess, w.Devices[i].LastBackup = ago(80*hour), successRun(80*hour)
		}
		if _, ok := findBanner(Evaluate(w, t0), "C01"); ok {
			t.Fatal("C01 for a fleet that is only stale")
		}
	})
	t.Run("C19_unseen_and_unknown_devices_are_not_reporting", func(t *testing.T) {
		w := healthyFleet(1, 10)
		for i := 0; i < 4; i++ {
			deferGateway(&w.Devices[i])
		}
		for i := 4; i < 8; i++ {
			goSilent(&w.Devices[i], 200*hour) // 4 deferred of 6 reporting
		}
		w.Devices[8].Complete = false // 4 of 5
		v := Evaluate(w, t0)
		if b, ok := findBanner(v, "C01"); !ok || !strings.Contains(b.Text, "4 of 5 reporting devices") {
			t.Fatalf("C01 = %+v ok=%v", b, ok)
		}
	})
}

func TestC12OfflineFleetRaisesNoConsoleBanner(t *testing.T) {
	// ADR 0013: devices being offline never raise a console banner, however many and for however long.
	for _, quiet := range []time.Duration{20 * time.Minute, 4 * time.Hour, 60 * time.Hour, 200 * time.Hour} {
		t.Run("C12_offline_fleet_raises_no_console_banner_"+quiet.String(), func(t *testing.T) {
			w := healthyFleet(2, 6)
			w.Probes = healthyProbes()
			for i := range w.Devices {
				goSilent(&w.Devices[i], quiet)
				w.Devices[i].LastSuccess = ago(quiet + time.Hour)
				w.Devices[i].LastBackup = successRun(quiet + time.Hour)
				w.Devices[i].LastAttemptStart = ago(quiet + time.Hour)
			}
			v := Evaluate(w, t0)
			if len(v.Banners) != 0 {
				t.Fatalf("banners %v for a fleet silent %v", bannerIDs(v), quiet)
			}
			for _, d := range v.Devices {
				switch d.Primary {
				case StateOK, StateMissed, StateStale, StateUnseen:
				default:
					t.Errorf("%s: %s", d.Hostname, d.Primary)
				}
			}
			// past unseen_after each customer collapses to one silence row: an entity row, not a banner.
			if wantRows := quiet > 168*time.Hour; (len(v.Collapses) == 2) != wantRows {
				t.Errorf("collapses %+v at %v", v.Collapses, quiet)
			}
		})
	}
}
