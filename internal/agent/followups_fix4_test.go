package agent

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/agent/internal/fakerestic"
	"github.com/c2tech-sys/cryobackup/internal/agent/scheduler"
	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// M1-FIX4 C: follow-ups from V-M1-FIX3 (items 5, 6 and 9).

// ---- item 5: after the resume wait, pause / hold / window win --------------

// A pause or a hold the operator set while the laptop slept is in the policy the
// console serves once it answers; the run the resume started must not start.
func TestResumeWaitRechecksPauseAndHold(t *testing.T) {
	for name, mutate := range map[string]func(*api.PolicyResponse){
		"paused": func(p *api.PolicyResponse) { p.Paused = true },
		"hold": func(p *api.PolicyResponse) {
			until := time.Now().Add(48 * time.Hour) // past the rig's 25-hour sleep
			p.HoldUntil = &until
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := resumeWaitRig(t)
			p.fc.mu.Lock()
			v2 := p.fc.policy
			p.fc.mu.Unlock()
			v2.Version, v2.ETag = 2, `"policy-v2"`
			mutate(&v2)
			p.fc.setPolicy(v2)
			p.fc.down.Store(false)
			waitFor(t, "the run to give up after the wait", 30*time.Second, func() bool {
				return strings.Contains(p.log.String(), "resume: the backup does not start after the wait")
			})
			if !strings.Contains(p.log.String(), "why="+name) {
				t.Errorf("the reason %q was not logged", name)
			}
			time.Sleep(500 * time.Millisecond)
			if n := p.starts(); n != 1 {
				t.Fatalf("%d runs reached restic, want only the boot run: the %s set during the sleep did not win", n, name)
			}
			if n := len(p.fc.closedRuns()); n != 1 {
				t.Fatalf("%d runs at the console, want only the boot run (a held-back start is not journaled)", n)
			}
			p.r.a.mu.Lock()
			active := p.r.a.running != nil
			p.r.a.mu.Unlock()
			if active {
				t.Fatal("the run is still marked active")
			}
			if name == "paused" {
				waitFor(t, "a heartbeat that says paused", 10*time.Second, func() bool {
					for _, hb := range p.fc.heartbeatList() {
						if hb.ServiceState == api.ServicePaused {
							return true
						}
					}
					return false
				})
			}
		})
	}
}

func TestResumeGate(t *testing.T) {
	// A Tuesday, 12:00 device-local time.
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.Local)
	past, future := now.Add(-time.Hour), now.Add(time.Hour)
	base := func() *api.PolicyResponse { return &api.PolicyResponse{Policy: testPolicy()} }
	cases := []struct {
		name string
		edit func(*api.PolicyResponse)
		want string
	}{
		{"nothing holds it", func(*api.PolicyResponse) {}, ""},
		{"paused", func(p *api.PolicyResponse) { p.Paused = true }, scheduler.ReasonPaused},
		{"paused until later", func(p *api.PolicyResponse) { p.Paused, p.PausedUntil = true, &future }, scheduler.ReasonPaused},
		{"a pause that has ended", func(p *api.PolicyResponse) { p.Paused, p.PausedUntil = true, &past }, ""},
		{"hold", func(p *api.PolicyResponse) { p.HoldUntil = &future }, scheduler.ReasonHold},
		{"a hold that has ended", func(p *api.PolicyResponse) { p.HoldUntil = &past }, ""},
		{"window closed", func(p *api.PolicyResponse) { p.Policy.Schedule.Window = "Mon/0-1" }, scheduler.ReasonWindow},
		{"window open", func(p *api.PolicyResponse) { p.Policy.Schedule.Window = "Tue/11-13" }, ""},
		{"window always", func(p *api.PolicyResponse) { p.Policy.Schedule.Window = "*/0-24" }, ""},
		{"window unparsable never blocks", func(p *api.PolicyResponse) { p.Policy.Schedule.Window = "Mon/8-99" }, ""},
	}
	for _, c := range cases {
		p := base()
		c.edit(p)
		if got := resumeGate(p, now); got != c.want {
			t.Errorf("%s: resumeGate = %q, want %q", c.name, got, c.want)
		}
	}
	if got := resumeGate(nil, now); got != "" {
		t.Errorf("no policy: %q", got)
	}
}

// ---- item 6: restic never ran, so the boot label goes back -----------------

// copyExe copies this test binary (the fake restic) to a file the test may
// change after the agent verified it.
func copyExe(t *testing.T) string {
	t.Helper()
	src, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	dst := filepath.Join(t.TempDir(), "restic-copy"+filepath.Ext(src))
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o700)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	return dst
}

