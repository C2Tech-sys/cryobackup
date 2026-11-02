package agent

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/agent/internal/fakerestic"
	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// M2-W3C: no answer to a result post can stop the agent from taking commands,
// and a cancel only stops the run it names.

func w3cAgent(t *testing.T) (*fakeConsole, *running, *syncBuf) {
	t.Helper()
	fc := newFakeConsole(t)
	fakerestic.Use(t, "backup_ok")
	o := fakeOptions(t, enroll(t, fc))
	log := &syncBuf{}
	o.Logger = slog.New(slog.NewTextHandler(log, &slog.HandlerOptions{Level: slog.LevelDebug}))
	r := start(t, o)
	waitFor(t, "the first scheduled run", 30*time.Second, func() bool { return len(fc.closedRuns()) == 1 })
	return fc, r, log
}

// A result the console refuses with a final 4xx is dropped once, journaled as
// result_refused, and the agent goes on to take the next command. Before the
// fix the agent re-posted the same result forever and fetched nothing new.
func TestRefusedResultDoesNotWedgeTheCommandLoop(t *testing.T) {
	fc, r, log := w3cAgent(t)
	for _, status := range []int{
		http.StatusBadRequest, http.StatusNotFound, http.StatusConflict, http.StatusGone,
		http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity,
		http.StatusTemporaryRedirect, http.StatusMovedPermanently,
	} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			refused := command(1, api.CommandCheckIn, "")
			fc.answerResults(refused.ID, status)
			fc.enqueue(refused)
			waitFor(t, "the refused result to be posted", 10*time.Second, func() bool { return fc.resultPostCount(refused.ID) >= 1 })

			// The next command is still fetched, run and answered.
			next := command(2, api.CommandCheckIn, "")
			fc.enqueue(next)
			waitFor(t, "the next command to be taken and answered", 10*time.Second, func() bool { return fc.resultPostCount(next.ID) >= 1 })

			if n := fc.resultPostCount(refused.ID); n != 1 {
				t.Errorf("the refused result was posted %d times, want 1", n)
			}
			row, ok, err := r.a.jr.Command(context.Background(), refused.ID)
			if err != nil || !ok {
				t.Fatalf("journal: ok=%v err=%v", ok, err)
			}
			if !row.Refused || row.Posted {
				t.Errorf("journal row = %+v, want refused and not posted", row)
			}
			// The console saw the next result before the agent journals it as
			// posted, so wait for that before counting what is left unposted.
			waitFor(t, "the next result to be marked posted", 10*time.Second, func() bool {
				row, _, _ := r.a.jr.Command(context.Background(), next.ID)
				return row.Posted
			})
			if un, _ := r.a.jr.UnpostedCommands(context.Background()); len(un) != 0 {
				t.Errorf("%d results still unposted", len(un))
			}
		})
	}
	text := log.String()
	if n := strings.Count(text, "result_refused"); n != 8 {
		t.Errorf("%d result_refused lines, want 8:\n%s", n, text)
	}
	for _, leak := range []string{`C:\`, `fixture`} {
		for _, line := range strings.Split(text, "\n") {
			if strings.Contains(line, "result_refused") && strings.Contains(line, leak) {
				t.Errorf("result_refused line carries %q: %s", leak, line)
			}
		}
	}
}

// A refused result that sat unposted across a restart (the console refused it
// on the immediate post's retry path, postResults) is dropped the same way.
func TestRefusedResultInTheRetryPathIsDropped(t *testing.T) {
	fc, r, _ := w3cAgent(t)
	// 503 on the immediate post leaves the result unposted; the retry in
	// postResults is then refused with 400.
	c1 := command(1, api.CommandCheckIn, "")
	fc.answerResults(c1.ID, http.StatusServiceUnavailable, http.StatusBadRequest)
	fc.enqueue(c1)
	waitFor(t, "the refused retry", 15*time.Second, func() bool { return fc.resultPostCount(c1.ID) >= 2 })
	c2 := command(2, api.CommandCheckIn, "")
	fc.enqueue(c2)
	waitFor(t, "the command after the refused retry", 15*time.Second, func() bool { return fc.resultPostCount(c2.ID) >= 1 })
	if n := fc.resultPostCount(c1.ID); n != 2 {
		t.Errorf("posts = %d, want 2 (503, then 400)", n)
	}
	row, _, _ := r.a.jr.Command(context.Background(), c1.ID)
	if !row.Refused {
		t.Errorf("journal row = %+v", row)
	}
}

// Auth problems, throttling and server errors keep the result and retry it
// with backoff, as before; the result is posted once the console recovers.
func TestRetryableResultAnswersAreRetried(t *testing.T) {
	fc, r, _ := w3cAgent(t)
	for _, status := range []int{
		http.StatusUnauthorized, http.StatusForbidden, http.StatusRequestTimeout,
		http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusServiceUnavailable,
	} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			c := command(1, api.CommandCheckIn, "")
			fc.answerResults(c.ID, status, status)
			fc.enqueue(c)
			waitFor(t, "the result to be retried until accepted", 20*time.Second, func() bool {
				row, ok, _ := r.a.jr.Command(context.Background(), c.ID)
				return ok && row.Posted
			})
			if n := fc.resultPostCount(c.ID); n != 3 {
				t.Errorf("posts = %d, want 3 (%d, %d, then accepted)", n, status, status)
			}
			if row, _, _ := r.a.jr.Command(context.Background(), c.ID); row.Refused {
				t.Error("a retryable answer dropped the result")
			}
		})
	}
}

// cancel with a run_id stops only that run: a cancel aimed at another run
// (a stale or finished one) leaves the active run alone and answers
// run_mismatch; the same cancel naming the active run stops it.
func TestCancelHonoursRunID(t *testing.T) {
	fc := newFakeConsole(t)
	dir := enroll(t, fc)
	fakerestic.Use(t, "hang_graceful")
	r := start(t, fakeOptions(t, dir))
	waitFor(t, "the open run at the console", 30*time.Second, func() bool {
		fc.mu.Lock()
		defer fc.mu.Unlock()
		return len(fc.runOrder) == 1
	})
	active := func() string {
		r.a.mu.Lock()
		defer r.a.mu.Unlock()
		if r.a.running == nil {
			return ""
		}
		return r.a.running.id
	}
	runID := active()
	if runID == "" {
		t.Fatal("no active run")
	}

	// A cancel for some other run (the id of an earlier, finished run).
	stale := command(1, api.CommandCancel, `{"run_id":"`+newULID()+`"}`)
	fc.enqueue(stale)
	waitFor(t, "the stale cancel result", 10*time.Second, func() bool { return len(fc.commandResults(stale.ID)) >= 1 })
	if res := fc.commandResults(stale.ID)[0]; res.Status != api.CommandSucceeded || string(res.Result) != `{"cancelled":false,"reason":"run_mismatch"}` {
		t.Errorf("stale cancel result = %+v %s", res, res.Result)
	}
	time.Sleep(500 * time.Millisecond)
	if got := active(); got != runID {
		t.Fatalf("a cancel for another run stopped the active run (active = %q, want %q)", got, runID)
	}
	if n := len(fc.closedRuns()); n != 0 {
		t.Fatalf("%d runs closed after a mismatched cancel", n)
	}

	// A cancel without a run_id keeps its old meaning; cancel names the run here.
	own := command(2, api.CommandCancel, `{"run_id":"`+runID+`"}`)
	fc.enqueue(own)
	waitFor(t, "the cancelled run", 30*time.Second, func() bool { return len(fc.closedRuns()) == 1 })
	if run := fc.closedRuns()[0]; run.ID != runID || run.Outcome != restic.OutcomeCancelled {
		t.Errorf("run = %+v", run)
	}
	waitFor(t, "the cancel result", 10*time.Second, func() bool { return len(fc.commandResults(own.ID)) >= 1 })
	if res := fc.commandResults(own.ID)[0]; res.Status != api.CommandSucceeded || string(res.Result) != `{"cancelled":true}` {
		t.Errorf("cancel result = %+v %s", res, res.Result)
	}

	// The run is over: a late cancel naming it stops nothing and says why.
	late := command(3, api.CommandCancel, `{"run_id":"`+runID+`"}`)
	fc.enqueue(late)
	waitFor(t, "the late cancel result", 10*time.Second, func() bool { return len(fc.commandResults(late.ID)) >= 1 })
	if res := fc.commandResults(late.ID)[0]; string(res.Result) != `{"cancelled":false,"reason":"run_mismatch"}` {
		t.Errorf("late cancel result = %s", res.Result)
	}
	// ... and one that names no run still reports that nothing is running.
	none := command(4, api.CommandCancel, "")
	fc.enqueue(none)
	waitFor(t, "the run-less cancel result", 10*time.Second, func() bool { return len(fc.commandResults(none.ID)) >= 1 })
	if res := fc.commandResults(none.ID)[0]; string(res.Result) != `{"cancelled":false,"reason":"no_run"}` {
		t.Errorf("run-less cancel result = %s", res.Result)
	}
}

// cmdCancel is the whole decision: run_id empty = any active run (old
// console), run_id set = only that run.
func TestCmdCancelDecision(t *testing.T) {
	a := &Agent{}
	ctx := context.Background()
	if ok, why := a.cmdCancel(ctx, ""); ok || why != "no_run" {
		t.Errorf("idle, no run_id = %v %q", ok, why)
	}
	if ok, why := a.cmdCancel(ctx, "R1"); ok || why != "run_mismatch" {
		t.Errorf("idle, run_id = %v %q", ok, why)
	}
	var cancelled int
	a.running = &activeRun{id: "R2", cancel: func(error) { cancelled++ }}
	if ok, why := a.cmdCancel(ctx, "R1"); ok || why != "run_mismatch" || cancelled != 0 {
		t.Errorf("newer run active, stale run_id = %v %q cancelled=%d", ok, why, cancelled)
	}
	if ok, _ := a.cmdCancel(ctx, "R2"); !ok || cancelled != 1 {
		t.Errorf("matching run_id: ok=%v cancelled=%d", ok, cancelled)
	}
	if ok, _ := a.cmdCancel(ctx, ""); !ok || cancelled != 2 {
		t.Errorf("no run_id stops the active run: ok=%v cancelled=%d", ok, cancelled)
	}
}

// A command whose id fails api validation (a hostile or broken console) gets
// no post at all: its result is journaled refused locally and the agent takes
// the next command.
func TestResultForAnInvalidCommandIDIsNotPosted(t *testing.T) {
	fc, r, log := w3cAgent(t)
	for _, id := range []string{"..", "."} {
		bad := api.Command{ID: id, Type: api.CommandCheckIn, CreatedAt: time.Now().Add(-time.Minute), ExpiresAt: time.Now().Add(time.Hour)}
		fc.enqueue(bad)
		next := command(2, api.CommandCheckIn, "")
		fc.enqueue(next)
		waitFor(t, "the command after the invalid id", 15*time.Second, func() bool { return fc.resultPostCount(next.ID) >= 1 })
		row, ok, err := r.a.jr.Command(context.Background(), id)
		if err != nil || !ok {
			t.Fatalf("%q: journal ok=%v err=%v", id, ok, err)
		}
		if !row.Refused || row.Posted {
			t.Errorf("%q: journal row = %+v, want refused locally", id, row)
		}
	}
	fc.mu.Lock()
	defer fc.mu.Unlock()
	for _, b := range fc.bodies {
		if b.Method == http.MethodPost && strings.Contains(b.Path, "/commands/") && !strings.HasSuffix(b.Path, "/result") {
			t.Errorf("unexpected post to %q", b.Path)
		}
	}
	if fc.resultPosts[".."] != 0 || fc.resultPosts["."] != 0 || fc.resultPosts[""] != 0 {
		t.Errorf("results were posted for invalid ids: %v", fc.resultPosts)
	}
	if !strings.Contains(log.String(), "result_refused") {
		t.Error("no result_refused line")
	}
}
