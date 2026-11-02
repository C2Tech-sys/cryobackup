package adminapi

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// ---- helpers (M2-W2C) ----

func (e *env) deviceNamed(host string) store.Device {
	e.t.Helper()
	l, err := e.st.ListDevices(context.Background(), store.ScopeAll(), store.ListDevicesOptions{Search: host, IncludeRetired: true})
	if err != nil || len(l.Items) != 1 {
		e.t.Fatalf("fixture device %s: %v", host, err)
	}
	return l.Items[0]
}

func (e *env) wakes() (*[]string, *sync.Mutex) {
	var mu sync.Mutex
	var got []string
	e.svc.WakeAgent = func(id string) { mu.Lock(); got = append(got, id); mu.Unlock() }
	return &got, &mu
}

func (e *env) openRun(dev string) store.Run {
	e.t.Helper()
	l, err := e.st.ListRuns(context.Background(), store.ScopeAll(), dev, store.Page{Limit: 20})
	if err != nil {
		e.t.Fatal(err)
	}
	for _, r := range l.Items {
		if r.Outcome == "" {
			return r
		}
	}
	e.t.Fatalf("device %s has no open run", dev)
	return store.Run{}
}

func (e *env) commandsOf(dev string) []store.Command {
	e.t.Helper()
	l, err := e.st.ListCommands(context.Background(), store.ScopeAll(), dev, store.Page{Limit: 200})
	if err != nil {
		e.t.Fatal(err)
	}
	return l.Items
}

func reasonOf(t *testing.T, b []byte) string {
	t.Helper()
	var p struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	return p.Reason
}

func TestCommandRolesAndCSRF(t *testing.T) {
	e := newEnv(t)
	dev := e.dev.ID
	idDev := e.deviceNamed("FRONTDESK-02").ID
	run := e.openRun(dev)
	order := []string{"read_only", "operator", "admin", "owner"}
	roles := map[string]*session{}
	for _, r := range order {
		e.addAdmin("c-"+r, r, nil)
		roles[r] = e.login("c-"+r, pw)
	}
	n := 0
	key := func() string { n++; return "matrix-" + strings.Repeat("x", n) }
	calls := []struct {
		name, method, path string
		body               func() any
		min                string
	}{
		{"list commands", "GET", "/api/v1/devices/" + dev + "/commands", nil, "read_only"},
		{"queue command", "POST", "/api/v1/devices/" + dev + "/commands", func() any { return QueueCommandRequest{Type: "check_in", IdempotencyKey: key()} }, "operator"},
		{"cancel command", "DELETE", "/api/v1/devices/" + dev + "/commands/01JNOSUCHCOMMAND00000000A", nil, "operator"},
		{"cancel run", "POST", "/api/v1/runs/" + run.ID + "/cancel", nil, "operator"},
		{"resolve identity", "POST", "/api/v1/devices/" + idDev + "/identity/resolve", func() any { return IdentityResolveRequest{Action: "keep", Reason: "matrix"} }, "admin"},
	}
	for _, c := range calls {
		for i, role := range order {
			var body any
			if c.body != nil {
				body = c.body()
			}
			res, b := e.req(c.method, c.path, roles[role], body)
			allowed := i >= indexOf(order, c.min)
			switch {
			case allowed && (res.StatusCode == 403 || res.StatusCode == 401):
				t.Errorf("%s as %s: refused with %d %s", c.name, role, res.StatusCode, b)
			case !allowed && (res.StatusCode != 403 || problemCode(t, res, b) != api.CodeForbidden):
				t.Errorf("%s as %s: want 403 forbidden, got %d %s", c.name, role, res.StatusCode, b)
			}
		}
	}
	// A cookie-authenticated write without the CSRF token queues nothing and audits nothing.
	s := roles["owner"]
	bad := &session{cookies: s.cookies, csrf: "wrong"}
	before, cmds := e.audits(), len(e.commandsOf(dev))
	for _, tc := range []struct{ method, path string }{
		{"POST", "/api/v1/devices/" + dev + "/commands"}, {"DELETE", "/api/v1/devices/" + dev + "/commands/x"},
		{"POST", "/api/v1/runs/" + run.ID + "/cancel"}, {"POST", "/api/v1/devices/" + idDev + "/identity/resolve"},
	} {
		res, b := e.req(tc.method, tc.path, bad, map[string]any{"type": "backup_now", "idempotency_key": "csrf"})
		if res.StatusCode != 403 || problemCode(t, res, b) != api.CodeForbidden {
			t.Errorf("%s %s with a bad CSRF token: %d", tc.method, tc.path, res.StatusCode)
		}
	}
	if e.audits() != before || len(e.commandsOf(dev)) != cmds {
		t.Fatal("a CSRF failure changed something")
	}
}

