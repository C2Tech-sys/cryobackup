package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/policy"
)

func sections(kv ...string) map[string]json.RawMessage {
	m := map[string]json.RawMessage{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = json.RawMessage(kv[i+1])
	}
	return m
}

func TestEffectivePolicyComposesDefaultCustomerAndOverride(t *testing.T) {
	w := seed(t)
	ctx := context.Background()

	// Customer B has no policy of its own: the effective policy is the default.
	def, defVer, defTag, err := w.s.EffectivePolicyFor(ctx, ScopeAll(), w.devB.ID)
	ok(t, err)
	want := policy.Default(policy.ClassUnknown)
	if wantTag := must[string](t)(policy.ETag(want)); defTag != wantTag || defVer != 1 {
		t.Fatalf("default effective: version %d etag %s (want 1, %s)", defVer, defTag, wantTag)
	}
	if def.Schedule.IntervalMinutes != 1440 {
		t.Fatalf("default interval = %d", def.Schedule.IntervalMinutes)
	}

	// Customer A gets a sparse policy: a 12 h interval and explicit retention rules (which must not inherit the default preset).
	rec := must[PolicyRecord](t)(w.s.CreatePolicy(ctx, ScopeAll(), PolicyInput{
		Scope: "customer", CustomerID: w.custA.ID, Name: "Alpha workstations", CreatedBy: w.admin.ID, Note: "initial",
		Sections: sections(
			"schedule", `{"interval_minutes":720}`,
			"retention", `{"rules":{"keep_daily":7,"keep_weekly":4},"keep_within":"30d"}`,
			"selection", `{"roots":["D:\\Data"]}`,
		),
	}))
	if rec.Version != 1 || rec.Scope != "customer" || rec.CustomerID != w.custA.ID {
		t.Fatalf("policy = %+v", rec)
	}
	a := must[Customer](t)(w.s.GetCustomer(ctx, ScopeAll(), w.custA.ID))
	a.PolicyID = rec.ID
	must[Customer](t)(w.s.UpdateCustomer(ctx, ScopeAll(), w.custA.ID, a))

	p, ver, tagA, err := w.s.EffectivePolicyFor(ctx, ScopeAll(), w.devA.ID)
	ok(t, err)
	if p.Schedule.IntervalMinutes != 720 || p.Schedule.Window != "*/0-24" {
		t.Fatalf("schedule = %+v (interval from the customer, the rest from the default)", p.Schedule)
	}
	if p.Retention.Preset != "" || p.Retention.Rules == nil || p.Retention.Rules.KeepDaily != 7 || p.Retention.KeepWithin != "30d" {
		t.Fatalf("retention = %+v: rules must replace the default preset", p.Retention)
	}
	if len(p.Selection.Roots) != 1 || len(p.Selection.Presets) != 0 {
		t.Fatalf("selection = %+v: naming roots must drop the default presets", p.Selection)
	}
	if p.SLA.StaleAfterHours != 72 {
		t.Fatalf("an untouched section lost its default: %+v", p.SLA)
	}
	if ver != 1 || tagA == defTag {
		t.Fatalf("version %d, etag changed=%v", ver, tagA != defTag)
	}
	// Stable across calls.
	if _, _, again, _ := w.s.EffectivePolicyFor(ctx, ScopeAll(), w.devA.ID); again != tagA {
		t.Fatal("the ETag is not deterministic")
	}

	// A device override changes the ETag and not the version.
	insertOverride := func(patch string, expires *int64, cleared *int64) {
		t.Helper()
		ok(t, w.s.Write(ctx, func(tx *Tx) error {
			_, err := tx.Exec(`INSERT INTO device_policy_overrides (id, device_id, patch, reason, expires_at, cleared_at, created_at, updated_at)
				VALUES (?,?,?,?,?,?,?,?)`, tx.id(), w.devA.ID, patch, "test", expires, cleared, tx.nowMS(), tx.nowMS())
			return err
		}))
	}
	expired := t0.Add(-time.Hour).UnixMilli()
	insertOverride(`{"sla":{"stale_after_hours":24}}`, &expired, nil) // expired: ignored
	if _, _, tag, _ := w.s.EffectivePolicyFor(ctx, ScopeAll(), w.devA.ID); tag != tagA {
		t.Fatal("an expired override changed the policy")
	}
	// One active override per device: clear the expired one before adding the live one.
	ok(t, w.s.Write(ctx, func(tx *Tx) error {
		_, err := tx.Exec(`UPDATE device_policy_overrides SET cleared_at = ? WHERE device_id = ?`, tx.nowMS(), w.devA.ID)
		return err
	}))
	insertOverride(`{"sla":{"stale_after_hours":48},"retention":{"preset":"30_days"}}`, nil, nil)
	p, ver2, tagO, err := w.s.EffectivePolicyFor(ctx, ScopeAll(), w.devA.ID)
	ok(t, err)
	if p.SLA.StaleAfterHours != 48 || ver2 != ver || tagO == tagA {
		t.Fatalf("override: stale=%d version=%d etag changed=%v", p.SLA.StaleAfterHours, ver2, tagO != tagA)
	}
	if p.Retention.Preset != "30_days" || p.Retention.Rules != nil {
		t.Fatalf("an override naming a preset must replace the customer's rules: %+v", p.Retention)
	}
	// The agent-side accessor agrees.
	if ap, av, atag, err := w.s.AgentEffectivePolicy(ctx, w.devA.ID); err != nil || atag != tagO || av != ver || ap.SLA.StaleAfterHours != 48 {
		t.Fatalf("AgentEffectivePolicy = %v %v %v", av, atag, err)
	}
	// Scope: customer B's scope cannot compose A's device.
	if _, _, _, err := w.s.EffectivePolicyFor(ctx, ScopeOf(w.custB.ID), w.devA.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("out-of-scope effective policy = %v", err)
	}
}

