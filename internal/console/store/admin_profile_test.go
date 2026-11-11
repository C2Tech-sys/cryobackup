package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// UpdateAdminProfile (M3-PROFILE): username and display name of one account.
func TestUpdateAdminProfile(t *testing.T) {
	s, clk := newStore(t)
	ctx := context.Background()
	a := must[Admin](t)(s.CreateAdmin(ctx, Admin{Username: "alex", Email: "alex@example.invalid", Role: "operator", PasswordHash: "$argon2id$test"}))
	other := must[Admin](t)(s.CreateAdmin(ctx, Admin{Username: "sam", Email: "sam@example.invalid", Role: "operator", PasswordHash: "$argon2id$test"}))
	profile := func(id, username, display string) error {
		return s.Write(ctx, func(tx *Tx) error { return tx.UpdateAdminProfile(id, username, display) })
	}

	t.Run("set the display name and the username", func(t *testing.T) {
		clk.Advance(time.Minute)
		if err := profile(a.ID, "alex.k", "Alex K."); err != nil {
			t.Fatal(err)
		}
		got := must[Admin](t)(s.GetAdmin(ctx, a.ID))
		if got.Username != "alex.k" || got.DisplayName != "Alex K." {
			t.Fatalf("stored %q / %q", got.Username, got.DisplayName)
		}
		if !got.UpdatedAt.After(a.UpdatedAt) {
			t.Fatalf("updated_at not advanced: %v -> %v", a.UpdatedAt, got.UpdatedAt)
		}
		if got.Email != a.Email || got.Role != a.Role {
			t.Fatal("a profile change touched the e-mail or the role")
		}
	})

	t.Run("an empty display name stores NULL", func(t *testing.T) {
		if err := profile(a.ID, "alex.k", ""); err != nil {
			t.Fatal(err)
		}
		if got := must[Admin](t)(s.GetAdmin(ctx, a.ID)); got.DisplayName != "" {
			t.Fatalf("display name %q after clearing", got.DisplayName)
		}
		var isNull bool
		err := s.Read(ctx, func(tx *Tx) error {
			return tx.QueryRow(`SELECT display_name IS NULL FROM admin_users WHERE id = ?`, a.ID).Scan(&isNull)
		})
		if err != nil || !isNull {
			t.Fatalf("display_name is not NULL (err %v)", err)
		}
	})

	t.Run("a username is required", func(t *testing.T) {
		if err := profile(a.ID, "", "x"); !errors.Is(err, ErrInvalid) {
			t.Fatalf("empty username: %v, want ErrInvalid", err)
		}
	})

	t.Run("a duplicate username is ErrConflict, case-insensitively", func(t *testing.T) {
		for _, name := range []string{"sam", "SAM", "Sam"} {
			err := profile(a.ID, name, "")
			if !errors.Is(err, ErrConflict) {
				t.Fatalf("username %q: %v, want errors.Is(err, ErrConflict)", name, err)
			}
		}
		if got := must[Admin](t)(s.GetAdmin(ctx, a.ID)); got.Username != "alex.k" {
			t.Fatalf("a refused change altered the username: %q", got.Username)
		}
		// Keeping one's own name (even in other case) is not a collision.
		if err := profile(other.ID, "SAM", "Sam"); err != nil {
			t.Fatalf("own name in new case: %v", err)
		}
	})

	t.Run("an unknown id is ErrNotFound", func(t *testing.T) {
		if err := profile("no-such-id", "ghost", ""); !errors.Is(err, ErrNotFound) {
			t.Fatalf("unknown id: %v, want ErrNotFound", err)
		}
	})
}
