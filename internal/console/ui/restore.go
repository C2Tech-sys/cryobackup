package ui

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/health"
	"github.com/c2tech-sys/cryobackup/internal/console/httpx"
	"github.com/c2tech-sys/cryobackup/internal/console/restores"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
)

// SnapOption is one entry of the snapshot select.
type SnapOption struct {
	ID       string
	Label    string
	Selected bool
}

// Crumb is one segment of the browse path. The browse listing is the single
// place the UI shows names from a client machine (components §0.3, §8.5): it is
// rendered once, never cached, never logged, and carried in POST bodies, never
// in a URL.
type Crumb struct {
	Label string
	Path  string
}

// Entry is one row of the listing.
type Entry struct {
	Name   string
	Path   string
	Kind   string // dir | file | other
	Size   string
	When   string
	Select bool
}

// Banner is an inline banner (components §9.3).
type Banner struct {
	Class string
	Title string
	Text  string
}

// Step is one step of the wizard bar.
type Step struct {
	N     int
	Label string
	State string // done | on | todo
}

// RestoreData is the restore wizard (B17).
type RestoreData struct {
	Page
	DeviceID   string
	Host       string
	Customer   string
	Snapshots  []SnapOption
	SnapshotID string
	Dir        string
	Crumbs     []Crumb
	Entries    []Entry
	Truncated  bool
	Error      *Banner
	Blocked    *Banner
	Steps      []Step
	NoSnaps    bool
	CanSubmit  bool
	// NeedCode asks for an authenticator code (the session's last check is older
	// than MFAWindow minutes, ADR 0027 ruling C5); KeepPath carries the file the
	// operator chose through that round trip, in the POST body only.
	NeedCode  bool
	KeepPath  string
	MFAWindow int
}

func (s *Service) restoreBase(ctx context.Context, p *auth.Principal, f health.Fleet, id string) (RestoreData, int, bool) {
	dev, err := s.Store.GetDevice(ctx, p.Scope, id)
	if err != nil {
		return RestoreData{}, http.StatusNotFound, false
	}
	custName := ""
	for _, c := range f.View.Customers {
		if c.ID == dev.CustomerID {
			custName = c.Name
		}
	}
	d := RestoreData{Page: s.page(ctx, p, f, "Restore files", "fleet", "/ui/events"), DeviceID: id, Host: dev.Hostname, Customer: custName}
	d.Dir = "/"
	switch esc, err := s.Store.ActiveEscrow(ctx, p.Scope, id); {
	case dev.RetiredAt != nil:
		d.Blocked = &Banner{ClassWarn, "This device is retired", "A retired device cannot be restored from the console."}
	case err == nil && esc.Custody != "escrowed":
		d.Blocked = &Banner{ClassWarn, "Customer-held key: no technician restore", "CryoBackup never stores this key, so the console cannot open the repository. Restore on the machine with the customer's key."}
	case s.MasterLoaded == nil || !s.MasterLoaded():
		d.Blocked = &Banner{ClassCrit, "Master key unavailable", "Restores are disabled until the master key is loaded (banner C12)."}
	}
	snaps, err := s.Store.ListSnapshots(ctx, p.Scope, id)
	if err != nil {
		d.Error = &Banner{ClassCrit, "Snapshots unavailable", "The snapshot list could not be loaded."}
	}
	sort.Slice(snaps, func(i, j int) bool { return snaps[i].TakenAt.After(snaps[j].TakenAt) })
	c := s.fcAt(ctx, f.Computed)
	for _, sn := range snaps {
		d.Snapshots = append(d.Snapshots, SnapOption{ID: sn.SnapshotID, Label: sn.TakenAt.In(c.loc()).Format("Mon 2 Jan 3:04 PM") + " · " + sn.SnapshotID[:8]})
	}
	d.NoSnaps = len(d.Snapshots) == 0
	return d, 0, true
}

func (d *RestoreData) steps() {
	st := func(n int, label, state string) Step { return Step{N: n, Label: label, State: state} }
	switch {
	case d.SnapshotID == "":
		d.Steps = []Step{st(1, "Snapshot", "on"), st(2, "Browse and select", "todo"), st(3, "Mode", "todo"), st(4, "Progress", "todo")}
	default:
		d.Steps = []Step{st(1, "Snapshot", "done"), st(2, "Browse and select", "on"), st(3, "Mode", "todo"), st(4, "Progress", "todo")}
	}
}

