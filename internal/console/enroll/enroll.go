// Package enroll mints and recognises the secrets of enrollment (architecture
// §5.1, §8.2): enrollment tokens (customer-scoped, expiring, use-capped) and
// device tokens (one per device, rotatable). Both are 32 random bytes, sent as
// base64url, stored only as the lower-case hex SHA-256 of what was sent. The
// enroll flow itself (the /enroll handler) lives in internal/console/agentapi;
// this package is what it and `cryo-console token` share.
package enroll

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// TokenLen is the length of a token in its base64url form (32 bytes, unpadded).
const TokenLen = 43

// NewToken returns a fresh secret and the hash to store.
func NewToken() (plain, hash string, err error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", "", fmt.Errorf("enroll: reading random bytes: %w", err)
	}
	plain = base64.RawURLEncoding.EncodeToString(b[:])
	return plain, HashToken(plain), nil
}

// HashToken returns the lower-case hex SHA-256 of a token as presented.
func HashToken(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// WellFormed reports whether s has the shape of a token: 43 base64url
// characters. It lets the API reject garbage before touching the database.
func WellFormed(s string) bool {
	if len(s) != TokenLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// MintOptions parameterise Mint.
type MintOptions struct {
	Label     string
	PolicyID  string // a template or a policy of the same customer; applied at enrollment as the new device's override
	Expires   time.Duration
	MaxUses   *int // nil = unlimited
	CreatedBy string
}

// Mint creates an enrollment token for the customer and returns the secret,
// which is shown once and never stored.
func Mint(ctx context.Context, st *store.Store, scope store.Scope, customerID string, o MintOptions) (string, store.EnrollmentToken, error) {
	if o.Expires <= 0 {
		o.Expires = 7 * 24 * time.Hour
	}
	plain, hash, err := NewToken()
	if err != nil {
		return "", store.EnrollmentToken{}, err
	}
	tok, err := st.CreateEnrollmentToken(ctx, scope, store.EnrollmentToken{
		CustomerID: customerID, Label: o.Label, TokenHash: hash, PolicyID: o.PolicyID,
		ExpiresAt: st.Now().Add(o.Expires), MaxUses: o.MaxUses, CreatedBy: o.CreatedBy,
	})
	if err != nil {
		return "", store.EnrollmentToken{}, err
	}
	return plain, tok, nil
}

// ParseDuration reads durations such as "90m", "12h", "7d" and "2w" (days and
// weeks are accepted on top of time.ParseDuration).
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty duration")
	}
	unit := s[len(s)-1]
	if unit == 'd' || unit == 'w' {
		n, err := strconv.Atoi(s[:len(s)-1])
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		d := time.Duration(n) * 24 * time.Hour
		if unit == 'w' {
			d *= 7
		}
		return d, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("invalid duration %q", s)
	}
	return d, nil
}
