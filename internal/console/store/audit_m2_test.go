package store

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// addAuditM2Matrix adds the M2-W3A methods to the §3.5 scope matrix (matrix() in
// scope_test.go calls it): the audit viewer's reads and the console-wide
// settings. A read of audit rows must show a scoped caller only its customers'
// events; the settings methods are console-wide, so they need scope.All.
func addAuditM2Matrix(m map[string]matrixCase) {
	actor := AuditActor{AdminID: "01J9Y4K3A0B1C2D3E4F5G6H7AA", Username: "matrix", Method: "api"}
	rowsOf := func(t AuditTable, err error) ([]string, error) {
		var out []string
		for _, e := range t.Rows {
			out = append(out, e.CustomerID)
		}
		return out, err
	}
	m["AuditTable"] = matrixCase{kind: list, call: func(w *world, sc Scope, t *target) ([]string, error) {
		return rowsOf(w.s.AuditTable(w.ctx, sc, AuditQuery{Size: 200}))
	}}
	m["AuditTableAt"] = matrixCase{kind: list, call: func(w *world, sc Scope, t *target) ([]string, error) {
		return rowsOf(w.s.AuditTableAt(w.ctx, sc, AuditQuery{}, 0, 500))
	}}
	m["GetAuditEvent"] = matrixCase{kind: byID, call: func(w *world, sc Scope, t *target) ([]string, error) {
		tb, err := w.s.AuditTable(w.ctx, ScopeAll(), AuditQuery{CustomerID: t.cust})
		if err != nil || len(tb.Rows) == 0 {
			return nil, errors.New("the seeded audit event is missing")
		}
		_, err = w.s.GetAuditEvent(w.ctx, sc, tb.Rows[0].ID)
		return nil, err
	}}
	m["AuditFacets"] = matrixCase{kind: list, call: func(w *world, sc Scope, t *target) ([]string, error) {
		// Each customer owns an action named after it; a scoped caller must not be offered the other's.
		for _, c := range []Customer{w.custA, w.custB} {
			if _, err := w.s.AppendAudit(w.ctx, AuditEvent{ActorType: "system", Action: "facet." + strings.ToLower(c.ID), CustomerID: c.ID}); err != nil {
				return nil, err
			}
		}
		f, err := w.s.AuditFacets(w.ctx, sc)
		var out []string
		for _, a := range f.Actions {
			if id, ok := strings.CutPrefix(a, "facet."); ok {
				for _, c := range []Customer{w.custA, w.custB} {
					if strings.EqualFold(id, c.ID) {
						out = append(out, c.ID)
					}
				}
			}
		}
		return out, err
	}}
	m["ListSettingValues"] = matrixCase{kind: globalWrite, call: func(w *world, sc Scope, t *target) ([]string, error) {
		_, err := w.s.ListSettingValues(w.ctx, sc)
		return nil, err
	}}
	m["SetSettingsAudited"] = matrixCase{kind: globalWrite, call: func(w *world, sc Scope, t *target) ([]string, error) {
		seq++
		_, err := w.s.SetSettingsAudited(w.ctx, sc, []SettingInput{{Key: SettingRefreshInterval, Value: json.RawMessage(itoaT(10 + seq))}}, AuditActor{AdminID: w.admin.ID, Username: actor.Username, Method: actor.Method})
		return nil, err
	}}
}

