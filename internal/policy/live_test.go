package policy

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

// This is the live test of ticket P1-T2 MUST DO 7: it runs the pinned restic
// against a temp tree and proves that the exclude file ExcludeFile generates
// is honoured. It is the source of the separator decision recorded in doc.go.
//
// Run it with CRYO_RESTIC set to the path of restic 0.19.1 (Windows only, the
// patterns are Windows paths):
//
//	set CRYO_RESTIC=C:\path\to\restic_0.19.1_windows_amd64.exe
//	go test -count=1 -run TestLiveRestic -v ./internal/policy/

const pinnedResticVersion = "restic 0.19.1 "

type liveFile struct {
	rel          string // relative to the tree root, backslash form
	keptBaseline bool   // kept when only the baseline is applied
	keptFull     bool   // kept when presets and custom excludes are applied
	why          string
}

// liveFiles builds the tree: every baseline pattern gets a file it must hit,
// and the paths that look similar but must survive get a file too.
func liveFiles() []liveFile {
	const p, p2 = `Users\fixture`, `Users\fixture[1]`
	chrome := p + `\AppData\Local\Google\Chrome\User Data\Default`
	return []liveFile{
		// Must survive everything.
		{p + `\Documents\report.docx`, true, true, "ordinary document"},
		{p + `\Documents\Temp\keepme.txt`, true, true, "a folder named Temp outside the profile Temp (anchoring)"},
		{p + `\Documents\Cache\notes.txt`, true, true, "a folder named Cache outside a browser profile (anchoring)"},
		{p + `\AppData\Roaming\app.cfg`, true, true, "roaming settings"},
		{chrome + `\Bookmarks`, true, true, "browser data that is not cache"},
		{`Windows\Other\o.txt`, true, true, "Windows folder that is not Temp or Prefetch"},
		{p2 + `\Documents\bracket.docx`, true, true, "profile directory containing a ["},
		{p + `\Downloads\readme.txt`, true, true, "download that is not an installer"},
		{p + `\Documents\proj\main.go`, true, true, "source code"},

		// Baseline: excluded in both runs.
		{p + `\AppData\Local\Temp\a.tmp`, false, false, "user Temp"},
		{`Windows\Temp\b.tmp`, false, false, "Windows Temp"},
		{p + `\AppData\Local\Microsoft\WindowsApps\python.exe`, false, false, "WindowsApps app-execution alias"},
		{p + `\AppData\Local\Microsoft\Windows\Other\w.txt`, true, true, "sibling of WindowsApps under Microsoft (anchoring)"},
		{`$Recycle.Bin\S-1-5\r.txt`, false, false, "Recycle Bin ($ escaping)"},
		{`System Volume Information\s.dat`, false, false, "System Volume Information"},
		{`pagefile.sys`, false, false, "pagefile"},
		{`hiberfil.sys`, false, false, "hiberfil"},
		{`swapfile.sys`, false, false, "swapfile"},
		{`DumpStack.log.tmp`, false, false, "mandatory: DumpStack"},
		{chrome + `\Cache\Cache_Data\f_1`, false, false, "Chrome Cache"},
		{chrome + `\Code Cache\js\c1`, false, false, "Chrome Code Cache (space in name)"},
		{chrome + `\GPUCache\g1`, false, false, "Chrome GPUCache"},
		{p + `\AppData\Local\Microsoft\Edge\User Data\Profile 1\Cache\Cache_Data\e1`, false, false, "Edge Cache, second profile"},
		{p + `\AppData\Local\Mozilla\Firefox\Profiles\abc.default\cache2\entries\e1`, false, false, "Firefox cache2"},
		{`Windows\Prefetch\p.pf`, false, false, "Prefetch"},
		{p + `\AppData\Local\Microsoft\Windows\Explorer\thumbcache_256.db`, false, false, "thumbnail cache"},
		{p + `\Pictures\Thumbs.db`, false, false, "Thumbs.db"},
		{`ProgramData\CryoBackup\cache\restic\data\x`, false, false, "mandatory: CryoBackup cache"},
		{`ProgramData\CryoBackup\state\journal.db`, false, false, "mandatory: CryoBackup state (also re-include attempt)"},
		{p + `\AppData\Local\restic\cache\c1`, false, false, "mandatory: per-profile restic cache"},
		{p2 + `\AppData\Local\Temp\bt.tmp`, false, false, "user Temp of a profile containing a ["},

		// Optional presets and custom excludes: kept by the baseline alone.
		{p + `\Documents\disk.vhdx`, true, false, "@vm_images"},
		{p + `\Downloads\setup.msi`, true, false, "@installers (top level)"},
		{p + `\Downloads\sub\tool.exe`, true, false, "@installers (** component)"},
		{p + `\Documents\proj\node_modules\m.js`, true, false, "@dev_caches"},
		{chrome + `\Service Worker\CacheStorage\s1`, true, false, "@browser_caches"},
		{p + `\Documents\old.bak`, true, false, "custom *.bak"},
		{p + `\Documents\~$lock.docx`, true, false, "custom ~$* ($ escaping in a custom pattern)"},
		{p + `\Documents\keep.bak`, true, true, "custom !re-include wins over *.bak"},
	}
}

