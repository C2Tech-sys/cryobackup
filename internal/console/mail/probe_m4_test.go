package mail

import (
	"bytes"
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

// probeWith runs Probe and asserts the relay saw no envelope or message command, whatever the
// outcome: the probe never sends.
func probeWith(t *testing.T, srv *fakeSrv, set Settings) error {
	t.Helper()
	err := NewSMTPSender(set).Probe(context.Background())
	for _, v := range []string{"MAIL", "RCPT", "DATA"} {
		if srv != nil && srv.sawCmd(v) {
			t.Fatalf("the probe wrote %s", v)
		}
	}
	if srv != nil && len(srv.captured()) != 0 {
		t.Fatal("the relay captured a message from the probe")
	}
	return err
}

// waitQuit waits briefly for the fake relay to record QUIT (it reads the command on its own goroutine).
func waitQuit(t *testing.T, srv *fakeSrv) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !srv.sawCmd("QUIT") {
		if time.Now().After(deadline) {
			t.Fatal("the probe did not say QUIT")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestProbeSTARTTLSWithAuthSendsNothing(t *testing.T) {
	p := testPKI(t)
	srv := startFake(t, fakeCfg{cert: &p.good, startTLS: true, authMech: "PLAIN LOGIN", user: "svc", pass: "s3cret-pw"})
	set := srv.settings("starttls")
	set.Username, set.PasswordFile = "svc", pwFile(t, "s3cret-pw\n")
	if err := probeWith(t, srv, set); err != nil {
		t.Fatalf("probe: %v", err)
	}
	waitQuit(t, srv)
	if a := srv.authList(); len(a) != 1 || a[0] != "svc:s3cret-pw:true" {
		t.Fatalf("auth seen by the relay = %v (one login, over TLS)", a)
	}
	if !srv.sawCmd("STARTTLS") {
		t.Fatal("no STARTTLS")
	}
	if bytes.Contains(srv.rawBytes(), []byte("s3cret-pw")) {
		t.Fatal("the password crossed the clear socket")
	}
}

func TestProbeImplicitTLSAndPlain(t *testing.T) {
	p := testPKI(t)
	t.Run("implicit TLS with LOGIN", func(t *testing.T) {
		srv := startFake(t, fakeCfg{cert: &p.good, implicit: true, authMech: "LOGIN", user: "svc", pass: "pw-1"})
		set := srv.settings("tls")
		set.Username, set.PasswordFile = "svc", pwFile(t, "pw-1")
		if err := probeWith(t, srv, set); err != nil {
			t.Fatalf("probe: %v", err)
		}
		waitQuit(t, srv)
		if a := srv.authList(); len(a) != 1 || a[0] != "svc:pw-1:true" {
			t.Fatalf("auth = %v", a)
		}
	})
	t.Run("plain without credentials when allowed", func(t *testing.T) {
		srv := startFake(t, fakeCfg{})
		set := srv.settings("plain")
		set.AllowPlaintext = true
		if err := probeWith(t, srv, set); err != nil {
			t.Fatalf("probe: %v", err)
		}
		waitQuit(t, srv)
		if len(srv.authList()) != 0 {
			t.Fatal("the probe signed in without a user name")
		}
	})
}

// The probe and Send open a connection the same way: for every failure that happens before the
// envelope, both report the same stage, class and temporariness, and where the opening succeeds
// both succeed (both run session in sender.go; this pins the pair).
func TestProbeOpensAsDeliverDoes(t *testing.T) {
	p := testPKI(t)
	cases := []struct {
		name     string
		cfg      fakeCfg
		security string
		user     string
		pass     string
		mutate   func(*Settings)
		stage    Stage
		class    Class
		temp     bool
	}{
		{"starttls missing", fakeCfg{}, "starttls", "", "", nil, StageTLS, ClassStartTLSMissing, false},
		{"starttls refused", fakeCfg{cert: &p.good, startTLS: true, reply: map[string]string{"STARTTLS": "454 4.7.0 no LEAK-REPLY-TEXT"}}, "starttls", "", "", nil, StageTLS, ClassStartTLSRejected, true},
		{"wrong certificate name", fakeCfg{cert: &p.wrongName, startTLS: true}, "starttls", "", "", nil, StageTLS, ClassCertInvalid, false},
		{"untrusted certificate, implicit", fakeCfg{cert: &p.untrusted, implicit: true}, "tls", "", "", nil, StageTLS, ClassCertInvalid, false},
		{"plain-text relay on the TLS port", fakeCfg{}, "tls", "", "", nil, StageTLS, ClassTLSHandshake, true},
		{"bad credentials", fakeCfg{cert: &p.good, startTLS: true, authMech: "PLAIN", user: "svc", pass: "right"}, "starttls", "svc", "wrong", nil, StageAuth, ClassAuthRejected, false},
		{"no mechanism we use", fakeCfg{cert: &p.good, startTLS: true, authMech: "CRAM-MD5"}, "starttls", "svc", "pw", nil, StageAuth, ClassAuthUnsupported, false},
		{"greeting refused", fakeCfg{greeting: "554 5.3.2 go away LEAK-REPLY-TEXT"}, "plain", "", "", func(s *Settings) { s.AllowPlaintext = true }, StageConnect, ClassProtocol, false},
		{"not an SMTP server", fakeCfg{greeting: "HTTP/1.1 400 Bad Request LEAK-REPLY-TEXT"}, "plain", "", "", func(s *Settings) { s.AllowPlaintext = true }, StageConnect, ClassProtocol, false},
		{"EHLO refused", fakeCfg{reply: map[string]string{"EHLO": "500 5.5.1 no LEAK-REPLY-TEXT"}}, "plain", "", "", func(s *Settings) { s.AllowPlaintext = true }, StageConnect, ClassProtocol, false},
		{"silent relay", fakeCfg{noGreeting: true}, "plain", "", "", func(s *Settings) { s.AllowPlaintext = true; s.GreetingTimeout = 150 * time.Millisecond }, StageConnect, ClassTimeout, true},
		{"hangs on AUTH", fakeCfg{cert: &p.good, startTLS: true, authMech: "PLAIN", hang: map[string]bool{"AUTH": true}}, "starttls", "svc", "pw", func(s *Settings) { s.Timeout = 300 * time.Millisecond }, StageAuth, ClassTimeout, true},
		// Success paths (no class): both sign in, and Send delivers.
		{"STARTTLS with AUTH", fakeCfg{cert: &p.good, startTLS: true, authMech: "PLAIN LOGIN", user: "svc", pass: "pw"}, "starttls", "svc", "pw", nil, "", "", false},
		{"implicit TLS, bare EHLO last line, bare AUTH success", fakeCfg{cert: &p.good, implicit: true, authMech: "PLAIN",
			reply: map[string]string{"EHLO": "250-" + testHost + "\n250-AUTH PLAIN\n250", "AUTH": "235"}},
			"tls", "svc", "pw", func(s *Settings) { s.Timeout = 5 * time.Second }, "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var errs [2]error
			for i := range errs {
				srv := startFake(t, tc.cfg)
				set := srv.settings(tc.security)
				if tc.user != "" {
					set.Username, set.PasswordFile = tc.user, pwFile(t, tc.pass)
				}
				if tc.mutate != nil {
					tc.mutate(&set)
				}
				if i == 0 {
					errs[i] = probeWith(t, srv, set)
				} else {
					errs[i] = sendWith(t, set, testMsg())
					if tc.class == "" && len(srv.captured()) != 1 {
						t.Fatalf("send: the relay kept %d messages, want 1", len(srv.captured()))
					}
				}
			}
			if tc.class == "" {
				if errs[0] != nil || errs[1] != nil {
					t.Fatalf("probe: %v; send: %v; want both to succeed", errs[0], errs[1])
				}
				return
			}
			wantErr(t, errs[0], tc.stage, tc.class, tc.temp)
			wantErr(t, errs[1], tc.stage, tc.class, tc.temp)
		})
	}
}

// RFC 5321 4.2.1: the text of a reply is optional. The probe reads a bare code as a reply on
// every path a Send does (the filter of reply.go installed after NewClient and after STARTTLS):
// a bare "250" as the whole EHLO reply or as its last line, and a bare AUTH success code.
func TestProbeReadsBareReplyCodes(t *testing.T) {
	p := testPKI(t)
	for _, tc := range []struct {
		name     string
		cfg      fakeCfg
		security string
		user     string
	}{
		{"bare 250 as the whole EHLO reply", fakeCfg{reply: map[string]string{"EHLO": "250"}}, "plain", ""},
		{"multi-line EHLO with a bare last line", fakeCfg{reply: map[string]string{"EHLO": "250-" + testHost + "\n250-8BITMIME\n250"}}, "plain", ""},
		{"multi-line EHLO with a bare last line, implicit TLS and AUTH", fakeCfg{cert: &p.good, implicit: true,
			reply: map[string]string{"EHLO": "250-" + testHost + "\n250-AUTH PLAIN\n250"}, authMech: "PLAIN", user: "svc", pass: "pw"}, "tls", "svc"},
		{"bare AUTH success after STARTTLS", fakeCfg{cert: &p.good, startTLS: true, authMech: "PLAIN", reply: map[string]string{"AUTH": "235"}}, "starttls", "svc"},
		{"bare AUTH success, implicit TLS, LOGIN", fakeCfg{cert: &p.good, implicit: true, authMech: "LOGIN", reply: map[string]string{"AUTH": "235"}}, "tls", "svc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := startFake(t, tc.cfg)
			set := srv.settings(tc.security)
			set.AllowPlaintext = tc.security == "plain"
			if tc.user != "" {
				set.Username, set.PasswordFile = tc.user, pwFile(t, "pw")
			}
			// A reply net/textproto cannot end shows as a timeout within seconds.
			set.Timeout = 5 * time.Second
			if err := probeWith(t, srv, set); err != nil {
				t.Fatalf("probe: %v", err)
			}
			waitQuit(t, srv)
		})
	}
}

// Settings and password-file problems fail before any connection, as for Send.
func TestProbeChecksSettingsAndPasswordBeforeDialling(t *testing.T) {
	srv := startFake(t, fakeCfg{})
	bad := srv.settings("plain") // plain without allow_plaintext
	wantErr(t, probeWith(t, srv, bad), StageConfig, ClassPlaintextRefused, false)
	set := srv.settings("starttls")
	set.Username, set.PasswordFile = "svc", pwFile(t, "")
	wantErr(t, probeWith(t, srv, set), StageConfig, ClassPasswordFile, false)
	set.PasswordFile = ""
	wantErr(t, probeWith(t, srv, set), StageConfig, ClassPasswordFile, false)
	if n := srv.connCount(); n != 0 {
		t.Fatalf("the probe dialled %d times with unusable settings", n)
	}
}

// A relay that accepts the connection and says nothing ends the probe within its clocks: the
// greeting deadline, and the caller's context when that is shorter.
func TestProbeOfASilentRelayEndsInTime(t *testing.T) {
	srv := startFake(t, fakeCfg{noGreeting: true})
	set := srv.settings("starttls")
	set.GreetingTimeout = 200 * time.Millisecond
	start := time.Now()
	wantErr(t, probeWith(t, srv, set), StageConnect, ClassTimeout, true)
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("the probe of a silent relay took %v", d)
	}
	set.GreetingTimeout = time.Hour
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	start = time.Now()
	wantErr(t, NewSMTPSender(set).Probe(ctx), StageConnect, ClassTimeout, true)
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("the context did not bound the probe: %v", d)
	}
}

