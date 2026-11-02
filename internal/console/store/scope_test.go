package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Scope unit tests
// ---------------------------------------------------------------------------

func TestScopeBasics(t *testing.T) {
	if (Scope{}).Valid() {
		t.Fatal("the zero Scope must be invalid")
	}
	if !ScopeAll().Valid() || !ScopeAll().Allows("anything") {
		t.Fatal("ScopeAll must allow everything")
	}
	s := ScopeOf("a", "b")
	if !s.Valid() || !s.Allows("a") || !s.Allows("b") || s.Allows("c") {
		t.Fatalf("ScopeOf(a,b) = %+v", s)
	}
	none := ScopeOf()
	if !none.Valid() || none.Allows("a") {
		t.Fatalf("ScopeOf() must be valid and see nothing: %+v", none)
	}
	if (Scope{}).Allows("a") {
		t.Fatal("an invalid scope must allow nothing")
	}
	// ScopeOf copies its argument.
	ids := []string{"x"}
	cp := ScopeOf(ids...)
	ids[0] = "y"
	if !cp.Allows("x") || cp.Allows("y") {
		t.Fatal("ScopeOf aliases its input")
	}
	// customer_scope JSON: NULL is all, an array is that set, junk is refused.
	if sc, err := ScopeFromJSON(""); err != nil || !sc.All {
		t.Fatalf("NULL scope = %+v, %v", sc, err)
	}
	if sc, err := ScopeFromJSON(`["a","b"]`); err != nil || sc.All || !sc.Allows("b") {
		t.Fatalf("array scope = %+v, %v", sc, err)
	}
	if sc, err := ScopeFromJSON(`[]`); err != nil || sc.All || sc.Allows("a") || !sc.Valid() {
		t.Fatalf("empty array scope = %+v, %v", sc, err)
	}
	if sc, err := ScopeFromJSON(`null`); err != nil || sc.All {
		t.Fatalf("JSON null must fail closed, got %+v, %v", sc, err)
	}
	if _, err := ScopeFromJSON(`{"a":1}`); !errors.Is(err, ErrInvalid) {
		t.Fatalf("junk scope = %v", err)
	}
	clause, args := ScopeOf("a", "b").clause("customer_id")
	if clause != "customer_id IN (?,?)" || len(args) != 2 {
		t.Fatalf("clause = %q %v", clause, args)
	}
	if c, _ := none.clause("customer_id"); c != "0 = 1" {
		t.Fatalf("empty clause = %q", c)
	}
	if c, _ := ScopeAll().clause("customer_id"); c != "1 = 1" {
		t.Fatalf("all clause = %q", c)
	}
}

// ---------------------------------------------------------------------------
// The §3.5 scope matrix
// ---------------------------------------------------------------------------

// target is one customer's seeded rows, addressed by the matrix.
type target struct {
	name     string
	cust     string
	dev      string
	run      string
	policy   string
	token    string
	restore  string
	command  string
	snapshot string
	// fresh rows made by a case's prep hook, so the call under test has something to act on.
	freshToken string
	freshRun   string
}

type kind int

const (
	byID        kind = iota // addresses one customer's row: allowed inside scope, ErrNotFound outside
	list                    // returns rows: only rows of allowed customers may appear
	globalWrite             // needs scope.All: ErrScope otherwise
)

type matrixCase struct {
	kind kind
	// call runs the method as sc against t. For list kinds it returns the customer id of every returned row.
	call func(w *world, sc Scope, t *target) (rows []string, err error)
	// globalRowsOK: rows with no customer (the default policy) may appear for any valid scope.
	globalRowsOK bool
	// prep runs (as an all-customer admin) before the fingerprint is taken, to create the row the call acts on.
	prep func(w *world, t *target)
}

var seq int

func uniq(prefix string) string {
	seq++
	return fmt.Sprintf("%s-%d", prefix, seq)
}

