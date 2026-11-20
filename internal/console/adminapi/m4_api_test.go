package adminapi

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/console/mail"
	"github.com/c2tech-sys/cryobackup/internal/console/notify"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// P20-W3A: the admin API of M4. Every route: its happy path, each refusal, its roles.

// withMail wires the mail side of the M4 routes over the env's store, sending through s.
func (e *env) withMail(s mail.Sender) *MailService {
	e.t.Helper()
	d := &notify.Dispatcher{Store: e.st, Sender: s, From: "CryoBackup <console@example.test>"}
	view := func(ctx context.Context) (state.FleetView, error) {
		f, err := e.svc.Fleet.Alarms(ctx)
		return f.View, err
	}
	m := &MailService{Test: &notify.TestMailer{Dispatch: d, Configured: true}, Configured: true,
		Digest: &notify.Digester{Store: e.st, Dispatch: d, View: view, Configured: true},
		SMTP:   mail.Settings{Host: "relay.example.test", Port: 587, Security: "starttls", Username: "relay-user", From: "console@example.test"}}
	e.svc.Mail = m
	return m
}

// as signs in a new account of role (scope nil: all customers).
func (e *env) as(name, role string, scope []string) *session {
	e.t.Helper()
	e.addAdmin(name, role, scope)
	return e.login(name, pw)
}

func (e *env) wantStatus(method, path string, s *session, body any, want int) []byte {
	e.t.Helper()
	res, b := e.req(method, path, s, body)
	if res.StatusCode != want {
		e.t.Fatalf("%s %s: %d %s (want %d)", method, path, res.StatusCode, b, want)
	}
	return b
}

