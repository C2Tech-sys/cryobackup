package supervisor

import (
	"context"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// hangCapMiB caps the alloc_hang child. It is larger than the alloc test's cap
// because the child is this test binary, and under -race the race runtime
// alone needs more than 128 MiB to start (it dies with exit 66 below that).
const hangCapMiB = 512

// statusNoMemoryExit is STATUS_NO_MEMORY as os.ProcessState.ExitCode reports it.
const statusNoMemoryExit = 0xC0000017

// runBounded runs spec and fails the test if Run has not returned within
// bound. A child that is never ended exits by itself much later
// (fakerestic.allocHangSafetyExit), so a broken supervisor fails here instead
// of hanging the suite.
func runBounded(t *testing.T, ctx context.Context, sup *Supervisor, spec Spec, bound time.Duration) (Result, time.Duration) {
	t.Helper()
	type out struct {
		res Result
		err error
	}
	done := make(chan out, 1)
	start := time.Now()
	go func() {
		res, err := sup.Run(ctx, spec)
		done <- out{res, err}
	}()
	select {
	case o := <-done:
		if o.err != nil {
			t.Fatal(o.err)
		}
		return o.res, time.Since(start)
	case <-time.After(bound):
		t.Fatalf("Run did not return within %s: the stuck child was not ended", bound)
	}
	return Result{}, 0
}

// TestMemoryCapHungChildIsTerminated: a child that hits its cap and then
// neither exits nor closes its pipes (fakerestic "alloc_hang", the behaviour
// measured with the Go runtime) is ended by the supervisor after
// memoryKillGrace, deterministically, and the run is failed/memory with
// STATUS_NO_MEMORY. The Go runtime's traceback on stderr is not counted as
// errors: the counts carry the one memory row only (M1-FIX1 C7, C10).
func TestMemoryCapHungChildIsTerminated(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("the process memory cap is a Windows job-object limit")
	}
	spec := fakeSpec(t, "alloc_hang", restic.KindBackup)
	spec.MemoryLimitMiB = hangCapMiB
	res, took := runBounded(t, context.Background(), verified(t, spec), spec, 30*time.Second)
	if took < memoryKillGrace {
		t.Errorf("Run returned after %s, before the %s grace: the child was not stuck or was killed early", took, memoryKillGrace)
	}
	if !res.MemoryLimitHit {
		t.Error("the job object's memory-limit message was not seen")
	}
	if res.ExitCode != statusNoMemoryExit {
		t.Errorf("exit = %#x, want STATUS_NO_MEMORY (the supervisor's memory kill)", res.ExitCode)
	}
	if res.Restic.Outcome != restic.OutcomeFailed || res.Restic.ErrorClass != restic.ClassMemory {
		t.Errorf("result = %+v, want failed/memory", res.Restic)
	}
	if res.CancelMode != "" || res.Cancel != restic.CancelNone {
		t.Errorf("cancel mode %q cause %d: nobody cancelled this run", res.CancelMode, res.Cancel)
	}
	if got, want := res.Counts.ByClass(), map[restic.ErrorClass]int{restic.ClassMemory: 1}; !reflect.DeepEqual(got, want) {
		t.Errorf("counts by class = %v, want %v (the runtime traceback is not a list of errors)", got, want)
	}
	// The traceback stays verbatim in the local run log.
	n := 0
	for _, l := range lines(t, spec.RunLogPath) {
		if strings.HasPrefix(l, "goroutine ") {
			n++
		}
	}
	if n == 0 {
		t.Error("the run log lost the runtime traceback")
	}
}

// TestServiceStopDuringMemoryGraceActsAtOnce: a service stop that arrives while
// a capped child is in its memory-kill grace ends the child immediately, not
// after the rest of the grace; the outcome stays failed/memory and the cancel
// is recorded as what it was (M1-FIX1 C9).
func TestServiceStopDuringMemoryGraceActsAtOnce(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("the process memory cap is a Windows job-object limit")
	}
	spec := fakeSpec(t, "alloc_hang", restic.KindBackup)
	spec.MemoryLimitMiB = hangCapMiB
	sup := verified(t, spec)

	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	var cancelledAt time.Time
	inGrace := make(chan struct{})
	testHookMemoryGrace = func() { close(inGrace) }
	t.Cleanup(func() { testHookMemoryGrace = nil })
	go func() {
		<-inGrace
		cancelledAt = time.Now()
		cancel(ErrServiceStop)
	}()

	res, _ := runBounded(t, ctx, sup, spec, 30*time.Second)
	if cancelledAt.IsZero() {
		t.Fatal("the memory grace was never entered")
	}
	if since := time.Since(cancelledAt); since >= memoryKillGrace-time.Second {
		t.Errorf("Run returned %s after the stop: the stop waited out the memory grace", since)
	}
	if res.ExitCode != statusNoMemoryExit {
		t.Errorf("exit = %#x, want STATUS_NO_MEMORY", res.ExitCode)
	}
	if res.Restic.Outcome != restic.OutcomeFailed || res.Restic.ErrorClass != restic.ClassMemory {
		t.Errorf("result = %+v, want failed/memory (the cap, not the stop, decides)", res.Restic)
	}
	if res.Cancel != restic.CancelService || res.CancelMode != CancelTerminated {
		t.Errorf("cancel cause %d mode %q, want the service stop recorded as terminated", res.Cancel, res.CancelMode)
	}
	var note string
	for _, l := range lines(t, spec.RunLogPath) {
		if strings.Contains(l, "cryo_supervisor_note") {
			note = l
		}
	}
	if !strings.Contains(note, "memory-cap grace") || !strings.Contains(note, `"cancel_cause":"service_stop"`) {
		t.Errorf("run log note = %q, want the stop during the memory grace", note)
	}
	if strings.Contains(note, "exited on its own") {
		t.Errorf("run log note says the child exited on its own: %q", note)
	}
}
