package ui

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/c2tech-sys/cryobackup/internal/console/adminapi"
)

// Agent downloads (S30; architecture §5.3 GET /downloads, M2).
//
// The §5 row says "Agent downloads; per-customer pre-bound page" and the release
// artifact route (GET /agent/release/artifact/...) is M11 and device-token only,
// so in M2 the console does not host or upload binaries. The page shows the
// installer the operator placed in the configured artifacts directory: its
// version, size and SHA-256 (computed here), and where to get it, so the hash
// can be compared on the endpoint and pre-filled into the RMM variables.

// MSIView is the installer as the pages show it.
type MSIView struct {
	// Found is true when an installer is in the artifacts directory.
	Found bool
	// Configured is false when no artifacts directory is set at all.
	Configured bool
	// Err is true when the directory or the file could not be read.
	Err          bool
	FileName     string
	Version      string
	VersionKnown bool
	SHA256       string
	Size         string
	Placed       string
}

func (s *Service) msiView(info adminapi.MSIInfo, err error, c fmtCtx) MSIView {
	v := MSIView{Configured: s.ArtifactsDir != ""}
	switch {
	case err == nil:
		v.Found, v.FileName, v.Version, v.VersionKnown, v.SHA256 = true, info.FileName, info.Version, info.Version != "", info.SHA256
		if info.Size < 1<<20 {
			v.Size = fmt.Sprintf("%.1f KiB", float64(info.Size)/(1<<10))
		} else {
			v.Size = fmt.Sprintf("%.1f MiB", float64(info.Size)/(1<<20))
		}
		v.Placed = c.dateClock(info.ModTime)
	case errors.Is(err, adminapi.ErrNoMSI):
	default:
		v.Err = true
	}
	return v
}

// DownloadsPage is /downloads.
type DownloadsPage struct {
	Page
	MSI           MSIView
	Notes         []Note
	ServerURL     string
	ServerProblem string
}

func (s *Service) downloadsPage(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	fleet, ok := s.fleetFor(w, r, p, "Downloads")
	if !ok {
		return
	}
	info, err := adminapi.CurrentMSI(s.ArtifactsDir)
	if err != nil && !errors.Is(err, adminapi.ErrNoMSI) {
		s.log().Error("ui: agent installer could not be read")
	}
	url, _, problem := adminapi.AgentServerURL(s.PublicURL)
	pg := DownloadsPage{Page: s.page(r.Context(), p, fleet, "Agent downloads", "downloads", ""), MSI: s.msiView(info, err, s.fc(r.Context())), Notes: s.pageNotes(), ServerURL: url, ServerProblem: problem}
	s.render(w, r, http.StatusOK, "downloads", "base", pg)
}
