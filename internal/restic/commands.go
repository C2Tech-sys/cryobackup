package restic

import (
	"strconv"
	"strings"
)

// RestoreOptions describes one "restic restore" run (architecture §6.3, engine §1.12).
type RestoreOptions struct {
	Common

	// SnapshotID is a snapshot id or "latest", optionally followed by
	// ":<subfolder>" (restoring a subfolder puts its contents at the target
	// root). Required.
	SnapshotID string
	// Target is the directory restic writes to (--target). Required.
	Target string
	// Overwrite is "" (omit; restic defaults to always) or one of always,
	// if-changed, if-newer, never.
	Overwrite string
	// Includes renders one --include per entry.
	Includes []string
	// IncludeFile renders --include-file.
	IncludeFile string
	// Verify renders --verify. Mutually exclusive with DryRun.
	Verify bool
	// DryRun simulates: it renders "--dry-run -vv", which makes restic list
	// what it would restore (path-bearing lines; the decoder drops them).
	DryRun bool
}

// Restore builds the restore command:
//
//	restore --json --target T [--overwrite O] [--include P]... [--include-file F]
//	[--verify | --dry-run -vv] [--limit-upload N] [--limit-download N]
//	[--retry-lock D] [Extra...] SNAPSHOT
//
// It never emits --delete (architecture §6.3).
func Restore(o RestoreOptions) (Command, error) {
	if err := o.Common.validate(); err != nil {
		return Command{}, err
	}
	if !validSnapshotRef(o.SnapshotID, true) {
		return Command{}, invalid("restore: snapshot id must be hex or latest, optionally with :subfolder")
	}
	if hasControlOrEmpty(o.Target) {
		return Command{}, invalid("restore: target is required")
	}
	switch o.Overwrite {
	case "", "always", "if-changed", "if-newer", "never":
	default:
		return Command{}, invalid("restore: overwrite must be always, if-changed, if-newer or never")
	}
	if o.Verify && o.DryRun {
		return Command{}, invalid("restore: verify and dry run are mutually exclusive")
	}
	for _, p := range o.Includes {
		if hasControlOrEmpty(p) {
			return Command{}, invalid("restore: include patterns must be non-empty")
		}
	}
	if o.IncludeFile != "" && hasControl(o.IncludeFile) {
		return Command{}, invalid("restore: include file path has a control character")
	}

	args := []string{"restore", "--json", "--target", o.Target}
	if o.Overwrite != "" {
		args = append(args, "--overwrite", o.Overwrite)
	}
	for _, p := range o.Includes {
		args = append(args, "--include", p)
	}
	if o.IncludeFile != "" {
		args = append(args, "--include-file", o.IncludeFile)
	}
	if o.Verify {
		args = append(args, "--verify")
	}
	if o.DryRun {
		args = append(args, "--dry-run", "-vv")
	}
	args = o.tail(args)
	args = append(args, o.SnapshotID)
	return Command{Kind: KindRestore, Args: args}, nil
}

// Snapshots builds "snapshots --json [--host H]". An empty host lists every
// snapshot in the repository.
func Snapshots(host string, c Common) (Command, error) {
	if err := c.validate(); err != nil {
		return Command{}, err
	}
	if hasControl(host) {
		return Command{}, invalid("snapshots: host has a control character")
	}
	args := []string{"snapshots", "--json"}
	if host != "" {
		args = append(args, "--host", host)
	}
	return Command{Kind: KindSnapshots, Args: c.tail(args)}, nil
}

// Ls builds "ls --json SNAPSHOT DIR". dir is an absolute snapshot path with
// forward slashes ("/C/Users"); restic lists that directory and its direct
// children, one level, because --recursive is never emitted. An empty dir
// becomes "/", which lists the snapshot roots. (Without any dir restic 0.19.1
// walks the entire tree: testdata/v0.19.1/MANIFEST.md, divergence 3.)
func Ls(snapshotID, dir string, c Common) (Command, error) {
	if err := c.validate(); err != nil {
		return Command{}, err
	}
	if !validSnapshotRef(snapshotID, false) {
		return Command{}, invalid("ls: snapshot id must be hex or latest")
	}
	if dir == "" {
		dir = "/"
	}
	if dir[0] != '/' || hasControl(dir) {
		return Command{}, invalid("ls: dir must be an absolute snapshot path starting with '/'")
	}
	args := []string{"ls", "--json", snapshotID, dir}
	return Command{Kind: KindLs, Args: c.tail(args)}, nil
}

