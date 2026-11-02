package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/ulid"
)

// P1-FIX7: the store methods the console workarounds were waiting for.

func TestEffectivePolicyDocCarriesTheDocumentID(t *testing.T) {
	w := seed(t)
	ctx := context.Background()
	def := must[PolicyRecord](t)(w.s.DefaultPolicy(ctx))

	// Customer B has no policy of its own: the document is the default.
	e := must[EffectivePolicy](t)(w.s.EffectivePolicyDoc(ctx, ScopeAll(), w.devB.ID))
	if e.PolicyID != def.ID || !ulid.Valid(e.PolicyID) || e.Version != def.Version {
		t.Fatalf("default: PolicyID %q version %d, want %q %d", e.PolicyID, e.Version, def.ID, def.Version)
	}
	// The old signature and the new method agree on everything else.
	p, ver, etag, err := w.s.EffectivePolicyFor(ctx, ScopeAll(), w.devB.ID)
	ok(t, err)
	if ver != e.Version || etag != e.ETag || p.Schedule.IntervalMinutes != e.Policy.Schedule.IntervalMinutes {
		t.Fatalf("EffectivePolicyFor differs from EffectivePolicyDoc: %d %s", ver, etag)
	}

	// Customer A gets its own document: the id switches, and the agent-side method reports the same.
	rec := must[PolicyRecord](t)(w.s.CreatePolicy(ctx, ScopeAll(), PolicyInput{Scope: "customer", CustomerID: w.custA.ID, Name: "Alpha",
		Sections: sections("schedule", `{"interval_minutes":720}`)}))
	a := must[Customer](t)(w.s.GetCustomer(ctx, ScopeAll(), w.custA.ID))
	a.PolicyID = rec.ID
	must[Customer](t)(w.s.UpdateCustomer(ctx, ScopeAll(), w.custA.ID, a))
	for name, got := range map[string]EffectivePolicy{
		"EffectivePolicyDoc":      must[EffectivePolicy](t)(w.s.EffectivePolicyDoc(ctx, ScopeAll(), w.devA.ID)),
		"AgentEffectivePolicyDoc": must[EffectivePolicy](t)(w.s.AgentEffectivePolicyDoc(ctx, w.devA.ID)),
	} {
		if got.PolicyID != rec.ID || got.PolicyID == def.ID || got.Version != rec.Version || got.Policy.Schedule.IntervalMinutes != 720 {
			t.Errorf("%s: %+v, want policy %s version %d", name, got, rec.ID, rec.Version)
		}
	}
	// Outside the caller's scope the document is as invisible as the device.
	if _, err := w.s.EffectivePolicyDoc(ctx, ScopeOf(w.custB.ID), w.devA.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("out of scope = %v, want ErrNotFound", err)
	}
	if _, err := w.s.AgentEffectivePolicyDoc(ctx, "01J9Y4K3A0B1C2D3E4F5G6H7ZZ"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown device = %v, want ErrNotFound", err)
	}
}

func TestDisableAdminEndsSessionsAndIsIdempotent(t *testing.T) {
	w := seed(t)
	ctx := context.Background()
	op := must[Admin](t)(w.s.CreateAdmin(ctx, Admin{Username: "tech", Email: "tech@example.invalid", Role: "operator", PasswordHash: "$argon2id$test"}))
	hash := sha256Hex("cookie-disable")
	must[Session](t)(w.s.CreateSession(ctx, Session{AdminID: op.ID, TokenHash: hash, ExpiresAt: t0.Add(time.Hour), AbsoluteExpiresAt: t0.Add(2 * time.Hour)}))

	ok(t, w.s.DisableAdmin(ctx, op.ID))
	got := must[Admin](t)(w.s.GetAdmin(ctx, op.ID))
	if got.DisabledAt == nil || !got.DisabledAt.Equal(t0) {
		t.Fatalf("DisabledAt = %v, want %v", got.DisabledAt, t0)
	}
	if _, err := w.s.GetSessionByHash(ctx, hash); !errors.Is(err, ErrNotFound) {
		t.Fatalf("session of a disabled admin = %v, want ErrNotFound", err)
	}
	// A second call keeps the first timestamp.
	w.clk.Advance(time.Hour)
	ok(t, w.s.DisableAdmin(ctx, op.ID))
	if again := must[Admin](t)(w.s.GetAdmin(ctx, op.ID)); again.DisabledAt == nil || !again.DisabledAt.Equal(t0) {
		t.Fatalf("DisabledAt moved to %v", again.DisabledAt)
	}
	// The owner is untouched.
	if root := must[Admin](t)(w.s.GetAdmin(ctx, w.admin.ID)); root.DisabledAt != nil {
		t.Fatal("DisableAdmin disabled another account")
	}
	if err := w.s.DisableAdmin(ctx, "01J9Y4K3A0B1C2D3E4F5G6H7ZZ"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown admin = %v, want ErrNotFound", err)
	}
}

