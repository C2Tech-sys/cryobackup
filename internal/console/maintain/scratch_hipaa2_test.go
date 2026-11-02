package maintain

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// rootOf is the filesystem root of the temp directory ("/" or `C:\`).
func rootOf() string { return filepath.VolumeName(os.TempDir()) + string(filepath.Separator) }

// TestScratchDirMayNotBeARootOrTopLevel (M1-HIPAA2 item 3): the worker empties
// <scratch>/tmp and <scratch>/rclone, so a filesystem root or a top-level
// directory is a configuration error; validation refuses it before anything
// on disk is touched. Deeper directories, the default included, are accepted.
func TestScratchDirMayNotBeARootOrTopLevel(t *testing.T) {
	root := rootOf()
	for _, bad := range []string{root, filepath.Join(root, "tmp"), filepath.Join(root, "var") + string(filepath.Separator), filepath.Join(root, "tmp", "..", "etc")} {
		_, err := Config{Bucket: "b", ScratchDir: bad}.withDefaults()
		if !errors.Is(err, errConfig) || !strings.Contains(err.Error(), "top-level") {
			t.Errorf("ScratchDir %q: err = %v, want a refusal", bad, err)
		}
	}
	for _, good := range []string{filepath.Join(root, "run", "cryo-maintain"), filepath.Join(root, "tmp", "cryo-maintain"), DefaultScratchDir(), t.TempDir()} {
		if _, err := (Config{Bucket: "b", ScratchDir: good}).withDefaults(); err != nil {
			t.Errorf("ScratchDir %q: %v", good, err)
		}
	}
}

// TestScratchDirMustBeTheWorkersOwn (M1-HIPAA2 item 3): a scratch directory
// owned by another user, or a symlink, is refused before its subdirectories
// are emptied; the worker's own directory is prepared.
func TestScratchDirMustBeTheWorkersOwn(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "scratch")
	if err := os.MkdirAll(filepath.Join(dir, "tmp"), 0o700); err != nil {
		t.Fatal(err)
	}
	canary := filepath.Join(dir, "tmp", "someone-elses-file")
	if err := os.WriteFile(canary, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	notMine := func(os.FileInfo) (bool, bool) { return false, true }
	if err := prepareScratch(dir, notMine); !errors.Is(err, errScratchNotOurs) {
		t.Fatalf("a directory of another user: %v", err)
	}
	if _, err := os.Stat(canary); err != nil {
		t.Fatal("a refused scratch directory was emptied")
	}
	mine := func(os.FileInfo) (bool, bool) { return true, true }
	if err := prepareScratch(dir, mine); err != nil {
		t.Fatalf("the worker's own directory: %v", err)
	}
	if _, err := os.Stat(canary); !os.IsNotExist(err) {
		t.Fatal("the worker's own tmp was not emptied")
	}
	unknown := func(os.FileInfo) (bool, bool) { return false, false }
	if err := prepareScratch(dir, unknown); err != nil {
		t.Fatalf("an owner that cannot be read skips the check: %v", err)
	}
	if runtime.GOOS != "windows" {
		link := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(dir, link); err != nil {
			t.Fatal(err)
		}
		if err := prepareScratch(link, mine); !errors.Is(err, errScratchNotOurs) {
			t.Fatalf("a symlink: %v", err)
		}
	}
}

