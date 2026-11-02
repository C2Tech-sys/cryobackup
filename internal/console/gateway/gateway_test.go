package gateway

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

// fakeHash returns a string with the exact shape of a bcrypt hash without
// paying for a real one; c picks the filler so hashes differ.
func fakeHash(c byte) string {
	return "$2y$10$" + strings.Repeat(string(c), 53)
}

var (
	hA = fakeHash('A')
	hB = fakeHash('B')
	hC = fakeHash('C')
	hP = fakeHash('P')
)

var goldenBody = "01HZZZZZZZZZZZZZZZZZZZZZZA:" + hA + "\n" +
	"01HZZZZZZZZZZZZZZZZZZZZZZB:" + hB + "\n" +
	"probe:" + hP + "\n"

// goldenSHA is the sha256 of goldenBody, written out literally so a change to
// the body rules is caught.
var goldenSHA = "a77c0fe15ca37b10c0ba8c2ec1415f1331431327190231db2390e1ad3a57fe27"

func TestRenderGolden(t *testing.T) {
	// Input deliberately unsorted; probe must slot in alphabetically.
	got, err := RenderE([]Login{
		{"01HZZZZZZZZZZZZZZZZZZZZZZB", hB},
		{"01HZZZZZZZZZZZZZZZZZZZZZZA", hA},
	}, hP, 7)
	if err != nil {
		t.Fatal(err)
	}
	want := "# cryobackup generation 7 sha256=" + goldenSHA + "\n" + goldenBody
	if string(got) != want {
		t.Fatalf("render mismatch\n got: %q\nwant: %q", got, want)
	}
	sum := sha256.Sum256([]byte(goldenBody))
	if hex.EncodeToString(sum[:]) != goldenSHA {
		t.Fatalf("goldenSHA constant is stale: body hashes to %x", sum)
	}
	if !bytes.Equal(Render([]Login{{"01HZZZZZZZZZZZZZZZZZZZZZZA", hA}, {"01HZZZZZZZZZZZZZZZZZZZZZZB", hB}}, hP, 7), got) {
		t.Fatal("Render differs from RenderE")
	}
}

func TestRenderOrderIndependent(t *testing.T) {
	a, _ := RenderE([]Login{{"x", hA}, {"y", hB}, {"z", hC}}, hP, 1)
	b, _ := RenderE([]Login{{"z", hC}, {"x", hA}, {"y", hB}}, hP, 1)
	if !bytes.Equal(a, b) {
		t.Fatal("render depends on input order")
	}
}

func TestParseRoundTrip(t *testing.T) {
	in := []Login{{"dev-2", hB}, {"dev-1", hA}}
	content, err := RenderE(in, hP, 42)
	if err != nil {
		t.Fatal(err)
	}
	gen, sha, logins, err := Parse(content)
	if err != nil {
		t.Fatal(err)
	}
	if gen != 42 {
		t.Fatalf("generation %d", gen)
	}
	body := content[bytes.IndexByte(content, '\n')+1:]
	if s := sha256.Sum256(body); sha != hex.EncodeToString(s[:]) {
		t.Fatalf("sha %s", sha)
	}
	want := []Login{{"dev-1", hA}, {"dev-2", hB}, {ProbeUsername, hP}}
	if fmt.Sprint(logins) != fmt.Sprint(want) {
		t.Fatalf("logins %v want %v", logins, want)
	}
	if !bytes.Equal(Render(logins[:2], hP, 42), content) {
		t.Fatal("re-render of parsed logins differs")
	}
}

func TestParseRejectsCorruption(t *testing.T) {
	good, _ := RenderE([]Login{{"dev-1", hA}}, hP, 3)
	cases := map[string][]byte{
		"empty":          nil,
		"no header":      []byte(goldenBody),
		"no newline":     []byte("# cryobackup generation 1 sha256=" + goldenSHA),
		"edited body":    bytes.Replace(good, []byte("dev-1"), []byte("dev-9"), 1),
		"truncated":      good[:len(good)-5],
		"bad generation": bytes.Replace(good, []byte("generation 3"), []byte("generation x"), 1),
		"neg generation": bytes.Replace(good, []byte("generation 3"), []byte("generation -3"), 1),
		"upper sha":      bytes.Replace(good, []byte("sha256="), []byte("sha256=ABCDEF"), 1),
	}
	for name, c := range cases {
		if _, _, _, err := Parse(c); !errors.Is(err, ErrFormat) {
			t.Errorf("%s: err = %v, want ErrFormat", name, err)
		}
	}
	// A header that matches its body but has a malformed line is still rejected.
	body := "no-colon-here\n"
	sum := sha256.Sum256([]byte(body))
	bad := "# cryobackup generation 1 sha256=" + hex.EncodeToString(sum[:]) + "\n" + body
	if _, _, _, err := Parse([]byte(bad)); !errors.Is(err, ErrFormat) {
		t.Errorf("malformed line: err = %v", err)
	}
}

