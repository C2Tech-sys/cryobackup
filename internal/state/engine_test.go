package state

import (
	"encoding/json"
	"fmt"
	"go/parser"
	"go/token"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestPrecedenceVocabulary(t *testing.T) {
	want := []State{
		StateRetired, StatePaused, StateUnknown, StateIdentityConflict, StatePendingFirst, StateBlank,
		StateStuck, StateFailed, StatePartial, StateUnseen, StateStale, StateEscrowUnverified,
		StateMaintFailed, StateMissed, StateDeferred, StateOutdated, StateOK,
	}
	got := Precedence()
	if !reflect.DeepEqual(got, want) || len(got) != 17 {
		t.Fatalf("Precedence() = %v", got)
	}
	got[0] = "tampered"
	if Precedence()[0] != StateRetired {
		t.Errorf("Precedence returned its internal slice")
	}
	for i, s := range want {
		if s.rank() != i {
			t.Errorf("%s rank %d, want %d", s, s.rank(), i)
		}
	}
	if State("nonsense").rank() != 17 {
		t.Errorf("unknown state rank")
	}
}

func TestSeverityOrderAndString(t *testing.T) {
	if !(SeverityNone < SeverityAlarm && SeverityAlarm < SeverityCritical) {
		t.Fatal("severities are not ordered")
	}
	for s, want := range map[Severity]string{SeverityNone: "none", SeverityAlarm: "alarm", SeverityCritical: "critical", Severity(9): "unknown"} {
		if s.String() != want {
			t.Errorf("Severity(%d).String() = %q, want %q", int(s), s.String(), want)
		}
	}
	critical := []State{StateUnknown, StateIdentityConflict, StateBlank, StateStuck, StateFailed, StateUnseen, StateStale}
	alarm := []State{StatePartial, StateEscrowUnverified, StateMaintFailed}
	none := []State{StateRetired, StatePaused, StatePendingFirst, StateMissed, StateDeferred, StateOutdated, StateOK}
	for _, s := range critical {
		if severityOf(s, false) != SeverityCritical {
			t.Errorf("%s is not critical", s)
		}
	}
	for _, s := range alarm {
		if severityOf(s, false) != SeverityAlarm {
			t.Errorf("%s is not alarm", s)
		}
	}
	for _, s := range none {
		if severityOf(s, false) != SeverityNone {
			t.Errorf("%s alarms", s)
		}
	}
	if severityOf(StateOutdated, true) != SeverityAlarm {
		t.Errorf("OUTDATED with outdated_alarm must alarm")
	}
}

func TestMillisParsing(t *testing.T) {
	cases := []struct {
		name string
		in   Millis
		ok   bool
	}{
		{"null", 0, false},
		{"negative", -5, false},
		{"one", 1, false},
		{"seconds epoch", 1_700_000_000, false},
		{"just below 1e12", 999_999_999_999, false},
		{"lower bound", 1_000_000_000_000, true},
		{"normal", Millis(t0.UnixMilli()), true},
		{"upper bound 2100-01-01", 4_102_444_800_000, true},
		{"just above 2100", 4_102_444_800_001, false},
		{"huge", 9_223_372_036_854_775_807, false},
		{"most negative", -9_223_372_036_854_775_808, false},
	}
	for _, c := range cases {
		got, ok := c.in.Time()
		if ok != c.ok {
			t.Errorf("%s: ok = %v, want %v", c.name, ok, c.ok)
		}
		if ok && (got.Location() != time.UTC || got.UnixMilli() != int64(c.in)) {
			t.Errorf("%s: parsed %v", c.name, got)
		}
		if !ok && !got.IsZero() {
			t.Errorf("%s: garbled value returned a time %v", c.name, got)
		}
	}
	if got, _ := ago(2 * hour).Time(); !got.Equal(t0.Add(-2 * hour)) {
		t.Errorf("round trip = %v", got)
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"2.5.9", "2.5.10", -1},
		{"2.5.10", "2.5.9", 1},
		{"2.5", "2.5.0", 0},
		{"2.5.0.1", "2.5", 1},
		{"1.0.0", "1.0.0", 0},
		{"v1.2.3", "1.2.3", 0},
		{"1.2.3-rc1", "1.2.3", 0},
		{"10.0.0", "9.9.9", 1},
		{"x.y", "0.0.1", -1},
		{"", "0.0.1", -1},
		{"dev", "0.0.0", -1},
		{"0.0.0", "dev", 1},
		{"x.y", "garbage", 0},
		{"", "", 0},
		{"1..2", "1.0.2", -1},
		{"99999999999999999999.0", "1.0", -1}, // overflow is garbled, not a panic
	}
	for _, c := range cases {
		if got := CompareVersions(c.a, c.b); got != c.want {
			t.Errorf("CompareVersions(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
		if got := CompareVersions(c.b, c.a); got != -c.want {
			t.Errorf("CompareVersions(%q, %q) = %d, want %d (antisymmetry)", c.b, c.a, got, -c.want)
		}
	}
}

func TestDefaultThresholdsAreTheSection36Defaults(t *testing.T) {
	d := DefaultThresholds()
	want := Thresholds{
		StaleAfter: 72 * hour, UnseenAfter: 168 * hour, FirstBackupGrace: 72 * hour, SeedExtension: 0,
		MaxFailedFiles: 20, MaxFailedPct: 0.5, ConsecutiveFailuresToAlarm: 1,
		MaxRun: 12 * hour, InitialMaxRun: 168 * hour, StallAfter: 180 * time.Minute, MissedFactor: 2.0,
		EscrowVerifyGrace: 24 * hour, EscrowRecheck: 30 * day, MaintenanceDeferralAlarmAfter: 10,
		Interval: 24 * hour,
	}
	if d != want {
		t.Errorf("DefaultThresholds() = %+v", d)
	}
}

func TestZeroThresholdsMeanDefaults(t *testing.T) {
	f := baseFacts()
	f.Thresholds = Thresholds{}
	if got := Classify(f, t0); got.Primary != StateOK {
		t.Errorf("zero thresholds on a healthy device: %s", got.Primary)
	}
	f.LastSuccess = ago(100 * hour)
	f.LastBackup = successRun(100 * hour)
	if got := Classify(f, t0); got.Primary != StateStale {
		t.Errorf("zero thresholds must behave as the defaults: %s", got.Primary)
	}
}

func TestErrorClassVocabulary(t *testing.T) {
	if got, want := HardFailureClasses(), []string{"disk", "internal", "parse", "permission", "repo_missing", "wrong_password"}; !reflect.DeepEqual(got, want) {
		t.Errorf("HardFailureClasses = %v", got)
	}
	if got, want := TolerableErrorClasses(), []string{"locked", "permission", "unsupported_file"}; !reflect.DeepEqual(got, want) {
		t.Errorf("TolerableErrorClasses = %v", got)
	}
	for _, c := range HardFailureClasses() {
		if !isHardFailureClass(c) {
			t.Errorf("%s not hard", c)
		}
	}
	for _, c := range []string{"network", "tls", "auth", "lock", "vss", "memory", "storage", "guard_refused", "unknown", "locked", "missing_source", ""} {
		if isHardFailureClass(c) {
			t.Errorf("%q must not be a hard failure class", c)
		}
	}
}

// TestPackageSourcesAreClockFreeAndStdlibOnly is the static half of B48: the
// engine's non-test sources never read the wall clock, never do I/O and import
// nothing outside the standard library.
func TestPackageSourcesAreClockFreeAndStdlibOnly(t *testing.T) {
	t.Run("B48_no_clock_reads_in_non_test_sources", func(t *testing.T) {
		needles := []string{"time." + "Now(", "time." + "Since(", "time." + "Until("}
		files := nonTestSources(t)
		if len(files) < 6 {
			t.Fatalf("found only %d source files", len(files))
		}
		for _, name := range files {
			src, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			for _, n := range needles {
				if strings.Contains(string(src), n) {
					t.Errorf("%s contains %q", filepath.Base(name), n)
				}
			}
		}
	})
	t.Run("B48_imports_are_standard_library_and_pure", func(t *testing.T) {
		banned := map[string]bool{
			"os": true, "os/exec": true, "log": true, "log/slog": true, "net": true, "net/http": true,
			"database/sql": true, "fmt": true, "io/ioutil": true, "syscall": true, "unsafe": true, "reflect": true,
		}
		fset := token.NewFileSet()
		for _, name := range nonTestSources(t) {
			f, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
			if err != nil {
				t.Fatal(err)
			}
			for _, imp := range f.Imports {
				path, _ := strconv.Unquote(imp.Path.Value)
				if banned[path] {
					t.Errorf("%s imports %q", filepath.Base(name), path)
				}
				if first := strings.SplitN(path, "/", 2)[0]; strings.Contains(first, ".") {
					t.Errorf("%s imports non-standard-library package %q", filepath.Base(name), path)
				}
			}
		}
	})
}

func nonTestSources(t *testing.T) []string {
	t.Helper()
	all, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, n := range all {
		if !strings.HasSuffix(n, "_test.go") {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// mixedWorld is a fleet with every kind of device state, for determinism tests.
func mixedWorld() World {
	cs := []CustomerFacts{
		customer(1, "Northwind Dental"), customer(2, "Contoso Law"), customer(3, "Fabrikam Auto"),
		{ID: custID(4), Name: "Adventure Works", State: "suspended"},
		{ID: custID(5), Name: "Tailspin Toys", State: "active", Expected: true},
		{ID: custID(6), Name: "Old Customer", State: "archived"},
	}
	mods := []devMod{nil, modStale, modFailed, modRetired, modUnknown, modTimedPause, modIndefPause, modMissed, modDeferred, modOutdated, modPending,
		func(f *DeviceFacts) {
			f.LastBackup = partialRun(1000, 99000, map[string]int{"permission": 600, "locked": 400})
		},
		func(f *DeviceFacts) { f.Ack = &AckFacts{AckedAt: ago(hour)}; modFailed(f) },
		func(f *DeviceFacts) { f.LastSeen = ago(200 * hour) },
	}
	var ds []DeviceFacts
	n := 0
	for ci := 1; ci <= 4; ci++ {
		for _, m := range mods {
			n++
			d := deviceOf(n, ci, fmt.Sprintf("host-%d-%02d", ci, n))
			if m != nil {
				m(&d)
			}
			ds = append(ds, d)
		}
	}
	ds = append(ds, deviceOf(900, 6, "ws-archived-01"), deviceOf(901, 77, "ws-orphan-01"))
	w := worldOf(cs, ds)
	w.PreviousDeviceCount = len(ds)
	return w
}

func TestEvaluateDeterministic(t *testing.T) {
	t.Run("B48_fleetview_json_is_byte_identical", func(t *testing.T) {
		w := mixedWorld()
		first, err := json.Marshal(Evaluate(w, t0))
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 20; i++ {
			again, _ := json.Marshal(Evaluate(w, t0))
			if string(again) != string(first) {
				t.Fatalf("run %d produced different bytes", i)
			}
		}
		if len(first) < 1000 {
			t.Fatalf("suspiciously small encoding: %d bytes", len(first))
		}
	})
	t.Run("B48_fleetview_independent_of_input_order", func(t *testing.T) {
		w := mixedWorld()
		want, _ := json.Marshal(Evaluate(w, t0))
		rng := rand.New(rand.NewPCG(7, 11))
		for i := 0; i < 25; i++ {
			p := w
			p.Devices = append([]DeviceFacts(nil), w.Devices...)
			p.Customers = append([]CustomerFacts(nil), w.Customers...)
			rng.Shuffle(len(p.Devices), func(a, b int) { p.Devices[a], p.Devices[b] = p.Devices[b], p.Devices[a] })
			rng.Shuffle(len(p.Customers), func(a, b int) { p.Customers[a], p.Customers[b] = p.Customers[b], p.Customers[a] })
			got, _ := json.Marshal(Evaluate(p, t0))
			if string(got) != string(want) {
				t.Fatalf("shuffle %d changed the encoding", i)
			}
		}
	})
	t.Run("B48_evaluate_does_not_mutate_the_world", func(t *testing.T) {
		w := mixedWorld()
		before, _ := json.Marshal(w)
		Evaluate(w, t0)
		after, _ := json.Marshal(w)
		if string(before) != string(after) {
			t.Errorf("Evaluate modified its input")
		}
	})
	t.Run("B48_mixed_world_is_actually_mixed", func(t *testing.T) {
		v := Evaluate(mixedWorld(), t0)
		seen := map[State]bool{}
		for _, d := range v.Devices {
			seen[d.Primary] = true
		}
		for _, s := range []State{StateOK, StateStale, StateFailed, StateRetired, StateUnknown, StatePaused, StateMissed,
			StateDeferred, StateOutdated, StatePendingFirst, StatePartial, StateUnseen} {
			if !seen[s] {
				t.Errorf("fixture lacks a %s device", s)
			}
		}
		if len(v.Customers) != 5 {
			t.Errorf("customers = %d, want 5 (archived excluded)", len(v.Customers))
		}
	})
	t.Run("B48_devices_sorted_by_customer_precedence_hostname", func(t *testing.T) {
		v := Evaluate(mixedWorld(), t0)
		key := map[string]int{}
		for _, c := range v.Customers {
			key[c.ID] = c.SortKey
		}
		for i := 1; i < len(v.Devices); i++ {
			a, b := v.Devices[i-1], v.Devices[i]
			ka, okA := key[a.CustomerID]
			kb, okB := key[b.CustomerID]
			if !okA {
				ka = CustomerUnknown.sortKey()
			}
			if !okB {
				kb = CustomerUnknown.sortKey()
			}
			if ka > kb || (ka == kb && a.Primary.rank() > b.Primary.rank()) ||
				(ka == kb && a.Primary.rank() == b.Primary.rank() && strings.ToLower(a.Hostname) > strings.ToLower(b.Hostname)) {
				t.Fatalf("devices %d/%d out of order: %s(%s) before %s(%s)", i-1, i, a.Hostname, a.Primary, b.Hostname, b.Primary)
			}
		}
	})
}

// ---- pairwise precedence -------------------------------------------------------------

func stateMutators() map[State]func(f *DeviceFacts) {
	return map[State]func(f *DeviceFacts){
		StateRetired:          func(f *DeviceFacts) { f.RetiredAt = ago(10 * day) },
		StatePaused:           func(f *DeviceFacts) { f.PausedAt, f.PausedUntil = ago(hour), ahead(5*hour) },
		StateUnknown:          func(f *DeviceFacts) { f.Complete = false },
		StateIdentityConflict: func(f *DeviceFacts) { f.IdentityConflictAt = ago(hour) },
		StatePendingFirst:     func(f *DeviceFacts) { newlyEnrolled(f, 1*hour) },
		StateBlank:            graceExpired,
		StateStuck:            func(f *DeviceFacts) { f.OpenRun = &RunFacts{StartedAt: ago(76 * hour)} },
		StateFailed:           func(f *DeviceFacts) { f.LastBackup = failedRun(30*time.Minute, "network"); f.ConsecutiveFailures = 1 },
		StatePartial: func(f *DeviceFacts) {
			f.LastBackup = partialRun(1000, 99000, map[string]int{"permission": 1000})
			f.LastBackup.FinishedAt = ago(30 * time.Minute)
		},
		StateUnseen: func(f *DeviceFacts) { f.LastSeen = ago(200 * hour) },
		StateStale: func(f *DeviceFacts) {
			f.LastSuccess = ago(100 * hour)
			if f.LastBackup != nil && f.LastBackup.Outcome == "success" {
				f.LastBackup = successRun(100 * hour)
			}
		},
		StateEscrowUnverified: func(f *DeviceFacts) { f.Escrow = &EscrowFacts{Custody: "escrowed", CreatedAt: ago(30 * day)} },
		StateMaintFailed:      func(f *DeviceFacts) { f.Maintenance.LastPrune = failedRun(hour, "storage") },
		StateMissed: func(f *DeviceFacts) {
			f.LastSuccess = ago(60 * hour)
			if f.LastBackup != nil && f.LastBackup.Outcome == "success" {
				f.LastBackup = successRun(60 * hour)
			}
			f.LastAttemptStart = 0
		},
		StateDeferred: func(f *DeviceFacts) { f.ServiceState = "deferred" },
		StateOutdated: func(f *DeviceFacts) { f.AgentVersion = "1.4.0" },
	}
}

// pairExcluded explains why two states cannot be asserted as "earlier is Primary,
// later stays a flag". An empty string means the pair must hold.
func pairExcluded(a, b State) string {
	pauseSuppresses := map[State]bool{StateBlank: true, StateStuck: true, StateFailed: true, StatePartial: true,
		StateUnseen: true, StateStale: true, StateMissed: true, StateDeferred: true, StateOutdated: true}
	pendingSuppresses := map[State]bool{StateFailed: true, StateUnseen: true, StateStale: true, StateMissed: true,
		StateDeferred: true, StateOutdated: true}
	switch {
	case a == StateOK || b == StateOK:
		return "OK means no other state applies"
	case a == StateRetired:
		return "RETIRED suppresses everything"
	case a == StateUnknown:
		return "UNKNOWN short-circuits: no other row is evaluated"
	case a == StatePaused && pauseSuppresses[b]:
		return "PAUSED suppresses " + string(b)
	case a == StatePendingFirst && b == StateBlank:
		return "PENDING_FIRST and BLANK are mutually exclusive"
	case a == StatePendingFirst && pendingSuppresses[b]:
		return "PENDING_FIRST suppresses " + string(b)
	case a == StateBlank && (b == StateStale || b == StateMissed):
		return string(b) + " needs a success since enrollment; BLANK means there is none"
	case a == StateStuck && (b == StateMissed || b == StateDeferred):
		return string(b) + " requires no open run; STUCK is an open run"
	case a == StateFailed && b == StatePartial:
		return "FAILED and PARTIAL both read the newest finished run"
	case a == StateUnseen && b == StateMissed:
		return "MISSED requires a recent heartbeat; UNSEEN means none"
	case a == StateStale && (b == StateMissed || b == StateDeferred):
		return string(b) + " yields to STALE"
	case a == StateMissed && b == StateDeferred:
		return "MISSED yields to DEFERRED"
	}
	return ""
}

func TestPrecedencePairwise(t *testing.T) {
	order := Precedence()
	mut := stateMutators()

	// each mutator alone yields exactly its state as Primary (OK is the empty case)
	for _, s := range order {
		if s == StateOK {
			continue
		}
		t.Run("alone_"+string(s), func(t *testing.T) {
			f := baseFacts()
			mut[s](&f)
			got := Classify(f, t0)
			if got.Primary != s {
				t.Errorf("primary = %s, want %s (flags %v)", got.Primary, s, got.Flags)
			}
		})
	}

	tested, skipped := 0, 0
	for i := 0; i < len(order); i++ {
		for j := i + 1; j < len(order); j++ {
			a, b := order[i], order[j]
			name := string(a) + "_before_" + string(b)
			if why := pairExcluded(a, b); why != "" {
				skipped++
				t.Run(name, func(t *testing.T) { t.Skip(why) })
				continue
			}
			tested++
			t.Run(name, func(t *testing.T) {
				f := baseFacts()
				mut[a](&f)
				mut[b](&f)
				got := Classify(f, t0)
				if got.Primary != a {
					t.Errorf("primary = %s, want %s (flags %v)", got.Primary, a, got.Flags)
				}
				if !hasState(got.Flags, a) || !hasState(got.Flags, b) {
					t.Errorf("flags %v must contain both %s and %s", got.Flags, a, b)
				}
				last := -1
				for _, fl := range got.Flags {
					if fl.rank() <= last {
						t.Errorf("flags %v are not in precedence order", got.Flags)
					}
					last = fl.rank()
				}
			})
		}
	}
	t.Logf("pairs asserted: %d, skipped as mutually exclusive or suppressed: %d", tested, skipped)
	if tested < 60 {
		t.Errorf("only %d pairs were asserted: the exclusion table is swallowing the test", tested)
	}
}

// ---- never quiet green ---------------------------------------------------------------

func randomWorld(rng *rand.Rand) (w World, mustNotBeClear bool) {
	mods := []devMod{nil, nil, nil, modStale, modFailed, modRetired, modTimedPause, modIndefPause, modMissed,
		modDeferred, modOutdated, modPending, modUnknown,
		func(f *DeviceFacts) { f.LastSeen = ago(300 * hour) },
		func(f *DeviceFacts) { f.LastSuccess = -1 },
		func(f *DeviceFacts) { f.Escrow = nil },
		func(f *DeviceFacts) { f.IdentityConflictAt = ago(hour) },
		func(f *DeviceFacts) { f.Complete = false; f.LoadErrorClass = "timeout" },
		func(f *DeviceFacts) { f.Ack = &AckFacts{AckedAt: ago(hour)} },
	}
	nCust := rng.IntN(5)
	var cs []CustomerFacts
	for c := 1; c <= nCust; c++ {
		cf := customer(c, fmt.Sprintf("Customer %d", c))
		switch rng.IntN(6) {
		case 0:
			cf.State = "suspended"
		case 1:
			cf.State = "archived"
		case 2:
			cf.Expected = true
		}
		cs = append(cs, cf)
	}
	nDev := rng.IntN(9)
	var ds []DeviceFacts
	for n := 1; n <= nDev; n++ {
		owner := 1
		if nCust > 0 {
			owner = 1 + rng.IntN(nCust+1) // sometimes an orphan
		}
		d := deviceOf(n, owner, fmt.Sprintf("host-%02d", n))
		for k := rng.IntN(3); k > 0; k-- {
			if m := mods[rng.IntN(len(mods))]; m != nil {
				m(&d)
			}
		}
		if !d.Complete {
			mustNotBeClear = true
		}
		ds = append(ds, d)
	}
	w = World{
		Devices:             ds,
		Customers:           cs,
		FleetEverEnrolled:   rng.IntN(2) == 0,
		PreviousDeviceCount: []int{-1, 0, 3, 50}[rng.IntN(4)],
		LoadComplete:        rng.IntN(5) != 0,
		LoadErrors:          []int{0, 0, 0, 1, 4}[rng.IntN(5)],
		LoadedAt:            t0,
		Probes:              healthyProbes(),
	}
	if !w.LoadComplete || w.LoadErrors > 0 {
		mustNotBeClear = true
	}
	if w.PreviousDeviceCount > 0 && len(w.Devices) == 0 {
		mustNotBeClear = true
	}
	return w, mustNotBeClear
}

// TestNeverQuietGreen is the property test behind the project's founding rule:
// a world with a failed or partial load, or any device whose facts are
// incomplete, can never be reported as all clear, whatever else is in it.
func TestNeverQuietGreen(t *testing.T) {
	rng := rand.New(rand.NewPCG(20261022, 42))
	clear, notClear, forced := 0, 0, 0
	for i := 0; i < 4000; i++ {
		w, mustNot := randomWorld(rng)
		v := Evaluate(w, t0)
		again := Evaluate(w, t0)
		if !reflect.DeepEqual(v, again) {
			t.Fatalf("iteration %d: Evaluate is not deterministic", i)
		}
		if mustNot {
			forced++
			if v.AllClear() {
				t.Fatalf("iteration %d: AllClear true for a world with incomplete data: %+v", i, w)
			}
			if v.Complete {
				t.Fatalf("iteration %d: Complete true for a world with incomplete data", i)
			}
		}
		if v.AllClear() {
			clear++
			// belt and braces: AllClear means a quiet view in every respect
			if len(v.Banners) != 0 || !v.Complete {
				t.Fatalf("iteration %d: clear with banners or incomplete", i)
			}
			for _, d := range v.Devices {
				if d.Primary == StateUnknown || d.Alarm != SeverityNone || hasState(d.Flags, StateUnknown) {
					t.Fatalf("iteration %d: clear with %s device %s alarm %s", i, d.Primary, d.Hostname, d.Alarm)
				}
			}
			for _, c := range v.Customers {
				if c.Alarm != SeverityNone || c.Status == CustomerMissing || c.Status == CustomerUnknown {
					t.Fatalf("iteration %d: clear with %s customer", i, c.Status)
				}
			}
			if len(v.Devices) == 0 && !v.FirstRun {
				t.Fatalf("iteration %d: clear with an empty fleet that is not a first run", i)
			}
		} else {
			notClear++
		}
		// every UNKNOWN device must be critical and visible
		for _, d := range v.Devices {
			if (d.Primary == StateUnknown || hasState(d.Flags, StateUnknown)) && d.Alarm != SeverityCritical {
				t.Fatalf("iteration %d: UNKNOWN device %s not critical", i, d.Hostname)
			}
		}
	}
	t.Logf("worlds: %d forced-not-clear, %d clear, %d not clear", forced, clear, notClear)
	if forced < 500 || clear < 20 || notClear < 500 {
		t.Errorf("property test is vacuous: forced %d clear %d notClear %d", forced, clear, notClear)
	}
}

func TestClassifyNeverPanicsOnGarbage(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	garbage := []Millis{0, -1, 1, 1_700_000_000, 999_999_999_999, 1_000_000_000_000, Millis(t0.UnixMilli()),
		4_102_444_800_000, 4_102_444_800_001, 9_223_372_036_854_775_807, -9_223_372_036_854_775_808}
	g := func() Millis { return garbage[rng.IntN(len(garbage))] }
	strs := []string{"", "x", "success", "failed", "partial", "deferred", "exploded", "network", "permission", "missing_source", "gateway"}
	s := func() string { return strs[rng.IntN(len(strs))] }
	run := func() *RunFacts {
		if rng.IntN(3) == 0 {
			return nil
		}
		return &RunFacts{Outcome: s(), StartedAt: g(), FinishedAt: g(), FailedFiles: int64(rng.IntN(5)) - 1,
			TotalFiles: int64(rng.IntN(5)) - 1, CountsKnown: rng.IntN(2) == 0, ErrorClass: s(),
			ErrorCounts: map[string]int{s(): rng.IntN(4) - 1}, DeferredReason: s(), VSSFallback: []string{s()},
			Initial: rng.IntN(2) == 0}
	}
	for i := 0; i < 5000; i++ {
		f := DeviceFacts{
			ID: "x", EnrolledAt: g(), EnrollConfirmedAt: g(), FirstBackupDueBy: g(), RetiredAt: g() * Millis(rng.IntN(2)),
			PausedAt: g() * Millis(rng.IntN(2)), PausedUntil: g(), IdentityConflictAt: g() * Millis(rng.IntN(2)),
			LastSeen: g(), ServiceState: s(), ProgressAdvancedAt: g(), ClockOffset: time.Duration(rng.Int64()),
			LastSuccess: g(), LastBackup: run(), OpenRun: run(), LastAttemptStart: g(),
			ConsecutiveFailures: rng.IntN(5) - 1, AgentVersion: s(), ExpectedVersion: s(),
			Escrow: &EscrowFacts{Custody: s(), CreatedAt: g(), VerifiedAt: g(), LastVerifyErrorClass: s()},
			Maintenance: &MaintFacts{LastForget: run(), LastPrune: run(), LastCheck: run(), LastForgetSuccessAt: g(),
				DeferredCount: rng.IntN(20) - 1, ForgetCadence: time.Duration(rng.IntN(3)) * 7 * day},
			Ack:      &AckFacts{AckedAt: g(), SnoozeUntil: g(), SnoozeUntilNextSuccess: rng.IntN(2) == 0, AckedAtSuccess: g()},
			Complete: rng.IntN(8) != 0,
			Thresholds: Thresholds{StaleAfter: time.Duration(rng.IntN(3)) * 72 * hour, UnseenAfter: 168 * hour,
				MaxRun: 12 * hour, Interval: time.Duration(rng.IntN(2)) * day, MissedFactor: float64(rng.IntN(3))},
		}
		got := Classify(f, t0)
		if got.Primary == "" || len(got.Flags) == 0 || got.Flags[0] != got.Primary {
			t.Fatalf("iteration %d: malformed result %+v", i, got)
		}
	}
}
