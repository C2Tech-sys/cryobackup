package ui

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// P20-W3B: acknowledge and snooze, where the alarm is shown (the device panel, the MISSING customer).

func (e *env) activeAck(subjectID string) *store.Ack {
	e.t.Helper()
	acks, err := e.st.ListActiveAcks(context.Background(), store.ScopeAll())
	if err != nil {
		e.t.Fatal(err)
	}
	for i := range acks {
		if acks[i].SubjectID == subjectID {
			return &acks[i]
		}
	}
	return nil
}

// alarmsBlock is the "Open alarms" section of a page.
func alarmsBlock(t *testing.T, body string) string {
	t.Helper()
	i := strings.Index(body, `id="alarms"`)
	if i < 0 {
		t.Fatal("no Open alarms block on the page")
	}
	j := strings.Index(body[i:], "</section>")
	return body[i : i+j]
}

func TestP20W3B_AnAcknowledgedAlarmStaysAnAlarm(t *testing.T) {
	e := newM4Env(t)
	e.login()
	body := e.page("/devices/"+e.dev[fixtures.AckedHost], 200)
	block := alarmsBlock(t, body)
	mustContain(t, "acked device", block, `<span class="st crit"><i aria-hidden="true"></i>FAILED</span>`, `class="alarm crit acked"`, ">ACK<",
		"<b>Acknowledged</b> by admin", "until cleared or the alarm ends", fixtures.AckNote, ">Clear<", "Change…")
	// the header chip is still the red FAILED chip, and the engine's ACK badge sits beside it
	head := body[strings.Index(body, `id="dev-h"`):strings.Index(body, `class="tabs"`)]
	if !strings.Contains(head, `class="st crit`) || !strings.Contains(head, "FAILED") || !strings.Contains(head, ">ACK<") {
		t.Error("the acknowledged device's header no longer reads FAILED with an ACK badge")
	}
	sn := alarmsBlock(t, e.page("/devices/"+e.dev[fixtures.SnoozedHost], 200))
	mustContain(t, "snoozed device", sn, ">SNOOZED<", "<b>Snoozed</b>", "until Wed 21 Oct", "laptop is at the customer")
	// a healthy device has no block at all
	if strings.Contains(e.page("/devices/"+e.dev["LEGAL-PC-01"], 200), `id="alarms"`) {
		t.Error("a healthy device shows an Open alarms block")
	}
}

