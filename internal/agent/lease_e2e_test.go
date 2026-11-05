package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/agent/internal/fakerestic"
	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/console/agentapi"
	consoleenroll "github.com/c2tech-sys/cryobackup/internal/console/enroll"
	"github.com/c2tech-sys/cryobackup/internal/console/escrow"
	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

type muxRegistrar struct{ m *http.ServeMux }

func (r muxRegistrar) Handle(p string, h http.Handler) { r.m.Handle(p, h) }

// The fleet cap is a safety property, so these tests measure it from the backup
// processes themselves: each one stamps when it really started and exited. The
// runner's StartedAt cannot show this, because it is stamped before the lease is
// requested (runner.go), so a run that waited its turn can look like an overlap.

// capStampDirEnv, when set in a backup process, makes it stamp its start and exit
// in that directory and wait there for the file capGate before it does anything.
const (
	capStampDirEnv = "CRYO_TEST_CAP_STAMPS"
	capGate        = "gate"
)

// The test binary is also the fake restic. init runs before TestMain's
// fakerestic.MaybeRun, so a backup process stops here first. The fake itself runs
// as a child, because it ends in os.Exit and could not stamp its own exit. The
// window measured here therefore contains the whole of the fake backup.
func init() {
	dir := os.Getenv(capStampDirEnv)
	if dir == "" || len(os.Args) < 2 || os.Args[1] != "backup" {
		return
	}
	stamp := func(kind string) {
		name := kind + "-" + strconv.Itoa(os.Getpid())
		tmp := filepath.Join(dir, "tmp-"+name)
		if os.WriteFile(tmp, []byte(strconv.FormatInt(time.Now().UnixNano(), 10)), 0o600) != nil {
			os.Exit(2)
		}
		// A rename, so that a reader never sees a stamp without its time.
		for deadline := time.Now().Add(30 * time.Second); os.Rename(tmp, filepath.Join(dir, name)) != nil; time.Sleep(5 * time.Millisecond) {
			if time.Now().After(deadline) { // see backupWindows: a sharing violation is brief
				os.Exit(2)
			}
		}
	}
	stamp("start")
	for deadline := time.Now().Add(5 * time.Minute); ; time.Sleep(5 * time.Millisecond) {
		if _, err := os.Stat(filepath.Join(dir, capGate)); err == nil {
			break
		}
		if time.Now().After(deadline) {
			os.Exit(2) // nothing opened the gate: never hang a test run for ever
		}
	}
	exe, err := os.Executable()
	if err != nil {
		os.Exit(2)
	}
	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, capStampDirEnv+"=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	code := 0
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if code = 2; errors.As(err, &ee) {
			code = ee.ExitCode()
		}
	}
	stamp("end")
	os.Exit(code)
}

// backupWindow is one backup process as it measured itself. A zero end means it
// has not exited.
type backupWindow struct {
	pid        string
	start, end time.Time
}

func (w backupWindow) String() string {
	if w.end.IsZero() {
		return fmt.Sprintf("pid %s from %s, still running", w.pid, w.start.Format("15:04:05.000000"))
	}
	return fmt.Sprintf("pid %s %s to %s", w.pid, w.start.Format("15:04:05.000000"), w.end.Format("15:04:05.000000"))
}

// backupWindows reads the stamps, in order of start.
func backupWindows(t *testing.T, dir string) []backupWindow {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	read := func(name string) time.Time {
		// On Windows a file just renamed can refuse an open for a moment ("being used
		// by another process"), so a read is retried. A stamp that stays unreadable fails.
		b, err := os.ReadFile(filepath.Join(dir, name))
		for deadline := time.Now().Add(30 * time.Second); err != nil && time.Now().Before(deadline); {
			time.Sleep(5 * time.Millisecond)
			b, err = os.ReadFile(filepath.Join(dir, name))
		}
		if err != nil {
			t.Fatal(err)
		}
		ns, err := strconv.ParseInt(string(b), 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		return time.Unix(0, ns)
	}
	var out []backupWindow
	for _, e := range entries {
		if pid, ok := strings.CutPrefix(e.Name(), "start-"); ok {
			w := backupWindow{pid: pid, start: read(e.Name())}
			if _, err := os.Stat(filepath.Join(dir, "end-"+pid)); err == nil {
				w.end = read("end-" + pid)
			}
			out = append(out, w)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].start.Before(out[j].start) })
	return out
}

// capOverlap describes the first two windows that overlap, "" when none do.
func capOverlap(windows []backupWindow) string {
	for i := 1; i < len(windows); i++ {
		if prev := windows[i-1]; prev.end.IsZero() || !prev.end.Before(windows[i].start) {
			return prev.String() + " overlaps " + windows[i].String()
		}
	}
	return ""
}

// capRig is the real console agent API (store, agentapi) with a cap of 1 on the
// storage target and two enrolled devices, whose backup processes stamp themselves.
type capRig struct {
	t         *testing.T
	st        *store.Store
	target    store.StorageTarget
	dirs      [2]string
	stamps    string
	openOnce  sync.Once
	notAnswer atomic.Int64 // start-lease calls answered other than 200: the agent then fails open (ADR 0005)
}

