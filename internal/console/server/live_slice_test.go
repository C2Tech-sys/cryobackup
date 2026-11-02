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
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/console/adminapi"
	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/console/cli"
	"github.com/c2tech-sys/cryobackup/internal/console/config"
	"github.com/c2tech-sys/cryobackup/internal/console/maintain"
	"github.com/c2tech-sys/cryobackup/internal/logx"
	"github.com/c2tech-sys/cryobackup/internal/restic"
	"github.com/c2tech-sys/cryobackup/internal/ulid"
)

// TestLiveM0SliceWithMaintain is the milestone-M0 slice against the REAL
// maintenance worker, restic 0.19.1 and rclone 1.75.1, with a local directory as
// the storage remote (an rclone alias, reached through restic's rclone: backend):
//
//	init -> customer + token -> /enroll (the console's real Init and CatConfig
//	through the admin-path socket) -> policy -> heartbeat -> a backup made with the
//	restic binary directly into the device's repository (the agent is not part of
//	this test and there is no gateway) -> runs upload -> the board is green ->
//	clock advance -> UNSEEN -> a technician restores one file through the real
//	Ls and Dump -> mint -> download -> sha256 equals the original -> log scan.
//
// maintain.Worker runs in-process on a unix socket, over the console's own store
// and the console's own key unwrapper (cli.MaintainKeys). It is skipped unless
// both binaries are named:
//
//	CRYO_RESTIC=<restic.exe> CRYO_RCLONE=<rclone.exe> go test -count=1 -run LiveM0 -v ./internal/console/server
func TestLiveM0SliceWithMaintain(t *testing.T) {
	resticBin, rcloneBin := os.Getenv("CRYO_RESTIC"), os.Getenv("CRYO_RCLONE")
	if resticBin == "" || rcloneBin == "" {
		t.Skip("CRYO_RESTIC and CRYO_RCLONE are not both set")
	}
	for _, p := range []string{resticBin, rcloneBin} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s: %v", filepath.Base(p), err)
		}
	}
	rawRestic, err := os.ReadFile(resticBin)
	if err != nil {
		t.Fatal(err)
	}
	resticSum := sha256.Sum256(rawRestic)

	// A short base directory: unix socket paths are limited to about 108 bytes.
	base, err := os.MkdirTemp("", "cs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	remoteDir := filepath.Join(base, "remote")
	if err := os.MkdirAll(remoteDir, 0o755); err != nil {
		t.Fatal(err)
	}
	rcloneConf := filepath.Join(base, "rclone.conf")
	if err := os.WriteFile(rcloneConf, []byte("[admin]\ntype = alias\nremote = "+filepath.ToSlash(remoteDir)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// 1. init, customer and token through the real commands.
	cfgPath := filepath.Join(base, "config.json")
	out := runCLI(t, "init", "--config", cfgPath, "--data-dir", filepath.Join(base, "data"), "--non-interactive", "--master-key-copy-confirmed",
		"--endpoint", "s3.example.invalid", "--bucket", "live-bucket", "--prefix", "p", "--gateway-url", "https://gateway.example.invalid:9443",
		"--owner", "owner", "--timezone", "America/Chicago", "--agent-listen", "127.0.0.1:0", "--admin-listen", "127.0.0.1:0")
	pwm := regexp.MustCompile(`Password \(shown once[^)]*\): (\S+)`).FindStringSubmatch(out)
	if pwm == nil {
		t.Fatalf("init printed no owner password:\n%s", out)
	}
	ownerPW := pwm[1]
	tokOut := runCLI(t, "token", "--config", cfgPath, "--create-customer", "Live Customer", "--uses", "5", "--expires", "2d")
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
	cfg.AdminpathSocket = filepath.Join(base, "a.sock")
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	// 2. The console (real admin-path client) and the real worker on the socket.
	clk := &fakeClock{t: time.Now().UTC().Truncate(time.Second)}
	var logs syncBuf
	lg := logx.New(&logs, logx.LevelDebug)
	app, err := cli.Build(context.Background(), cfg, cli.BuildOptions{Clock: clk.Now, Log: lg})
	if err != nil {
		t.Fatal(err)
	}
	keys, err := cli.MaintainKeys(cfg.MasterKeyPath, app.Store)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := maintain.New(maintain.Config{
		SocketPath: cfg.AdminpathSocket, ResticProgram: resticBin, ResticSHA256: hex.EncodeToString(resticSum[:]),
		RcloneProgram: rcloneBin, RcloneConfig: rcloneConf, Remote: "admin", Bucket: "live-bucket", Prefix: "p",
		ScratchDir: filepath.Join(base, "scratch"), TickEvery: time.Hour, Version: "live-test", Logger: lg,
	}, app.Store, keys, clk.Now)
	if err != nil {
		t.Fatalf("maintain.New: %v", err)
	}
	if err := app.Server.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	appDone, workerDone := make(chan error, 1), make(chan error, 1)
	go func() { workerDone <- worker.Loop(ctx) }()
	go func() { appDone <- app.Run(ctx) }()
	defer func() {
		cancel()
		for name, ch := range map[string]chan error{"console": appDone, "worker": workerDone} {
			select {
			case <-ch:
			case <-time.After(30 * time.Second):
				t.Errorf("%s shutdown timed out", name)
			}
		}
	}()
	// Wait until the socket answers.
	deadline := time.Now().Add(30 * time.Second)
	for {
		pctx, pcancel := context.WithTimeout(ctx, 3*time.Second)
		pr, perr := app.Backend.Ping(pctx)
		pcancel()
		if perr == nil {
			if pr.Version != "live-test" || !pr.MasterKeyLoaded {
				t.Fatalf("ping: %+v", pr)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the worker never answered on the socket: %v\nlogs:\n%s", perr, logs.String())
		}
		time.Sleep(50 * time.Millisecond)
	}

	agent := agentCall{t: t, base: "http://" + app.Server.AgentAddr()}
	admin := &adminCall{t: t, base: "http://" + app.Server.AdminAddr()}
	gen := ulid.NewGenerator(clk.Now, rand.Reader)
	newID := func() string { id, _ := gen.New(); return id }

	// 3. Enroll: the real Init and CatConfig run through the socket against the local remote.
	var er api.EnrollResponse
	req := api.EnrollRequest{Nonce: strings.Repeat("d4", 16), Hostname: "LIVE-PC-01", OS: "windows", OSVersion: "Windows 11 Pro", Arch: api.ArchAMD64, DeviceClass: api.DeviceClassDesktop,
		HWFingerprint: strings.Repeat("ab", 16), AgentVersion: "1.4.2", ResticVersion: "0.19.1"}
	if code := agent.do("POST", api.RouteEnroll, enrollToken, req, &er); code != 200 {
		t.Fatalf("enroll: %d\nlogs:\n%s", code, logs.String())
	}
	if !ulid.Valid(er.DeviceID) || !ulid.Valid(er.CustomerID) || !ulid.Valid(er.Policy.PolicyID) || er.Custody != api.CustodyEscrowed || len(er.RepoPassword) != 64 {
		t.Fatalf("enroll response: %+v", er)
	}
	devToken := string(er.DeviceToken)
	repoDir := filepath.Join(remoteDir, "live-bucket", "p", er.DeviceID)
	if _, err := os.Stat(filepath.Join(repoDir, "config")); err != nil {
		t.Fatalf("the real restic init did not create the repository under the local remote: %v", err)
	}
	cctx, ccancel := context.WithTimeout(ctx, time.Minute)
	cfgDoc, err := app.Backend.CatConfig(cctx, adminpath.DeviceRequest{DeviceID: er.DeviceID, RequestID: "live-cat"})
	ccancel()
	if err != nil || cfgDoc.ID == "" {
		t.Fatalf("CatConfig through the socket: %+v %v", cfgDoc, err)
	}

	// 4. Policy pull, then (two hours later) the heartbeat that acknowledges the gateway secret.
	var pr api.PolicyResponse
	if code := agent.do("GET", api.RoutePolicy, devToken, nil, &pr); code != 200 || pr.PendingGatewaySecret == nil || pr.PolicyID != er.Policy.PolicyID {
		t.Fatalf("policy: %d %+v", code, pr)
	}
	clk.Advance(2 * time.Hour)
	var hr api.HeartbeatResponse
	hb := api.HeartbeatRequest{AgentVersion: "1.4.2", ResticVersion: "0.19.1", UptimeS: 100, DeviceTime: clk.Now().Add(2 * time.Second), ServiceState: api.ServiceIdle,
		PolicyETag: pr.ETag, PolicyVersionApplied: pr.Version, HWFingerprint: req.HWFingerprint, GatewaySecretAcked: true}
	if code := agent.do("POST", api.RouteHeartbeat, devToken, hb, &hr); code != 200 {
		t.Fatalf("heartbeat: %d", code)
	}

	// 5. The "agent": a real backup with the restic binary into the device's repository, no gateway.
	tree := filepath.Join(base, "tree")
	if err := os.MkdirAll(filepath.Join(tree, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	original := make([]byte, 200*1024)
	_, _ = rand.Read(original)
	original = append([]byte("quarterly report\n"), original...)
	for name, body := range map[string][]byte{"docs/Quarterly-Report.txt": original, "hello.txt": []byte("hello\n")} {
		if err := os.WriteFile(filepath.Join(tree, filepath.FromSlash(name)), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	repo, err := restic.AdminRepository("admin", "live-bucket", "p", er.DeviceID)
	if err != nil {
		t.Fatal(err)
	}
	bcmd, err := restic.Backup(restic.BackupOptions{
		Common: restic.Common{Extra: restic.AdminOptions(filepath.ToSlash(rcloneBin), false)},
		Host:   er.DeviceID, Roots: []string{tree},
	})
	if err != nil {
		t.Fatal(err)
	}
	benv, err := restic.Env{Repository: repo, Password: string(er.RepoPassword), CacheDir: filepath.Join(base, "agent-cache")}.Environ(nil)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(resticBin, bcmd.Args...)
	cmd.Env = append(append(os.Environ(), benv...), "RCLONE_CONFIG="+rcloneConf)
	var berr bytes.Buffer
	cmd.Stderr = &berr
	if err := cmd.Run(); err != nil {
		t.Fatalf("restic backup: %v\n%s", err, berr.String())
	}
	sctx, scancel := context.WithTimeout(ctx, time.Minute)
	snaps, err := app.Backend.Snapshots(sctx, adminpath.DeviceRequest{DeviceID: er.DeviceID, RequestID: "live-snaps"})
	scancel()
	if err != nil || len(snaps) != 1 {
		t.Fatalf("Snapshots through the socket: %d %v", len(snaps), err)
	}
	snap := snaps[0]

	// 6. Run upload and the snapshot cache, as the agent would.
	fin := clk.Now().Add(-10 * time.Minute)
	code0 := 0
	run := api.RunRecord{ID: newID(), Kind: restic.KindBackup, Trigger: api.TriggerSchedule, StartedAt: fin.Add(-12 * time.Minute), FinishedAt: &fin, Outcome: restic.OutcomeSuccess,
		ExitCode: &code0, SnapshotID: snap.ID, Attempt: 1, PolicyVersion: pr.Version, AgentVersion: "1.4.2", Initial: true,
		Stats: api.RunStats{TotalFilesProcessed: 2, DataAdded: uint64(len(original))}}
	var rr api.RunsResponse
	if code := agent.do("POST", api.RouteRuns, devToken, api.RunsRequest{Runs: []api.RunRecord{run}}, &rr); code != 200 || rr.Results[0].Status != api.RunCreated {
		t.Fatalf("runs: %d %+v", code, rr)
	}
	upload := snap
	upload.Username = "" // the agent never sends the OS user name (api validation: must be empty)
	if code := agent.do("POST", api.RouteSnapshots, devToken, api.SnapshotsRequest{Snapshots: []restic.Snapshot{upload}}, nil); code != 200 {
		t.Fatalf("snapshots: %d", code)
	}

	// 7. The board is green.
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
	if s := state("LIVE-PC-01"); s != "OK" {
		t.Fatalf("after a heartbeat and a successful run the device must be OK, got %s", s)
	}
	// The version route reports the worker that really answered.
	code, vb, _ := admin.do("GET", "/api/v1/version", nil)
	var ver adminapi.VersionResponse
	_ = json.Unmarshal(vb, &ver)
	if code != 200 || ver.Maintain == nil || !ver.Maintain.Reachable || ver.Maintain.Version != "live-test" || !ver.Maintain.MasterKeyLoaded {
		t.Fatalf("version: %d %s", code, vb)
	}

	// 8. The agent goes silent past unseen_after: the board row is red.
	clk.Advance(169 * time.Hour)
	admin.login("owner", ownerPW)
	if s := state("LIVE-PC-01"); s != "UNSEEN" {
		t.Fatalf("a silent device must be UNSEEN, got %s", s)
	}

	// 9. The technician finds the file with the real Ls, then restores it with the real Dump.
	treeSnap := toSnapshotPath(tree)
	lctx, lcancel := context.WithTimeout(ctx, time.Minute)
	level, err := app.Backend.Ls(lctx, adminpath.LsRequest{DeviceRequest: adminpath.DeviceRequest{DeviceID: er.DeviceID, RequestID: "live-ls"}, SnapshotID: snap.ID, Dir: treeSnap + "/docs"})
	lcancel()
	if err != nil || len(level.Nodes) != 1 || level.Nodes[0].Name != "Quarterly-Report.txt" || level.Nodes[0].Type != "file" {
		t.Fatalf("Ls through the socket: %+v %v", level, err)
	}
	path := treeSnap + "/docs/Quarterly-Report.txt"
	// ADR 0008: nothing restored lands on the console host's disk: not in the
	// data or artifacts directories, not in the worker's scratch space, not in
	// restic's default cache. (The repository directory is the storage target.)
	userCache := ""
	if d, err := os.UserCacheDir(); err == nil {
		userCache = filepath.Join(d, "restic")
	}
	diskBefore, cacheBefore := diskFiles(t, base, remoteDir), diskFiles(t, userCache)
	code, b, _ := admin.do("POST", "/api/v1/devices/"+er.DeviceID+"/restores", adminapi.CreateRestoreRequest{SnapshotID: snap.ID, Mode: "download_zip", Path: path})
	if code != 202 {
		t.Fatalf("restore: %d %s\nlogs:\n%s", code, b, logs.String())
	}
	var rj adminapi.RestoreJSON
	_ = json.Unmarshal(b, &rj)
	deadline = time.Now().Add(90 * time.Second)
	for rj.Status != "ready" && rj.Status != "failed" && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		_, b, _ = admin.do("GET", "/api/v1/restores/"+rj.ID, nil)
		_ = json.Unmarshal(b, &rj)
	}
	if rj.Status != "ready" || !rj.DownloadReady {
		t.Fatalf("restore never became ready: %+v\nlogs:\n%s", rj, logs.String())
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
	f, err := zr.File[0].Open()
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(f)
	if sha256.Sum256(got) != sha256.Sum256(original) {
		t.Fatalf("restored sha256 %x differs from the original %x", sha256.Sum256(got), sha256.Sum256(original))
	}
	sum := sha256.Sum256(got)
	t.Logf("M0 slice LIVE (maintain + restic 0.19.1 + rclone): enrolled %s, init/cat config/snapshots/ls/dump through the socket, restored file sha256=%s (%d bytes) equals the original",
		er.DeviceID, hex.EncodeToString(sum[:]), len(got))
	if code, _, _ := admin.do("GET", dr.DownloadURL, nil); code == 200 {
		t.Fatal("the download token is single use")
	}
	sameDisk(t, "after the live restore", diskBefore, diskFiles(t, base, remoteDir))
	sameDisk(t, "restic's default cache after the live restore", cacheBefore, diskFiles(t, userCache))
	_, b, _ = admin.do("GET", "/api/v1/restores/"+rj.ID, nil)
	_ = json.Unmarshal(b, &rj)
	zsum := sha256.Sum256(zipBytes)
	if rj.Status != "delivered" || rj.Bytes == nil || *rj.Bytes != int64(len(zipBytes)) || rj.SHA256 != hex.EncodeToString(zsum[:]) {
		t.Fatalf("the delivery is not recorded with its size and sha256: %+v", rj)
	}

	// 10. Nothing sensitive reached a log line, the worker's included.
	logText := logs.String()
	for name, secret := range map[string]string{"device token": devToken, "repo password": string(er.RepoPassword), "gateway password": string(er.Gateway.Password),
		"enrollment token": enrollToken, "owner password": ownerPW, "download token": dr.DownloadURL[strings.Index(dr.DownloadURL, "token=")+6:]} {
		if secret != "" && strings.Contains(logText, secret) {
			t.Errorf("%s reached the log", name)
		}
	}
	for _, bad := range []string{"token=", "Quarterly", "hello.txt", filepath.ToSlash(tree), tree, base, "/docs"} {
		if strings.Contains(logText, bad) {
			t.Errorf("log contains %q", bad)
		}
	}
	if !strings.Contains(logText, "restic run finished") {
		t.Errorf("the worker logged no restic run:\n%s", logText)
	}
	res, err := http.Get(admin.base + "/healthz")
	if err != nil {
		t.Fatalf("healthz: %v", err)
	}
	res.Body.Close()
}

// toSnapshotPath turns a local path into restic's snapshot path form: forward
// slashes, and "C:\x" becomes "/C/x".
func toSnapshotPath(p string) string {
	p = filepath.ToSlash(p)
	if len(p) >= 2 && p[1] == ':' {
		p = "/" + p[:1] + p[2:]
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return p
}