// startAfter is start with a step between New (which verifies restic) and Run.
func startAfter(t *testing.T, o Options, between func()) *running {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	a, err := New(ctx, o)
	if err != nil {
		cancel()
		t.Fatalf("New: %v", err)
	}
	between()
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
		case <-r.done:
		case <-time.After(30 * time.Second):
			t.Error("the agent did not stop within 30 s")
		}
		a.Close()
	}
	t.Cleanup(r.stop)
	return r
}

func bootPending(a *Agent) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.bootPending
}

// The binary changed between the boot check and the run: the supervisor's
// guard refuses it, restic never runs, and the boot label is kept for the next
// start.
func TestGuardRefusalAtStartKeepsTheBootLabel(t *testing.T) {
	fc := newFakeConsole(t)
	fakerestic.Use(t, "backup_ok")
	o := fakeOptions(t, enroll(t, fc))
	exe := copyExe(t)
	o.ResticPath, o.ResticSHA256 = exe, fileSHA(t, exe)
	r := startAfter(t, o, func() {
		f, err := os.OpenFile(exe, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.Write([]byte("tampered"))
		_ = f.Close()
	})
	waitFor(t, "the refused run at the console", 30*time.Second, func() bool { return len(fc.closedRuns()) == 1 })
	run := fc.closedRuns()[0]
	if run.Outcome != restic.OutcomeFailed || run.ErrorClass != restic.ClassGuardRefused || run.Trigger != api.TriggerBoot {
		t.Fatalf("run = %+v, want failed / guard_refused, trigger boot", run)
	}
	waitFor(t, "the run to be over", 10*time.Second, func() bool {
		r.a.mu.Lock()
		defer r.a.mu.Unlock()
		return r.a.running == nil
	})
	if !bootPending(r.a) {
		t.Fatal("bootPending was dropped although restic never ran (guard refusal)")
	}
}

// A cancel that lands while the supervisor re-checks the binary (restic has not
// started): cancelled, and the boot label is kept.
func TestCancelBeforeResticStartedKeepsTheBootLabel(t *testing.T) {
	fc := newFakeConsole(t)
	fakerestic.Use(t, "version_slow") // "restic version" takes 3 s
	o := fakeOptions(t, enroll(t, fc))
	exe := copyExe(t)
	o.ResticPath, o.ResticSHA256 = exe, fileSHA(t, exe)
	log := &syncBuf{}
	o.Logger = slog.New(slog.NewTextHandler(log, &slog.HandlerOptions{Level: slog.LevelDebug}))
	r := startAfter(t, o, func() {
		// Same bytes, new time: the supervisor checks the binary again.
		later := time.Now().Add(time.Hour)
		if err := os.Chtimes(exe, later, later); err != nil {
			t.Fatal(err)
		}
	})
	waitFor(t, "the run to reach the supervisor", 30*time.Second, func() bool {
		return strings.Contains(log.String(), `msg="backup starting"`)
	})
	time.Sleep(300 * time.Millisecond) // inside the 3 s version query
	if ok, _ := r.a.cmdCancel(context.Background(), ""); !ok {
		t.Fatal("no active run to cancel")
	}
	waitFor(t, "the cancelled run at the console", 30*time.Second, func() bool { return len(fc.closedRuns()) == 1 })
	run := fc.closedRuns()[0]
	if run.Outcome != restic.OutcomeCancelled || run.Trigger != api.TriggerBoot || run.ExitCode != nil {
		t.Fatalf("run = %+v, want cancelled before restic started (no exit code), trigger boot", run)
	}
	if !strings.Contains(log.String(), "cancelled before restic was started") {
		t.Fatalf("the run did not end in the binary check; log:\n%s", log.String())
	}
	if !bootPending(r.a) {
		t.Fatal("bootPending was dropped although restic never ran (cancelled before start)")
	}
}

// ---- item 9: the wait asks for a heartbeat at once ---------------------------

// The first heartbeat of the wait is requested when the wait begins, not one
// probe period later.
func TestResumeWaitKicksTheHeartbeatAtOnce(t *testing.T) {
	a := &Agent{clock: time.Now, o: Options{resumeWait: time.Minute, resumeProbe: time.Hour},
		consoleReached: make(chan struct{}, 1), kickHeartbeat: make(chan struct{}, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() { done <- a.waitForConsole(ctx) }()
	select {
	case <-a.kickHeartbeat:
	case <-time.After(2 * time.Second):
		cancel()
		<-done
		t.Fatal("the wait did not ask for a heartbeat when it began")
	}
	cancel()
	if <-done {
		t.Fatal("the wait reported an answer it never had")
	}
}