func TestQueueCommandShapeIdempotencyAndWake(t *testing.T) {
	e := newEnv(t)
	s := e.login("admin", pw)
	woke, mu := e.wakes()
	dev := e.deviceNamed("FRONTDESK-01").ID
	path := "/api/v1/devices/" + dev + "/commands"
	before := e.audits()

	res, b := e.req("POST", path, s, QueueCommandRequest{Type: "backup_now", IdempotencyKey: "k-1"})
	if res.StatusCode != 201 {
		t.Fatalf("queue: %d %s", res.StatusCode, b)
	}
	first := decode[CommandResponse](t, b)
	if first.ID == "" || first.DeviceID != dev || first.Type != "backup_now" || first.Status != "queued" || first.ComputedAt.IsZero() || first.Coalesced ||
		!first.ExpiresAt.After(first.CreatedAt) || string(first.Params) != `{}` {
		t.Fatalf("shape: %+v", first)
	}
	if got := first.ExpiresAt.Sub(first.CreatedAt); got != store.DefaultOperatorTTL("backup_now") {
		t.Fatalf("default lifetime %v", got)
	}
	mu.Lock()
	if len(*woke) != 1 || (*woke)[0] != dev {
		t.Fatalf("the device's long poll was not woken: %v", *woke)
	}
	mu.Unlock()
	if rows := e.auditOf("command.queue", dev); len(rows) != 1 || e.audits() != before+1 {
		t.Fatalf("audit rows: %d (total %d -> %d)", len(rows), before, e.audits())
	}
	// The same key, in the body or in the Idempotency-Key header: the same command, 200, no second wake or audit row.
	res, b = e.req("POST", path, s, QueueCommandRequest{Type: "backup_now", IdempotencyKey: "k-1"})
	if again := decode[CommandResponse](t, b); res.StatusCode != 200 || again.ID != first.ID {
		t.Fatalf("replay by body: %d %+v", res.StatusCode, again)
	}
	res, b = e.req("POST", path, s, map[string]any{"type": "backup_now"}, "Idempotency-Key", "k-1")
	if again := decode[CommandResponse](t, b); res.StatusCode != 200 || again.ID != first.ID {
		t.Fatalf("replay by header: %d %s", res.StatusCode, b)
	}
	// Another key for the same request while it is in flight coalesces.
	res, b = e.req("POST", path, s, map[string]any{"type": "backup_now"}, "Idempotency-Key", "k-2")
	if co := decode[CommandResponse](t, b); res.StatusCode != 200 || co.ID != first.ID || !co.Coalesced {
		t.Fatalf("coalesce: %d %s", res.StatusCode, b)
	}
	// The key reused for a different intent is a conflict.
	if res, b := e.req("POST", path, s, QueueCommandRequest{Type: "check_in", IdempotencyKey: "k-1"}); res.StatusCode != 409 || problemCode(t, res, b) != api.CodeConflict {
		t.Fatalf("reused key: %d %s", res.StatusCode, b)
	}
	// Header and body naming different keys is a validation error.
	res, b = e.req("POST", path, s, QueueCommandRequest{Type: "check_in", IdempotencyKey: "a"}, "Idempotency-Key", "b")
	if res.StatusCode != 400 || problemFields(t, b)["idempotency_key"] == "" {
		t.Fatalf("key mismatch: %d %s", res.StatusCode, b)
	}
	mu.Lock()
	if len(*woke) != 1 {
		t.Fatalf("only a new command wakes the device: %v", *woke)
	}
	mu.Unlock()
	if rows := e.auditOf("command.queue", dev); len(rows) != 1 {
		t.Fatalf("audit rows after replays = %d, want 1", len(rows))
	}
	// A custom lifetime.
	res, b = e.req("POST", path, s, QueueCommandRequest{Type: "refresh_policy", IdempotencyKey: "k-3", TTLSeconds: 600})
	if c := decode[CommandResponse](t, b); res.StatusCode != 201 || c.ExpiresAt.Sub(c.CreatedAt) != 10*time.Minute {
		t.Fatalf("ttl: %d %s", res.StatusCode, b)
	}
}

