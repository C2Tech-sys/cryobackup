package ui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
)

// The device panel (B07) as a tabbed full page: Summary (the M0 facts), History
// (runs with filters and paging), Settings (rename, notes, class, update ring)
// and Audit (the device's audit rows, admin only). Each tab is a deep link:
// /devices/{id}, /devices/{id}/history, /devices/{id}/settings, /devices/{id}/audit.
// There is no action menu or command here yet (another wave).

// PanelTab is one tab of the device panel.
type PanelTab struct {
	Key     string
	Label   string
	Href    string
	Current bool
}

// DevPanelPage is the device panel in any tab.
type DevPanelPage struct {
	Page
	Dev      DeviceView
	Tab      string
	Tabs     []PanelTab
	History  *HistoryView
	Settings *DevSettingsForm
	Audit    *AuditView
	Policy   *DevPolicyView // M2-W2A: effective policy and override (policy_device.go)
	Saved    string
	Actions  *ActionBar   // M2-W2C: the action menu in the header (commands.go)
	Commands *CmdListView // M2-W2C: the Commands tab (commands.go)
}

// ---- history ----

// HistRow is one run of the history table.
type HistRow struct {
	ID       string
	Started  string
	ISO      string
	Kind     string
	Chip     Chip
	Summary  string
	Duration string
	Exit     string
	Trigger  string
}

// HistFilterForm is the history filter bar.
type HistFilterForm struct {
	Kind    []Option
	Outcome []Option
	Range   []Option
	Size    int
}

// HistoryView is the History tab.
type HistoryView struct {
	Rows    []HistRow
	Form    HistFilterForm
	Chips   []FilterChip
	Clear   string
	Pager   PagerView
	Warn    *CauseView
	Empty   *EmptyView
	Summary string
	Action  string
}

type histQuery struct {
	Kind, Outcome, Range string
	Page, Size           int
}

var histRanges = []struct {
	Key, Label string
	D          time.Duration
}{{"", "All time", 0}, {"24h", "Last 24 hours", 24 * time.Hour}, {"7d", "Last 7 days", 7 * 24 * time.Hour}, {"30d", "Last 30 days", 30 * 24 * time.Hour}}

func parseHistQuery(v url.Values) (histQuery, []store.FieldError) {
	q := histQuery{Kind: v.Get("kind"), Outcome: v.Get("outcome"), Range: v.Get("range"), Page: atoiDefault(v.Get("page"), 1), Size: store.NormalPageSize(atoiDefault(v.Get("size"), 0))}
	var bad []store.FieldError
	if q.Kind != "" && !inList(q.Kind, store.RunKinds) {
		bad = append(bad, store.FieldError{Field: "kind", Message: "Unknown run kind."})
		q.Kind = ""
	}
	if q.Outcome != "" && !inList(q.Outcome, store.RunOutcomes) {
		bad = append(bad, store.FieldError{Field: "outcome", Message: "Unknown outcome."})
		q.Outcome = ""
	}
	known := q.Range == ""
	for _, r := range histRanges {
		known = known || r.Key == q.Range
	}
	if !known {
		bad = append(bad, store.FieldError{Field: "range", Message: "Unknown range."})
		q.Range = ""
	}
	return q, bad
}