func newCapRig(t *testing.T) *capRig {
	t.Helper()
	ctx := context.Background()
	st, err := store.OpenMemory(ctx, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	target, err := st.CreateStorageTarget(ctx, store.StorageTarget{Name: "Lab storage", Provider: "s3_compatible", Endpoint: "s3.example.invalid",
		Bucket: "bkt", Prefix: "fleet", GatewayURL: "https://gateway.example.invalid:9443", CredentialRef: "CRYO_TEST_S3", IsDefault: true,
		MaxConcurrentBackups: 1})
	if err != nil {
		t.Fatal(err)
	}
	sections, err := store.PolicySections(testPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreatePolicy(ctx, store.ScopeAll(), store.PolicyInput{Scope: "default", Name: "Default", Sections: sections}); err != nil {
		t.Fatal(err)
	}
	cust, err := st.CreateCustomer(ctx, store.ScopeAll(), store.Customer{Name: "Example Clinic", KeyCustody: "escrowed", Expected: true})
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := consoleenroll.Mint(ctx, st, store.ScopeAll(), cust.ID, consoleenroll.MintOptions{Expires: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	mk, err := escrow.NewMasterKey(bytes.Repeat([]byte{7}, escrow.MasterKeyLen))
	if err != nil {
		t.Fatal(err)
	}
	svc := &agentapi.Service{Store: st, Keys: &escrow.Keys{Master: mk, Store: st}, Backend: &fixtures.Backend{},
		GatewayURL: "https://gateway.example.invalid:9443", Clock: time.Now}
	mux := http.NewServeMux()
	svc.Register(muxRegistrar{mux})
	r := &capRig{t: t, st: st, target: target, stamps: t.TempDir()}
	console := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != api.RouteStartLease {
			mux.ServeHTTP(w, req)
			return
		}
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		mux.ServeHTTP(rec, req)
		if rec.status != http.StatusOK {
			r.notAnswer.Add(1)
		}
	}))
	t.Cleanup(console.Close)

	t.Setenv(capStampDirEnv, r.stamps)
	fakerestic.Use(t, "backup_ok")
	for i := range r.dirs {
		r.dirs[i] = filepath.Join(t.TempDir(), "state")
		if _, err := Enroll(ctx, EnrollOptions{StateDir: r.dirs[i], ServerURL: console.URL, Token: token, InsecureLab: true,
			AllowUnprotectedState: true, InsecurePlaintextSecrets: plainSecrets()}); err != nil {
			t.Fatalf("enroll %d: %v", i, err)
		}
	}
	return r
}

// statusRecorder remembers the status a handler answered.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// startAgent starts device i's agent. A refused start retries after 3 s: the
// console allows a device 6 asks at once and one more per 10 s, and past that it
// answers 429, which fails open by design (ADR 0005) and is not what is tested here.
func (r *capRig) startAgent(i int) {
	r.t.Helper()
	o := fakeOptions(r.t, r.dirs[i])
	o.leaseWait = func(time.Duration) time.Duration { return 3 * time.Second }
	start(r.t, o)
	r.t.Cleanup(r.openGate) // before the agent stops (cleanups are LIFO)
}

// openGate lets every backup process, present and future, get on with its backup.
func (r *capRig) openGate() {
	r.openOnce.Do(func() {
		if err := os.WriteFile(filepath.Join(r.stamps, capGate), nil, 0o600); err != nil {
			r.t.Error(err)
		}
	})
}

// capDevice is what the console holds about one device's runs.
type capDevice struct {
	success  store.Run
	deferred int
}

func (r *capRig) devices() map[string]*capDevice {
	r.t.Helper()
	l, err := r.st.ListRuns(context.Background(), store.ScopeAll(), "", store.Page{Limit: 200})
	if err != nil {
		r.t.Fatal(err)
	}
	out := map[string]*capDevice{}
	for _, run := range l.Items {
		d := out[run.DeviceID]
		if d == nil {
			d = &capDevice{}
			out[run.DeviceID] = d
		}
		switch {
		case run.Outcome == "success":
			d.success = run
		case run.Outcome == "deferred" && run.DeferredReason == "fleet_cap":
			d.deferred++
		}
	}
	return out
}

// failIfOverlap stops the test on a measured overlap. Fail-open calls are named,
// because they are the one way an overlap is not a broken cap.
func (r *capRig) failIfOverlap(when string) {
	r.t.Helper()
	if o := capOverlap(backupWindows(r.t, r.stamps)); o != "" {
		r.t.Fatalf("%s: THE CAP OF 1 IS BROKEN: %s (start-lease calls answered other than 200: %d)", when, o, r.notAnswer.Load())
	}
}

// waitBothSucceeded waits for a successful backup on both devices, checking the
// measured windows all the way, and returns the console's picture.
func (r *capRig) waitBothSucceeded() map[string]*capDevice {
	r.t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for {
		r.failIfOverlap("waiting for both backups")
		got, n := r.devices(), 0
		for _, d := range got {
			if d.success.ID != "" {
				n++
			}
		}
		if n == 2 {
			return got
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("no successful backup on both devices after 3 min; windows %v, start-lease calls answered other than 200: %d",
				backupWindows(r.t, r.stamps), r.notAnswer.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// checkFinished is what holds once both devices have succeeded, however they got there.
func (r *capRig) checkFinished(got map[string]*capDevice) {
	r.t.Helper()
	r.failIfOverlap("after both backups")
	windows := backupWindows(r.t, r.stamps)
	if len(windows) < 2 {
		r.t.Fatalf("%d backup processes measured for two successful backups: %v", len(windows), windows)
	}
	if n := r.notAnswer.Load(); n != 0 {
		r.t.Fatalf("%d start-lease calls answered other than 200, so the agent may have failed open", n)
	}
	for id, d := range got {
		var stats struct {
			Lease string `json:"lease"`
		}
		if err := json.Unmarshal(d.success.Stats, &stats); err != nil || stats.Lease != string(api.LeaseGranted) {
			r.t.Errorf("device %s ran with lease %q (%v), want %q", id, stats.Lease, err, api.LeaseGranted)
		}
		if d.success.Attempt != 1 {
			r.t.Errorf("device %s ran attempt %d: the deferral moved the retry ladder", id, d.success.Attempt)
		}
	}
	// Both closed uploads released their leases.
	if leases, err := r.st.ListStartLeases(context.Background(), r.target.ID, time.Now()); err != nil || len(leases) != 0 {
		r.t.Fatalf("leases left after both runs: %+v %v", leases, err)
	}
}

// TestFleetCapEndToEnd drives the refusal rather than hoping for it (ADR 0005): two
// real agents, a cap of 1. The first device holds the lease and its backup process
// is kept running by a gate; only then does the second agent start, and it must be
// refused, journal deferred/fleet_cap and start no process. The gate opens on that
// evidence, and the second device runs once the first's closed upload has released
// the lease. The processes' own stamps never overlap.
func TestFleetCapEndToEnd(t *testing.T) {
	r := newCapRig(t)
	r.startAgent(0)
	waitFor(t, "the first backup process, holding the lease", 2*time.Minute, func() bool { return len(backupWindows(t, r.stamps)) >= 1 })
	leases, err := r.st.ListStartLeases(context.Background(), r.target.ID, time.Now())
	if err != nil || len(leases) != 1 {
		t.Fatalf("a backup process runs and the leases are %+v %v, want the one", leases, err)
	}
	first := leases[0].DeviceID

	r.startAgent(1)
	second := ""
	deadline := time.Now().Add(2 * time.Minute)
	for second == "" {
		r.failIfOverlap("while the first backup runs")
		for id, d := range r.devices() {
			if id != first && d.deferred > 0 {
				second = id
			}
		}
		if second == "" {
			if time.Now().After(deadline) {
				t.Fatalf("the second device was not refused within 2 min; start-lease calls answered other than 200: %d", r.notAnswer.Load())
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	// Refused, and the first backup has not moved: one process, still inside the gate.
	windows := backupWindows(t, r.stamps)
	if len(windows) != 1 || !windows[0].end.IsZero() {
		t.Fatalf("after the refusal the processes are %v, want only the first, still running", windows)
	}

	r.openGate()
	got := r.waitBothSucceeded()
	r.checkFinished(got)
	if got[second] == nil || got[second].deferred == 0 || got[first].deferred != 0 {
		t.Fatalf("deferrals: the first device %+v, the second %+v; want only the second deferred", got[first], got[second])
	}
	windows = backupWindows(t, r.stamps)
	t.Logf("measured: first exited %v before the second started", windows[1].start.Sub(windows[0].end).Round(time.Millisecond))
}

// TestFleetCapBothDueAtOnce is the race the first test takes out of the picture:
// both agents start together with no gate, so who wins and whether anyone is refused
// is up to the machine. Nothing here depends on that: the measured windows never
// overlap either way. The runner's StartedAt is logged beside them, which is what
// an overlap in StartedAt alone turns out to be.
func TestFleetCapBothDueAtOnce(t *testing.T) {
	r := newCapRig(t)
	r.openGate()
	r.startAgent(0)
	r.startAgent(1)
	got := r.waitBothSucceeded()
	r.checkFinished(got)
	windows := backupWindows(t, r.stamps)
	var runs []store.Run
	for _, d := range got {
		runs = append(runs, d.success)
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].StartedAt.Before(runs[j].StartedAt) })
	t.Logf("measured: first exited %v before the second started; StartedAt of the second is %v after the first run's FinishedAt",
		windows[1].start.Sub(windows[0].end).Round(time.Millisecond), runs[1].StartedAt.Sub(*runs[0].FinishedAt).Round(time.Millisecond))
}
