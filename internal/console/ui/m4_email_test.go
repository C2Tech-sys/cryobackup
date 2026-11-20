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
	"github.com/c2tech-sys/cryobackup/internal/console/mail"
	"github.com/c2tech-sys/cryobackup/internal/console/notify"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// P20-W3B: /settings/email. Every read and write goes through the admin API's handlers in
// process; these tests drive the page over HTTP as a browser would.

// newM4Env is newEnv with the mail world of fixtures mode (fixtures.SeedM4).
func newM4Env(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	if err := fixtures.SeedM4(context.Background(), e.st, e.now); err != nil {
		t.Fatal(err)
	}
	e.syncEpisodes() // as the fixtures app does at start (cli syncFixtureEpisodes)
	return e
}

// syncEpisodes records the open episodes of the fleet as one evaluator pass would (its notifier's
// sync step, mail off), so every alarm the pages show can be acknowledged.
func (e *env) syncEpisodes() {
	e.t.Helper()
	f, err := e.svc.Fleet.Alarms(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := (&notify.Notifier{Store: e.st}).Pass(context.Background(), f.View); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) recipientID(name string) string {
	e.t.Helper()
	rs, err := e.st.ListRecipients(context.Background(), store.ScopeAll())
	if err != nil {
		e.t.Fatal(err)
	}
	for _, r := range rs {
		if r.Name == name {
			return r.ID
		}
	}
	e.t.Fatalf("no recipient %q", name)
	return ""
}

// m4Recipient adds a recipient with every switch on, through the store, and returns its id.
func (e *env) m4Recipient(name, addr string) string {
	e.t.Helper()
	r, err := e.st.CreateRecipient(context.Background(), store.ScopeAll(), store.RecipientInput{Name: name, Address: addr, Alerts: true, Recoveries: true, Digest: true},
		store.AuditActor{AdminID: e.adminID("admin"), Username: "admin"})
	if err != nil {
		e.t.Fatal(err)
	}
	return r.ID
}

func TestP20W3B_EmailPageShowsTheFixtureWorld(t *testing.T) {
	e := newM4Env(t)
	e.login()
	body := e.page("/settings/email", 200)
	mustContain(t, "email page", body,
		// shell and navigation
		`aria-label="Settings sections"`, `href="/settings/email" aria-current="page"`, `<h2>E-mail</h2>`,
		// status block: fixed sentence, fixtures chip, security, last send
		`id="status"`, "FIXTURES", "The console never accepts a password in a form.", "smtp.password_file", "Last send",
		// recipients, long ones included, with their switches
		`id="recipients"`, fixtures.RecipientNightDesk, fixtures.RecipientManager, fixtures.RecipientOnCall, fixtures.RecipientLong, fixtures.RecipientLongAddress,
		`class="swstate on"`, `class="swstate off"`, "Use for test", "Edit…", "+ Add a recipient",
		// test mail, prefilled with the admin's own address
		`id="test"`, `name="to"`, `value="admin@example-msp.internal"`, "Send test mail",
		// digest block
		`id="digest"`, "Send the daily digest", `name="digest_local_time"`, "Preview", "Send now", "Send again…",
		// the delivery log: every outcome and every suppress label
		`id="deliveries"`, ">SENT<", ">FAILED<", "RETRYING · next", ">QUEUED<", ">NOT SENT<",
		"Acknowledged", "Snoozed", "Device parked", "Customer suspended", "Alarm already resolved", "Duplicate", "recipient removed since", "an address typed for a test",
		"recipient_rejected", "auth_rejected", "timeout", "Older")
	for _, bad := range []string{"ZgotmplZ", `type="password"`, "NOT EDITABLE YET"} {
		if strings.Contains(body, bad) {
			t.Errorf("the e-mail page contains %q", bad)
		}
	}
	// The log pages: 34 rows at 25 a page.
	older := regexp.MustCompile(`href="(/settings/email\?page=2#deliveries)"`).FindStringSubmatch(body)
	if older == nil {
		t.Fatal("no Older link")
	}
	p2 := e.page(older[1], 200)
	mustContain(t, "page 2", p2, "Showing entries 26-", "Newer", "Daily digest")
	if strings.Contains(p2, `href="/settings/email?page=3#deliveries"`) {
		t.Error("page 2 offers a third page of a 34-row log")
	}
	// Filters: kind and outcome narrow the log, and say so.
	f := e.page("/settings/email?kind=test&outcome=failed", 200)
	mustContain(t, "filtered", f, "Kind: Test", "Outcome: Failed or retrying", "auth_rejected", "Clear all")
	if strings.Contains(f, "recipient_rejected") {
		t.Error("the filter left an alert row in")
	}
	// A filter nothing matches is a neutral empty with its own words, never an error.
	mustContain(t, "no match", e.page("/settings/email?kind=report", 200), "No messages match these filters")
}

func TestP20W3B_SettingsLinksToTheDigestBlock(t *testing.T) {
	e := newEnv(t)
	e.login()
	body := e.page("/settings", 200)
	mustContain(t, "settings", body, `href="/settings/email#digest"`, "Edit in e-mail settings", `href="/settings/email"`, `aria-label="Settings sections"`)
	if strings.Contains(body, "Becomes effective with milestone </small>") {
		t.Error("a digest row still says it is not editable yet, with no milestone")
	}
}

func TestP20W3B_NoRecipientIsAnAlarmNotAnEmptyTable(t *testing.T) {
	e := newEnv(t) // no recipients: LoadFixtures writes none
	e.login()
	body := e.page("/settings/email", 200)
	mustContain(t, "empty", body, "No recipients", "Alarms are mailed to nobody", `class="panel-state alarm"`, "+ Add a recipient")
	// The log of a console that never sent anything is neutral and says how to fill it.
	mustContain(t, "empty log", body, "Nothing has been sent yet")
}

func TestP20W3B_RecipientsAreAddedEditedAndRemovedThroughTheAPI(t *testing.T) {
	e := newM4Env(t)
	e.login()
	before := e.auditTotal()
	res, _ := e.post("/ui/settings/email/recipients", url.Values{"name": {"Front office"}, "address": {"front@example.test"}, "alerts": {"1"}, "digest": {"1"}}, false)
	if res.StatusCode != http.StatusSeeOther || !strings.Contains(e.location(res), "done=rcpt_added") {
		t.Fatalf("add: %d %s", res.StatusCode, e.location(res))
	}
	id := e.recipientID("Front office")
	rc, _ := e.st.GetRecipient(context.Background(), store.ScopeAll(), id)
	if !rc.Alerts || rc.Recoveries || !rc.Digest || rc.Address != "front@example.test" {
		t.Fatalf("stored %+v", rc)
	}
	mustContain(t, "notice", e.page(e.location(res), 200), "Recipient added.")
	// edit: the switches are what the form says (an unticked box is off)
	res, _ = e.post("/ui/settings/email/recipients/"+id, url.Values{"name": {"Front office"}, "address": {"front@example.test"}, "recoveries": {"1"}}, false)
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("save: %d", res.StatusCode)
	}
	rc, _ = e.st.GetRecipient(context.Background(), store.ScopeAll(), id)
	if rc.Alerts || !rc.Recoveries || rc.Digest {
		t.Fatalf("after save %+v", rc)
	}
	res, _ = e.post("/ui/settings/email/recipients/"+id+"/delete", url.Values{}, false)
	if res.StatusCode != http.StatusSeeOther || !strings.Contains(e.location(res), "rcpt_deleted") {
		t.Fatalf("delete: %d %s", res.StatusCode, e.location(res))
	}
	if got := e.mutAuditActionsSince(before); strings.Join(got, ",") != "recipient.create,recipient.update,recipient.delete" {
		t.Errorf("audit actions %v, want the API's recipient.create, recipient.update, recipient.delete", got)
	}
	// a recipient that is gone is a plain message
	res, _ = e.post("/ui/settings/email/recipients/"+id+"/delete", url.Values{}, false)
	if !strings.Contains(e.location(res), "rcpt_gone") {
		t.Errorf("a second delete: %s", e.location(res))
	}
}

