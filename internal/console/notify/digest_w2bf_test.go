package notify

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/mail"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// P20-W2BF item 2: 2 OK devices, 1 paused, 1 awaiting its first backup. The headline and the
// subject count only what is backed up and name the rest.
func TestGoldenDigestAllClearNamesWhatIsNotBackedUp(t *testing.T) {
	v := greenView(4)
	v.Devices[2].Primary, v.Devices[2].Flags, v.Devices[2].CurrentBackup = state.StatePaused, []state.State{state.StatePaused}, false
	v.Devices[3].Primary, v.Devices[3].Flags, v.Devices[3].CurrentBackup = state.StatePendingFirst, []state.State{state.StatePendingFirst}, false
	if !v.AllClear() {
		t.Fatal("the control fleet is not all clear")
	}
	c, err := RenderDigest(v, nil, Options{Loc: cst})
	if err != nil {
		t.Fatal(err)
	}
	// C4: an exception (a parked or never-backed-up device) means no "All clear".
	if c.Subject != "[CryoBackup] Backup digest Tue 17 Nov: 2 of 4 devices backed up, 1 paused, 1 awaiting first backup" {
		t.Fatalf("subject %q", c.Subject)
	}
	if first := strings.SplitN(c.Text, "\n", 3)[1]; first != "2 of 4 devices backed up. 1 paused, 1 awaiting first backup." {
		t.Fatalf("first line %q", first)
	}
	if strings.Contains(c.Text, "All 4") || strings.Contains(c.Subject, "all clear, 4 devices") || strings.Contains(c.Text, ": 4 backed up") {
		t.Fatalf("a number described as backed up holds devices that are not:\n%s", c.Text)
	}
	golden(t, "digest_green_partial.txt", c.Text)
	golden(t, "digest_green_partial.html", c.HTML)
}

// P20-W2BF item 3: devices whose policy leaves them out of the digest are not listed and are
// named in the headline and the totals, so a day never reads as all clear without saying so.
func TestDigestNamesWhatThePolicyLeavesOut(t *testing.T) {
	v := greenView(2)
	v.Customers = append(v.Customers, state.CustomerState{ID: custAlpine, Name: "Alpine Orthodontics"})
	d := dev(devAlp1, custAlpine, "ALP-OFFICE", state.StateFailed, state.SeverityCritical, time.Hour)
	d.DigestExcluded = true
	v.Devices = append(v.Devices, d)
	c, err := RenderDigest(v, nil, Options{Loc: cst})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(c.Text, "ALP-OFFICE") || strings.Contains(c.Text, "Alpine") {
		t.Fatalf("the excluded customer is listed:\n%s", c.Text)
	}
	// B4: the day is judged over the included devices; the excluded critical device is named only
	// in the "not included by policy" line.
	if c.Subject != "[CryoBackup] Backup digest Tue 17 Nov: all clear, 2 of 2 devices backed up, 1 customer not included by policy" {
		t.Fatalf("subject %q", c.Subject)
	}
	if !strings.Contains(c.Text, "\nTotals: 2 devices: 2 backed up.") || !strings.Contains(c.Text, "1 customer not included by policy.") {
		t.Fatalf("totals:\n%s", c.Text)
	}
	// The excluded device is green: the day is all clear, and still says what it left out.
	v.Devices[2].Primary, v.Devices[2].Flags, v.Devices[2].Alarm, v.Devices[2].AlarmFlags, v.Devices[2].CurrentBackup = state.StateOK, []state.State{state.StateOK}, state.SeverityNone, nil, true
	c, err = RenderDigest(v, nil, Options{Loc: cst})
	if err != nil {
		t.Fatal(err)
	}
	if first := strings.SplitN(c.Text, "\n", 3)[1]; first != "All clear: 2 of 2 devices backed up. 1 customer not included by policy." {
		t.Fatalf("first line %q", first)
	}
}

// P20-W2BF item 7: the subject counts a missing customer once, and agrees with the totals line.
func TestDigestSubjectCountsAMissingCustomerOnce(t *testing.T) {
	v, acks := mixedView()
	c, err := RenderDigest(v, acks, Options{Loc: cst})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(c.Subject, ": 2 critical, 2 warnings, 1 customer missing") {
		t.Fatalf("subject %q", c.Subject)
	}
	if !strings.Contains(c.Text, "Exceptions: 2 critical, 2 warnings, 1 parked or deferred. 3 customers, 1 missing.") {
		t.Fatalf("totals:\n%s", c.Text)
	}
}

