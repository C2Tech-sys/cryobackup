package server_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/cli"
	"github.com/c2tech-sys/cryobackup/internal/console/config"
	"github.com/c2tech-sys/cryobackup/internal/console/server"
)

// startAdminFixtures runs the fixtures console with the given admin TLS files
// (empty for plain HTTP) and returns the bound admin address and the server.
func startAdminFixtures(t *testing.T, cert, key string) (string, *server.Server) {
	t.Helper()
	cfg := config.Default()
	cfg.Resolve()
	cfg.Fixtures = true
	cfg.AdminListen = "127.0.0.1:0"
	cfg.Admin.TLS = config.TLS{Cert: cert, Key: key}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	app, err := cli.Build(context.Background(), cfg, cli.BuildOptions{FixturesPassword: "fixtures-pass-1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Server.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- app.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			t.Error("shutdown timed out")
		}
	})
	return app.Server.AdminAddr(), app.Server
}

// loginCookies signs in to the fixtures console at base with client and returns
// the Set-Cookie list of the redirect.
func loginCookies(t *testing.T, client *http.Client, base string) []*http.Cookie {
	t.Helper()
	res, err := client.PostForm(base+"/login", url.Values{"username": {"admin"}, "password": {"fixtures-pass-1"}})
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("login answered %d, want 303", res.StatusCode)
	}
	return res.Cookies()
}

// TestAdminListenerServesTLSAndMarksCookiesSecure is the M1-T7 proof: with
// admin.tls set the admin listener serves TLS 1.2+ and refuses older versions
// and plain HTTP, the real login over that connection returns Secure session and
// CSRF cookies (auth.secureConn keys off r.TLS) and a client with a jar keeps
// working; with admin.tls unset the listener is the same plain HTTP as before and
// the cookies are not Secure.
func TestAdminListenerServesTLSAndMarksCookiesSecure(t *testing.T) {
	cert, key := selfSigned(t, t.TempDir())
	addr, s := startAdminFixtures(t, cert, key)
	if !s.AdminTLSEnabled() {
		t.Fatal("AdminTLSEnabled must be true when admin.tls is set")
	}
	dial := func(min, max uint16) error {
		c, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true, MinVersion: min, MaxVersion: max})
		if err == nil {
			_ = c.Close()
		}
		return err
	}
	if err := dial(tls.VersionTLS12, tls.VersionTLS13); err != nil {
		t.Fatalf("TLS 1.2/1.3 must work: %v", err)
	}
	if err := dial(tls.VersionTLS10, tls.VersionTLS11); err == nil {
		t.Fatal("TLS 1.1 and older must be refused")
	}
	// Plain HTTP to the TLS port gets no application answer.
	if res, err := http.Get("http://" + addr + "/healthz"); err == nil {
		res.Body.Close()
		if res.StatusCode == http.StatusOK {
			t.Fatal("plain HTTP answered on the TLS admin listener")
		}
	}

	client := &http.Client{
		Transport:     &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	base := "https://" + addr
	res, err := client.Get(base + "/healthz")
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("healthz over TLS: %v %v", err, res)
	}
	res.Body.Close()
	var session, csrf *http.Cookie
	for _, c := range loginCookies(t, client, base) {
		switch c.Name {
		case auth.SessionCookie:
			session = c
		case auth.CSRFCookie:
			csrf = c
		}
	}
	if session == nil || csrf == nil {
		t.Fatalf("login over TLS set no session/csrf cookie pair ")
	}
	if !session.Secure || !csrf.Secure || !session.HttpOnly {
		t.Fatalf("over TLS: session Secure=%v HttpOnly=%v, csrf Secure=%v; want Secure and HttpOnly", session.Secure, session.HttpOnly, csrf.Secure)
	}
}

// TestAdminListenerWithoutTLSIsUnchanged pins the "unset" half of the ticket.
func TestAdminListenerWithoutTLSIsUnchanged(t *testing.T) {
	addr, s := startAdminFixtures(t, "", "")
	if s.AdminTLSEnabled() {
		t.Fatal("AdminTLSEnabled must be false when admin.tls is unset")
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	base := "http://" + addr
	res, err := client.Get(base + "/healthz")
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("healthz over plain HTTP: %v %v", err, res)
	}
	res.Body.Close()
	cookies := loginCookies(t, client, base)
	if len(cookies) < 2 {
		t.Fatalf("login set %d cookies", len(cookies))
	}
	for _, c := range cookies {
		if c.Secure {
			t.Fatalf("cookie %s is Secure on plain HTTP: a browser would never send it back", c.Name)
		}
	}
}

