package agent

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/agent/internal/fakerestic"
	"github.com/c2tech-sys/cryobackup/internal/api"
)

// M3-UPLINK-L9 (ADR 0018 decision 2): one answer from the console wakes every
// uplink loop, and the heartbeat's minimum gap still holds after a kick.
//
// The tests put a gate in front of the fake console. It answers 503 to the
// routes it is told to refuse (with a Retry-After when asked) and records every
// request it saw, answered or not, so a test can say which loop knocked when.

// l9Arrival is one request the gate saw.
type l9Arrival struct {
	at       time.Time
	answered bool
}

// l9Gate fronts a fakeConsole. A route key is "heartbeat", "policy",
// "commands", "enroll" or "other" (runs, snapshots, results, lease).
type l9Gate struct {
	fc  *fakeConsole
	srv *httptest.Server

	mu       sync.Mutex
	refuse   map[string]bool // key -> answer 503
	retry    map[string]int  // key -> Retry-After seconds on a refusal
	arrivals map[string][]l9Arrival
}

var l9Keys = []string{"heartbeat", "policy", "commands", "other"}

// newL9Gate puts the gate in front of fc: fc.URL() is the gate's from now on,
// so call it before enrolling.
func newL9Gate(t *testing.T, fc *fakeConsole) *l9Gate {
	t.Helper()
	g := &l9Gate{fc: fc, refuse: map[string]bool{}, retry: map[string]int{}, arrivals: map[string][]l9Arrival{}}
	g.srv = httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(g.srv.Close)
	fc.srv = g.srv
	return g
}

func l9Key(r *http.Request) string {
	switch {
	case r.URL.Path == api.RouteEnroll:
		return "enroll"
	case r.URL.Path == api.RouteHeartbeat:
		return "heartbeat"
	case r.URL.Path == api.RoutePolicy:
		return "policy"
	case r.URL.Path == api.RouteCommands && r.Method == http.MethodGet:
		return "commands"
	}
	return "other"
}

func (g *l9Gate) serve(w http.ResponseWriter, r *http.Request) {
	key := l9Key(r)
	g.mu.Lock()
	refused := g.refuse[key] && key != "enroll"
	ra := g.retry[key]
	g.arrivals[key] = append(g.arrivals[key], l9Arrival{at: time.Now(), answered: !refused})
	g.mu.Unlock()
	if !refused {
		g.fc.serve(w, r)
		return
	}
	e := api.NewError(api.CodeUnavailable)
	if ra > 0 {
		e.RetryAfterS = ra
		w.Header().Set("Retry-After", strconv.Itoa(ra))
	}
	w.Header().Set("Content-Type", api.ContentTypeProblem)
	w.WriteHeader(e.Status)
	_ = json.NewEncoder(w).Encode(e)
}

// refuseKey makes the route answer 503 (true) or normally (false).
func (g *l9Gate) refuseKey(key string, refuse bool) {
	g.mu.Lock()
	g.refuse[key] = refuse
	g.mu.Unlock()
}

// refuseAll is refuseKey for every uplink route (not enroll).
func (g *l9Gate) refuseAll(refuse bool) {
	g.mu.Lock()
	for _, k := range l9Keys {
		g.refuse[k] = refuse
	}
	g.mu.Unlock()
}

// retryAfter sets the Retry-After (seconds) that refusals of the route carry.
func (g *l9Gate) retryAfter(key string, seconds int) {
	g.mu.Lock()
	g.retry[key] = seconds
	g.mu.Unlock()
}

// count is how many requests of the route were answered (true) or refused
// (false).
func (g *l9Gate) count(key string, answered bool) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, a := range g.arrivals[key] {
		if a.answered == answered {
			n++
		}
	}
	return n
}

// answeredTime is the time of the answered request of the route that came
// after n others, counting from zero.
func (g *l9Gate) answeredTime(key string, n int) (time.Time, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	i := 0
	for _, a := range g.arrivals[key] {
		if !a.answered {
			continue
		}
		if i == n {
			return a.at, true
		}
		i++
	}
	return time.Time{}, false
}

// firstArrivalAfter is the time of the first request of the route (answered or
// not) later than t.
func (g *l9Gate) firstArrivalAfter(key string, t time.Time) (time.Time, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, a := range g.arrivals[key] {
		if a.at.After(t) {
			return a.at, true
		}
	}
	return time.Time{}, false
}

// l9Agent starts an agent against a gated fake console with a paused policy
// (no backup runs, so nothing but the loops kicks the heartbeat), and waits
// until each of the heartbeat, policy and command loops has been answered once.
func l9Agent(t *testing.T, tune func(*Options)) (*fakeConsole, *l9Gate, *running, *syncBuf) {
	t.Helper()
	fakerestic.Use(t, "backup_ok") // restic version for the start-up check; a paused policy never runs it again
	fc := newFakeConsole(t)
	fc.setPolicy(api.PolicyResponse{Policy: testPolicy(), PolicyID: testPolicyID, Version: 1, ETag: `"policy-v1"`, Paused: true})
	g := newL9Gate(t, fc)
	o := fakeOptions(t, enroll(t, fc))
	buf := &syncBuf{}
	o.Logger = slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if tune != nil {
		tune(&o)
	}
	r := start(t, o)
	waitFor(t, "every loop to be answered once", 10*time.Second, func() bool {
		return g.count("heartbeat", true) >= 1 && g.count("policy", true) >= 1 && g.count("commands", true) >= 1
	})
	return fc, g, r, buf
}

