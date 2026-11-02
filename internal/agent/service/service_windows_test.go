//go:build windows

package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
)

var errStop = errors.New("test: service stop")

// drive runs Execute on its own goroutine with channels standing in for the SCM.
func drive(t *testing.T, h Host) (reqs chan svc.ChangeRequest, statuses chan svc.Status, done chan [2]uint32) {
	t.Helper()
	reqs = make(chan svc.ChangeRequest, 8)
	statuses = make(chan svc.Status, 64)
	done = make(chan [2]uint32, 1)
	go func() {
		specific, code := (&handler{host: h}).Execute(nil, reqs, statuses)
		v := uint32(0)
		if specific {
			v = 1
		}
		done <- [2]uint32{v, code}
	}()
	return
}

func next(t *testing.T, statuses chan svc.Status) svc.Status {
	t.Helper()
	select {
	case s := <-statuses:
		return s
	case <-time.After(5 * time.Second):
		t.Fatal("no status reported")
		return svc.Status{}
	}
}

func TestExecuteStartsAndStops(t *testing.T) {
	for _, tc := range []struct {
		cmd  svc.Cmd
		kind string
	}{
		{svc.Stop, "stop"},
		{svc.Shutdown, "shutdown"},
		{svc.PreShutdown, "preshutdown"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			var (
				mu    sync.Mutex
				cause error
				kind  string
			)
			ran := make(chan struct{})
			reqs, statuses, done := drive(t, Host{
				StopCause: errStop,
				Run: func(ctx context.Context) error {
					close(ran)
					<-ctx.Done()
					mu.Lock()
					cause = context.Cause(ctx)
					mu.Unlock()
					time.Sleep(100 * time.Millisecond) // the supervisor cancelling restic
					return nil
				},
				OnStop: func(k string) { mu.Lock(); kind = k; mu.Unlock() },
			})

			if s := next(t, statuses); s.State != svc.StartPending {
				t.Fatalf("first status = %+v, want StartPending", s)
			}
			running := next(t, statuses)
			const want = svc.AcceptStop | svc.AcceptShutdown | svc.AcceptPreShutdown | svc.AcceptPowerEvent | svc.AcceptSessionChange
			if running.State != svc.Running || running.Accepts&want != want {
				t.Fatalf("running status = %+v, must accept stop, shutdown, pre-shutdown, power and session events", running)
			}
			<-ran

			reqs <- svc.ChangeRequest{Cmd: tc.cmd}
			stopping := next(t, statuses)
			if stopping.State != svc.StopPending || stopping.WaitHint == 0 || stopping.CheckPoint == 0 {
				t.Fatalf("stop-pending status = %+v, want a wait hint and a checkpoint", stopping)
			}
			select {
			case r := <-done:
				if r != [2]uint32{0, 0} {
					t.Errorf("Execute returned %v", r)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("Execute did not return after the agent shut down")
			}
			mu.Lock()
			defer mu.Unlock()
			if !errors.Is(cause, errStop) {
				t.Errorf("the agent's context was cancelled with %v, want the service-stop cause", cause)
			}
			if kind != tc.kind {
				t.Errorf("OnStop kind = %q, want %q", kind, tc.kind)
			}
		})
	}
}

func TestExecuteInterrogateAndIgnoredEvents(t *testing.T) {
	var mu sync.Mutex
	var events []Event
	reqs, statuses, done := drive(t, Host{
		StopCause: errStop,
		Run:       func(ctx context.Context) error { <-ctx.Done(); return nil },
		OnEvent:   func(e Event) { mu.Lock(); events = append(events, e); mu.Unlock() },
	})
	next(t, statuses) // StartPending
	next(t, statuses) // Running

	cur := svc.Status{State: svc.Running, Accepts: svc.AcceptStop}
	reqs <- svc.ChangeRequest{Cmd: svc.Interrogate, CurrentStatus: cur}
	if s := next(t, statuses); s != cur {
		t.Errorf("interrogate answered %+v, want the current status", s)
	}
	reqs <- svc.ChangeRequest{Cmd: svc.PowerEvent, EventType: PBTAPMSuspend}
	reqs <- svc.ChangeRequest{Cmd: svc.PowerEvent, EventType: PBTAPMResumeAutomatic}
	reqs <- svc.ChangeRequest{Cmd: svc.SessionChange, EventType: WTSSessionLogon}
	reqs <- svc.ChangeRequest{Cmd: svc.Interrogate, CurrentStatus: cur}
	next(t, statuses) // the service is still answering: the events did not stop it

	mu.Lock()
	got := append([]Event(nil), events...)
	mu.Unlock()
	if len(got) != 3 || !got[0].Power || got[0].Code != PBTAPMSuspend || got[2].Power || got[2].Code != WTSSessionLogon {
		t.Errorf("events = %+v", got)
	}
	reqs <- svc.ChangeRequest{Cmd: svc.Stop}
	<-done
}