func m4Reason(t *testing.T, b []byte) (code, reason, detail string) {
	t.Helper()
	var p struct {
		Code   string `json:"code"`
		Reason string `json:"reason"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatalf("problem body %s: %v", b, err)
	}
	return p.Code, p.Reason, p.Detail
}

func fieldOf(t *testing.T, b []byte) []store.FieldError {
	t.Helper()
	var p struct {
		Code   string             `json:"code"`
		Errors []store.FieldError `json:"errors"`
	}
	if err := json.Unmarshal(b, &p); err != nil || p.Code != api.CodeInvalidRequest {
		t.Fatalf("validation body %s: %v", b, err)
	}
	return p.Errors
}

// ---- recipients ----

func TestM4RecipientsLifecycleAndTheLastAlertRecipient(t *testing.T) {
	e := newEnv(t)
	owner := e.login("admin", pw)

	list := decode[RecipientsResponse](t, e.wantStatus("GET", "/api/v1/notification-rules", owner, nil, 200))
	if len(list.Recipients) != 0 || !list.NoAlertRecipient || list.Notice != noAlertRecipientNotice || list.ComputedAt.IsZero() {
		t.Fatalf("empty list %+v", list)
	}

	b := e.wantStatus("POST", "/api/v1/notification-rules", owner, map[string]any{"name": "Night desk", "address": " ops@example.com "}, 201)
	created := decode[RecipientResponse](t, b)
	r1 := created.Recipient
	if r1.Address != "ops@example.com" || !r1.Alerts || !r1.Recoveries || !r1.Digest || created.AlertRecipients != 1 || created.NoAlertRecipient || created.Notice != "" {
		t.Fatalf("create: a switch left out is on: %s", b)
	}
	r2 := decode[RecipientResponse](t, e.wantStatus("POST", "/api/v1/notification-rules", owner,
		map[string]any{"name": "Digest only", "address": "digest@example.com", "alerts": false, "recoveries": false}, 201)).Recipient
	if r2.Alerts || r2.Recoveries || !r2.Digest {
		t.Fatalf("explicit switches: %+v", r2)
	}

	// PATCH keeps what it does not name.
	p := decode[RecipientResponse](t, e.wantStatus("PATCH", "/api/v1/notification-rules/"+r1.ID, owner, map[string]any{"name": "Night desk 2"}, 200))
	if p.Recipient.Name != "Night desk 2" || p.Recipient.Address != "ops@example.com" || !p.Recipient.Alerts || !p.Recipient.Digest {
		t.Fatalf("patch %+v", p.Recipient)
	}

	// Turning off the last alerts switch is allowed and said.
	p = decode[RecipientResponse](t, e.wantStatus("PATCH", "/api/v1/notification-rules/"+r1.ID, owner, map[string]any{"alerts": false}, 200))
	if !p.NoAlertRecipient || p.Notice != noAlertRecipientNotice || p.AlertRecipients != 0 || p.DigestRecipients != 2 {
		t.Fatalf("last alerts switch off: %+v", p)
	}
	e.wantStatus("PATCH", "/api/v1/notification-rules/"+r1.ID, owner, map[string]any{"alerts": true}, 200)

	// Deleting the last recipient with alerts is allowed and the answer says alarms go to nobody.
	d := decode[RecipientDeleteResponse](t, e.wantStatus("DELETE", "/api/v1/notification-rules/"+r1.ID, owner, nil, 200))
	if d.ID != r1.ID || !d.NoAlertRecipient || d.Notice != noAlertRecipientNotice || d.DigestRecipients != 1 {
		t.Fatalf("delete %+v", d)
	}
	list = decode[RecipientsResponse](t, e.wantStatus("GET", "/api/v1/notification-rules", owner, nil, 200))
	if len(list.Recipients) != 1 || list.Recipients[0].ID != r2.ID {
		t.Fatalf("after delete %+v", list)
	}
	if n := e.auditCount("recipient.create", r1.ID) + e.auditCount("recipient.update", r1.ID) + e.auditCount("recipient.delete", r1.ID); n != 5 {
		t.Fatalf("%d audit rows for r1, want 5 (create, three updates, delete)", n)
	}
}

func TestM4RecipientRefusals(t *testing.T) {
	e := newEnv(t)
	owner := e.login("admin", pw)
	bad := "Ops Team <ops@example.com>"
	b := e.wantStatus("POST", "/api/v1/notification-rules", owner, map[string]any{"name": "Ops", "address": bad}, 400)
	fe := fieldOf(t, b)
	if len(fe) != 1 || fe[0].Field != "address" || !strings.Contains(fe[0].Message, "plain e-mail address") || strings.Contains(string(b), "Ops Team") {
		t.Fatalf("a refused address names the rule, never the input: %s", b)
	}
	b = e.wantStatus("POST", "/api/v1/notification-rules", owner, map[string]any{"address": "ops@example.com"}, 400)
	if fe := fieldOf(t, b); len(fe) != 1 || fe[0].Field != "name" {
		t.Fatalf("missing name: %s", b)
	}
	e.wantStatus("POST", "/api/v1/notification-rules", owner, map[string]any{"name": "Ops", "address": "ops@example.com", "extra": 1}, 400)
	r := decode[RecipientResponse](t, e.wantStatus("POST", "/api/v1/notification-rules", owner, map[string]any{"name": "Ops", "address": "ops@example.com"}, 201)).Recipient
	b = e.wantStatus("POST", "/api/v1/notification-rules", owner, map[string]any{"name": "Ops 2", "address": "OPS@example.com"}, 409)
	if code, reason, detail := m4Reason(t, b); code != api.CodeConflict || reason != "address_taken" || detail != detailAddressTaken || strings.Contains(string(b), "OPS@") {
		t.Fatalf("duplicate: %s", b)
	}
	e.wantStatus("PATCH", "/api/v1/notification-rules/01HNOSUCHRULE0000000000000", owner, map[string]any{"name": "x"}, 404)
	e.wantStatus("DELETE", "/api/v1/notification-rules/01HNOSUCHRULE0000000000000", owner, nil, 404)
	b = e.wantStatus("PATCH", "/api/v1/notification-rules/"+r.ID, owner, map[string]any{"address": "a@b"}, 400)
	if fe := fieldOf(t, b); len(fe) != 1 || fe[0].Field != "address" {
		t.Fatalf("patch address: %s", b)
	}

	// Roles: an admin reads, only the owner writes; an operator does neither.
	admin := e.as("w3a-admin", "admin", nil)
	e.wantStatus("GET", "/api/v1/notification-rules", admin, nil, 200)
	e.wantStatus("POST", "/api/v1/notification-rules", admin, map[string]any{"name": "A", "address": "a@example.com"}, 403)
	e.wantStatus("PATCH", "/api/v1/notification-rules/"+r.ID, admin, map[string]any{"name": "A"}, 403)
	e.wantStatus("DELETE", "/api/v1/notification-rules/"+r.ID, admin, nil, 403)
	op := e.as("w3a-op", "operator", nil)
	e.wantStatus("GET", "/api/v1/notification-rules", op, nil, 403)
}

// ---- delivery log ----

func (e *env) seedDelivery(in store.DeliveryInput, fail string) store.Delivery {
	e.t.Helper()
	ctx := context.Background()
	d, _, err := e.st.EnqueueDelivery(ctx, store.ScopeAll(), in)
	if err != nil {
		e.t.Fatal(err)
	}
	if fail != "" {
		if d, err = e.st.MarkDeliveryFailed(ctx, store.ScopeAll(), d.ID, fail, nil); err != nil {
			e.t.Fatal(err)
		}
	} else if in.SuppressReason == "" {
		if d, err = e.st.MarkDeliverySent(ctx, store.ScopeAll(), d.ID); err != nil {
			e.t.Fatal(err)
		}
	}
	e.clk.Advance(time.Second)
	return d
}

func TestM4DeliveryLogFiltersPagesAndNamesTheRecipient(t *testing.T) {
	e := newEnv(t)
	owner := e.login("admin", pw)
	keep := decode[RecipientResponse](t, e.wantStatus("POST", "/api/v1/notification-rules", owner, map[string]any{"name": "Night desk", "address": "ops@example.com"}, 201)).Recipient
	gone := decode[RecipientResponse](t, e.wantStatus("POST", "/api/v1/notification-rules", owner, map[string]any{"name": "Temp", "address": "temp@example.com"}, 201)).Recipient
	a := e.seedDelivery(store.DeliveryInput{Kind: store.DeliveryAlert, RuleID: keep.ID, DedupeKey: "alert:1:" + keep.ID, SubjectType: "console"}, "auth_rejected")
	g := e.seedDelivery(store.DeliveryInput{Kind: store.DeliveryDigest, RuleID: gone.ID, DedupeKey: "digest:2026-11-01:" + gone.ID, SubjectType: "console"}, "")
	s := e.seedDelivery(store.DeliveryInput{Kind: store.DeliveryRecovery, RuleID: keep.ID, DedupeKey: "recovery:2:" + keep.ID, SubjectType: "console", SuppressReason: "acknowledged"}, "")
	tm := e.seedDelivery(store.DeliveryInput{Kind: store.DeliveryTest, DedupeKey: "test:3:x", SubjectType: "console"}, "")
	e.wantStatus("DELETE", "/api/v1/notification-rules/"+gone.ID, owner, nil, 200)

	l := decode[DeliveryLogResponse](t, e.wantStatus("GET", "/api/v1/notification-log", owner, nil, 200))
	if len(l.Deliveries) != 4 || l.Truncated || l.DisplayTimezone == "" {
		t.Fatalf("log %+v", l)
	}
	byID := map[string]DeliveryJSON{}
	for i, d := range l.Deliveries {
		byID[d.ID] = d
		if i > 0 && d.At.After(l.Deliveries[i-1].At) {
			t.Fatal("the log is newest first")
		}
	}
	if l.Deliveries[0].ID != tm.ID {
		t.Fatalf("newest first: %+v", l.Deliveries[0])
	}
	if d := byID[a.ID]; d.Recipient != "Night desk" || d.Outcome != "failed" || d.ErrorClass != "auth_rejected" || d.ErrorHint != mail.Hint(mail.ClassAuthRejected) || d.Attempt != 1 || d.AtLocal == "" {
		t.Fatalf("failed alert row %+v", d)
	}
	if d := byID[g.ID]; d.Recipient != "" || !d.RecipientDeleted || d.Outcome != "sent" {
		t.Fatalf("a deleted recipient's row %+v", d)
	}
	if d := byID[s.ID]; d.Outcome != "suppressed" || d.SuppressReason != "acknowledged" {
		t.Fatalf("suppressed row %+v", d)
	}
	if d := byID[tm.ID]; d.RecipientDeleted || d.Recipient != "" || d.Kind != "test" {
		t.Fatalf("a test to a typed address has no recipient and none was deleted: %+v", d)
	}

	f := decode[DeliveryLogResponse](t, e.wantStatus("GET", "/api/v1/notification-log?kind=alert&outcome=failed", owner, nil, 200))
	if len(f.Deliveries) != 1 || f.Deliveries[0].ID != a.ID {
		t.Fatalf("filter %+v", f)
	}
	p1 := decode[DeliveryLogResponse](t, e.wantStatus("GET", "/api/v1/notification-log?limit=3", owner, nil, 200))
	if len(p1.Deliveries) != 3 || !p1.Truncated || p1.NextCursor != "o3" {
		t.Fatalf("page 1 %+v", p1)
	}
	p2 := decode[DeliveryLogResponse](t, e.wantStatus("GET", "/api/v1/notification-log?limit=3&cursor="+p1.NextCursor, owner, nil, 200))
	if len(p2.Deliveries) != 1 || p2.Truncated || p2.Deliveries[0].ID != l.Deliveries[3].ID {
		t.Fatalf("page 2 %+v", p2)
	}
	b := e.wantStatus("GET", "/api/v1/notification-log?kind=bogus&outcome=lost", owner, nil, 400)
	if fe := fieldOf(t, b); len(fe) != 2 || fe[0].Field != "kind" || fe[1].Field != "outcome" || strings.Contains(string(b), "bogus") {
		t.Fatalf("bad filters %s", b)
	}
	e.wantStatus("GET", "/api/v1/notification-log?limit=0", owner, nil, 400)
	e.wantStatus("GET", "/api/v1/notification-log?cursor=x", owner, nil, 400)
	e.wantStatus("GET", "/api/v1/notification-log", e.as("w3a-admin", "admin", nil), nil, 200)
	e.wantStatus("GET", "/api/v1/notification-log", e.as("w3a-op", "operator", nil), nil, 403)
}

// A store read that fails is a 500 with the house body, and its log line names the route and a
// class word, never the SQL error.
func TestM4StoreReadErrorIsA500WithClassWordsOnly(t *testing.T) {
	e := newEnv(t)
	var logs bytes.Buffer
	e.svc.Log = logx.New(&logs, logx.LevelDebug)
	owner := e.login("admin", pw)
	if err := e.st.Write(context.Background(), func(tx *store.Tx) error {
		_, err := tx.Exec(`ALTER TABLE notification_log RENAME TO notification_log_gone`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	res, b := e.req("GET", "/api/v1/notification-log", owner, nil)
	if res.StatusCode != 500 || problemCode(t, res, b) != api.CodeInternal || strings.Contains(string(b), "notification_log") {
		t.Fatalf("%d %s", res.StatusCode, b)
	}
	out := logs.String()
	if !strings.Contains(out, `"route":"delivery_log","class":"failed"`) || strings.Contains(out, "notification_log") || strings.Contains(out, "no such table") {
		t.Fatalf("log line: %s", out)
	}
}

// ---- SMTP status ----

func TestM4SMTPStatusShowsNoHostAndNoSecret(t *testing.T) {
	e := newEnv(t)
	owner := e.login("admin", pw)
	e.wantStatus("GET", "/api/v1/settings/smtp", owner, nil, 503) // a console wired without its mail side says so
	m := e.withMail(mail.NewFakeSender())
	dir := filepath.Join(t.TempDir(), "secret-dir-marker")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	pwFile := filepath.Join(dir, "relay.pw")
	if err := os.WriteFile(pwFile, []byte("hunter2-not-a-password\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m.SMTP.PasswordFile = pwFile
	b := e.wantStatus("GET", "/api/v1/settings/smtp", owner, nil, 200)
	st := decode[SMTPStatusResponse](t, b)
	if !st.Configured || st.Fixtures || !st.HostSet || st.Port != 587 || st.Security != "starttls" || !st.UsernameSet || !st.PasswordFileSet || !st.PasswordFileReadable || st.From != "console@example.test" || st.ConfigError != "" {
		t.Fatalf("status %s", b)
	}
	for _, leak := range []string{"relay.example.test", "relay-user", "hunter2", "secret-dir-marker", "relay.pw"} {
		if strings.Contains(string(b), leak) {
			t.Fatalf("the status shows %q: %s", leak, b)
		}
	}
	m.SMTP.PasswordFile = filepath.Join(dir, "missing.pw")
	m.SMTP.Security = "plain"
	st = decode[SMTPStatusResponse](t, e.wantStatus("GET", "/api/v1/settings/smtp", owner, nil, 200))
	if st.PasswordFileReadable || st.ConfigError != string(mail.ClassPlaintextRefused) || st.ConfigHint == "" {
		t.Fatalf("unreadable file, plain with a user: %+v", st)
	}
	m.SMTP, m.Configured = mail.Settings{Security: "starttls"}, false
	st = decode[SMTPStatusResponse](t, e.wantStatus("GET", "/api/v1/settings/smtp", owner, nil, 200))
	if st.Configured || st.HostSet || st.PasswordFileSet || st.ConfigError != "" {
		t.Fatalf("not configured: %+v", st)
	}
	e.wantStatus("GET", "/api/v1/settings/smtp", e.as("w3a-admin", "admin", nil), nil, 200)
	e.wantStatus("GET", "/api/v1/settings/smtp", e.as("w3a-op", "operator", nil), nil, 403)
}

// ---- test mail ----

func TestM4TestMailSendsAndAnswersPerRecipient(t *testing.T) {
	e := newEnv(t)
	fake := mail.NewFakeSender()
	e.withMail(fake)
	owner := e.login("admin", pw)
	admin, err := e.st.GetAdminByUsername(context.Background(), "admin")
	if err != nil {
		t.Fatal(err)
	}
	b := e.wantStatus("POST", "/api/v1/settings/smtp/test", owner, nil, 200) // no body: the caller's own address
	r := decode[SMTPTestResponse](t, b)
	if !r.Sent || r.DeliveryID == "" || len(r.Recipients) != 1 || r.Recipients[0].To != admin.Email || r.Recipients[0].Outcome != "sent" || r.Recipients[0].Class != "" {
		t.Fatalf("own address: %s", b)
	}
	sent := fake.Sent()
	if len(sent) != 1 || sent[0].To[0] != admin.Email || sent[0].Kind != mail.KindTest {
		t.Fatalf("sent %+v", sent)
	}
	if n := e.auditCount("smtp.test", ""); n != 1 {
		t.Fatalf("%d smtp.test audit rows", n)
	}

	// A failure: the stage and the class word, never the relay's text.
	e.clk.Advance(notify.TestMailInterval)
	fake.FailNext(1, &mail.SendError{Stage: mail.StageRecipient, Class: mail.ClassRecipientRejected})
	b = e.wantStatus("POST", "/api/v1/settings/smtp/test", e.as("w3a-admin", "admin", nil), SMTPTestRequest{To: "typo@example.com"}, 200)
	r = decode[SMTPTestResponse](t, b)
	one := r.Recipients[0]
	if r.Sent || one.To != "typo@example.com" || one.Outcome != "failed" || one.Stage != "recipient" || one.Class != "recipient_rejected" || one.Hint != mail.Hint(mail.ClassRecipientRejected) {
		t.Fatalf("failure: %s", b)
	}
	row := decode[DeliveryLogResponse](t, e.wantStatus("GET", "/api/v1/notification-log?kind=test&outcome=failed", owner, nil, 200)).Deliveries
	if len(row) != 1 || row[0].ID != r.DeliveryID || row[0].NextRetryAt != nil {
		t.Fatalf("the failed test is final in the log: %+v", row)
	}

	// The rate limit: one test per 10 s per console, whoever asks.
	e.clk.Advance(3 * time.Second)
	res, b := e.req("POST", "/api/v1/settings/smtp/test", owner, SMTPTestRequest{To: "ops@example.com"})
	if code, reason, _ := m4Reason(t, b); res.StatusCode != 429 || code != api.CodeRateLimited || reason != "test_too_soon" || res.Header.Get("Retry-After") != "7" {
		t.Fatalf("rate limit: %d %s %v", res.StatusCode, b, res.Header)
	}
	if fake.Calls() != 2 {
		t.Fatalf("a refused test sends nothing: %d calls", fake.Calls())
	}
}

func TestM4TestMailRefusals(t *testing.T) {
	e := newEnv(t)
	owner := e.login("admin", pw)
	e.wantStatus("POST", "/api/v1/settings/smtp/test", owner, nil, 503) // no mail side
	fake := mail.NewFakeSender()
	m := e.withMail(fake)
	bad := "Ops <ops@example.com>"
	b := e.wantStatus("POST", "/api/v1/settings/smtp/test", owner, SMTPTestRequest{To: bad}, 400)
	if fe := fieldOf(t, b); len(fe) != 1 || fe[0].Field != "to" || strings.Contains(string(b), "Ops <") {
		t.Fatalf("bad address: %s", b)
	}
	e.wantStatus("POST", "/api/v1/settings/smtp/test", owner, `{"to":"ops@example.com","cc":"x@example.com"}`, 400)
	m.Test.Configured = false
	b = e.wantStatus("POST", "/api/v1/settings/smtp/test", owner, SMTPTestRequest{To: "ops@example.com"}, 409)
	if code, reason, detail := m4Reason(t, b); code != api.CodeConflict || reason != "smtp_not_configured" || detail != detailNotConfigured {
		t.Fatalf("not configured: %s", b)
	}
	m.Test.Configured = true
	e.wantStatus("POST", "/api/v1/settings/smtp/test", e.as("w3a-op", "operator", nil), SMTPTestRequest{To: "ops@example.com"}, 403)

	// An account without an address must type one.
	if err := e.st.Write(context.Background(), func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE admin_users SET email = '' WHERE username = 'admin'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	b = e.wantStatus("POST", "/api/v1/settings/smtp/test", e.login("admin", pw), SMTPTestRequest{}, 400)
	if fe := fieldOf(t, b); len(fe) != 1 || fe[0].Field != "to" || fe[0].Message != noOwnAddress {
		t.Fatalf("no own address: %s", b)
	}
	if fake.Calls() != 0 {
		t.Fatalf("no refusal sends: %d calls", fake.Calls())
	}
}

// hangSender blocks until its context ends.
type hangSender struct{}

func (hangSender) Send(ctx context.Context, _ mail.Message) error {
	<-ctx.Done()
	return &mail.SendError{Stage: mail.StageConnect, Class: mail.ClassTimeout, Temporary: true}
}

func TestM4TestMailHangingSenderDoesNotHangTheRequest(t *testing.T) {
	e := newEnv(t)
	m := e.withMail(hangSender{})
	m.Test.Dispatch.SendTimeout = 100 * time.Millisecond
	start := time.Now()
	b := e.wantStatus("POST", "/api/v1/settings/smtp/test", e.login("admin", pw), SMTPTestRequest{To: "ops@example.com"}, 200)
	r := decode[SMTPTestResponse](t, b)
	if r.Sent || r.Recipients[0].Class != "timeout" || time.Since(start) > 10*time.Second {
		t.Fatalf("hanging relay: %s after %v", b, time.Since(start))
	}
}

// ---- digest ----

func TestM4DigestPreviewAndSendNow(t *testing.T) {
	e := newEnv(t)
	owner := e.login("admin", pw)
	e.wantStatus("GET", "/api/v1/digest/preview", owner, nil, 503)
	e.wantStatus("POST", "/api/v1/digest/send", owner, nil, 503)
	fake := mail.NewFakeSender()
	m := e.withMail(fake)

	pv := decode[DigestPreviewResponse](t, e.wantStatus("GET", "/api/v1/digest/preview", owner, nil, 200))
	if !strings.Contains(pv.Subject, "CryoBackup") || pv.Text == "" || !strings.Contains(pv.HTML, "<html") || fake.Calls() != 0 {
		t.Fatalf("preview %+v", pv)
	}
	if l := decode[DeliveryLogResponse](t, e.wantStatus("GET", "/api/v1/notification-log", owner, nil, 200)); len(l.Deliveries) != 0 {
		t.Fatal("a preview records nothing")
	}

	d := decode[DigestSendResponse](t, e.wantStatus("POST", "/api/v1/digest/send", owner, nil, 200))
	if d.Skipped != notify.DigestSkippedNoRecipient || d.Sent != 0 {
		t.Fatalf("no digest recipient: %+v", d)
	}
	rc := decode[RecipientResponse](t, e.wantStatus("POST", "/api/v1/notification-rules", owner, map[string]any{"name": "Morning", "address": "morning@example.com"}, 201)).Recipient
	d = decode[DigestSendResponse](t, e.wantStatus("POST", "/api/v1/digest/send", owner, DigestSendRequest{}, 200))
	if d.Sent != 1 || d.Forced || d.Date == "" || len(d.Recipients) != 1 || d.Recipients[0].ID != rc.ID || d.Recipients[0].Name != "Morning" || d.Recipients[0].Outcome != notify.DigestSent {
		t.Fatalf("send now: %+v", d)
	}
	d = decode[DigestSendResponse](t, e.wantStatus("POST", "/api/v1/digest/send", owner, nil, 200))
	if d.Sent != 0 || d.Recipients[0].Outcome != notify.DigestAlreadySent {
		t.Fatalf("again: a no-op for the day already sent: %+v", d)
	}
	d = decode[DigestSendResponse](t, e.wantStatus("POST", "/api/v1/digest/send", owner, DigestSendRequest{Force: true}, 200))
	if d.Sent != 1 || !d.Forced {
		t.Fatalf("forced: %+v", d)
	}
	if fake.Calls() != 2 || e.auditCount("digest.send", "") != 4 {
		t.Fatalf("calls %d, audit rows %d", fake.Calls(), e.auditCount("digest.send", ""))
	}

	m.Configured = false
	b := e.wantStatus("POST", "/api/v1/digest/send", owner, nil, 409)
	if _, reason, _ := m4Reason(t, b); reason != "smtp_not_configured" {
		t.Fatalf("not configured %s", b)
	}
	m.Configured = true
	admin := e.as("w3a-admin", "admin", nil)
	e.wantStatus("GET", "/api/v1/digest/preview", admin, nil, 200)
	e.wantStatus("POST", "/api/v1/digest/send", admin, nil, 403)
	e.wantStatus("GET", "/api/v1/digest/preview", e.as("w3a-op", "operator", nil), nil, 403)
}

// ---- digest settings: the existing settings route reads and writes them ----

func TestM4DigestSettingsThroughTheSettingsRoute(t *testing.T) {
	e := newEnv(t)
	owner := e.login("admin", pw)
	value := func(key string) SettingJSON {
		t.Helper()
		for _, s := range decode[SettingsResponse](t, e.wantStatus("GET", "/api/v1/settings", owner, nil, 200)).Settings {
			if s.Key == key {
				return s
			}
		}
		t.Fatalf("no setting %s", key)
		return SettingJSON{}
	}
	if err := e.st.Write(context.Background(), func(tx *store.Tx) error { // the fixtures store a value: start from none
		_, err := tx.Exec(`DELETE FROM settings WHERE key IN ('digest_enabled', 'digest_enabled.source')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if v := value(store.SettingDigestEnabled); string(v.Value) != "true" || v.Stored || !v.Editable {
		t.Fatalf("digest_enabled with no row reads its default, true: %+v", v)
	}
	if v := value(store.SettingDigestLocalTime); string(v.Value) != `"07:00"` || !v.Editable {
		t.Fatalf("digest_local_time: %+v", v)
	}
	e.wantStatus("PUT", "/api/v1/settings/digest_enabled", owner, map[string]any{"value": false}, 200)
	e.wantStatus("PUT", "/api/v1/settings/digest_local_time", owner, map[string]any{"value": "18:30"}, 200)
	ds, err := e.st.ReadDigestSettings(context.Background(), store.ScopeAll())
	if err != nil || ds.Enabled || ds.LocalTime != "18:30" {
		t.Fatalf("stored: %+v %v", ds, err)
	}
	if v := value(store.SettingDigestEnabled); string(v.Value) != "false" || !v.Stored {
		t.Fatalf("after the write: %+v", v)
	}
	b := e.wantStatus("PUT", "/api/v1/settings/digest_local_time", owner, map[string]any{"value": "24:00"}, 400)
	if fe := fieldOf(t, b); len(fe) != 1 || fe[0].Field != "value" {
		t.Fatalf("bad time %s", b)
	}
	e.wantStatus("PUT", "/api/v1/settings/digest_enabled", owner, map[string]any{"value": "yes"}, 400)
	e.wantStatus("PUT", "/api/v1/settings/digest_enabled", e.as("w3a-admin", "admin", nil), map[string]any{"value": true}, 403)
}

// ---- m3_system.go: the C03 pending condition with the banner's 15-minute age ----

func TestM4SystemHealthPendingAlertWaitsFifteenMinutes(t *testing.T) {
	failedPending := func(oldest state.Millis) bool {
		f := r5Passing()
		f.Probes.SMTP.PendingAlerts, f.Probes.SMTP.LastSendOKAt, f.Probes.SMTP.OldestPendingAt = 1, 0, oldest
		for _, a := range f.smtpAttempts() {
			if a.Result == failed && a.Why == "alert e-mail is waiting to go out and none has been delivered recently" {
				return true
			}
		}
		return false
	}
	for _, tc := range []struct {
		name   string
		oldest state.Millis
		want   bool
	}{
		{"queued 14 minutes ago", r5Ago(14 * time.Minute), false},
		{"queued 15 minutes ago", r5Ago(15 * time.Minute), true},
		{"age unknown", 0, true},
		{"age garbled (an hour ahead)", ms(r5Now.Add(time.Hour)), true},
		{"queued 4 minutes ahead (clock skew)", ms(r5Now.Add(4 * time.Minute)), false},
	} {
		if got := failedPending(tc.oldest); got != tc.want {
			t.Errorf("%s: failed %v, want %v", tc.name, got, tc.want)
		}
	}
}