func TestP20W3B_RecipientRefusalsComeBackToTheForm(t *testing.T) {
	e := newM4Env(t)
	e.login()
	res, body := e.post("/ui/settings/email/recipients", url.Values{"name": {""}, "address": {"Night <night@example.test>"}}, false)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad input: %d", res.StatusCode)
	}
	mustContain(t, "bad input", body, `id="add-name-err"`, `id="add-addr-err"`, "without a display name", `class="card addrcpt" open`, `aria-invalid="true"`)
	// the typed address comes back for correction, escaped
	if !strings.Contains(body, `value="Night &lt;night@example.test&gt;"`) {
		t.Error("the typed address is not kept in the form")
	}
	// a duplicate address is the API's 409, on the address field
	res, body = e.post("/ui/settings/email/recipients", url.Values{"name": {"Copy"}, "address": {"NIGHT-DESK@example.test"}}, false)
	if res.StatusCode != http.StatusConflict || !strings.Contains(body, "Another recipient already has this address.") {
		t.Fatalf("duplicate: %d", res.StatusCode)
	}
	// an edit refusal opens that row's form, with the error, and changes nothing
	id := e.recipientID(fixtures.RecipientOnCall)
	res, body = e.post("/ui/settings/email/recipients/"+id, url.Values{"name": {"On-call"}, "address": {"not an address"}}, false)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad edit: %d", res.StatusCode)
	}
	mustContain(t, "bad edit", body, `id="e-`+id+`-addr-err"`, `<details class="rowmenu rcpt-menu" open>`)
	if rc, _ := e.st.GetRecipient(context.Background(), store.ScopeAll(), id); rc.Name != fixtures.RecipientOnCall {
		t.Errorf("a refused edit changed the recipient: %+v", rc)
	}
}