func TestP20W3B_AcknowledgeSnoozePresetsAndClear(t *testing.T) {
	e := newM4Env(t)
	e.login()
	id := e.dev[fixtures.SnoozedHost]
	loc := e.loc
	now := e.now.In(loc)
	tomorrow8 := time.Date(now.Year(), now.Month(), now.Day()+1, 8, 0, 0, 0, loc)
	for _, c := range []struct {
		mode, until string
		want        func(a *store.Ack) bool
	}{
		{"ack", "", func(a *store.Ack) bool { return a.SnoozeUntil == nil && !a.UntilNextSuccess }},
		{"1h", "", func(a *store.Ack) bool { return a.SnoozeUntil != nil && a.SnoozeUntil.Equal(e.now.Add(time.Hour)) }},
		{"tomorrow", "", func(a *store.Ack) bool { return a.SnoozeUntil != nil && a.SnoozeUntil.Equal(tomorrow8) }},
		{"3d", "", func(a *store.Ack) bool { return a.SnoozeUntil != nil && a.SnoozeUntil.Equal(e.now.Add(72*time.Hour)) }},
		{"1w", "", func(a *store.Ack) bool { return a.SnoozeUntil != nil && a.SnoozeUntil.Equal(e.now.Add(7*24*time.Hour)) }},
		{"next", "", func(a *store.Ack) bool { return a.UntilNextSuccess }},
		{"until", "2026-10-25T09:30", func(a *store.Ack) bool {
			return a.SnoozeUntil != nil && a.SnoozeUntil.Equal(time.Date(2026, 10, 25, 9, 30, 0, 0, loc))
		}},
	} {
		res, _ := e.post("/ui/acks", url.Values{"subject_type": {"device"}, "subject_id": {id}, "alarm_type": {"stale"}, "mode": {c.mode}, "until": {c.until}, "note": {"checked by phone"}}, false)
		want := "ack=snoozed"
		if c.mode == "ack" {
			want = "ack=acked"
		}
		if res.StatusCode != http.StatusSeeOther || !strings.Contains(e.location(res), want) || !strings.HasPrefix(e.location(res), "/devices/"+id) {
			t.Fatalf("%s: %d %s", c.mode, res.StatusCode, e.location(res))
		}
		a := e.activeAck(id)
		if a == nil || !c.want(a) || a.Note != "checked by phone" {
			t.Errorf("%s: stored %+v", c.mode, a)
		}
	}
	if n := len(e.auditRows("alarm_ack.create", "")); n < 7 {
		t.Errorf("%d alarm_ack.create audit rows for 7 acknowledgements", n)
	}
	mustContain(t, "notice", e.page("/devices/"+id+"?ack=snoozed", 200), "Alarm snoozed.")
	// a date that is not in the future comes back as a plain refusal; nothing changes
	before := e.activeAck(id)
	res, _ := e.post("/ui/acks", url.Values{"subject_type": {"device"}, "subject_id": {id}, "alarm_type": {"stale"}, "mode": {"until"}, "until": {"2020-01-01T08:00"}}, false)
	if !strings.Contains(e.location(res), "ack=err_until") || e.activeAck(id).ID != before.ID {
		t.Errorf("a past snooze: %s", e.location(res))
	}
	// clear
	res, _ = e.post("/ui/acks/"+before.ID+"/clear", url.Values{"subject_type": {"device"}, "subject_id": {id}}, false)
	if res.StatusCode != http.StatusSeeOther || !strings.Contains(e.location(res), "ack=cleared") || e.activeAck(id) != nil {
		t.Fatalf("clear: %d %s", res.StatusCode, e.location(res))
	}
	if len(e.auditRows("alarm_ack.clear", before.ID)) != 1 {
		t.Error("the clear wrote no alarm_ack.clear row")
	}
	// clearing it again changes nothing and writes no second row (the store's ClearAck is idempotent)
	e.mustPost("/ui/acks/"+before.ID+"/clear", url.Values{"subject_type": {"device"}, "subject_id": {id}}, http.StatusSeeOther)
	if len(e.auditRows("alarm_ack.clear", before.ID)) != 1 {
		t.Error("a second clear wrote a second audit row")
	}
	// an acknowledgement that does not exist is a plain message
	res, _ = e.post("/ui/acks/01NOSUCHACK000000000000000/clear", url.Values{"subject_type": {"device"}, "subject_id": {id}}, false)
	if !strings.Contains(e.location(res), "ack=ack_gone") {
		t.Errorf("an unknown acknowledgement: %s", e.location(res))
	}
	block := alarmsBlock(t, e.page("/devices/"+id+"?ack=cleared", 200))
	mustContain(t, "after clear", block, "Acknowledgement cleared.", "Not acknowledged", "Acknowledge…")
}

func TestP20W3B_AnAlarmThatIsNotOpenIsAPlainMessage(t *testing.T) {
	e := newEnv(t) // no evaluator pass: RECEPTION-2's PARTIAL has no episode yet
	e.login()
	id := e.dev["RECEPTION-2"]
	block := alarmsBlock(t, e.page("/devices/"+id, 200))
	// F2: an alarm the console has not recorded keeps its chip and gets one plain line, not a button
	mustContain(t, "not recorded", block, "PARTIAL", "Not recorded yet: it can be acknowledged after the console", "next alarm check. Fixtures mode runs none.")
	if strings.Contains(block, "Acknowledge…") || strings.Contains(block, `action="/ui/acks"`) {
		t.Error("an Acknowledge is offered for an alarm with no open episode: it would answer not open")
	}
	e.svc.Fixtures = false // outside fixtures mode the evaluator runs every minute, and the line says so
	mustContain(t, "production line", alarmsBlock(t, e.page("/devices/"+id, 200)), "within about 1 min.")
	e.svc.Fixtures = true
	// a form posted anyway (an old page) answers with the plain message, not an error page
	res, _ := e.post("/ui/acks", url.Values{"subject_type": {"device"}, "subject_id": {id}, "alarm_type": {"partial"}, "mode": {"ack"}}, false)
	if res.StatusCode != http.StatusSeeOther || !strings.Contains(e.location(res), "ack=not_open") {
		t.Fatalf("not open: %d %s", res.StatusCode, e.location(res))
	}
	body := e.page(e.location(res), 200)
	mustContain(t, "not open page", body, "This alarm is not open.", `class="banner info dense"`)
	// after the evaluator's pass records it, the same alarm is offered and the form works
	e.syncEpisodes()
	block = alarmsBlock(t, e.page("/devices/"+id, 200))
	mustContain(t, "recorded", block, "Acknowledge…", "Snooze until tomorrow 8:00 AM", `type="datetime-local"`)
	res, _ = e.post("/ui/acks", url.Values{"subject_type": {"device"}, "subject_id": {id}, "alarm_type": {"partial"}, "mode": {"ack"}}, false)
	if !strings.Contains(e.location(res), "ack=acked") {
		t.Fatalf("after the sync: %s", e.location(res))
	}
	// another customer's device, or a device that does not exist, is a 404 page before anything else
	if res, _ := e.post("/ui/acks", url.Values{"subject_type": {"device"}, "subject_id": {"01NOSUCHDEVICE0000000000000"}, "alarm_type": {"stale"}, "mode": {"ack"}}, false); res.StatusCode != http.StatusNotFound {
		t.Errorf("an unknown device: %d", res.StatusCode)
	}
}

