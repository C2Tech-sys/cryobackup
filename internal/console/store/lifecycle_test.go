package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// commands
// ---------------------------------------------------------------------------

func TestCommandLifecycle(t *testing.T) {
	w := seed(t)
	ctx := context.Background()
	in := CommandInput{Type: "backup_now", Params: json.RawMessage(`{"reason":"manual"}`), IdempotencyKey: "k1", CreatedBy: w.admin.ID}
	c1, created, err := w.s.EnqueueCommand(ctx, ScopeAll(), w.devA.ID, in)
	ok(t, err)
	if !created || c1.Status != "queued" || !c1.ExpiresAt.Equal(t0.Add(24*time.Hour)) {
		t.Fatalf("command = %+v created=%v", c1, created)
	}
	// Same key, same content: the same command, not created.
	c1b, created, err := w.s.EnqueueCommand(ctx, ScopeAll(), w.devA.ID, in)
	ok(t, err)
	if created || c1b.ID != c1.ID {
		t.Fatalf("idempotent enqueue created=%v id %s vs %s", created, c1b.ID, c1.ID)
	}
	// Same key, different intent: conflict.
	diff := in
	diff.Params = json.RawMessage(`{"reason":"other"}`)
	if _, _, err := w.s.EnqueueCommand(ctx, ScopeAll(), w.devA.ID, diff); !errors.Is(err, ErrConflict) {
		t.Fatalf("reused key with other params = %v, want ErrConflict", err)
	}
	// The same key on another device is a different command.
	if _, created, err := w.s.EnqueueCommand(ctx, ScopeAll(), w.devB.ID, in); err != nil || !created {
		t.Fatalf("same key on another device: created=%v err=%v", created, err)
	}

	// Delivery marks it delivered once; the second poll is empty.
	got := must[[]Command](t)(w.s.AgentNextCommands(ctx, w.devA.ID, 10))
	if len(got) != 1 || got[0].ID != c1.ID || got[0].Status != "delivered" || got[0].DeliveredAt == nil {
		t.Fatalf("first poll = %+v", got)
	}
	if again := must[[]Command](t)(w.s.AgentNextCommands(ctx, w.devA.ID, 10)); len(again) != 0 {
		t.Fatalf("second poll returned %d commands", len(again))
	}
	// An unacknowledged delivery is handed out again after the redelivery delay.
	w.clk.Advance(3 * time.Minute)
	if again := must[[]Command](t)(w.s.AgentNextCommands(ctx, w.devA.ID, 10)); len(again) != 1 {
		t.Fatalf("redelivery returned %d commands, want 1", len(again))
	}

	// Ack path, idempotent, and no going backwards.
	acked := must[Command](t)(w.s.AgentAckCommand(ctx, w.devA.ID, c1.ID, CommandAck{Status: "acked"}))
	if acked.Status != "acked" || acked.AckedAt == nil {
		t.Fatalf("acked = %+v", acked)
	}
	must[Command](t)(w.s.AgentAckCommand(ctx, w.devA.ID, c1.ID, CommandAck{Status: "acked"}))
	done := must[Command](t)(w.s.AgentAckCommand(ctx, w.devA.ID, c1.ID, CommandAck{Status: "succeeded", Result: json.RawMessage(`{"files":12}`)}))
	if done.Status != "succeeded" || done.CompletedAt == nil || string(done.Result) != `{"files":12}` {
		t.Fatalf("done = %+v", done)
	}
	must[Command](t)(w.s.AgentAckCommand(ctx, w.devA.ID, c1.ID, CommandAck{Status: "succeeded"})) // repeat: no-op
	if _, err := w.s.AgentAckCommand(ctx, w.devA.ID, c1.ID, CommandAck{Status: "failed"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("succeeded -> failed = %v, want ErrConflict", err)
	}
	if _, err := w.s.AgentAckCommand(ctx, w.devA.ID, c1.ID, CommandAck{Status: "bogus"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bogus status = %v, want ErrInvalid", err)
	}
	// Another device cannot touch it.
	if _, err := w.s.AgentAckCommand(ctx, w.devB.ID, c1.ID, CommandAck{Status: "acked"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ack from another device = %v, want ErrNotFound", err)
	}
	list := must[Listing[Command]](t)(w.s.ListCommands(ctx, ScopeAll(), w.devA.ID, Page{}))
	if len(list.Items) != 1 {
		t.Fatalf("ListCommands = %d", len(list.Items))
	}
}

func TestCommandExpiry(t *testing.T) {
	w := seed(t)
	ctx := context.Background()
	c, _, err := w.s.EnqueueCommand(ctx, ScopeAll(), w.devA.ID, CommandInput{Type: "check_in", IdempotencyKey: "short", TTL: time.Minute})
	ok(t, err)
	w.clk.Advance(2 * time.Minute)
	if got := must[[]Command](t)(w.s.AgentNextCommands(ctx, w.devA.ID, 10)); len(got) != 0 {
		t.Fatalf("an expired command was delivered: %+v", got)
	}
	if n := must[int64](t)(w.s.ExpireCommands(ctx)); n != 1 {
		t.Fatalf("ExpireCommands = %d, want 1", n)
	}
	if _, err := w.s.AgentAckCommand(ctx, w.devA.ID, c.ID, CommandAck{Status: "acked"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("ack of an expired command = %v, want ErrConflict", err)
	}
	if _, _, err := w.s.EnqueueCommand(ctx, ScopeAll(), w.devA.ID, CommandInput{Type: "no_such_type", IdempotencyKey: "z"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown type = %v, want ErrInvalid", err)
	}
	if _, _, err := w.s.EnqueueCommand(ctx, ScopeAll(), w.devA.ID, CommandInput{Type: "check_in"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("missing idempotency key = %v, want ErrInvalid", err)
	}
}

// ---------------------------------------------------------------------------
// restore requests
// ---------------------------------------------------------------------------

func TestRestoreRequestLifecycle(t *testing.T) {
	w := seed(t)
	ctx := context.Background()
	snap := strings.Repeat("c", 64)
	req := RestoreRequest{
		DeviceID: w.devA.ID, SnapshotID: snap, Mode: "download_zip", SelectionCount: 1, SelectionHash: "h",
		SelectionWrapped: []byte("encrypted-selection"), RequestedBy: w.admin.ID, ExpiresAt: t0.Add(time.Hour),
	}
	r := must[RestoreRequest](t)(w.s.CreateRestoreRequest(ctx, ScopeAll(), req))
	if r.Status != "queued" || r.Overwrite != "if-changed" || string(r.SelectionWrapped) != "encrypted-selection" {
		t.Fatalf("restore = %+v", r)
	}
	// Progress, artifact, download, purge.
	ready, purge := "ready", t0.Add(2*time.Hour)
	art := "artifact-1.zip"
	size := int64(1234)
	r = must[RestoreRequest](t)(w.s.UpdateRestoreRequest(ctx, ScopeAll(), r.ID, RestoreUpdate{Status: &ready, ArtifactRef: &art, PurgeAfter: &purge, Bytes: &size,
		Progress: json.RawMessage(`{"percent":100}`)}))
	if r.Status != "ready" || r.ArtifactRef != art || r.Bytes == nil || *r.Bytes != 1234 || r.PurgeAfter == nil {
		t.Fatalf("after update = %+v", r)
	}
	// While live (ready, unexpired) the device itself may fetch it; another device may not.
	if _, err := w.s.AgentGetRestoreRequest(ctx, w.devA.ID, r.ID); err != nil {
		t.Fatalf("AgentGetRestoreRequest(owner, ready) = %v", err)
	}
	r = must[RestoreRequest](t)(w.s.UpdateRestoreRequest(ctx, ScopeAll(), r.ID, RestoreUpdate{CountDownload: true}))
	r = must[RestoreRequest](t)(w.s.UpdateRestoreRequest(ctx, ScopeAll(), r.ID, RestoreUpdate{CountDownload: true}))
	if r.DownloadCount != 2 {
		t.Fatalf("download_count = %d", r.DownloadCount)
	}
	purged := "purged"
	empty := ""
	r = must[RestoreRequest](t)(w.s.UpdateRestoreRequest(ctx, ScopeAll(), r.ID, RestoreUpdate{Status: &purged, ClearSelection: true, ArtifactRef: &empty}))
	if r.SelectionWrapped != nil || r.ArtifactRef != "" || r.Status != "purged" || r.SelectionHash != "h" {
		t.Fatalf("after purge = %+v (the hash and count persist for audit; the blob and artifact do not)", r)
	}
	got := must[RestoreRequest](t)(w.s.GetRestoreRequest(ctx, ScopeAll(), r.ID))
	if got.ID != r.ID {
		t.Fatal("GetRestoreRequest")
	}
	list := must[Listing[RestoreRequest]](t)(w.s.ListRestoreRequests(ctx, ScopeAll(), w.devA.ID, Page{}))
	if len(list.Items) != 1 {
		t.Fatalf("ListRestoreRequests = %d", len(list.Items))
	}
	// A purged restore is no longer served to anyone, its own device included.
	if _, err := w.s.AgentGetRestoreRequest(ctx, w.devA.ID, r.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("AgentGetRestoreRequest(owner, purged) = %v, want ErrNotFound", err)
	}
	if _, err := w.s.AgentGetRestoreRequest(ctx, w.devB.ID, r.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("AgentGetRestoreRequest(other) = %v, want ErrNotFound", err)
	}
}

// Change 6 (P1-FIX3): the agent is served only live restores.
func TestAgentGetRestoreRequestServesOnlyLiveRestores(t *testing.T) {
	w := seed(t)
	ctx := context.Background()
	mk := func() RestoreRequest {
		return must[RestoreRequest](t)(w.s.CreateRestoreRequest(ctx, ScopeAll(), RestoreRequest{DeviceID: w.devA.ID,
			SnapshotID: strings.Repeat("c", 64), Mode: "download_zip", ExpiresAt: t0.Add(time.Hour)}))
	}
	setStatus := func(id, status string) {
		ok(t, w.s.Write(ctx, func(tx *Tx) error {
			_, err := tx.Exec(`UPDATE restore_requests SET status = ? WHERE id = ?`, status, id)
			return err
		}))
	}
	for _, live := range []string{"queued", "running", "ready"} {
		r := mk()
		setStatus(r.ID, live)
		if _, err := w.s.AgentGetRestoreRequest(ctx, w.devA.ID, r.ID); err != nil {
			t.Errorf("status %s: AgentGetRestoreRequest = %v, want it served", live, err)
		}
	}
	for _, dead := range []string{"delivered", "failed", "cancelled", "expired", "purged"} {
		r := mk()
		setStatus(r.ID, dead)
		if _, err := w.s.AgentGetRestoreRequest(ctx, w.devA.ID, r.ID); !errors.Is(err, ErrNotFound) {
			t.Errorf("status %s: AgentGetRestoreRequest = %v, want ErrNotFound", dead, err)
		}
	}
	// Past expires_at, a restore still marked ready is not served.
	exp := mk()
	setStatus(exp.ID, "ready")
	w.clk.Advance(61 * time.Minute)
	if _, err := w.s.AgentGetRestoreRequest(ctx, w.devA.ID, exp.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("past expires_at: AgentGetRestoreRequest = %v, want ErrNotFound", err)
	}
	// Past purge_after (with a later expiry), likewise.
	pur := must[RestoreRequest](t)(w.s.CreateRestoreRequest(ctx, ScopeAll(), RestoreRequest{DeviceID: w.devA.ID,
		SnapshotID: strings.Repeat("c", 64), Mode: "download_zip", ExpiresAt: w.clk.Now().Add(24 * time.Hour)}))
	soon := w.clk.Now().Add(time.Minute)
	ready := "ready"
	pur = must[RestoreRequest](t)(w.s.UpdateRestoreRequest(ctx, ScopeAll(), pur.ID, RestoreUpdate{Status: &ready, PurgeAfter: &soon}))
	if _, err := w.s.AgentGetRestoreRequest(ctx, w.devA.ID, pur.ID); err != nil {
		t.Errorf("before purge_after: %v", err)
	}
	w.clk.Advance(2 * time.Minute)
	if _, err := w.s.AgentGetRestoreRequest(ctx, w.devA.ID, pur.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("past purge_after: AgentGetRestoreRequest = %v, want ErrNotFound", err)
	}
}

func TestRestoreRelayRules(t *testing.T) {
	w := seed(t)
	ctx := context.Background()
	other := w.enroll("tok-a", "A-DESK-REPL")
	relay := RestoreRequest{DeviceID: w.devA.ID, TargetDeviceID: other.Device.ID, SnapshotID: strings.Repeat("d", 64), Mode: "device_relay", ExpiresAt: t0.Add(time.Hour)}
	r := must[RestoreRequest](t)(w.s.CreateRestoreRequest(ctx, ScopeAll(), relay))
	// The target device pulls the artifact; the source device is also allowed to read its own restore.
	if _, err := w.s.AgentGetRestoreRequest(ctx, other.Device.ID, r.ID); err != nil {
		t.Fatalf("target device cannot fetch the relay restore: %v", err)
	}
	// Data never crosses customers.
	cross := relay
	cross.TargetDeviceID = w.devB.ID
	if _, err := w.s.CreateRestoreRequest(ctx, ScopeAll(), cross); !errors.Is(err, ErrInvalid) {
		t.Fatalf("relay to another customer's device = %v, want ErrInvalid", err)
	}
	// Schema rules.
	bad := RestoreRequest{DeviceID: w.devA.ID, SnapshotID: strings.Repeat("e", 64), Mode: "device_original", Verify: true, Simulate: true, ExpiresAt: t0.Add(time.Hour)}
	if _, err := w.s.CreateRestoreRequest(ctx, ScopeAll(), bad); !errors.Is(err, ErrInvalid) {
		t.Fatalf("verify+simulate = %v, want ErrInvalid", err)
	}
	noTarget := relay
	noTarget.TargetDeviceID = ""
	if _, err := w.s.CreateRestoreRequest(ctx, ScopeAll(), noTarget); !errors.Is(err, ErrInvalid) {
		t.Fatalf("relay without a target = %v, want ErrInvalid", err)
	}
}

// ---------------------------------------------------------------------------
// job leases
// ---------------------------------------------------------------------------

func TestJobLease(t *testing.T) {
	s, clk := newStore(t)
	ctx := context.Background()
	got := must[bool](t)(s.AcquireJobLease(ctx, "htpasswd-sync", "console-1", time.Minute))
	if !got {
		t.Fatal("first acquire failed")
	}
	if must[bool](t)(s.AcquireJobLease(ctx, "htpasswd-sync", "console-2", time.Minute)) {
		t.Fatal("a second holder acquired a live lease")
	}
	if !must[bool](t)(s.AcquireJobLease(ctx, "htpasswd-sync", "console-1", time.Minute)) {
		t.Fatal("the holder could not renew its own lease")
	}
	// Release by someone else is a no-op; the lease holds.
	ok(t, s.ReleaseJobLease(ctx, "htpasswd-sync", "console-2"))
	if must[bool](t)(s.AcquireJobLease(ctx, "htpasswd-sync", "console-2", time.Minute)) {
		t.Fatal("a foreign release freed the lease")
	}
	// Expiry hands it over.
	clk.Advance(61 * time.Second)
	if !must[bool](t)(s.AcquireJobLease(ctx, "htpasswd-sync", "console-2", time.Minute)) {
		t.Fatal("an expired lease was not taken over")
	}
	// Release frees it immediately.
	ok(t, s.ReleaseJobLease(ctx, "htpasswd-sync", "console-2"))
	if !must[bool](t)(s.AcquireJobLease(ctx, "htpasswd-sync", "console-1", time.Minute)) {
		t.Fatal("a released lease was not acquirable")
	}
	st := must[JobState](t)(s.GetJobState(ctx, "htpasswd-sync"))
	if st.LeaseHolder != "console-1" || st.LeaseUntil == nil {
		t.Fatalf("state = %+v", st)
	}
	if _, err := s.AcquireJobLease(ctx, "x", "", time.Minute); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty holder = %v", err)
	}
}

func TestRecordJobRun(t *testing.T) {
	s, clk := newStore(t)
	ctx := context.Background()
	next := clk.Now().Add(10 * time.Minute)
	ok(t, s.RecordJobRun(ctx, "heartbeat-ring-trim", JobRun{Status: "ok", StartedAt: clk.Now(), FinishedAt: clk.Now().Add(1500 * time.Millisecond), NextDueAt: &next}))
	ok(t, s.RecordJobRun(ctx, "heartbeat-ring-trim", JobRun{Status: "error", ErrorClass: "storage", StartedAt: clk.Now(), FinishedAt: clk.Now().Add(time.Second)}))
	st := must[JobState](t)(s.GetJobState(ctx, "heartbeat-ring-trim"))
	if st.RunCount != 2 || st.LastStatus != "error" || st.LastErrorClass != "storage" || st.LastDurationMS == nil || *st.LastDurationMS != 1000 || st.NextDueAt != nil {
		t.Fatalf("state = %+v", st)
	}
	if err := s.RecordJobRun(ctx, "j", JobRun{Status: "ok", StartedAt: clk.Now(), FinishedAt: clk.Now().Add(-time.Second)}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("finished before started = %v", err)
	}
	if err := s.RecordJobRun(ctx, "j", JobRun{Status: "weird", StartedAt: clk.Now(), FinishedAt: clk.Now()}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad status = %v", err)
	}
	if _, err := s.GetJobState(ctx, "never-ran"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown job = %v", err)
	}
	if list := must[[]JobState](t)(s.ListJobStates(ctx)); len(list) != 1 {
		t.Fatalf("ListJobStates = %d", len(list))
	}
}

// ---------------------------------------------------------------------------
// presence and the heartbeat ring
// ---------------------------------------------------------------------------

func TestPresenceUpsertWritesRingAndDeviceFacts(t *testing.T) {
	w := seed(t)
	ctx := context.Background()
	bat, onAC := 14, false
	uptime := int64(3600)
	off := int64(-1500)
	pv := 1
	err := w.s.AgentUpsertPresence(ctx, w.devA.ID, Presence{
		ServiceState: "deferred", AgentVersion: "1.5.0", ResticVersion: "0.19.1", UptimeS: &uptime, OnAC: &onAC, BatteryPct: &bat,
		ClockOffsetMS: &off, LastErrorClass: "network", PolicyVersionApplied: &pv, SourceIP: "203.0.113.9",
	})
	ok(t, err)
	p := must[Presence](t)(w.s.GetPresence(ctx, ScopeAll(), w.devA.ID))
	if p.ServiceState != "deferred" || p.BatteryPct == nil || *p.BatteryPct != 14 || p.OnAC == nil || *p.OnAC || !p.ReceivedAt.Equal(t0) || p.LastErrorClass != "network" {
		t.Fatalf("presence = %+v", p)
	}
	d := must[Device](t)(w.s.GetDevice(ctx, ScopeAll(), w.devA.ID))
	if d.AgentVersion != "1.5.0" || d.EnrollConfirmedAt == nil || d.PolicyVersionApplied == nil || *d.PolicyVersionApplied != 1 {
		t.Fatalf("device after heartbeat = %+v", d)
	}
	var n int
	ok(t, w.s.Read(ctx, func(tx *Tx) error {
		return tx.QueryRow(`SELECT count(*) FROM heartbeats WHERE device_id = ?`, w.devA.ID).Scan(&n)
	}))
	if n != 1 {
		t.Fatalf("%d ring rows, want 1", n)
	}
	// A second heartbeat upserts presence (still one row) and appends to the ring.
	w.clk.Advance(time.Minute)
	w.heartbeat(w.devA.ID)
	ok(t, w.s.Read(ctx, func(tx *Tx) error {
		if err := tx.QueryRow(`SELECT count(*) FROM heartbeats WHERE device_id = ?`, w.devA.ID).Scan(&n); err != nil {
			return err
		}
		var pr int
		if err := tx.QueryRow(`SELECT count(*) FROM device_presence WHERE device_id = ?`, w.devA.ID).Scan(&pr); err != nil {
			return err
		}
		if pr != 1 {
			t.Errorf("%d presence rows", pr)
		}
		return nil
	}))
	if n != 2 {
		t.Fatalf("%d ring rows, want 2", n)
	}
	if err := w.s.AgentUpsertPresence(ctx, "01J9Y4K3A0B1C2D3E4F5G6H7ZZ", Presence{ServiceState: "idle"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("presence for an unknown device = %v", err)
	}
	if err := w.s.AgentUpsertPresence(ctx, w.devA.ID, Presence{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("presence without a service state = %v", err)
	}
	if err := w.s.AgentUpsertPresence(ctx, w.devA.ID, Presence{ServiceState: "idle", BatteryPct: func() *int { x := 150; return &x }()}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("battery 150 = %v, want ErrInvalid", err)
	}
}

func TestProgressAdvancedAtMovesOnlyWhenCountsChange(t *testing.T) {
	w := seed(t)
	ctx := context.Background()
	hb := func(progress string) {
		ok(t, w.s.AgentUpsertPresence(ctx, w.devA.ID, Presence{ServiceState: "running", Progress: json.RawMessage(progress)}))
	}
	advanced := func() float64 {
		p := must[Presence](t)(w.s.GetPresence(ctx, ScopeAll(), w.devA.ID))
		var m map[string]any
		ok(t, json.Unmarshal(p.Progress, &m))
		return m["advanced_at"].(float64)
	}
	hb(`{"files_done":10}`)
	first := advanced()
	if int64(first) != t0.UnixMilli() {
		t.Fatalf("advanced_at = %v, want now", first)
	}
	w.clk.Advance(5 * time.Minute)
	hb(`{"files_done":10}`) // stalled: same counts
	if advanced() != first {
		t.Fatal("advanced_at moved although the counts did not")
	}
	w.clk.Advance(5 * time.Minute)
	hb(`{"files_done":25}`)
	if int64(advanced()) != w.clk.Now().UnixMilli() {
		t.Fatal("advanced_at did not move when the counts changed")
	}
	if err := w.s.AgentUpsertPresence(ctx, w.devA.ID, Presence{ServiceState: "running", Progress: json.RawMessage(`[1]`)}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("non-object progress = %v", err)
	}
}

func TestTrimHeartbeats(t *testing.T) {
	w := seed(t)
	ctx := context.Background()
	for i := 0; i < 7; i++ {
		ok(t, w.s.AgentUpsertPresence(ctx, w.devA.ID, Presence{ReceivedAt: w.clk.Now(), ServiceState: "idle"}))
		w.clk.Advance(10 * time.Minute)
	}
	count := func() (n int) {
		ok(t, w.s.Read(ctx, func(tx *Tx) error { return tx.QueryRow(`SELECT count(*) FROM heartbeats`).Scan(&n) }))
		return n
	}
	before := count() // 7 ring rows
	cutoff := t0.Add(35 * time.Minute)
	removed := must[int64](t)(w.s.TrimHeartbeats(ctx, cutoff, 2)) // tiny batches: it must loop
	if removed != 4 || count() != before-4 {
		t.Fatalf("removed %d, %d left of %d; want 4 removed (received 0,10,20,30 min ago the cutoff)", removed, count(), before)
	}
	if again := must[int64](t)(w.s.TrimHeartbeats(ctx, cutoff, 100)); again != 0 {
		t.Fatalf("second trim removed %d", again)
	}
	// Presence (the latest row) is never trimmed.
	if _, err := w.s.GetPresence(ctx, ScopeAll(), w.devA.ID); err != nil {
		t.Fatalf("presence missing after trim: %v", err)
	}
}

// ---------------------------------------------------------------------------
// audit
// ---------------------------------------------------------------------------

func TestAuditAppendListAndImmutability(t *testing.T) {
	w := seed(t)
	ctx := context.Background()
	ev := must[AuditEvent](t)(w.s.AppendAudit(ctx, AuditEvent{
		ActorType: "admin", ActorID: w.admin.ID, ActorLabel: "root", Method: "web", Action: "device.pause",
		ObjectType: "device", ObjectID: w.devA.ID, CustomerID: w.custA.ID, Details: json.RawMessage(`{"reason":"awaiting replacement"}`),
	}))
	if len(ev.ID) != 26 || ev.Result != "ok" || ev.Severity != "info" || !ev.OccurredAt.Equal(t0) {
		t.Fatalf("event = %+v", ev)
	}
	list := must[Listing[AuditEvent]](t)(w.s.ListAudit(ctx, ScopeAll(), AuditFilter{Action: "device.pause"}, Page{}))
	if len(list.Items) != 1 || list.Items[0].ID != ev.ID || string(list.Items[0].Details) != `{"reason":"awaiting replacement"}` {
		t.Fatalf("list = %+v", list.Items)
	}
	// The trigger blocks an UPDATE; DELETE (retention by age) is allowed.
	err := w.s.Write(ctx, func(tx *Tx) error {
		_, err := tx.Exec(`UPDATE audit_events SET action = 'tampered' WHERE id = ?`, ev.ID)
		return err
	})
	var ce *ConstraintError
	if !errors.Is(err, ErrInvalid) || !errors.As(err, &ce) || !strings.Contains(ce.Message, "immutable") {
		t.Fatalf("UPDATE audit_events = %v, want ErrInvalid from the immutability trigger", err)
	}
	if got := must[Listing[AuditEvent]](t)(w.s.ListAudit(ctx, ScopeAll(), AuditFilter{ObjectID: w.devA.ID}, Page{})); got.Items[0].Action != "device.pause" {
		t.Fatalf("the row changed: %+v", got.Items[0])
	}
	ok(t, w.s.Write(ctx, func(tx *Tx) error {
		_, err := tx.Exec(`DELETE FROM audit_events WHERE id = ?`, ev.ID)
		return err
	}))
	// Path-free: a client path in details is refused.
	for _, bad := range []string{`{"file":"C:\\Users\\x\\a.docx"}`, `{"share":"\\\\srv\\share"}`, `{"file":"D:/data/x"}`} {
		if _, err := w.s.AppendAudit(ctx, AuditEvent{ActorType: "system", Action: "x", Details: json.RawMessage(bad)}); !errors.Is(err, ErrInvalid) {
			t.Errorf("details %s accepted (%v)", bad, err)
		}
	}
	for _, good := range []string{`{"url":"https://example.invalid/x"}`, `{"n":3}`, `{"at":"12:30"}`} {
		if _, err := w.s.AppendAudit(ctx, AuditEvent{ActorType: "system", Action: "x", Details: json.RawMessage(good)}); err != nil {
			t.Errorf("details %s rejected: %v", good, err)
		}
	}
	if _, err := w.s.AppendAudit(ctx, AuditEvent{Action: "x"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("event without an actor type = %v", err)
	}
}

func TestAuditScopeHidesOtherCustomersAndSystemEvents(t *testing.T) {
	w := seed(t)
	ctx := context.Background()
	for _, c := range []string{w.custA.ID, w.custB.ID, ""} {
		must[AuditEvent](t)(w.s.AppendAudit(ctx, AuditEvent{ActorType: "system", Action: "probe", CustomerID: c}))
	}
	seen := func(sc Scope) map[string]int {
		l := must[Listing[AuditEvent]](t)(w.s.ListAudit(ctx, sc, AuditFilter{Action: "probe"}, Page{}))
		m := map[string]int{}
		for _, e := range l.Items {
			m[e.CustomerID]++
		}
		return m
	}
	if m := seen(ScopeAll()); len(m) != 3 {
		t.Fatalf("all sees %v", m)
	}
	if m := seen(ScopeOf(w.custA.ID)); len(m) != 1 || m[w.custA.ID] != 1 {
		t.Fatalf("scope A sees %v: must be only A's events (system events are for Scope.All)", m)
	}
}

// ---------------------------------------------------------------------------
// admins and sessions
// ---------------------------------------------------------------------------

func TestAdminLockoutAndPassword(t *testing.T) {
	w := seed(t)
	ctx := context.Background()
	a := must[Admin](t)(w.s.CreateAdmin(ctx, Admin{Username: "j.doe", Email: "j@example.invalid", Role: "operator", CustomerScope: []string{w.custA.ID}, PasswordHash: "$argon2id$x"}))
	if sc := a.Scope(); sc.All || !sc.Allows(w.custA.ID) || sc.Allows(w.custB.ID) {
		t.Fatalf("scope = %+v", sc)
	}
	if sc := w.admin.Scope(); !sc.All {
		t.Fatal("an owner without customer_scope must have all customers")
	}
	got := must[Admin](t)(w.s.GetAdminByUsername(ctx, "J.DOE")) // case-insensitive
	if got.ID != a.ID {
		t.Fatal("username lookup is not case-insensitive")
	}
	for i := 1; i <= 4; i++ {
		a = must[Admin](t)(w.s.RecordLoginFailure(ctx, a.ID, 5, 15*time.Minute))
		if a.FailedLoginCount != i || a.LockedUntil != nil {
			t.Fatalf("after %d failures: %+v", i, a)
		}
	}
	a = must[Admin](t)(w.s.RecordLoginFailure(ctx, a.ID, 5, 15*time.Minute))
	if a.LockedUntil == nil || !a.LockedUntil.Equal(t0.Add(15*time.Minute)) {
		t.Fatalf("the fifth failure must lock for 15 minutes: %+v", a)
	}
	ok(t, w.s.ClearLoginFailures(ctx, a.ID, "203.0.113.5"))
	a = must[Admin](t)(w.s.GetAdmin(ctx, a.ID))
	if a.FailedLoginCount != 0 || a.LockedUntil != nil || a.LastLoginIP != "203.0.113.5" || a.LastLoginAt == nil {
		t.Fatalf("after success: %+v", a)
	}
	a = must[Admin](t)(w.s.RecordLoginFailure(ctx, a.ID, 1, time.Hour))
	ok(t, w.s.UpdateAdminPassword(ctx, a.ID, "$argon2id$new", true))
	a = must[Admin](t)(w.s.GetAdmin(ctx, a.ID))
	if a.PasswordHash != "$argon2id$new" || !a.MustChangePassword || a.LockedUntil != nil || a.FailedLoginCount != 0 {
		t.Fatalf("after password change: %+v", a)
	}
	if err := w.s.UpdateAdminPassword(ctx, "01J9Y4K3A0B1C2D3E4F5G6H7ZZ", "x", false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown admin = %v", err)
	}
	if list := must[[]Admin](t)(w.s.ListAdmins(ctx)); len(list) != 2 {
		t.Fatalf("ListAdmins = %d", len(list))
	}
	// Schema rules surface as ErrInvalid: only operator/read_only may be scoped.
	if _, err := w.s.CreateAdmin(ctx, Admin{Username: "x", Email: "x@example.invalid", Role: "admin", CustomerScope: []string{w.custA.ID}, PasswordHash: "h"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a scoped admin-role account = %v, want ErrInvalid", err)
	}
	if _, err := w.s.CreateAdmin(ctx, Admin{Username: "J.Doe", Email: "x@example.invalid", Role: "operator", PasswordHash: "h"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate username (nocase) = %v, want ErrConflict", err)
	}
}

func TestSessions(t *testing.T) {
	w := seed(t)
	ctx := context.Background()
	hash := sha256Hex("cookie-1")
	s := must[Session](t)(w.s.CreateSession(ctx, Session{AdminID: w.admin.ID, TokenHash: hash, SourceIP: "203.0.113.1",
		ExpiresAt: t0.Add(30 * time.Minute), AbsoluteExpiresAt: t0.Add(12 * time.Hour)}))
	if got := must[Session](t)(w.s.GetSessionByHash(ctx, hash)); got.ID != s.ID || got.MFAVerifiedAt != nil {
		t.Fatalf("session = %+v", got)
	}
	// Activity slides the idle expiry, never past the absolute cap.
	w.clk.Advance(20 * time.Minute)
	ok(t, w.s.TouchSession(ctx, s.ID, 30*time.Minute))
	ok(t, w.s.MarkSessionMFA(ctx, s.ID))
	got := must[Session](t)(w.s.GetSessionByHash(ctx, hash))
	if !got.ExpiresAt.Equal(t0.Add(50*time.Minute)) || got.MFAVerifiedAt == nil || !got.LastSeenAt.Equal(t0.Add(20*time.Minute)) {
		t.Fatalf("after touch: %+v", got)
	}
	ok(t, w.s.TouchSession(ctx, s.ID, 100*time.Hour))
	if got = must[Session](t)(w.s.GetSessionByHash(ctx, hash)); !got.ExpiresAt.Equal(got.AbsoluteExpiresAt) {
		t.Fatalf("idle expiry %v passed the absolute cap %v", got.ExpiresAt, got.AbsoluteExpiresAt)
	}
	// Expired sessions are not found, and the cleanup job removes them.
	w.clk.Advance(13 * time.Hour)
	if _, err := w.s.GetSessionByHash(ctx, hash); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired session = %v, want ErrNotFound", err)
	}
	if n := must[int64](t)(w.s.DeleteExpiredSessions(ctx)); n != 1 {
		t.Fatalf("DeleteExpiredSessions = %d", n)
	}
	// Logout.
	s2 := must[Session](t)(w.s.CreateSession(ctx, Session{AdminID: w.admin.ID, TokenHash: sha256Hex("cookie-2"), ExpiresAt: w.clk.Now().Add(time.Hour), AbsoluteExpiresAt: w.clk.Now().Add(2 * time.Hour)}))
	ok(t, w.s.DeleteSession(ctx, s2.ID))
	ok(t, w.s.DeleteSession(ctx, s2.ID)) // idempotent
	if _, err := w.s.GetSessionByHash(ctx, sha256Hex("cookie-2")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted session = %v", err)
	}
	// Schema rules: the cookie hash must be sha256 hex.
	if _, err := w.s.CreateSession(ctx, Session{AdminID: w.admin.ID, TokenHash: "plain-cookie", ExpiresAt: w.clk.Now().Add(time.Hour), AbsoluteExpiresAt: w.clk.Now().Add(time.Hour)}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("non-hash token = %v, want ErrInvalid", err)
	}
	if _, err := w.s.CreateSession(ctx, Session{AdminID: w.admin.ID, TokenHash: sha256Hex("c3"), ExpiresAt: w.clk.Now().Add(2 * time.Hour), AbsoluteExpiresAt: w.clk.Now().Add(time.Hour)}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("absolute < idle expiry = %v, want ErrInvalid", err)
	}
}

func TestSettingsAndStorageTargets(t *testing.T) {
	w := seed(t)
	ctx := context.Background()
	if v := must[Setting](t)(w.s.GetSetting(ctx, "display_timezone")); string(v.Value) != `"UTC"` {
		t.Fatalf("seeded display_timezone = %s", v.Value)
	}
	w.clk.Advance(time.Minute)
	ok(t, w.s.PutSetting(ctx, "display_timezone", json.RawMessage(`"America/Chicago"`), w.admin.ID))
	v := must[Setting](t)(w.s.GetSetting(ctx, "display_timezone"))
	if string(v.Value) != `"America/Chicago"` || v.UpdatedBy != w.admin.ID || !v.UpdatedAt.Equal(t0.Add(time.Minute)) {
		t.Fatalf("setting = %+v", v)
	}
	if err := w.s.PutSetting(ctx, "k", json.RawMessage(`{bad`), ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid JSON = %v", err)
	}
	if err := w.s.PutSetting(ctx, "k", json.RawMessage(`1`), "no-such-admin"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown admin = %v (a foreign-key violation)", err)
	}
	if _, err := w.s.GetSetting(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing setting = %v", err)
	}
	if n := len(must[[]Setting](t)(w.s.ListSettings(ctx))); n < 6 {
		t.Fatalf("ListSettings = %d", n)
	}
	def := must[StorageTarget](t)(w.s.DefaultStorageTarget(ctx))
	if def.ID != w.target.ID || !def.IsDefault || def.ObjectLockMode != "none" || def.DefaultPruneCadenceDays != 30 {
		t.Fatalf("default target = %+v", def)
	}
	// A second default is a conflict naming the rule; governance needs days; compliance is not representable.
	_, err := w.s.CreateStorageTarget(ctx, StorageTarget{Name: "Second", Provider: "backblaze_b2", Endpoint: "e2", Bucket: "b2", GatewayURL: "g", CredentialRef: "C", IsDefault: true})
	var ce *ConstraintError
	if !errors.Is(err, ErrConflict) || !errors.As(err, &ce) || ce.Constraint != "uq_storage_targets_default" {
		t.Fatalf("second default target = %v", err)
	}
	if _, err := w.s.CreateStorageTarget(ctx, StorageTarget{Name: "Locked", Provider: "wasabi", Endpoint: "e3", Bucket: "b3", GatewayURL: "g", CredentialRef: "C", ObjectLockMode: "governance"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("governance without days = %v", err)
	}
	if _, err := w.s.CreateStorageTarget(ctx, StorageTarget{Name: "Compliance", Provider: "wasabi", Endpoint: "e4", Bucket: "b4", GatewayURL: "g", CredentialRef: "C", ObjectLockMode: "compliance"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("compliance mode = %v", err)
	}
	if n := len(must[[]StorageTarget](t)(w.s.ListStorageTargets(ctx))); n != 1 {
		t.Fatalf("ListStorageTargets = %d", n)
	}
}

// An audit row appended inside Write commits or rolls back with the change it describes.
func TestAuditCommitsWithTheChange(t *testing.T) {
	w := seed(t)
	ctx := context.Background()
	boom := errors.New("boom")
	err := w.s.Write(ctx, func(tx *Tx) error {
		if _, err := tx.Exec(`UPDATE customers SET notes = 'x' WHERE id = ?`, w.custA.ID); err != nil {
			return err
		}
		if _, err := tx.AppendAudit(AuditEvent{ActorType: "admin", ActorID: w.admin.ID, Action: "customer.update", CustomerID: w.custA.ID}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Write = %v", err)
	}
	if l := must[Listing[AuditEvent]](t)(w.s.ListAudit(ctx, ScopeAll(), AuditFilter{Action: "customer.update"}, Page{})); len(l.Items) != 0 {
		t.Fatalf("the audit row of a rolled-back change survived: %+v", l.Items)
	}
	ok(t, w.s.Write(ctx, func(tx *Tx) error {
		_, err := tx.AppendAudit(AuditEvent{ActorType: "admin", ActorID: w.admin.ID, Action: "customer.update", CustomerID: w.custA.ID})
		return err
	}))
	if l := must[Listing[AuditEvent]](t)(w.s.ListAudit(ctx, ScopeAll(), AuditFilter{Action: "customer.update"}, Page{})); len(l.Items) != 1 {
		t.Fatalf("committed audit rows = %d", len(l.Items))
	}
}

func TestConsumeEnrollmentTokenDirect(t *testing.T) {
	w := seed(t)
	ctx := context.Background()
	tok := must[EnrollmentToken](t)(w.s.ConsumeEnrollmentToken(ctx, sha256Hex("tok-a")))
	if tok.UsedCount != 2 || tok.CustomerID != w.custA.ID { // seed's A-DESK-1 already used one
		t.Fatalf("consumed = %+v", tok)
	}
	if _, err := w.s.ConsumeEnrollmentToken(ctx, sha256Hex("unknown")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown = %v", err)
	}
	w.clk.Advance(48 * time.Hour)
	if _, err := w.s.ConsumeEnrollmentToken(ctx, sha256Hex("tok-a")); !errors.Is(err, ErrConflict) {
		t.Fatalf("expired = %v", err)
	}
}
