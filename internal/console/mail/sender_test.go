package mail

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"net"
	netmail "net/mail"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/config"
	"github.com/c2tech-sys/cryobackup/internal/logx"
)

func isKnownClass(c Class) bool {
	for _, k := range Classes() {
		if k == c {
			return true
		}
	}
	return false
}

// wantErr asserts err is a *SendError at stage with class and Temporary, with a
// class from the fixed list and an Error() that is words only.
func wantErr(t *testing.T, err error, stage Stage, class Class, temporary bool) {
	t.Helper()
	var se *SendError
	if !errors.As(err, &se) {
		t.Fatalf("want *SendError %s/%s, got %v", stage, class, err)
	}
	if se.Stage != stage || se.Class != class || se.Temporary != temporary {
		t.Fatalf("got %s/%s temporary=%v, want %s/%s temporary=%v", se.Stage, se.Class, se.Temporary, stage, class, temporary)
	}
	if !isKnownClass(se.Class) {
		t.Fatalf("class %q is not in Classes()", se.Class)
	}
	if se.Hint() == "" {
		t.Fatalf("class %q has no hint", se.Class)
	}
}

func pwFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "smtp-pass")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func testMsg() Message {
	return Message{
		To:      []string{"ops@example.com"},
		Subject: "Backup alert",
		Text:    "Two devices failed.\n",
		HTML:    "<p>Two devices failed.</p>\n",
		Kind:    KindAlert,
	}
}

func sendWith(t *testing.T, set Settings, m Message) error {
	t.Helper()
	return NewSMTPSender(set).Send(context.Background(), m)
}

func TestSTARTTLSDeliversWithAuth(t *testing.T) {
	p := testPKI(t)
	srv := startFake(t, fakeCfg{cert: &p.good, startTLS: true, authMech: "PLAIN LOGIN", user: "svc", pass: "s3cret-pw"})
	set := srv.settings("starttls")
	set.Username, set.PasswordFile = "svc", pwFile(t, "s3cret-pw\n")
	m := testMsg()
	m.To = []string{"ops@example.com", "Night Desk <night@example.org>"}
	if err := sendWith(t, set, m); err != nil {
		t.Fatalf("send: %v", err)
	}
	got := srv.captured()
	if len(got) != 1 {
		t.Fatalf("relay captured %d messages", len(got))
	}
	c := got[0]
	if !c.tls {
		t.Fatal("the message was not sent over TLS")
	}
	if c.from != "console@example.com" {
		t.Fatalf("envelope sender = %q: Settings.From is the default", c.from)
	}
	if strings.Join(c.rcpts, ",") != "ops@example.com,night@example.org" {
		t.Fatalf("envelope recipients = %v", c.rcpts)
	}
	msg, err := netmail.ReadMessage(bytes.NewReader(c.data))
	if err != nil {
		t.Fatalf("relay got an unparseable message: %v", err)
	}
	if msg.Header.Get("Subject") != "Backup alert" || msg.Header.Get("Auto-Submitted") != "auto-generated" {
		t.Fatalf("headers = %v", msg.Header)
	}
	if a := srv.authList(); len(a) != 1 || a[0] != "svc:s3cret-pw:true" {
		t.Fatalf("auth seen by the relay = %v (must be one login, over TLS)", a)
	}
	// Nothing about the credential crossed the clear socket.
	raw := srv.rawBytes()
	plainAuth := base64.StdEncoding.EncodeToString([]byte("\x00svc\x00s3cret-pw"))
	for _, needle := range []string{"s3cret-pw", plainAuth, "AUTH"} {
		if bytes.Contains(raw, []byte(needle)) {
			t.Fatalf("the clear socket carried %q", needle)
		}
	}
}

func TestImplicitTLSDelivers(t *testing.T) {
	p := testPKI(t)
	srv := startFake(t, fakeCfg{cert: &p.good, implicit: true, authMech: "PLAIN", user: "svc", pass: "pw-1"})
	set := srv.settings("tls")
	set.Port = 465
	set.Username, set.PasswordFile = "svc", pwFile(t, "pw-1")
	if err := sendWith(t, set, testMsg()); err != nil {
		t.Fatalf("send: %v", err)
	}
	if len(srv.captured()) != 1 || !srv.captured()[0].tls {
		t.Fatalf("captured = %+v", srv.captured())
	}
	if bytes.Contains(srv.rawBytes(), []byte("pw-1")) || bytes.Contains(srv.rawBytes(), []byte("EHLO")) {
		t.Fatal("implicit TLS leaked clear text on the socket")
	}
}

