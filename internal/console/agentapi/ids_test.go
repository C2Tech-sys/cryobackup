package agentapi

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/ulid"
)

// P1-FIX7: customer_id and policy_id on the wire.

func TestEnrollAndPolicyCarryCustomerAndPolicyIDs(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	out, r := h.enroll(nonce(30))
	if r.code != 200 {
		t.Fatalf("enroll: %d %s", r.code, r.body)
	}
	dev, err := h.st.GetDevice(ctx, store.ScopeAll(), out.DeviceID)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.CustomerID) != 26 || !ulid.Valid(out.CustomerID) || out.CustomerID != dev.CustomerID {
		t.Fatalf("customer_id %q, stored device row says %q", out.CustomerID, dev.CustomerID)
	}
	// With no customer policy the document is the default policy.
	def, err := h.st.DefaultPolicy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cust, err := h.st.GetCustomer(ctx, store.ScopeAll(), dev.CustomerID)
	if err != nil {
		t.Fatal(err)
	}
	want := def
	if cust.PolicyID != "" {
		if want, err = h.st.GetPolicy(ctx, store.ScopeAll(), cust.PolicyID); err != nil {
			t.Fatal(err)
		}
	}
	if len(out.Policy.PolicyID) != 26 || !ulid.Valid(out.Policy.PolicyID) || out.Policy.PolicyID != want.ID || out.Policy.Version != want.Version {
		t.Fatalf("enroll policy_id %q version %d, stored row %q version %d", out.Policy.PolicyID, out.Policy.Version, want.ID, want.Version)
	}
	// The pull carries the same id.
	var p api.PolicyResponse
	pr := h.do("GET", api.RoutePolicy, string(out.DeviceToken), nil)
	if pr.code != 200 {
		t.Fatalf("policy: %d %s", pr.code, pr.body)
	}
	pr.json(t, &p)
	if len(p.PolicyID) != 26 || p.PolicyID != want.ID || !ulid.Valid(p.PolicyID) {
		t.Fatalf("pull policy_id %q, want %q", p.PolicyID, want.ID)
	}

	// Give the customer a policy of its own: the id switches to that row, for a new enrollment and for the pull.
	rec, err := h.st.CreatePolicy(ctx, store.ScopeAll(), store.PolicyInput{Scope: "customer", CustomerID: cust.ID, Name: "Own",
		Sections: map[string]json.RawMessage{"schedule": json.RawMessage(`{"interval_minutes":720}`)}})
	if err != nil {
		t.Fatal(err)
	}
	cust.PolicyID = rec.ID
	if _, err := h.st.UpdateCustomer(ctx, store.ScopeAll(), cust.ID, cust); err != nil {
		t.Fatal(err)
	}
	h.clk.Advance(time.Minute)
	out2, r := h.enroll(nonce(31))
	if r.code != 200 {
		t.Fatalf("second enroll: %d %s", r.code, r.body)
	}
	if out2.Policy.PolicyID != rec.ID || out2.Policy.PolicyID == def.ID || out2.CustomerID != cust.ID {
		t.Fatalf("second enroll: policy_id %q customer_id %q, want %q %q", out2.Policy.PolicyID, out2.CustomerID, rec.ID, cust.ID)
	}
	h.clk.Advance(time.Minute)
	pr = h.do("GET", api.RoutePolicy, string(out.DeviceToken), nil)
	pr.json(t, &p)
	if pr.code != 200 || p.PolicyID != rec.ID {
		t.Fatalf("pull after the customer policy: %d policy_id %q, want %q", pr.code, p.PolicyID, rec.ID)
	}
}

// The device id is chosen by the handler before anything is wrapped: the escrow row
// opens under the device id the response carries, and a replay still opens.
func TestEnrollWrapsUnderTheRealDeviceIDFromTheStart(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	out, r := h.enroll(nonce(32))
	if r.code != 200 || !ulid.Valid(out.DeviceID) {
		t.Fatalf("enroll: %d %s", r.code, r.body)
	}
	got, err := h.keys.Unwrap(ctx, out.DeviceID)
	if err != nil || got != string(out.RepoPassword) {
		t.Fatalf("Unwrap under the device id: %q %v", got, err)
	}
	gw, err := h.keys.GatewaySecret(ctx, out.DeviceID)
	if err != nil || gw != string(out.Gateway.Password) {
		t.Fatalf("GatewaySecret under the device id: %v", err)
	}
	again, r := h.enroll(nonce(32))
	if r.code != 200 || again.DeviceID != out.DeviceID || again.RepoPassword != out.RepoPassword {
		t.Fatalf("replay: %d %s", r.code, r.body)
	}
}