func crumbs(dir string) []Crumb {
	out := []Crumb{{Label: "Snapshot root", Path: "/"}}
	cur := ""
	for _, seg := range strings.Split(strings.Trim(dir, "/"), "/") {
		if seg == "" {
			continue
		}
		cur += "/" + seg
		out = append(out, Crumb{Label: seg, Path: cur})
	}
	return out
}

func (s *Service) browse(ctx context.Context, d *RestoreData, p *auth.Principal, snapshot, dir string) {
	d.SnapshotID = snapshot
	for i := range d.Snapshots {
		d.Snapshots[i].Selected = d.Snapshots[i].ID == snapshot
	}
	dir = cleanDir(dir)
	d.Dir, d.Crumbs = dir, crumbs(dir)
	if d.Blocked != nil || s.Backend == nil {
		return
	}
	res, err := s.Backend.Ls(ctx, adminpath.LsRequest{DeviceRequest: adminpath.DeviceRequest{DeviceID: d.DeviceID, RequestID: httpx.RequestID(ctx)},
		SnapshotID: snapshot, Dir: dir, Limit: 500})
	if err != nil {
		d.Error = lsBanner(err)
		return
	}
	c := fmtCtx{Loc: s.loc(ctx)}
	for _, n := range res.Nodes {
		if clean := cleanDir(n.Path); clean == dir && n.Type == "dir" {
			continue // restic prints the directory node itself
		}
		e := Entry{Name: n.Name, Path: n.Path, When: n.Mtime.In(c.loc()).Format("2006-01-02 3:04 PM")}
		if n.Mtime.IsZero() {
			e.When = ""
		}
		switch n.Type {
		case "dir":
			e.Kind = "dir"
		case "file":
			e.Kind, e.Size, e.Select = "file", byteSize(n.Size), true
		default:
			e.Kind = "other"
		}
		d.Entries = append(d.Entries, e)
	}
	sort.SliceStable(d.Entries, func(i, j int) bool {
		if (d.Entries[i].Kind == "dir") != (d.Entries[j].Kind == "dir") {
			return d.Entries[i].Kind == "dir"
		}
		return strings.ToLower(d.Entries[i].Name) < strings.ToLower(d.Entries[j].Name)
	})
	d.Truncated = res.Truncated
	d.CanSubmit = true
}

func cleanDir(dir string) string {
	if dir == "" || dir[0] != '/' || len(dir) > 4096 || strings.ContainsAny(dir, "\x00\r\n") {
		return "/"
	}
	return path.Clean(dir)
}

// lsBanner turns a failed browse into a path-free banner: the fixed sentence of
// the admin-path error code, nothing the worker said.
func lsBanner(err error) *Banner {
	var ae *adminpath.Error
	switch {
	case errors.As(err, &ae):
		return &Banner{ClassCrit, "Browsing is unavailable", adminpath.Message(ae.Code)}
	case errors.Is(err, adminpath.ErrUnavailable):
		return &Banner{ClassCrit, "Browsing is unavailable", "The maintenance worker is not running or not reachable (banner C07)."}
	case errors.Is(err, context.DeadlineExceeded):
		return &Banner{ClassCrit, "Browsing timed out", "The repository did not answer in time. Try again."}
	}
	return &Banner{ClassCrit, "Browsing is unavailable", "An unexpected error occurred while listing the snapshot."}
}

func (s *Service) restorePage(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	f, err := s.Fleet.Fleet(r.Context(), p.Scope)
	if err != nil {
		s.render(w, r, http.StatusServiceUnavailable, "error", "base", errorData(s.fallbackPage(r.Context(), p), 503, "Restore unavailable", "The fleet could not be loaded. Try again shortly."))
		return
	}
	d, _, ok := s.restoreBase(r.Context(), p, f, r.PathValue("id"))
	if !ok {
		s.notFound(w, r, p, "This device")
		return
	}
	if len(d.Snapshots) > 0 && d.Blocked == nil {
		s.browse(r.Context(), &d, p, d.Snapshots[0].ID, "/")
	}
	d.steps()
	s.render(w, r, http.StatusOK, "restore", "base", d)
}

