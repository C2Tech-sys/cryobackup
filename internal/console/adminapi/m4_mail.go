package adminapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/httpx"
	"github.com/c2tech-sys/cryobackup/internal/console/mail"
	"github.com/c2tech-sys/cryobackup/internal/console/notify"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// The admin API of M4 (ADR 0020, P20-W3A): mail recipients, the delivery log, the SMTP status and
// test mail, acknowledgements and snoozes, and the digest's preview and send-now. This file holds
// the routes that need the mail side of the console (MailService) and the helpers every M4 route
// shares; m4_recipients.go and m4_acks.go hold the rest.
//
// Roles (the matrix of the P20-W3A report):
//
//	GET  /api/v1/notification-rules         AD   recipients are settings: who sees settings sees them
//	POST/PATCH/DELETE notification-rules    OW   who changes settings decides who receives customer and host names
//	GET  /api/v1/notification-log           AD
//	GET  /api/v1/settings/smtp              AD
//	POST /api/v1/settings/smtp/test         AD   ADR 0020 decision 11 names owner and admin
//	GET  /api/v1/digest/preview             AD   the digest names every customer: a console-wide read
//	POST /api/v1/digest/send                OW
//	GET  /api/v1/acks                       RO   an acknowledgement is shown on every device row
//	POST /api/v1/acks, DELETE /acks/{id}    OP   acknowledging an alarm is day-to-day work (architecture §5.2)
//
// The digest's own settings (digest_enabled, digest_local_time) are ordinary settings: GET
// /api/v1/settings and PUT /api/v1/settings/{key} (settings.go) read and write them.
//
// Every console-wide route (recipients, log, SMTP, digest) needs an all-customer caller: a scoped
// account gets 403 (store.ErrScope), because the answer would name other customers.
//
// Nothing here logs an address, a name, a host or a note: log lines carry the route and class words.

// Route patterns.
const (
	PatternRecipients      = "GET /api/v1/notification-rules"
	PatternRecipientCreate = "POST /api/v1/notification-rules"
	PatternRecipientPatch  = "PATCH /api/v1/notification-rules/{id}"
	PatternRecipientDelete = "DELETE /api/v1/notification-rules/{id}"
	PatternDeliveryLog     = "GET /api/v1/notification-log"
	PatternSMTPStatus      = "GET /api/v1/settings/smtp"
	PatternSMTPTest        = "POST /api/v1/settings/smtp/test"
	PatternAcks            = "GET /api/v1/acks"
	PatternAckCreate       = "POST /api/v1/acks"
	PatternAckClear        = "DELETE /api/v1/acks/{id}"
	PatternDigestPreview   = "GET /api/v1/digest/preview"
	PatternDigestSend      = "POST /api/v1/digest/send"
)

// m4Patterns lists the patterns registerM4 adds.
func m4Patterns() []string {
	return []string{PatternRecipients, PatternRecipientCreate, PatternRecipientPatch, PatternRecipientDelete, PatternDeliveryLog,
		PatternSMTPStatus, PatternSMTPTest, PatternAcks, PatternAckCreate, PatternAckClear, PatternDigestPreview, PatternDigestSend}
}

func (s *Service) registerM4(r httpx.Registrar) {
	req := func(min auth.Role, h http.HandlerFunc) http.Handler { return s.Auth.Require(min, auth.ModeAPI, h) }
	r.Handle(PatternRecipients, req(auth.RoleAdmin, s.listRecipients))
	r.Handle(PatternRecipientCreate, req(auth.RoleOwner, s.createRecipient))
	r.Handle(PatternRecipientPatch, req(auth.RoleOwner, s.patchRecipient))
	r.Handle(PatternRecipientDelete, req(auth.RoleOwner, s.deleteRecipient))
	r.Handle(PatternDeliveryLog, req(auth.RoleAdmin, s.listDeliveries))
	r.Handle(PatternSMTPStatus, req(auth.RoleAdmin, s.smtpStatus))
	r.Handle(PatternSMTPTest, req(auth.RoleAdmin, s.smtpTest))
	r.Handle(PatternAcks, req(auth.RoleReadOnly, s.listAcks))
	r.Handle(PatternAckCreate, req(auth.RoleOperator, s.createAck))
	r.Handle(PatternAckClear, req(auth.RoleOperator, s.clearAck))
	r.Handle(PatternDigestPreview, req(auth.RoleAdmin, s.digestPreview))
	r.Handle(PatternDigestSend, req(auth.RoleOwner, s.digestSend))
}