func TestPolicyCreateUpdateAndRevisions(t *testing.T) {
	w := seed(t)
	ctx := context.Background()
	rec := must[PolicyRecord](t)(w.s.CreatePolicy(ctx, ScopeAll(), PolicyInput{Scope: "customer", CustomerID: w.custA.ID, Name: "P", Sections: sections("schedule", `{"interval_minutes":600}`)}))
	w.clk.Advance(time.Minute)
	name := "P renamed"
	upd := must[PolicyRecord](t)(w.s.UpdatePolicy(ctx, ScopeAll(), rec.ID, PolicyUpdate{
		ExpectedVersion: 1, Name: &name, Sections: sections("sla", `{"stale_after_hours":60}`), ChangedBy: w.admin.ID, Note: "tighten",
	}))
	if upd.Version != 2 || upd.Name != name || string(upd.Sections["schedule"]) != `{"interval_minutes":600}` || !upd.UpdatedAt.Equal(t0.Add(time.Minute)) {
		t.Fatalf("updated = %+v (untouched sections must stay)", upd)
	}
	revs := must[[]PolicyRevision](t)(w.s.ListPolicyRevisions(ctx, ScopeAll(), rec.ID))
	if len(revs) != 2 || revs[0].Version != 2 || revs[0].ChangeNote != "tighten" || revs[0].ChangedBy != w.admin.ID || revs[1].Version != 1 {
		t.Fatalf("revisions = %+v", revs)
	}
	var body map[string]json.RawMessage
	ok(t, json.Unmarshal(revs[0].Body, &body))
	if len(body) != 9 || string(body["sla"]) != `{"stale_after_hours":60}` {
		t.Fatalf("revision body = %s", revs[0].Body)
	}
	// Optimistic concurrency.
	if _, err := w.s.UpdatePolicy(ctx, ScopeAll(), rec.ID, PolicyUpdate{ExpectedVersion: 1, Name: &name}); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale ExpectedVersion = %v, want ErrConflict", err)
	}
	// Validation: unseen must not be shorter than stale; unknown and non-object sections.
	bad := []map[string]json.RawMessage{
		sections("sla", `{"stale_after_hours":500,"unseen_after_hours":100}`),
		sections("nonsense", `{}`),
		sections("schedule", `[1]`),
	}
	for i, b := range bad {
		if _, err := w.s.UpdatePolicy(ctx, ScopeAll(), rec.ID, PolicyUpdate{Sections: b}); !errors.Is(err, ErrInvalid) {
			t.Errorf("bad update %d = %v, want ErrInvalid", i, err)
		}
	}
	if got := must[PolicyRecord](t)(w.s.GetPolicy(ctx, ScopeAll(), rec.ID)); got.Version != 2 {
		t.Fatalf("a rejected update changed the version to %d", got.Version)
	}
	if _, err := w.s.CreatePolicy(ctx, ScopeAll(), PolicyInput{Scope: "customer", CustomerID: w.custA.ID, Name: "Q", Sections: bad[0]}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("create with an invalid composed policy = %v", err)
	}
}