func itoaT(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func appendEv(t *testing.T, w *world, ev AuditEvent) AuditEvent {
	t.Helper()
	return must[AuditEvent](t)(w.s.AppendAudit(w.ctx, ev))
}

func TestAuditTableFiltersPagesAndTotals(t *testing.T) {
	w := seed(t)
	base := t0
	for i := 0; i < 7; i++ {
		appendEv(t, w, AuditEvent{OccurredAt: base.Add(time.Duration(i) * time.Hour), ActorType: "admin", ActorID: "", ActorLabel: "Alice",
			Action: "policy.update", ObjectType: "policy", CustomerID: w.custA.ID})
	}
	appendEv(t, w, AuditEvent{OccurredAt: base, ActorType: "admin", ActorLabel: "Bob", Action: "customer.update", ObjectType: "customer", CustomerID: w.custB.ID, Result: "denied"})
	appendEv(t, w, AuditEvent{OccurredAt: base, ActorType: "system", Action: "auth_login", Result: "error"}) // customer-less, with an underscore

	all, err := w.s.AuditTable(w.ctx, ScopeAll(), AuditQuery{})
	ok(t, err)
	if all.Total < 9 || all.Page.Total != all.Total {
		t.Fatalf("unfiltered total = %d / %d, want every event", all.Page.Total, all.Total)
	}

	tb, err := w.s.AuditTable(w.ctx, ScopeAll(), AuditQuery{Actor: "alice", Action: "policy.update"})
	ok(t, err)
	if tb.Page.Total != 7 || len(tb.Rows) != 7 || tb.Total != all.Total {
		t.Fatalf("actor+action filter: total %d rows %d of %d", tb.Page.Total, len(tb.Rows), tb.Total)
	}
	for i := 1; i < len(tb.Rows); i++ {
		if tb.Rows[i].OccurredAt.After(tb.Rows[i-1].OccurredAt) {
			t.Fatal("rows are not newest first")
		}
	}

	// A family filter; the underscore of another action must not act as a wildcard.
	fam, err := w.s.AuditTable(w.ctx, ScopeAll(), AuditQuery{Action: "policy.*"})
	ok(t, err)
	if fam.Page.Total != 7 {
		t.Fatalf("policy.* matched %d, want 7", fam.Page.Total)
	}
	if got := must[AuditTable](t)(w.s.AuditTable(w.ctx, ScopeAll(), AuditQuery{Action: "auth_login"})).Page.Total; got != 1 {
		t.Fatalf("auth_login matched %d, want 1", got)
	}

	// Result, customer and object type.
	if got := must[AuditTable](t)(w.s.AuditTable(w.ctx, ScopeAll(), AuditQuery{Result: "denied"})).Page.Total; got != 1 {
		t.Fatalf("denied matched %d, want 1", got)
	}
	if got := must[AuditTable](t)(w.s.AuditTable(w.ctx, ScopeAll(), AuditQuery{CustomerID: w.custB.ID, ObjectType: "customer"})).Page.Total; got != 1 {
		t.Fatalf("customer+object matched %d, want 1", got)
	}

	// Date range: since inclusive, until exclusive.
	rng, err := w.s.AuditTable(w.ctx, ScopeAll(), AuditQuery{Action: "policy.update", Since: base.Add(2 * time.Hour), Until: base.Add(5 * time.Hour)})
	ok(t, err)
	if rng.Page.Total != 3 {
		t.Fatalf("range matched %d, want the 3 events at +2h, +3h, +4h", rng.Page.Total)
	}

	// Paging: 7 rows, 25 per page is one page; a page past the end clamps and says so.
	pg, err := w.s.AuditTable(w.ctx, ScopeAll(), AuditQuery{Action: "policy.update", Size: 25, Page: 9})
	ok(t, err)
	if !pg.Page.Clamped || pg.Page.Page != 1 || pg.Page.From != 1 || pg.Page.To != 7 {
		t.Fatalf("page past the end: %+v", pg.Page)
	}
	// A filter that matches nothing still reports the denominator.
	none, err := w.s.AuditTable(w.ctx, ScopeAll(), AuditQuery{Actor: "nobody"})
	ok(t, err)
	if len(none.Rows) != 0 || none.Total != all.Total || none.Page.Total != 0 {
		t.Fatalf("empty result: %d rows, total %d, matching %d", len(none.Rows), none.Total, none.Page.Total)
	}
	// The offset form agrees.
	at, err := w.s.AuditTableAt(w.ctx, ScopeAll(), AuditQuery{Action: "policy.update"}, 5, 5)
	ok(t, err)
	if len(at.Rows) != 2 || at.Page.From != 6 || at.Page.To != 7 || at.Page.Total != 7 {
		t.Fatalf("offset window: rows %d page %+v", len(at.Rows), at.Page)
	}
}

func TestAuditTableRejectsBadFilters(t *testing.T) {
	w := seed(t)
	for name, q := range map[string]AuditQuery{
		"action":      {Action: "policy update; DROP TABLE"},
		"object_type": {ObjectType: "Device Thing"},
		"result":      {Result: "maybe"},
		"to":          {Since: t0, Until: t0.Add(-time.Hour)},
	} {
		_, err := w.s.AuditTable(w.ctx, ScopeAll(), q)
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
			continue
		}
		fe := FieldErrors(err)
		if len(fe) != 1 || fe[0].Field != name {
			t.Errorf("%s: field errors = %+v", name, fe)
		}
	}
}

