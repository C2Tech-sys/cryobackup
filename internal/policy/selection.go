package policy

import (
	"bytes"
	"errors"
	"sort"
	"strings"
)

// Environment holds facts about the device that only the agent can discover.
// It is plain input here: this package never touches the machine.
type Environment struct {
	// SystemDrive is the system volume, "C:". Empty means "C:".
	SystemDrive string
	// ProfileDirs are the user profile directories, e.g. C:\Users\fixture.
	ProfileDirs []string
	// FixedDrives are the fixed (non-removable, non-network) volumes: "C",
	// "C:" and `C:\` are all accepted.
	FixedDrives []string
	// ProgramData is the ProgramData directory. Empty means <SystemDrive>\ProgramData.
	ProgramData string
}

func (e Environment) systemDrive() string {
	if e.SystemDrive == "" {
		return "C:"
	}
	return strings.TrimRight(e.SystemDrive, `\`)
}

func (e Environment) programData() string {
	if e.ProgramData == "" {
		return e.systemDrive() + `\ProgramData`
	}
	return strings.TrimRight(e.ProgramData, `\`)
}

// driveRoot normalises "c", "C:" or `c:\` to `C:\`.
func driveRoot(d string) (string, bool) {
	d = strings.TrimRight(strings.TrimSpace(d), `\`)
	d = strings.TrimSuffix(d, ":")
	if len(d) != 1 || !isAlpha(d[0]) {
		return "", false
	}
	return strings.ToUpper(d) + `:\`, true
}

// Roots expands the selection into the list of backup roots: the explicit
// roots plus the expansion of each preset, normalised (trailing backslash
// removed except on a drive root), deduplicated case-insensitively (the first
// spelling wins, explicit roots before presets) and sorted case-insensitively.
//
//	all_user_profiles  each of env.ProfileDirs
//	documents_desktop  <profile>\Documents and <profile>\Desktop for each profile
//	fixed_drives       each of env.FixedDrives as X:\
//
// It fails loudly instead of returning an empty or partial list: a root that
// is not an absolute Windows path, an unknown preset, a preset whose facts
// the environment did not supply, or a selection that yields no roots.
func Roots(sel Selection, env Environment) ([]string, error) {
	var out []string
	var problems []string
	add := func(r string) { out = append(out, normRoot(r)) }

	for _, r := range sel.Roots {
		if !isAbsWindowsPath(r) {
			problems = append(problems, `root `+quote(r)+` is not an absolute Windows path`)
			continue
		}
		add(r)
	}
	profiles := func(preset string) []string {
		var ok []string
		for _, d := range env.ProfileDirs {
			if !isAbsWindowsPath(d) {
				problems = append(problems, "profile directory "+quote(d)+" is not an absolute Windows path")
				continue
			}
			ok = append(ok, strings.TrimRight(d, `\`))
		}
		if len(env.ProfileDirs) == 0 {
			problems = append(problems, "preset "+preset+" needs profile directories, but the environment lists none")
		}
		return ok
	}
	for _, name := range sel.Presets {
		switch name {
		case PresetAllUserProfiles:
			for _, d := range profiles(name) {
				add(d)
			}
		case PresetDocumentsDesktop:
			for _, d := range profiles(name) {
				add(d + `\Documents`)
				add(d + `\Desktop`)
			}
		case PresetFixedDrives:
			if len(env.FixedDrives) == 0 {
				problems = append(problems, "preset "+name+" needs fixed drives, but the environment lists none")
			}
			for _, d := range env.FixedDrives {
				r, ok := driveRoot(d)
				if !ok {
					problems = append(problems, "fixed drive "+quote(d)+" is not a drive letter")
					continue
				}
				add(r)
			}
		default:
			problems = append(problems, "unknown selection preset "+quote(name))
		}
	}
	if len(problems) > 0 {
		return nil, errors.New("selection: " + strings.Join(problems, "; "))
	}

	seen := map[string]bool{}
	uniq := out[:0]
	for _, r := range out {
		k := strings.ToLower(r)
		if !seen[k] {
			seen[k] = true
			uniq = append(uniq, r)
		}
	}
	sort.SliceStable(uniq, func(i, j int) bool { return strings.ToLower(uniq[i]) < strings.ToLower(uniq[j]) })
	if len(uniq) == 0 {
		return nil, errors.New("selection: no roots (list a root or a preset)")
	}
	return uniq, nil
}

// Token prefixes that Baseline and ExclusionPresets entries may start with.
// They are replaced when the exclude file is generated.
const (
	// TokenProfile expands to every directory in Environment.ProfileDirs (one
	// line per profile). With no profiles the line is omitted.
	TokenProfile = `%PROFILE%`
	// TokenSystemDrive expands to Environment.SystemDrive ("C:").
	TokenSystemDrive = `%SYSTEMDRIVE%`
)

// Baseline is the published default exclusion baseline (matrix r50;
// architecture §3.6): what a Windows file backup never wants. Entries are
// restic exclude patterns written with backslash separators (see the package
// comment for why) and are matched case-insensitively (--iexclude-file). An
// entry starting with a Token is anchored to a profile or the system drive; the
// rest are single names or globs that match a path component at any depth.
// The FilesNotToBackup registry key (r50) is read by the agent and appended
// through Selection.Excludes. Treat the slice as read-only.
var Baseline = []string{
	// Temporary files.
	TokenProfile + `\AppData\Local\Temp`,
	TokenSystemDrive + `\Windows\Temp`,
	// App-execution aliases: reparse points restic cannot archive ("unsupported
	// file type irregular"). Present in every profile; never user data.
	TokenProfile + `\AppData\Local\Microsoft\WindowsApps`,
	// Recycle Bin and system volume metadata.
	`$Recycle.Bin`,
	`System Volume Information`,
	// Paging, hibernation and swap files.
	`pagefile.sys`,
	`hiberfil.sys`,
	`swapfile.sys`,
	// Browser caches: Chrome, Edge, Firefox. Bookmarks, logins and history live
	// elsewhere in the profile and stay in the backup.
	TokenProfile + `\AppData\Local\Google\Chrome\User Data\*\Cache`,
	TokenProfile + `\AppData\Local\Google\Chrome\User Data\*\Code Cache`,
	TokenProfile + `\AppData\Local\Google\Chrome\User Data\*\GPUCache`,
	TokenProfile + `\AppData\Local\Microsoft\Edge\User Data\*\Cache`,
	TokenProfile + `\AppData\Local\Microsoft\Edge\User Data\*\Code Cache`,
	TokenProfile + `\AppData\Local\Microsoft\Edge\User Data\*\GPUCache`,
	TokenProfile + `\AppData\Local\Mozilla\Firefox\Profiles\*\cache2`,
	// Windows prefetch and thumbnail caches.
	TokenSystemDrive + `\Windows\Prefetch`,
	`thumbcache_*.db`,
	`Thumbs.db`,
}

// ExclusionPresets are the published, optional exclusion presets (matrix r54).
// A selection's excludes list turns one on with an entry "@name"; entries use
// the same syntax and tokens as Baseline. Treat the map as read-only.
//
//	browser_caches  the remaining browser cache-like data: Service Worker
//	                caches, shader caches and Firefox start-up caches
//	vm_images       virtual disks and disc images: *.vhd, *.vhdx, *.vmdk, *.iso
//	installers      *.msi and *.exe under each profile's Downloads folder
//	dev_caches      package and build caches that can be re-fetched:
//	                node_modules, __pycache__, .gradle/caches, .m2/repository,
//	                .nuget/packages, .cargo/registry. Not .git: repositories are
//	                source of truth and stay in the backup.
var ExclusionPresets = map[string][]string{
	"browser_caches": {
		TokenProfile + `\AppData\Local\Google\Chrome\User Data\*\Service Worker\CacheStorage`,
		TokenProfile + `\AppData\Local\Google\Chrome\User Data\ShaderCache`,
		TokenProfile + `\AppData\Local\Google\Chrome\User Data\GrShaderCache`,
		TokenProfile + `\AppData\Local\Microsoft\Edge\User Data\*\Service Worker\CacheStorage`,
		TokenProfile + `\AppData\Local\Microsoft\Edge\User Data\ShaderCache`,
		TokenProfile + `\AppData\Local\Microsoft\Edge\User Data\GrShaderCache`,
		TokenProfile + `\AppData\Local\Mozilla\Firefox\Profiles\*\startupCache`,
		TokenProfile + `\AppData\Local\Mozilla\Firefox\Profiles\*\thumbnails`,
	},
	"vm_images": {
		`*.vhd`,
		`*.vhdx`,
		`*.vmdk`,
		`*.iso`,
	},
	"installers": {
		TokenProfile + `\Downloads\**\*.msi`,
		TokenProfile + `\Downloads\**\*.exe`,
	},
	"dev_caches": {
		`node_modules`,
		`__pycache__`,
		TokenProfile + `\.gradle\caches`,
		TokenProfile + `\.m2\repository`,
		TokenProfile + `\.nuget\packages`,
		TokenProfile + `\.cargo\registry`,
	},
}

func exclusionPresetNames() []string {
	names := make([]string, 0, len(ExclusionPresets))
	for n := range ExclusionPresets {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// MandatoryExcludes returns the patterns every generated exclude file starts
// with and, when a custom exclude tries to re-include something, ends with
// again: CryoBackup's own state and restic cache (<ProgramData>\CryoBackup),
// any per-profile default restic cache, and the files and folders no file
// backup can read or should read. The set does not depend on the policy.
func MandatoryExcludes(env Environment) []string {
	out := []string{env.programData() + `\CryoBackup`}
	for _, d := range env.ProfileDirs {
		if isAbsWindowsPath(d) {
			out = append(out, strings.TrimRight(d, `\`)+`\AppData\Local\restic`)
		}
	}
	return append(out,
		`pagefile.sys`,
		`hiberfil.sys`,
		`swapfile.sys`,
		`DumpStack.log.tmp`,
		`$Recycle.Bin`,
		`System Volume Information`,
	)
}

// globLiteral makes a path from the environment safe to embed in a pattern.
// On Windows a backslash separates, so it cannot escape; the one character that
// is special in a Windows path is '[', which a character class can neutralise.
// ('*' and '?' cannot occur in a Windows file name.)
func globLiteral(s string) string { return strings.ReplaceAll(s, "[", "[[]") }

// expand resolves the tokens of one Baseline / preset entry. It returns no
// lines for a profile-anchored entry when the environment has no profiles.
func expand(entry string, env Environment) []string {
	switch {
	case strings.HasPrefix(entry, TokenProfile):
		rest := strings.TrimPrefix(entry, TokenProfile)
		var out []string
		for _, d := range env.ProfileDirs {
			if isAbsWindowsPath(d) {
				out = append(out, globLiteral(strings.TrimRight(d, `\`))+rest)
			}
		}
		return out
	case strings.HasPrefix(entry, TokenSystemDrive):
		return []string{globLiteral(env.systemDrive()) + strings.TrimPrefix(entry, TokenSystemDrive)}
	}
	return []string{entry}
}

// ExcludeFile renders the restic --iexclude-file content for a selection:
// UTF-8, LF line endings, one pattern per line, "#" comment lines, in this
// order:
//
//  1. MandatoryExcludes
//  2. Baseline
//  3. each exclusion preset named by an "@name" entry of sel.Excludes
//  4. the other sel.Excludes entries, verbatim ("!pattern" re-includes)
//  5. only if step 4 contains a re-include: MandatoryExcludes again
//
// restic evaluates the lines in order and the last matching line wins, so step
// 5 is what keeps a custom "!pagefile.sys" or "!C:\ProgramData\CryoBackup" from
// re-including the mandatory set. Exact duplicate lines (case-insensitive) in
// steps 2-4 are written once. Every '$' is doubled because restic expands
// $VAR in exclude files; the doubled form is the literal dollar of $Recycle.Bin
// and of Office's ~$ lock files. The output is a pure function of its inputs
// (no timestamp). Invalid custom entries (empty, multi-line, starting with '#')
// are skipped; Validate rejects them first.
func ExcludeFile(sel Selection, env Environment) []byte {
	var b bytes.Buffer
	line := func(s string) { b.WriteString(strings.ReplaceAll(s, "$", "$$")); b.WriteByte('\n') }
	comment := func(s string) { b.WriteString("# " + s + "\n") }

	comment("CryoBackup exclude patterns for restic --iexclude-file.")
	comment("Generated from the policy; changes are overwritten.")
	comment("Backslash separators; matched case-insensitively; the last matching line wins.")
	b.WriteByte('\n')

	comment("mandatory")
	mandatory := MandatoryExcludes(env)
	for _, m := range mandatory {
		line(m)
	}
	seen := map[string]bool{}
	for _, m := range mandatory {
		seen[strings.ToLower(m)] = true
	}
	emit := func(p string) {
		if k := strings.ToLower(p); !seen[k] {
			seen[k] = true
			line(p)
		}
	}
	section := func(name string, patterns []string) {
		b.WriteByte('\n')
		comment(name)
		skipped := 0
		for _, e := range patterns {
			lines := expand(e, env)
			if len(lines) == 0 {
				skipped++
			}
			for _, l := range lines {
				emit(l)
			}
		}
		if skipped > 0 {
			comment("omitted: profile-relative patterns need profile directories; none supplied")
		}
	}

	section("baseline", Baseline)

	var custom []string
	var presets []string
	presetSeen := map[string]bool{}
	for _, ex := range sel.Excludes {
		if name, ok := ExclusionPresetRef(ex); ok {
			if _, known := ExclusionPresets[name]; known && !presetSeen[name] {
				presetSeen[name] = true
				presets = append(presets, name)
			}
			continue
		}
		if t := strings.TrimSpace(ex); t != "" && !strings.ContainsAny(t, "\r\n") && !strings.HasPrefix(t, "#") {
			custom = append(custom, t)
		}
	}
	for _, name := range presets {
		section("preset "+name, ExclusionPresets[name])
	}
	reinclude := false
	if len(custom) > 0 {
		b.WriteByte('\n')
		comment("custom")
		for _, c := range custom {
			emit(c)
			reinclude = reinclude || strings.HasPrefix(c, "!")
		}
	}
	if reinclude {
		b.WriteByte('\n')
		comment("mandatory again: a re-include above must not win over these")
		for _, m := range mandatory {
			line(m)
		}
	}
	return b.Bytes()
}
