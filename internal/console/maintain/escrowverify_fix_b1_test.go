package maintain

import (
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// The escrow verifier (M2-FIX-B1 for never-verified keys, ADR 0030 for the weekly
// recheck): every escrowed key is proven with cat config when it is due, and the
// outcome is recorded on its escrow row.

func (e *tenv) catCalls() int {
	n := 0
	for _, c := range e.repoCalls() {
		if c.Args[0] == "cat" {
			n++
		}
	}
	return n
}

// catDevices lists the devices cat config ran for, in order (the repository ends in the device id).
func (e *tenv) catDevices() []string {
	var out []string
	for _, c := range e.repoCalls() {
		if c.Args[0] == "cat" {
			out = append(out, c.Repo[strings.LastIndex(c.Repo, "/")+1:])
		}
	}
	return out
}

func (e *tenv) escrowOf(id string) store.Escrow {
	e.t.Helper()
	esc, err := e.st.ActiveEscrow(e.ctx, store.ScopeAll(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	return esc
}

func (e *tenv) escrowFlagged(id string) bool {
	e.t.Helper()
	f, err := e.st.LoadDeviceFacts(e.ctx, store.ScopeAll(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, s := range state.Classify(f, e.clk.Now()).Flags {
		if s == state.StateEscrowUnverified {
			return true
		}
	}
	return false
}

// provenNowExcept marks every active escrowed key proven now (no attempt on record) except
// the named devices', so a pass has exactly those keys to look at.
func (e *tenv) provenNowExcept(ids ...string) {
	e.t.Helper()
	now := e.clk.Now().UnixMilli()
	if err := e.st.Write(e.ctx, func(tx *store.Tx) error {
		q := `UPDATE repo_key_escrow SET verified_at = ?, last_verify_attempt_at = NULL, last_verify_error_class = NULL
			WHERE custody = 'escrowed' AND retired_at IS NULL`
		args := []any{now}
		for _, id := range ids {
			q += ` AND device_id <> ?`
			args = append(args, id)
		}
		_, err := tx.Exec(q, args...)
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
}

// setProof sets one key's proof and attempt times (zero: NULL) and clears its verdict.
func (e *tenv) setProof(id string, verified, attempt time.Time) {
	e.t.Helper()
	var v, a any
	if !verified.IsZero() {
		v = verified.UnixMilli()
	}
	if !attempt.IsZero() {
		a = attempt.UnixMilli()
	}
	if err := e.st.Write(e.ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE repo_key_escrow SET verified_at = ?, last_verify_attempt_at = ?, last_verify_error_class = NULL
			WHERE device_id = ? AND retired_at IS NULL`, v, a, id)
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
}

// anyDeviceID is deviceID for a device that may be retired.
func (e *tenv) anyDeviceID(host string) string {
	e.t.Helper()
	l, err := e.st.ListDevices(e.ctx, store.ScopeAll(), store.ListDevicesOptions{Search: host, IncludeRetired: true})
	if err != nil {
		e.t.Fatal(err)
	}
	for _, d := range l.Items {
		if d.Hostname == host {
			return d.ID
		}
	}
	e.t.Fatalf("device %s", host)
	return ""
}

func (e *tenv) auditCount() int {
	e.t.Helper()
	var n int
	if err := e.st.Read(e.ctx, func(tx *store.Tx) error {
		return tx.QueryRow(`SELECT COUNT(*) FROM audit_events`).Scan(&n)
	}); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func TestFixB1SchedulerVerifiesANeverVerifiedEscrow(t *testing.T) {
	e := newTenv(t, []fakeRule{
		{Cmd: "cat", Exit: 1, Stderr: "inline:x509: certificate signed by unknown authority\n"},
		{Cmd: "stats", Stdout: "stats_raw_data.json"},
	}, nil)
	dev := e.deviceID("TECH-BENCH-07") // escrowed, confirmed, enrolled 40 days ago, never verified
	e.onlyDevice(dev)
	e.provenNowExcept(dev)
	if !e.escrowFlagged(dev) || e.escrowOf(dev).VerifiedAt != nil {
		t.Fatal("precondition: the fixture device must be ESCROW_UNVERIFIED with verified_at NULL")
	}
	auditBefore := e.auditCount()

	// The storage is unreachable: the key is not proven, only the attempt is recorded.
	rep, err := e.w.RunOnce(e.ctx)
	if err != nil || rep.EscrowVerified != 0 || rep.EscrowUnverified != 1 || rep.Failed != 0 {
		t.Fatalf("report = %+v, %v", rep, err)
	}
	esc := e.escrowOf(dev)
	if esc.VerifiedAt != nil || esc.LastVerifyErrorClass != "" || esc.LastVerifyAttemptAt == nil || !esc.LastVerifyAttemptAt.Equal(e.clk.Now()) {
		t.Fatalf("after an unreachable attempt: %+v", esc)
	}
	if !e.escrowFlagged(dev) {
		t.Fatal("an unproven key must keep ESCROW_UNVERIFIED")
	}
	// Inside the retry hour nothing runs again.
	e.clk.Advance(10 * time.Minute)
	before := e.catCalls()
	if _, err := e.w.RunOnce(e.ctx); err != nil || e.catCalls() != before {
		t.Fatalf("cat config retried inside the hour: %d -> %d (%v)", before, e.catCalls(), err)
	}

	// The storage is back: the key opens the repository, verified_at is set, the flag clears.
	e.scenario([]fakeRule{{Cmd: "cat", Stdout: "cat_config.json"}, {Cmd: "stats", Stdout: "stats_raw_data.json"}})
	e.clk.Advance(time.Hour)
	rep, err = e.w.RunOnce(e.ctx)
	if err != nil || rep.EscrowVerified != 1 || rep.EscrowUnverified != 0 {
		t.Fatalf("report = %+v, %v", rep, err)
	}
	esc = e.escrowOf(dev)
	if esc.VerifiedAt == nil || !esc.VerifiedAt.Equal(e.clk.Now()) || esc.LastVerifyErrorClass != "" {
		t.Fatalf("after a successful cat config: %+v", esc)
	}
	if e.escrowFlagged(dev) {
		t.Fatal("ESCROW_UNVERIFIED after the scheduler verified the key")
	}
	// No audit row for the scheduler's verification.
	if n := e.auditCount(); n != auditBefore {
		t.Fatalf("audit rows %d -> %d: the verification is not audited", auditBefore, n)
	}
}

// ADR 0030: a proven key is checked again once a week (a little earlier per device), not before.
func TestP22HSchedulerRechecksAProvenKeyWeekly(t *testing.T) {
	e := newTenv(t, []fakeRule{{Cmd: "cat", Stdout: "cat_config.json"}, {Cmd: "stats", Stdout: "stats_raw_data.json"}}, nil)
	dev := e.esc
	e.onlyDevice(dev)
	e.provenNowExcept()
	proven := e.clk.Now()
	off := escrowVerifyOffset(dev)
	// Just before its due time: not checked (every other key proven at the same moment as it).
	e.clk.Advance(state.EscrowVerifyEvery - off - time.Minute)
	e.provenNowExcept(dev)
	if rep, err := e.w.RunOnce(e.ctx); err != nil || e.catCalls() != 0 || rep.EscrowVerified != 0 {
		t.Fatalf("checked before its due time: %+v, %v, %d cat calls", rep, err, e.catCalls())
	}
	if esc := e.escrowOf(dev); !esc.VerifiedAt.Equal(proven) {
		t.Fatalf("precondition: %+v", esc)
	}
	// At its due time: checked, and proven again.
	e.clk.Advance(time.Minute)
	e.provenNowExcept(dev)
	rep, err := e.w.RunOnce(e.ctx)
	if err != nil || rep.EscrowVerified != 1 || e.catCalls() != 1 {
		t.Fatalf("due key: %+v, %v, %d cat calls", rep, err, e.catCalls())
	}
	if esc := e.escrowOf(dev); esc.VerifiedAt == nil || !esc.VerifiedAt.Equal(e.clk.Now()) {
		t.Fatalf("the recheck did not record its proof: %+v", esc)
	}
	// Proven now: not due again for another week.
	e.clk.Advance(3 * time.Hour)
	e.provenNowExcept(dev)
	if _, err := e.w.RunOnce(e.ctx); err != nil || e.catCalls() != 1 {
		t.Fatalf("a freshly proven key was checked again: %d cat calls (%v)", e.catCalls(), err)
	}
}

// ADR 0030: a retired device is a candidate (maintenance or not), its failure is recorded, and its
// own row stays quiet (RETIRED); purging, unconfirmed and customer-held devices are never checked.
func TestP22HSchedulerVerifiesRetiredDevicesNotPurgingOnes(t *testing.T) {
	e := newTenv(t, []fakeRule{{Cmd: "cat", Exit: 10, Stderr: "exit10_repo_missing.stderr.jsonl"}, {Cmd: "stats", Stdout: "stats_raw_data.json"}}, nil)
	retired, purging, unconfirmed := e.anyDeviceID("OLD-RECEPTION-PC"), e.anyDeviceID("SPARE-PC-09"), e.deviceID("KIOSK-WAIT-01")
	e.onlyDevice(e.esc)
	e.provenNowExcept(retired, purging, unconfirmed)
	old := e.clk.Now().Add(-30 * 24 * time.Hour)
	for _, id := range []string{retired, purging, unconfirmed} {
		e.setProof(id, old, time.Time{})
	}
	rep, err := e.w.RunOnce(e.ctx)
	if err != nil || rep.EscrowUnverified != 1 || rep.EscrowVerified != 0 {
		t.Fatalf("report = %+v, %v", rep, err)
	}
	if got := e.catDevices(); len(got) != 1 || got[0] != retired {
		t.Fatalf("cat config ran for %v, want only the retired device %s", got, retired)
	}
	if esc := e.escrowOf(retired); esc.LastVerifyErrorClass != "repo_missing" {
		t.Fatalf("the retired device's failure is not recorded: %+v", esc)
	}
	f, _ := e.st.LoadDeviceFacts(e.ctx, store.ScopeAll(), retired)
	if ds := state.Classify(f, e.clk.Now()); ds.Primary != state.StateRetired || ds.Alarm != state.SeverityNone {
		t.Fatalf("the retired device alarms on its own row: %+v", ds)
	}
	keys, err := e.st.EscrowVerifyKeys(e.ctx, store.ScopeAll())
	if err != nil {
		t.Fatal(err)
	}
	if n := state.SummarizeEscrowVerification(&state.EscrowVerifyFacts{Keys: keys}, e.clk.Now()).RetiredFailing; n != 1 {
		t.Fatalf("retired failing = %d, want 1 (banner C22's count)", n)
	}
}

// The classes: success proves, wrong_password and repo_missing are failures, anything else is an
// attempt only; and a proof older than the recheck stays loud however the attempts go.
func TestP22HSchedulerFailureVersusAttemptClasses(t *testing.T) {
	for _, c := range []struct {
		name      string
		rule      fakeRule
		wantClass string // recorded last_verify_error_class
		proven    bool
	}{
		{"success", fakeRule{Cmd: "cat", Stdout: "cat_config.json"}, "", true},
		{"wrong_password", fakeRule{Cmd: "cat", Exit: 12, Stderr: "exit12_wrong_password_plain.stderr.txt"}, "wrong_password", false},
		{"repo_missing", fakeRule{Cmd: "cat", Exit: 10, Stderr: "exit10_repo_missing.stderr.jsonl"}, "repo_missing", false},
		{"storage unreachable", fakeRule{Cmd: "cat", Exit: 1, Stderr: "inline:dial tcp: lookup storage.example.invalid: no such host\n"}, "", false},
		{"tls", fakeRule{Cmd: "cat", Exit: 1, Stderr: "inline:x509: certificate signed by unknown authority\n"}, "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newTenv(t, []fakeRule{c.rule, {Cmd: "stats", Stdout: "stats_raw_data.json"}}, nil)
			dev := e.esc
			e.onlyDevice(dev)
			e.provenNowExcept(dev)
			stale := e.clk.Now().Add(-31 * 24 * time.Hour) // the verifier has not proven it for a month
			e.setProof(dev, stale, time.Time{})
			if !e.escrowFlagged(dev) {
				t.Fatal("precondition: a 31-day-old proof is ESCROW_UNVERIFIED (recheck enforced)")
			}
			rep, err := e.w.RunOnce(e.ctx)
			if err != nil || rep.EscrowVerified+rep.EscrowUnverified != 1 {
				t.Fatalf("report = %+v, %v", rep, err)
			}
			esc := e.escrowOf(dev)
			if esc.LastVerifyErrorClass != c.wantClass || esc.LastVerifyAttemptAt == nil || !esc.LastVerifyAttemptAt.Equal(e.clk.Now()) {
				t.Fatalf("recorded %+v, want class %q and the attempt", esc, c.wantClass)
			}
			if c.proven != (esc.VerifiedAt != nil && esc.VerifiedAt.Equal(e.clk.Now())) {
				t.Fatalf("proof = %v, want proven %v", esc.VerifiedAt, c.proven)
			}
			if !c.proven && (esc.VerifiedAt == nil || !esc.VerifiedAt.Equal(stale)) {
				t.Fatalf("a failed check moved the proof: %v", esc.VerifiedAt)
			}
			// Never green when stale: only a proof clears the flag.
			if e.escrowFlagged(dev) == c.proven {
				t.Fatalf("flagged = %v after %s", e.escrowFlagged(dev), c.name)
			}
			// Path-free log lines: no repository, bucket or prefix.
			if l := e.logs.String(); strings.Contains(l, "test-bucket") || strings.Contains(l, "rclone:") {
				t.Fatalf("a log line carries the repository: %s", l)
			}
		})
	}
}

// Only the scheduler lease holder verifies: a pass that finds the lease held elsewhere runs no
// cat config and records nothing.
func TestP22HOnlyTheLeaseHolderVerifies(t *testing.T) {
	e := newTenv(t, []fakeRule{{Cmd: "cat", Stdout: "cat_config.json"}, {Cmd: "stats", Stdout: "stats_raw_data.json"}}, nil)
	dev := e.deviceID("TECH-BENCH-07")
	e.provenNowExcept(dev)
	if got, err := e.st.AcquireJobLease(e.ctx, SchedulerJob, "another-console", time.Hour); err != nil || !got {
		t.Fatalf("take the lease: %v %v", got, err)
	}
	rep, err := e.w.RunOnce(e.ctx)
	if err != nil || !rep.Skipped || e.catCalls() != 0 || rep.EscrowVerified != 0 {
		t.Fatalf("a pass without the lease: %+v, %v, %d cat calls", rep, err, e.catCalls())
	}
	if esc := e.escrowOf(dev); esc.VerifiedAt != nil || esc.LastVerifyAttemptAt != nil {
		t.Fatalf("a pass without the lease recorded: %+v", esc)
	}
}

// noMasterKey is a key unwrapper that reports its master key missing.
type noMasterKey struct{ *fakeKeys }

func (noMasterKey) MasterKeyLoaded() bool { return false }

// A verifier that cannot run (no master key) runs nothing, records nothing and says so at error
// level; the keys then age into ESCROW_UNVERIFIED, never a stale green.
func TestP22HVerifierWithoutMasterKeyIsLoud(t *testing.T) {
	e := newTenv(t, []fakeRule{{Cmd: "cat", Stdout: "cat_config.json"}, {Cmd: "stats", Stdout: "stats_raw_data.json"}}, nil)
	dev := e.esc
	e.onlyDevice(dev)
	e.provenNowExcept(dev)
	e.setProof(dev, e.clk.Now().Add(-31*24*time.Hour), time.Time{})
	e.w.keys = noMasterKey{e.keys}
	rep, err := e.w.RunOnce(e.ctx)
	if err != nil || e.catCalls() != 0 || rep.EscrowVerified != 0 || rep.EscrowDeferred != 1 {
		t.Fatalf("report = %+v, %v, %d cat calls", rep, err, e.catCalls())
	}
	if esc := e.escrowOf(dev); esc.LastVerifyAttemptAt != nil {
		t.Fatalf("an attempt was recorded without a key: %+v", esc)
	}
	if l := e.logs.String(); !strings.Contains(l, "escrow verification cannot run") || !strings.Contains(l, "master_key_unavailable") {
		t.Fatalf("no error line: %s", l)
	}
	if !e.escrowFlagged(dev) {
		t.Fatal("a stale proof reads green while the verifier cannot run")
	}
}

func TestFixB1SchedulerLeavesCustomerHeldKeysAlone(t *testing.T) {
	e := newTenv(t, []fakeRule{{Cmd: "cat", Stdout: "cat_config.json"}, {Cmd: "stats", Stdout: "stats_raw_data.json"}}, nil)
	e.onlyDevice(e.held)
	e.provenNowExcept()
	if rep, err := e.w.RunOnce(e.ctx); err != nil || rep.EscrowVerified != 0 || e.catCalls() != 0 {
		t.Fatalf("a customer-held device was checked: %+v, %v", rep, err)
	}
	for _, d := range e.catDevices() {
		if d == e.held {
			t.Fatal("cat config ran for the customer-held device")
		}
	}
}
