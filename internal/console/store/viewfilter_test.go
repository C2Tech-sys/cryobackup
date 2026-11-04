package store

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// M3-T3 delta H1/L4: a saved view's filter is a typed clause tree (components.md
// 5.4), validated on write and read fail-closed.

func sharedView(t *testing.T, w *world, name, entity, filter string) SavedView {
	t.Helper()
	v, err := w.s.CreateSavedViewAudited(w.ctx, ScopeAll(), SavedViewInput{Name: name, Entity: entity, Shared: true, Filter: json.RawMessage(filter)}, actorOf(w))
	if err != nil {
		t.Fatalf("create %q: %v", name, err)
	}
	return v
}

func visibleTo(t *testing.T, w *world, sc Scope, id string) bool {
	t.Helper()
	vs, err := w.s.ListSavedViews(w.ctx, sc, matrixViewer, "")
	ok(t, err)
	for _, v := range vs {
		if v.ID == id {
			if _, gerr := w.s.GetSavedView(w.ctx, sc, matrixViewer, id); gerr != nil {
				t.Fatalf("listed but not gettable: %v", gerr)
			}
			return true
		}
	}
	if _, gerr := w.s.GetSavedView(w.ctx, sc, matrixViewer, id); !errors.Is(gerr, ErrNotFound) {
		t.Fatalf("not listed, but get = %v, want ErrNotFound", gerr)
	}
	return false
}

func TestSharedViewVisibilityResolvesEveryReferenceInsideTheCallersScope(t *testing.T) { // H1
	w := seed(t)
	a, b := ScopeOf(w.custA.ID), ScopeOf(w.custB.ID)
	clause := func(field, op, value string) string {
		return `{"field":"` + field + `","op":"` + op + `","value":` + value + `}`
	}
	cases := []struct {
		name, entity, filter string
		wantA, wantB         bool
	}{
		{"customer clause for A", "devices", clause("customer", "in", `["`+w.custA.ID+`"]`), true, false},
		{"customer clause for B (the reported bypass)", "devices", clause("customer", "in", `["`+w.custB.ID+`"]`), false, true},
		{"hostname of A", "devices", clause("hostname", "eq", `"A-DESK-1"`), true, false},
		{"hostname of B", "devices", clause("hostname", "eq", `"B-DESK-1"`), false, true},
		{"hostname prefix only A matches", "devices", clause("hostname", "starts_with", `"A-"`), true, false},
		{"hostname fragment both customers match", "devices", clause("hostname", "contains", `"DESK"`), false, false},
		{"hostname that matches nothing", "devices", clause("hostname", "eq", `"NOBODY-9"`), false, false},
		{"unknown customer id", "devices", clause("customer", "in", `["01J9Y4K3A0B1C2D3E4F5G6H7ZZ"]`), false, false},
		{"B hidden inside a not inside an and", "devices", `{"op":"and","clauses":[` + clause("state", "in", `["STALE"]`) + `,{"op":"not","clauses":[` + clause("customer", "in", `["`+w.custB.ID+`"]`) + `]}]}`, false, true},
		{"A and B in one list", "devices", clause("customer", "in", `["`+w.custA.ID+`","`+w.custB.ID+`"]`), false, false},
		{"B's device id in a runs view", "runs", clause("device", "in", `["`+w.devB.ID+`"]`), false, true},
		{"A's device id in a runs view", "runs", clause("device", "in", `["`+w.devA.ID+`"]`), true, false},
		{"customer name of B", "customers", clause("name", "eq", `"Bravo Legal"`), false, true},
		{"state only", "devices", clause("state", "in", `["STALE","FAILED"]`), true, true},
		{"no filter at all", "devices", `{}`, true, true},
	}
	ids := make([]string, len(cases))
	for i, c := range cases {
		ids[i] = sharedView(t, w, c.name, c.entity, c.filter).ID
	}
	for i, c := range cases {
		if got := visibleTo(t, w, a, ids[i]); got != c.wantA {
			t.Errorf("%s: scoped to A sees it = %v, want %v", c.name, got, c.wantA)
		}
		if got := visibleTo(t, w, b, ids[i]); got != c.wantB {
			t.Errorf("%s: scoped to B sees it = %v, want %v", c.name, got, c.wantB)
		}
		if !visibleTo(t, w, ScopeAll(), ids[i]) {
			t.Errorf("%s: an all-customer caller must see every shared view", c.name)
		}
		if got := visibleTo(t, w, ScopeOf(), ids[i]); got && c.name != "state only" && c.name != "no filter at all" {
			t.Errorf("%s: an empty scope sees it", c.name)
		}
	}
}

func TestAViewFilterThatCannotBeParsedIsHiddenFromAScopedCaller(t *testing.T) { // H1(c): fail closed
	w := seed(t)
	v := sharedView(t, w, "legacy", "devices", `{}`)
	for name, raw := range map[string]string{
		"the old key shape":   `{"customer":"` + w.custB.ID + `"}`,
		"a nested old shape":  `{"all":[{"customer_id":"` + w.custB.ID + `"}]}`,
		"unknown field":       `{"field":"favourite_colour","op":"eq","value":"red"}`,
		"an op the field has": `{"field":"state","op":"contains","value":"STALE"}`,
		"a plain array":       `[1,2]`,
	} {
		raw := raw
		ok(t, w.s.Write(w.ctx, func(tx *Tx) error {
			_, err := tx.Exec(`UPDATE saved_views SET filter = ? WHERE id = ?`, raw, v.ID)
			return err
		}))
		if visibleTo(t, w, ScopeOf(w.custA.ID), v.ID) {
			t.Errorf("%s: a filter that does not parse was shown to a scoped caller", name)
		}
		if !visibleTo(t, w, ScopeAll(), v.ID) {
			t.Errorf("%s: an all-customer caller still sees it", name)
		}
	}
}

