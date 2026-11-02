package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestAgentEnrollCreatesEverything(t *testing.T) {
	w := seed(t)
	res := w.enroll("tok-a", "A-DESK-2")
	d := res.Device
	if res.Replay || res.GatewayUsername != d.ID {
		t.Fatalf("Replay=%v GatewayUsername=%q (device %s)", res.Replay, res.GatewayUsername, d.ID)
	}
	if d.CustomerID != w.custA.ID || d.EnrolledVia != "token" || d.EnrollNonce != "nonce-A-DESK-2" || d.EnrollConfirmedAt != nil {
		t.Fatalf("device = %+v", d)
	}
	if want := "fleet/" + d.ID + "/"; d.RepoPath != want {
		t.Fatalf("repo_path = %q, want %q", d.RepoPath, want)
	}
	if d.FirstBackupDueBy == nil || !d.FirstBackupDueBy.Equal(t0.Add(72*time.Hour)) {
		t.Fatalf("first_backup_due_by = %v, want enrollment + the policy's 72 h grace", d.FirstBackupDueBy)
	}
	if !d.EnrolledAt.Equal(t0) {
		t.Fatalf("enrolled_at = %v, want the injected clock", d.EnrolledAt)
	}
	if res.Custody != "escrowed" || !bytes.Equal(res.RepoKeyWrapped, []byte("wrapped-repo-A-DESK-2")) || !bytes.Equal(res.GatewaySecretWrapped, []byte("wrapped-gateway-A-DESK-2")) {
		t.Fatalf("result secrets = %+v", res)
	}
	esc := must[Escrow](t)(w.s.ActiveEscrow(w.ctx, ScopeAll(), d.ID))
	if esc.Custody != "escrowed" || esc.KeyVersion != 1 || esc.VerifiedAt != nil || esc.MasterKeyID != "test-key" {
		t.Fatalf("escrow = %+v", esc)
	}
	ms := must[MaintenanceSchedule](t)(w.s.GetMaintenanceSchedule(w.ctx, ScopeAll(), d.ID))
	if !ms.Enabled {
		t.Fatal("maintenance should be enabled for an escrowed device")
	}
	// The token was spent exactly once for this enrollment.
	toks := must[[]EnrollmentToken](t)(w.s.ListEnrollmentTokens(w.ctx, ScopeAll(), w.custA.ID))
	if len(toks) != 1 || toks[0].UsedCount != 2 { // A-DESK-1 from seed and A-DESK-2
		t.Fatalf("used_count = %+v", toks)
	}
	// fleet_ever_enrolled flipped, audit event written, device token resolves.
	if v := must[Setting](t)(w.s.GetSetting(w.ctx, "fleet_ever_enrolled")); string(v.Value) != "true" {
		t.Fatalf("fleet_ever_enrolled = %s", v.Value)
	}
	ev := must[Listing[AuditEvent]](t)(w.s.ListAudit(w.ctx, ScopeAll(), AuditFilter{Action: "device.enroll", ObjectID: d.ID}, Page{}))
	if len(ev.Items) != 1 || ev.Items[0].ActorType != "device" || ev.Items[0].CustomerID != w.custA.ID {
		t.Fatalf("audit = %+v", ev.Items)
	}
	id, tok, err := w.s.AgentLookupDeviceToken(w.ctx, sha256Hex("device-token-A-DESK-2"))
	ok(t, err)
	if id != d.ID || tok.DeviceID != d.ID {
		t.Fatalf("token resolves to %s", id)
	}
}

