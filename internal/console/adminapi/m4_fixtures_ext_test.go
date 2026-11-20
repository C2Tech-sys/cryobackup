package adminapi_test

import (
	"bytes"
	"context"
	"encoding/base32"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminapi"
	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/cli"
	"github.com/c2tech-sys/cryobackup/internal/console/config"
	"github.com/c2tech-sys/cryobackup/internal/console/mail"
)

// P20-W3A: every M4 route works against the fixture world of a real fixtures-mode console, and
// every message it sends lands in the FakeSender: nothing leaves the process.

type fxClient struct {
	t       *testing.T
	base    string
	cookies []*http.Cookie
	csrf    string
}

func (c *fxClient) do(method, path string, body any, want int) []byte {
	c.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	r, _ := http.NewRequest(method, c.base+path, rd)
	r.Header.Set("Content-Type", "application/json")
	for _, ck := range c.cookies {
		r.AddCookie(ck)
	}
	if method != "GET" && c.csrf != "" {
		r.Header.Set("Origin", c.base)
		r.Header.Set(auth.CSRFHeader, c.csrf)
	}
	res, err := http.DefaultClient.Do(r)
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode != want {
		c.t.Fatalf("%s %s: %d %s (want %d)", method, path, res.StatusCode, b, want)
	}
	if fresh := res.Cookies(); len(fresh) > 0 {
		keep := fresh
		for _, old := range c.cookies {
			dup := false
			for _, f := range fresh {
				dup = dup || f.Name == old.Name
			}
			if !dup {
				keep = append(keep, old)
			}
		}
		c.cookies = keep
	}
	return b
}

func fxDecode[T any](t *testing.T, b []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
	return v
}

func TestM4RoutesWorkInFixturesMode(t *testing.T) {
	cfg := config.Default()
	cfg.Resolve()
	cfg.Fixtures = true
	cfg.AdminListen = "127.0.0.1:0"
	a, err := cli.Build(context.Background(), cfg, cli.BuildOptions{FixturesPassword: "fixtures-pass-1"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	fake, ok := a.Mail.(*mail.FakeSender)
	if !ok || a.Admin.Mail == nil || a.Admin.Mail.Test != a.TestMail || a.Admin.Mail.Digest == nil {
		t.Fatalf("fixtures wiring: sender %T, mail %+v", a.Mail, a.Admin.Mail)
	}
	totpAt := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	a.Auth.TOTPNow = func() time.Time { return totpAt }
	srv := httptest.NewServer(a.Server.AdminHandler())
	t.Cleanup(srv.Close)

	// Sign in: the password step, then the forced TOTP enrollment of the fixture owner.
	c := &fxClient{t: t, base: srv.URL}
	lr := fxDecode[adminapi.LoginResponse](t, c.do("POST", "/api/v1/auth/login", adminapi.LoginRequest{Username: "admin", Password: "fixtures-pass-1"}, 200))
	c.csrf = lr.CSRFToken
	tb := fxDecode[adminapi.TOTPBeginResponse](t, c.do("POST", "/api/v1/me/totp/begin", nil, 200))
	secret, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ReplaceAll(tb.Secret, " ", ""))
	if err != nil {
		t.Fatal(err)
	}
	done := fxDecode[adminapi.RecoveryCodesResponse](t, c.do("POST", "/api/v1/me/totp/confirm", adminapi.TOTPConfirmRequest{Code: auth.TOTPCode(secret, totpAt)}, 200))
	if !done.LoginComplete || done.CSRFToken == "" {
		t.Fatalf("login: %+v", done)
	}
	c.csrf = done.CSRFToken

	st := fxDecode[adminapi.SMTPStatusResponse](t, c.do("GET", "/api/v1/settings/smtp", nil, 200))
	if !st.Configured || !st.Fixtures {
		t.Fatalf("smtp status %+v", st)
	}
	rc := fxDecode[adminapi.RecipientResponse](t, c.do("POST", "/api/v1/notification-rules", map[string]any{"name": "Night desk", "address": "ops@example.test"}, 201)).Recipient
	if l := fxDecode[adminapi.RecipientsResponse](t, c.do("GET", "/api/v1/notification-rules", nil, 200)); len(l.Recipients) != 1 {
		t.Fatalf("recipients %+v", l)
	}
	c.do("PATCH", "/api/v1/notification-rules/"+rc.ID, map[string]any{"recoveries": false}, 200)
	tr := fxDecode[adminapi.SMTPTestResponse](t, c.do("POST", "/api/v1/settings/smtp/test", map[string]any{"to": "desk@example.test"}, 200))
	if !tr.Sent {
		t.Fatalf("test %+v", tr)
	}
	if pv := fxDecode[adminapi.DigestPreviewResponse](t, c.do("GET", "/api/v1/digest/preview", nil, 200)); pv.Subject == "" || pv.Text == "" {
		t.Fatalf("preview %+v", pv)
	}
	if ds := fxDecode[adminapi.DigestSendResponse](t, c.do("POST", "/api/v1/digest/send", map[string]any{"force": true}, 200)); ds.Sent != 1 {
		t.Fatalf("digest %+v", ds)
	}
	if l := fxDecode[adminapi.DeliveryLogResponse](t, c.do("GET", "/api/v1/notification-log", nil, 200)); len(l.Deliveries) != 2 {
		t.Fatalf("log %+v", l)
	}

	// The fixture world has one snoozed alarm: clear it, then acknowledge it again.
	acks := fxDecode[adminapi.AcksResponse](t, c.do("GET", "/api/v1/acks", nil, 200)).Acks
	if len(acks) != 1 {
		t.Fatalf("fixture acks %+v", acks)
	}
	c.do("DELETE", "/api/v1/acks/"+acks[0].ID, nil, 204)
	again := fxDecode[adminapi.AckResponse](t, c.do("POST", "/api/v1/acks", adminapi.CreateAckRequest{SubjectType: acks[0].SubjectType, SubjectID: acks[0].SubjectID,
		AlarmType: acks[0].AlarmType, Kind: adminapi.AckKindAcknowledge, Note: "fixture check"}, 201))
	if again.Note != "fixture check" {
		t.Fatalf("ack %+v", again)
	}
	c.do("DELETE", "/api/v1/notification-rules/"+rc.ID, nil, 200)

	sent := fake.Sent()
	if len(sent) != 2 || sent[0].Kind != mail.KindTest || sent[1].Kind != mail.KindDigest {
		t.Fatalf("the FakeSender holds every message: %+v", sent)
	}
}