func TestP20W3B_TheMissingCustomerIsAcknowledgedOnItsPage(t *testing.T) {
	e := newM4Env(t)
	e.login()
	cid := e.customerID(fixtures.MissingCustomer)
	block := alarmsBlock(t, e.page("/customers/"+cid, 200))
	mustContain(t, "missing", block, `<span class="st crit"><i aria-hidden="true"></i>MISSING</span>`, "Acknowledge…", "Snooze until the alarm ends")
	res, _ := e.post("/ui/acks", url.Values{"subject_type": {"customer"}, "subject_id": {cid}, "alarm_type": {"missing_customer"}, "mode": {"3d"}, "note": {"office moved; agents reinstalled Monday"}}, false)
	if res.StatusCode != http.StatusSeeOther || e.location(res) != "/customers/"+cid+"?ack=snoozed#alarms" {
		t.Fatalf("missing ack: %d %s", res.StatusCode, e.location(res))
	}
	body := e.page("/customers/"+cid, 200)
	mustContain(t, "missing acked", alarmsBlock(t, body), ">SNOOZED<", "office moved; agents reinstalled Monday", "MISSING")
	if !strings.Contains(body, "MISSING.") {
		t.Error("the MISSING cause banner went away after the snooze: an acknowledged alarm is still an alarm")
	}
}

func TestP20W3B_ReadOnlySeesAcknowledgementsAndNoButtons(t *testing.T) {
	e := newM4Env(t)
	e.signInAs("viewer", "read_only", nil)
	block := alarmsBlock(t, e.page("/devices/"+e.dev[fixtures.AckedHost], 200))
	mustContain(t, "read-only", block, fixtures.AckNote, "needs the operator role")
	for _, dead := range []string{"Acknowledge…", ">Clear<", "Change…", `action="/ui/acks`} {
		if strings.Contains(block, dead) {
			t.Errorf("a read-only account is offered %q", dead)
		}
	}
	if res, _ := e.post("/ui/acks", url.Values{"subject_type": {"device"}, "subject_id": {e.dev[fixtures.SnoozedHost]}, "alarm_type": {"stale"}, "mode": {"ack"}}, false); res.StatusCode != http.StatusForbidden {
		t.Errorf("a read-only acknowledgement: %d", res.StatusCode)
	}
}

