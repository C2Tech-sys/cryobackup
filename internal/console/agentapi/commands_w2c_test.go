package agentapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/agent/commands"
	"github.com/c2tech-sys/cryobackup/internal/agent/journal"
	"github.com/c2tech-sys/cryobackup/internal/agent/uplink"
	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// M2-W2C end to end, in process: an operator queues a command through the same
// store call the admin API and the UI make, and a fake agent built from the real
// pieces of the agent (the uplink client, the command executor, the journal)
// polls, executes and reports exactly as internal/agent's commandsLoop does.

type fakeAgent struct {
	t       *testing.T
	h       *harness
	up      *uplink.Client
	jr      *journal.Journal
	cutPost atomic.Bool // result posts fail at the network while set
	exec    *commands.Executor
	mu      sync.Mutex
	backups int
	cancels int
	active  bool // a run is open: cancel finds something to stop
}

func (h *harness) newFakeAgent(token string) *fakeAgent {
	h.t.Helper()
	jr, err := journal.Open(context.Background(), journal.Options{Path: filepath.Join(h.t.TempDir(), "j.db"), Clock: h.clk.Now})
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { _ = jr.Close() })
	var a *fakeAgent
	cut := roundTrip(func(r *http.Request) (*http.Response, error) {
		if a != nil && a.cutPost.Load() && r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/result") {
			return nil, errors.New("connection reset")
		}
		return http.DefaultTransport.RoundTrip(r)
	})
	up, err := uplink.New(uplink.Options{BaseURL: h.srv.URL, Token: func() string { return token }, AgentVersion: "1.4.2", InsecureLab: true, Clock: h.clk.Now, HTTPClient: &http.Client{Transport: cut}})
	if err != nil {
		h.t.Fatal(err)
	}
	a = &fakeAgent{t: h.t, h: h, up: up, jr: jr}
	a.exec = &commands.Executor{Journal: jr, Clock: h.clk.Now, Handlers: commands.Handlers{
		BackupNow: func(context.Context) (bool, error) {
			a.mu.Lock()
			defer a.mu.Unlock()
			a.backups++
			a.active = true
			return true, nil
		},
		CancelRun: func(context.Context, string) (bool, string) {
			a.mu.Lock()
			defer a.mu.Unlock()
			a.cancels++
			was := a.active
			a.active = false
			return was, commands.ReasonNoRun
		},
		RefreshPolicy: func(context.Context) error { return nil },
		CheckIn:       func(context.Context) error { return nil },
	}}
	return a
}

// poll is one pass of the agent's commands loop: long poll, execute each
// command, post each result. It returns how many commands it was handed.
func (a *fakeAgent) poll(wait time.Duration) int {
	a.t.Helper()
	ctx := context.Background()
	resp, err := a.up.PollCommands(ctx, wait)
	if err != nil {
		a.t.Fatalf("poll: %v", err)
	}
	for _, c := range resp.Commands {
		res, err := a.exec.Execute(ctx, c)
		if err != nil {
			a.t.Fatalf("execute: %v", err)
		}
		if err := a.up.CommandResult(ctx, c.ID, res); err != nil {
			a.t.Fatalf("result: %v", err)
		}
	}
	return len(resp.Commands)
}

func (a *fakeAgent) counts() (backups, cancels int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.backups, a.cancels
}

var opActor = store.AuditActor{Username: "operator", IP: "192.0.2.7", Method: "api"}

// queue is the operator's side: the store call both the admin API and the UI
// make, then the wake-up they perform (adminapi.Service.WakeAgent and
// ui.Service.WakeAgent are wired to Notifier.Notify).
func (h *harness) queue(dev, typ, key string, ttl time.Duration, params string) store.QueueResult {
	h.t.Helper()
	in := store.CommandInput{Type: typ, IdempotencyKey: key, TTL: ttl}
	if params != "" {
		in.Params = json.RawMessage(params)
	}
	res, err := h.st.QueueCommandAudited(context.Background(), store.ScopeAll(), dev, in, opActor)
	if err != nil {
		h.t.Fatal(err)
	}
	if res.Created {
		h.svc.Notifier.Notify(dev)
	}
	return res
}

func (h *harness) command(dev, id string) store.Command {
	h.t.Helper()
	l, err := h.st.ListCommands(context.Background(), store.ScopeAll(), dev, store.Page{Limit: 100})
	if err != nil {
		h.t.Fatal(err)
	}
	for _, c := range l.Items {
		if c.ID == id {
			return c
		}
	}
	h.t.Fatalf("command %s not found", id)
	return store.Command{}
}