// TestScratchDirIsCheckedBeforeItIsCreated (M1-HIPAA2 follow-up 5): a missing
// scratch directory is created only under a real directory. When the
// directory it would be created under is a symlink (it would land somewhere
// else) or a file, the worker refuses before creating anything.
func TestScratchDirIsCheckedBeforeItIsCreated(t *testing.T) {
	mine := func(os.FileInfo) (bool, bool) { return true, true }
	base := t.TempDir()

	file := filepath.Join(base, "a-file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Linux answers ENOTDIR here, Windows "path not found": both are a refusal.
	if err := prepareScratch(filepath.Join(file, "scratch"), mine); !errors.Is(err, errScratchNotOurs) {
		t.Errorf("under a file: %v", err)
	}

	elsewhere := filepath.Join(base, "elsewhere")
	if err := os.MkdirAll(filepath.Join(elsewhere, "real"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(elsewhere, link); err != nil {
		if runtime.GOOS == "linux" {
			t.Fatalf("the symlink cases must run on Linux: %v", err)
		}
		t.Logf("symlinks unavailable here (%v): the symlink cases run on Linux", err)
	} else {
		// The directory it would be created under is the symlink itself.
		if err := prepareScratch(filepath.Join(link, "deeper", "scratch"), mine); !errors.Is(err, errScratchSymlink) {
			t.Errorf("under a symlink: %v", err)
		}
		// A symlink higher up: link/real is a real directory, reached through link.
		if err := prepareScratch(filepath.Join(link, "real", "scratch"), mine); !errors.Is(err, errScratchSymlink) {
			t.Errorf("under a symlinked ancestor: %v", err)
		}
		// An existing directory reached through a symlink.
		if err := prepareScratch(filepath.Join(link, "real"), mine); !errors.Is(err, errScratchSymlink) {
			t.Errorf("an existing directory below a symlink: %v", err)
		}
		if entries, _ := os.ReadDir(filepath.Join(elsewhere, "real")); len(entries) != 0 {
			t.Errorf("a refused scratch directory was created or emptied through the symlink: %v", entries)
		}
		if entries, _ := os.ReadDir(elsewhere); len(entries) != 1 {
			t.Errorf("a refused scratch directory was created through the symlink: %v", entries)
		}
	}

	// A plain missing path is created, 0700, with its tmp and rclone.
	fresh := filepath.Join(base, "plain", "scratch")
	if err := prepareScratch(fresh, mine); err != nil {
		t.Fatalf("a missing directory under a real one: %v", err)
	}
	for _, sub := range []string{"tmp", "rclone"} {
		if fi, err := os.Lstat(filepath.Join(fresh, sub)); err != nil || !fi.IsDir() {
			t.Fatalf("%s: %v", sub, err)
		}
	}
}

// TestScratchDirOwnerIsReadOnLinux: the real owner check (Linux only) refuses a
// system directory owned by root when the tests do not run as root, or a
// directory handed to another uid when they do.
func TestScratchDirOwnerIsReadOnLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("POSIX ownership is read on Linux only")
	}
	var dir string
	if os.Geteuid() == 0 {
		dir = filepath.Join(t.TempDir(), "foreign")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(dir, 65534, 65534); err != nil {
			t.Skipf("cannot hand a directory to another uid: %v", err)
		}
	} else {
		for _, d := range []string{"/usr/share", "/usr/lib", "/var/lib"} {
			if fi, err := os.Lstat(d); err == nil && fi.IsDir() {
				dir = d
				break
			}
		}
		if dir == "" {
			t.Skip("no root-owned system directory to test against")
		}
	}
	if err := prepareScratch(dir, ownedByMe); !errors.Is(err, errScratchNotOurs) {
		t.Fatalf("%s: err = %v, want a refusal", dir, err)
	}
	if mine, known := ownedByMe(mustLstat(t, t.TempDir())); !known || !mine {
		t.Fatalf("the test's own temp directory: mine=%v known=%v", mine, known)
	}
}

func mustLstat(t *testing.T, p string) os.FileInfo {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi
}

// TestRunRefusesATopLevelScratchDir: `maintain --once` with
// CRYO_MAINTAIN_SCRATCH_DIR set to a top-level directory exits with the usage
// code and says why, without touching the directory.
func TestRunRefusesATopLevelScratchDir(t *testing.T) {
	dir := t.TempDir()
	exe, sum := fakeResticPath(t)
	cfg := filepath.Join(dir, "config.json")
	body := `{"db_path":` + jsonStr(filepath.Join(dir, "c.db")) + `,"master_key_path":` + jsonStr(filepath.Join(dir, "master.key")) + `}`
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRYO_MAINTAIN_RESTIC", exe)
	t.Setenv("CRYO_MAINTAIN_RESTIC_SHA256", sum)
	t.Setenv("CRYO_MAINTAIN_BUCKET", "bucket")
	t.Setenv("CRYO_MAINTAIN_SCRATCH_DIR", rootOf())
	keys := func(string, *store.Store) (KeyUnwrapper, error) { return &fakeKeys{pw: "x"}, nil }
	var out, errb bytes.Buffer
	if code := Run([]string{"--config", cfg, "--once"}, &out, &errb, keys); code != ExitUsage {
		t.Fatalf("exit %d, want %d: %s", code, ExitUsage, errb.String())
	}
	if !strings.Contains(errb.String(), "CRYO_MAINTAIN_SCRATCH_DIR") || !strings.Contains(errb.String(), "top-level") {
		t.Fatalf("the refusal does not say why: %q", errb.String())
	}
	t.Setenv("CRYO_MAINTAIN_MAX_STREAMS", "lots")
	t.Setenv("CRYO_MAINTAIN_SCRATCH_DIR", filepath.Join(dir, "scratch"))
	errb.Reset()
	if code := Run([]string{"--config", cfg, "--once"}, &out, &errb, keys); code != ExitUsage || !strings.Contains(errb.String(), "CRYO_MAINTAIN_MAX_STREAMS") {
		t.Fatalf("a bad CRYO_MAINTAIN_MAX_STREAMS: exit %d: %q", code, errb.String())
	}
}