func TestAgentEnrollIsIdempotentOnNonceUntilConfirmed(t *testing.T) {
	w := seed(t)
	first := w.enroll("tok-a", "A-LAPTOP-9")

	// The response was lost: the agent retries with the same nonce and a fresh device token.
	in := enrollInput("tok-a", "A-LAPTOP-9")
	in.DeviceTokenHash = sha256Hex("device-token-retry")
	in.GatewaySecretWrapped = []byte("a-different-secret-that-must-not-be-used")
	in.GatewayPasswordHash = fixtureBcrypt
	in.RepoKeyWrapped = []byte("a-different-key-that-must-not-be-used")
	again := must[EnrollResult](t)(w.s.AgentEnroll(w.ctx, in))

	if !again.Replay || again.Device.ID != first.Device.ID {
		t.Fatalf("replay = %v, device %s vs %s", again.Replay, again.Device.ID, first.Device.ID)
	}
	if !bytes.Equal(again.GatewaySecretWrapped, first.GatewaySecretWrapped) || !bytes.Equal(again.RepoKeyWrapped, first.RepoKeyWrapped) {
		t.Fatal("a replay must re-deliver the SAME still-undelivered secrets, not the ones in the retry")
	}
	// No second use of the token, no second device.
	toks := must[[]EnrollmentToken](t)(w.s.ListEnrollmentTokens(w.ctx, ScopeAll(), w.custA.ID))
	if toks[0].UsedCount != 2 { // seed's A-DESK-1 plus this one
		t.Fatalf("used_count = %d after a replay, want 2", toks[0].UsedCount)
	}
	devs := must[Listing[Device]](t)(w.s.ListDevices(w.ctx, ScopeAll(), ListDevicesOptions{CustomerID: w.custA.ID}))
	if len(devs.Items) != 2 {
		t.Fatalf("%d devices for customer A, want 2", len(devs.Items))
	}
	// The old device token is gone (a hash cannot be turned back into the plaintext the lost response carried), the new one works.
	if _, _, err := w.s.AgentLookupDeviceToken(w.ctx, sha256Hex("device-token-A-LAPTOP-9")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old token lookup = %v, want ErrNotFound", err)
	}
	if id, _, err := w.s.AgentLookupDeviceToken(w.ctx, sha256Hex("device-token-retry")); err != nil || id != first.Device.ID {
		t.Fatalf("new token lookup = %q, %v", id, err)
	}
	// Still deliverable through the escrow accessor.
	if _, err := w.s.AgentEscrowForDelivery(w.ctx, first.Device.ID); err != nil {
		t.Fatalf("AgentEscrowForDelivery before confirmation: %v", err)
	}

	// First authenticated call confirms the enrollment; afterwards the secrets are no longer returned.
	w.heartbeat(first.Device.ID)
	if _, err := w.s.AgentEscrowForDelivery(w.ctx, first.Device.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("AgentEscrowForDelivery after confirmation = %v, want ErrNotFound", err)
	}
	if _, err := w.s.AgentEnroll(w.ctx, enrollInput("tok-a", "A-LAPTOP-9")); !errors.Is(err, ErrConflict) {
		t.Fatalf("replay after confirmation = %v, want ErrConflict", err)
	}
	d := must[Device](t)(w.s.GetDevice(w.ctx, ScopeAll(), first.Device.ID))
	if d.EnrollConfirmedAt == nil {
		t.Fatal("the heartbeat did not set enroll_confirmed_at")
	}
}

func TestAgentEnrollNonceBelongsToItsToken(t *testing.T) {
	w := seed(t)
	w.enroll("tok-a", "A-DESK-5")
	in := enrollInput("tok-b", "A-DESK-5") // same nonce, a different customer's token
	if _, err := w.s.AgentEnroll(w.ctx, in); !errors.Is(err, ErrConflict) {
		t.Fatalf("nonce replay under another token = %v, want ErrConflict", err)
	}
}

