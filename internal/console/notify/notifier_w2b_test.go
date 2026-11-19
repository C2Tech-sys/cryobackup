package notify

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/mail"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// ackDevice acknowledges the FAILED alarm of a device.
func (w *world) ackDevice(id string) store.Ack {
	w.t.Helper()
	a, err := w.st.CreateAck(w.ctx, store.ScopeAll(), store.AckInput{SubjectType: store.EpisodeDevice, SubjectID: id, AlarmType: "failed"}, w.actor)
	if err != nil {
		w.t.Fatal(err)
	}
	return a
}

// P20-W2B item 8. The mark step of a recovery batch changes fewer episodes than the batch holds
// (another writer settled one first): the notice lists only the episodes this pass marked.
func TestNotifyRecoveryBatchSendsOnlyWhatItMarked(t *testing.T) {
	w := newWorld(t)
	site := w.site("Summit Eye Care", 2)
	w.recipient("ops@example.test", true, true)
	w.set(state.StateFailed, site...)
	w.pass()
	w.clk.Advance(time.Minute)
	w.set(state.StateOK, site...)
	w.n.beforeMark = func(kind string) error {
		if kind != keyRecovery {
			return nil
		}
		w.n.beforeMark = nil
		due, err := w.st.ListRecoveryDueEpisodes(w.ctx, store.ScopeAll())
		if err != nil {
			return err
		}
		for _, e := range due {
			if e.SubjectID == site[0] {
				_, err = w.st.MarkRecoveryNotified(w.ctx, store.ScopeAll(), []string{e.ID}, w.clk.Now().Add(-time.Second))
			}
		}
		return err
	}
	res := w.pass()
	if res.Recovered != 1 || res.Sent != 1 {
		t.Fatalf("recovery pass %+v", res)
	}
	h0, h1 := w.host[site[0]], w.host[site[1]]
	if w.mentions(mail.KindRecovery, h0) != 0 || w.mentions(mail.KindRecovery, h1) != 1 {
		t.Fatalf("recovery names dev0 %d, dev1 %d", w.mentions(mail.KindRecovery, h0), w.mentions(mail.KindRecovery, h1))
	}
	last := w.fake.Sent()[len(w.fake.Sent())-1]
	if !strings.Contains(last.Subject, "1 alarm resolved") {
		t.Fatalf("subject %q", last.Subject)
	}
}

// P20-W2B item 9. A Notifier and a Retrier on one store, with a frozen clock, picking batch ids
// at the same time: no two picks are ever equal.
func TestBatchIDsAreUniqueAcrossNotifierAndRetrier(t *testing.T) {
	w := newWorld(t)
	other := &Notifier{Store: w.st, Dispatch: w.n.Dispatch}
	pickers := []func(context.Context) (int64, error){w.n.batchID, w.r.batchID, other.batchID}
	var mu sync.Mutex
	seen := map[int64]bool{}
	var wg sync.WaitGroup
	for _, pick := range pickers {
		wg.Add(1)
		go func(pick func(context.Context) (int64, error)) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				id, err := pick(w.ctx)
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				if seen[id] {
					t.Errorf("batch id %d picked twice", id)
				}
				seen[id] = true
				mu.Unlock()
			}
		}(pick)
	}
	wg.Wait()
	if len(seen) != 150 {
		t.Fatalf("%d distinct ids", len(seen))
	}
}

// The mid-pass acknowledgement re-read leaves nothing to mark: the suppressed rows say so in the
// log (why muted_mid_pass), not "overlap".
func TestNotifyAckMidPassLeavingNothingSaysSo(t *testing.T) {
	w := newWorld(t)
	site := w.site("Summit Eye Care", 1)
	w.recipient("ops@example.test", true, true)
	w.set(state.StateFailed, site...)
	w.n.beforeMark = func(string) error {
		w.n.beforeMark = nil
		w.ackDevice(site[0])
		return nil
	}
	if res := w.pass(); res.Messages != 0 || res.Sent != 0 {
		t.Fatalf("pass %+v", res)
	}
	logs := w.logs.String()
	if !strings.Contains(logs, "muted_mid_pass") || strings.Contains(logs, "overlap") {
		t.Fatalf("log:\n%s", logs)
	}
}
