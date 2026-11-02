package store

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/policy"
)

// t0 is the frozen "now" of the tests. It is far from the real clock so that a
// stray time.Now() in the store shows up as a wrong timestamp.
var t0 = time.Date(2026, 10, 22, 13, 42, 10, 0, time.UTC)

// fakeClock is an injectable, advanceable Clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock(t time.Time) *fakeClock { return &fakeClock{t: t} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// newStore opens a private store on a frozen clock.
func newStore(t *testing.T) (*Store, *fakeClock) {
	t.Helper()
	clk := newFakeClock(t0)
	s, err := OpenMemory(context.Background(), clk.Now)
	if err != nil {
		t.Fatalf("OpenMemory: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, clk
}

// world is a seeded store: one default storage target and policy, an owner
// admin, two customers (A and B) each with a token and one enrolled device.
type world struct {
	t      *testing.T
	s      *Store
	clk    *fakeClock
	ctx    context.Context
	target StorageTarget
	admin  Admin
	custA  Customer
	custB  Customer
	tokA   EnrollmentToken
	tokB   EnrollmentToken
	devA   Device
	devB   Device
}

// must adapts a (value, error) call for use in a test: must[T](t)(f()).
func must[T any](t *testing.T) func(T, error) T {
	return func(v T, err error) T {
		t.Helper()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		return v
	}
}

func ok(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// seed builds the standard two-customer world through the public API.
func seed(t *testing.T) *world {
	t.Helper()
	s, clk := newStore(t)
	ctx := context.Background()
	w := &world{t: t, s: s, clk: clk, ctx: ctx}
	w.target = must[StorageTarget](t)(s.CreateStorageTarget(ctx, StorageTarget{
		Name: "Example Storage", Provider: "wasabi", Endpoint: "s3.example-storage.invalid", Bucket: "cryo-test", Prefix: "fleet",
		GatewayURL: "https://gateway.example.invalid:9443", CredentialRef: "CRYO_TEST_S3", IsDefault: true,
	}))
	secs := must[map[string]json.RawMessage](t)(PolicySections(policy.Default(policy.ClassUnknown)))
	must[PolicyRecord](t)(s.CreatePolicy(ctx, ScopeAll(), PolicyInput{Scope: "default", Name: "Default", Sections: secs}))
	w.admin = must[Admin](t)(s.CreateAdmin(ctx, Admin{Username: "root", Email: "root@example.invalid", Role: "owner", PasswordHash: "$argon2id$test"}))
	w.custA = must[Customer](t)(s.CreateCustomer(ctx, ScopeAll(), Customer{Name: "Alpha Dental"}))
	w.custB = must[Customer](t)(s.CreateCustomer(ctx, ScopeAll(), Customer{Name: "Bravo Legal"}))
	w.tokA = must[EnrollmentToken](t)(s.CreateEnrollmentToken(ctx, ScopeAll(), EnrollmentToken{CustomerID: w.custA.ID, TokenHash: sha256Hex("tok-a"), ExpiresAt: t0.Add(24 * time.Hour)}))
	w.tokB = must[EnrollmentToken](t)(s.CreateEnrollmentToken(ctx, ScopeAll(), EnrollmentToken{CustomerID: w.custB.ID, TokenHash: sha256Hex("tok-b"), ExpiresAt: t0.Add(24 * time.Hour)}))
	w.devA = w.enroll("tok-a", "A-DESK-1").Device
	w.devB = w.enroll("tok-b", "B-DESK-1").Device
	return w
}

// enrollInput builds a complete EnrollInput for a host, with deterministic fake secrets.
func enrollInput(token, host string) EnrollInput {
	return EnrollInput{
		TokenHash: sha256Hex(token), Nonce: "nonce-" + host, Hostname: host, OSVersion: "Windows 11 Pro", Arch: "amd64",
		DeviceClass: "desktop", AgentVersion: "1.4.2", ResticVersion: "0.19.1",
		DeviceTokenHash:     sha256Hex("device-token-" + host),
		GatewayPasswordHash: fixtureBcrypt, GatewaySecretWrapped: []byte("wrapped-gateway-" + host),
		RepoKeyWrapped: []byte("wrapped-repo-" + host), WrapAlg: "aes-256-gcm+hkdf-sha256/v1", MasterKeyID: "test-key",
	}
}

func (w *world) enroll(token, host string) EnrollResult {
	w.t.Helper()
	return must[EnrollResult](w.t)(w.s.AgentEnroll(w.ctx, enrollInput(token, host)))
}

// heartbeat reports the device as seen "now" and idle, which also confirms enrollment.
func (w *world) heartbeat(deviceID string) {
	w.t.Helper()
	ok(w.t, w.s.AgentUpsertPresence(w.ctx, deviceID, Presence{ReceivedAt: w.clk.Now(), ServiceState: "idle", AgentVersion: "1.4.2"}))
}

// runInput builds a finished successful backup run ending at end (device time).
func runInput(id string, end time.Time) RunInput {
	fin := end
	return RunInput{
		ID: id, Kind: "backup", Trigger: "schedule", StartedAt: end.Add(-10 * time.Minute), FinishedAt: &fin, Outcome: "success",
		Stats: []byte(`{"total_files_processed":1000,"failed_files":0}`),
	}
}

// ids are the 26-character identifiers the agent would generate.
const (
	runID1 = "01J9Y4K3A0B1C2D3E4F5G6H7J1"
	runID2 = "01J9Y4K3A0B1C2D3E4F5G6H7J2"
	runID3 = "01J9Y4K3A0B1C2D3E4F5G6H7J3"
)
