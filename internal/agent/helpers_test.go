package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/agent/internal/fakerestic"
	"github.com/c2tech-sys/cryobackup/internal/policy"
)

func TestMain(m *testing.M) {
	fakerestic.MaybeRun() // the test binary doubles as the fake restic
	// The untrusted verdict is machine state (HKLM on Windows): this package's
	// tests keep it in memory, per state root, so they neither depend on nor
	// change the machine's (see state_fix10_test.go).
	useMemVerdicts()
	os.Exit(m.Run())
}

// plainSecrets is true where there is no DPAPI (Linux CI).
func plainSecrets() bool { return runtime.GOOS != "windows" }

func fileSHA(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// enroll enrolls a fresh state directory with the fake console.
func enroll(t *testing.T, fc *fakeConsole) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "state")
	res, err := Enroll(context.Background(), EnrollOptions{
		StateDir: dir, ServerURL: fc.URL(), Token: testEnrollToken, InsecureLab: true, AllowUnprotectedState: true,
		InsecurePlaintextSecrets: plainSecrets(),
	})
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if res.DeviceID != testDeviceID {
		t.Fatalf("device id = %q", res.DeviceID)
	}
	return dir
}

// fakeOptions runs this test binary as restic and tunes every loop to test speed.
func fakeOptions(t *testing.T, stateDir string) Options {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return Options{
		StateDir: stateDir, ResticPath: exe, ResticSHA256: fileSHA(t, exe),
		InsecurePlaintextSecrets: plainSecrets(), AllowUnprotectedState: true,
		environment:    func() policy.Environment { return policy.Environment{} },
		heartbeatEvery: 80 * time.Millisecond, heartbeatMinGap: 10 * time.Millisecond,
		pollWait: 200 * time.Millisecond, schedTick: 100 * time.Millisecond, progressEvery: 50 * time.Millisecond,
		cancelGrace: 500 * time.Millisecond, backoffMin: 50 * time.Millisecond, refusedProbe: 300 * time.Millisecond,
	}
}

// running is a started agent.
type running struct {
	a    *Agent
	stop func()
	done chan error
}

// start boots an agent and runs it until stop (or the end of the test).
func start(t *testing.T, o Options) *running {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	a, err := New(ctx, o)
	if err != nil {
		cancel()
		t.Fatalf("New: %v", err)
	}
	r := &running{a: a, done: make(chan error, 1)}
	go func() { r.done <- a.Run(ctx) }()
	stopped := false
	r.stop = func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case err := <-r.done:
			if err != nil {
				t.Errorf("Run returned %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Error("the agent did not stop within 30 s")
		}
		a.Close()
	}
	t.Cleanup(r.stop)
	return r
}

func waitFor(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out after %v waiting for %s", d, what)
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func contains(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
