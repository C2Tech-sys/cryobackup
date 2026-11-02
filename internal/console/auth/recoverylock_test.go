package auth

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// The argon2id verification of a recovery code holds no database write lock:
// while a slow verifier runs, an unrelated console write (here an audit row,
// standing in for an agent check-in) completes at once (foreman verification
// notes, M2-W1A).
func TestRecoveryHashingDoesNotHoldTheWriteLock(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.addAdmin(t, "ow", "owner", "right horse")
	login(t, e, "ow", "right horse")
	code := e.mfa.codes["ow"][2]
	p, _ := pendingOf(t, e, "ow", "right horse")

	const perCode = 150 * time.Millisecond // ten codes: about 1.5 s of "hashing"
	started := make(chan struct{})
	var once sync.Once
	recoveryVerifier = func(adminID, c, stored string) bool {
		once.Do(func() { close(started) })
		time.Sleep(perCode)
		return verifyRecoveryCode(adminID, c, stored)
	}
	t.Cleanup(func() { recoveryVerifier = verifyRecoveryCode })

	done := make(chan error, 1)
	go func() {
		_, _, err := e.m.VerifyRecoveryLogin(ctx, p, code, "web", "192.0.2.1", "")
		done <- err
	}()
	<-started
	t0 := time.Now()
	if err := e.st.Write(ctx, func(tx *store.Tx) error {
		_, err := tx.AppendAudit(store.AuditEvent{ActorType: "system", Action: "test.unrelated_write", Result: "ok"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	took := time.Since(t0)
	select {
	case err := <-done:
		t.Fatalf("the recovery login finished before the unrelated write was measured (err %v); the stub is too fast", err)
	default:
	}
	if took > 5*perCode {
		t.Fatalf("an unrelated write waited %v behind the recovery-code hashing", took)
	}
	if err := <-done; err != nil {
		t.Fatalf("recovery login: %v", err)
	}
}
