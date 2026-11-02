package gateway

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

const headerPrefix = "# cryobackup generation "
const headerSHAKey = " sha256="

// ErrFormat is wrapped by every error Parse and Current return for a file that
// is not a well-formed CryoBackup htpasswd file.
var ErrFormat = errors.New("gateway: malformed htpasswd file")

// Render is RenderE for callers that have already validated their input. On
// invalid input it returns nil; never write that to disk (Sync rejects it).
func Render(logins []Login, probeHash string, generation int64) []byte {
	b, err := RenderE(logins, probeHash, generation)
	if err != nil {
		return nil
	}
	return b
}

// RenderE renders the htpasswd file: the header line
//
//	# cryobackup generation <n> sha256=<hex of the body>
//
// then one "<username>:<hash>\n" line per login, sorted by username in byte
// order, with the probe login included. The body is everything after the
// header line. It rejects an invalid or duplicate username, the reserved
// "probe" username in logins, a negative generation, and any hash that is not
// a bcrypt string.
func RenderE(logins []Login, probeHash string, generation int64) ([]byte, error) {
	if generation < 0 {
		return nil, errors.New("gateway: negative generation")
	}
	if !validHash(probeHash) {
		return nil, errors.New("gateway: probe hash is not a bcrypt string")
	}
	all := make([]Login, 0, len(logins)+1)
	for _, l := range logins {
		if l.Username == ProbeUsername {
			return nil, fmt.Errorf("gateway: username %q is reserved", ProbeUsername)
		}
		all = append(all, l)
	}
	all = append(all, Login{Username: ProbeUsername, Hash: probeHash})
	if err := checkLogins(all); err != nil {
		return nil, err
	}
	slices.SortFunc(all, func(a, b Login) int { return strings.Compare(a.Username, b.Username) })

	var body bytes.Buffer
	for _, l := range all {
		body.WriteString(l.Username)
		body.WriteByte(':')
		body.WriteString(l.Hash)
		body.WriteByte('\n')
	}
	var out bytes.Buffer
	out.WriteString(headerPrefix)
	out.WriteString(strconv.FormatInt(generation, 10))
	out.WriteString(headerSHAKey)
	out.WriteString(bodySHA(body.Bytes()))
	out.WriteByte('\n')
	out.Write(body.Bytes())
	return out.Bytes(), nil
}

// Parse reads a file produced by Render. It returns the generation, the body
// hash, and every login line including the reserved probe login. It fails with
// ErrFormat when the header is missing or malformed, when the header's sha256
// does not match the body, or when any line is invalid.
func Parse(content []byte) (generation int64, bodySHAHex string, logins []Login, err error) {
	gen, sha, body, err := splitHeader(content)
	if err != nil {
		return 0, "", nil, err
	}
	if got := bodySHA(body); got != sha {
		return 0, "", nil, fmt.Errorf("%w: header sha256 %s does not match body %s", ErrFormat, sha, got)
	}
	if len(body) > 0 && body[len(body)-1] != '\n' {
		return 0, "", nil, fmt.Errorf("%w: body does not end in a newline", ErrFormat)
	}
	for _, line := range strings.Split(strings.TrimSuffix(string(body), "\n"), "\n") {
		if line == "" {
			continue
		}
		user, hash, ok := strings.Cut(line, ":")
		if !ok {
			return 0, "", nil, fmt.Errorf("%w: line without ':'", ErrFormat)
		}
		logins = append(logins, Login{Username: user, Hash: hash})
	}
	if err := checkLogins(logins); err != nil {
		return 0, "", nil, fmt.Errorf("%w: %v", ErrFormat, err)
	}
	return gen, sha, logins, nil
}

// splitHeader splits content into the header's generation and sha256 and the
// body. It does not verify the sha256 against the body.
func splitHeader(content []byte) (gen int64, sha string, body []byte, err error) {
	line, body, ok := bytes.Cut(content, []byte("\n"))
	if !ok {
		return 0, "", nil, fmt.Errorf("%w: no header line", ErrFormat)
	}
	rest, ok := strings.CutPrefix(string(line), headerPrefix)
	if !ok {
		return 0, "", nil, fmt.Errorf("%w: header does not start with %q", ErrFormat, strings.TrimSpace(headerPrefix))
	}
	genStr, sha, ok := strings.Cut(rest, headerSHAKey)
	if !ok || genStr == "" || len(genStr) > 18 || len(sha) != 64 {
		return 0, "", nil, fmt.Errorf("%w: bad header", ErrFormat)
	}
	for i := 0; i < len(genStr); i++ {
		if genStr[i] < '0' || genStr[i] > '9' {
			return 0, "", nil, fmt.Errorf("%w: generation is not a number", ErrFormat)
		}
	}
	if _, err := hex.DecodeString(sha); err != nil || sha != strings.ToLower(sha) {
		return 0, "", nil, fmt.Errorf("%w: sha256 is not lowercase hex", ErrFormat)
	}
	gen, err = strconv.ParseInt(genStr, 10, 64)
	if err != nil {
		return 0, "", nil, fmt.Errorf("%w: generation: %v", ErrFormat, err)
	}
	return gen, sha, body, nil
}

func bodySHA(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// checkLogins validates every username and hash and rejects duplicates.
func checkLogins(logins []Login) error {
	seen := make(map[string]struct{}, len(logins))
	for _, l := range logins {
		if err := checkUsername(l.Username); err != nil {
			return err
		}
		if !validHash(l.Hash) {
			return fmt.Errorf("gateway: hash for %q is not a bcrypt string", l.Username)
		}
		if _, dup := seen[l.Username]; dup {
			return fmt.Errorf("gateway: duplicate username %q", l.Username)
		}
		seen[l.Username] = struct{}{}
	}
	return nil
}

// checkUsername rejects anything that could break out of an htpasswd line:
// empty, ':' , any control character or space, invalid UTF-8, and a leading
// '#', which would make the line a comment.
func checkUsername(u string) error {
	if u == "" {
		return errors.New("gateway: empty username")
	}
	if !utf8.ValidString(u) {
		return errors.New("gateway: username is not valid UTF-8")
	}
	if u[0] == '#' {
		return errors.New("gateway: username starts with '#'")
	}
	for _, r := range u {
		if r == ':' || r <= ' ' || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return errors.New("gateway: username contains a forbidden character")
		}
	}
	return nil
}