func TestRenderRejectsInvalid(t *testing.T) {
	bad := []struct {
		name   string
		logins []Login
		probe  string
		gen    int64
	}{
		{"colon in username", []Login{{"a:b", hA}}, hP, 1},
		{"newline in username", []Login{{"a\nb", hA}}, hP, 1},
		{"carriage return", []Login{{"a\rb", hA}}, hP, 1},
		{"space in username", []Login{{"a b", hA}}, hP, 1},
		{"tab in username", []Login{{"a\tb", hA}}, hP, 1},
		{"nul in username", []Login{{"a\x00b", hA}}, hP, 1},
		{"comment username", []Login{{"#a", hA}}, hP, 1},
		{"empty username", []Login{{"", hA}}, hP, 1},
		{"invalid utf8", []Login{{"a\xffb", hA}}, hP, 1},
		{"reserved probe", []Login{{ProbeUsername, hA}}, hP, 1},
		{"duplicate", []Login{{"a", hA}, {"a", hB}}, hP, 1},
		{"md5 hash", []Login{{"a", "$apr1$abcdefgh$abcdefghijklmnopqrstuv"}}, hP, 1},
		{"plaintext hash", []Login{{"a", "hunter2"}}, hP, 1},
		{"empty hash", []Login{{"a", ""}}, hP, 1},
		{"hash with newline", []Login{{"a", hA[:59] + "\n"}}, hP, 1},
		{"hash with colon", []Login{{"a", hA[:59] + ":"}}, hP, 1},
		{"short hash", []Login{{"a", hA[:59]}}, hP, 1},
		{"bad version", []Login{{"a", "$2z$10$" + strings.Repeat("A", 53)}}, hP, 1},
		{"cost zero", []Login{{"a", "$2y$00$" + strings.Repeat("A", 53)}}, hP, 1},
		{"bad probe hash", nil, "nope", 1},
		{"empty probe hash", nil, "", 1},
		{"negative generation", nil, hP, -1},
	}
	for _, c := range bad {
		if out, err := RenderE(c.logins, c.probe, c.gen); err == nil || out != nil {
			t.Errorf("%s: RenderE accepted invalid input", c.name)
		}
		if Render(c.logins, c.probe, c.gen) != nil {
			t.Errorf("%s: Render returned content for invalid input", c.name)
		}
	}
	if _, err := RenderE(nil, hP, 0); err != nil {
		t.Errorf("probe-only render at generation 0: %v", err)
	}
}

func TestHashPasswordAndCheck(t *testing.T) {
	h, err := HashPassword("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$2y$10$") || len(h) != 60 || !validHash(h) {
		t.Fatalf("hash form %q", h)
	}
	if !CheckPassword(h, "correct horse") {
		t.Fatal("correct password rejected")
	}
	if CheckPassword(h, "correct horsf") || CheckPassword(h, "") {
		t.Fatal("wrong password accepted")
	}
	if cost, _ := bcrypt.Cost([]byte(h)); cost != 10 {
		t.Fatalf("cost %d", cost)
	}
	// The $2a$ spelling of the same hash verifies too.
	if !CheckPassword("$2a$"+h[4:], "correct horse") {
		t.Fatal("$2a$ spelling does not verify")
	}
	if CheckPassword("garbage", "x") || CheckPassword("", "x") {
		t.Fatal("garbage hash matched")
	}
	h2, _ := HashPassword("correct horse")
	if h == h2 {
		t.Fatal("hash is not salted")
	}
	if _, err := HashPassword(""); err == nil {
		t.Fatal("empty password hashed")
	}
}

type fixedReader struct{ b byte }

func (f fixedReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = f.b
	}
	return len(p), nil
}

func TestNewPassword(t *testing.T) {
	p, err := NewPassword(fixedReader{0xab})
	if err != nil {
		t.Fatal(err)
	}
	if p != strings.Repeat("ab", 32) {
		t.Fatalf("got %q", p)
	}
	a, _ := NewPassword(nil)
	b, _ := NewPassword(nil)
	if len(a) != 64 || a == b || strings.Trim(a, "0123456789abcdef") != "" {
		t.Fatalf("crypto/rand passwords %q %q", a, b)
	}
	if _, err := NewPassword(strings.NewReader("short")); err == nil {
		t.Fatal("short read accepted")
	}
}

