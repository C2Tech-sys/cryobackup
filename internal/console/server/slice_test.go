package server_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/console/adminapi"
	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/cli"
	"github.com/c2tech-sys/cryobackup/internal/console/config"
	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
	"github.com/c2tech-sys/cryobackup/internal/logx"
	"github.com/c2tech-sys/cryobackup/internal/restic"
	"github.com/c2tech-sys/cryobackup/internal/ulid"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) Advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

func runCLI(t *testing.T, args ...string) string {
	t.Helper()
	var out, errb bytes.Buffer
	if code := cli.Main(args, cli.IO{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errb}); code != 0 {
		t.Fatalf("cryo-console %s: exit %d\n%s%s", args[0], code, out.String(), errb.String())
	}
	return out.String()
}

// agentCall is the fake agent: it speaks the api wire types to the agent listener.
type agentCall struct {
	t    *testing.T
	base string
}

func (a agentCall) do(method, path, token string, body any, out any) int {
	a.t.Helper()
	var rd io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	}
	r, _ := http.NewRequest(method, a.base+path, rd)
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(r)
	if err != nil {
		a.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if out != nil && res.StatusCode == 200 {
		if err := json.Unmarshal(b, out); err != nil {
			a.t.Fatalf("%s: %v: %s", path, err, b)
		}
	}
	return res.StatusCode
}

type adminCall struct {
	t        *testing.T
	base     string
	cookies  []*http.Cookie
	csrf     string
	recovery []string // M2-W1A: recovery codes from the enrollment (mfa_helpers_test.go)
}

func (a *adminCall) login(user, pw string) {
	a.t.Helper()
	raw, _ := json.Marshal(adminapi.LoginRequest{Username: user, Password: pw})
	res, err := http.Post(a.base+"/api/v1/auth/login", "application/json", bytes.NewReader(raw))
	if err != nil {
		a.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 {
		a.t.Fatalf("login: %d %s", res.StatusCode, b)
	}
	var lr adminapi.LoginResponse
	_ = json.Unmarshal(b, &lr)
	a.cookies, a.csrf = res.Cookies(), lr.CSRFToken
	a.finishMFA(lr) // M2-W1A: the password step is not a login
}

func (a *adminCall) do(method, path string, body any) (int, []byte, http.Header) {
	a.t.Helper()
	var rd io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	}
	r, _ := http.NewRequest(method, a.base+path, rd)
	r.Header.Set("Content-Type", "application/json")
	for _, c := range a.cookies {
		r.AddCookie(c)
	}
	if method != "GET" {
		r.Header.Set("Origin", a.base)
		r.Header.Set(auth.CSRFHeader, a.csrf)
	}
	res, err := http.DefaultClient.Do(r)
	if err != nil {
		a.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, b, res.Header
}

// TestM0SliceInProcess is the milestone-M0 slice against a fake admin path:
// init -> customer + token -> /enroll from a fake agent (the api types) -> policy
// pull -> heartbeat -> run upload -> the board shows the device green -> the agent
// goes silent past unseen_after -> the board row is red -> a technician restores
// one file as a zip -> its sha256 equals the original. The same flow runs against
// the real maintain worker, restic and rclone in TestLiveM0SliceWithMaintain
// (live_slice_test.go, gated on CRYO_RESTIC and CRYO_RCLONE).
// diskFiles lists every file under the given roots except the database (its
// WAL and journal files come and go) and the directories named in skip.
func diskFiles(t *testing.T, root string, skip ...string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		for _, s := range skip {
			if p == s {
				return filepath.SkipDir
			}
		}
		if !d.IsDir() && !strings.Contains(filepath.Base(p), ".db") {
			out[p] = true
		}
		return nil
	})
	return out
}

// sameDisk fails for every file that appeared since before.
func sameDisk(t *testing.T, when string, before, after map[string]bool) {
	t.Helper()
	for p := range after {
		if !before[p] {
			t.Errorf("%s: a file appeared on the console host's disk: %s", when, p)
		}
	}
}