func TestQueueCommandValidationAndStateRefusals(t *testing.T) {
	e := newEnv(t)
	s := e.login("admin", pw)
	dev := e.deviceNamed("FRONTDESK-01").ID
	path := "/api/v1/devices/" + dev + "/commands"
	before := e.audits()
	for name, tc := range map[string]struct {
		body  any
		field string
	}{
		"unknown type":     {QueueCommandRequest{Type: "format_disk", IdempotencyKey: "v"}, "type"},
		"other milestone":  {QueueCommandRequest{Type: "uninstall", IdempotencyKey: "v"}, "type"},
		"no key":           {QueueCommandRequest{Type: "backup_now"}, "idempotency_key"},
		"ttl too short":    {QueueCommandRequest{Type: "backup_now", IdempotencyKey: "v", TTLSeconds: 5}, "ttl"},
		"ttl too long":     {QueueCommandRequest{Type: "backup_now", IdempotencyKey: "v", TTLSeconds: 30 * 86400}, "ttl"},
		"params on a type": {QueueCommandRequest{Type: "backup_now", IdempotencyKey: "v", Params: json.RawMessage(`{"path":"x"}`)}, "params"},
	} {
		res, b := e.req("POST", path, s, tc.body)
		if res.StatusCode != 400 || problemFields(t, b)[tc.field] == "" {
			t.Errorf("%s: %d %s, want a %q field error", name, res.StatusCode, b, tc.field)
		}
	}
	if res, _ := e.req("POST", path, s, `{"type":"backup_now","idempotency_key":"v","surprise":1}`); res.StatusCode != 400 {
		t.Errorf("unknown field: %d", res.StatusCode)
	}
	if e.audits() != before {
		t.Fatal("rejected requests wrote audit rows")
	}
	// A parked device is not backed up on demand (409 with a reason); a retired one takes nothing.
	parked := e.deviceNamed("XRAY-OLD-01").ID
	res, b := e.req("POST", "/api/v1/devices/"+parked+"/commands", s, QueueCommandRequest{Type: "backup_now", IdempotencyKey: "p"})
	if res.StatusCode != 409 || problemCode(t, res, b) != api.CodeConflict || reasonOf(t, b) != "device_parked" {
		t.Fatalf("parked: %d %s", res.StatusCode, b)
	}
	retired := e.deviceNamed("OLD-RECEPTION-PC").ID
	res, b = e.req("POST", "/api/v1/devices/"+retired+"/commands", s, QueueCommandRequest{Type: "check_in", IdempotencyKey: "r"})
	if res.StatusCode != 409 || reasonOf(t, b) != "device_retired" {
		t.Fatalf("retired: %d %s", res.StatusCode, b)
	}
	if res, b := e.req("POST", "/api/v1/devices/01JNOSUCHDEVICE0000000000/commands", s, QueueCommandRequest{Type: "check_in", IdempotencyKey: "n"}); res.StatusCode != 404 || problemCode(t, res, b) != api.CodeNotFound {
		t.Fatalf("unknown device: %d", res.StatusCode)
	}
}

