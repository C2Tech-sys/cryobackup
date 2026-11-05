package store

import (
	"encoding/json"
	"errors"
	"testing"
)

// M3-T3 fix round 3 (M2): a shared view's audience, not its name or filter,
// decides who sees it.

// The reported leak: a view named after another customer, or whose text clauses
// carry that customer's name, was visible to a scoped account whenever its
// filter named no customer id. With the audience all customers (what every view
// shared before migration 0005 has) it is hidden from Alpha whatever the name
// and the text say; the same text with the audience Bravo is hidden too.
func TestSharedViewNamedAfterAnotherCustomerIsHiddenFromAScopedAccount(t *testing.T) { // r3 M2
	w := seed(t)
	alpha := ScopeOf(w.custA.ID)
	for _, f := range []string{
		`{}`,
		`{"field":"state","op":"in","value":["STALE"]}`,
		`{"field":"tag","op":"contains","value":"Bravo Legal"}`,
		`{"field":"os","op":"contains","value":"Bravo Legal"}`,
	} {
		for _, aud := range []ViewAudience{{All: true}, {Customers: []string{w.custB.ID}}} {
			v := sharedView(t, w, uniq("Bravo Legal exam rooms"), "devices", f, aud)
			if visibleTo(t, w, alpha, v.ID) {
				t.Errorf("filter %s, audience %+v: the view %q is visible to an account scoped to Alpha Dental", f, aud, v.Name)
			}
			if !visibleTo(t, w, ScopeAll(), v.ID) {
				t.Errorf("filter %s, audience %+v: an all-customer account does not see it", f, aud)
			}
		}
	}
}

// A view whose audience is Alpha is visible to Alpha and not to Bravo, and its
// name (which mentions Bravo) changes nothing: names are never read for scope.
// A scoped account may share with its own customers.
func TestSharedViewWithAudienceAlphaIsVisibleToAlphaOnly(t *testing.T) { // r3 M2
	w := seed(t)
	alpha, bravo := ScopeOf(w.custA.ID), ScopeOf(w.custB.ID)
	v := sharedView(t, w, "Bravo Legal comparison", "devices", `{"field":"tag","op":"contains","value":"Bravo Legal"}`, ViewAudience{Customers: []string{w.custA.ID}})
	if !visibleTo(t, w, alpha, v.ID) || visibleTo(t, w, bravo, v.ID) || !visibleTo(t, w, ScopeAll(), v.ID) {
		t.Fatalf("audience Alpha: Alpha %v, Bravo %v, all %v; want true, false, true",
			visibleTo(t, w, alpha, v.ID), visibleTo(t, w, bravo, v.ID), visibleTo(t, w, ScopeAll(), v.ID))
	}
	if v.Audience == nil || v.Audience.All || len(v.Audience.Customers) != 1 || v.Audience.Customers[0] != w.custA.ID {
		t.Fatalf("stored audience = %+v", v.Audience)
	}
	// A sharer scoped to Alpha shares with Alpha; Bravo's account does not see it.
	scoped := alphaOperator(t, w)
	sv, err := w.s.CreateSavedViewAudited(w.ctx, alpha, SavedViewInput{Name: "Alpha shared", Entity: "devices", Shared: true,
		Audience: &ViewAudience{Customers: []string{w.custA.ID, w.custA.ID}}}, scoped)
	ok(t, err)
	if len(sv.Audience.Customers) != 1 {
		t.Errorf("a repeated id is kept once: %+v", sv.Audience)
	}
	if visibleTo(t, w, bravo, sv.ID) || !visibleTo(t, w, alpha, sv.ID) {
		t.Error("a view shared by an Alpha account with Alpha is seen by the wrong accounts")
	}
}

