package server

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/config"
	"github.com/c2tech-sys/cryobackup/internal/logx"
)

// lockedBuf is a bytes.Buffer safe for the server goroutines that log into it.
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func (l *lockedBuf) records(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(l.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line is not JSON: %q", line)
		}
		out = append(out, m)
	}
	return out
}

func TestClassifyNetHTTPMessages(t *testing.T) {
	for _, tc := range []struct{ msg, class, kind string }{
		{"http: TLS handshake error from 203.0.113.9:51234: EOF\n", ClassTLSHandshakeFailed, "eof"},
		{"http: TLS handshake error from [2001:db8::1]:443: tls: first record does not look like a TLS handshake", ClassTLSHandshakeFailed, "not_tls"},
		{"http: TLS handshake error from 203.0.113.9:1: client sent an HTTP request to an HTTPS server", ClassTLSHandshakeFailed, "not_tls"},
		{"http: TLS handshake error from 198.51.100.7:1: remote error: tls: unknown certificate", ClassTLSHandshakeFailed, "unknown_certificate"},
		{"http: TLS handshake error from 198.51.100.7:1: remote error: tls: bad certificate", ClassTLSHandshakeFailed, "bad_certificate"},
		{"http: TLS handshake error from 198.51.100.7:1: remote error: tls: certificate expired", ClassTLSHandshakeFailed, "certificate_expired"},
		{"http: TLS handshake error from 198.51.100.7:1: remote error: tls: unknown certificate authority", ClassTLSHandshakeFailed, "unknown_ca"},
		{"http: TLS handshake error from 198.51.100.7:1: tls: no cipher suite supported by both client and server", ClassTLSHandshakeFailed, "no_shared_cipher"},
		{"http: TLS handshake error from 198.51.100.7:1: tls: client offered only unsupported versions: [302 301]", ClassTLSHandshakeFailed, "unsupported_version"},
		{"http: TLS handshake error from 198.51.100.7:1: tls: received unexpected handshake message of type *tls.clientKeyExchangeMsg", ClassTLSHandshakeFailed, "client_hello_malformed"},
		{"http: TLS handshake error from 198.51.100.7:1: tls: oversized record received with length 20527", ClassTLSHandshakeFailed, "client_hello_malformed"},
		{"http: TLS handshake error from 198.51.100.7:1: read tcp 10.0.0.1:8443->198.51.100.7:1: i/o timeout", ClassTLSHandshakeFailed, "timeout"},
		{"http: TLS handshake error from 198.51.100.7:1: read tcp 10.0.0.1:8443->198.51.100.7:1: read: connection reset by peer", ClassTLSHandshakeFailed, "reset"},
		{"http: TLS handshake error from 198.51.100.7:1: tls: no certificates configured", ClassTLSHandshakeFailed, "server_certificate"},
		{"http: TLS handshake error from 198.51.100.7:1: something nobody has seen", ClassTLSHandshakeFailed, "other"},
		{"http: TLS handshake error from 198.51.100.7:1", ClassTLSHandshakeFailed, "other"},
		// Text a peer shapes cannot pick a kind: only the reason's start counts.
		{"http: TLS handshake error from 198.51.100.7:1: tls: bogus EOF remote error: tls: bad certificate", ClassTLSHandshakeFailed, "other"},
		// And a message that merely mentions the phrase is not a handshake error.
		{"http: panic serving 1.2.3.4:5: TLS handshake error from x", ClassHTTPServerError, "panic"},
		{"http: Accept error: accept tcp [::]:8443: accept4: too many open files; retrying in 5ms", ClassHTTPServerError, "accept"},
		{"http: panic serving 127.0.0.1:1: boom\ngoroutine 1 [running]:", ClassHTTPServerError, "panic"},
		{"http: superfluous response.WriteHeader call from x", ClassHTTPServerError, "write_header"},
		{"http2: server: error reading preface from client 127.0.0.1:1: EOF", ClassHTTPServerError, "http2"},
		{"something else entirely", ClassHTTPServerError, "other"},
	} {
		class, kind := classify(tc.msg)
		if class != tc.class || kind != tc.kind {
			t.Errorf("classify(%.40q) = %s/%s, want %s/%s", tc.msg, class, kind, tc.class, tc.kind)
		}
	}
}

