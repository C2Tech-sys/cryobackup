package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/mail"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// ---- rendering ----

// greenView is a complete fleet of n OK devices at one customer.
func greenView(n int) state.FleetView {
	v := state.FleetView{ComputedAt: renderNow, Complete: true, Customers: []state.CustomerState{{ID: custSummit, Name: "Summit Eye Care"}}}
	for i := 0; i < n; i++ {
		v.Devices = append(v.Devices, state.DeviceState{ID: fmt.Sprintf("01DEVGREEN%016d", i), CustomerID: custSummit,
			Hostname: fmt.Sprintf("SUMMIT-PC-%02d", i+1), Primary: state.StateOK, Flags: []state.State{state.StateOK}})
	}
	return v
}

// dev is one device of a hand-built view in state s at severity sev since the given age.
func dev(id, cust, host string, s state.State, sev state.Severity, age time.Duration) state.DeviceState {
	d := state.DeviceState{ID: id, CustomerID: cust, Hostname: host, Primary: s, Flags: []state.State{s}, Alarm: sev, Since: renderNow.Add(-age)}
	if sev > state.SeverityNone {
		d.AlarmFlags = []state.State{s}
	}
	return d
}

const (
	custAlpine = "01CUSTALPINE0000000000000C"
	devAlp1    = "01DEVALPINE000000000000001"
	devAlp2    = "01DEVALPINE000000000000002"
	devPark    = "01DEVPARK00000000000000004"
)

// mixedView has a MISSING customer, critical and alarm-level devices, a parked device, an OK one,
// and a suspended customer.
func mixedView() (state.FleetView, []store.Ack) {
	v := state.FleetView{ComputedAt: renderNow, Complete: true,
		Customers: []state.CustomerState{
			{ID: custSummit, Name: "Summit Eye Care"},
			{ID: custNorth, Name: "Northwind Dental", Status: state.CustomerMissing, Alarm: state.SeverityCritical},
			{ID: custAlpine, Name: "Alpine Orthodontics", Muted: true},
		},
		Devices: []state.DeviceState{
			dev(devFront, custSummit, "FRONTDESK-01", state.StateFailed, state.SeverityCritical, 19*time.Hour),
			dev(devRecep, custSummit, "RECEPTION-02", state.StateOK, state.SeverityNone, 0),
			dev(devLab, custSummit, "LAB-PC", state.StatePartial, state.SeverityAlarm, 6*time.Hour),
			dev(devAlp1, custAlpine, "ALP-OFFICE", state.StateStale, state.SeverityCritical, 80*time.Hour),
			dev(devAlp2, custAlpine, "ALP-XRAY", state.StateOutdated, state.SeverityAlarm, 30*24*time.Hour),
			dev(devPark, custSummit, "SPARE-LAPTOP", state.StatePaused, state.SeverityNone, 9*24*time.Hour),
		},
	}
	v.Devices[5].Attrs = []state.Attr{state.AttrPausedIndefinite}
	v.Devices[0].AlarmFlags = []state.State{state.StateFailed, state.StateEscrowUnverified}
	acks := []store.Ack{{SubjectType: store.EpisodeDevice, SubjectID: devFront, AlarmType: "failed"}}
	return v, acks
}