func TestPlainDeliversOnlyWhenAllowed(t *testing.T) {
	srv := startFake(t, fakeCfg{})
	set := srv.settings("plain")
	set.Port = 25

	// Without the opt-in: refused before any connection is made.
	err := sendWith(t, set, testMsg())
	wantErr(t, err, StageConfig, ClassPlaintextRefused, false)
	if n := srv.connCount(); n != 0 {
		t.Fatalf("plain without the opt-in connected %d times", n)
	}

	set.AllowPlaintext = true
	if err := sendWith(t, set, testMsg()); err != nil {
		t.Fatalf("plain with the opt-in: %v", err)
	}
	got := srv.captured()
	if len(got) != 1 || got[0].tls {
		t.Fatalf("captured = %+v", got)
	}
}

func TestPlainNeverCarriesCredentials(t *testing.T) {
	// A hostile relay that offers AUTH in clear and no STARTTLS.
	srv := startFake(t, fakeCfg{authMech: "PLAIN LOGIN", authBeforeTLS: true, user: "svc", pass: "pw"})
	set := srv.settings("plain")
	set.AllowPlaintext = true
	set.Username = "svc"
	set.PasswordFile = pwFile(t, "pw")
	err := sendWith(t, set, testMsg())
	wantErr(t, err, StageConfig, ClassPlaintextRefused, false)
	if srv.connCount() != 0 {
		t.Fatal("a plain send with a user name connected")
	}

	// STARTTLS required but the relay does not offer it, and offers AUTH anyway:
	// the client stops; no AUTH, no MAIL, and the password is not on the socket.
	set = srv.settings("starttls")
	set.Username, set.PasswordFile = "svc", pwFile(t, "pw")
	err = sendWith(t, set, testMsg())
	wantErr(t, err, StageTLS, ClassStartTLSMissing, false)
	if srv.sawCmd("AUTH") || srv.sawCmd("MAIL") || srv.sawCmd("DATA") {
		t.Fatalf("commands after a missing STARTTLS: AUTH=%v MAIL=%v", srv.sawCmd("AUTH"), srv.sawCmd("MAIL"))
	}
	if bytes.Contains(srv.rawBytes(), []byte("AUTH")) || bytes.Contains(srv.rawBytes(), []byte("pw")) {
		t.Fatal("credentials reached a clear socket")
	}
	if srv.authClr {
		t.Fatal("the relay saw AUTH on a clear connection")
	}
}

func TestSTARTTLSMissingIsNeverOpportunistic(t *testing.T) {
	srv := startFake(t, fakeCfg{}) // no TLS offered at all
	err := sendWith(t, srv.settings("starttls"), testMsg())
	wantErr(t, err, StageTLS, ClassStartTLSMissing, false)
	if srv.sawCmd("MAIL") || len(srv.captured()) != 0 {
		t.Fatal("a message was sent in clear text although STARTTLS is required")
	}
}

func TestSTARTTLSRefusedByRelay(t *testing.T) {
	p := testPKI(t)
	srv := startFake(t, fakeCfg{cert: &p.good, startTLS: true,
		reply: map[string]string{"STARTTLS": "454 4.7.0 TLS not available now LEAK-REPLY-TEXT"}})
	err := sendWith(t, srv.settings("starttls"), testMsg())
	wantErr(t, err, StageTLS, ClassStartTLSRejected, true)
	if srv.sawCmd("MAIL") {
		t.Fatal("MAIL after a refused STARTTLS")
	}
}

func TestBadCertificates(t *testing.T) {
	p := testPKI(t)
	certs := map[string]tls.Certificate{"wrong name": p.wrongName, "untrusted issuer": p.untrusted, "expired": p.expired}
	for name, cert := range certs {
		for _, mode := range []string{"starttls", "tls"} {
			t.Run(name+"/"+mode, func(t *testing.T) {
				c := cert
				srv := startFake(t, fakeCfg{cert: &c, startTLS: mode == "starttls", implicit: mode == "tls",
					authMech: "PLAIN", user: "svc", pass: "pw"})
				set := srv.settings(mode)
				set.Username, set.PasswordFile = "svc", pwFile(t, "pw")
				err := sendWith(t, set, testMsg())
				wantErr(t, err, StageTLS, ClassCertInvalid, false)
				if srv.sawCmd("AUTH") || srv.sawCmd("MAIL") {
					t.Fatal("the client went on after a certificate failure")
				}
				if bytes.Contains(srv.rawBytes(), []byte("AUTH PLAIN")) {
					t.Fatal("credentials were sent after a certificate failure")
				}
			})
		}
	}
	// With no Roots the system pool is used, which does not contain the test CA.
	srv := startFake(t, fakeCfg{cert: &p.good, startTLS: true})
	set := srv.settings("starttls")
	set.Roots = nil
	wantErr(t, sendWith(t, set, testMsg()), StageTLS, ClassCertInvalid, false)
}

