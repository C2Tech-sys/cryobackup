package ui

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminapi"
	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/httpx"
	"github.com/c2tech-sys/cryobackup/internal/console/jobs"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// Acknowledge and snooze (ADR 0020 decision 7, matrix row r163, components 7.2; P20-W3B).
//
// The control lives where an alarm is explained: the device panel's Summary tab (one row per open
// alarm of the device) and the customer page of a MISSING customer. Elsewhere (the board, the
// device list, Overdue, Not seen, a customer's "Needs attention") the alarm chip stays the primary
// state and the engine's ACK or SNOOZED badge sits beside it; the chip links to the device, where
// the control is.
//
// Every write goes through the admin API's handlers in process (POST /api/v1/acks, DELETE
// /api/v1/acks/{id}): the store writes alarm_ack.create or alarm_ack.clear in the change's
// transaction, refuses an alarm that is not open (409 alarm_not_open, shown here as a plain
// message) and never copies the note to the audit trail. An acknowledgement never changes a
// state: the chip stays red or amber, and the row says who, when, until when and the note.

const (
	patAckCreate = "POST /ui/acks"
	patAckClear  = "POST /ui/acks/{id}/clear"
)

func ackUIPatterns() []string { return []string{patAckCreate, patAckClear} }

func (s *Service) registerAcks(r httpx.Registrar) {
	op := func(h http.HandlerFunc) http.Handler { return s.Auth.Require(auth.RoleOperator, auth.ModeUI, h) }
	r.Handle(patAckCreate, op(s.ackCreate))
	r.Handle(patAckClear, op(s.ackClear))
}

// AlarmsView is the "Open alarms" block of a device or a MISSING customer.
type AlarmsView struct {
	Notice *Note
	Err    *Note
	Rows   []AlarmRow
	CanAck bool
	Why    string
	CSRF   string
	Zone   string
}

// AlarmRow is one open alarm: its chip, and its acknowledgement or the form to make one.
type AlarmRow struct {
	Chip Chip
	Ack  *AckLine
	Form AckForm
	// Recorded: the console holds an open episode of this alarm, so the API accepts an
	// acknowledgement of it. An alarm shown before the evaluator recorded it is not offered one.
	Recorded bool
	// NotYet is the line shown instead of the button while the alarm is not recorded.
	NotYet string
}

// AckLine is an acknowledgement or snooze in force.
type AckLine struct {
	ID     string
	Badge  Badge
	Lead   string // "Acknowledged" or "Snoozed"
	By     string
	At     When
	Until  string
	Note   string
	Action string // the clear form
}

// AckForm is the acknowledge popover of one alarm.
type AckForm struct {
	Action      string
	Label       string
	SubjectType string
	SubjectID   string
	AlarmType   string
	PopID       string
	Tomorrow    string
	Min, Max    string
	NextLabel   string
	// Mode, Until and Note are what the form opens with: "ack" and empty for a new acknowledgement,
	// the existing kind, end and note for "Change…" (F6). Change is true for the latter.
	Mode   string
	Until  string
	Note   string
	Change bool
}

// ackTypeRE is the shape of an alarm_acks.alarm_type word.
var ackTypeRE = regexp.MustCompile(`^[a-z_]{1,40}$`)

var ackNotices = map[string]Note{
	"acked":     {"ok", "Alarm acknowledged.", "E-mail for this alarm is muted. The alarm stays on the board, marked ACK, until its cause clears."},
	"snoozed":   {"ok", "Alarm snoozed.", "E-mail for this alarm is muted until the snooze ends. If the alarm is still open then, the alert goes out again."},
	"cleared":   {"ok", "Acknowledgement cleared.", "The alarm is not muted any more: its next alert goes out as usual."},
	"not_open":  {"info", "This alarm is not open.", "It may have cleared since the page was loaded, or the console has not recorded it yet. Nothing was changed."},
	"ack_gone":  {"info", "That acknowledgement had already ended.", "Nothing was changed."},
	"err_mode":  {"crit", "Not acknowledged.", "Choose how long: acknowledge until cleared, a snooze preset, a date, or until the next success."},
	"err_until": {"crit", "Not snoozed.", "The end must be in the future and at most a year away."},
	"err_gap":   {"crit", "Not snoozed.", "That time does not exist on that day: the clocks skip it in the display time zone. Choose a time outside the skipped hour."},
	"err_note":  {"crit", "Not acknowledged.", "The note can have at most 500 characters of plain text."},
	"err_type":  {"crit", "Not acknowledged.", "That alarm cannot be acknowledged here."},
	"err_fail":  {"crit", "Not acknowledged.", "The console could not record the acknowledgement. Nothing was changed."},
}

