package maintain

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// unverifyAll clears verified_at and the attempt on every candidate key and
// returns how many keys the verifier now has due.
func (e *tenv) unverifyAll() int {
	e.t.Helper()
	if err := e.st.Write(e.ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE repo_key_escrow SET verified_at = NULL, last_verify_attempt_at = NULL WHERE custody = 'escrowed' AND retired_at IS NULL`)
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	keys, err := e.st.EscrowVerifyKeys(e.ctx, store.ScopeAll())
	if err != nil {
		e.t.Fatal(err)
	}
	n := 0
	for _, k := range keys {
		if escrowVerifyDue(k, e.clk.Now()) {
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
	keys, _ := e.st.EscrowVerifyKeys(e.ctx, store.ScopeAll())
	// Three keys had an attempt two hours ago (due again); every other one was never attempted.
	tried := map[string]bool{}
	for _, k := range keys[:3] {
		tried[k.DeviceID] = true
		e.setProof(k.DeviceID, time.Time{}, e.clk.Now().Add(-2*time.Hour))
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

// Due and stagger (ADR 0030): never proven is due; a proof is due at EscrowVerifyEvery less the
// device's stable offset, which spreads a fleet proven on one pass over a day; an attempt within
// the retry hour holds the key back, a garbled or far-future one does not.
func TestP22HEscrowDueAndStagger(t *testing.T) {
	now := t0
	ms := func(t time.Time) state.Millis { return state.Millis(t.UnixMilli()) }
	key := func(id string, verified, attempt time.Time) state.EscrowKeyFacts {
		k := state.EscrowKeyFacts{DeviceID: id}
		if !verified.IsZero() {
			k.Escrow.VerifiedAt = ms(verified)
		}
		if !attempt.IsZero() {
			k.LastAttemptAt = ms(attempt)
		}
		return k
	}
	id := "01JDEVICE00000000000000001"
	off := escrowVerifyOffset(id)
	if off < 0 || off >= escrowVerifyStagger || off != escrowVerifyOffset(id) {
		t.Fatalf("offset %v is not stable in [0, %v)", off, escrowVerifyStagger)
	}
	for _, c := range []struct {
		name string
		k    state.EscrowKeyFacts
		want bool
	}{
		{"never proven, never attempted", key(id, time.Time{}, time.Time{}), true},
		{"never proven, attempted 59 min ago", key(id, time.Time{}, now.Add(-59*time.Minute)), false},
		{"never proven, attempted an hour ago", key(id, time.Time{}, now.Add(-time.Hour)), true},
		{"proven just inside its cadence", key(id, now.Add(-(state.EscrowVerifyEvery - off - time.Second)), time.Time{}), false},
		{"proven at its due time", key(id, now.Add(-(state.EscrowVerifyEvery - off)), time.Time{}), true},
		{"proven 8 days ago, attempted 10 min ago", key(id, now.Add(-8*24*time.Hour), now.Add(-10*time.Minute)), false},
		{"garbled proof", state.EscrowKeyFacts{DeviceID: id, Escrow: state.EscrowFacts{VerifiedAt: 42}}, true},
		{"attempt a day in the future", key(id, time.Time{}, now.Add(24*time.Hour)), true},
	} {
		if got := escrowVerifyDue(c.k, now); got != c.want {
			t.Errorf("%s: due %v, want %v", c.name, got, c.want)
		}
	}
	// A fleet proven on one pass comes due over the stagger window, not on one pass.
	hours := map[time.Duration]bool{}
	for i := 0; i < 200; i++ {
		hours[escrowVerifyOffset(fmt.Sprintf("01JDEV%020d", i)).Truncate(time.Hour)] = true
	}
	if len(hours) < 20 {
		t.Fatalf("200 devices fall in %d of 24 hour buckets: the stagger does not spread them", len(hours))
	}
	// Order: never proven (never attempted first), then the oldest proof, then device id.
	due := []state.EscrowKeyFacts{
		key("d-proven-new", now.Add(-7*24*time.Hour), time.Time{}),
		key("d-never-tried", time.Time{}, now.Add(-2*time.Hour)),
		key("d-proven-old", now.Add(-9*24*time.Hour), time.Time{}),
		key("d-never-b", time.Time{}, time.Time{}),
		key("d-never-a", time.Time{}, time.Time{}),
	}
	escrowVerifyOrder(due)
	var got []string
	for _, k := range due {
		got = append(got, k.DeviceID)
	}
	want := []string{"d-never-a", "d-never-b", "d-never-tried", "d-proven-old", "d-proven-new"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order %v, want %v", got, want)
		}
	}
}