// seedTarget adds one of every customer-owned row for a customer and its device.
func seedTarget(w *world, name string, cust Customer, dev Device) *target {
	ctx := w.ctx
	t := &target{name: name, cust: cust.ID, dev: dev.ID}
	w.heartbeat(dev.ID)
	runID := map[string]string{"A": runID1, "B": runID2}[name]
	ingest(w.t, w, dev.ID, runInput(runID, t0.Add(-time.Hour)))
	t.run = runID
	snap := strings.Repeat(map[string]string{"A": "a", "B": "b"}[name], 64)
	must2(w.t)(w.s.AgentReplaceSnapshots(ctx, dev.ID, []Snapshot{{SnapshotID: snap, TakenAt: t0.Add(-time.Hour), Paths: []string{`C:\Users`}}}))
	t.snapshot = snap
	pol := must[PolicyRecord](w.t)(w.s.CreatePolicy(ctx, ScopeAll(), PolicyInput{Scope: "customer", CustomerID: cust.ID, Name: name + " policy",
		Sections: sections("schedule", `{"interval_minutes":700}`)}))
	t.policy = pol.ID
	toks := must[[]EnrollmentToken](w.t)(w.s.ListEnrollmentTokens(ctx, ScopeAll(), cust.ID))
	t.token = toks[0].ID
	rr := must[RestoreRequest](w.t)(w.s.CreateRestoreRequest(ctx, ScopeAll(), RestoreRequest{DeviceID: dev.ID, SnapshotID: snap, Mode: "download_zip", ExpiresAt: t0.Add(time.Hour)}))
	t.restore = rr.ID
	cmd, _, err := w.s.EnqueueCommand(ctx, ScopeAll(), dev.ID, CommandInput{Type: "check_in", IdempotencyKey: "seed-" + name})
	ok(w.t, err)
	t.command = cmd.ID
	must[AuditEvent](w.t)(w.s.AppendAudit(ctx, AuditEvent{ActorType: "system", Action: "matrix.seed", CustomerID: cust.ID}))
	return t
}

// deviceCustomer maps device ids to customers for list results that carry only a device id.
func rowsOfRuns(w *world, items []Run, custOf map[string]string) []string {
	var out []string
	for _, r := range items {
		out = append(out, custOf[r.DeviceID])
	}
	return out
}