func TestEnrollmentTokenStates(t *testing.T) {
	w := seed(t)
	// Unknown token: not found. A failed enrollment must not burn a use.
	in := enrollInput("nope", "X-1")
	if _, err := w.s.AgentEnroll(w.ctx, in); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown token = %v, want ErrNotFound", err)
	}
	bad := enrollInput("tok-a", "X-2")
	bad.RepoKeyWrapped = nil // escrowed custody needs the wrapped key
	if _, err := w.s.AgentEnroll(w.ctx, bad); !errors.Is(err, ErrInvalid) {
		t.Fatalf("missing escrow key = %v, want ErrInvalid", err)
	}
	toks := must[[]EnrollmentToken](t)(w.s.ListEnrollmentTokens(w.ctx, ScopeAll(), w.custA.ID))
	if toks[0].UsedCount != 1 {
		t.Fatalf("a failed enrollment burned a use: used_count = %d, want 1", toks[0].UsedCount)
	}

	// Revoked.
	ok(t, w.s.RevokeEnrollmentToken(w.ctx, ScopeAll(), w.tokA.ID))
	if _, err := w.s.AgentEnroll(w.ctx, enrollInput("tok-a", "X-3")); !errors.Is(err, ErrConflict) {
		t.Fatalf("revoked token = %v, want ErrConflict", err)
	}
	// Expired.
	exp := must[EnrollmentToken](t)(w.s.CreateEnrollmentToken(w.ctx, ScopeAll(), EnrollmentToken{CustomerID: w.custB.ID, TokenHash: sha256Hex("tok-exp"), ExpiresAt: t0.Add(time.Hour)}))
	_ = exp
	w.clk.Advance(2 * time.Hour)
	if _, err := w.s.AgentEnroll(w.ctx, enrollInput("tok-exp", "X-4")); !errors.Is(err, ErrConflict) {
		t.Fatalf("expired token = %v, want ErrConflict", err)
	}
	// A token must expire in the future.
	if _, err := w.s.CreateEnrollmentToken(w.ctx, ScopeAll(), EnrollmentToken{CustomerID: w.custB.ID, TokenHash: sha256Hex("old"), ExpiresAt: t0}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("token expiring in the past = %v, want ErrInvalid", err)
	}
}

// Two concurrent consumers of a max_uses=1 token: exactly one wins; the loser is ErrConflict.
func TestEnrollmentTokenMaxUsesRace(t *testing.T) {
	w := seed(t)
	one := 1
	must[EnrollmentToken](t)(w.s.CreateEnrollmentToken(w.ctx, ScopeAll(), EnrollmentToken{CustomerID: w.custA.ID, TokenHash: sha256Hex("single"), ExpiresAt: t0.Add(time.Hour), MaxUses: &one}))
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := w.s.ConsumeEnrollmentToken(w.ctx, sha256Hex("single"))
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	wins, conflicts := 0, 0
	for err := range results {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, ErrConflict):
			conflicts++
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if wins != 1 || conflicts != 7 {
		t.Fatalf("wins=%d conflicts=%d, want 1 and 7", wins, conflicts)
	}

	// The same through AgentEnroll with distinct nonces: one device only.
	must[EnrollmentToken](t)(w.s.CreateEnrollmentToken(w.ctx, ScopeAll(), EnrollmentToken{CustomerID: w.custA.ID, TokenHash: sha256Hex("single2"), ExpiresAt: t0.Add(time.Hour), MaxUses: &one}))
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			in := enrollInput("single2", "RACE-"+string(rune('A'+i)))
			_, err := w.s.AgentEnroll(w.ctx, in)
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	wins = 0
	for err := range errs {
		if err == nil {
			wins++
		} else if !errors.Is(err, ErrConflict) {
			t.Errorf("unexpected error: %v", err)
		}
	}
	if wins != 1 {
		t.Fatalf("%d enrollments succeeded on a max_uses=1 token", wins)
	}
}