// DigestService is what the digest routes need of notify.Digester.
type DigestService interface {
	PreviewDigest(ctx context.Context) (notify.Content, error)
	SendDigestNow(ctx context.Context, force bool) (notify.DigestResult, error)
}

// MailService is the mail side of the console for the M4 routes. Service.Mail nil answers the
// routes that need it (SMTP status, test mail, digest) with 503: a console wired without it must
// say so, never answer "not configured" for a relay it was never told about.
type MailService struct {
	// Test sends the test message; it carries the console's rate limit and is shared with the
	// e-mail page. Nil answers the test route with 503.
	Test *notify.TestMailer
	// Digest renders and sends the digest (notify.Digester). Nil answers the digest routes with 503.
	Digest DigestService
	// SMTP is the smtp block of config.json, for the read-only status.
	SMTP mail.Settings
	// Configured reports whether mail can be sent: a relay is configured, or fixtures mode.
	Configured bool
	// Fixtures is true in fixtures mode: the sender is a mail.FakeSender and nothing leaves the process.
	Fixtures bool
}

// Fixed texts of the M4 problem answers. They name no host, address or name.
const (
	detailNotConfigured = "Alert e-mail is not set up: the smtp block of the console configuration names no relay."
	detailAlarmNotOpen  = "This alarm is not open."
	detailAddressTaken  = "Another recipient already has this address."
)

// detailTooSoon is the fixed sentence of a test refused by the console's rate limit.
var detailTooSoon = "The console sends one test message every " + strconv.Itoa(int(notify.TestMailInterval/time.Second)) + " seconds."

// m4Problem is a problem answer with a stable machine reason and a fixed sentence beside the code.
type m4Problem struct {
	*api.Error
	Reason string `json:"reason,omitempty"`
	Detail string `json:"detail,omitempty"`
}

func m4Fail(w http.ResponseWriter, r *http.Request, code, reason, detail string, retryAfter time.Duration) {
	e := api.NewError(code)
	e.RequestID = httpx.RequestID(r.Context())
	h := w.Header()
	h.Set("Content-Type", api.ContentTypeProblem)
	h.Set("Cache-Control", "no-store")
	if retryAfter > 0 {
		secs := int((retryAfter + time.Second - 1) / time.Second)
		e.RetryAfterS = secs
		h.Set(api.HeaderRetryAfter, strconv.Itoa(secs))
	}
	w.WriteHeader(e.Status)
	_ = json.NewEncoder(w).Encode(m4Problem{Error: e, Reason: reason, Detail: detail})
}

// errClass is the class word a failed M4 route logs: never the error text, which can carry SQL.
func errClass(err error) string {
	switch {
	case errors.Is(err, store.ErrClosed):
		return "store_closed"
	case errors.Is(err, store.ErrBusy):
		return "store_busy"
	case errors.Is(err, store.ErrCorrupt):
		return "store_corrupt"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	}
	return "failed"
}

// failM4 answers a failed M4 route in the house style: a rejected input lists its fields (400),
// an alarm that is not open is a 409 with its reason, the store's sentinels map as everywhere
// else, and anything unexpected is a 500 whose log line names the route and a class word only.
func (s *Service) failM4(w http.ResponseWriter, r *http.Request, route string, err error) {
	switch {
	case errors.Is(err, store.ErrAlarmNotOpen):
		m4Fail(w, r, api.CodeConflict, "alarm_not_open", detailAlarmNotOpen, 0)
	case len(store.FieldErrors(err)) > 0, errors.Is(err, store.ErrAudit):
		s.failM2(w, r, err)
	case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrScope), errors.Is(err, store.ErrInvalid), errors.Is(err, store.ErrConflict):
		s.fail(w, r, err)
	default:
		s.log().Error("admin api: request failed", logx.Str("route", route), logx.Class(errClass(err)))
		httpx.Problem(w, r, api.CodeInternal, 0)
	}
}

// needAll refuses a console-wide read or write for a caller limited to some customers.
func needAll(w http.ResponseWriter, r *http.Request, p *auth.Principal) bool {
	if !p.Scope.All {
		httpx.Problem(w, r, api.CodeForbidden, 0)
		return false
	}
	return true
}

// mailOr503 returns the MailService, or answers 503 when the console was wired without one.
func (s *Service) mailOr503(w http.ResponseWriter, r *http.Request) *MailService {
	if s.Mail == nil {
		httpx.Problem(w, r, api.CodeUnavailable, 30*time.Second)
		return nil
	}
	return s.Mail
}

// errNotAudited marks a refusal because the audit row could not be written.
var errNotAudited = errors.New("adminapi: not audited")