func TestAuditViewerIsScoped(t *testing.T) {
	w := seed(t)
	a := appendEv(t, w, AuditEvent{ActorType: "admin", ActorLabel: "Alice", Action: "customer.update", CustomerID: w.custA.ID})
	b := appendEv(t, w, AuditEvent{ActorType: "admin", ActorLabel: "Alice", Action: "customer.update", CustomerID: w.custB.ID})
	g := appendEv(t, w, AuditEvent{ActorType: "admin", ActorLabel: "Alice", Action: "settings.update"}) // console-wide

	scopeA := ScopeOf(w.custA.ID)
	tb, err := w.s.AuditTable(w.ctx, scopeA, AuditQuery{Size: 200})
	ok(t, err)
	for _, e := range tb.Rows {
		if e.CustomerID != w.custA.ID {
			t.Fatalf("scope A sees %q (customer %q)", e.Action, e.CustomerID)
		}
	}
	// The denominator is the caller's own too: it does not reveal the size of the other customer's trail.
	allA := must[AuditTable](t)(w.s.AuditTable(w.ctx, scopeA, AuditQuery{Size: 200}))
	if allA.Total != tb.Page.Total {
		t.Fatalf("scope A total %d != its rows %d", allA.Total, tb.Page.Total)
	}
	// Filtering by the other customer's id returns nothing, not an error.
	other := must[AuditTable](t)(w.s.AuditTable(w.ctx, scopeA, AuditQuery{CustomerID: w.custB.ID}))
	if other.Page.Total != 0 {
		t.Fatalf("scope A filtering for B saw %d rows", other.Page.Total)
	}
	if _, err := w.s.GetAuditEvent(w.ctx, scopeA, a.ID); err != nil {
		t.Fatalf("own event: %v", err)
	}
	for _, id := range []string{b.ID, g.ID, "nope"} {
		if _, err := w.s.GetAuditEvent(w.ctx, scopeA, id); !errors.Is(err, ErrNotFound) {
			t.Errorf("event %s outside scope: err = %v, want ErrNotFound", id, err)
		}
	}
	if _, err := w.s.GetAuditEvent(w.ctx, ScopeAll(), g.ID); err != nil {
		t.Fatalf("a console-wide event is visible to scope.All: %v", err)
	}
	if _, err := w.s.AuditTable(w.ctx, Scope{}, AuditQuery{}); !errors.Is(err, ErrScope) {
		t.Fatalf("zero scope: %v", err)
	}
}

func TestAuditDetailsNeverShowWhatTheStoreRefuses(t *testing.T) {
	raw := json.RawMessage(`{"fields":["a","b"],"note":"ok","where":"C:\\Users\\pat\\file.txt","unc":"\\\\srv\\share","password":"hunter2","api_key":"k","long":"` + strings.Repeat("x", 500) + `"}`)
	got := map[string]AuditDetail{}
	for _, d := range AuditDetails(raw) {
		got[d.Key] = d
	}
	if got["note"].Value != "ok" || got["note"].Withheld != "" {
		t.Errorf("plain value: %+v", got["note"])
	}
	if !strings.Contains(got["fields"].Value, `"a"`) {
		t.Errorf("nested value is shown as JSON: %+v", got["fields"])
	}
	for _, k := range []string{"where", "unc", "password", "api_key"} {
		if got[k].Value != "" || got[k].Withheld == "" {
			t.Errorf("%s must be withheld: %+v", k, got[k])
		}
	}
	if r := []rune(got["long"].Value); len(r) != 401 {
		t.Errorf("long value is cut to 400 runes plus an ellipsis, got %d", len(r))
	}
	if d := AuditDetails(json.RawMessage(`[1,2]`)); len(d) != 1 || d[0].Withheld == "" {
		t.Errorf("non-object details are withheld: %+v", d)
	}
	if AuditDetails(nil) != nil {
		t.Error("no details, no lines")
	}
}

