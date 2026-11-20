package ui

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminapi"
	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/httpx"
	"github.com/c2tech-sys/cryobackup/internal/console/mail"
	"github.com/c2tech-sys/cryobackup/internal/console/notify"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
)

// The e-mail settings page of M4 (ADR 0020, P20-W3B): /settings/email.
//
// One rule shapes it, the rule of the M3-T4 pages (m3t4.go): no fact is computed here. Every read
// and every write goes through the admin API's own handlers in process (apiGet, apiSend), as the
// signed-in account, so the page and /api/v1 cannot disagree, refuse alike, and write the same
// audit rows: recipient.create, recipient.update, recipient.delete (the store's, in the change's
// transaction), smtp.test and digest.send (the handler's, before the action). The one exception is
// the digest's two settings, which this page saves in one call to store.SetSettingsAudited, the
// store call PUT /api/v1/settings/{key} makes (settings.update, one row per changed key), so that a
// new time and the on/off switch are saved together or not at all.
//
// Roles are the API's matrix: the page is an admin page; recipients, the digest settings and
// send-now are owner writes; the test mail is an admin write. A role that cannot write sees the
// values and the reason, never a dead button.
//
// No password ever enters a form here. The relay, its port and security, the sender address and
// the sign-in are host configuration (config.json and its password file, ADR 0020 decision 2) and
// the page says so in a fixed sentence.

const (
	patEmail           = "GET /settings/email"
	patEmailRcptAdd    = "POST /ui/settings/email/recipients"
	patEmailRcptSave   = "POST /ui/settings/email/recipients/{id}"
	patEmailRcptDelete = "POST /ui/settings/email/recipients/{id}/delete"
	patEmailTest       = "POST /ui/settings/email/test"
	patEmailDigest     = "POST /ui/settings/email/digest"
	patEmailDigestSend = "POST /ui/settings/email/digest/send"
)

// emailPath is the page; its sections carry these anchors (the banners link to them).
const (
	emailPath        = "/settings/email"
	anchorStatus     = "status"
	anchorRecipients = "recipients"
	anchorTest       = "test"
	anchorDigest     = "digest"
	anchorDeliveries = "deliveries"
)

// m4UIPatterns lists the patterns registerM4 adds (m4_acks.go adds the acknowledgement routes).
func m4UIPatterns() []string {
	return append([]string{patEmail, patEmailRcptAdd, patEmailRcptSave, patEmailRcptDelete, patEmailTest, patEmailDigest, patEmailDigestSend}, ackUIPatterns()...)
}

func (s *Service) registerM4(r httpx.Registrar) {
	ad := func(h http.HandlerFunc) http.Handler { return s.Auth.Require(auth.RoleAdmin, auth.ModeUI, h) }
	ow := func(h http.HandlerFunc) http.Handler { return s.Auth.Require(auth.RoleOwner, auth.ModeUI, h) }
	r.Handle(patEmail, ad(s.emailPage))
	r.Handle(patEmailRcptAdd, ow(s.emailRecipientAdd))
	r.Handle(patEmailRcptSave, ow(s.emailRecipientSave))
	r.Handle(patEmailRcptDelete, ow(s.emailRecipientDelete))
	r.Handle(patEmailTest, ad(s.emailTest))
	r.Handle(patEmailDigest, ow(s.emailDigestSave))
	r.Handle(patEmailDigestSend, ow(s.emailDigestSend))
	s.registerAcks(r)
}

// ---- view models ----

// EmailPage is /settings/email.
type EmailPage struct {
	Page
	Notice *Note
	// CanOwn: the owner changes recipients and the digest and sends it now.
	CanOwn bool
	Status EmailStatusView
	Rcpt   RecipientsView
	Test   TestMailView
	Digest DigestView
	Log    DeliveryLogView
}

// EmailStatusView is the status block: is mail set up, and how did the last send go.
type EmailStatusView struct {
	Err        *EmptyView
	Chip       Chip
	Configured bool
	Fixtures   bool
	Facts      []Fact
	Problem    *Note
	Last       *LastSendView
	LastErr    bool
}

// LastSendView is the newest send attempt in the delivery log (sent or failed).
type LastSendView struct {
	Chip  Chip
	When  When
	Kind  string
	Class string
	Hint  string
}

// RecipientsView is the recipients table and its forms.
type RecipientsView struct {
	Err     *EmptyView
	Rows    []RecipientRow
	Empty   *EmptyView
	Counts  string
	Notice  *Note
	CanEdit bool
	Why     string
	CSRF    string
	Add     RecipientForm
	AddOpen bool
}

// RecipientRow is one recipient.
type RecipientRow struct {
	ID, Name, Address          string
	Alerts, Recoveries, Digest bool
	Since                      string
	Form                       RecipientForm
	Open                       bool
	UseHref                    string
}

// RecipientForm is the add form or a row's edit form, with what was typed and what was refused.
type RecipientForm struct {
	Action                     string
	Name, Address              string
	Alerts, Recoveries, Digest bool
	NameErr, AddrErr           string
	Err                        *Note
}

// TestMailView is the "Send test mail" block (ADR 0020 decision 11).
type TestMailView struct {
	Enabled bool
	Why     string
	To      string
	ToErr   string
	Own     string
	Result  *TestResultView
	Note    *Note
	CSRF    string
}

// TestResultView is the answer of one test, shown inline.
type TestResultView struct {
	Sent       bool
	Chip       Chip
	To         string
	Stage      string
	StageLabel string
	Class      string
	Hint       string
	Temporary  bool
}

// DigestView is the digest block: on or off, its time, preview and send-now.
type DigestView struct {
	Err        *EmptyView
	Enabled    bool
	Time       string
	TimeErr    string
	Zone       string
	ZoneNow    string
	CanEdit    bool
	Why        string
	CSRF       string
	Recipients int
	SentToday  string
	LastSent   string
	CanSend    bool
	SendWhy    string
	Result     *DigestSendView
	Preview    *DigestPreviewView
	PreviewErr *Note
	FormErr    *Note
}

// DigestSendView is the answer of a send-now.
type DigestSendView struct {
	Note    Note
	Rows    []DigestSendRow
	OffNote string
}

// DigestSendRow is what became of one recipient's digest.
type DigestSendRow struct {
	Name string
	Chip Chip
}

// DigestPreviewView is the digest as it would go out now. HTML goes into a sandboxed frame.
type DigestPreviewView struct {
	Subject string
	HTML    string
	Text    string
}