// Every refused audience is a 400 (ErrInvalid with a field error), on create and
// on update, and nothing is written or audited.
func TestSharedViewAudienceIsRefusedOutsideTheSharersScope(t *testing.T) { // r3 M2
	w := seed(t)
	alpha := ScopeOf(w.custA.ID)
	scoped := alphaOperator(t, w)
	ids := func(v ...string) *ViewAudience { return &ViewAudience{Customers: v} }
	creates := []struct {
		name  string
		sc    Scope
		in    SavedViewInput
		field string
	}{
		{"shared without an audience", ScopeAll(), SavedViewInput{Shared: true}, "audience"},
		{"an audience on a private view", ScopeAll(), SavedViewInput{Audience: &ViewAudience{All: true}}, "audience"},
		{"both forms", ScopeAll(), SavedViewInput{Shared: true, Audience: &ViewAudience{All: true, Customers: []string{w.custA.ID}}}, "audience"},
		{"an empty list", ScopeAll(), SavedViewInput{Shared: true, Audience: ids()}, "audience"},
		{"an id that is not an id", ScopeAll(), SavedViewInput{Shared: true, Audience: ids("a b")}, "audience"},
		{"a customer that does not exist", ScopeAll(), SavedViewInput{Shared: true, Audience: ids("01J9Y4K3A0B1C2D3E4F5G6H7ZZ")}, "audience"},
		{"Bravo, by an account scoped to Alpha", alpha, SavedViewInput{Shared: true, Audience: ids(w.custB.ID)}, "audience"},
		{"Alpha and Bravo, by an account scoped to Alpha", alpha, SavedViewInput{Shared: true, Audience: ids(w.custA.ID, w.custB.ID)}, "audience"},
		{"all customers, by an account scoped to Alpha", alpha, SavedViewInput{Shared: true, Audience: &ViewAudience{All: true}}, "audience"},
		{"a filter naming Bravo, audience Alpha", ScopeAll(), SavedViewInput{Shared: true, Audience: ids(w.custA.ID),
			Filter: json.RawMessage(`{"field":"customer","op":"in","value":["` + w.custB.ID + `"]}`)}, "filter"},
		{"a filter naming Bravo's hostname, audience Alpha", ScopeAll(), SavedViewInput{Shared: true, Audience: ids(w.custA.ID),
			Filter: json.RawMessage(`{"field":"hostname","op":"eq","value":"B-DESK-1"}`)}, "filter"},
	}
	before := auditCount(t, w, "view.create")
	for _, c := range creates {
		in := c.in
		in.Name, in.Entity = uniq("refused"), "devices"
		_, err := w.s.CreateSavedViewAudited(w.ctx, c.sc, in, scoped)
		if !errors.Is(err, ErrInvalid) || len(FieldErrors(err)) == 0 || FieldErrors(err)[0].Field != c.field {
			t.Errorf("create, %s: err = %v, want a %s field error (400)", c.name, err, c.field)
		}
	}
	if n := auditCount(t, w, "view.create"); n != before {
		t.Errorf("refused creates wrote %d audit rows", n-before)
	}

	// Update. The owner is scoped to Alpha.
	priv := must[SavedView](t)(w.s.CreateSavedViewAudited(w.ctx, alpha, SavedViewInput{Name: "private", Entity: "devices"}, scoped))
	yes, no := true, false
	bravoFilter := json.RawMessage(`{"field":"customer","op":"in","value":["` + w.custB.ID + `"]}`)
	updates := auditCount(t, w, "view.update")
	for name, p := range map[string]SavedViewPatch{
		"made shared without an audience":    {Shared: &yes},
		"made shared with Bravo":             {Shared: &yes, Audience: ids(w.custB.ID)},
		"made shared with all customers":     {Shared: &yes, Audience: &ViewAudience{All: true}},
		"an audience while staying private":  {Audience: ids(w.custA.ID)},
		"made shared, a filter naming Bravo": {Shared: &yes, Audience: ids(w.custA.ID), Filter: &bravoFilter},
	} {
		if _, _, err := w.s.UpdateSavedViewAudited(w.ctx, alpha, priv.ID, p, scoped, false); !errors.Is(err, ErrInvalid) {
			t.Errorf("update, %s: err = %v, want ErrInvalid (400)", name, err)
		}
	}
	if n := auditCount(t, w, "view.update"); n != updates {
		t.Errorf("refused updates wrote %d audit rows", n-updates)
	}
	// Shared with Alpha: accepted. Then a filter naming Bravo, or an audience of Bravo, is refused.
	v, changed, err := w.s.UpdateSavedViewAudited(w.ctx, alpha, priv.ID, SavedViewPatch{Shared: &yes, Audience: ids(w.custA.ID)}, scoped, false)
	if err != nil || !changed || !v.Shared || v.Audience == nil || len(v.Audience.Customers) != 1 {
		t.Fatalf("share with Alpha: %+v changed=%v err=%v", v, changed, err)
	}
	if _, _, err := w.s.UpdateSavedViewAudited(w.ctx, alpha, priv.ID, SavedViewPatch{Filter: &bravoFilter}, scoped, false); !errors.Is(err, ErrInvalid) {
		t.Errorf("a filter naming Bravo on a view for Alpha = %v, want ErrInvalid", err)
	}
	if _, _, err := w.s.UpdateSavedViewAudited(w.ctx, alpha, priv.ID, SavedViewPatch{Audience: ids(w.custB.ID)}, scoped, false); !errors.Is(err, ErrInvalid) {
		t.Errorf("audience Bravo by an Alpha account = %v, want ErrInvalid", err)
	}
	// An all-customer admin widens it to Alpha and Bravo; then making it private clears the audience.
	v, _, err = w.s.UpdateSavedViewAudited(w.ctx, ScopeAll(), priv.ID, SavedViewPatch{Audience: ids(w.custB.ID, w.custA.ID)}, actorOf(w), true)
	ok(t, err)
	if len(v.Audience.Customers) != 2 || visibleTo(t, w, alpha, v.ID) {
		t.Errorf("audience Alpha and Bravo: %+v, visible to Alpha alone = %v (want false)", v.Audience, visibleTo(t, w, alpha, v.ID))
	}
	// Its audience now reaches past the owner's scope: the owner can no longer rename it (r4 LOW-4; making it
	// private is the M3-INT B.d case, TestOwnerWhoLostTheAudienceMayOnlyReduceItsExposure); the admin makes it private.
	renamed := "renamed by the Alpha owner"
	if _, _, err := w.s.UpdateSavedViewAudited(w.ctx, alpha, priv.ID, SavedViewPatch{Name: &renamed}, scoped, false); !errors.Is(err, ErrNotFound) {
		t.Errorf("the Alpha owner renaming a view for Alpha and Bravo = %v, want ErrNotFound", err)
	}
	v, _, err = w.s.UpdateSavedViewAudited(w.ctx, ScopeAll(), priv.ID, SavedViewPatch{Shared: &no}, actorOf(w), true)
	ok(t, err)
	if v.Shared || v.Audience != nil {
		t.Errorf("made private: shared %v audience %+v", v.Shared, v.Audience)
	}
	var col *string
	ok(t, w.s.Read(w.ctx, func(tx *Tx) error {
		return tx.QueryRow(`SELECT audience FROM saved_views WHERE id = ?`, priv.ID).Scan(&col)
	}))
	if col != nil {
		t.Errorf("a private view kept the audience %q", *col)
	}
}

