package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminapi"
	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/gateway"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// M3-T6 delta M1: in the real serve wiring (the real App, its running job runner, the
// real htpasswd file), a retire tells the truth about the gateway when the sync cannot
// write, the banner stands until a sync passes, and it clears by itself.

type retireWorld struct {
	t    *testing.T
	app  *App
	c    *r3Browser
	dir  string
	cust store.Customer
	tok  string
	n    int
}

func (w *retireWorld) enroll(host string) string {
	w.t.Helper()
	ctx := context.Background()
	w.n++
	hash, err := gateway.HashPassword("gateway-password-" + host)
	if err != nil {
		w.t.Fatal(err)
	}
	sum := func(s string) string { b := sha256.Sum256([]byte(s)); return hex.EncodeToString(b[:]) }
	res, err := w.app.Store.AgentEnroll(ctx, store.EnrollInput{TokenHash: sum(w.tok), Nonce: "nonce-" + host, Hostname: host, OSVersion: "Windows 11 Pro", Arch: "amd64", DeviceClass: "desktop",
		AgentVersion: "1.4.2", ResticVersion: "0.19.1", DeviceTokenHash: sum("device-" + host), GatewayPasswordHash: hash, GatewaySecretWrapped: []byte("wrapped-" + host),
		RepoKeyWrapped: []byte("wrapped-repo-" + host), WrapAlg: "aes-256-gcm+hkdf-sha256/v1", MasterKeyID: "test-key"})
	if err != nil {
		w.t.Fatal(err)
	}
	return res.Device.ID
}

// inFile says whether the login is in the file at dir.
func (w *retireWorld) inFile(dir, id string) bool {
	w.t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, gateway.FileName))
	if err != nil {
		w.t.Fatal(err)
	}
	_, _, logins, err := gateway.Parse(b)
	if err != nil {
		w.t.Fatal(err)
	}
	for _, l := range logins {
		if l.Username == id {
			return true
		}
	}
	return false
}

