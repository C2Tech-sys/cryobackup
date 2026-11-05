package adminapi

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/escrow"
	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
	"github.com/c2tech-sys/cryobackup/internal/console/health"
	"github.com/c2tech-sys/cryobackup/internal/console/restores"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

type env struct {
	t    *testing.T
	st   *store.Store
	clk  *clock
	srv  *httptest.Server
	svc  *Service
	rest *restores.Service
	be   *fixtures.Backend
	dir  string // the artifacts directory: must stay empty (ADR 0008)
	dev  store.Device
	held store.Device
	mfa  *mfaState
	// extra is the Evaluator's Extra banners, swappable while streams run (setExtra, m3_hooks_test.go).
	extra *bannerHook
}

type session struct {
	cookies []*http.Cookie
	csrf    string
}

// testPasswordHash is the argon2id hash of the test password, computed once per
// test binary (M3-INT B.e): every env and every added account shares it, so an
// env costs one argon2 verify at login instead of a hash and a verify. Under
// -race one argon2 run takes about a second.
var testPasswordHash = sync.OnceValues(func() (string, error) { return auth.HashPassword("pw-for-tests-1") })

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	clk := &clock{t: fixtures.Now()}
	st, err := store.OpenMemory(ctx, clk.Now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	hash, err := testPasswordHash()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.LoadFixtures(ctx, st, clk.Now(), store.WithAdminPasswordHash(hash)); err != nil {
		t.Fatal(err)
	}
	mk, _ := escrow.NewMasterKey(bytes.Repeat([]byte{1}, 32))
	dir := t.TempDir()
	be := &fixtures.Backend{}
	rest := &restores.Service{Store: st, Backend: be, Master: mk, ArtifactsDir: dir, Clock: clk.Now}
	am := auth.NewManager(st, 12*time.Hour, 7*24*time.Hour, 100, 5, 15*time.Minute, clk.Now)
	// Fleet.Extra is set once, here, before the server starts; tests change the banners through
	// e.setExtra, never by writing the field while a handler or a stream may be evaluating.
	extra := &bannerHook{}
	svc := &Service{Auth: am, Store: st, Restores: rest, Backend: be, Clock: clk.Now,
		Fleet: &health.Evaluator{Store: st, Clock: clk.Now, MasterLoaded: mk.Loaded, Extra: extra.banners}}
	mux := http.NewServeMux()
	svc.Register(muxReg{mux})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	e := &env{t: t, st: st, clk: clk, srv: srv, svc: svc, rest: rest, be: be, dir: dir, extra: extra}
	e.mfa = newMFA(am, mk) // accounts_test.go: the MFA step of e.login
	find := func(h string) store.Device {
		l, _ := st.ListDevices(ctx, store.ScopeAll(), store.ListDevicesOptions{Search: h})
		if len(l.Items) != 1 {
			t.Fatalf("fixture %s", h)
		}
		return l.Items[0]
	}
	e.dev, e.held = find("DR-OFFICE-PC"), find("LEGAL-PC-01")
	for _, d := range []store.Device{e.dev, e.held} {
		if _, _, err := st.AgentReplaceSnapshots(ctx, d.ID, []store.Snapshot{{DeviceID: d.ID, SnapshotID: fixtures.FixtureSnapshotID, TakenAt: clk.Now().Add(-time.Hour), Hostname: d.Hostname, Paths: []string{`C:\Users`}}}); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

type muxReg struct{ m *http.ServeMux }

func (r muxReg) Handle(p string, h http.Handler) { r.m.Handle(p, h) }

func (e *env) req(method, path string, s *session, body any, hdr ...string) (*http.Response, []byte) {
	e.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	default:
		raw, _ := json.Marshal(b)
		rd = bytes.NewReader(raw)
	}
	r, _ := http.NewRequest(method, e.srv.URL+path, rd)
	r.Header.Set("Content-Type", "application/json")
	if s != nil {
		for _, c := range s.cookies {
			r.AddCookie(c)
		}
		if method != "GET" {
			r.Header.Set("Origin", e.srv.URL)
			r.Header.Set(auth.CSRFHeader, s.csrf)
		}
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	res, err := http.DefaultClient.Do(r)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res, b
}

func (e *env) login(user, pw string) *session {
	e.t.Helper()
	res, body := e.req("POST", "/api/v1/auth/login", nil, LoginRequest{Username: user, Password: pw})
	if res.StatusCode != 200 {
		e.t.Fatalf("login: %d %s", res.StatusCode, body)
	}
	return e.completeMFA(user, &session{cookies: res.Cookies(), csrf: csrfFrom(e.t, body)}, body)
}

func csrfFrom(t *testing.T, body []byte) string {
	var lr LoginResponse
	if err := json.Unmarshal(body, &lr); err != nil {
		t.Fatal(err)
	}
	return lr.CSRFToken
}

func (e *env) addAdmin(name, role string, scope []string) {
	h, _ := testPasswordHash()
	if _, err := e.st.CreateAdmin(context.Background(), store.Admin{Username: name, Email: name + "@example.invalid", Role: role, PasswordHash: h, CustomerScope: scope}); err != nil {
		e.t.Fatal(err)
	}
}

func problemCode(t *testing.T, res *http.Response, body []byte) string {
	t.Helper()
	if ct := res.Header.Get("Content-Type"); ct != api.ContentTypeProblem {
		t.Fatalf("content type %q: %s", ct, body)
	}
	var p api.Error
	if err := json.Unmarshal(body, &p); err != nil || p.Status != res.StatusCode || p.Type != api.ErrorTypePrefix+p.Code {
		t.Fatalf("problem body: %s (%v)", body, err)
	}
	return p.Code
}

func TestLoginAndLogout(t *testing.T) {
	e := newEnv(t)
	res, body := e.req("POST", "/api/v1/auth/login", nil, LoginRequest{Username: "admin", Password: "pw-for-tests-1"})
	var lr LoginResponse
	_ = json.Unmarshal(body, &lr)
	if res.StatusCode != 200 || !lr.MFARequired || !lr.MFAEnrollmentRequired || lr.CSRFToken == "" || lr.Admin.Role != "owner" || lr.ComputedAt.IsZero() {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	var sessCookie *http.Cookie
	for _, c := range res.Cookies() {
		if c.Name == auth.SessionCookie {
			sessCookie = c
		}
	}
	// The test server is plain HTTP, so Secure must be off (FIX17).
	if sessCookie == nil || !sessCookie.HttpOnly || sessCookie.Secure || sessCookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("session cookie: %+v", sessCookie)
	}
	for name, tc := range map[string]struct {
		body any
		hdr  []string
		want int
	}{
		"bad password":   {LoginRequest{Username: "admin", Password: "nope"}, nil, 401},
		"unknown user":   {LoginRequest{Username: "ghost", Password: "nope"}, nil, 401},
		"missing fields": {LoginRequest{Username: "admin"}, nil, 400},
		"unknown field":  {`{"username":"admin","password":"x","extra":1}`, nil, 400},
		"foreign origin": {LoginRequest{Username: "admin", Password: "pw-for-tests-1"}, []string{"Origin", "https://evil.example"}, 403},
		"not json":       {`nope`, nil, 400},
	} {
		res, b := e.req("POST", "/api/v1/auth/login", nil, tc.body, tc.hdr...)
		if res.StatusCode != tc.want {
			t.Errorf("%s: %d %s", name, res.StatusCode, b)
			continue
		}
		problemCode(t, res, b)
	}
	s := e.login("admin", "pw-for-tests-1")
	if res, _ := e.req("GET", "/api/v1/version", s, nil); res.StatusCode != 200 {
		t.Fatalf("version: %d", res.StatusCode)
	}
	if res, _ := e.req("POST", "/api/v1/auth/logout", s, nil); res.StatusCode != 204 {
		t.Fatalf("logout: %d", res.StatusCode)
	}
	if res, b := e.req("GET", "/api/v1/version", s, nil); res.StatusCode != 401 || problemCode(t, res, b) != api.CodeUnauthorized {
		t.Fatalf("after logout: %d", res.StatusCode)
	}
}

func TestEveryRouteNeedsASessionAndWritesNeedCSRF(t *testing.T) {
	e := newEnv(t)
	for _, p := range Patterns() {
		if p == PatternLogin {
			continue
		}
		method, path, _ := strings.Cut(p, " ")
		path = strings.NewReplacer("{id}", e.dev.ID).Replace(path)
		res, b := e.req(method, path, nil, nil)
		if res.StatusCode != 401 || problemCode(t, res, b) != api.CodeUnauthorized {
			t.Errorf("%s anonymous: %d", p, res.StatusCode)
		}
	}
	s := e.login("admin", "pw-for-tests-1")
	bad := &session{cookies: s.cookies, csrf: "wrong"}
	res, b := e.req("POST", "/api/v1/devices/"+e.dev.ID+"/restores", bad, CreateRestoreRequest{SnapshotID: fixtures.FixtureSnapshotID, Mode: "download_zip", Path: "/C/x"})
	if res.StatusCode != 403 || problemCode(t, res, b) != api.CodeForbidden {
		t.Fatalf("csrf: %d %s", res.StatusCode, b)
	}
	if e.be.Count("dump") != 0 {
		t.Fatal("a CSRF failure must not run a dump")
	}
}

func TestDeviceListFiltersPagesAndScopes(t *testing.T) {
	e := newEnv(t)
	s := e.login("admin", "pw-for-tests-1")
	get := func(q string) DeviceListResponse {
		res, b := e.req("GET", "/api/v1/devices"+q, s, nil)
		if res.StatusCode != 200 {
			t.Fatalf("%s: %d %s", q, res.StatusCode, b)
		}
		var out DeviceListResponse
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	all := get("")
	if all.Total < 20 || len(all.Devices) != all.Total || !all.Complete || all.ComputedAt.IsZero() || all.Truncated {
		t.Fatalf("list: total=%d len=%d", all.Total, len(all.Devices))
	}
	for _, d := range all.Devices {
		if d.State == "RETIRED" {
			t.Fatal("retired devices are hidden by default")
		}
		if d.Customer == "" || d.Hostname == "" || d.Severity == "" {
			t.Fatalf("incomplete row: %+v", d)
		}
	}
	// Exception-first: the first rows belong to the worst customer group, not the alphabet.
	if all.Devices[0].Customer == "Northwind Dental" {
		t.Fatalf("not exception-first: %+v", all.Devices[0])
	}
	failed := get("?state=failed")
	if failed.Total != 1 || failed.Devices[0].Hostname != "FRONTDESK-01" || failed.Devices[0].State != "FAILED" {
		t.Fatalf("state filter: %+v", failed)
	}
	if get("?q=legal").Total != 2 || get("?include_retired=true").Total != all.Total+1 {
		t.Fatal("search / include_retired")
	}
	page1 := get("?limit=5")
	if len(page1.Devices) != 5 || !page1.Truncated || page1.NextCursor != "o5" || page1.Total != all.Total {
		t.Fatalf("paging: %+v", page1)
	}
	page2 := get("?limit=5&cursor=" + page1.NextCursor)
	if page2.Devices[0].ID != all.Devices[5].ID {
		t.Fatal("page 2 must continue where page 1 ended")
	}
	for _, q := range []string{"?limit=0", "?limit=501", "?cursor=zz", "?state=bogus"} {
		res, b := e.req("GET", "/api/v1/devices"+q, s, nil)
		if res.StatusCode != 400 || problemCode(t, res, b) != api.CodeInvalidRequest {
			t.Errorf("%s: %d", q, res.StatusCode)
		}
	}
	// A scoped operator sees only its customer.
	cs, _ := e.st.ListCustomers(context.Background(), store.ScopeAll())
	var nw store.Customer
	for _, c := range cs {
		if c.Name == "Northwind Dental" {
			nw = c
		}
	}
	e.addAdmin("scoped", "operator", []string{nw.ID})
	sc := e.login("scoped", "pw-for-tests-1")
	res, b := e.req("GET", "/api/v1/devices", sc, nil)
	var out DeviceListResponse
	_ = json.Unmarshal(b, &out)
	if res.StatusCode != 200 || out.Total != 3 {
		t.Fatalf("scoped list: %d total=%d", res.StatusCode, out.Total)
	}
	for _, d := range out.Devices {
		if d.Customer != "Northwind Dental" {
			t.Fatalf("scope leak: %+v", d)
		}
	}
	// Another customer's device is a 404, not a 403.
	res, b = e.req("GET", "/api/v1/devices/"+e.dev.ID, sc, nil)
	if res.StatusCode != 404 || problemCode(t, res, b) != api.CodeNotFound {
		t.Fatalf("out-of-scope detail: %d", res.StatusCode)
	}
}

func TestDeviceDetail(t *testing.T) {
	e := newEnv(t)
	s := e.login("admin", "pw-for-tests-1")
	res, b := e.req("GET", "/api/v1/devices/"+e.dev.ID, s, nil)
	if res.StatusCode != 200 {
		t.Fatalf("%d %s", res.StatusCode, b)
	}
	var d DeviceDetailJSON
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatal(err)
	}
	if d.Hostname != "DR-OFFICE-PC" || d.Customer != "Summit Eye Care" || d.PolicyVersion < 1 || d.PolicyETag == "" || d.Custody != "escrowed" ||
		d.ServiceState != "running" || d.Snapshots != 1 || len(d.Runs) == 0 || d.ComputedAt.IsZero() {
		t.Fatalf("detail: %+v", d)
	}
	for _, r := range d.Runs {
		for k := range r.ErrorCounts {
			if strings.ContainsAny(k, `/\:`) {
				t.Fatalf("error counts must be classes: %v", r.ErrorCounts)
			}
		}
	}
	if res, _ := e.req("GET", "/api/v1/devices/01J9ZZ0000000000000000ZZZZ", s, nil); res.StatusCode != 404 {
		t.Fatalf("unknown id: %d", res.StatusCode)
	}
}

func (e *env) mint(s *session, id string) (string, *http.Response) {
	e.t.Helper()
	res, b := e.req("POST", "/api/v1/restores/"+id+"/download", s, nil)
	if res.StatusCode != 200 {
		return "", res
	}
	var dr DownloadResponse
	if err := json.Unmarshal(b, &dr); err != nil || !strings.HasPrefix(dr.DownloadURL, "/api/v1/restores/"+id+"/download?token=") {
		e.t.Fatalf("mint body: %s", b)
	}
	if want := e.clk.Now().Add(2 * time.Minute); !dr.ExpiresAt.Equal(want) {
		e.t.Fatalf("a token lives two minutes: %v want %v", dr.ExpiresAt, want)
	}
	return dr.DownloadURL, res
}

func (e *env) newRestore(s *session, path string) RestoreJSON {
	e.t.Helper()
	res, b := e.req("POST", "/api/v1/devices/"+e.dev.ID+"/restores", s, CreateRestoreRequest{SnapshotID: fixtures.FixtureSnapshotID, Mode: "download_zip", Path: path})
	if res.StatusCode != 202 {
		e.t.Fatalf("create: %d %s", res.StatusCode, b)
	}
	var rj RestoreJSON
	_ = json.Unmarshal(b, &rj)
	return rj
}

func TestRestoreLifecycleAndSingleUseDownload(t *testing.T) {
	e := newEnv(t)
	s := e.login("admin", "pw-for-tests-1")
	path := "/C/Users/Public/Documents/Quarterly-Report.txt"
	res, b := e.req("POST", "/api/v1/devices/"+e.dev.ID+"/restores", s, CreateRestoreRequest{SnapshotID: fixtures.FixtureSnapshotID, Mode: "download_zip", Path: path})
	if res.StatusCode != 202 {
		t.Fatalf("create: %d %s", res.StatusCode, b)
	}
	var rj RestoreJSON
	_ = json.Unmarshal(b, &rj)
	if rj.Status != "ready" || !rj.DownloadReady || res.Header.Get("Location") != "/api/v1/restores/"+rj.ID {
		t.Fatalf("restore: %+v", rj)
	}
	if strings.Contains(string(b), "Quarterly") || strings.Contains(string(b), "Documents") || strings.Contains(string(b), "token") {
		t.Fatalf("neither the path nor a token may appear in a status body: %s", b)
	}
	_, b = e.req("GET", "/api/v1/restores/"+rj.ID, s, nil)
	if strings.Contains(string(b), "token") {
		t.Fatalf("a status body carries no token: %s", b)
	}
	// Token alone is not enough: no session, no download.
	url1, _ := e.mint(s, rj.ID)
	if res, b := e.req("GET", url1, nil, nil); res.StatusCode != 401 {
		t.Fatalf("token without a session: %d %s", res.StatusCode, b)
	}
	// Another admin's session cannot use it, nor can a second session of the same admin.
	e.addAdmin("second", "owner", nil)
	other := e.login("second", "pw-for-tests-1")
	if res, b := e.req("GET", url1, other, nil); res.StatusCode != 403 || problemCode(t, res, b) != api.CodeForbidden {
		t.Fatalf("a token from another session: %d", res.StatusCode)
	}
	again := e.login("admin", "pw-for-tests-1")
	if res, _ := e.req("GET", url1, again, nil); res.StatusCode != 403 {
		t.Fatalf("token from another session of the same admin: %d", res.StatusCode)
	}
	// ADR 0008: the zip streams from the repository into the response. No
	// Range, no announced length (net/http adds one only when a small body was
	// complete before the first flush); delivered, with size and SHA-256, after
	// the last byte.
	res, full := e.req("GET", url1, s, nil, "Range", "bytes=0-9")
	if res.StatusCode != 200 || res.Header.Get("Accept-Ranges") != "" || res.Header.Get("Content-Range") != "" ||
		(res.ContentLength != -1 && res.ContentLength != int64(len(full))) {
		t.Fatalf("a streamed download ignores Range: %d len=%d %v", res.StatusCode, res.ContentLength, res.Header)
	}
	if res.Header.Get("Content-Type") != "application/zip" || !strings.Contains(res.Header.Get("Content-Disposition"), "attachment") ||
		res.Header.Get("Referrer-Policy") != "no-referrer" || res.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("download headers: %v", res.Header)
	}
	zr, err := zip.NewReader(bytes.NewReader(full), int64(len(full)))
	if err != nil || len(zr.File) != 1 {
		t.Fatalf("zip: %v", err)
	}
	f, _ := zr.File[0].Open()
	content, _ := io.ReadAll(f)
	if !strings.Contains(string(content), "Quarterly report") {
		t.Fatalf("content %q", content)
	}
	sum := sha256.Sum256(full)
	_, b = e.req("GET", "/api/v1/restores/"+rj.ID, s, nil)
	got := RestoreJSON{}
	_ = json.Unmarshal(b, &got)
	if got.Status != "delivered" || got.DownloadCount != 1 || got.Bytes == nil || *got.Bytes != int64(len(full)) || got.SHA256 != hex.EncodeToString(sum[:]) || !got.DownloadReady {
		t.Fatalf("after the last byte: %+v", got)
	}
	if f := filesIn(e.dir); len(f) != 0 {
		t.Fatalf("a download left files on disk: %v", f)
	}
	// Single use: the token is dead once used.
	res, b = e.req("GET", url1, s, nil)
	if res.StatusCode != 403 {
		t.Fatalf("reuse of a used token: %d", res.StatusCode)
	}
	problemCode(t, res, b)
	// A second download needs a new token and streams again; minting replaces the earlier token.
	old, _ := e.mint(s, rj.ID)
	fresh, _ := e.mint(s, rj.ID)
	if res, _ := e.req("GET", old, s, nil); res.StatusCode != 403 {
		t.Fatalf("minting again must invalidate the previous token: %d", res.StatusCode)
	}
	res, body := e.req("GET", fresh, s, nil)
	if res.StatusCode != 200 || !bytes.Equal(body, full) {
		t.Fatalf("second download: %d, %d bytes", res.StatusCode, len(body))
	}
	if cur, _ := e.st.GetRestoreRequest(context.Background(), store.ScopeAll(), rj.ID); cur.Status != "delivered" || cur.DownloadCount != 2 {
		t.Fatalf("second download: %+v", cur)
	}
	// At expires_at the restore offers nothing more.
	e.clk.Advance(restores.DefaultTTL)
	s = e.login("admin", "pw-for-tests-1") // the old session aged with the clock
	if res, b := e.req("POST", "/api/v1/restores/"+rj.ID+"/download", s, nil); res.StatusCode != http.StatusGone || problemCode(t, res, b) != api.CodeNotFound {
		t.Fatalf("mint after expiry: %d", res.StatusCode)
	}
	_, b = e.req("GET", "/api/v1/restores/"+rj.ID, s, nil)
	got = RestoreJSON{}
	_ = json.Unmarshal(b, &got)
	if got.DownloadReady {
		t.Fatalf("an expired restore must offer no download: %+v", got)
	}

	// Expiry: two minutes. The session is still alive; the token is not.
	third := e.newRestore(s, path)
	exp, _ := e.mint(s, third.ID)
	e.clk.Advance(2*time.Minute + time.Second)
	if res, _ := e.req("GET", exp, s, nil); res.StatusCode != 403 {
		t.Fatalf("expired token: %d", res.StatusCode)
	}
	// A token for one restore does not open another.
	fourth := e.newRestore(s, path)
	u4, _ := e.mint(s, fourth.ID)
	tok := u4[strings.Index(u4, "token="):]
	if res, _ := e.req("GET", "/api/v1/restores/"+third.ID+"/download?"+tok, s, nil); res.StatusCode == 200 {
		t.Fatal("a token must not open a different restore")
	}
	if res, _ := e.req("GET", "/api/v1/restores/"+fourth.ID+"/download?token=garbage", s, nil); res.StatusCode != 403 {
		t.Fatalf("garbage token: %d", res.StatusCode)
	}
	if res, _ := e.req("GET", "/api/v1/restores/"+fourth.ID+"/download", s, nil); res.StatusCode != 403 {
		t.Fatalf("no token: %d", res.StatusCode)
	}
}

// filesIn lists every file under dir.
func filesIn(dir string) []string {
	var out []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			out = append(out, p)
		}
		return nil
	})
	return out
}