func TestPolicyScopeRules(t *testing.T) {
	w := seed(t)
	ctx := context.Background()
	recB := must[PolicyRecord](t)(w.s.CreatePolicy(ctx, ScopeAll(), PolicyInput{Scope: "customer", CustomerID: w.custB.ID, Name: "B only", Sections: sections("schedule", `{"interval_minutes":900}`)}))
	scopeA := ScopeOf(w.custA.ID)
	if _, err := w.s.GetPolicy(ctx, scopeA, recB.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("A reading B's policy = %v, want ErrNotFound", err)
	}
	if _, err := w.s.UpdatePolicy(ctx, scopeA, recB.ID, PolicyUpdate{Sections: sections("sla", `{"stale_after_hours":80}`)}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("A updating B's policy = %v, want ErrNotFound", err)
	}
	if _, err := w.s.CreatePolicy(ctx, scopeA, PolicyInput{Scope: "customer", CustomerID: w.custB.ID, Name: "x"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("A creating a policy for B = %v, want ErrNotFound", err)
	}
	def := must[PolicyRecord](t)(w.s.DefaultPolicy(ctx))
	if _, err := w.s.GetPolicy(ctx, scopeA, def.ID); err != nil {
		t.Fatalf("a scoped admin can read the default policy: %v", err)
	}
	if _, err := w.s.UpdatePolicy(ctx, scopeA, def.ID, PolicyUpdate{Sections: sections("sla", `{"stale_after_hours":80}`)}); !errors.Is(err, ErrScope) {
		t.Fatalf("a scoped admin editing the default = %v, want ErrScope", err)
	}
	if _, err := w.s.CreatePolicy(ctx, scopeA, PolicyInput{Scope: "template", Name: "T"}); !errors.Is(err, ErrScope) {
		t.Fatalf("a scoped admin creating a template = %v, want ErrScope", err)
	}
	list := must[[]PolicyRecord](t)(w.s.ListPolicies(ctx, scopeA))
	if len(list) != 1 || list[0].Scope != "default" {
		t.Fatalf("scope A lists %d policies: %+v", len(list), list)
	}
	if all := must[[]PolicyRecord](t)(w.s.ListPolicies(ctx, ScopeAll())); len(all) != 2 {
		t.Fatalf("all lists %d policies", len(all))
	}
	// A customer cannot adopt another customer's policy.
	a := must[Customer](t)(w.s.GetCustomer(ctx, ScopeAll(), w.custA.ID))
	a.PolicyID = recB.ID
	if _, err := w.s.UpdateCustomer(ctx, ScopeAll(), w.custA.ID, a); !errors.Is(err, ErrInvalid) {
		t.Fatalf("adopting another customer's policy = %v, want ErrInvalid", err)
	}
	// ... nor the default policy by id (NULL means default).
	a.PolicyID = def.ID
	if _, err := w.s.UpdateCustomer(ctx, ScopeAll(), w.custA.ID, a); !errors.Is(err, ErrInvalid) {
		t.Fatalf("pointing a customer at the default by id = %v, want ErrInvalid", err)
	}
	// A template is fine for any customer.
	tpl := must[PolicyRecord](t)(w.s.CreatePolicy(ctx, ScopeAll(), PolicyInput{Scope: "template", Name: "Server", TemplateDeviceClass: "desktop", Sections: sections("schedule", `{"interval_minutes":360}`)}))
	a.PolicyID = tpl.ID
	must[Customer](t)(w.s.UpdateCustomer(ctx, ScopeAll(), w.custA.ID, a))
	if p, _, _, _ := w.s.EffectivePolicyFor(ctx, ScopeAll(), w.devA.ID); p.Schedule.IntervalMinutes != 360 {
		t.Fatalf("template not applied: %d", p.Schedule.IntervalMinutes)
	}
}

func TestNoDefaultPolicyIsLoud(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	must[StorageTarget](t)(s.CreateStorageTarget(ctx, StorageTarget{Name: "T", Provider: "wasabi", Endpoint: "e", Bucket: "b", GatewayURL: "g", CredentialRef: "C", IsDefault: true}))
	c := must[Customer](t)(s.CreateCustomer(ctx, ScopeAll(), Customer{Name: "No Policy Co"}))
	must[EnrollmentToken](t)(s.CreateEnrollmentToken(ctx, ScopeAll(), EnrollmentToken{CustomerID: c.ID, TokenHash: sha256Hex("t"), ExpiresAt: t0.Add(time.Hour)}))
	in := enrollInput("t", "NP-1")
	if _, err := s.AgentEnroll(ctx, in); !errors.Is(err, ErrNoDefaultPolicy) {
		t.Fatalf("enroll without a default policy and without an explicit grace = %v, want ErrNoDefaultPolicy", err)
	}
	in.FirstBackupGrace = 72 * time.Hour
	res := must[EnrollResult](t)(s.AgentEnroll(ctx, in))
	_, _, _, err := s.EffectivePolicyFor(ctx, ScopeAll(), res.Device.ID)
	if !errors.Is(err, ErrNoDefaultPolicy) {
		t.Fatalf("EffectivePolicyFor without a default policy = %v, want ErrNoDefaultPolicy", err)
	}
}

func TestNormalizeSectionUnions(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"retention", `{"rules":{"keep_daily":3},"keep_within":"7d"}`, `{"keep_within":"7d","preset":null,"rules":{"keep_daily":3}}`},
		{"retention", `{"preset":"30_days"}`, `{"preset":"30_days","rules":null}`},
		{"retention", `{"keep_within":"5d"}`, `{"keep_within":"5d"}`},
		{"selection", `{"roots":["D:\\x"]}`, `{"presets":[],"roots":["D:\\x"]}`},
		{"selection", `{"presets":["fixed_drives"]}`, `{"presets":["fixed_drives"],"roots":[]}`},
		{"selection", `{"excludes":["*.tmp"]}`, `{"excludes":["*.tmp"]}`},
		{"sla", `{"stale_after_hours":5}`, `{"stale_after_hours":5}`},
	}
	for _, c := range cases {
		got := string(normalizeSection(c.name, json.RawMessage(c.in)))
		if got != c.want {
			t.Errorf("normalizeSection(%s, %s) = %s, want %s", c.name, c.in, got, c.want)
		}
	}
}
