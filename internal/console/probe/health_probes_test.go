package probe

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// §4.8 C06/C07 inputs: the gateway probe as the reserved probe login. 200 and
// 404 are healthy, 401, a 5xx, a TLS failure and a timeout are failures, and
// the certificate's notAfter is recorded (C05 gateway half).
func TestC06GatewayProbeOutcomes(t *testing.T) {
	notAfter := time.Now().Add(40 * 24 * time.Hour).UTC().Truncate(time.Second)
	good := newCert(t, notAfter, "gateway.test")
	var gotUser, gotPass, gotPath string
	status := http.StatusNotFound
	srv := tlsServer(t, good, func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPass, _ = r.BasicAuth()
		gotPath = r.URL.Path
		w.WriteHeader(status)
	})
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	s := GatewaySettings{URL: "https://gateway.test:" + port, Roots: roots(good), Timeout: 3 * time.Second,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, srv.Listener.Addr().String())
		}}
	for _, c := range []struct {
		status int
		ok     bool
		class  string
	}{{404, true, ""}, {200, true, ""}, {401, false, "unauthorized"}, {403, false, "unauthorized"}, {502, false, "status_502"}, {500, false, "status_500"}} {
		status = c.status
		o := CheckGateway(context.Background(), s, "probe", "pw-123")
		if o.OK != c.ok || o.Class != c.class {
			t.Errorf("status %d: %+v", c.status, o)
		}
		if !o.NotAfter.Equal(notAfter) {
			t.Errorf("status %d: notAfter %v, want %v", c.status, o.NotAfter, notAfter)
		}
	}
	if gotUser != "probe" || gotPass != "pw-123" || gotPath != GatewayProbePath {
		t.Fatalf("request: user %q path %q", gotUser, gotPath)
	}

	t.Run("untrusted certificate is a failure", func(t *testing.T) {
		u := s
		u.Roots = nil
		if o := CheckGateway(context.Background(), u, "probe", "pw"); o.OK || o.Class != "untrusted" {
			t.Fatalf("%+v", o)
		}
	})
	t.Run("timeout is a failure", func(t *testing.T) {
		slow := tlsServer(t, good, func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		})
		u := s
		u.Timeout = 300 * time.Millisecond
		u.Dial = func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, slow.Listener.Addr().String())
		}
		if o := CheckGateway(context.Background(), u, "probe", "pw"); o.OK || o.Class != "timeout" {
			t.Fatalf("%+v", o)
		}
	})
	t.Run("refused is a failure", func(t *testing.T) {
		ln, _ := net.Listen("tcp", "127.0.0.1:0")
		addr := ln.Addr().String()
		_ = ln.Close()
		u := s
		u.Dial = func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		}
		if o := CheckGateway(context.Background(), u, "probe", "pw"); o.OK || o.Class != "refused" {
			t.Fatalf("%+v", o)
		}
	})
}

// smtpViolations counts commands a probe must never send (MAIL, RCPT, DATA,
// AUTH or anything else beyond EHLO/HELO/QUIT); TestC09SMTPProbeOutcomes fails on any.
var smtpViolations atomic.Int64

// fakeSMTP serves one scripted SMTP session per connection.
func fakeSMTP(t *testing.T, greeting string, ehlo []string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				r := bufio.NewReader(c)
				_, _ = c.Write([]byte(greeting + "\r\n"))
				if !strings.HasPrefix(greeting, "220") {
					return
				}
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					switch cmd := strings.ToUpper(strings.Fields(line + " x")[0]); cmd {
					case "EHLO", "HELO":
						for i, l := range ehlo {
							sep := "-"
							if i == len(ehlo)-1 {
								sep = " "
							}
							_, _ = c.Write([]byte("250" + sep + l + "\r\n"))
						}
					case "QUIT":
						_, _ = c.Write([]byte("221 bye\r\n"))
						return
					default: // MAIL, RCPT, DATA, AUTH, ...: the probe sends no mail and never logs in
						smtpViolations.Add(1)
						_, _ = c.Write([]byte("502 no\r\n"))
					}
				}
			}(c)
		}
	}()
	return ln.Addr().String()
}

