package adminapi

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/httpx"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// ---- status ----

// Overall verdicts of GET /api/v1/status.
const (
	OverallOK       = "ok"
	OverallWarning  = "warning"
	OverallCritical = "critical"
	OverallUnknown  = "unknown"
)

// StatusDeviceJSON is a device as an external monitor sees it.
type StatusDeviceJSON struct {
	ID            string     `json:"id"`
	CustomerID    string     `json:"customer_id"`
	Hostname      string     `json:"hostname"`
	State         string     `json:"state"`
	Flags         []string   `json:"flags"`
	Severity      string     `json:"severity"`
	Since         *time.Time `json:"since,omitempty"`
	LastSuccessAt *time.Time `json:"last_success_at,omitempty"`
	LastSeenAt    *time.Time `json:"last_seen_at,omitempty"`
	Acknowledged  bool       `json:"acknowledged"`
}

// StatusCustomerJSON is a customer's rollup as an external monitor sees it.
type StatusCustomerJSON struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Status    string         `json:"status"`
	Severity  string         `json:"severity"`
	Active    int            `json:"active"`
	Paused    int            `json:"paused"`
	Counts    map[string]int `json:"counts"`
	HealthPct *float64       `json:"health_pct,omitempty"`
	Muted     bool           `json:"muted"`
}

// StatusResponse is GET /api/v1/status: the state of every device and customer
// and one overall verdict, for a monitor that polls the console. Overall is ok
// only when the engine says all clear (no banner, nothing alarming, complete
// data, a non-empty fleet); critical when anything is critical; unknown when the
// data is incomplete or the fleet is empty without being a first run; warning
// otherwise. The HTTP status is 200 in every case: the verdict is in the body,
// so a monitor can tell "the console says critical" from "the console is down".
type StatusResponse struct {
	StampJSON
	Overall   string               `json:"overall"`
	AllClear  bool                 `json:"all_clear"`
	FirstRun  bool                 `json:"first_run"`
	Banners   []BannerJSON         `json:"banners"`
	Customers []StatusCustomerJSON `json:"customers"`
	Devices   []StatusDeviceJSON   `json:"devices"`
}

// overall folds a fleet view into the one verdict. It is a reading of the
// engine's output, not a second evaluation.
func overall(v state.FleetView, devices []state.DeviceState, customers []state.CustomerState) string {
	critical, warn, unknown := false, false, !v.Complete
	for _, b := range v.Banners {
		if b.Severity >= state.SeverityCritical {
			critical = true
		} else {
			warn = true
		}
	}
	for _, d := range devices {
		switch {
		case d.Primary == state.StateRetired:
		case d.Alarm >= state.SeverityCritical:
			critical = true
		case d.Primary == state.StateUnknown:
			unknown = true
		case d.Alarm > state.SeverityNone || isWarning(d):
			warn = true
		}
	}
	for _, c := range customers {
		switch {
		case c.Alarm >= state.SeverityCritical || c.Status == state.CustomerMissing:
			critical = true
		case c.Status == state.CustomerUnknown:
			unknown = true
		case c.Status == state.CustomerWarn || c.Alarm > state.SeverityNone:
			warn = true
		}
	}
	if len(v.Devices) == 0 && !v.FirstRun {
		unknown = true
	}
	switch {
	case critical:
		return OverallCritical
	case unknown:
		return OverallUnknown
	case warn:
		return OverallWarning
	case v.AllClear():
		return OverallOK
	}
	// Not all clear for a reason none of the above names: never ok.
	return OverallWarning
}