// restorePost handles browse (navigate one level, change snapshot) and submit.
// The path travels only in this POST body and in the restore request, which
// stores it encrypted and clears it when the restore expires.
func (s *Service) restorePost(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "The form could not be read.", http.StatusBadRequest)
		return
	}
	f, err := s.Fleet.Fleet(r.Context(), p.Scope)
	if err != nil {
		http.Error(w, "The fleet could not be loaded.", http.StatusServiceUnavailable)
		return
	}
	id := r.PathValue("id")
	d, _, ok := s.restoreBase(r.Context(), p, f, id)
	if !ok {
		s.notFound(w, r, p, "This device")
		return
	}
	snapshot := r.PostFormValue("snapshot")
	valid := false
	for _, so := range d.Snapshots {
		valid = valid || so.ID == snapshot
	}
	if !valid {
		if len(d.Snapshots) == 0 {
			d.steps()
			s.render(w, r, http.StatusOK, "restore", "base", d)
			return
		}
		snapshot = d.Snapshots[0].ID
	}
	action := r.PostFormValue("action")
	if action != "submit" {
		dir := r.PostFormValue("dir")
		if action == "snapshot" {
			dir = "/"
		}
		s.browse(r.Context(), &d, p, snapshot, dir)
		d.steps()
		s.renderRestore(w, r, http.StatusOK, d)
		return
	}
	// Every restore needs a fresh MFA check (ADR 0027 ruling C5), checked first,
	// before any worker listing. A session that is not fresh goes through the
	// console's step-up (the purge dialogs' flow, purge_adr19.go stepUp: POST
	// /api/v1/auth/step-up with the authenticator code the form carries in
	// "code"); without a code, or with one the step-up refuses, the wizard
	// answers 403 with the reason and nothing is written or asked of the worker.
	// The Actor is built once, here, and the same value gates this page and goes
	// to the service, so the page cannot pass a check the service would refuse.
	// refuse answers a refusal with its own banner; it replaces the page's
	// standing notice (a retired or customer-held device), so the operator reads
	// the rule that applied to this submit.
	refuse := func(status int, b *Banner) {
		d.Error = b
		if d.Blocked != nil {
			d.Blocked = b
		}
		s.renderRestore(w, r, status, d)
	}
	// askCode sets the page up for the step-up without a worker call: the chosen
	// snapshot and folder stay selected, the chosen file rides along as KeepPath,
	// and the form shows the authenticator-code field (restore.html restore_box).
	askCode := func() {
		d.SnapshotID = snapshot
		for i := range d.Snapshots {
			d.Snapshots[i].Selected = d.Snapshots[i].ID == snapshot
		}
		d.Dir = cleanDir(r.PostFormValue("cur"))
		d.Crumbs = crumbs(d.Dir)
		if p := r.PostFormValue("path"); restores.ValidPath(p) {
			d.KeepPath = p
		}
		d.NeedCode, d.MFAWindow, d.CanSubmit = true, int(auth.FreshMFAWindow/time.Minute), true
		d.steps()
	}
	if code := strings.ReplaceAll(strings.TrimSpace(r.PostFormValue("code")), " ", ""); code != "" {
		refusal, done := s.stepUp(w, r, p, code)
		if done {
			return
		}
		if refusal != nil {
			askCode()
			refuse(refusal.status, &Banner{refusal.Note.Class, refusal.Note.Title, refusal.Note.Text})
			return
		}
	}
	who := restores.Actor{AdminID: p.Admin.ID, Username: p.Admin.Username, IP: httpx.ClientIP(r), Method: "web",
		FreshMFA: s.Auth.FreshMFA(p, s.Auth.Now())}
	if !who.FreshMFA {
		askCode()
		refuse(http.StatusForbidden, &Banner{ClassWarn, "A recent MFA check is required",
			"Every restore needs an authenticator check from the last " + strconv.Itoa(int(auth.FreshMFAWindow/time.Minute)) +
				" minutes on this session. Enter the current code from your authenticator app below and continue; a recovery code is not accepted."})
		return
	}
	filePath := r.PostFormValue("path")
	s.browse(r.Context(), &d, p, snapshot, r.PostFormValue("cur"))
	d.steps()
	if !restores.ValidPath(filePath) {
		d.Error = &Banner{ClassWarn, "Choose a file first", "Select one file in the listing, then prepare the zip."}
		s.renderRestore(w, r, http.StatusUnprocessableEntity, d)
		return
	}
	rr, err := s.Restores.Create(r.Context(), who, p.Scope, id, snapshot, filePath)
	if err != nil {
		status, banner, known := restoreRefusal(err)
		switch {
		case known:
			d.Error = banner
			if errors.Is(err, restores.ErrStepUp) {
				d.NeedCode, d.MFAWindow, d.KeepPath = true, int(auth.FreshMFAWindow/time.Minute), filePath
			}
		case errors.Is(err, store.ErrAudit):
			s.log().Error("restore: not created, not audited", logx.Class("audit"), logx.Err(err))
			status, d.Error = http.StatusInternalServerError, &Banner{ClassCrit, "The restore was not started", "Its audit record could not be written (NOT AUDITED). Nothing was changed. Check the console database and try again."}
		default:
			s.log().Error("restore: create failed", logx.Err(err))
			status, d.Error = http.StatusInternalServerError, &Banner{ClassCrit, "The restore could not be started", "Try again shortly."}
		}
		refuse(status, d.Error)
		return
	}
	target := "/restores/" + rr.ID
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", target)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// restoreRefusal is the page of each rule a restore create can refuse by: its
// HTTP status and its own sentence (fix E4). known is false for an error that is
// a fault, not a rule (the caller logs it and answers 500).
func restoreRefusal(err error) (status int, b *Banner, known bool) {
	switch {
	case errors.Is(err, restores.ErrStepUp):
		return http.StatusForbidden, &Banner{ClassWarn, "A recent MFA check is required", "Enter the current code from your authenticator app below and continue."}, true
	case errors.Is(err, restores.ErrCustomerHeld):
		return http.StatusConflict, &Banner{ClassWarn, "Customer-held key", "No technician restore for a customer-held key."}, true
	case errors.Is(err, restores.ErrDevicePurging):
		return http.StatusConflict, &Banner{ClassWarn, "This device is being purged", "Its backups are being removed, so nothing can be restored from it."}, true
	case errors.Is(err, restores.ErrRetiredDownloadOnly):
		return http.StatusConflict, &Banner{ClassWarn, "This device is retired: download only", "A retired device can be downloaded from as a zip, not restored to."}, true
	case errors.Is(err, restores.ErrAgentTooOld):
		return http.StatusConflict, &Banner{ClassWarn, "The agent on this device is too old", "Restoring to the device needs agent " + restores.MinDeviceRestoreAgent + " or newer. Download as zip instead."}, true
	case errors.Is(err, restores.ErrRestoreActive):
		return http.StatusConflict, &Banner{ClassWarn, "A restore is already active for this device", "Wait for it to finish, or download as zip."}, true
	case errors.Is(err, restores.ErrMasterKey):
		return http.StatusServiceUnavailable, &Banner{ClassCrit, "Master key unavailable", "Restores are disabled until the master key is loaded."}, true
	case errors.Is(err, restores.ErrInvalid):
		return http.StatusBadRequest, &Banner{ClassWarn, "That selection is not valid", "Pick a file from the current listing and try again."}, true
	case errors.Is(err, store.ErrNotFound):
		return http.StatusNotFound, &Banner{ClassWarn, "Not found", "The device or snapshot is no longer available."}, true
	}
	return 0, nil, false
}
func (s *Service) renderRestore(w http.ResponseWriter, r *http.Request, status int, d RestoreData) {
	if r.Header.Get("HX-Request") == "true" {
		s.render(w, r, status, "restore", "restore_box", d)
		return
	}
	s.render(w, r, status, "restore", "base", d)
}

