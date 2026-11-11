package auth

import (
	"context"
	"errors"
	"testing"
)

// A display name is what the shell shows for a person, so it must not be able
// to pass for someone else: invisible and bidi-override characters are refused,
// and so is another account's username (ignoring case). The account's own
// username is fine as its display name.
func TestDisplayNameCannotImpersonate(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.addAdmin(t, "ann", "operator", "right horse")
	e.addAdmin(t, "bob", "operator", "right horse")
	p, _ := e.m.Authenticate(ctx, login(t, e, "ann", "right horse").Token)

	cases := []struct {
		name, display string
		want          error
	}{
		{"zero-width space", "Ann" + string(rune(0x200b)) + "Marie", ErrBadDisplayName},
		{"zero-width joiner", "Ann" + string(rune(0x200d)) + "Marie", ErrBadDisplayName},
		{"right-to-left override", string(rune(0x202e)) + "Ann", ErrBadDisplayName},
		{"soft hyphen", "An" + string(rune(0xad)) + "n", ErrBadDisplayName},
		{"byte order mark", string(rune(0xfeff)) + "Ann", ErrBadDisplayName},
		{"line separator", "Ann" + string(rune(0x2028)) + "Marie", ErrBadDisplayName},
		{"paragraph separator", "Ann" + string(rune(0x2029)) + "Marie", ErrBadDisplayName},
		{"private use", "Ann" + string(rune(0xe000)), ErrBadDisplayName},
		{"another account's username", "bob", ErrDisplayNameTaken},
		{"another account's username, other case", "BOB", ErrDisplayNameTaken},
		{"another account's username, padded", "  Bob ", ErrDisplayNameTaken},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := e.m.ChangeProfile(ctx, p, "ann", c.display, "web", ""); !errors.Is(err, c.want) {
				t.Fatalf("got %v, want %v", err, c.want)
			}
		})
	}
	if got := mustAdmin(t, e, "ann"); got.DisplayName != "" {
		t.Fatalf("a refused display name was stored: %q", got.DisplayName)
	}
	if n := auditCount(t, e.st, "admin.display_name_change"); n != 0 {
		t.Fatalf("%d display-name audit rows after refused changes", n)
	}

	// Allowed: ordinary letters, marks, punctuation and symbols; the own username.
	for _, ok := range []string{"Ann Marie", "Ann-Marie O'Neil", "Ánh Nguyễn", "山田 太郎", "Ann (ops) ★", "ann", "ANN"} {
		if err := e.m.ChangeProfile(ctx, p, "ann", ok, "web", ""); err != nil {
			t.Fatalf("%q refused: %v", ok, err)
		}
	}
	// A collision inside one call rolls back the username change made with it.
	if err := e.m.ChangeProfile(ctx, p, "ann.renamed", "bob", "web", ""); !errors.Is(err, ErrDisplayNameTaken) {
		t.Fatalf("got %v, want ErrDisplayNameTaken", err)
	}
	if got := mustAdmin(t, e, "ann"); got.Username != "ann" {
		t.Fatalf("username changed although the display name was refused: %q", got.Username)
	}
}