func TestTLSConfigIsStrict(t *testing.T) {
	cfg := NewSMTPSender(Settings{Host: testHost, Port: 587, Security: "starttls"}).tlsConfig()
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Fatalf("MinVersion = %x, want TLS 1.2", cfg.MinVersion)
	}
	if cfg.ServerName != testHost || cfg.InsecureSkipVerify {
		t.Fatalf("server name %q, skip verify %v", cfg.ServerName, cfg.InsecureSkipVerify)
	}
}

func TestPlainTextRelayOnTLSPort(t *testing.T) {
	// Implicit TLS against a relay that speaks clear text: a handshake failure,
	// not a hang and not a certificate verdict.
	srv := startFake(t, fakeCfg{})
	set := srv.settings("tls")
	set.GreetingTimeout = 2 * time.Second
	err := sendWith(t, set, testMsg())
	var se *SendError
	if !errors.As(err, &se) || se.Stage != StageTLS || (se.Class != ClassTLSHandshake && se.Class != ClassTimeout) {
		t.Fatalf("got %v", err)
	}
}

func TestAuthOutcomes(t *testing.T) {
	p := testPKI(t)
	cases := []struct {
		name  string
		cfg   fakeCfg
		pass  string
		stage Stage
		class Class
		temp  bool
		ok    bool
		want  string // first auth the relay should have seen
	}{
		{"PLAIN accepted", fakeCfg{authMech: "PLAIN"}, "pw", "", "", false, true, "svc:pw:true"},
		{"LOGIN only is used", fakeCfg{authMech: "LOGIN"}, "pw", "", "", false, true, "svc:pw:true"},
		{"PLAIN preferred over LOGIN", fakeCfg{authMech: "LOGIN PLAIN"}, "pw", "", "", false, true, "svc:pw:true"},
		{"wrong password", fakeCfg{authMech: "PLAIN"}, "nope", StageAuth, ClassAuthRejected, false, false, "svc:nope:true"},
		{"wrong password over LOGIN", fakeCfg{authMech: "LOGIN"}, "nope", StageAuth, ClassAuthRejected, false, false, "svc:nope:true"},
		{"temporary refusal", fakeCfg{authMech: "PLAIN", reply: map[string]string{"AUTH": "454 4.7.0 try later LEAK-REPLY-TEXT"}}, "pw", StageAuth, ClassAuthRejected, true, false, ""},
		{"no AUTH offered", fakeCfg{}, "pw", StageAuth, ClassAuthUnsupported, false, false, ""},
		{"only CRAM-MD5 offered", fakeCfg{authMech: "CRAM-MD5"}, "pw", StageAuth, ClassAuthUnsupported, false, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg
			cfg.cert, cfg.startTLS, cfg.user, cfg.pass = &p.good, true, "svc", "pw"
			srv := startFake(t, cfg)
			set := srv.settings("starttls")
			set.Username, set.PasswordFile = "svc", pwFile(t, tc.pass)
			err := sendWith(t, set, testMsg())
			if tc.ok {
				if err != nil {
					t.Fatalf("send: %v", err)
				}
			} else {
				wantErr(t, err, tc.stage, tc.class, tc.temp)
				if srv.sawCmd("MAIL") {
					t.Fatal("MAIL after a failed AUTH")
				}
			}
			if tc.want != "" {
				a := srv.authList()
				if len(a) == 0 || a[0] != tc.want {
					t.Fatalf("auth seen = %v, want %q first", a, tc.want)
				}
			}
		})
	}
}

