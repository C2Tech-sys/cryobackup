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
	e := newM4Env(t)
	e.login()
	id := e.dev["RECEPTION-2"] // PARTIAL, and no episode recorded: the evaluator does not run in tests
	block := alarmsBlock(t, e.page("/devices/"+id, 200))
	mustContain(t, "not acked", block, "PARTIAL", "Acknowledge…", "Snooze until tomorrow 8:00 AM", `type="datetime-local"`)
	res, _ := e.post("/ui/acks", url.Values{"subject_type": {"device"}, "subject_id": {id}, "alarm_type": {"partial"}, "mode": {"ack"}}, false)
	if res.StatusCode != http.StatusSeeOther || !strings.Contains(e.location(res), "ack=not_open") {
		t.Fatalf("not open: %d %s", res.StatusCode, e.location(res))
	}
	body := e.page(e.location(res), 200)
	mustContain(t, "not open page", body, "This alarm is not open.", `class="banner info dense"`)
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
	if strings.Count(dev, "fileserver") != 1 || !strings.Contains(dev, want) {
		t.Errorf("the typed note is not shown exactly once in its element")
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