func matrix() map[string]matrixCase {
	m := map[string]matrixCase{
		"GetCustomer": {kind: byID, call: func(w *world, sc Scope, t *target) ([]string, error) {
			_, err := w.s.GetCustomer(w.ctx, sc, t.cust)
			return nil, err
		}},
		"ListCustomers": {kind: list, call: func(w *world, sc Scope, t *target) ([]string, error) {
			cs, err := w.s.ListCustomers(w.ctx, sc)
			var rows []string
			for _, c := range cs {
				rows = append(rows, c.ID)
			}
			return rows, err
		}},
		"CreateCustomer": {kind: globalWrite, call: func(w *world, sc Scope, t *target) ([]string, error) {
			_, err := w.s.CreateCustomer(w.ctx, sc, Customer{Name: uniq("New Customer")})
			return nil, err
		}},
		"UpdateCustomer": {kind: byID, call: func(w *world, sc Scope, t *target) ([]string, error) {
			c := must[Customer](w.t)(w.s.GetCustomer(w.ctx, ScopeAll(), t.cust))
			c.Notes = "changed by " + uniq("matrix")
			_, err := w.s.UpdateCustomer(w.ctx, sc, t.cust, c)
			return nil, err
		}},
		"CreatePolicy": {kind: byID, call: func(w *world, sc Scope, t *target) ([]string, error) {
			_, err := w.s.CreatePolicy(w.ctx, sc, PolicyInput{Scope: "customer", CustomerID: t.cust, Name: uniq("P")})
			return nil, err
		}},
		"GetPolicy": {kind: byID, call: func(w *world, sc Scope, t *target) ([]string, error) {
			_, err := w.s.GetPolicy(w.ctx, sc, t.policy)
			return nil, err
		}},
		"ListPolicies": {kind: list, globalRowsOK: true, call: func(w *world, sc Scope, t *target) ([]string, error) {
			ps, err := w.s.ListPolicies(w.ctx, sc)
			var rows []string
			for _, p := range ps {
				rows = append(rows, p.CustomerID)
			}
			return rows, err
		}},
		"UpdatePolicy": {kind: byID, call: func(w *world, sc Scope, t *target) ([]string, error) {
			seq++
			_, err := w.s.UpdatePolicy(w.ctx, sc, t.policy, PolicyUpdate{Sections: sections("sla", `{"stale_after_hours":`+fmt.Sprint(60+seq%10)+`}`)})
			return nil, err
		}},
		"ListPolicyRevisions": {kind: byID, call: func(w *world, sc Scope, t *target) ([]string, error) {
			_, err := w.s.ListPolicyRevisions(w.ctx, sc, t.policy)
			return nil, err
		}},
		"EffectivePolicyFor": {kind: byID, call: func(w *world, sc Scope, t *target) ([]string, error) {
			_, _, _, err := w.s.EffectivePolicyFor(w.ctx, sc, t.dev)
			return nil, err
		}},
		"EffectivePolicyDoc": {kind: byID, call: func(w *world, sc Scope, t *target) ([]string, error) {
			_, err := w.s.EffectivePolicyDoc(w.ctx, sc, t.dev)
			return nil, err
		}},
		"TenDayCounts": {kind: list, call: func(w *world, sc Scope, t *target) ([]string, error) {
			hc, err := w.s.TenDayCounts(w.ctx, sc, "", time.Time{}, time.Time{})
			custOf := map[string]string{w.devA.ID: w.custA.ID, w.devB.ID: w.custB.ID}
			var rows []string
			for _, h := range hc {
				rows = append(rows, custOf[h.DeviceID])
			}
			return rows, err
		}},
		"CreateEnrollmentToken": {kind: byID, call: func(w *world, sc Scope, t *target) ([]string, error) {
			_, err := w.s.CreateEnrollmentToken(w.ctx, sc, EnrollmentToken{CustomerID: t.cust, TokenHash: sha256Hex(uniq("tok")), ExpiresAt: w.clk.Now().Add(time.Hour)})
			return nil, err
		}},
		"RevokeEnrollmentToken": {kind: byID,
			prep: func(w *world, t *target) {
				t.freshToken = must[EnrollmentToken](w.t)(w.s.CreateEnrollmentToken(w.ctx, ScopeAll(), EnrollmentToken{CustomerID: t.cust, TokenHash: sha256Hex(uniq("rev")), ExpiresAt: w.clk.Now().Add(time.Hour)})).ID
			},
			call: func(w *world, sc Scope, t *target) ([]string, error) {
				return nil, w.s.RevokeEnrollmentToken(w.ctx, sc, t.freshToken)
			}},
		"ListEnrollmentTokens": {kind: list, call: func(w *world, sc Scope, t *target) ([]string, error) {
			ts, err := w.s.ListEnrollmentTokens(w.ctx, sc, "")
			var rows []string
			for _, x := range ts {
				rows = append(rows, x.CustomerID)
			}
			return rows, err
		}},
		"GetDevice": {kind: byID, call: func(w *world, sc Scope, t *target) ([]string, error) {
			_, err := w.s.GetDevice(w.ctx, sc, t.dev)
			return nil, err
		}},
		"ListDevices": {kind: list, call: func(w *world, sc Scope, t *target) ([]string, error) {
			l, err := w.s.ListDevices(w.ctx, sc, ListDevicesOptions{IncludeRetired: true})
			var rows []string
			for _, d := range l.Items {
				rows = append(rows, d.CustomerID)
			}
			return rows, err
		}},
		"UpdateDevice": {kind: byID, call: func(w *world, sc Scope, t *target) ([]string, error) {
			n := uniq("friendly")
			_, err := w.s.UpdateDevice(w.ctx, sc, t.dev, DeviceUpdate{FriendlyName: &n})
			return nil, err
		}},
		"GetPresence": {kind: byID, call: func(w *world, sc Scope, t *target) ([]string, error) {
			_, err := w.s.GetPresence(w.ctx, sc, t.dev)
			return nil, err
		}},
		"ActiveEscrow": {kind: byID, call: func(w *world, sc Scope, t *target) ([]string, error) {
			_, err := w.s.ActiveEscrow(w.ctx, sc, t.dev)
			return nil, err
		}},
		"GetMaintenanceSchedule": {kind: byID, call: func(w *world, sc Scope, t *target) ([]string, error) {
			_, err := w.s.GetMaintenanceSchedule(w.ctx, sc, t.dev)
			return nil, err
		}},
		"GetRun": {kind: byID, call: func(w *world, sc Scope, t *target) ([]string, error) {
			_, err := w.s.GetRun(w.ctx, sc, t.run)
			return nil, err
		}},
		"ListRuns": {kind: list, call: func(w *world, sc Scope, t *target) ([]string, error) {
			l, err := w.s.ListRuns(w.ctx, sc, "", Page{})
			return rowsOfRuns(w, l.Items, map[string]string{w.devA.ID: w.custA.ID, w.devB.ID: w.custB.ID}), err
		}},
		"CreateConsoleRun": {kind: byID, call: func(w *world, sc Scope, t *target) ([]string, error) {
			_, err := w.s.CreateConsoleRun(w.ctx, sc, ConsoleRunInput{DeviceID: t.dev, Kind: "check"})
			return nil, err
		}},
		"FinishConsoleRun": {kind: byID,
			prep: func(w *world, t *target) {
				t.freshRun = must[Run](w.t)(w.s.CreateConsoleRun(w.ctx, ScopeAll(), ConsoleRunInput{DeviceID: t.dev, Kind: "check"})).ID
			},
			call: func(w *world, sc Scope, t *target) ([]string, error) {
				_, err := w.s.FinishConsoleRun(w.ctx, sc, t.freshRun, ConsoleRunResult{Outcome: "success"})
				return nil, err
			}},
		"ListSnapshots": {kind: byID, call: func(w *world, sc Scope, t *target) ([]string, error) {
			_, err := w.s.ListSnapshots(w.ctx, sc, t.dev)
			return nil, err
		}},
		"EnqueueCommand": {kind: byID, call: func(w *world, sc Scope, t *target) ([]string, error) {
			_, _, err := w.s.EnqueueCommand(w.ctx, sc, t.dev, CommandInput{Type: "check_in", IdempotencyKey: uniq("cmd")})
			return nil, err
		}},
		"ListCommands": {kind: byID, call: func(w *world, sc Scope, t *target) ([]string, error) {
			_, err := w.s.ListCommands(w.ctx, sc, t.dev, Page{})
			return nil, err
		}},
		"CreateRestoreRequest": {kind: byID, call: func(w *world, sc Scope, t *target) ([]string, error) {
			_, err := w.s.CreateRestoreRequest(w.ctx, sc, RestoreRequest{DeviceID: t.dev, SnapshotID: t.snapshot, Mode: "download_zip", ExpiresAt: w.clk.Now().Add(time.Hour)})
			return nil, err
		}},
		"CreateRestoreRequestAudited": {kind: byID, call: func(w *world, sc Scope, t *target) ([]string, error) {
			_, err := w.s.CreateRestoreRequestAudited(w.ctx, sc, RestoreRequest{DeviceID: t.dev, SnapshotID: t.snapshot, Mode: "download_zip", ExpiresAt: w.clk.Now().Add(time.Hour)},
				AuditActor{Username: "matrix", Method: "api"})
			return nil, err
		}},
		"GetRestoreRequest": {kind: byID, call: func(w *world, sc Scope, t *target) ([]string, error) {
			_, err := w.s.GetRestoreRequest(w.ctx, sc, t.restore)
			return nil, err
		}},
		"UpdateRestoreRequest": {kind: byID, call: func(w *world, sc Scope, t *target) ([]string, error) {
			st := "running"
			_, err := w.s.UpdateRestoreRequest(w.ctx, sc, t.restore, RestoreUpdate{Status: &st, CountDownload: true})
			return nil, err
		}},
		"ListRestoreRequests": {kind: byID, call: func(w *world, sc Scope, t *target) ([]string, error) {
			_, err := w.s.ListRestoreRequests(w.ctx, sc, t.dev, Page{})
			return nil, err
		}},
		"ListAudit": {kind: list, call: func(w *world, sc Scope, t *target) ([]string, error) {
			l, err := w.s.ListAudit(w.ctx, sc, AuditFilter{}, Page{Limit: 1000})
			var rows []string
			for _, e := range l.Items {
				rows = append(rows, e.CustomerID)
			}
			return rows, err
		}},
		"LoadWorld": {kind: list, call: func(w *world, sc Scope, t *target) ([]string, error) {
			world, err := w.s.LoadWorld(w.ctx, sc, LoadOptions{})
			var rows []string
			for _, c := range world.Customers {
				rows = append(rows, c.ID)
			}
			for _, d := range world.Devices {
				rows = append(rows, d.CustomerID)
			}
			return rows, err
		}},
		"LoadDeviceFacts": {kind: byID, call: func(w *world, sc Scope, t *target) ([]string, error) {
			_, err := w.s.LoadDeviceFacts(w.ctx, sc, t.dev)
			return nil, err
		}},
	}
	addM2Matrix(m)       // the M2 customer, device and run methods (scope_m2_test.go)
	addEnrollM2Matrix(m) // M2-W2B: token management (enroll_m2_test.go)
	addW2CMatrix(m)      // M2-W2C: commands and identity conflicts (scope_w2c_test.go)
	addAuditM2Matrix(m)  // M2-W3A: audit viewer reads and console-wide settings (audit_m2_test.go)
	return m
}

