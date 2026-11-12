package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
)

// M3-L12: "console reachable again" follows a request the console answered,
// never an outbox pass that found nothing due.
//
// After a failed upload the record is put back with a later retry time, so the
// next pass finds nothing due and sends nothing. The loop used to take that
// for a success after failures: a false "console reachable again from=outbox",
// every loop woken to fail again, the backoff reset. Seen live during a console
// outage. The pass is forced here by kicking the outbox loop while the record's
// retry time is still ahead, as a finished run (journalSkipped), a resume or
// another loop's recovery kicks it.

func TestOutboxPassWithNothingDueIsNotARecovery(t *testing.T) {
	const backoff = 400 * time.Millisecond // the retry time of a failed record is far enough ahead to kick into
	variants := []struct {
		name string
		cut  func(g *l9Gate, fc *fakeConsole, on bool)
	}{
		{"every route refused", func(g *l9Gate, _ *fakeConsole, on bool) { g.refuseAll(on) }},
		{"connections cut", func(_ *l9Gate, fc *fakeConsole, on bool) { fc.down.Store(on) }},
	}
	for _, v := range variants {
		t.Run(v.name, func(t *testing.T) {
			fc, g, r, buf := l9Agent(t, func(o *Options) { o.backoffMin = backoff })
			ctx := context.Background()
			back := func() int { return strings.Count(buf.String(), "console reachable again") }
			uploads := func() int { return g.count("other", true) + g.count("other", false) }

			// The console goes away and a finished run waits in the outbox.
			v.cut(g, fc, true)
			r.a.journalSkipped(ctx, api.TriggerManual)
			waitFor(t, "the upload to have failed twice", 20*time.Second, func() bool { return uploads() >= 2 })
			var next time.Time
			waitFor(t, "the failed record to be put back with a later retry time", 5*time.Second, func() bool {
				n, ok, _ := r.a.jr.OutboxNext(ctx)
				next = n
				return ok && next.Sub(time.Now()) > 300*time.Millisecond
			})

			// Several outbox passes with nothing due while the console is gone.
			before := uploads()
			for range 6 {
				kick(r.a.kickOutbox)
				time.Sleep(25 * time.Millisecond)
			}
			time.Sleep(100 * time.Millisecond) // the last pass finishes
			if time.Until(next) <= 0 {
				t.Fatal("the record became due during the passes: the test did not exercise a pass with nothing due")
			}
			if n := uploads(); n != before {
				t.Errorf("%d upload requests during passes with nothing due, want 0", n-before)
			}
			if n := back(); n != 0 {
				t.Fatalf("%d \"console reachable again\" lines while the console was down:\n%s", n, buf.String())
			}

			// The record's retry time passes: a third upload, which fails too.
			waitFor(t, "a third upload attempt", 20*time.Second, func() bool { return uploads() > before })
			time.Sleep(100 * time.Millisecond)
			if n := back(); n != 0 {
				t.Fatalf("%d \"console reachable again\" lines after a failed upload:\n%s", n, buf.String())
			}

			// The console answers: one line, once, whoever sees it first.
			v.cut(g, fc, false)
			kick(r.a.kickOutbox)
			waitFor(t, "the record to be delivered", 20*time.Second, func() bool {
				n, _ := r.a.jr.OutboxDepth(ctx)
				return n == 0
			})
			waitFor(t, "the recovery line", 10*time.Second, func() bool { return back() >= 1 })
			time.Sleep(500 * time.Millisecond) // the woken loops get their answers too
			if n := back(); n != 1 {
				t.Fatalf("%d \"console reachable again\" lines after the recovery, want 1:\n%s", n, buf.String())
			}
			if strings.ContainsAny(lineWith(buf.String(), "console reachable again"), `\/`) {
				t.Errorf("a path-like character in the recovery line: %s", lineWith(buf.String(), "console reachable again"))
			}
		})
	}
}

// lineWith returns the first line of s that contains sub.
func lineWith(s, sub string) string {
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, sub) {
			return line
		}
	}
	return ""
}
