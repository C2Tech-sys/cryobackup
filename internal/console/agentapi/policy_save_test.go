package agentapi

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// M2-W2A item 6: a policy or override saved in the console reaches the agent
// through the existing policy endpoint and ETag, with nothing else to do.
func TestSavedPolicyAndOverrideReachTheAgentThroughTheETag(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	e := h.enrolled()
	tok := string(e.DeviceToken)
	admin, err := h.st.GetAdminByUsername(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	actor := store.AuditActor{AdminID: admin.ID, Username: "admin", Method: "web"}

	pull := func(inm string) (resp, api.PolicyResponse) {
		t.Helper()
		h.clk.Advance(time.Minute) // the policy route allows 6 pulls a minute
		var hdr []string
		if inm != "" {
			hdr = []string{"If-None-Match", inm}
		}
		r := h.do("GET", api.RoutePolicy, tok, nil, hdr...)
		var p api.PolicyResponse
		if r.code == 200 {
			r.json(t, &p)
		}
		return r, p
	}
	// acknowledge the pending gateway secret so that an unchanged policy answers 304
	hb := hbReq(h)
	hb.GatewaySecretAcked = true
	h.clk.Advance(time.Minute)
	if r := h.do("POST", api.RouteHeartbeat, tok, hb); r.code != 200 {
		t.Fatalf("heartbeat: %d %s", r.code, r.body)
	}
	r, base := pull("")
	if r.code != 200 {
		t.Fatalf("first pull: %d", r.code)
	}
	if r, _ = pull(base.ETag); r.code != 304 {
		t.Fatalf("nothing saved yet: want 304, got %d", r.code)
	}

	defID := base.PolicyID
	save := func(id string, p store.ProposedPolicy) {
		t.Helper()
		rep, err := h.st.PreviewPolicyChange(ctx, store.ScopeAll(), id, admin.ID, p)
		if err != nil {
			t.Fatalf("preview: %v", err)
		}
		if _, changed, err := h.st.UpdatePolicyAudited(ctx, store.ScopeAll(), id, p, rep.Token, actor); err != nil || !changed {
			t.Fatalf("save: %v changed=%v", err, changed)
		}
	}

	// 1. the default policy is edited: the agent sees a new ETag, version and value
	def, err := h.st.GetPolicy(ctx, store.ScopeAll(), defID)
	if err != nil {
		t.Fatal(err)
	}
	var sched map[string]any
	if err := json.Unmarshal(def.Sections["schedule"], &sched); err != nil {
		t.Fatal(err)
	}
	sched["interval_minutes"] = 480
	raw, _ := json.Marshal(sched)
	save(defID, store.ProposedPolicy{Sections: map[string]json.RawMessage{"schedule": raw}})
	r, saved := pull(base.ETag)
	if r.code != 200 || saved.ETag == base.ETag || saved.Version != base.Version+1 || saved.Policy.Schedule.IntervalMinutes != 480 {
		t.Fatalf("after the save: %d etag %q->%q v%d->%d interval %d", r.code, base.ETag, saved.ETag, base.Version, saved.Version, saved.Policy.Schedule.IntervalMinutes)
	}
	if r, _ = pull(saved.ETag); r.code != 304 {
		t.Fatalf("the new ETag must be stable: %d", r.code)
	}

	// 2. a per-device override changes the ETag (not the policy version) and the value
	if _, _, err := h.st.SetDeviceOverride(ctx, store.ScopeAll(), e.DeviceID, store.OverrideInput{
		Patch: json.RawMessage(`{"schedule":{"interval_minutes":240}}`), Reason: "test"}, actor); err != nil {
		t.Fatal(err)
	}
	r, ov := pull(saved.ETag)
	if r.code != 200 || ov.ETag == saved.ETag || ov.Version != saved.Version || ov.Policy.Schedule.IntervalMinutes != 240 {
		t.Fatalf("after the override: %d etag same=%v v%d interval %d", r.code, ov.ETag == saved.ETag, ov.Version, ov.Policy.Schedule.IntervalMinutes)
	}

	// 3. clearing it returns the agent to the saved policy
	if _, err := h.st.ClearDeviceOverride(ctx, store.ScopeAll(), e.DeviceID, actor); err != nil {
		t.Fatal(err)
	}
	r, back := pull(ov.ETag)
	if r.code != 200 || back.ETag != saved.ETag || back.Policy.Schedule.IntervalMinutes != 480 {
		t.Fatalf("after clearing: %d etag %q want %q interval %d", r.code, back.ETag, saved.ETag, back.Policy.Schedule.IntervalMinutes)
	}
}
