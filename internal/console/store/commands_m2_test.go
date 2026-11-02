package store

import (
	"errors"
	"strings"
	"testing"
	"time"
)

var wActor = AuditActor{AdminID: "", Username: "tester", IP: "192.0.2.10", Method: "api"}

func (w *world) cmdAuditCount(action, objectID string) int {
	w.t.Helper()
	l := must[Listing[AuditEvent]](w.t)(w.s.ListAudit(w.ctx, ScopeAll(), AuditFilter{Action: action, ObjectID: objectID}, Page{Limit: 100}))
	return len(l.Items)
}

func queue(w *world, scope Scope, dev, typ, key string) (QueueResult, error) {
	return w.s.QueueCommandAudited(w.ctx, scope, dev, CommandInput{Type: typ, IdempotencyKey: key}, wActor)
}

func TestQueueCommandAuditedQueuesOnceWithOneAuditRow(t *testing.T) {
	w := seed(t)
	res := must[QueueResult](t)(queue(w, ScopeAll(), w.devA.ID, "backup_now", "k1"))
	if !res.Created || res.Coalesced || res.Command.Status != "queued" || res.Command.Type != "backup_now" {
		t.Fatalf("first queue: %+v", res)
	}
	if got := res.Command.ExpiresAt.Sub(res.Command.CreatedAt); got != DefaultOperatorTTL("backup_now") {
		t.Fatalf("ttl = %v", got)
	}
	if n := w.cmdAuditCount("command.queue", w.devA.ID); n != 1 {
		t.Fatalf("audit rows = %d, want 1", n)
	}
	ev := must[Listing[AuditEvent]](t)(w.s.ListAudit(w.ctx, ScopeAll(), AuditFilter{Action: "command.queue"}, Page{Limit: 5})).Items[0]
	if ev.ObjectType != "device" || ev.CustomerID != w.custA.ID || ev.ActorLabel != "tester" || ev.Result != "ok" || ev.Method != "api" {
		t.Fatalf("audit row: %+v", ev)
	}
	// Replay of the same key: the same command, no second audit row.
	again := must[QueueResult](t)(queue(w, ScopeAll(), w.devA.ID, "backup_now", "k1"))
	if again.Created || again.Command.ID != res.Command.ID {
		t.Fatalf("replay: %+v", again)
	}
	// A different key while the first is still in flight coalesces onto it.
	co := must[QueueResult](t)(queue(w, ScopeAll(), w.devA.ID, "backup_now", "k2"))
	if co.Created || !co.Coalesced || co.Command.ID != res.Command.ID {
		t.Fatalf("coalesce: %+v", co)
	}
	if n := w.cmdAuditCount("command.queue", w.devA.ID); n != 1 {
		t.Fatalf("audit rows after replay and coalesce = %d, want 1", n)
	}
	// The same key for a different intent is refused.
	if _, err := queue(w, ScopeAll(), w.devA.ID, "check_in", "k1"); !errors.Is(err, ErrConflict) {
		t.Fatalf("reused key: %v", err)
	}
	// Once the first has finished, a new request is a new command.
	w.clk.Advance(time.Second)
	cmds := must[[]Command](t)(w.s.AgentNextCommands(w.ctx, w.devA.ID, 10))
	for _, c := range cmds {
		if c.ID == res.Command.ID {
			must[Command](t)(w.s.AgentAckCommand(w.ctx, w.devA.ID, c.ID, CommandAck{Status: "succeeded"}))
		}
	}
	next := must[QueueResult](t)(queue(w, ScopeAll(), w.devA.ID, "backup_now", "k3"))
	if !next.Created || next.Command.ID == res.Command.ID {
		t.Fatalf("after completion: %+v", next)
	}
}

