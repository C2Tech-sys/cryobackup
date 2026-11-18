package maintain

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// Fixes from the blind verification of ADR 0019 wave 1 (ticket P19-F1), in the
// same harness as purge_adr19_test.go: the fake program is rclone, no remote is
// involved.

const (
	f1DNS    = "inline:2026/11/17 10:00:00 ERROR : dial tcp: lookup s3.example: no such host\n"
	f1Denied = "inline:2026/11/17 10:00:00 ERROR : AccessDenied: Access Denied\n\tstatus code: 403, forbidden\n"
)

var (
	f1SizeMissing = fakeRule{Cmd: "size", Exit: 3, Stderr: notFound}
	f1ListMissing = fakeRule{Cmd: "lsjson", Exit: 3, Stdout: "inline:[\n", Stderr: notFound}
	f1SizeSlow    = fakeRule{Cmd: "size", SleepMS: 20000, Stdout: ruleSize5.Stdout}
)

// waitFor polls cond every 25 ms for up to 10 s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 400; i++ {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// running reports whether the fake has been started with this subcommand.
func (e *tenv) running(cmd string) bool {
	for _, c := range e.rcloneCalls() {
		if c.Args[0] == cmd {
			return true
		}
	}
	return false
}

func setPurgeDeadline(t *testing.T, d time.Duration) {
	t.Helper()
	old := purgeDeadline
	purgeDeadline = d
	t.Cleanup(func() { purgeDeadline = old })
}

// TestPurgeRepoNeedsThePurgeMarkADR19 (item 1): a retired device that the
// console has not marked for purge is refused with not_requested and nothing
// runs; once store.RequestDevicePurge has marked it (owner, audited), the same
// call purges it.
func TestPurgeRepoNeedsThePurgeMarkADR19(t *testing.T) {
	e := newPurgeEnv(t, []fakeRule{ruleSize5, rulePurgeOK, ruleListNil}, nil)
	id := e.unmarkedID()

	mark := len(e.calls())
	res, err := e.w.PurgeRepo(e.ctx, purgeReq(id))
	var r *PurgeRefusedError
	if !errors.As(err, &r) || r.Reason != PurgeRefusedNotRequested {
		t.Fatalf("retired, unmarked: err = %v (result %+v), want refusal %q", err, res, PurgeRefusedNotRequested)
	}
	wantCode(t, err, adminpath.CodeInvalidRequest, "")
	if c := e.callsSince(mark); len(c) != 0 {
		t.Fatalf("a purge of an unmarked device started the fake: %+v", c)
	}
	if res != (adminpath.PurgeRepoResult{}) || e.lease(id) != "" {
		t.Fatalf("a refusal carried a result (%+v) or took the lease (%q)", res, e.lease(id))
	}
	if !strings.Contains(e.logs.String(), "repository purge refused") || !strings.Contains(e.logs.String(), "not_requested") {
		t.Errorf("the refusal is not logged with its reason:\n%s", e.logs.String())
	}

	admin, err := e.st.GetAdminByUsername(e.ctx, store.FixtureAdminUsername)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.RequestDevicePurge(e.ctx, store.ScopeAll(), id, "lab machine gone",
		store.AuditActor{AdminID: admin.ID, Username: admin.Username, Method: "api"}); err != nil {
		t.Fatalf("RequestDevicePurge: %v", err)
	}
	res, err = e.w.PurgeRepo(e.ctx, purgeReq(id))
	if err != nil || res.ObjectsRemoved != 5 || res.RemainingObjects != 0 {
		t.Fatalf("after the mark: %+v, %v; want the purge to run", res, err)
	}
	e.noLeak(t, nil)
}