// ackForm is the popover of one alarm of a subject.
func ackForm(subjectType, subjectID, alarm, what string, c fmtCtx, zone string) AckForm {
	now := c.Now.In(c.loc())
	tomorrow := time.Date(now.Year(), now.Month(), now.Day()+1, 8, 0, 0, 0, c.loc())
	f := AckForm{Action: "/ui/acks", Label: what, SubjectType: subjectType, SubjectID: subjectID, AlarmType: alarm, PopID: "ack-" + subjectID + "-" + alarm,
		Tomorrow: tomorrow.Format("Mon 2 Jan, 3:04 PM"), Min: now.Add(time.Minute).Format(datetimeLocal), Max: now.Add(store.MaxSnooze - time.Hour).Format(datetimeLocal),
		NextLabel: "Until the next successful backup", Mode: "ack"}
	if subjectType == store.EpisodeCustomer {
		f.NextLabel = "Until the alarm ends"
	}
	return f
}

// ackLine words an acknowledgement in force.
func ackLine(a adminapi.AckJSON, c fmtCtx, s *Service, r *http.Request) *AckLine {
	at := a.AcknowledgedAt
	l := &AckLine{ID: a.ID, By: a.AcknowledgedByName, At: s.when(r.Context(), &at), Note: a.Note, Action: "/ui/acks/" + a.ID + "/clear"}
	if l.By == "" {
		l.By = "an account since deleted"
	}
	switch {
	case a.Kind == adminapi.AckKindSnooze && a.SnoozeUntil != nil:
		l.Lead, l.Badge = "Snoozed", Badge{"SNOOZED", ClassNeutral}
		l.Until = "until " + snoozeEndText(c, *a.SnoozeUntil)
	case a.Kind == adminapi.AckKindSnooze:
		l.Lead, l.Badge = "Snoozed", Badge{"SNOOZED", ClassNeutral}
		l.Until = "until the next successful backup"
		if a.SubjectType == store.EpisodeCustomer {
			l.Until = "until the alarm ends"
		}
	default:
		l.Lead, l.Badge = "Acknowledged", Badge{"ACK", ClassNeutral}
		l.Until = "until cleared or the alarm ends"
	}
	return l
}

// alarmsView builds the block for a subject from its open alarms (state names) and the
// acknowledgements the admin API lists for it. host names the subject in the popover's label.
func (s *Service) alarmsView(r *http.Request, p *auth.Principal, subjectType, subjectID, host string, alarms []string, c fmtCtx) *AlarmsView {
	v := &AlarmsView{CSRF: p.CSRF, Zone: s.zone(r.Context()), CanAck: p.Role >= auth.RoleOperator}
	if !v.CanAck {
		v.Why = "Acknowledging or snoozing an alarm needs the operator role."
	}
	if n, ok := ackNotices[r.URL.Query().Get("ack")]; ok {
		v.Notice = &n
	}
	// Every read that failed is reported, each in its own words: one never hides another (G5).
	var readErrs []string
	var acks adminapi.AcksResponse
	if err := s.apiGet(r, "/api/v1/acks?subject_id="+url.QueryEscape(subjectID), &acks); err != nil {
		readErrs = append(readErrs, "the acknowledgements could not be read, so whether these alarms are acknowledged is unknown")
		acks.Acks = nil
	}
	// Which alarms can be acknowledged now: the API's ack-create accepts an alarm with an open episode
	// of the same subject and state (store.CreateAck). The same store read decides here, so the page
	// never offers an Acknowledge that would answer "not open".
	recorded, unchecked := map[string]bool{}, false
	if eps, err := s.Store.ListOpenEpisodes(r.Context(), p.Scope); err != nil {
		readErrs = append(readErrs, "the open alarms could not be checked, so whether they can be acknowledged now is unknown")
		unchecked = true
	} else {
		for _, e := range eps {
			if e.SubjectType == subjectType && e.SubjectID == subjectID {
				recorded[e.AlarmType] = true
			}
		}
	}
	if len(readErrs) > 0 {
		v.Err = &Note{"crit", "Not everything about these alarms could be read.", capFirst(strings.Join(readErrs, "; and ")) + ". This is an alarm, not \"none\". Reload the page to try again."}
	}
	notYet := "Not recorded yet: it can be acknowledged after the console's next alarm check, within about " + durText(jobs.AlarmEvaluatorEvery.Milliseconds()) + "."
	if s.Fixtures {
		notYet = "Not recorded yet: it can be acknowledged after the console's next alarm check. Fixtures mode runs none."
	}
	if unchecked { // H3: what was not read is not stated as fact; no form is offered for it either
		notYet = "Could not be checked: reload the page."
	}
	byType := map[string]adminapi.AckJSON{}
	for _, a := range acks.Acks {
		if a.SubjectType == subjectType && a.SubjectID == subjectID {
			byType[a.AlarmType] = a
		}
	}
	seen := map[string]bool{}
	add := func(alarm string) {
		t, ok := store.AckAlarmType(alarm)
		if !ok || seen[t] {
			return
		}
		seen[t] = true
		st := state.State(alarm)
		label := stateLabel(st)
		class := ClassOf(st)
		if alarm == store.AlarmMissing {
			label, class = "MISSING", ClassCrit
		}
		row := AlarmRow{Chip: Chip{ID: "al-" + subjectID + "-" + t, Label: label, Class: class}, Form: ackForm(subjectType, subjectID, t, label+" on "+host, c, v.Zone),
			Recorded: recorded[alarm]}
		if !row.Recorded {
			row.NotYet = notYet
		}
		if a, ok := byType[t]; ok {
			row.Ack = ackLine(a, c, s, r)
			row.Form.Change, row.Form.Note = true, a.Note // "Change…" opens with what is in force
			switch {
			case a.Kind == adminapi.AckKindSnooze && a.SnoozeUntil != nil:
				row.Form.Mode, row.Form.Until = "until", a.SnoozeUntil.In(c.loc()).Format(datetimeLocal)
			case a.Kind == adminapi.AckKindSnooze:
				row.Form.Mode = "next"
			}
		}
		v.Rows = append(v.Rows, row)
	}
	for _, a := range alarms {
		add(a)
	}
	for t := range byType { // an acknowledgement whose alarm the view no longer flags: still listed, so it can be cleared
		if alarm, ok := store.EpisodeAlarmOfAck(t); ok {
			add(alarm)
		}
	}
	// Nothing to show only when there is no alarm, no read error and no answer to report: the outcome of
	// an acknowledgement (not_open, cleared, a refusal) is shown even when no alarm is open any more.
	if len(v.Rows) == 0 && v.Err == nil && v.Notice == nil {
		return nil
	}
	return v
}