// globalMethods are exported methods that are deliberately not scoped, with the reason.
// Adding an exported method to Store without classifying it here, in matrix() or by
// the Agent prefix fails TestScopeMatrixCoversEveryExportedMethod.
var globalMethods = map[string]string{
	"Close": "lifecycle", "Read": "transaction primitive", "Write": "transaction primitive", "Now": "clock", "Path": "file path of the database",
	"SchemaVersion": "migration marker",
	"GetSetting":    "global configuration", "ListSettings": "global configuration", "PutSetting": "global configuration",
	"CreateStorageTarget": "global configuration", "DefaultStorageTarget": "global configuration", "ListStorageTargets": "global configuration",
	"DefaultPolicy": "the default policy is global",
	"CreateAdmin":   "admin accounts", "GetAdmin": "admin accounts", "GetAdminByUsername": "admin accounts (login)", "ListAdmins": "admin accounts",
	"UpdateAdminPassword": "admin accounts", "RecordLoginFailure": "admin accounts", "ClearLoginFailures": "admin accounts",
	"CreateSession": "sessions", "GetSessionByHash": "sessions", "TouchSession": "sessions", "MarkSessionMFA": "sessions",
	"DeleteSession": "sessions", "DeleteExpiredSessions": "session-cleanup job",
	"ConsumeEnrollmentToken":       "agent-side: the hash is the credential",
	"GetEnrollmentTokenByHash":     "agent-side: the hash is the credential",
	"EnrollNonceKnown":             "agent-side: the nonce is the credential",
	"DisableAdmin":                 "admin accounts",
	"IntegrityCheck":               "diagnostics (doctor)",
	"ListExpiredRestores":          "restore-purge job",
	"ListGatewayLoginsForHtpasswd": "htpasswd-sync job", "MarkHtpasswdSynced": "htpasswd-sync job",
	"TrimHeartbeats": "heartbeat-ring-trim job", "ExpireCommands": "command-expiry job",
	"GetStorageTarget": "global configuration", "SetStorageTargetMaxConcurrentBackups": "global configuration (the fleet cap, ADR 0005)",
	"DeleteExpiredStartLeases": "heartbeat-ring-trim job (expired start leases)",
	"ListStartLeases":          "fleet cap of a global storage target (CLI and tests); ids only, no customer data",
	"AppendAudit":              "audit trail is append-only; customer_id is data, not an access path",
	"LoadDisplayPrefs":         "global configuration (M2-W3A: the display zone and refresh interval)",
	"PutOperatorSetting":       "global configuration (M2-W3A: init stores the operator zone)",
	"AcquireJobLease":          "job state", "ReleaseJobLease": "job state", "RecordJobRun": "job state", "GetJobState": "job state", "ListJobStates": "job state",
}

