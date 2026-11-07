package ui

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/health"
	"github.com/c2tech-sys/cryobackup/internal/console/httpx"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// The device action menu and the Commands tab of the device panel (M2-W2C,
// components 7.1, architecture §5.3 S07/S08). Back up now, Cancel run, Refresh
// policy and Check in now queue a command for the device; the Commands tab shows
// each one from queued to its result; Undo cancels a command that was not
// delivered yet; an admin resolves an identity conflict. Every mutation is
// operator-gated by the route, CSRF-checked by Require and audited by the store
// in the same transaction. Results travel in a short fixed code in the redirect
// URL (cmd=...), never in text, so the notice a page shows is always one of the
// sentences below.

// Route patterns of this file.
const (
	patDeviceActions    = "GET /ui/devices/{id}/actions"
	patDeviceBackupNow  = "POST /ui/devices/{id}/backup-now"
	patDeviceCancelRun  = "POST /ui/devices/{id}/cancel-run"
	patDeviceRefreshPol = "POST /ui/devices/{id}/refresh-policy"
	patDeviceCheckIn    = "POST /ui/devices/{id}/check-in"
	patCommandUndo      = "POST /ui/devices/{id}/commands/{cid}/cancel"
	patIdentityResolve  = "POST /ui/devices/{id}/identity/resolve"
)

// commandPatterns lists what registerCommandsUI adds.
func commandPatterns() []string {
	return []string{patDeviceActions, patDeviceBackupNow, patDeviceCancelRun, patDeviceRefreshPol, patDeviceCheckIn, patCommandUndo, patIdentityResolve}
}

func (s *Service) registerCommandsUI(r httpx.Registrar) {
	op := func(h http.HandlerFunc) http.Handler { return s.Auth.Require(auth.RoleOperator, auth.ModeUI, h) }
	ad := func(h http.HandlerFunc) http.Handler { return s.Auth.Require(auth.RoleAdmin, auth.ModeUI, h) }
	r.Handle(patDeviceActions, op(s.actionsPartial)) // OP per architecture §5 (S07), like the actions it offers
	r.Handle(patDeviceBackupNow, op(s.commandPost(store.CommandBackupNow)))
	r.Handle(patDeviceCancelRun, op(s.commandPost(store.CommandCancel)))
	r.Handle(patDeviceRefreshPol, op(s.commandPost(store.CommandRefreshPolicy)))
	r.Handle(patDeviceCheckIn, op(s.commandPost(store.CommandCheckIn)))
	r.Handle(patCommandUndo, op(s.commandUndoPost))
	r.Handle(patIdentityResolve, ad(s.identityResolvePost))
}

// ---- vocabulary ----

var commandLabels = map[string]string{
	store.CommandBackupNow:     "Back up now",
	store.CommandCancel:        "Cancel run",
	store.CommandRefreshPolicy: "Refresh policy",
	store.CommandCheckIn:       "Check in now",
	"restart_service":          "Restart service",
	"update_agent":             "Update agent",
	"pause":                    "Park",
	"resume":                   "Resume",
	"hold":                     "Hold",
	"restore":                  "Restore",
	"uninstall":                "Uninstall",
}

func commandLabel(typ string) string {
	if l, ok := commandLabels[typ]; ok {
		return l
	}
	return typ
}

// CommandChip is the status chip of a command (components 3.2 language: four
// classes plus neutral; the word is always shown, colour is never alone).
func CommandChip(id, status string, small bool) Chip {
	var label, class string
	switch status {
	case "queued":
		label, class = "QUEUED", ClassInfo
	case "delivered":
		label, class = "DELIVERED", ClassInfo
	case "acked":
		label, class = "STARTED", ClassInfo
	case "succeeded":
		label, class = "DONE", ClassOK
	case "failed":
		label, class = "FAILED", ClassCrit
	case "expired":
		label, class = "EXPIRED", ClassWarn
	case "cancelled":
		label, class = "CANCELLED", ClassNeutral
	default:
		label, class = "UNKNOWN STATUS", ClassCrit
	}
	return Chip{ID: id, Label: label, Class: class, Small: small}
}

