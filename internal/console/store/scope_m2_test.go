package store

import (
	"errors"
	"time"
)

// addM2Matrix adds the M2-W1C methods to the §3.5 scope matrix (matrix() in
// scope_test.go calls it): every scoped method of customers_m2.go,
// devices_m2.go and runs_m2.go runs as an all-customer admin, an operator
// scoped to A, a read-only user scoped to B and the zero Scope.
func addM2Matrix(m map[string]matrixCase) {
	actor := AuditActor{AdminID: "01J9Y4K3A0B1C2D3E4F5G6H7AA", Username: "matrix", Method: "api"}
	m["ListCustomerRows"] = matrixCase{kind: list, call: func(w *world, sc Scope, t *target) ([]string, error) {
		rows, err := w.s.ListCustomerRows(w.ctx, sc, CustomerListQuery{IncludeArchived: true})
		var out []string
		for _, r := range rows {
			out = append(out, r.ID)
		}
		return out, err
	}}
	m["GetCustomerRow"] = matrixCase{kind: byID, call: func(w *world, sc Scope, t *target) ([]string, error) {
		_, err := w.s.GetCustomerRow(w.ctx, sc, t.cust)
		return nil, err
	}}
	m["CreateCustomerAudited"] = matrixCase{kind: globalWrite, call: func(w *world, sc Scope, t *target) ([]string, error) {
		_, err := w.s.CreateCustomerAudited(w.ctx, sc, CustomerInput{Name: uniq("Audited Customer")}, actor)
		return nil, err
	}}
	m["UpdateCustomerAudited"] = matrixCase{kind: byID, call: func(w *world, sc Scope, t *target) ([]string, error) {
		notes := "changed by " + uniq("matrix")
		_, _, err := w.s.UpdateCustomerAudited(w.ctx, sc, t.cust, CustomerPatch{Notes: &notes}, actor)
		return nil, err
	}}
	m["PolicyChoicesFor"] = matrixCase{kind: list, call: func(w *world, sc Scope, t *target) ([]string, error) {
		var out []string
		for _, id := range []string{w.custA.ID, w.custB.ID} {
			choices, err := w.s.PolicyChoicesFor(w.ctx, sc, id)
			if err != nil {
				return nil, err
			}
			for _, c := range choices {
				if c.Scope == "customer" {
					out = append(out, id)
				}
			}
		}
		return out, nil
	}}
	m["ListDeviceRows"] = matrixCase{kind: list, call: func(w *world, sc Scope, t *target) ([]string, error) {
		rows, err := w.s.ListDeviceRows(w.ctx, sc, DeviceFilter{IncludeRetired: true})
		var out []string
		for _, r := range rows {
			out = append(out, r.CustomerID)
		}
		return out, err
	}}
	m["DeviceTable"] = matrixCase{kind: list, call: func(w *world, sc Scope, t *target) ([]string, error) {
		tb, err := w.s.DeviceTable(w.ctx, sc, DeviceFilter{IncludeRetired: true}, nil, DeviceTableQuery{}, t0, time.UTC)
		var out []string
		for _, r := range tb.Rows {
			out = append(out, r.CustomerID)
		}
		return out, err
	}}
	m["DeviceNextDue"] = matrixCase{kind: byID, call: func(w *world, sc Scope, t *target) ([]string, error) {
		got, err := w.s.DeviceNextDue(w.ctx, sc, []string{t.dev}, nil, t0, time.UTC)
		if err == nil && len(got) == 0 {
			err = ErrNotFound // a device outside scope has no entry: the same answer as an unknown one
		}
		return nil, err
	}}
	m["ListDeviceAudit"] = matrixCase{kind: byID, call: func(w *world, sc Scope, t *target) ([]string, error) {
		_, err := w.s.ListDeviceAudit(w.ctx, sc, t.dev, Page{Limit: 10})
		return nil, err
	}}
	m["DeviceAuditTotal"] = matrixCase{kind: byID, call: func(w *world, sc Scope, t *target) ([]string, error) {
		_, err := w.s.DeviceAuditTotal(w.ctx, sc, t.dev)
		return nil, err
	}}
	m["ListDeviceRuns"] = matrixCase{kind: byID, call: func(w *world, sc Scope, t *target) ([]string, error) {
		pg, err := w.s.ListDeviceRuns(w.ctx, sc, t.dev, RunFilter{}, 1, 25)
		if err == nil && len(pg.Items) == 0 {
			err = errors.New("the seeded run is missing from the history")
		}
		return nil, err
	}}
	m["ListDeviceRunsAt"] = matrixCase{kind: byID, call: func(w *world, sc Scope, t *target) ([]string, error) {
		_, err := w.s.ListDeviceRunsAt(w.ctx, sc, t.dev, RunFilter{}, 0, 25)
		return nil, err
	}}
	m["UpdateDeviceSettings"] = matrixCase{kind: byID, call: func(w *world, sc Scope, t *target) ([]string, error) {
		notes := "changed by " + uniq("matrix")
		_, _, err := w.s.UpdateDeviceSettings(w.ctx, sc, t.dev, DeviceSettingsPatch{Notes: &notes}, actor)
		return nil, err
	}}
	addPolicyMatrix(m) // M2-W2A: policies and overrides (scope_policies_m2_test.go)
}