var scopeType = reflect.TypeOf(Scope{})
var ctxType = reflect.TypeOf((*context.Context)(nil)).Elem()

// Every exported method of *Store is either in the scope matrix (its second
// parameter is a Scope), an Agent method keyed by the authenticated device, or
// listed as global with a reason. A new method cannot be forgotten.
func TestScopeMatrixCoversEveryExportedMethod(t *testing.T) {
	cases := matrix()
	st := reflect.TypeOf(&Store{})
	seen := map[string]bool{}
	for i := 0; i < st.NumMethod(); i++ {
		m := st.Method(i)
		seen[m.Name] = true
		hasScope := m.Type.NumIn() > 2 && m.Type.In(1) == ctxType && m.Type.In(2) == scopeType
		_, inMatrix := cases[m.Name]
		_, isGlobal := globalMethods[m.Name]
		isAgent := strings.HasPrefix(m.Name, "Agent")
		switch {
		case hasScope && !inMatrix:
			t.Errorf("%s takes a Scope but is not in the scope matrix", m.Name)
		case !hasScope && inMatrix:
			t.Errorf("%s is in the scope matrix but does not take a Scope as its first argument after ctx", m.Name)
		case isAgent && (hasScope || isGlobal || inMatrix):
			t.Errorf("%s is an Agent method: it must take no Scope and appear in no list (the device id comes from the token)", m.Name)
		case !hasScope && !isAgent && !isGlobal:
			t.Errorf("%s is not scoped, not an Agent method and not in globalMethods: classify it (customer-owned data needs a Scope, §3.5 item 1)", m.Name)
		}
		if isAgent {
			// §3.5 item 4: the first argument after ctx is the authenticated principal, never a Scope or a request body id.
			for j := 2; j < m.Type.NumIn(); j++ {
				if m.Type.In(j) == scopeType {
					t.Errorf("%s: an Agent method must not take a Scope", m.Name)
				}
			}
		}
	}
	for name := range cases {
		if !seen[name] {
			t.Errorf("the matrix lists %s but *Store has no such method", name)
		}
	}
	for name := range globalMethods {
		if !seen[name] {
			t.Errorf("globalMethods lists %s but *Store has no such method", name)
		}
	}
	if len(cases) < 30 {
		t.Errorf("only %d scoped methods in the matrix; expected the full set", len(cases))
	}
}

