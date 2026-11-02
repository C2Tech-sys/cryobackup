package auth

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
)

// A password change, a password reset through a link and a reset-mfa are the
// "this account may be compromised" actions: each revokes the account's API
// tokens in the same transaction as the change (foreman review, M2-W1A).
func TestCredentialResetsRevokeAPITokens(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.addAdmin(t, "ow", "owner", "right horse")
	e.addAdmin(t, "op", "operator", "right horse")
	owp, _ := e.m.Authenticate(ctx, login(t, e, "ow", "right horse").Token)
	opPW := "right horse"
	n := 0
	newToken := func() string {
		t.Helper()
		opp, err := e.m.Authenticate(ctx, login(t, e, "op", opPW).Token)
		if err != nil {
			t.Fatal(err)
		}
		n++
		tok, _, err := e.m.CreateAPIToken(ctx, opp, "t"+strconv.Itoa(n), ScopeFull, 30, "web", "")
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	// 1. Password change.
	tok := newToken()
	opp, _ := e.m.Authenticate(ctx, login(t, e, "op", opPW).Token)
	if err := e.m.ChangePassword(ctx, opp, opPW, "operator password two", "web", ""); err != nil {
		t.Fatal(err)
	}
	opPW = "operator password two"
	if _, err := e.m.AuthenticateBearer(ctx, tok); !errors.Is(err, ErrNoSession) {
		t.Fatalf("a token survived a password change: %v", err)
	}
	// 2. Password reset through a link.
	tok = newToken()
	link, err := e.m.ResetAccountPassword(ctx, owp, mustAdmin(t, e, "op").ID, "web", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.CompletePasswordReset(ctx, link.Token, "operator password three", ""); err != nil {
		t.Fatal(err)
	}
	opPW = "operator password three"
	if _, err := e.m.AuthenticateBearer(ctx, tok); !errors.Is(err, ErrNoSession) {
		t.Fatalf("a token survived a password reset: %v", err)
	}
	// 3. Reset MFA (the account re-enrolls at its next login).
	tok = newToken()
	if err := e.m.ResetAccountMFA(ctx, owp, mustAdmin(t, e, "op").ID, "lost phone", "web", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.AuthenticateBearer(ctx, tok); !errors.Is(err, ErrNoSession) {
		t.Fatalf("a token survived reset-mfa: %v", err)
	}
	// Each audit row records how many tokens it revoked.
	for _, action := range []string{"admin.password_change", "admin.password_reset.complete", "admin.reset_mfa"} {
		found := false
		for _, ev := range allAudit(t, e.st) {
			if ev.Action == action && ev.Result == "ok" && strings.Contains(string(ev.Details), `"tokens_revoked":1`) {
				found = true
			}
		}
		if !found {
			t.Errorf("the %s audit row does not record the revoked token", action)
		}
	}
}