// FindOptions describes one "restic find" (engine §1.9).
type FindOptions struct {
	Common

	// Pattern is the name pattern. Required; it must not start with '-'.
	Pattern string
	// Host renders -H.
	Host string
	// IgnoreCase renders --ignore-case.
	IgnoreCase bool
	// Snapshots renders one --snapshot per id.
	Snapshots []string
}

// Find builds "find --json [-H host] [--ignore-case] [--snapshot id]... PATTERN".
func Find(o FindOptions) (Command, error) {
	if err := o.Common.validate(); err != nil {
		return Command{}, err
	}
	if hasControlOrEmpty(o.Pattern) || o.Pattern[0] == '-' {
		return Command{}, invalid("find: pattern is required and must not start with '-'")
	}
	if hasControl(o.Host) {
		return Command{}, invalid("find: host has a control character")
	}
	for _, s := range o.Snapshots {
		if !validSnapshotRef(s, false) {
			return Command{}, invalid("find: snapshot ids must be hex or latest")
		}
	}
	args := []string{"find", "--json"}
	if o.Host != "" {
		args = append(args, "-H", o.Host)
	}
	if o.IgnoreCase {
		args = append(args, "--ignore-case")
	}
	for _, s := range o.Snapshots {
		args = append(args, "--snapshot", s)
	}
	args = o.tail(args)
	args = append(args, o.Pattern)
	return Command{Kind: KindFind, Args: args}, nil
}

// Dump builds "dump -a zip --target FILE SNAPSHOT PATH": one path per call,
// written as a zip archive to targetFile (architecture §6.3, DH27). There is no
// --json. path is absolute with forward slashes; a snapshot of the form
// "<id>:<subfolder>" with path "/" puts the subfolder's contents at the archive
// root. The console never stages a restore (ADR 0008); it uses DumpStdout.
func Dump(snapshotID, path, targetFile string, c Common) (Command, error) {
	if err := dumpCheck(snapshotID, path, c); err != nil {
		return Command{}, err
	}
	if hasControlOrEmpty(targetFile) {
		return Command{}, invalid("dump: target file is required")
	}
	args := []string{"dump", "-a", "zip", "--target", targetFile, snapshotID, path}
	return Command{Kind: KindDump, Args: c.tail(args)}, nil
}

// DumpStdout builds "dump -a zip SNAPSHOT PATH" without --target: restic
// writes the archive to its stdout, which the caller streams (ADR 0008: a
// technician restore never touches the console host's disk). The validation
// is Dump's. restic 0.19.1 writes a zip only for a directory; for a single
// file it writes the file's raw bytes (the maintenance worker wraps them).
func DumpStdout(snapshotID, path string, c Common) (Command, error) {
	if err := dumpCheck(snapshotID, path, c); err != nil {
		return Command{}, err
	}
	args := []string{"dump", "-a", "zip", snapshotID, path}
	return Command{Kind: KindDump, Args: c.tail(args)}, nil
}

// dumpCheck is the validation Dump and DumpStdout share.
func dumpCheck(snapshotID, path string, c Common) error {
	if err := c.validate(); err != nil {
		return err
	}
	if !validSnapshotRef(snapshotID, true) {
		return invalid("dump: snapshot id must be hex or latest, optionally with :subfolder")
	}
	if path == "" || path[0] != '/' || hasControl(path) {
		return invalid("dump: path must be an absolute snapshot path starting with '/'")
	}
	return nil
}

// Unlock builds "unlock" (stale locks only) or, with removeAll, "unlock
// --remove-all". Removing every lock is console-only and only for a quiesced
// device (architecture §6.3); this is the only builder that emits --remove-all.
func Unlock(removeAll bool, c Common) (Command, error) {
	if err := c.validate(); err != nil {
		return Command{}, err
	}
	args := []string{"unlock"}
	if removeAll {
		args = append(args, "--remove-all")
	}
	return Command{Kind: KindUnlock, Args: c.tail(args)}, nil
}