func TestRestoreRefusalsAndRoles(t *testing.T) {
	e := newEnv(t)
	s := e.login("admin", "pw-for-tests-1")
	create := func(sess *session, dev string, req CreateRestoreRequest) (*http.Response, []byte) {
		return e.req("POST", "/api/v1/devices/"+dev+"/restores", sess, req)
	}
	good := CreateRestoreRequest{SnapshotID: fixtures.FixtureSnapshotID, Mode: "download_zip", Path: "/C/Users/Public/Desktop/Welcome.txt"}
	res, b := create(s, e.held.ID, good)
	if res.StatusCode != 409 || problemCode(t, res, b) != api.CodeCustodyCustomerHeld {
		t.Fatalf("customer-held: %d %s", res.StatusCode, b)
	}
	for name, req := range map[string]CreateRestoreRequest{
		"other mode":       {SnapshotID: good.SnapshotID, Mode: "device_original", Path: good.Path},
		"unknown snapshot": {SnapshotID: strings.Repeat("a", 64), Mode: "download_zip", Path: good.Path},
		"relative path":    {SnapshotID: good.SnapshotID, Mode: "download_zip", Path: "C/x"},
	} {
		res, b := create(s, e.dev.ID, req)
		if res.StatusCode != 400 || problemCode(t, res, b) != api.CodeInvalidRequest {
			t.Errorf("%s: %d %s", name, res.StatusCode, b)
		}
	}
	if res, _ := create(s, "01J9ZZ0000000000000000ZZZZ", good); res.StatusCode != 404 {
		t.Fatalf("unknown device: %d", res.StatusCode)
	}
	if res, b := e.req("POST", "/api/v1/devices/"+e.dev.ID+"/restores", s, `{"snapshot_id":"x","bogus":true}`); res.StatusCode != 400 {
		t.Fatalf("strict decoding: %d %s", res.StatusCode, b)
	}
	// read_only: may read a restore, may not create one nor get a download link.
	e.addAdmin("viewer", "read_only", nil)
	ro := e.login("viewer", "pw-for-tests-1")
	res, b = create(ro, e.dev.ID, good)
	if res.StatusCode != 403 || problemCode(t, res, b) != api.CodeForbidden {
		t.Fatalf("read_only create: %d", res.StatusCode)
	}
	_, b = create(s, e.dev.ID, good)
	var rj RestoreJSON
	_ = json.Unmarshal(b, &rj)
	res, b = e.req("GET", "/api/v1/restores/"+rj.ID, ro, nil)
	var seen RestoreJSON
	_ = json.Unmarshal(b, &seen)
	if res.StatusCode != 200 || seen.DownloadReady {
		t.Fatalf("read_only status: %d %+v", res.StatusCode, seen)
	}
	if res, _ := e.req("POST", "/api/v1/restores/"+rj.ID+"/download", ro, nil); res.StatusCode != 403 {
		t.Fatalf("read_only mint: %d", res.StatusCode)
	}
	if res, _ := e.req("GET", "/api/v1/restores/"+rj.ID+"/download?token=x", ro, nil); res.StatusCode != 403 {
		t.Fatalf("read_only download: %d", res.StatusCode)
	}
	if res, _ := e.req("GET", "/api/v1/restores/01J9ZZ0000000000000000ZZZZ", s, nil); res.StatusCode != 404 {
		t.Fatalf("unknown restore: %d", res.StatusCode)
	}
}