func TestEnvelopeAndDataRejections(t *testing.T) {
	cases := []struct {
		name  string
		cfg   fakeCfg
		stage Stage
		class Class
		temp  bool
	}{
		{"sender 550", fakeCfg{reply: map[string]string{"MAIL": "550 5.7.1 not allowed LEAK-REPLY-TEXT"}}, StageSender, ClassSenderRejected, false},
		{"sender 451", fakeCfg{reply: map[string]string{"MAIL": "451 4.3.0 busy"}}, StageSender, ClassSenderRejected, true},
		{"recipient 550", fakeCfg{reply: map[string]string{"RCPT": "550 5.1.1 no such user LEAK-REPLY-TEXT"}}, StageRecipient, ClassRecipientRejected, false},
		{"recipient 450", fakeCfg{reply: map[string]string{"RCPT": "450 4.2.1 mailbox busy"}}, StageRecipient, ClassRecipientRejected, true},
		{"recipient 452", fakeCfg{reply: map[string]string{"RCPT": "452 4.5.3 too many"}}, StageRecipient, ClassRecipientRejected, true},
		{"data 554", fakeCfg{reply: map[string]string{"DATA": "554 5.3.0 no"}}, StageData, ClassMessageRejected, false},
		{"data 451", fakeCfg{reply: map[string]string{"DATA": "451 4.3.0 local error"}}, StageData, ClassMessageRejected, true},
		{"after data 554", fakeCfg{reply: map[string]string{"DATAEND": "554 5.7.1 spam LEAK-REPLY-TEXT"}}, StageData, ClassMessageRejected, false},
		{"after data 451", fakeCfg{reply: map[string]string{"DATAEND": "451 4.7.1 greylisted"}}, StageData, ClassMessageRejected, true},
		{"greeting 554", fakeCfg{greeting: "554 5.3.2 no service LEAK-REPLY-TEXT"}, StageConnect, ClassProtocol, false},
		{"greeting 421", fakeCfg{greeting: "421 4.3.2 shutting down"}, StageConnect, ClassProtocol, true},
		{"ehlo 502", fakeCfg{reply: map[string]string{"EHLO": "502 5.5.1 nope"}}, StageConnect, ClassProtocol, false},
		{"ehlo 421", fakeCfg{reply: map[string]string{"EHLO": "421 4.4.2 going away"}}, StageConnect, ClassProtocol, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := startFake(t, tc.cfg)
			set := srv.settings("plain")
			set.AllowPlaintext = true
			err := sendWith(t, set, testMsg())
			wantErr(t, err, tc.stage, tc.class, tc.temp)
			if len(srv.captured()) != 0 {
				t.Fatal("the relay kept a message although the send failed")
			}
			if tc.stage == StageSender && srv.sawCmd("RCPT") {
				t.Fatal("RCPT after a refused sender")
			}
		})
	}
}

func TestRefusedRecipientSendsNothing(t *testing.T) {
	// All or nothing: with two recipients and the second refused, DATA is never
	// issued, so a retry cannot deliver twice to the first.
	srv := startFake(t, fakeCfg{rcptReply: map[string]string{"bad@example.org": "550 5.1.1 unknown LEAK-REPLY-TEXT"}})
	set := srv.settings("plain")
	set.AllowPlaintext = true
	m := testMsg()
	m.To = []string{"ok@example.com", "bad@example.org"}
	err := sendWith(t, set, m)
	wantErr(t, err, StageRecipient, ClassRecipientRejected, false)
	if srv.sawCmd("DATA") || len(srv.captured()) != 0 {
		t.Fatal("DATA was sent although a recipient was refused")
	}
}

func TestQuitFailureDoesNotFailADeliveredMessage(t *testing.T) {
	srv := startFake(t, fakeCfg{dropOnQuit: true})
	set := srv.settings("plain")
	set.AllowPlaintext = true
	if err := sendWith(t, set, testMsg()); err != nil {
		t.Fatalf("a delivered message reported as failed: %v", err)
	}
	if len(srv.captured()) != 1 {
		t.Fatal("message not delivered")
	}
}

func TestDefaultSenderAndExplicitFrom(t *testing.T) {
	srv := startFake(t, fakeCfg{})
	set := srv.settings("plain")
	set.AllowPlaintext = true
	m := testMsg()
	m.From = "Cryo Console <alerts@example.net>"
	if err := sendWith(t, set, m); err != nil {
		t.Fatal(err)
	}
	if got := srv.captured()[0].from; got != "alerts@example.net" {
		t.Fatalf("envelope sender = %q, want the message's own From", got)
	}
}

