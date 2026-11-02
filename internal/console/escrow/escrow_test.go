package escrow

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/ulid"
)

func seqKey(t *testing.T) *MasterKey {
	t.Helper()
	b := make([]byte, 32)
	for i := range b {
		b[i] = byte(i)
	}
	m, err := NewMasterKey(b)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

const (
	vecDevice = "01J9ZZ0000000000000000ABCD"
	vecPlain  = "correct horse battery staple 0123456789abcdef"
	// Computed independently with Python: HKDF-SHA256 (hmac/hashlib) over master
	// bytes 0..31, info "cryobackup/escrow/v1/<device>/1", then AES-256-GCM from the
	// "cryptography" package with nonce bytes 100..111 and AAD "<device>|1".
	vecBlob = "6465666768696a6b6c6d6e6f127ffeb35f5e9d31253694cc15a7aa4aebe2b200e7aa9bb2d92b3d834c0bdaffdbfe58dd107dce89adb766c6187c8a0e0f22d9bf6cb61b249b159264e8"
)

func TestKnownVectors(t *testing.T) {
	m := seqKey(t)
	if got := m.KeyID(); got != "630dcd2966c43366" {
		t.Fatalf("KeyID = %s", got)
	}
	k, err := m.derive(infoFor(ContextRepoKey, vecDevice, 1))
	if err != nil || hex.EncodeToString(k) != "d77ac55a98da92fb2d6f3e92ca4a590e1bbac8f2a5ca64a29ffa49cc3746b8c4" {
		t.Fatalf("HKDF key = %x %v", k, err)
	}
	blob, _ := hex.DecodeString(vecBlob)
	pt, err := m.UnwrapRepoKey(vecDevice, 1, blob)
	if err != nil || string(pt) != vecPlain {
		t.Fatalf("known blob did not decrypt: %q %v", pt, err)
	}
	if WrapAlg != "aes-256-gcm+hkdf-sha256/v1" {
		t.Fatal("wrap_alg changed")
	}
}

func TestRoundTripAndFreshNonces(t *testing.T) {
	m := seqKey(t)
	a, err := m.WrapRepoKey(vecDevice, 1, []byte(vecPlain))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := m.WrapRepoKey(vecDevice, 1, []byte(vecPlain))
	if bytes.Equal(a, b) {
		t.Fatal("two wraps of the same plaintext must differ (random nonce)")
	}
	if len(a) != 12+len(vecPlain)+16 {
		t.Fatalf("blob layout: %d bytes", len(a))
	}
	pt, err := m.UnwrapRepoKey(vecDevice, 1, a)
	if err != nil || string(pt) != vecPlain {
		t.Fatalf("round trip: %q %v", pt, err)
	}
}

func TestTamperAndContextBinding(t *testing.T) {
	m := seqKey(t)
	blob, _ := hex.DecodeString(vecBlob)
	flip := func(i int) []byte { c := append([]byte(nil), blob...); c[i] ^= 1; return c }
	cases := map[string]func() error{
		"nonce flip":         func() error { _, err := m.UnwrapRepoKey(vecDevice, 1, flip(0)); return err },
		"ciphertext flip":    func() error { _, err := m.UnwrapRepoKey(vecDevice, 1, flip(20)); return err },
		"tag flip":           func() error { _, err := m.UnwrapRepoKey(vecDevice, 1, flip(len(blob)-1)); return err },
		"truncated":          func() error { _, err := m.UnwrapRepoKey(vecDevice, 1, blob[:20]); return err },
		"empty":              func() error { _, err := m.UnwrapRepoKey(vecDevice, 1, nil); return err },
		"other device (AAD)": func() error { _, err := m.UnwrapRepoKey("01J9ZZ0000000000000000ABCE", 1, blob); return err },
		"other version":      func() error { _, err := m.UnwrapRepoKey(vecDevice, 2, blob); return err },
		"other context":      func() error { _, err := m.Unwrap(ContextGatewaySecret, vecDevice, 1, blob); return err },
		"other master key": func() error {
			o, _ := NewMasterKey(bytes.Repeat([]byte{9}, 32))
			_, err := o.UnwrapRepoKey(vecDevice, 1, blob)
			return err
		},
	}
	for name, f := range cases {
		if err := f(); !errors.Is(err, ErrDecrypt) {
			t.Errorf("%s: want ErrDecrypt, got %v", name, err)
		}
	}
}

func TestNoKey(t *testing.T) {
	var m *MasterKey
	if m.Loaded() || m.KeyID() != "" || m.Bytes() != nil {
		t.Fatal("nil key must look empty")
	}
	if _, err := m.WrapRepoKey("d", 1, []byte("x")); !errors.Is(err, ErrMasterKeyUnavailable) {
		t.Fatalf("got %v", err)
	}
	if _, err := (&MasterKey{}).UnwrapRepoKey("d", 1, make([]byte, 40)); !errors.Is(err, ErrMasterKeyUnavailable) {
		t.Fatalf("got %v", err)
	}
	if _, err := (&Keys{}).Unwrap(context.Background(), "d"); !errors.Is(err, ErrMasterKeyUnavailable) {
		t.Fatalf("got %v", err)
	}
}

func TestKeyFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "master.key")
	m, err := GenerateMasterKey(p, nil)
	if err != nil || !m.Loaded() {
		t.Fatal(err)
	}
	if _, err := GenerateMasterKey(p, nil); err == nil || strings.Contains(err.Error(), dir) {
		t.Fatalf("second generate must fail without printing the path: %v", err)
	}
	l, err := LoadMasterKey(p)
	if err != nil || l.KeyID() != m.KeyID() {
		t.Fatalf("load: %v", err)
	}
	if _, err := LoadMasterKey(filepath.Join(dir, "missing")); !errors.Is(err, ErrMasterKeyUnavailable) {
		t.Fatalf("missing file: %v", err)
	}
	for name, body := range map[string]string{"not base64": "%%%%", "short": base64.StdEncoding.EncodeToString([]byte("short"))} {
		q := filepath.Join(dir, strings.ReplaceAll(name, " ", "_"))
		_ = os.WriteFile(q, []byte(body), 0o600)
		if _, err := LoadMasterKey(q); !errors.Is(err, ErrKeyFile) {
			t.Errorf("%s: %v", name, err)
		}
	}
	id := sha256.Sum256(m.Bytes())
	if m.KeyID() != hex.EncodeToString(id[:])[:16] {
		t.Fatal("KeyID must be hex(sha256(master))[:16]")
	}
}

