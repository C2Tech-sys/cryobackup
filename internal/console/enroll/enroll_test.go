package enroll

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

func TestTokenShape(t *testing.T) {
	p, h, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if !WellFormed(p) || len(h) != 64 || h != HashToken(p) || strings.ToLower(h) != h {
		t.Fatalf("token %q hash %q", p, h)
	}
	q, _, _ := NewToken()
	if p == q {
		t.Fatal("tokens must differ")
	}
	for _, bad := range []string{"", "short", strings.Repeat("a", 42), strings.Repeat("a", 44), strings.Repeat("+", 43), strings.Repeat("a", 42) + "="} {
		if WellFormed(bad) {
			t.Errorf("%q must not be well formed", bad)
		}
	}
}

func TestParseDuration(t *testing.T) {
	ok := map[string]time.Duration{"7d": 7 * 24 * time.Hour, "2w": 14 * 24 * time.Hour, "90m": 90 * time.Minute, "12h": 12 * time.Hour}
	for in, want := range ok {
		if got, err := ParseDuration(in); err != nil || got != want {
			t.Errorf("%s: %v %v", in, got, err)
		}
	}
	for _, in := range []string{"", "0d", "-1d", "xd", "5", "1y"} {
		if _, err := ParseDuration(in); err == nil {
			t.Errorf("%q must fail", in)
		}
	}
}

func TestMintStoresOnlyTheHash(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	st, err := store.OpenMemory(ctx, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := store.LoadFixtures(ctx, st, now); err != nil {
		t.Fatal(err)
	}
	cs, _ := st.ListCustomers(ctx, store.ScopeAll())
	uses := 3
	plain, tok, err := Mint(ctx, st, store.ScopeAll(), cs[0].ID, MintOptions{Label: "t", Expires: 48 * time.Hour, MaxUses: &uses})
	if err != nil {
		t.Fatal(err)
	}
	if tok.TokenHash != HashToken(plain) || !tok.ExpiresAt.Equal(now.Add(48*time.Hour)) || tok.MaxUses == nil || *tok.MaxUses != 3 {
		t.Fatalf("token row: %+v", tok)
	}
	if strings.Contains(tok.Label+tok.TokenHash, plain) {
		t.Fatal("secret stored")
	}
	if _, _, err := Mint(ctx, st, store.ScopeOf("other"), cs[0].ID, MintOptions{}); err == nil {
		t.Fatal("out-of-scope mint must fail")
	}
}