func TestEnrollCustomerHeldCustodyAndDeviceCap(t *testing.T) {
	w := seed(t)
	held := must[Customer](t)(w.s.CreateCustomer(w.ctx, ScopeAll(), Customer{Name: "Held Legal", KeyCustody: "customer_held"}))
	must[EnrollmentToken](t)(w.s.CreateEnrollmentToken(w.ctx, ScopeAll(), EnrollmentToken{CustomerID: held.ID, TokenHash: sha256Hex("tok-h"), ExpiresAt: t0.Add(time.Hour)}))
	in := enrollInput("tok-h", "H-1")
	in.RepoKeyWrapped, in.WrapAlg, in.MasterKeyID = nil, "", ""
	res := must[EnrollResult](t)(w.s.AgentEnroll(w.ctx, in))
	if res.Custody != "customer_held" || len(res.RepoKeyWrapped) != 0 {
		t.Fatalf("custody = %q key=%q", res.Custody, res.RepoKeyWrapped)
	}
	esc := must[Escrow](t)(w.s.ActiveEscrow(w.ctx, ScopeAll(), res.Device.ID))
	if esc.Custody != "customer_held" || esc.WrappedKey != nil {
		t.Fatalf("escrow = %+v", esc)
	}
	if ms := must[MaintenanceSchedule](t)(w.s.GetMaintenanceSchedule(w.ctx, ScopeAll(), res.Device.ID)); ms.Enabled {
		t.Fatal("maintenance must be disabled for customer-held keys")
	}

	// Device cap.
	cap := 1
	c := must[Customer](t)(w.s.GetCustomer(w.ctx, ScopeAll(), held.ID))
	c.DeviceCap = &cap
	must[Customer](t)(w.s.UpdateCustomer(w.ctx, ScopeAll(), held.ID, c))
	in2 := enrollInput("tok-h", "H-2")
	in2.RepoKeyWrapped, in2.WrapAlg, in2.MasterKeyID = nil, "", ""
	if _, err := w.s.AgentEnroll(w.ctx, in2); !errors.Is(err, ErrConflict) {
		t.Fatalf("enrolling beyond the device cap = %v, want ErrConflict", err)
	}
}

func TestGatewaySecretLifecycleAndHtpasswd(t *testing.T) {
	w := seed(t)
	ctx := context.Background()
	got := must[[]byte](t)(w.s.AgentPendingGatewaySecret(ctx, w.devA.ID))
	if !bytes.Equal(got, []byte("wrapped-gateway-A-DESK-1")) {
		t.Fatalf("pending secret = %q", got)
	}
	entries := must[[]HtpasswdEntry](t)(w.s.ListGatewayLoginsForHtpasswd(ctx))
	if len(entries) != 2 || entries[0].PasswordHash != fixtureBcrypt {
		t.Fatalf("htpasswd entries = %+v", entries)
	}
	if entries[0].Username > entries[1].Username {
		t.Fatal("htpasswd entries are not ordered by username")
	}
	ok(t, w.s.AgentAckGatewaySecret(ctx, w.devA.ID))
	ok(t, w.s.AgentAckGatewaySecret(ctx, w.devA.ID)) // idempotent
	if _, err := w.s.AgentPendingGatewaySecret(ctx, w.devA.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("pending after ack = %v, want ErrNotFound", err)
	}
	n := must[int64](t)(w.s.MarkHtpasswdSynced(ctx, []string{w.devA.ID, w.devB.ID}))
	if n != 2 {
		t.Fatalf("MarkHtpasswdSynced = %d", n)
	}

	// Retiring a device removes its login from the file and kills its tokens.
	_, err := w.s.UpdateDevice(ctx, ScopeAll(), w.devA.ID, DeviceUpdate{Retire: &RetireSpec{Reason: "decommissioned"}})
	ok(t, err)
	entries = must[[]HtpasswdEntry](t)(w.s.ListGatewayLoginsForHtpasswd(ctx))
	if len(entries) != 1 || entries[0].Username != w.devB.ID {
		t.Fatalf("after retire, htpasswd entries = %+v", entries)
	}
	if _, _, err := w.s.AgentLookupDeviceToken(ctx, sha256Hex("device-token-A-DESK-1")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("retired device token = %v, want ErrNotFound", err)
	}
}

