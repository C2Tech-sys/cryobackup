package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

// The argon2id parameters of ticket P1-T5: t=3, m=64 MiB, p=4, 16-byte salt,
// 32-byte key, encoded as a PHC string
// ($argon2id$v=19$m=65536,t=3,p=4$<salt>$<hash>, unpadded standard base64).
const (
	argonTime    = 3
	argonMemKiB  = 64 * 1024
	argonThreads = 4
	saltLen      = 16
	keyLen       = 32

	// Upper bounds accepted when verifying a stored hash, so a corrupted or
	// hostile row cannot make a login attempt allocate gigabytes.
	maxMemKiB  = 1 << 20
	maxTime    = 16
	maxThreads = 64
)

var b64 = base64.RawStdEncoding

// HashPassword returns the argon2id PHC string of plain with a fresh salt.
func HashPassword(plain string) (string, error) {
	if plain == "" {
		return "", errors.New("auth: empty password")
	}
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: reading random bytes: %w", err)
	}
	key := argon2.IDKey([]byte(plain), salt, argonTime, argonMemKiB, argonThreads, keyLen)
	return encode(argonMemKiB, argonTime, argonThreads, salt, key), nil
}

func encode(mem, t uint32, p uint8, salt, key []byte) string {
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, mem, t, p, b64.EncodeToString(salt), b64.EncodeToString(key))
}

type phc struct {
	mem, time uint32
	threads   uint8
	salt, key []byte
}

func parsePHC(s string) (phc, bool) {
	parts := strings.Split(s, "$")
	// "", "argon2id", "v=19", "m=..,t=..,p=..", salt, key
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return phc{}, false
	}
	if parts[2] != "v="+strconv.Itoa(argon2.Version) {
		return phc{}, false
	}
	var out phc
	seen := 0
	for _, kv := range strings.Split(parts[3], ",") {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return phc{}, false
		}
		n, err := strconv.ParseUint(v, 10, 32)
		if err != nil {
			return phc{}, false
		}
		switch k {
		case "m":
			out.mem, seen = uint32(n), seen|1
		case "t":
			out.time, seen = uint32(n), seen|2
		case "p":
			if n > 255 {
				return phc{}, false
			}
			out.threads, seen = uint8(n), seen|4
		default:
			return phc{}, false
		}
	}
	if seen != 7 || out.mem < 8 || out.mem > maxMemKiB || out.time < 1 || out.time > maxTime || out.threads < 1 || out.threads > maxThreads {
		return phc{}, false
	}
	salt, err1 := b64.DecodeString(parts[4])
	key, err2 := b64.DecodeString(parts[5])
	if err1 != nil || err2 != nil || len(salt) < 8 || len(key) < 16 || len(key) > 128 {
		return phc{}, false
	}
	out.salt, out.key = salt, key
	return out, true
}

// VerifyPassword checks plain against an argon2id PHC string in constant time.
// A malformed hash, the unusable marker "!" and an empty password all fail.
// needsRehash is true when the hash is valid but uses weaker parameters than
// the current ones, so the caller can upgrade it after a successful login.
func VerifyPassword(encoded, plain string) (ok, needsRehash bool) {
	h, valid := parsePHC(encoded)
	if !valid || plain == "" {
		return false, false
	}
	got := argon2.IDKey([]byte(plain), h.salt, h.time, h.mem, h.threads, uint32(len(h.key)))
	if subtle.ConstantTimeCompare(got, h.key) != 1 {
		return false, false
	}
	return true, h.mem < argonMemKiB || h.time < argonTime || len(h.key) < keyLen
}

// dummyHash is a constant, pre-computed argon2id PHC string at the current
// parameters. It is verified against when the username is unknown, the account
// is disabled or locked, so the time of a failed login does not reveal which
// case it was. It protects nothing and matches no known password. A test pins
// its parameters to the live ones so it cannot drift.
const dummyHash = "$argon2id$v=19$m=65536,t=3,p=4$EGoF+wVnA6w1iPiFBDXWYQ$r8uAnK+6X6D9K+dFmeEl5TWXKCFc+ZcWfSMxukiZGQg"

// BurnPassword spends the time of one verification.
func BurnPassword(plain string) { VerifyPassword(dummyHash, plain) }

// GeneratePassword returns a random password of n characters from an alphabet
// without look-alike characters, for init and `user add`.
func GeneratePassword(n int) (string, error) {
	const alphabet = "abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	if n < 12 {
		n = 12
	}
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	out := make([]byte, n)
	for i, c := range b {
		out[i] = alphabet[int(c)%len(alphabet)]
	}
	return string(out), nil
}
