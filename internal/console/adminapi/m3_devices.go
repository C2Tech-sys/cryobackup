package adminapi

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/httpx"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// deviceInView returns the engine's verdict on one device. A device the store
// knows but the evaluated view does not carry is UNKNOWN and critical, never a
// missing row that reads as fine (fail loudly).
func deviceInView(fd fleetData, dev store.Device) state.DeviceState {
	for _, d := range fd.View.Devices {
		if d.ID == dev.ID {
			return d
		}
	}
	return state.DeviceState{ID: dev.ID, CustomerID: dev.CustomerID, Hostname: dev.Hostname, Primary: state.StateUnknown, Alarm: state.SeverityCritical,
		Reason: state.ReasonFactsUnavailable}
}

// ---- devices/{id}/state ----

// ExplainJSON is one line of the explain tooltip: a state the device is in and
// the plain-words reason, from the facts the engine judged. Alarming is true
// when the state raises an alarm.
type ExplainJSON struct {
	State    string `json:"state"`
	Alarming bool   `json:"alarming"`
	Text     string `json:"text"`
}

// StateInputsJSON are the facts behind the verdict. Nothing here is a path:
// error classes and counts, versions, times.
type StateInputsJSON struct {
	LastSuccessAt       *time.Time     `json:"last_success_at,omitempty"`
	LastSeenAt          *time.Time     `json:"last_seen_at,omitempty"`
	LastOutcome         string         `json:"last_outcome,omitempty"`
	OpenRunAgeS         *int64         `json:"open_run_age_s,omitempty"`
	OpenRunLimitS       *int64         `json:"open_run_limit_s,omitempty"`
	OpenRunInitial      bool           `json:"open_run_initial,omitempty"`
	FailedFiles         int64          `json:"failed_files,omitempty"`
	TotalFiles          int64          `json:"total_files,omitempty"`
	ErrorClass          string         `json:"error_class,omitempty"`
	ErrorCounts         map[string]int `json:"error_counts,omitempty"`
	ConsecutiveFailures int            `json:"consecutive_failures,omitempty"`
	AgentVersion        string         `json:"agent_version,omitempty"`
	ExpectedVersion     string         `json:"expected_version,omitempty"`
	DeferredReason      string         `json:"deferred_reason,omitempty"`
	GraceDeadline       *time.Time     `json:"grace_deadline,omitempty"`
	PausedUntil         *time.Time     `json:"paused_until,omitempty"`
	PauseReason         string         `json:"pause_reason,omitempty"`
	MaintOp             string         `json:"maintenance_op,omitempty"`
	EscrowIssue         string         `json:"escrow_issue,omitempty"`
	VSSVolumes          []string       `json:"vss_fallback_volumes,omitempty"`
	ClockOffsetMS       int64          `json:"clock_offset_ms,omitempty"`
	LoadErrorClass      string         `json:"load_error_class,omitempty"`
	GarbledFields       []string       `json:"garbled_fields,omitempty"`
}

// DeviceStateResponse is GET /api/v1/devices/{id}/state: the computed state with
// its reasons and inputs, for the explain tooltip.
type DeviceStateResponse struct {
	StampJSON
	DeviceID     string          `json:"device_id"`
	CustomerID   string          `json:"customer_id"`
	Customer     string          `json:"customer"`
	Hostname     string          `json:"hostname"`
	State        string          `json:"state"`
	Flags        []string        `json:"flags"`
	Severity     string          `json:"severity"`
	Reason       string          `json:"reason,omitempty"`
	Since        *time.Time      `json:"since,omitempty"`
	Attributes   []string        `json:"attributes"`
	Acknowledged bool            `json:"acknowledged"`
	Muted        bool            `json:"muted,omitempty"`
	SnoozedUntil *time.Time      `json:"snoozed_until,omitempty"`
	Explain      []ExplainJSON   `json:"explain"`
	Inputs       StateInputsJSON `json:"inputs"`
}

func secs(d time.Duration) *int64 {
	if d <= 0 {
		return nil
	}
	v := int64(d / time.Second)
	return &v
}