// TestTLSFlagsAreReadSafelyWhileServing: AdminTLSEnabled and TLSEnabled are
// called while Serve runs (serve.go reads them after starting the listeners).
// net/http assigns srv.TLSConfig itself inside Serve when it is nil (the HTTP/2
// setup of a plain listener), so an implementation that reads the live
// *http.Server field races with Serve and, once Serve has started, reports TLS
// on a plain listener. Run under -race (M1-FIX1 E17).
func TestTLSFlagsAreReadSafelyWhileServing(t *testing.T) {
	cfg := config.Default()
	cfg.Resolve()
	cfg.AdminListen, cfg.AgentListen, cfg.InsecureHTTPLab = "127.0.0.1:0", "127.0.0.1:0", true
	s, err := server.New(cfg, server.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	stopReaders := make(chan struct{})
	readersDone := make(chan struct{})
	var sawTLS bool
	go func() {
		defer close(readersDone)
		for {
			select {
			case <-stopReaders:
				return
			default:
			}
			if s.AdminTLSEnabled() || s.TLSEnabled() {
				sawTLS = true
			}
		}
	}()
	go func() { done <- s.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("shutdown timed out")
		}
	})

	// Both listeners are answering: Serve has finished setting them up.
	for _, u := range []string{"http://" + s.AdminAddr() + "/healthz", "http://" + s.AgentAddr() + "/x"} {
		res, err := http.Get(u)
		if err != nil {
			t.Fatalf("plain HTTP listener not serving: %v", err)
		}
		res.Body.Close()
	}
	close(stopReaders)
	<-readersDone
	if sawTLS {
		t.Error("a TLS flag read true on a plain listener while Serve ran")
	}
	if s.AdminTLSEnabled() || s.TLSEnabled() {
		t.Errorf("after Serve started: AdminTLSEnabled=%v TLSEnabled=%v on plain listeners, want false", s.AdminTLSEnabled(), s.TLSEnabled())
	}
}

func TestAdminTLSMisconfigurationFailsLoudly(t *testing.T) {
	dir := t.TempDir()
	cert, _ := selfSigned(t, dir)
	cfg := config.Default()
	cfg.Resolve()
	cfg.Fixtures = true
	cfg.AdminListen = "127.0.0.1:0"

	half := cfg
	half.Admin.TLS = config.TLS{Cert: cert}
	if _, err := server.New(half, server.Options{AgentDisabled: true}); err == nil {
		t.Fatal("admin.tls.cert without admin.tls.key must refuse to start, not serve plain HTTP")
	}
	if err := half.Validate(); err == nil {
		t.Fatal("Validate must reject a lone admin.tls.cert")
	}

	bad := cfg
	bad.Admin.TLS = config.TLS{Cert: cert, Key: filepath.Join(dir, "missing.pem")}
	s, err := server.New(bad, server.Options{AgentDisabled: true})
	if err != nil {
		t.Fatal(err)
	}
	err = s.Listen()
	if err == nil || strings.Contains(err.Error(), dir) || !strings.Contains(err.Error(), "admin") {
		t.Fatalf("a broken admin key must fail at Listen with a path-free error that names the admin listener: %v", err)
	}
	// The failed Listen must not leave the admin port bound.
	if s.AdminAddr() == "" {
		return
	}
	ln, lerr := net.Listen("tcp", s.AdminAddr())
	if lerr != nil {
		t.Fatalf("the admin listener leaked after a failed Listen: %v", lerr)
	}
	_ = ln.Close()
}

// certWithSerial writes a fresh self-signed certificate with the given serial.
func certWithSerial(t *testing.T, certPath, keyPath string, serial int64) {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "localhost"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, DNSNames: []string{"localhost"}}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalECPrivateKey(k)
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestAdminTLSCertificateIsReloadedWithoutARestart proves the admin listener
// uses the same reloading certificate source as the agent listener: a renewed
// file is served after the reloader's check interval, with no restart.
func TestAdminTLSCertificateIsReloadedWithoutARestart(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "a.crt"), filepath.Join(dir, "a.key")
	certWithSerial(t, certPath, keyPath, 41)
	cfg := config.Default()
	cfg.Resolve()
	cfg.Fixtures = true
	cfg.AdminListen = "127.0.0.1:0"
	cfg.Admin.TLS = config.TLS{Cert: certPath, Key: keyPath}
	clk := &fakeClock{t: time.Now().UTC().Truncate(time.Second)}
	s, err := server.New(cfg, server.Options{AgentDisabled: true, Clock: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx) }()
	defer func() { cancel(); <-done }()

	serial := func() int64 {
		c, err := tls.Dial("tcp", s.AdminAddr(), &tls.Config{InsecureSkipVerify: true})
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		return c.ConnectionState().PeerCertificates[0].SerialNumber.Int64()
	}
	if got := serial(); got != 41 {
		t.Fatalf("first certificate serial %d, want 41", got)
	}
	certWithSerial(t, certPath, keyPath, 42)
	future := time.Now().Add(2 * time.Minute)
	if err := os.Chtimes(certPath, future, future); err != nil {
		t.Fatal(err)
	}
	clk.Advance(2 * time.Minute)
	if got := serial(); got != 42 {
		t.Fatalf("after renewal the admin listener served serial %d, want 42", got)
	}
}
