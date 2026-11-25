package maintain

import (
	"context"
	"io"
	"path"
	"strings"

	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
)

// Dump implements adminpath.Backend (architecture §6.3, engine §1.13, ADR 0008,
// ADR 0028). It streams snapshot paths as one zip archive into dst and writes
// nothing to disk: restic dump writes to its stdout, and the worker re-encodes
// that pipe into dst as it arrives (dump_multi.go).
//
// A request with Path is the download of Paths: [Path] (ADR 0028: a single
// selected folder goes the same way as several, so restic's own folder zip,
// which writes a link's target as the content of a file, is not used). The
// subfolder of a "<snapshot>:<subfolder>" id is joined in front of Path. The
// snapshot root itself ("/" with no subfolder) has no name of its own: its
// tar entries keep their snapshot paths without the leading "/", so the zip's
// top-level entries are the snapshot's roots.
//
// A failed write to dst (the console or the technician went away) cancels the
// restic run at once. A restic run that sends no byte for DumpIdleLimit is
// stopped the same way (Worker.dumpRun). Bytes and SHA256 of the result are
// filled in by adminpath.Handler from what crossed the socket; Skipped counts
// what was left out.
//
// A stream holds a restic slot for as long as the browser takes, so streams
// have their own budget (Config.MaxStreams, below MaxConcurrent): one over the
// budget is refused at once with CodeBusy, never queued, and maintenance always
// keeps slots. A download holds one stream slot for all its runs.
func (w *Worker) Dump(ctx context.Context, req adminpath.DumpRequest, dst io.Writer) (adminpath.DumpResult, error) {
	if dst == nil {
		return adminpath.DumpResult{}, adminpath.NewError(adminpath.CodeInternal, "")
	}
	// In-process callers skip the Handler's validation; the path cap and the
	// clean-path rule are what keep the archive's names safe.
	if err := req.Validate(); err != nil {
		return adminpath.DumpResult{}, adminpath.NewError(adminpath.CodeInvalidRequest, "")
	}
	snapID, sels := dumpSelections(req)
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
	return w.dumpMulti(ctx, t, snapID, sels, dst)
}

// dumpSelections turns a validated request into the snapshot id the restic
// runs use and the download's selections. Paths are collapsed (a path inside
// another selected folder is dropped). Path is cleaned and joined to the
// snapshot id's subfolder; the snapshot root is the one selection with no name.
func dumpSelections(req adminpath.DumpRequest) (string, []*selection) {
	if req.Paths != nil {
		var sels []*selection
		for _, p := range adminpath.CollapsePaths(req.Paths) {
			sels = append(sels, &selection{path: p})
		}
		return req.SnapshotID, sels
	}
	snapID, sub, _ := strings.Cut(req.SnapshotID, ":")
	full := path.Join("/", sub, req.Path)
	if full == "/" {
		return snapID, []*selection{{path: "/", root: true, typ: "dir"}}
	}
	return snapID, []*selection{{path: full}}
}

// streamErr is the error of a dump whose destination failed: the caller's
// context error when it is done (the console disconnected), else unavailable.
func (w *Worker) streamErr(ctx context.Context) error {
	return w.ctxOr(ctx, adminpath.NewError(adminpath.CodeUnavailable, ""))
}

// cancelWriter forwards to w and calls cancel on the first failed write.
type cancelWriter struct {
	w      io.Writer
	cancel func()
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