func TestBodyRoundTripsThroughDotStuffing(t *testing.T) {
	srv := startFake(t, fakeCfg{})
	set := srv.settings("plain")
	set.AllowPlaintext = true
	m := testMsg()
	m.Subject = "Sauvegarde \u00e9chou\u00e9e \u2013 \u5099\u4efd"
	m.Text = ".starts with a dot\n..two dots\n\u00fcber caf\u00e9\n"
	m.HTML = ""
	if err := sendWith(t, set, m); err != nil {
		t.Fatal(err)
	}
	msg, err := netmail.ReadMessage(bytes.NewReader(srv.captured()[0].data))
	if err != nil {
		t.Fatal(err)
	}
	_ = msg
	// Compare through the decoder helper of message_test.go.
	text := decodeSinglePart(t, srv.captured()[0].data)
	if text != strings.ReplaceAll(m.Text, "\n", "\r\n") {
		t.Fatalf("body after the wire = %q", text)
	}
}

func TestTimeoutsAndCancellation(t *testing.T) {
	t.Run("silent relay hits the greeting deadline", func(t *testing.T) {
		srv := startFake(t, fakeCfg{noGreeting: true})
		set := srv.settings("plain")
		set.AllowPlaintext = true
		set.GreetingTimeout = 200 * time.Millisecond
		start := time.Now()
		err := sendWith(t, set, testMsg())
		wantErr(t, err, StageConnect, ClassTimeout, true)
		if time.Since(start) > 5*time.Second {
			t.Fatalf("took %v", time.Since(start))
		}
	})
	t.Run("silent after the greeting hits the whole-send deadline", func(t *testing.T) {
		srv := startFake(t, fakeCfg{hang: map[string]bool{"DATA": true}})
		set := srv.settings("plain")
		set.AllowPlaintext = true
		set.Timeout = 300 * time.Millisecond
		err := sendWith(t, set, testMsg())
		wantErr(t, err, StageData, ClassTimeout, true)
	})
	t.Run("the context deadline is the outer limit", func(t *testing.T) {
		srv := startFake(t, fakeCfg{hang: map[string]bool{"RCPT": true}})
		set := srv.settings("plain")
		set.AllowPlaintext = true
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		err := NewSMTPSender(set).Send(ctx, testMsg())
		wantErr(t, err, StageRecipient, ClassTimeout, true)
	})
	t.Run("a cancelled context is canceled, not a timeout", func(t *testing.T) {
		srv := startFake(t, fakeCfg{hang: map[string]bool{"MAIL": true}})
		set := srv.settings("plain")
		set.AllowPlaintext = true
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(200*time.Millisecond, cancel)
		err := NewSMTPSender(set).Send(ctx, testMsg())
		wantErr(t, err, StageSender, ClassCanceled, true)
	})
	t.Run("a dial that never returns hits the dial deadline", func(t *testing.T) {
		set := Settings{Host: testHost, Port: 25, Security: "plain", AllowPlaintext: true, From: "a@example.com",
			DialTimeout: 150 * time.Millisecond,
			Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
				<-ctx.Done()
				return nil, ctx.Err()
			}}
		wantErr(t, sendWith(t, set, testMsg()), StageConnect, ClassTimeout, true)
	})
}

func TestConnectFailures(t *testing.T) {
	t.Run("refused", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		ln.Close() // nothing listens any more
		set := Settings{Host: testHost, Port: 25, Security: "plain", AllowPlaintext: true, From: "a@example.com",
			Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, network, addr)
			}}
		wantErr(t, sendWith(t, set, testMsg()), StageConnect, ClassRefused, true)
	})
	t.Run("name does not exist", func(t *testing.T) {
		set := Settings{Host: testHost, Port: 25, Security: "plain", AllowPlaintext: true, From: "a@example.com",
			Dial: func(context.Context, string, string) (net.Conn, error) {
				return nil, &net.DNSError{Err: "no such host LEAK-REPLY-TEXT", Name: testHost, IsNotFound: true}
			}}
		wantErr(t, sendWith(t, set, testMsg()), StageConnect, ClassDNS, false)
	})
	t.Run("lookup failure is temporary", func(t *testing.T) {
		set := Settings{Host: testHost, Port: 25, Security: "plain", AllowPlaintext: true, From: "a@example.com",
			Dial: func(context.Context, string, string) (net.Conn, error) {
				return nil, &net.DNSError{Err: "server misbehaving", Name: testHost, IsTemporary: true}
			}}
		wantErr(t, sendWith(t, set, testMsg()), StageConnect, ClassDNS, true)
	})
	t.Run("any other dial error", func(t *testing.T) {
		set := Settings{Host: testHost, Port: 25, Security: "plain", AllowPlaintext: true, From: "a@example.com",
			Dial: func(context.Context, string, string) (net.Conn, error) {
				return nil, errors.New("network is unreachable")
			}}
		wantErr(t, sendWith(t, set, testMsg()), StageConnect, ClassNetwork, true)
	})
}

