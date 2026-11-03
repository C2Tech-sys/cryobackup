package maintain

import (
	"sync"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// unverifyAll clears verified_at on every active escrowed key and returns how
// many scheduler candidates now have a never-verified key.
func (e *tenv) unverifyAll() int {
	e.t.Helper()
	if err := e.st.Write(e.ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE repo_key_escrow SET verified_at = NULL, last_verify_attempt_at = NULL WHERE custody = 'escrowed' AND retired_at IS NULL`)
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	cands, err := e.w.candidates(e.ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	n := 0
	for _, c := range cands {
		if c.escrowUnverified {
			n++
		}
	}
	return n
}

// The escrow phase of a pass starts at most escrowVerifyPerPass checks,
// never-attempted keys first, then the oldest attempt; the rest wait.
func TestFixB1EscrowPhaseIsBoundedPerPass(t *testing.T) {
	e := newTenv(t, []fakeRule{{Cmd: "cat", Stdout: "cat_config.json"}, {Cmd: "stats", Stdout: "stats_raw_data.json"}}, nil)
	due := e.unverifyAll()
	if due < escrowVerifyPerPass+3 {
		t.Fatalf("the fixtures give %d never-verified candidates; the test needs at least %d", due, escrowVerifyPerPass+3)
	}
	cands, _ := e.w.candidates(e.ctx)
	// Three keys had an attempt two hours ago (due again); every other one was never attempted.
	tried := map[string]bool{}
	for _, c := range cands[:3] {
		tried[c.deviceID] = true
		if err := e.st.Write(e.ctx, func(tx *store.Tx) error {
			_, err := tx.Exec(`UPDATE repo_key_escrow SET last_verify_attempt_at = ? WHERE device_id = ? AND retired_at IS NULL`,
				e.clk.Now().Add(-2*time.Hour).UnixMilli(), c.deviceID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}

	rep, err := e.w.RunOnce(e.ctx)
	if err != nil || rep.EscrowVerified != escrowVerifyPerPass || rep.EscrowDeferred != due-escrowVerifyPerPass {
		t.Fatalf("first pass: %+v, %v (due %d)", rep, err, due)
	}
	if e.catCalls() != escrowVerifyPerPass {
		t.Fatalf("cat config ran %d times, want %d", e.catCalls(), escrowVerifyPerPass)
	}
	for id := range tried {
		if e.escrowOf(id).VerifiedAt != nil {
			t.Fatal("a key with an earlier attempt was verified before the never-attempted ones")
		}
	}
	// Later passes work through the rest; nothing is checked twice.
	verified := rep.EscrowVerified
	for pass := 0; pass < due && verified < due; pass++ {
		e.clk.Advance(time.Minute)
		rep, err = e.w.RunOnce(e.ctx)
		if err != nil || rep.EscrowVerified > escrowVerifyPerPass {
			t.Fatalf("pass %d: %+v, %v", pass, rep, err)
		}
		verified += rep.EscrowVerified
	}
	if verified != due || e.catCalls() != due {
		t.Fatalf("verified %d with %d cat calls, want %d", verified, e.catCalls(), due)
	}
}

// The escrow phase starts no check after escrowVerifyBudget, so a blackholed
// storage endpoint cannot keep a pass running past the scheduler lease.
func TestFixB1EscrowPhaseStopsAtTheTimeBudget(t *testing.T) {
	e := newTenv(t, []fakeRule{{Cmd: "cat", Stdout: "cat_config.json"}, {Cmd: "stats", Stdout: "stats_raw_data.json"}}, nil)
	due := e.unverifyAll()
	// Every read of the worker's clock is 50 s later than the one before (a slow storage endpoint).
	var mu sync.Mutex
	reads := 0
	e.w.clock = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		reads++
		return e.clk.Now().Add(time.Duration(reads) * 50 * time.Second)
	}
	rep, err := e.w.RunOnce(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.EscrowVerified < 1 || rep.EscrowVerified >= escrowVerifyPerPass || rep.EscrowVerified+rep.EscrowDeferred != due {
		t.Fatalf("with a 2 min budget and 50 s per clock read: %+v (due %d)", rep, due)
	}
}