// r207: with OnEvent wired, a burst of power and session events does not hold
// up a Stop that follows (the control loop returns to the request channel as
// soon as OnEvent returns, which the contract requires it to do).
func TestStopAfterAnEventBurst(t *testing.T) {
	var n atomic.Int32
	reqs := make(chan svc.ChangeRequest, 1024)
	statuses := make(chan svc.Status, 64)
	done := make(chan [2]uint32, 1)
	h := &handler{host: Host{
		StopCause: errStop,
		Run:       func(ctx context.Context) error { <-ctx.Done(); return nil },
		OnEvent:   func(Event) { n.Add(1) },
	}}
	go func() {
		_, code := h.Execute(nil, reqs, statuses)
		done <- [2]uint32{0, code}
	}()
	next(t, statuses) // StartPending
	next(t, statuses) // Running
	for i := 0; i < 400; i++ {
		reqs <- svc.ChangeRequest{Cmd: svc.PowerEvent, EventType: PBTAPMResumeAutomatic}
		reqs <- svc.ChangeRequest{Cmd: svc.SessionChange, EventType: WTSSessionUnlock}
	}
	reqs <- svc.ChangeRequest{Cmd: svc.Stop}
	select {
	case r := <-done:
		if r != [2]uint32{0, 0} {
			t.Errorf("Execute returned %v", r)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Stop was held up behind the events")
	}
	if n.Load() != 800 {
		t.Errorf("OnEvent saw %d of 800 events before the stop", n.Load())
	}
}

func TestExecuteReportsAnAgentThatEndsByItself(t *testing.T) {
	_, statuses, done := drive(t, Host{
		StopCause: errStop,
		Run:       func(ctx context.Context) error { return errors.New("the journal is gone") },
	})
	next(t, statuses)
	select {
	case r := <-done:
		if r[1] != 1 {
			t.Errorf("exit code = %d, want 1 for a failed agent", r[1])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Execute did not notice that the agent ended")
	}
}

// FIX19 end to end on the real registry: registering the Event Log source twice
// succeeds. It needs an elevated process and a machine where the source is not
// already registered (it removes the source afterwards, so it never touches a
// real installation).
func TestEnsureEventSourceTwiceElevated(t *testing.T) {
	if !windows.GetCurrentProcessToken().IsElevated() {
		t.Skip("this process is not elevated: registering an Event Log source needs HKLM write access (the windows-latest CI runner is elevated)")
	}
	if ok, err := sourceRegistered(); err != nil || ok {
		t.Skipf("the %q source is already registered here (or unreadable: %v); not touching a real installation", EventSource, err)
	}
	t.Cleanup(func() { _ = eventlog.Remove(EventSource) })
	for i := 1; i <= 2; i++ {
		if err := EnsureEventSource(); err != nil {
			t.Fatalf("EnsureEventSource #%d: %v", i, err)
		}
	}
	if ok, err := sourceRegistered(); err != nil || !ok {
		t.Fatalf("source registered = %v, %v after EnsureEventSource", ok, err)
	}
	// L-EVT-1: the registration names this executable (cryo-agent.exe in
	// production, which carries the message table), not EventCreate.exe.
	exe, _ := exePath()
	if got, err := registeredMessageFile(); err != nil || !strings.EqualFold(got, exe) {
		t.Errorf("EventMessageFile = %q, %v; want the running executable %q", got, err, exe)
	}
	// FIX6: the open right after registering is the one that flaked.
	el, err := OpenEventLogFresh()
	if err != nil {
		t.Fatalf("OpenEventLogFresh after EnsureEventSource: %v", err)
	}
	defer el.Close()
	if err := el.Write("info", 1000, "test line"); err != nil {
		t.Errorf("Write: %v", err)
	}
}

// L-EVT-1 on an upgraded machine: a source an older agent registered with
// EventCreate.exe is pointed at the running executable. Elevated only, and
// only where no real source exists (it is removed afterwards).
func TestEnsureEventSourceRepointsEventCreateElevated(t *testing.T) {
	if !windows.GetCurrentProcessToken().IsElevated() {
		t.Skip("this process is not elevated: registering an Event Log source needs HKLM write access (the windows-latest CI runner is elevated)")
	}
	if ok, err := sourceRegistered(); err != nil || ok {
		t.Skipf("the %q source is already registered here (or unreadable: %v); not touching a real installation", EventSource, err)
	}
	t.Cleanup(func() { _ = eventlog.Remove(EventSource) })
	if err := eventlog.InstallAsEventCreate(EventSource, eventlog.Error|eventlog.Warning|eventlog.Info); err != nil {
		t.Fatalf("register the old way: %v", err)
	}
	if err := EnsureEventSource(); err != nil {
		t.Fatalf("EnsureEventSource: %v", err)
	}
	exe, _ := exePath()
	if got, err := registeredMessageFile(); err != nil || !strings.EqualFold(got, exe) {
		t.Errorf("EventMessageFile = %q, %v after EnsureEventSource; want %q", got, err, exe)
	}
}

func TestStopBudgets(t *testing.T) {
	if !(StopBudgetFor("preshutdown") > StopBudgetFor("stop") && StopBudgetFor("stop") > StopBudgetFor("shutdown")) {
		t.Error("pre-shutdown must get the longest budget and shutdown (about 20 s from Windows) the shortest")
	}
	if StopBudgetFor("stop") >= 30*time.Second {
		t.Error("a stop budget of 30 s or more would outlast the SCM's patience")
	}
}