// DeliveryLogView is the delivery log, newest first.
type DeliveryLogView struct {
	Err      *EmptyView
	Rows     []DeliveryRow
	Empty    *EmptyView
	Kinds    []Option
	Outcomes []Option
	Chips    []FilterChip
	Clear    string
	Summary  string
	Newer    string
	Older    string
	Zone     string
}

// DeliveryRow is one message of the log.
type DeliveryRow struct {
	ID            string
	When          When
	Kind          string
	Recipient     string
	RecipientNote string
	Chip          Chip
	Attempt       int
	Detail        string
	DetailMono    bool
	DetailHint    string
}

// ---- words ----

var emailNotices = map[string]Note{
	"rcpt_added":      {"ok", "Recipient added.", "It receives what its switches say from the next message on."},
	"rcpt_saved":      {"ok", "Recipient saved.", "The change applies to the next message."},
	"rcpt_deleted":    {"ok", "Recipient removed.", "Its rows stay in the delivery log, marked as removed."},
	"rcpt_gone":       {"info", "That recipient was already removed.", "Nothing was changed."},
	"digest_saved":    {"ok", "Digest settings saved.", "The next digest follows them."},
	"digest_nochange": {"info", "Nothing changed.", "Those digest settings are already in effect, so no audit row was written."},
}

// deliveryKindLabel words a delivery kind; an unknown word shows as it is, never blank.
func deliveryKindLabel(k string) string {
	switch k {
	case store.DeliveryAlert:
		return "Alert"
	case store.DeliveryRecovery:
		return "Recovery"
	case store.DeliveryNag:
		return "Reminder"
	case store.DeliveryDigest:
		return "Daily digest"
	case store.DeliveryReport:
		return "Report"
	case store.DeliveryTest:
		return "Test"
	case store.DeliveryEvent:
		return "Event"
	}
	return k
}

// suppressLabel words why a message was not sent, saying only what the row records. The store keeps
// the schema's reason words (acknowledged, snoozed, paused, deduped, customer_suspended,
// below_threshold); notify writes the precise case of a "deduped" only in its log line, so the row
// cannot tell a duplicate key from a recipient switched off or removed, a digest switched off, a test
// that is never retried or a batch muted mid-pass, and a label read from the console's present state
// would change after the fact. So:
//
//	acknowledged        "Acknowledged"
//	snoozed             "Snoozed"
//	paused              "Device parked" (the schema's word; the notifier never writes it today)
//	customer_suspended  "Customer suspended"
//	below_threshold     "Alarm already resolved" (notify's word for an alert whose alarms all ended first)
//	deduped             "Not sent: duplicate, switched off or removed"
//	""                  "no reason recorded"
//	anything else       the raw word
func suppressLabel(reason string) string {
	switch reason {
	case "acknowledged":
		return "Acknowledged"
	case "snoozed":
		return "Snoozed"
	case "paused":
		return "Device parked"
	case "customer_suspended":
		return "Customer suspended"
	case "below_threshold":
		return "Alarm already resolved"
	case "deduped":
		return "Not sent: duplicate, switched off or removed"
	case "":
		return "no reason recorded"
	}
	return reason
}

// safeMailClass is a class word of a failed send: mail's vocabulary only, else "(withheld)". The
// column is free text in the schema, so a value nobody vetted is never printed.
func safeMailClass(c string) string {
	for _, k := range mail.Classes() {
		if string(k) == c {
			return c
		}
	}
	return withheld
}

// stageLabel words the stage a test failed at.
func stageLabel(st string) string {
	switch mail.Stage(st) {
	case mail.StageConnect:
		return "connecting to the relay"
	case mail.StageTLS:
		return "starting TLS"
	case mail.StageAuth:
		return "signing in"
	case mail.StageSender:
		return "the sender address"
	case mail.StageRecipient:
		return "the recipient address"
	case mail.StageData:
		return "sending the message"
	case mail.StageConfig:
		return "the configuration"
	}
	return st
}

// securityLabel words smtp.security.
func securityLabel(sec string, port int) string {
	var w string
	switch sec {
	case "starttls":
		w = "STARTTLS"
	case "tls":
		w = "Implicit TLS"
	case "plain":
		w = "Plain, unencrypted"
	case "":
		return "not set"
	default:
		w = sec
	}
	if port > 0 {
		w += " · port " + strconv.Itoa(port)
	}
	return w
}

// deliveryChip is the outcome chip of a log row.
func deliveryChip(id, outcome string, next *time.Time, c fmtCtx) Chip {
	ch := Chip{ID: "dl-" + id, Small: true}
	switch outcome {
	case store.DeliverySent:
		ch.Label, ch.Class = "SENT", ClassOK
	case store.DeliveryQueued:
		ch.Label, ch.Class = "QUEUED", ClassInfo
	case store.DeliveryFailed:
		if next != nil {
			ch.Label, ch.Class, ch.Qual, ch.Joiner = "RETRYING", ClassWarn, "next "+next.In(c.loc()).Format("3:04 PM"), " · "
		} else {
			ch.Label, ch.Class = "FAILED", ClassCrit
		}
	case store.DeliverySuppressed:
		ch.Label, ch.Class = "NOT SENT", ClassNeutral
	default:
		ch.Label, ch.Class = strings.ToUpper(outcome), ClassNeutral
	}
	return ch
}

// ---- reading ----

