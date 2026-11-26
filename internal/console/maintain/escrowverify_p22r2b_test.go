package maintain

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// P22-R2b: the escrow verifier's fixes from the opus review (B1 to B3, G1 to G3).

// catArgvs returns the argv of every cat call so far.
func (e *tenv) catArgvs() []fakeCall {
	var out []fakeCall
	for _, c := range e.repoCalls() {
		if c.Args[0] == "cat" {
			out = append(out, c)
		}
	}
	return out
}

// B1: "restic cat config" without --no-lock creates a lock object in the repository (proven on
// restic 0.19.1 with locks/ write-denied), so the verifier's check, an on-demand verification and
// enrollment's config read all run with --no-lock and without --retry-lock, in production mode.
// Killed mutant: catConfig back on w.common().
func TestP22R2bEscrowCatConfigRunsWithNoLock(t *testing.T) {
	e := newTenv(t, []fakeRule{{Cmd: "cat", Stdout: "cat_config.json"}, {Cmd: "stats", Stdout: "stats_raw_data.json"}, {Cmd: "snapshots", Stdout: "snapshots.json"}}, nil)
	dev1 := e.esc
	e.onlyDevice(dev1)
	e.provenNowExcept(dev1)
	e.setProof(dev1, e.clk.Now().Add(-8*24*time.Hour), time.Time{})
	// The scheduler's check.
	if rep, err := e.w.RunOnce(e.ctx); err != nil || rep.EscrowVerified != 1 {
		t.Fatalf("pass = %+v, %v", rep, err)
	}
	// The console's on-demand check (adminpath CatConfig).
	if _, err := e.w.CatConfig(e.ctx, dev(dev1)); err != nil {
		t.Fatal(err)
	}
	calls := e.catArgvs()
	if len(calls) != 2 {
		t.Fatalf("%d cat calls, want 2", len(calls))
	}
	for _, c := range calls {
		if e.w.inDrill() || !hasArg(c, "--no-lock") || hasArg(c, "--retry-lock") {
			t.Fatalf("cat config argv %v: want --no-lock and no --retry-lock in production mode", c.Args)
		}
	}
	// Reads that need the repository's lock protocol keep their --retry-lock.
	if _, err := e.w.Snapshots(e.ctx, dev(dev1)); err != nil {
		t.Fatal(err)
	}
	seen := false
	for _, c := range e.repoCalls() {
		if c.Args[0] == "snapshots" {
			seen = true
			if hasArg(c, "--no-lock") || !hasArg(c, "--retry-lock") {
				t.Fatalf("snapshots argv %v changed", c.Args)
			}
		}
	}
	if !seen {
		t.Fatal("no snapshots call")
	}
}