// The fleet comes back within one heartbeat of the console answering anything.
// The heartbeat is parked: the console refused it with a Retry-After of 30 s,
// which the agent honours exactly, while the command loop (or the policy loop)
// has failed several times on its own 50 ms backoff. When only that loop's route
// starts answering, its success must wake the heartbeat: before ADR 0018 only a
// heartbeat's own success told anyone the console was back, so the heartbeat
// slept out its 30 s.
func TestOutageRecoveryIsOneHeartbeatAway(t *testing.T) {
	for _, first := range []string{"commands", "policy"} {
		t.Run(first+" answers first", func(t *testing.T) {
			_, g, r, _ := l9Agent(t, nil)

			g.refuseAll(true)
			g.retryAfter("heartbeat", 30)
			if first == "policy" {
				kick(r.a.kickPolicy) // the policy loop sleeps 15 min when healthy: start its failing retries
			}
			waitFor(t, "the heartbeat to be refused and parked", 10*time.Second, func() bool { return g.count("heartbeat", false) >= 1 })
			waitFor(t, "the loop to fail several times", 10*time.Second, func() bool { return g.count(first, false) >= 3 })

			answered0 := g.count(first, true)
			g.refuseKey(first, false)
			var reached time.Time
			waitFor(t, "the "+first+" route to be answered", 10*time.Second, func() bool {
				var ok bool
				reached, ok = g.answeredTime(first, answered0)
				return ok
			})

			// A heartbeat arrives within a bound far shorter than the 30 s it
			// was parked for: 2 x the minimum gap plus slack.
			bound := 2*r.a.o.heartbeatMinGap + 500*time.Millisecond
			var hb time.Time
			waitFor(t, "a heartbeat after the "+first+" loop got through", 5*time.Second, func() bool {
				var ok bool
				hb, ok = g.firstArrivalAfter("heartbeat", reached)
				return ok
			})
			if d := hb.Sub(reached); d > bound {
				t.Errorf("the heartbeat came %v after the console answered the %s loop, want within %v", d, first, bound)
			}
		})
	}
}

var l9BackLine = regexp.MustCompile(`msg="console reachable again" from=(heartbeat|policy|outbox|commands|lease|snapshots)\b`)

// One recovery is one "console reachable again" line, however many loops see
// the console answer: the first call after a failure logs, the others find the
// failure already accounted for. A second outage and recovery logs a second
// line.
func TestConsoleBackLogsOnce(t *testing.T) {
	_, g, r, buf := l9Agent(t, nil)

	outage := func() {
		t.Helper()
		hb, pol, cmd := g.count("heartbeat", false), g.count("policy", false), g.count("commands", false)
		g.refuseAll(true)
		kick(r.a.kickPolicy)
		waitFor(t, "the heartbeat, command and policy loops to fail repeatedly", 20*time.Second, func() bool {
			return g.count("heartbeat", false) >= hb+3 && g.count("commands", false) >= cmd+3 && g.count("policy", false) >= pol+2
		})
	}
	recovery := func() {
		t.Helper()
		hb, pol, cmd := g.count("heartbeat", true), g.count("policy", true), g.count("commands", true)
		g.refuseAll(false)
		waitFor(t, "every loop to be answered again", 20*time.Second, func() bool {
			return g.count("heartbeat", true) > hb && g.count("policy", true) > pol && g.count("commands", true) > cmd
		})
	}
	lines := func() int { return len(l9BackLine.FindAllString(buf.String(), -1)) }

	outage()
	recovery()
	waitFor(t, "the recovery line", 10*time.Second, func() bool { return lines() >= 1 })
	time.Sleep(500 * time.Millisecond) // the woken loops get their answers too
	if n := lines(); n != 1 {
		t.Fatalf("%d recovery lines after the first recovery, want 1:\n%s", n, buf.String())
	}

	outage()
	recovery()
	waitFor(t, "the second recovery line", 10*time.Second, func() bool { return lines() >= 2 })
	time.Sleep(500 * time.Millisecond)
	if n := lines(); n != 2 {
		t.Fatalf("%d recovery lines after the second recovery, want 2:\n%s", n, buf.String())
	}
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.Contains(line, "console reachable again") && strings.ContainsAny(line, `\/`) {
			t.Errorf("a path-like character in the recovery line: %s", line)
		}
	}
}

// Kicks never beat the heartbeat's minimum gap: two kicks 1 ms apart, right
// after a heartbeat, make one heartbeat, and it comes a full gap after the one
// before. (Before the fix the second kick waited out the gap in the buffer and
// made a second heartbeat a gap later.)
func TestKickRespectsHeartbeatMinGap(t *testing.T) {
	const gap = 400 * time.Millisecond
	_, g, r, _ := l9Agent(t, func(o *Options) {
		o.heartbeatEvery = time.Hour // no scheduled heartbeat inside the test
		o.heartbeatMinGap = gap
	})
	first, ok := g.answeredTime("heartbeat", 0)
	if !ok {
		t.Fatal("no first heartbeat")
	}
	time.Sleep(50 * time.Millisecond) // the loop is asleep again
	kick(r.a.kickHeartbeat)
	time.Sleep(time.Millisecond)
	kick(r.a.kickHeartbeat)

	waitFor(t, "the kicked heartbeat", 5*time.Second, func() bool { return g.count("heartbeat", true) >= 2 })
	second, _ := g.answeredTime("heartbeat", 1)
	if d := second.Sub(first); d < gap {
		t.Errorf("the kicked heartbeat came %v after the previous one, want at least the %v gap", d, gap)
	}
	if d := second.Sub(first); d > gap+time.Second {
		t.Errorf("the kicked heartbeat came %v after the previous one, want about the %v gap", d, gap)
	}
	time.Sleep(3 * gap / 2) // a second gap passes: the two kicks were one heartbeat
	if n := g.count("heartbeat", true); n != 2 {
		t.Errorf("%d heartbeats, want 2 (the first and one for both kicks)", n)
	}
}