func TestKeysUnwrapIsStrictAndDeviceBound(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	st, err := store.OpenMemory(ctx, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := store.LoadFixtures(ctx, st, now); err != nil {
		t.Fatal(err)
	}
	m := seqKey(t)
	// Use a fixture customer and mint a token for it.
	custs, _ := st.ListCustomers(ctx, store.ScopeAll())
	var cust store.Customer
	for _, c := range custs {
		if c.Name == "Northwind Dental" {
			cust = c
		}
	}
	sum := sha256.Sum256([]byte("tok"))
	if _, err := st.CreateEnrollmentToken(ctx, store.ScopeAll(), store.EnrollmentToken{CustomerID: cust.ID, TokenHash: hex.EncodeToString(sum[:]), ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	nonce := strings.Repeat("ab", 16)
	repo := strings.Repeat("0f", 32)
	// The enroll handler chooses the device id first and wraps under it.
	id := ulid.NewGenerator(func() time.Time { return now }, nil).MustNew()
	wrapped, _ := m.WrapRepoKey(id, 1, []byte(repo))
	gw, _ := m.Wrap(ContextGatewaySecret, id, 1, []byte("gwpass"))
	res, err := st.AgentEnroll(ctx, store.EnrollInput{TokenHash: hex.EncodeToString(sum[:]), Nonce: nonce, DeviceID: id, Hostname: "H1",
		DeviceTokenHash: strings.Repeat("a", 64), GatewayPasswordHash: "$2y$10$" + strings.Repeat("a", 53), GatewaySecretWrapped: gw,
		RepoKeyWrapped: wrapped, WrapAlg: WrapAlg, MasterKeyID: m.KeyID()})
	if err != nil {
		t.Fatal(err)
	}
	if res.Device.ID != id {
		t.Fatalf("device id %s, want the supplied %s", res.Device.ID, id)
	}
	keys := &Keys{Master: m, Store: st}
	got, err := keys.Unwrap(ctx, id)
	if err != nil || got != repo {
		t.Fatalf("unwrap: %q %v", got, err)
	}
	gs, err := keys.GatewaySecret(ctx, id)
	if err != nil || gs != "gwpass" {
		t.Fatalf("gateway secret: %q %v", gs, err)
	}
	// Strict: a row wrapped under any other id (the old provisional scheme) does not open.
	other2 := strings.Repeat("ab", 16)
	legacy, _ := m.WrapRepoKey("pending:"+other2, 1, []byte(repo))
	if _, err := m.UnwrapRepoKey(id, 1, legacy); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("a wrap under another id opened under the device id: %v", err)
	}
	// Wrong master key: the id check fires before any decryption.
	other, _ := NewMasterKey(bytes.Repeat([]byte{7}, 32))
	if _, err := (&Keys{Master: other, Store: st}).Unwrap(ctx, id); !errors.Is(err, ErrMasterKeyMismatch) {
		t.Fatalf("got %v", err)
	}
	// A customer-held device.
	pine, _ := st.ListDevices(ctx, store.ScopeAll(), store.ListDevicesOptions{Search: "LEGAL-PC-01"})
	if len(pine.Items) != 1 {
		t.Fatal("fixture device missing")
	}
	if _, err := keys.Unwrap(ctx, pine.Items[0].ID); !errors.Is(err, ErrCustomerHeld) {
		t.Fatalf("customer-held: %v", err)
	}
	if _, err := keys.Unwrap(ctx, "01J9ZZ0000000000000000ZZZZ"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown device: %v", err)
	}
}