// notices are the only texts the cmd= code can show.
var cmdNotices = map[string]CauseView{
	"queued":               {Class: ClassOK, Title: "Queued.", Text: "An online device receives it within seconds; an offline one when it next checks in."},
	"coalesced":            {Class: ClassInfo, Title: "Already queued.", Text: "An identical request is still in flight, so no second one was added."},
	"cancelled":            {Class: ClassOK, Title: "Cancelled.", Text: "The command was withdrawn before the device received it."},
	"kept":                 {Class: ClassOK, Title: "Identity conflict resolved.", Text: "The device keeps its identity. The resolution is in the audit trail."},
	"retired":              {Class: ClassWarn, Title: "Device retired.", Text: "Its device token is revoked and its gateway login goes at the next sync; the device page says if that fails. Enroll the real machine again as a new device."},
	"err_parked":           {Class: ClassWarn, Title: "Not queued.", Text: "The device is parked. Resume it first."},
	"err_retired":          {Class: ClassWarn, Title: "Not queued.", Text: "The device is retired."},
	"err_delivered":        {Class: ClassWarn, Title: "Too late to undo.", Text: "The device already has this command. To stop a running backup, use Cancel run."},
	"err_expired":          {Class: ClassWarn, Title: "Nothing to cancel.", Text: "The command expired before the device received it."},
	"err_invalid":          {Class: ClassCrit, Title: "Not queued.", Text: "The request was not valid. Nothing was changed."},
	"err_no_run":           {Class: ClassWarn, Title: "No run to cancel.", Text: "This device has no open backup run."},
	"err_identity_reason":  {Class: ClassCrit, Title: "Not resolved.", Text: "A reason is required: one line, no file paths."},
	"err_identity_confirm": {Class: ClassCrit, Title: "Not resolved.", Text: "Type the device name exactly to confirm retiring it."},
	"err_identity_action":  {Class: ClassCrit, Title: "Not resolved.", Text: "Choose keep or retire."},
	"err_identity_none":    {Class: ClassInfo, Title: "Nothing to resolve.", Text: "This device has no identity conflict (it may have been resolved already)."},
}

// ---- views ----

// ActionItem is one entry of the device action menu.
type ActionItem struct {
	ID      string
	Key     string // backup_now | cancel_run | refresh_policy | check_in
	Label   string
	Action  string // POST target
	IdemKey string
	CSRF    string
	Primary bool
	// Confirm, when set, puts the action behind an inline confirmation with this text.
	Confirm    string
	ConfirmBtn string
	Enabled    bool
	Why        string // why a disabled item is unavailable (aria-describedby)
	Hint       string // note under an enabled item ("Runs when ... next checks in.")
	State      *Chip  // QUEUED, DELIVERED, ... when the same command is already in flight
	Suffix     *Chip  // QUEUED, on an enabled item whose device is offline
	StateNote  string // "Queued Mon 30 Oct, 1:42 PM"
	Undo       string // POST target that cancels the in-flight command ("" = cannot be undone)
	// Pause, when set, is the Park dialog (m3t5_pause.go): the item opens a form instead of posting at once.
	Pause *PauseForm
	// Retire, when set, is the Retire dialog (m3t6_retire.go), the same way.
	Retire *RetireForm
}

// IdentityPanel is the banner and the resolve form of an identity conflict.
type IdentityPanel struct {
	Action     string
	CSRF       string
	Host       string
	Since      string
	ISO        string
	CanResolve bool
	Why        string
}

// ActionBar is the device action menu in the panel header.
type ActionBar struct {
	DeviceID string
	Host     string
	Notice   *CauseView
	Items    []ActionItem // the bar itself
	More     []ActionItem // the "More" menu
	Identity *IdentityPanel
}

