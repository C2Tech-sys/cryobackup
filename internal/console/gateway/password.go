package gateway

import (
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

// ProbeUsername is the reserved htpasswd username used by the health prober.
// It is not a ULID, so it cannot collide with a device login, and it is never
// stored in gateway_logins.
const ProbeUsername = "probe"

const (
	// bcryptCost is the work factor for every gateway password (architecture 7.2).
	bcryptCost = 10
	// probeInfo is the HKDF info string that domain-separates the probe password.
	probeInfo = "cryobackup/probe/v1"
	// minMasterKeyLen is the shortest master key ProbePassword accepts.
	minMasterKeyLen = 16
)

// Login is one htpasswd entry. Hash is a bcrypt string as stored in
// gateway_logins.password_hash.
type Login struct {
	Username string
	Hash     string
}

// HashPassword bcrypts plain at cost 10 and returns the "$2y$" form that
// htpasswd tools and rclone expect. Go's bcrypt emits "$2a$"; the two differ
// only in the version byte, and Go's CheckPassword accepts both. See doc.go
// for the live rclone result.
func HashPassword(plain string) (string, error) {
	if plain == "" {
		return "", errors.New("gateway: empty password")
	}
	h, err := bcrypt.GenerateFromPassword([]byte(plain), bcryptCost)
	if err != nil {
		return "", fmt.Errorf("gateway: bcrypt: %w", err)
	}
	s := string(h)
	if !strings.HasPrefix(s, "$2a$") {
		return "", fmt.Errorf("gateway: unexpected bcrypt prefix %q", s[:min(4, len(s))])
	}
	return "$2y$" + s[4:], nil
}

// CheckPassword reports whether plain matches the bcrypt hash. An empty
// password, a malformed hash or any other error is a non-match.
func CheckPassword(hash, plain string) bool {
	if plain == "" || !validHash(hash) {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain)) == nil
}

// NewPassword returns 32 random bytes from rand as 64 lowercase hex characters
// (hex needs no URL escaping, DH3). A nil rand means crypto/rand.Reader.
func NewPassword(r io.Reader) (string, error) {
	if r == nil {
		r = rand.Reader
	}
	var b [32]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return "", fmt.Errorf("gateway: reading random bytes: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// ProbePassword derives the probe login's plaintext password from the console
// master key: HKDF-SHA256 with no salt and info "cryobackup/probe/v1", 32
// bytes, hex encoded. It is deterministic, never stored, and the master key
// must be at least 16 bytes.
func ProbePassword(masterKey []byte) (string, error) {
	if len(masterKey) < minMasterKeyLen {
		return "", fmt.Errorf("gateway: master key must be at least %d bytes", minMasterKeyLen)
	}
	k, err := hkdf.Key(sha256.New, masterKey, nil, probeInfo, 32)
	if err != nil {
		return "", fmt.Errorf("gateway: hkdf: %w", err)
	}
	return hex.EncodeToString(k), nil
}

// validHash reports whether h has the exact shape of a bcrypt string:
// "$2<a|b|x|y>$<2-digit cost>$" followed by 53 characters of the bcrypt
// base64 alphabet (60 bytes total). It never contains ':' or whitespace, so it
// is safe on an htpasswd line.
func validHash(h string) bool {
	if len(h) != 60 || h[0] != '$' || h[1] != '2' || h[3] != '$' || h[6] != '$' {
		return false
	}
	switch h[2] {
	case 'a', 'b', 'x', 'y':
	default:
		return false
	}
	if h[4] < '0' || h[4] > '3' || h[5] < '0' || h[5] > '9' {
		return false
	}
	cost := int(h[4]-'0')*10 + int(h[5]-'0')
	if cost < bcrypt.MinCost || cost > bcrypt.MaxCost {
		return false
	}
	for i := 7; i < 60; i++ {
		c := h[i]
		ok := c == '.' || c == '/' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
		if !ok {
			return false
		}
	}
	return true
}