func TestDeviceTokenRotationAndGrace(t *testing.T) {
	w := seed(t)
	ctx := context.Background()
	oldHash := sha256Hex("device-token-A-DESK-1")
	_, oldTok, err := w.s.AgentLookupDeviceToken(ctx, oldHash)
	ok(t, err)
	tok := must[DeviceToken](t)(w.s.AgentRotateDeviceToken(ctx, w.devA.ID, oldTok.ID, sha256Hex("rotated-1"), time.Hour))
	if tok.SupersededAt != nil {
		t.Fatal("the new token is current, not superseded")
	}
	// Both work inside the grace window.
	for _, h := range []string{oldHash, sha256Hex("rotated-1")} {
		if id, _, err := w.s.AgentLookupDeviceToken(ctx, h); err != nil || id != w.devA.ID {
			t.Fatalf("lookup %.8s inside grace: %q, %v", h, id, err)
		}
	}
	// After the grace the superseded token is dead, the current one is not.
	w.clk.Advance(61 * time.Minute)
	if _, _, err := w.s.AgentLookupDeviceToken(ctx, oldHash); !errors.Is(err, ErrNotFound) {
		t.Fatalf("superseded token after grace = %v, want ErrNotFound", err)
	}
	if _, _, err := w.s.AgentLookupDeviceToken(ctx, sha256Hex("rotated-1")); err != nil {
		t.Fatalf("current token: %v", err)
	}
	ok(t, w.s.AgentTouchDeviceToken(ctx, tok.ID, "203.0.113.7"))
	if _, err := w.s.AgentRotateDeviceToken(ctx, "no-such-device", tok.ID, sha256Hex("x"), time.Hour); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rotate for an unknown device = %v", err)
	}
	if _, err := w.s.AgentRotateDeviceToken(ctx, w.devA.ID, tok.ID, sha256Hex("y"), 0); !errors.Is(err, ErrInvalid) {
		t.Fatalf("rotate with no grace = %v", err)
	}
}

// Change 4 (P1-FIX3): only the device's current token may rotate, and a rotation
// leaves at most one grace-window token behind.
func TestDeviceTokenRotationRequiresCurrentToken(t *testing.T) {
	w := seed(t)
	ctx := context.Background()
	h0, h1, h2 := sha256Hex("device-token-A-DESK-1"), sha256Hex("rotated-1"), sha256Hex("rotated-2")
	_, t0Tok, err := w.s.AgentLookupDeviceToken(ctx, h0)
	ok(t, err)
	t1 := must[DeviceToken](t)(w.s.AgentRotateDeviceToken(ctx, w.devA.ID, t0Tok.ID, h1, time.Hour))

	// The superseded token (still valid in its grace window) cannot mint a successor.
	if _, err := w.s.AgentRotateDeviceToken(ctx, w.devA.ID, t0Tok.ID, sha256Hex("evil"), time.Hour); !errors.Is(err, ErrConflict) {
		t.Fatalf("rotate with a superseded token = %v, want ErrConflict", err)
	}
	// Neither can another device's current token, nor an id that does not exist.
	_, bTok, err := w.s.AgentLookupDeviceToken(ctx, sha256Hex("device-token-B-DESK-1"))
	ok(t, err)
	if _, err := w.s.AgentRotateDeviceToken(ctx, w.devA.ID, bTok.ID, sha256Hex("evil"), time.Hour); !errors.Is(err, ErrConflict) {
		t.Fatalf("rotate with another device's token = %v, want ErrConflict", err)
	}
	if _, err := w.s.AgentRotateDeviceToken(ctx, w.devA.ID, "no-such-token", sha256Hex("evil"), time.Hour); !errors.Is(err, ErrConflict) {
		t.Fatalf("rotate with an unknown token id = %v, want ErrConflict", err)
	}
	// Refusals changed nothing: t0 and t1 still resolve, the evil hash does not.
	for _, h := range []string{h0, h1} {
		if _, _, err := w.s.AgentLookupDeviceToken(ctx, h); err != nil {
			t.Fatalf("lookup %.8s after refused rotations: %v", h, err)
		}
	}
	if _, _, err := w.s.AgentLookupDeviceToken(ctx, sha256Hex("evil")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a refused rotation minted a token: %v", err)
	}

	// Rotating the current token again revokes the earlier grace-window token at once.
	w.clk.Advance(time.Minute)
	t2 := must[DeviceToken](t)(w.s.AgentRotateDeviceToken(ctx, w.devA.ID, t1.ID, h2, time.Hour))
	if _, _, err := w.s.AgentLookupDeviceToken(ctx, h0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the oldest token must be revoked by the second rotation, lookup = %v", err)
	}
	for _, h := range []string{h1, h2} {
		if _, _, err := w.s.AgentLookupDeviceToken(ctx, h); err != nil {
			t.Fatalf("lookup %.8s after the second rotation: %v", h, err)
		}
	}
	var live, graceWindow int
	ok(t, w.s.Read(ctx, func(tx *Tx) error {
		if err := tx.QueryRow(`SELECT count(*) FROM device_tokens WHERE device_id = ? AND revoked_at IS NULL AND superseded_at IS NULL`, w.devA.ID).Scan(&live); err != nil {
			return err
		}
		return tx.QueryRow(`SELECT count(*) FROM device_tokens WHERE device_id = ? AND revoked_at IS NULL AND superseded_at IS NOT NULL`, w.devA.ID).Scan(&graceWindow)
	}))
	if live != 1 || graceWindow != 1 || t2.SupersededAt != nil {
		t.Fatalf("current=%d grace-window=%d, want exactly one of each", live, graceWindow)
	}
}