// Dial failures classify as for Send.
func TestProbeDialFailures(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	base := Settings{Host: testHost, Port: 25, Security: "plain", AllowPlaintext: true, From: "a@example.com"}
	refusedSet := base
	refusedSet.Dial = func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}
	wantErr(t, probeWith(t, nil, refusedSet), StageConnect, ClassRefused, true)
	dnsSet := base
	dnsSet.Dial = func(context.Context, string, string) (net.Conn, error) {
		return nil, &net.DNSError{Err: "no such host", Name: testHost, IsNotFound: true}
	}
	wantErr(t, probeWith(t, nil, dnsSet), StageConnect, ClassDNS, false)
}

// The probe's error carries words only: no reply text, user name, password or host.
func TestProbeErrorsAreWordsOnly(t *testing.T) {
	p := testPKI(t)
	srv := startFake(t, fakeCfg{cert: &p.good, startTLS: true, authMech: "PLAIN", user: "svc", pass: "right"})
	set := srv.settings("starttls")
	set.Username, set.PasswordFile = "USER-MARKER-7f3", pwFile(t, "PASS-MARKER-9c1")
	err := probeWith(t, srv, set)
	wantErr(t, err, StageAuth, ClassAuthRejected, false)
	var se *SendError
	_ = errors.As(err, &se)
	for _, s := range []string{err.Error(), se.Hint()} {
		for _, leak := range []string{"LEAK-REPLY-TEXT", "USER-MARKER-7f3", "PASS-MARKER-9c1", testHost, "535"} {
			if strings.Contains(s, leak) {
				t.Fatalf("%q carries %q", s, leak)
			}
		}
	}
}