func smtpAt(addr, security string) SMTPSettings {
	h, p, _ := net.SplitHostPort(addr)
	port := 0
	for _, r := range p {
		port = port*10 + int(r-'0')
	}
	return SMTPSettings{Host: h, Port: port, Security: security, Timeout: 3 * time.Second}
}

// §4.8 C09/C10 inputs: the SMTP probe connects and says EHLO, sends no mail,
// and fails on a refusing relay, a 5xx greeting or a relay that cannot STARTTLS.
func TestC09SMTPProbeOutcomes(t *testing.T) {
	if o := CheckSMTP(context.Background(), SMTPSettings{}); o.OK || o.Class != "not_configured" {
		t.Fatalf("unconfigured: %+v", o)
	}
	ok := fakeSMTP(t, "220 relay ready", []string{"relay", "8BITMIME"})
	if o := CheckSMTP(context.Background(), smtpAt(ok, "plain")); !o.OK {
		t.Fatalf("plain: %+v", o)
	}
	if o := CheckSMTP(context.Background(), smtpAt(ok, "starttls")); o.OK || o.Class != "starttls_unsupported" {
		t.Fatalf("starttls without the extension: %+v", o)
	}
	busy := fakeSMTP(t, "554 go away", nil)
	if o := CheckSMTP(context.Background(), smtpAt(busy, "plain")); o.OK || o.Class != "greeting_5xx" {
		t.Fatalf("5xx greeting: %+v", o)
	}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	closed := ln.Addr().String()
	_ = ln.Close()
	if o := CheckSMTP(context.Background(), smtpAt(closed, "plain")); o.OK || o.Class != "refused" {
		t.Fatalf("refused: %+v", o)
	}
	if n := smtpViolations.Load(); n != 0 {
		t.Fatalf("the SMTP probe sent %d commands beyond EHLO and QUIT (MAIL, RCPT, DATA or AUTH)", n)
	}
}

// M3 m2c: the SMTP probe is bounded. A relay that accepts the connection and
// never greets is a timeout, not a probe that hangs.
func TestC09SMTPProbeIsBounded(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close() // held open, silent
		}
	}()
	s := smtpAt(ln.Addr().String(), "plain")
	s.Timeout = 300 * time.Millisecond
	done := make(chan Outcome, 1)
	go func() { done <- CheckSMTP(context.Background(), s) }()
	select {
	case o := <-done:
		if o.OK || o.Class != "timeout" {
			t.Fatalf("silent relay: %+v", o)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the SMTP probe outlived its bound")
	}
}

// ADR 0013 bounds a whole run at TotalTimeout: the responder's own stage
// deadline never reaches past the run's deadline (before M3-T2 it was set to
// now plus a full stage timeout, so a slow handshake stretched the run).
func TestC11ResponderNeverOutlivesTheRunDeadline(t *testing.T) {
	good := newCert(t, time.Now().Add(90*24*time.Hour), "agents.test")
	srv := tlsServer(t, good, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	})
	p := prober(t, srv, "https", time.Now, nil)
	p.Roots = roots(good)
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	start := time.Now()
	r := p.check(ctx) // the stage timeout is 10 s; the run has 0.7 s
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("the responder ran %v past a 0.7 s run deadline", took)
	}
	if r.OK || r.Stage != StageResponder || r.Class != "timeout" {
		t.Fatalf("%+v", r)
	}
}

func TestDiskSpaceOfATempDir(t *testing.T) {
	free, total, err := DiskSpace(t.TempDir())
	if err != nil || total <= 0 || free < 0 || free > total {
		t.Fatalf("free %d total %d err %v", free, total, err)
	}
}
