package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // RFC 6238 TOTP is HMAC-SHA-1; every authenticator app implements exactly this
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"
)

// TOTP parameters (RFC 6238 with the defaults every authenticator app uses):
// HMAC-SHA-1, 6 digits, a 30-second step, and codes accepted from the step
// before to the step after the current one (clock drift of up to 30 s either
// way). The secret is 20 random bytes (160 bits, the RFC 4226 recommendation).
const (
	TOTPDigits    = 6
	TOTPPeriod    = 30 * time.Second
	TOTPSkew      = 1
	totpSecretLen = 20
	// ContextTOTP is the escrow wrap context of admin_users.totp_secret_wrapped
	// (architecture §8.2). The row id is the admin id, the version 1.
	ContextTOTP = "cryobackup/totp/v1"
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewTOTPSecret returns a fresh random TOTP secret.
func NewTOTPSecret() ([]byte, error) {
	b := make([]byte, totpSecretLen)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return b, nil
}

// TOTPSecretText is the secret as an authenticator app takes it: unpadded
// base32 (RFC 4648), upper case.
func TOTPSecretText(secret []byte) string { return b32.EncodeToString(secret) }

// TOTPSecretGroups is TOTPSecretText in groups of four for reading aloud or
// typing ("ABCD EFGH ...").
func TOTPSecretGroups(secret []byte) string {
	s := TOTPSecretText(secret)
	var parts []string
	for len(s) > 4 {
		parts = append(parts, s[:4])
		s = s[4:]
	}
	return strings.Join(append(parts, s), " ")
}

// TOTPStep is the RFC 6238 time step of t.
func TOTPStep(t time.Time) int64 { return t.Unix() / int64(TOTPPeriod/time.Second) }

// TOTPCodeAt returns the code of secret for a time step (RFC 4226 HOTP with
// dynamic truncation).
func TOTPCodeAt(secret []byte, step int64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(step))
	m := hmac.New(sha1.New, secret)
	m.Write(msg[:])
	sum := m.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	bin := (uint32(sum[off])&0x7f)<<24 | uint32(sum[off+1])<<16 | uint32(sum[off+2])<<8 | uint32(sum[off+3])
	code := strconv.FormatUint(uint64(bin%1_000_000), 10)
	return strings.Repeat("0", TOTPDigits-len(code)) + code
}

// TOTPCode returns the code of secret at time t.
func TOTPCode(secret []byte, t time.Time) string { return TOTPCodeAt(secret, TOTPStep(t)) }

// normalizeCode strips spaces and dashes a user may type ("123 456").
func normalizeCode(code string) string {
	return strings.Map(func(r rune) rune {
		if r == ' ' || r == '-' || r == '\t' {
			return -1
		}
		return r
	}, code)
}

// matchTOTP compares code with the codes of the steps now-1, now and now+1, in
// constant time for each (every candidate is always compared), and returns the
// step that matched. A step at or before notAfter is never accepted (the
// replay guard's last used step; pass -1 for none).
func matchTOTP(secret []byte, code string, now time.Time, notAfter int64) (int64, bool) {
	code = normalizeCode(code)
	if len(code) != TOTPDigits {
		return 0, false
	}
	cur := TOTPStep(now)
	var found int64
	ok := 0
	for d := int64(-TOTPSkew); d <= TOTPSkew; d++ {
		st := cur + d
		eq := subtle.ConstantTimeCompare([]byte(TOTPCodeAt(secret, st)), []byte(code))
		if eq == 1 && st > notAfter && ok == 0 {
			found, ok = st, 1
		}
	}
	return found, ok == 1
}

// OTPAuthURI is the otpauth:// URI an authenticator app scans (the Key Uri
// Format). issuer and account are display labels; the secret is in the query.
// The URI carries the secret: it is rendered into the page once (as a QR) and
// never logged.
func OTPAuthURI(issuer, account string, secret []byte) string {
	label := url.PathEscape(issuer) + ":" + url.PathEscape(account)
	q := url.Values{}
	q.Set("secret", TOTPSecretText(secret))
	q.Set("issuer", issuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", strconv.Itoa(TOTPDigits))
	q.Set("period", strconv.Itoa(int(TOTPPeriod/time.Second)))
	return "otpauth://totp/" + label + "?" + q.Encode()
}

// ---- recovery codes ----

// RecoveryCodeCount is how many single-use recovery codes an enrollment gets.
const RecoveryCodeCount = 10

// recoveryAlphabet has no look-alike characters (no 0/o, 1/l/i).
const recoveryAlphabet = "abcdefghjkmnpqrstuvwxyz23456789"

// NewRecoveryCodes returns n codes of the form "xxxxx-xxxxx" (ten characters
// from a 31-letter alphabet: about 49 bits each).
func NewRecoveryCodes(n int) ([]string, error) {
	out := make([]string, 0, n)
	for len(out) < n {
		b := make([]byte, 10)
		if _, err := rand.Read(b); err != nil {
			return nil, err
		}
		var sb strings.Builder
		for i, c := range b {
			if i == 5 {
				sb.WriteByte('-')
			}
			// 256 is not a multiple of 31; the bias (at most 1/256 per character) is
			// irrelevant at 49 bits and an attempt limit of a few tries.
			sb.WriteByte(recoveryAlphabet[int(c)%len(recoveryAlphabet)])
		}
		out = append(out, sb.String())
	}
	return out, nil
}

// normalizeRecovery lower-cases a code and removes separators, so "ABCDE-FGHJK",
// "abcde fghjk" and "abcdefghjk" are the same code.
func normalizeRecovery(code string) string { return strings.ToLower(normalizeCode(code)) }

// Recovery codes are hashed with argon2id, a deliberately slow KDF, so a copy
// of the database does not give the codes away to a cheap offline search
// (each code has about 49 bits). The setting is lighter than the password one
// (t=1, 8 MiB, p=1, 16-byte salt, 32-byte key) because a login with a recovery
// code verifies it against every unused code of the account (up to ten, all of
// them every time, so the time does not tell which one matched). The stored
// form is the same PHC string as a password hash, and VerifyPassword checks it.
// Before M2-W1A shipped the codes were SHA-256; no production code was ever
// issued with that format, so there is nothing to migrate.
const (
	recoveryTime    = 1
	recoveryMemKiB  = 8 * 1024
	recoveryThreads = 1
)

// recoveryInput binds a code to its account, normalised.
func recoveryInput(adminID, code string) string {
	return "cryobackup/recovery/v1|" + adminID + "|" + normalizeRecovery(code)
}

// HashRecoveryCode is the stored form of a recovery code: an argon2id PHC
// string with a fresh salt over the account id and the normalised code.
func HashRecoveryCode(adminID, code string) string {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		// The OS random source does not fail on a supported platform; a hash
		// that can never verify is the safe answer if it ever does.
		return "!"
	}
	key := argon2.IDKey([]byte(recoveryInput(adminID, code)), salt, recoveryTime, recoveryMemKiB, recoveryThreads, keyLen)
	return encode(recoveryMemKiB, recoveryTime, recoveryThreads, salt, key)
}

// verifyRecoveryCode checks a code against one stored hash in constant time.
func verifyRecoveryCode(adminID, code, stored string) bool {
	ok, _ := VerifyPassword(stored, recoveryInput(adminID, code))
	return ok
}
