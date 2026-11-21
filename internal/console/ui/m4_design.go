package ui

import (
	"context"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminapi"
	"github.com/c2tech-sys/cryobackup/internal/console/mail"
	"github.com/c2tech-sys/cryobackup/internal/console/notify"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// P20-W3B: the M4 section of /design. Every component of /settings/email and of the acknowledge
// control in each of its states, rendered by the same partials the pages use (m4_mail.html), from
// fixed sample values: nothing here reads the store except the digest preview, which renders the
// fixture fleet exactly as the real preview would.

// DesignMail is the "Alert e-mail" section of /design.
type DesignMail struct {
	Statuses   []designMailStatus
	Recipients []designRcpt
	Tests      []designTest
	TestOff    TestMailView
	Digests    []designDigest
	Log        DeliveryLogView
	// Stacked is one row of the log rendered by the live template, laid out as a phone shows it (J4).
	Stacked DeliveryLogView
	Alarms  []designAlarms
	Preview *DigestPreviewView
	// Samples are the alert and the recovery notice of the fixture fleet, as a mail client shows their
	// HTML part (P20-MD); the digest is Preview.
	Samples []designMailSample
}

type designMailSample struct {
	Name, Subject, HTML string
}

type designMailStatus struct {
	Name string
	V    EmailStatusView
}

type designRcpt struct {
	Name string
	V    RecipientsView
}

type designTest struct {
	Name string
	V    TestResultView
}

type designDigest struct {
	Name string
	V    DigestView
}

type designAlarms struct {
	Name string
	V    AlarmsView
}

func (s *Service) designMail(ctx context.Context, now time.Time, fleet state.FleetView) *DesignMail {
	c := s.fc(ctx)
	when := func(d time.Duration) When { t := now.Add(-d); return s.when(ctx, &t) }
	d := &DesignMail{}

	// status
	sent := &LastSendView{Chip: deliveryChip("d-last-ok", store.DeliverySent, nil, c), When: when(26 * time.Minute), Kind: "Alert"}
	failed := &LastSendView{Chip: deliveryChip("d-last-bad", store.DeliveryFailed, nil, c), When: when(3 * time.Hour), Kind: "Daily digest", Class: string(mail.ClassAuthRejected), Hint: mail.Hint(mail.ClassAuthRejected)}
	configured := s.statusView(adminapi.SMTPStatusResponse{Configured: true, HostSet: true, Security: "starttls", Port: 587, UsernameSet: true, PasswordFileSet: true, PasswordFileReadable: true, From: "backups@example.test"})
	configured.Last = sent
	broken := s.statusView(adminapi.SMTPStatusResponse{HostSet: true, Security: "starttls", Port: 587, UsernameSet: true, PasswordFileSet: true, From: "backups@example.test",
		ConfigError: string(mail.ClassPasswordFile), ConfigHint: mail.Hint(mail.ClassPasswordFile)})
	broken.Last = failed
	none := s.statusView(adminapi.SMTPStatusResponse{})
	fix := s.statusView(adminapi.SMTPStatusResponse{Configured: true, Fixtures: true})
	fix.LastErr = true
	d.Statuses = []designMailStatus{{"Configured, last send delivered", configured}, {"Cannot send: the password file is unreadable, the last digest failed", broken},
		{"Not configured, nothing sent yet", none}, {"Fixtures mode, and a delivery log that could not be read", fix}}

	// recipients
	row := func(id, name, addr string, a, r, dg bool) RecipientRow {
		return RecipientRow{ID: id, Name: name, Address: addr, Alerts: a, Recoveries: r, Digest: dg, Since: "added " + c.day(now.AddDate(0, 0, -12)), UseHref: "/design/mail",
			Form: RecipientForm{Action: "/design/mail", Name: name, Address: addr, Alerts: a, Recoveries: r, Digest: dg}}
	}
	one := []RecipientRow{row("d1", "Night desk", "night-desk@example.test", true, true, true)}
	many := append(append([]RecipientRow{}, one...),
		row("d2", "Service manager", "service.manager@example.test", false, false, true),
		row("d3", "On-call rotation", "oncall@example.test", true, false, false),
		row("d4", "Escalations for after-hours coverage, second-line technicians and the partner NOC", "after-hours.escalations.second-line-technicians@noc-partner.example.test", true, true, false))
	add := RecipientForm{Action: "/design/mail", Alerts: true, Recoveries: true, Digest: true}
	edit := row("d5", "Front office", "front office@example", false, true, false)
	edit.Open = true
	edit.Form.AddrErr = "Use one plain e-mail address such as name@example.com, without a display name."
	edit.Form.Err = &Note{"crit", "Not saved.", "Fix the field marked below."}
	d.Recipients = []designRcpt{
		{"Empty, e-mail set up (an alarm: alarms go to nobody)", RecipientsView{CanEdit: true, Add: add, Empty: &EmptyView{Kind: "alarm", Title: "No recipients", Body: "Alarms are mailed to nobody: e-mail is set up, but nobody receives it. Add a recipient below."}}},
		{"One recipient", RecipientsView{CanEdit: true, Add: add, Rows: one, Counts: "1 recipient receives alerts · 1 recipient receives the digest"}},
		{"Many, with a long name and address", RecipientsView{CanEdit: true, Add: add, Rows: many, Counts: "3 recipients receive alerts · 2 recipients receive the digest"}},
		{"An edit refused (the row's form opens with the error); nobody receives alerts", RecipientsView{CanEdit: true, Add: add, Rows: []RecipientRow{edit}, Counts: "0 recipients receive alerts · 0 recipients receive the digest",
			Notice: &Note{"warn", "No recipient receives alerts.", "Alarms are mailed to nobody until a recipient has the alerts switch on."}}},
		{"Read-only (admin): no Edit, no Add, the reason instead", RecipientsView{Rows: one, Counts: "1 recipient receives alerts · 1 recipient receives the digest", Why: "Changing recipients needs the owner role: they decide who receives customer and host names."}},
		{"Could not be read (alarm)", RecipientsView{Err: mailFailure("the recipients", &apiError{Status: 500})}},
	}

	// test results: delivered, then one failure per class word
	d.Tests = []designTest{{"Delivered", TestResultView{Sent: true, Chip: Chip{ID: "dt-ok", Label: "SENT", Class: ClassOK}, To: "desk@example.test"}}}
	stageOf := map[mail.Class]mail.Stage{mail.ClassTimeout: mail.StageConnect, mail.ClassRefused: mail.StageConnect, mail.ClassDNS: mail.StageConnect, mail.ClassNetwork: mail.StageConnect,
		mail.ClassCertInvalid: mail.StageTLS, mail.ClassTLSHandshake: mail.StageTLS, mail.ClassStartTLSMissing: mail.StageTLS, mail.ClassStartTLSRejected: mail.StageTLS,
		mail.ClassAuthRejected: mail.StageAuth, mail.ClassAuthUnsupported: mail.StageAuth, mail.ClassSenderRejected: mail.StageSender, mail.ClassRecipientRejected: mail.StageRecipient,
		mail.ClassMessageRejected: mail.StageData, mail.ClassProtocol: mail.StageData}
	for _, cl := range mail.Classes() {
		st, ok := stageOf[cl]
		if !ok {
			st = mail.StageConfig
		}
		d.Tests = append(d.Tests, designTest{string(cl), TestResultView{Chip: Chip{ID: "dt-" + string(cl), Label: "FAILED", Class: ClassCrit, Qual: string(cl), Joiner: " · "}, To: "desk@example.test",
			Stage: string(st), StageLabel: stageLabel(string(st)), Class: string(cl), Hint: mail.Hint(cl), Temporary: cl == mail.ClassTimeout || cl == mail.ClassNetwork}})
	}
	d.TestOff = TestMailView{To: "admin@example.test", Why: "E-mail is not set up: the smtp block of the console configuration names no relay."}

	// digest
	base := DigestView{Enabled: true, Time: "07:00", Zone: s.zone(ctx), ZoneNow: zoneNow(now, s.loc(ctx)), CanEdit: true, Recipients: 2, LastSent: c.dayClock(now.Add(-6 * time.Hour)), CanSend: true}
	today := base
	today.SentToday = now.In(c.loc()).Format("Mon 2 Jan") + " at 7:00 AM"
	today.Result = &DigestSendView{Note: Note{"info", "Today's digest had already gone out.", "Nothing was sent again. To send a second copy anyway, use Send again."},
		Rows: []DigestSendRow{{"Night desk", digestOutcome("already_sent")}, {"Service manager", digestOutcome("already_sent")}}}
	off := base
	off.Enabled = false
	off.Result = &DigestSendView{Note: Note{"crit", "The digest did not reach everyone.", "1 recipient was sent the digest and 1 send failed; a temporary failure is retried within 15 minutes. The delivery log below has the class word."},
		OffNote: "The daily digest is switched off; this was sent once on request.", Rows: []DigestSendRow{{"Night desk", digestOutcome("sent")}, {"Service manager", digestOutcome("failed")},
			{"(recipient removed)", digestOutcome("recipient_gone")}, {"On-call rotation", digestOutcome("in_retry")}, {"Partner NOC", digestOutcome("gave_up")}, {"Front office", digestOutcome("deferred")}}}
	ro := base
	ro.CanEdit, ro.CanSend, ro.Why, ro.SendWhy = false, false, "Changing the digest and sending it now need the owner role.", "Sending the digest now needs the owner role."
	notime := base
	notime.Time, notime.TimeErr, notime.FormErr = "25:99", "Use a time of day as HH:MM on a 24-hour clock, such as 07:00.", &Note{"crit", "Not saved.", "Fix the time marked below."}
	notime.CanSend, notime.SendWhy = false, "No recipient has the digest switch on, so there is nobody to send it to."
	partly := base
	partly.Result = &DigestSendView{Note: incompleteNote(1, 1, 1), Rows: []DigestSendRow{{"Night desk", digestOutcome("sent")}, {"Service manager", digestOutcome("failed")}}}
	failedAll := base
	failedAll.Result = &DigestSendView{Note: couldNotComplete}
	d.Digests = []designDigest{{"On; today's went out; Send now answered", today}, {"Off; sent once on request, one recipient failed (every outcome)", off},
		{"Incomplete: one sent, one failed, and for one the outcome could not be recorded (only what is known)", partly},
		{"The API answered 500: nothing is claimed either way", failedAll}, {"Read-only (admin)", ro}, {"A time refused; nobody receives it", notime}}
	if ct, err := notify.RenderDigest(fleet, nil, notify.Options{Loc: s.loc(ctx)}); err == nil {
		d.Preview = &DigestPreviewView{Subject: ct.Subject, HTML: mailPreviewHTML(ct.HTML), Text: ct.Text}
	}
	d.Samples = designMailSamples(fleet, notify.Options{Loc: s.loc(ctx), ConsoleURL: "https://console.example.com/"})

	// the delivery log: each outcome and each suppress label, and a word nobody knows
	next := now.Add(4 * time.Minute)
	type dl struct {
		kind, who, note, outcome, reason, class string
		attempt                                 int
		retry                                   bool
		gone                                    bool
	}
	rows := []dl{
		{"alert", "Night desk", "", store.DeliverySent, "", "", 1, false, false},
		{"recovery", "Night desk", "", store.DeliverySent, "", "", 1, false, false},
		{"digest", "Service manager", "", store.DeliveryQueued, "", "", 1, false, false},
		{"alert", "Escalations for after-hours coverage, second-line technicians and the partner NOC", "", store.DeliveryFailed, "", "timeout", 2, true, false},
		{"alert", "", "recipient removed since", store.DeliveryFailed, "", "recipient_rejected", 4, false, true},
		{"test", "", "an address typed for a test", store.DeliveryFailed, "", "auth_rejected", 1, false, false},
	}
	for _, r := range []string{"acknowledged", "snoozed", "paused", "customer_suspended", "below_threshold", "deduped"} {
		rows = append(rows, dl{"alert", "On-call rotation", "", store.DeliverySuppressed, r, "", 1, false, false})
	}
	rows = append(rows, dl{"alert", "", "recipient removed since", store.DeliverySuppressed, "deduped", "", 1, false, true},
		dl{"digest", "Night desk", "", store.DeliverySuppressed, "deduped", "", 1, false, false},
		dl{"event", "Night desk", "", store.DeliverySuppressed, "a_future_word", "", 1, false, false},
		dl{"alert", "Night desk", "", store.DeliverySuppressed, "", "", 1, false, false})
	d.Log = DeliveryLogView{Zone: s.zone(ctx), Summary: "Showing entries 1-18, newest first; the oldest entry is the last row"}
	for i, r := range rows {
		var nx *time.Time
		if r.retry {
			nx = &next
		}
		row := DeliveryRow{ID: "d" + string(rune('a'+i)), When: when(time.Duration(i*37) * time.Minute), Kind: deliveryKindLabel(r.kind), Recipient: r.who, RecipientNote: r.note,
			Chip: deliveryChip("d"+string(rune('a'+i)), r.outcome, nx, c), Attempt: r.attempt}
		switch r.outcome {
		case store.DeliveryFailed:
			row.Detail, row.DetailMono, row.DetailHint = r.class, true, mail.Hint(mail.Class(r.class))
		case store.DeliverySuppressed:
			row.Detail = suppressLabel(r.reason)
		case store.DeliveryQueued:
			row.Detail = "waiting to be sent"
		default:
			row.Detail = "accepted by the relay"
		}
		d.Log.Rows = append(d.Log.Rows, row)
	}
	for _, row := range d.Log.Rows {
		if row.Attempt > 1 && row.Chip.Label == "FAILED" {
			d.Stacked = DeliveryLogView{Zone: d.Log.Zone, Rows: []DeliveryRow{row}}
			break
		}
	}

	// acknowledgements: not acknowledged, acknowledged, snoozed to a date and to the next success, read-only, not open
	form := func(alarm, label string) AckForm {
		return ackForm(store.EpisodeDevice, "sample", alarm, label+" on FRONTDESK-01", c, s.zone(ctx))
	}
	at := now.Add(-40 * time.Minute)
	ack := &AckLine{ID: "a1", Badge: Badge{"ACK", ClassNeutral}, Lead: "Acknowledged", By: "admin", At: s.when(ctx, &at), Until: "until cleared or the alarm ends",
		Note: "Vendor replaces the VSS writer on Thursday; failures are expected until then.", Action: "/design/mail"}
	snooze := &AckLine{ID: "a2", Badge: Badge{"SNOOZED", ClassNeutral}, Lead: "Snoozed", By: "operator.one", At: s.when(ctx, &at), Until: "until " + snoozeEndText(c, now.Add(20*time.Hour)), Action: "/design/mail"}
	nextOK := &AckLine{ID: "a3", Badge: Badge{"SNOOZED", ClassNeutral}, Lead: "Snoozed", By: "an account since deleted", At: s.when(ctx, &at), Until: "until the next successful backup", Action: "/design/mail"}
	failedChip := Chip{ID: "da-f", Label: "FAILED", Class: ClassCrit}
	staleChip := Chip{ID: "da-s", Label: "STALE", Class: ClassCrit}
	partChip := Chip{ID: "da-p", Label: "PARTIAL", Class: ClassWarn}
	missChip := Chip{ID: "da-m", Label: "MISSING", Class: ClassCrit}
	d.Alarms = []designAlarms{
		{"Not acknowledged (critical and alarm level)", AlarmsView{CanAck: true, Rows: []AlarmRow{{Chip: failedChip, Form: form("failed", "FAILED"), Recorded: true}, {Chip: partChip, Form: form("partial", "PARTIAL"), Recorded: true}}}},
		{"Shown before the console recorded it: no Acknowledge yet, one plain line", AlarmsView{CanAck: true, Rows: []AlarmRow{{Chip: staleChip, NotYet: notRecordedText(false)}}}},
		{"Acknowledged, with its note; snoozed to a date; snoozed to the next success", AlarmsView{CanAck: true, Notice: &Note{"ok", "Alarm acknowledged.", "E-mail for this alarm is muted. The alarm stays on the board, marked ACK, until its cause clears."},
			Rows: []AlarmRow{{Chip: failedChip, Ack: ack, Recorded: true, Form: form("failed", "FAILED")}, {Chip: staleChip, Ack: snooze, Recorded: true, Form: form("stale", "STALE")}, {Chip: partChip, Ack: nextOK, Recorded: true, Form: form("partial", "PARTIAL")}}}},
		{"Read-only: the line, no buttons, the reason", AlarmsView{Why: "Acknowledging or snoozing an alarm needs the operator role.", Rows: []AlarmRow{{Chip: missChip, Ack: ack, Recorded: true}}}},
		{"The alarm was not open (a plain message, not an error page)", AlarmsView{CanAck: true, Notice: &Note{"info", "This alarm is not open.", "It may have cleared since the page was loaded, or the console has not recorded it yet. Nothing was changed."},
			Rows: []AlarmRow{{Chip: missChip, Recorded: true, Form: ackForm(store.EpisodeCustomer, "sample-c", "missing_customer", "MISSING on Blue Ridge Accounting", c, s.zone(ctx))}}}},
		{"Both reads failed: both are reported, and no row states what was not read", AlarmsView{CanAck: true, Err: &Note{"crit", "Not everything about these alarms could be read.",
			"The acknowledgements could not be read, so whether these alarms are acknowledged is unknown; and the open alarms could not be checked, so whether they can be acknowledged now is unknown. This is an alarm, not \"none\". Reload the page to try again."},
			Rows: []AlarmRow{{Chip: failedChip, NotYet: uncheckedText}, {Chip: partChip, NotYet: uncheckedText}}}},
	}
	return d
}

// designMailSamples renders the alert and the recovery notice for up to three critical devices of
// the fixture fleet, as the notifier would (P20-MD). A fleet without one renders none.
func designMailSamples(fleet state.FleetView, o notify.Options) []designMailSample {
	var open, closed []store.Episode
	for _, dv := range fleet.Devices {
		if len(open) == 3 || !notify.IsCritical(string(dv.Primary)) {
			continue
		}
		e := store.Episode{ID: dv.ID, SubjectType: store.EpisodeDevice, SubjectID: dv.ID, CustomerID: dv.CustomerID, AlarmType: string(dv.Primary), StateSince: dv.Since}
		open = append(open, e)
		end := fleet.ComputedAt
		e.ClosedAt = &end
		closed = append(closed, e)
	}
	if len(open) == 0 {
		return nil
	}
	var out []designMailSample
	if c, err := notify.RenderAlert(fleet, open, o); err == nil {
		out = append(out, designMailSample{"Alert: the critical devices of the fixture fleet", c.Subject, mailPreviewHTML(c.HTML)})
	}
	if c, err := notify.RenderRecovery(fleet, closed, o); err == nil {
		out = append(out, designMailSample{"Recovery notice: the same alarms, ended", c.Subject, mailPreviewHTML(c.HTML)})
	}
	return out
}