// emailData builds the page from the admin API's answers. Each block that could not be read says
// so in its own place; the rest of the page still renders.
func (s *Service) emailData(r *http.Request, p *auth.Principal) EmailPage {
	f, err := s.Fleet.Fleet(r.Context(), p.Scope)
	var pg Page
	if err != nil {
		pg = s.fallbackPage(r.Context(), p)
		pg.Title, pg.Nav = "E-mail settings", "settings"
	} else {
		pg = s.page(r.Context(), p, f, "E-mail settings", "settings", "/ui/events")
	}
	c := s.fc(r.Context())
	d := EmailPage{Page: pg, CanOwn: p.Role >= auth.RoleOwner}
	q := r.URL.Query()
	if n, ok := emailNotices[q.Get("done")]; ok {
		d.Notice = &n
	}

	// status
	var st adminapi.SMTPStatusResponse
	if err := s.apiGet(r, "/api/v1/settings/smtp", &st); err != nil {
		d.Status.Err = mailFailure("the mail settings", err)
	} else {
		d.Status = s.statusView(st)
	}
	configured := d.Status.Err == nil && d.Status.Configured
	d.Status.Last, d.Status.LastErr = s.lastSend(r, c)

	// digest settings first: the delivery log's labels need to know whether the digest is off
	digestOn, digestTime, setErr := s.digestSettings(r)

	// recipients
	var rs adminapi.RecipientsResponse
	rcptErr := s.apiGet(r, "/api/v1/notification-rules", &rs)
	d.Rcpt = RecipientsView{CanEdit: d.CanOwn, CSRF: p.CSRF, Add: RecipientForm{Action: "/ui/settings/email/recipients", Alerts: true, Recoveries: true, Digest: true}}
	if !d.CanOwn {
		d.Rcpt.Why = "Changing recipients needs the owner role: they decide who receives customer and host names."
	}
	toPrefill := ""
	if rcptErr != nil {
		d.Rcpt.Err = mailFailure("the recipients", rcptErr)
	} else {
		d.Rcpt.Counts = plural(rs.AlertRecipients, "recipient receives", "recipients receive") + " alerts · " + plural(rs.DigestRecipients, "recipient receives", "recipients receive") + " the digest"
		for _, rc := range rs.Recipients {
			row := RecipientRow{ID: rc.ID, Name: rc.Name, Address: rc.Address, Alerts: rc.Alerts, Recoveries: rc.Recoveries, Digest: rc.Digest,
				Since: "added " + c.day(rc.CreatedAt), UseHref: listURL(emailPath, url.Values{"to": {rc.ID}}) + "#" + anchorTest,
				Form: RecipientForm{Action: "/ui/settings/email/recipients/" + rc.ID, Name: rc.Name, Address: rc.Address, Alerts: rc.Alerts, Recoveries: rc.Recoveries, Digest: rc.Digest}}
			if q.Get("to") == rc.ID {
				toPrefill = rc.Address
			}
			d.Rcpt.Rows = append(d.Rcpt.Rows, row)
		}
		if len(rs.Recipients) == 0 {
			kind, body := "partial", "Alarms are mailed to nobody. Add a recipient below; the console mails alerts and the digest to the recipients listed here."
			if configured {
				kind = "alarm"
				body = "Alarms are mailed to nobody: e-mail is set up, but nobody receives it. Add a recipient below."
			}
			if !d.CanOwn {
				body = strings.Replace(body, " Add a recipient below;", " An owner adds recipients here;", 1)
				body = strings.Replace(body, " Add a recipient below.", " An owner adds recipients here.", 1)
			}
			d.Rcpt.Empty = &EmptyView{Kind: kind, Title: "No recipients", Body: body}
		} else if rs.NoAlertRecipient {
			d.Rcpt.Notice = &Note{"warn", "No recipient receives alerts.", "Alarms are mailed to nobody until a recipient has the alerts switch on."}
		}
	}

	// test mail
	d.Test = TestMailView{CSRF: p.CSRF, Own: p.Admin.Email, To: p.Admin.Email, Enabled: true}
	if toPrefill != "" {
		d.Test.To = toPrefill
	}
	switch {
	case d.Status.Err != nil:
		d.Test.Enabled, d.Test.Why = false, "The mail settings could not be read, so the console cannot tell whether a test can be sent."
	case !configured:
		d.Test.Enabled, d.Test.Why = false, "E-mail is not set up: the smtp block of the console configuration names no relay."
	}

	if !d.Test.Enabled { // no "Use for test" beside a recipient when the test cannot be sent: no dead links
		for i := range d.Rcpt.Rows {
			d.Rcpt.Rows[i].UseHref = ""
		}
	}

	// digest
	d.Digest = DigestView{CanEdit: d.CanOwn, CSRF: p.CSRF, Zone: s.zone(r.Context()), ZoneNow: zoneNow(s.now(), s.loc(r.Context()))}
	if setErr != nil {
		d.Digest.Err = mailFailure("the digest settings", setErr)
	} else {
		d.Digest.Enabled, d.Digest.Time = digestOn, digestTime
	}
	if !d.CanOwn {
		d.Digest.Why = "Changing the digest and sending it now need the owner role."
	}
	d.Digest.Recipients = rs.DigestRecipients
	d.Digest.CanSend = d.CanOwn
	switch {
	case !d.CanOwn:
		d.Digest.SendWhy = "Sending the digest now needs the owner role."
	case d.Status.Err == nil && !configured:
		d.Digest.CanSend, d.Digest.SendWhy = false, "E-mail is not set up, so the digest cannot be sent."
	case rcptErr == nil && rs.DigestRecipients == 0:
		d.Digest.CanSend, d.Digest.SendWhy = false, "No recipient has the digest switch on, so there is nobody to send it to."
	}
	if t, ok := s.lastDigest(r); ok {
		d.Digest.LastSent = c.dayClock(t)
		if t.In(c.loc()).Format("2006-01-02") == c.Now.In(c.loc()).Format("2006-01-02") {
			d.Digest.SentToday = c.Now.In(c.loc()).Format("Mon 2 Jan") + " at " + t.In(c.loc()).Format("3:04 PM")
		}
	}
	if q.Get("preview") == "1" {
		var pv adminapi.DigestPreviewResponse
		if err := s.apiGet(r, "/api/v1/digest/preview", &pv); err != nil {
			d.Digest.PreviewErr = &Note{"crit", "The preview could not be rendered.", previewWhy(err)}
		} else {
			d.Digest.Preview = &DigestPreviewView{Subject: pv.Subject, HTML: pv.HTML, Text: pv.Text}
		}
	}

	// delivery log
	d.Log = s.deliveryLog(r, c)
	return d
}

// mailFailure is the panel of a block the admin API could not answer: an alarm, never an empty block.
func mailFailure(what string, err error) *EmptyView {
	v := apiFailure(what, err)
	if apiStatus(err) == http.StatusServiceUnavailable {
		v.Body = "This console was started without its mail service, so it cannot answer for " + what + ". This is an alarm: restart the console, then check its log."
	}
	return v
}

func previewWhy(err error) string {
	if apiStatus(err) == http.StatusServiceUnavailable {
		return "This console was started without its mail service. Restart the console, then check its log."
	}
	return "The console could not render the digest from the fleet as it is now. Reload the page to try again; if it persists, check the console log."
}

