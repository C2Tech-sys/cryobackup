package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/agent/config"
	"github.com/c2tech-sys/cryobackup/internal/agent/diag"
	"github.com/c2tech-sys/cryobackup/internal/agent/internal/fakerestic"
	"github.com/c2tech-sys/cryobackup/internal/agent/journal"
	"github.com/c2tech-sys/cryobackup/internal/agent/secrets"
	"github.com/c2tech-sys/cryobackup/internal/agent/supervisor"
	"github.com/c2tech-sys/cryobackup/internal/agent/uplink"
	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/policy"
	"github.com/c2tech-sys/cryobackup/internal/restic"
	"github.com/c2tech-sys/cryobackup/internal/ulid"
)

func openJournalRO(t *testing.T, stateDir string) *journal.Journal {
	t.Helper()
	jr, err := journal.Open(context.Background(), journal.Options{
		Path: config.Layout{Root: stateDir}.JournalFile(), ReadOnly: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = jr.Close() })
	return jr
}

var testIDs = ulid.NewGenerator(time.Now, nil)

// newULID returns a fresh, valid run or command id.
func newULID() string { return testIDs.MustNew() }

// command builds a console command with a fresh id (n only numbers the call sites).
func command(n int, typ api.CommandType, params string) api.Command {
	_ = n
	c := api.Command{ID: newULID(), Type: typ, CreatedAt: time.Now().Add(-time.Minute), ExpiresAt: time.Now().Add(time.Hour)}
	if params != "" {
		c.Params = json.RawMessage(params)
	}
	return c
}

// ---- enrollment -----------------------------------------------------------

func TestEnrollStoresEverythingAndRetriesWithTheSameNonce(t *testing.T) {
	fc := newFakeConsole(t)
	fc.dropEnrollOnce.Store(true) // the console stores the enrollment, the response is lost
	dir := filepath.Join(t.TempDir(), "state")
	opts := EnrollOptions{
		StateDir: dir, ServerURL: fc.URL(), Token: testEnrollToken, InsecureLab: true, InsecurePlaintextSecrets: plainSecrets(),
		AllowUnprotectedState: true,
		RetryBudget:           -1, // this test drives the retry itself, across two calls; enroll_retry_test.go covers the in-call retry
	}
	layout := config.Layout{Root: dir}

	if _, err := Enroll(context.Background(), opts); err == nil {
		t.Fatal("the first enroll should fail: its response was lost")
	}
	cfg, err := config.Load(layout.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.EnrollNonce == "" || len(cfg.EnrollNonce) != 32 || cfg.Enrolled() {
		t.Fatalf("after the lost response: nonce %q enrolled %v; the nonce must be on disk and the device not enrolled", cfg.EnrollNonce, cfg.Enrolled())
	}

	res, err := Enroll(context.Background(), opts)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	fc.mu.Lock()
	nonces, token := append([]string(nil), fc.enrollNonces...), fc.deviceToken
	fc.mu.Unlock()
	if len(nonces) != 2 || nonces[0] != nonces[1] || nonces[0] != cfg.EnrollNonce {
		t.Fatalf("nonces seen by the console: %v (stored %s): a retry must reuse the nonce", nonces, cfg.EnrollNonce)
	}
	if res.DeviceID != testDeviceID || res.Custody != api.CustodyEscrowed || res.PolicyVersion != 1 {
		t.Errorf("result = %+v", res)
	}

	// The latest response wins: the token the console issued last is the one stored.
	store, err := secrets.New(secrets.Options{Dir: layout.SecretsDir(), InsecurePlaintext: plainSecrets()})
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		secrets.NameDeviceToken: token, secrets.NameGatewayCred: testGatewaySec, secrets.NameRepoKey: testRepoSecret,
	} {
		got, err := store.Get(name)
		if err != nil || string(got) != want {
			t.Errorf("secret %s = %q (%v), want %q", name, got, err, want)
		}
	}
	if token != "device-token-2" {
		t.Errorf("console token = %q, want the second issue", token)
	}

	// Nothing secret in agent.json, the policy mirror or the journal.
	cfg, _ = config.Load(layout.ConfigFile())
	if cfg.DeviceID != testDeviceID || cfg.GatewayURL != "https://gateway.example:9443" || cfg.Custody != "escrowed" || !cfg.InsecureLab {
		t.Errorf("config = %+v", cfg)
	}
	if cfg.CustomerID != testCustomerID {
		t.Errorf("customer id in agent.json = %q, want the console's %q (the snapshot tag cb.customer)", cfg.CustomerID, testCustomerID)
	}
	jr := openJournalRO(t, dir)
	raw, ok, _ := jr.KVGet(context.Background(), journal.KVPolicyJSON)
	if !ok {
		t.Fatal("the first policy was not stored")
	}
	for _, file := range []string{layout.ConfigFile(), layout.PolicyFile(), layout.ETagFile(), layout.JournalFile()} {
		body := readFile(t, file)
		if contains(body, testGatewaySec, testRepoSecret, "device-token-") {
			t.Errorf("a secret is in %s", filepath.Base(file))
		}
	}
	if contains(raw, testGatewaySec, testRepoSecret) {
		t.Error("a secret is in the stored policy")
	}
	if etag := readFile(t, layout.ETagFile()); etag != `"policy-v1"` {
		t.Errorf("etag file = %q", etag)
	}

	// Enrolling again is refused: a second enrollment is a new device.
	if _, err := Enroll(context.Background(), opts); !errors.Is(err, ErrAlreadyEnrolled) {
		t.Errorf("third enroll: %v, want ErrAlreadyEnrolled", err)
	}
}