func TestAuditFacetsListWhatTheTableHolds(t *testing.T) {
	w := seed(t)
	appendEv(t, w, AuditEvent{ActorType: "admin", ActorLabel: "Zed", Action: "zzz.only_b", ObjectType: "zobj", CustomerID: w.custB.ID})
	appendEv(t, w, AuditEvent{ActorType: "admin", ActorLabel: "Amy", Action: "aaa.only_a", ObjectType: "aobj", CustomerID: w.custA.ID})
	all := must[AuditFacets](t)(w.s.AuditFacets(w.ctx, ScopeAll()))
	has := func(list []string, v string) bool {
		for _, x := range list {
			if x == v {
				return true
			}
		}
		return false
	}
	if !has(all.Actions, "zzz.only_b") || !has(all.Actions, "aaa.only_a") || !has(all.ObjectTypes, "zobj") || !has(all.Actors, "Zed") {
		t.Fatalf("scope.All facets miss values: %+v", all)
	}
	a := must[AuditFacets](t)(w.s.AuditFacets(w.ctx, ScopeOf(w.custA.ID)))
	if has(a.Actions, "zzz.only_b") || has(a.ObjectTypes, "zobj") || has(a.Actors, "Zed") {
		t.Fatalf("scope A is offered B's values: %+v", a)
	}
	if !has(a.Actions, "aaa.only_a") {
		t.Fatalf("scope A misses its own action: %+v", a)
	}
}

func TestAuditDetailsWalkNestedObjectsAndCatchBareSecretWords(t *testing.T) {
	raw := json.RawMessage(`{"meta":{"password":"x","note":"fine","deeper":{"Token":"t","path":"C:\\Users\\x"}},"items":[{"totp":"123456"},{"ok":"yes"}],` +
		`"code":"9","KEY":"k","Secret":"s","passphrase":"p","exit_code":3,"tags":["a","b"]}`)
	got := map[string]AuditDetail{}
	for _, d := range AuditDetails(raw) {
		got[d.Key] = d
	}
	for _, k := range []string{"meta.password", "meta.deeper.Token", "meta.deeper.path", "items[0].totp", "code", "KEY", "Secret", "passphrase"} {
		if d, ok := got[k]; !ok || d.Value != "" || d.Withheld == "" {
			t.Errorf("%s must be withheld: %+v (present %v)", k, d, ok)
		}
	}
	if got["exit_code"].Value != "3" || got["exit_code"].Withheld != "" { // M2-W3E: a plain fact on the allow-list
		t.Errorf("exit_code is shown: %+v", got["exit_code"])
	}
	if got["meta.note"].Value != "fine" || got["items[1].ok"].Value != "yes" || got["tags"].Value != `["a","b"]` {
		t.Errorf("plain nested values are shown: %+v %+v %+v", got["meta.note"], got["items[1].ok"], got["tags"])
	}
	for _, d := range got {
		if strings.Contains(d.Value, "password") || strings.Contains(d.Value, "123456") || strings.Contains(d.Value, `"x"`) {
			t.Errorf("a nested secret leaked into a value: %+v", d)
		}
	}
	// The API form: same rule at any depth, JSON types kept elsewhere.
	safe := string(SafeAuditDetails(json.RawMessage(`{"meta":{"password":"x","n":5},"list":[{"token":"t"}],"fine":true,"p":"\\\\srv\\share"}`)))
	for _, leak := range []string{`"x"`, `"t"`, "srv"} {
		if strings.Contains(safe, leak) {
			t.Errorf("SafeAuditDetails leaks %s: %s", leak, safe)
		}
	}
	if !strings.Contains(safe, `"n":5`) || !strings.Contains(safe, `"fine":true`) || strings.Count(safe, "[withheld]") != 3 {
		t.Errorf("SafeAuditDetails: %s", safe)
	}
}