// TestPurgeRepoParentMustExistADR19 (item 2): the device prefix's "not found"
// counts as zeros only when the parent <remote>:<bucket>/<prefix> was listed.
// A parent that is missing (a wrong bucket or remote) or cannot be listed is a
// classified error, and nothing after the parent listing runs.
func TestPurgeRepoParentMustExistADR19(t *testing.T) {
	parent := func(r fakeRule) fakeRule { r.Cmd, r.Contains = "lsjson", "--dirs-only"; return r }
	for name, c := range map[string]struct {
		parent fakeRule
		code   string
		class  restic.ErrorClass
	}{
		"parent not found":          {parent(fakeRule{Exit: 3, Stdout: "inline:[\n", Stderr: notFound}), adminpath.CodeStorageUnreachable, restic.ClassStorage},
		"parent listing fails":      {parent(fakeRule{Exit: 1, Stderr: f1DNS}), adminpath.CodeStorageUnreachable, restic.ClassNetwork},
		"parent listing denied":     {parent(fakeRule{Exit: 1, Stderr: f1Denied}), adminpath.CodeResticFailed, restic.ClassAuth},
		"parent listing unreadable": {parent(fakeRule{Stdout: "inline:{}"}), adminpath.CodeResticFailed, restic.ClassParse},
		"parent listing cut short":  {parent(fakeRule{Stdout: `inline:[{"Path":"x","IsDir":true},`}), adminpath.CodeResticFailed, restic.ClassParse},
		"parent listing slow":       {parent(fakeRule{SleepMS: 20000, Stdout: "inline:[]"}), adminpath.CodeTimeout, ""},
	} {
		t.Run(name, func(t *testing.T) {
			if c.code == adminpath.CodeTimeout {
				setPurgeDeadline(t, 1500*time.Millisecond)
			}
			// The device prefix answers "not found" too: the old code read that as
			// zeros and reported the repository gone.
			e := newPurgeEnv(t, []fakeRule{c.parent, f1SizeMissing, rulePurgeOK, f1ListMissing}, nil)
			id := e.retiredID()
			mark := len(e.calls())
			res, err := e.w.PurgeRepo(e.ctx, purgeReq(id))
			wantCode(t, err, c.code, c.class)
			if res != (adminpath.PurgeRepoResult{}) {
				t.Errorf("a failed parent listing carried a result: %+v", res)
			}
			calls := e.callsSince(mark)
			if len(calls) != 1 || calls[0].Args[0] != "lsjson" || calls[0].Args[1] != "--dirs-only" {
				t.Fatalf("calls = %+v, want the parent listing and nothing else", calls)
			}
			if h := e.lease(id); h != "" {
				t.Errorf("lease left held by %q", h)
			}
			e.noLeak(t, err)
		})
	}

	t.Run("parent present, device prefix absent: zeros", func(t *testing.T) {
		e := newPurgeEnv(t, []fakeRule{ruleParentOK, f1SizeMissing, rulePurgeOK, f1ListMissing}, nil)
		id := e.retiredID()
		mark := len(e.calls())
		res, err := e.w.PurgeRepo(e.ctx, purgeReq(id))
		if err != nil || res != (adminpath.PurgeRepoResult{Elapsed: res.Elapsed}) {
			t.Fatalf("= %+v, %v; want success with zeros", res, err)
		}
		calls := e.callsSince(mark)
		want := []string{
			"lsjson --dirs-only --no-modtime --no-mimetype -- admin:test-bucket/fleet",
			"size --json -- admin:test-bucket/fleet/" + id,
			"lsjson -R --files-only --no-modtime --no-mimetype -- admin:test-bucket/fleet/" + id,
		}
		if len(calls) != len(want) {
			t.Fatalf("calls = %+v, want %q", calls, want)
		}
		for i, c := range calls {
			if got := strings.Join(c.Args, " "); got != want[i] {
				t.Errorf("call %d = %q, want %q", i, got, want[i])
			}
		}
		e.noLeak(t, nil)
	})
}

