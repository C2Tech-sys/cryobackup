package maintain

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/gateway"
)

// selfSigned writes a throw-away certificate for 127.0.0.1 and returns the file
// paths of the certificate and the key.
func selfSigned(t *testing.T) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "cryo-live-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certPath, keyPath = filepath.Join(dir, "gateway.crt"), filepath.Join(dir, "gateway.key")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

// TestLiveSmokeGatewayScript runs test/lab/smoke-gateway.sh against a real rclone
// gateway started with the TLS flags of deploy/rclone/gateway-flags.md, the htpasswd
// rendered by internal/console/gateway (with the probe login derived from a master
// key) and the probe password derived by the script through probe-password.py (key on stdin).
// Needs bash, curl and python; skipped when any is missing.
func TestLiveSmokeGatewayScript(t *testing.T) {
	_, rcloneBin := liveBinaries(t)
	for _, tool := range []string{"bash", "curl"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not found", tool)
		}
	}
	if _, err := exec.LookPath("python3"); err != nil {
		if _, err := exec.LookPath("python"); err != nil {
			t.Skip("python not found")
		}
	}
	masterKey := bytes.Repeat([]byte{0x2a}, 32)
	probePW, err := gateway.ProbePassword(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	probeHash, err := gateway.HashPassword(probePW)
	if err != nil {
		t.Fatal(err)
	}
	devHash, err := gateway.HashPassword("unused-device-password")
	if err != nil {
		t.Fatal(err)
	}
	content := gateway.Render([]gateway.Login{{Username: "01LIVEDEVICE000000000000A", Hash: devHash}}, probeHash, 1)
	dir := t.TempDir()
	if err := gateway.Sync(dir, content); err != nil {
		t.Fatal(err)
	}
	cert, key := selfSigned(t)
	gw := startGateway(t, rcloneBin, filepath.Join(dir, gateway.FileName), true,
		"--cert", cert, "--key", key, "--min-tls-version", "tls1.2")
	if !strings.HasPrefix(gw.base, "https://") {
		t.Fatalf("gateway base = %q, want https", gw.base)
	}

	keyFile := filepath.Join(t.TempDir(), "master.key")
	if err := os.WriteFile(keyFile, []byte(base64.StdEncoding.EncodeToString(masterKey)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "..", "test", "lab", "smoke-gateway.sh"))
	if err != nil {
		t.Fatal(err)
	}
	run := func(keyPath string) (string, int) {
		cmd := exec.Command("bash", script, gw.base)
		cmd.Env = append(os.Environ(), "CACERT="+cert, "MASTER_KEY_FILE="+keyPath)
		out, err := cmd.CombinedOutput()
		code := 0
		if err != nil {
			ee, ok := err.(*exec.ExitError)
			if !ok {
				t.Fatalf("script did not start: %v", err)
			}
			code = ee.ExitCode()
		}
		return string(out), code
	}

	out, code := run(keyFile)
	t.Logf("smoke output:\n%s", out)
	if code != 0 || !strings.Contains(out, "RESULT: PASS") {
		t.Fatalf("smoke script exit %d with the right master key", code)
	}
	if strings.Contains(out, probePW) {
		t.Fatal("the smoke script printed the probe password")
	}

	// A different master key derives a different probe password: the gateway refuses it.
	wrong := filepath.Join(t.TempDir(), "other.key")
	if err := os.WriteFile(wrong, []byte(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x2b}, 32))+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, code = run(wrong)
	t.Logf("smoke output (wrong key):\n%s", out)
	if code != 1 || !strings.Contains(out, "FAIL probe login: 401") {
		t.Fatalf("smoke script exit %d with the wrong master key, want 1 and a 401 on the probe login", code)
	}
}