func TestSettingsValidate(t *testing.T) {
	ok := Settings{Host: "mail.example.com", Port: 587, Security: "starttls", From: "a@example.com"}
	cases := []struct {
		name  string
		mod   func(*Settings)
		class Class // empty: valid
	}{
		{"valid", func(*Settings) {}, ""},
		{"no host", func(s *Settings) { s.Host = "" }, ClassNotConfigured},
		{"port 0", func(s *Settings) { s.Port = 0 }, ClassConfigInvalid},
		{"port too high", func(s *Settings) { s.Port = 70000 }, ClassConfigInvalid},
		{"security word", func(s *Settings) { s.Security = "ssl" }, ClassConfigInvalid},
		{"plain without opt-in", func(s *Settings) { s.Security = "plain" }, ClassPlaintextRefused},
		{"plain with opt-in", func(s *Settings) { s.Security, s.AllowPlaintext = "plain", true }, ""},
		{"plain with opt-in and a user", func(s *Settings) { s.Security, s.AllowPlaintext, s.Username = "plain", true, "u" }, ClassPlaintextRefused},
		{"helo with a line break", func(s *Settings) { s.HeloName = "a\r\nMAIL FROM:<x@y>" }, ClassConfigInvalid},
		{"from with a line break", func(s *Settings) { s.From = "a@example.com\r\nBcc: x@y" }, ClassConfigInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := ok
			tc.mod(&s)
			err := s.Validate()
			if tc.class == "" {
				if err != nil {
					t.Fatalf("want valid: %v", err)
				}
				return
			}
			wantErr(t, err, StageConfig, tc.class, false)
			// And a send reports the same, without dialling.
			s.Dial = func(context.Context, string, string) (net.Conn, error) {
				t.Error("dialled with invalid settings")
				return nil, errors.New("no")
			}
			wantErr(t, sendWith(t, s, testMsg()), StageConfig, tc.class, false)
		})
	}
}

func TestSendRefusesAnInvalidMessageBeforeDialling(t *testing.T) {
	srv := startFake(t, fakeCfg{})
	set := srv.settings("plain")
	set.AllowPlaintext = true
	for name, m := range map[string]Message{
		"subject injection":   {To: []string{"a@example.com"}, Subject: "x\r\nBcc: evil@example.net", Text: "t"},
		"recipient injection": {To: []string{"a@example.com\r\nRCPT TO:<evil@example.net>"}, Subject: "s", Text: "t"},
		"no recipient":        {Subject: "s", Text: "t"},
		"no text":             {To: []string{"a@example.com"}, Subject: "s"},
	} {
		err := sendWith(t, set, m)
		wantErr(t, err, StageConfig, ClassMessageInvalid, false)
		if strings.Contains(err.Error(), "evil") {
			t.Fatalf("%s: error echoes the value: %v", name, err)
		}
	}
	if srv.connCount() != 0 {
		t.Fatal("an invalid message caused a connection")
	}
}

func TestPasswordFileProblemsFailBeforeDialling(t *testing.T) {
	p := testPKI(t)
	srv := startFake(t, fakeCfg{cert: &p.good, startTLS: true, authMech: "PLAIN", user: "svc", pass: "pw"})
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty")
	two := filepath.Join(dir, "two")
	big := filepath.Join(dir, "big")
	os.WriteFile(empty, []byte("\n"), 0o600)
	os.WriteFile(two, []byte("one\ntwo\n"), 0o600)
	os.WriteFile(big, bytes.Repeat([]byte("x"), maxPasswordFile+1), 0o600)
	for name, path := range map[string]string{
		"unset":     "",
		"missing":   filepath.Join(dir, "absent"),
		"directory": dir,
		"empty":     empty,
		"two lines": two,
		"too large": big,
	} {
		set := srv.settings("starttls")
		set.Username, set.PasswordFile = "svc", path
		err := sendWith(t, set, testMsg())
		if err == nil {
			t.Fatalf("%s: accepted", name)
		}
		wantErr(t, err, StageConfig, ClassPasswordFile, false)
	}
	if srv.connCount() != 0 {
		t.Fatalf("an unusable password file still caused %d connections", srv.connCount())
	}
}

func TestPasswordFileTooOpenIsRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the Unix permission check cannot be exercised on Windows (modeTooOpen is covered by TestModeTooOpen)")
	}
	p := testPKI(t)
	srv := startFake(t, fakeCfg{cert: &p.good, startTLS: true, authMech: "PLAIN", user: "svc", pass: "pw"})
	for _, mode := range []os.FileMode{0o644, 0o640, 0o604, 0o660, 0o666} {
		path := pwFile(t, "pw")
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		set := srv.settings("starttls")
		set.Username, set.PasswordFile = "svc", path
		wantErr(t, sendWith(t, set, testMsg()), StageConfig, ClassPasswordFileMode, false)
	}
	if srv.connCount() != 0 {
		t.Fatal("an over-permissive password file was used")
	}
	for _, mode := range []os.FileMode{0o600, 0o400} {
		path := pwFile(t, "pw")
		os.Chmod(path, mode)
		set := srv.settings("starttls")
		set.Username, set.PasswordFile = "svc", path
		if err := sendWith(t, set, testMsg()); err != nil {
			t.Fatalf("mode %o: %v", mode, err)
		}
	}
}

func TestModeTooOpen(t *testing.T) {
	for m, want := range map[os.FileMode]bool{
		0o600: false, 0o400: false, 0o700: false, 0o640: true, 0o604: true, 0o644: true, 0o660: true, 0o666: true, 0o007: true,
	} {
		if got := modeTooOpen(m); got != want {
			t.Errorf("modeTooOpen(%o) = %v, want %v", m, got, want)
		}
	}
}