func TestM0SliceInProcess(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	out := runCLI(t, "init", "--config", cfgPath, "--data-dir", filepath.Join(dir, "data"), "--non-interactive", "--master-key-copy-confirmed",
		"--endpoint", "s3.example.invalid", "--bucket", "slice-bucket", "--prefix", "p", "--gateway-url", "https://gateway.example.invalid:9443",
		"--owner", "owner", "--timezone", "America/Chicago", "--agent-listen", "127.0.0.1:0", "--admin-listen", "127.0.0.1:0")
	pwm := regexp.MustCompile(`Password \(shown once[^)]*\): (\S+)`).FindStringSubmatch(out)
	if pwm == nil {
		t.Fatalf("init printed no owner password:\n%s", out)
	}
	ownerPW := pwm[1]
	if info, err := os.Stat(cfgPath); err != nil || info.Size() == 0 {
		t.Fatal("config.json missing")
	}
	tokOut := runCLI(t, "token", "--config", cfgPath, "--create-customer", "Slice Customer", "--uses", "5", "--expires", "2d")
	tokm := regexp.MustCompile(`never stored\):\n(\S+)`).FindStringSubmatch(tokOut)
	if tokm == nil {
		t.Fatalf("token printed no secret:\n%s", tokOut)
	}
	enrollToken := tokm[1]

	cfg, err := config.Read(cfgPath, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	cfg.InsecureHTTPLab, cfg.AgentListen, cfg.AdminListen = true, "127.0.0.1:0", "127.0.0.1:0"
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	clk := &fakeClock{t: time.Now().UTC().Truncate(time.Second)}
	var logs syncBuf
	be := &fixtures.Backend{}
	app, err := cli.Build(context.Background(), cfg, cli.BuildOptions{Clock: clk.Now, Backend: be, Log: logx.New(&logs, logx.LevelDebug)})
	if err != nil {
		t.Fatal(err)
	}
	// serve registers every M0 job, command-expiry included (FIX7).
	have := map[string]bool{}
	for _, n := range app.Runner.Names() {
		have[n] = true
	}
	for _, want := range []string{"htpasswd-sync", "heartbeat-ring-trim", "restore-purge", "session-expire", "command-expiry"} {
		if !have[want] {
			t.Errorf("serve does not register the %s job (have %v)", want, app.Runner.Names())
		}
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
	agent := agentCall{t: t, base: "http://" + app.Server.AgentAddr()}
	admin := &adminCall{t: t, base: "http://" + app.Server.AdminAddr()}
	gen := ulid.NewGenerator(clk.Now, rand.Reader)
	newID := func() string { id, _ := gen.New(); return id }

	// 1. Enroll.
	var er api.EnrollResponse
	req := api.EnrollRequest{Nonce: strings.Repeat("c3", 16), Hostname: "SLICE-PC-01", OS: "windows", OSVersion: "Windows 11 Pro", Arch: api.ArchAMD64, DeviceClass: api.DeviceClassDesktop,
		HWFingerprint: strings.Repeat("ab", 16), AgentVersion: "1.4.2", ResticVersion: "0.19.1"}
	if code := agent.do("POST", api.RouteEnroll, enrollToken, req, &er); code != 200 {
		t.Fatalf("enroll: %d\nlogs:\n%s", code, logs.String())
	}
	if er.DeviceID == "" || er.Custody != api.CustodyEscrowed || len(er.RepoPassword) != 64 || er.Gateway.Username != er.DeviceID {
		t.Fatalf("enroll response: %+v", er)
	}
	if !ulid.Valid(er.CustomerID) || !ulid.Valid(er.Policy.PolicyID) {
		t.Fatalf("enroll must carry customer_id and policy_id ULIDs: %q %q", er.CustomerID, er.Policy.PolicyID)
	}
	devToken := string(er.DeviceToken)
	if be.Count("init") != 1 || be.Count("cat_config") != 1 {
		t.Fatalf("the repository must be initialised and checked through the admin path: init=%d cat=%d", be.Count("init"), be.Count("cat_config"))
	}
	// htpasswd-sync wrote the gateway's login file, with the device and the probe login.
	htpasswd := filepath.Join(cfg.GatewayAuthDir, "htpasswd")
	deadline := time.Now().Add(15 * time.Second)
	for {
		b, err := os.ReadFile(htpasswd)
		if err == nil && strings.Contains(string(b), er.DeviceID+":$2y$") && strings.Contains(string(b), "probe:$2y$") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("htpasswd never got the new login (err=%v)\nlogs:\n%s", err, logs.String())
		}
		time.Sleep(50 * time.Millisecond)
	}

	// 2. Policy pull: pending gateway secret now, a 304 once it is acknowledged.
	var pr api.PolicyResponse
	if code := agent.do("GET", api.RoutePolicy, devToken, nil, &pr); code != 200 || pr.PendingGatewaySecret == nil || string(pr.PendingGatewaySecret.Password) != string(er.Gateway.Password) {
		t.Fatalf("policy: %d %+v", code, pr)
	}
	// 3. Two hours later the agent heartbeats (acknowledging the gateway secret).
	clk.Advance(2 * time.Hour)
	var hr api.HeartbeatResponse
	hb := api.HeartbeatRequest{AgentVersion: "1.4.2", ResticVersion: "0.19.1", UptimeS: 100, DeviceTime: clk.Now().Add(2 * time.Second), ServiceState: api.ServiceIdle,
		PolicyETag: pr.ETag, PolicyVersionApplied: pr.Version, HWFingerprint: req.HWFingerprint, GatewaySecretAcked: true}
	if code := agent.do("POST", api.RouteHeartbeat, devToken, hb, &hr); code != 200 || hr.ClockOffsetMS != 2000 {
		t.Fatalf("heartbeat: %d %+v", code, hr)
	}
	// 4. Run upload and snapshot cache.
	fin := clk.Now().Add(-10 * time.Minute)
	code0 := 0
	run := api.RunRecord{ID: newID(), Kind: restic.KindBackup, Trigger: api.TriggerSchedule, StartedAt: fin.Add(-12 * time.Minute), FinishedAt: &fin, Outcome: restic.OutcomeSuccess,
		ExitCode: &code0, SnapshotID: fixtures.FixtureSnapshotID, Attempt: 1, PolicyVersion: pr.Version, AgentVersion: "1.4.2", Initial: true,
		Stats: api.RunStats{TotalFilesProcessed: 5000, DataAdded: 1 << 20}}
	var rr api.RunsResponse
	if code := agent.do("POST", api.RouteRuns, devToken, api.RunsRequest{Runs: []api.RunRecord{run}}, &rr); code != 200 || rr.Results[0].Status != api.RunCreated {
		t.Fatalf("runs: %d %+v", code, rr)
	}
	snap := restic.Snapshot{ID: fixtures.FixtureSnapshotID, ShortID: fixtures.FixtureSnapshotID[:8], Time: fin, Paths: []string{`C:\Users`}, Hostname: "SLICE-PC-01"}
	if code := agent.do("POST", api.RouteSnapshots, devToken, api.SnapshotsRequest{Snapshots: []restic.Snapshot{snap}}, nil); code != 200 {
		t.Fatalf("snapshots: %d", code)
	}
	clk.Advance(30 * time.Second)
	var pr2 api.PolicyResponse
	if code := agent.do("GET", api.RoutePolicy, devToken, nil, &pr2); code != 200 || pr2.PendingGatewaySecret != nil {
		t.Fatalf("acknowledged secret must be gone: %d %+v", code, pr2)
	}

	// 5. The board turns green.
	admin.login("owner", ownerPW)
	state := func(host string) string {
		code, b, _ := admin.do("GET", "/api/v1/devices?q="+host, nil)
		if code != 200 {
			t.Fatalf("devices: %d %s", code, b)
		}
		var l adminapi.DeviceListResponse
		_ = json.Unmarshal(b, &l)
		if len(l.Devices) != 1 {
			t.Fatalf("expected one %s, got %d (%s)", host, len(l.Devices), b)
		}
		return l.Devices[0].State
	}
	if s := state("SLICE-PC-01"); s != "OK" {
		t.Fatalf("after a heartbeat and a successful run the device must be OK, got %s", s)
	}
	code, page, _ := admin.do("GET", "/", nil)
	if code != 200 || !strings.Contains(string(page), "SLICE-PC-01") || !strings.Contains(string(page), "Slice Customer") || !strings.Contains(string(page), `class="st ok`) {
		t.Fatalf("board: %d (missing the green device)", code)
	}
	if strings.Contains(string(page), "AGENT API SILENT") {
		t.Fatal("no banner while agents are checking in")
	}

	// 6. Stop heartbeats, advance past unseen_after_hours: the row turns red.
	clk.Advance(169 * time.Hour)
	admin.login("owner", ownerPW) // the session itself expired meanwhile
	if s := state("SLICE-PC-01"); s != "UNSEEN" {
		t.Fatalf("a silent device must be UNSEEN, got %s", s)
	}
	_, page, _ = admin.do("GET", "/", nil)
	for _, want := range []string{"UNSEEN", `class="st crit`} {
		if !strings.Contains(string(page), want) {
			t.Errorf("board lacks %q after the agent went silent", want)
		}
	}
	// ADR 0013 (F5): a device that merely went quiet changes only its own row; the
	// console-level agent banner comes from the endpoint self-probe, not silence.
	for _, not := range []string{"AGENT API UNREACHABLE", "No agent has checked in for"} {
		if strings.Contains(string(page), not) {
			t.Errorf("board shows %q although only one device is asleep", not)
		}
	}

	// 7. Technician restore of one file as a zip; the sha256 matches the original,
	// and nothing restored lands on the console's disk (ADR 0008).
	diskBefore := diskFiles(t, dir)
	path := "/C/Users/Public/Documents/Quarterly-Report.txt"
	code, b, _ := admin.do("POST", "/api/v1/devices/"+er.DeviceID+"/restores", adminapi.CreateRestoreRequest{SnapshotID: fixtures.FixtureSnapshotID, Mode: "download_zip", Path: path})
	if code != 202 {
		t.Fatalf("restore: %d %s", code, b)
	}
	var rj adminapi.RestoreJSON
	_ = json.Unmarshal(b, &rj)
	deadline = time.Now().Add(15 * time.Second)
	for rj.Status != "ready" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		_, b, _ = admin.do("GET", "/api/v1/restores/"+rj.ID, nil)
		_ = json.Unmarshal(b, &rj)
	}
	if rj.Status != "ready" || !rj.DownloadReady {
		t.Fatalf("restore never became ready: %+v", rj)
	}
	code, b, _ = admin.do("POST", "/api/v1/restores/"+rj.ID+"/download", nil)
	var dr adminapi.DownloadResponse
	_ = json.Unmarshal(b, &dr)
	if code != 200 {
		t.Fatalf("mint: %d %s", code, b)
	}
	code, zipBytes, hdr := admin.do("GET", dr.DownloadURL, nil)
	if code != 200 || hdr.Get("Content-Type") != "application/zip" {
		t.Fatalf("download: %d %s", code, hdr.Get("Content-Type"))
	}
	zr, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil || len(zr.File) != 1 {
		t.Fatalf("zip: %v", err)
	}
	f, _ := zr.File[0].Open()
	got, _ := io.ReadAll(f)
	want := fixtures.DefaultFiles()[0].Content
	if sha256.Sum256(got) != sha256.Sum256(want) {
		t.Fatalf("restored sha256 %x differs from the original %x", sha256.Sum256(got), sha256.Sum256(want))
	}
	t.Logf("M0 slice (in-process, fake admin path): restored file sha256=%s", hex.EncodeToString(func() []byte { s := sha256.Sum256(got); return s[:] }()))
	if code, _, _ := admin.do("GET", dr.DownloadURL, nil); code == 200 {
		t.Fatal("the download token is single use")
	}
	sameDisk(t, "after the restore", diskBefore, diskFiles(t, dir))

	// 8. Nothing sensitive reached a log line.
	logText := logs.String()
	for name, secret := range map[string]string{"device token": devToken, "repo password": string(er.RepoPassword), "gateway password": string(er.Gateway.Password),
		"enrollment token": enrollToken, "owner password": ownerPW, "download token": dr.DownloadURL[strings.Index(dr.DownloadURL, "token=")+6:]} {
		if secret != "" && strings.Contains(logText, secret) {
			t.Errorf("%s reached the log", name)
		}
	}
	for _, bad := range []string{"token=", "Quarterly", "Documents", `C:\`, "/C/Users"} {
		if strings.Contains(logText, bad) {
			t.Errorf("log contains %q", bad)
		}
	}
	if !strings.Contains(logText, `"route":"GET__api_v1_restores__id__download"`) {
		t.Errorf("the download is logged by route pattern only:\n%s", logText)
	}
	// /healthz agrees the console is healthy.
	res, err := http.Get(admin.base + "/healthz")
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("healthz: %v", err)
	}
	res.Body.Close()
}