func TestProbePassword(t *testing.T) {
	key := bytes.Repeat([]byte{1}, 32)
	a, err := ProbePassword(key)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := ProbePassword(append([]byte(nil), key...))
	if a != b {
		t.Fatal("not deterministic")
	}
	if len(a) != 64 || strings.Trim(a, "0123456789abcdef") != "" {
		t.Fatalf("shape %q", a)
	}
	other, _ := ProbePassword(bytes.Repeat([]byte{2}, 32))
	if other == a {
		t.Fatal("different keys gave the same password")
	}
	// Pinned vector: HKDF-SHA256(ikm = 32 x 0x01, salt = nil, info = "cryobackup/probe/v1").
	// Changing it silently would lock the prober out after an upgrade.
	const vector = "7184df07847e362d53d3fec5c56791212ed247cb3123fdf9b6ec4162ad91df6c"
	if a != vector {
		t.Fatalf("probe password vector changed: %s", a)
	}
	if _, err := ProbePassword(nil); err == nil {
		t.Fatal("nil key accepted")
	}
	if _, err := ProbePassword([]byte("short")); err == nil {
		t.Fatal("short key accepted")
	}
	h, err := HashPassword(a)
	if err != nil || !CheckPassword(h, a) {
		t.Fatalf("probe password does not survive HashPassword: %v", err)
	}
}

func render(t *testing.T, gen int64, users ...string) []byte {
	t.Helper()
	logins := make([]Login, 0, len(users))
	for _, u := range users {
		logins = append(logins, Login{u, hA})
	}
	b, err := RenderE(logins, hP, gen)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSyncWritesFile(t *testing.T) {
	dir := t.TempDir()
	content := render(t, 1, "dev-1")
	if err := Sync(dir, content); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("read back: %v", err)
	}
	if runtime.GOOS != "windows" {
		st, _ := os.Stat(filepath.Join(dir, FileName))
		if st.Mode().Perm() != 0o640 {
			t.Fatalf("mode %v", st.Mode().Perm())
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("temp files left behind: %v", entries)
	}
	gen, sha, err := Current(dir)
	if err != nil || gen != 1 {
		t.Fatalf("Current: %d %v", gen, err)
	}
	_, wantSHA, _, _ := Parse(content)
	if sha != wantSHA {
		t.Fatalf("Current sha %s want %s", sha, wantSHA)
	}
}

func TestSyncRefusesInvalidContent(t *testing.T) {
	dir := t.TempDir()
	good := render(t, 1, "dev-1")
	if err := Sync(dir, good); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string][]byte{"nil": nil, "empty": {}, "garbage": []byte("x:y\n"), "truncated": good[:len(good)-3]} {
		if err := Sync(dir, c); err == nil {
			t.Errorf("%s: Sync accepted invalid content", name)
		}
	}
	got, _ := os.ReadFile(filepath.Join(dir, FileName))
	if !bytes.Equal(got, good) {
		t.Fatal("a refused Sync changed the deployed file")
	}
	if err := Sync(filepath.Join(dir, "missing"), good); err == nil {
		t.Fatal("Sync into a missing directory succeeded")
	}
}

func TestCurrentAbsentAndCorrupt(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := Current(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("absent: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte("whatever\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Current(dir); !errors.Is(err, ErrFormat) {
		t.Fatalf("corrupt: %v", err)
	}
}

// TestSyncAtomicUnderReader: a reader loops while a writer syncs 200 times; the
// reader must never see a missing, empty, truncated or mixed file.
func TestSyncAtomicUnderReader(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	if err := Sync(dir, render(t, 0, "seed")); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var reads, openErrs int
	var bad error
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			b, err := os.ReadFile(path)
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					bad = fmt.Errorf("file vanished: %w", err)
					return
				}
				openErrs++ // Windows sharing violation during replace: no content seen
				continue
			}
			reads++
			if len(b) == 0 {
				bad = errors.New("empty file observed")
				return
			}
			if _, _, _, err := Parse(b); err != nil {
				bad = fmt.Errorf("partial or mixed file observed (%d bytes): %w", len(b), err)
				return
			}
		}
	}()
	for gen := int64(1); gen <= 200; gen++ {
		users := make([]string, 0, 300)
		for i := 0; i < 50+int(gen%5)*60; i++ {
			users = append(users, fmt.Sprintf("device-%04d-%d", i, gen))
		}
		if err := Sync(dir, render(t, gen, users...)); err != nil {
			close(stop)
			wg.Wait()
			t.Fatalf("sync %d: %v", gen, err)
		}
	}
	close(stop)
	wg.Wait()
	if bad != nil {
		t.Fatal(bad)
	}
	t.Logf("reader completed %d clean reads, %d transient open errors", reads, openErrs)
	if reads == 0 {
		t.Fatal("reader never read the file")
	}
	if gen, _, err := Current(dir); err != nil || gen != 200 {
		t.Fatalf("final generation %d %v", gen, err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("temp files left behind: %d entries", len(entries))
	}
}