// P20-W2BF item 7: a digest that failed at 23:59 and is retried after midnight carries the date
// of its key; one still pending when the digest is turned off is suppressed (deduped, why
// digest_disabled).
func TestDigestRetryKeepsItsDateAndStopsWhenDisabled(t *testing.T) {
	w, g := digestWorld(t, "23:59")
	w.digestRecipient("a@example.test")
	w.digestRecipient("b@example.test")
	w.fake.FailWhen(func(m mail.Message) *mail.SendError {
		return &mail.SendError{Stage: mail.StageConnect, Class: mail.ClassTimeout, Temporary: true}
	})
	w.at("2026-11-18 23:59")
	if res := runDigest(t, g); res.Failed != 2 {
		t.Fatalf("first run %+v", res)
	}
	w.fake.FailWhen(func(m mail.Message) *mail.SendError {
		if m.To[0] == "b@example.test" {
			return &mail.SendError{Stage: mail.StageConnect, Class: mail.ClassTimeout, Temporary: true}
		}
		return nil
	})
	w.at("2026-11-19 00:01")
	if rr := w.retry(); rr.Sent != 1 {
		t.Fatalf("retry %+v", rr)
	}
	got := w.digests()["a@example.test"]
	if len(got) != 1 || !strings.Contains(got[0].Subject, "Wed 18 Nov") {
		t.Fatalf("retried digest %v", got)
	}
	w.digestOn(false)
	w.fake.FailWhen(nil)
	w.at("2026-11-19 00:10")
	if rr := w.retry(); rr.Sent != 0 || rr.Suppressed != 1 {
		t.Fatalf("retry after the digest was turned off %+v", rr)
	}
	if !strings.Contains(w.logs.String(), `"why":"digest_disabled"`) {
		t.Fatalf("log:\n%s", w.logs.String())
	}
	for _, d := range w.digestRows() {
		if d.Result == store.DeliverySuppressed && d.SuppressReason != "deduped" {
			t.Fatalf("suppress reason %v", d.SuppressReason)
		}
	}
}

// P20-W2BF B4: the headline per case. The day's all-clear is judged over the included devices; a
// day with nothing backed up never says "all clear". Each case is a golden.
func TestGoldenDigestHeadlines(t *testing.T) {
	paused := greenView(3)
	for i := range paused.Devices {
		paused.Devices[i].Primary, paused.Devices[i].Flags, paused.Devices[i].CurrentBackup = state.StatePaused, []state.State{state.StatePaused}, false
	}
	empty := state.FleetView{ComputedAt: renderNow, Complete: true, FirstRun: true}
	allExcluded := greenView(2)
	for i := range allExcluded.Devices {
		allExcluded.Devices[i].DigestExcluded = true
	}
	excludedCritical := greenView(2)
	excludedCritical.Customers = append(excludedCritical.Customers, state.CustomerState{ID: custAlpine, Name: "Alpine Orthodontics", Alarm: state.SeverityCritical})
	x := dev(devAlp1, custAlpine, "ALP-OFFICE", state.StateFailed, state.SeverityCritical, time.Hour)
	x.DigestExcluded = true
	excludedCritical.Devices = append(excludedCritical.Devices, x)
	for _, tc := range []struct {
		name, golden, subject, first string
		v                            state.FleetView
	}{
		{"all paused", "digest_all_paused", "no devices backed up, 3 paused", "No devices backed up: 3 paused.", paused},
		{"no devices", "digest_no_devices", "no devices enrolled yet", "No devices enrolled yet: nothing is being backed up.", empty},
		{"all excluded", "digest_all_excluded", "no devices included, 1 customer not included by policy",
			"No devices included in this digest: 1 customer not included by policy.", allExcluded},
		{"excluded critical, the rest OK", "digest_excluded_critical", "all clear, 2 of 2 devices backed up, 1 customer not included by policy",
			"All clear: 2 of 2 devices backed up. 1 customer not included by policy.", excludedCritical},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := RenderDigest(tc.v, nil, Options{Loc: cst})
			if err != nil {
				t.Fatal(err)
			}
			if want := "[CryoBackup] Backup digest Tue 17 Nov: " + tc.subject; c.Subject != want {
				t.Fatalf("subject %q, want %q", c.Subject, want)
			}
			if first := strings.SplitN(c.Text, "\n", 3)[1]; first != tc.first {
				t.Fatalf("first line %q, want %q", first, tc.first)
			}
			if tc.golden != "digest_excluded_critical" && strings.Contains(strings.ToLower(c.Subject+c.Text), "all clear") {
				t.Fatalf("a day with nothing backed up says all clear:\n%s", c.Text)
			}
			golden(t, tc.golden+".txt", c.Text)
			golden(t, tc.golden+".html", c.HTML)
		})
	}
}

// P20-W2BF C4, D4 and D7: a device in an exception state is counted under that exception only in
// the Totals, whatever the age of its last success, so an outdated agent with current backups has
// "0 backed up, 1 on an outdated agent" there. The headline leads with the exception, "1 device
// needs attention: 1 on an outdated agent.": never "All clear", and never "No devices backed up"
// while the engine says the device has a current backup.
func TestGoldenDigestOutdatedAgentCountedUnderItsException(t *testing.T) {
	v := greenView(1)
	v.Devices[0].Primary, v.Devices[0].Flags = state.StateOutdated, []state.State{state.StateOutdated}
	c, err := RenderDigest(v, nil, Options{Loc: cst})
	if err != nil {
		t.Fatal(err)
	}
	if c.Subject != "[CryoBackup] Backup digest Tue 17 Nov: 1 device needs attention, 1 on an outdated agent" {
		t.Fatalf("subject %q", c.Subject)
	}
	if first := strings.SplitN(c.Text, "\n", 3)[1]; first != "1 device needs attention: 1 on an outdated agent." {
		t.Fatalf("first line %q", first)
	}
	if !strings.Contains(c.Text, "\nTotals: 1 device: 0 backed up, 1 on an outdated agent.") {
		t.Fatalf("totals:\n%s", c.Text)
	}
	golden(t, "digest_outdated.txt", c.Text)
	golden(t, "digest_outdated.html", c.HTML)
}