// CmdRow is one command of the Commands tab.
type CmdRow struct {
	ID      string
	When    string
	ISO     string
	Label   string
	Chip    Chip
	Detail  string
	Timing  string
	By      string
	Undo    string
	CSRF    string
	Expires string
}

// CmdListView is the Commands tab.
type CmdListView struct {
	DeviceID string
	Rows     []CmdRow
	Form     []Option
	Status   string
	Action   string
	Pager    PagerView
	Chips    []FilterChip
	Clear    string
	Pending  bool // something is in flight: the list refreshes itself
	Poll     string
	Empty    *EmptyView
	Summary  string
}

var cmdErrCode = regexp.MustCompile(`^[a-z_]{1,24}$`)

// noticeFrom reads the cmd= result code of a redirect.
func noticeFrom(r *http.Request) *CauseView {
	code := r.URL.Query().Get("cmd")
	if !cmdErrCode.MatchString(code) {
		return nil
	}
	n, ok := cmdNotices[code]
	if !ok {
		return nil
	}
	if why := r.URL.Query().Get("why"); code == "retired_pending" && cmdErrCode.MatchString(why) {
		n.Text += " Reason: " + why + "." // M3-T6: a job's error class, one of a known shape
	}
	if t := r.URL.Query().Get("t"); code == "queued" && cmdErrCode.MatchString(t) {
		if _, known := commandLabels[t]; known {
			n.Title = commandLabel(t) + " queued."
		}
	}
	return &n
}

func newUIKey() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "ui-" + hex.EncodeToString(b[:])
}

// deviceOnline mirrors the summary's rule: heard from within two minutes.
func deviceOnline(ds state.DeviceState, dev store.Device, now time.Time) bool {
	return dev.EnrollConfirmedAt != nil && !ds.Detail.LastSeen.IsZero() && now.Sub(ds.Detail.LastSeen) <= 2*time.Minute
}

func parkedNow(dev store.Device, now time.Time) bool {
	return dev.PausedAt != nil && (dev.PausedUntil == nil || dev.PausedUntil.After(now))
}

// inflight returns the newest command of each type that is still in flight.
func inflight(cmds []store.Command, now time.Time) map[string]store.Command {
	out := map[string]store.Command{}
	for _, c := range cmds { // newest first
		if _, seen := out[c.Type]; !seen && c.Pending(now) {
			out[c.Type] = c
		}
	}
	return out
}

func deviceState(f health.Fleet, id string) (state.DeviceState, bool) {
	for i := range f.View.Devices {
		if f.View.Devices[i].ID == id {
			return f.View.Devices[i], true
		}
	}
	return state.DeviceState{}, false
}

