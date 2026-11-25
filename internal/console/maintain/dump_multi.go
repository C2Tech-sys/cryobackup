package maintain

import (
	"archive/tar"
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"sync/atomic"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// The several-paths download (ADR 0028, completing ADR 0008). One request names
// up to adminpath.MaxDumpPaths snapshot paths; the worker streams them as ONE
// zip into the admin-path response, one restic run per path, one run at a time,
// with nothing on disk and constant memory:
//
//   - a file is `restic dump <snap> <file>`: its raw bytes become one entry;
//   - a folder is `restic dump -a tar <snap> <folder>`, read with archive/tar
//     (PAX records included) and written entry by entry into the same zip.
//
// What restic 0.19.1 does was proven by the P22 spike (item 6): one path per
// call; `-a` is ignored for a file; tar entries carry the full snapshot path
// without the leading "/"; the dumped folder has no entry of its own; a
// junction comes out as a symlink; a cut stream is a detectably truncated tar.
// So the worker renames every entry relative to the selected item's parent,
// writes the folder's own entry itself (an empty folder would otherwise
// vanish), leaves symlinks, junctions, hard links and devices out and counts
// them in a final CRYOBACKUP-SKIPPED.txt that names no file, and refuses the
// whole download if any entry name could escape the folder it is extracted to
// (zip-slip).
//
// The zip's central directory is written only after every restic run exited 0
// and every tar ended cleanly. Anything else returns an error before it, so the
// bytes already sent never form a readable archive, and the console aborts its
// HTTP response (adminpath trailer; dump.go's rule for one path, extended to
// the whole download).

// DumpIdleLimit is how long a dump's restic child may go without sending a byte
// before the worker stops it (idle watchdog). With its storage gone, restic
// 0.19.1 does not exit: it retried for more than 20 minutes in the P22 spike
// (item 6, gateway killed), and an object-storage outage behind a live rclone
// was not tested, so without a limit a download would hang for the whole
// stream timeout (Timeouts.Dump, about 12 hours). The limit must also cover
// restic's silent start, when it reads every index file without a cache (ADR
// 0008) before the first byte: one cold `ls` of one folder took up to 62 s in
// the spike (item 7, local disk, loaded workstation; object storage not
// measured), and a dump pays the same start. Five minutes is about five times
// that worst start and a quarter of restic's observed retry span. The clock
// runs only while the worker waits for restic, never while it waits for the
// technician's browser to take the bytes (a slow download is not a stall).
const DumpIdleLimit = 5 * time.Minute

// dumpIdleOverride, when above zero, replaces DumpIdleLimit (nanoseconds; the
// tests shorten the limit). Atomic: a download runs on the admin-path server's
// goroutines.
var dumpIdleOverride atomic.Int64

// dumpIdleLimit is the idle limit in force.
func dumpIdleLimit() time.Duration {
	if n := dumpIdleOverride.Load(); n > 0 {
		return time.Duration(n)
	}
	return DumpIdleLimit
}

// skippedEntryName is the last entry of a several-paths zip when anything was
// left out: counts by type, never a name.
const skippedEntryName = "CRYOBACKUP-SKIPPED.txt"

// Reasons a dump run is cancelled by the worker itself (context causes).
var (
	errDumpIdle   = errors.New("maintain: restic sent no data within the idle limit")
	errUnsafeName = errors.New("maintain: an archive entry name could escape its folder")
	errDestFailed = errors.New("maintain: the stream destination failed")
)

// skipKinds are the kinds of entries left out of a several-paths zip, in the
// order CRYOBACKUP-SKIPPED.txt lists them, with the words it uses.
var skipKinds = []struct{ key, label string }{
	{"symlink", "symbolic links and junctions"},
	{"hardlink", "hard links"},
	{"device", "devices"},
	{"fifo", "named pipes"},
	{"socket", "sockets"},
	{"other", "other special entries"},
}

// nodeSkipKind is the skip kind of a restic node type; "" for a file or a
// folder, which are streamed.
func nodeSkipKind(typ string) string {
	switch typ {
	case "file", "dir":
		return ""
	case "symlink":
		return "symlink"
	case "dev", "chardev":
		return "device"
	case "fifo":
		return "fifo"
	case "socket":
		return "socket"
	}
	return "other"
}

// tarSkipKind is the skip kind of a tar entry type that is not a regular file
// or a directory.
func tarSkipKind(flag byte) string {
	switch flag {
	case tar.TypeSymlink:
		return "symlink"
	case tar.TypeLink:
		return "hardlink"
	case tar.TypeChar, tar.TypeBlock:
		return "device"
	case tar.TypeFifo:
		return "fifo"
	}
	return "other"
}

// selection is one top-level item of a several-paths download.
//
// PATH-BEARING: path and name are file names; never log them.
type selection struct {
	path  string // clean absolute snapshot path
	name  string // its top-level entry name in the zip
	typ   string // restic node type from the parent's listing; "" until found
	mtime time.Time
}

// dumpMulti streams req.Paths as one zip into dst (see the file comment). ctx
// already carries the dump timeout and t the unlocked repository; the caller
// holds the stream slot.
func (w *Worker) dumpMulti(ctx context.Context, t *target, req adminpath.DumpRequest, dst io.Writer) (adminpath.DumpResult, error) {
	var sels []*selection
	for _, p := range adminpath.CollapsePaths(req.Paths) {
		sels = append(sels, &selection{path: p})
	}
	snapID, elapsed, err := w.lookupSelections(ctx, t, req.SnapshotID, sels)
	if err != nil {
		return adminpath.DumpResult{}, err
	}
	if !nameSelections(sels) {
		return adminpath.DumpResult{}, w.unsafeNameErr(t)
	}

	streamCtx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	out := &cancelWriter{w: dst, cancel: func() { stop(errDestFailed) }}
	b := &zipBuilder{zw: zip.NewWriter(out), skipped: map[string]int{}}
	for _, s := range sels {
		if kind := nodeSkipKind(s.typ); kind != "" {
			b.skipped[kind]++
			continue
		}
		var (
			cmd     restic.Command
			berr    error
			consume func(io.Reader) error
		)
		if s.typ == "dir" {
			if err := b.dir(s.name, s.mtime); err != nil {
				return adminpath.DumpResult{}, w.streamErr(ctx)
			}
			cmd, berr = restic.DumpTarStdout(snapID, s.path, w.common())
			sel := s
			consume = func(r io.Reader) error { return b.addTar(r, sel) }
		} else {
			fw, err := b.file(s.name, s.mtime)
			if err != nil {
				return adminpath.DumpResult{}, w.streamErr(ctx)
			}
			cmd, berr = restic.DumpFileStdout(snapID, s.path, w.common())
			consume = func(r io.Reader) error { _, cerr := io.Copy(fw, r); return cerr }
		}
		if berr != nil {
			return adminpath.DumpResult{}, adminpath.NewError(adminpath.CodeInvalidRequest, "")
		}
		res, err := w.dumpRun(streamCtx, t, cmd, out, consume)
		elapsed += res.Elapsed
		if err != nil {
			return adminpath.DumpResult{}, err
		}
	}
	// Every run exited 0 and every tar ended cleanly: only now does the
	// archive get its central directory.
	if err := b.finish(w.clock().UTC()); err != nil {
		return adminpath.DumpResult{}, w.streamErr(ctx)
	}
	return adminpath.DumpResult{ResultMeta: okMeta(elapsed)}, nil
}

// lookupSelections fills each selection's node type and mtime from one `ls`
// of its parent folder (one restic run per distinct parent, in the order the
// parents first appear), so the worker never trusts the caller about what a
// path is. It returns the snapshot id every later run must use: the full id
// restic reported, so a "latest" that moves during a long download (a backup
// landing) cannot mix two snapshots into one zip. A selection that is not in
// the snapshot is not_found, before any byte is streamed.
func (w *Worker) lookupSelections(ctx context.Context, t *target, snapID string, sels []*selection) (string, time.Duration, error) {
	var parents []string
	byParent := map[string]map[string]*selection{}
	for _, s := range sels {
		p := path.Dir(s.path)
		if byParent[p] == nil {
			byParent[p] = map[string]*selection{}
			parents = append(parents, p)
		}
		byParent[p][s.path] = s
	}
	var elapsed time.Duration
	for _, parent := range parents {
		want := byParent[parent]
		cmd, err := restic.Ls(snapID, parent, w.common())
		if err != nil {
			return "", elapsed, adminpath.NewError(adminpath.CodeInvalidRequest, "")
		}
		resolved := ""
		res, err := w.exec(ctx, t, cmd, w.cfg.Timeouts.Ls, func(r io.Reader) error {
			dec := restic.NewDecoder(r)
			for {
				msg, derr := dec.Next()
				if derr != nil {
					if errors.Is(derr, io.EOF) {
						return nil
					}
					return derr
				}
				switch m := msg.(type) {
				case restic.LsSnapshot:
					resolved = m.ID
				case restic.Node:
					if s := want[m.Path]; s != nil && s.typ == "" {
						s.typ, s.mtime = m.Type, m.Mtime
						if s.typ == "" {
							s.typ = "other"
						}
					}
				}
			}
		})
		if err != nil {
			return "", elapsed, err
		}
		elapsed += res.Elapsed
		if err := readError(ctx, res); err != nil {
			return "", elapsed, err
		}
		if resolved != "" {
			if _, err := restic.Ls(resolved, "/", restic.Common{}); err == nil {
				snapID = resolved
			}
		}
	}
	for _, s := range sels {
		if s.typ == "" {
			return "", elapsed, adminpath.NewError(adminpath.CodeNotFound, "")
		}
	}
	return snapID, elapsed, nil
}

// nameSelections gives each selection its top-level entry name: its base name,
// with " (2)", " (3)", ... before the extension when an earlier selection (or
// the skipped-entries file) already holds that name, compared without case as
// Windows would on extraction. It reports false when a base name is not a safe
// entry name.
func nameSelections(sels []*selection) bool {
	used := map[string]bool{strings.ToLower(skippedEntryName): true}
	for _, s := range sels {
		base := path.Base(s.path)
		if !safeEntryName(base) {
			return false
		}
		name := base
		for n := 2; used[strings.ToLower(name)]; n++ {
			name = numbered(base, n, s.typ != "dir")
		}
		used[strings.ToLower(name)] = true
		s.name = name
	}
	return true
}

// numbered is base with " (n)" added: before the extension of a file whose
// name has one ("report (2).txt"), else at the end ("Documents (2)").
func numbered(base string, n int, isFile bool) string {
	suffix := fmt.Sprintf(" (%d)", n)
	if ext := path.Ext(base); isFile && ext != "" && ext != base {
		return strings.TrimSuffix(base, ext) + suffix + ext
	}
	return base + suffix
}

// safeEntryName is the zip-slip guard: a name is safe when it is relative, is
// already clean (no empty, "." or ".." component, no trailing "/"), and holds
// no control character, backslash or colon, so no extractor on Windows or
// Linux can read it as an absolute path, a drive ("C:"), an alternate data
// stream or a step up out of the folder it extracts into. Windows file names
// cannot hold a backslash or a colon, so nothing legitimate is refused.
func safeEntryName(name string) bool {
	if name == "" || name[0] == '/' || path.Clean(name) != name {
		return false
	}
	for _, c := range strings.Split(name, "/") {
		if c == "" || c == "." || c == ".." {
			return false
		}
	}
	for i := 0; i < len(name); i++ {
		if b := name[i]; b < 0x20 || b == 0x7f || b == '\\' || b == ':' {
			return false
		}
	}
	return true
}

// unsafeNameErr logs the zip-slip refusal (a class, never the name) and returns
// the error the download fails with.
func (w *Worker) unsafeNameErr(t *target) error {
	w.log.Warn("dump refused: an archive entry name could escape its folder; nothing usable was sent",
		attrDevice(t.dev.ID), attrClass("dump_unsafe_name"))
	return adminpath.NewError(adminpath.CodeResticFailed, restic.ClassGuardRefused)
}

// dumpRun runs one dump child under the idle watchdog and turns its end into
// the download's error, or nil when restic exited 0 and consume read its whole
// output. ctx is the download's stream context (cancelled when dst fails); out
// is the destination guard whose failed flag tells a gone browser from a
// failed run.
func (w *Worker) dumpRun(ctx context.Context, t *target, cmd restic.Command, out *cancelWriter, consume func(io.Reader) error) (execResult, error) {
	limit := dumpIdleLimit()
	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	res, err := w.exec(runCtx, t, cmd, w.cfg.Timeouts.Dump, func(r io.Reader) error {
		ir := newIdleReader(r, limit, func() { cancel(errDumpIdle) })
		defer ir.stop()
		cerr := consume(ir)
		if errors.Is(cerr, errUnsafeName) {
			cancel(cerr) // nothing more of this archive is wanted: stop restic now
			return cerr
		}
		// Read to the end of restic's stdout, which comes when it exits, still
		// under the watchdog: after a complete tar restic only removes its lock,
		// and a restic that hangs there is stopped like any stall. After a tar
		// that ended early or a write that failed, restic's own exit code, not
		// a half-read pipe, then says what happened.
		_, _ = io.Copy(io.Discard, ir)
		return cerr
	})
	cause := context.Cause(runCtx)
	parentDone := ctx.Err() != nil && !errors.Is(context.Cause(ctx), errDestFailed)
	switch {
	case out.failed:
		if parentDone {
			return res, ctx.Err()
		}
		return res, adminpath.NewError(adminpath.CodeUnavailable, "")
	case parentDone:
		return res, ctx.Err() // the console went away, or the dump timeout passed
	case err != nil:
		return res, err // the child could not run at all
	case errors.Is(cause, errDumpIdle):
		return res, w.dumpIdleStopped(t, res, limit)
	case errors.Is(cause, errUnsafeName):
		return res, w.unsafeNameErr(t)
	}
	return res, readError(ctx, res)
}

// dumpIdleStopped handles a dump child the idle watchdog stopped: one log line
// (a class, never a path), then `restic unlock` through the admin path in the
// background, because a killed restic leaves its lock object in the repository
// (P22 spike, item 6) and the download's answer must not wait for the cleanup.
// The unlock removes stale locks only, so it never touches a live run's lock;
// it is recorded as a console run like every unlock (runs.go). The error names
// the storage when restic's stderr did, else a timeout.
func (w *Worker) dumpIdleStopped(t *target, res execResult, limit time.Duration) error {
	deviceID := t.dev.ID
	class := res.errorClass()
	w.log.Warn("dump stopped: restic sent no data within the idle limit; removing its stale lock",
		attrDevice(deviceID), attrClass("dump_idle"), attrElapsed(limit))
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), w.cfg.Timeouts.Unlock+closeTimeout)
		defer cancel()
		ur, err := w.unlock(ctx, deviceID, false, triggerManual)
		if err != nil || ur.Exit != 0 {
			w.log.Warn("stale lock not removed after a stalled dump; the scheduler's unlock step retries it",
				attrDevice(deviceID), attrClass("dump_idle_unlock"), attrExit(ur.Exit))
		}
	}()
	if storageClass(class) {
		return adminpath.NewError(adminpath.CodeStorageUnreachable, class)
	}
	return adminpath.NewError(adminpath.CodeTimeout, "")
}

