package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/agent/config"
	"github.com/c2tech-sys/cryobackup/internal/agent/internal/fakerestic"
	"github.com/c2tech-sys/cryobackup/internal/agent/journal"
	"github.com/c2tech-sys/cryobackup/internal/agent/uplink"
	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// M3-UPLINK-L4 (ADR 0018 decision 1): three heartbeats in a row answered 401
// mean the console refuses the device token. The agent says so once, its
// uplink loops stop their cadence, one heartbeat an hour keeps asking, and the
// next accepted heartbeat ends the state. Backups are not touched.

// l4Event is one Event Log line the agent wrote.
type l4Event struct {
	level string
	id    uint32
	msg   string
}

// l4Events collects the Event Log lines through Options.eventHook (the real
// Event Log is a no-op off Windows and machine state on it).
type l4Events struct {
	mu  sync.Mutex
	evs []l4Event
}

func (e *l4Events) add(level string, id uint32, msg string) {
	e.mu.Lock()
	e.evs = append(e.evs, l4Event{level, id, msg})
	e.mu.Unlock()
}

// withID returns the lines written under Event Log id.
func (e *l4Events) withID(id uint32) []l4Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []l4Event
	for _, ev := range e.evs {
		if ev.id == id {
			out = append(out, ev)
		}
	}
	return out
}

// l4Lines counts the lines of the log that contain every one of subs.
func l4Lines(log string, subs ...string) int {
	n := 0
	for _, line := range strings.Split(log, "\n") {
		ok := true
		for _, s := range subs {
			ok = ok && strings.Contains(line, s)
		}
		if ok {
			n++
		}
	}
	return n
}

// l4NotAccepted is how many heartbeats reached the console and were not
// accepted (a 401, or a 403 while forbid is set); the fake records every body
// it receives, and keeps a heartbeat only when it accepted it.
func l4NotAccepted(fc *fakeConsole) int {
	n := 0
	for _, b := range fc.allBodies() {
		if b.Path == api.RouteHeartbeat {
			n++
		}
	}
	return n - len(fc.heartbeatList())
}

// l4Counts is what every loop has asked the console so far.
type l4Counts struct{ heartbeat, policy, commands, other int }

func l4Count(g *l9Gate) l4Counts {
	c := func(k string) int { return g.count(k, true) + g.count(k, false) }
	return l4Counts{c("heartbeat"), c("policy"), c("commands"), c("other")}
}

func (c l4Counts) String() string {
	return fmt.Sprintf("heartbeat %d, policy %d, commands %d, other %d", c.heartbeat, c.policy, c.commands, c.other)
}

// l4Agent is l9Agent (paused policy: no backup runs, so nothing but the loops
// talks to the console) with the Event Log observable.
func l4Agent(t *testing.T, tune func(*Options)) (*fakeConsole, *l9Gate, *running, *syncBuf, *l4Events) {
	t.Helper()
	ev := &l4Events{}
	fc, g, r, buf := l9Agent(t, func(o *Options) {
		o.eventHook = ev.add
		if tune != nil {
			tune(o)
		}
	})
	return fc, g, r, buf, ev
}

// refuse revokes the device and waits until the agent has concluded it is
// refused.
func l4Refuse(t *testing.T, fc *fakeConsole, r *running) {
	t.Helper()
	fc.revoke()
	waitFor(t, "the agent to be refused", 10*time.Second, r.a.isRefused)
}