// actionBar builds the device action menu: what each action does now, given the
// role, the device's lifecycle, whether it is online, an open run and the
// commands already in flight.
func (s *Service) actionBar(r *http.Request, p *auth.Principal, f health.Fleet, dev store.Device, ds state.DeviceState) ActionBar {
	c := s.fcAt(r.Context(), f.Computed)
	now := f.Computed
	bar := ActionBar{DeviceID: dev.ID, Host: dev.Hostname, Notice: noticeFrom(r)}
	var cmds []store.Command
	if l, err := s.Store.ListCommands(r.Context(), p.Scope, dev.ID, store.Page{Limit: 50}); err == nil {
		cmds = l.Items
	} else {
		s.log().Error("ui: commands unreadable for the action menu", logx.Err(err))
		bar.Notice = &CauseView{Class: ClassCrit, Title: "Commands unavailable.", Text: "The command queue could not be read, so in-flight commands are not shown. Try again shortly."}
	}
	pend := inflight(cmds, now)
	retired := dev.RetiredAt != nil || ds.Primary == state.StateRetired
	parked := parkedNow(dev, now)
	online := deviceOnline(ds, dev, now)
	canOp := p.Role >= auth.RoleOperator
	runOpen := ds.Detail.OpenRunAge > 0 || ds.Detail.OpenRunLimit > 0

	mk := func(key, typ, label, path string) ActionItem {
		it := ActionItem{ID: "act-" + strings.ReplaceAll(key, "_", "-"), Key: key, Label: label, Action: "/ui/devices/" + dev.ID + "/" + path,
			IdemKey: newUIKey(), CSRF: p.CSRF, Enabled: true}
		switch {
		case retired:
			it.Enabled, it.Why = false, "Retired."
		case !canOp:
			it.Enabled, it.Why = false, "Needs the operator role."
		}
		if !it.Enabled {
			return it
		}
		if cmd, ok := pend[typ]; ok {
			st := cmd.EffectiveStatus(now)
			ch := CommandChip("cs-"+key, st, true)
			it.State = &ch
			it.StateNote = commandLabel(typ) + " " + strings.ToLower(ch.Label) + " " + c.dayClock(cmd.CreatedAt)
			if st == "queued" {
				it.Undo = "/ui/devices/" + dev.ID + "/commands/" + cmd.ID + "/cancel"
			}
			it.Enabled = false
			it.Why = "Already in flight; it finishes on the Commands tab."
			return it
		}
		if online {
			if key == "backup_now" {
				it.Hint = "The device is online; it starts within seconds."
			}
		} else {
			// Queued-when-offline (components 7.1): the QUEUED suffix and the hint say when it runs.
			it.Hint = "Runs when " + dev.Hostname + " next checks in. Expires " + c.dayClock(now.Add(store.DefaultOperatorTTL(typ))) + "."
			it.Suffix = &Chip{ID: "q-" + key, Label: "QUEUED", Class: ClassInfo, Small: true}
		}
		return it
	}

	// Back up now, or Cancel run while a run is open (components 7.1 table).
	switch {
	case runOpen && !retired:
		it := mk("cancel_run", store.CommandCancel, "Cancel run", "cancel-run")
		if it.Enabled {
			it.Confirm, it.ConfirmBtn = "Cancel this run? Files already uploaded are kept.", "Cancel run"
			it.Hint = ""
		}
		bar.Items = append(bar.Items, it)
	default:
		it := mk("backup_now", store.CommandBackupNow, "Back up now", "backup-now")
		if it.Enabled && parked {
			it.Enabled = false
			if dev.PausedUntil != nil {
				it.Why = "Parked until " + c.dayClock(*dev.PausedUntil) + ". Resume first."
			} else {
				it.Why = "Parked with no end date. Resume first."
			}
		}
		it.Primary = true
		bar.Items = append(bar.Items, it)
	}
	bar.Items = append(bar.Items, mk("refresh_policy", store.CommandRefreshPolicy, "Refresh policy", "refresh-policy"))
	bar.Items = append(bar.Items, s.parkItems(p, dev, c, retired, parked))  // M3-T5 (B15): Park or Resume
	bar.Items = append(bar.Items, s.retireItem(r, p, dev, retired, parked)) // M3-T6 (B07): Retire
	bar.More = append(bar.More, mk("check_in", store.CommandCheckIn, "Check in now", "check-in"))

	if dev.IdentityConflictAt != nil && !retired {
		bar.Identity = &IdentityPanel{Action: "/ui/devices/" + dev.ID + "/identity/resolve", CSRF: p.CSRF, Host: dev.Hostname,
			Since: c.dayClock(*dev.IdentityConflictAt), ISO: dev.IdentityConflictAt.UTC().Format(time.RFC3339), CanResolve: p.Role >= auth.RoleAdmin}
		if !bar.Identity.CanResolve {
			bar.Identity.Why = "Resolving an identity conflict needs the admin role."
		}
	}
	return bar
}

// actionsPartial is GET /ui/devices/{id}/actions: the action menu as a fragment,
// so a page can refresh availability from current presence.
func (s *Service) actionsPartial(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	fleet, ok := s.fleetFor(w, r, p, "Device actions")
	if !ok {
		return
	}
	dev, err := s.Store.GetDevice(r.Context(), p.Scope, r.PathValue("id"))
	ds, inView := deviceState(fleet, r.PathValue("id"))
	if err != nil || !inView {
		http.NotFound(w, r)
		return
	}
	s.render(w, r, http.StatusOK, "device", "dev_actions", s.actionBar(r, p, fleet, dev, ds))
}