// ---- restore status ----

// RestoreView is the progress and download page of one restore.
type RestoreView struct {
	ID          string
	DeviceID    string
	Host        string
	Status      Chip
	Message     string
	Bytes       string
	CanDownload bool
	CSRF        string
	Poll        bool
	Expires     string
	Created     string
	Steps       []Step
	Failed      bool
}

// RestoreStatusData is GET /restores/{id}.
type RestoreStatusData struct {
	Page
	R RestoreView
}

func failureMessage(class string) string {
	switch class {
	case "network", "storage", "storage_unreachable", "unavailable", "timeout":
		return "The storage or the maintenance worker could not be reached. Nothing was restored. Try again once the conditions above clear."
	case "wrong_password", "auth":
		return "The repository key did not open the repository. Nothing was restored. The escrow copy may need verification."
	case "repo_missing":
		return "The device's repository does not exist yet. Nothing was restored."
	case "not_found":
		return "The file or snapshot was not found in the repository. Nothing was restored."
	case "custody_customer_held":
		return "The device's key is held by the customer. No technician restore is possible."
	case "cancelled":
		return "The restore was cancelled because the console was stopping. Nothing was restored."
	case "disconnected":
		return "The download was interrupted before it finished. Nothing was kept on the server; download again to start over."
	case "artifact_missing":
		return "The prepared zip is no longer on the server. Start the restore again."
	}
	return "The restore failed. Nothing was restored. The request id in the console log identifies it."
}

