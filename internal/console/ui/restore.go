package ui

import (
	"context"
	"errors"
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/health"
	"github.com/c2tech-sys/cryobackup/internal/console/httpx"
	"github.com/c2tech-sys/cryobackup/internal/console/restores"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// The restore wizard (B17; ADR 0027, ADR 0028, ADR 0030; plan B.1; ticket P22-G).
//
// One page, four steps: a point in time (the snapshot) -> browse and select (checkboxes on
// files and folders) -> the destination -> review. Every step is a plain form post to
// POST /ui/devices/{id}/restore, so the wizard works with JavaScript off; htmx only swaps the
// form in place. The selection (the "basket") travels in the POST body as repeated "path"
// fields: a selected item shown in the current listing is its own checked checkbox, every other
// one rides along as a hidden field. It is never put in a URL, a cookie or browser storage, and
// the body is capped (restoreBodyMax).
//
// The browse listing, the basket and the review are the only places the console shows names from
// a client machine (components §0.3, §8.5): rendered per request, never cached (no-store), never
// logged. Browsing a snapshot is audited as restore.browse, path-free, once per admin, device and
// snapshot per hour (restores.AuditBrowse).
//
// "Continue" moves one step on; the hidden "step" field says which step the form is at. A post
// that carries no step (the single-file form of earlier versions, and the admin pages' tests of
// it) is a create: it goes straight to the restore with the mode it names, a download when it
// names none. Every create needs a fresh authenticator check (ruling C5): the review step asks
// for the code when the session's last check is older than the window.

const (
	// restoreBasketMax is the most items one restore takes after nested items are folded into
	// their selected folder (ADR 0028: a zip of up to 50 items; the wizard uses the same cap for
	// a device restore).
	restoreBasketMax = adminpath.MaxDumpPaths
	// restoreLsPage is one page of the browse listing (plan B.1: Ls pages of 500).
	restoreLsPage = 500
	// restoreBodyMax caps a wizard post (plan B.1: 64 KiB): the basket rides in it.
	restoreBodyMax = 64 << 10
	// restorePostedMax bounds how many "path" values one post may carry before it is refused.
	restorePostedMax = 2 * restoreLsPage
)

// Wizard steps (the hidden "step" field).
const (
	stepSelect = "select"
	stepDest   = "dest"
	stepReview = "review"
)

// The destination words of the form ("mode"). The service's own words are restores.Mode*.
const (
	modeZip      = "zip"
	modeOriginal = "original" // original location, existing files kept (ADR 0027 ruling C1 default)
	modeSide     = "side"     // a new folder beside, nothing existing touched (ruling C2)
	modeReplace  = "replace"  // original location, changed files replaced: the typed host name confirms
)

// SnapOption is one entry of the snapshot select.
type SnapOption struct {
	ID       string
	Label    string
	Selected bool
}

// Crumb is one segment of the browse path. The browse listing is the single place the UI shows
// names from a client machine (components §0.3, §8.5): it is rendered once, never cached, never
// logged, and carried in POST bodies, never in a URL.
type Crumb struct {
	Label string
	Path  string
}

// Entry is one row of the listing.
type Entry struct {
	Name string
	Path string
	Kind string // dir | file | other
	// What says what an "other" entry is (a link, a device), which cannot be selected.
	What string
	Size string
	When string
	// Checked: the entry is in the basket (its checkbox carries it). Covered: a folder above it
	// is in the basket, so it is included already (a checked, disabled box).
	Checked bool
	Covered bool
}

// BasketItem is one selected file or folder.
type BasketItem struct {
	Path   string
	Name   string
	Parent string
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

// ModeOpt is one destination of the destination step.
type ModeOpt struct {
	Value    string
	Title    string
	Text     string
	Checked  bool
	Disabled bool
	// Why says why a disabled destination is not offered.
	Why string
}

// ReviewView is what the review step says will happen, in plain words.
type ReviewView struct {
	Title   string
	Lines   []string
	Facts   []Fact
	Notes   []Banner
	Button  string
	Replace bool
}

// RestoreData is the restore wizard (B17).
type RestoreData struct {
	Page
	DeviceID string
	Host     string
	Customer string
	// Step is select, dest or review; Steps is the bar.
	Step  string
	Steps []Step

	Snapshots  []SnapOption
	SnapshotID string
	SnapLabel  string
	NoSnaps    bool

	Dir     string
	Crumbs  []Crumb
	Entries []Entry
	// Inside names the selected folder the current folder lies in ("" when none).
	Inside string
	// Paging of the listing: Offset of this page, Total children (0 from a worker that predates
	// paging), and the offsets of the neighbouring pages.
	Offset, Total, From, To int
	PrevOff, NextOff        int
	HasPrev, HasNext        bool
	Truncated               bool
	Listed                  bool
	// PageText says which entries this page shows ("1–500 of 2,000").
	PageText string

	Basket []BasketItem
	// Hidden are the basket paths that ride as hidden fields (every basket path not shown as an
	// enabled checkbox of this listing).
	Hidden    []string
	BasketMax int
	Over      bool

	Notice  *Banner
	Error   *Banner
	Blocked *Banner
	// BlockedStatus is the HTTP status of a create the Blocked rule refuses.
	BlockedStatus int
	// Retired: the device is retired, so the wizard offers a download only (ADR 0030).
	Retired bool

	Mode  string
	Modes []ModeOpt
	// DeviceWhy, when set, is why no destination on the device is offered.
	DeviceWhy string
	// ActiveID is the device restore that is already queued or running for this device.
	ActiveID string

	Review     *ReviewView
	ConfirmErr string

	CanSubmit bool
	// NeedCode asks for an authenticator code (the session's last check is older than MFAWindow
	// minutes, ADR 0027 ruling C5).
	NeedCode  bool
	MFAWindow int

	dev store.Device
}

// ---- the device and what it allows ----

func (s *Service) restoreBase(ctx context.Context, p *auth.Principal, f health.Fleet, id string) (RestoreData, bool) {
	dev, err := s.Store.GetDevice(ctx, p.Scope, id)
	if err != nil {
		return RestoreData{}, false
	}
	custName := ""
	for _, c := range f.View.Customers {
		if c.ID == dev.CustomerID {
			custName = c.Name
		}
	}
	d := RestoreData{Page: s.page(ctx, p, f, "Restore files", "fleet", "/ui/events"), DeviceID: id, Host: dev.Hostname, Customer: custName,
		Dir: "/", BasketMax: restoreBasketMax, MFAWindow: int(auth.FreshMFAWindow / time.Minute), dev: dev}
	esc, escErr := s.Store.ActiveEscrow(ctx, p.Scope, id)
	switch {
	case dev.Purging():
		d.Blocked, d.BlockedStatus = &Banner{ClassWarn, "This device is being purged", "Its backups are being removed, so nothing can be restored from it."}, http.StatusConflict
	case errors.Is(escErr, store.ErrNotFound) || (escErr == nil && esc.Custody != "escrowed"):
		d.Blocked, d.BlockedStatus = &Banner{ClassWarn, "Customer-held key: no technician restore", "CryoBackup never stores this key, so the console cannot open the repository. Restore on the machine with the customer's key."}, http.StatusConflict
	case escErr != nil:
		d.Blocked, d.BlockedStatus = &Banner{ClassCrit, "Restore unavailable", "The device's key record could not be read. Try again shortly."}, http.StatusServiceUnavailable
	case s.MasterLoaded == nil || !s.MasterLoaded():
		d.Blocked, d.BlockedStatus = &Banner{ClassCrit, "Master key unavailable", "Restores are disabled until the master key is loaded (banner C12)."}, http.StatusServiceUnavailable
	case dev.RetiredAt != nil:
		d.Retired = true
	}
	switch {
	case dev.RetiredAt != nil:
		d.DeviceWhy = "This device is retired: it can be downloaded from, not restored to (ADR 0030)."
	case !restores.AgentSupportsDeviceRestore(dev.AgentVersion) && dev.AgentVersion == "":
		d.DeviceWhy = "The agent on " + dev.Hostname + " has not reported its version. Restoring to the device needs agent " + restores.MinDeviceRestoreAgent + " or newer."
	case !restores.AgentSupportsDeviceRestore(dev.AgentVersion):
		d.DeviceWhy = "The agent on " + dev.Hostname + " is " + dev.AgentVersion + ". Restoring to the device needs agent " + restores.MinDeviceRestoreAgent + " or newer."
	default:
		if aid := s.activeDeviceRestore(ctx, p, id); aid != "" {
			d.ActiveID = aid
			d.DeviceWhy = "A restore to " + dev.Hostname + " is already queued or running. It must finish, expire or be cancelled first."
		}
	}
	s.loadSnapshots(ctx, p, f, &d)
	return d, true
}

// activeDeviceRestore returns the id of a device restore of the device that is queued (and not
// past its expiry) or running, or "". It only decides what the wizard offers; the service decides
// the create (restores.ErrRestoreActive).
func (s *Service) activeDeviceRestore(ctx context.Context, p *auth.Principal, id string) string {
	l, err := s.Store.ListRestoreRequests(ctx, p.Scope, id, store.Page{Limit: 50})
	if err != nil {
		return ""
	}
	now := s.now()
	for _, r := range l.Items {
		if store.IsDeviceRestoreMode(r.Mode) && (r.Status == "running" || (r.Status == "queued" && now.Before(r.ExpiresAt))) {
			return r.ID
		}
	}
	return ""
}

func (s *Service) loadSnapshots(ctx context.Context, p *auth.Principal, f health.Fleet, d *RestoreData) {
	d.Snapshots = nil
	snaps, err := s.Store.ListSnapshots(ctx, p.Scope, d.DeviceID)
	if err != nil {
		d.Error = &Banner{ClassCrit, "Snapshots unavailable", "The snapshot list could not be loaded."}
	}
	sort.Slice(snaps, func(i, j int) bool { return snaps[i].TakenAt.After(snaps[j].TakenAt) })
	c := s.fcAt(ctx, f.Computed)
	for _, sn := range snaps {
		d.Snapshots = append(d.Snapshots, SnapOption{ID: sn.SnapshotID, Label: snapLabel(sn, c)})
	}
	d.NoSnaps = len(d.Snapshots) == 0
}

func snapLabel(sn store.Snapshot, c fmtCtx) string {
	short := sn.SnapshotID
	if len(short) > 8 {
		short = short[:8]
	}
	return sn.TakenAt.In(c.loc()).Format("Mon 2 Jan 3:04 PM") + " · " + short
}

// refreshSnapshots reads the device's snapshots from the repository through the maintenance
// worker (adminpath Snapshots) and makes the console's cache equal to them, as the agent's own
// upload does. It returns the banner that says how it went.
func (s *Service) refreshSnapshots(ctx context.Context, d *RestoreData) *Banner {
	if s.Backend == nil {
		return &Banner{ClassCrit, "Refresh unavailable", "The console has no maintenance worker to read the repository with (banner C07)."}
	}
	list, err := s.Backend.Snapshots(ctx, adminpath.DeviceRequest{DeviceID: d.DeviceID, RequestID: httpx.RequestID(ctx)})
	if err != nil {
		b := lsBanner(err)
		b.Title = "The snapshot list was not refreshed"
		return b
	}
	out := make([]store.Snapshot, 0, len(list))
	for _, sn := range list {
		out = append(out, store.Snapshot{DeviceID: d.DeviceID, SnapshotID: sn.ID, TakenAt: sn.Time, ParentID: sn.Parent, Hostname: sn.Hostname,
			Paths: sn.Paths, Tags: sn.Tags, ProgramVersion: sn.ProgramVersion})
	}
	if _, _, err := s.Store.AgentReplaceSnapshots(ctx, d.DeviceID, out); err != nil {
		s.log().Warn("restore: snapshot refresh not stored", logx.ID("device_id", d.DeviceID), logx.Class("store"))
		return &Banner{ClassCrit, "The snapshot list was not refreshed", "The repository answered, but the console could not store its list. Try again shortly."}
	}
	return &Banner{ClassInfo, "Snapshot list refreshed from the repository.", plural(len(out), "snapshot", "snapshots") + " found."}
}

// ---- the selection ----

// basketFrom is the basket a post carries: its valid "path" values, each clean, absolute and not
// the snapshot root, without the ones a remove button names, deduplicated, nested items folded
// into their selected folder. folded counts the items folded in; dropped the values refused as
// malformed (a tampered form).
func basketFrom(posted []string, drop map[string]bool) (basket []string, folded, dropped int) {
	var keep []string
	for _, p := range posted {
		switch {
		case drop[p]:
		case !restores.ValidPath(p) || p == "/":
			dropped++
		default:
			keep = append(keep, p)
		}
	}
	uniq := map[string]bool{}
	for _, p := range keep {
		uniq[p] = true
	}
	basket = adminpath.CollapsePaths(keep)
	folded = len(uniq) - len(basket)
	sort.SliceStable(basket, func(i, j int) bool { return strings.ToLower(basket[i]) < strings.ToLower(basket[j]) })
	return basket, folded, dropped
}

func (d *RestoreData) setBasket(paths []string) {
	d.Basket = nil
	for _, p := range paths {
		d.Basket = append(d.Basket, BasketItem{Path: p, Name: path.Base(p), Parent: path.Dir(p)})
	}
	d.Over = len(paths) > restoreBasketMax
}

func (d *RestoreData) basketPaths() []string {
	out := make([]string, 0, len(d.Basket))
	for _, b := range d.Basket {
		out = append(out, b.Path)
	}
	return out
}

// selectedAbove returns the basket folder that holds p ("" when none).
func (d *RestoreData) selectedAbove(p string) string {
	for _, b := range d.Basket {
		if b.Path != p && strings.HasPrefix(p, b.Path+"/") {
			return b.Path
		}
	}
	return ""
}

// markListing sets each entry's checkbox from the basket and works out which basket paths ride
// as hidden fields.
func (d *RestoreData) markListing() {
	in := map[string]bool{}
	for _, b := range d.Basket {
		in[b.Path] = true
	}
	d.Inside = d.selectedAbove(d.Dir)
	if in[d.Dir] {
		d.Inside = d.Dir
	}
	shown := map[string]bool{}
	for i := range d.Entries {
		e := &d.Entries[i]
		switch {
		case e.Kind == "other":
		case d.Inside != "" || d.selectedAbove(e.Path) != "":
			e.Covered = true
		case in[e.Path]:
			e.Checked = true
			shown[e.Path] = true
		}
	}
	d.Hidden = nil
	for _, b := range d.Basket {
		if !shown[b.Path] {
			d.Hidden = append(d.Hidden, b.Path)
		}
	}
}

// ---- steps ----

func (d *RestoreData) steps() {
	st := func(n int, label string, at int) Step {
		switch {
		case n < at:
			return Step{N: n, Label: label, State: "done"}
		case n == at:
			return Step{N: n, Label: label, State: "on"}
		}
		return Step{N: n, Label: label, State: "todo"}
	}
	at := 2
	switch {
	case d.Step == stepDest:
		at = 3
	case d.Step == stepReview:
		at = 4
	case d.SnapshotID == "":
		at = 1
	}
	d.Steps = []Step{st(1, "Snapshot", at), st(2, "Select", at), st(3, "Destination", at), st(4, "Review", at)}
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

func (d *RestoreData) chooseSnapshot(id string) {
	d.SnapshotID = id
	for i := range d.Snapshots {
		d.Snapshots[i].Selected = d.Snapshots[i].ID == id
		if d.Snapshots[i].Selected {
			d.SnapLabel = d.Snapshots[i].Label
		}
	}
}

func (s *Service) restoreActor(r *http.Request, p *auth.Principal) restores.Actor {
	return restores.Actor{AdminID: p.Admin.ID, Username: p.Admin.Username, IP: httpx.ClientIP(r), Method: "web", FreshMFA: s.Auth.FreshMFA(p, s.Auth.Now())}
}

// browse lists one page of one folder of the snapshot. The browse is audited first
// (restores.AuditBrowse, once per admin, device and snapshot per hour); a browse whose audit row
// cannot be written is refused and shows nothing.
func (s *Service) browse(r *http.Request, d *RestoreData, p *auth.Principal, snapshot, dir string, off int) {
	ctx := r.Context()
	d.Step = stepSelect
	d.chooseSnapshot(snapshot)
	dir = cleanDir(dir)
	d.Dir, d.Crumbs = dir, crumbs(dir)
	if off < 0 || off%restoreLsPage != 0 {
		off = 0
	}
	if d.Blocked != nil || s.Backend == nil || snapshot == "" {
		d.markListing()
		return
	}
	if s.Restores != nil {
		if err := s.Restores.AuditBrowse(ctx, s.restoreActor(r, p), p.Scope, d.DeviceID, snapshot); err != nil {
			if errors.Is(err, store.ErrAudit) {
				s.log().Error("restore: browse refused, not audited", logx.ID("device_id", d.DeviceID), logx.Class("audit"))
				d.Error = &Banner{ClassCrit, "Browsing refused: NOT AUDITED", "The audit record of this browse could not be written, so the listing is not shown. Check the console database and try again."}
			} else {
				d.Error = &Banner{ClassCrit, "Browsing is unavailable", "The browse could not be recorded. Try again shortly."}
			}
			d.markListing()
			return
		}
	}
	res, err := s.Backend.Ls(ctx, adminpath.LsRequest{DeviceRequest: adminpath.DeviceRequest{DeviceID: d.DeviceID, RequestID: httpx.RequestID(ctx)},
		SnapshotID: snapshot, Dir: dir, Limit: restoreLsPage, Offset: off})
	if err != nil {
		d.Error = lsBanner(err)
		d.markListing()
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
			e.Kind, e.Size = "file", byteSize(n.Size)
		default:
			e.Kind, e.What = "other", nodeWhat(n.Type)
		}
		if !restores.ValidPath(e.Path) {
			e.Kind, e.What = "other", "a name the console cannot select"
		}
		d.Entries = append(d.Entries, e)
	}
	sort.SliceStable(d.Entries, func(i, j int) bool {
		if (d.Entries[i].Kind == "dir") != (d.Entries[j].Kind == "dir") {
			return d.Entries[i].Kind == "dir"
		}
		return strings.ToLower(d.Entries[i].Name) < strings.ToLower(d.Entries[j].Name)
	})
	d.Listed, d.Truncated, d.Offset, d.Total = true, res.Truncated, off, res.Total
	if len(d.Entries) > 0 {
		d.From, d.To = off+1, off+len(res.Nodes)
	}
	d.HasPrev, d.PrevOff = off > 0, max(off-restoreLsPage, 0)
	d.HasNext, d.NextOff = res.Truncated, off+restoreLsPage
	switch n := len(d.Entries); {
	case res.Total > 0 && (d.HasPrev || d.HasNext):
		d.PageText = commas(uint64(d.From)) + "–" + commas(uint64(d.To)) + " of " + commas(uint64(res.Total)) + " entries"
	case res.Truncated: // a worker that predates paging gives no total
		d.PageText = "the first " + commas(uint64(d.To)) + " entries"
	default:
		d.PageText = commas(uint64(n)) + " " + oneMany(n, "entry", "entries")
	}
	d.markListing()
}

// nodeWhat names a restic node type the wizard cannot select.
func nodeWhat(t string) string {
	switch t {
	case "symlink":
		return "a link: left out of a zip"
	case "dev", "chardev":
		return "a device entry"
	case "fifo":
		return "a named pipe"
	case "socket":
		return "a socket"
	}
	return "a special entry"
}

func cleanDir(dir string) string {
	if dir == "" || dir[0] != '/' || len(dir) > 4096 || strings.ContainsAny(dir, "\x00\r\n") {
		return "/"
	}
	return path.Clean(dir)
}

// lsBanner turns a failed browse into a path-free banner: the fixed sentence of the admin-path
// error code, nothing the worker said.
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

// ---- destination and review ----

// modes builds the destination step's choices. The default is the original location with
// existing files kept (ruling C1) whenever a device restore is offered, else the download.
func (d *RestoreData) modes(chosen string) {
	device := d.DeviceWhy == ""
	if chosen == "" {
		chosen = modeZip
		if device {
			chosen = modeOriginal
		}
	}
	// d.Mode keeps what was posted, even a destination this device is not offered (a stale page,
	// a forged form): the create refuses it with its own page, never turns it into another mode.
	// The radio shown checked is the download then.
	d.Mode = chosen
	shown := chosen
	if !device && chosen != modeZip {
		shown = modeZip
	}
	opt := func(v, title, text string, dev bool) ModeOpt {
		o := ModeOpt{Value: v, Title: title, Text: text, Checked: v == shown}
		if dev && !device {
			o.Disabled, o.Why, o.Checked = true, d.DeviceWhy, false
		}
		return o
	}
	d.Modes = []ModeOpt{
		opt(modeOriginal, "Original location, keep existing files", "Each item goes back where it was on "+d.Host+". A file that is already there is left as it is; only missing files are written. Nothing is deleted.", true),
		opt(modeSide, "A new folder on "+d.Host, "Everything lands in a new CryoBackup-Restore folder on the system drive, named after this restore. Nothing that exists is touched.", true),
		opt(modeReplace, "Original location, replace changed files", "Each item goes back where it was; a file that differs from the snapshot is replaced by the snapshot's copy. Nothing is deleted. You type the host name to confirm.", true),
		opt(modeZip, "Download as a zip", "The zip streams from the snapshot straight to your browser. Nothing is stored on the server. The download link works once.", false),
	}
}

func (d *RestoreData) createInput(confirm string) restores.CreateInput {
	in := restores.CreateInput{SnapshotID: d.SnapshotID, Paths: d.basketPaths()}
	switch d.Mode {
	case modeOriginal:
		in.Mode, in.Overwrite = restores.ModeDeviceOriginal, api.DeviceOverwriteNever
	case modeSide:
		in.Mode = restores.ModeDeviceSide
	case modeReplace:
		in.Mode, in.Overwrite, in.ConfirmHostName = restores.ModeDeviceOriginal, api.DeviceOverwriteIfChanged, confirm
	default:
		in.Mode = restores.ModeDownloadZip
	}
	return in
}

// restoreOfflineAfter is the engine's offline rule (state/classify.go offlineAfter: no heartbeat
// for two minutes), restated because the engine does not export it.
const restoreOfflineAfter = 2 * time.Minute

// liveFacts are what the review and the status page say about the device right now: whether it
// is offline, when it was last heard from, and whether a backup is running on it.
type liveFacts struct {
	Offline    bool
	SeenKnown  bool
	LastSeen   time.Time
	BackupOpen bool
}

func (s *Service) restoreLiveFacts(ctx context.Context, p *auth.Principal, f health.Fleet, id string) liveFacts {
	var out liveFacts
	for _, ds := range f.View.Devices {
		if ds.ID != id {
			continue
		}
		for _, a := range ds.Attrs {
			if a == state.AttrOffline {
				out.Offline = true
			}
		}
		if ds.Primary == state.StateUnseen { // the engine drops the offline badge once UNSEEN says more
			out.Offline = true
		}
		out.BackupOpen = ds.Detail.OpenRunAge > 0 || ds.Detail.OpenRunLimit > 0
	}
	if pr, err := s.Store.GetPresence(ctx, p.Scope, id); err == nil && !pr.ReceivedAt.IsZero() {
		out.SeenKnown, out.LastSeen = true, pr.ReceivedAt
		if s.now().Sub(pr.ReceivedAt) > restoreOfflineAfter {
			out.Offline = true
		}
	}
	if !out.SeenKnown {
		out.Offline = true
	}
	return out
}

// review sets the page up for the review step: what will happen in plain words, the device's
// state for a device restore, and the authenticator-code field when the session is not fresh.
func (s *Service) review(ctx context.Context, p *auth.Principal, f health.Fleet, d *RestoreData) {
	var live liveFacts
	if d.Mode != modeZip {
		live = s.restoreLiveFacts(ctx, p, f, d.DeviceID)
	}
	reviewWith(s.fc(ctx), d, live)
	d.NeedCode = !s.Auth.FreshMFA(p, s.Auth.Now())
}

// reviewWith is review with the formatting context and the device's live facts given (/design
// builds its samples with it).
func reviewWith(c fmtCtx, d *RestoreData, live liveFacts) {
	d.Step = stepReview
	n := len(d.Basket)
	items := plural(n, "item", "items")
	rv := &ReviewView{}
	expiry := c.dayClock(c.Now.Add(restores.DefaultTTL))
	switch d.Mode {
	case modeZip:
		rv.Title = "Download " + items + " from " + d.Host + " as a zip"
		rv.Button = "Continue to the download"
		rv.Lines = []string{
			"The next page offers the zip. It streams from the snapshot straight to your browser: nothing is stored on the server, and the download link works once.",
			"A download that is cut off leaves no usable zip: start it again from the beginning.",
		}
		if d.Retired {
			rv.Lines = append(rv.Lines, "This device is retired, so a download is the only way to get these files back (ADR 0030).")
		}
	default:
		rv.Button = "Start the restore"
		switch d.Mode {
		case modeOriginal:
			rv.Title = "Restore " + items + " to their original places on " + d.Host
			rv.Lines = []string{"Files that already exist there are kept as they are; only missing files are written.", "Nothing on " + d.Host + " is deleted."}
		case modeSide:
			rv.Title = "Restore " + items + " into a new folder on " + d.Host
			rv.Lines = []string{"Everything lands in a new CryoBackup-Restore folder on the system drive of " + d.Host + ", named after this restore.", "Nothing that exists on " + d.Host + " is touched."}
		case modeReplace:
			rv.Title = "Restore " + items + " to their original places on " + d.Host + ", replacing changed files"
			rv.Replace = true
			rv.Lines = []string{"A file that differs from the snapshot's copy is overwritten with the snapshot's copy. Changes made since the snapshot are lost for those files.", "Nothing on " + d.Host + " is deleted."}
		}
		switch {
		case live.Offline && live.SeenKnown:
			rv.Notes = append(rv.Notes, Banner{ClassWarn, d.Host + " is offline.", "Last seen " + ageLong(c.Now.Sub(live.LastSeen)) + " (" + c.dayClock(live.LastSeen) + "). The restore waits until it is online, until " + expiry + ". For files now, go back and choose Download as a zip."})
		case live.Offline:
			rv.Notes = append(rv.Notes, Banner{ClassWarn, d.Host + " has not been heard from.", "The restore waits until it is online, until " + expiry + ". For files now, go back and choose Download as a zip."})
		default:
			rv.Notes = append(rv.Notes, Banner{ClassInfo, d.Host + " is online.", "It picks the restore up at its next check-in, normally within a minute. The request waits until " + expiry + "."})
		}
		if live.BackupOpen {
			rv.Notes = append(rv.Notes, Banner{ClassInfo, "A backup is running on " + d.Host + ".", "The device runs one repository task at a time: the restore starts when the backup finishes."})
		}
	}
	modeLabel := ""
	for _, m := range d.Modes {
		if m.Value == d.Mode {
			modeLabel = m.Title
		}
	}
	rv.Facts = []Fact{{Key: "Device", Value: d.Host}, {Key: "Snapshot", Value: d.SnapLabel, Mono: true}, {Key: "Destination", Value: modeLabel}, {Key: "Selected", Value: items}}
	d.Review = rv
	d.CanSubmit = true
}

// ---- handlers ----

func (s *Service) restorePage(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	f, err := s.Fleet.Fleet(r.Context(), p.Scope)
	if err != nil {
		s.render(w, r, http.StatusServiceUnavailable, "error", "base", errorData(s.fallbackPage(r.Context(), p), 503, "Restore unavailable", "The fleet could not be loaded. Try again shortly."))
		return
	}
	d, ok := s.restoreBase(r.Context(), p, f, r.PathValue("id"))
	if !ok {
		s.notFound(w, r, p, "This device")
		return
	}
	if d.NoSnaps && d.Blocked == nil && s.Backend != nil { // plan B.1: the cache is empty, so ask the repository once
		if b := s.refreshSnapshots(r.Context(), &d); b.Class != ClassInfo {
			d.Error = b
		} else {
			s.loadSnapshots(r.Context(), p, f, &d)
		}
	}
	snap := ""
	if len(d.Snapshots) > 0 {
		snap = d.Snapshots[0].ID
	}
	s.browse(r, &d, p, snap, "/", 0)
	d.CanSubmit = d.Listed
	d.steps()
	s.render(w, r, http.StatusOK, "restore", "base", d)
}

// wizForm is one wizard post, read once.
type wizForm struct {
	step, action      string
	snapshot, snapCur string
	cur, dir          string
	dirSet            bool
	off               int
	posted            []string
	drop              map[string]bool
	mode, confirm     string
	code              string
}

func readWizForm(r *http.Request) wizForm {
	f := wizForm{step: r.PostFormValue("step"), action: r.PostFormValue("action"), snapshot: r.PostFormValue("snapshot"), snapCur: r.PostFormValue("snapcur"),
		cur: r.PostFormValue("cur"), mode: r.PostFormValue("mode"), confirm: r.PostFormValue("confirm_host"),
		code: strings.ReplaceAll(strings.TrimSpace(r.PostFormValue("code")), " ", ""), drop: map[string]bool{}}
	f.dir, f.dirSet = r.PostFormValue("dir"), r.PostForm.Has("dir")
	// "offcur" is the page the listing is at; a paging button names the page to go to ("page").
	f.off, _ = strconv.Atoi(r.PostFormValue("offcur"))
	if r.PostForm.Has("page") {
		f.off, _ = strconv.Atoi(r.PostFormValue("page"))
	}
	f.posted = r.PostForm["path"]
	for _, p := range r.PostForm["drop"] {
		f.drop[p] = true
	}
	switch f.mode {
	case "download_zip": // the single-file form of earlier versions
		f.mode = modeZip
	case modeZip, modeOriginal, modeSide, modeReplace, "":
	default:
		f.mode = ""
	}
	return f
}

// restorePost handles every step of the wizard (see the comment at the top of this file).
func (s *Service) restorePost(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	r.Body = http.MaxBytesReader(w, r.Body, restoreBodyMax)
	if err := r.ParseForm(); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			s.errorPage(w, r, p, http.StatusRequestEntityTooLarge, "Selection too large", "The selection is too large to carry in one form (64 KiB). Select the folders that hold the files instead of the files one by one.")
			return
		}
		http.Error(w, "The form could not be read.", http.StatusBadRequest)
		return
	}
	in := readWizForm(r)
	if len(in.posted) > restorePostedMax {
		s.errorPage(w, r, p, http.StatusRequestEntityTooLarge, "Selection too large", "More than "+strconv.Itoa(restorePostedMax)+" items were posted at once. Select the folders that hold them instead.")
		return
	}
	f, err := s.Fleet.Fleet(r.Context(), p.Scope)
	if err != nil {
		http.Error(w, "The fleet could not be loaded.", http.StatusServiceUnavailable)
		return
	}
	id := r.PathValue("id")
	d, ok := s.restoreBase(r.Context(), p, f, id)
	if !ok {
		s.notFound(w, r, p, "This device")
		return
	}

	// The point in time. A refresh first reads the repository; then the posted snapshot must be
	// one the cache holds, else the newest one is used.
	if in.action == "refresh" {
		d.Notice = s.refreshSnapshots(r.Context(), &d)
		if d.Notice.Class == ClassCrit {
			d.Error, d.Notice = d.Notice, nil
		}
		s.loadSnapshots(r.Context(), p, f, &d)
	}
	snapshot := ""
	for _, so := range d.Snapshots {
		if so.ID == in.snapshot {
			snapshot = so.ID
		}
	}
	if snapshot == "" && len(d.Snapshots) > 0 {
		snapshot = d.Snapshots[0].ID
	}

	basket, folded, dropped := basketFrom(in.posted, in.drop)
	if in.action == "clear" {
		basket = nil
	}
	dir, off := in.cur, in.off
	if in.dirSet {
		dir, off = in.dir, 0
	}
	// A new snapshot is another point in time: what was selected in the old one is not carried
	// over (it may not exist there). The form names the snapshot its listing came from (snapcur).
	if in.snapCur != "" && snapshot != in.snapCur {
		if len(basket) > 0 {
			d.Notice = &Banner{ClassInfo, "The selection was cleared.", "It belonged to the snapshot you left; select again in this one."}
		}
		basket, folded, dir, off = nil, 0, "/", 0
		if in.step != stepSelect && in.step != "" {
			in.step, in.action = stepSelect, "snapshot"
		}
	}
	if in.action == "snapshot" {
		dir, off = "/", 0
	}
	d.setBasket(basket)
	switch {
	case dropped > 0:
		d.Error = &Banner{ClassWarn, "Some selected items were not valid.", plural(dropped, "item was", "items were") + " left out of the selection."}
	case folded > 0 && d.Notice == nil:
		d.Notice = &Banner{ClassInfo, plural(folded, "item is", "items are") + " inside a selected folder.", "The folder already holds " + oneMany(folded, "it", "them") + ", so " + oneMany(folded, "it was", "they were") + " folded into it."}
	}
	d.chooseSnapshot(snapshot)
	if in.step == "" && in.mode == "" {
		in.mode = modeZip // the single-file form of earlier versions named no mode: it was a download
	}
	d.modes(in.mode)

	if in.action == "submit" && (in.step == "" || in.step == stepReview) {
		s.restoreCreate(w, r, p, f, &d, in)
		return
	}
	next := stepSelect
	switch {
	case in.action == "back" && in.step == stepReview:
		next = stepDest
	case in.action == "submit" && in.step == stepSelect:
		next = stepDest
	case in.action == "submit" && in.step == stepDest:
		next = stepReview
	}
	if next != stepSelect && (len(d.Basket) == 0 || d.Over || d.Blocked != nil) {
		next = stepSelect
		switch {
		case d.Blocked != nil:
		case len(d.Basket) == 0:
			d.Error = &Banner{ClassWarn, "Choose a file first", "Tick the files and folders you need in the listing, then continue."}
		case d.Over:
			d.Error = overBanner(len(d.Basket))
		}
	}
	if next != stepSelect { // the destination and review steps carry the folder the listing was at, for Back
		d.Dir, d.Offset = cleanDir(dir), off
		if d.Offset < 0 || d.Offset%restoreLsPage != 0 {
			d.Offset = 0
		}
	}
	status := http.StatusOK
	switch next {
	case stepDest:
		d.Step = stepDest
		d.Hidden = d.basketPaths()
		d.CanSubmit = true
	case stepReview:
		s.review(r.Context(), p, f, &d)
		d.Hidden = d.basketPaths()
	default:
		s.browse(r, &d, p, snapshot, dir, off)
		if d.Over && d.Error == nil {
			d.Error = overBanner(len(d.Basket))
		}
		if in.action == "submit" && d.Error != nil {
			status = http.StatusUnprocessableEntity
		}
		d.CanSubmit = d.Listed || len(d.Basket) > 0
	}
	d.steps()
	s.renderRestore(w, r, status, d)
}

