package probe

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/logx"
)

// agentAPI answers like the console's agent API does to a request without a token.
func agentAPI(w http.ResponseWriter, r *http.Request) {
	e := api.NewError(api.CodeUnauthorized)
	w.Header().Set("Content-Type", api.ContentTypeProblem)
	w.Header().Set(api.HeaderServerTime, api.FormatServerTime(time.Now()))
	w.WriteHeader(e.Status)
	_ = jsonEncode(w, e)
}

func jsonEncode(w http.ResponseWriter, e *api.Error) error {
	b := `{"type":"` + e.Type + `","title":"` + e.Title + `","status":401,"code":"` + e.Code + `"}`
	_, err := w.Write([]byte(b))
	return err
}

type cert struct {
	tls  tls.Certificate
	x509 *x509.Certificate
	pem  []byte
}

// newCert makes a self-signed server certificate for the names, valid until notAfter.
func newCert(t *testing.T, notAfter time.Time, dns ...string) cert {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "probe test"},
		NotBefore: time.Now().Add(-48 * time.Hour), NotAfter: notAfter, DNSNames: dns, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true, IsCA: true}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	return cert{tls: tls.Certificate{Certificate: [][]byte{der}, PrivateKey: k}, x509: c, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

func tlsServer(t *testing.T, c cert, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	s := httptest.NewUnstartedServer(h)
	s.TLS = &tls.Config{Certificates: []tls.Certificate{c.tls}}
	s.StartTLS()
	t.Cleanup(s.Close)
	return s
}

func roots(c cert) *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(c.x509)
	return p
}

// prober targets https://agents.test:<port of srv> with name resolution faked to 127.0.0.1.
func prober(t *testing.T, srv *httptest.Server, scheme string, clock func() time.Time, log *logx.Logger) *Prober {
	t.Helper()
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	p := New(Settings{PublicURL: scheme + "://agents.test:" + port}, clock, log)
	p.Lookup = func(context.Context, string) ([]string, error) { return []string{"127.0.0.1"}, nil }
	return p
}

