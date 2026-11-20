package notify

import (
	"fmt"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/mail"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// The invariant probe of P20-W2BF C1 and D3, in the style of the cell tables of the earlier fix
// rounds: every combination of how A's first send goes, the shape of B's row of the batch, a mute
// of dev0 (acknowledgement, snooze, suspended customer), when it comes, whether it ends, and when
// dev0 closes. dev1 fails throughout and is never muted by an acknowledgement, so every cell that is
// not suspended before the pass forms a batch. The relay accepts everything after the first pass
// (B's in-flight send in the inflightFail cells excepted), so at least one recipient's relay accepts
// mail.
//
// B's row shapes, each made the way the product makes it (retry_c1_test.go): sent (ok), failed
// with a retry (temp), failed for good (perm), in flight past A's retry and then sent (inflightOK)
// or failed with a retry (inflightFail), deferred because A's send used up the pass's send budget
// (deferred), and both rows left queued by a pass that stopped between its marks and its sends, a
// restart there (restart; A's first send does not happen, so a is "-").
//
// Over every cell:
//  1. an alarm open and unmuted at the end has at least one delivered alert;
//  2. no recipient is alerted twice about one episode, except after a snooze that lapsed while the
//     alarm persisted (ADR 0020 decision 7 re-raises it), and none gets two recoveries;
//  3. no recovery names dev0 when no delivered alert ever named it, unless the cell is in the
//     residual class of the package doc (probeCell.residual): there it is counted as residual,
//     never as a violation, and outside it it is a violation, so no such outcome goes unclassified;
//  4. an alarm muted before the pass and never unmuted is never alerted (dev0; dev1 too when the
//     mute is a suspended customer).
//
// Cells (P20-W2BF D1): the full matrix, 741 cells ((2 x 6 + 1) row shapes x 19 mute cases x 3
// close times), runs in a build without the race detector (about 0.4 s a cell). A race build takes
// about 4 s a cell, so the whole matrix would not fit the race gate's timeout: it runs the fixed
// subset probeCover chooses, 46 cells (5 of them residual), which hits every interleaving class at
// least once. With -short either build runs the smaller subset of 39 cells. The switch is the
// build-tagged constant raceBuild (probe_race_m4_test.go, probe_norace_m4_test.go); the counts are
// asserted, so a change to the matrix that moves them fails until this comment says so.
const (
	probeFullCells  = 741
	probeRaceCells  = 46
	probeShortCells = 39
)

type probeCell struct {
	aFirst, bFirst, mute, muteAt, unmute, close string
}

func (c probeCell) String() string {
	return fmt.Sprintf("a=%s b=%s mute=%s at=%s unmute=%s close=%s", c.aFirst, c.bFirst, c.mute, c.muteAt, c.unmute, c.close)
}

// inBatch reports whether dev0 is in the batch: it was not muted when the batch formed.
func (c probeCell) inBatch() bool { return c.mute == "none" || c.muteAt != "beforePass" }

// residual reports whether the cell is in the residual class the package doc accepts: dev0 closes
// while its alert is still undelivered to every recipient and at least one row of the batch is
// still queued and has never been attempted. Then each recipient with the recoveries switch can
// get a "resolved" for dev0 although nobody was mailed it. The three ways in:
//   - restart: both rows stay queued until the retry job takes them (store.QueuedStaleAfter), so a
//     close before or after the first retry run falls inside;
//   - inflightOK, inflightFail, deferred: B's row is queued, never attempted, while A's first send
//     failed (A delivered dev0 otherwise) and dev0 closes before A's retry delivers it.
func (c probeCell) residual() bool {
	if !c.inBatch() {
		return false
	}
	switch c.bFirst {
	case "restart":
		return c.close != "never"
	case "inflightOK", "inflightFail", "deferred":
		return c.aFirst == "temp" && c.close == "beforeRetry"
	}
	return false
}

// probeCells is the full matrix, in a fixed order.
func probeCells() []probeCell {
	var cells []probeCell
	for _, b := range []string{"ok", "temp", "perm", "inflightOK", "inflightFail", "deferred", "restart"} {
		as := []string{"ok", "temp"}
		if b == "restart" {
			as = []string{"-"}
		}
		for _, a := range as {
			for _, cl := range []string{"never", "beforeRetry", "afterRetry"} {
				cells = append(cells, probeCell{a, b, "none", "-", "-", cl})
				for _, m := range []string{"ack", "snooze", "suspend"} {
					for _, at := range []string{"beforePass", "beforeRetry", "afterRetry"} {
						for _, u := range []string{"no", "yes"} {
							cells = append(cells, probeCell{a, b, m, at, u, cl})
						}
					}
				}
			}
		}
	}
	return cells
}

// probeCover is the subset of cells the race and short runs take: the greedy cover, in cell order
// (the first cell wins a tie), of these classes, each of which some cell of the subset hits:
//   - every (A's first send, B's row shape, close time) with dev0 in the batch: every interleaving
//     of the sends, the in-flight window, the deferral, the restart and the close, including every
//     residual way in;
//
// and, unless short:
//   - every (mute kind, mute time, unmute): every mute interleaving, including invariant 4's cells;
//   - every pair of values of any two of the six dimensions.
func probeCover(cells []probeCell, short bool) []probeCell {
	classes := func(c probeCell) []string {
		var ks []string
		if c.inBatch() {
			ks = append(ks, "abc/"+c.aFirst+"/"+c.bFirst+"/"+c.close)
		}
		if short {
			return ks
		}
		ks = append(ks, "mute/"+c.mute+"/"+c.muteAt+"/"+c.unmute)
		dims := []string{c.aFirst, c.bFirst, c.mute, c.muteAt, c.unmute, c.close}
		for i := range dims {
			for j := i + 1; j < len(dims); j++ {
				ks = append(ks, fmt.Sprintf("pair/%d=%s/%d=%s", i, dims[i], j, dims[j]))
			}
		}
		return ks
	}
	todo := map[string]bool{}
	for _, c := range cells {
		for _, k := range classes(c) {
			todo[k] = true
		}
	}
	var out []probeCell
	taken := make([]bool, len(cells))
	for len(todo) > 0 {
		best, gain := -1, 0
		for i, c := range cells {
			if taken[i] {
				continue
			}
			g := 0
			for _, k := range classes(c) {
				if todo[k] {
					g++
				}
			}
			if g > gain {
				best, gain = i, g
			}
		}
		taken[best] = true
		for _, k := range classes(cells[best]) {
			delete(todo, k)
		}
		out = append(out, cells[best])
	}
	return out
}

func TestC1InvariantProbe(t *testing.T) {
	cells := probeCells()
	if len(cells) != probeFullCells {
		t.Fatalf("the full matrix has %d cells; its doc comment says %d", len(cells), probeFullCells)
	}
	mode, want := "full", probeFullCells
	switch {
	case testing.Short():
		mode, want = "short", probeShortCells
		cells = probeCover(cells, true)
	case raceBuild:
		mode, want = "race subset", probeRaceCells
		cells = probeCover(cells, false)
	}
	if len(cells) != want {
		t.Fatalf("the %s run has %d cells; its doc comment says %d", mode, len(cells), want)
	}
	violations, residuals, spurious := 0, 0, 0
	for _, c := range cells {
		if c.residual() {
			residuals++
		}
		t.Run(c.String(), func(t *testing.T) {
			vs, sp := runProbeCell(t, c)
			if sp {
				spurious++
			}
			for _, v := range vs {
				violations++
				t.Errorf("%s: %s", c, v)
			}
		})
	}
	t.Logf("C1 invariant probe (%s): %d cells, %d violations, %d residual cells, %d of them with a spurious recovery",
		mode, len(cells), violations, residuals, spurious)
}

// runProbeCell plays one cell and returns its violations, and whether it ended with a recovery
// naming dev0 that no delivered alert named (always a residual cell when there is no violation).
func runProbeCell(t *testing.T, c probeCell) ([]string, bool) {
	t.Helper()
	w := newWorld(t)
	w.recipient(addrA, true, true)
	w.recipient(addrB, true, true)
	site := w.site("Summit Eye Care", 2)
	dev0, cust := site[0], w.cust["Summit Eye Care"]
	snoozeFor := 24 * time.Hour
	if c.unmute == "yes" {
		snoozeFor = 3 * time.Minute
	}
	muted := false
	mute := func() {
		switch c.mute {
		case "ack":
			if w.states[dev0] == state.StateOK {
				return // nothing open to acknowledge
			}
			w.ackDevice(dev0)
		case "snooze":
			if w.states[dev0] == state.StateOK {
				return
			}
			w.snooze(dev0, snoozeFor)
		case "suspend":
			w.muted[cust] = true
		}
		muted = true
	}
	unmute := func() {
		if !muted || c.unmute != "yes" {
			return
		}
		switch c.mute {
		case "ack":
			w.clearAcks()
		case "snooze":
			w.clk.Advance(snoozeFor + time.Minute) // the snooze lapses
		case "suspend":
			w.muted[cust] = false
		}
		muted = false
	}
	perm := &mail.SendError{Stage: mail.StageRecipient, Class: mail.ClassRecipientRejected}

	if c.muteAt == "beforePass" {
		w.set(state.StateFailed, dev0) // an acknowledgement needs an open episode
		w.n.Configured = false
		w.pass()
		w.n.Configured = true
		mute()
	}
	w.fake.FailWhen(func(m mail.Message) *mail.SendError {
		switch {
		case m.To[0] == addrA && c.aFirst == "temp":
			return temporary()
		case m.To[0] == addrB && c.bFirst == "perm":
			return perm
		case m.To[0] == addrB && c.bFirst == "temp":
			return temporary()
		}
		return nil
	})
	w.set(state.StateFailed, site...)
	var f *inFlight
	inflight := false
	switch c.bFirst {
	case "inflightOK", "inflightFail":
		// Not in flight when the whole batch was muted at formation (a suspended customer): no row.
		f, inflight = w.passInFlight(addrB)
		if !inflight {
			f.finish()
		}
	case "deferred":
		w.passDeferring(addrA)
	case "restart":
		w.passStoppingBeforeSend()
	default:
		w.pass()
	}
	w.fake.FailWhen(nil)
	if c.muteAt == "beforeRetry" {
		mute()
	}
	if c.close == "beforeRetry" {
		w.set(state.StateOK, dev0)
		w.pass() // beside the pass in flight, when there is one
	}
	w.clk.Advance(time.Minute)
	w.retry()
	if inflight {
		if c.bFirst == "inflightFail" {
			w.fake.FailWhen(func(mail.Message) *mail.SendError { return temporary() })
		}
		f.finish()
		w.fake.FailWhen(nil)
	}
	if c.muteAt == "afterRetry" {
		mute()
	}
	if c.close == "afterRetry" {
		w.set(state.StateOK, dev0)
	}
	w.clk.Advance(time.Minute)
	w.pass()
	unmute()
	for i := 0; i < 4; i++ { // past the whole retry schedule (1, 5 and 15 minutes) and QueuedStaleAfter
		w.clk.Advance(6 * time.Minute)
		w.pass()
		w.retry()
	}

	var out []string
	h0, h1 := w.host[dev0], w.host[site[1]]
	open0 := c.close == "never"
	unmutedAtEnd := c.mute == "none" || !muted
	sum := func(kind mail.Kind, h string) int { return w.count(addrA, kind, h) + w.count(addrB, kind, h) }
	if open0 && unmutedAtEnd && sum(mail.KindAlert, h0) == 0 {
		out = append(out, "(1) dev0 is open and unmuted, and no alert naming it was delivered")
	}
	dev1Muted := c.mute == "suspend" && muted // a suspended customer mutes dev1 too
	if !dev1Muted && sum(mail.KindAlert, h1) == 0 {
		out = append(out, "(1) dev1 is open and unmuted, and no alert naming it was delivered")
	}
	maxAlerts := 1
	if c.mute == "snooze" && c.unmute == "yes" {
		maxAlerts = 2 // the re-raise after a lapsed snooze (ADR 0020 decision 7)
	}
	for _, addr := range []string{addrA, addrB} {
		if n := w.count(addr, mail.KindAlert, h0); n > maxAlerts {
			out = append(out, fmt.Sprintf("(2) %s alerted %d times about dev0", addr, n))
		}
		if n := w.count(addr, mail.KindAlert, h1); n > 1 {
			out = append(out, fmt.Sprintf("(2) %s alerted %d times about dev1", addr, n))
		}
		if n := w.count(addr, mail.KindRecovery, h0); n > 1 {
			out = append(out, fmt.Sprintf("(2) %s got %d recoveries for dev0", addr, n))
		}
	}
	spurious := sum(mail.KindRecovery, h0) > 0 && sum(mail.KindAlert, h0) == 0
	if spurious && !c.residual() {
		out = append(out, "(3) a recovery names dev0, no delivered alert ever named it, and the cell is outside the residual class")
	}
	if c.muteAt == "beforePass" && c.unmute == "no" {
		if n := sum(mail.KindAlert, h0); n > 0 {
			out = append(out, fmt.Sprintf("(4) dev0 was muted before the pass and never unmuted, and was alerted %d times", n))
		}
		if n := sum(mail.KindAlert, h1); c.mute == "suspend" && n > 0 {
			out = append(out, fmt.Sprintf("(4) dev1's customer was suspended before the pass and never resumed, and dev1 was alerted %d times", n))
		}
	}
	return out, spurious
}