// Three heartbeats answered 401 in a row: REFUSED, once. One ERROR line with the
// class and the first 401's time, one Event Log entry (id 1008), the journal
// key; then no loop calls the console (the probe is two seconds away here).
func TestThree401sEnterRefusedOnce(t *testing.T) {
	fc, g, r, buf, ev := l4Agent(t, func(o *Options) { o.refusedProbe = 2 * time.Second })
	ctx := context.Background()
	if r.a.isRefused() {
		t.Fatal("refused before anything was refused")
	}

	fc.revoke()
	waitFor(t, "the agent to be refused", 10*time.Second, r.a.isRefused)
	if n := l4NotAccepted(fc); n != 3 {
		t.Fatalf("refused after %d heartbeats answered 401, want 3", n)
	}

	// The journal key is the first 401's time, RFC 3339 UTC.
	v, ok, err := r.a.jr.KVGet(ctx, journal.KVTokenRefusedSince)
	if err != nil || !ok {
		t.Fatalf("journal key %q: %q, %v, %v", journal.KVTokenRefusedSince, v, ok, err)
	}
	since, err := time.Parse(time.RFC3339, v)
	if err != nil || since.Location() != time.UTC {
		t.Fatalf("journal key %q is not an RFC 3339 UTC time: %v", v, err)
	}
	g.mu.Lock()
	first401 := g.arrivals["heartbeat"][len(fc.heartbeatList())].at // the arrivals before it were accepted
	g.mu.Unlock()
	if d := since.Sub(first401); d < -2*time.Second || d > 2*time.Second {
		t.Errorf("the journal key says %s, the first 401 arrived at %s", v, first401.UTC().Format(time.RFC3339))
	}

	// Once: one ERROR line, one Event Log entry, neither with a URL, a path or a
	// token.
	log := buf.String()
	if n := l4Lines(log, "level=ERROR", "class=token_refused"); n != 1 {
		t.Errorf("%d ERROR lines with class=token_refused, want 1:\n%s", n, log)
	}
	for _, want := range []string{"the console refuses the device token", "since=" + v, `action="re-enroll or uninstall"`} {
		if !strings.Contains(log, want) {
			t.Errorf("the log lacks %q:\n%s", want, log)
		}
	}
	evs := ev.withID(1008)
	if len(evs) != 1 || evs[0].level != "error" || !strings.Contains(evs[0].msg, "the console refuses the device token") ||
		!strings.Contains(evs[0].msg, "backups keep running on the last policy") || !strings.Contains(evs[0].msg, "once an hour") {
		t.Fatalf("Event Log 1008 entries = %+v", evs)
	}
	var said []string // what the episode wrote: its log lines and its Event Log text
	for _, line := range strings.Split(log, "\n") {
		if strings.Contains(line, "token_refused") {
			said = append(said, line)
		}
	}
	for _, text := range append(said, evs[0].msg) {
		for _, bad := range []string{"http://", "https://", fc.URL(), "device-token", "revoked-token", `\`, "/state"} {
			if strings.Contains(text, bad) {
				t.Errorf("a refused-token line carries %q: %s", bad, text)
			}
		}
	}

	// Five heartbeat periods later no loop has called the console: the probe is
	// two seconds away, and a loop that asks "refused?" before it calls sleeps.
	time.Sleep(100 * time.Millisecond) // a request that was already on its way
	before := l4Count(g)
	time.Sleep(5 * 80 * time.Millisecond)
	if after := l4Count(g); after != before {
		t.Errorf("the loops called the console while refused: %s -> %s", before, after)
	}
	if !r.a.isRefused() {
		t.Error("no longer refused")
	}
}

// The rule itself, against every kind of answer: only a 401 counts, any other
// outcome restarts the count, and the third 401 in a row refuses. Table over
// the sequence of heartbeat outcomes.
func TestRefusedRuleCountsOnlyConsecutive401s(t *testing.T) {
	e401 := func() error { return &uplink.Error{Op: "heartbeat", Status: 401, Code: api.CodeUnauthorized} }
	other := map[string]error{
		"403":     &uplink.Error{Op: "heartbeat", Status: 403, Code: api.CodeForbidden},
		"429":     &uplink.Error{Op: "heartbeat", Status: 429, Code: api.CodeRateLimited},
		"503":     &uplink.Error{Op: "heartbeat", Status: 503, Code: api.CodeUnavailable},
		"network": &uplink.Error{Op: "heartbeat", Network: true},
		"tls":     &uplink.Error{Op: "heartbeat", TLS: true},
		"invalid": errors.New("agent: heartbeat does not validate"),
	}
	for name, o := range other {
		t.Run("401 401 "+name+" 401 401 is not refused", func(t *testing.T) {
			a := l4Bare(t)
			for i, err := range []error{e401(), e401(), o, e401(), e401()} {
				if refused := a.heartbeatRefused(context.Background(), err); refused {
					t.Fatalf("refused after outcome %d", i+1)
				}
			}
			if n := a.tokenRefusals; n != 2 {
				t.Errorf("count = %d, want 2", n)
			}
			if !a.heartbeatRefused(context.Background(), e401()) {
				t.Error("the third 401 in a row after it did not refuse")
			}
		})
		t.Run(name+" while refused changes nothing", func(t *testing.T) {
			a := l4Bare(t)
			for range 3 {
				a.heartbeatRefused(context.Background(), e401())
			}
			if !a.isRefused() {
				t.Fatal("three 401s did not refuse")
			}
			if !a.heartbeatRefused(context.Background(), o) || !a.isRefused() {
				t.Error("a failed probe ended REFUSED")
			}
			if n := a.tokenRefusals; n != 3 {
				t.Errorf("count = %d after a failed probe, want 3 (unchanged)", n)
			}
		})
	}
	t.Run("a 2xx restarts the count", func(t *testing.T) {
		a := l4Bare(t)
		a.heartbeatRefused(context.Background(), e401())
		a.heartbeatRefused(context.Background(), e401())
		if a.heartbeatAccepted(context.Background()) {
			t.Error("an accepted heartbeat ended REFUSED although the agent was not refused")
		}
		if a.heartbeatRefused(context.Background(), e401()) || a.heartbeatRefused(context.Background(), e401()) {
			t.Error("two 401s after a 2xx refused")
		}
	})
	t.Run("a wrapped 401 counts", func(t *testing.T) {
		a := l4Bare(t)
		for range 3 {
			a.heartbeatRefused(context.Background(), fmt.Errorf("heartbeat: %w", e401()))
		}
		if !a.isRefused() {
			t.Error("a wrapped 401 did not count")
		}
	})
}

// l4Bare is an Agent with just what the refused-token rule touches: a clock, a
// log, a journal and the Event Log hook.
func l4Bare(t *testing.T) *Agent {
	t.Helper()
	jr, err := journal.Open(context.Background(), journal.Options{Path: filepath.Join(t.TempDir(), "journal.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = jr.Close() })
	return &Agent{
		o: Options{eventHook: func(string, uint32, string) {}}, jr: jr, clock: time.Now,
		log: slog.New(slog.NewTextHandler(&syncBuf{}, nil)),
	}
}

// Two 401s and then a failure that is not a 401 (the connection cut, or a 503)
// restart the count: more 401s after it do not refuse until three come in a
// row. A restored token ends it all without REFUSED ever having been entered.
func TestTwo401sThenNetworkDoNotEnterRefused(t *testing.T) {
	for _, kind := range []string{"connection cut", "503"} {
		t.Run(kind, func(t *testing.T) {
			// A longer backoff than the tests' 50 ms: the window between the second
			// 401 and the next heartbeat (at least 160 ms here) is the time the test
			// has to interpose the failure.
			fc, g, r, buf, ev := l4Agent(t, func(o *Options) { o.backoffMin = 200 * time.Millisecond })
			cut := func(on bool) {
				if kind == "503" {
					g.refuseKey("heartbeat", on)
				} else {
					fc.down.Store(on)
				}
			}
			count := func() int {
				r.a.mu.Lock()
				defer r.a.mu.Unlock()
				return r.a.tokenRefusals
			}

			fc.revoke()
			waitFor(t, "two heartbeats answered 401", 10*time.Second, func() bool { return l4NotAccepted(fc) >= 2 })
			cut(true)
			total := func() int { c := l4Count(g); return c.heartbeat }
			base := total()
			waitFor(t, "the cut heartbeat", 10*time.Second, func() bool { return total() > base })
			waitFor(t, "the count to restart", 10*time.Second, func() bool { return count() == 0 })
			cut(false)
			if r.a.isRefused() {
				t.Fatal("refused after two 401s and a failure")
			}

			// Two more 401s: the count is two, not four.
			waitFor(t, "two more heartbeats answered 401", 30*time.Second, func() bool { return l4NotAccepted(fc) >= 4 })
			waitFor(t, "the count to be two", 10*time.Second, func() bool { return count() == 2 })
			if r.a.isRefused() {
				t.Fatal("refused: the failure did not restart the count")
			}

			// The token comes back: accepted, never refused.
			fc.restore()
			accepted := len(fc.heartbeatList())
			kick(r.a.kickHeartbeat)
			waitFor(t, "an accepted heartbeat", 10*time.Second, func() bool { return len(fc.heartbeatList()) > accepted })
			waitFor(t, "the count to restart", 10*time.Second, func() bool { return count() == 0 })
			if r.a.isRefused() || l4Lines(buf.String(), "token_refused") != 0 || len(ev.withID(1008)) != 0 {
				t.Errorf("REFUSED left a trace:\n%s\n%+v", buf.String(), ev.withID(1008))
			}
		})
	}
}

// A 403 is a valid token wrongly used: three of them, and more, never refuse.
func TestThree403sDoNotEnterRefused(t *testing.T) {
	fc, _, r, buf, ev := l4Agent(t, nil)
	fc.forbid.Store(true)
	waitFor(t, "five heartbeats answered 403", 30*time.Second, func() bool { return l4NotAccepted(fc) >= 5 })
	r.a.mu.Lock()
	n := r.a.tokenRefusals
	r.a.mu.Unlock()
	if r.a.isRefused() || n != 0 {
		t.Fatalf("refused (%v) or counting (%d) after 403s", r.a.isRefused(), n)
	}
	if l4Lines(buf.String(), "token_refused") != 0 || len(ev.withID(1008)) != 0 {
		t.Errorf("403s left a REFUSED trace:\n%s", buf.String())
	}
}

// The next accepted heartbeat ends REFUSED: the state, the journal key and one
// INFO line; every loop wakes and asks the console at once. Until then the
// hourly probe (300 ms here) keeps asking, and no 401 adds a second ERROR line
// or Event Log entry however long it lasts.
func TestRefusedEndsOnTheNextAnswer(t *testing.T) {
	fc, g, r, buf, ev := l4Agent(t, func(o *Options) { o.refusedProbe = 300 * time.Millisecond })
	ctx := context.Background()
	l4Refuse(t, fc, r)
	want, _, _ := r.a.jr.KVGet(ctx, journal.KVTokenRefusedSince)

	// Three probes later it is still the same episode.
	base := l4NotAccepted(fc)
	waitFor(t, "three probes answered 401", 10*time.Second, func() bool { return l4NotAccepted(fc) >= base+3 })
	if !r.a.isRefused() {
		t.Fatal("a 401 probe ended REFUSED")
	}
	if got, _, _ := r.a.jr.KVGet(ctx, journal.KVTokenRefusedSince); got != want {
		t.Errorf("the journal key moved from %q to %q", want, got)
	}
	if n := l4Lines(buf.String(), "level=ERROR", "token_refused"); n != 1 {
		t.Errorf("%d ERROR lines after the probes, want 1", n)
	}
	if n := len(ev.withID(1008)); n != 1 {
		t.Errorf("%d Event Log 1008 entries after the probes, want 1", n)
	}

	before := l4Count(g)
	fc.restore()
	restored := time.Now()
	waitFor(t, "the agent to leave REFUSED within two probes", 600*time.Millisecond, func() bool { return !r.a.isRefused() })
	t.Logf("left REFUSED %v after the token came back", time.Since(restored).Round(time.Millisecond))
	if _, ok, _ := r.a.jr.KVGet(ctx, journal.KVTokenRefusedSince); ok {
		t.Error("the journal key survived the end of REFUSED")
	}
	r.a.mu.Lock()
	since, n := r.a.refusedSince, r.a.tokenRefusals
	r.a.mu.Unlock()
	if !since.IsZero() || n != 0 {
		t.Errorf("state left behind: since %v, count %d", since, n)
	}
	waitFor(t, "the INFO line", 5*time.Second, func() bool {
		return l4Lines(buf.String(), "level=INFO", "the console accepts the device token again", "refused_for=") == 1
	})
	// The wake-all: the policy and commands loops, which had stopped, ask again.
	waitFor(t, "the policy and commands loops to ask again", 5*time.Second, func() bool {
		now := l4Count(g)
		return now.policy > before.policy && now.commands > before.commands
	})
	if n := l4Lines(buf.String(), "level=ERROR", "token_refused"); n != 1 {
		t.Errorf("%d ERROR lines at the end, want 1", n)
	}
	if n := l4Lines(buf.String(), "the console accepts the device token again"); n != 1 {
		t.Errorf("%d \"accepts again\" lines, want 1", n)
	}
	if n := len(ev.withID(1008)); n != 1 {
		t.Errorf("%d Event Log 1008 entries at the end, want 1", n)
	}
	// And it can happen again: a second retirement is a new episode.
	fc.revoke()
	waitFor(t, "a second REFUSED", 10*time.Second, r.a.isRefused)
	waitFor(t, "its entries", 5*time.Second, func() bool { return len(ev.withID(1008)) == 2 })
	if n := l4Lines(buf.String(), "level=ERROR", "token_refused"); n != 2 {
		t.Errorf("%d ERROR lines after the second episode, want 2", n)
	}
}

// The end of REFUSED wakes every loop (ADR 0018 decision 2's wake-all): the
// loops' own refused sleep is five seconds here, so a policy pull and a command
// poll within a second of the first accepted heartbeat can only be the kick. The
// heartbeat is kicked rather than left to its probe (also five seconds): a kick
// wakes it, as it wakes it from any sleep.
func TestRefusedEndWakesEveryLoop(t *testing.T) {
	fc, g, r, _, _ := l4Agent(t, func(o *Options) { o.refusedProbe = 5 * time.Second })
	l4Refuse(t, fc, r)
	time.Sleep(100 * time.Millisecond) // a request that was already on its way
	before := l4Count(g)

	fc.restore()
	kick(r.a.kickHeartbeat)
	waitFor(t, "the agent to leave REFUSED", 3*time.Second, func() bool { return !r.a.isRefused() })
	waitFor(t, "the policy and commands loops to ask at once", 3*time.Second, func() bool {
		now := l4Count(g)
		return now.policy > before.policy && now.commands > before.commands
	})
}

// The journal key lives and dies with the in-memory state: a process that
// starts finds none, even when the previous one stopped while refused.
func TestRefusedKeyDoesNotSurviveARestart(t *testing.T) {
	fakerestic.Use(t, "backup_ok")
	fc := newFakeConsole(t)
	dir := enroll(t, fc)
	pol := fc.policy
	pol.Paused = true
	fc.setPolicy(pol)
	o := fakeOptions(t, dir)

	r := start(t, o)
	waitFor(t, "an accepted heartbeat", 10*time.Second, func() bool { return len(fc.heartbeatList()) >= 1 })
	l4Refuse(t, fc, r)
	r.stop()

	jpath := config.Layout{Root: dir}.JournalFile()
	read := func() (string, bool) {
		t.Helper()
		jr, err := journal.Open(context.Background(), journal.Options{Path: jpath, ReadOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		defer jr.Close()
		v, ok, _ := jr.KVGet(context.Background(), journal.KVTokenRefusedSince)
		return v, ok
	}
	if _, ok := read(); !ok {
		t.Fatal("the stopped agent left no key (nothing to test)")
	}

	// Restarted with the token accepted again: no key (and not refused).
	fc.restore()
	r2 := start(t, o)
	waitFor(t, "an accepted heartbeat after the restart", 10*time.Second, func() bool { return len(fc.heartbeatList()) >= 2 })
	if v, ok := read(); ok || r2.a.isRefused() {
		t.Errorf("after a restart that found the token accepted: key %q (%v), refused %v", v, ok, r2.a.isRefused())
	}
}

// Backups are not stopped by REFUSED: the agent does not touch the schedule, the
// run ladder or the lease. The scheduled (boot) run comes due after the agent
// has been refused for a while; it asks the console for its start lease exactly
// as it would otherwise. The retired token gets a 401 there too, which the
// lease path has always answered with "the backup does not start" and its own
// backoff, so the run waits on that and the agent stays REFUSED; when the
// console accepts the token again the same run goes ahead and its record
// reaches the console.
func TestRefusedKeepsBackupSchedule(t *testing.T) {
	fakerestic.Use(t, "backup_ok")
	fc := newFakeConsole(t)
	// The first scheduled start comes three seconds after the agent starts: the
	// agent is refused (about 300 ms) well before it is due.
	pol := fc.policy
	pol.Policy.Schedule.StartupDelayS = 3
	fc.setPolicy(pol)
	fc.leaseMode.Store(leaseGrant)
	dir := enroll(t, fc)
	ev := &l4Events{}
	buf := &syncBuf{}
	o := fakeOptions(t, dir)
	o.eventHook = ev.add
	o.Logger = slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	r := start(t, o)

	waitFor(t, "an accepted heartbeat", 10*time.Second, func() bool { return len(fc.heartbeatList()) >= 1 })
	l4Refuse(t, fc, r)
	leaseAsks := func() int {
		n := 0
		for _, b := range fc.allBodies() {
			if b.Path == api.RouteStartLease {
				n++
			}
		}
		return n
	}
	if n := leaseAsks(); n != 0 {
		t.Fatalf("%d lease requests before the run was due", n)
	}

	// The run comes due while REFUSED: the start lease is skipped (the console
	// would answer it 401 too, and a refused agent does not knock between its
	// probes), the backup runs against the gateway on the last policy, and its
	// record waits in the outbox, because the run upload loop is asleep too.
	waitFor(t, "the run to finish locally while refused", 30*time.Second, func() bool {
		n, _ := r.a.jr.OutboxDepth(context.Background())
		return n >= 1
	})
	if !r.a.isRefused() {
		t.Error("REFUSED ended without an accepted heartbeat")
	}
	if n := leaseAsks(); n != 0 {
		t.Errorf("%d start-lease requests while refused, want 0 (the lease is skipped, ADR 0018)", n)
	}
	if n := fc.postCount(); n != 0 {
		t.Errorf("%d run records reached the console while refused, want 0", n)
	}
	if !strings.Contains(buf.String(), "start lease skipped: the console refuses the device token") {
		t.Errorf("the skipped lease is not in the log:\n%s", buf.String())
	}
	if n := l4Lines(buf.String(), "level=ERROR", "token_refused"); n != 1 {
		t.Errorf("%d token_refused ERROR lines, want 1", n)
	}

	// The token is accepted again: the queued record lands (the outbox loop is
	// woken with the others) and the agent is no longer refused.
	fc.restore()
	run := firstClosed(t, fc, "the queued run once the console accepts the token")
	if run.Outcome != restic.OutcomeSuccess || run.Trigger != api.TriggerBoot || run.Stats.Lease != api.LeaseBypassed {
		t.Errorf("run = %+v (want a successful boot run with the lease bypassed)", run)
	}
	if r.a.isRefused() {
		t.Error("still refused after the run landed")
	}
}
