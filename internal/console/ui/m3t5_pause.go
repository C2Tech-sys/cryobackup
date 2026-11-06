package ui

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminapi"
	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
)

// M3-T5: the pause dialog (B15, components 7.1). "Park…" opens a form: for 1, 2,
// 4, 8 or 24 hours, until a date, or indefinitely, with a reason (required). It
// posts to POST /ui/devices/{id}/pause, which goes through POST
// /api/v1/devices/{id}/pause: the one handler that validates, parks, cancels an
// open run, audits, and refuses a retired device. A parked device shows
// "Resume" in its place.

// PauseForm is the dialog of the Park action.
type PauseForm struct {
	Action string
	CSRF   string
	Hours  []int
	Zone   string // the zone "until" is read in
	Min    string // datetime-local bounds, in that zone
	Max    string
}

const datetimeLocal = "2006-01-02T15:04"

// parkItems are the Park (or Resume) entry of the action bar for a device.
func (s *Service) parkItems(p *auth.Principal, dev store.Device, c fmtCtx, retired, parked bool) ActionItem {
	if parked {
		it := ActionItem{ID: "act-resume", Key: "resume", Label: "Resume", Action: "/ui/devices/" + dev.ID + "/resume", IdemKey: newUIKey(), CSRF: p.CSRF, Enabled: true}
		if dev.PausedUntil != nil {
			it.Hint = "Parked until " + c.dayClock(*dev.PausedUntil) + ". Resume to back up again now."
		} else {
			it.Hint = "Parked with no end date. Resume to back up again."
		}
		if dev.PauseReason != "" {
			it.Hint += " Reason: " + dev.PauseReason + "."
		}
		if p.Role < auth.RoleOperator {
			it.Enabled, it.Why = false, "Needs the operator role."
		}
		return it
	}
	it := ActionItem{ID: "act-park", Key: "park", Label: "Park…", CSRF: p.CSRF, Enabled: true,
		Hint: "Stops backups on this device for a while or until you resume it. A backup that is running is cancelled."}
	switch {
	case retired:
		it.Enabled, it.Why, it.Hint = false, "Retired.", ""
	case p.Role < auth.RoleOperator:
		it.Enabled, it.Why, it.Hint = false, "Needs the operator role.", ""
	default:
		now := c.Now.In(c.loc())
		it.Pause = &PauseForm{Action: "/ui/devices/" + dev.ID + "/pause", CSRF: p.CSRF, Hours: store.PauseHours, Zone: c.Zone,
			Min: now.Add(time.Hour).Format(datetimeLocal), Max: now.Add(store.MaxPauseSpan).Format(datetimeLocal)}
	}
	return it
}

// the notices of the pause and resume answers
func init() {
	for k, v := range map[string]CauseView{
		"parked":           {Class: ClassOK, Title: "Device parked.", Text: "No backups run until it ends or you resume it. A running backup was cancelled."},
		"resumed":          {Class: ClassOK, Title: "Device resumed.", Text: "Backups run again at the next schedule or when you press Back up now."},
		"err_pause_reason": {Class: ClassCrit, Title: "Not parked.", Text: "A reason is required: one line, no file paths."},
		"err_pause_when":   {Class: ClassCrit, Title: "Not parked.", Text: "Choose for how long: 1, 2, 4, 8 or 24 hours, until a date, or indefinitely."},
		"err_pause_until":  {Class: ClassCrit, Title: "Not parked.", Text: "The end must be at least a minute from now and at most a year away."},
		"err_pause_fail":   {Class: ClassCrit, Title: "Not parked.", Text: "The console could not park the device. Nothing was changed."},
		"err_resume_fail":  {Class: ClassCrit, Title: "Not resumed.", Text: "The console could not resume the device. Nothing was changed."},
	} {
		if _, ok := cmdNotices[k]; !ok {
			cmdNotices[k] = v
		}
	}
}

func (s *Service) devicePausePost(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if err := r.ParseForm(); err != nil {
		s.errorPage(w, r, p, http.StatusBadRequest, "Form not read", "The form could not be read.")
		return
	}
	id := r.PathValue("id")
	dev, err := s.Store.GetDevice(r.Context(), p.Scope, id) // 404 for another customer's device, before anything else
	if err != nil {
		s.notFound(w, r, p, "This device")
		return
	}
	if dev.RetiredAt != nil {
		back(w, r, id, "", "err_retired")
		return
	}
	reason := strings.TrimSpace(r.PostFormValue("reason"))
	if reason == "" {
		back(w, r, id, "", "err_pause_reason")
		return
	}
	body := adminapi.DevicePauseRequest{Reason: reason}
	switch when := r.PostFormValue("when"); when {
	case "1", "2", "4", "8", "24":
		h, _ := strconv.Atoi(when)
		body.DurationHours = &h
	case "indefinite":
		body.Indefinite = true
	case "until":
		t, perr := time.ParseInLocation(datetimeLocal, strings.TrimSpace(r.PostFormValue("until")), s.loc(r.Context()))
		if perr != nil {
			back(w, r, id, "", "err_pause_until")
			return
		}
		u := t.UTC()
		body.Until = &u
	default:
		back(w, r, id, "", "err_pause_when")
		return
	}
	status, resp, err := s.apiSend(r, http.MethodPost, "/api/v1/devices/"+id+"/pause", body)
	if s.writeFailed(w, r, p, status, err) {
		return
	}
	switch {
	case err == nil && status == http.StatusOK:
		back(w, r, id, "", "parked")
	case status == http.StatusConflict:
		back(w, r, id, "", "err_retired")
	case status == http.StatusBadRequest:
		code := "err_pause_reason"
		var pr struct {
			Errors []struct {
				Field string `json:"field"`
			} `json:"errors"`
		}
		_ = json.Unmarshal(resp, &pr)
		for _, f := range pr.Errors {
			if f.Field == "until" {
				code = "err_pause_until"
			}
		}
		back(w, r, id, "", code)
	default:
		s.log().Error("ui: park failed", logx.Class("render"), logx.Int("status", status))
		back(w, r, id, "", "err_pause_fail")
	}
}

func (s *Service) deviceResumePost(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if err := r.ParseForm(); err != nil {
		s.errorPage(w, r, p, http.StatusBadRequest, "Form not read", "The form could not be read.")
		return
	}
	id := r.PathValue("id")
	if _, err := s.Store.GetDevice(r.Context(), p.Scope, id); err != nil {
		s.notFound(w, r, p, "This device")
		return
	}
	status, _, err := s.apiSend(r, http.MethodPost, "/api/v1/devices/"+id+"/resume", map[string]any{})
	if s.writeFailed(w, r, p, status, err) {
		return
	}
	switch {
	case err == nil && status == http.StatusOK:
		back(w, r, id, "", "resumed")
	case status == http.StatusConflict:
		back(w, r, id, "", "err_retired")
	default:
		s.log().Error("ui: resume failed", logx.Class("render"), logx.Int("status", status))
		back(w, r, id, "", "err_resume_fail")
	}
}