func (s *Service) status(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	fd, ok := s.loadFleet(w, r, p)
	if !ok {
		return
	}
	out := StatusResponse{StampJSON: fd.stamp(), AllClear: fd.View.AllClear(), FirstRun: fd.View.FirstRun, Banners: bannersJSON(fd.View.Banners),
		Overall: overall(fd.View, fd.Devices, fd.Customers), Customers: make([]StatusCustomerJSON, 0, len(fd.Customers)), Devices: make([]StatusDeviceJSON, 0, len(fd.Devices))}
	for _, c := range fd.Customers {
		counts := map[string]int{}
		for st, n := range c.Counts {
			if n > 0 {
				counts[string(st)] = n
			}
		}
		row := StatusCustomerJSON{ID: c.ID, Name: c.Name, Status: string(c.Status), Severity: c.Alarm.String(), Active: c.Active, Paused: c.Paused, Counts: counts, Muted: c.Muted}
		if c.HealthKnown {
			v := c.HealthPct
			row.HealthPct = &v
		}
		out.Customers = append(out.Customers, row)
	}
	for _, d := range fd.Devices {
		flags := strs(d.Flags)
		if flags == nil {
			flags = []string{}
		}
		out.Devices = append(out.Devices, StatusDeviceJSON{ID: d.ID, CustomerID: d.CustomerID, Hostname: d.Hostname, State: string(d.Primary), Flags: flags, Severity: d.Alarm.String(),
			Since: ptrTime(d.Since), LastSuccessAt: ptrTime(d.Detail.LastSuccess), LastSeenAt: ptrTime(d.Detail.LastSeen), Acknowledged: d.Ack != nil})
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// ---- metrics ----

// metricsWriter renders the Prometheus text exposition format by hand: counts
// and durations only, labels from fixed vocabularies (state, status, severity,
// job name). No hostname, customer name, device name or path is ever a label or
// a value, and none is available to this code.
type metricsWriter struct{ b bytes.Buffer }

func (m *metricsWriter) head(name, typ, help string) {
	fmt.Fprintf(&m.b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
}

func (m *metricsWriter) sample(name string, v float64, labels ...string) {
	m.b.WriteString(name)
	for i := 0; i+1 < len(labels); i += 2 {
		if i == 0 {
			m.b.WriteByte('{')
		} else {
			m.b.WriteByte(',')
		}
		fmt.Fprintf(&m.b, `%s="%s"`, labels[i], escapeLabel(labels[i+1]))
		if i+2 >= len(labels) {
			m.b.WriteByte('}')
		}
	}
	fmt.Fprintf(&m.b, " %s\n", formatSample(v))
}

func formatSample(v float64) string {
	if v == float64(int64(v)) {
		return fmt.Sprintf("%d", int64(v))
	}
	return fmt.Sprintf("%g", v)
}

var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

func escapeLabel(s string) string { return labelEscaper.Replace(s) }

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// MetricsContentType is the Prometheus text exposition media type.
const MetricsContentType = "text/plain; version=0.0.4; charset=utf-8"

// customerStatuses is the closed set of rollup statuses a metric is emitted for.
var customerStatuses = []state.CustomerStatus{state.CustomerMissing, state.CustomerUnknown, state.CustomerAlarm, state.CustomerWarn, state.CustomerOK,
	state.CustomerOnboarding, state.CustomerEmpty}

func (s *Service) metrics(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	f, err := s.Fleet.Fleet(r.Context(), p.Scope)
	if err != nil {
		// A scrape that fails is the monitor's own alarm; it must not read as zeros.
		s.fail(w, r, err)
		return
	}
	now := f.Computed
	var m metricsWriter

	states := map[state.State]int{}
	severities := map[string]int{}
	overdue, unseen := 0, 0
	for _, d := range f.View.Devices {
		states[d.Primary]++
		if d.Primary == state.StateRetired {
			continue
		}
		severities[d.Alarm.String()]++
		if len(overdueChips(d)) > 0 {
			overdue++
		}
		if hasState(d, state.StateUnseen) {
			unseen++
		}
	}
	m.head("cryobackup_devices", "gauge", "Devices by computed state (retired devices included under RETIRED).")
	for _, st := range state.Precedence() {
		m.sample("cryobackup_devices", float64(states[st]), "state", string(st))
	}
	m.head("cryobackup_devices_by_severity", "gauge", "Non-retired devices by their highest alarm severity.")
	for _, sev := range []state.Severity{state.SeverityCritical, state.SeverityAlarm, state.SeverityNone} {
		m.sample("cryobackup_devices_by_severity", float64(severities[sev.String()]), "severity", sev.String())
	}
	m.head("cryobackup_devices_overdue", "gauge", "Devices that are STALE, MISSED or BLANK.")
	m.sample("cryobackup_devices_overdue", float64(overdue))
	m.head("cryobackup_devices_unseen", "gauge", "Devices with no recent heartbeat.")
	m.sample("cryobackup_devices_unseen", float64(unseen))

	cust := map[state.CustomerStatus]int{}
	for _, c := range f.View.Customers {
		cust[c.Status]++
	}
	m.head("cryobackup_customers", "gauge", "Customers by rollup status.")
	for _, st := range customerStatuses {
		m.sample("cryobackup_customers", float64(cust[st]), "status", string(st))
	}

	banners := map[string]int{}
	for _, b := range f.View.Banners {
		banners[bannerSeverity(b.Severity)]++
	}
	m.head("cryobackup_banners", "gauge", "Console-level conditions raised now, by severity.")
	m.sample("cryobackup_banners", float64(banners["critical"]), "severity", "critical")
	m.sample("cryobackup_banners", float64(banners["warn"]), "severity", "warn")

	m.head("cryobackup_fleet_complete", "gauge", "1 when the facts load was complete; 0 means every number here is a lower bound.")
	m.sample("cryobackup_fleet_complete", b2f(f.View.Complete))
	m.head("cryobackup_fleet_all_clear", "gauge", "1 only when nothing at all is wrong and the data is complete.")
	m.sample("cryobackup_fleet_all_clear", b2f(f.View.AllClear()))
	m.head("cryobackup_fleet_computed_timestamp_seconds", "gauge", "When the fleet view was computed.")
	m.sample("cryobackup_fleet_computed_timestamp_seconds", float64(now.Unix()))
	m.head("cryobackup_fleet_loaded_timestamp_seconds", "gauge", "When the facts behind the fleet view were read.")
	m.sample("cryobackup_fleet_loaded_timestamp_seconds", float64(f.View.LoadedAt.Unix()))

	m.head("cryobackup_job_runs_total", "counter", "Runs of a console job since it first ran.")
	for _, j := range f.Jobs {
		m.sample("cryobackup_job_runs_total", float64(j.RunCount), "job", j.Name)
	}
	m.head("cryobackup_job_up", "gauge", "1 when the job's last run succeeded.")
	for _, j := range f.Jobs {
		m.sample("cryobackup_job_up", b2f(j.LastStatus == "ok"), "job", j.Name)
	}
	m.head("cryobackup_job_overdue", "gauge", "1 when the job's next run was due over a minute ago and it is not running.")
	for _, j := range f.Jobs {
		m.sample("cryobackup_job_overdue", b2f(jobOverdue(j, now)), "job", j.Name)
	}
	m.head("cryobackup_job_last_finished_timestamp_seconds", "gauge", "When the job last finished.")
	for _, j := range f.Jobs {
		if j.LastFinishedAt != nil {
			m.sample("cryobackup_job_last_finished_timestamp_seconds", float64(j.LastFinishedAt.Unix()), "job", j.Name)
		}
	}
	m.head("cryobackup_job_last_duration_seconds", "gauge", "How long the job's last run took.")
	for _, j := range f.Jobs {
		if j.LastDurationMS != nil {
			m.sample("cryobackup_job_last_duration_seconds", float64(*j.LastDurationMS)/1000, "job", j.Name)
		}
	}
	if v, err := s.Store.SchemaVersion(r.Context()); err == nil {
		m.head("cryobackup_schema_version", "gauge", "Version of the console database schema.")
		m.sample("cryobackup_schema_version", float64(v))
	}

	h := w.Header()
	h.Set("Content-Type", MetricsContentType)
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(m.b.Bytes())
}
