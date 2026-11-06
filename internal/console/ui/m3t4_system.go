package ui

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminapi"
	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/logx"
)

// System health (S36), M3-T4: one row per component, read from the same
// handler as GET /api/v1/system/health, plus the job table for administrators
// with "Run now".
//
// A component row says "not checked" when the console has no evidence, and
// never "ok" in that case: the API decides (a banner raised for the row, C00
// when the console's self-checks are not wired, a probe that has not run) and
// this page only shows its verdict. "Run now" goes through the same handler
// as POST /api/v1/system/jobs/{name}/run, so it writes exactly one audit row,
// and a run whose audit row cannot be written starts nothing.

// SysBanner is a banner raised for a row.
type SysBanner struct {
	ID    string
	Label string
}

// SysRow is one component of the health table.
type SysRow struct {
	ID      string
	Label   string
	Status  Chip
	Detail  string
	Banners []SysBanner
	Checked string
}

// SysCondition is a console banner as listed under the table.
type SysCondition struct {
	ID    string
	Label string
	Class string
	Text  string
	Row   string // the component the banner speaks for
}

// SysJob is one row of the job table.
type SysJob struct {
	Name     string
	Status   Chip
	Overdue  bool
	Last     string
	LastSub  string
	Duration string
	Next     string
	NextCls  string
	Runs     int
	ErrClass string
	CanRun   bool
	Why      string // why Run now is not offered
	CSRF     string
	Action   string
}

// SysJobs is the job table (administrators only).
type SysJobs struct {
	Rows []SysJob
	Err  *EmptyView
}

// SystemPage is S36.
type SystemPage struct {
	Page
	Overall    Chip
	OverallTxt string
	Rows       []SysRow
	Conditions []SysCondition
	Versions   []Fact
	Jobs       *SysJobs // nil below administrator
	JobsNote   string   // what a non-administrator sees in place of the table
	Flash      *CauseView
	Empty      *EmptyView
	AsOf       string
	Checked    string
	Live       string // this page's own URL, polled to refresh the live region
}

// sysStatusChip maps an API component status to a chip. not_checked is never
// calm: it is an amber, dashed NOT CHECKED, because no evidence is no health.
func sysStatusChip(id, status string) Chip {
	switch status {
	case "ok":
		return Chip{ID: id, Label: "OK", Class: ClassOK}
	case "warn":
		return Chip{ID: id, Label: "WARNING", Class: ClassWarn}
	case "failed":
		return Chip{ID: id, Label: "FAILED", Class: ClassCrit}
	case "not_checked":
		return Chip{ID: id, Label: "NOT CHECKED", Class: ClassWarn, Dashed: true}
	}
	return Chip{ID: id, Label: "UNKNOWN", Class: ClassCrit, Dashed: true}
}

func (s *Service) systemPage(w http.ResponseWriter, r *http.Request) {
	s.renderSystem(w, r, http.StatusOK, s.acceptedFlash(principal(r), r.URL.Query().Get("flash")))
}

// renderSystem renders S36 with an optional flash (the answer to "Run now").
func (s *Service) renderSystem(w http.ResponseWriter, r *http.Request, status int, flash *CauseView) {
	p := principal(r)
	fleet, ok := s.fleetFor(w, r, p, "System")
	if !ok {
		return
	}
	c := s.fcAt(r.Context(), fleet.Computed)
	pg := SystemPage{Page: s.page(r.Context(), p, fleet, "System health", "system", "/ui/events"), AsOf: c.clock(fleet.Computed)}
	pg.Flash, pg.Live = flash, r.URL.RequestURI()
	var h adminapi.SystemHealthResponse
	if err := s.apiGet(r, "/api/v1/system/health", &h); err != nil {
		s.log().Error("ui: system health failed", logx.Class("render"), logx.Err(err))
		pg.Empty = apiFailure("system health", err)
		pg.Empty.Body += " No component is shown as OK while the console cannot read its own health."
	} else {
		pg.Overall = sysStatusChip("sys-overall", h.Overall)
		pg.OverallTxt = map[string]string{
			"ok": "Every component has evidence and is healthy.", "warn": "At least one component needs attention.",
			"failed": "At least one component has failed.", "not_checked": "At least one component has no evidence: it is not being monitored.",
		}[h.Overall]
		if pg.OverallTxt == "" {
			pg.OverallTxt = "The overall state is not recognised: treat it as not checked."
		}
		rowOf := map[string]string{}
		for _, comp := range h.Components {
			row := SysRow{ID: comp.ID, Label: comp.Label, Status: sysStatusChip("sys-"+comp.ID, comp.Status), Detail: capFirst(comp.Detail)}
			for _, id := range comp.Banners {
				row.Banners = append(row.Banners, SysBanner{ID: id, Label: BannerLabel(id)})
				rowOf[id] = comp.Label
			}
			if comp.CheckedAt != nil {
				row.Checked = c.dayClock(*comp.CheckedAt)
			}
			pg.Rows = append(pg.Rows, row)
		}
		if len(pg.Rows) == 0 {
			pg.Empty = &EmptyView{Kind: "alarm", Title: "No components reported", Body: "The console returned no component at all. That is an alarm, not a clean bill of health."}
		}
		for _, b := range h.Banners {
			cls := ClassWarn
			if b.Severity == "critical" {
				cls = ClassCrit
			}
			pg.Conditions = append(pg.Conditions, SysCondition{ID: b.ID, Label: BannerLabel(b.ID), Class: cls, Text: b.Text, Row: rowOf[b.ID]})
		}
		pg.Checked = fmt.Sprintf("Computed %s (%s)%s", c.clock(h.ComputedAt), c.Zone, map[bool]string{true: "", false: ". The facts load was incomplete, so this page is a lower bound."}[h.Complete])
		pg.Versions = versionFacts(h.Versions, c)
	}
	if p.Role >= auth.RoleAdmin {
		pg.Jobs = s.jobsView(r, p, c)
	} else {
		pg.JobsNote = "The job table and Run now are for administrators."
	}
	s.render(w, r, status, "system", "base", pg)
}