func (s *Service) restoreView(ctx context.Context, p *auth.Principal, rr store.RestoreRequest, host string) RestoreView {
	c := s.fc(ctx)
	v := RestoreView{CSRF: p.CSRF, ID: rr.ID, DeviceID: rr.DeviceID, Host: host, Created: rr.CreatedAt.In(c.loc()).Format("Mon 3:04:05 PM"), Expires: rr.ExpiresAt.In(c.loc()).Format("Mon 2 Jan 3:04 PM")}
	step := func(on int) []Step {
		out := []Step{{1, "Snapshot", "done"}, {2, "Browse and select", "done"}, {3, "Mode", "done"}, {4, "Progress", "on"}}
		if on == 5 {
			out[3].State = "done"
		}
		return out
	}
	v.Steps = step(4)
	switch rr.Status {
	case "queued":
		v.Status, v.Poll = Chip{ID: "q", Label: "QUEUED", Class: ClassInfo, Pulse: true}, true
		v.Message = "Waiting for the maintenance worker to start. This page refreshes by itself."
	case "running":
		v.Status, v.Poll = Chip{ID: "q", Label: "RUNNING", Class: ClassInfo, Pulse: true}, true
		v.Message = "Preparing the zip from the snapshot. Large files take a while. This page refreshes by itself."
	case "ready", "delivered":
		// ADR 0008: nothing is prepared or stored; the zip is built from the
		// snapshot while it downloads, so its size is known only afterwards.
		live := s.Restores != nil && s.Restores.Downloadable(rr)
		v.Steps = step(5)
		if rr.Bytes != nil {
			v.Bytes = byteSize(uint64(*rr.Bytes))
		}
		until := rr.ExpiresAt.In(c.loc()).Format("Mon 3:04 PM")
		if rr.Status == "ready" {
			v.Status = Chip{ID: "q", Label: "READY", Class: ClassOK}
			v.Message = "Ready to download. The zip streams straight from the snapshot: nothing is stored on the server, and a dropped download starts again from the beginning. The link works once; a new one can be made until " + until + "."
		} else {
			v.Status = Chip{ID: "q", Label: "DELIVERED", Class: ClassNeutral}
			v.Message = "The zip was downloaded. Nothing was kept on the server; you can download it again until " + until + ", and it streams from the snapshot again."
		}
		if rr.ErrorClass != "" {
			v.Message = "The last download did not finish (class: " + rr.ErrorClass + "). " + v.Message
		}
		if !live {
			v.Message = "This restore has expired. Start another restore if you need the file again."
		}
		// No token in the page: the button is a POST that mints one and redirects.
		v.CanDownload = live && p.Role >= auth.RoleOperator
	case "failed":
		v.Status, v.Failed = Chip{ID: "q", Label: "FAILED", Class: ClassCrit}, true
		v.Message = failureMessage(rr.ErrorClass)
		if rr.ErrorClass != "" {
			v.Message += " Class: " + rr.ErrorClass + "."
		}
	default:
		v.Status = Chip{ID: "q", Label: strings.ToUpper(rr.Status), Class: ClassNeutral}
		v.Message = "This restore is no longer available. Start another restore if you need the file again."
		if rr.Status == "purged" || rr.Status == "expired" {
			v.Steps = step(5)
		}
	}
	return v
}