// B2: a proof time later than now + FutureSkewAllowed proves nothing. The verifier re-proves it and
// the state rules read it as unverified, so the device row and banner C22 are not green for the
// skew plus the recheck interval. Killed mutants: the future-proof clause dropped from
// escrowVerifyDue; recorded() replaced by Time() in escrowIssueRecheck (state tests).
func TestP22R2bFutureProofIsDue(t *testing.T) {
	now := t0
	ms := func(t time.Time) state.Millis { return state.Millis(t.UnixMilli()) }
	key := func(proof time.Time) state.EscrowKeyFacts {
		return state.EscrowKeyFacts{DeviceID: "01JDEVICE00000000000000001", Escrow: state.EscrowFacts{Custody: "escrowed", CreatedAt: ms(now.Add(-90 * 24 * time.Hour)), VerifiedAt: ms(proof)}}
	}
	for _, c := range []struct {
		name  string
		proof time.Time
		want  bool
	}{
		{"60 days ahead", now.Add(60 * 24 * time.Hour), true},
		{"a day ahead", now.Add(24 * time.Hour), true},
		{"just past the skew", now.Add(state.FutureSkewAllowed + time.Second), true},
		{"inside the skew", now.Add(state.FutureSkewAllowed - time.Second), false},
		{"a minute ago", now.Add(-time.Minute), false},
	} {
		if got := escrowVerifyDue(key(c.proof), now); got != c.want {
			t.Errorf("%s: due %v, want %v", c.name, got, c.want)
		}
	}
	// End to end: a key proven "60 days from now" is checked by the next pass and proven for real.
	e := newTenv(t, []fakeRule{{Cmd: "cat", Stdout: "cat_config.json"}, {Cmd: "stats", Stdout: "stats_raw_data.json"}}, nil)
	dev1 := e.esc
	e.onlyDevice(dev1)
	e.provenNowExcept(dev1)
	e.setProof(dev1, e.clk.Now().Add(60*24*time.Hour), time.Time{})
	if !e.escrowFlagged(dev1) {
		t.Fatal("precondition: a proof 60 days ahead must not read green")
	}
	rep, err := e.w.RunOnce(e.ctx)
	if err != nil || rep.EscrowVerified != 1 || e.catCalls() != 1 {
		t.Fatalf("pass = %+v, %v, %d cat calls", rep, err, e.catCalls())
	}
	if esc := e.escrowOf(dev1); esc.VerifiedAt == nil || !esc.VerifiedAt.Equal(e.clk.Now()) {
		t.Fatalf("the future proof was not replaced: %+v", esc)
	}
	if e.escrowFlagged(dev1) {
		t.Fatal("ESCROW_UNVERIFIED after the real proof")
	}
}