// waitForPoll waits until the device's long poll is held by the service.
func (h *harness) waitForPoll(dev string) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if rel, ok := h.svc.pollFlight.TryAcquire(dev); ok {
			rel()
		} else {
			return
		}
		if time.Now().After(deadline) {
			h.t.Fatal("the long poll never started")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestBackUpNowReachesAnOnlineAgentWithinTheLongPoll(t *testing.T) {
	h := newHarness(t)
	e := h.enrolled()
	agent := h.newFakeAgent(string(e.DeviceToken))

	// The agent is online: it sits in a 30 s long poll (the agent's default wait).
	handed := make(chan int, 1)
	go func() { handed <- agent.poll(api.CommandsLongPollMax) }()
	h.waitForPoll(e.DeviceID)

	asked := time.Now()
	res := h.queue(e.DeviceID, "backup_now", "e2e-1", 0, "")
	if !res.Created {
		t.Fatalf("not queued: %+v", res)
	}
	select {
	case n := <-handed:
		if n != 1 {
			t.Fatalf("the agent was handed %d commands", n)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Back up now did not reach the online agent")
	}
	if took := time.Since(asked); took > 5*time.Second {
		t.Fatalf("delivered, started and answered in %v; the acceptance is 30 s and a woken poll takes milliseconds", took)
	}
	if b, _ := agent.counts(); b != 1 {
		t.Fatalf("the agent started %d backups, want 1", b)
	}
	got := h.command(e.DeviceID, res.Command.ID)
	if got.Status != "succeeded" || got.DeliveredAt == nil || got.CompletedAt == nil || string(got.Result) != `{"started":true}` {
		t.Fatalf("command after the round trip: %+v (result %s)", got, got.Result)
	}
	// It is not handed out again, and the operator sees it as done.
	if n := agent.poll(0); n != 0 {
		t.Fatalf("a finished command was delivered again (%d)", n)
	}
	if st := got.EffectiveStatus(h.clk.Now()); st != "succeeded" {
		t.Fatalf("effective status %s", st)
	}
	// One audit row for the operator's request; the agent's reports are not operator mutations.
	l, _ := h.st.ListAudit(context.Background(), store.ScopeAll(), store.AuditFilter{Action: "command.queue", ObjectID: e.DeviceID}, store.Page{Limit: 10})
	if len(l.Items) != 1 {
		t.Fatalf("audit rows = %d", len(l.Items))
	}
}

// Without the wake-up the command still arrives, at the end of the poll the
// agent already has open: never later than one poll length, which is why the
// wake-up is wired and why this bound is the floor, not the plan.
func TestBackUpNowWithoutAWakeUpArrivesWithinOnePollLength(t *testing.T) {
	h := newHarness(t)
	e := h.enrolled()
	agent := h.newFakeAgent(string(e.DeviceToken))
	const wait = 2 * time.Second
	handed := make(chan int, 1)
	go func() { handed <- agent.poll(wait) }()
	h.waitForPoll(e.DeviceID)
	asked := time.Now()
	res, err := h.st.QueueCommandAudited(context.Background(), store.ScopeAll(), e.DeviceID, store.CommandInput{Type: "backup_now", IdempotencyKey: "nowake"}, opActor)
	if err != nil || !res.Created {
		t.Fatal(err)
	}
	if n := <-handed; n != 0 {
		t.Fatalf("the open poll should end empty, got %d", n)
	}
	if n := agent.poll(wait); n != 1 {
		t.Fatalf("the next poll was handed %d commands", n)
	}
	if took := time.Since(asked); took > wait+3*time.Second {
		t.Fatalf("took %v, want within one poll length (%v) plus slack", took, wait)
	}
	if got := h.command(e.DeviceID, res.Command.ID); got.Status != "succeeded" {
		t.Fatalf("status %s", got.Status)
	}
}

func TestCancelBeforeDeliveryNeverReachesTheAgent(t *testing.T) {
	h := newHarness(t)
	e := h.enrolled()
	agent := h.newFakeAgent(string(e.DeviceToken))
	res := h.queue(e.DeviceID, "backup_now", "c-1", 0, "") // the device is offline: it is not polling
	c, changed, err := h.st.CancelCommandAudited(context.Background(), store.ScopeAll(), e.DeviceID, res.Command.ID, opActor)
	if err != nil || !changed || c.Status != "cancelled" {
		t.Fatalf("cancel: %+v %v %v", c, changed, err)
	}
	h.svc.Notifier.Notify(e.DeviceID)
	if n := agent.poll(0); n != 0 {
		t.Fatalf("a cancelled command was delivered (%d)", n)
	}
	if b, _ := agent.counts(); b != 0 {
		t.Fatalf("the agent started %d backups after the operator withdrew the request", b)
	}
	// A device that reports on a cancelled command (it cannot have received it) is answered 204 and ignored.
	r := h.do("POST", api.RouteCommandResult(res.Command.ID), string(e.DeviceToken), api.CommandResultRequest{Status: api.CommandSucceeded})
	if r.code != 204 {
		t.Fatalf("result for a cancelled command: %d", r.code)
	}
	if got := h.command(e.DeviceID, res.Command.ID); got.Status != "cancelled" {
		t.Fatalf("status %s", got.Status)
	}
}

func TestCancelRunningBackupIsHonouredByTheAgent(t *testing.T) {
	h := newHarness(t)
	e := h.enrolled()
	agent := h.newFakeAgent(string(e.DeviceToken))
	h.queue(e.DeviceID, "backup_now", "r-1", 0, "")
	if n := agent.poll(0); n != 1 {
		t.Fatalf("backup_now handed %d", n)
	}
	// The run is open; the operator cancels it.
	cancel := h.queue(e.DeviceID, "cancel", "r-2", 0, `{"run_id":"01J9Y4K3A0B1C2D3E4F5G6H7J1"}`)
	if !cancel.Created {
		t.Fatalf("cancel not queued: %+v", cancel)
	}
	if n := agent.poll(0); n != 1 {
		t.Fatalf("cancel handed %d", n)
	}
	if _, c := agent.counts(); c != 1 {
		t.Fatalf("the agent's cancel handler ran %d times, want 1", c)
	}
	got := h.command(e.DeviceID, cancel.Command.ID)
	if got.Status != "succeeded" || string(got.Result) != `{"cancelled":true}` {
		t.Fatalf("cancel result: %s %s", got.Status, got.Result)
	}
	// Cancelling again with nothing running is a success that says nothing was stopped.
	again := h.queue(e.DeviceID, "cancel", "r-3", 0, "")
	agent.poll(0)
	if got := h.command(e.DeviceID, again.Command.ID); got.Status != "succeeded" || string(got.Result) != `{"cancelled":false,"reason":"no_run"}` {
		t.Fatalf("cancel with no run: %s %s", got.Status, got.Result)
	}
	// Idempotent at the console: the same request while one is in flight adds none.
	h.queue(e.DeviceID, "backup_now", "r-4", 0, "")
	a := h.queue(e.DeviceID, "cancel", "r-5", 0, "")
	b := h.queue(e.DeviceID, "cancel", "r-6", 0, "")
	if b.Command.ID != a.Command.ID || !b.Coalesced {
		t.Fatalf("a second cancel while one is in flight: %+v", b)
	}
}

func TestExpiredCommandIsNeverDeliveredAndShowsAsExpired(t *testing.T) {
	h := newHarness(t)
	e := h.enrolled()
	agent := h.newFakeAgent(string(e.DeviceToken))
	res := h.queue(e.DeviceID, "backup_now", "x-1", time.Minute, "") // an offline device: nobody polls
	h.clk.Advance(2 * time.Minute)
	// Past its deadline but before the 60 s job: stored queued, read expired, not delivered.
	if st := h.command(e.DeviceID, res.Command.ID); st.Status != "queued" || st.EffectiveStatus(h.clk.Now()) != "expired" {
		t.Fatalf("stored %s, effective %s", st.Status, st.EffectiveStatus(h.clk.Now()))
	}
	if n := agent.poll(0); n != 0 {
		t.Fatalf("an expired command was delivered (%d)", n)
	}
	if b, _ := agent.counts(); b != 0 {
		t.Fatalf("an expired Back up now started %d backups", b)
	}
	// The command-expiry job records it.
	n, err := h.st.ExpireCommands(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("ExpireCommands = %d, %v", n, err)
	}
	if st := h.command(e.DeviceID, res.Command.ID); st.Status != "expired" {
		t.Fatalf("after the job: %s", st.Status)
	}
	// An expired command is no longer in flight: the operator can ask again.
	if again := h.queue(e.DeviceID, "backup_now", "x-2", 0, ""); !again.Created {
		t.Fatalf("a new request after expiry: %+v", again)
	}
}