func inputsJSON(d state.Detail) StateInputsJSON {
	in := StateInputsJSON{LastSuccessAt: ptrTime(d.LastSuccess), LastSeenAt: ptrTime(d.LastSeen), LastOutcome: d.LastOutcome, OpenRunAgeS: secs(d.OpenRunAge),
		OpenRunLimitS: secs(d.OpenRunLimit), OpenRunInitial: d.OpenRunInitial, FailedFiles: d.FailedFiles, TotalFiles: d.TotalFiles, ErrorClass: d.ErrorClass,
		ConsecutiveFailures: d.ConsecutiveFailures, AgentVersion: d.AgentVersion, ExpectedVersion: d.ExpectedVersion, DeferredReason: d.DeferredReason,
		GraceDeadline: ptrTime(d.GraceDeadline), PausedUntil: ptrTime(d.PauseUntil), PauseReason: d.PauseReason, MaintOp: d.MaintOp, EscrowIssue: string(d.EscrowIssue),
		VSSVolumes: d.VSSVolumes, ClockOffsetMS: d.ClockOffset.Milliseconds(), LoadErrorClass: d.LoadErrorClass, GarbledFields: d.GarbledFields}
	if len(d.ErrorCounts) > 0 {
		in.ErrorCounts = map[string]int{}
		for _, c := range d.ErrorCounts {
			in.ErrorCounts[c.Class] = c.N
		}
	}
	return in
}

// ageWords renders a duration as the largest whole unit: "3 d", "5 h", "12 min".
func ageWords(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%d d", int(d/(24*time.Hour)))
	case d >= time.Hour:
		return fmt.Sprintf("%d h", int(d/time.Hour))
	case d >= time.Minute:
		return fmt.Sprintf("%d min", int(d/time.Minute))
	}
	return "under a minute"
}

// explainState says in words why d is in st. It reads only the Detail the
// engine produced; it decides nothing.
func explainState(d state.DeviceState, st state.State, now time.Time) string {
	x := d.Detail
	since := func(t time.Time) string {
		if t.IsZero() {
			return "never"
		}
		return ageWords(now.Sub(t)) + " ago"
	}
	switch st {
	case state.StateRetired:
		return "Retired. Excluded from rings and rollups."
	case state.StatePaused:
		s := "Paused by an operator"
		if !x.PauseUntil.IsZero() {
			s += " until " + x.PauseUntil.UTC().Format(time.RFC3339)
		} else {
			s += " with no end"
		}
		if x.PauseReason != "" {
			s += ": " + x.PauseReason
		}
		return s + "."
	case state.StateUnknown:
		s := "The facts for this device could not be loaded completely, so no verdict is possible"
		if x.LoadErrorClass != "" {
			s += " (" + x.LoadErrorClass + ")"
		}
		return s + "."
	case state.StateIdentityConflict:
		return "A second machine presented this device's identity. Resolve the conflict before trusting its backups."
	case state.StatePendingFirst:
		s := "No successful backup yet"
		if !x.GraceDeadline.IsZero() {
			s += "; the first one is due by " + x.GraceDeadline.UTC().Format(time.RFC3339)
		}
		return s + "."
	case state.StateBlank:
		return "No successful backup since enrollment, and the first-backup grace is over (" + string(d.Reason) + ")."
	case state.StateStuck:
		return fmt.Sprintf("A backup has been running for %s, over its limit of %s.", ageWords(x.OpenRunAge), ageWords(x.OpenRunLimit))
	case state.StateFailed:
		s := "The newest backup failed"
		if x.ErrorClass != "" {
			s += " (" + x.ErrorClass + ")"
		}
		s += " and nothing has succeeded since"
		if x.ConsecutiveFailures > 1 {
			s += fmt.Sprintf("; %d failures in a row", x.ConsecutiveFailures)
		}
		return s + "."
	case state.StatePartial:
		return fmt.Sprintf("The newest backup was partial and over tolerance: %d of %d files failed.", x.FailedFiles, x.TotalFiles)
	case state.StateUnseen:
		return "No heartbeat since " + since(x.LastSeen) + "."
	case state.StateStale:
		return "The last successful backup was " + since(x.LastSuccess) + ", older than the limit."
	case state.StateEscrowUnverified:
		return "The escrowed repository key has not been proven recoverable (" + string(x.EscrowIssue) + ")."
	case state.StateMaintFailed:
		return "Retention maintenance failed or is starved (" + x.MaintOp + ")."
	case state.StateMissed:
		return "A scheduled backup did not happen; last success " + since(x.LastSuccess) + "."
	case state.StateDeferred:
		return "The agent is deliberately not running backups (" + string(d.Reason) + ")."
	case state.StateOutdated:
		return fmt.Sprintf("The agent runs %s; %s is expected.", x.AgentVersion, x.ExpectedVersion)
	case state.StateOK:
		return "Nothing is wrong."
	}
	return string(st)
}