// ---- the Commands tab ----

func (s *Service) commandsTab(r *http.Request, p *auth.Principal, f health.Fleet, dev store.Device, ds state.DeviceState) (*CmdListView, int, error) {
	c := s.fcAt(r.Context(), f.Computed)
	now := f.Computed
	status := r.URL.Query().Get("status")
	if status != "" && !inList(status, store.CommandStatuses) {
		status = ""
	}
	page, size := atoiDefault(r.URL.Query().Get("page"), 1), store.NormalPageSize(atoiDefault(r.URL.Query().Get("size"), 0))
	pg, err := s.Store.ListDeviceCommands(r.Context(), p.Scope, dev.ID, store.CommandFilter{Status: status}, now, page, size, 0, 0)
	if err != nil {
		return nil, http.StatusServiceUnavailable, err
	}
	base := "/devices/" + dev.ID + "/commands"
	href := func(st string, pageNo, sz int) string {
		v := url.Values{}
		if st != "" {
			v.Set("status", st)
		}
		if pageNo > 1 {
			v.Set("page", strconv.Itoa(pageNo))
		}
		if sz != store.DefaultPageSize {
			v.Set("size", strconv.Itoa(sz))
		}
		return listURL(base, v)
	}
	v := &CmdListView{DeviceID: dev.ID, Status: status, Action: base, Poll: href(status, pg.Page.Page, pg.Page.Size)}
	v.Form = opts(status, "Any status", store.CommandStatuses, func(st string) string { return CommandChip("", st, true).Label })
	for i := range v.Form {
		if v.Form[i].Value != "" {
			v.Form[i].Label = capFirst(strings.ToLower(CommandChip("", v.Form[i].Value, true).Label))
		}
	}
	v.Pager = buildPager(pg.Page, "commands", func(pageNo, sz int) string { return href(status, pageNo, sz) }, nil)
	v.Summary = v.Pager.Text
	if status != "" {
		v.Chips = append(v.Chips, FilterChip{Label: "Status: " + strings.ToLower(CommandChip("", status, true).Label), Href: href("", 1, pg.Page.Size)})
	}
	v.Clear = href("", 1, pg.Page.Size)
	names := map[string]string{}
	by := func(id string) string {
		if id == "" {
			return "system"
		}
		if n, ok := names[id]; ok {
			return n
		}
		n := "an account that no longer exists"
		if a, err := s.Store.GetAdmin(r.Context(), id); err == nil {
			n = a.Username
		}
		names[id] = n
		return n
	}
	for _, cmd := range pg.Items {
		st := cmd.EffectiveStatus(now)
		row := CmdRow{ID: cmd.ID, When: c.dayClockSec(cmd.CreatedAt), ISO: cmd.CreatedAt.UTC().Format(time.RFC3339), Label: commandLabel(cmd.Type),
			Chip: CommandChip("c-"+cmd.ID, st, true), Detail: commandDetail(cmd, st), Timing: commandTiming(cmd, st, c), By: by(cmd.CreatedBy),
			Expires: c.dayClock(cmd.ExpiresAt), CSRF: p.CSRF}
		if st == "queued" && p.Role >= auth.RoleOperator {
			row.Undo = "/ui/devices/" + dev.ID + "/commands/" + cmd.ID + "/cancel"
		}
		if cmd.Pending(now) {
			v.Pending = true
		}
		v.Rows = append(v.Rows, row)
	}
	switch {
	case status != "" && len(v.Rows) == 0:
		total := 0
		if all, err := s.Store.ListDeviceCommands(r.Context(), p.Scope, dev.ID, store.CommandFilter{}, now, 1, store.DefaultPageSize, 0, 0); err == nil {
			total = all.Page.Total
		}
		v.Empty = &EmptyView{Kind: "neutral", Title: "No commands match this filter", Body: fmt.Sprintf("0 of %d shown.", total), Link: &Link{Href: v.Clear, Label: "Clear filter"}}
	case len(v.Rows) == 0:
		who := "has not been seen yet"
		switch {
		case deviceOnline(ds, dev, now):
			who = "is online"
		case !ds.Detail.LastSeen.IsZero():
			who = "was last seen " + c.dayClock(ds.Detail.LastSeen)
		}
		v.Empty = &EmptyView{Kind: "neutral", Title: "No commands yet", Body: dev.Hostname + " " + who + ". Back up now and the other actions queue a command here; you can watch it from queued to its result."}
	}
	return v, http.StatusOK, nil
}