func TestGetEnrollmentTokenByHashSpendsNothing(t *testing.T) {
	w := seed(t)
	ctx := context.Background()
	got := must[EnrollmentToken](t)(w.s.GetEnrollmentTokenByHash(ctx, sha256Hex("tok-a")))
	if got.ID != w.tokA.ID || got.CustomerID != w.custA.ID || got.UsedCount != 1 { // seed enrolled one device with it
		t.Fatalf("token = %+v", got)
	}
	again := must[EnrollmentToken](t)(w.s.GetEnrollmentTokenByHash(ctx, sha256Hex("tok-a")))
	if again.UsedCount != got.UsedCount {
		t.Fatalf("a lookup spent a use: %d -> %d", got.UsedCount, again.UsedCount)
	}
	if _, err := w.s.GetEnrollmentTokenByHash(ctx, sha256Hex("no-such-token")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown hash = %v, want ErrNotFound", err)
	}
	// A revoked token is still returned (the caller decides), with its revocation visible.
	ok(t, w.s.RevokeEnrollmentToken(ctx, ScopeAll(), w.tokB.ID))
	if rev := must[EnrollmentToken](t)(w.s.GetEnrollmentTokenByHash(ctx, sha256Hex("tok-b"))); rev.RevokedAt == nil {
		t.Fatal("a revoked token came back without RevokedAt")
	}
}

func TestEnrollNonceKnown(t *testing.T) {
	w := seed(t)
	ctx := context.Background()
	if known, err := w.s.EnrollNonceKnown(ctx, "nonce-A-DESK-1"); err != nil || !known {
		t.Fatalf("seeded nonce: %v %v", known, err)
	}
	if known, err := w.s.EnrollNonceKnown(ctx, "nonce-never-used"); err != nil || known {
		t.Fatalf("unknown nonce: %v %v", known, err)
	}
}

func TestAgentEnrollHonoursACallerSuppliedDeviceID(t *testing.T) {
	w := seed(t)
	ctx := context.Background()
	id := ulid.NewGenerator(w.clk.Now, nil).MustNew()

	in := enrollInput("tok-a", "A-DESK-7")
	in.DeviceID = id
	res := must[EnrollResult](t)(w.s.AgentEnroll(ctx, in))
	if res.Device.ID != id || res.GatewayUsername != id || res.Device.RepoPath != "fleet/"+id+"/" {
		t.Fatalf("device %s username %s repo %s, want everything keyed by %s", res.Device.ID, res.GatewayUsername, res.Device.RepoPath, id)
	}
	if esc := must[Escrow](t)(w.s.ActiveEscrow(ctx, ScopeAll(), id)); esc.DeviceID != id {
		t.Fatalf("escrow row belongs to %s", esc.DeviceID)
	}
	if _, err := w.s.AgentPendingGatewaySecret(ctx, id); err != nil {
		t.Fatalf("gateway login for the supplied id: %v", err)
	}

	// A replay (same nonce) returns the stored device and ignores a new candidate id.
	other := ulid.NewGenerator(w.clk.Now, nil).MustNew()
	in2 := enrollInput("tok-a", "A-DESK-7")
	in2.DeviceID, in2.DeviceTokenHash = other, sha256Hex("device-token-replay")
	rep := must[EnrollResult](t)(w.s.AgentEnroll(ctx, in2))
	if !rep.Replay || rep.Device.ID != id {
		t.Fatalf("replay = %v device %s, want the stored device %s", rep.Replay, rep.Device.ID, id)
	}

	// A malformed id is refused before anything is written; a taken id is a conflict.
	bad := enrollInput("tok-a", "A-DESK-8")
	bad.DeviceID = "not-a-ulid"
	if _, err := w.s.AgentEnroll(ctx, bad); !errors.Is(err, ErrInvalid) {
		t.Fatalf("malformed id = %v, want ErrInvalid", err)
	}
	dup := enrollInput("tok-a", "A-DESK-9")
	dup.DeviceID = id
	if _, err := w.s.AgentEnroll(ctx, dup); !errors.Is(err, ErrConflict) {
		t.Fatalf("taken id = %v, want ErrConflict", err)
	}
	// Neither failure burned a use of the token: seed (1) + A-DESK-7 (1).
	if tok := must[EnrollmentToken](t)(w.s.GetEnrollmentTokenByHash(ctx, sha256Hex("tok-a"))); tok.UsedCount != 2 {
		t.Fatalf("used_count = %d, want 2", tok.UsedCount)
	}
}