// CatConfig builds "cat config": it proves the repository exists (exit 10 if
// not) and the password is right (exit 12 if not). Its stdout is a JSON object
// that DecodeConfig reads; restic does not take --json here.
func CatConfig(c Common) (Command, error) {
	if err := c.validate(); err != nil {
		return Command{}, err
	}
	return Command{Kind: KindCatConfig, Args: c.tail([]string{"cat", "config"})}, nil
}

// StatsMode is a value of "restic stats --mode". "debug" is deliberately not
// offered.
type StatsMode string

// Stats modes.
const (
	StatsRestoreSize     StatsMode = "restore-size"
	StatsFilesByContents StatsMode = "files-by-contents"
	StatsBlobsPerFile    StatsMode = "blobs-per-file"
	StatsRawData         StatsMode = "raw-data"
)

// Stats builds "stats --json --mode M [--host H]".
func Stats(mode StatsMode, host string, c Common) (Command, error) {
	if err := c.validate(); err != nil {
		return Command{}, err
	}
	switch mode {
	case StatsRestoreSize, StatsFilesByContents, StatsBlobsPerFile, StatsRawData:
	default:
		return Command{}, invalid("stats: mode must be restore-size, files-by-contents, blobs-per-file or raw-data")
	}
	if hasControl(host) {
		return Command{}, invalid("stats: host has a control character")
	}
	args := []string{"stats", "--json", "--mode", string(mode)}
	if host != "" {
		args = append(args, "--host", host)
	}
	return Command{Kind: KindStats, Args: c.tail(args)}, nil
}

// Version builds "version --json", the start-up check against the pinned
// restic (architecture §6.3).
func Version() Command {
	return Command{Kind: KindVersion, Args: []string{"version", "--json"}}
}

// ForgetOptions describes one "restic forget" (architecture §7.3 steps 3-4).
type ForgetOptions struct {
	Common

	// Host selects the snapshots (--host). Required.
	Host string
	// GroupBy is --group-by; "" means "host,paths".
	GroupBy string
	// KeepLast and the other Keep counts render --keep-<period> N when N is
	// positive. Zero omits the flag; -1 renders "unlimited" (restic keeps every
	// snapshot in that period); anything below -1 is an error.
	KeepLast, KeepHourly, KeepDaily, KeepWeekly, KeepMonthly, KeepYearly int
	// KeepWithin is a restic duration such as "30d" or "1y2m" (units y, m, d, h
	// in that order). Required: it makes a flood of fake snapshots unable to
	// push real history out of the keep windows (architecture §7.3, engine §1.14).
	KeepWithin string
	// KeepTags renders one --keep-tag per entry.
	KeepTags []string
	// DryRun renders --dry-run.
	DryRun bool
}

// Forget builds
//
//	forget --json [--dry-run] --host H --group-by G [--keep-last N] [--keep-hourly N]
//	[--keep-daily N] [--keep-weekly N] [--keep-monthly N] [--keep-yearly N]
//	--keep-within W [--keep-tag T]... [Common...]
//
// It never emits --prune: forget and prune are separate invocations because
// "forget --prune" mixes JSON and text (engine §1.9). A missing KeepWithin is
// an error.
func Forget(o ForgetOptions) (Command, error) {
	if err := o.Common.validate(); err != nil {
		return Command{}, err
	}
	if hasControlOrEmpty(o.Host) {
		return Command{}, invalid("forget: host is required")
	}
	group := o.GroupBy
	if group == "" {
		group = "host,paths"
	}
	for i := 0; i < len(group); i++ {
		c := group[i]
		if !(c >= 'a' && c <= 'z' || c == ',') {
			return Command{}, invalid("forget: group-by must be a comma-separated list of host, paths, tags")
		}
	}
	if o.KeepWithin == "" || !keepWithinRE.MatchString(o.KeepWithin) {
		return Command{}, invalid("forget: keep-within is required and must look like 1y2m3d4h")
	}
	for _, t := range o.KeepTags {
		if hasControlOrEmpty(t) || strings.ContainsRune(t, ',') {
			return Command{}, invalid("forget: keep tags must be non-empty and free of ','")
		}
	}
	keeps := []struct {
		flag string
		n    int
	}{
		{"--keep-last", o.KeepLast}, {"--keep-hourly", o.KeepHourly}, {"--keep-daily", o.KeepDaily},
		{"--keep-weekly", o.KeepWeekly}, {"--keep-monthly", o.KeepMonthly}, {"--keep-yearly", o.KeepYearly},
	}
	args := []string{"forget", "--json"}
	if o.DryRun {
		args = append(args, "--dry-run")
	}
	args = append(args, "--host", o.Host, "--group-by", group)
	for _, k := range keeps {
		switch {
		case k.n < -1:
			return Command{}, invalid("forget: keep counts must be -1 (unlimited), 0 (omit) or positive")
		case k.n == -1:
			args = append(args, k.flag, "unlimited")
		case k.n > 0:
			args = append(args, k.flag, strconv.Itoa(k.n))
		}
	}
	args = append(args, "--keep-within", o.KeepWithin)
	for _, t := range o.KeepTags {
		args = append(args, "--keep-tag", t)
	}
	return Command{Kind: KindForget, Args: o.tail(args)}, nil
}