// Change 7 (P1-FIX3): the DH17 replay is bound to the token's lifetime and to the enrolled hardware.
func TestAgentEnrollReplayRequiresLiveTokenAndSameFingerprint(t *testing.T) {
	w := seed(t)
	in := enrollInput("tok-a", "A-LAPTOP-7")
	in.HWFingerprint = "hw-fingerprint-1"
	first := must[EnrollResult](t)(w.s.AgentEnroll(w.ctx, in))

	// Another machine presenting the leaked nonce and token is refused, and nothing changes.
	stranger := in
	stranger.HWFingerprint = "hw-fingerprint-OTHER"
	stranger.DeviceTokenHash = sha256Hex("device-token-stranger")
	if _, err := w.s.AgentEnroll(w.ctx, stranger); !errors.Is(err, ErrConflict) {
		t.Fatalf("replay with another fingerprint = %v, want ErrConflict", err)
	}
	noFP := in
	noFP.HWFingerprint = ""
	noFP.DeviceTokenHash = sha256Hex("device-token-nofp")
	if _, err := w.s.AgentEnroll(w.ctx, noFP); !errors.Is(err, ErrConflict) {
		t.Fatalf("replay with no fingerprint = %v, want ErrConflict", err)
	}
	if _, _, err := w.s.AgentLookupDeviceToken(w.ctx, sha256Hex("device-token-stranger")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a refused replay minted a device token: %v", err)
	}
	if id, _, err := w.s.AgentLookupDeviceToken(w.ctx, in.DeviceTokenHash); err != nil || id != first.Device.ID {
		t.Fatalf("a refused replay revoked the original token: %q, %v", id, err)
	}

	// The same hardware still replays with the existing semantics: same device, same secrets, fresh token.
	retry := in
	retry.DeviceTokenHash = sha256Hex("device-token-retry-7")
	retry.GatewaySecretWrapped = []byte("must-not-be-used")
	again := must[EnrollResult](t)(w.s.AgentEnroll(w.ctx, retry))
	if !again.Replay || again.Device.ID != first.Device.ID || !bytes.Equal(again.GatewaySecretWrapped, first.GatewaySecretWrapped) ||
		!bytes.Equal(again.RepoKeyWrapped, first.RepoKeyWrapped) {
		t.Fatalf("replay with the same fingerprint = %+v", again)
	}
	if id, _, err := w.s.AgentLookupDeviceToken(w.ctx, retry.DeviceTokenHash); err != nil || id != first.Device.ID {
		t.Fatalf("fresh device token after replay: %q, %v", id, err)
	}

	// Once the enrollment token has expired, the replay is refused even for the right machine.
	w.clk.Advance(25 * time.Hour)
	retry.DeviceTokenHash = sha256Hex("device-token-retry-late")
	if _, err := w.s.AgentEnroll(w.ctx, retry); !errors.Is(err, ErrConflict) {
		t.Fatalf("replay after the token expired = %v, want ErrConflict", err)
	}
	// ... and the boundary is exclusive: expires_at <= now is expired.
	w2 := seed(t)
	tok := must[EnrollmentToken](t)(w2.s.CreateEnrollmentToken(w2.ctx, ScopeAll(), EnrollmentToken{CustomerID: w2.custA.ID,
		TokenHash: sha256Hex("tok-edge"), ExpiresAt: t0.Add(time.Hour)}))
	edge := enrollInput("tok-edge", "A-EDGE-1")
	must[EnrollResult](t)(w2.s.AgentEnroll(w2.ctx, edge))
	w2.clk.Advance(time.Hour) // now == expires_at
	edge.DeviceTokenHash = sha256Hex("device-token-edge-2")
	if _, err := w2.s.AgentEnroll(w2.ctx, edge); !errors.Is(err, ErrConflict) {
		t.Fatalf("replay at expires_at = %v, want ErrConflict (token %s)", err, tok.ID)
	}
}

