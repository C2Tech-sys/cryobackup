package adminapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/console/mail"
	"github.com/c2tech-sys/cryobackup/internal/console/notify"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
)

// P20-W3D: follow-ups from the wave 3 blind checks.

// A recipient edit changes only what the request names, atomically: 30 pairs of a rename and
// switching alerts off, each pair sent at the same moment, all end with both changes.
func TestM4RecipientConcurrentPatchesBothStand(t *testing.T) {
	e := newEnv(t)
	owner := e.login("admin", pw)
	for i := 0; i < 30; i++ {
		r := decode[RecipientResponse](t, e.wantStatus("POST", "/api/v1/notification-rules", owner,
			map[string]any{"name": fmt.Sprintf("Old %d", i), "address": fmt.Sprintf("pair%d@example.com", i)}, 201)).Recipient
		name := fmt.Sprintf("New %d", i)
		var wg sync.WaitGroup
		start := make(chan struct{})
		codes := make([]int, 2)
		for j, body := range []map[string]any{{"name": name}, {"alerts": false}} {
			wg.Add(1)
			go func(j int, body map[string]any) {
				defer wg.Done()
				<-start
				res, _ := e.req("PATCH", "/api/v1/notification-rules/"+r.ID, owner, body)
				codes[j] = res.StatusCode
			}(j, body)
		}
		close(start)
		wg.Wait()
		if codes[0] != 200 || codes[1] != 200 {
			t.Fatalf("pair %d: statuses %v", i, codes)
		}
		list := decode[RecipientsResponse](t, e.wantStatus("GET", "/api/v1/notification-rules", owner, nil, 200))
		var got *RecipientJSON
		for k := range list.Recipients {
			if list.Recipients[k].ID == r.ID {
				got = &list.Recipients[k]
			}
		}
		if got == nil || got.Name != name || got.Alerts || !got.Recoveries || !got.Digest {
			t.Fatalf("pair %d lost an edit: %+v (want name %q, alerts off, the rest on)", i, got, name)
		}
	}
}

// An edit that changes nothing still answers the row and writes no audit row.
func TestM4RecipientNoOpPatchAnswersTheRowUnaudited(t *testing.T) {
	e := newEnv(t)
	owner := e.login("admin", pw)
	r := decode[RecipientResponse](t, e.wantStatus("POST", "/api/v1/notification-rules", owner,
		map[string]any{"name": "Ops", "address": "ops@example.com", "digest": false}, 201)).Recipient
	for _, body := range []map[string]any{{}, {"name": "Ops", "digest": false, "alerts": true}} {
		p := decode[RecipientResponse](t, e.wantStatus("PATCH", "/api/v1/notification-rules/"+r.ID, owner, body, 200))
		if p.Recipient.ID != r.ID || p.Recipient.Name != "Ops" || p.Recipient.Digest || !p.Recipient.Alerts || !p.Recipient.UpdatedAt.Equal(r.UpdatedAt) {
			t.Fatalf("no-op patch %v answered %+v", body, p.Recipient)
		}
	}
	if n := e.auditCount("recipient.update", r.ID); n != 0 {
		t.Fatalf("%d recipient.update audit rows for a no-op patch", n)
	}
}

// A refused audit of the test mail or the digest send logs the action by name: the log field
// helper refuses a dot followed by a letter, so "smtp.test" is logged as smtp_test (the form of
// the route names of these handlers), never as "invalid".
func TestM4RefusedAuditLogNamesTheAction(t *testing.T) {
	e := newEnv(t)
	var logs bytes.Buffer
	e.svc.Log = logx.New(&logs, logx.LevelDebug)
	fake := mail.NewFakeSender()
	e.withMail(fake)
	owner := e.login("admin", pw)
	e.setAuditRefusal(true)
	e.wantStatus("POST", "/api/v1/settings/smtp/test", owner, SMTPTestRequest{To: "ops@example.com"}, 500)
	e.wantStatus("POST", "/api/v1/digest/send", owner, nil, 500)
	out := logs.String()
	for _, want := range []string{`"action":"smtp_test"`, `"action":"digest_send"`} {
		if !strings.Contains(out, want) {
			t.Errorf("no %s in the log lines: %s", want, out)
		}
	}
	if strings.Contains(out, `"action":"invalid"`) || fake.Calls() != 0 {
		t.Fatalf("log lines: %s; %d sends", out, fake.Calls())
	}
}

