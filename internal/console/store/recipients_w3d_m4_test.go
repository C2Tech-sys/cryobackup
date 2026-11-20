package store

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// A patch changes only the fields it names; the others keep what is stored.
func TestM4PatchRecipientChangesOnlyNamedFields(t *testing.T) {
	w := seed(t)
	all, actor := ScopeAll(), m4Actor(w)
	r := must[Recipient](t)(w.s.CreateRecipient(w.ctx, all, RecipientInput{Name: "A", Address: "a@example.test", Alerts: true, Recoveries: true, Digest: true}, actor))
	w.clk.Advance(time.Second)
	u := must[Recipient](t)(w.s.PatchRecipient(w.ctx, all, r.ID, RecipientPatch{Name: ptr("  B  ")}, actor))
	if u.Name != "B" || u.Address != "a@example.test" || !u.Alerts || !u.Recoveries || !u.Digest || !u.UpdatedAt.After(r.UpdatedAt) {
		t.Fatalf("rename: %+v", u)
	}
	u = must[Recipient](t)(w.s.PatchRecipient(w.ctx, all, r.ID, RecipientPatch{Digest: ptr(false)}, actor))
	if u.Name != "B" || !u.Alerts || !u.Recoveries || u.Digest {
		t.Fatalf("digest off: %+v", u)
	}
	if n := auditCount(t, w, "recipient.update"); n != 2 {
		t.Fatalf("%d recipient.update audit rows, want 2", n)
	}
}

// A patch that changes nothing (empty, or the stored values) writes nothing and no audit row.
func TestM4PatchRecipientNoOpIsNotAudited(t *testing.T) {
	w := seed(t)
	all, actor := ScopeAll(), m4Actor(w)
	r := must[Recipient](t)(w.s.CreateRecipient(w.ctx, all, RecipientInput{Name: "A", Address: "a@example.test", Alerts: true}, actor))
	w.clk.Advance(time.Second)
	for _, p := range []RecipientPatch{{}, {Name: ptr("A"), Alerts: ptr(true), Digest: ptr(false)}} {
		u := must[Recipient](t)(w.s.PatchRecipient(w.ctx, all, r.ID, p, actor))
		if !u.UpdatedAt.Equal(r.UpdatedAt) || u != r {
			t.Fatalf("a no-op patch changed the row: %+v vs %+v", u, r)
		}
	}
	if n := auditCount(t, w, "recipient.update"); n != 0 {
		t.Fatalf("a no-op patch was audited (%d rows)", n)
	}
}

// Errors: unknown id, an invalid named field, an address another recipient has, a non-global scope.
func TestM4PatchRecipientErrors(t *testing.T) {
	w := seed(t)
	all, actor := ScopeAll(), m4Actor(w)
	a := must[Recipient](t)(w.s.CreateRecipient(w.ctx, all, RecipientInput{Name: "A", Address: "a@example.test", Alerts: true}, actor))
	must[Recipient](t)(w.s.CreateRecipient(w.ctx, all, RecipientInput{Name: "B", Address: "b@example.test", Alerts: true}, actor))
	if _, err := w.s.PatchRecipient(w.ctx, all, "01JNOSUCHRULE0000000000000", RecipientPatch{Name: ptr("x")}, actor); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id: %v", err)
	}
	var ve *ValidationError
	if _, err := w.s.PatchRecipient(w.ctx, all, a.ID, RecipientPatch{Address: ptr("Ops <a@example.test>")}, actor); !errors.As(err, &ve) {
		t.Fatalf("invalid address: %v", err)
	}
	if _, err := w.s.PatchRecipient(w.ctx, all, a.ID, RecipientPatch{Name: ptr("")}, actor); !errors.As(err, &ve) {
		t.Fatalf("empty name: %v", err)
	}
	if _, err := w.s.PatchRecipient(w.ctx, all, a.ID, RecipientPatch{Address: ptr("B@example.test")}, actor); !errors.Is(err, ErrConflict) {
		t.Fatalf("taken address: %v", err)
	}
	if _, err := w.s.PatchRecipient(w.ctx, ScopeOf(w.custA.ID), a.ID, RecipientPatch{Name: ptr("x")}, actor); !errors.Is(err, ErrScope) {
		t.Fatalf("customer scope: %v", err)
	}
	if g := must[Recipient](t)(w.s.GetRecipient(w.ctx, all, a.ID)); g != a {
		t.Fatalf("a refused patch changed the row: %+v", g)
	}
}

// A rule disabled outside the console reads with its switches off; a patch that changes it
// enables it with the switches it then has, and other trigger entries are kept.
func TestM4PatchRecipientDisabledRuleAndOtherTriggers(t *testing.T) {
	w := seed(t)
	all, actor := ScopeAll(), m4Actor(w)
	r := must[Recipient](t)(w.s.CreateRecipient(w.ctx, all, RecipientInput{Name: "A", Address: "a@example.test", Alerts: true, Recoveries: true, Digest: true}, actor))
	ok(t, w.s.Write(w.ctx, func(tx *Tx) error {
		_, err := tx.Exec(`UPDATE notification_rules SET enabled = 0, triggers = '["alert","nag"]' WHERE id = ?`, r.ID)
		return err
	}))
	u := must[Recipient](t)(w.s.PatchRecipient(w.ctx, all, r.ID, RecipientPatch{Alerts: ptr(true)}, actor))
	if !u.Alerts || u.Recoveries || u.Digest {
		t.Fatalf("patched disabled rule: %+v", u)
	}
	if trig, _, _, _, _, _, enabled := rawRule(t, w, r.ID); enabled != 1 || trig != `["nag","alert"]` {
		t.Fatalf("enabled = %d, triggers = %s", enabled, trig)
	}
}

// Two edits at the same moment that name different fields both stand: 30 pairs of a rename and
// switching alerts off, each pair started together, all end with the new name and alerts off.
func TestM4PatchRecipientConcurrentEditsBothStand(t *testing.T) {
	w := seed(t)
	all, actor := ScopeAll(), m4Actor(w)
	for i := 0; i < 30; i++ {
		r := must[Recipient](t)(w.s.CreateRecipient(w.ctx, all, RecipientInput{Name: uniq("Old"), Address: uniq("p") + "@example.test", Alerts: true, Recoveries: true, Digest: true}, actor))
		name := uniq("New")
		var wg sync.WaitGroup
		start := make(chan struct{})
		errs := make([]error, 2)
		for j, p := range []RecipientPatch{{Name: ptr(name)}, {Alerts: ptr(false)}} {
			wg.Add(1)
			go func(j int, p RecipientPatch) {
				defer wg.Done()
				<-start
				_, errs[j] = w.s.PatchRecipient(w.ctx, all, r.ID, p, actor)
			}(j, p)
		}
		close(start)
		wg.Wait()
		for _, err := range errs {
			ok(t, err)
		}
		g := must[Recipient](t)(w.s.GetRecipient(w.ctx, all, r.ID))
		if g.Name != name || g.Alerts || !g.Recoveries || !g.Digest {
			t.Fatalf("pair %d lost an edit: %+v (want name %s, alerts off)", i, g, name)
		}
	}
}
