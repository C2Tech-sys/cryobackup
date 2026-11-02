package server_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/cli"
	"github.com/c2tech-sys/cryobackup/internal/console/config"
)

// The enrollment wizard must carry the operator's configured public_url and
// read the configured artifacts_dir (M2-W2B). cli.Build is what hands them to
// the UI and the admin API, so this goes through the real Build, not a
// hand-made Service.
func TestEnrollWizardUsesTheConfiguredPublicURLAndArtifactsDir(t *testing.T) {
	const publicURL = "https://backup.example.test:8443"
	cfg := config.Default()
	cfg.Resolve()
	cfg.Fixtures = true
	cfg.AdminListen = "127.0.0.1:0"
	cfg.PublicURL = publicURL
	cfg.ArtifactsDir = t.TempDir()
	msi := []byte("installer bytes")
	if err := os.WriteFile(filepath.Join(cfg.ArtifactsDir, "cryobackup-agent-3.1.4.msi"), msi, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	app, err := cli.Build(context.Background(), cfg, cli.BuildOptions{FixturesPassword: "fixtures-pass-1"})
	if err != nil {
		t.Fatal(err)
	}
	if app.Admin.PublicURL != publicURL || app.Admin.ArtifactsDir != cfg.ArtifactsDir {
		t.Fatalf("adminapi.Service got public_url %q and artifacts_dir %q", app.Admin.PublicURL, app.Admin.ArtifactsDir)
	}
	if err := app.Server.Listen(); err != nil {
		t.Fatal(err)
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
	base := "http://" + app.Server.AdminAddr()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.PostForm(base+"/login", url.Values{"username": {"admin"}, "password": {"fixtures-pass-1"}})
	if err != nil || res.StatusCode != 303 {
		t.Fatalf("login: %v %v", err, res)
	}
	res.Body.Close()
	cookies := uiEnroll(t, client, base, res.Cookies())
	csrf := ""
	for _, c := range cookies {
		if c.Name == auth.CSRFCookie {
			csrf = c.Value
		}
	}
	do := func(method, path string, form url.Values) (int, string) {
		var body io.Reader
		if form != nil {
			form.Set("csrf_token", csrf)
			body = strings.NewReader(form.Encode())
		}
		r, _ := http.NewRequest(method, base+path, body)
		if form != nil {
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			r.Header.Set("Origin", base)
		}
		for _, c := range cookies {
			r.AddCookie(c)
		}
		res, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b)
	}

	code, page := do("GET", "/enroll", nil)
	if code != 200 || strings.Contains(page, "Public URL not set") {
		t.Fatalf("/enroll: %d (public_url is configured, the page must not say it is missing)", code)
	}
	m := regexp.MustCompile(`<option value="([0-9A-Z]{26})">Summit Eye Care</option>`).FindStringSubmatch(page)
	if m == nil {
		t.Fatal("no Summit Eye Care in the customer list")
	}
	code, page = do("POST", "/ui/enroll/token", url.Values{"customer_id": {m[1]}, "expires": {"7d"}, "max_uses": {"10"}, "device_class": {"unknown"}})
	if code != 200 {
		t.Fatalf("create: %d", code)
	}
	var rmm string
	for _, b := range regexp.MustCompile(`(?s)<pre class="code"[^>]*aria-label="RMM script variables[^"]*">(.*?)</pre>`).FindAllStringSubmatch(page, -1) {
		rmm = html.UnescapeString(b[1])
	}
	if !strings.Contains(rmm, "$ServerUrl   = '"+publicURL+"'") {
		t.Fatalf("the RMM block does not carry the configured public URL:\n%s", rmm)
	}
	sum := sha256.Sum256(msi)
	if !strings.Contains(rmm, hex.EncodeToString(sum[:])) {
		t.Fatalf("the RMM block does not carry the hash of the MSI in artifacts_dir:\n%s", rmm)
	}
	if code, dl := do("GET", "/downloads", nil); code != 200 || !strings.Contains(dl, "cryobackup-agent-3.1.4.msi") || !strings.Contains(dl, hex.EncodeToString(sum[:])) {
		t.Fatalf("/downloads: %d", code)
	}
}