// resticRun runs restic with a repository password in the environment and
// returns stdout and stderr separately.
func resticRun(t *testing.T, exe, cache string, args ...string) (string, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Env = append(os.Environ(), "RESTIC_PASSWORD=fixture-password-not-secret", "RESTIC_CACHE_DIR="+cache)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("restic %s: %v\nstderr: %s", strings.Join(args, " "), err, errb.String())
	}
	return out.String(), errb.String()
}

// liveBackup does a dry-run backup of tree with exclude file content and
// returns the relative file paths (forward-slash form) restic would save, and
// the summary's files_new.
func liveBackup(t *testing.T, exe, base, tree, repo string, excludes []byte) (map[string]bool, int) {
	t.Helper()
	ef := filepath.Join(base, "excludes.txt")
	if err := os.WriteFile(ef, excludes, 0o644); err != nil {
		t.Fatal(err)
	}
	stdout, _ := resticRun(t, exe, filepath.Join(base, "cache"),
		"backup", "--dry-run", "--json", "-vv", "--iexclude-file", ef, "-r", repo, tree)
	marker := "/" + filepath.Base(base) + "/tree/"
	got := map[string]bool{}
	filesNew := -1
	for _, line := range strings.Split(stdout, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var m struct {
			MessageType string `json:"message_type"`
			Action      string `json:"action"`
			Item        string `json:"item"`
			FilesNew    int    `json:"files_new"`
		}
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("unparsable restic JSON line %q: %v", line, err)
		}
		switch {
		case m.MessageType == "summary":
			filesNew = m.FilesNew
		case m.MessageType == "verbose_status" && m.Action == "new" && !strings.HasSuffix(m.Item, "/"):
			if i := strings.Index(m.Item, marker); i >= 0 {
				got[m.Item[i+len(marker):]] = true
			}
		}
	}
	if filesNew < 0 {
		t.Fatalf("no summary line in restic output:\n%s", stdout)
	}
	return got, filesNew
}

