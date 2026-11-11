package auth

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// auditOf returns the audit rows of one action, newest first.
func auditOf(t *testing.T, st *store.Store, action string) []store.AuditEvent {
	t.Helper()
	l, err := st.ListAudit(context.Background(), store.ScopeAll(), store.AuditFilter{Action: action}, store.Page{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	return l.Items
}

func detailsOf(t *testing.T, ev store.AuditEvent) map[string]any {
	t.Helper()
	var d map[string]any
	if len(ev.Details) > 0 {
		if err := json.Unmarshal(ev.Details, &d); err != nil {
			t.Fatalf("details %s: %v", ev.Details, err)
		}
	}
	return d
}

// A bad username or display name is refused before anything is written.
func TestChangeProfileValidation(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.addAdmin(t, "ann", "operator", "right horse")
	p, _ := e.m.Authenticate(ctx, login(t, e, "ann", "right horse").Token)

	cases := []struct {
		name, username, display string
		want                    error
	}{
		{"empty username", "", "", ErrBadUsername},
		{"blank username", "   ", "", ErrBadUsername},
		{"space inside", "ann marie", "", ErrBadUsername},
		{"leading dot", ".ann", "", ErrBadUsername},
		{"slash", "ann/x", "", ErrBadUsername},
		{"non-ASCII", "anné", "", ErrBadUsername},
		{"101 characters", strings.Repeat("a", 101), "", ErrBadUsername},
		{"display with newline", "ann", "Ann\nMarie", ErrBadDisplayName},
		{"display with tab", "ann", "Ann\tMarie", ErrBadDisplayName},
		{"display with NUL", "ann", "Ann\x00", ErrBadDisplayName},
		{"display with escape", "ann", "\x1b[31mAnn", ErrBadDisplayName},
		{"display 101 characters", "ann", strings.Repeat("é", 101), ErrBadDisplayName},
		{"display invalid UTF-8", "ann", "Ann\xff", ErrBadDisplayName},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := e.m.ChangeProfile(ctx, p, c.username, c.display, "web", ""); !errors.Is(err, c.want) {
				t.Fatalf("got %v, want %v", err, c.want)
			}
		})
	}
	if got := mustAdmin(t, e, "ann"); got.Username != "ann" || got.DisplayName != "" {
		t.Fatalf("a refused change was stored: %q / %q", got.Username, got.DisplayName)
	}
	if n := auditCount(t, e.st, "admin.username_change") + auditCount(t, e.st, "admin.display_name_change"); n != 0 {
		t.Fatalf("%d profile audit rows after refused changes", n)
	}

	// The boundaries are accepted: 100 characters each; surrounding space is trimmed.
	if err := e.m.ChangeProfile(ctx, p, "  "+strings.Repeat("a", 100)+" ", strings.Repeat("é", 100), "web", ""); err != nil {
		t.Fatalf("100-character values: %v", err)
	}
	if got := mustAdmin(t, e, strings.Repeat("a", 100)); got.Username != strings.Repeat("a", 100) {
		t.Fatalf("username not trimmed: %q", got.Username)
	}
	// An e-mail address is a valid username (usernameRE allows @).
	if err := e.m.ChangeProfile(ctx, p, "ann@example.invalid", "", "web", ""); err != nil {
		t.Fatalf("e-mail style username: %v", err)
	}
}