// Change 1 (P1-FIX3): a token's adopted device and policy are resolved inside the
// store transaction and may not belong to another customer.
func TestCreateEnrollmentTokenReferencesStayInsideTheCustomer(t *testing.T) {
	w := seed(t)
	opA := ScopeOf(w.custA.ID)
	mk := func(sc Scope, cust string, mut func(*EnrollmentToken)) error {
		tok := EnrollmentToken{CustomerID: cust, TokenHash: sha256Hex(uniq("ref")), ExpiresAt: t0.Add(time.Hour)}
		mut(&tok)
		_, err := w.s.CreateEnrollmentToken(w.ctx, sc, tok)
		return err
	}
	polB := must[PolicyRecord](t)(w.s.CreatePolicy(w.ctx, ScopeAll(), PolicyInput{Scope: "customer", CustomerID: w.custB.ID, Name: "B policy"}))
	polA := must[PolicyRecord](t)(w.s.CreatePolicy(w.ctx, ScopeAll(), PolicyInput{Scope: "customer", CustomerID: w.custA.ID, Name: "A policy"}))
	tmpl := must[PolicyRecord](t)(w.s.CreatePolicy(w.ctx, ScopeAll(), PolicyInput{Scope: "template", Name: "Template"}))

	// Adopted device: out of scope is not found, another customer's (in scope) is invalid, own is fine.
	if err := mk(opA, w.custA.ID, func(t *EnrollmentToken) { one := 1; t.AdoptsDeviceID, t.MaxUses = w.devB.ID, &one }); !errors.Is(err, ErrNotFound) {
		t.Errorf("operator A adopting B's device = %v, want ErrNotFound", err)
	}
	if err := mk(ScopeAll(), w.custA.ID, func(t *EnrollmentToken) { one := 1; t.AdoptsDeviceID, t.MaxUses = w.devB.ID, &one }); !errors.Is(err, ErrInvalid) {
		t.Errorf("A's token adopting B's device (all-customer scope) = %v, want ErrInvalid", err)
	}
	if err := mk(ScopeAll(), w.custA.ID, func(t *EnrollmentToken) { one := 1; t.AdoptsDeviceID, t.MaxUses = "no-such-device", &one }); !errors.Is(err, ErrNotFound) {
		t.Errorf("adopting an unknown device = %v, want ErrNotFound", err)
	}
	if err := mk(opA, w.custA.ID, func(t *EnrollmentToken) { one := 1; t.AdoptsDeviceID, t.MaxUses = w.devA.ID, &one }); err != nil {
		t.Errorf("operator A adopting A's own device = %v", err)
	}
	// Policy: another customer's is invalid, a template or the customer's own is fine.
	if err := mk(opA, w.custA.ID, func(t *EnrollmentToken) { t.PolicyID = polB.ID }); !errors.Is(err, ErrInvalid) {
		t.Errorf("A's token naming B's policy = %v, want ErrInvalid", err)
	}
	if err := mk(opA, w.custA.ID, func(t *EnrollmentToken) { t.PolicyID = "no-such-policy" }); !errors.Is(err, ErrInvalid) {
		t.Errorf("A's token naming an unknown policy = %v, want ErrInvalid", err)
	}
	for _, id := range []string{polA.ID, tmpl.ID} {
		if err := mk(opA, w.custA.ID, func(t *EnrollmentToken) { t.PolicyID = id }); err != nil {
			t.Errorf("A's token naming policy %s = %v", id, err)
		}
	}
	// Nothing was inserted by the refused calls.
	toks := must[[]EnrollmentToken](t)(w.s.ListEnrollmentTokens(w.ctx, ScopeAll(), w.custA.ID))
	for _, tk := range toks {
		if tk.AdoptsDeviceID == w.devB.ID || tk.PolicyID == polB.ID {
			t.Fatalf("a refused call left a token behind: %+v", tk)
		}
	}
}