func TestReadPasswordFileTrimsOneLineEnd(t *testing.T) {
	for in, want := range map[string]string{
		"pw":           "pw",
		"pw\n":         "pw",
		"pw\r\n":       "pw",
		" pw with sp ": " pw with sp ",
		"p\tw\n":       "p\tw",
	} {
		got, err := ReadPasswordFile(pwFile(t, in))
		if err != nil || got != want {
			t.Errorf("ReadPasswordFile(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "\n", "a\nb", "a\r\nb\r\n", "a\rb", "a\x00b"} {
		if _, err := ReadPasswordFile(pwFile(t, bad)); err == nil {
			t.Errorf("ReadPasswordFile(%q) accepted", bad)
		}
	}
}

func TestRotatedPasswordIsPickedUpWithoutRestart(t *testing.T) {
	p := testPKI(t)
	srv := startFake(t, fakeCfg{cert: &p.good, startTLS: true, authMech: "PLAIN", user: "svc", pass: "first"})
	path := pwFile(t, "first\n")
	set := srv.settings("starttls")
	set.Username, set.PasswordFile = "svc", path
	snd := NewSMTPSender(set)
	if err := snd.Send(context.Background(), testMsg()); err != nil {
		t.Fatalf("first send: %v", err)
	}
	srv.setPass("second")
	wantErr(t, snd.Send(context.Background(), testMsg()), StageAuth, ClassAuthRejected, false)
	if err := os.WriteFile(path, []byte("second\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := snd.Send(context.Background(), testMsg()); err != nil {
		t.Fatalf("send after rotation: %v", err)
	}
}

func TestPasswordFileUnusedWithoutAUsername(t *testing.T) {
	srv := startFake(t, fakeCfg{})
	set := srv.settings("plain")
	set.AllowPlaintext = true
	set.PasswordFile = filepath.Join(t.TempDir(), "absent")
	if err := sendWith(t, set, testMsg()); err != nil {
		t.Fatalf("a password file with no user name must be ignored: %v", err)
	}
}

// Nothing a relay says, nobody's address, no host, user name, password or path
// reaches an error string, a hint or a log line.
func TestNoReplyTextAddressOrPasswordInErrorsOrLogs(t *testing.T) {
	p := testPKI(t)
	const pass = "hunter2-LEAKPW"
	rcpt := "patient-rcpt@example.org"
	pwPath := pwFile(t, pass)
	secrets := []string{"LEAK-REPLY-TEXT", rcpt, "console@example.com", testHost, pass, "svc-user", pwPath, filepath.Base(pwPath), "127.0.0.1"}

	var logBuf bytes.Buffer
	log := logx.New(&logBuf, logx.LevelDebug)

	scenarios := map[string]fakeCfg{
		"sender":    {reply: map[string]string{"MAIL": "550 5.7.1 LEAK-REPLY-TEXT " + rcpt}},
		"recipient": {reply: map[string]string{"RCPT": "550 5.1.1 LEAK-REPLY-TEXT " + rcpt}},
		"data":      {reply: map[string]string{"DATAEND": "554 5.7.1 LEAK-REPLY-TEXT " + rcpt}},
		"greeting":  {greeting: "554 LEAK-REPLY-TEXT " + rcpt},
		"auth":      {cert: &p.good, startTLS: true, authMech: "PLAIN", user: "svc-user", pass: "different"},
		"starttls":  {cert: &p.good, startTLS: true, reply: map[string]string{"STARTTLS": "454 LEAK-REPLY-TEXT " + rcpt}},
		"cert":      {cert: &p.wrongName, startTLS: true},
		"no tls":    {authMech: "PLAIN"},
		"success":   {},
	}
	var seen []string
	for name, cfg := range scenarios {
		srv := startFake(t, cfg)
		sec := "plain"
		if cfg.startTLS || name == "no tls" {
			sec = "starttls"
		}
		set := srv.settings(sec)
		set.AllowPlaintext = sec == "plain"
		set.Log = log
		if name == "auth" {
			set.Username, set.PasswordFile = "svc-user", pwPath
		}
		m := testMsg()
		m.To = []string{rcpt}
		err := NewSMTPSender(set).Send(context.Background(), m)
		if name == "success" {
			if err != nil {
				t.Fatalf("success scenario: %v", err)
			}
			continue
		}
		if err == nil {
			t.Fatalf("%s: expected a failure", name)
		}
		var se *SendError
		if !errors.As(err, &se) {
			t.Fatalf("%s: not a *SendError: %v", name, err)
		}
		seen = append(seen, err.Error(), se.Hint())
	}
	// Plus the failures that never reach the relay.
	for _, set := range []Settings{
		{Host: testHost, Port: 25, Security: "plain", From: "console@example.com", Log: log},
		{Host: testHost, Port: 587, Security: "starttls", Username: "svc-user", PasswordFile: pwPath + ".absent", From: "console@example.com", Log: log},
		{Log: log},
	} {
		err := NewSMTPSender(set).Send(context.Background(), testMsg())
		seen = append(seen, err.Error())
	}
	for _, s := range seen {
		for _, secret := range secrets {
			if strings.Contains(s, secret) {
				t.Errorf("error or hint %q contains %q", s, secret)
			}
		}
	}
	logs := logBuf.String()
	if !strings.Contains(logs, "mail send failed") || !strings.Contains(logs, "recipient_rejected") || !strings.Contains(logs, "mail sent") {
		t.Fatalf("the sender must log failures by class and successes by count; log:\n%s", logs)
	}
	for _, secret := range secrets {
		if strings.Contains(logs, secret) {
			t.Errorf("a log line contains %q:\n%s", secret, logs)
		}
	}
}

func TestEveryClassHasAHintAndIsUnique(t *testing.T) {
	seen := map[Class]bool{}
	for _, c := range Classes() {
		if seen[c] {
			t.Errorf("class %q listed twice", c)
		}
		seen[c] = true
		if Hint(c) == "" {
			t.Errorf("class %q has no hint", c)
		}
		if strings.ContainsAny(string(c), " \t.@:/") {
			t.Errorf("class %q is not a plain word", c)
		}
	}
	if Hint("nonsense") != "" {
		t.Error("an unknown class must have no hint")
	}
}

func TestSendErrorTextIsWordsOnly(t *testing.T) {
	e := &SendError{Stage: StageRecipient, Class: ClassRecipientRejected, Temporary: true}
	if got := e.Error(); got != "mail: send failed at recipient: recipient_rejected (temporary)" {
		t.Fatalf("Error() = %q", got)
	}
	e.Temporary = false
	if got := e.Error(); strings.Contains(got, "temporary") {
		t.Fatalf("Error() = %q", got)
	}
}

func TestSettingsFromConfig(t *testing.T) {
	// SettingsFromConfig is a field-for-field copy; the test keeps it honest when
	// the config grows a key.
	set := SettingsFromConfig(config.SMTP{Host: "mail.example.com", Port: 25, Security: "plain", AllowPlaintext: true,
		Username: "u", PasswordFile: "/run/secrets/smtp", From: "a@example.com"})
	if set.Host != "mail.example.com" || set.Port != 25 || set.Security != "plain" || !set.AllowPlaintext ||
		set.Username != "u" || set.PasswordFile != "/run/secrets/smtp" || set.From != "a@example.com" {
		t.Fatalf("SettingsFromConfig = %+v", set)
	}
	if !set.Configured() || (Settings{}).Configured() {
		t.Fatal("Configured")
	}
}