func TestP20W3B_TestMailOutcomesInline(t *testing.T) {
	e := newM4Env(t)
	e.login()
	// prefilled from a recipient row: one click, the address is looked up by id (never in the URL)
	id := e.recipientID(fixtures.RecipientOnCall)
	pre := e.page("/settings/email?to="+id, 200)
	mustContain(t, "prefill", pre, `value="oncall@example.test"`)
	res, body := e.post("/ui/settings/email/test", url.Values{"to": {"desk@example.test"}}, false)
	if res.StatusCode != 200 {
		t.Fatalf("test: %d", res.StatusCode)
	}
	mustContain(t, "sent", body, `class="testres ok"`, ">SENT<", "desk@example.test", "The relay accepted the message.")
	if sent := e.mail.Sent(); len(sent) != 1 || sent[0].Kind != mail.KindTest || sent[0].To[0] != "desk@example.test" {
		t.Fatalf("the FakeSender holds %+v", sent)
	}
	if len(e.auditRows("smtp.test", "")) != 1 {
		t.Error("the test wrote no smtp.test audit row")
	}
	// a second test inside ten seconds: the console's limit, said plainly
	res, body = e.post("/ui/settings/email/test", url.Values{"to": {"desk@example.test"}}, false)
	if res.StatusCode != http.StatusTooManyRequests || !strings.Contains(body, "one test message every 10 seconds") {
		t.Fatalf("too soon: %d", res.StatusCode)
	}
	// a failure names the stage and the class word, with the class's one line of advice; never the relay's text
	e.advance(time.Minute)
	e.mail.FailNext(1, &mail.SendError{Stage: mail.StageAuth, Class: mail.ClassAuthRejected})
	res, body = e.post("/ui/settings/email/test", url.Values{"to": {"desk@example.test"}}, false)
	if res.StatusCode != 200 {
		t.Fatalf("failed test: %d", res.StatusCode)
	}
	mustContain(t, "failed", body, `class="testres crit"`, "FAILED · auth_rejected", "signing in", "What to check:", mail.Hint(mail.ClassAuthRejected))
	// an address that is not one plain address is a field error; nothing is sent
	e.advance(time.Minute)
	n := len(e.mail.Sent())
	res, body = e.post("/ui/settings/email/test", url.Values{"to": {"a@b"}}, false)
	if res.StatusCode != http.StatusBadRequest || !strings.Contains(body, `id="test-to-err"`) || len(e.mail.Sent()) != n {
		t.Fatalf("bad address: %d", res.StatusCode)
	}
}

func TestP20W3B_SMTPNotConfiguredDisablesTheTestWithItsReason(t *testing.T) {
	e := newM4Env(t)
	e.mailSvc.Configured, e.mailSvc.Fixtures, e.mailSvc.Test.Configured = false, false, false
	e.login()
	body := e.page("/settings/email", 200)
	mustContain(t, "not configured", body, "NOT CONFIGURED", `aria-disabled="true" aria-describedby="test-why"`, "E-mail is not set up: the smtp block",
		`aria-describedby="dg-send-why"`, "E-mail is not set up, so the digest cannot be sent.")
	if strings.Contains(body, `<button class="btn primary" type="submit">Send test mail</button>`) || strings.Contains(body, ">Use for test</a>") {
		t.Error("a dead Send button or Use for test link is offered")
	}
	res, body := e.post("/ui/settings/email/test", url.Values{"to": {"desk@example.test"}}, false)
	if res.StatusCode != http.StatusConflict || !strings.Contains(body, "Not sent: e-mail is not set up.") {
		t.Fatalf("POST: %d", res.StatusCode)
	}
	if len(e.mail.Sent()) != 0 {
		t.Error("a message went out without a relay")
	}
}

func TestP20W3B_ConsoleWithoutMailServiceSaysSo(t *testing.T) {
	e := newM4Env(t)
	e.svc.m3.api.Mail = nil // a console wired without it (before P20-W3B the pages' API was)
	e.login()
	body := e.page("/settings/email", 200)
	mustContain(t, "no mail service", body, "started without its mail service", `class="panel-state alarm"`)
	res, body := e.post("/ui/settings/email/test", url.Values{"to": {"desk@example.test"}}, false)
	if res.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, "started without its mail service") {
		t.Fatalf("test without a mail service: %d", res.StatusCode)
	}
}