// The digest send says whether the daily digest is switched on: a send with it off still sends
// (once, on request) and the answer says digest_enabled false.
func TestM4DigestSendSaysWhetherTheDigestIsOn(t *testing.T) {
	e := newEnv(t)
	fake := mail.NewFakeSender()
	e.withMail(fake)
	owner := e.login("admin", pw)
	e.wantStatus("POST", "/api/v1/notification-rules", owner, map[string]any{"name": "Morning", "address": "morning@example.com"}, 201)
	setDigest := func(v string) {
		t.Helper()
		if err := e.st.Write(context.Background(), func(tx *store.Tx) error {
			_, err := tx.Exec(`INSERT INTO settings (key, value, created_at, updated_at) VALUES ('digest_enabled', ?, 0, 0)
				ON CONFLICT(key) DO UPDATE SET value = excluded.value`, v)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	setDigest("true")
	var raw map[string]json.RawMessage
	b := e.wantStatus("POST", "/api/v1/digest/send", owner, nil, 200)
	if err := json.Unmarshal(b, &raw); err != nil || string(raw["digest_enabled"]) != "true" {
		t.Fatalf("digest on: %s", b)
	}
	setDigest("false")
	b = e.wantStatus("POST", "/api/v1/digest/send", owner, DigestSendRequest{Force: true}, 200)
	d := decode[DigestSendResponse](t, b)
	if err := json.Unmarshal(b, &raw); err != nil || string(raw["digest_enabled"]) != "false" || d.Sent != 1 {
		t.Fatalf("digest off: %s", b)
	}
	// A setting that cannot be read: the daily job does not send either, so the answer says false.
	setDigest(`"maybe"`) // valid JSON, neither true nor false
	b = e.wantStatus("POST", "/api/v1/digest/send", owner, DigestSendRequest{Force: true}, 200)
	if err := json.Unmarshal(b, &raw); err != nil || string(raw["digest_enabled"]) != "false" {
		t.Fatalf("digest setting unreadable: %s", b)
	}
	if fake.Calls() != 3 {
		t.Fatalf("%d sends, want 3", fake.Calls())
	}
}

// A test mail the relay accepted whose delivery row could not be recorded answers 200 with the
// outcome sent and recorded false (the mail went out), and logs the store failure in class words.
func TestM4TestMailSentButNotRecordedAnswersSent(t *testing.T) {
	e := newEnv(t)
	var logs bytes.Buffer
	e.svc.Log = logx.New(&logs, logx.LevelDebug)
	fake := mail.NewFakeSender()
	e.withMail(fake)
	owner := e.login("admin", pw)
	b := e.wantStatus("POST", "/api/v1/settings/smtp/test", owner, SMTPTestRequest{To: "ops@example.com"}, 200)
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil || string(raw["recorded"]) != "true" {
		t.Fatalf("recorded: %s", b)
	}
	refuse := func(sql string) {
		t.Helper()
		if err := e.st.Write(context.Background(), func(tx *store.Tx) error { _, err := tx.Exec(sql); return err }); err != nil {
			t.Fatal(err)
		}
	}
	refuse(`CREATE TRIGGER refuse_outcome BEFORE UPDATE OF result ON notification_log WHEN NEW.result IN ('sent', 'failed') BEGIN SELECT RAISE(ABORT, 'outcome refused by the test'); END`)
	e.clk.Advance(notify.TestMailInterval)
	b = e.wantStatus("POST", "/api/v1/settings/smtp/test", owner, SMTPTestRequest{To: "ops@example.com"}, 200)
	r := decode[SMTPTestResponse](t, b)
	if err := json.Unmarshal(b, &raw); err != nil || string(raw["recorded"]) != "false" || !r.Sent || r.DeliveryID == "" ||
		len(r.Recipients) != 1 || r.Recipients[0].Outcome != "sent" {
		t.Fatalf("sent, not recorded: %s", b)
	}
	// A failed send whose outcome could not be recorded is answered the same way: failed, recorded false.
	e.clk.Advance(notify.TestMailInterval)
	fake.FailNext(1, &mail.SendError{Stage: mail.StageRecipient, Class: mail.ClassRecipientRejected})
	b = e.wantStatus("POST", "/api/v1/settings/smtp/test", owner, SMTPTestRequest{To: "ops@example.com"}, 200)
	r = decode[SMTPTestResponse](t, b)
	if err := json.Unmarshal(b, &raw); err != nil || string(raw["recorded"]) != "false" || r.Sent || r.Recipients[0].Outcome != "failed" || r.Recipients[0].Class != "recipient_rejected" {
		t.Fatalf("failed, not recorded: %s", b)
	}
	out := logs.String()
	if !strings.Contains(out, "not recorded") || !strings.Contains(out, `"route":"smtp_test"`) {
		t.Fatalf("no log line for the store failure: %s", out)
	}
	for _, leak := range []string{"ops@example.com", "outcome refused by the test", "relay.example.test"} {
		if strings.Contains(out, leak) {
			t.Fatalf("the log carries %q: %s", leak, out)
		}
	}
	if fake.Calls() != 3 {
		t.Fatalf("%d sends, want 3", fake.Calls())
	}
}
