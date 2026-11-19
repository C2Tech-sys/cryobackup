package notify

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/mail"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// The invariant probe of P20-W2BF C1, in the style of the cell tables of the earlier fix rounds:
// every combination of how each recipient's first send goes, whether another recipient's send is
// still in flight when a retry runs, a mute of dev0 (acknowledgement, snooze, suspended customer),
// when it comes, whether it ends, and when dev0 closes. dev1 fails throughout and is never muted,
// so every cell forms a batch. The relay accepts everything after the first pass (B's in-flight
// send in the inflightFail cells excepted), so at least one recipient's relay accepts mail.
//
// Over every cell:
//  1. an alarm open and unmuted at the end has at least one delivered alert;
//  2. no recipient is alerted twice about one episode, except after a snooze that lapsed while the
//     alarm persisted (ADR 0020 decision 7 re-raises it), and none gets two recoveries;
//  3. no recovery names dev0 when no delivered alert ever named it, except in the residual cells the
//     package doc names (B's in-flight send fails after A's retry left dev0 out because it closed).

type probeCell struct {
	aFirst, bFirst, mute, muteAt, unmute, close string
}

func (c probeCell) String() string {
	return fmt.Sprintf("a=%s b=%s mute=%s at=%s unmute=%s close=%s", c.aFirst, c.bFirst, c.mute, c.muteAt, c.unmute, c.close)
}

// residual reports whether the cell is the accepted residual case: dev0 was in the batch (not
// muted when it formed), B's send was in flight and then failed, and dev0 closed before A's retry.
func (c probeCell) residual() bool {
	inBatch := c.mute == "none" || c.muteAt != "beforePass"
	return inBatch && c.bFirst == "inflightFail" && c.close == "beforeRetry"
}

func probeCells() []probeCell {
	var cells []probeCell
	for _, a := range []string{"ok", "temp"} {
		for _, b := range []string{"ok", "temp", "perm", "inflightOK", "inflightFail"} {
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

func TestC1InvariantProbe(t *testing.T) {
	cells := probeCells()
	violations, residuals := 0, 0
	for _, c := range cells {
		if c.residual() {
			residuals++
		}
		for _, v := range runProbeCell(t, c) {
			violations++
			t.Errorf("%s: %s", c, v)
		}
	}
	t.Logf("C1 invariant probe: %d cells, %d violations, %d residual cells", len(cells), violations, residuals)
}

func runProbeCell(t *testing.T, c probeCell) []string {
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
	temp := &mail.SendError{Stage: mail.StageConnect, Class: mail.ClassTimeout, Temporary: true}
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
			return temp
		case m.To[0] == addrB && c.bFirst == "perm":
			return perm
		case m.To[0] == addrB && c.bFirst != "ok":
			return temp
		}
		return nil
	})
	w.set(state.StateFailed, site...)
	w.pass()
	w.fake.FailWhen(nil)
	inflight := strings.HasPrefix(c.bFirst, "inflight")
	var row store.Delivery
	if inflight {
		// No row when the whole batch was muted at formation (a suspended customer): nothing in flight.
		row, inflight = w.inFlightRow(addrB)
	}
	if c.muteAt == "beforeRetry" {
		mute()
	}
	if c.close == "beforeRetry" {
		w.set(state.StateOK, dev0)
		w.pass()
	}
	w.clk.Advance(time.Minute)
	w.retry()
	if inflight {
		if c.bFirst == "inflightFail" {
			w.fake.FailWhen(func(mail.Message) *mail.SendError { return temp })
		}
		w.finishInFlight(row, addrB)
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
	for i := 0; i < 4; i++ { // past the whole retry schedule (1, 5 and 15 minutes)
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
	if sum(mail.KindRecovery, h0) > 0 && sum(mail.KindAlert, h0) == 0 && !c.residual() {
		out = append(out, "(3) a recovery names dev0, and no delivered alert ever named it")
	}
	return out
}
