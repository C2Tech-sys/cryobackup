package adminapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/httpx"
	"github.com/c2tech-sys/cryobackup/internal/console/restores"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// Restore creation for every mode (ADR 0027, ADR 0028, ADR 0030; ticket P22-E).
//
// POST /api/v1/devices/{id}/restores creates a download (download_zip, one
// path or up to 50) or a restore to the device (device_original, device_side).
// The route is for the operator role on a signed-in session whose last TOTP
// check is fresh (auth.RequireFreshMFA, ruling C5): an API token is never fresh
// and gets 403 mfa_step_up_required, as does a stale session, which then calls
// POST /api/v1/auth/step-up and repeats the request.
//
// Refusals, each by its own code: 400 invalid_request (shape, paths, mode,
// overwrite, an unknown snapshot), 400 with the field confirm_host_name (replace
// mode without the exact host name), 404 not_found, 409 device_purging, 409
// device_retired_download_only (a device mode on a retired device; a download
// is allowed), 409 custody_customer_held, 409 agent_too_old (a device mode on
// an agent older than restores.MinDeviceRestoreAgent), 409 device_restore_active
// (one active device restore per device), 503 master_key_unavailable, and 500
// when the audit row cannot be written (nothing is created). A device restore
// wakes the device's command long poll.

// restoreBodyMax caps the body: a selection of api.MaxRestorePathsTotalBytes of
// paths plus its JSON framing (paths hold no character JSON must escape except
// '"', which Windows forbids in a name).
const restoreBodyMax = api.MaxRestorePathsTotalBytes + 8<<10

func (s *Service) createRestore(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	var req CreateRestoreRequest
	if !decodeBodyMax(w, r, restoreBodyMax, &req) {
		return
	}
	in := restores.CreateInput{SnapshotID: req.SnapshotID, Mode: req.Mode, Overwrite: api.DeviceRestoreOverwrite(req.Overwrite), ConfirmHostName: req.ConfirmHostName}
	device := false
	switch req.Mode {
	case restores.ModeDownloadZip:
		switch {
		case req.Path != "" && req.Paths == nil:
			in.Paths = []string{req.Path}
		case req.Path == "" && len(req.Paths) > 0:
			in.Paths = req.Paths
		default:
			httpx.Problem(w, r, api.CodeInvalidRequest, 0)
			return
		}
	case restores.ModeDeviceOriginal, restores.ModeDeviceSide:
		if req.Path != "" || len(req.Paths) == 0 {
			httpx.Problem(w, r, api.CodeInvalidRequest, 0)
			return
		}
		in.Paths, device = req.Paths, true
	default:
		httpx.Problem(w, r, api.CodeInvalidRequest, 0)
		return
	}
	id := r.PathValue("id")
	who := restores.Actor{AdminID: p.Admin.ID, Username: p.Admin.Username, IP: httpx.ClientIP(r), Method: "api", FreshMFA: s.Auth.FreshMFA(p, s.Auth.Now())}
	rr, err := s.Restores.CreateRestore(r.Context(), who, p.Scope, id, in)
	if err != nil {
		s.failRestore(w, r, err)
		return
	}
	if device && s.WakeAgent != nil {
		s.WakeAgent(id)
	}
	w.Header().Set("Location", "/api/v1/restores/"+rr.ID)
	httpx.WriteJSON(w, http.StatusAccepted, s.restoreJSON(p, rr))
}

// failRestore answers the refusals of restore creation that have codes of their
// own and leaves the rest to fail.
func (s *Service) failRestore(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, restores.ErrStepUp):
		httpx.Problem(w, r, api.CodeMFAStepUpRequired, 0)
	case errors.Is(err, restores.ErrConfirmHost):
		s.confirmMismatch(w, r, "confirm_host_name", "Type the device's host name exactly as shown to replace existing files.")
	case errors.Is(err, restores.ErrAgentTooOld):
		httpx.Problem(w, r, api.CodeAgentTooOld, 0)
	case errors.Is(err, restores.ErrRetiredDownloadOnly):
		httpx.Problem(w, r, api.CodeDeviceRetiredDownloadOnly, 0)
	case errors.Is(err, restores.ErrDevicePurging):
		httpx.Problem(w, r, api.CodeDevicePurging, 0)
	case errors.Is(err, restores.ErrRestoreActive):
		httpx.Problem(w, r, api.CodeDeviceRestoreActive, 0)
	case errors.Is(err, restores.ErrRestoreExpired):
		httpx.Problem(w, r, api.CodeRestoreExpired, 0)
	default:
		s.fail(w, r, err)
	}
}

// restoreJSON is a restore as the API shows it (RestoreJSON).
func (s *Service) restoreJSON(p *auth.Principal, rr store.RestoreRequest) RestoreJSON {
	oc := restores.DescribeOutcome(rr)
	out := RestoreJSON{ID: rr.ID, DeviceID: rr.DeviceID, SnapshotID: rr.SnapshotID, Mode: rr.Mode, Status: rr.Status, ErrorClass: rr.ErrorClass,
		Outcome: oc.Text, SelectionCount: rr.SelectionCount, Bytes: rr.Bytes, Skipped: oc.Skipped, DownloadCount: rr.DownloadCount,
		CreatedAt: rr.CreatedAt, StartedAt: rr.StartedAt, FinishedAt: rr.FinishedAt, ExpiresAt: rr.ExpiresAt, PurgeAfter: rr.PurgeAfter, ComputedAt: s.now()}
	if wm, ok := restores.WireMode(rr.Mode); ok {
		out.Mode = string(wm)
		if wo, ok := restores.WireOverwrite(rr.Overwrite); ok {
			out.Overwrite = string(wo)
		}
		if rr.Status == "delivered" || rr.Status == "failed" || rr.Status == "cancelled" {
			fr, fs, br := oc.FilesRestored, oc.FilesSkipped, oc.BytesRestored
			out.FilesRestored, out.FilesSkipped, out.BytesRestored, out.Errors = &fr, &fs, &br, oc.Errors
		}
	} else {
		var pr struct {
			SHA256 string `json:"sha256"`
		}
		if len(rr.Progress) > 0 && json.Unmarshal(rr.Progress, &pr) == nil {
			out.SHA256 = pr.SHA256
		}
	}
	out.DownloadReady = s.Restores.Downloadable(rr) && p.Role >= auth.RoleOperator
	return out
}
