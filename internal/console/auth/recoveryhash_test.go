package auth

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// Recovery codes are stored as salted argon2id PHC strings, not a fast hash
// (foreman verification notes, M2-W1A).
func TestRecoveryCodesAreHashedWithArgon2id(t *testing.T) {
	h1, h2 := HashRecoveryCode("01J9ZZ0000000000000000AAAA", "abcde-fghjk"), HashRecoveryCode("01J9ZZ0000000000000000AAAA", "abcde-fghjk")
	if !strings.HasPrefix(h1, "$argon2id$v=19$m=8192,t=1,p=1$") || h1 == h2 {
		t.Fatalf("hash format or salt: %s / %s", h1, h2)
	}
	if !verifyRecoveryCode("01J9ZZ0000000000000000AAAA", "ABCDE FGHJK", h1) {
		t.Fatal("a code typed in upper case with a space must verify")
	}
	if verifyRecoveryCode("01J9ZZ0000000000000000BBBB", "abcde-fghjk", h1) {
		t.Fatal("the hash is bound to its account")
	}
	if verifyRecoveryCode("01J9ZZ0000000000000000AAAA", "abcde-fghjm", h1) {
		t.Fatal("a wrong code verified")
	}
	// The rows written by an enrollment are argon2id too.
	e := newEnv(t)
	e.addAdmin(t, "ow", "owner", "right horse")
	login(t, e, "ow", "right horse")
	n := 0
	if err := e.st.Read(context.Background(), func(tx *store.Tx) error {
		cs, err := tx.UnusedRecoveryCodes(mustAdmin(t, e, "ow").ID)
		for _, c := range cs {
			if strings.HasPrefix(c.CodeHash, "$argon2id$") {
				n++
			}
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if n != RecoveryCodeCount {
		t.Fatalf("%d of %d stored codes are argon2id", n, RecoveryCodeCount)
	}
}

// One recovery code used by several pending sessions at once signs in exactly one.
func TestConcurrentUseOfOneRecoveryCode(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.addAdmin(t, "ow", "owner", "right horse")
	login(t, e, "ow", "right horse")
	code := e.mfa.codes["ow"][5]
	const n = 4
	ps := make([]*Principal, n)
	for i := range ps {
		ps[i], _ = pendingOf(t, e, "ow", "right horse")
	}
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range ps {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, errs[i] = e.m.VerifyRecoveryLogin(ctx, ps[i], code, "web", "192.0.2.1", "")
		}(i)
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, ErrBadCode):
			t.Fatalf("unexpected: %v", err)
		}
	}
	if ok != 1 {
		t.Fatalf("%d sign-ins with one recovery code, want 1", ok)
	}
}