func TestSyncIfOwner(t *testing.T) {
	dir := t.TempDir()
	// Absent file: proceed.
	if err := SyncIfOwner(dir, render(t, 5, "a"), 5); err != nil {
		t.Fatalf("absent: %v", err)
	}
	// Equal generation: proceed.
	if err := SyncIfOwner(dir, render(t, 5, "a", "b"), 5); err != nil {
		t.Fatalf("equal: %v", err)
	}
	// Older deployed: proceed.
	if err := SyncIfOwner(dir, render(t, 6, "a", "b", "c"), 6); err != nil {
		t.Fatalf("older: %v", err)
	}
	deployed, _ := os.ReadFile(filepath.Join(dir, FileName))
	// Newer deployed: refuse and leave the file untouched.
	err := SyncIfOwner(dir, render(t, 5, "x"), 5)
	if !errors.Is(err, ErrNewerGeneration) {
		t.Fatalf("newer: err = %v, want ErrNewerGeneration", err)
	}
	after, _ := os.ReadFile(filepath.Join(dir, FileName))
	if !bytes.Equal(deployed, after) {
		t.Fatal("refused SyncIfOwner changed the file")
	}
	// Content generation must match the owner's.
	if err := SyncIfOwner(dir, render(t, 9, "x"), 8); err == nil || errors.Is(err, ErrNewerGeneration) {
		t.Fatalf("mismatched generation: %v", err)
	}
	// Invalid content refused.
	if err := SyncIfOwner(dir, nil, 7); err == nil {
		t.Fatal("nil content accepted")
	}
	// A corrupt deployed file is healed, not treated as newer.
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte("garbage"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := SyncIfOwner(dir, render(t, 1, "a"), 1); err != nil {
		t.Fatalf("corrupt deployed: %v", err)
	}
}

func TestDrifted(t *testing.T) {
	dir := t.TempDir()
	want := render(t, 3, "a", "b")
	if d, err := Drifted(dir, want); err != nil || !d {
		t.Fatalf("absent file: drifted=%v err=%v", d, err)
	}
	if err := Sync(dir, want); err != nil {
		t.Fatal(err)
	}
	if d, err := Drifted(dir, want); err != nil || d {
		t.Fatalf("identical: drifted=%v err=%v", d, err)
	}
	// Same body at another generation is not drift.
	if d, err := Drifted(dir, render(t, 4, "a", "b")); err != nil || d {
		t.Fatalf("generation only: drifted=%v err=%v", d, err)
	}
	// A different login set is drift.
	if d, err := Drifted(dir, render(t, 3, "a", "b", "c")); err != nil || !d {
		t.Fatalf("different logins: drifted=%v err=%v", d, err)
	}
	// A hand edit that keeps the header is drift.
	edited := bytes.Replace(want, []byte("a:"), []byte("z:"), 1)
	if err := os.WriteFile(filepath.Join(dir, FileName), edited, 0o640); err != nil {
		t.Fatal(err)
	}
	if d, err := Drifted(dir, want); err != nil || !d {
		t.Fatalf("hand edit: drifted=%v err=%v", d, err)
	}
	// Garbage and empty files are drift.
	for _, c := range []string{"", "garbage"} {
		if err := os.WriteFile(filepath.Join(dir, FileName), []byte(c), 0o640); err != nil {
			t.Fatal(err)
		}
		if d, err := Drifted(dir, want); err != nil || !d {
			t.Fatalf("%q: drifted=%v err=%v", c, d, err)
		}
	}
	if _, err := Drifted(dir, []byte("not a rendering")); err == nil {
		t.Fatal("invalid expected content accepted")
	}
}