func (s *Service) deviceState(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	dev, err := s.Store.GetDevice(r.Context(), p.Scope, r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	fd, ok := s.loadFleet(w, r, p)
	if !ok {
		return
	}
	d := deviceInView(fd, dev)
	out := DeviceStateResponse{StampJSON: fd.stamp(), DeviceID: d.ID, CustomerID: d.CustomerID, Customer: fd.customerName(d.CustomerID), Hostname: d.Hostname,
		State: string(d.Primary), Flags: strs(d.Flags), Severity: d.Alarm.String(), Reason: string(d.Reason), Since: ptrTime(d.Since), Attributes: strs(d.Attrs),
		Acknowledged: d.Ack != nil, Explain: []ExplainJSON{}, Inputs: inputsJSON(d.Detail)}
	if out.Flags == nil {
		out.Flags = []string{}
	}
	if d.Ack != nil {
		out.Muted, out.SnoozedUntil = d.Ack.Muted, ptrTime(d.Ack.SnoozedUntil)
	}
	flags := d.Flags
	if len(flags) == 0 {
		flags = []state.State{d.Primary}
	}
	for _, f := range flags {
		out.Explain = append(out.Explain, ExplainJSON{State: string(f), Alarming: slices.Contains(d.AlarmFlags, f), Text: explainState(d, f, fd.Computed)})
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// ---- devices/{id}/heartbeats ----

// HeartbeatJSON is one heartbeat of the ring. It carries presence and health
// counters only.
type HeartbeatJSON struct {
	ReceivedAt     time.Time `json:"received_at"`
	AgentVersion   string    `json:"agent_version,omitempty"`
	UptimeS        *int64    `json:"uptime_s,omitempty"`
	OnAC           *bool     `json:"on_ac,omitempty"`
	BatteryPct     *int      `json:"battery_pct,omitempty"`
	Metered        *bool     `json:"metered,omitempty"`
	VPN            *bool     `json:"vpn,omitempty"`
	QueueDepth     *int      `json:"queue_depth,omitempty"`
	JournalDepth   *int      `json:"journal_depth,omitempty"`
	ClockOffsetMS  *int64    `json:"clock_offset_ms,omitempty"`
	ServiceState   string    `json:"service_state"`
	LastErrorClass string    `json:"last_error_class,omitempty"`
}

// DeviceHeartbeatsResponse is GET /api/v1/devices/{id}/heartbeats, newest first.
// Truncated says older heartbeats inside the range were left out.
type DeviceHeartbeatsResponse struct {
	ComputedAt time.Time       `json:"computed_at"`
	DeviceID   string          `json:"device_id"`
	Since      time.Time       `json:"since"`
	Count      int             `json:"count"`
	Truncated  bool            `json:"truncated"`
	Heartbeats []HeartbeatJSON `json:"heartbeats"`
}

var heartbeatRanges = map[string]time.Duration{"1h": time.Hour, "6h": 6 * time.Hour, "24h": 24 * time.Hour}

func (s *Service) deviceHeartbeats(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	q := r.URL.Query()
	rng := time.Duration(0)
	switch v := q.Get("range"); {
	case v == "":
		rng = 24 * time.Hour
	default:
		d, ok := heartbeatRanges[v]
		if !ok {
			httpx.Problem(w, r, api.CodeInvalidRequest, 0)
			return
		}
		rng = d
	}
	limit := store.MaxHeartbeatRows
	if v := q.Get("limit"); v != "" {
		n, ok := atoiRange(v, 1, store.MaxHeartbeatRows)
		if !ok {
			httpx.Problem(w, r, api.CodeInvalidRequest, 0)
			return
		}
		limit = n
	}
	now := s.now()
	since := now.Add(-rng)
	rows, truncated, err := s.Store.ListHeartbeats(r.Context(), p.Scope, r.PathValue("id"), since, limit)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := DeviceHeartbeatsResponse{ComputedAt: now, DeviceID: r.PathValue("id"), Since: since, Count: len(rows), Truncated: truncated, Heartbeats: make([]HeartbeatJSON, 0, len(rows))}
	for _, h := range rows {
		out.Heartbeats = append(out.Heartbeats, HeartbeatJSON{ReceivedAt: h.ReceivedAt, AgentVersion: h.AgentVersion, UptimeS: h.UptimeS, OnAC: h.OnAC, BatteryPct: h.BatteryPct,
			Metered: h.Metered, VPN: h.VPN, QueueDepth: h.QueueDepth, JournalDepth: h.JournalDepth, ClockOffsetMS: h.ClockOffsetMS, ServiceState: h.ServiceState,
			LastErrorClass: h.LastErrorClass})
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// ---- devices/{id}/ten-day ----

// DeviceTenDayResponse is GET /api/v1/devices/{id}/ten-day: ten cells, oldest
// first, in the customer's zone. A retired device has no strip (Days is empty).
type DeviceTenDayResponse struct {
	StampJSON
	DeviceID string    `json:"device_id"`
	State    string    `json:"state"`
	Zone     string    `json:"zone,omitempty"`
	Days     []DayJSON `json:"days"`
}

func (s *Service) deviceTenDay(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	dev, err := s.Store.GetDevice(r.Context(), p.Scope, r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	f, err := s.Fleet.Fleet(r.Context(), p.Scope)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	fd := fleetData{Fleet: f, Customer: dev.CustomerID}
	d := deviceInView(fd, dev)
	out := DeviceTenDayResponse{StampJSON: fd.stamp(), DeviceID: dev.ID, State: string(d.Primary), Days: []DayJSON{}}
	if d.Primary != state.StateRetired {
		strips, err := s.tenDay(r, p, fd, []state.DeviceState{d})
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if cells, ok := strips[d.ID]; ok {
			out.Days, out.Zone = daysJSON(cells), cells[0].Date.Location().String()
		}
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// ---- pause and resume ----

// DevicePauseRequest is the body of POST /api/v1/devices/{id}/pause: a reason
// and exactly one of duration_hours (1, 2, 4, 8 or 24), until (a time at least a
// minute away and at most a year) or indefinite.
type DevicePauseRequest struct {
	Reason        string     `json:"reason"`
	DurationHours *int       `json:"duration_hours"`
	Until         *time.Time `json:"until"`
	Indefinite    bool       `json:"indefinite"`
}

// DevicePauseResponse is the answer of pause and resume. Changed is false when
// the request matched the current state (nothing was written).
type DevicePauseResponse struct {
	ComputedAt time.Time  `json:"computed_at"`
	DeviceID   string     `json:"device_id"`
	Paused     bool       `json:"paused"`
	PausedAt   *time.Time `json:"paused_at,omitempty"`
	// PausedUntil is absent for an indefinite pause (Indefinite true).
	PausedUntil    *time.Time `json:"paused_until,omitempty"`
	Indefinite     bool       `json:"indefinite"`
	Reason         string     `json:"reason,omitempty"`
	Changed        bool       `json:"changed"`
	CancelledRunID string     `json:"cancelled_run_id,omitempty"`
	QueuedCommands int        `json:"queued_commands"`
}

func (s *Service) pauseResponse(res store.PauseResult) DevicePauseResponse {
	now := s.now()
	d := res.Device
	paused := d.PausedAt != nil && (d.PausedUntil == nil || d.PausedUntil.After(now))
	out := DevicePauseResponse{ComputedAt: now, DeviceID: d.ID, Paused: paused, Changed: res.Changed, CancelledRunID: res.CancelledRunID, QueuedCommands: res.QueuedCommands}
	if paused {
		out.PausedAt, out.PausedUntil, out.Indefinite, out.Reason = ptrTime(*d.PausedAt), d.PausedUntil, d.PausedUntil == nil, d.PauseReason
		if out.PausedUntil != nil {
			out.PausedUntil = ptrTime(*d.PausedUntil)
		}
	}
	return out
}

// deviceChanged tells the listening parts of the console a device changed: the
// agent's open command poll, and (through fleetChanged, the one helper for it)
// the change generation and the hub. The route's fleetSignal wrapper signals
// again when the answer is written; the second signal is harmless.
func (s *Service) deviceChanged(id string) {
	if s.WakeAgent != nil {
		s.WakeAgent(id)
	}
	s.fleetChanged(id)
}

func (s *Service) devicePause(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	var req DevicePauseRequest
	if !decodeBodyMax(w, r, 4<<10, &req) {
		return
	}
	forms := 0
	for _, set := range []bool{req.DurationHours != nil, req.Until != nil, req.Indefinite} {
		if set {
			forms++
		}
	}
	if forms != 1 || (req.DurationHours != nil && !slices.Contains(store.PauseHours, *req.DurationHours)) {
		httpx.Problem(w, r, api.CodeInvalidRequest, 0)
		return
	}
	spec := store.PauseRequest{Reason: strings.TrimSpace(req.Reason)}
	switch {
	case req.DurationHours != nil:
		u := s.now().Add(time.Duration(*req.DurationHours) * time.Hour)
		spec.Until = &u
	case req.Until != nil:
		u := req.Until.UTC()
		spec.Until = &u
	}
	id := r.PathValue("id")
	res, err := s.Store.PauseDeviceAudited(r.Context(), p.Scope, id, spec, actorOf(r, p))
	if err != nil {
		s.failM2(w, r, err)
		return
	}
	if res.Changed {
		s.deviceChanged(id)
	}
	httpx.WriteJSON(w, http.StatusOK, s.pauseResponse(res))
}

func (s *Service) deviceResume(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	id := r.PathValue("id")
	res, err := s.Store.ResumeDeviceAudited(r.Context(), p.Scope, id, actorOf(r, p))
	if err != nil {
		s.failM2(w, r, err)
		return
	}
	if res.Changed {
		s.deviceChanged(id)
	}
	httpx.WriteJSON(w, http.StatusOK, s.pauseResponse(res))
}