func inList(v string, list []string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

func (q histQuery) values() url.Values {
	v := url.Values{}
	if q.Kind != "" {
		v.Set("kind", q.Kind)
	}
	if q.Outcome != "" {
		v.Set("outcome", q.Outcome)
	}
	if q.Range != "" {
		v.Set("range", q.Range)
	}
	if q.Page > 1 {
		v.Set("page", strconv.Itoa(q.Page))
	}
	if q.Size != store.DefaultPageSize {
		v.Set("size", strconv.Itoa(q.Size))
	}
	return v
}

func (q histQuery) filtered() bool { return q.Kind != "" || q.Outcome != "" || q.Range != "" }

func opts(sel, anyLabel string, vals []string, label func(string) string) []Option {
	out := []Option{{Value: "", Label: anyLabel, Selected: sel == ""}}
	for _, v := range vals {
		out = append(out, Option{Value: v, Label: label(v), Selected: v == sel})
	}
	return out
}

func capFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func (s *Service) historyView(r *http.Request, p *auth.Principal, dev store.Device, c fmtCtx, q histQuery) (*HistoryView, int, error) {
	var f store.RunFilter
	f.Kind, f.Outcome = q.Kind, q.Outcome
	for _, rg := range histRanges {
		if rg.Key == q.Range && rg.D > 0 {
			f.Since = c.Now.Add(-rg.D)
		}
	}
	pageData, err := s.Store.ListDeviceRuns(r.Context(), p.Scope, dev.ID, f, q.Page, q.Size)
	if err != nil {
		return nil, http.StatusServiceUnavailable, err
	}
	q.Page, q.Size = pageData.Page.Page, pageData.Page.Size
	base := "/devices/" + dev.ID + "/history"
	href := func(mod func(*histQuery)) string {
		m := q
		mod(&m)
		return listURL(base, m.values())
	}
	h := &HistoryView{Action: base}
	for _, ru := range pageData.Items {
		rr := runRow(ru, c)
		when := ru.StartedAt
		h.Rows = append(h.Rows, HistRow{ID: ru.ID, Started: c.dayClock(when), ISO: when.UTC().Format(time.RFC3339), Kind: ru.Kind, Chip: rr.Chip, Summary: rr.Summary,
			Duration: rr.Duration, Exit: rr.Exit, Trigger: ru.Trigger})
	}
	h.Form = HistFilterForm{
		Kind:    opts(q.Kind, "Any kind", store.RunKinds, capFirst),
		Outcome: opts(q.Outcome, "Any outcome", store.RunOutcomes, capFirst),
		Size:    q.Size,
	}
	h.Form.Range = nil
	for _, rg := range histRanges {
		h.Form.Range = append(h.Form.Range, Option{Value: rg.Key, Label: rg.Label, Selected: rg.Key == q.Range})
	}
	chip := func(label string, mod func(*histQuery)) {
		h.Chips = append(h.Chips, FilterChip{Label: label, Href: href(mod)})
	}
	if q.Kind != "" {
		chip("Kind: "+q.Kind, func(m *histQuery) { m.Kind, m.Page = "", 1 })
	}
	if q.Outcome != "" {
		chip("Outcome: "+q.Outcome, func(m *histQuery) { m.Outcome, m.Page = "", 1 })
	}
	if q.Range != "" {
		for _, rg := range histRanges {
			if rg.Key == q.Range {
				chip(rg.Label, func(m *histQuery) { m.Range, m.Page = "", 1 })
			}
		}
	}
	h.Clear = href(func(m *histQuery) { *m = histQuery{Size: m.Size, Page: 1} })
	h.Pager = buildPager(pageData.Page, "runs", func(page, size int) string { return href(func(m *histQuery) { m.Page, m.Size = page, size }) }, nil)
	h.Summary = h.Pager.Text
	if len(pageData.Items) == 0 {
		switch {
		case q.filtered():
			total := s.runTotal(r, p, dev.ID)
			h.Empty = &EmptyView{Kind: "neutral", Title: "No runs match these filters", Body: fmt.Sprintf("0 of %d shown.", total), Link: &Link{Href: h.Clear, Label: "Clear filters"}}
		default:
			// A missing history is a silence signal once the first-backup grace is
			// over (components 5.6): a warning, not an empty list.
			due := dev.EnrolledAt.Add(72 * time.Hour)
			if dev.FirstBackupDueBy != nil {
				due = *dev.FirstBackupDueBy
			}
			if c.Now.Before(due) {
				h.Empty = &EmptyView{Kind: "neutral", Title: "No runs yet", Body: "This device enrolled " + c.dayClock(dev.EnrolledAt) + " (" + s.zone(r.Context()) + "). Its first backup is due by " + c.dayClock(due) + "."}
			} else {
				h.Warn = &CauseView{Class: ClassWarn, Title: "No runs recorded since enrollment " + c.dayClock(dev.EnrolledAt) + ".",
					Text: "The first-backup grace ended " + c.dayClock(due) + ". A device with no history past its grace period is an alarm, not an empty list."}
			}
		}
	}
	return h, http.StatusOK, nil
}

// runTotal is the unfiltered number of runs of a device, for the "0 of N" text.
func (s *Service) runTotal(r *http.Request, p *auth.Principal, id string) int {
	pg, err := s.Store.ListDeviceRuns(r.Context(), p.Scope, id, store.RunFilter{}, 1, store.DefaultPageSize)
	if err != nil {
		return 0
	}
	return pg.Page.Total
}

// ---- audit ----

// AuditRowView is one audit row of the device.
type AuditRowView struct {
	When    string
	ISO     string
	Actor   string
	Action  string
	Result  Chip
	Details string
}

// AuditView is the Audit tab.
type AuditView struct {
	Rows  []AuditRowView
	Pager PagerView
	Warn  *CauseView
}

var auditActionText = map[string]string{
	"device.update": "Settings changed",
	"device.enroll": "Enrolled",
}

// auditDetails renders the path-free details of an audit row as a sentence.
func auditDetails(ev store.AuditEvent) string {
	if t := commandAuditDetails(ev); t != "" { // M2-W2C (commands.go)
		return t
	}
	var d struct {
		Fields      []string          `json:"fields"`
		DeviceClass map[string]string `json:"device_class"`
		UpdateRing  map[string]string `json:"update_ring"`
	}
	if len(ev.Details) == 0 || json.Unmarshal(ev.Details, &d) != nil {
		return ""
	}
	var parts []string
	if len(d.Fields) > 0 {
		names := make([]string, len(d.Fields))
		for i, f := range d.Fields {
			names[i] = strings.ReplaceAll(f, "_", " ")
		}
		parts = append(parts, "Changed "+strings.Join(names, ", "))
	}
	if d.DeviceClass["to"] != "" {
		parts = append(parts, "class "+d.DeviceClass["from"]+" to "+d.DeviceClass["to"])
	}
	if d.UpdateRing["to"] != "" {
		parts = append(parts, "ring "+d.UpdateRing["from"]+" to "+d.UpdateRing["to"])
	}
	return strings.Join(parts, " · ")
}

func (s *Service) auditView(r *http.Request, p *auth.Principal, deviceID string, c fmtCtx) (*AuditView, error) {
	page, size := atoiDefault(r.URL.Query().Get("page"), 1), store.NormalPageSize(atoiDefault(r.URL.Query().Get("size"), 0))
	total, err := s.Store.DeviceAuditTotal(r.Context(), p.Scope, deviceID)
	if err != nil {
		return nil, err
	}
	off, lim, info := store.Paginate(total, page, size)
	l, err := s.Store.ListDeviceAudit(r.Context(), p.Scope, deviceID, store.Page{Limit: lim, Cursor: store.OffsetCursor(off)})
	if err != nil {
		return nil, err
	}
	v := &AuditView{}
	for _, ev := range l.Items {
		actor := ev.ActorLabel
		if actor == "" {
			actor = ev.ActorType
		}
		act := auditActionText[ev.Action]
		if act == "" {
			act = ev.Action
		}
		res := Chip{ID: "ar-" + ev.ID, Label: strings.ToUpper(ev.Result), Class: ClassOK, Small: true}
		switch ev.Result {
		case "denied":
			res.Class = ClassWarn
		case "error":
			res.Class = ClassCrit
		}
		v.Rows = append(v.Rows, AuditRowView{When: c.dayClock(ev.OccurredAt), ISO: ev.OccurredAt.UTC().Format(time.RFC3339), Actor: actor, Action: act, Result: res, Details: auditDetails(ev)})
	}
	base := "/devices/" + deviceID + "/audit"
	v.Pager = buildPager(info, "audit rows", func(pg, sz int) string {
		q := url.Values{}
		if pg > 1 {
			q.Set("page", strconv.Itoa(pg))
		}
		if sz != store.DefaultPageSize {
			q.Set("size", strconv.Itoa(sz))
		}
		return listURL(base, q)
	}, nil)
	if len(v.Rows) == 0 {
		v.Warn = &CauseView{Class: ClassWarn, Title: "No audit rows for this device.", Text: "Enrollment and every change are audited; a device with none is a gap in the record."}
	}
	return v, nil
}

// ---- settings ----

// DevSettingsForm is the Settings tab.
type DevSettingsForm struct {
	Action   string
	CSRF     string
	ReadOnly bool
	Why      string // why the form is read-only
	Friendly string
	Notes    string
	Class    string
	Ring     string
	Classes  []Option
	Rings    []Option
	Errors   map[string]string
	Summary  []store.FieldError
	Facts    []Fact
}

// Err returns the error text of a field.
func (f DevSettingsForm) Err(field string) string { return f.Errors[field] }

func (s *Service) settingsForm(p *auth.Principal, dev store.Device, c fmtCtx) DevSettingsForm {
	f := DevSettingsForm{Action: "/ui/devices/" + dev.ID + "/settings", CSRF: p.CSRF, Errors: map[string]string{},
		Friendly: dev.FriendlyName, Notes: dev.Notes, Class: dev.DeviceClass, Ring: dev.UpdateRing}
	switch {
	case dev.RetiredAt != nil:
		f.ReadOnly, f.Why = true, "This device is retired; its settings can no longer change."
	case p.Role < auth.RoleOperator:
		f.ReadOnly, f.Why = true, "Changing a device's settings needs the operator role."
	}
	f.fillOptions()
	f.Facts = []Fact{{Key: "Hostname", Value: dev.Hostname, Mono: true}}
	if dev.OSVersion != "" {
		f.Facts = append(f.Facts, Fact{Key: "Operating system", Value: osLabel(dev.OSVersion)})
	}
	if dev.Arch != "" {
		f.Facts = append(f.Facts, Fact{Key: "Architecture", Value: dev.Arch, Mono: true})
	}
	if dev.AgentVersion != "" {
		f.Facts = append(f.Facts, Fact{Key: "Agent", Value: safeVersion(dev.AgentVersion), Mono: true})
	}
	if dev.ResticVersion != "" {
		f.Facts = append(f.Facts, Fact{Key: "Restic", Value: resticLabel(dev.ResticVersion), Mono: true})
	}
	f.Facts = append(f.Facts, Fact{Key: "Enrolled", Value: c.dateClock(dev.EnrolledAt) + " · via " + orDefault(dev.EnrolledVia, "token"), Mono: true})
	return f
}

func (f *DevSettingsForm) fillOptions() {
	f.Classes, f.Rings = nil, nil
	for _, v := range store.DeviceClasses {
		f.Classes = append(f.Classes, Option{Value: v, Label: capFirst(v), Selected: v == f.Class})
	}
	for _, v := range store.UpdateRings {
		lbl := "Stable"
		if v == "pilot" {
			lbl = "Pilot (receives agent updates first)"
		}
		f.Rings = append(f.Rings, Option{Value: v, Label: lbl, Selected: v == f.Ring})
	}
}

func (f *DevSettingsForm) finish() {
	f.fillOptions()
	order := []string{"friendly_name", "notes", "device_class", "update_ring", "settings"}
	f.Summary = nil
	for _, k := range order {
		if m := f.Errors[k]; m != "" {
			f.Summary = append(f.Summary, store.FieldError{Field: k, Message: m})
		}
	}
	extra := []string{}
	for k := range f.Errors {
		if !inList(k, order) {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	for _, k := range extra {
		f.Summary = append(f.Summary, store.FieldError{Field: k, Message: f.Errors[k]})
	}
}

// ---- the handler ----

func (s *Service) panelTabs(id, cur string, p *auth.Principal, canRestore bool) []PanelTab {
	tabs := []PanelTab{{Key: "summary", Label: "Summary", Href: "/devices/" + id}, {Key: "history", Label: "History", Href: "/devices/" + id + "/history"},
		{Key: "commands", Label: "Commands", Href: "/devices/" + id + "/commands"}, // M2-W2C
		{Key: "settings", Label: "Settings", Href: "/devices/" + id + "/settings"},
		{Key: "policy", Label: "Policy", Href: "/devices/" + id + "/policy"}} // M2-W2A (policy_device.go)
	if p.Role >= auth.RoleAdmin {
		tabs = append(tabs, PanelTab{Key: "audit", Label: "Audit", Href: "/devices/" + id + "/audit"})
	}
	if canRestore {
		tabs = append(tabs, PanelTab{Key: "restore", Label: "Restore", Href: "/devices/" + id + "/restore"})
	}
	for i := range tabs {
		tabs[i].Current = tabs[i].Key == cur
	}
	return tabs
}

func (s *Service) devicePanel(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	tab := r.PathValue("tab")
	switch tab {
	case "", "summary", "history", "commands", "settings", "audit":
	default:
		s.notFound(w, r, p, "This page")
		return
	}
	if tab == "summary" {
		tab = ""
	}
	if tab == "audit" && p.Role < auth.RoleAdmin {
		s.errorPage(w, r, p, http.StatusForbidden, "Not permitted", "The audit trail needs the admin role.")
		return
	}
	fleet, err := s.Fleet.Fleet(r.Context(), p.Scope)
	if err != nil {
		s.log().Error("device: fleet failed", logx.Err(err))
		s.render(w, r, http.StatusServiceUnavailable, "error", "base", errorData(s.fallbackPage(r.Context(), p), 503, "Device unavailable", "The device could not be loaded. Try again shortly."))
		return
	}
	id := r.PathValue("id")
	v, ok := s.deviceView(r.Context(), p, fleet, id)
	if !ok {
		s.notFound(w, r, p, "This device")
		return
	}
	dev, err := s.Store.GetDevice(r.Context(), p.Scope, id)
	if err != nil {
		s.notFound(w, r, p, "This device")
		return
	}
	cur := tab
	if cur == "" {
		cur = "summary"
	}
	events := "/ui/events"
	if cur == "summary" {
		events = "/ui/events?device=" + id
	}
	c := s.fcAt(r.Context(), fleet.Computed)
	pg := DevPanelPage{Page: s.page(r.Context(), p, fleet, "Device detail", "devices", events), Dev: v, Tab: cur, Tabs: s.panelTabs(id, cur, p, v.CanRestore), Saved: savedText(r.URL.Query().Get("saved"))}
	status := http.StatusOK
	if ds, ok := deviceState(fleet, id); ok { // M2-W2C: the action menu on every tab
		bar := s.actionBar(r, p, fleet, dev, ds)
		pg.Actions = &bar
	}
	switch cur {
	case "commands": // M2-W2C
		ds, _ := deviceState(fleet, id)
		cv, st, err := s.commandsTab(r, p, fleet, dev, ds)
		if err != nil {
			s.log().Error("ui: command history failed", logx.Err(err))
			s.render(w, r, st, "error", "base", errorData(s.page(r.Context(), p, fleet, "Commands unavailable", "devices", ""), st, "Commands unavailable", "The command history could not be read. This is an alarm, not an empty list. Try again shortly."))
			return
		}
		pg.Commands = cv
		if r.Header.Get("HX-Request") == "true" && r.Header.Get("HX-History-Restore-Request") != "true" {
			// The in-flight refresh asks for the list alone (and the action menu out of band), not the whole page.
			s.render(w, r, http.StatusOK, "device", "device_commands_poll", pg)
			return
		}
	case "history":
		q, bad := parseHistQuery(r.URL.Query())
		if len(bad) > 0 {
			s.badQuery(w, r, p, fleet, bad)
			return
		}
		h, st, err := s.historyView(r, p, dev, c, q)
		if err != nil {
			s.log().Error("ui: run history failed", logx.Err(err))
			s.render(w, r, st, "error", "base", errorData(s.page(r.Context(), p, fleet, "History unavailable", "devices", ""), st, "History unavailable", "The run history could not be read. This is an alarm, not an empty list. Try again shortly."))
			return
		}
		pg.History = h
	case "settings":
		f := s.settingsForm(p, dev, c)
		pg.Settings = &f
	case "audit":
		a, err := s.auditView(r, p, id, c)
		if err != nil {
			s.log().Error("ui: device audit failed", logx.Err(err))
			s.render(w, r, http.StatusServiceUnavailable, "error", "base", errorData(s.page(r.Context(), p, fleet, "Audit unavailable", "devices", ""), 503, "Audit unavailable", "The audit rows could not be read. Try again shortly."))
			return
		}
		pg.Audit = a
	}
	s.render(w, r, status, "device", "base", pg)
}

// deviceSettingsPost is POST /ui/devices/{id}/settings (operator).
func (s *Service) deviceSettingsPost(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
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
	friendly := strings.TrimSpace(r.PostFormValue("friendly_name"))
	notes := strings.TrimSpace(strings.ReplaceAll(r.PostFormValue("notes"), "\r\n", "\n"))
	class, ring := r.PostFormValue("device_class"), r.PostFormValue("update_ring")
	_, changed, err := s.Store.UpdateDeviceSettings(r.Context(), p.Scope, id, store.DeviceSettingsPatch{FriendlyName: &friendly, Notes: &notes, DeviceClass: &class, UpdateRing: &ring}, s.actor(r, p))
	if err == nil {
		if changed {
			s.changed()
		}
		http.Redirect(w, r, "/devices/"+id+"/settings?saved=1", http.StatusSeeOther)
		return
	}
	fe := store.FieldErrors(err)
	if len(fe) == 0 {
		s.storeFailed(w, r, p, err)
		return
	}
	fleet, ok := s.fleetFor(w, r, p, "Device")
	if !ok {
		return
	}
	v, ok := s.deviceView(r.Context(), p, fleet, id)
	if !ok {
		s.notFound(w, r, p, "This device")
		return
	}
	c := s.fcAt(r.Context(), fleet.Computed)
	f := s.settingsForm(p, dev, c)
	f.Friendly, f.Notes, f.Class, f.Ring = friendly, notes, class, ring
	for _, e := range fe {
		if f.Errors[e.Field] == "" {
			f.Errors[e.Field] = e.Message
		}
	}
	f.finish()
	pg := DevPanelPage{Page: s.page(r.Context(), p, fleet, "Device detail", "devices", "/ui/events"), Dev: v, Tab: "settings", Tabs: s.panelTabs(id, "settings", p, v.CanRestore), Settings: &f}
	s.render(w, r, http.StatusBadRequest, "device", "base", pg)
}
