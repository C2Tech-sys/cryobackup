package auth

import (
	"context"
	"errors"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// M2-W3D: signing out leaves a trace (auth.logout), like signing in does, and the
// trace is best effort: it never stands between the operator and the exit.
func TestEndSessionWritesALogoutRowAndNeverBlocksTheExit(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.addAdmin(t, "ann", "operator", "right horse")
	rows := func() []store.AuditEvent {
		l, err := e.st.ListAudit(ctx, store.ScopeAll(), store.AuditFilter{Action: "auth.logout"}, store.Page{Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		return l.Items
	}

	res := login(t, e, "ann", "right horse")
	p, err := e.m.Authenticate(ctx, res.Token)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.m.EndSession(ctx, p, "api", "203.0.113.9"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.Authenticate(ctx, res.Token); !errors.Is(err, ErrNoSession) {
		t.Fatalf("after sign-out: %v", err)
	}
	got := rows()
	if len(got) != 1 || got[0].ActorLabel != "ann" || got[0].Method != "api" || got[0].SourceIP != "203.0.113.9" || got[0].Result != "ok" {
		t.Fatalf("logout rows: %+v", got)
	}

	// With the audit table refusing inserts the sign-out still happens.
	res = login(t, e, "ann", "right horse")
	p, _ = e.m.Authenticate(ctx, res.Token)
	if err := e.st.Write(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`CREATE TRIGGER refuse_audits BEFORE INSERT ON audit_events BEGIN SELECT RAISE(ABORT, 'audit refused by the test'); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.m.EndSession(ctx, p, "web", "203.0.113.9"); err != nil {
		t.Fatalf("a failed audit write blocked the sign-out: %v", err)
	}
	if _, err := e.m.Authenticate(ctx, res.Token); !errors.Is(err, ErrNoSession) {
		t.Fatalf("the session survived a sign-out whose audit failed: %v", err)
	}
	if len(rows()) != 1 {
		t.Fatalf("a row appeared although the audit table refuses inserts")
	}

	// No principal: nothing to end, nothing to audit.
	if err := e.m.EndSession(ctx, nil, "web", ""); err != nil || len(rows()) != 1 {
		t.Fatalf("nil principal: %v, %d rows", err, len(rows()))
	}
}
