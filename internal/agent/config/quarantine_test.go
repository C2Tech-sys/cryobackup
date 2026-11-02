package config

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// plant builds an untrusted tree as a squatter would leave it: a layout
// directory or two with files the agent would otherwise read.
func plant(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{
		filepath.Join("config", "agent.json"):         `{"server_url":"https://attacker.example","device_id":"01J00000000000000000000000"}`,
		filepath.Join("secrets", "device.token"):      "planted",
		filepath.Join("cache", "restic", "deep", "x"): "planted",
	}
	for rel, body := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return files
}

var stamp = time.Date(2026, 10, 22, 15, 30, 0, 0, time.UTC)

// stubSuffixes replaces the random source of quarantine names with the given
// suffixes, in order; it returns the number drawn so far.
func stubSuffixes(t *testing.T, suffixes ...string) *int {
	t.Helper()
	old := quarantineSuffix
	t.Cleanup(func() { quarantineSuffix = old })
	n := 0
	quarantineSuffix = func() (string, error) {
		if n >= len(suffixes) {
			t.Errorf("more than %d quarantine names drawn", len(suffixes))
			return "ffffffff", nil
		}
		n++
		return suffixes[n-1], nil
	}
	return &n
}

// mustBeEmptyDir fails unless d is a directory nothing was moved into.
func mustBeEmptyDir(t *testing.T, d string) {
	t.Helper()
	entries, err := os.ReadDir(d)
	if err != nil || len(entries) != 0 {
		t.Errorf("%s was written to or removed: %d entries, %v", filepath.Base(d), len(entries), err)
	}
}

func TestQuarantineRootMovesTheWholeTreeAside(t *testing.T) {
	stubSuffixes(t, "0a1b2c3d")
	l := Layout{Root: filepath.Join(t.TempDir(), "CryoBackup")}
	files := plant(t, l.Root)
	q, err := QuarantineRoot(l.Root, stamp)
	if err != nil {
		t.Fatalf("QuarantineRoot: %v", err)
	}
	want := filepath.Join(filepath.Dir(l.Root), "CryoBackup.quarantine-20261022T153000Z-0a1b2c3d")
	if q.Path != want || l.QuarantinePath(stamp.In(time.FixedZone("x", 3600)), "0a1b2c3d") != want {
		t.Errorf("quarantine path = %s, want %s (UTC stamp, random suffix)", q.Path, want)
	}
	if _, err := os.Lstat(l.Root); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the root is still there: %v", err)
	}
	for rel, body := range files {
		b, err := os.ReadFile(filepath.Join(q.Path, rel))
		if err != nil || string(b) != body {
			t.Errorf("%s was not moved intact: %q %v", rel, b, err)
		}
	}
	if !CanRestrict() && q.SecureErr == nil {
		t.Error("an unelevated quarantine reports its owner and DACL as set")
	}
	if got := l.Quarantines(); len(got) != 1 || got[0] != "20261022T153000Z-0a1b2c3d" {
		t.Errorf("Quarantines = %v", got)
	}
}

// The real random source: 8 lower-case hex digits, different on every draw.
func TestQuarantineNameIsUnpredictable(t *testing.T) {
	l := Layout{Root: filepath.Join(t.TempDir(), "CryoBackup")}
	plant(t, l.Root)
	q, err := QuarantineRoot(l.Root, stamp)
	if err != nil {
		t.Fatalf("QuarantineRoot: %v", err)
	}
	if !regexp.MustCompile(`^CryoBackup\.quarantine-20261022T153000Z-[0-9a-f]{8}$`).MatchString(filepath.Base(q.Path)) {
		t.Errorf("quarantine name %s is not <name>.quarantine-<UTC stamp>-<8 hex>", filepath.Base(q.Path))
	}
	a, err1 := quarantineSuffix()
	b, err2 := quarantineSuffix()
	if err1 != nil || err2 != nil || a == b {
		t.Errorf("two draws: %q %q (%v %v)", a, b, err1, err2)
	}
}