func TestStagesFailAndPass(t *testing.T) {
	now := time.Now()
	good := newCert(t, now.Add(90*24*time.Hour), "agents.test")
	wrongName := newCert(t, now.Add(90*24*time.Hour), "other.test")

	t.Run("healthy", func(t *testing.T) {
		p := prober(t, tlsServer(t, good, agentAPI), "https", time.Now, nil)
		p.Roots = roots(good)
		r := p.Once(context.Background())
		if !r.OK || r.NotAfter.IsZero() {
			t.Fatalf("healthy endpoint: %+v", r)
		}
		if st := p.Status(); !st.Known || st.Failures != 0 || !st.LastOK.Equal(r.At) {
			t.Fatalf("status: %+v", st)
		}
	})
	t.Run("rate limited is still this console", func(t *testing.T) {
		p := prober(t, tlsServer(t, good, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", api.ContentTypeProblem+"; charset=utf-8")
			w.Header().Set(api.HeaderServerTime, api.FormatServerTime(time.Now()))
			w.WriteHeader(429)
			_, _ = w.Write([]byte(`{"type":"urn:cryobackup:error:rate_limited","title":"x","status":429,"code":"rate_limited","retry_after_s":30}`))
		}), "https", time.Now, nil)
		p.Roots = roots(good)
		if r := p.Once(context.Background()); !r.OK {
			t.Fatalf("429 problem document: %+v", r)
		}
	})

	cases := []struct {
		name  string
		setup func(t *testing.T) *Prober
		stage Stage
		class string
	}{
		{"dns nxdomain", func(t *testing.T) *Prober {
			p := prober(t, tlsServer(t, good, agentAPI), "https", time.Now, nil)
			p.Lookup = func(context.Context, string) ([]string, error) {
				return nil, &net.DNSError{Err: "no such host", Name: "agents.test", IsNotFound: true}
			}
			return p
		}, StageDNS, "nxdomain"},
		{"dns timeout", func(t *testing.T) *Prober {
			p := prober(t, tlsServer(t, good, agentAPI), "https", time.Now, nil)
			p.Lookup = func(ctx context.Context, _ string) ([]string, error) { <-ctx.Done(); return nil, ctx.Err() }
			p.StageTimeout = 100 * time.Millisecond
			return p
		}, StageDNS, "timeout"},
		{"connect refused", func(t *testing.T) *Prober {
			srv := tlsServer(t, good, agentAPI)
			p := prober(t, srv, "https", time.Now, nil)
			srv.Close()
			return p
		}, StageConnect, "refused"},
		{"tls untrusted", func(t *testing.T) *Prober {
			p := prober(t, tlsServer(t, good, agentAPI), "https", time.Now, nil)
			p.Roots = x509.NewCertPool() // trusts nothing
			return p
		}, StageTLS, "untrusted"},
		{"tls wrong host name", func(t *testing.T) *Prober {
			p := prober(t, tlsServer(t, wrongName, agentAPI), "https", time.Now, nil)
			p.Roots = roots(wrongName)
			return p
		}, StageTLS, "hostname"},
		{"tls not spoken", func(t *testing.T) *Prober {
			srv := httptest.NewServer(http.HandlerFunc(agentAPI))
			t.Cleanup(srv.Close)
			return prober(t, srv, "https", time.Now, nil)
		}, StageTLS, "handshake"},
		{"certificate expired", func(t *testing.T) *Prober {
			p := prober(t, tlsServer(t, good, agentAPI), "https", func() time.Time { return now.Add(91 * 24 * time.Hour) }, nil)
			p.Roots = roots(good)
			return p
		}, StageCertExpired, "expired"},
		{"wrong responder: proxy error page", func(t *testing.T) *Prober {
			p := prober(t, tlsServer(t, good, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "bad gateway", 502) }), "https", time.Now, nil)
			p.Roots = roots(good)
			return p
		}, StageResponder, "status_502"},
		{"wrong responder: a 401 that is not ours", func(t *testing.T) *Prober {
			p := prober(t, tlsServer(t, good, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "unauthorized", 401) }), "https", time.Now, nil)
			p.Roots = roots(good)
			return p
		}, StageResponder, "content_type"},
		{"wrong responder: no server time", func(t *testing.T) *Prober {
			p := prober(t, tlsServer(t, good, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", api.ContentTypeProblem)
				w.WriteHeader(401)
				_, _ = w.Write([]byte(`{"type":"urn:cryobackup:error:unauthorized","status":401,"code":"unauthorized"}`))
			}), "https", time.Now, nil)
			p.Roots = roots(good)
			return p
		}, StageResponder, "server_time"},
		{"wrong responder: another problem document", func(t *testing.T) *Prober {
			p := prober(t, tlsServer(t, good, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", api.ContentTypeProblem)
				w.Header().Set(api.HeaderServerTime, api.FormatServerTime(time.Now()))
				w.WriteHeader(401)
				_, _ = w.Write([]byte(`{"type":"about:blank","status":401,"code":"denied"}`))
			}), "https", time.Now, nil)
			p.Roots = roots(good)
			return p
		}, StageResponder, "body"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := c.setup(t)
			r := p.Once(context.Background())
			if r.OK || r.Stage != c.stage || r.Class != c.class {
				t.Fatalf("got %+v, want stage %s class %s", r, c.stage, c.class)
			}
		})
	}
}

func TestPlainHTTPProbesTheResponder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(agentAPI))
	t.Cleanup(srv.Close)
	p := prober(t, srv, "http", time.Now, nil)
	if st := p.Status(); !st.Runs || st.Limit != LimitPlainHTTP {
		t.Fatalf("plain http status: %+v", st)
	}
	if r := p.Once(context.Background()); !r.OK || !r.NotAfter.IsZero() {
		t.Fatalf("plain http probe: %+v", r)
	}
}

func TestConnectAddressAndIPLiteral(t *testing.T) {
	good := newCert(t, time.Now().Add(30*24*time.Hour), "agents.test")
	srv := tlsServer(t, good, agentAPI)
	// connect_address: DNS still resolves the public name, the dial goes elsewhere.
	p := New(Settings{PublicURL: "https://agents.test:1", ConnectAddress: srv.Listener.Addr().String()}, time.Now, nil)
	var looked string
	p.Lookup = func(_ context.Context, h string) ([]string, error) { looked = h; return []string{"192.0.2.1"}, nil }
	p.Roots = roots(good)
	if r := p.Once(context.Background()); !r.OK || looked != "agents.test" {
		t.Fatalf("connect_address: %+v (looked up %q)", r, looked)
	}
	// An IP literal skips DNS and is verified against the IP SAN.
	p = New(Settings{PublicURL: "https://" + srv.Listener.Addr().String()}, time.Now, nil)
	p.Lookup = func(context.Context, string) ([]string, error) { t.Fatal("no DNS for an IP literal"); return nil, nil }
	p.Roots = roots(good)
	if r := p.Once(context.Background()); !r.OK {
		t.Fatalf("ip literal: %+v", r)
	}
}