func TestGoldenDigestAllGreen(t *testing.T) {
	c, err := RenderDigest(greenView(3), nil, Options{Loc: cst, ConsoleURL: "https://console.example.com/"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Kind != mail.KindDigest || c.Subject != "[CryoBackup] Backup digest Tue 17 Nov: all clear, 3 devices backed up" {
		t.Fatalf("kind %q subject %q", c.Kind, c.Subject)
	}
	if first := strings.SplitN(c.Text, "\n", 3)[1]; first != "All 3 devices backed up." {
		t.Fatalf("first line %q", first)
	}
	golden(t, "digest_green.txt", c.Text)
	golden(t, "digest_green.html", c.HTML)
	m := c.Message("ops@example.com")
	m.From = "CryoBackup <console@example.com>"
	if err := m.Validate(); err != nil {
		t.Fatalf("rendered digest does not validate: %v", err)
	}
}

func TestGoldenDigestMixed(t *testing.T) {
	v, acks := mixedView()
	c, err := RenderDigest(v, acks, Options{Loc: cst, ConsoleURL: "https://console.example.com/"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Subject != "[CryoBackup] Backup digest Tue 17 Nov: 2 critical, 2 warnings, 1 customer missing" /* P20-W2BF: the missing customer once */ {
		t.Fatalf("subject %q", c.Subject)
	}
	golden(t, "digest_mixed.txt", c.Text)
	golden(t, "digest_mixed.html", c.HTML)
	// Critical before warnings before parked; the acknowledged line is listed and marked; the
	// suspended customer is listed and labelled; the OK device is not listed.
	text := c.Text
	ic, iw, ip := strings.Index(text, "\nCritical\n"), strings.Index(text, "\nWarnings\n"), strings.Index(text, "\nParked and long-deferred devices\n")
	if ic < 0 || iw < ic || ip < iw {
		t.Fatalf("section order: critical %d, warnings %d, parked %d", ic, iw, ip)
	}
	if !strings.Contains(text, "FRONTDESK-01 since") || !strings.Contains(text, "[ACKNOWLEDGED]") || strings.Contains(text, "RECEPTION-02") {
		t.Fatalf("lines:\n%s", text)
	}
	if !strings.Contains(text, "Alpine Orthodontics (suspended: notifications muted)") {
		t.Fatalf("suspended customer not labelled:\n%s", text)
	}
}

func TestGoldenDigestWithAConsoleBanner(t *testing.T) {
	v := greenView(2)
	v.Banners = []state.Banner{
		{ID: "C03", Severity: state.SeverityCritical, Text: "Alert e-mail is not being delivered: 3 attempts in a row failed."},
		{ID: "C18", Severity: state.SeverityAlarm, Text: "Alert e-mail has no recipient: alarms are not mailed to anyone."},
	}
	c, err := RenderDigest(v, nil, Options{Loc: cst})
	if err != nil {
		t.Fatal(err)
	}
	if c.Subject != "[CryoBackup] Backup digest Tue 17 Nov: 0 critical, 0 warnings, 2 console conditions" {
		t.Fatalf("subject %q", c.Subject)
	}
	if strings.Index(c.Text, "[CRITICAL] Alert e-mail") > strings.Index(c.Text, "\nCritical\n") {
		t.Fatal("the banner state is not first")
	}
	golden(t, "digest_banner.txt", c.Text)
	golden(t, "digest_banner.html", c.HTML)
}

// bigView is 300 devices at 30 customers with every kind of exception.
func bigView() state.FleetView {
	v := state.FleetView{ComputedAt: renderNow, Complete: true}
	kinds := []struct {
		s   state.State
		sev state.Severity
	}{{state.StateOK, state.SeverityNone}, {state.StateOK, state.SeverityNone}, {state.StateFailed, state.SeverityCritical},
		{state.StateStale, state.SeverityCritical}, {state.StatePartial, state.SeverityAlarm}, {state.StateOK, state.SeverityNone},
		{state.StateUnseen, state.SeverityCritical}, {state.StateMaintFailed, state.SeverityAlarm}, {state.StatePaused, state.SeverityNone},
		{state.StateOK, state.SeverityNone}}
	for c := 0; c < 30; c++ {
		id := fmt.Sprintf("01CUSTBIG%017d", c)
		cs := state.CustomerState{ID: id, Name: fmt.Sprintf("Customer %02d Family Practice", c+1)}
		if c%10 == 9 {
			cs.Status, cs.Alarm = state.CustomerMissing, state.SeverityCritical
		}
		v.Customers = append(v.Customers, cs)
		for i := 0; i < 10; i++ {
			k := kinds[(c+i)%len(kinds)]
			v.Devices = append(v.Devices, dev(fmt.Sprintf("01DEVBIG%04d%014d", c, i), id, fmt.Sprintf("C%02d-WS-%02d", c+1, i+1), k.s, k.sev, time.Duration(c+i+1)*time.Hour))
		}
	}
	return v
}

func TestGoldenDigest300Devices(t *testing.T) {
	c, err := RenderDigest(bigView(), nil, Options{Loc: cst, ConsoleURL: "https://console.example.com/"})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Subject) > 120 || !strings.HasPrefix(c.Subject, "[CryoBackup] Backup digest Tue 17 Nov: ") {
		t.Fatalf("subject %q (%d bytes)", c.Subject, len(c.Subject))
	}
	for _, r := range c.Subject {
		if r > 127 {
			t.Fatalf("subject is not ASCII: %q", c.Subject)
		}
	}
	if !strings.Contains(c.Text, "Totals: 300 devices:") {
		t.Fatalf("totals:\n%s", c.Text[len(c.Text)-600:])
	}
	golden(t, "digest_300.txt", c.Text)
	golden(t, "digest_300.html", c.HTML)
}

// ---- the job, send-now, preview and retry ----

var chicago = func() *time.Location {
	l, err := time.LoadLocation("America/Chicago")
	if err != nil {
		panic(err)
	}
	return l
}()

// digestWorld is a world with a Digester in Chicago time and the digest at at (HH:MM).
func digestWorld(t *testing.T, at string) (*world, *Digester) {
	t.Helper()
	w := newWorld(t)
	w.site("Summit Eye Care", 2)
	g := &Digester{Store: w.st, Dispatch: w.n.Dispatch, Configured: true, ConsoleURL: "https://console.example.test/", Log: w.n.Log,
		Loc: func() *time.Location { return chicago }, View: func(context.Context) (state.FleetView, error) { return w.view(), nil }}
	w.r.Rerender = map[string]RerenderFunc{store.DeliveryDigest: g.Rerender}
	w.digestTime(at)
	return w, g
}

func (w *world) digestTime(at string) {
	w.t.Helper()
	b, _ := json.Marshal(at)
	if err := w.st.PutSetting(w.ctx, store.SettingDigestLocalTime, b, w.actor.AdminID); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) digestOn(on bool) {
	w.t.Helper()
	if err := w.st.PutSetting(w.ctx, store.SettingDigestEnabled, json.RawMessage(fmt.Sprint(on)), w.actor.AdminID); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) digestRecipient(addr string) store.Recipient {
	w.t.Helper()
	r, err := w.st.CreateRecipient(w.ctx, store.ScopeAll(), store.RecipientInput{Name: "Digest " + addr, Address: addr, Digest: true}, w.actor)
	if err != nil {
		w.t.Fatal(err)
	}
	return r
}

// at sets the clock to a local time in Chicago.
func (w *world) at(local string) {
	w.t.Helper()
	tm, err := time.ParseInLocation("2006-01-02 15:04", local, chicago)
	if err != nil {
		w.t.Fatal(err)
	}
	w.clk.mu.Lock()
	w.clk.t = tm.UTC()
	w.clk.mu.Unlock()
}

func (w *world) digests() map[string][]mail.Message {
	out := map[string][]mail.Message{}
	for _, m := range w.fake.Sent() {
		if m.Kind == mail.KindDigest {
			out[m.To[0]] = append(out[m.To[0]], m)
		}
	}
	return out
}

func (w *world) digestRows() []store.Delivery {
	var out []store.Delivery
	for _, d := range w.deliveries() {
		if d.Kind == store.DeliveryDigest {
			out = append(out, d)
		}
	}
	return out
}

func runDigest(t *testing.T, g *Digester) DigestResult {
	t.Helper()
	res, err := g.Run(context.Background())
	if err != nil {
		t.Fatalf("digest run: %v", err)
	}
	return res
}

// At the time, every recipient with the digest switch gets one digest; the next runs none; a
// recipient without the switch gets none; an all-green day is still sent.
func TestDigestOncePerRecipientAtTheTime(t *testing.T) {
	w, g := digestWorld(t, "07:00")
	a, b := w.digestRecipient("a@example.test"), w.digestRecipient("b@example.test")
	w.recipient("alerts-only@example.test", true, true)
	w.at("2026-11-18 06:59")
	if res := runDigest(t, g); res.Skipped != DigestSkippedNotDue || len(w.digests()) != 0 {
		t.Fatalf("before the time: %+v", res)
	}
	w.at("2026-11-18 07:00")
	if res := runDigest(t, g); res.Sent != 2 || res.Date != "2026-11-18" {
		t.Fatalf("at the time: %+v", res)
	}
	for i := 0; i < 3; i++ {
		w.clk.Advance(time.Minute)
		if res := runDigest(t, g); res.Skipped != DigestSkippedAlreadySent {
			t.Fatalf("run %d after: %+v", i, res)
		}
	}
	got := w.digests()
	if len(got) != 2 || len(got["a@example.test"]) != 1 || len(got["b@example.test"]) != 1 {
		t.Fatalf("digests %v", got)
	}
	if s := got["a@example.test"][0].Subject; !strings.Contains(s, "all clear, 2 devices backed up") {
		t.Fatalf("an all-green day: subject %q", s)
	}
	keys := map[string]bool{}
	for _, d := range w.digestRows() {
		keys[d.DedupeKey] = true
	}
	if !keys["digest:2026-11-18:"+a.ID] || !keys["digest:2026-11-18:"+b.ID] || len(keys) != 2 {
		t.Fatalf("keys %v", keys)
	}
	w.noLeaks()
}

// A console that was down at the time sends today's digest when it comes back the same day; one
// that comes back the next morning before the time never sends yesterday's.
func TestDigestCatchUpTodayNeverYesterday(t *testing.T) {
	w, g := digestWorld(t, "07:00")
	w.digestRecipient("a@example.test")
	w.at("2026-11-18 11:42") // down from before 07:00 until now
	if res := runDigest(t, g); res.Sent != 1 {
		t.Fatalf("catch-up: %+v", res)
	}
	w.at("2026-11-20 06:10") // down all of the 19th; back before the 20th's time
	if res := runDigest(t, g); res.Skipped != DigestSkippedNotDue || len(w.digests()["a@example.test"]) != 1 {
		t.Fatalf("yesterday's digest was sent late: %+v", res)
	}
	w.at("2026-11-20 07:00")
	if res := runDigest(t, g); res.Sent != 1 || res.Date != "2026-11-20" {
		t.Fatalf("the 20th: %+v", res)
	}
}

// Daylight-saving days in Chicago, with the clock stepping a minute at a time over two local
// days around each change: a time the change skips (02:30 on 8 March) and one it repeats (01:30
// on 1 November). Exactly one digest per local date per recipient, and none missing.
func TestDigestAcrossDaylightSavingChanges(t *testing.T) {
	for _, tc := range []struct {
		name, at, from string
		dates          []string
	}{
		{"spring_forward_skipped_time", "02:30", "2026-03-07 00:00", []string{"2026-03-07", "2026-03-08", "2026-03-09"}},
		{"fall_back_repeated_time", "01:30", "2026-10-31 00:00", []string{"2026-10-31", "2026-11-01", "2026-11-02"}},
		{"spring_forward_morning", "07:00", "2026-03-07 00:00", []string{"2026-03-07", "2026-03-08", "2026-03-09"}},
		{"fall_back_morning", "07:00", "2026-10-31 00:00", []string{"2026-10-31", "2026-11-01", "2026-11-02"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, g := digestWorld(t, tc.at)
			a := w.digestRecipient("a@example.test")
			w.at(tc.from)
			end := w.clk.Now().Add(72 * time.Hour)
			for w.clk.Now().Before(end) {
				runDigest(t, g)
				w.clk.Advance(time.Minute)
			}
			perDate := map[string]int{}
			for _, d := range w.digestRows() {
				perDate[d.DedupeKey]++
			}
			if len(w.digests()["a@example.test"]) != len(tc.dates) {
				t.Fatalf("%d digests sent over %v: %v", len(w.digests()["a@example.test"]), tc.dates, perDate)
			}
			for _, date := range tc.dates {
				if perDate["digest:"+date+":"+a.ID] != 1 {
					t.Fatalf("date %s: %v", date, perDate)
				}
			}
		})
	}
}

// The time or the zone changed during the day, after the day's digest went out: no second one
// that day. Changed to a time that has passed before the day's digest went out: it goes out now.
func TestDigestTimeChangedDuringTheDay(t *testing.T) {
	w, g := digestWorld(t, "07:00")
	w.digestRecipient("a@example.test")
	w.at("2026-11-18 07:05")
	runDigest(t, g)
	for _, step := range []struct{ local, time string }{{"2026-11-18 15:00", "15:00"}, {"2026-11-18 15:01", "06:00"}, {"2026-11-18 23:59", "23:59"}} {
		w.digestTime(step.time)
		w.at(step.local)
		runDigest(t, g)
	}
	// The zone moves west: the local date is still the 18th there, whose digest went out.
	g.Loc = func() *time.Location { return time.FixedZone("UTC-10", -10*3600) }
	w.digestTime("07:00")
	w.at("2026-11-19 03:00") // 23:00 on the 18th at UTC-10
	runDigest(t, g)
	if n := len(w.digests()["a@example.test"]); n != 1 {
		t.Fatalf("%d digests on one local date", n)
	}
	// A later time not yet reached on a fresh day, then moved earlier past now: it goes out now.
	g.Loc = func() *time.Location { return chicago }
	w.digestTime("18:00")
	w.at("2026-11-19 10:00")
	if res := runDigest(t, g); res.Skipped != DigestSkippedNotDue {
		t.Fatalf("before 18:00: %+v", res)
	}
	w.digestTime("09:00")
	if res := runDigest(t, g); res.Sent != 1 || res.Date != "2026-11-19" {
		t.Fatalf("moved before now: %+v", res)
	}
}

// One recipient failing does not stop the others, and the retry job sends that one alone,
// re-rendered as the state is at retry time.
func TestDigestOneRecipientFailsAndIsRetriedAlone(t *testing.T) {
	w, g := digestWorld(t, "07:00")
	w.digestRecipient("a@example.test")
	w.digestRecipient("b@example.test")
	w.fake.FailWhen(func(m mail.Message) *mail.SendError {
		if m.To[0] == "a@example.test" {
			return &mail.SendError{Stage: mail.StageConnect, Class: mail.ClassTimeout, Temporary: true}
		}
		return nil
	})
	w.at("2026-11-18 07:00")
	if res := runDigest(t, g); res.Sent != 1 || res.Failed != 1 {
		t.Fatalf("first run: %+v", res)
	}
	w.fake.FailWhen(nil)
	w.set(state.StateFailed, w.devices[0]) // the state changed before the retry
	w.clk.Advance(time.Minute)
	if res := runDigest(t, g); res.Skipped != DigestSkippedAlreadySent {
		t.Fatalf("the job sent again: %+v", res)
	}
	if rr := w.retry(); rr.Sent != 1 {
		t.Fatalf("retry %+v", rr)
	}
	got := w.digests()
	if len(got["a@example.test"]) != 1 || len(got["b@example.test"]) != 1 {
		t.Fatalf("digests %v", got)
	}
	if !strings.Contains(got["a@example.test"][0].Text, w.host[w.devices[0]]) || strings.Contains(got["b@example.test"][0].Text, w.host[w.devices[0]]) {
		t.Fatal("the retry did not render the state at retry time")
	}
}

// Disabled, not configured, no recipient: nothing is enqueued, and the result says which.
func TestDigestNothingEnqueuedWhenItCannotOrMustNotGo(t *testing.T) {
	w, g := digestWorld(t, "07:00")
	w.at("2026-11-18 08:00")
	if res := runDigest(t, g); res.Skipped != DigestSkippedNoRecipient {
		t.Fatalf("no recipient: %+v", res)
	}
	w.digestRecipient("a@example.test")
	w.digestOn(false)
	if res := runDigest(t, g); res.Skipped != DigestSkippedDisabled {
		t.Fatalf("disabled: %+v", res)
	}
	w.digestOn(true)
	g.Configured = false
	if res := runDigest(t, g); res.Skipped != DigestSkippedNotConfigured {
		t.Fatalf("not configured: %+v", res)
	}
	if len(w.digestRows()) != 0 || w.fake.Calls() != 0 {
		t.Fatalf("%d rows, %d sends", len(w.digestRows()), w.fake.Calls())
	}
	w.digestTime("7am")
	g.Configured = true
	if _, err := g.Run(context.Background()); err != ErrDigestTime {
		t.Fatalf("an unusable time: %v", err)
	}
}

// Send now: today's digest when there is none yet (and then the job sends no other); a no-op that
// says so once it went out; forced, a separate message that does not count as today's.
func TestDigestSendNow(t *testing.T) {
	w, g := digestWorld(t, "07:00")
	a := w.digestRecipient("a@example.test")
	w.at("2026-11-18 05:00")
	res, err := g.SendDigestNow(context.Background(), false)
	if err != nil || res.Sent != 1 || res.Recipients[0].RuleID != a.ID {
		t.Fatalf("send now: %+v %v", res, err)
	}
	w.at("2026-11-18 07:00")
	if res := runDigest(t, g); res.Skipped != DigestSkippedAlreadySent {
		t.Fatalf("the job after a send-now: %+v", res)
	}
	res, err = g.SendDigestNow(context.Background(), false)
	if err != nil || res.Sent != 0 || len(res.Recipients) != 1 || res.Recipients[0].Outcome != DigestAlreadySent {
		t.Fatalf("send now again: %+v %v", res, err)
	}
	res, err = g.SendDigestNow(context.Background(), true)
	if err != nil || res.Sent != 1 || !res.Forced {
		t.Fatalf("forced: %+v %v", res, err)
	}
	if n := len(w.digests()["a@example.test"]); n != 2 {
		t.Fatalf("%d digests", n)
	}
	var forced store.Delivery
	for _, d := range w.digestRows() {
		if strings.HasPrefix(d.DedupeKey, keyDigestForced+":") {
			forced = d
		}
	}
	if forced.ID == "" || strings.HasPrefix(forced.DedupeKey, "digest:") {
		t.Fatalf("forced key %q", forced.DedupeKey)
	}
	// A forced send before the day's time does not count as the day's digest.
	w.at("2026-11-19 05:00")
	if res, err := g.SendDigestNow(context.Background(), true); err != nil || res.Sent != 1 {
		t.Fatalf("forced on the 19th: %+v %v", res, err)
	}
	w.at("2026-11-19 07:00")
	if res := runDigest(t, g); res.Sent != 1 {
		t.Fatalf("the 19th's digest after a forced send: %+v", res)
	}
	g.Configured = false
	if res, _ := g.SendDigestNow(context.Background(), true); res.Skipped != DigestSkippedNotConfigured {
		t.Fatalf("not configured: %+v", res)
	}
}

// Preview renders the digest and writes nothing.
func TestDigestPreviewWritesNothing(t *testing.T) {
	w, g := digestWorld(t, "07:00")
	w.digestRecipient("a@example.test")
	g.Configured = false
	c, err := g.PreviewDigest(context.Background())
	if err != nil || c.Kind != mail.KindDigest || !strings.Contains(c.Text, "All 2 devices backed up.") {
		t.Fatalf("preview %v:\n%s", err, c.Text)
	}
	if len(w.deliveries()) != 0 || w.fake.Calls() != 0 {
		t.Fatal("the preview wrote or sent something")
	}
}