// audit writes the one audit row of an action that is about to happen, before it happens: a
// refusal to audit refuses the action and nothing is done ("NOT AUDITED"). details are fixed words
// and ids only.
func (s *Service) audit(ctx context.Context, r *http.Request, p *auth.Principal, action, objectType, objectID string, details map[string]any) error {
	a := actorOf(r, p)
	ev := store.AuditEvent{ActorType: "admin", ActorID: a.AdminID, ActorLabel: a.Username, Method: a.Method, SourceIP: a.IP,
		Action: action, ObjectType: objectType, ObjectID: objectID, Result: "ok"}
	if len(details) > 0 {
		b, err := json.Marshal(details)
		if err != nil {
			return err
		}
		ev.Details = b
	}
	if _, err := s.Store.AppendAudit(ctx, ev); err != nil {
		// The log field helper refuses a dot followed by a letter ("smtp.test" would log as
		// "invalid"): the action is logged with underscores, the form of these routes' names.
		s.log().Error("admin api: change refused, not audited", logx.Class("audit"), logx.Str("action", strings.ReplaceAll(action, ".", "_")))
		return errNotAudited
	}
	return nil
}

// notAudited answers a refusal to audit as every route does: a 500 with the house body (the
// data is unchanged and the log line says why).
func notAudited(w http.ResponseWriter, r *http.Request) {
	httpx.Problem(w, r, api.CodeInternal, 0)
}

// ---- SMTP status ----

// SMTPStatusResponse is GET /api/v1/settings/smtp: the mail configuration as the settings page
// shows it. The relay host and the user name are host configuration and are not shown, as the
// storage location is shown by id only; whether each is set is. The password is never read
// into the answer: only whether its file is named and can be read.
type SMTPStatusResponse struct {
	ComputedAt time.Time `json:"computed_at"`
	// Configured is true when mail can be sent (a relay is named, or fixtures mode).
	Configured bool `json:"configured"`
	// Fixtures is true in fixtures mode: messages are recorded in the process and never sent.
	Fixtures       bool   `json:"fixtures"`
	HostSet        bool   `json:"host_set"`
	Security       string `json:"security,omitempty"`
	Port           int    `json:"port,omitempty"`
	AllowPlaintext bool   `json:"allow_plaintext"`
	UsernameSet    bool   `json:"username_set"`
	// PasswordFileSet says smtp.password_file is named; PasswordFileReadable that it can be read as
	// a password file now (false when it is not named).
	PasswordFileSet      bool   `json:"password_file_set"`
	PasswordFileReadable bool   `json:"password_file_readable"`
	From                 string `json:"from,omitempty"`
	// ConfigError is the class word of a configuration that cannot send (mail.Settings.Validate),
	// with its fixed hint; empty when the configuration is usable or not configured at all.
	ConfigError string `json:"config_error,omitempty"`
	ConfigHint  string `json:"config_hint,omitempty"`
}