func TestP20W3B_DigestSettingsPreviewAndSendNow(t *testing.T) {
	e := newM4Env(t)
	e.login()
	before := e.auditTotal()
	res, _ := e.post("/ui/settings/email/digest", url.Values{"digest_enabled": {"1"}, "digest_local_time": {"06:30"}}, false)
	if res.StatusCode != http.StatusSeeOther || !strings.Contains(e.location(res), "digest_saved") {
		t.Fatalf("save: %d %s", res.StatusCode, e.location(res))
	}
	ds, _ := e.st.ReadDigestSettings(context.Background(), store.ScopeAll())
	if !ds.Enabled || ds.LocalTime != "06:30" {
		t.Fatalf("stored %+v", ds)
	}
	if got := e.mutAuditActionsSince(before); len(got) == 0 {
		t.Error("saving the digest settings wrote no audit row")
	}
	body := e.page(e.location(res), 200)
	mustContain(t, "saved", body, "Digest settings saved.", `value="06:30"`, "America/Chicago")
	res, body = e.post("/ui/settings/email/digest", url.Values{"digest_enabled": {"1"}, "digest_local_time": {"25:99"}}, false)
	if res.StatusCode != http.StatusBadRequest || !strings.Contains(body, `id="dg-time-err"`) {
		t.Fatalf("bad time: %d", res.StatusCode)
	}
	// preview: a sandboxed frame (no script, no same-origin) and the text part
	pv := e.page("/settings/email?preview=1", 200)
	mustContain(t, "preview", pv, `id="digest-preview"`, `sandbox=""`, `srcdoc="`, "The plain-text part", "PREVIEW")
	if strings.Contains(pv, "allow-scripts") || strings.Contains(pv, "allow-same-origin") {
		t.Error("the preview frame is not fully sandboxed")
	}
	// send now: today's digest went out at 7:00 in the fixture world, so only a forced send mails
	res, body = e.post("/ui/settings/email/digest/send", url.Values{}, false)
	if res.StatusCode != 200 {
		t.Fatalf("send now: %d", res.StatusCode)
	}
	mustContain(t, "send now", body, "ALREADY SENT TODAY")
	n := len(e.mail.Sent())
	res, body = e.post("/ui/settings/email/digest/send", url.Values{"force": {"1"}}, false)
	if res.StatusCode != 200 || !strings.Contains(body, "Digest sent again.") || len(e.mail.Sent()) != n+2 {
		t.Fatalf("forced: %d, %d messages", res.StatusCode, len(e.mail.Sent())-n)
	}
	// with the digest switched off, a send on request says it was a one-off
	e.mustPost("/ui/settings/email/digest", url.Values{"digest_local_time": {"06:30"}}, http.StatusSeeOther)
	_, body = e.post("/ui/settings/email/digest/send", url.Values{"force": {"1"}}, false)
	mustContain(t, "off note", body, "The daily digest is switched off; this was sent once on request.", ">OFF<")
}

func TestP20W3B_RolesMatchTheAPI(t *testing.T) {
	e := newM4Env(t)
	e.signInAs("admin2", "admin", nil)
	body := e.page("/settings/email", 200)
	mustContain(t, "admin view", body, "Changing recipients needs the owner role", "Sending the digest now needs the owner role.", "Use for test", `<button class="btn primary" type="submit">Send test mail</button>`)
	for _, dead := range []string{"Edit…", "+ Add a recipient", "Save digest settings", ">Send again…<"} {
		if strings.Contains(body, dead) {
			t.Errorf("an admin is offered %q", dead)
		}
	}
	if res, _ := e.post("/ui/settings/email/recipients", url.Values{"name": {"X"}, "address": {"x@example.test"}}, false); res.StatusCode != http.StatusForbidden {
		t.Errorf("an admin adds a recipient: %d", res.StatusCode)
	}
	if res, _ := e.post("/ui/settings/email/test", url.Values{"to": {"x@example.test"}}, false); res.StatusCode != 200 {
		t.Errorf("an admin's test mail: %d", res.StatusCode)
	}
	e.signInAs("op1", "operator", nil)
	if res, _ := e.do("GET", "/settings/email", nil); res.StatusCode != http.StatusForbidden {
		t.Errorf("an operator opens the e-mail page: %d", res.StatusCode)
	}
	// (a scoped account is an operator or read-only one: the schema allows no scoped admin)
}