// resultDoc is what an agent reports; every field is a fixed token.
type resultDoc struct {
	Started    *bool  `json:"started"`
	Cancelled  *bool  `json:"cancelled"`
	Reason     string `json:"reason"`
	ErrorClass string `json:"error_class"`
	LateStatus string `json:"late_status"`
}

var token = regexp.MustCompile(`^[a-z0-9_]{1,40}$`)

// commandDetail says in a sentence what happened. Only fixed phrases and the
// agent's fixed tokens (a-z, 0-9, underscore) are ever shown, never free text.
func commandDetail(c store.Command, status string) string {
	var d resultDoc
	if len(c.Result) > 0 {
		_ = json.Unmarshal(c.Result, &d)
	}
	reason := d.Reason
	if !token.MatchString(reason) {
		reason = ""
	}
	switch status {
	case "queued":
		return "Waiting for the device to check in."
	case "delivered":
		return "The device has it and has not answered yet."
	case "acked":
		return "The device accepted it."
	case "expired":
		switch {
		case d.LateStatus == "succeeded":
			return "The device answered after the deadline: it reported the command done."
		case d.LateStatus == "failed":
			return "The device answered after the deadline: it reported the command failed."
		case c.DeliveredAt != nil:
			return "The device received it but had not answered when it expired."
		}
		return "The device did not receive it in time. Nothing ran."
	case "cancelled":
		return "Withdrawn before the device received it."
	case "succeeded":
		switch c.Type {
		case store.CommandBackupNow:
			if d.Started != nil && !*d.Started {
				return "Not started: a backup was already running."
			}
			return "The backup started."
		case store.CommandCancel:
			if d.Cancelled != nil && !*d.Cancelled {
				if reason == "run_mismatch" {
					return "A newer run had started; it was left running."
				}
				return "No run was active, so nothing was stopped."
			}
			return "The run was stopped. Files already uploaded are kept."
		case store.CommandRefreshPolicy:
			return "The device pulled its policy."
		case store.CommandCheckIn:
			return "The device checked in."
		}
		return "Done."
	case "failed":
		var parts []string
		switch reason {
		case "not_implemented_m0":
			parts = append(parts, "This agent does not support the command yet.")
		case "expired":
			parts = append(parts, "The device received it after it expired and dropped it.")
		case "start_failed":
			parts = append(parts, "The backup could not start.")
		case "", "failed":
			parts = append(parts, "The command failed.")
		default:
			parts = append(parts, "The command failed ("+reason+").")
		}
		if token.MatchString(d.ErrorClass) && d.ErrorClass != "unknown" {
			parts = append(parts, "Error class: "+d.ErrorClass+".")
		}
		return strings.Join(parts, " ")
	}
	return ""
}

// dayClockSec is dayClock with seconds ("Wed 21 Oct, 1:12:05 PM"): the Commands tab shows the time a command was
// queued to the second, so the delay to delivery can be read straight off the row (the 30 s acceptance bound).
func (c fmtCtx) dayClockSec(t time.Time) string { return t.In(c.loc()).Format("Mon 2 Jan, 3:04:05 PM") }