func TestQueueCommandAuditedValidates(t *testing.T) {
	w := seed(t)
	cases := []struct {
		name  string
		in    CommandInput
		field string
	}{
		{"unknown type", CommandInput{Type: "format_disk", IdempotencyKey: "k"}, "type"},
		{"type for another milestone", CommandInput{Type: "uninstall", IdempotencyKey: "k"}, "type"},
		{"no key", CommandInput{Type: "backup_now"}, "idempotency_key"},
		{"long key", CommandInput{Type: "backup_now", IdempotencyKey: strings.Repeat("k", 129)}, "idempotency_key"},
		{"ttl too short", CommandInput{Type: "backup_now", IdempotencyKey: "k", TTL: time.Second}, "ttl"},
		{"ttl too long", CommandInput{Type: "backup_now", IdempotencyKey: "k", TTL: 30 * 24 * time.Hour}, "ttl"},
		{"params on backup_now", CommandInput{Type: "backup_now", IdempotencyKey: "k", Params: []byte(`{"path":"C:\\Users"}`)}, "params"},
		{"unknown cancel param", CommandInput{Type: "cancel", IdempotencyKey: "k", Params: []byte(`{"files":"x"}`)}, "params"},
		{"run id that is a path", CommandInput{Type: "cancel", IdempotencyKey: "k", Params: []byte(`{"run_id":"C:\\Users\\x"}`)}, "params"},
		{"params not an object", CommandInput{Type: "cancel", IdempotencyKey: "k", Params: []byte(`[1]`)}, "params"},
	}
	for _, c := range cases {
		_, err := w.s.QueueCommandAudited(w.ctx, ScopeAll(), w.devA.ID, c.in, wActor)
		if !errors.Is(err, ErrInvalid) || len(FieldErrors(err)) == 0 || FieldErrors(err)[0].Field != c.field {
			t.Errorf("%s: err = %v, want a %q field error", c.name, err, c.field)
		}
	}
	if n := w.cmdAuditCount("command.queue", w.devA.ID); n != 0 {
		t.Fatalf("a rejected request wrote %d audit rows", n)
	}
	// A cancel may name its run.
	res := must[QueueResult](t)(w.s.QueueCommandAudited(w.ctx, ScopeAll(), w.devA.ID,
		CommandInput{Type: "cancel", IdempotencyKey: "c1", Params: []byte(`{"run_id":"` + runID1 + `"}`)}, wActor))
	if !res.Created || !strings.Contains(string(res.Command.Params), runID1) {
		t.Fatalf("cancel with run: %+v", res)
	}
	// A different run is a different intent: not coalesced.
	other := must[QueueResult](t)(w.s.QueueCommandAudited(w.ctx, ScopeAll(), w.devA.ID,
		CommandInput{Type: "cancel", IdempotencyKey: "c2", Params: []byte(`{"run_id":"` + runID2 + `"}`)}, wActor))
	if !other.Created {
		t.Fatalf("cancel of another run was coalesced: %+v", other)
	}
}

func TestQueueCommandAuditedRefusesRetiredAndParkedDevices(t *testing.T) {
	w := seed(t)
	// Parked: backup_now is refused with a reason, a policy refresh is not.
	must[Device](t)(w.s.UpdateDevice(w.ctx, ScopeAll(), w.devA.ID, DeviceUpdate{Pause: &PauseSpec{Reason: "awaiting hardware"}}))
	if _, err := queue(w, ScopeAll(), w.devA.ID, "backup_now", "p1"); !errors.Is(err, ErrDeviceParked) || !errors.Is(err, ErrConflict) {
		t.Fatalf("parked backup_now: %v", err)
	}
	if res := must[QueueResult](t)(queue(w, ScopeAll(), w.devA.ID, "refresh_policy", "p2")); !res.Created {
		t.Fatalf("parked refresh_policy: %+v", res)
	}
	// Retired: nothing is queued.
	must[Device](t)(w.s.UpdateDevice(w.ctx, ScopeAll(), w.devB.ID, DeviceUpdate{Retire: &RetireSpec{Reason: "replaced"}}))
	if _, err := queue(w, ScopeAll(), w.devB.ID, "check_in", "r1"); !errors.Is(err, ErrDeviceRetired) {
		t.Fatalf("retired: %v", err)
	}
	if n := w.cmdAuditCount("command.queue", w.devB.ID); n != 0 {
		t.Fatalf("refusals wrote %d audit rows", n)
	}
}

func TestQueueCommandAuditedIsScoped(t *testing.T) {
	w := seed(t)
	if _, err := queue(w, ScopeOf(w.custA.ID), w.devB.ID, "backup_now", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("A's operator queueing for B's device: %v, want ErrNotFound", err)
	}
	if _, err := queue(w, Scope{}, w.devA.ID, "backup_now", "x"); !errors.Is(err, ErrScope) {
		t.Fatalf("zero scope: %v", err)
	}
	l := must[Listing[Command]](t)(w.s.ListCommands(w.ctx, ScopeAll(), w.devB.ID, Page{Limit: 50}))
	for _, c := range l.Items {
		if c.Type == "backup_now" {
			t.Fatal("a denied request left a command on the other customer's device")
		}
	}
}

