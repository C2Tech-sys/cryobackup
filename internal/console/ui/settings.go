package ui

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/httpx"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
)

// The settings screen (S26, architecture §5.3 GET /settings, admin and above;
// M2-W3A). Admins see every setting; the owner changes the ones that are
// editable in M2: the display time zone and the refresh interval. A change goes
// through store.SetSettingsAudited (validated, audited, one transaction) and is
// applied to the live display preferences, so the next page uses it.

const (
	patSettings     = "GET /settings"
	patSettingsPost = "POST /ui/settings"
)

func settingsUIPatterns() []string { return []string{patSettings, patSettingsPost} }

// registerAuditSettings adds the audit viewer and the settings routes. The
// pages are admin pages; saving settings is an owner action (CSRF-checked by
// Require, body capped by the gate).
func (s *Service) registerAuditSettings(r httpx.Registrar) {
	ad := func(h http.HandlerFunc) http.Handler { return s.Auth.Require(auth.RoleAdmin, auth.ModeUI, h) }
	ow := func(h http.HandlerFunc) http.Handler { return s.Auth.Require(auth.RoleOwner, auth.ModeUI, h) }
	r.Handle(patAudit, ad(s.auditPage))
	r.Handle(patAuditTable, ad(s.auditTablePartial))
	r.Handle(patSettings, ad(s.settingsPage))
	r.Handle(patSettingsPost, ow(s.settingsSave))
}

// SettingRowV is a setting that is shown but not editable here.
type SettingRowV struct {
	Label, Help, Value, Since string
}

// SettingsPage is /settings.
type SettingsPage struct {
	Page
	CanEdit bool
	ListErr bool
	Notice  *Note
	// PrefsNote is why the stored display settings were not used at start, if any.
	PrefsNote string
	// ConfigNotes say config.json names a different value than the one saved here (the saved one wins).
	ConfigNotes []string

	Zones      []string
	ZoneIn     string
	ZoneErr    string
	ZoneNow    string
	ZoneBy     string
	RefreshIn  string
	RefreshErr string
	RefreshMin int
	RefreshMax int
	RefreshBy  string
	FormErr    *Note

	Others []SettingRowV
}

var settingsNotices = map[string]Note{
	"saved":    {"ok", "Settings saved.", "They apply to the next page you open. Times stay stored in UTC."},
	"nochange": {"info", "Nothing changed.", "Those values are already in effect, so no audit row was written."},
}

// zoneNow is "3:42 PM CDT (UTC-05:00)" for the zone at now.
func zoneNow(now time.Time, loc *time.Location) string {
	t := now.In(loc)
	name, off := t.Zone()
	sign := "+"
	if off < 0 {
		sign, off = "-", -off
	}
	return t.Format("3:04 PM") + " " + name + " (UTC" + sign + two(off/3600) + ":" + two(off%3600/60) + ")"
}

func two(n int) string {
	if n < 10 {
		return "0" + string(rune('0'+n))
	}
	return string(rune('0'+n/10)) + string(rune('0'+n%10))
}

func (s *Service) settingsData(r *http.Request, p *auth.Principal) SettingsPage {
	f, err := s.Fleet.Fleet(r.Context(), p.Scope)
	var pg Page
	if err != nil {
		pg = s.fallbackPage(r.Context(), p)
		pg.Title, pg.Nav = "Settings", "settings"
	} else {
		pg = s.page(r.Context(), p, f, "Settings", "settings", "/ui/events")
	}
	snap := s.displayOf(r.Context())
	d := SettingsPage{Page: pg, CanEdit: p.Role >= auth.RoleOwner, Zones: store.CommonZones, ZoneIn: snap.Zone, RefreshIn: strconv.Itoa(snap.RefreshS),
		ZoneNow: zoneNow(s.now(), snap.Loc)}
	if s.Display != nil {
		d.PrefsNote, d.ConfigNotes = s.Display.Note(), s.Display.ConfigNotes()
	}
	def, _ := store.SettingDefOf(store.SettingRefreshInterval)
	d.RefreshMin, d.RefreshMax = def.Min, def.Max
	vals, err := s.Store.ListSettingValues(r.Context(), p.Scope)
	if err != nil {
		s.log().Error("ui: settings failed", logx.Err(err))
		d.ListErr = true
		return d
	}
	for _, v := range vals {
		by := ""
		if v.UpdatedByName != "" && !v.UpdatedAt.IsZero() {
			by = "Last changed by " + v.UpdatedByName + " on " + s.fc(r.Context()).dateClock(v.UpdatedAt)
		}
		switch v.Key {
		case store.SettingDisplayTimezone:
			d.ZoneBy = by
		case store.SettingRefreshInterval:
			d.RefreshBy = by
		default:
			row := SettingRowV{Label: v.Label, Help: v.Help, Since: v.Since, Value: settingText(v)}
			d.Others = append(d.Others, row)
		}
	}
	if k := r.URL.Query().Get("done"); k != "" {
		if n, ok := settingsNotices[k]; ok {
			d.Notice = &n
		}
	}
	return d
}