func (s *Service) statusView(st adminapi.SMTPStatusResponse) EmailStatusView {
	v := EmailStatusView{Configured: st.Configured, Fixtures: st.Fixtures}
	switch {
	case st.Fixtures:
		v.Chip = Chip{ID: "smtp-st", Label: "FIXTURES", Class: ClassInfo, Title: "Fixtures mode", Reason: "Messages are recorded in the console process and never sent."}
	case st.ConfigError != "":
		v.Chip = Chip{ID: "smtp-st", Label: "CANNOT SEND", Class: ClassCrit}
		v.Problem = &Note{"crit", "The smtp settings cannot send (" + safeMailClass(st.ConfigError) + ").", st.ConfigHint}
	case st.Configured:
		v.Chip = Chip{ID: "smtp-st", Label: "CONFIGURED", Class: ClassOK}
	default:
		v.Chip = Chip{ID: "smtp-st", Label: "NOT CONFIGURED", Class: ClassWarn}
	}
	if st.Fixtures {
		v.Facts = append(v.Facts, Fact{Key: "Relay", Value: "none: fixtures mode records each message in the process"})
	} else if st.HostSet {
		v.Facts = append(v.Facts, Fact{Key: "Relay", Value: "set in config.json"})
	} else {
		v.Facts = append(v.Facts, Fact{Key: "Relay", Value: "not set"})
	}
	if !st.Fixtures {
		v.Facts = append(v.Facts, Fact{Key: "Security", Value: securityLabel(st.Security, st.Port)})
		if st.Security == "plain" && st.AllowPlaintext {
			v.Facts = append(v.Facts, Fact{Key: "Plaintext", Value: "allowed by smtp.allow_plaintext; credentials are never sent over it"})
		}
	}
	from := st.From
	if from == "" {
		from = "not set"
	}
	v.Facts = append(v.Facts, Fact{Key: "From", Value: from, Mono: st.From != ""})
	if !st.Fixtures {
		signin := "no sign-in"
		switch {
		case st.UsernameSet && st.PasswordFileSet && st.PasswordFileReadable:
			signin = "user name set · password file readable"
		case st.UsernameSet && st.PasswordFileSet:
			signin = "user name set · password file NOT readable"
		case st.UsernameSet:
			signin = "user name set · no password file"
		}
		v.Facts = append(v.Facts, Fact{Key: "Sign-in", Value: signin})
	}
	return v
}

// lastSend is the newest attempt that was sent or failed (a queued or suppressed row is not a send).
func (s *Service) lastSend(r *http.Request, c fmtCtx) (*LastSendView, bool) {
	var sent, failed adminapi.DeliveryLogResponse
	if err := s.apiGet(r, "/api/v1/notification-log?limit=1&outcome=sent", &sent); err != nil {
		return nil, true
	}
	if err := s.apiGet(r, "/api/v1/notification-log?limit=1&outcome=failed", &failed); err != nil {
		return nil, true
	}
	var pick *adminapi.DeliveryJSON
	if len(sent.Deliveries) > 0 {
		pick = &sent.Deliveries[0]
	}
	if len(failed.Deliveries) > 0 && (pick == nil || failed.Deliveries[0].At.After(pick.At)) {
		pick = &failed.Deliveries[0]
	}
	if pick == nil {
		return nil, false
	}
	at := pick.At
	v := &LastSendView{Chip: deliveryChip("last", pick.Outcome, pick.NextRetryAt, c), When: s.when(r.Context(), &at), Kind: deliveryKindLabel(pick.Kind)}
	if pick.ErrorClass != "" {
		v.Class = safeMailClass(pick.ErrorClass)
		if v.Class != withheld {
			v.Hint = pick.ErrorHint
		}
	}
	return v, false
}

// lastDigest is the time of the newest digest delivered.
func (s *Service) lastDigest(r *http.Request) (time.Time, bool) {
	var l adminapi.DeliveryLogResponse
	if err := s.apiGet(r, "/api/v1/notification-log?limit=1&kind=digest&outcome=sent", &l); err != nil || len(l.Deliveries) == 0 {
		return time.Time{}, false
	}
	return l.Deliveries[0].At, true
}

// digestSettings reads digest_enabled and digest_local_time as GET /api/v1/settings answers them.
func (s *Service) digestSettings(r *http.Request) (on bool, at string, err error) {
	var sr adminapi.SettingsResponse
	if err := s.apiGet(r, "/api/v1/settings", &sr); err != nil {
		return false, "", err
	}
	on = true
	for _, v := range sr.Settings {
		switch v.Key {
		case store.SettingDigestEnabled:
			if json.Unmarshal(v.Value, &on) != nil {
				return false, "", &apiError{Code: "bad_body"}
			}
		case store.SettingDigestLocalTime:
			_ = json.Unmarshal(v.Value, &at) // null: not set (banner C14b says so)
		}
	}
	return on, at, nil
}

// deliveryPageSize is the delivery log's page.
const deliveryPageSize = 25

var deliveryOutcomeWords = []struct{ v, l string }{{store.DeliverySent, "Sent"}, {store.DeliveryFailed, "Failed or retrying"}, {store.DeliveryQueued, "Queued"}, {store.DeliverySuppressed, "Not sent"}}

