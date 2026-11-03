package maintain

import (
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// M2-FIX-B1 (lab finding F7): a device enrolled before enrollment recorded the
// escrow verification has verified_at NULL. The scheduler proves its key once
// with cat config and records it; ESCROW_UNVERIFIED clears.

func (e *tenv) catCalls() int {
	n := 0
	for _, c := range e.repoCalls() {
		if c.Args[0] == "cat" {
			n++
		}
	}
	return n
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

func TestFixB1SchedulerVerifiesANeverVerifiedEscrowOnce(t *testing.T) {
	e := newTenv(t, []fakeRule{
		{Cmd: "cat", Exit: 1, Stderr: "inline:x509: certificate signed by unknown authority\n"},
		{Cmd: "stats", Stdout: "stats_raw_data.json"},
	}, nil)
	dev := e.deviceID("TECH-BENCH-07") // escrowed, confirmed, enrolled 40 days ago, never verified
	e.onlyDevice(dev)
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
	// Verified once is enough: later passes do not run cat config for it.
	before = e.catCalls()
	e.clk.Advance(3 * time.Hour)
	if _, err := e.w.RunOnce(e.ctx); err != nil || e.catCalls() != before {
		t.Fatalf("a verified key was checked again: %d -> %d (%v)", before, e.catCalls(), err)
	}
	// No audit row for the verification.
	if n := e.auditCount(); n != auditBefore {
		t.Fatalf("audit rows %d -> %d: the verification is not audited", auditBefore, n)
	}
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

func TestFixB1SchedulerRecordsAWrongKey(t *testing.T) {
	e := newTenv(t, []fakeRule{
		{Cmd: "cat", Exit: 12, Stderr: "exit12_wrong_password_plain.stderr.txt"},
		{Cmd: "stats", Stdout: "stats_raw_data.json"},
	}, nil)
	dev := e.deviceID("TECH-BENCH-07")
	e.onlyDevice(dev)
	rep, err := e.w.RunOnce(e.ctx)
	if err != nil || rep.EscrowUnverified != 1 {
		t.Fatalf("report = %+v, %v", rep, err)
	}
	esc := e.escrowOf(dev)
	if esc.VerifiedAt != nil || esc.LastVerifyErrorClass != "wrong_password" {
		t.Fatalf("after a wrong key: %+v", esc)
	}
	f, _ := e.st.LoadDeviceFacts(e.ctx, store.ScopeAll(), dev)
	if got := state.Classify(f, e.clk.Now()); got.Primary != state.StateEscrowUnverified || got.Reason != state.ReasonEscrowVerifyFailed {
		t.Fatalf("state = %s %s, want ESCROW_UNVERIFIED escrow_verify_failed", got.Primary, got.Reason)
	}
}

func TestFixB1SchedulerLeavesVerifiedAndCustomerHeldKeysAlone(t *testing.T) {
	e := newTenv(t, []fakeRule{{Cmd: "cat", Stdout: "cat_config.json"}, {Cmd: "stats", Stdout: "stats_raw_data.json"}}, nil)
	e.onlyDevice(e.esc) // verified five days ago
	if rep, err := e.w.RunOnce(e.ctx); err != nil || rep.EscrowVerified != 0 || e.catCalls() != 0 {
		t.Fatalf("a verified key was checked: %+v, %v, %d cat calls", rep, err, e.catCalls())
	}
	e.onlyDevice(e.held)
	if rep, err := e.w.RunOnce(e.ctx); err != nil || rep.EscrowVerified != 0 || e.catCalls() != 0 {
		t.Fatalf("a customer-held device was checked: %+v, %v", rep, err)
	}
}