func TestListExpiredRestores(t *testing.T) {
	w := seed(t)
	ctx := context.Background()
	snap := strings.Repeat("d", 64)
	// ADR 0008: technician downloads stream; the job expires spent selections
	// and scrubs any archive an earlier version staged (artifact_ref set).
	mk := func(mode, status string, expires time.Time, sel bool, art string) string {
		r := RestoreRequest{DeviceID: w.devA.ID, SnapshotID: snap, Mode: mode, ExpiresAt: expires}
		if sel {
			r.SelectionWrapped = []byte{1, 2, 3}
		}
		r = must[RestoreRequest](t)(w.s.CreateRestoreRequest(ctx, ScopeAll(), r))
		u := RestoreUpdate{Status: &status}
		if art != "" {
			purge := expires
			u.ArtifactRef, u.PurgeAfter = &art, &purge
		}
		must[RestoreRequest](t)(w.s.UpdateRestoreRequest(ctx, ScopeAll(), r.ID, u))
		return r.ID
	}
	later, earlier := t0.Add(time.Hour), t0.Add(-time.Hour)
	live := mk("download_zip", "ready", later, true, "")
	liveDelivered := mk("download_zip", "delivered", later, true, "")
	expired := mk("download_zip", "ready", earlier, true, "")
	expiredDelivered := mk("download_zip", "delivered", earlier, true, "")
	legacyQueued := mk("download_zip", "queued", later, true, "")           // an earlier version's row: can never stream
	stagedLive := mk("download_zip", "ready", later, true, "restore-x.zip") // a staged archive is scrubbed at once
	failedNoSel := mk("download_zip", "failed", earlier, false, "")         // nothing left to clear: never listed
	deviceRestore := mk("device_original", "queued", earlier, true, "")     // not a technician download

	got := must[[]RestoreRequest](t)(w.s.ListExpiredRestores(ctx, t0))
	have := map[string]bool{}
	for _, r := range got {
		have[r.ID] = true
	}
	for id, want := range map[string]bool{live: false, liveDelivered: false, failedNoSel: false, deviceRestore: false,
		expired: true, expiredDelivered: true, legacyQueued: true, stagedLive: true} {
		if have[id] != want {
			t.Errorf("restore %s listed = %v, want %v", id, have[id], want)
		}
	}
	if len(got) != 4 {
		t.Fatalf("%d rows, want 4", len(got))
	}

	// The job clears the reference and the selection: a handled restore is not
	// listed again.
	expiredStatus, empty := "expired", ""
	for _, r := range got {
		must[RestoreRequest](t)(w.s.UpdateRestoreRequest(ctx, ScopeAll(), r.ID, RestoreUpdate{Status: &expiredStatus, ArtifactRef: &empty, ClearSelection: true}))
	}
	if again := must[[]RestoreRequest](t)(w.s.ListExpiredRestores(ctx, t0)); len(again) != 0 {
		t.Fatalf("%d rows after the sweep, want 0", len(again))
	}
	// Time passing makes the two live ones (expires_at = t0+1h) due.
	due := must[[]RestoreRequest](t)(w.s.ListExpiredRestores(ctx, t0.Add(2*time.Hour)))
	if len(due) != 2 || (due[0].ID != live && due[1].ID != live) || (due[0].ID != liveDelivered && due[1].ID != liveDelivered) {
		t.Fatalf("after expires_at: %d rows, want the two live restores", len(due))
	}
}