// afterText is a short delay: "<1 s", "2 s", "95 s", "4 m 05 s". Never negative.
func afterText(d time.Duration) string {
	switch {
	case d < time.Second:
		return "<1 s"
	case d < 120*time.Second:
		return fmt.Sprintf("%d s", int(d/time.Second))
	}
	return fmt.Sprintf("%d m %02d s", int(d/time.Minute), int(d%time.Minute/time.Second))
}

// commandTiming is the lifecycle line: when it was delivered, answered, and when it expires.
func commandTiming(c store.Command, status string, f fmtCtx) string {
	var parts []string
	if c.DeliveredAt != nil {
		parts = append(parts, "delivered "+f.clock(*c.DeliveredAt)+" (after "+afterText(c.DeliveredAt.Sub(c.CreatedAt))+")")
	}
	if c.CompletedAt != nil && c.Status != "cancelled" {
		parts = append(parts, "answered "+f.clock(*c.CompletedAt))
	} else if c.AckedAt != nil {
		parts = append(parts, "accepted "+f.clock(*c.AckedAt))
	}
	if c.CompletedAt != nil && c.Status == "cancelled" {
		parts = append(parts, "cancelled "+f.clock(*c.CompletedAt))
	}
	switch status {
	case "queued", "delivered", "acked":
		parts = append(parts, "expires "+f.clock(c.ExpiresAt))
	case "expired":
		parts = append(parts, "expired "+f.clock(c.ExpiresAt))
	}
	return strings.Join(parts, " · ")
}

// ---- audit text for this file's actions ----

func init() {
	// The device audit tab shows these as sentences (devicepanel.go falls back to the raw action name).
	auditActionText["command.queue"] = "Command queued"
	auditActionText["command.cancel"] = "Command cancelled"
	auditActionText["device.identity_resolve"] = "Identity conflict resolved"
}

// commandAuditDetails renders the path-free details of this file's audit rows.
func commandAuditDetails(ev store.AuditEvent) string {
	var d struct {
		Type       string `json:"type"`
		TTL        int64  `json:"ttl_s"`
		Resolution string `json:"resolution"`
		Reason     string `json:"reason"`
		Gateway    string `json:"gateway_login"`
	}
	if len(ev.Details) == 0 || json.Unmarshal(ev.Details, &d) != nil {
		return ""
	}
	switch ev.Action {
	case "command.queue":
		return commandLabel(d.Type) + " · lifetime " + ageLong(time.Duration(d.TTL)*time.Second)
	case "command.cancel":
		return commandLabel(d.Type)
	case "device.identity_resolve":
		return "Resolution: " + d.Resolution + " · " + d.Reason
	case "device.retire":
		return d.Reason
	case "device.retire_sync":
		if d.Reason != "" {
			return "Gateway login: " + d.Gateway + " · " + d.Reason
		}
		return "Gateway login: " + d.Gateway
	}
	return ""
}

// ---- POST handlers ----

// back redirects to a device page with a result code.
func back(w http.ResponseWriter, r *http.Request, id, tab, code string, extra ...string) {
	v := url.Values{"cmd": {code}}
	for i := 0; i+1 < len(extra); i += 2 {
		v.Set(extra[i], extra[i+1])
	}
	path := "/devices/" + id
	if tab != "" {
		path += "/" + tab
	}
	http.Redirect(w, r, listURL(path, v), http.StatusSeeOther)
}