func (s *Service) deliveryLog(r *http.Request, c fmtCtx) DeliveryLogView {
	q := r.URL.Query()
	kind, outcome := q.Get("kind"), q.Get("outcome")
	if deliveryKindLabel(kind) == kind {
		kind = "" // not a known kind: no filter (the API would refuse it)
	}
	known := false
	for _, o := range deliveryOutcomeWords {
		known = known || o.v == outcome
	}
	if !known {
		outcome = ""
	}
	page := atoiDefault(q.Get("page"), 1)
	if page < 1 || page > 10000 {
		page = 1
	}
	v := DeliveryLogView{Zone: s.zone(r.Context())}
	v.Kinds = []Option{{Value: "", Label: "Any kind", Selected: kind == ""}}
	for _, k := range []string{store.DeliveryAlert, store.DeliveryRecovery, store.DeliveryDigest, store.DeliveryTest, store.DeliveryNag, store.DeliveryReport, store.DeliveryEvent} {
		v.Kinds = append(v.Kinds, Option{Value: k, Label: deliveryKindLabel(k), Selected: k == kind})
	}
	v.Outcomes = []Option{{Value: "", Label: "Any outcome", Selected: outcome == ""}}
	for _, o := range deliveryOutcomeWords {
		v.Outcomes = append(v.Outcomes, Option{Value: o.v, Label: o.l, Selected: o.v == outcome})
	}
	href := func(k, o string, pg int) string {
		u := url.Values{}
		if k != "" {
			u.Set("kind", k)
		}
		if o != "" {
			u.Set("outcome", o)
		}
		if pg > 1 {
			u.Set("page", strconv.Itoa(pg))
		}
		return listURL(emailPath, u) + "#" + anchorDeliveries
	}
	if kind != "" {
		v.Chips = append(v.Chips, FilterChip{Label: "Kind: " + deliveryKindLabel(kind), Href: href("", outcome, 1)})
	}
	if outcome != "" {
		for _, o := range deliveryOutcomeWords {
			if o.v == outcome {
				v.Chips = append(v.Chips, FilterChip{Label: "Outcome: " + o.l, Href: href(kind, "", 1)})
			}
		}
	}
	v.Clear = href("", "", 1)
	api := url.Values{"limit": {strconv.Itoa(deliveryPageSize)}}
	if page > 1 {
		api.Set("cursor", "o"+strconv.Itoa((page-1)*deliveryPageSize))
	}
	if kind != "" {
		api.Set("kind", kind)
	}
	if outcome != "" {
		api.Set("outcome", outcome)
	}
	var l adminapi.DeliveryLogResponse
	if err := s.apiGet(r, "/api/v1/notification-log?"+api.Encode(), &l); err != nil {
		v.Err = mailFailure("the delivery log", err)
		return v
	}
	for _, x := range l.Deliveries {
		at := x.At
		row := DeliveryRow{ID: x.ID, When: s.when(r.Context(), &at), Kind: deliveryKindLabel(x.Kind), Chip: deliveryChip(x.ID, x.Outcome, x.NextRetryAt, c), Attempt: x.Attempt}
		switch {
		case x.Recipient != "":
			row.Recipient = x.Recipient
		case x.RecipientDeleted:
			row.RecipientNote = "recipient removed since"
		case x.Kind == store.DeliveryTest:
			row.RecipientNote = "an address typed for a test"
		default:
			row.RecipientNote = "no recipient"
		}
		switch x.Outcome {
		case store.DeliveryFailed:
			row.Detail, row.DetailMono = safeMailClass(x.ErrorClass), true
			if row.Detail != withheld {
				row.DetailHint = x.ErrorHint
			}
			if x.ErrorClass == "" {
				row.Detail, row.DetailMono = "no class recorded", false
			}
		case store.DeliverySuppressed:
			row.Detail = suppressLabel(x.SuppressReason)
		case store.DeliveryQueued:
			row.Detail = "waiting to be sent"
		case store.DeliverySent:
			row.Detail = "accepted by the relay"
		}
		v.Rows = append(v.Rows, row)
	}
	from := (page-1)*deliveryPageSize + 1
	switch {
	case len(v.Rows) == 0 && page > 1:
		v.Empty = &EmptyView{Kind: "neutral", Title: "No older entries", Body: "That page is past the end of the delivery log.", Link: &Link{Href: href(kind, outcome, 1), Label: "Newest entries"}}
	case len(v.Rows) == 0 && (kind != "" || outcome != ""):
		v.Empty = &EmptyView{Kind: "neutral", Title: "No messages match these filters", Body: "The delivery log has no message of this kind and outcome.", Link: &Link{Href: v.Clear, Label: "Clear filters"}}
	case len(v.Rows) == 0:
		v.Empty = &EmptyView{Kind: "neutral", Title: "Nothing has been sent yet", Body: "Every message the console sends, or decides not to send, appears here. Send a test message to see the first row."}
	default:
		v.Summary = "Showing entries " + strconv.Itoa(from) + "-" + strconv.Itoa(from+len(v.Rows)-1) + ", newest first"
		if l.Truncated {
			v.Summary += "; older entries follow"
		} else {
			v.Summary += "; the oldest entry is the last row"
		}
	}
	if page > 1 {
		v.Newer = href(kind, outcome, page-1)
	}
	if l.Truncated {
		v.Older = href(kind, outcome, page+1)
	}
	return v
}

// ---- rendering ----

func (s *Service) renderEmail(w http.ResponseWriter, r *http.Request, status int, mutate func(*EmailPage)) {
	d := s.emailData(r, principal(r))
	if mutate != nil {
		mutate(&d)
	}
	s.render(w, r, status, "settings_email", "base", d)
}

func (s *Service) emailPage(w http.ResponseWriter, r *http.Request) {
	if !s.allCustomers(w, r, principal(r)) {
		return
	}
	s.renderEmail(w, r, http.StatusOK, nil)
}

// emailForm reads a small form post; false when it answered.
func (s *Service) emailForm(w http.ResponseWriter, r *http.Request, p *auth.Principal) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if err := r.ParseForm(); err != nil {
		s.errorPage(w, r, p, http.StatusBadRequest, "Form not read", "The form could not be read.")
		return false
	}
	return s.allCustomers(w, r, p)
}

func redirectEmail(w http.ResponseWriter, r *http.Request, done, anchor string) {
	http.Redirect(w, r, listURL(emailPath, url.Values{"done": {done}})+"#"+anchor, http.StatusSeeOther)
}

// fieldErrors reads the errors[] of a 400 answer.
func fieldErrors(body []byte) []store.FieldError {
	var p struct {
		Errors []store.FieldError `json:"errors"`
	}
	_ = json.Unmarshal(body, &p)
	return p.Errors
}

// problemReason reads the reason of an M4 problem answer.
func problemReason(body []byte) (reason string, retryAfter int) {
	var p struct {
		Reason      string `json:"reason"`
		RetryAfterS int    `json:"retry_after_s"`
	}
	_ = json.Unmarshal(body, &p)
	return p.Reason, p.RetryAfterS
}

// ---- recipients ----

func recipientInput(r *http.Request) (RecipientForm, adminapi.RecipientRequest) {
	f := RecipientForm{Name: strings.TrimSpace(r.PostFormValue("name")), Address: strings.TrimSpace(r.PostFormValue("address")),
		Alerts: r.PostFormValue("alerts") == "1", Recoveries: r.PostFormValue("recoveries") == "1", Digest: r.PostFormValue("digest") == "1"}
	name, addr, a, rv, dg := f.Name, f.Address, f.Alerts, f.Recoveries, f.Digest
	return f, adminapi.RecipientRequest{Name: &name, Address: &addr, Alerts: &a, Recoveries: &rv, Digest: &dg}
}