func TestEnrollRefusesBadInput(t *testing.T) {
	fc := newFakeConsole(t)
	base := EnrollOptions{StateDir: filepath.Join(t.TempDir(), "s"), ServerURL: fc.URL(), Token: "wrong", InsecureLab: true, InsecurePlaintextSecrets: plainSecrets(), AllowUnprotectedState: true}
	_, err := Enroll(context.Background(), base)
	var ue *uplink.Error
	if !errors.As(err, &ue) || ue.Status != 401 {
		t.Fatalf("wrong token: %v", err)
	}
	if _, err := os.Stat(config.Layout{Root: base.StateDir}.ConfigFile()); err != nil {
		t.Error("the nonce should be persisted even when the attempt fails")
	}
	noTok := base
	noTok.Token = " "
	if _, err := Enroll(context.Background(), noTok); err == nil {
		t.Error("an empty token was accepted")
	}
	httpNoLab := base
	httpNoLab.Token, httpNoLab.InsecureLab, httpNoLab.StateDir = testEnrollToken, false, filepath.Join(t.TempDir(), "s2")
	if _, err := Enroll(context.Background(), httpNoLab); err == nil {
		t.Error("an http console was accepted without the lab flag")
	}

	// A console that sends no customer id: nothing is stored (the snapshot
	// tag cb.customer would have no value, and enrollment is the only time it arrives).
	fc.omitCustomer.Store(true)
	noCust := base
	noCust.Token, noCust.StateDir = testEnrollToken, filepath.Join(t.TempDir(), "s3")
	if _, err := Enroll(context.Background(), noCust); err == nil || !strings.Contains(err.Error(), "customer id") {
		t.Errorf("an enrollment without a customer id: %v", err)
	}
	if cfg, _ := config.Load(config.Layout{Root: noCust.StateDir}.ConfigFile()); cfg.Enrolled() {
		t.Error("the device is enrolled without a customer id")
	}
	fc.omitCustomer.Store(false)
}

// An unprivileged enrollment cannot restrict the state directory: it is
// refused before the console is contacted (no token is spent) unless the lab
// flag is given.
func TestEnrollRefusesAnUnprotectedStateDirectory(t *testing.T) {
	if runtime.GOOS == "windows" && secrets.Elevated() {
		t.Skip("elevated: the state directory is restricted here, so there is nothing to refuse (P1-T8 runs the elevated path)")
	}
	fc := newFakeConsole(t)
	o := EnrollOptions{StateDir: filepath.Join(t.TempDir(), "s"), ServerURL: fc.URL(), Token: testEnrollToken, InsecureLab: true, InsecurePlaintextSecrets: plainSecrets()}
	_, err := Enroll(context.Background(), o)
	if !errors.Is(err, ErrACLRefused) || !strings.Contains(err.Error(), "--insecure-unprotected-state") {
		t.Fatalf("unprotected enrollment: %v, want ErrACLRefused naming the lab flag", err)
	}
	fc.mu.Lock()
	n := len(fc.enrollNonces)
	fc.mu.Unlock()
	if n != 0 {
		t.Errorf("the console was contacted %d times before the refusal", n)
	}
	o.AllowUnprotectedState = true
	res, err := Enroll(context.Background(), o)
	if err != nil {
		t.Fatalf("with the lab flag: %v", err)
	}
	if res.ACLApplied || res.StateWarning == "" {
		t.Errorf("result = %+v; the lab enrollment must say the state directory is not verified", res)
	}
}

// ---- boot -----------------------------------------------------------------

func TestBootRefusalsAndReportedConditions(t *testing.T) {
	ctx := context.Background()
	fc := newFakeConsole(t)

	if _, err := New(ctx, Options{StateDir: filepath.Join(t.TempDir(), "empty"), InsecurePlaintextSecrets: plainSecrets(), AllowUnprotectedState: true}); !errors.Is(err, ErrNotEnrolled) {
		t.Fatalf("not enrolled: %v", err)
	}

	t.Run("console mode without the lab flag refuses an unprotected state directory", func(t *testing.T) {
		if runtime.GOOS == "windows" && secrets.Elevated() {
			t.Skip("elevated: the state directory is restricted here (P1-T8 runs the elevated path)")
		}
		dir := enroll(t, fc)
		o := fakeOptions(t, dir)
		o.AllowUnprotectedState = false
		if _, err := New(ctx, o); !errors.Is(err, ErrACLRefused) {
			t.Fatalf("New without the lab flag: %v, want ErrACLRefused", err)
		}
	})
	t.Run("the lab flag is warned about on every start and shown by status", func(t *testing.T) {
		fakerestic.Use(t, "backup_ok")
		dir := enroll(t, fc)
		a, err := New(ctx, fakeOptions(t, dir))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		a.Close()
		if log := readFile(t, config.Layout{Root: dir}.AgentLog()); !strings.Contains(log, "INSECURE: --insecure-unprotected-state") {
			t.Errorf("agent.log carries no warning about the lab flag:\n%s", log)
		}
		rep, err := diag.Status(ctx, dir, time.Now())
		// Elevated (Windows), the agent restricts and verifies the state
		// directory even when the lab flag is given; unelevated it cannot.
		wantVerified := runtime.GOOS == "windows" && secrets.Elevated()
		if err != nil || rep.InsecureState == "" || rep.StateVerified != wantVerified {
			t.Errorf("status: insecure %q verified %v, want %v (%v)", rep.InsecureState, rep.StateVerified, wantVerified, err)
		}
	})
	t.Run("a state directory that changes while the agent runs stops backups and reports permission", func(t *testing.T) {
		fakerestic.Use(t, "backup_ok")
		dir := enroll(t, fc)
		o := fakeOptions(t, dir)
		tampered := false
		o.verifyState = func(string) error {
			if tampered {
				return config.ErrStateUntrusted
			}
			return nil
		}
		a, err := New(ctx, o)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		defer a.Close()
		a.stateVerified = true // as after a verified start (needs elevation, see PrepareState)
		if err := a.sendHeartbeat(ctx); err != nil {
			t.Fatal(err)
		}
		if _, ok := a.plannerInput(ctx, time.Now()); !ok {
			t.Fatal("a verified state directory does not schedule")
		}
		tampered = true
		if err := a.sendHeartbeat(ctx); err != nil {
			t.Fatal(err)
		}
		hbs := fc.heartbeatList()
		if last := hbs[len(hbs)-1]; last.LastErrorClass != restic.ClassPermission {
			t.Errorf("heartbeat class = %q, want permission", last.LastErrorClass)
		}
		if _, ok := a.plannerInput(ctx, time.Now()); ok {
			t.Error("the scheduler would plan on a state directory whose ACL changed")
		}
		tampered = false // the verdict holds until a restart verifies again
		if hb := a.buildHeartbeat(ctx); hb.LastErrorClass != restic.ClassPermission {
			t.Errorf("after the descriptor reads back again: class %q, want permission until a restart", hb.LastErrorClass)
		}
	})

	t.Run("no device token is a refusal", func(t *testing.T) {
		dir := enroll(t, fc)
		if err := os.Remove(filepath.Join(config.Layout{Root: dir}.SecretsDir(), secrets.NameDeviceToken)); err != nil {
			t.Fatal(err)
		}
		if _, err := New(ctx, fakeOptions(t, dir)); err == nil {
			t.Fatal("started without a device token")
		}
	})
	t.Run("a restic binary that fails verification is reported, not fatal", func(t *testing.T) {
		fakerestic.Use(t, "backup_ok")
		dir := enroll(t, fc)
		o := fakeOptions(t, dir)
		o.ResticSHA256 = strings.Repeat("0", 64)
		a, err := New(ctx, o)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		defer a.Close()
		if !errors.Is(a.resticErr, supervisor.ErrHashMismatch) {
			t.Fatalf("resticErr = %v", a.resticErr)
		}
		if hb := a.buildHeartbeat(ctx); hb.LastErrorClass != restic.ClassGuardRefused {
			t.Errorf("heartbeat class = %q, want guard_refused", hb.LastErrorClass)
		}
		if _, ok := a.plannerInput(ctx, time.Now()); ok {
			t.Error("the scheduler would plan with a restic binary that failed verification")
		}
	})
	t.Run("customer-held custody: no repository key, backups refused and reported", func(t *testing.T) {
		fakerestic.Use(t, "backup_ok")
		dir := enroll(t, fc)
		if err := os.Remove(filepath.Join(config.Layout{Root: dir}.SecretsDir(), secrets.NameRepoKey)); err != nil {
			t.Fatal(err)
		}
		a, err := New(ctx, fakeOptions(t, dir))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		defer a.Close()
		if hb := a.buildHeartbeat(ctx); hb.LastErrorClass != restic.ClassGuardRefused {
			t.Errorf("heartbeat class = %q", hb.LastErrorClass)
		}
		if _, ok := a.plannerInput(ctx, time.Now()); ok {
			t.Error("the scheduler would plan without a repository key")
		}
	})
	t.Run("ACL not applied is fatal in service mode only", func(t *testing.T) {
		fakerestic.Use(t, "backup_ok")
		dir := enroll(t, fc)
		o := fakeOptions(t, dir)
		o.ServiceMode = true
		a, err := New(ctx, o)
		if runtime.GOOS == "windows" && secrets.Elevated() {
			if err != nil {
				t.Fatalf("elevated service mode: %v", err)
			}
			a.Close()
			return
		}
		if !errors.Is(err, ErrACLRefused) {
			t.Fatalf("service mode without elevation: %v, want ErrACLRefused", err)
		}
	})
}