func TestListCommandsShowsEffectiveStatusAndPages(t *testing.T) {
	e := newEnv(t)
	s := e.login("admin", pw)
	dev := e.deviceNamed("FRONTDESK-01").ID // the fixtures hold an expired back-up-now and a failed policy refresh here
	list := func(q string) CommandListResponse {
		code, b := e.get("/api/v1/devices/"+dev+"/commands"+q, s)
		if code != 200 {
			t.Fatalf("%s: %d %s", q, code, b)
		}
		return decode[CommandListResponse](t, b)
	}
	all := list("")
	if all.Total < 2 || all.ComputedAt.IsZero() || len(all.Commands) != all.Total {
		t.Fatalf("all: %+v", all)
	}
	statuses := map[string]bool{}
	for _, c := range all.Commands {
		statuses[c.Status] = true
	}
	if !statuses["expired"] || !statuses["failed"] {
		t.Fatalf("statuses %v", statuses)
	}
	if x := list("?status=expired"); x.Total != 1 || x.Commands[0].Status != "expired" {
		t.Fatalf("expired filter: %+v", x)
	}
	// A queued command past its deadline reads expired before the job records it.
	res, b := e.req("POST", "/api/v1/devices/"+dev+"/commands", s, QueueCommandRequest{Type: "check_in", IdempotencyKey: "late", TTLSeconds: 60})
	if res.StatusCode != 201 {
		t.Fatalf("%d %s", res.StatusCode, b)
	}
	queued := decode[CommandResponse](t, b)
	if x := list("?status=queued"); x.Total != 1 || x.Commands[0].ID != queued.ID {
		t.Fatalf("queued filter: %+v", x)
	}
	e.clk.Advance(2 * time.Minute)
	if x := list("?status=queued"); x.Total != 0 {
		t.Fatalf("an expired command is still listed as queued: %+v", x)
	}
	if x := list("?status=expired"); x.Total != 2 {
		t.Fatalf("expired after the deadline: %+v", x)
	}
	// The JSON says expired for it, though the stored status is still queued until the job ticks.
	for _, c := range list("").Commands {
		if c.ID == queued.ID && c.Status != "expired" {
			t.Fatalf("a command past its deadline is reported as %s, want expired", c.Status)
		}
	}
	if code, _ := e.get("/api/v1/devices/"+dev+"/commands?status=bogus", s); code != 400 {
		t.Fatalf("bad status: %d", code)
	}
	p1 := list("?limit=1")
	if len(p1.Commands) != 1 || !p1.Truncated || p1.NextCursor != "o1" {
		t.Fatalf("page 1: %+v", p1)
	}
	if p2 := list("?limit=1&cursor=" + p1.NextCursor); len(p2.Commands) != 1 || p2.Commands[0].ID == p1.Commands[0].ID {
		t.Fatalf("page 2: %+v", p2)
	}
}

func TestCancelCommandIsIdempotentAndOnlyBeforeDelivery(t *testing.T) {
	e := newEnv(t)
	s := e.login("admin", pw)
	dev := e.deviceNamed("FRONTDESK-01").ID
	res, b := e.req("POST", "/api/v1/devices/"+dev+"/commands", s, QueueCommandRequest{Type: "backup_now", IdempotencyKey: "c-1"})
	if res.StatusCode != 201 {
		t.Fatalf("%d %s", res.StatusCode, b)
	}
	id := decode[CommandResponse](t, b).ID
	path := "/api/v1/devices/" + dev + "/commands/" + id
	for i := 0; i < 2; i++ {
		if res, b := e.req("DELETE", path, s, nil); res.StatusCode != 204 {
			t.Fatalf("cancel #%d: %d %s", i+1, res.StatusCode, b)
		}
	}
	if rows := e.auditOf("command.cancel", dev); len(rows) != 1 {
		t.Fatalf("audit rows = %d, want 1", len(rows))
	}
	_, b = e.req("GET", "/api/v1/devices/"+dev+"/commands?status=cancelled", s, nil)
	if l := decode[CommandListResponse](t, b); l.Total != 1 || l.Commands[0].ID != id {
		t.Fatalf("cancelled list: %s", b)
	}
	// Delivered: too late, 409 with its reason.
	_, b = e.req("POST", "/api/v1/devices/"+dev+"/commands", s, QueueCommandRequest{Type: "backup_now", IdempotencyKey: "c-2"})
	id2 := decode[CommandResponse](t, b).ID
	if got, err := e.st.AgentNextCommands(context.Background(), dev, 5); err != nil || len(got) == 0 {
		t.Fatalf("delivery: %v %v", got, err)
	}
	res, b = e.req("DELETE", "/api/v1/devices/"+dev+"/commands/"+id2, s, nil)
	if res.StatusCode != 409 || reasonOf(t, b) != "command_delivered" {
		t.Fatalf("cancel of a delivered command: %d %s", res.StatusCode, b)
	}
	if rows := e.auditOf("command.cancel", dev); len(rows) != 1 {
		t.Fatalf("a refused cancel wrote an audit row (%d)", len(rows))
	}
	// Unknown id: 404.
	if res, _ := e.req("DELETE", "/api/v1/devices/"+dev+"/commands/01JNOSUCHCOMMAND00000000A", s, nil); res.StatusCode != 404 {
		t.Fatalf("unknown command: %d", res.StatusCode)
	}
}