// alphaOperator is a real operator account scoped to Alpha, as the actor of its own views.
func alphaOperator(t *testing.T, w *world) AuditActor {
	t.Helper()
	a := must[Admin](t)(w.s.CreateAdmin(w.ctx, Admin{Username: uniq("alpha-op"), Email: uniq("alpha") + "@example.invalid", Role: "operator",
		CustomerScope: []string{w.custA.ID}, PasswordHash: "$argon2id$test"}))
	return AuditActor{AdminID: a.ID, Username: a.Username, Method: "api"}
}

// r4 LOW-4, as amended by M3-INT B.d: an owner who has lost a customer cannot
// edit their own shared view whose audience is that customer (rename, filter,
// columns, or anything beside making it private): 404, as for a view they
// cannot see, so no text of theirs reaches accounts they no longer share a
// customer with. Making it private and deleting it only reduce who sees it, so
// both are allowed. Their other views stay editable; an all-customer admin can
// still delete such a view.
func TestOwnerWhoLostTheAudienceMayOnlyReduceItsExposure(t *testing.T) { // r4 LOW-4, M3-INT B.d
	w := seed(t)
	owner := alphaOperator(t, w)
	both := ScopeOf(w.custA.ID, w.custB.ID)
	forBravo := func(name string) SavedView {
		return must[SavedView](t)(w.s.CreateSavedViewAudited(w.ctx, both, SavedViewInput{Name: name, Entity: "devices", Shared: true,
			Audience: &ViewAudience{Customers: []string{w.custB.ID}}}, owner))
	}
	forB, toDelete, forAdmin := forBravo("For Bravo"), forBravo("For Bravo, to delete"), forBravo("For Bravo, for the admin")
	forA := must[SavedView](t)(w.s.CreateSavedViewAudited(w.ctx, both, SavedViewInput{Name: "For Alpha", Entity: "devices", Shared: true,
		Audience: &ViewAudience{Customers: []string{w.custA.ID}}}, owner))
	priv := must[SavedView](t)(w.s.CreateSavedViewAudited(w.ctx, both, SavedViewInput{Name: "Mine", Entity: "devices"}, owner))

	alphaOnly := ScopeOf(w.custA.ID) // the owner lost Bravo
	if got := must[SavedView](t)(w.s.GetSavedView(w.ctx, alphaOnly, owner.AdminID, forB.ID)); got.AudienceInside(alphaOnly) {
		t.Error("the view for Bravo reads as inside the scope of an owner scoped to Alpha")
	}
	n, no := "Renamed after losing Bravo", false
	filter, cols := json.RawMessage(`{"q":"pushed"}`), json.RawMessage(`["hostname"]`)
	for what, p := range map[string]SavedViewPatch{
		"rename":                {Name: &n},
		"filter":                {Filter: &filter},
		"columns":               {Columns: &cols},
		"private and a rename":  {Shared: &no, Name: &n},
		"private and a filter":  {Shared: &no, Filter: &filter},
		"an audience of Alpha":  {Audience: &ViewAudience{Customers: []string{w.custA.ID}}},
		"private, then Alpha's": {Shared: &no, Audience: &ViewAudience{Customers: []string{w.custA.ID}}},
	} {
		if _, _, err := w.s.UpdateSavedViewAudited(w.ctx, alphaOnly, forB.ID, p, owner, false); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s of the view for Bravo by an owner scoped to Alpha = %v, want ErrNotFound", what, err)
		}
	}
	if got := must[SavedView](t)(w.s.GetSavedView(w.ctx, ScopeAll(), w.admin.ID, forB.ID)); got.Name != "For Bravo" || !got.Shared {
		t.Errorf("a refused change altered the view: %+v", got)
	}
	v, changed, err := w.s.UpdateSavedViewAudited(w.ctx, alphaOnly, forB.ID, SavedViewPatch{Shared: &no}, owner, false)
	if err != nil || !changed || v.Shared || v.Audience != nil {
		t.Errorf("making it private: %+v changed=%v err=%v, want a private view", v, changed, err)
	}
	if err := w.s.DeleteSavedViewAudited(w.ctx, alphaOnly, toDelete.ID, owner, false); err != nil {
		t.Errorf("the owner deleting their view for Bravo: %v", err)
	}
	for _, id := range []string{forA.ID, priv.ID} {
		if _, _, err := w.s.UpdateSavedViewAudited(w.ctx, alphaOnly, id, SavedViewPatch{Name: ptrTo(uniq("still mine"))}, owner, false); err != nil {
			t.Errorf("the owner's view %s inside their scope: %v", id, err)
		}
	}
	if err := w.s.DeleteSavedViewAudited(w.ctx, ScopeAll(), forAdmin.ID, actorOf(w), true); err != nil {
		t.Errorf("an all-customer admin deleting it: %v", err)
	}
}

func ptrTo[T any](v T) *T { return &v }