func versionFacts(v adminapi.VersionsJSON, c fmtCtx) []Fact {
	out := []Fact{{Key: "Console", Value: safeVersion(v.Console), Mono: true}}
	if v.Commit != "" {
		out = append(out, Fact{Key: "Build", Value: safeCommit(v.Commit) + " · " + safeBuildDate(v.BuildDate), Mono: true})
	}
	out = append(out, Fact{Key: "Database schema", Value: strconv.Itoa(v.Schema), Mono: true})
	switch w := v.Maintain; {
	case w == nil:
		out = append(out, Fact{Key: "Maintenance worker", Value: "not probed: this console has no worker client"})
	case !w.Reachable:
		out = append(out, Fact{Key: "Maintenance worker", Value: "not reachable"})
	default:
		ver := safeVersion(w.Version)
		key := "master key loaded"
		if !w.MasterKeyLoaded {
			key = "master key NOT loaded"
		}
		out = append(out, Fact{Key: "Maintenance worker", Value: "reachable · " + ver + " · " + key})
		out = append(out, Fact{Key: "Engine tools", Value: "restic " + safeVersion(orUnknown(w.ResticVersion)) + " · rclone " + safeVersion(orUnknown(w.RcloneVersion))})
	}
	return out
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// jobsView reads the job table through the admin API.
func (s *Service) jobsView(r *http.Request, p *auth.Principal, c fmtCtx) *SysJobs {
	v := &SysJobs{}
	var jl adminapi.JobListResponse
	if err := s.apiGet(r, "/api/v1/system/jobs", &jl); err != nil {
		s.log().Error("ui: job table failed", logx.Class("render"), logx.Err(err))
		v.Err = apiFailure("the job table", err)
		return v
	}
	now := jl.ComputedAt
	for _, j := range jl.Jobs {
		row := SysJob{Name: j.Name, Overdue: j.Overdue, Runs: j.RunCount, ErrClass: "", CSRF: p.CSRF, Action: "/ui/system/jobs/" + url.PathEscape(j.Name) + "/run"}
		if !jobNameRE.MatchString(j.Name) { // the console names its jobs; anything else is not shown, and not run from here
			row.Name, row.Action = withheld, ""
		}
		id := "job-" + j.Name
		switch {
		case j.Running:
			row.Status = Chip{ID: id, Label: "RUNNING", Class: ClassInfo, Pulse: true}
		case j.Status == "ok":
			row.Status = Chip{ID: id, Label: "OK", Class: ClassOK}
		case j.Status == "error":
			row.Status = Chip{ID: id, Label: "ERROR", Class: ClassCrit}
		case j.Status == "skipped":
			row.Status = Chip{ID: id, Label: "SKIPPED", Class: ClassNeutral}
		case j.Status == "never_run":
			row.Status = Chip{ID: id, Label: "NEVER RUN", Class: ClassWarn, Dashed: true}
		default:
			row.Status = Chip{ID: id, Label: "UNKNOWN", Class: ClassCrit, Dashed: true}
		}
		if j.LastErrorClass != "" {
			row.ErrClass = safeJobClass(j.LastErrorClass)
		}
		switch {
		case j.LastFinishedAt != nil:
			row.Last, row.LastSub = ageLong(now.Sub(*j.LastFinishedAt)), c.dayClock(*j.LastFinishedAt)
		case j.LastStartedAt != nil:
			row.Last, row.LastSub = "started "+ageLong(now.Sub(*j.LastStartedAt)), c.dayClock(*j.LastStartedAt)
		default:
			row.Last = "never"
		}
		if j.LastDurationMS != nil {
			row.Duration = durText(*j.LastDurationMS)
		}
		if j.NextDueAt != nil {
			if d := j.NextDueAt.Sub(now); d > 0 {
				row.Next = ageFuture(d)
			} else {
				row.Next, row.NextCls = "due "+ageLong(-d), "warn"
			}
		}
		switch {
		case row.Action == "":
			row.Why = "not a job this console names"
		case !j.Registered:
			row.Why = "not registered in this console"
		case j.Running:
			row.Why = "already running"
		default:
			row.CanRun = true
		}
		v.Rows = append(v.Rows, row)
	}
	if len(v.Rows) == 0 {
		v.Err = &EmptyView{Kind: "alarm", Title: "No jobs are known", Body: "The console lists no background job and none has ever run. The alarm evaluator, the health prober and the cleanup jobs should all be here: an empty table means the job runner is not running."}
	}
	return v
}

// systemFlash is the answer to a "Run now" that did not start a job. It is
// rendered with the error status, never carried in a URL.
func systemFlash(name, res string) *CauseView {
	if !jobNameRE.MatchString(name) {
		return nil
	}
	switch res {
	case "busy":
		return &CauseView{Class: ClassWarn, Title: "Not started.", Text: name + " is already running. Nothing was started and nothing was recorded."}
	case "unknown":
		return &CauseView{Class: ClassWarn, Title: "Not started.", Text: name + " is not a job this console runs."}
	case "unavailable":
		return &CauseView{Class: ClassCrit, Title: "Not started.", Text: "This console cannot run jobs now: the job runner is not available, or the console is shutting down."}
	case "refused":
		return &CauseView{Class: ClassCrit, Title: "Refused (NOT AUDITED).", Text: "The console could not write the audit entry for this run, or could not read the job's state, so nothing was started. Check the console database and try again."}
	case "forbidden":
		return &CauseView{Class: ClassCrit, Title: "Not allowed.", Text: "Only administrators can run a job."}
	}
	return &CauseView{Class: ClassCrit, Title: "Not started.", Text: "The console could not start " + name + ". Try again shortly."}
}

// The accepted answer is the one a URL could fake ("Run accepted" with no run),
// so it travels as a flash token: name, expiry and an HMAC over both and the
// caller's session secret, keyed with a random per-process key. A token made for
// another session, another job, or another time does not verify, and neither does
// one older than flashTTL. The page itself says nothing it did not check.
const flashTTL = 2 * time.Minute

func (s *Service) flashMAC(p *auth.Principal, name string, exp int64) string {
	m := hmac.New(sha256.New, s.m3.flashKey[:])
	m.Write([]byte(p.CSRF + "|" + name + "|" + strconv.FormatInt(exp, 10)))
	return hex.EncodeToString(m.Sum(nil))
}

func (s *Service) flashToken(p *auth.Principal, name string) string {
	exp := s.now().Add(flashTTL).Unix()
	return url.QueryEscape(name + "~" + strconv.FormatInt(exp, 10) + "~" + s.flashMAC(p, name, exp))
}

// acceptedFlash is the "Run accepted" banner for a verified token, else nil.
func (s *Service) acceptedFlash(p *auth.Principal, token string) *CauseView {
	parts := strings.Split(token, "~")
	if len(parts) != 3 || !jobNameRE.MatchString(parts[0]) {
		return nil
	}
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || s.now().Unix() > exp || !hmac.Equal([]byte(parts[2]), []byte(s.flashMAC(p, parts[0], exp))) {
		return nil
	}
	return &CauseView{Class: ClassOK, Title: "Run accepted.", Text: parts[0] + " was started on this console, outside its schedule. Its result appears in the job table when it finishes. One audit entry was written."}
}

var jobNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// systemJobRun is POST /ui/system/jobs/{name}/run: it asks the admin API's own
// handler to run the job, so the audit row, the busy check and the shutdown
// gate are the API's. An accepted run redirects back to the table (303); every
// other answer is the System page itself with the matching error status and the
// reason, so a refused run can never look like a started one.
func (s *Service) systemJobRun(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !jobNameRE.MatchString(name) {
		http.NotFound(w, r)
		return
	}
	if s.jobsOf() == nil { // no runner at all: say so, and write no audit row
		s.renderSystem(w, r, http.StatusServiceUnavailable, systemFlash(name, "unavailable"))
		return
	}
	status, _, err := s.apiCall(r, http.MethodPost, "/api/v1/system/jobs/"+url.PathEscape(name)+"/run")
	result, code := "failed", http.StatusBadGateway
	switch {
	case err != nil:
		s.log().Error("ui: run now failed", logx.Class("render"), logx.Err(err))
	case status == http.StatusAccepted:
		http.Redirect(w, r, "/system?flash="+s.flashToken(principal(r), name)+"#jobs", http.StatusSeeOther)
		return
	case status == http.StatusConflict:
		result, code = "busy", http.StatusConflict
	case status == http.StatusNotFound:
		result, code = "unknown", http.StatusNotFound
	case status == http.StatusServiceUnavailable:
		result, code = "unavailable", http.StatusServiceUnavailable
	case status == http.StatusForbidden:
		result, code = "forbidden", http.StatusForbidden
	case status == http.StatusInternalServerError:
		result, code = "refused", http.StatusInternalServerError // a refused audit row, or a job state that could not be read: either way nothing started
	}
	s.renderSystem(w, r, code, systemFlash(name, result))
}
