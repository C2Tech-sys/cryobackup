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
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminapi"
	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/config"
	"github.com/c2tech-sys/cryobackup/internal/console/gateway"
	"github.com/c2tech-sys/cryobackup/internal/console/jobs"
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

// inFile says whether the login is in the file at dir. A reader can meet the job's rename on
// Windows, so it tries again before it calls that a failure.
func (w *retireWorld) inFile(dir, id string) bool {
	w.t.Helper()
	var err error
	for i := 0; i < 200; i++ {
		var raw []byte
		if raw, err = os.ReadFile(filepath.Join(dir, gateway.FileName)); err == nil {
			var logins []gateway.Login
			if _, _, logins, err = gateway.Parse(raw); err == nil {
				for _, l := range logins {
					if l.Username == id {
						return true
					}
				}
				return false
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	w.t.Fatal(err)
	return false
}

// renameDir moves a directory the running console may have a file of open in, on Windows.
func renameDir(t *testing.T, from, to string) {
	t.Helper()
	var err error
	for i := 0; i < 400; i++ {
		if err = os.Rename(from, to); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal(err)
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

	// The fault: the sync cannot replace the file, and the file can still be read.
	restore := blockLoginFileWrites(t, w.dir)

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
	if !w.inFile(w.dir, dead) {
		t.Fatal("the test proves nothing: the login left the file")
	}
	if b := w.banner("C17"); !strings.HasPrefix(b, "critical: ") || !strings.Contains(b, "the gateway may still accept it until the sync succeeds") {
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
	for _, want := range []string{"its gateway login is not removed", "the gateway may still accept this device&#39;s login until the sync succeeds", "Reason: io.", "A banner stays up"} {
		if !strings.Contains(notice, want) {
			t.Errorf("the notice does not say %q", want)
		}
	}
	// A repeat reads the current state and writes nothing.
	if again := w.retire(dead); again.Changed || again.GatewayLogin != adminapi.GatewayLoginPending {
		t.Errorf("repeat: %+v", again)
	}

	restore()

	// The fix. The next sync removes the logins and the banner clears, with no second retire row.
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

// blockLoginFileWrites makes the sync unable to replace the deployed file while the file stays
// readable, and returns the way back. Windows has no directory permission bits, and refuses to
// rename over a read-only file; elsewhere a read-only directory refuses the temp file.
func blockLoginFileWrites(t *testing.T, dir string) (restore func()) {
	t.Helper()
	path := filepath.Join(dir, gateway.FileName)
	if runtime.GOOS == "windows" {
		if err := os.Chmod(path, 0o444); err != nil {
			t.Fatal(err)
		}
		done := false
		restore = func() {
			if !done {
				done = true
				if err := os.Chmod(path, 0o640); err != nil {
					t.Fatal(err)
				}
			}
		}
		t.Cleanup(restore)
		return restore
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	if f, err := os.CreateTemp(dir, "probe"); err == nil { // root ignores the mode bits
		_ = f.Close()
		_ = os.Remove(f.Name())
		_ = os.Chmod(dir, 0o755)
		t.Skip("this user can write to a read-only directory: the fault cannot be made here")
	}
	done := false
	restore = func() {
		if !done {
			done = true
			if err := os.Chmod(dir, 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Cleanup(restore)
	return restore
}

// The file cannot be read at all, and nothing has been seen in it: pending with the class,
// C17b and not C17. Then it can, and everything clears.
func TestRetireWithAnUnreadableFileReadsNotCheckedInRealServeWiring(t *testing.T) {
	w, ids := r3World(t, 3)
	app, ctx := w.app, context.Background()
	renameDir(t, w.dir, w.dir+".aside")
	if err := os.WriteFile(w.dir, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := w.retire(ids[0])
	if got.GatewayLogin != adminapi.GatewayLoginPending || (got.GatewayLoginReason != jobs.LoginFileUnreadable && got.GatewayLoginReason != jobs.LoginFileMissing) {
		t.Fatalf("answer: %+v", got)
	}
	if b := w.banner("C17b"); !strings.Contains(b, "cannot be read") {
		t.Errorf("C17b = %q", b)
	}
	if b := w.banner("C17"); b != "" {
		t.Errorf("nothing was read, yet C17 = %q", b)
	}
	if d := w.auditDetails(ids[0]); len(d) != 1 || !strings.Contains(d[0], `"gateway_login":"pending"`) {
		t.Errorf("audit: %v", d)
	}
	if err := os.Remove(w.dir); err != nil {
		t.Fatal(err)
	}
	renameDir(t, w.dir+".aside", w.dir)
	app.Htpasswd.Sig.Notify()
	w.waitFor("the sync and a clean read", func() bool { app.Htpasswd.Sig.Notify(); return !w.inFile(w.dir, ids[0]) })
	w.waitFor("the banner to clear", func() bool { return w.banner("C17b") == "" && w.banner("C17") == "" })
	if c := app.Htpasswd.Check(ctx); c.ReadClass != "" || c.Has(ids[0]) {
		t.Errorf("check: %+v", c)
	}
}

// An old copy of the file put back, with the job reading ok: the prober's tick reads the file,
// and C17 comes up with no sync having run. The sync then repairs it, and it clears.
func TestAnOldCopyOfTheFilePutBackIsSeenByTheProbersTick(t *testing.T) {
	w, ids := r3World(t, 2)
	app := w.app
	path := filepath.Join(w.dir, gateway.FileName)
	old, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := w.retire(ids[0]); got.GatewayLogin != adminapi.GatewayLoginRemoved {
		t.Fatalf("answer: %+v", got)
	}
	if b := w.banner("C17"); b != "" {
		t.Fatalf("before: %s", b)
	}
	// One tick of the real prober, by hand: the test does not wait out its 60 s. A sync still queued can repair
	// the file first, which is right, and a tick in flight is not doubled and may have read before the copy
	// went back. So: put it back, run a tick that begins after that, and judge only a file that still holds it.
	seen := false
	for i := 0; i < 20 && !seen; i++ {
		if err := os.WriteFile(path, old, 0o640); err != nil {
			time.Sleep(20 * time.Millisecond) // the job's rename, on Windows
			continue
		}
		app.Prober.Wait() // a tick already in flight ends first
		if err := app.Prober.Run(context.Background()); err != nil {
			t.Fatal(err)
		}
		app.Prober.Wait()
		// The tick's own row says it was the tick that read it, not a run of the job that happened to.
		row, rerr := app.Store.GetJobState(context.Background(), jobs.ProbeRowLoginFile)
		seen = rerr == nil && row.LastStatus == "error" && row.LastErrorClass == "retired_login_in_file" &&
			strings.HasPrefix(w.banner("C17"), "critical: ") && w.inFile(w.dir, ids[0])
	}
	if !seen {
		t.Fatal("the tick never saw the old copy")
	}
	app.Htpasswd.Sig.Notify()
	w.waitFor("the sync to repair the file", func() bool { return !w.inFile(w.dir, ids[0]) })
	w.waitFor("the banner to clear", func() bool { return w.banner("C17") == "" })
}

// m22's successor: fixtures mode has no gateway and no file. Its fictional file holds no retired
// login, so a retire answers removed, with no wait and no banner.
func TestFixturesModeRetireAnswersRemoved(t *testing.T) {
	cfg := config.Default()
	cfg.Resolve()
	cfg.Fixtures = true
	cfg.AdminListen = "127.0.0.1:0"
	app, err := Build(context.Background(), cfg, BuildOptions{FixturesPassword: "fixtures-pass-1"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	srv := httptest.NewServer(app.Server.AdminRouter)
	t.Cleanup(srv.Close)
	w := &retireWorld{t: t, app: app, c: r3SignIn(t, app, srv.URL)}
	l, err := app.Store.ListDevices(context.Background(), store.ScopeAll(), store.ListDevicesOptions{Search: "FRONTDESK-01"})
	if err != nil || len(l.Items) != 1 {
		t.Fatalf("fixture device: %d, %v", len(l.Items), err)
	}
	began := time.Now()
	got := w.retire(l.Items[0].ID)
	if got.GatewayLogin != adminapi.GatewayLoginRemoved {
		t.Fatalf("answer: %+v", got)
	}
	if took := time.Since(began); took >= adminapi.DefaultGatewaySyncWait {
		t.Errorf("fixtures mode waited %v", took)
	}
	if b := w.banner("C17"); b != "" {
		t.Errorf("C17 = %q", b)
	}
	if b := w.banner("C17b"); b != "" {
		t.Errorf("C17b = %q", b)
	}
}