// All fields apply together or not at all: a bad display name keeps a good
// username and e-mail from being stored, and a username collision rolls back
// the e-mail change made in the same call.
func TestUpdateSelfProfileIsAllOrNothing(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.addAdmin(t, "ann", "operator", "right horse")
	p, _ := e.m.Authenticate(ctx, login(t, e, "ann", "right horse").Token)
	newName, badDisplay, newEmail := "ann2", "x\ny", "ann2@example.invalid"
	err := e.m.UpdateSelf(ctx, p, SelfUpdate{Username: &newName, DisplayName: &badDisplay, Email: &newEmail}, "web", "")
	if !errors.Is(err, ErrBadDisplayName) {
		t.Fatalf("got %v", err)
	}
	if got := mustAdmin(t, e, "ann"); got.Email != "ann@example.invalid" {
		t.Fatalf("e-mail changed despite the refusal: %q", got.Email)
	}
	e.addAdmin(t, "bob", "operator", "right horse")
	taken := "BOB"
	err = e.m.UpdateSelf(ctx, p, SelfUpdate{Username: &taken, Email: &newEmail}, "web", "")
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("collision: %v, want store.ErrConflict", err)
	}
	if got := mustAdmin(t, e, "ann"); got.Email != "ann@example.invalid" || got.Username != "ann" {
		t.Fatalf("collision left a partial change: %q %q", got.Username, got.Email)
	}
	if n := auditCount(t, e.st, "admin.email_change"); n != 0 {
		t.Fatalf("%d e-mail audit rows after a rolled-back change", n)
	}
}