// fingerprint is everything customer-owned that a denied mutation must not touch.
func fingerprint(w *world, t *target) string {
	ctx := w.ctx
	var b strings.Builder
	dump := func(label string, v any, err error) {
		j, _ := json.Marshal(v)
		fmt.Fprintf(&b, "%s=%s err=%v\n", label, j, err)
	}
	c, err := w.s.GetCustomer(ctx, ScopeAll(), t.cust)
	dump("customer", c, err)
	d, err := w.s.GetDevice(ctx, ScopeAll(), t.dev)
	dump("device", d, err)
	p, err := w.s.GetPolicy(ctx, ScopeAll(), t.policy)
	dump("policy", p, err)
	revs, err := w.s.ListPolicyRevisions(ctx, ScopeAll(), t.policy)
	dump("revisions", len(revs), err)
	toks, err := w.s.ListEnrollmentTokens(ctx, ScopeAll(), t.cust)
	dump("tokens", toks, err)
	rr, err := w.s.GetRestoreRequest(ctx, ScopeAll(), t.restore)
	dump("restore", rr, err)
	rl, err := w.s.ListRestoreRequests(ctx, ScopeAll(), t.dev, Page{Limit: 1000})
	dump("restores", len(rl.Items), err)
	runs, err := w.s.ListRuns(ctx, ScopeAll(), t.dev, Page{Limit: 1000})
	dump("runs", runs.Items, err)
	cmds, err := w.s.ListCommands(ctx, ScopeAll(), t.dev, Page{Limit: 1000})
	dump("commands", len(cmds.Items), err)
	return b.String()
}