// recipientRefused fills a form with the API's refusal; ok false when the refusal is not one a form shows.
func recipientRefused(f *RecipientForm, status int, body []byte) bool {
	switch status {
	case http.StatusBadRequest:
		for _, fe := range fieldErrors(body) {
			switch fe.Field {
			case "name":
				f.NameErr = fe.Message
			case "address":
				f.AddrErr = fe.Message
			}
		}
		if f.NameErr == "" && f.AddrErr == "" {
			f.Err = &Note{"crit", "Not saved.", "The recipient was not accepted. Check the name and the address."}
		} else {
			f.Err = &Note{"crit", "Not saved.", "Fix the field marked below."}
		}
		return true
	case http.StatusConflict:
		f.AddrErr = "Another recipient already has this address."
		f.Err = &Note{"crit", "Not saved.", "Each address is one recipient: edit the one that has it instead."}
		return true
	}
	return false
}

func (s *Service) emailRecipientAdd(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !s.emailForm(w, r, p) {
		return
	}
	f, body := recipientInput(r)
	f.Action = "/ui/settings/email/recipients"
	status, resp, err := s.apiSend(r, http.MethodPost, "/api/v1/notification-rules", body)
	if s.writeFailed(w, r, p, status, err) {
		return
	}
	switch {
	case status == http.StatusCreated:
		redirectEmail(w, r, "rcpt_added", anchorRecipients)
	case recipientRefused(&f, status, resp):
		s.renderEmail(w, r, status, func(d *EmailPage) { d.Rcpt.Add, d.Rcpt.AddOpen = f, true })
	default:
		s.writeRefused(w, r, p, status, "ui: recipient add refused")
	}
}

func (s *Service) emailRecipientSave(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !s.emailForm(w, r, p) {
		return
	}
	id := r.PathValue("id")
	if !idParam(id) {
		http.NotFound(w, r)
		return
	}
	f, body := recipientInput(r)
	f.Action = "/ui/settings/email/recipients/" + id
	status, resp, err := s.apiSend(r, http.MethodPatch, "/api/v1/notification-rules/"+url.PathEscape(id), body)
	if s.writeFailed(w, r, p, status, err) {
		return
	}
	switch {
	case status == http.StatusOK:
		redirectEmail(w, r, "rcpt_saved", anchorRecipients)
	case status == http.StatusNotFound:
		redirectEmail(w, r, "rcpt_gone", anchorRecipients)
	case recipientRefused(&f, status, resp):
		s.renderEmail(w, r, status, func(d *EmailPage) {
			for i := range d.Rcpt.Rows {
				if d.Rcpt.Rows[i].ID == id {
					d.Rcpt.Rows[i].Form, d.Rcpt.Rows[i].Open = f, true
				}
			}
		})
	default:
		s.writeRefused(w, r, p, status, "ui: recipient save refused")
	}
}

func (s *Service) emailRecipientDelete(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !s.emailForm(w, r, p) {
		return
	}
	id := r.PathValue("id")
	if !idParam(id) {
		http.NotFound(w, r)
		return
	}
	status, _, err := s.apiSend(r, http.MethodDelete, "/api/v1/notification-rules/"+url.PathEscape(id), nil)
	if s.writeFailed(w, r, p, status, err) {
		return
	}
	switch status {
	case http.StatusOK:
		redirectEmail(w, r, "rcpt_deleted", anchorRecipients)
	case http.StatusNotFound:
		redirectEmail(w, r, "rcpt_gone", anchorRecipients)
	default:
		s.writeRefused(w, r, p, status, "ui: recipient delete refused")
	}
}

// writeRefused answers a refusal no form on the page explains (403, an unexpected 4xx): an error
// page that says nothing was changed, never a redirect that reads as success.
func (s *Service) writeRefused(w http.ResponseWriter, r *http.Request, p *auth.Principal, status int, msg string) {
	s.log().Warn(msg, logx.Class("refused"), logx.Int("status", status))
	if status == http.StatusForbidden {
		s.errorPage(w, r, p, http.StatusForbidden, "Not permitted", "This account is not allowed to make that change. Nothing was changed.")
		return
	}
	s.errorPage(w, r, p, http.StatusBadRequest, "Not saved", "The console refused the change, so nothing was changed. Reload the page and try again.")
}

// ---- test mail ----

func (s *Service) emailTest(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !s.emailForm(w, r, p) {
		return
	}
	to := strings.TrimSpace(r.PostFormValue("to"))
	status, resp, err := s.apiSend(r, http.MethodPost, "/api/v1/settings/smtp/test", adminapi.SMTPTestRequest{To: to})
	if err == nil && status == http.StatusServiceUnavailable { // a console wired without its mail service: say so where the button is
		s.renderEmail(w, r, http.StatusServiceUnavailable, func(d *EmailPage) {
			d.Test.To, d.Test.Note = to, &Note{"crit", "Not sent.", "This console was started without its mail service. Restart the console, then check its log."}
		})
		return
	}
	// A 500 from the test route is an audit row that could not be written (nothing was sent) or a store
	// failure before the send: the answer does not say which (the house problem body marks neither), so
	// the page says only what is true of both (G2). A message that went out but could not be recorded is
	// not a 500: it is a 200 with recorded false, shown as SENT with the warning below.
	if err == nil && status == http.StatusInternalServerError {
		s.log().Error("ui: test mail failed in the admin API", logx.Class("render"), logx.Int("status", status))
		s.renderEmail(w, r, http.StatusInternalServerError, func(d *EmailPage) {
			d.Test.To, d.Test.Note = to, &couldNotComplete
		})
		return
	}
	if s.writeFailed(w, r, p, status, err) {
		return
	}
	set := func(fn func(t *TestMailView)) func(*EmailPage) {
		return func(d *EmailPage) {
			d.Test.To = to
			fn(&d.Test)
		}
	}
	switch status {
	case http.StatusOK:
		var tr struct {
			adminapi.SMTPTestResponse
			// Recorded is the field P20-W3D adds (whether the attempt is in the delivery log); absent before it.
			Recorded *bool `json:"recorded"`
		}
		if json.Unmarshal(resp, &tr) != nil || len(tr.Recipients) == 0 {
			s.renderEmail(w, r, http.StatusOK, set(func(t *TestMailView) {
				t.Note = &Note{"warn", "The answer could not be read.", "Look for the newest test row in the delivery log below."}
			}))
			return
		}
		one := tr.Recipients[0]
		res := &TestResultView{Sent: one.Outcome == "sent", To: one.To, Stage: one.Stage, StageLabel: stageLabel(one.Stage), Class: safeMailClass(one.Class), Temporary: one.Temporary}
		if res.Sent {
			res.Chip = Chip{ID: "test-res", Label: "SENT", Class: ClassOK}
		} else {
			res.Chip = Chip{ID: "test-res", Label: "FAILED", Class: ClassCrit, Qual: res.Class, Joiner: " · "}
			if res.Class != withheld {
				res.Hint = one.Hint
			}
		}
		s.renderEmail(w, r, http.StatusOK, set(func(t *TestMailView) {
			t.Result = res
			if tr.Recorded != nil && !*tr.Recorded {
				t.Note = &Note{"warn", "The attempt is not in the delivery log.", "The console could not record it; check the console log and the database."}
			}
		}))
	case http.StatusBadRequest:
		msg := "Use one plain e-mail address such as name@example.com, without a display name."
		for _, fe := range fieldErrors(resp) {
			if fe.Field == "to" {
				msg = fe.Message
			}
		}
		s.renderEmail(w, r, http.StatusBadRequest, set(func(t *TestMailView) { t.ToErr = msg }))
	case http.StatusTooManyRequests:
		_, after := problemReason(resp)
		wait := "a few seconds"
		if after > 0 {
			wait = plural(after, "second", "seconds")
		}
		s.renderEmail(w, r, http.StatusTooManyRequests, set(func(t *TestMailView) {
			t.Note = &Note{"warn", "Not sent: a test went out moments ago.", "The console sends one test message every " + strconv.Itoa(int(notify.TestMailInterval/time.Second)) + " seconds. Try again in " + wait + "."}
		}))
	case http.StatusConflict:
		s.renderEmail(w, r, http.StatusConflict, set(func(t *TestMailView) {
			t.Note = &Note{"warn", "Not sent: e-mail is not set up.", "The smtp block of the console configuration names no relay."}
		}))
	default:
		s.writeRefused(w, r, p, status, "ui: test mail refused")
	}
}