func TestLiveRestic(t *testing.T) {
	exe := os.Getenv("CRYO_RESTIC")
	if exe == "" {
		t.Skip("CRYO_RESTIC not set: skipping the live pattern test against restic")
	}
	if runtime.GOOS != "windows" {
		t.Skip("the generated patterns are Windows paths; the live test runs on Windows only")
	}
	base, err := os.MkdirTemp("", "cryopolicy")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	tree := filepath.Join(base, "tree")
	repo := filepath.Join(base, "repo")

	files := liveFiles()
	for _, f := range files {
		path := filepath.Join(tree, f.rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("create %s: %v", f.rel, err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatalf("create %s: %v", f.rel, err)
		}
	}

	ver, _ := resticRun(t, exe, filepath.Join(base, "cache"), "version")
	if !strings.HasPrefix(ver, pinnedResticVersion) {
		t.Fatalf("restic version %q: this test is pinned to %q (ADR 0003); re-check engine §1.5 before changing the pin", strings.TrimSpace(ver), strings.TrimSpace(pinnedResticVersion))
	}
	t.Logf("restic: %s", strings.TrimSpace(strings.SplitN(ver, "\n", 2)[0]))
	resticRun(t, exe, filepath.Join(base, "cache"), "init", "-r", repo, "-q")

	// The environment is the fixture tree: SystemDrive and ProgramData point
	// into it, so every anchored pattern is an absolute path inside the tree
	// (the tree itself lives under %TEMP%, which a loose "Temp" pattern would
	// otherwise exclude wholesale - the reason the baseline is anchored).
	env := Environment{
		SystemDrive: tree,
		ProfileDirs: []string{filepath.Join(tree, `Users\fixture`), filepath.Join(tree, `Users\fixture[1]`)},
		ProgramData: filepath.Join(tree, "ProgramData"),
	}
	if !isAbsWindowsPath(env.ProfileDirs[0]) {
		t.Skipf("temp directory %q is not a drive-absolute path", base)
	}
	keepBak := filepath.Join(env.ProfileDirs[0], `Documents\keep.bak`)
	selections := map[string]Selection{
		"baseline": {},
		"full": {Excludes: []string{
			"@vm_images", "@installers", "@dev_caches", "@browser_caches",
			`*.bak`, `!` + keepBak, `~$*`,
			`!pagefile.sys`, `!` + filepath.Join(env.ProgramData, "CryoBackup"), // must not win over the mandatory set
		}},
	}

	check := func(t *testing.T, name string, got map[string]bool, filesNew int) {
		t.Helper()
		var kept int
		for _, f := range files {
			want := f.keptBaseline
			if name == "full" {
				want = f.keptFull
			}
			if want {
				kept++
			}
			if have := got[strings.ReplaceAll(f.rel, `\`, "/")]; have != want {
				t.Errorf("%s: %s (%s): restic kept=%v, want %v", name, f.rel, f.why, have, want)
			}
		}
		if filesNew != kept {
			t.Errorf("%s: restic summary says %d new files, want %d", name, filesNew, kept)
		}
		for rel := range got {
			found := false
			for _, f := range files {
				found = found || strings.EqualFold(strings.ReplaceAll(f.rel, `\`, "/"), rel)
			}
			if !found {
				t.Errorf("%s: restic reports an unexpected file %q", name, rel)
			}
		}
	}

	names := make([]string, 0, len(selections))
	for n := range selections {
		names = append(names, n)
	}
	sort.Strings(names)

	// The decision under test: the exact bytes ExcludeFile emits (backslash
	// separators, doubled dollar signs).
	for _, name := range names {
		t.Run("generated/"+name, func(t *testing.T) {
			got, n := liveBackup(t, exe, base, tree, repo, ExcludeFile(selections[name], env))
			check(t, name, got, n)
		})
	}

	// The separator probe: the same file with forward slashes. Informational -
	// our output stays backslash - but it records what the binary does.
	t.Run("probe/forward_slash_separators", func(t *testing.T) {
		var out []string
		for _, l := range strings.Split(string(ExcludeFile(selections["full"], env)), "\n") {
			if !strings.HasPrefix(l, "#") {
				l = strings.ReplaceAll(l, `\`, "/")
			}
			out = append(out, l)
		}
		got, n := liveBackup(t, exe, base, tree, repo, []byte(strings.Join(out, "\n")))
		same := n == len(got)
		for _, f := range files {
			if got[strings.ReplaceAll(f.rel, `\`, "/")] != f.keptFull {
				same = false
			}
		}
		t.Logf("SEPARATOR PROBE: forward-slash patterns on Windows restic 0.19.1 honoured identically to backslash: %v (files saved: %d)", same, n)
	})

	// Negative control for the dollar escaping: a single $ is expanded by
	// restic as an environment variable, so $Recycle.Bin matches nothing.
	t.Run("control/single_dollar_is_expanded", func(t *testing.T) {
		got, _ := liveBackup(t, exe, base, tree, repo, []byte("$Recycle.Bin\n"))
		if !got["$Recycle.Bin/S-1-5/r.txt"] {
			t.Fatal("an unescaped $Recycle.Bin excluded the Recycle Bin: restic no longer expands $VAR, so the doubled-dollar escaping must be re-decided")
		}
		got, _ = liveBackup(t, exe, base, tree, repo, []byte("$$Recycle.Bin\n"))
		if got["$Recycle.Bin/S-1-5/r.txt"] {
			t.Fatal("a doubled $$Recycle.Bin did not exclude the Recycle Bin")
		}
	})

	// Negative control for the order rule: the last matching line wins.
	t.Run("control/last_match_wins", func(t *testing.T) {
		got, _ := liveBackup(t, exe, base, tree, repo, []byte("pagefile.sys\n!pagefile.sys\n"))
		if !got["pagefile.sys"] {
			t.Fatal("a later !pagefile.sys did not re-include: ExcludeFile's repeat of the mandatory block would not be needed")
		}
		got, _ = liveBackup(t, exe, base, tree, repo, []byte("!pagefile.sys\npagefile.sys\n"))
		if got["pagefile.sys"] {
			t.Fatal("an earlier !pagefile.sys beat a later exclude: the last-match-wins rule in doc.go is wrong")
		}
	})
}