func TestTenDayCounts(t *testing.T) {
	w := seed(t)
	ctx := context.Background()
	hourA := t0.Add(-3 * time.Hour).Truncate(time.Hour)
	mk := func(id string, end time.Time, outcome string) RunInput {
		r := runInput(id, end)
		r.Outcome = outcome
		return r
	}
	ingest(t, w, w.devA.ID,
		mk("01J9Y4K3A0B1C2D3E4F5G6H7C1", hourA.Add(5*time.Minute), "success"),
		mk("01J9Y4K3A0B1C2D3E4F5G6H7C2", hourA.Add(20*time.Minute), "partial"), // within tolerance: success
		mk("01J9Y4K3A0B1C2D3E4F5G6H7C3", hourA.Add(40*time.Minute), "failed"),
		mk("01J9Y4K3A0B1C2D3E4F5G6H7C4", hourA.Add(50*time.Minute), "cancelled"), // neutral: not counted
		mk("01J9Y4K3A0B1C2D3E4F5G6H7C5", hourA.Add(70*time.Minute), "failed"),    // next hour
		mk("01J9Y4K3A0B1C2D3E4F5G6H7C6", t0.Add(-30*24*time.Hour), "success"),    // outside the window
	)
	ingest(t, w, w.devB.ID, mk("01J9Y4K3A0B1C2D3E4F5G6H7C7", hourA.Add(10*time.Minute), "success"))
	// A maintenance run of the console is not a backup.
	r := must[Run](t)(w.s.CreateConsoleRun(ctx, ScopeAll(), ConsoleRunInput{DeviceID: w.devA.ID, Kind: "check"}))
	must[Run](t)(w.s.FinishConsoleRun(ctx, ScopeAll(), r.ID, ConsoleRunResult{Outcome: "success"}))

	from := t0.Add(-10 * 24 * time.Hour)
	got := must[[]HourCount](t)(w.s.TenDayCounts(ctx, ScopeAll(), w.devA.ID, from, time.Time{}))
	if len(got) != 2 {
		t.Fatalf("device A: %+v, want two hours", got)
	}
	if got[0].DeviceID != w.devA.ID || !got[0].Hour.Equal(hourA) || got[0].Success != 2 || got[0].Failure != 1 {
		t.Fatalf("first hour = %+v, want 2 successes and 1 failure at %v", got[0], hourA)
	}
	if !got[1].Hour.Equal(hourA.Add(time.Hour)) || got[1].Success != 0 || got[1].Failure != 1 {
		t.Fatalf("second hour = %+v", got[1])
	}

	// deviceID "" covers every device in scope; the scope narrows it.
	all := must[[]HourCount](t)(w.s.TenDayCounts(ctx, ScopeAll(), "", from, time.Time{}))
	if len(all) != 3 {
		t.Fatalf("all devices: %+v, want 3 rows", all)
	}
	onlyB := must[[]HourCount](t)(w.s.TenDayCounts(ctx, ScopeOf(w.custB.ID), "", from, time.Time{}))
	if len(onlyB) != 1 || onlyB[0].DeviceID != w.devB.ID {
		t.Fatalf("scoped to B: %+v", onlyB)
	}
	// from is inclusive, to exclusive.
	if cut := must[[]HourCount](t)(w.s.TenDayCounts(ctx, ScopeAll(), w.devA.ID, hourA.Add(time.Hour), time.Time{})); len(cut) != 1 {
		t.Fatalf("from cut: %+v", cut)
	}
	if cut := must[[]HourCount](t)(w.s.TenDayCounts(ctx, ScopeAll(), w.devA.ID, from, hourA.Add(time.Hour))); len(cut) != 1 || cut[0].Failure != 1 || cut[0].Success != 2 {
		t.Fatalf("to cut: %+v", cut)
	}
	// An out-of-scope device is not found, and the zero scope fails closed.
	if _, err := w.s.TenDayCounts(ctx, ScopeOf(w.custB.ID), w.devA.ID, from, time.Time{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("out of scope = %v, want ErrNotFound", err)
	}
	if _, err := w.s.TenDayCounts(ctx, Scope{}, "", from, time.Time{}); !errors.Is(err, ErrScope) {
		t.Fatalf("zero scope = %v, want ErrScope", err)
	}
	// A retired device drops out of the all-devices form.
	if _, err := w.s.UpdateDevice(ctx, ScopeAll(), w.devB.ID, DeviceUpdate{Retire: &RetireSpec{Reason: "gone"}}); err != nil {
		t.Fatal(err)
	}
	if rest := must[[]HourCount](t)(w.s.TenDayCounts(ctx, ScopeAll(), "", from, time.Time{})); len(rest) != 2 {
		t.Fatalf("after retiring B: %+v, want device A's two rows", rest)
	}
}

func TestIntegrityCheck(t *testing.T) {
	w := seed(t)
	got, err := w.s.IntegrityCheck(context.Background())
	if err != nil || got != "ok" {
		t.Fatalf("IntegrityCheck = %q, %v", got, err)
	}
}