func TestCancelRunQueuesACancelCommand(t *testing.T) {
	e := newEnv(t)
	s := e.login("admin", pw)
	woke, mu := e.wakes()
	run := e.openRun(e.dev.ID)
	res, b := e.req("POST", "/api/v1/runs/"+run.ID+"/cancel", s, nil)
	if res.StatusCode != 201 {
		t.Fatalf("cancel run: %d %s", res.StatusCode, b)
	}
	c := decode[CommandResponse](t, b)
	if c.Type != "cancel" || c.DeviceID != e.dev.ID || c.Status != "queued" || !strings.Contains(string(c.Params), run.ID) {
		t.Fatalf("command: %+v", c)
	}
	mu.Lock()
	if len(*woke) != 1 {
		t.Fatalf("wake: %v", *woke)
	}
	mu.Unlock()
	// A second request while the first is in flight returns it (idempotent), with no second row.
	res, b = e.req("POST", "/api/v1/runs/"+run.ID+"/cancel", s, nil)
	if again := decode[CommandResponse](t, b); res.StatusCode != 200 || again.ID != c.ID || !again.Coalesced {
		t.Fatalf("repeat: %d %s", res.StatusCode, b)
	}
	if rows := e.auditOf("command.queue", e.dev.ID); len(rows) != 1 {
		t.Fatalf("audit rows = %d", len(rows))
	}
	// A finished run has nothing to cancel.
	l, _ := e.st.ListRuns(context.Background(), store.ScopeAll(), e.dev.ID, store.Page{Limit: 50})
	var done string
	for _, r := range l.Items {
		if r.Outcome != "" && r.Kind == "backup" {
			done = r.ID
			break
		}
	}
	res, b = e.req("POST", "/api/v1/runs/"+done+"/cancel", s, nil)
	if res.StatusCode != 409 || reasonOf(t, b) != "run_finished" {
		t.Fatalf("finished run: %d %s", res.StatusCode, b)
	}
	if res, _ := e.req("POST", "/api/v1/runs/01JNOSUCHRUN000000000000A/cancel", s, nil); res.StatusCode != 404 {
		t.Fatalf("unknown run: %d", res.StatusCode)
	}
}

func TestCommandRoutesAreScopedNotFoundNotForbidden(t *testing.T) {
	e := newEnv(t)
	harbor := e.customerNamed("Harbor Family Practice")
	e.addAdmin("harbor-op", "operator", []string{harbor.ID})
	owner := e.login("admin", pw)
	op := e.login("harbor-op", pw)
	foreign := e.deviceNamed("FRONTDESK-01").ID
	_, b := e.req("POST", "/api/v1/devices/"+foreign+"/commands", owner, QueueCommandRequest{Type: "backup_now", IdempotencyKey: "theirs"})
	theirs := decode[CommandResponse](t, b).ID
	run := e.openRun(e.dev.ID) // DR-OFFICE-PC is a Summit device too
	e.wakes()
	audits, cmds := e.audits(), len(e.commandsOf(foreign))
	for _, tc := range []struct {
		method, path string
		body         any
	}{
		{"GET", "/api/v1/devices/" + foreign + "/commands", nil},
		{"POST", "/api/v1/devices/" + foreign + "/commands", QueueCommandRequest{Type: "backup_now", IdempotencyKey: "x"}},
		{"DELETE", "/api/v1/devices/" + foreign + "/commands/" + theirs, nil},
		{"POST", "/api/v1/runs/" + run.ID + "/cancel", nil},
	} {
		res, b := e.req(tc.method, tc.path, op, tc.body)
		if res.StatusCode != 404 || problemCode(t, res, b) != api.CodeNotFound {
			t.Errorf("%s %s as a Harbor operator: %d, want 404 not_found", tc.method, tc.path, res.StatusCode)
		}
	}
	if got := len(e.commandsOf(foreign)); got != cmds || e.audits() != audits {
		t.Errorf("denied requests changed data: commands %d -> %d, audit %d -> %d", cmds, got, audits, e.audits())
	}
	for _, c := range e.commandsOf(foreign) {
		if c.ID == theirs && c.Status != "queued" {
			t.Errorf("a foreign command became %s", c.Status)
		}
	}
	// Inside its own scope the same operator works.
	own := e.deviceNamed("FRONTDESK-H1").ID
	if res, b := e.req("POST", "/api/v1/devices/"+own+"/commands", op, QueueCommandRequest{Type: "backup_now", IdempotencyKey: "mine"}); res.StatusCode != 201 {
		t.Errorf("own device: %d %s", res.StatusCode, b)
	}
}