// The audit rows say what changed, once, and only when it changed.
func TestChangeProfileAuditRows(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	acct := e.addAdmin(t, "ann", "operator", "right horse")
	p, _ := e.m.Authenticate(ctx, login(t, e, "ann", "right horse").Token)

	// Display name only: no username row.
	if err := e.m.ChangeProfile(ctx, p, "ann", "Ann Example", "web", "192.0.2.7"); err != nil {
		t.Fatal(err)
	}
	if n := auditCount(t, e.st, "admin.username_change"); n != 0 {
		t.Fatalf("%d username rows for an unchanged username", n)
	}
	rows := auditOf(t, e.st, "admin.display_name_change")
	if len(rows) != 1 {
		t.Fatalf("%d display-name rows, want 1", len(rows))
	}
	d := detailsOf(t, rows[0])
	if d["set"] != true || len(d) != 1 {
		t.Fatalf("display-name details %v, want {set:true} and nothing else (the text is not logged)", d)
	}
	if strings.Contains(string(rows[0].Details), "Example") {
		t.Fatal("the display name text reached the audit row")
	}
	if rows[0].ObjectType != "admin" || rows[0].ObjectID != acct.ID || rows[0].Method != "web" || rows[0].Result != "ok" {
		t.Fatalf("row %+v", rows[0])
	}
	if rows[0].ActorLabel != "ann" {
		t.Fatalf("actor label %q, want the username at the time", rows[0].ActorLabel)
	}

	// Username only: one username row with from/to, no new display-name row.
	if err := e.m.ChangeProfile(ctx, p, "ann.e", "Ann Example", "web", ""); err != nil {
		t.Fatal(err)
	}
	un := auditOf(t, e.st, "admin.username_change")
	if len(un) != 1 {
		t.Fatalf("%d username rows, want 1", len(un))
	}
	if d := detailsOf(t, un[0]); d["from"] != "ann" || d["to"] != "ann.e" || len(d) != 2 {
		t.Fatalf("username details %v", d)
	}
	if n := auditCount(t, e.st, "admin.display_name_change"); n != 1 {
		t.Fatalf("%d display-name rows after a username-only change, want still 1", n)
	}

	// Both at once, the display name cleared: {set:false}.
	if err := e.m.ChangeProfile(ctx, p, "ann.f", "", "web", ""); err != nil {
		t.Fatal(err)
	}
	if n := auditCount(t, e.st, "admin.username_change"); n != 2 {
		t.Fatalf("%d username rows, want 2", n)
	}
	rows = auditOf(t, e.st, "admin.display_name_change")
	if len(rows) != 2 {
		t.Fatalf("%d display-name rows, want 2", len(rows))
	}
	cleared := false
	for _, r := range rows {
		if detailsOf(t, r)["set"] == false {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("no display-name row with {set:false} after clearing it")
	}

	// Saving the same values writes nothing.
	before := len(allAudit(t, e.st))
	if err := e.m.ChangeProfile(ctx, p, "ann.f", "  ", "web", ""); err != nil {
		t.Fatal(err)
	}
	if after := len(allAudit(t, e.st)); after != before {
		t.Fatalf("an unchanged save wrote %d audit rows", after-before)
	}
	// A case-only difference is a change: the stored spelling changes.
	if err := e.m.ChangeProfile(ctx, p, "Ann.F", "", "web", ""); err != nil {
		t.Fatal(err)
	}
	if n := auditCount(t, e.st, "admin.username_change"); n != 3 {
		t.Fatalf("%d username rows after a case-only change, want 3", n)
	}
}

// A username change ends no session and revokes no token; the old name stops
// signing in, the new one works.
func TestUsernameChangeKeepsSessionsAndTokens(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.addAdmin(t, "ann", "owner", "right horse")
	a := login(t, e, "ann", "right horse")
	b := login(t, e, "ann", "right horse")
	p, _ := e.m.Authenticate(ctx, a.Token)
	secret, _, err := e.m.CreateAPIToken(ctx, p, "t1", ScopeFull, 7, "web", "")
	if err != nil {
		t.Fatal(err)
	}

	if err := e.m.ChangeProfile(ctx, p, "ann.renamed", "Ann", "web", ""); err != nil {
		t.Fatal(err)
	}
	for name, tok := range map[string]string{"this session": a.Token, "another session": b.Token} {
		got, err := e.m.Authenticate(ctx, tok)
		if err != nil {
			t.Fatalf("%s ended by a username change: %v", name, err)
		}
		if got.Admin.Username != "ann.renamed" || got.Admin.DisplayName != "Ann" {
			t.Fatalf("%s sees %q / %q", name, got.Admin.Username, got.Admin.DisplayName)
		}
	}
	if tp, err := e.m.AuthenticateBearer(ctx, secret); err != nil {
		t.Fatalf("API token revoked by a username change: %v", err)
	} else if tp.Admin.Username != "ann.renamed" {
		t.Fatalf("token sees %q", tp.Admin.Username)
	}
	if n := auditCount(t, e.st, "admin.password_change"); n != 0 {
		t.Fatalf("%d password-change rows from a username change", n)
	}

	// Sign-in: the old name fails, the new one works, in any case.
	if _, err := e.m.Login(ctx, "ann", "right horse", "192.0.2.1", ""); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("login with the old username: %v, want ErrInvalidCredentials", err)
	}
	// The MFA helper keys its authenticator secrets by username: carry ann's over.
	e.mfa.mu.Lock()
	e.mfa.secrets["ann.renamed"], e.mfa.codes["ann.renamed"] = e.mfa.secrets["ann"], e.mfa.codes["ann"]
	e.mfa.mu.Unlock()
	login(t, e, "ann.renamed", "right horse")
	login(t, e, "ANN.RENAMED", "right horse")
}

// Only a signed-in session may change the profile: a token and no principal are refused.
func TestChangeProfileNeedsASession(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.addAdmin(t, "ann", "owner", "right horse")
	p, _ := e.m.Authenticate(ctx, login(t, e, "ann", "right horse").Token)
	secret, _, err := e.m.CreateAPIToken(ctx, p, "t1", ScopeFull, 7, "web", "")
	if err != nil {
		t.Fatal(err)
	}
	tp, err := e.m.AuthenticateBearer(ctx, secret)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.m.ChangeProfile(ctx, tp, "other", "", "api", ""); !errors.Is(err, ErrSessionRequired) {
		t.Fatalf("token: %v, want ErrSessionRequired", err)
	}
	if err := e.m.ChangeProfile(ctx, nil, "other", "", "web", ""); !errors.Is(err, ErrNoSession) {
		t.Fatalf("no principal: %v, want ErrNoSession", err)
	}
}