func TestViewFilterIsValidatedOnCreateAndUpdate(t *testing.T) { // H1(b)
	w := seed(t)
	deep := `{"field":"state","op":"in","value":["OK"]}`
	for i := 0; i < 6; i++ {
		deep = `{"op":"and","clauses":[` + deep + `]}`
	}
	many := make([]string, 41)
	for i := range many {
		many[i] = `{"field":"state","op":"in","value":["OK"]}`
	}
	bad := map[string]string{
		"the old key shape":       `{"customer_id":"x"}`,
		"a clause with no value":  `{"field":"customer","op":"in"}`,
		"an unknown field":        `{"field":"bogus","op":"eq","value":"x"}`,
		"an op the field lacks":   `{"field":"state","op":"contains","value":["OK"]}`,
		"an empty group":          `{"op":"and","clauses":[]}`,
		"an unknown group op":     `{"op":"xor","clauses":[{"field":"state","op":"in","value":["OK"]}]}`,
		"a string for a list":     `{"field":"customer","op":"in","value":"abc"}`,
		"an unknown state name":   `{"field":"state","op":"in","value":["EXPLODED"]}`,
		"an unknown key":          `{"field":"state","op":"in","value":["OK"],"extra":1}`,
		"too deep":                deep,
		"too many clauses":        `{"op":"and","clauses":[` + strings.Join(many, ",") + `]}`,
		"a path":                  `{"field":"hostname","op":"eq","value":"C:\\Users\\x"}`,
		"a control character":     `{"field":"hostname","op":"eq","value":"a\nb"}`,
		"a runs field on devices": `{"field":"outcome","op":"in","value":["failed"]}`,
	}
	before := auditCount(t, w, "view.create")
	for name, f := range bad {
		_, err := w.s.CreateSavedViewAudited(w.ctx, ScopeAll(), SavedViewInput{Name: "v " + name, Entity: "devices", Filter: json.RawMessage(f)}, actorOf(w))
		if !errors.Is(err, ErrInvalid) || len(FieldErrors(err)) == 0 || FieldErrors(err)[0].Field != "filter" {
			t.Errorf("create with %s: err = %v, want a filter field error", name, err)
		}
	}
	if n := auditCount(t, w, "view.create"); n != before {
		t.Errorf("rejected creates wrote %d audit rows", n-before)
	}
	if vs := must[[]SavedView](t)(w.s.ListSavedViews(w.ctx, ScopeAll(), w.admin.ID, "")); len(vs) != 0 {
		t.Errorf("%d views were stored although every filter was rejected", len(vs))
	}
	// The same filter is valid on another entity: validation follows the entity.
	if _, err := w.s.CreateSavedViewAudited(w.ctx, ScopeAll(), SavedViewInput{Name: "runs ok", Entity: "runs", Filter: json.RawMessage(`{"field":"outcome","op":"in","value":["failed"]}`)}, actorOf(w)); err != nil {
		t.Errorf("an outcome clause on runs: %v", err)
	}

	// Update: the patch is validated against the stored entity; nothing is written or audited.
	v := sharedView(t, w, "good", "devices", `{"field":"state","op":"in","value":["OK"]}`)
	updates := auditCount(t, w, "view.update")
	for name, f := range bad {
		raw := json.RawMessage(f)
		_, _, err := w.s.UpdateSavedViewAudited(w.ctx, ScopeAll(), v.ID, SavedViewPatch{Filter: &raw}, actorOf(w), false)
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("update with %s: err = %v, want ErrInvalid", name, err)
		}
	}
	if n := auditCount(t, w, "view.update"); n != updates {
		t.Errorf("rejected updates wrote %d audit rows", n-updates)
	}
	got := must[SavedView](t)(w.s.GetSavedView(w.ctx, ScopeAll(), w.admin.ID, v.ID))
	if !sameJSON(got.Filter, []byte(`{"field":"state","op":"in","value":["OK"]}`)) {
		t.Errorf("a rejected update changed the stored filter: %s", got.Filter)
	}
}

func TestViewNamesAreCheckedForPathsLikePauseReasons(t *testing.T) { // L4
	w := seed(t)
	for _, name := range []string{`C:\Users\someone\Documents`, `\\fileserver\share`, `backup of C:/Users/x`} {
		_, err := w.s.CreateSavedViewAudited(w.ctx, ScopeAll(), SavedViewInput{Name: name, Entity: "devices"}, actorOf(w))
		if !errors.Is(err, ErrInvalid) || len(FieldErrors(err)) == 0 || FieldErrors(err)[0].Field != "name" {
			t.Errorf("create %q: err = %v, want a name field error", name, err)
		}
	}
	v := must[SavedView](t)(w.s.CreateSavedViewAudited(w.ctx, ScopeAll(), SavedViewInput{Name: "fine", Entity: "devices"}, actorOf(w)))
	bad := `C:\Users\x`
	if _, _, err := w.s.UpdateSavedViewAudited(w.ctx, ScopeAll(), v.ID, SavedViewPatch{Name: &bad}, actorOf(w), false); !errors.Is(err, ErrInvalid) {
		t.Errorf("rename to a path: %v", err)
	}
	if n := auditCount(t, w, "view.create"); n != 1 {
		t.Errorf("%d view.create rows; only the valid view may have one", n)
	}
}