// B3: a recorded failure (wrong_password, repo_missing) is retried hourly, as the comment says, not
// at the next weekly due time: the key is due once the one-hour retry hold has passed. Killed
// mutant: the LastVerifyErrorClass clause dropped from escrowVerifyDue.
func TestP22R2bFailedKeyIsRetriedHourly(t *testing.T) {
	now := t0
	ms := func(t time.Time) state.Millis { return state.Millis(t.UnixMilli()) }
	failed := func(class string, proof, attempt time.Time) state.EscrowKeyFacts {
		k := state.EscrowKeyFacts{DeviceID: "01JDEVICE00000000000000001", Escrow: state.EscrowFacts{Custody: "escrowed", VerifiedAt: ms(proof), LastVerifyErrorClass: class}}
		if !attempt.IsZero() {
			k.LastAttemptAt = ms(attempt)
		}
		return k
	}
	day := now.Add(-24 * time.Hour)
	for _, c := range []struct {
		name string
		k    state.EscrowKeyFacts
		want bool
	}{
		{"1-day-old proof, repo_missing, attempt 2 h ago", failed("repo_missing", day, now.Add(-2*time.Hour)), true},
		{"1-day-old proof, wrong_password, attempt 2 h ago", failed("wrong_password", day, now.Add(-2*time.Hour)), true},
		{"the same, attempt 59 min ago (retry hold)", failed("repo_missing", day, now.Add(-59*time.Minute)), false},
		{"the same, attempt exactly an hour ago", failed("repo_missing", day, now.Add(-EscrowVerifyRetry)), true},
		{"failure never attempted since", failed("repo_missing", day, time.Time{}), true},
		{"no failure, 1-day-old proof, attempt 2 h ago", failed("", day, now.Add(-2*time.Hour)), false},
	} {
		if got := escrowVerifyDue(c.k, now); got != c.want {
			t.Errorf("%s: due %v, want %v", c.name, got, c.want)
		}
	}
	// End to end: the repository is repaired after a failed check; the next pass an hour later proves
	// the key again and clears the failure, without waiting a week or pressing Verify now.
	e := newTenv(t, []fakeRule{{Cmd: "cat", Exit: 10, Stderr: "exit10_repo_missing.stderr.jsonl"}, {Cmd: "stats", Stdout: "stats_raw_data.json"}}, nil)
	dev1 := e.esc
	e.onlyDevice(dev1)
	e.provenNowExcept(dev1)
	e.setProof(dev1, e.clk.Now().Add(-8*24*time.Hour), time.Time{}) // due by age: the first check happens
	if rep, err := e.w.RunOnce(e.ctx); err != nil || rep.EscrowUnverified != 1 {
		t.Fatalf("failing pass = %+v, %v", rep, err)
	}
	esc := e.escrowOf(dev1)
	if esc.LastVerifyErrorClass != "repo_missing" {
		t.Fatalf("failure not recorded: %+v", esc)
	}
	// The failed check kept the old proof (8 days). Make it 1 day old, so that only the recorded failure
	// can make the key due again, and only after the retry hold (B3).
	if err := e.st.Write(e.ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE repo_key_escrow SET verified_at = ? WHERE device_id = ? AND retired_at IS NULL`, e.clk.Now().Add(-24*time.Hour).UnixMilli(), dev1)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	e.scenario([]fakeRule{{Cmd: "cat", Stdout: "cat_config.json"}, {Cmd: "stats", Stdout: "stats_raw_data.json"}})
	e.clk.Advance(30 * time.Minute)
	before := e.catCalls()
	if _, err := e.w.RunOnce(e.ctx); err != nil || e.catCalls() != before {
		t.Fatalf("retried inside the hour: %d -> %d (%v)", before, e.catCalls(), err)
	}
	e.clk.Advance(31 * time.Minute)
	rep, err := e.w.RunOnce(e.ctx)
	if err != nil || rep.EscrowVerified != 1 {
		t.Fatalf("retry pass = %+v, %v", rep, err)
	}
	if esc := e.escrowOf(dev1); esc.LastVerifyErrorClass != "" || esc.VerifiedAt == nil || !esc.VerifiedAt.Equal(e.clk.Now()) {
		t.Fatalf("the repaired key is still failing: %+v", esc)
	}
	if e.escrowFlagged(dev1) {
		t.Fatal("ESCROW_UNVERIFIED after the repair was proven")
	}
}

// G1: the customer's key_custody clause of the candidate query. A device of a customer-held customer
// is never checked, even though its key row says escrowed: no cat config runs for it. This is the
// end-to-end assertion; it does not kill M1 (the customer clause dropped from store.escrowVerifyKeysSQL)
// by itself, because the worker also refuses a customer-held device before it starts restic
// (Worker.prepare, DH22). The store's TestP22R2bCustomerHeldCustomerIsNotACandidate is what kills M1.
func TestP22R2bCustomerHeldCustomerGetsNoCatConfig(t *testing.T) {
	e := newTenv(t, []fakeRule{{Cmd: "cat", Stdout: "cat_config.json"}, {Cmd: "stats", Stdout: "stats_raw_data.json"}}, nil)
	dev1 := e.esc
	d, err := e.st.GetDevice(e.ctx, store.ScopeAll(), dev1)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.Write(e.ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE customers SET key_custody = 'customer_held' WHERE id = ?`, d.CustomerID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if esc := e.escrowOf(dev1); esc.Custody != "escrowed" {
		t.Fatalf("precondition: the key row says %q", esc.Custody)
	}
	e.provenNowExcept()
	e.setProof(dev1, e.clk.Now().Add(-30*24*time.Hour), time.Time{}) // due, were it a candidate
	rep, err := e.w.RunOnce(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range e.catDevices() {
		dd, err := e.st.GetDevice(e.ctx, store.ScopeAll(), id)
		if err != nil {
			t.Fatal(err)
		}
		if dd.CustomerID == d.CustomerID {
			t.Fatalf("cat config ran for device %s of a customer-held customer (%+v)", id, rep)
		}
	}
}

// G2: the weekly cadence is pinned to its literal, and so is the due boundary with the device's
// stagger offset (EscrowVerifyDueAt, which the device page reads too). Killed mutants: M2 (6 d) and
// M2b (8 d) for EscrowVerifyEvery.
func TestP22R2bWeeklyCadenceAndDueBoundary(t *testing.T) {
	if state.EscrowVerifyEvery != 7*24*time.Hour {
		t.Fatalf("EscrowVerifyEvery = %v, want 7 days", state.EscrowVerifyEvery)
	}
	now := t0
	id := "01JDEVICE00000000000000001"
	off := escrowVerifyOffset(id)
	ms := func(t time.Time) state.Millis { return state.Millis(t.UnixMilli()) }
	key := func(proof time.Time) state.EscrowKeyFacts {
		return state.EscrowKeyFacts{DeviceID: id, Escrow: state.EscrowFacts{Custody: "escrowed", VerifiedAt: ms(proof)}}
	}
	// Proof aged 7 d less the offset: due; one second less: not.
	if !escrowVerifyDue(key(now.Add(-(7*24*time.Hour - off))), now) {
		t.Error("not due at 7 days less the offset")
	}
	if escrowVerifyDue(key(now.Add(-(7*24*time.Hour - off - time.Second))), now) {
		t.Error("due one second before 7 days less the offset")
	}
	proven := now.Add(-3 * time.Hour).Truncate(time.Millisecond)
	if got, want := EscrowVerifyDueAt(id, proven), proven.Add(7*24*time.Hour-off); !got.Equal(want) {
		t.Errorf("EscrowVerifyDueAt = %v, want %v", got, want)
	}
	if EscrowVerifyRetry != time.Hour {
		t.Errorf("EscrowVerifyRetry = %v", EscrowVerifyRetry)
	}
}

// G3 (unit): only wrong_password and repo_missing are verdicts. A busy worker and a deadline are an
// attempt with a class that is not recorded as a failure (ErrorClass empty, not OK).
// Killed mutants: M3a (timeout -> ErrorClass), M3b (busy -> ErrorClass).
func TestP22R2bClassifyEscrowCheckTimeoutAndBusy(t *testing.T) {
	for _, c := range []struct {
		name      string
		err       error
		wantClass string
	}{
		{"busy", adminpath.NewError(adminpath.CodeBusy, ""), adminpath.CodeBusy},
		{"deadline", context.DeadlineExceeded, adminpath.CodeTimeout},
		{"wrapped deadline", errors.Join(errors.New("x"), context.DeadlineExceeded), adminpath.CodeTimeout},
		{"adminpath timeout", adminpath.NewError(adminpath.CodeTimeout, ""), adminpath.CodeTimeout},
		{"storage unreachable", adminpath.NewError(adminpath.CodeStorageUnreachable, ""), adminpath.CodeStorageUnreachable},
	} {
		v, class, skip := ClassifyEscrowCheck(c.err)
		if v.OK || v.ErrorClass != "" || skip || class != c.wantClass {
			t.Errorf("%s: %+v class %q skip %v, want not OK, no recorded failure, class %q", c.name, v, class, skip, c.wantClass)
		}
	}
	for _, code := range []string{adminpath.CodeWrongPassword, adminpath.CodeRepoMissing} {
		if v, class, _ := ClassifyEscrowCheck(adminpath.NewError(code, "")); v.OK || v.ErrorClass != code || class != code {
			t.Errorf("%s: %+v %q", code, v, class)
		}
	}
}

// G3 (scheduler): a cat config that outlasts Timeouts.CatConfig, and one that never gets a slot within
// schedulerSlotWait, record the attempt only: no failure class, the proof unmoved, ESCROW_UNVERIFIED
// kept by the stale proof, and no key is called wrong. Killed mutants: M3a, M3b.
func TestP22R2bTimeoutAndBusyRecordTheAttemptOnly(t *testing.T) {
	for _, mode := range []string{"timeout", "no slot"} {
		t.Run(mode, func(t *testing.T) {
			rules := []fakeRule{{Cmd: "cat", Stdout: "cat_config.json"}, {Cmd: "stats", Stdout: "stats_raw_data.json"}}
			if mode == "timeout" {
				rules[0] = fakeRule{Cmd: "cat", SleepMS: 20000, Stdout: "cat_config.json"}
			}
			e := newTenv(t, rules, func(c *Config) { c.Timeouts.CatConfig = 300 * time.Millisecond })
			dev1 := e.esc
			e.onlyDevice(dev1)
			e.provenNowExcept(dev1)
			stale := e.clk.Now().Add(-31 * 24 * time.Hour)
			e.setProof(dev1, stale, time.Time{})
			if mode == "no slot" {
				old := schedulerSlotWait
				schedulerSlotWait = 300 * time.Millisecond
				t.Cleanup(func() { schedulerSlotWait = old })
				for i := 0; i < cap(e.w.sem); i++ {
					e.w.sem <- struct{}{}
				}
				t.Cleanup(func() {
					for len(e.w.sem) > 0 {
						<-e.w.sem
					}
				})
			}
			rep, err := e.w.RunOnce(e.ctx)
			if err != nil || rep.EscrowVerified != 0 || rep.EscrowUnverified != 1 {
				t.Fatalf("pass = %+v, %v", rep, err)
			}
			esc := e.escrowOf(dev1)
			if esc.LastVerifyErrorClass != "" || esc.LastVerifyAttemptAt == nil || !esc.LastVerifyAttemptAt.Equal(e.clk.Now()) {
				t.Fatalf("recorded %+v: want the attempt only, no failure class", esc)
			}
			if esc.VerifiedAt == nil || !esc.VerifiedAt.Equal(stale) {
				t.Fatalf("the proof moved: %v", esc.VerifiedAt)
			}
			if !e.escrowFlagged(dev1) {
				t.Fatal("a stale proof reads green after an attempt that reached no verdict")
			}
			if l := e.logs.String(); !strings.Contains(l, "escrow verification failed") {
				t.Errorf("no warning line: %s", l)
			}
		})
	}
}

// L1: when the candidate set does not cover every confirmed escrowed device (store.ErrEscrowCandidates), the
// pass says so loudly (error line, class escrow_candidates_mismatch) and STILL verifies the candidates it
// has: each is a valid key, and stopping would leave all of them unchecked because of one that is missing.
// Killed mutant: the pass returns on the mismatch.
func TestP22R2bMismatchedCandidatesAreStillVerified(t *testing.T) {
	e := newTenv(t, []fakeRule{{Cmd: "cat", Stdout: "cat_config.json"}, {Cmd: "stats", Stdout: "stats_raw_data.json"}}, nil)
	a, b := e.esc, e.deviceID("TECH-BENCH-07")
	e.onlyDevice(a)
	e.provenNowExcept(a, b)
	old := e.clk.Now().Add(-8 * 24 * time.Hour)
	e.setProof(a, old, time.Time{})
	e.setProof(b, old, time.Time{})
	restore := store.ForceEscrowCandidateMismatch("d.id <> '" + b + "'")
	defer restore()
	rep, err := e.w.RunOnce(e.ctx)
	if err != nil || rep.EscrowVerified != 1 {
		t.Fatalf("pass = %+v, %v: want the one candidate found verified", rep, err)
	}
	if got := e.catDevices(); len(got) != 1 || got[0] != a {
		t.Fatalf("cat config ran for %v, want only %s", got, a)
	}
	if l := e.logs.String(); !strings.Contains(l, "escrow_candidates_mismatch") || !strings.Contains(l, `"level":"ERROR"`) {
		t.Fatalf("no loud mismatch line: %s", l)
	}
	if esc := e.escrowOf(b); esc.LastVerifyAttemptAt != nil {
		t.Fatalf("the missing candidate was touched: %+v", esc)
	}
}
