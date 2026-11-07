package adminapi

import (
	"net/http"
	"strings"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/health"
	"github.com/c2tech-sys/cryobackup/internal/console/httpx"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// M3-T3: the admin read API of the state engine and the fleet board
// (architecture §5.2 rows with from = M3). Every fleet-derived answer here
// comes from one health.Evaluator.Fleet call, the same FleetView the board and
// the SSE stream render, so the API and the pages cannot disagree; no route
// computes a state of its own.
//
// Route patterns. They are exported so the route-coverage tests refer to the
// same strings.
const (
	PatternCustomerHealth     = "GET /api/v1/customers/{id}/health"
	PatternDeviceState        = "GET /api/v1/devices/{id}/state"
	PatternDeviceHeartbeats   = "GET /api/v1/devices/{id}/heartbeats"
	PatternDeviceTenDay       = "GET /api/v1/devices/{id}/ten-day"
	PatternDevicePause        = "POST /api/v1/devices/{id}/pause"
	PatternDeviceResume       = "POST /api/v1/devices/{id}/resume"
	PatternRuns               = "GET /api/v1/runs"
	PatternRun                = "GET /api/v1/runs/{id}"
	PatternStorageTargetProbe = "POST /api/v1/storage-targets/{id}/probe"
	PatternAlarms             = "GET /api/v1/alarms"
	PatternViews              = "GET /api/v1/views"
	PatternViewCreate         = "POST /api/v1/views"
	PatternViewPatch          = "PATCH /api/v1/views/{id}"
	PatternViewDelete         = "DELETE /api/v1/views/{id}"
	PatternFleetSummary       = "GET /api/v1/fleet/summary"
	PatternFleetBanners       = "GET /api/v1/fleet/banners"
	PatternFleetOverdue       = "GET /api/v1/fleet/overdue"
	PatternFleetUnseen        = "GET /api/v1/fleet/unseen"
	PatternFleetTenDay        = "GET /api/v1/fleet/ten-day"
	PatternStatus             = "GET /api/v1/status"
	PatternEvents             = "GET /api/v1/events"
	PatternSystemHealth       = "GET /api/v1/system/health"
	PatternSystemJobs         = "GET /api/v1/system/jobs"
	PatternSystemJobRun       = "POST /api/v1/system/jobs/{name}/run"
	// PatternMetrics is the Prometheus text exposition (architecture §5.3). It is
	// not under /api/v1, so the OpenAPI diff does not list it.
	PatternMetrics = "GET /metrics"
)

// m3Patterns lists the patterns registerM3 adds.
func m3Patterns() []string {
	return []string{PatternCustomerHealth, PatternDeviceState, PatternDeviceHeartbeats, PatternDeviceTenDay, PatternDevicePause, PatternDeviceResume, PatternDeviceRetire,
		PatternRuns, PatternRun, PatternStorageTargetProbe, PatternAlarms, PatternViews, PatternViewCreate, PatternViewPatch, PatternViewDelete,
		PatternFleetSummary, PatternFleetBanners, PatternFleetOverdue, PatternFleetUnseen, PatternFleetTenDay, PatternStatus, PatternEvents,
		PatternSystemHealth, PatternSystemJobs, PatternSystemJobRun, PatternMetrics}
}

// registerM3 adds the M3 routes. Reads need read_only; pause and resume
// operator; retiring a device, the job table, running a job and the storage
// probe admin.
//
// /status and /metrics are for external monitors and take an API token. The
// architecture names narrower token scopes for them (status:read, metrics:read);
// auth.ScopeRead and auth.ScopeFull (read-only role or the account's role) are
// the only scopes the token store knows today, so both routes accept a session
// or any live token at read-only level and no more.
func (s *Service) registerM3(r httpx.Registrar) {
	ro := func(h http.HandlerFunc) http.Handler { return s.Auth.Require(auth.RoleReadOnly, auth.ModeAPI, h) }
	op := func(h http.HandlerFunc) http.Handler { return s.Auth.Require(auth.RoleOperator, auth.ModeAPI, h) }
	ad := func(h http.HandlerFunc) http.Handler { return s.Auth.Require(auth.RoleAdmin, auth.ModeAPI, h) }
	r.Handle(PatternFleetSummary, ro(s.fleetSummary))
	r.Handle(PatternFleetBanners, ro(s.fleetBanners))
	r.Handle(PatternFleetOverdue, ro(s.fleetOverdue))
	r.Handle(PatternFleetUnseen, ro(s.fleetUnseen))
	r.Handle(PatternFleetTenDay, ro(s.fleetTenDay))
	r.Handle(PatternAlarms, ro(s.alarms))
	r.Handle(PatternCustomerHealth, ro(s.customerHealth))
	r.Handle(PatternDeviceState, ro(s.deviceState))
	r.Handle(PatternDeviceHeartbeats, ro(s.deviceHeartbeats))
	r.Handle(PatternDeviceTenDay, ro(s.deviceTenDay))
	r.Handle(PatternDevicePause, op(s.devicePause))
	r.Handle(PatternDeviceResume, op(s.deviceResume))
	r.Handle(PatternDeviceRetire, ad(s.deviceRetire)) // M3-T6 (retire.go)
	r.Handle(PatternRuns, ro(s.searchRuns))
	r.Handle(PatternRun, ro(s.getRunDetail))
	r.Handle(PatternViews, ro(s.listViews))
	r.Handle(PatternViewCreate, ro(s.createView))
	r.Handle(PatternViewPatch, ro(s.patchView))
	r.Handle(PatternViewDelete, ro(s.deleteView))
	r.Handle(PatternStatus, ro(s.status))
	r.Handle(PatternMetrics, ro(s.metrics))
	r.Handle(PatternEvents, ro(s.events))
	r.Handle(PatternSystemHealth, ro(s.systemHealth))
	r.Handle(PatternSystemJobs, ad(s.systemJobs))
	r.Handle(PatternSystemJobRun, ad(s.systemJobRun))
	r.Handle(PatternStorageTargetProbe, ad(s.storageTargetProbe))
}

// ---- shared shapes ----

// BannerJSON is one console-level condition (architecture §4.6). Severity is
// "critical" or "warn".
type BannerJSON struct {
	ID       string     `json:"id"`
	Subject  string     `json:"subject,omitempty"`
	Severity string     `json:"severity"`
	Text     string     `json:"text"`
	Count    int        `json:"count,omitempty"`
	At       *time.Time `json:"at,omitempty"`
}

func bannerSeverity(s state.Severity) string {
	if s >= state.SeverityCritical {
		return "critical"
	}
	return "warn"
}

func bannersJSON(in []state.Banner) []BannerJSON {
	out := make([]BannerJSON, 0, len(in))
	for _, b := range in {
		out = append(out, BannerJSON{ID: b.ID, Subject: b.Subject, Severity: bannerSeverity(b.Severity), Text: b.Text, Count: b.Count, At: ptrTime(b.At)})
	}
	return out
}

// StampJSON is the freshness stamp every fleet-derived body carries.
type StampJSON struct {
	ComputedAt time.Time `json:"computed_at"`
	LoadedAt   time.Time `json:"loaded_at"`
	// Complete is false when any part of the facts load failed: the answer is a
	// lower bound and says so, it is never read as "all fine".
	Complete bool `json:"complete"`
}

// fleetData is the evaluated fleet inside the caller's scope, optionally
// narrowed to one customer. The banners stay console-wide: a condition of the
// console is not a condition of one customer.
type fleetData struct {
	health.Fleet
	Customer  string // the customer filter, "" for the whole scope
	Devices   []state.DeviceState
	Customers []state.CustomerState
	Collapses []state.Collapse
	names     map[string]string
}

func (f fleetData) stamp() StampJSON {
	return StampJSON{ComputedAt: f.Computed, LoadedAt: f.View.LoadedAt, Complete: f.View.Complete}
}

func (f fleetData) customerName(id string) string { return f.names[id] }

// loadFleet evaluates the fleet for the caller and applies the optional
// customer query parameter. A customer outside the caller's scope is 404,
// exactly like one that does not exist. It writes the problem and returns false
// when it cannot answer.
func (s *Service) loadFleet(w http.ResponseWriter, r *http.Request, p *auth.Principal) (fleetData, bool) {
	cust := strings.TrimSpace(r.URL.Query().Get("customer"))
	if cust != "" {
		if _, err := s.Store.GetCustomer(r.Context(), p.Scope, cust); err != nil {
			s.failM2(w, r, err)
			return fleetData{}, false
		}
	}
	f, err := s.Fleet.Fleet(r.Context(), p.Scope)
	if err != nil {
		s.fail(w, r, err)
		return fleetData{}, false
	}
	fd := fleetData{Fleet: f, Customer: cust, names: map[string]string{}}
	for _, c := range f.View.Customers {
		fd.names[c.ID] = c.Name
		if cust == "" || c.ID == cust {
			fd.Customers = append(fd.Customers, c)
		}
	}
	for _, d := range f.View.Devices {
		if cust == "" || d.CustomerID == cust {
			fd.Devices = append(fd.Devices, d)
		}
	}
	for _, c := range f.View.Collapses {
		if cust == "" || c.Scope == state.CollapseFleet || c.CustomerID == cust {
			fd.Collapses = append(fd.Collapses, c)
		}
	}
	return fd, true
}

// ---- classification helpers (read the engine's verdicts; no state of their own) ----

// hasState reports whether d is in st as its primary state or any flag.
func hasState(d state.DeviceState, st state.State) bool {
	if d.Primary == st {
		return true
	}
	for _, f := range d.Flags {
		if f == st {
			return true
		}
	}
	return false
}

// overdueStates are the flags that make a device overdue: the last success is
// older than the limit, a scheduled run did not happen, or no success exists
// and the first-backup grace is gone.
var overdueStates = []state.State{state.StateStale, state.StateMissed, state.StateBlank}

// overdueChips lists d's overdue flags in precedence order, the "reason chips"
// of the overdue list.
func overdueChips(d state.DeviceState) []string {
	var out []string
	for _, st := range overdueStates {
		if hasState(d, st) {
			out = append(out, string(st))
		}
	}
	return out
}

// isWarning is the board's "warning" bucket: an alarm-level state, or a quiet
// state that still needs a look (a missed run, a deferral, an old agent).
func isWarning(d state.DeviceState) bool {
	if d.Alarm == state.SeverityAlarm {
		return true
	}
	if d.Alarm != state.SeverityNone {
		return false
	}
	switch d.Primary {
	case state.StateMissed, state.StateDeferred, state.StateOutdated:
		return true
	}
	return false
}