// The leak test of the new pages: path-like and patient-like strings planted where a careless
// renderer would print them (a class word of the delivery log, which the schema leaves free text)
// appear nowhere; the acknowledgement note an operator typed appears only in its own element.
func TestP20W3B_NewPagesLeakNothing(t *testing.T) {
	e := newM4Env(t)
	e.login()
	planted := []string{`C:\Users\jdoe\Charts\MRN-004417.pdf`, "Jane Q. Patient DOB 1961-04-02"}
	if err := e.st.Write(context.Background(), func(tx *store.Tx) error {
		for i, p := range planted {
			if _, err := tx.Exec(`INSERT INTO notification_log (id, dedupe_key, kind, subject_type, channel, result, error_class, attempt, sent_at, created_at, updated_at)
				VALUES (?, ?, 'test', 'console', 'email', 'failed', ?, 1, ?, ?, ?)`, "01JLEAKTEST000000000000000"[:25]+string(rune('A'+i)), "leak:"+string(rune('a'+i)), p, e.now.UnixMilli(), e.now.UnixMilli(), e.now.UnixMilli()); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	typed := `see \\fileserver\scans\intake for Jane Q. Patient`
	id := e.dev[fixtures.SnoozedHost]
	e.mustPost("/ui/acks", url.Values{"subject_type": {"device"}, "subject_id": {id}, "alarm_type": {"stale"}, "mode": {"ack"}, "note": {typed}}, http.StatusSeeOther)
	pages := map[string]string{
		"email":   e.page("/settings/email", 200),
		"preview": e.page("/settings/email?preview=1", 200),
		"design":  e.page("/design/mail", 200),
		"device":  e.page("/devices/"+id, 200),
		"other":   e.page("/devices/"+e.dev[fixtures.AckedHost], 200),
	}
	for name, body := range pages {
		for _, p := range planted {
			if strings.Contains(body, p) || strings.Contains(body, strings.ReplaceAll(p, `\`, `\\`)) {
				t.Errorf("%s shows the planted %q", name, p)
			}
		}
		if name == "email" && !strings.Contains(body, "(withheld)") {
			t.Error("the planted class words are not withheld in the delivery log")
		}
	}
	// the typed note: on its device's page, inside the note element, and nowhere else
	dev := pages["device"]
	want := `<q class="acknote">see \\fileserver\scans\intake for Jane Q. Patient</q>`
	// (twice: the line that shows it, and the "Change…" form that edits it; both inside the alarms block)
	block := alarmsBlock(t, dev)
	if strings.Count(dev, "fileserver") != 2 || strings.Count(block, "fileserver") != 2 || !strings.Contains(block, want) ||
		!strings.Contains(block, ">"+typed+"</textarea>") {
		t.Errorf("the typed note is not shown only in its line and its Change form")
	}
	for name, body := range pages {
		if name != "device" && strings.Contains(body, "fileserver") {
			t.Errorf("%s shows the note typed on another page", name)
		}
	}
	// and never in the audit trail
	for _, ev := range e.auditRows("alarm_ack.create", "") {
		if strings.Contains(string(ev.Details), "fileserver") {
			t.Error("the note was copied to the audit trail")
		}
	}
	if m := regexp.MustCompile(`[A-Za-z]:\\Users`).FindString(pages["email"]); m != "" {
		t.Errorf("the e-mail page shows a path: %q", m)
	}
}

// F1: every outcome of an acknowledgement is shown, also on a page with no open alarm (the alarm just
// cleared, the acknowledgement was the last one): on the device panel and on the customer page.
func TestP20W3B_F1_AckOutcomesShowWithNoOpenAlarm(t *testing.T) {
	e := newM4Env(t)
	e.login()
	pages := map[string]string{"device": "/devices/" + e.dev["LEGAL-PC-01"], "customer": "/customers/" + e.customerID("Pinecrest Legal")}
	for where, base := range pages {
		if strings.Contains(e.page(base, 200), `id="alarms"`) {
			t.Fatalf("%s: a healthy subject shows the alarms block with nothing to say", where)
		}
		for code, n := range ackNotices {
			body := e.page(base+"?ack="+code, 200)
			if !strings.Contains(body, `id="alarms"`) || !strings.Contains(body, n.Title) || !strings.Contains(body, "No open alarm here now.") {
				t.Errorf("%s ?ack=%s: the notice %q is not shown", where, code, n.Title)
			}
		}
	}
}

// F5: "3 days" and "1 week" keep the local time of day across both changes of daylight saving time.
func TestP20W3B_F5_SnoozePresetsKeepTheWallClock(t *testing.T) {
	loc, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatal(err)
	}
	at := func(s string) time.Time {
		tm, err := time.ParseInLocation("2006-01-02 15:04", s, loc)
		if err != nil {
			t.Fatal(err)
		}
		return tm
	}
	for _, c := range []struct {
		mode, now, want string
		hours           float64
	}{
		{"3d", "2026-10-30 10:00", "2026-11-02 10:00", 73},  // across the fall change (1 Nov 2026)
		{"1w", "2026-10-28 10:00", "2026-11-04 10:00", 169}, // the week across it
		{"3d", "2027-03-12 10:00", "2027-03-15 10:00", 71},  // across the spring change (14 Mar 2027)
		{"1w", "2027-03-10 10:00", "2027-03-17 10:00", 167},
		{"tomorrow", "2027-03-13 22:15", "2027-03-14 08:00", 8.75},
		{"1h", "2026-11-01 01:30", "2026-11-01 01:30", 1}, // an hour on is an hour: 1:30 CDT becomes 1:30 CST
	} {
		now := at(c.now)
		got, ok := snoozeEnd(c.mode, now, loc)
		if !ok || got.In(loc).Format("2006-01-02 15:04") != c.want || got.Sub(now).Hours() != c.hours {
			t.Errorf("%s from %s: %s (%.2f h), want %s (%.2f h)", c.mode, c.now, got.In(loc).Format("2006-01-02 15:04 MST"), got.Sub(now).Hours(), c.want, c.hours)
		}
	}
}

// F6: "Change…" opens with the acknowledgement in force: its kind, end and note.
func TestP20W3B_F6_ChangeOpensWithWhatIsInForce(t *testing.T) {
	e := newM4Env(t)
	e.login()
	acked := alarmsBlock(t, e.page("/devices/"+e.dev[fixtures.AckedHost], 200))
	mustContain(t, "plain ack", acked, `value="ack" checked`, `>`+fixtures.AckNote+`</textarea>`, ">Save</button>")
	sn := alarmsBlock(t, e.page("/devices/"+e.dev[fixtures.SnoozedHost], 200))
	// LoadFixtures snoozes LT-OPTOM-04 for 24 hours from the fixture clock: Wed 21 Oct, 1:42 PM in Chicago
	mustContain(t, "dated snooze", sn, `value="until" checked`, `value="2026-10-21T13:42"`, `>laptop is at the customer&#39;s other site</textarea>`)
	// a new acknowledgement opens on "Acknowledge" with an empty note
	e.mustPost("/ui/acks/"+e.activeAck(e.dev[fixtures.SnoozedHost]).ID+"/clear", url.Values{"subject_type": {"device"}, "subject_id": {e.dev[fixtures.SnoozedHost]}}, http.StatusSeeOther)
	fresh := alarmsBlock(t, e.page("/devices/"+e.dev[fixtures.SnoozedHost], 200))
	mustContain(t, "new", fresh, `value="ack" checked`, `maxlength="500"></textarea>`, ">Acknowledge</button>")
}

// G3: snooze ends on a day where the local time does not exist or exists twice, in a zone that moves at
// 02:00 (America/Chicago) and one that moves at 01:00 (Europe/London): the end falls on the right day,
// never more than an hour from the local time asked for, and within an hour of N days on. Starts on an
// ambiguous local time (the second 01:30 of a fall-back night) too.
func TestP20W3B_G3_SnoozeEndsOnTransitionDays(t *testing.T) {
	for _, z := range []struct {
		zone                       string
		springNow, springEnd       string // N days on lands in the skipped hour
		fallNow, fallEnd           string // N days on lands in the repeated hour
		ambiguousStart, ambEndDate string // the second occurrence of an ambiguous time, plus a week
	}{
		{"America/Chicago", "2027-03-11 02:30", "2027-03-14", "2026-10-29 01:30", "2026-11-01", "2026-11-01 01:30", "2026-11-08"},
		{"Europe/London", "2027-03-25 01:30", "2027-03-28", "2026-10-22 01:30", "2026-10-25", "2026-10-25 01:30", "2026-11-01"},
	} {
		loc, err := time.LoadLocation(z.zone)
		if err != nil {
			t.Fatal(err)
		}
		parse := func(s string) time.Time {
			tm, err := time.ParseInLocation("2006-01-02 15:04", s, loc)
			if err != nil {
				t.Fatal(err)
			}
			return tm
		}
		check := func(name string, now time.Time, mode, wantDate string, days int) {
			got, ok := snoozeEnd(mode, now, loc)
			if !ok {
				t.Fatalf("%s %s: no end", z.zone, name)
			}
			l, n := got.In(loc), now.In(loc)
			// the local clock asked for, compared as wall time (both read as UTC fields)
			want := time.Date(n.Year(), n.Month(), n.Day()+days, n.Hour(), n.Minute(), 0, 0, time.UTC)
			wall := time.Date(l.Year(), l.Month(), l.Day(), l.Hour(), l.Minute(), 0, 0, time.UTC)
			if d := wall.Sub(want); d < -time.Hour || d > time.Hour {
				t.Errorf("%s %s: ends %s local, %v from the %s asked for", z.zone, name, l.Format("2006-01-02 15:04 MST"), d, want.Format("2006-01-02 15:04"))
			}
			if el := got.Sub(now); el < time.Duration(days)*24*time.Hour-time.Hour || el > time.Duration(days)*24*time.Hour+time.Hour {
				t.Errorf("%s %s: %v after the start, want %d days within an hour", z.zone, name, el, days)
			}
			if l.Format("2006-01-02") != wantDate && wall.Sub(want).Abs() < time.Hour {
				t.Errorf("%s %s: ends on %s, want %s", z.zone, name, l.Format("2006-01-02"), wantDate)
			}
		}
		check("3 days into the skipped hour", parse(z.springNow), "3d", z.springEnd, 3)
		check("3 days into the repeated hour", parse(z.fallNow), "3d", z.fallEnd, 3)
		// ParseInLocation picks one of the two occurrences, which one is not specified: find both
		first, clock := parse(z.ambiguousStart), parse(z.ambiguousStart).In(loc).Format("15:04")
		second := first.Add(time.Hour)
		if second.In(loc).Format("15:04") != clock {
			second, first = first, first.Add(-time.Hour)
		}
		if first.In(loc).Format("15:04") != clock || second.In(loc).Format("15:04") != clock {
			t.Fatalf("%s: %s is not ambiguous", z.zone, z.ambiguousStart)
		}
		check("a week from the second 01:30", second, "1w", z.ambEndDate, 7)
		check("a week from the first 01:30", first, "1w", z.ambEndDate, 7)
	}
}

// G5: when both reads behind the alarms block fail, both are reported: the open-episodes failure does not
// overwrite the acknowledgements failure. (Both read alarm_episodes; renaming it fails them together.)
func TestP20W3B_G5_BothReadFailuresAreReported(t *testing.T) {
	e := newM4Env(t)
	e.login()
	if err := e.st.Write(context.Background(), func(tx *store.Tx) error {
		_, err := tx.Exec(`ALTER TABLE alarm_episodes RENAME TO alarm_episodes_gone`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	res, body := e.do("GET", "/devices/"+e.dev[fixtures.AckedHost], nil)
	if res.StatusCode != 200 {
		t.Skipf("the device page itself needs the episodes (%d): the two failures cannot be seen apart here", res.StatusCode)
	}
	block := alarmsBlock(t, body)
	mustContain(t, "both", block, "Not everything about these alarms could be read.", "The acknowledgements could not be read", "and the open alarms could not be checked")
}

// H2: a snooze end in the hour the clocks repeat says which of the two it is: the zone abbreviation is
// shown with the end, in the acknowledgement line and in the engine's SNOOZED badge.
func TestP20W3B_H2_ASnoozeEndShowsItsZone(t *testing.T) {
	e := newM4Env(t)
	e.login()
	id := e.dev[fixtures.SnoozedHost]
	for _, c := range []struct {
		utc  time.Time
		zone string
	}{{time.Date(2026, 11, 1, 6, 30, 0, 0, time.UTC), "CDT"}, {time.Date(2026, 11, 1, 7, 30, 0, 0, time.UTC), "CST"}} {
		u := c.utc
		if _, err := e.st.CreateAck(context.Background(), store.ScopeAll(), store.AckInput{SubjectType: store.EpisodeDevice, SubjectID: id, AlarmType: "stale", SnoozeUntil: &u},
			store.AuditActor{AdminID: e.adminID("admin"), Username: "admin"}); err != nil {
			t.Fatal(err)
		}
		body := e.page("/devices/"+id, 200)
		mustContain(t, c.zone, alarmsBlock(t, body), "until Sun 1 Nov, 1:30 AM "+c.zone)
		mustContain(t, c.zone+" badge", body, "SNOOZED until Sun 1:30 AM "+c.zone)
	}
}

// H3: when the open episodes cannot be read, no row says "Not recorded yet" (that was not read either):
// each says it could not be checked, and no acknowledge form is offered.
func TestP20W3B_H3_AnUncheckedAlarmIsNotCalledUnrecorded(t *testing.T) {
	e := newM4Env(t)
	e.login()
	if err := e.st.Write(context.Background(), func(tx *store.Tx) error {
		_, err := tx.Exec(`ALTER TABLE alarm_episodes RENAME TO alarm_episodes_gone`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	res, body := e.do("GET", "/devices/"+e.dev["RECEPTION-2"], nil)
	if res.StatusCode != 200 {
		t.Fatalf("device page: %d", res.StatusCode)
	}
	block := alarmsBlock(t, body)
	mustContain(t, "unchecked", block, "Could not be checked: reload the page.")
	if strings.Contains(block, "Not recorded yet") || strings.Contains(block, `action="/ui/acks`) {
		t.Error("an alarm whose episode could not be read is called unrecorded, or offered a form")
	}
}