func TestNotConfigured(t *testing.T) {
	p := New(Settings{}, time.Now, nil)
	if st := p.Status(); st.Runs || st.Limit != LimitNoPublicURL {
		t.Fatalf("no public_url: %+v", st)
	}
	done := make(chan struct{})
	go func() { p.Run(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run must return at once when the probe cannot run")
	}
	dir := t.TempDir()
	bad := filepath.Join(dir, "ca.pem")
	_ = os.WriteFile(bad, []byte("not a certificate"), 0o600)
	for _, f := range []string{bad, filepath.Join(dir, "absent.pem")} {
		if st := New(Settings{PublicURL: "https://agents.test", CAFile: f}, time.Now, nil).Status(); st.Runs || st.Limit != LimitCAFile {
			t.Fatalf("unusable ca_file: %+v", st)
		}
	}
	good := newCert(t, time.Now().Add(time.Hour), "agents.test")
	ca := filepath.Join(dir, "good.pem")
	_ = os.WriteFile(ca, good.pem, 0o600)
	if p := New(Settings{PublicURL: "https://agents.test", CAFile: ca}, time.Now, nil); !p.Status().Runs || p.Roots == nil {
		t.Fatalf("usable ca_file: %+v", p.Status())
	}
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// The streak, the clear, and logging: one line per change, no host, no path.
func TestStreakClearAndLogs(t *testing.T) {
	good := newCert(t, time.Now().Add(90*24*time.Hour), "agents.test")
	var logs syncBuf
	clk := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return clk }
	advance := func(d time.Duration) { mu.Lock(); clk = clk.Add(d); mu.Unlock() }
	srv := tlsServer(t, good, agentAPI)
	p := prober(t, srv, "https", clock, logx.New(&logs, logx.LevelDebug))
	p.Roots = roots(good)
	down := errors.New("down")
	p.Lookup = func(context.Context, string) ([]string, error) { return nil, down }
	p.Once(context.Background())
	first := p.Status().FailingSince
	advance(3 * time.Minute)
	p.Once(context.Background())
	st := p.Status()
	if st.Failures != 2 || !st.FailingSince.Equal(first) || st.Last.Stage != StageDNS {
		t.Fatalf("streak: %+v", st)
	}
	p.Lookup = func(context.Context, string) ([]string, error) { return []string{"127.0.0.1"}, nil }
	advance(3 * time.Minute)
	p.Once(context.Background())
	if st = p.Status(); st.Failures != 0 || !st.FailingSince.IsZero() || !st.Last.OK {
		t.Fatalf("clear: %+v", st)
	}
	out := logs.String()
	if n := strings.Count(out, "agent endpoint probe failing"); n != 1 {
		t.Fatalf("one failing line per change, got %d:\n%s", n, out)
	}
	if !strings.Contains(out, "agent endpoint probe passing") || strings.Contains(out, "agents.test") || strings.Contains(out, "127.0.0.1") {
		t.Fatalf("logs:\n%s", out)
	}
}

// Run probes at once, keeps going on the ticker and stops with the context.
func TestRunProbesAtOnceAndStops(t *testing.T) {
	good := newCert(t, time.Now().Add(90*24*time.Hour), "agents.test")
	srv := tlsServer(t, good, agentAPI)
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	p := New(Settings{PublicURL: "https://agents.test:" + port, Interval: 50 * time.Millisecond}, time.Now, nil)
	var mu sync.Mutex
	calls := 0
	p.Lookup = func(context.Context, string) ([]string, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return []string{"127.0.0.1"}, nil
	}
	p.Roots = roots(good)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		mu.Lock()
		n := calls
		mu.Unlock()
		if n >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Run did not keep probing")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if st := p.Status(); !st.Known || !st.Last.OK || st.Started.IsZero() {
		t.Fatalf("status after Run: %+v", st)
	}
}