// Ticket P1-FIX10 item 2: a directory squatting the predictable name (the
// stamp alone, the shape before FIX10) no longer blocks a repair.
func TestQuarantineRootIgnoresASquattedStampName(t *testing.T) {
	l := Layout{Root: filepath.Join(t.TempDir(), "CryoBackup")}
	plant(t, l.Root)
	squatted := filepath.Join(filepath.Dir(l.Root), "CryoBackup.quarantine-"+stamp.Format(QuarantineStamp))
	if err := os.Mkdir(squatted, 0o700); err != nil {
		t.Fatal(err)
	}
	q, err := QuarantineRoot(l.Root, stamp)
	if err != nil {
		t.Fatalf("a squatted stamp name blocked the quarantine: %v", err)
	}
	if q.Path == squatted {
		t.Fatalf("the tree was moved onto the squatted name")
	}
	if _, err := os.Stat(filepath.Join(q.Path, "config", "agent.json")); err != nil {
		t.Errorf("the tree is not in %s: %v", filepath.Base(q.Path), err)
	}
	mustBeEmptyDir(t, squatted)
}

// A taken name is retried with a fresh suffix; the taken one is not touched.
func TestQuarantineRootRetriesATakenName(t *testing.T) {
	n := stubSuffixes(t, "aaaaaaaa", "bbbbbbbb")
	l := Layout{Root: filepath.Join(t.TempDir(), "CryoBackup")}
	plant(t, l.Root)
	taken := l.QuarantinePath(stamp, "aaaaaaaa")
	if err := os.Mkdir(taken, 0o700); err != nil {
		t.Fatal(err)
	}
	q, err := QuarantineRoot(l.Root, stamp)
	if err != nil || q.Path != l.QuarantinePath(stamp, "bbbbbbbb") || *n != 2 {
		t.Fatalf("QuarantineRoot: %+v %v after %d names", q, err, *n)
	}
	mustBeEmptyDir(t, taken)
}

