package agentapi

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/console/enroll"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// M2-W3B: identity-conflict detection on heartbeat, and the enrollment token's
// policy, both through the real agent API.

func (h *harness) beat(tok string, fp string) resp {
	h.t.Helper()
	h.clk.Advance(21 * time.Second) // past the 20 s heartbeat limiter
	hb := hbReq(h)
	hb.HWFingerprint = fp
	return h.do("POST", api.RouteHeartbeat, tok, hb)
}

func (h *harness) deviceState(id string) state.DeviceState {
	h.t.Helper()
	f, err := h.st.LoadDeviceFacts(context.Background(), store.ScopeAll(), id)
	if err != nil {
		h.t.Fatal(err)
	}
	return state.Classify(f, h.clk.Now())
}

func (h *harness) auditRows(action, deviceID string) []store.AuditEvent {
	h.t.Helper()
	l, err := h.st.ListAudit(context.Background(), store.ScopeAll(), store.AuditFilter{Action: action, ObjectID: deviceID}, store.Page{Limit: 50})
	if err != nil {
		h.t.Fatal(err)
	}
	return l.Items
}

func TestHeartbeatWithAChangedFingerprintRaisesTheIdentityConflictOnce(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	e := h.enrolled() // enrolled with fingerprint abab...
	tok := string(e.DeviceToken)
	same := strings.Repeat("ab", 16)
	other := strings.Repeat("cd", 16)

	// The enrolled fingerprint: no conflict, however many times.
	for i := 0; i < 2; i++ {
		if r := h.beat(tok, same); r.code != 200 {
			t.Fatalf("heartbeat with the enrolled fingerprint: %d %s", r.code, r.body)
		}
	}
	// A heartbeat that sends no fingerprint (an older agent) is not a mismatch.
	if r := h.beat(tok, ""); r.code != 200 {
		t.Fatalf("heartbeat without a fingerprint: %d %s", r.code, r.body)
	}
	dev, _ := h.st.GetDevice(ctx, store.ScopeAll(), e.DeviceID)
	if dev.IdentityConflictAt != nil || h.deviceState(e.DeviceID).Primary == state.StateIdentityConflict {
		t.Fatalf("a conflict was raised without a changed fingerprint: %+v", dev.IdentityConflictAt)
	}

	// A different fingerprint: the heartbeat is still answered 200, the flag is set, the board shows it.
	r := h.beat(tok, other)
	if r.code != 200 {
		t.Fatalf("heartbeat with a new fingerprint must still be accepted: %d %s", r.code, r.body)
	}
	var out api.HeartbeatResponse
	r.json(t, &out)
	if out.PolicyETag == "" {
		t.Fatalf("the response is the normal one: %+v", out)
	}
	dev, _ = h.st.GetDevice(ctx, store.ScopeAll(), e.DeviceID)
	if dev.IdentityConflictAt == nil || dev.RetiredAt != nil {
		t.Fatalf("after the changed fingerprint: conflict=%v retired=%v", dev.IdentityConflictAt, dev.RetiredAt)
	}
	if got := h.deviceState(e.DeviceID).Primary; got != state.StateIdentityConflict {
		t.Fatalf("board state = %s, want IDENTITY_CONFLICT", got)
	}
	if h.pub.changed[e.DeviceID] == 0 {
		t.Fatal("the change was not published")
	}
	raisedAt := *dev.IdentityConflictAt

	// The device is never locked out: more heartbeats from either machine, the policy pull and the command poll all work.
	for _, fp := range []string{other, same, other} {
		if r := h.beat(tok, fp); r.code != 200 {
			t.Fatalf("heartbeat during a conflict: %d %s", r.code, r.body)
		}
	}
	h.clk.Advance(time.Minute)
	if r := h.do("GET", api.RoutePolicy, tok, nil); r.code != 200 {
		t.Fatalf("policy pull during a conflict: %d", r.code)
	}
	// Raised once: one system audit row, the original time, no fingerprint text.
	rows := h.auditRows("device.identity_conflict", e.DeviceID)
	if len(rows) != 1 || rows[0].ActorType != "system" || rows[0].Severity != "critical" {
		t.Fatalf("audit rows: %+v", rows)
	}
	if blob := string(rows[0].Details); strings.Contains(blob, same) || strings.Contains(blob, other) {
		t.Fatalf("a fingerprint is in the audit details: %s", blob)
	}
	dev, _ = h.st.GetDevice(ctx, store.ScopeAll(), e.DeviceID)
	if dev.IdentityConflictAt == nil || !dev.IdentityConflictAt.Equal(raisedAt) {
		t.Fatalf("the conflict time moved: %v -> %v", raisedAt, dev.IdentityConflictAt)
	}

	// Keep: the conflict ends and the next heartbeat's fingerprint becomes the device's. It does not come back.
	if _, err := h.st.ResolveIdentityConflict(ctx, store.ScopeAll(), e.DeviceID,
		store.IdentityResolution{Action: store.ResolveKeep, Reason: "hardware replaced"}, store.AuditActor{Username: "tester", Method: "api"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if r := h.beat(tok, other); r.code != 200 {
			t.Fatalf("heartbeat after keep: %d %s", r.code, r.body)
		}
	}
	dev, _ = h.st.GetDevice(ctx, store.ScopeAll(), e.DeviceID)
	if dev.IdentityConflictAt != nil || dev.HWFingerprint != other {
		t.Fatalf("after keep: conflict=%v fingerprint accepted=%v", dev.IdentityConflictAt, dev.HWFingerprint == other)
	}
	if n := len(h.auditRows("device.identity_conflict", e.DeviceID)); n != 1 {
		t.Fatalf("conflict audit rows after keep = %d, want still 1", n)
	}
}

func TestHeartbeatFingerprintFailureDoesNotFailTheHeartbeatAndLogsNoFingerprint(t *testing.T) {
	h := newHarness(t)
	e := h.enrolled()
	tok := string(e.DeviceToken)
	h.beat(tok, strings.Repeat("ab", 16))
	// An audit store that refuses writes: the conflict cannot be recorded, the heartbeat is still answered.
	if err := h.st.Write(context.Background(), func(tx *store.Tx) error {
		_, err := tx.Exec(`CREATE TRIGGER refuse_audits BEFORE INSERT ON audit_events BEGIN SELECT RAISE(ABORT, 'audit refused by the test'); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if r := h.beat(tok, strings.Repeat("cd", 16)); r.code != 200 {
		t.Fatalf("heartbeat when the conflict cannot be audited: %d %s", r.code, r.body)
	}
	dev, _ := h.st.GetDevice(context.Background(), store.ScopeAll(), e.DeviceID)
	if dev.IdentityConflictAt != nil {
		t.Fatal("the conflict was recorded without its audit row")
	}
}

func TestEnrollmentTokenPolicyReachesTheAgentsPolicyPull(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	custs, _ := h.st.ListCustomers(ctx, store.ScopeAll())
	custID := ""
	for _, c := range custs {
		if c.Name == "Northwind Dental" {
			custID = c.ID
		}
	}
	tpl, err := h.st.CreatePolicy(ctx, store.ScopeAll(), store.PolicyInput{Scope: "template", Name: "Slow first backup",
		Sections: map[string]json.RawMessage{"sla": json.RawMessage(`{"first_backup_grace_hours":200}`)}})
	if err != nil {
		t.Fatal(err)
	}
	withP, _, err := enroll.Mint(ctx, h.st, store.ScopeAll(), custID, enroll.MintOptions{Expires: time.Hour, PolicyID: tpl.ID})
	if err != nil {
		t.Fatal(err)
	}

	// A device enrolled with the plain token follows its customer's policy.
	plain, r := h.enroll(nonce(30))
	if r.code != 200 {
		t.Fatalf("plain enroll: %d %s", r.code, r.body)
	}
	// A device enrolled with the token that names the template gets it composed in, at enroll and on every pull.
	rr := h.do("POST", api.RouteEnroll, withP, enrollReq(nonce(31)))
	if rr.code != 200 {
		t.Fatalf("enroll with a policy token: %d %s", rr.code, rr.body)
	}
	var got api.EnrollResponse
	rr.json(t, &got)

	if plain.Policy.Policy.SLA.FirstBackupGraceHours == 200 {
		t.Fatal("the plain device got the token policy's value")
	}
	if got.Policy.Policy.SLA.FirstBackupGraceHours != 200 {
		t.Fatalf("enroll response policy: grace hours %d, want 200", got.Policy.Policy.SLA.FirstBackupGraceHours)
	}
	h.clk.Advance(time.Minute)
	pull := h.do("GET", api.RoutePolicy, string(got.DeviceToken), nil)
	if pull.code != 200 {
		t.Fatalf("policy pull: %d %s", pull.code, pull.body)
	}
	var pol api.PolicyResponse
	pull.json(t, &pol)
	if pol.Policy.SLA.FirstBackupGraceHours != 200 {
		t.Fatalf("pulled policy: grace hours %d, want 200 (the token's policy)", pol.Policy.SLA.FirstBackupGraceHours)
	}
	if pol.ETag != got.Policy.ETag {
		t.Fatalf("the enroll response and the pull disagree on the policy: %q vs %q", got.Policy.ETag, pol.ETag)
	}
	pp := h.do("GET", api.RoutePolicy, string(plain.DeviceToken), nil)
	var plainPol api.PolicyResponse
	pp.json(t, &plainPol)
	if plainPol.ETag == pol.ETag || plainPol.Policy.SLA.FirstBackupGraceHours == 200 {
		t.Fatalf("the plain device's policy moved: %+v", plainPol.Policy.SLA)
	}
	// The override is visible on the device and records where it came from.
	o, err := h.st.GetDeviceOverride(ctx, store.ScopeAll(), got.DeviceID)
	if err != nil || o == nil || !strings.Contains(o.Reason, "Slow first backup") {
		t.Fatalf("override: %+v %v", o, err)
	}
}