// P20-W2BF D4: the Totals line partitions the devices. Every device the digest speaks for is
// counted once: as backed up when it has a current backup and is in no exception state, otherwise
// under its exception state only, whatever the age of its last success. Over every engine state
// alone and mixed, with and without a current backup, the numbers before "Exceptions:" sum to the
// device count, the backed-up number is exactly that count, and the headline keeps the batch 2
// invariants over the same number (never "All clear" with an exception, never "No devices backed
// up" when one is). D7: "No devices backed up" only when no included device has a current backup
// per the engine.
func TestDigestTotalsPartitionTheDevices(t *testing.T) {
	mk := func(i int, s state.State, current bool) state.DeviceState {
		d := dev(fmt.Sprintf("01DEVPART%017d", i), custSummit, fmt.Sprintf("SUMMIT-PC-%02d", i+1), s, state.SeverityNone, time.Hour)
		d.CurrentBackup = current
		return d
	}
	type tc struct {
		name string
		devs []state.DeviceState
	}
	var cases []tc
	all := state.Precedence()
	for _, s := range all {
		for _, current := range []bool{true, false} {
			cases = append(cases, tc{fmt.Sprintf("%s current=%v", s, current), []state.DeviceState{mk(0, s, current)}})
		}
	}
	for _, mix := range []string{"current", "not current", "alternating"} {
		var ds []state.DeviceState
		for i, s := range all {
			ds = append(ds, mk(i, s, mix == "current" || mix == "alternating" && i%2 == 0))
		}
		cases = append(cases, tc{"all states " + mix, ds})
		ds = append(append([]state.DeviceState(nil), ds...), mk(len(all), state.StateOK, true), mk(len(all)+1, state.StateOK, true))
		cases = append(cases, tc{"all states " + mix + " and two OK", ds})
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := greenView(0)
			v.Devices = c.devs
			devices, backedUp, exception, anyCurrent := 0, 0, false, false
			for _, d := range c.devs {
				if d.Primary == state.StateRetired {
					continue
				}
				devices++
				anyCurrent = anyCurrent || d.CurrentBackup
				if d.Primary == state.StateOK && d.CurrentBackup {
					backedUp++
				} else {
					exception = true
				}
			}
			out, err := RenderDigest(v, nil, Options{Loc: cst})
			if err != nil {
				t.Fatal(err)
			}
			var line string
			for _, l := range strings.Split(out.Text, "\n") {
				if strings.HasPrefix(l, "Totals: ") {
					line = l
				}
			}
			counts, _, ok := strings.Cut(strings.TrimPrefix(line, "Totals: "), ". Exceptions:")
			head, parts, ok2 := strings.Cut(counts, ": ")
			if !ok || !ok2 {
				t.Fatalf("no Totals line:\n%s", out.Text)
			}
			var n, sum, shown int
			if _, err := fmt.Sscanf(head, "%d", &n); err != nil || n != devices {
				t.Fatalf("Totals speak for %q, want %d devices: %s", head, devices, line)
			}
			for i, p := range strings.Split(parts, ", ") {
				var k int
				if _, err := fmt.Sscanf(p, "%d", &k); err != nil {
					t.Fatalf("part %q: %v", p, err)
				}
				if i == 0 {
					if !strings.HasSuffix(p, " backed up") {
						t.Fatalf("first part %q is not the backed-up number: %s", p, line)
					}
					shown = k
				}
				sum += k
			}
			if sum != devices || shown != backedUp {
				t.Fatalf("Totals sum to %d (backed up %d), want %d devices (backed up %d): %s", sum, shown, devices, backedUp, line)
			}
			first := strings.SplitN(out.Text, "\n", 3)[1]
			if exception && (strings.Contains(strings.ToLower(first), "all clear") || strings.HasPrefix(first, "All ") || strings.HasPrefix(first, "The 1 device")) {
				t.Fatalf("headline %q with an exception", first)
			}
			if backedUp > 0 && strings.Contains(first, "No devices backed up") {
				t.Fatalf("headline %q with %d backed up", first, backedUp)
			}
			if anyCurrent && strings.Contains(strings.ToLower(first+out.Subject), "no devices backed up") { // D7
				t.Fatalf("headline %q (subject %q) while the engine says a device has a current backup", first, out.Subject)
			}
		})
	}
}