func (s *Service) loadRestore(w http.ResponseWriter, r *http.Request, p *auth.Principal) (store.RestoreRequest, string, bool) {
	rr, err := s.Store.GetRestoreRequest(r.Context(), p.Scope, r.PathValue("id"))
	if err != nil {
		s.notFound(w, r, p, "This restore")
		return store.RestoreRequest{}, "", false
	}
	host := ""
	if d, err := s.Store.GetDevice(r.Context(), p.Scope, rr.DeviceID); err == nil {
		host = d.Hostname
	}
	return rr, host, true
}

func (s *Service) restoreStatus(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	rr, host, ok := s.loadRestore(w, r, p)
	if !ok {
		return
	}
	f, err := s.Fleet.Fleet(r.Context(), p.Scope)
	if err != nil {
		s.render(w, r, http.StatusServiceUnavailable, "error", "base", errorData(s.fallbackPage(r.Context(), p), 503, "Restore unavailable", "The fleet could not be loaded. Try again shortly."))
		return
	}
	s.render(w, r, http.StatusOK, "restore_status", "base", RestoreStatusData{Page: s.page(r.Context(), p, f, "Restore", "fleet", "/ui/events"), R: s.withBusyNotice(r, s.restoreView(r.Context(), p, rr, host))})
}

// noticeBusy is adminapi.BusyNotice: the admin API sends a browser back to the
// restore page with ?notice=busy&at=<unix seconds> when every restore stream
// is in use.
const noticeBusy = "busy"

// busyMessage is what the restore page says then. The console does not know
// the worker's stream budget (CRYO_MAINTAIN_MAX_STREAMS), so it names no number.
const busyMessage = "Other restores are already downloading. Try again in a moment."

// busyNoticeTTL is how long the notice stays on the page and its live part.
const busyNoticeTTL = time.Minute

// withBusyNotice puts the busy notice in front of the message while it is
// current: the download was refused before it began, nothing was recorded or
// spent. The page reads it from its own URL; the live partial from the URL of
// the page it sits on (htmx's HX-Current-URL), so a refresh keeps it until
// the user acts (a new page) or busyNoticeTTL has passed.
func (s *Service) withBusyNotice(r *http.Request, v RestoreView) RestoreView {
	if !v.CanDownload {
		return v
	}
	q := r.URL.Query()
	if q.Get("notice") == "" {
		if cur := r.Header.Get("HX-Current-URL"); cur != "" {
			if u, err := url.Parse(cur); err == nil && u.Path == "/restores/"+v.ID {
				q = u.Query()
			}
		}
	}
	if q.Get("notice") != noticeBusy {
		return v
	}
	at, err := strconv.ParseInt(q.Get("at"), 10, 64)
	if err != nil {
		return v
	}
	if age := s.now().Sub(time.Unix(at, 0)); age < -5*time.Second || age > busyNoticeTTL {
		return v
	}
	v.Message = busyMessage + " " + v.Message
	return v
}

func (s *Service) restorePartial(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	rr, err := s.Store.GetRestoreRequest(r.Context(), p.Scope, r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	host := ""
	if d, err := s.Store.GetDevice(r.Context(), p.Scope, rr.DeviceID); err == nil {
		host = d.Hostname
	}
	s.render(w, r, http.StatusOK, "restore_status", "restore_live", s.withBusyNotice(r, s.restoreView(r.Context(), p, rr, host)))
}

// restoreDownload is POST /ui/restores/{id}/download: it mints the single-use
// token for this session and redirects to the GET URL, so a token never sits in
// page HTML waiting to be copied. The redirect is the only place it appears.
func (s *Service) restoreDownload(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	rr, err := s.Store.GetRestoreRequest(r.Context(), p.Scope, r.PathValue("id"))
	if err != nil {
		s.notFound(w, r, p, "This restore")
		return
	}
	tok, _, err := s.Restores.MintToken(rr, p.SessionID)
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	if err != nil {
		// Not downloadable (expired, failed): back to the status page, which says why.
		http.Redirect(w, r, "/restores/"+rr.ID, http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/api/v1/restores/"+rr.ID+"/download?token="+tok, http.StatusSeeOther)
}