// oneMany is one or many by n, without the number (plural puts it first).
func oneMany(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func overBanner(n int) *Banner {
	return &Banner{ClassCrit, strconv.Itoa(n) + " items are selected; one restore takes at most " + strconv.Itoa(restoreBasketMax) + ".",
		"Select the folder that holds them instead, or remove some. Continue stays off until the selection fits."}
}

// restoreCreate is the review step's Continue (and a post with no step): the fresh-MFA check,
// then the restore through the service, then its status page. Every refusal re-renders the
// review with its own banner and status.
func (s *Service) restoreCreate(w http.ResponseWriter, r *http.Request, p *auth.Principal, f health.Fleet, d *RestoreData, in wizForm) {
	d.Hidden = d.basketPaths()
	d.Dir, d.Offset = cleanDir(in.cur), 0
	if in.off > 0 && in.off%restoreLsPage == 0 {
		d.Offset = in.off
	}
	refuse := func(status int, b *Banner) {
		if d.Blocked != nil {
			d.Blocked, d.Error = b, nil // the rule that applied to this submit replaces the standing notice
			d.Step = stepSelect
			d.markListing()
		} else {
			d.Error = b
		}
		d.steps()
		s.renderRestore(w, r, status, *d)
	}
	toReview := func() {
		if d.Blocked == nil {
			s.review(r.Context(), p, f, d)
		}
	}
	// Every restore needs a fresh MFA check (ADR 0027 ruling C5), checked first, before any
	// worker call. A session that is not fresh goes through the console's step-up (the purge
	// dialogs' flow, purge_adr19.go stepUp) with the code the review step carries in "code";
	// without one, or with one the step-up refuses, the review step answers 403 with the field.
	if in.code != "" {
		refusal, done := s.stepUp(w, r, p, in.code)
		if done {
			return
		}
		if refusal != nil {
			toReview()
			d.NeedCode = true
			refuse(refusal.status, &Banner{refusal.Note.Class, refusal.Note.Title, refusal.Note.Text})
			return
		}
	}
	who := s.restoreActor(r, p)
	if !who.FreshMFA {
		toReview()
		d.NeedCode = true
		refuse(http.StatusForbidden, &Banner{ClassWarn, "A recent MFA check is required",
			"Every restore needs an authenticator check from the last " + strconv.Itoa(d.MFAWindow) +
				" minutes on this session. Enter the current code from your authenticator app below and continue; a recovery code is not accepted."})
		return
	}
	if len(d.Basket) == 0 || d.Over {
		s.browse(r, d, p, d.SnapshotID, in.cur, in.off)
		d.Error = &Banner{ClassWarn, "Choose a file first", "Tick the files and folders you need in the listing, then continue."}
		if d.Over {
			d.Error = overBanner(len(d.Basket))
		}
		d.CanSubmit = d.Listed
		d.steps()
		s.renderRestore(w, r, http.StatusUnprocessableEntity, *d)
		return
	}
	rr, err := s.Restores.CreateRestore(r.Context(), who, p.Scope, d.DeviceID, d.createInput(in.confirm))
	if err != nil {
		status, banner, known := restoreRefusal(err)
		switch {
		case d.Blocked != nil && (errors.Is(err, restores.ErrInvalid) || !known):
			// The service refused on the form (no snapshot is cached for a blocked device); the
			// rule the page already showed is the one that applies.
			status, banner = d.BlockedStatus, d.Blocked
		case known:
		case errors.Is(err, store.ErrAudit):
			s.log().Error("restore: not created, not audited", logx.Class("audit"), logx.Err(err))
			status, banner = http.StatusInternalServerError, &Banner{ClassCrit, "The restore was not started", "Its audit record could not be written (NOT AUDITED). Nothing was changed. Check the console database and try again."}
		default:
			s.log().Error("restore: create failed", logx.Err(err))
			status, banner = http.StatusInternalServerError, &Banner{ClassCrit, "The restore could not be started", "Try again shortly."}
		}
		toReview()
		switch {
		case errors.Is(err, restores.ErrStepUp):
			d.NeedCode = true
		case errors.Is(err, restores.ErrConfirmHost):
			d.ConfirmErr = "Type " + d.Host + " exactly as shown, then start the restore."
		}
		refuse(status, banner)
		return
	}
	if rr.Mode != store.RestoreModeDownloadZip && s.WakeAgent != nil {
		s.WakeAgent(d.DeviceID) // the device's command long poll returns now (as the admin API does)
	}
	target := "/restores/" + rr.ID
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", target)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// restoreRefusal is the page of each rule a restore create can refuse by: its HTTP status and
// its own sentence (fix E4). known is false for an error that is a fault, not a rule (the caller
// logs it and answers 500).
func restoreRefusal(err error) (status int, b *Banner, known bool) {
	switch {
	case errors.Is(err, restores.ErrStepUp):
		return http.StatusForbidden, &Banner{ClassWarn, "A recent MFA check is required", "Enter the current code from your authenticator app below and continue."}, true
	case errors.Is(err, restores.ErrConfirmHost):
		return http.StatusBadRequest, &Banner{ClassWarn, "The host name does not match", "Replacing files needs the device's host name typed exactly as shown. Nothing was started."}, true
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
		return http.StatusBadRequest, &Banner{ClassWarn, "That selection is not valid", "Go back, pick from the current listing and try again."}, true
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