// ---- digest ----

func (s *Service) emailDigestSave(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !s.emailForm(w, r, p) {
		return
	}
	on := r.PostFormValue("digest_enabled") == "1"
	at := strings.TrimSpace(r.PostFormValue("digest_local_time"))
	if len(at) == 8 && strings.HasSuffix(at, ":00") { // a browser's time input may add seconds
		at = at[:5]
	}
	traw, _ := json.Marshal(at)
	inputs := []store.SettingInput{{Key: store.SettingDigestEnabled, Value: json.RawMessage(strconv.FormatBool(on))}, {Key: store.SettingDigestLocalTime, Value: traw}}
	again := func(status int, timeErr string, note *Note) {
		s.renderEmail(w, r, status, func(d *EmailPage) {
			d.Digest.Enabled, d.Digest.Time, d.Digest.TimeErr, d.Digest.FormErr = on, at, timeErr, note
			d.Notice = nil
		})
	}
	if _, err := store.ValidateSetting(store.SettingDigestLocalTime, traw); err != nil {
		msg := "Use a time of day as HH:MM on a 24-hour clock, such as 07:00."
		if fe := store.FieldErrors(err); len(fe) > 0 {
			msg = fe[0].Message
		}
		again(http.StatusBadRequest, msg, &Note{"crit", "Not saved.", "Fix the time marked below."})
		return
	}
	changes, err := s.Store.SetSettingsAudited(r.Context(), p.Scope, inputs, s.actor(r, p))
	switch {
	case err == nil:
	case errors.Is(err, store.ErrAudit):
		s.log().Error("ui: digest settings refused, not audited", logx.Class("audit"))
		again(http.StatusInternalServerError, "", &Note{"crit", "Not saved.", "The change could not be audited, so it was refused. Check the database and try again."})
		return
	case errors.Is(err, store.ErrScope):
		s.errorPage(w, r, p, http.StatusForbidden, "Not permitted", "Changing console settings needs an account that can see every customer.")
		return
	case len(store.FieldErrors(err)) > 0:
		again(http.StatusBadRequest, store.FieldErrors(err)[0].Message, &Note{"crit", "Not saved.", "Fix the value marked below."})
		return
	default:
		s.log().Error("ui: digest settings save failed", logx.Class("store"))
		again(http.StatusServiceUnavailable, "", &Note{"crit", "Not saved.", "The settings could not be written. Try again shortly."})
		return
	}
	s.Display.Apply(changes)
	done := "digest_nochange"
	for _, ch := range changes {
		if !ch.Unchanged {
			done = "digest_saved"
		}
	}
	redirectEmail(w, r, done, anchorDigest)
}

// digestOutcome words one recipient's outcome of a send-now (notify.Digest* words).
func digestOutcome(o string) Chip {
	ch := Chip{ID: "ds-" + o, Small: true}
	switch o {
	case "sent":
		ch.Label, ch.Class = "SENT", ClassOK
	case "failed":
		ch.Label, ch.Class = "FAILED", ClassCrit
	case "already_sent":
		ch.Label, ch.Class = "ALREADY SENT TODAY", ClassNeutral
	case "in_retry":
		ch.Label, ch.Class = "WAITING FOR A RETRY", ClassWarn
	case "gave_up":
		ch.Label, ch.Class = "NOT DELIVERED TODAY", ClassCrit
	case "recipient_gone":
		ch.Label, ch.Class = "RECIPIENT REMOVED", ClassNeutral
	case "deferred":
		ch.Label, ch.Class = "DEFERRED TO THE NEXT RUN", ClassInfo
	default:
		ch.Label, ch.Class = strings.ToUpper(strings.ReplaceAll(o, "_", " ")), ClassNeutral
	}
	return ch
}