// commandPost returns the handler that queues one command type for the device.
func (s *Service) commandPost(typ string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := principal(r)
		r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
		if err := r.ParseForm(); err != nil {
			s.errorPage(w, r, p, http.StatusBadRequest, "Form not read", "The form could not be read.")
			return
		}
		id := r.PathValue("id")
		if _, err := s.Store.GetDevice(r.Context(), p.Scope, id); err != nil { // 404 for another customer's device, before anything else
			s.storeFailed(w, r, p, err)
			return
		}
		key := strings.TrimSpace(r.PostFormValue("key"))
		if key == "" || len(key) > 128 {
			key = newUIKey()
		}
		in := store.CommandInput{Type: typ, IdempotencyKey: key}
		if typ == store.CommandCancel {
			// Stop the device's open backup run. The command names the run the
			// operator saw; the agent stops it only if that run is still the active
			// one, and answers run_mismatch (left running) when a newer run began.
			runs, err := s.Store.ListRuns(r.Context(), p.Scope, id, store.Page{Limit: 20})
			if err != nil {
				s.storeFailed(w, r, p, err)
				return
			}
			var runID string
			for _, ru := range runs.Items {
				if ru.Outcome == "" && ru.Origin == "agent" {
					runID = ru.ID
					break
				}
			}
			if runID == "" {
				back(w, r, id, "", "err_no_run")
				return
			}
			in.Params, _ = json.Marshal(map[string]string{"run_id": runID})
		}
		res, err := s.Store.QueueCommandAudited(r.Context(), p.Scope, id, in, s.actor(r, p))
		switch {
		case err == nil:
			if res.Created {
				s.wakeAgent(id)
				s.changed()
			}
			code := "queued"
			if res.Coalesced {
				code = "coalesced"
			}
			back(w, r, id, "commands", code, "t", typ)
		case errors.Is(err, store.ErrDeviceParked):
			back(w, r, id, "", "err_parked")
		case errors.Is(err, store.ErrDeviceRetired):
			back(w, r, id, "", "err_retired")
		case errors.Is(err, store.ErrInvalid):
			back(w, r, id, "", "err_invalid")
		default:
			s.storeFailed(w, r, p, err)
		}
	}
}

func (s *Service) wakeAgent(deviceID string) {
	if s.WakeAgent != nil {
		s.WakeAgent(deviceID)
	}
}

// commandUndoPost cancels a command that has not been delivered yet.
func (s *Service) commandUndoPost(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	id := r.PathValue("id")
	_, changed, err := s.Store.CancelCommandAudited(r.Context(), p.Scope, id, r.PathValue("cid"), s.actor(r, p))
	switch {
	case err == nil:
		if changed {
			s.changed()
		}
		back(w, r, id, "commands", "cancelled")
	case errors.Is(err, store.ErrCommandDelivered):
		back(w, r, id, "commands", "err_delivered")
	case errors.Is(err, store.ErrCommandExpired):
		back(w, r, id, "commands", "err_expired")
	default:
		s.storeFailed(w, r, p, err)
	}
}

// identityResolvePost resolves an identity conflict (admin).
func (s *Service) identityResolvePost(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if err := r.ParseForm(); err != nil {
		s.errorPage(w, r, p, http.StatusBadRequest, "Form not read", "The form could not be read.")
		return
	}
	id := r.PathValue("id")
	dev, err := s.Store.GetDevice(r.Context(), p.Scope, id)
	if err != nil {
		s.storeFailed(w, r, p, err)
		return
	}
	action := r.PostFormValue("action")
	if action == store.ResolveRetire && !strings.EqualFold(strings.TrimSpace(r.PostFormValue("confirm")), dev.Hostname) {
		back(w, r, id, "", "err_identity_confirm")
		return
	}
	_, err = s.Store.ResolveIdentityConflict(r.Context(), p.Scope, id, store.IdentityResolution{Action: action, Reason: r.PostFormValue("reason")}, s.actor(r, p))
	switch {
	case err == nil:
		s.changed()
		code := "kept"
		if action == store.ResolveRetire {
			code = "retired"
			if s.SyncGateway != nil { // the retired device's gateway login must stop working now
				s.SyncGateway()
			}
		}
		back(w, r, id, "", code)
	case errors.Is(err, store.ErrNoIdentityConflict):
		back(w, r, id, "", "err_identity_none")
	case errors.Is(err, store.ErrDeviceRetired):
		back(w, r, id, "", "err_retired")
	case len(store.FieldErrors(err)) > 0:
		code := "err_identity_reason"
		if fe := store.FieldErrors(err); fe[0].Field == "action" {
			code = "err_identity_action"
		}
		back(w, r, id, "", code)
	default:
		s.storeFailed(w, r, p, err)
	}
}