func (s *Service) smtpStatus(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	if !needAll(w, r, p) {
		return
	}
	m := s.mailOr503(w, r)
	if m == nil {
		return
	}
	set := m.SMTP
	out := SMTPStatusResponse{ComputedAt: s.now(), Configured: m.Configured, Fixtures: m.Fixtures, HostSet: set.Host != "", Security: set.Security,
		Port: set.Port, AllowPlaintext: set.AllowPlaintext, UsernameSet: set.Username != "", PasswordFileSet: set.PasswordFile != "", From: set.From}
	if out.PasswordFileSet {
		_, err := mail.ReadPasswordFile(set.PasswordFile)
		out.PasswordFileReadable = err == nil
	}
	if set.Host != "" {
		var se *mail.SendError
		if err := set.Validate(); errors.As(err, &se) {
			out.ConfigError, out.ConfigHint = string(se.Class), se.Hint()
		}
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// ---- test mail ----

// SMTPTestRequest is the optional body of POST /api/v1/settings/smtp/test. To empty (or no body)
// sends to the caller's own account address.
type SMTPTestRequest struct {
	To string `json:"to"`
}

// SMTPTestRecipientJSON is the outcome for one recipient of a test.
type SMTPTestRecipientJSON struct {
	To      string `json:"to"`
	Outcome string `json:"outcome"` // sent or failed
	// Stage and Class name a failure (mail.Stage, mail.Class words); Hint is the fixed sentence of
	// the class. Never the relay's own reply.
	Stage     string `json:"stage,omitempty"`
	Class     string `json:"class,omitempty"`
	Temporary bool   `json:"temporary,omitempty"`
	Hint      string `json:"hint,omitempty"`
}

// SMTPTestResponse is the answer of a test that was attempted, delivered or not.
type SMTPTestResponse struct {
	ComputedAt time.Time `json:"computed_at"`
	Sent       bool      `json:"sent"`
	// Recorded is false when the outcome could not be written to the delivery log: the test was
	// attempted (and, with Sent, delivered) all the same, and DeliveryID names its queued row.
	Recorded   bool                    `json:"recorded"`
	DeliveryID string                  `json:"delivery_id"`
	Recipients []SMTPTestRecipientJSON `json:"recipients"`
}

// noOwnAddress is the field error of a test without an address by a caller whose account has none.
const noOwnAddress = "Your account has no e-mail address: type the address to send the test to."

func (s *Service) smtpTest(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	var req SMTPTestRequest
	if !decodeBody(w, r, &req, true) {
		return
	}
	m := s.mailOr503(w, r)
	if m == nil {
		return
	}
	if m.Test == nil {
		httpx.Problem(w, r, api.CodeUnavailable, 30*time.Second)
		return
	}
	to, self := req.To, false
	if to == "" {
		to, self = p.Admin.Email, true
		if to == "" {
			s.failM2(w, r, &store.ValidationError{Fields: []store.FieldError{{Field: "to", Message: noOwnAddress}}})
			return
		}
	}
	recipient := "typed"
	if self {
		recipient = "own_account"
	}
	res, err := m.Test.Send(r.Context(), to, func(ctx context.Context) error {
		return s.audit(ctx, r, p, "smtp.test", "smtp", "", map[string]any{"recipient": recipient})
	})
	var soon *notify.TestMailTooSoonError
	recorded := true
	switch {
	case errors.As(err, &soon):
		m4Fail(w, r, api.CodeRateLimited, "test_too_soon", detailTooSoon, soon.RetryAfter)
		return
	case errors.Is(err, notify.ErrTestMailNotConfigured):
		m4Fail(w, r, api.CodeConflict, "smtp_not_configured", detailNotConfigured, 0)
		return
	case errors.Is(err, errNotAudited):
		notAudited(w, r)
		return
	case err != nil && (res.Sent || res.Failure != nil):
		// The relay was asked (and with Sent accepted the message); only recording the outcome
		// failed. A 500 would invite a second test for a message that went out: the answer is the
		// outcome, with recorded false.
		s.log().Error("admin api: test mail outcome not recorded", logx.Str("route", "smtp_test"), logx.Class(errClass(err)))
		recorded = false
	case err != nil:
		s.failM4(w, r, "smtp_test", err)
		return
	}
	one := SMTPTestRecipientJSON{To: res.To, Outcome: "sent"}
	if f := res.Failure; f != nil {
		one = SMTPTestRecipientJSON{To: res.To, Outcome: "failed", Stage: string(f.Stage), Class: string(f.Class), Temporary: f.Temporary, Hint: f.Hint()}
	}
	httpx.WriteJSON(w, http.StatusOK, SMTPTestResponse{ComputedAt: s.now(), Sent: res.Sent, Recorded: recorded, DeliveryID: res.Delivery.ID, Recipients: []SMTPTestRecipientJSON{one}})
}

// ---- digest ----

// DigestPreviewResponse is GET /api/v1/digest/preview: the digest as it would be sent now.
type DigestPreviewResponse struct {
	ComputedAt time.Time `json:"computed_at"`
	Subject    string    `json:"subject"`
	Text       string    `json:"text"`
	HTML       string    `json:"html"`
}

func (s *Service) digestPreview(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	if !needAll(w, r, p) {
		return
	}
	m := s.mailOr503(w, r)
	if m == nil {
		return
	}
	if m.Digest == nil {
		httpx.Problem(w, r, api.CodeUnavailable, 30*time.Second)
		return
	}
	c, err := m.Digest.PreviewDigest(r.Context())
	if err != nil {
		s.failM4(w, r, "digest_preview", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, DigestPreviewResponse{ComputedAt: s.now(), Subject: c.Subject, Text: c.Text, HTML: c.HTML})
}

// DigestSendRequest is the optional body of POST /api/v1/digest/send.
type DigestSendRequest struct {
	// Force sends again although today's digest went out; such a send does not count as today's.
	Force bool `json:"force"`
}

// DigestSendRecipientJSON is what became of one recipient's digest (notify.Digest* outcome words).
type DigestSendRecipientJSON struct {
	ID      string `json:"id"`
	Name    string `json:"name,omitempty"`
	Outcome string `json:"outcome"`
}

// DigestSendResponse is the answer of POST /api/v1/digest/send.
type DigestSendResponse struct {
	ComputedAt time.Time `json:"computed_at"`
	// Date is the local date of the digest in the display zone.
	Date   string `json:"date,omitempty"`
	Forced bool   `json:"forced"`
	// Skipped says why nothing was attempted (notify.DigestSkipped* words); empty when recipients were looked at.
	Skipped string `json:"skipped,omitempty"`
	// DigestEnabled is the digest_enabled setting after the send: false means the daily digest is
	// switched off and this one was sent once on request. A setting that cannot be read is false
	// too (the daily job does not send then either) and is logged.
	DigestEnabled bool `json:"digest_enabled"`
	// Incomplete is true when the send met an error for one or more recipients after it had attempted
	// at least one (sent or failed above zero). The digester goes on to the others after such an error,
	// and a recipient it met the error on is in neither count: its message may not have been queued,
	// may have gone out without its delivery row being updated, or may have failed unrecorded. The
	// counts are what is known; look at the delivery log before sending again.
	Incomplete bool                      `json:"incomplete,omitempty"`
	Sent       int                       `json:"sent"`
	Failed     int                       `json:"failed"`
	Recipients []DigestSendRecipientJSON `json:"recipients"`
}

func (s *Service) digestSend(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	var req DigestSendRequest
	if !decodeBody(w, r, &req, true) {
		return
	}
	if !needAll(w, r, p) {
		return
	}
	m := s.mailOr503(w, r)
	if m == nil {
		return
	}
	if m.Digest == nil {
		httpx.Problem(w, r, api.CodeUnavailable, 30*time.Second)
		return
	}
	if !m.Configured {
		m4Fail(w, r, api.CodeConflict, "smtp_not_configured", detailNotConfigured, 0)
		return
	}
	if s.audit(r.Context(), r, p, "digest.send", "digest", "", map[string]any{"force": req.Force}) != nil {
		notAudited(w, r)
		return
	}
	res, err := m.Digest.SendDigestNow(r.Context(), req.Force)
	s.committed(r) // delivery rows may exist even when a later recipient failed
	incomplete := false
	switch {
	case err != nil && res.Sent+res.Failed > 0:
		// Something was attempted besides the error: answering 500 would say nothing was sent and invite a
		// second send. Answer what happened, marked incomplete (P20-W3B G2).
		s.log().Error("admin api: digest send incomplete, an outcome not recorded", logx.Str("route", "digest_send"), logx.Class(errClass(err)),
			logx.Count("sent", res.Sent), logx.Count("failed", res.Failed))
		incomplete = true
	case err != nil:
		s.failM4(w, r, "digest_send", err)
		return
	}
	names := map[string]string{}
	if rs, err := s.Store.ListRecipients(r.Context(), p.Scope); err == nil {
		for _, rc := range rs {
			names[rc.ID] = rc.Name
		}
	} // the names are a convenience: the outcome is answered without them
	enabled := false
	if set, err := s.Store.ReadDigestSettings(r.Context(), p.Scope); err != nil {
		s.log().Warn("admin api: digest setting not read", logx.Str("route", "digest_send"), logx.Class(errClass(err)))
	} else {
		enabled = set.Enabled
	}
	out := DigestSendResponse{ComputedAt: s.now(), Date: res.Date, Forced: res.Forced, Skipped: res.Skipped, DigestEnabled: enabled, Incomplete: incomplete, Sent: res.Sent, Failed: res.Failed,
		Recipients: make([]DigestSendRecipientJSON, 0, len(res.Recipients))}
	for _, rc := range res.Recipients {
		out.Recipients = append(out.Recipients, DigestSendRecipientJSON{ID: rc.RuleID, Name: names[rc.RuleID], Outcome: rc.Outcome})
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// pendingOldEnough is the age test of banner C03's "alerts pending and nothing delivered" for the
// smtp row of /system/health (m3_system.go), the same rule as the banner's (internal/state,
// pendingLongEnough): the oldest pending alert must have waited state.SMTPPendingMinAge, and an
// age that is unknown (zero) or garbled (further ahead than state.FutureSkewAllowed) counts as old
// enough, because a check that cannot tell is an alarm, never silence.
func pendingOldEnough(sm *state.SMTPFacts, now time.Time) bool {
	queued, ok := sm.OldestPendingAt.Time()
	if !ok || queued.After(now.Add(state.FutureSkewAllowed)) {
		return true
	}
	return now.Sub(queued) >= state.SMTPPendingMinAge
}