func (s *Service) emailDigestSend(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !s.emailForm(w, r, p) {
		return
	}
	force := r.PostFormValue("force") == "1"
	// Who has the digest switch, read before the send (J3): a recipient removed while it runs must not
	// shrink the count of recipients whose outcome is unknown. -1 when it cannot be read.
	before := recipientsBefore(r, s)
	status, resp, err := s.apiSend(r, http.MethodPost, "/api/v1/digest/send", adminapi.DigestSendRequest{Force: force})
	if err == nil && status == http.StatusServiceUnavailable {
		s.renderEmail(w, r, http.StatusServiceUnavailable, func(d *EmailPage) {
			d.Digest.Result = &DigestSendView{Note: Note{"crit", "Nothing was sent.", "This console was started without its mail service. Restart the console, then check its log."}}
		})
		return
	}
	// A 500 from send-now is an audit row that could not be written, or an error before any recipient was
	// attempted (an error met after some recipients were attempted answers 200, incomplete): the answer
	// does not say which (G2).
	if err == nil && status == http.StatusInternalServerError {
		s.log().Error("ui: digest send failed in the admin API", logx.Class("render"), logx.Int("status", status))
		s.renderEmail(w, r, http.StatusInternalServerError, func(d *EmailPage) {
			d.Digest.Result = &DigestSendView{Note: couldNotComplete}
		})
		return
	}
	if s.writeFailed(w, r, p, status, err) {
		return
	}
	switch status {
	case http.StatusOK:
		var ds struct {
			adminapi.DigestSendResponse
			// DigestEnabled is the field P20-W3D adds; absent before it, when the page's own read decides.
			DigestEnabled *bool `json:"digest_enabled"`
		}
		_ = json.Unmarshal(resp, &ds)
		v := &DigestSendView{}
		for _, rc := range ds.Recipients {
			name := rc.Name
			if name == "" {
				name = "(recipient removed)"
			}
			v.Rows = append(v.Rows, DigestSendRow{Name: name, Chip: digestOutcome(rc.Outcome)})
		}
		switch {
		case ds.Incomplete:
			// An error was met for some recipients: say only what is known (H1). The ones the answer does not
			// list may or may not have received it (not queued, sent but not recorded, failed unrecorded).
			v.Note = incompleteNote(ds.Sent, ds.Failed, unlisted(len(ds.Recipients), before))
		case ds.Skipped == "not_configured":
			v.Note = Note{"warn", "Nothing was sent: e-mail is not set up.", "The smtp block of the console configuration names no relay."}
		case ds.Skipped == "no_recipient":
			v.Note = Note{"warn", "Nothing was sent: nobody receives the digest.", "Switch the digest on for a recipient first."}
		case ds.Skipped != "":
			v.Note = Note{"info", "Nothing was sent.", "The console answered " + strings.ReplaceAll(ds.Skipped, "_", " ") + "."}
		case ds.Failed > 0:
			v.Note = Note{"crit", "The digest did not reach everyone.", plural(ds.Sent, "recipient was", "recipients were") + " sent the digest and " + plural(ds.Failed, "send", "sends") + " failed; a temporary failure is retried within 15 minutes. The delivery log below has the class word."}
		case ds.Sent == 0 && !force:
			v.Note = Note{"info", "Today's digest had already gone out.", "Nothing was sent again. To send a second copy anyway, use Send again."}
		case force:
			v.Note = Note{"ok", "Digest sent again.", plural(ds.Sent, "recipient was", "recipients were") + " sent a copy. It does not count as today's digest: the daily one still goes out at its time."}
		default:
			v.Note = Note{"ok", "Digest sent.", plural(ds.Sent, "recipient was", "recipients were") + " sent today's digest. The daily run does not send it again today."}
		}
		s.renderEmail(w, r, http.StatusOK, func(d *EmailPage) {
			off := d.Digest.Err == nil && !d.Digest.Enabled
			if ds.DigestEnabled != nil {
				off = !*ds.DigestEnabled
			}
			if off && ds.Sent > 0 {
				v.OffNote = "The daily digest is switched off; this was sent once on request."
			}
			d.Digest.Result = v
		})
	case http.StatusConflict:
		s.renderEmail(w, r, http.StatusConflict, func(d *EmailPage) {
			d.Digest.Result = &DigestSendView{Note: Note{"warn", "Nothing was sent: e-mail is not set up.", "The smtp block of the console configuration names no relay."}}
		})
	default:
		s.writeRefused(w, r, p, status, "ui: digest send refused")
	}
}

// rcptFormData and ackFormData carry a form and what its template needs beside it (funcs "rcptform", "ackform").
type rcptFormData struct {
	F RecipientForm
	P string
}

type ackFormData struct {
	AckForm
	CSRF string
}

// mailBannerActions are the M4 banners and the part of /settings/email that fixes each, with the
// System health row (adminapi healthRowBanners) that a role without the page is offered instead.
var mailBannerActions = map[string]struct{ anchor, label, owner, row string }{
	"C03":  {anchorStatus, "Open e-mail settings", "", "smtp"},
	"C03b": {anchorStatus, "Open e-mail settings", "", "smtp"},
	"C18":  {anchorRecipients, "See recipients", "Add a recipient", "smtp"}, // only an owner adds one
	"C21":  {anchorDeliveries, "Open the delivery log", "", "smtp"},
	"C14":  {anchorDigest, "Open the digest settings", "", "digest"},
	"C14b": {anchorDigest, "Open the digest settings", "", "digest"},
	"C20":  {anchorRecipients, "See recipients", "Choose digest recipients", "digest"},
}

// couldNotComplete is the note of a test or send-now the admin API answered 500: the answer does not say
// whether anything went out, so the page says only what is true either way (G2).
var couldNotComplete = Note{"crit", "The console could not complete this.", "Check the delivery log before sending again."}

// unlisted is how many recipients with the digest switch an incomplete send-now does not list: the
// ones it met an error on, whose outcome is unknown. -1 when the recipients could not be counted.
func unlisted(listed, digestRecipients int) int {
	if digestRecipients < 0 {
		return -1
	}
	if n := digestRecipients - listed; n > 0 {
		return n
	}
	return 0
}

// incompleteNote is the note of an incomplete send-now (H1): what is known, and nothing more, as an
// alert (J2).
func incompleteNote(sent, failed, unknown int) Note {
	text := ""
	if failed > 0 {
		text = strconv.Itoa(failed) + " could not be sent. "
	}
	switch {
	case unknown > 0:
		text += "For " + strconv.Itoa(unknown) + " more the outcome could not be recorded: they may or may not have received it. "
	case unknown < 0:
		text += "For some recipients the outcome could not be recorded: they may or may not have received it. "
	}
	// Always critical (J2, foreman ruling): the console does not know who received the digest.
	return Note{"crit", "Sent to " + plural(sent, "recipient", "recipients") + ".", text + "Check the delivery log below before sending again."}
}

// recipientsBefore is the number of recipients with the digest switch, read before a send-now so an
// incomplete one can say how many are unaccounted for; -1 when it cannot be read (the note then says
// "some recipients").
func recipientsBefore(r *http.Request, s *Service) int {
	var rs adminapi.RecipientsResponse
	if s.apiGet(r, "/api/v1/notification-rules", &rs) != nil {
		return -1
	}
	return rs.DigestRecipients
}
