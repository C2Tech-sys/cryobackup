package cli

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/config"
)

func writeCertPair(t *testing.T, dir string) (certPath, keyPath string) {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalECPrivateKey(k)
	certPath, keyPath = filepath.Join(dir, "a.crt"), filepath.Join(dir, "a.key")
	_ = os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	_ = os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600)
	return
}

// TestDoctorAdminTLS covers the admin-TLS line of cryo-console doctor: on, off
// on loopback (fine), off on a non-loopback bind (warn, including a mesh
// address), and the two broken shapes (fail).
func TestDoctorAdminTLS(t *testing.T) {
	dir := t.TempDir()
	cert, key := writeCertPair(t, dir)
	bad := filepath.Join(dir, "bad.pem")
	_ = os.WriteFile(bad, []byte("not a pem"), 0o600)

	cases := []struct {
		name   string
		listen string
		tls    config.TLS
		level  string
		detail string
	}{
		{"on", "127.0.0.1:8080", config.TLS{Cert: cert, Key: key}, "OK", "on"},
		{"on at a mesh address", "100.100.10.20:8080", config.TLS{Cert: cert, Key: key}, "OK", "on"},
		{"off on loopback v4", "127.0.0.1:8080", config.TLS{}, "OK", "off"},
		{"off on loopback v6", "[::1]:8080", config.TLS{}, "OK", "off"},
		{"off on localhost", "localhost:8080", config.TLS{}, "OK", "off"},
		{"off on a mesh address", "100.100.10.20:8080", config.TLS{}, "WARN", "not loopback"},
		{"off on a private address", "10.0.0.5:8080", config.TLS{}, "WARN", "not loopback"},
		{"off on all interfaces", "0.0.0.0:8080", config.TLS{}, "WARN", "not loopback"},
		{"unloadable pair", "127.0.0.1:8080", config.TLS{Cert: bad, Key: bad}, "FAIL", "do not load"},
		{"cert without key", "127.0.0.1:8080", config.TLS{Cert: cert}, "FAIL", "together"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.AdminListen, cfg.Admin.TLS = tc.listen, tc.tls
			var level, name, detail string
			calls := 0
			checkAdminTLS(cfg, func(l, n, d string) { level, name, detail = l, n, d; calls++ })
			if calls != 1 || name != "admin tls" || level != tc.level || !strings.Contains(detail, tc.detail) {
				t.Fatalf("got %d report(s): %s %s %q; want one %s containing %q", calls, level, name, detail, tc.level, tc.detail)
			}
			if strings.Contains(detail, dir) {
				t.Fatalf("doctor must not print paths: %q", detail)
			}
		})
	}
}

// TestDoctorPrintsTheAdminTLSLine runs the real command: the line is there, and
// a non-loopback admin bind without TLS is a WARN that does not by itself fail
// doctor.
func TestDoctorPrintsTheAdminTLSLine(t *testing.T) {
	dir := t.TempDir()
	if code, _, e := run(t, "", initArgs(dir, "--master-key-copy-confirmed", "--admin-listen", "127.0.0.1:0", "--agent-listen", "127.0.0.1:0")...); code != 0 {
		t.Fatal(e)
	}
	_, out, _ := run(t, "", "doctor", "--config", filepath.Join(dir, "config.json"))
	if !strings.Contains(out, "OK    admin tls") || !strings.Contains(out, "off (admin_listen is loopback)") {
		t.Fatalf("doctor lacks the admin tls line:\n%s", out)
	}
}

// serveFixturesWithConfig is serveFixtures with a --config file (the fixtures
// console reads admin.tls from it).
func serveFixturesWithConfig(t *testing.T, cfgPath string) (int, string, string) {
	t.Helper()
	t.Setenv("CRYO_FIXTURES_PASSWORD", "fixtures-pass-1")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	old := serveContext
	serveContext = func() (context.Context, context.CancelFunc) { return ctx, cancel }
	t.Cleanup(func() { serveContext = old })
	out, errb := &stopOnBanner{cancel: cancel}, &bytes.Buffer{}
	code := Main([]string{"serve", "--fixtures", "--config", cfgPath, "--admin-addr", "127.0.0.1:0"}, IO{Stdin: strings.NewReader(""), Stdout: out, Stderr: errb})
	return code, out.String(), errb.String()
}

// TestServeBannerUsesTheAdminScheme: the printed admin and design-check URLs say
// https:// exactly when the admin listener serves TLS.
func TestServeBannerUsesTheAdminScheme(t *testing.T) {
	cert, key := writeCertPair(t, t.TempDir())
	withTLS, _ := json.Marshal(map[string]any{"admin": map[string]any{"tls": map[string]string{"cert": cert, "key": key}}})
	for _, tc := range []struct {
		name, cfg, scheme, other string
	}{
		{"tls on", string(withTLS), "https", "http"},
		{"tls off", `{}`, "http", "https"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "c.json")
			_ = os.WriteFile(p, []byte(tc.cfg), 0o600)
			code, out, errb := serveFixturesWithConfig(t, p)
			if code != ExitOK {
				t.Fatalf("serve: %d stdout=%q stderr=%q", code, out, errb)
			}
			for _, line := range []string{"admin listener: ", "design check page: "} {
				if !strings.Contains(out, line+tc.scheme+"://127.0.0.1:") || strings.Contains(out, line+tc.other+"://") {
					t.Fatalf("%q must use %s://:\n%s", line, tc.scheme, out)
				}
			}
		})
	}
}

// TestInitWritesTheAdminTLSBlock covers --admin-tls-cert and --admin-tls-key.
func TestInitWritesTheAdminTLSBlock(t *testing.T) {
	dir := t.TempDir()
	code, out, errb := run(t, "", initArgs(dir, "--master-key-copy-confirmed", "--admin-tls-cert", "/tls/admin.crt", "--admin-tls-key", "/tls/admin.key")...)
	if code != 0 {
		t.Fatalf("init: %d\n%s\n%s", code, out, errb)
	}
	cfg, err := config.Read(filepath.Join(dir, "config.json"), func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Admin.TLS.Cert != "/tls/admin.crt" || cfg.Admin.TLS.Key != "/tls/admin.key" {
		t.Fatalf("admin.tls not written: %+v", cfg.Admin)
	}

	// One half is a usage error, and nothing is written.
	for _, half := range [][]string{{"--admin-tls-cert", "/tls/admin.crt"}, {"--admin-tls-key", "/tls/admin.key"}} {
		d := t.TempDir()
		code, _, errb := run(t, "", initArgs(d, append([]string{"--master-key-copy-confirmed"}, half...)...)...)
		if code != ExitUsage || !strings.Contains(errb, "together") {
			t.Fatalf("half %v: %d %s", half, code, errb)
		}
		if fileExists(filepath.Join(d, "config.json")) {
			t.Fatalf("half %v: config.json was written", half)
		}
	}
}
