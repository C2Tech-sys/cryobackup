// Package ulid is CryoBackup's in-house ULID generator (architecture §1.2:
// "an in-house 40-line ULID" is preferred to a dependency). A ULID is 26
// characters of Crockford base32: 48 bits of Unix milliseconds followed by 80
// bits of entropy, so identifiers sort by creation time and are unique without
// coordination. The schema stores every row id as such a string (replica/
// schema.sql, CHECK (length(id) = 26)).
//
// The package is standard library only and never reads the wall clock itself:
// a Generator is built from an injected clock and an entropy reader, so tests
// are deterministic. Generator is safe for concurrent use and monotonic: two
// identifiers produced in the same millisecond (or after the clock stepped
// backwards) still sort in generation order, by incrementing the previous
// entropy instead of drawing fresh bytes.
//
// Shared packages: the console store uses it for every primary key; the agent
// uses the same encoding for run ids, which the console treats as idempotency
// keys.
package ulid

import (
	"crypto/rand"
	"errors"
	"io"
	"strings"
	"sync"
	"time"
)

// Len is the length of an encoded ULID.
const Len = 26

// alphabet is Crockford's base32 without I, L, O and U.
const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// maxMillis is the largest timestamp a ULID can carry (48 bits).
const maxMillis = 1<<48 - 1

// ErrMillisOverflow is returned when the clock is beyond the year 10889, the
// last instant a 48-bit millisecond timestamp can name.
var ErrMillisOverflow = errors.New("ulid: timestamp does not fit in 48 bits")

// Generator produces ULIDs from an injected clock and entropy source.
type Generator struct {
	clock   func() time.Time
	entropy io.Reader

	mu     sync.Mutex
	lastMS uint64
	last   [10]byte
	primed bool
}

// NewGenerator returns a Generator. clock must not be nil (callers pass the
// store's Clock, tests a frozen one); a nil entropy reader means
// crypto/rand.Reader.
func NewGenerator(clock func() time.Time, entropy io.Reader) *Generator {
	if clock == nil {
		panic("ulid: NewGenerator requires a clock")
	}
	if entropy == nil {
		entropy = rand.Reader
	}
	return &Generator{clock: clock, entropy: entropy}
}

// New returns the next identifier. It fails only if the entropy reader fails
// or the clock is out of range; a caller treats either as a fatal condition,
// never as a reason to invent an identifier.
func (g *Generator) New() (string, error) {
	ms := g.clock().UnixMilli()
	if ms < 0 || uint64(ms) > maxMillis {
		return "", ErrMillisOverflow
	}
	now := uint64(ms)

	g.mu.Lock()
	defer g.mu.Unlock()

	switch {
	case !g.primed || now > g.lastMS:
		if _, err := io.ReadFull(g.entropy, g.last[:]); err != nil {
			return "", err
		}
		g.lastMS = now
		g.primed = true
	default:
		// Same millisecond, or the clock went backwards: stay on the last
		// timestamp and increment the entropy so order is preserved.
		if !incr(&g.last) {
			if g.lastMS == maxMillis {
				return "", ErrMillisOverflow
			}
			g.lastMS++
			if _, err := io.ReadFull(g.entropy, g.last[:]); err != nil {
				return "", err
			}
		}
	}
	return encode(g.lastMS, g.last), nil
}

// MustNew is New for callers that cannot recover (the entropy source of the
// operating system is gone); it panics on error.
func (g *Generator) MustNew() string {
	s, err := g.New()
	if err != nil {
		panic(err)
	}
	return s
}

// incr adds one to b as a big-endian integer and reports false on overflow.
func incr(b *[10]byte) bool {
	for i := len(b) - 1; i >= 0; i-- {
		b[i]++
		if b[i] != 0 {
			return true
		}
	}
	return false
}

// encode renders the 128-bit value ms<<80 | entropy as 26 characters.
func encode(ms uint64, e [10]byte) string {
	var out [Len]byte
	// 10 characters of timestamp (48 bits, the first character carries 3).
	out[0] = alphabet[(ms>>45)&0x07]
	out[1] = alphabet[(ms>>40)&0x1f]
	out[2] = alphabet[(ms>>35)&0x1f]
	out[3] = alphabet[(ms>>30)&0x1f]
	out[4] = alphabet[(ms>>25)&0x1f]
	out[5] = alphabet[(ms>>20)&0x1f]
	out[6] = alphabet[(ms>>15)&0x1f]
	out[7] = alphabet[(ms>>10)&0x1f]
	out[8] = alphabet[(ms>>5)&0x1f]
	out[9] = alphabet[ms&0x1f]
	// 16 characters of entropy (80 bits).
	out[10] = alphabet[(e[0]&0xf8)>>3]
	out[11] = alphabet[((e[0]&0x07)<<2)|((e[1]&0xc0)>>6)]
	out[12] = alphabet[(e[1]&0x3e)>>1]
	out[13] = alphabet[((e[1]&0x01)<<4)|((e[2]&0xf0)>>4)]
	out[14] = alphabet[((e[2]&0x0f)<<1)|((e[3]&0x80)>>7)]
	out[15] = alphabet[(e[3]&0x7c)>>2]
	out[16] = alphabet[((e[3]&0x03)<<3)|((e[4]&0xe0)>>5)]
	out[17] = alphabet[e[4]&0x1f]
	out[18] = alphabet[(e[5]&0xf8)>>3]
	out[19] = alphabet[((e[5]&0x07)<<2)|((e[6]&0xc0)>>6)]
	out[20] = alphabet[(e[6]&0x3e)>>1]
	out[21] = alphabet[((e[6]&0x01)<<4)|((e[7]&0xf0)>>4)]
	out[22] = alphabet[((e[7]&0x0f)<<1)|((e[8]&0x80)>>7)]
	out[23] = alphabet[(e[8]&0x7c)>>2]
	out[24] = alphabet[((e[8]&0x03)<<3)|((e[9]&0xe0)>>5)]
	out[25] = alphabet[e[9]&0x1f]
	return string(out[:])
}

// decodeChar returns the 5-bit value of c, or -1.
func decodeChar(c byte) int {
	i := strings.IndexByte(alphabet, c)
	if i >= 0 {
		return i
	}
	// Crockford allows lower case; the application only ever writes upper
	// case, but a lower-case id from an agent still parses.
	if c >= 'a' && c <= 'z' {
		return strings.IndexByte(alphabet, c-'a'+'A')
	}
	return -1
}

// Valid reports whether s is a well-formed ULID: 26 Crockford characters whose
// timestamp fits in 48 bits (the first character is at most '7').
func Valid(s string) bool {
	if len(s) != Len {
		return false
	}
	for i := 0; i < Len; i++ {
		v := decodeChar(s[i])
		if v < 0 || (i == 0 && v > 7) {
			return false
		}
	}
	return true
}

// Time returns the creation time encoded in s (UTC, millisecond precision).
func Time(s string) (time.Time, bool) {
	if !Valid(s) {
		return time.Time{}, false
	}
	var ms uint64
	for i := 0; i < 10; i++ {
		ms = ms<<5 | uint64(decodeChar(s[i]))
	}
	return time.UnixMilli(int64(ms)).UTC(), true
}