// A burst of handshake failures is counted exactly, logs one WARN per interval
// with the count since the last one, and never carries an address or text.
func TestErrorLogCountsAndRateLimits(t *testing.T) {
	var buf lockedBuf
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	e := newErrorLog(logx.New(&buf, logx.LevelInfo), "agent", func() time.Time { return now })
	l := e.Logger()
	for i := 0; i < 50; i++ {
		l.Printf("http: TLS handshake error from 203.0.113.9:%d: tls: first record does not look like a TLS handshake /etc/x", 40000+i)
	}
	if got := e.TLSHandshakeFailures(); got != 50 {
		t.Fatalf("counted %d handshake failures, want 50", got)
	}
	recs := buf.records(t)
	if len(recs) != 1 {
		t.Fatalf("%d WARN lines for one burst inside one interval, want 1:\n%s", len(recs), buf.String())
	}
	if recs[0]["level"] != "WARN" || recs[0]["class"] != ClassTLSHandshakeFailed || recs[0]["listener"] != "agent" || recs[0]["count"] != float64(1) {
		t.Fatalf("first record = %v", recs[0])
	}
	now = now.Add(errorLogInterval + time.Second)
	l.Printf("http: TLS handshake error from 203.0.113.9:1: tls: first record does not look like a TLS handshake")
	recs = buf.records(t)
	if len(recs) != 2 || recs[1]["count"] != float64(50) {
		t.Fatalf("second summary should carry the 50 events since the first: %v", recs)
	}
	for _, bad := range []string{"203.0.113.9", "first record", "<path:", "/etc/x", "http:"} {
		if strings.Contains(buf.String(), bad) {
			t.Errorf("the log carries %q:\n%s", bad, buf.String())
		}
	}
	// Other server errors are a separate typed class, counted apart.
	l.Printf("http: Accept error: accept tcp 10.0.0.1:8443: too many open files; retrying in 5ms")
	if e.ServerErrors() != 1 || e.TLSHandshakeFailures() != 51 {
		t.Fatalf("counters = %d handshake, %d other", e.TLSHandshakeFailures(), e.ServerErrors())
	}
	last := buf.records(t)[2]
	if last["class"] != ClassHTTPServerError || last["kind"] != "accept" {
		t.Fatalf("other-class record = %v", last)
	}
	if strings.Contains(buf.String(), "10.0.0.1") {
		t.Fatalf("an address reached the log:\n%s", buf.String())
	}
}

// End to end: scanners hitting a real TLS listener leave typed lines and a
// count, and not the raw net/http text or a bare <path:...> message.
func TestScannerHandshakesOnTheAgentListenerLogTyped(t *testing.T) {
	dir := t.TempDir()
	cert, key := writeSelfSigned(t, dir)
	cfg := config.Default()
	cfg.Resolve()
	cfg.AdminListen, cfg.AgentListen = "127.0.0.1:0", "127.0.0.1:0"
	cfg.TLS = config.TLS{Cert: cert, Key: key}
	var buf lockedBuf
	s, err := New(cfg, Options{Log: logx.New(&buf, logx.LevelDebug)})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Serve(ctx) }()

	const probes = 4
	for i := 0; i < probes; i++ {
		c, err := net.Dial("tcp", s.AgentAddr())
		if err != nil {
			t.Fatal(err)
		}
		_, _ = c.Write([]byte("GET /admin/config.php HTTP/1.1\r\nHost: x\r\n\r\n")) // plain HTTP on the TLS port
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, _ = c.Read(make([]byte, 256))
		_ = c.Close()
	}
	// A client that offers only TLS 1.1 is refused during the handshake as well.
	if c, err := tls.Dial("tcp", s.AgentAddr(), &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS10, MaxVersion: tls.VersionTLS11}); err == nil {
		_ = c.Close()
		t.Fatal("TLS 1.1 must be refused")
	}
	// A client that verifies the certificate (it is self-signed) hangs up with an alert: a real certificate problem.
	if c, err := tls.Dial("tcp", s.AgentAddr(), &tls.Config{MinVersion: tls.VersionTLS12}); err == nil {
		_ = c.Close()
		t.Fatal("a self-signed certificate must not verify")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if a, _ := s.TLSHandshakeFailures(); a >= probes+2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	agent, admin := s.TLSHandshakeFailures()
	if agent < probes+2 || admin != 0 {
		t.Fatalf("handshake failures: agent %d (want >= %d), admin %d", agent, probes+2, admin)
	}
	// The counter moves before the WARN line is written: wait for the line of the last probe.
	for until := time.Now().Add(5 * time.Second); time.Now().Before(until); time.Sleep(10 * time.Millisecond) {
		o := buf.String()
		if strings.Contains(o, `"kind":"unsupported_version"`) && (strings.Contains(o, `"kind":"bad_certificate"`) || strings.Contains(o, `"kind":"unknown_ca"`) ||
			strings.Contains(o, `"kind":"unknown_certificate"`) || strings.Contains(o, `"kind":"certificate_expired"`)) {
			break
		}
	}
	out := buf.String()
	if !strings.Contains(out, `"class":"`+ClassTLSHandshakeFailed+`"`) {
		t.Fatalf("no tls_handshake_failed record:\n%s", out)
	}
	// The reason kinds tell scanner noise from a real certificate problem.
	kinds := map[string]bool{}
	for _, rec := range buf.records(t) {
		if rec["class"] == ClassTLSHandshakeFailed && rec["level"] == "WARN" {
			kinds[rec["kind"].(string)] = true
		}
	}
	if !kinds["not_tls"] || !kinds["unsupported_version"] {
		t.Errorf("scanner kinds missing (want not_tls and unsupported_version): %v", kinds)
	}
	if !(kinds["bad_certificate"] || kinds["unknown_ca"] || kinds["unknown_certificate"] || kinds["certificate_expired"]) {
		t.Errorf("a client that rejects the certificate left no certificate kind: %v\n%s", kinds, out)
	}
	for _, bad := range []string{"127.0.0.1", "TLS handshake error", "<path:", "first record", "config.php"} {
		if strings.Contains(out, bad) {
			t.Errorf("the log carries %q:\n%s", bad, out)
		}
	}
	for _, rec := range buf.records(t) {
		if rec["level"] == "WARN" && rec["class"] == ClassTLSHandshakeFailed && rec["listener"] != "agent" {
			t.Errorf("warn from the wrong listener: %v", rec)
		}
	}
}

func writeSelfSigned(t *testing.T, dir string) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPath, keyPath = dir+"/c.pem", dir+"/k.pem"
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}