// M2-W1B: an unknown policy id and another customer's policy are the same
// answer, so a scoped caller cannot probe which policy ids exist.
func TestCustomerPolicyCheckIsNoExistenceOracle(t *testing.T) {
	w := seed(t)
	polB := must[PolicyRecord](t)(w.s.CreatePolicy(w.ctx, ScopeAll(), PolicyInput{Scope: "customer", CustomerID: w.custB.ID, Name: "B policy"}))
	opA := ScopeOf(w.custA.ID)

	n := 0
	mkToken := func(policyID string) error {
		n++
		_, err := w.s.CreateEnrollmentToken(w.ctx, opA, EnrollmentToken{CustomerID: w.custA.ID, TokenHash: sha256Hex(fmt.Sprintf("oracle-tok-%d", n)),
			ExpiresAt: t0.Add(time.Hour), PolicyID: policyID})
		return err
	}
	update := func(policyID string) error {
		c := w.custA
		c.PolicyID = policyID
		_, err := w.s.UpdateCustomer(w.ctx, opA, w.custA.ID, c)
		return err
	}
	for name, call := range map[string]func(string) error{"CreateEnrollmentToken": mkToken, "UpdateCustomer": update} {
		other, unknown := call(polB.ID), call("01J9Y4K3A0B1C2D3E4F5G6H7ZZ")
		if !errors.Is(other, ErrInvalid) || !errors.Is(unknown, ErrInvalid) {
			t.Fatalf("%s: other-customer policy = %v, unknown policy = %v, want ErrInvalid for both", name, other, unknown)
		}
		if other.Error() != unknown.Error() {
			t.Errorf("%s: existence oracle: other customer's policy says %q, unknown policy says %q", name, other, unknown)
		}
	}
}

// M2-W1B: a device token past its expiry cannot mint a successor, even though
// its hash matches and it is still the device's current token.
func TestRotateRefusesAnExpiredDeviceToken(t *testing.T) {
	expire := func(w *world) DeviceToken {
		_, tok, err := w.s.AgentLookupDeviceToken(w.ctx, sha256Hex("device-token-A-DESK-1"))
		ok(t, err)
		ok(t, w.s.Write(w.ctx, func(tx *Tx) error {
			_, err := tx.Exec(`UPDATE device_tokens SET expires_at = ? WHERE id = ?`, t0.Add(time.Hour).UnixMilli(), tok.ID)
			return err
		}))
		return tok
	}
	// Control: inside its lifetime the token rotates, so the expiry column alone does not block.
	w := seed(t)
	tok := expire(w)
	w.clk.Advance(30 * time.Minute)
	must[DeviceToken](t)(w.s.AgentRotateDeviceToken(w.ctx, w.devA.ID, tok.ID, sha256Hex("rot-live"), time.Hour))

	// Past its expiry: refused, and nothing is minted.
	w2 := seed(t)
	tok2 := expire(w2)
	w2.clk.Advance(2 * time.Hour)
	if _, err := w2.s.AgentRotateDeviceToken(w2.ctx, w2.devA.ID, tok2.ID, sha256Hex("rot-expired"), time.Hour); !errors.Is(err, ErrConflict) {
		t.Fatalf("rotate with an expired token = %v, want ErrConflict", err)
	}
	if _, _, err := w2.s.AgentLookupDeviceToken(w2.ctx, sha256Hex("rot-expired")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a refused rotation minted a token: %v", err)
	}
}
