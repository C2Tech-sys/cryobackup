package auth

import (
	"context"
	"errors"
	"testing"
)

// The auth layer refuses every account change to an owner's API token
// (ErrSessionRequired), whatever the transport: the admin API gate is one
// guard, this is the other (foreman review 2, M2-W1A).
func TestOwnerOperationsRefuseAPITokens(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.addAdmin(t, "ow", "owner", "right horse")
	e.addAdmin(t, "op", "operator", "right horse")
	sess, _ := e.m.Authenticate(ctx, login(t, e, "ow", "right horse").Token)
	tok, _, err := e.m.CreateAPIToken(ctx, sess, "full owner", ScopeFull, 30, "web", "")
	if err != nil {
		t.Fatal(err)
	}
	tp, err := e.m.AuthenticateBearer(ctx, tok)
	if err != nil || tp.Role != RoleOwner {
		t.Fatalf("token principal: %v %+v", err, tp)
	}
	id := mustAdmin(t, e, "op").ID
	ops := map[string]func(p *Principal) error{
		"create": func(p *Principal) error {
			_, _, err := e.m.CreateAccount(ctx, p, AccountInput{Username: "minted", Email: "m@example.invalid", Role: "read_only"}, "api", "")
			return err
		},
		"update":         func(p *Principal) error { _, err := e.m.UpdateAccount(ctx, p, id, "admin", nil, "api", ""); return err },
		"disable":        func(p *Principal) error { return e.m.DisableAccount(ctx, p, id, "", "api", "") },
		"enable":         func(p *Principal) error { return e.m.EnableAccount(ctx, p, id, "api", "") },
		"reset mfa":      func(p *Principal) error { return e.m.ResetAccountMFA(ctx, p, id, "", "api", "") },
		"reset password": func(p *Principal) error { _, err := e.m.ResetAccountPassword(ctx, p, id, "api", ""); return err },
		"delete":         func(p *Principal) error { return e.m.DeleteAccount(ctx, p, id, "api", "") },
	}
	for name, op := range ops {
		if err := op(tp); !errors.Is(err, ErrSessionRequired) {
			t.Errorf("%s with an owner token: %v, want ErrSessionRequired", name, err)
		}
	}
	if a := mustAdmin(t, e, "op"); a.Role != "operator" || a.DisabledAt != nil {
		t.Fatalf("a refused token changed the account: %+v", a)
	}
	// The owner's session can.
	if _, err := e.m.UpdateAccount(ctx, sess, id, "admin", nil, "web", ""); err != nil {
		t.Fatalf("owner session: %v", err)
	}
}
