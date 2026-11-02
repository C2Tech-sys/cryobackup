package auth

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// Two pending sessions of one account without TOTP confirm their enrollments
// at the same time: exactly one wins, the other is ErrAlreadyEnrolled, and the
// stored secret is the winner's (foreman verification notes, M2-W1A).
func TestConcurrentEnrollmentsOnlyOneWins(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.addAdmin(t, "ow", "owner", "right horse")
	const n = 4
	ps := make([]*Principal, n)
	secrets := make([][]byte, n)
	for i := range ps {
		ps[i], _ = pendingOf(t, e, "ow", "right horse")
		en, err := e.m.BeginTOTP(ctx, ps[i])
		if err != nil {
			t.Fatal(err)
		}
		secrets[i] = secretOf(t, en.Secret)
	}
	at := e.mfa.next()
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range ps {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, errs[i] = e.m.ConfirmTOTP(ctx, ps[i], TOTPCode(secrets[i], at), "", "web", "192.0.2.1", "")
		}(i)
	}
	wg.Wait()
	won := -1
	for i, err := range errs {
		switch {
		case err == nil:
			if won >= 0 {
				t.Fatalf("enrollments %d and %d both succeeded", won, i)
			}
			won = i
		case !errors.Is(err, ErrAlreadyEnrolled):
			t.Fatalf("enrollment %d: %v", i, err)
		}
	}
	if won < 0 {
		t.Fatal("no enrollment succeeded")
	}
	a := mustAdmin(t, e, "ow")
	got, err := e.m.Master.Unwrap(ContextTOTP, a.ID, 1, a.TOTPSecretWrapped)
	if err != nil || string(got) != string(secrets[won]) {
		t.Fatal("the stored secret is not the winner's")
	}
}
