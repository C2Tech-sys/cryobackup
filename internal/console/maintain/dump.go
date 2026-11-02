package maintain

import (
	"archive/zip"
	"context"
	"errors"
	"io"
	"path"
	"strings"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// Dump implements adminpath.Backend (architecture §6.3, engine §1.13, ADR 0008).
// It streams one snapshot path as a zip archive into dst and writes nothing to
// disk: restic dump writes to its stdout, and the worker copies that pipe to
// dst as it arrives.
//
// restic 0.19.1 writes a zip only for a directory: `dump -a zip` of a single
// file writes the file's raw bytes (verified live, recorded in doc.go). So the
// worker first asks `ls` what the path is. A directory is restic's own zip
// stream, copied through unchanged; a file's raw bytes are wrapped on the fly
// by archive/zip into a one-entry archive (archive/zip needs no seeking: it
// writes a data descriptor after the entry), so the result is a zip in both
// cases and a file that is itself a zip is never mistaken for the archive. The
// one-entry archive is closed (its central directory written) only after
// restic exited 0, so a failed run never ends in a well-formed zip.
//
// A failed write to dst (the console or the technician went away) cancels the
// restic run at once. Bytes and SHA256 of the result are filled in by
// adminpath.Handler from what crossed the socket.
//
// A stream holds a restic slot for as long as the browser takes, so streams
// have their own budget (Config.MaxStreams, below MaxConcurrent): one over the
// budget is refused at once with CodeBusy, never queued, and maintenance always
// keeps slots.
func (w *Worker) Dump(ctx context.Context, req adminpath.DumpRequest, dst io.Writer) (adminpath.DumpResult, error) {
	if dst == nil {
		return adminpath.DumpResult{}, adminpath.NewError(adminpath.CodeInternal, "")
	}
	select {
	case w.streams <- struct{}{}:
		defer func() { <-w.streams }()
	default:
		w.log.Warn("dump refused: every stream slot is in use",
			attrDevice(req.DeviceID), attrClass("streams_busy"), attrCount(w.cfg.MaxStreams))
		return adminpath.DumpResult{}, adminpath.NewError(adminpath.CodeBusy, "")
	}
	t, err := w.prepare(ctx, req.DeviceID)
	if err != nil {
		return adminpath.DumpResult{}, err
	}
	defer t.drop()
	ctx, cancel := context.WithTimeout(ctx, w.cfg.Timeouts.Dump)
	defer cancel()
	var elapsed time.Duration

	// What is the path? "/" of the snapshot (or of its subfolder) is a directory
	// by definition; anything else is looked up.
	snapID, sub, _ := strings.Cut(req.SnapshotID, ":")
	full := path.Join("/", sub, req.Path)
	isDir, mtime := true, time.Time{}
	if full != "/" {
		lsCmd, err := restic.Ls(snapID, full, w.common())
		if err != nil {
			return adminpath.DumpResult{}, adminpath.NewError(adminpath.CodeInvalidRequest, "")
		}
		found := false
		res, err := w.exec(ctx, t, lsCmd, w.cfg.Timeouts.Ls, func(r io.Reader) error {
			dec := restic.NewDecoder(r)
			for {
				msg, derr := dec.Next()
				if derr != nil {
					if errors.Is(derr, io.EOF) {
						return nil
					}
					return derr
				}
				if n, isNode := msg.(restic.Node); isNode && !found && n.Path == full {
					found, isDir, mtime = true, n.Type == "dir", n.Mtime
				}
			}
		})
		if err != nil {
			return adminpath.DumpResult{}, err
		}
		elapsed += res.Elapsed
		if err := readError(ctx, res); err != nil {
			return adminpath.DumpResult{}, err
		}
		if !found {
			return adminpath.DumpResult{}, adminpath.NewError(adminpath.CodeNotFound, "")
		}
	}

	// No --target: restic writes the archive to its stdout, so no file name
	// ever reaches restic.
	dumpCmd, err := restic.DumpStdout(req.SnapshotID, req.Path, w.common())
	if err != nil {
		return adminpath.DumpResult{}, adminpath.NewError(adminpath.CodeInvalidRequest, "")
	}
	// A failed write to dst cancels the run: restic must not keep reading the
	// repository for a reader that is gone.
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	out := &cancelWriter{w: dst, cancel: stop}
	var zw *zip.Writer
	if !isDir {
		entry := path.Base(req.Path)
		if entry == "." || entry == "/" || entry == "" {
			entry = "file"
		}
		zw = zip.NewWriter(out)
		hdr := &zip.FileHeader{Name: entry, Method: zip.Deflate}
		if !mtime.IsZero() {
			hdr.Modified = mtime
		}
		fw, err := zw.CreateHeader(hdr)
		if err != nil {
			return adminpath.DumpResult{}, w.streamErr(ctx)
		}
		res, err := w.exec(runCtx, t, dumpCmd, w.cfg.Timeouts.Dump, func(r io.Reader) error {
			_, cerr := io.Copy(fw, r)
			return cerr
		})
		if err != nil {
			return adminpath.DumpResult{}, w.ctxOr(ctx, err)
		}
		elapsed += res.Elapsed
		if out.failed {
			return adminpath.DumpResult{}, w.streamErr(ctx)
		}
		if err := readError(runCtx, res); err != nil {
			return adminpath.DumpResult{}, err
		}
		if err := zw.Close(); err != nil {
			return adminpath.DumpResult{}, w.streamErr(ctx)
		}
		return adminpath.DumpResult{ResultMeta: okMeta(elapsed)}, nil
	}
	res, err := w.exec(runCtx, t, dumpCmd, w.cfg.Timeouts.Dump, func(r io.Reader) error {
		_, cerr := io.Copy(out, r)
		return cerr
	})
	if err != nil {
		return adminpath.DumpResult{}, w.ctxOr(ctx, err)
	}
	elapsed += res.Elapsed
	if out.failed {
		return adminpath.DumpResult{}, w.streamErr(ctx)
	}
	if err := readError(runCtx, res); err != nil {
		return adminpath.DumpResult{}, err
	}
	return adminpath.DumpResult{ResultMeta: okMeta(elapsed)}, nil
}

// streamErr is the error of a dump whose destination failed: the caller's
// context error when it is done (the console disconnected), else unavailable.
func (w *Worker) streamErr(ctx context.Context) error {
	return w.ctxOr(ctx, adminpath.NewError(adminpath.CodeUnavailable, ""))
}

// cancelWriter forwards to w and cancels the run on the first failed write.
type cancelWriter struct {
	w      io.Writer
	cancel context.CancelFunc
	failed bool
}

func (c *cancelWriter) Write(p []byte) (int, error) {
	if c.failed {
		return 0, io.ErrClosedPipe
	}
	n, err := c.w.Write(p)
	if err != nil {
		c.failed = true
		c.cancel()
	}
	return n, err
}