// Five taken names exhaust the retries: ErrQuarantineExists, nothing moved,
// nothing written into any of them.
func TestQuarantineRootExhaustsItsRetries(t *testing.T) {
	suffixes := []string{"00000001", "00000002", "00000003", "00000004", "00000005"}
	n := stubSuffixes(t, suffixes...)
	l := Layout{Root: filepath.Join(t.TempDir(), "CryoBackup")}
	plant(t, l.Root)
	for _, s := range suffixes {
		if err := os.Mkdir(l.QuarantinePath(stamp, s), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	_, err := QuarantineRoot(l.Root, stamp)
	if !errors.Is(err, ErrQuarantineExists) {
		t.Fatalf("five taken names: %v, want ErrQuarantineExists", err)
	}
	if *n != QuarantineAttempts || QuarantineAttempts != 5 {
		t.Errorf("%d names drawn, want %d (QuarantineAttempts = %d)", *n, 5, QuarantineAttempts)
	}
	if strings.Contains(err.Error(), filepath.Dir(l.Root)) {
		t.Errorf("the error names the path: %v", err)
	}
	if _, err := os.Stat(l.ConfigFile()); err != nil {
		t.Errorf("the root was changed: %v", err)
	}
	for _, s := range suffixes {
		mustBeEmptyDir(t, l.QuarantinePath(stamp, s))
	}
}

func TestQuarantineRootRefusesReparsePoints(t *testing.T) {
	base := t.TempDir()
	elsewhere := filepath.Join(base, "elsewhere")
	if err := os.Mkdir(elsewhere, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Run("inside the tree", func(t *testing.T) {
		l := Layout{Root: filepath.Join(base, "CryoBackup")}
		plant(t, l.Root)
		link(t, elsewhere, filepath.Join(l.Root, "journal"))
		_, err := QuarantineRoot(l.Root, stamp)
		if !errors.Is(err, ErrReparsePoint) {
			t.Fatalf("a junction inside: %v, want ErrReparsePoint", err)
		}
		if strings.Contains(err.Error(), base) {
			t.Errorf("the error names the path: %v", err)
		}
		if _, err := os.Stat(l.ConfigFile()); err != nil {
			t.Errorf("the tree was moved anyway: %v", err)
		}
		if q := l.Quarantines(); len(q) != 0 {
			t.Errorf("a quarantine appeared: %v", q)
		}
	})
	t.Run("the root", func(t *testing.T) {
		root := filepath.Join(base, "rootlink")
		link(t, elsewhere, root)
		if _, err := QuarantineRoot(root, stamp); !errors.Is(err, ErrReparsePoint) {
			t.Fatalf("a junction root: %v, want ErrReparsePoint", err)
		}
		if _, err := os.Lstat(root); err != nil {
			t.Errorf("the link was moved: %v", err)
		}
	})
	if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
		t.Errorf("the link target was written to: %d entries", len(entries))
	}
}

// Quarantines lists the new shape and the pre-FIX10 one, oldest first, and
// leaves out every other name.
func TestQuarantinesParsesTheNameShapes(t *testing.T) {
	l := Layout{Root: filepath.Join(t.TempDir(), "CryoBackup")}
	for _, n := range []string{
		"CryoBackup.quarantine-20261022T153000Z-0a1b2c3d",
		"CryoBackup.quarantine-20261021T090000Z",
		"CryoBackup.quarantine-20261023T000000Z-ffffffff",
		"CryoBackup.quarantine-garbage",
		"CryoBackup.quarantine-20261022T153001Z-0A1B2C3D", // upper-case hex (another second: NTFS names are case-insensitive)
		"CryoBackup.quarantine-20261022T153000Z-0a1b2c3",
		"CryoBackup.quarantine-20261022T153000Z-0a1b2c3dx",
		"CryoBackup.quarantine-",
		"Other.quarantine-20261022T153000Z-0a1b2c3d",
	} {
		if err := os.Mkdir(filepath.Join(filepath.Dir(l.Root), n), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	got := strings.Join(l.Quarantines(), ",")
	want := "20261021T090000Z,20261022T153000Z-0a1b2c3d,20261023T000000Z-ffffffff"
	if got != want {
		t.Errorf("Quarantines = %s, want %s", got, want)
	}
}

// Even with the lab flag (nothing can be restricted), a root or child whose
// owner is another account is refused before anything is created in it.
func TestEnsureLabRefusesAForeignOwner(t *testing.T) {
	foreign := func(p string) error { return ErrForeignOwner }
	notElevated := func() bool { return false }

	l := Layout{Root: filepath.Join(t.TempDir(), "CryoBackup")}
	if err := os.Mkdir(l.Root, 0o700); err != nil {
		t.Fatal(err)
	}
	ran := false
	_, err := l.Ensure(EnsureOptions{Restrict: func(string) error { ran = true; return ErrACLNotApplied }, elevated: notElevated, labOwner: foreign})
	if !errors.Is(err, ErrForeignOwner) {
		t.Fatalf("a foreign-owned root in a lab run: %v, want ErrForeignOwner", err)
	}
	if ran {
		t.Error("the restrict hook ran on a foreign-owned root")
	}
	for _, d := range l.Children() {
		if _, err := os.Lstat(d); err == nil {
			t.Errorf("%s was created inside a foreign-owned root", l.rel(d))
		}
	}

	// A foreign-owned child of this user's root: refused as well.
	l2 := Layout{Root: filepath.Join(t.TempDir(), "CryoBackup")}
	if err := os.MkdirAll(l2.SecretsDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	childForeign := func(p string) error {
		if p == l2.SecretsDir() {
			return ErrForeignOwner
		}
		return nil
	}
	if _, err := l2.Ensure(EnsureOptions{elevated: notElevated, labOwner: childForeign}); !errors.Is(err, ErrForeignOwner) {
		t.Fatalf("a foreign-owned child in a lab run: %v, want ErrForeignOwner", err)
	}

	// This user's own tree is fine.
	l3 := Layout{Root: filepath.Join(t.TempDir(), "CryoBackup")}
	if err := os.Mkdir(l3.Root, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := l3.Ensure(EnsureOptions{elevated: notElevated}); err != nil {
		t.Fatalf("this user's own root: %v", err)
	}
}