// deviceAlarms are the device's alarms that an episode records: its alarm flags (the flags the
// evaluator syncs into episodes, notify.episodeKeys) of an episode alarm type.
func deviceAlarms(ds state.DeviceState) []string {
	open := map[string]bool{}
	for _, a := range store.EpisodeAlarmTypes(store.EpisodeDevice) {
		open[a] = true
	}
	var out []string
	for _, f := range ds.AlarmFlags {
		if open[string(f)] {
			out = append(out, string(f))
		}
	}
	return out
}

// snoozeEnd is the end of a snooze preset at now in the operator's zone loc: one hour on, tomorrow at
// 8:00 AM, or the same local time of day 3 or 7 days on, so that across a change to or from daylight
// saving time "3 days" ends at the hour the operator expects rather than 72 hours on. On a day where that
// local time does not exist (the clocks jump over it) or exists twice (the clocks go back over it), Go's
// time.Date picks one of the two offsets of the transition, which one is not specified: the end is then
// never more than an hour from the local time asked for. The page shows the stored end with its zone
// abbreviation (snoozeEndText: "1:30 AM CDT" or "1:30 AM CST"), so the operator sees which it was.
func snoozeEnd(mode string, now time.Time, loc *time.Location) (time.Time, bool) {
	l := now.In(loc)
	switch mode {
	case "1h":
		return now.Add(time.Hour), true
	case "tomorrow":
		return time.Date(l.Year(), l.Month(), l.Day()+1, 8, 0, 0, 0, loc), true
	case "3d":
		return time.Date(l.Year(), l.Month(), l.Day()+3, l.Hour(), l.Minute(), l.Second(), l.Nanosecond(), loc), true
	case "1w":
		return time.Date(l.Year(), l.Month(), l.Day()+7, l.Hour(), l.Minute(), l.Second(), l.Nanosecond(), loc), true
	}
	return time.Time{}, false
}

// snoozeEndText is a snooze or acknowledgement end as every page shows it: the day, the clock and the
// zone abbreviation, so an end in the hour the clocks repeat says which of the two it is (H2).
func snoozeEndText(c fmtCtx, t time.Time) string {
	return c.dayClock(t) + " " + c.zoneAbbr(t)
}

// ---- writes ----

// ackBack returns to the page of the subject: the device panel or the customer page, never a URL
// the form names.
func ackBack(w http.ResponseWriter, r *http.Request, subjectType, subjectID, code string) {
	base := "/devices/"
	if subjectType == store.EpisodeCustomer {
		base = "/customers/"
	}
	http.Redirect(w, r, base+url.PathEscape(subjectID)+"?ack="+code+"#alarms", http.StatusSeeOther)
}