// idleReader is the idle watchdog on a child's stdout: onIdle runs when a Read
// has waited limit without getting a byte. The timer runs only inside Read, so
// the time the caller spends writing what it read (a slow browser) never
// counts.
type idleReader struct {
	r     io.Reader
	limit time.Duration
	t     *time.Timer
}

func newIdleReader(r io.Reader, limit time.Duration, onIdle func()) *idleReader {
	t := time.AfterFunc(limit, onIdle)
	t.Stop()
	return &idleReader{r: r, limit: limit, t: t}
}

func (i *idleReader) Read(p []byte) (int, error) {
	i.t.Reset(i.limit)
	n, err := i.r.Read(p)
	i.t.Stop()
	return n, err
}

func (i *idleReader) stop() { i.t.Stop() }

// zipBuilder writes the several-paths zip.
type zipBuilder struct {
	zw      *zip.Writer
	skipped map[string]int
}

// dir adds a folder entry ("name/").
func (b *zipBuilder) dir(name string, mtime time.Time) error {
	h := &zip.FileHeader{Name: name + "/", Method: zip.Store}
	if !mtime.IsZero() {
		h.Modified = mtime
	}
	_, err := b.zw.CreateHeader(h)
	return err
}

// file adds a file entry and returns the writer for its bytes.
func (b *zipBuilder) file(name string, mtime time.Time) (io.Writer, error) {
	h := &zip.FileHeader{Name: name, Method: zip.Deflate}
	if !mtime.IsZero() {
		h.Modified = mtime
	}
	return b.zw.CreateHeader(h)
}

