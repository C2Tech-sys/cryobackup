package store

import (
	"encoding/json"
	"fmt"
)

// addPolicyMatrix adds the M2-W2A methods (policies_m2.go, overrides_m2.go) to
// the §3.5 scope matrix; addM2Matrix calls it. Every scoped method runs as an
// all-customer admin, an operator scoped to A, a read-only user scoped to B and
// the zero Scope, against both customers' seeded rows.
func addPolicyMatrix(m map[string]matrixCase) {
	actor := func(w *world) AuditActor { return AuditActor{AdminID: w.admin.ID, Username: "matrix", Method: "api"} }
	// fresh holds, per customer, the unassigned policy the delete cases act on.
	fresh := map[string]string{}
	mkFresh := func(w *world, t *target) {
		p := must[PolicyRecord](w.t)(w.s.CreatePolicyAudited(w.ctx, ScopeAll(),
			PolicyInput{Scope: "customer", CustomerID: t.cust, Name: uniq("Disposable")}, actor(w)))
		fresh[t.cust] = p.ID
	}
	change := func() ProposedPolicy {
		seq++
		return ProposedPolicy{Sections: map[string]json.RawMessage{"sla": json.RawMessage(fmt.Sprintf(`{"stale_after_hours":%d}`, 60+seq%10))}}
	}
	m["ListPolicySummaries"] = matrixCase{kind: list, globalRowsOK: true, call: func(w *world, sc Scope, t *target) ([]string, error) {
		ps, err := w.s.ListPolicySummaries(w.ctx, sc)
		var rows []string
		for _, p := range ps {
			rows = append(rows, p.CustomerID)
		}
		return rows, err
	}}
	m["ListPolicyRevisionViews"] = matrixCase{kind: byID, call: func(w *world, sc Scope, t *target) ([]string, error) {
		_, err := w.s.ListPolicyRevisionViews(w.ctx, sc, t.policy, 5)
		return nil, err
	}}
	m["CreatePolicyAudited"] = matrixCase{kind: byID, call: func(w *world, sc Scope, t *target) ([]string, error) {
		_, err := w.s.CreatePolicyAudited(w.ctx, sc, PolicyInput{Scope: "customer", CustomerID: t.cust, Name: uniq("Created")}, actor(w))
		return nil, err
	}}
	m["PreviewPolicyChange"] = matrixCase{kind: byID, call: func(w *world, sc Scope, t *target) ([]string, error) {
		_, err := w.s.PreviewPolicyChange(w.ctx, sc, t.policy, w.admin.ID, change())
		return nil, err
	}}
	m["UpdatePolicyAudited"] = matrixCase{kind: byID, call: func(w *world, sc Scope, t *target) ([]string, error) {
		p := change()
		rep, err := w.s.PreviewPolicyChange(w.ctx, sc, t.policy, w.admin.ID, p)
		if err != nil {
			return nil, err
		}
		_, _, err = w.s.UpdatePolicyAudited(w.ctx, sc, t.policy, p, rep.Token, actor(w))
		return nil, err
	}}
	m["PreviewPolicyDelete"] = matrixCase{kind: byID, prep: mkFresh, call: func(w *world, sc Scope, t *target) ([]string, error) {
		_, err := w.s.PreviewPolicyDelete(w.ctx, sc, fresh[t.cust], w.admin.ID)
		return nil, err
	}}
	m["DeletePolicyAudited"] = matrixCase{kind: byID, prep: mkFresh, call: func(w *world, sc Scope, t *target) ([]string, error) {
		rep, err := w.s.PreviewPolicyDelete(w.ctx, sc, fresh[t.cust], w.admin.ID)
		if err != nil {
			return nil, err
		}
		return nil, w.s.DeletePolicyAudited(w.ctx, sc, fresh[t.cust], rep.Token, actor(w))
	}}
	m["PreviewPolicyAssignment"] = matrixCase{kind: byID, call: func(w *world, sc Scope, t *target) ([]string, error) {
		_, err := w.s.PreviewPolicyAssignment(w.ctx, sc, t.cust, t.policy, w.admin.ID)
		return nil, err
	}}
	m["AssignCustomerPolicyAudited"] = matrixCase{kind: byID, call: func(w *world, sc Scope, t *target) ([]string, error) {
		rep, err := w.s.PreviewPolicyAssignment(w.ctx, sc, t.cust, t.policy, w.admin.ID)
		if err != nil {
			return nil, err
		}
		_, err = w.s.AssignCustomerPolicyAudited(w.ctx, sc, t.cust, t.policy, rep.Token, actor(w))
		return nil, err
	}}
	m["GetCustomerPolicy"] = matrixCase{kind: byID, call: func(w *world, sc Scope, t *target) ([]string, error) {
		_, err := w.s.GetCustomerPolicy(w.ctx, sc, t.cust)
		return nil, err
	}}
	m["GetDeviceOverride"] = matrixCase{kind: byID, call: func(w *world, sc Scope, t *target) ([]string, error) {
		_, err := w.s.GetDeviceOverride(w.ctx, sc, t.dev)
		return nil, err
	}}
	m["SetDeviceOverride"] = matrixCase{kind: byID, call: func(w *world, sc Scope, t *target) ([]string, error) {
		seq++
		in := OverrideInput{Patch: json.RawMessage(fmt.Sprintf(`{"sla":{"max_run_hours":%d}}`, 13+seq%10)), Reason: "matrix"}
		_, _, err := w.s.SetDeviceOverride(w.ctx, sc, t.dev, in, actor(w))
		return nil, err
	}}
	m["ClearDeviceOverride"] = matrixCase{kind: byID, call: func(w *world, sc Scope, t *target) ([]string, error) {
		_, err := w.s.ClearDeviceOverride(w.ctx, sc, t.dev, actor(w))
		return nil, err
	}}
	m["GetDeviceEffective"] = matrixCase{kind: byID, call: func(w *world, sc Scope, t *target) ([]string, error) {
		_, err := w.s.GetDeviceEffective(w.ctx, sc, t.dev)
		return nil, err
	}}
}