// The generated scope matrix of architecture §3.5 item 5: every scoped method
// runs as (a) an all-customer admin, (b) an operator scoped to customer A and
// (c) a read-only user scoped to customer B, plus the zero Scope, against seeded
// rows of both customers. No row of another customer is visible or changeable;
// an out-of-scope by-id call is ErrNotFound; the zero Scope is ErrScope.
func TestScopeMatrix(t *testing.T) {
	w := seed(t)
	tA := seedTarget(w, "A", w.custA, w.devA)
	tB := seedTarget(w, "B", w.custB, w.devB)
	targets := []*target{tA, tB}
	customerOf := map[string]string{tA.cust: "A", tB.cust: "B"}
	principals := []struct {
		name  string
		scope Scope
	}{
		{"all-customer admin", ScopeAll()},
		{"operator scoped to A", ScopeOf(w.custA.ID)},
		{"read-only scoped to B", ScopeOf(w.custB.ID)},
	}

	names := make([]string, 0)
	cases := matrix()
	for n := range cases {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, name := range names {
		c := cases[name]
		t.Run(name, func(t *testing.T) {
			wt := *w
			wt.t = t
			// Zero scope: always ErrScope, never data.
			if c.prep != nil {
				c.prep(&wt, tA)
			}
			rows, err := c.call(&wt, Scope{}, tA)
			if !errors.Is(err, ErrScope) || len(rows) != 0 {
				t.Errorf("zero Scope: err=%v rows=%v, want ErrScope and nothing", err, rows)
			}
			for _, p := range principals {
				switch c.kind {
				case list:
					rows, err := c.call(&wt, p.scope, tA)
					if err != nil {
						t.Errorf("%s: %v", p.name, err)
						continue
					}
					seenOwn := map[string]bool{}
					for _, cust := range rows {
						switch {
						case cust == "" && c.globalRowsOK:
						case p.scope.Allows(cust) && cust != "":
							seenOwn[customerOf[cust]] = true
						default:
							t.Errorf("%s sees a row of customer %q outside its scope", p.name, cust)
						}
					}
					for _, tg := range targets {
						if p.scope.Allows(tg.cust) && !seenOwn[tg.name] {
							t.Errorf("%s does not see customer %s's rows (the list is empty for an allowed customer)", p.name, tg.name)
						}
					}
				case byID, globalWrite:
					for _, tg := range targets {
						if c.prep != nil {
							c.prep(&wt, tg)
						}
						before := fingerprint(&wt, tg)
						_, err := c.call(&wt, p.scope, tg)
						allowed := p.scope.Allows(tg.cust)
						switch {
						case c.kind == globalWrite && p.scope.All:
							if err != nil {
								t.Errorf("%s: %v", p.name, err)
							}
						case c.kind == globalWrite:
							if !errors.Is(err, ErrScope) {
								t.Errorf("%s: global write = %v, want ErrScope", p.name, err)
							}
						case allowed:
							if err != nil {
								t.Errorf("%s on its own customer %s: %v", p.name, tg.name, err)
							}
						default:
							if !errors.Is(err, ErrNotFound) {
								t.Errorf("%s on customer %s's row: err = %v, want ErrNotFound (404, not 403)", p.name, tg.name, err)
							}
							if after := fingerprint(&wt, tg); after != before {
								t.Errorf("%s changed customer %s's data through a denied call:\nbefore:\n%s\nafter:\n%s", p.name, tg.name, before, after)
							}
						}
					}
				}
			}
		})
	}
}

// §3.5 item 4: an Agent method is keyed by the authenticated device and can touch no other device's rows.
func TestAgentMethodsAreConfinedToTheirDevice(t *testing.T) {
	w := seed(t)
	ctx := context.Background()
	tA := seedTarget(w, "A", w.custA, w.devA)
	tB := seedTarget(w, "B", w.custB, w.devB)

	// Commands: each device only ever receives its own.
	got := must[[]Command](t)(w.s.AgentNextCommands(ctx, w.devA.ID, 50))
	for _, c := range got {
		if c.DeviceID != w.devA.ID {
			t.Fatalf("device A was handed device %s's command", c.DeviceID)
		}
	}
	if len(got) == 0 {
		t.Fatal("device A got no commands")
	}
	if _, err := w.s.AgentAckCommand(ctx, w.devA.ID, tB.command, CommandAck{Status: "acked"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("A acking B's command = %v", err)
	}
	// Restores.
	if _, err := w.s.AgentGetRestoreRequest(ctx, w.devA.ID, tB.restore); !errors.Is(err, ErrNotFound) {
		t.Fatalf("A reading B's restore = %v", err)
	}
	// Runs: A cannot claim, overwrite or even probe B's run id.
	res := ingest(t, w, w.devA.ID, runInput(tB.run, t0))
	wantStatus(t, res[0], IngestRejected, ReasonWrongDevice)
	if r := must[Run](t)(w.s.GetRun(ctx, ScopeAll(), tB.run)); r.DeviceID != w.devB.ID {
		t.Fatal("A took over B's run")
	}
	// Snapshots: replacing A's cache leaves B's alone.
	must2(t)(w.s.AgentReplaceSnapshots(ctx, w.devA.ID, nil))
	if l := must[[]Snapshot](t)(w.s.ListSnapshots(ctx, ScopeAll(), w.devB.ID)); len(l) != 1 {
		t.Fatalf("B has %d snapshots after A replaced its own", len(l))
	}
	if l := must[[]Snapshot](t)(w.s.ListSnapshots(ctx, ScopeAll(), w.devA.ID)); len(l) != 0 {
		t.Fatalf("A still has %d snapshots", len(l))
	}
	_ = tA
	// Escrow and gateway secret delivery: B's unconfirmed device only ever yields B's own.
	w2 := w.enroll("tok-b", "B-DESK-2")
	e := must[Escrow](t)(w.s.AgentEscrowForDelivery(ctx, w2.Device.ID))
	if e.DeviceID != w2.Device.ID {
		t.Fatalf("escrow for delivery belongs to %s", e.DeviceID)
	}
	sec := must[[]byte](t)(w.s.AgentPendingGatewaySecret(ctx, w2.Device.ID))
	if string(sec) != "wrapped-gateway-B-DESK-2" {
		t.Fatalf("pending secret = %q", sec)
	}
	// Presence for A does not change B's.
	before := must[Presence](t)(w.s.GetPresence(ctx, ScopeAll(), w.devB.ID))
	w.clk.Advance(time.Minute)
	w.heartbeat(w.devA.ID)
	if after := must[Presence](t)(w.s.GetPresence(ctx, ScopeAll(), w.devB.ID)); !after.ReceivedAt.Equal(before.ReceivedAt) {
		t.Fatal("A's heartbeat moved B's presence")
	}
}