// ackSubject reads and checks the subject fields of an acknowledgement form; false when it answered.
func (s *Service) ackSubject(w http.ResponseWriter, r *http.Request, p *auth.Principal) (string, string, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if err := r.ParseForm(); err != nil {
		s.errorPage(w, r, p, http.StatusBadRequest, "Form not read", "The form could not be read.")
		return "", "", false
	}
	typ, id := r.PostFormValue("subject_type"), r.PostFormValue("subject_id")
	if !idParam(id) {
		s.notFound(w, r, p, "This alarm's device or customer")
		return "", "", false
	}
	switch typ {
	case store.EpisodeDevice:
		if _, err := s.Store.GetDevice(r.Context(), p.Scope, id); err != nil {
			s.notFound(w, r, p, "This device")
			return "", "", false
		}
	case store.EpisodeCustomer:
		if _, err := s.Store.GetCustomerRow(r.Context(), p.Scope, id); err != nil {
			s.notFound(w, r, p, "This customer")
			return "", "", false
		}
	default:
		s.notFound(w, r, p, "This alarm's device or customer")
		return "", "", false
	}
	return typ, id, true
}

func (s *Service) ackCreate(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	typ, id, ok := s.ackSubject(w, r, p)
	if !ok {
		return
	}
	alarm := r.PostFormValue("alarm_type")
	if !ackTypeRE.MatchString(alarm) {
		ackBack(w, r, typ, id, "err_type")
		return
	}
	body := adminapi.CreateAckRequest{SubjectType: typ, SubjectID: id, AlarmType: alarm, Kind: adminapi.AckKindSnooze,
		Note: strings.TrimSpace(strings.ReplaceAll(r.PostFormValue("note"), "\r\n", "\n"))}
	c := s.fc(r.Context())
	now := c.Now.In(c.loc())
	until := func(t time.Time) { u := t.UTC(); body.SnoozeUntil = &u }
	switch mode := r.PostFormValue("mode"); mode {
	case "ack":
		body.Kind = adminapi.AckKindAcknowledge
	case "1h", "tomorrow", "3d", "1w":
		end, _ := snoozeEnd(mode, now, c.loc())
		until(end)
	case "next":
		body.UntilNextSuccess = true
	case "until":
		raw := strings.TrimSpace(r.PostFormValue("until"))
		t, err := time.ParseInLocation(datetimeLocal, raw, c.loc())
		if err != nil {
			ackBack(w, r, typ, id, "err_until")
			return
		}
		if t.Format(datetimeLocal) != raw { // a time the clocks skip: ask again (as the park dialog does)
			ackBack(w, r, typ, id, "err_gap")
			return
		}
		until(t)
	default:
		ackBack(w, r, typ, id, "err_mode")
		return
	}
	status, resp, err := s.apiSend(r, http.MethodPost, "/api/v1/acks", body)
	if s.writeFailed(w, r, p, status, err) {
		return
	}
	switch status {
	case http.StatusCreated:
		code := "snoozed"
		if body.Kind == adminapi.AckKindAcknowledge {
			code = "acked"
		}
		ackBack(w, r, typ, id, code)
	case http.StatusConflict:
		ackBack(w, r, typ, id, "not_open")
	case http.StatusNotFound:
		s.notFound(w, r, p, "This device or customer")
	case http.StatusBadRequest:
		code := "err_fail"
		for _, fe := range fieldErrors(resp) {
			switch fe.Field {
			case "snooze_until":
				code = "err_until"
			case "note":
				code = "err_note"
			case "alarm_type", "subject_type", "subject_id":
				code = "err_type"
			case "kind":
				code = "err_mode"
			}
		}
		ackBack(w, r, typ, id, code)
	default:
		s.writeRefused(w, r, p, status, "ui: acknowledgement refused")
	}
}

func (s *Service) ackClear(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	typ, id, ok := s.ackSubject(w, r, p)
	if !ok {
		return
	}
	ack := r.PathValue("id")
	if !idParam(ack) {
		ackBack(w, r, typ, id, "ack_gone")
		return
	}
	status, _, err := s.apiSend(r, http.MethodDelete, "/api/v1/acks/"+url.PathEscape(ack), nil)
	if s.writeFailed(w, r, p, status, err) {
		return
	}
	switch status {
	case http.StatusNoContent:
		ackBack(w, r, typ, id, "cleared")
	case http.StatusNotFound:
		ackBack(w, r, typ, id, "ack_gone")
	default:
		s.writeRefused(w, r, p, status, "ui: acknowledgement clear refused")
	}
}