// PruneOptions describes one "restic prune" (architecture §7.3 step 5).
type PruneOptions struct {
	Common

	// MaxUnused is --max-unused: "5%", "unlimited" or bytes with an optional
	// k/m/g/t suffix. "" omits it.
	MaxUnused string
	// RepackCacheableOnly renders --repack-cacheable-only.
	RepackCacheableOnly bool
	// MaxRepackSize is --max-repack-size: bytes with an optional k/m/g/t suffix.
	MaxRepackSize string
	// DryRun renders --dry-run.
	DryRun bool
}

// Prune builds "prune [--max-unused X] [--repack-cacheable-only]
// [--max-repack-size S] [--dry-run]". Prune has no JSON output (engine §1.9);
// feed its stdout to ParsePruneText.
func Prune(o PruneOptions) (Command, error) {
	if err := o.Common.validate(); err != nil {
		return Command{}, err
	}
	if o.MaxUnused != "" && !maxUnusedRE.MatchString(o.MaxUnused) {
		return Command{}, invalid("prune: max-unused must be a percentage, unlimited or a size")
	}
	if o.MaxRepackSize != "" && !sizeRE.MatchString(o.MaxRepackSize) {
		return Command{}, invalid("prune: max-repack-size must be a size such as 500M")
	}
	args := []string{"prune"}
	if o.MaxUnused != "" {
		args = append(args, "--max-unused", o.MaxUnused)
	}
	if o.RepackCacheableOnly {
		args = append(args, "--repack-cacheable-only")
	}
	if o.MaxRepackSize != "" {
		args = append(args, "--max-repack-size", o.MaxRepackSize)
	}
	if o.DryRun {
		args = append(args, "--dry-run")
	}
	return Command{Kind: KindPrune, Args: o.tail(args)}, nil
}

// CheckOptions describes one "restic check" (architecture §7.3 step 6).
type CheckOptions struct {
	Common

	// ReadDataSubset is --read-data-subset: "n/t", "x%" or a size such as
	// "10G". "" checks structure only.
	ReadDataSubset string
	// WithCache renders --with-cache.
	WithCache bool
}

// Check builds "check --json [--read-data-subset S] [--with-cache]".
func Check(o CheckOptions) (Command, error) {
	if err := o.Common.validate(); err != nil {
		return Command{}, err
	}
	if s := o.ReadDataSubset; s != "" && !validSubset(s) {
		return Command{}, invalid("check: read-data-subset must be n/t, a percentage or a size")
	}
	args := []string{"check", "--json"}
	if o.ReadDataSubset != "" {
		args = append(args, "--read-data-subset", o.ReadDataSubset)
	}
	if o.WithCache {
		args = append(args, "--with-cache")
	}
	return Command{Kind: KindCheck, Args: o.tail(args)}, nil
}

func validSubset(s string) bool {
	if m := subsetFracRE.FindStringSubmatch(s); m != nil {
		n, err1 := strconv.Atoi(m[1])
		t, err2 := strconv.Atoi(m[2])
		return err1 == nil && err2 == nil && n >= 1 && t >= n
	}
	return subsetPctRE.MatchString(s) || sizeRE.MatchString(s) && strings.IndexAny(s, "kKmMgGtT") > 0
}