func TestCancelCommandAudited(t *testing.T) {
	w := seed(t)
	q := must[QueueResult](t)(queue(w, ScopeAll(), w.devA.ID, "backup_now", "k1"))
	c, changed, err := w.s.CancelCommandAudited(w.ctx, ScopeAll(), w.devA.ID, q.Command.ID, wActor)
	if err != nil || !changed || c.Status != "cancelled" || c.CompletedAt == nil {
		t.Fatalf("cancel: %+v changed=%v err=%v", c, changed, err)
	}
	if n := w.cmdAuditCount("command.cancel", w.devA.ID); n != 1 {
		t.Fatalf("audit rows = %d, want 1", n)
	}
	// Idempotent: again is a success that changes and audits nothing.
	c, changed, err = w.s.CancelCommandAudited(w.ctx, ScopeAll(), w.devA.ID, q.Command.ID, wActor)
	if err != nil || changed || c.Status != "cancelled" {
		t.Fatalf("second cancel: %+v changed=%v err=%v", c, changed, err)
	}
	if n := w.cmdAuditCount("command.cancel", w.devA.ID); n != 1 {
		t.Fatalf("audit rows after the second cancel = %d, want 1", n)
	}
	// The agent never receives it and cannot report on it.
	for _, d := range must[[]Command](t)(w.s.AgentNextCommands(w.ctx, w.devA.ID, 50)) {
		if d.ID == q.Command.ID {
			t.Fatal("a cancelled command was delivered")
		}
	}
	if _, err := w.s.AgentAckCommand(w.ctx, w.devA.ID, q.Command.ID, CommandAck{Status: "succeeded"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("ack of a cancelled command: %v", err)
	}
	// A new request after a cancel is a new command (the cancelled one is not in flight).
	if n := must[QueueResult](t)(queue(w, ScopeAll(), w.devA.ID, "backup_now", "k2")); !n.Created {
		t.Fatalf("queue after cancel: %+v", n)
	}
}

func TestCancelCommandAuditedRefusesDeliveredAndExpired(t *testing.T) {
	w := seed(t)
	q := must[QueueResult](t)(queue(w, ScopeAll(), w.devA.ID, "backup_now", "k1"))
	delivered := must[[]Command](t)(w.s.AgentNextCommands(w.ctx, w.devA.ID, 50))
	if len(delivered) == 0 {
		t.Fatal("nothing delivered")
	}
	before := w.cmdAuditCount("command.cancel", w.devA.ID)
	if _, _, err := w.s.CancelCommandAudited(w.ctx, ScopeAll(), w.devA.ID, q.Command.ID, wActor); !errors.Is(err, ErrCommandDelivered) {
		t.Fatalf("cancel of a delivered command: %v", err)
	}
	// A queued command whose time ran out before the job marked it is expired, not cancellable.
	e := must[QueueResult](t)(queue(w, ScopeAll(), w.devA.ID, "check_in", "k2"))
	w.clk.Advance(DefaultOperatorTTL("check_in") + time.Minute)
	if _, _, err := w.s.CancelCommandAudited(w.ctx, ScopeAll(), w.devA.ID, e.Command.ID, wActor); !errors.Is(err, ErrCommandExpired) {
		t.Fatalf("cancel of an expired command: %v", err)
	}
	if n := w.cmdAuditCount("command.cancel", w.devA.ID); n != before {
		t.Fatalf("refusals wrote audit rows: %d -> %d", before, n)
	}
}

func TestCancelCommandAuditedIsScoped(t *testing.T) {
	w := seed(t)
	q := must[QueueResult](t)(queue(w, ScopeAll(), w.devB.ID, "backup_now", "k1"))
	if _, _, err := w.s.CancelCommandAudited(w.ctx, ScopeOf(w.custA.ID), w.devB.ID, q.Command.ID, wActor); !errors.Is(err, ErrNotFound) {
		t.Fatalf("A cancelling B's command: %v, want ErrNotFound", err)
	}
	// A command id from another device, addressed through an allowed device.
	if _, _, err := w.s.CancelCommandAudited(w.ctx, ScopeAll(), w.devA.ID, q.Command.ID, wActor); !errors.Is(err, ErrNotFound) {
		t.Fatalf("B's command through A's device: %v, want ErrNotFound", err)
	}
	if got := must[Command](t)(firstCommand(w, w.devB.ID, "backup_now")); got.Status != "queued" {
		t.Fatalf("a denied cancel changed the command: %s", got.Status)
	}
}

func firstCommand(w *world, dev, typ string) (Command, error) {
	l, err := w.s.ListCommands(w.ctx, ScopeAll(), dev, Page{Limit: 100})
	if err != nil {
		return Command{}, err
	}
	for _, c := range l.Items {
		if c.Type == typ {
			return c, nil
		}
	}
	return Command{}, ErrNotFound
}

func TestEffectiveStatusShowsExpiredBeforeTheJobRuns(t *testing.T) {
	w := seed(t)
	q := must[QueueResult](t)(queue(w, ScopeAll(), w.devA.ID, "backup_now", "k1"))
	if got := q.Command.EffectiveStatus(w.clk.Now()); got != "queued" {
		t.Fatalf("fresh: %s", got)
	}
	if !q.Command.Pending(w.clk.Now()) {
		t.Fatal("a fresh queued command is pending")
	}
	w.clk.Advance(DefaultOperatorTTL("backup_now") + time.Second)
	stored := must[Command](t)(firstCommand(w, w.devA.ID, "backup_now"))
	if stored.Status != "queued" {
		t.Fatalf("the job has not run, the stored status is still queued, got %s", stored.Status)
	}
	if got := stored.EffectiveStatus(w.clk.Now()); got != "expired" {
		t.Fatalf("effective status after the deadline = %s, want expired", got)
	}
	if stored.Pending(w.clk.Now()) {
		t.Fatal("an expired command is not pending")
	}
	// A finished command never becomes expired by the clock.
	done := stored
	done.Status = "succeeded"
	if got := done.EffectiveStatus(w.clk.Now()); got != "succeeded" {
		t.Fatalf("succeeded: %s", got)
	}
	// The job agrees.
	if n := must[int64](t)(w.s.ExpireCommands(w.ctx)); n < 1 {
		t.Fatalf("ExpireCommands = %d", n)
	}
	if got := must[Command](t)(firstCommand(w, w.devA.ID, "backup_now")).Status; got != "expired" {
		t.Fatalf("after the job: %s", got)
	}
}

func TestListDeviceCommandsFiltersByEffectiveStatus(t *testing.T) {
	w := seed(t)
	a := must[QueueResult](t)(queue(w, ScopeAll(), w.devA.ID, "check_in", "k1"))   // will expire (15 min)
	b := must[QueueResult](t)(queue(w, ScopeAll(), w.devA.ID, "backup_now", "k2")) // stays queued (4 h)
	c := must[QueueResult](t)(queue(w, ScopeAll(), w.devA.ID, "refresh_policy", "k3"))
	if _, _, err := w.s.CancelCommandAudited(w.ctx, ScopeAll(), w.devA.ID, c.Command.ID, wActor); err != nil {
		t.Fatal(err)
	}
	w.clk.Advance(30 * time.Minute)
	list := func(f CommandFilter) []string {
		p := must[CommandPage](t)(w.s.ListDeviceCommands(w.ctx, ScopeAll(), w.devA.ID, f, w.clk.Now(), 1, 50, 0, 0))
		var ids []string
		for _, it := range p.Items {
			ids = append(ids, it.ID)
		}
		return ids
	}
	if got := list(CommandFilter{}); len(got) != 3 || got[0] != c.Command.ID {
		t.Fatalf("all, newest first: %v", got)
	}
	if got := list(CommandFilter{Status: "expired"}); len(got) != 1 || got[0] != a.Command.ID {
		t.Fatalf("expired (by clock, job not run): %v", got)
	}
	if got := list(CommandFilter{Status: "queued"}); len(got) != 1 || got[0] != b.Command.ID {
		t.Fatalf("queued: %v", got)
	}
	if got := list(CommandFilter{Status: "cancelled"}); len(got) != 1 || got[0] != c.Command.ID {
		t.Fatalf("cancelled: %v", got)
	}
	if got := list(CommandFilter{Type: "backup_now"}); len(got) != 1 {
		t.Fatalf("type: %v", got)
	}
	if _, err := w.s.ListDeviceCommands(w.ctx, ScopeAll(), w.devA.ID, CommandFilter{Status: "bogus"}, w.clk.Now(), 1, 50, 0, 0); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad status: %v", err)
	}
	// Scope: B's device is not A's to list (404, not an empty list).
	if _, err := w.s.ListDeviceCommands(w.ctx, ScopeOf(w.custA.ID), w.devB.ID, CommandFilter{}, w.clk.Now(), 1, 50, 0, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other customer's device: %v", err)
	}
	// Windowed (API cursor) paging.
	p := must[CommandPage](t)(w.s.ListDeviceCommands(w.ctx, ScopeAll(), w.devA.ID, CommandFilter{}, w.clk.Now(), 0, 0, 1, 1))
	if len(p.Items) != 1 || p.Page.Total != 3 || p.Page.From != 2 {
		t.Fatalf("window: %+v", p.Page)
	}
}

func TestFixtureCommandsAreSeeded(t *testing.T) {
	s, clk := newStore(t)
	ok(t, LoadFixtures(t.Context(), s, clk.Now()))
	devs := must[[]Device](t)(devicesOf(t, s))
	seen := map[string]int{}
	for _, d := range devs {
		l := must[Listing[Command]](t)(s.ListCommands(t.Context(), ScopeAll(), d.ID, Page{Limit: 50}))
		for _, c := range l.Items {
			seen[c.EffectiveStatus(clk.Now())]++
		}
	}
	for _, st := range []string{"queued", "succeeded", "expired", "failed", "cancelled"} {
		if seen[st] == 0 {
			t.Errorf("the fixtures hold no %s command (%v)", st, seen)
		}
	}
}

func devicesOf(t *testing.T, s *Store) ([]Device, error) {
	l, err := s.ListDevices(t.Context(), ScopeAll(), ListDevicesOptions{IncludeRetired: true, Page: Page{Limit: 200}})
	return l.Items, err
}