func TestResolveIdentityOverTheAPI(t *testing.T) {
	e := newEnv(t)
	s := e.login("admin", pw)
	syncs := 0
	e.svc.SyncGateway = func() { syncs++ }
	dev := e.deviceNamed("FRONTDESK-02").ID
	path := "/api/v1/devices/" + dev + "/identity/resolve"
	before := e.audits()
	for name, body := range map[string]any{
		"no reason":   IdentityResolveRequest{Action: "keep"},
		"bad action":  IdentityResolveRequest{Action: "ignore", Reason: "x"},
		"path reason": IdentityResolveRequest{Action: "keep", Reason: `from C:\Users\bob`},
	} {
		if res, b := e.req("POST", path, s, body); res.StatusCode != 400 || len(problemFields(t, b)) == 0 {
			t.Errorf("%s: %d %s", name, res.StatusCode, b)
		}
	}
	if e.audits() != before {
		t.Fatal("rejections wrote audit rows")
	}
	res, b := e.req("POST", path, s, IdentityResolveRequest{Action: "keep", Reason: "hardware swap confirmed"})
	if r := decode[IdentityResolveResponse](t, b); res.StatusCode != 200 || r.ID != dev || r.Resolution != "keep" || r.Retired {
		t.Fatalf("keep: %d %s", res.StatusCode, b)
	}
	if rows := e.auditOf("device.identity_resolve", dev); len(rows) != 1 || rows[0].Severity != "warning" {
		t.Fatalf("audit rows: %+v", rows)
	}
	res, b = e.req("POST", path, s, IdentityResolveRequest{Action: "keep", Reason: "again"})
	if res.StatusCode != 409 || reasonOf(t, b) != "no_identity_conflict" {
		t.Fatalf("second resolve: %d %s", res.StatusCode, b)
	}
	if rows := e.auditOf("device.identity_resolve", dev); len(rows) != 1 {
		t.Fatalf("audit rows = %d", len(rows))
	}
	// The device no longer reads as an identity conflict.
	_, b = e.req("GET", "/api/v1/devices/"+dev, s, nil)
	if d := decode[DeviceDetailJSON](t, b); d.State == "IDENTITY_CONFLICT" {
		t.Fatalf("still in conflict: %s", d.State)
	}
	// Retire: a second flagged device.
	other := e.deviceNamed("LT-OPTOM-04").ID
	if err := e.st.Write(context.Background(), func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE devices SET identity_conflict_at = ? WHERE id = ?`, e.clk.Now().UnixMilli(), other)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	res, b = e.req("POST", "/api/v1/devices/"+other+"/identity/resolve", s, IdentityResolveRequest{Action: "retire", Reason: "clone of a restored image"})
	if r := decode[IdentityResolveResponse](t, b); res.StatusCode != 200 || !r.Retired {
		t.Fatalf("retire: %d %s", res.StatusCode, b)
	}
	if syncs != 1 {
		t.Fatalf("the gateway sync ran %d times after one keep and one retire, want 1 (retire only)", syncs)
	}
}