// TestRestoreFailureShowsAClass: a download that fails before its first byte is
// a problem answer; the class is recorded and the restore stays downloadable.
func TestRestoreFailureShowsAClass(t *testing.T) {
	e := newEnv(t)
	s := e.login("admin", "pw-for-tests-1")
	e.be.Errors = map[string]error{"dump": errNoSuchSnapshot()}
	rj := e.newRestore(s, "/C/Users/Public/Desktop/Welcome.txt")
	if rj.Status != "ready" || !rj.DownloadReady {
		t.Fatalf("create: %+v", rj)
	}
	u, _ := e.mint(s, rj.ID)
	res, b := e.req("GET", u, s, nil)
	if res.StatusCode == 200 || res.Header.Get("Content-Type") == "application/zip" {
		t.Fatalf("a failed stream answered %d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}
	problemCode(t, res, b)
	_, b = e.req("GET", "/api/v1/restores/"+rj.ID, s, nil)
	var got RestoreJSON
	_ = json.Unmarshal(b, &got)
	if got.Status != "ready" || got.ErrorClass == "" || !got.DownloadReady || got.Bytes != nil {
		t.Fatalf("after a failed download: %+v", got)
	}
}

// TestMidStreamFailureAbortsTheResponse: once bytes went out, a failure must not
// end the response cleanly (a browser would keep a truncated zip as complete):
// the connection is cut, and the failure is recorded.
func TestMidStreamFailureAbortsTheResponse(t *testing.T) {
	e := newEnv(t)
	s := e.login("admin", "pw-for-tests-1")
	e.be.FailDumpAfter = 64
	rj := e.newRestore(s, "/C/Users/Public/Documents")
	u, _ := e.mint(s, rj.ID)
	req, _ := http.NewRequest("GET", e.srv.URL+u, nil)
	for _, c := range s.cookies {
		req.AddCookie(c)
	}
	res, err := e.srv.Client().Do(req)
	if err == nil {
		_, err = io.ReadAll(res.Body)
		res.Body.Close()
	}
	if err == nil {
		t.Fatal("a stream that failed mid-way ended like a complete download")
	}
	cur, _ := e.st.GetRestoreRequest(context.Background(), store.ScopeAll(), rj.ID)
	if cur.Status != "ready" || cur.ErrorClass != "network" || cur.DownloadCount != 0 {
		t.Fatalf("after a cut stream: %+v", cur)
	}
}

func TestVersion(t *testing.T) {
	e := newEnv(t)
	s := e.login("admin", "pw-for-tests-1")
	res, b := e.req("GET", "/api/v1/version", s, nil)
	var v VersionResponse
	_ = json.Unmarshal(b, &v)
	if res.StatusCode != 200 || v.Schema < 1 || v.Console == "" || v.Maintain == nil || !v.Maintain.Reachable || v.ComputedAt.IsZero() {
		t.Fatalf("%d %+v", res.StatusCode, v)
	}
	// The ping carries no tool versions yet: the route says "unknown" rather than leaving the field out or guessing.
	if v.Maintain.ResticVersion != "unknown" || v.Maintain.RcloneVersion != "unknown" {
		t.Fatalf("tool versions: %+v", v.Maintain)
	}
}

func errNoSuchSnapshot() error {
	return adminpath.NewError(adminpath.CodeNotFound, restic.ClassUnknown)
}

// Account-enumeration / lockout oracle: an unknown user, a wrong password and a
// locked account (even with the right password) answer with the same status,
// content type and body, so the response says nothing about the account.
func TestLoginFailuresAreIndistinguishable(t *testing.T) {
	e := newEnv(t)
	e.addAdmin("lockme", "read_only", nil)
	// Lock "lockme": five wrong passwords (LockAfter is 5).
	for i := 0; i < 5; i++ {
		if res, b := e.req("POST", "/api/v1/auth/login", nil, LoginRequest{Username: "lockme", Password: "nope"}); res.StatusCode != 401 {
			t.Fatalf("priming failure %d: %d %s", i, res.StatusCode, b)
		}
	}
	if a, err := e.st.GetAdminByUsername(context.Background(), "lockme"); err != nil || a.LockedUntil == nil {
		t.Fatalf("the priming did not lock the account: %+v %v", a, err)
	}
	type outcome struct {
		status int
		ctype  string
		body   string
	}
	rows := []struct {
		name string
		req  LoginRequest
	}{
		{"unknown user", LoginRequest{Username: "ghost", Password: "nope"}},
		{"wrong password", LoginRequest{Username: "admin", Password: "nope"}},
		{"locked, wrong password", LoginRequest{Username: "lockme", Password: "nope"}},
		{"locked, right password", LoginRequest{Username: "lockme", Password: "pw-for-tests-1"}},
	}
	var first *outcome
	for _, row := range rows {
		res, b := e.req("POST", "/api/v1/auth/login", nil, row.req)
		got := outcome{res.StatusCode, res.Header.Get("Content-Type"), string(b)}
		if got.status != 401 {
			t.Errorf("%s: status %d, want 401: %s", row.name, got.status, got.body)
		}
		if len(res.Cookies()) != 0 {
			t.Errorf("%s: a failed login set cookies", row.name)
		}
		if first == nil {
			first = &got
			continue
		}
		if got != *first {
			t.Errorf("%s differs from %s:\n got  %+v\n want %+v", row.name, rows[0].name, got, *first)
		}
	}
}