// settingText shows a stored value in words.
func settingText(v store.SettingValue) string {
	if v.Stored == nil {
		return "not set"
	}
	var str string
	if json.Unmarshal(v.Stored, &str) == nil {
		return str
	}
	txt := strings.Trim(string(v.Stored), " ")
	switch {
	case txt == "true":
		return "yes"
	case txt == "false":
		return "no"
	case v.Unit != "":
		return txt + " " + v.Unit
	}
	return txt
}

func (s *Service) renderSettings(w http.ResponseWriter, r *http.Request, status int, mutate func(*SettingsPage)) {
	d := s.settingsData(r, principal(r))
	if mutate != nil {
		mutate(&d)
	}
	if d.ListErr && status == http.StatusOK {
		status = http.StatusServiceUnavailable
	}
	s.render(w, r, status, "settings", "base", d)
}

// allCustomers refuses an account limited to some customers: the settings are
// console-wide, so only an account that can see every customer may read or change them.
func (s *Service) allCustomers(w http.ResponseWriter, r *http.Request, p *auth.Principal) bool {
	if p.Scope.All {
		return true
	}
	s.errorPage(w, r, p, http.StatusForbidden, "Not permitted", "Console settings are console-wide, so they need an account that can see every customer.")
	return false
}

func (s *Service) settingsPage(w http.ResponseWriter, r *http.Request) {
	if !s.allCustomers(w, r, principal(r)) {
		return
	}
	s.renderSettings(w, r, http.StatusOK, nil)
}

// settingsSave is POST /ui/settings: the owner's save of the editable settings.
func (s *Service) settingsSave(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !s.allCustomers(w, r, p) {
		return
	}
	zone := strings.TrimSpace(r.PostFormValue("display_timezone"))
	refresh := strings.TrimSpace(r.PostFormValue("refresh_interval_s"))
	zraw, _ := json.Marshal(zone)
	inputs := []store.SettingInput{{Key: store.SettingDisplayTimezone, Value: zraw}, {Key: store.SettingRefreshInterval, Value: json.RawMessage(refresh)}}

	// Validate each field on its own first, so one post shows every problem at its field.
	var zoneErr, refreshErr string
	for _, in := range inputs {
		if _, err := store.ValidateSetting(in.Key, in.Value); err != nil {
			msg := "That value is not accepted."
			if fe := store.FieldErrors(err); len(fe) > 0 {
				msg = fe[0].Message
			}
			if in.Key == store.SettingDisplayTimezone {
				zoneErr = msg
			} else {
				refreshErr = msg
			}
		}
	}
	again := func(status int, note *Note) {
		s.renderSettings(w, r, status, func(d *SettingsPage) {
			d.ZoneIn, d.RefreshIn, d.ZoneErr, d.RefreshErr, d.FormErr = zone, refresh, zoneErr, refreshErr, note
			d.Notice = nil
		})
	}
	if zoneErr != "" || refreshErr != "" {
		again(http.StatusBadRequest, &Note{"crit", "Nothing was saved.", "Fix the setting marked below."})
		return
	}
	changes, err := s.Store.SetSettingsAudited(r.Context(), p.Scope, inputs, s.actor(r, p))
	switch {
	case err == nil:
	case errors.Is(err, store.ErrAudit):
		s.log().Error("ui: settings change refused, not audited", logx.Class("audit"), logx.Err(err))
		again(http.StatusInternalServerError, &Note{"crit", "Nothing was saved.", "The change could not be audited, so it was refused. Check the database and try again."})
		return
	case errors.Is(err, store.ErrScope):
		s.errorPage(w, r, p, http.StatusForbidden, "Not permitted", "Changing console settings needs an account that can see every customer.")
		return
	default:
		s.log().Error("ui: settings save failed", logx.Err(err))
		again(http.StatusServiceUnavailable, &Note{"crit", "Nothing was saved.", "The settings could not be written. Try again shortly."})
		return
	}
	s.Display.Apply(changes)
	done := "nochange"
	for _, c := range changes {
		if !c.Unchanged {
			done = "saved"
		}
	}
	http.Redirect(w, r, "/settings?done="+done, http.StatusSeeOther)
}