// waitFor polls a condition for up to 15 s of real time.
func (w *retireWorld) waitFor(what string, ok func() bool) {
	w.t.Helper()
	end := time.Now().Add(15 * time.Second)
	for !ok() {
		if time.Now().After(end) {
			w.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// api calls the admin API as the signed-in browser.
func (w *retireWorld) api(method, path string, body any) (int, []byte) {
	w.t.Helper()
	var rd io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = strings.NewReader(string(raw))
	}
	req, _ := http.NewRequest(method, w.c.base+path, rd)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", w.c.base)
	req.Header.Set(auth.CSRFHeader, w.c.csrf)
	for _, ck := range w.c.cs {
		req.AddCookie(ck)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, b
}

// banner returns the C17 banner of the fleet's answer ("" when it is not up).
func (w *retireWorld) banner(id string) string {
	w.t.Helper()
	code, b := w.api("GET", "/api/v1/fleet/banners", nil)
	if code != 200 {
		w.t.Fatalf("banners: %d %s", code, b)
	}
	var out struct {
		Banners []struct {
			ID, Severity, Text string
		} `json:"banners"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		w.t.Fatal(err)
	}
	for _, x := range out.Banners {
		if x.ID == id {
			return x.Severity + ": " + x.Text
		}
	}
	return ""
}

func (w *retireWorld) retire(id string) adminapi.DeviceRetireResponse {
	w.t.Helper()
	code, b := w.api("POST", "/api/v1/devices/"+id+"/retire", adminapi.DeviceRetireRequest{Reason: "compromised endpoint"})
	if code != 200 {
		w.t.Fatalf("retire: %d %s", code, b)
	}
	var out adminapi.DeviceRetireResponse
	if err := json.Unmarshal(b, &out); err != nil {
		w.t.Fatal(err)
	}
	return out
}

func (w *retireWorld) auditDetails(id string) []string {
	w.t.Helper()
	l, err := w.app.Store.ListAudit(context.Background(), store.ScopeAll(), store.AuditFilter{Action: "device.retire_sync"}, store.Page{Limit: 50})
	if err != nil {
		w.t.Fatal(err)
	}
	var out []string
	for _, ev := range l.Items {
		if ev.ObjectID == id {
			out = append(out, string(ev.Details))
		}
	}
	return out
}

func TestRetireTellsTheTruthWhenTheSyncCannotWriteInRealServeWiring(t *testing.T) { // M3-T6 delta M1
	app, _ := buildFromInit(t, "America/Chicago", "America/Chicago")
	ctx, cancel := context.WithCancel(context.Background())
	app.Runner.Start(ctx)
	t.Cleanup(func() { cancel(); app.Runner.Stop() })
	srv := httptest.NewServer(app.Server.AdminRouter)
	t.Cleanup(srv.Close)
	w := &retireWorld{t: t, app: app, c: r3SignIn(t, app, srv.URL), dir: app.Htpasswd.Dir, tok: "enroll-secret-m3t6"}
	cust, err := app.Store.CreateCustomer(ctx, store.ScopeAll(), store.Customer{Name: "Retire Test Customer"})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(w.tok))
	if _, err := app.Store.CreateEnrollmentToken(ctx, store.ScopeAll(), store.EnrollmentToken{CustomerID: cust.ID, TokenHash: hex.EncodeToString(sum[:]), ExpiresAt: app.clock().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	w.cust = cust
	dead, viaUI, later := w.enroll("DEAD-PC-01"), w.enroll("DEAD-PC-02"), w.enroll("DEAD-PC-03")
	if err := os.MkdirAll(w.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	app.Htpasswd.Sig.Notify()
	w.waitFor("the logins to reach the file", func() bool {
		_, err := os.Stat(filepath.Join(w.dir, gateway.FileName))
		if errors.Is(err, fs.ErrNotExist) {
			app.Htpasswd.Sig.Notify()
			return false
		}
		return err == nil && w.inFile(w.dir, dead) && w.inFile(w.dir, viaUI) && w.inFile(w.dir, later)
	})
	if b := w.banner("C17"); b != "" {
		t.Fatalf("before any retire: %s", b)
	}

	// The fault. Windows has no directory permission bits, so a plain file takes the directory's place: every
	// platform's temp-file create fails, as on an unwritable mount. The deployed file itself is untouched.
	aside := w.dir + ".aside"
	if err := os.Rename(w.dir, aside); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(w.dir, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The answer is pending, within the bound, with the reason class.
	began := time.Now()
	got := w.retire(dead)
	if took := time.Since(began); took > adminapi.DefaultGatewaySyncWait+3*time.Second {
		t.Errorf("the retire took %v: the wait must stay bounded", took)
	}
	if !got.Retired || got.LoginsRevoked != 1 || got.GatewayLogin != adminapi.GatewayLoginPending || got.GatewayLoginReason != "io" {
		t.Fatalf("answer: %+v, want pending with reason io", got)
	}
	// The login really is still in the file, and the critical banner says so.
	if !w.inFile(aside, dead) {
		t.Fatal("the test proves nothing: the login left the file")
	}
	if b := w.banner("C17"); !strings.HasPrefix(b, "critical: ") || !strings.Contains(b, "the gateway may still accept it until the sync succeeds") || !strings.Contains(b, "io") {
		t.Fatalf("banner C17 = %q", b)
	}
	// The device page and the audit row say the same.
	_, page := w.c.do("GET", "/devices/"+dead, nil)
	for _, want := range []string{"gateway login not removed", "the gateway may still accept this device&#39;s login until the sync succeeds", "reason: io"} {
		if !strings.Contains(page, want) {
			t.Errorf("the device page does not say %q", want)
		}
	}
	if d := w.auditDetails(dead); len(d) != 1 || !strings.Contains(d[0], `"gateway_login":"pending"`) || !strings.Contains(d[0], `"reason":"io"`) {
		t.Errorf("audit: %v", d)
	}

	// The UI path gets the same truth, in its notice.
	res, _ := w.c.do("POST", "/ui/devices/"+viaUI+"/retire", url.Values{"csrf_token": {w.c.csrf}, "reason": {"compromised endpoint"}, "confirm": {"DEAD-PC-02"}})
	loc := res.Header.Get("Location")
	if !strings.Contains(loc, "cmd=retired_pending") || !strings.Contains(loc, "why=io") {
		t.Fatalf("UI retire went to %q", loc)
	}
	_, notice := w.c.do("GET", loc, nil)
	for _, want := range []string{"its gateway login is not removed", "the gateway may still accept this device&#39;s login until the sync succeeds", "Reason: io."} {
		if !strings.Contains(notice, want) {
			t.Errorf("the notice does not say %q", want)
		}
	}
	// A repeat reads the current state and writes nothing.
	if again := w.retire(dead); again.Changed || again.GatewayLogin != adminapi.GatewayLoginPending {
		t.Errorf("repeat: %+v", again)
	}

	// The fix. The next sync removes the logins and the banner clears, with no second retire row.
	if err := os.Remove(w.dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(aside, w.dir); err != nil {
		t.Fatal(err)
	}
	w.waitFor("the next sync to remove the logins", func() bool {
		app.Htpasswd.Sig.Notify()
		return !w.inFile(w.dir, dead) && !w.inFile(w.dir, viaUI)
	})
	if !w.inFile(w.dir, later) {
		t.Error("the active device's login left the file")
	}
	w.waitFor("the banner to clear", func() bool { return w.banner("C17") == "" })
	_, page = w.c.do("GET", "/devices/"+dead, nil)
	if strings.Contains(page, "gateway login not removed") || strings.Contains(page, "may still accept") {
		t.Error("the device page still says pending after the sync passed")
	}
	if again := w.retire(dead); again.GatewayLogin != adminapi.GatewayLoginRemoved || again.GatewayLoginReason != "" {
		t.Errorf("repeat after the fix: %+v", again)
	}
	if d := w.auditDetails(dead); len(d) != 1 {
		t.Errorf("%d retire_sync rows: a later sync writes none here", len(d))
	}

	// The normal path, through the UI's own private API: removed, in the answer, quickly.
	began = time.Now()
	res, _ = w.c.do("POST", "/ui/devices/"+later+"/retire", url.Values{"csrf_token": {w.c.csrf}, "reason": {"lab machine"}, "confirm": {"DEAD-PC-03"}})
	if loc := res.Header.Get("Location"); !strings.Contains(loc, "cmd=retired_ok") {
		t.Fatalf("a retire with a working sync went to %q", loc)
	}
	if took := time.Since(began); took >= adminapi.DefaultGatewaySyncWait {
		t.Errorf("a working sync took the whole wait (%v): the UI's API is not signalling it", took)
	}
	if w.inFile(w.dir, later) {
		t.Error("the answer said removed and the login is in the file")
	}
	if d := w.auditDetails(later); len(d) != 1 || !strings.Contains(d[0], `"gateway_login":"removed"`) {
		t.Errorf("audit: %v", d)
	}
	if b := w.banner("C17"); b != "" {
		t.Errorf("banner after a clean retire: %s", b)
	}
}