// TestPurgeRepoExcludesASecondPurgeWithoutAScheduleRowADR19 (item 4a): a
// device with no maintenance_schedule row has no database lease, yet a second
// purge of it in the same worker is refused as locked, runs nothing, and the
// claim is released when the first call ends.
func TestPurgeRepoExcludesASecondPurgeWithoutAScheduleRowADR19(t *testing.T) {
	e := newPurgeEnv(t, []fakeRule{f1SizeSlow, rulePurgeOK, ruleListNil}, nil)
	id := e.retiredID()
	if err := e.st.Write(e.ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`DELETE FROM maintenance_schedule WHERE device_id = ?`, id)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(e.ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := e.w.PurgeRepo(ctx, purgeReq(id))
		done <- err
	}()
	waitFor(t, "the first purge's measurement", func() bool { return e.running("size") })

	mark := len(e.calls())
	sctx, scancel := context.WithTimeout(e.ctx, 5*time.Second)
	defer scancel()
	_, err := e.w.PurgeRepo(sctx, purgeReq(id))
	wantCode(t, err, adminpath.CodeLocked, restic.ClassLock)
	if c := e.callsSince(mark); len(c) != 0 {
		t.Fatalf("a second purge of the same device started the fake: %+v", c)
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("first purge: %v, want context.Canceled", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the first purge did not stop")
	}

	e.scenario(withParent([]fakeRule{ruleSize5, rulePurgeOK, ruleListNil}))
	if res, err := e.w.PurgeRepo(e.ctx, purgeReq(id)); err != nil || res.ObjectsRemoved != 5 {
		t.Fatalf("after the first purge ended: %+v, %v; the claim must be free", res, err)
	}
}

// TestPurgeRepoDeadlineADR19 (item 4b): the whole call is bounded by
// purgeDeadline, a slow rclone step and a wait for a concurrency slot alike;
// the answer is timeout, and the lease and the claim are released.
func TestPurgeRepoDeadlineADR19(t *testing.T) {
	t.Run("a step that outlives the deadline", func(t *testing.T) {
		setPurgeDeadline(t, 1500*time.Millisecond)
		e := newPurgeEnv(t, []fakeRule{f1SizeSlow, rulePurgeOK, ruleListNil}, nil)
		id := e.retiredID()
		ctx, cancel := context.WithTimeout(e.ctx, 15*time.Second) // a safety net, never the deadline under test
		defer cancel()
		start := time.Now()
		_, err := e.w.PurgeRepo(ctx, purgeReq(id))
		wantCode(t, err, adminpath.CodeTimeout, "")
		if d := time.Since(start); d > 12*time.Second {
			t.Errorf("the purge took %v: the deadline did not stop it", d)
		}
		if e.running("purge") {
			t.Error("rclone purge ran after the measurement timed out")
		}
		if h := e.lease(id); h != "" {
			t.Errorf("lease left held by %q", h)
		}
		if !strings.Contains(e.logs.String(), "repository purge stopped at its deadline") {
			t.Errorf("the deadline is not logged:\n%s", e.logs.String())
		}
		e.noLeak(t, err)

		purgeDeadline = 110 * time.Minute
		e.scenario(withParent([]fakeRule{ruleSize5, rulePurgeOK, ruleListNil}))
		if res, err := e.w.PurgeRepo(e.ctx, purgeReq(id)); err != nil || res.ObjectsRemoved != 5 {
			t.Fatalf("after a timed-out purge: %+v, %v; the claim and the lease must be free", res, err)
		}
	})

	t.Run("waiting for a concurrency slot", func(t *testing.T) {
		setPurgeDeadline(t, 500*time.Millisecond)
		e := newPurgeEnv(t, []fakeRule{ruleSize5, rulePurgeOK, ruleListNil}, nil)
		id := e.retiredID()
		for i := 0; i < cap(e.w.sem); i++ {
			e.w.sem <- struct{}{}
		}
		defer func() {
			for i := 0; i < cap(e.w.sem); i++ {
				<-e.w.sem
			}
		}()
		mark := len(e.calls())
		ctx, cancel := context.WithTimeout(e.ctx, 15*time.Second) // safety net
		defer cancel()
		_, err := e.w.PurgeRepo(ctx, purgeReq(id))
		wantCode(t, err, adminpath.CodeTimeout, "")
		if c := e.callsSince(mark); len(c) != 0 {
			t.Fatalf("calls = %+v, want none: no slot came free", c)
		}
		if h := e.lease(id); h != "" {
			t.Errorf("lease left held by %q", h)
		}
	})

	t.Run("the caller's own cancel is not a timeout", func(t *testing.T) {
		e := newPurgeEnv(t, []fakeRule{ruleSize5, rulePurgeOK, ruleListNil}, nil)
		ctx, cancel := context.WithCancel(e.ctx)
		cancel()
		_, err := e.w.PurgeRepo(ctx, purgeReq(e.retiredID()))
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	})
}

// TestPurgeDeadlinesADR19 pins the arithmetic: every step at its own maximum
// fits in purgeDeadline, which is below the lease and below the admin-path
// client's timeout for purge_repo.
func TestPurgeDeadlinesADR19(t *testing.T) {
	steps := purgeParentListTimeout + purgeListTimeout + purgeRemoveTimeout + purgeListTimeout
	if steps > purgeDeadline {
		t.Errorf("the steps add up to %v, beyond the purge deadline %v", steps, purgeDeadline)
	}
	if purgeDeadline >= purgeLeaseTTL {
		t.Errorf("purge deadline %v is not below the lease %v", purgeDeadline, purgeLeaseTTL)
	}
	if c := adminpath.DefaultTimeouts()[adminpath.MethodPurgeRepo]; purgeDeadline >= c {
		t.Errorf("purge deadline %v is not below the client's timeout %v", purgeDeadline, c)
	}
}