func TestBootClosesStaleRuns(t *testing.T) {
	ctx := context.Background()
	fc := newFakeConsole(t)
	fakerestic.Use(t, "backup_ok")
	dir := enroll(t, fc)

	// A previous process died with a run open.
	jr, err := journal.Open(ctx, journal.Options{Path: config.Layout{Root: dir}.JournalFile()})
	if err != nil {
		t.Fatal(err)
	}
	stale := api.RunRecord{ID: newULID(), Kind: restic.KindBackup, Trigger: api.TriggerSchedule,
		StartedAt: time.Now().Add(-3 * time.Hour).UTC(), Attempt: 1, PolicyVersion: 1, AgentVersion: "old"}
	if err := jr.StartRun(ctx, stale); err != nil {
		t.Fatal(err)
	}
	_ = jr.Close()

	a, err := New(ctx, fakeOptions(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	row, ok, _ := a.jr.Run(ctx, stale.ID)
	if !ok || row.Outcome != restic.OutcomeAbandoned || row.FinishedAt == nil {
		t.Fatalf("stale run = %+v", row)
	}
	if err := a.deliverOutbox(ctx); err != nil {
		t.Fatal(err)
	}
	got, ok := fc.run(stale.ID)
	if !ok || got.Outcome != restic.OutcomeAbandoned {
		t.Fatalf("the console's copy: %+v %v", got, ok)
	}
	if n, _ := a.jr.OutboxDepth(ctx); n != 0 {
		t.Errorf("outbox depth = %d after delivery", n)
	}
}

// ---- policy ---------------------------------------------------------------

func TestPolicyValidationKeepsThePreviousPolicy(t *testing.T) {
	ctx := context.Background()
	fc := newFakeConsole(t)
	dir := enroll(t, fc)
	fakerestic.Use(t, "backup_ok")
	a, err := New(ctx, fakeOptions(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	v2 := api.PolicyResponse{Policy: testPolicy(), PolicyID: testPolicyID, Version: 2, ETag: `"policy-v2"`}
	if err := a.applyPolicy(ctx, v2); err != nil {
		t.Fatal(err)
	}
	if a.pol.Version != 2 || a.polETag != `"policy-v2"` {
		t.Fatalf("policy = v%d %s", a.pol.Version, a.polETag)
	}

	bad := v2
	bad.Version, bad.ETag = 3, `"policy-v3"`
	bad.Policy.Selection.Roots = []string{"relative\\path"}
	if err := a.applyPolicy(ctx, bad); err == nil {
		t.Fatal("an invalid policy was accepted")
	}
	if a.pol.Version != 2 || a.polETag != `"policy-v2"` {
		t.Errorf("the previous policy was not kept: v%d %s", a.pol.Version, a.polETag)
	}
	if hb := a.buildHeartbeat(ctx); hb.LastErrorClass != restic.ClassParse || hb.PolicyVersionApplied != 2 || hb.PolicyETag != `"policy-v2"` {
		t.Errorf("heartbeat = class %q version %d etag %s; want parse and the old policy", hb.LastErrorClass, hb.PolicyVersionApplied, hb.PolicyETag)
	}
	if raw, _, _ := a.jr.KVGet(ctx, journal.KVPolicyJSON); !strings.Contains(raw, `"version":2`) {
		t.Error("the last-known-good policy in the journal was replaced by the invalid one")
	}

	good := v2
	good.Version, good.ETag = 4, `"policy-v4"`
	if err := a.applyPolicy(ctx, good); err != nil {
		t.Fatal(err)
	}
	if hb := a.buildHeartbeat(ctx); hb.LastErrorClass != "" || hb.PolicyVersionApplied != 4 {
		t.Errorf("after a valid policy: class %q version %d", hb.LastErrorClass, hb.PolicyVersionApplied)
	}

	// A restart loads the last-known-good policy without the console.
	a.Close()
	a2, err := New(ctx, fakeOptions(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	defer a2.Close()
	if a2.pol == nil || a2.pol.Version != 4 || a2.polETag != `"policy-v4"` {
		t.Fatalf("after restart: %+v", a2.pol)
	}
}

func TestGatewayCredentialRotation(t *testing.T) {
	ctx := context.Background()
	fc := newFakeConsole(t)
	dir := enroll(t, fc)
	fakerestic.Use(t, "backup_ok")
	a, err := New(ctx, fakeOptions(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	const rotated = "rotated-gateway-password-xyz"
	resp := api.PolicyResponse{Policy: testPolicy(), PolicyID: testPolicyID, Version: 2, ETag: `"policy-v2"`,
		PendingGatewaySecret: &api.GatewaySecret{Password: rotated, IssuedAt: time.Now()}}
	if err := a.applyPolicy(ctx, resp); err != nil {
		t.Fatal(err)
	}
	got, err := a.sec.Get(secrets.NameGatewayCred)
	if err != nil || string(got) != rotated {
		t.Fatalf("stored gateway credential = %q %v", got, err)
	}
	if raw, _, _ := a.jr.KVGet(ctx, journal.KVPolicyJSON); strings.Contains(raw, rotated) {
		t.Error("the rotated secret is in the journal")
	}
	if hb := a.buildHeartbeat(ctx); !hb.GatewaySecretAcked {
		t.Fatal("the heartbeat does not acknowledge the rotation")
	}

	// The next successful heartbeat carries the ack and clears it (DH13).
	if err := a.sendHeartbeat(ctx); err != nil {
		t.Fatal(err)
	}
	hbs := fc.heartbeatList()
	if len(hbs) != 1 || !hbs[0].GatewaySecretAcked {
		t.Fatalf("heartbeats = %+v", hbs)
	}
	if hb := a.buildHeartbeat(ctx); hb.GatewaySecretAcked {
		t.Error("the ack is repeated after it was delivered")
	}
	if _, ok, _ := a.jr.KVGet(ctx, journal.KVGatewayAckPend); ok {
		t.Error("the ack flag survived a delivered heartbeat")
	}
	// A rotation that arrives with a policy that does not validate is still applied:
	// the console has already changed the login, so the device must not be left without it.
	const rotatedAgain = "second-rotated-gateway-password"
	badPol := api.PolicyResponse{Policy: testPolicy(), PolicyID: testPolicyID, Version: 3, ETag: `"policy-v3"`,
		PendingGatewaySecret: &api.GatewaySecret{Password: rotatedAgain, IssuedAt: time.Now()}}
	badPol.Policy.Selection.Roots = []string{"relative"}
	if err := a.applyPolicy(ctx, badPol); err == nil {
		t.Fatal("an invalid policy was accepted")
	}
	if got, _ := a.sec.Get(secrets.NameGatewayCred); string(got) != rotatedAgain {
		t.Errorf("gateway credential after an invalid policy with a rotation = %q", got)
	}
	// The repository the next backup uses carries the new password, by environment only.
	spec, err := a.baseSpec(restic.Command{Kind: restic.KindBackup, Args: []string{"backup"}})
	if err != nil || !strings.Contains(spec.Env.Repository, rotatedAgain) {
		t.Fatalf("repository after rotation: %v", err)
	}
}

// ---- runs end to end (fake restic) -----------------------------------------

func TestBackupEndToEnd(t *testing.T) {
	ctx := context.Background()
	fc := newFakeConsole(t)
	dir := enroll(t, fc)
	fakerestic.Use(t, "backup_ok")
	layout := config.Layout{Root: dir}
	r := start(t, fakeOptions(t, dir))

	waitFor(t, "the closed run at the console", 30*time.Second, func() bool { return len(fc.closedRuns()) == 1 })
	run := fc.closedRuns()[0]
	if run.Kind != restic.KindBackup || run.Outcome != restic.OutcomeSuccess || run.ExitCode == nil || *run.ExitCode != 0 {
		t.Fatalf("run = %+v", run)
	}
	if len(run.SnapshotID) != 64 || run.Stats.FilesNew != 5 || run.Stats.TotalFilesProcessed != 5 || run.Stats.FailedFiles != 0 {
		t.Errorf("snapshot %q stats %+v", run.SnapshotID, run.Stats)
	}
	if !run.Initial || run.Attempt != 1 || run.PolicyVersion != 1 || run.Trigger != api.TriggerBoot || run.AgentVersion == "" {
		t.Errorf("run facts = initial %v attempt %d policy %d trigger %s agent %q", run.Initial, run.Attempt, run.PolicyVersion, run.Trigger, run.AgentVersion)
	}
	if run.FinishedAt == nil || run.FinishedAt.Before(run.StartedAt) || time.Since(*run.FinishedAt) > time.Minute {
		t.Errorf("times = %v .. %v", run.StartedAt, run.FinishedAt)
	}
	if len(run.ErrorCounts) != 0 || run.ErrorClass != "" {
		t.Errorf("a clean run reported errors: %+v %q", run.ErrorCounts, run.ErrorClass)
	}
	if err := api.AssertPathFree(run); err != nil {
		t.Errorf("the uploaded record is not path-free: %v", err)
	}

	// The snapshot list follows a success, without the account name.
	waitFor(t, "the snapshot upload", 30*time.Second, func() bool { return len(fc.snapshotUploads()) >= 1 })
	for _, s := range fc.snapshotUploads()[0].Snapshots {
		if s.Username != "" || len(s.ID) != 64 {
			t.Errorf("uploaded snapshot = %+v", s)
		}
	}

	// Heartbeats are valid, identify the device and end idle.
	waitFor(t, "an idle heartbeat after the run", 30*time.Second, func() bool {
		hbs := fc.heartbeatList()
		return len(hbs) >= 2 && hbs[len(hbs)-1].ServiceState == api.ServiceIdle
	})
	hbs := fc.heartbeatList()
	last := hbs[len(hbs)-1]
	if last.ResticVersion != supervisor.PinnedVersion || len(last.HWFingerprint) != 64 || last.PolicyETag != `"policy-v1"` || last.PolicyVersionApplied != 1 {
		t.Errorf("heartbeat = %+v", last)
	}
	if time.Since(last.DeviceTime) > time.Minute {
		t.Errorf("device time = %v", last.DeviceTime)
	}

	// The journal knows the delivery; the local files are where they should be.
	jr := openJournalRO(t, dir)
	waitFor(t, "delivered_at in the journal", 30*time.Second, func() bool {
		row, ok, _ := jr.Run(ctx, run.ID)
		return ok && row.DeliveredAt != nil
	})
	if n, _ := jr.OutboxDepth(ctx); n != 0 {
		t.Errorf("outbox depth = %d", n)
	}
	logLines := strings.Split(strings.TrimSpace(readFile(t, layout.RunLog(run.ID))), "\n")
	if want := len(fixtureLines(t, "backup_ok.stdout.jsonl", "backup_ok.stderr.jsonl")); len(logLines) != want {
		t.Errorf("run log has %d lines, the fixtures %d", len(logLines), want)
	}
	if ex := readFile(t, layout.ExcludeFile()); !strings.Contains(ex, "$$Recycle.Bin") {
		t.Error("excludes.txt was not generated from the policy")
	}
	if rt := readFile(t, layout.RootsFile()); !strings.Contains(rt, `C:\fixture\tree`) {
		t.Errorf("roots.txt = %q", rt)
	}
	if entries, _ := os.ReadDir(layout.TempDir()); len(entries) != 0 {
		t.Errorf("tmp holds %d entries after the run", len(entries))
	}

	// No second run: the interval is a day and this one succeeded.
	time.Sleep(700 * time.Millisecond)
	if n := len(fc.closedRuns()); n != 1 {
		t.Errorf("%d runs after a success; the next is a day away", n)
	}

	r.stop()
	rep, err := diag.Status(ctx, dir, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	rep.Render(&buf)
	out := buf.String()
	for _, want := range []string{testDeviceID, "success", "policy:", "version 1", "last heartbeat:", "next due:"} {
		if !strings.Contains(out, want) {
			t.Errorf("status output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, dir) || strings.Contains(out, `C:\fixture`) {
		t.Errorf("status output carries a path:\n%s", out)
	}
}

func fixtureLines(t *testing.T, names ...string) []string {
	t.Helper()
	var out []string
	for _, n := range names {
		for _, l := range strings.Split(strings.TrimSpace(readFile(t, filepath.Join(fakerestic.Dir(), n))), "\n") {
			if strings.TrimSpace(l) != "" {
				out = append(out, l)
			}
		}
	}
	return out
}

func TestFailedBackupIsReportedLoudlyAndBacksOff(t *testing.T) {
	fc := newFakeConsole(t)
	dir := enroll(t, fc)
	fakerestic.Use(t, "exit12")
	start(t, fakeOptions(t, dir))

	waitFor(t, "the failed run", 30*time.Second, func() bool { return len(fc.closedRuns()) == 1 })
	run := fc.closedRuns()[0]
	if run.Outcome != restic.OutcomeFailed || run.ErrorClass != restic.ClassWrongPassword || run.ExitCode == nil || *run.ExitCode != 12 || run.SnapshotID != "" {
		t.Fatalf("run = %+v", run)
	}
	// The failure is on the next heartbeat, and the device is not hammering the gateway.
	waitFor(t, "a heartbeat that reports the class", 30*time.Second, func() bool {
		hbs := fc.heartbeatList()
		return len(hbs) > 0 && hbs[len(hbs)-1].LastErrorClass == restic.ClassWrongPassword
	})
	time.Sleep(800 * time.Millisecond)
	if n := len(fc.closedRuns()); n != 1 {
		t.Errorf("%d runs; a failed run must wait out its backoff (minutes) before the retry", n)
	}
	if len(fc.snapshotUploads()) != 0 {
		t.Error("a snapshot list was uploaded after a failure")
	}
}

func TestUnreachableConsoleBacksUpAndQueues(t *testing.T) {
	ctx := context.Background()
	fc := newFakeConsole(t)
	dir := enroll(t, fc)
	fakerestic.Use(t, "backup_ok")
	fc.down.Store(true)
	start(t, fakeOptions(t, dir))
	jr := openJournalRO(t, dir)

	// §6.7: the console is unreachable, the agent backs up on the last-known-good policy and queues the result.
	waitFor(t, "a finished run in the journal", 30*time.Second, func() bool {
		rows, _ := jr.RecentRuns(ctx, restic.KindBackup, 5)
		return len(rows) == 1 && rows[0].FinishedAt != nil && rows[0].Outcome == restic.OutcomeSuccess
	})
	if n, _ := jr.OutboxDepth(ctx); n != 1 {
		t.Fatalf("outbox depth = %d, want the one undelivered result", n)
	}
	if len(fc.closedRuns()) != 0 || len(fc.heartbeatList()) != 0 {
		t.Fatal("the console received something while it was down")
	}

	fc.down.Store(false)
	waitFor(t, "the queued result to arrive once the console is back", 40*time.Second, func() bool { return len(fc.closedRuns()) == 1 })
	waitFor(t, "an empty outbox", 30*time.Second, func() bool { n, _ := jr.OutboxDepth(ctx); return n == 0 })
	rows, _ := jr.RecentRuns(ctx, restic.KindBackup, 5)
	if rows[0].DeliveredAt == nil {
		t.Error("delivered_at not set")
	}
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if len(fc.runs) != 1 {
		t.Errorf("the console holds %d runs, want 1 (idempotent on the run id)", len(fc.runs))
	}
}

func TestRejectedRunIsJournaledAndNotRetried(t *testing.T) {
	ctx := context.Background()
	fc := newFakeConsole(t)
	dir := enroll(t, fc)
	fakerestic.Use(t, "backup_ok")
	a, err := New(ctx, fakeOptions(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	now := time.Now().UTC()
	rec := api.RunRecord{ID: newULID(), Kind: restic.KindBackup, Trigger: api.TriggerSchedule, StartedAt: now.Add(-time.Minute), Attempt: 1, AgentVersion: "t"}
	fc.mu.Lock()
	fc.rejectRuns[rec.ID] = api.ReasonConflict
	fc.mu.Unlock()
	if err := a.jr.StartRun(ctx, rec); err != nil {
		t.Fatal(err)
	}
	fin := now
	rec.FinishedAt, rec.Outcome = &fin, restic.OutcomeFailed
	rec.ErrorClass = restic.ClassUnknown
	if err := a.jr.FinishRun(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if err := a.deliverOutbox(ctx); err != nil {
		t.Fatal(err)
	}
	row, _, _ := a.jr.Run(ctx, rec.ID)
	if row.RejectReason != api.ReasonConflict || row.DeliveredAt != nil {
		t.Fatalf("row = %+v", row)
	}
	posts := fc.postCount()
	if err := a.deliverOutbox(ctx); err != nil {
		t.Fatal(err)
	}
	if fc.postCount() != posts {
		t.Error("a rejected run was sent again")
	}
}

// ---- commands -------------------------------------------------------------

func TestCommandsEndToEnd(t *testing.T) {
	fc := newFakeConsole(t)
	dir := enroll(t, fc)
	fakerestic.Use(t, "backup_ok")
	start(t, fakeOptions(t, dir))
	waitFor(t, "the first scheduled run", 30*time.Second, func() bool { return len(fc.closedRuns()) == 1 })

	// backup_now: a manual run that bypasses the schedule.
	bn := command(1, api.CommandBackupNow, "")
	fc.enqueue(bn)
	waitFor(t, "the manual run", 30*time.Second, func() bool { return len(fc.closedRuns()) == 2 })
	if got := fc.closedRuns()[1]; got.Trigger != api.TriggerManual || got.Outcome != restic.OutcomeSuccess || got.Initial {
		t.Errorf("manual run = %+v", got)
	}
	waitFor(t, "the backup_now result", 10*time.Second, func() bool { return len(fc.commandResults(bn.ID)) >= 1 })
	if res := fc.commandResults(bn.ID)[0]; res.Status != api.CommandSucceeded || string(res.Result) != `{"started":true}` {
		t.Errorf("backup_now result = %+v", res)
	}

	// Not implemented in M0: failed with the fixed reason, never dropped.
	pause := command(2, api.CommandPause, "")
	fc.enqueue(pause)
	waitFor(t, "the pause result", 10*time.Second, func() bool { return len(fc.commandResults(pause.ID)) >= 1 })
	if res := fc.commandResults(pause.ID)[0]; res.Status != api.CommandFailed || res.ErrorClass != restic.ClassUnknown || !strings.Contains(string(res.Result), "not_implemented_m0") {
		t.Errorf("pause result = %+v", res)
	}

	// The console delivers the same command id again: the result is posted again, nothing runs twice.
	fc.enqueue(bn)
	waitFor(t, "the result of the repeated command", 10*time.Second, func() bool { return len(fc.commandResults(bn.ID)) >= 2 })
	time.Sleep(500 * time.Millisecond)
	if n := len(fc.closedRuns()); n != 2 {
		t.Errorf("%d runs after a repeated backup_now id, want 2", n)
	}
	if rs := fc.commandResults(bn.ID); string(rs[0].Result) != string(rs[1].Result) || rs[0].Status != rs[1].Status {
		t.Errorf("the repeated result differs: %+v", rs)
	}

	// refresh_policy pulls a new policy; check_in succeeds.
	v2 := api.PolicyResponse{Policy: testPolicy(), PolicyID: testPolicyID, Version: 2, ETag: `"policy-v2"`}
	v2.Policy.Retention.KeepWithin = "30d"
	fc.setPolicy(v2)
	rp, ci := command(3, api.CommandRefreshPolicy, ""), command(4, api.CommandCheckIn, "")
	fc.enqueue(rp)
	fc.enqueue(ci)
	waitFor(t, "refresh_policy and check_in results", 10*time.Second, func() bool {
		return len(fc.commandResults(rp.ID)) >= 1 && len(fc.commandResults(ci.ID)) >= 1
	})
	for _, id := range []string{rp.ID, ci.ID} {
		if res := fc.commandResults(id)[0]; res.Status != api.CommandSucceeded {
			t.Errorf("%s result = %+v", id, res)
		}
	}
	waitFor(t, "the new policy to be applied", 10*time.Second, func() bool {
		hbs := fc.heartbeatList()
		return hbs[len(hbs)-1].PolicyVersionApplied == 2
	})
}

func TestCancelCommandCancelsTheRun(t *testing.T) {
	fc := newFakeConsole(t)
	dir := enroll(t, fc)
	fakerestic.Use(t, "hang_graceful")
	start(t, fakeOptions(t, dir))

	waitFor(t, "the open run at the console", 30*time.Second, func() bool {
		fc.mu.Lock()
		defer fc.mu.Unlock()
		return len(fc.runOrder) == 1
	})
	cancel := command(1, api.CommandCancel, "")
	fc.enqueue(cancel)
	waitFor(t, "the cancelled run", 30*time.Second, func() bool { return len(fc.closedRuns()) == 1 })
	run := fc.closedRuns()[0]
	if run.Outcome != restic.OutcomeCancelled || run.ExitCode == nil || *run.ExitCode != 130 {
		t.Fatalf("run = %+v", run)
	}
	waitFor(t, "the cancel result", 10*time.Second, func() bool { return len(fc.commandResults(cancel.ID)) >= 1 })
	if res := fc.commandResults(cancel.ID)[0]; res.Status != api.CommandSucceeded || string(res.Result) != `{"cancelled":true}` {
		t.Errorf("cancel result = %+v", res)
	}
	time.Sleep(600 * time.Millisecond)
	if n := len(fc.closedRuns()); n != 1 {
		t.Errorf("%d runs; the cancelled run's retry waits out its backoff", n)
	}
}

func TestBackupNowWhileRunningIsSkipped(t *testing.T) {
	fc := newFakeConsole(t)
	dir := enroll(t, fc)
	fakerestic.Use(t, "hang_graceful")
	r := start(t, fakeOptions(t, dir))
	waitFor(t, "the run to be active", 30*time.Second, func() bool {
		r.a.mu.Lock()
		defer r.a.mu.Unlock()
		return r.a.running != nil
	})
	bn := command(1, api.CommandBackupNow, "")
	fc.enqueue(bn)
	waitFor(t, "the skipped run", 30*time.Second, func() bool {
		for _, run := range fc.closedRuns() {
			if run.Outcome == restic.OutcomeSkipped {
				return true
			}
		}
		return false
	})
	waitFor(t, "the result", 10*time.Second, func() bool { return len(fc.commandResults(bn.ID)) >= 1 })
	if res := fc.commandResults(bn.ID)[0]; res.Status != api.CommandSucceeded || !strings.Contains(string(res.Result), `"started":false`) {
		t.Errorf("result = %+v", res)
	}
}

// ---- stop -----------------------------------------------------------------

func TestServiceStopInterruptsARunAndDeliversIt(t *testing.T) {
	ctx := context.Background()
	fc := newFakeConsole(t)
	dir := enroll(t, fc)
	fakerestic.Use(t, "hang_ignore")
	r := start(t, fakeOptions(t, dir))
	waitFor(t, "the open run at the console", 30*time.Second, func() bool {
		fc.mu.Lock()
		defer fc.mu.Unlock()
		return len(fc.runOrder) == 1
	})

	r.stop() // the service stops: restic is cancelled, the run is closed as interrupted, one last upload is tried

	closed := fc.closedRuns()
	if len(closed) != 1 || closed[0].Outcome != restic.OutcomeInterrupted {
		t.Fatalf("the console's runs after a stop: %+v", closed)
	}
	jr := openJournalRO(t, dir)
	if _, ok, _ := jr.KVGet(ctx, journal.KVStopMarker); !ok {
		t.Error("no stop marker was left for the next start")
	}
	if n, _ := jr.OpenRunCount(ctx); n != 0 {
		t.Errorf("%d runs still open after a stop", n)
	}

	// The next start finds nothing stale and consumes the marker.
	a, err := New(ctx, fakeOptions(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if _, ok, _ := a.jr.KVGet(ctx, journal.KVStopMarker); ok {
		t.Error("the stop marker survived the next start")
	}
}

// ---- wire mapping ---------------------------------------------------------

func TestFillRecordMapsOutcomesToTheWireRecord(t *testing.T) {
	snap := strings.Repeat("ab", 32)
	base := func() api.RunRecord {
		fin := time.Now().UTC()
		return api.RunRecord{ID: newULID(), Kind: restic.KindBackup, Trigger: api.TriggerSchedule,
			StartedAt: fin.Add(-time.Minute), FinishedAt: &fin, Attempt: 2, PolicyVersion: 3, AgentVersion: "t"}
	}
	counts := make(restic.ErrorCounts)
	counts.Add(restic.ErrorEvent{During: "archival", Class: restic.ClassPermission, Ext: "docx"})
	counts.Add(restic.ErrorEvent{During: "archival", Class: restic.ClassPermission, Ext: "docx"})

	tests := []struct {
		name    string
		res     supervisor.Result
		outcome restic.Outcome
		check   func(*testing.T, api.RunRecord)
	}{
		{"success", supervisor.Result{ExitCode: 0, Restic: restic.Result{Outcome: restic.OutcomeSuccess},
			Summary: &restic.BackupSummary{SnapshotID: snap, FilesNew: 4, FilesUnmodified: 1, TotalFilesProcessed: 5, TotalBytesProcessed: 100, DataAdded: 10, TotalDuration: 1.5},
			Elapsed: 2 * time.Second}, restic.OutcomeSuccess, func(t *testing.T, r api.RunRecord) {
			if r.SnapshotID != snap || r.Stats.FilesNew != 4 || r.Stats.TotalDurationS != 1.5 || r.Stats.DataAdded != 10 || len(r.ErrorCounts) != 0 {
				t.Errorf("%+v", r)
			}
		}},
		{"partial counts failed files and keeps the class", supervisor.Result{ExitCode: 3, Restic: restic.Result{Outcome: restic.OutcomePartial},
			Summary: &restic.BackupSummary{SnapshotID: snap, TotalFilesProcessed: 9}, Counts: counts, FailedFiles: 2}, restic.OutcomePartial, func(t *testing.T, r api.RunRecord) {
			if r.SnapshotID != snap || r.Stats.FailedFiles != 2 || r.ErrorClass != restic.ClassPermission {
				t.Errorf("%+v", r)
			}
			if len(r.ErrorCounts) != 1 || r.ErrorCounts[0] != (restic.ErrorProjection{During: "archival", Class: restic.ClassPermission, Ext: "docx", Count: 2}) {
				t.Errorf("error counts = %+v", r.ErrorCounts)
			}
		}},
		{"deferred on a lock", supervisor.Result{ExitCode: 11, Restic: restic.Result{Outcome: restic.OutcomeDeferred, DeferredReason: restic.DeferredLock, ErrorClass: restic.ClassLock}},
			restic.OutcomeDeferred, func(t *testing.T, r api.RunRecord) {
				if r.DeferredReason != restic.DeferredLock || r.ErrorClass != restic.ClassLock || r.SnapshotID != "" {
					t.Errorf("%+v", r)
				}
			}},
		{"failed, repository missing", supervisor.Result{ExitCode: 10, Restic: restic.Result{Outcome: restic.OutcomeFailed, ErrorClass: restic.ClassRepoMissing}},
			restic.OutcomeFailed, func(t *testing.T, r api.RunRecord) {
				if r.ErrorClass != restic.ClassRepoMissing || *r.ExitCode != 10 {
					t.Errorf("%+v", r)
				}
			}},
		{"cancelled at max runtime", supervisor.Result{ExitCode: 130, Restic: restic.Result{Outcome: restic.OutcomeCancelled}, LongRun: true},
			restic.OutcomeCancelled, func(t *testing.T, r api.RunRecord) {
				if !r.Stats.LongRun || r.SnapshotID != "" {
					t.Errorf("%+v", r)
				}
			}},
		{"a short snapshot id is not sent", supervisor.Result{ExitCode: 0, Restic: restic.Result{Outcome: restic.OutcomeSuccess},
			Summary: &restic.BackupSummary{SnapshotID: "abcd1234"}}, restic.OutcomeSuccess, func(t *testing.T, r api.RunRecord) {
			if r.SnapshotID != "" {
				t.Errorf("snapshot id %q", r.SnapshotID)
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := base()
			fillRecord(&rec, tc.res)
			if rec.Outcome != tc.outcome {
				t.Fatalf("outcome = %s", rec.Outcome)
			}
			if err := rec.Validate(); err != nil {
				t.Fatalf("the record does not validate: %v", err)
			}
			if err := api.AssertPathFree(rec); err != nil {
				t.Fatalf("not path-free: %v", err)
			}
			tc.check(t, rec)
		})
	}
}

func TestAssembleBuildsTheCommandFromThePolicy(t *testing.T) {
	ctx := context.Background()
	fc := newFakeConsole(t)
	dir := enroll(t, fc)
	fakerestic.Use(t, "backup_ok")
	a, err := New(ctx, fakeOptions(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	pol := api.PolicyResponse{Policy: testPolicy(), PolicyID: testPolicyID, Version: 7, ETag: `"p7"`}
	pol.Policy.Schedule.LockWaitS = 90
	pol.Policy.Selection.ExcludeLargerThan = "2g"
	pol.Policy.Throttle = policy.Throttle{Upload: 512, Download: 1024}
	runID := newULID()
	cmd, spec, err := a.assemble(ctx, pol, runID, true)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(cmd.Args, " ")
	for _, want := range []string{
		"backup --json --host " + testDeviceID,
		// The real ids: the customer from enrollment, the policy from the policy response.
		"--tag cb.customer=" + testCustomerID, "--tag cb.policy=" + testPolicyID + ".7", "--tag cb.run=" + runID,
		"--iexclude-file", "--exclude-larger-than 2147483648",
		"--limit-upload 512", "--limit-download 1024", "--retry-lock 90s", `-- C:\fixture\tree`,
	} {
		if !strings.Contains(args, want) {
			t.Errorf("argv lacks %q:\n%s", want, args)
		}
	}
	for _, placeholder := range []string{"unassigned", "effective."} {
		if strings.Contains(args, placeholder) {
			t.Errorf("argv carries the placeholder %q:\n%s", placeholder, args)
		}
	}
	// Windows-only flags (VSS and cloud placeholders): present on Windows, absent elsewhere.
	if !pol.Policy.Selection.VSS.Enabled || pol.Policy.Selection.VSS.TimeoutS == 0 || !pol.Policy.Selection.ExcludeCloudFiles {
		t.Fatal("the test policy must enable VSS with a timeout and exclude cloud files")
	}
	onWindows := runtime.GOOS == "windows"
	for _, row := range []struct{ flag string }{
		{"--use-fs-snapshot"},
		{"-o vss.timeout=" + strconv.Itoa(pol.Policy.Selection.VSS.TimeoutS) + "s"},
		{"--exclude-cloud-files"},
	} {
		if got := strings.Contains(args, row.flag); got != onWindows {
			t.Errorf("%s: present = %v on %s, want %v:\n%s", row.flag, got, runtime.GOOS, onWindows, args)
		}
	}
	for _, secret := range []string{testRepoSecret, testGatewaySec} {
		if strings.Contains(args, secret) {
			t.Errorf("a secret is in argv: %s", secret)
		}
	}
	if !strings.HasPrefix(spec.Env.Repository, "rest:http") && !strings.HasPrefix(spec.Env.Repository, "rest:https://"+testDeviceID+":") {
		t.Errorf("repository = %q", spec.Env.Repository)
	}
	if spec.Env.Password != testRepoSecret || spec.Env.CacheDir == "" || spec.Env.TempDir == "" {
		t.Errorf("env = %+v", spec.Env)
	}
	// The initial run gets the initial cap, a later run the normal one.
	if spec.MaxRuntime != time.Duration(pol.Policy.SLA.InitialMaxRunHours)*time.Hour {
		t.Errorf("initial max runtime = %v", spec.MaxRuntime)
	}
	_, spec2, _ := a.assemble(ctx, pol, runID, false)
	if spec2.MaxRuntime != time.Duration(pol.Policy.Schedule.MaxRuntimeHours)*time.Hour {
		t.Errorf("max runtime = %v", spec2.MaxRuntime)
	}
	// A selection that cannot be resolved refuses to start (guard_refused), it does not guess.
	bad := pol
	bad.Policy.Selection.Roots = []string{}
	bad.Policy.Selection.Presets = []string{policy.PresetAllUserProfiles}
	if _, _, err := a.assemble(ctx, bad, runID, false); err == nil {
		t.Error("a preset with no profile directories produced a command")
	} else {
		var ae *assembleError
		if !errors.As(err, &ae) || ae.class != restic.ClassGuardRefused {
			t.Errorf("err = %v", err)
		}
	}
	// Without the ids there is no attributable snapshot: refused, not tagged with a placeholder.
	noPolicyID := pol
	noPolicyID.PolicyID = ""
	for name, f := range map[string]func() error{
		"no policy id": func() error { _, _, err := a.assemble(ctx, noPolicyID, runID, false); return err },
		"no customer id": func() error {
			saved := a.cfg.CustomerID
			a.cfg.CustomerID = "" // an agent.json written before the console sent the id
			defer func() { a.cfg.CustomerID = saved }()
			_, _, err := a.assemble(ctx, pol, runID, false)
			return err
		},
	} {
		var ae *assembleError
		if err := f(); !errors.As(err, &ae) || ae.class != restic.ClassGuardRefused {
			t.Errorf("%s: %v, want a guard_refused refusal", name, err)
		}
	}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]int64{"": 0, "100": 100, "1k": 1 << 10, "2M": 2 << 20, "3g": 3 << 30, "1t": 1 << 40, "x": 0, "-5": 0} {
		if got := parseSize(in); got != want {
			t.Errorf("parseSize(%q) = %d, want %d", in, got, want)
		}
	}
}
