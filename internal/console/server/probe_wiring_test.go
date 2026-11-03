package server_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/cli"
	"github.com/c2tech-sys/cryobackup/internal/console/config"
	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
	"github.com/c2tech-sys/cryobackup/internal/console/probe"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/console/ui"
	"github.com/c2tech-sys/cryobackup/internal/logx"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// writeAgentCert writes a self-signed certificate for localhost, valid for
// the given time, and its key, as PEM files in dir.
func writeAgentCert(t *testing.T, dir string, valid time.Duration) (certPath, keyPath string) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"}, DNSNames: []string{"localhost"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(valid), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true, IsCA: true}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalECPrivateKey(k)
	certPath, keyPath = filepath.Join(dir, "agent.crt"), filepath.Join(dir, "agent.key")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	_ = ln.Close()
	return port
}

// ADR 0013: serve probes its own public agent endpoint the way an agent does.
// Against the real agent listener (TLS from a private CA given as
// agent_probe.ca_file, public_url naming localhost) the probe passes every
// stage, including "this console's agent API answered", and the certificate's
// remaining 10 days surface as the C05 warning, with no C04 and no C17.
func TestServeProbesItsOwnAgentEndpoint(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	runCLI(t, "init", "--config", cfgPath, "--data-dir", filepath.Join(dir, "data"), "--non-interactive", "--master-key-copy-confirmed",
		"--endpoint", "s3.example.invalid", "--bucket", "probe-bucket", "--prefix", "p", "--gateway-url", "https://gateway.example.invalid:9443",
		"--owner", "owner", "--timezone", "America/Chicago", "--agent-listen", "127.0.0.1:0", "--admin-listen", "127.0.0.1:0")
	cfg, err := config.Read(cfgPath, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	certPath, keyPath := writeAgentCert(t, dir, 10*24*time.Hour+time.Hour)
	port := freePort(t)
	cfg.AgentListen, cfg.AdminListen = "127.0.0.1:"+port, "127.0.0.1:0"
	cfg.TLS = config.TLS{Cert: certPath, Key: keyPath}
	cfg.PublicURL = "https://localhost:" + port
	cfg.AgentProbe.CAFile = certPath
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	var logs syncBuf
	app, err := cli.Build(context.Background(), cfg, cli.BuildOptions{Backend: &fixtures.Backend{}, Log: logx.New(&logs, logx.LevelDebug)})
	if err != nil {
		t.Fatal(err)
	}
	if st := app.Endpoint.Status(); !st.Runs || st.Known || st.Limit != "" {
		t.Fatalf("before Run: %+v", st)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- app.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			t.Error("shutdown timed out")
		}
	}()
	deadline := time.Now().Add(30 * time.Second)
	for !app.Endpoint.Status().Known {
		if time.Now().After(deadline) {
			t.Fatalf("the first probe never completed\nlogs:\n%s", logs.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	st := app.Endpoint.Status()
	if !st.Last.OK || st.Last.NotAfter.IsZero() || st.Host != "localhost:"+port {
		t.Fatalf("probe of the real agent listener: %+v\nlogs:\n%s", st, logs.String())
	}
	f, err := app.Admin.Fleet.Fleet(context.Background(), store.ScopeAll())
	if err != nil {
		t.Fatal(err)
	}
	var c05 *state.Banner
	for i, b := range f.View.Banners {
		switch b.ID {
		case "C04", "C17", "C18":
			t.Errorf("unexpected %s: %s", b.ID, b.Text)
		case "C05":
			c05 = &f.View.Banners[i]
		}
	}
	if c05 == nil || c05.Severity != state.SeverityAlarm || c05.Text != "Agent listener certificate expires in 10 days." {
		t.Fatalf("C05: %+v", c05)
	}
	if out := logs.String(); !strings.Contains(out, "agent endpoint probe passing") || strings.Contains(out, "localhost") {
		t.Fatalf("probe logs (path-free, host-free):\n%s", out)
	}

	// The same probe pointed at the admin listener (plain HTTP, another
	// service) fails at TLS: a wrong address is never taken for the agent API.
	wrong := probe.New(probe.Settings{PublicURL: cfg.PublicURL, CAFile: certPath, ConnectAddress: app.Server.AdminAddr()}, time.Now, logx.New(&logs, logx.LevelDebug))
	if r := wrong.Once(context.Background()); r.OK || r.Stage != probe.StageTLS {
		t.Fatalf("admin listener taken for the agent API: %+v", r)
	}
}

// Without public_url the probe cannot run: serve starts normally and the
// console says so with one warning (C17), never a critical banner.
func TestServeWithoutPublicURLWarnsOnce(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	runCLI(t, "init", "--config", cfgPath, "--data-dir", filepath.Join(dir, "data"), "--non-interactive", "--master-key-copy-confirmed",
		"--endpoint", "s3.example.invalid", "--bucket", "probe-bucket", "--prefix", "p", "--gateway-url", "https://gateway.example.invalid:9443",
		"--owner", "owner", "--timezone", "America/Chicago", "--agent-listen", "127.0.0.1:0", "--admin-listen", "127.0.0.1:0")
	cfg, err := config.Read(cfgPath, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	cfg.InsecureHTTPLab, cfg.AgentListen, cfg.AdminListen, cfg.PublicURL = true, "127.0.0.1:0", "127.0.0.1:0", ""
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	var logs syncBuf
	app, err := cli.Build(context.Background(), cfg, cli.BuildOptions{Backend: &fixtures.Backend{}, Log: logx.New(&logs, logx.LevelDebug)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- app.Run(ctx) }()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(logs.String(), "agent endpoint probe is not running") {
		if time.Now().After(deadline) {
			t.Fatalf("no warning that the probe cannot run\nlogs:\n%s", logs.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	f, err := app.Admin.Fleet.Fleet(context.Background(), store.ScopeAll())
	cancel()
	<-done
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(logs.String(), "agent endpoint probe is not running"); n != 1 {
		t.Fatalf("warned %d times", n)
	}
	var got []string
	for _, b := range f.View.Banners {
		got = append(got, b.ID)
		if b.Severity >= state.SeverityCritical && (b.ID == "C04" || b.ID == "C17") {
			t.Fatalf("critical %s: %s", b.ID, b.Text)
		}
	}
	if strings.Join(got, ",") != "C17" || !strings.Contains(f.View.Banners[0].Text, "public_url is not set") {
		t.Fatalf("banners %v", got)
	}
}

// Fixtures mode shows the ADR 0013 banner on the board from the same path: a
// healthy probe whose certificate expires in 12 days, rendered with its label.
func TestFixturesShowTheEndpointBanner(t *testing.T) {
	cfg := config.Default()
	cfg.Resolve()
	cfg.Fixtures = true
	cfg.AdminListen = "127.0.0.1:0"
	app, err := cli.Build(context.Background(), cfg, cli.BuildOptions{FixturesPassword: "fixtures-pass-1"})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	f, err := app.Admin.Fleet.Fleet(context.Background(), store.ScopeAll())
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, b := range f.View.Banners {
		ids = append(ids, b.ID)
	}
	if strings.Join(ids, ",") != "C05" || f.View.Banners[0].Text != "Agent listener certificate expires in 12 days." {
		t.Fatalf("fixtures banners: %v %+v", ids, f.View.Banners)
	}
	for id, want := range map[string]string{"C04": "AGENT API UNREACHABLE", "C05": "CERTIFICATE EXPIRING", "C17": "AGENT API NOT CHECKED", "C18": "AGENTS STOPPED TOGETHER"} {
		if got := ui.BannerLabel(id); got != want {
			t.Errorf("label %s: %q, want %q", id, got, want)
		}
	}
}