// addTar re-emits one folder's `dump -a tar` stream into the zip. Every entry
// must lie under the selected folder (restic names it by its full snapshot path
// without the leading "/"); it is renamed to the selection's entry name plus
// its path below the folder, and that name must pass safeEntryName. A failure
// of either is errUnsafeName, which refuses the whole download. Regular files
// and folders are written; everything else is counted.
func (b *zipBuilder) addTar(r io.Reader, s *selection) error {
	prefix := strings.TrimPrefix(s.path, "/")
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		switch {
		case err == io.EOF:
			return nil
		case errors.Is(err, tar.ErrInsecurePath):
			return errUnsafeName
		case err != nil:
			return err
		}
		raw := strings.TrimSuffix(hdr.Name, "/")
		if raw == prefix {
			if hdr.Typeflag == tar.TypeDir {
				continue // the folder's own entry is already written
			}
			return errUnsafeName
		}
		rest, ok := strings.CutPrefix(raw, prefix+"/")
		if !ok {
			return errUnsafeName
		}
		name := s.name + "/" + rest
		if !safeEntryName(name) {
			return errUnsafeName
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := b.dir(name, hdr.ModTime); err != nil {
				return err
			}
		case tar.TypeReg:
			fw, err := b.file(name, hdr.ModTime)
			if err != nil {
				return err
			}
			if _, err := io.Copy(fw, tr); err != nil {
				return err
			}
		default:
			b.skipped[tarSkipKind(hdr.Typeflag)]++
		}
	}
}

// finish writes CRYOBACKUP-SKIPPED.txt when anything was left out, then the
// central directory.
func (b *zipBuilder) finish(now time.Time) error {
	total := 0
	for _, n := range b.skipped {
		total += n
	}
	if total > 0 {
		var sb strings.Builder
		fmt.Fprintf(&sb, "CryoBackup left %d item(s) out of this download because they are not regular files or folders.\r\n", total)
		sb.WriteString("Their names are not listed here.\r\n\r\n")
		for _, k := range skipKinds {
			if n := b.skipped[k.key]; n > 0 {
				fmt.Fprintf(&sb, "%s: %d\r\n", k.label, n)
			}
		}
		fw, err := b.file(skippedEntryName, now)
		if err != nil {
			return err
		}
		if _, err := io.WriteString(fw, sb.String()); err != nil {
			return err
		}
	}
	return b.zw.Close()
}
