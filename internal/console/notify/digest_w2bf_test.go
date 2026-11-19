package notify

import (
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
	v.Devices[2].Primary, v.Devices[2].Flags = state.StatePaused, []state.State{state.StatePaused}
	v.Devices[3].Primary, v.Devices[3].Flags = state.StatePendingFirst, []state.State{state.StatePendingFirst}
	if !v.AllClear() {
		t.Fatal("the control fleet is not all clear")
	}
	c, err := RenderDigest(v, nil, Options{Loc: cst})
	if err != nil {
		t.Fatal(err)
	}
	if c.Subject != "[CryoBackup] Backup digest Tue 17 Nov: all clear, 2 of 4 devices backed up, 1 paused, 1 awaiting first backup" {
		t.Fatalf("subject %q", c.Subject)
	}
	if first := strings.SplitN(c.Text, "\n", 3)[1]; first != "All clear: 2 of 4 devices backed up. 1 paused, 1 awaiting first backup." {
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
	if !strings.HasSuffix(c.Subject, ", 1 customer not included by policy") || strings.Contains(c.Subject, "all clear") {
		t.Fatalf("subject %q", c.Subject)
	}
	if !strings.Contains(c.Text, "\nTotals: 2 devices: 2 backed up.") || !strings.Contains(c.Text, "1 customer not included by policy.") {
		t.Fatalf("totals:\n%s", c.Text)
	}
	// The excluded device is green: the day is all clear, and still says what it left out.
	v.Devices[2].Primary, v.Devices[2].Flags, v.Devices[2].Alarm, v.Devices[2].AlarmFlags = state.StateOK, []state.State{state.StateOK}, state.SeverityNone, nil
	c, err = RenderDigest(v, nil, Options{Loc: cst})
	if err != nil {
		t.Fatal(err)
	}
	if first := strings.SplitN(c.Text, "\n", 3)[1]; first != "All 2 devices backed up. 1 customer not included by policy." {
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
