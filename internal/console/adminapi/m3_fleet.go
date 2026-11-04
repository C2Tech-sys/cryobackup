package adminapi

import (
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/health"
	"github.com/c2tech-sys/cryobackup/internal/console/httpx"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// ---- fleet/summary ----

// DeviceCountsJSON counts the non-retired devices of the answer. Healthy is the
// numerator of the within-SLA percentage (a fresh success and no alarm of any
// kind); the denominator is Active (not paused). WithinSLAPct is absent when
// Active is zero: n/a, never 100 (architecture §4.5).
type DeviceCountsJSON struct {
	Total        int      `json:"total"`
	Active       int      `json:"active"`
	Paused       int      `json:"paused"`
	Healthy      int      `json:"healthy"`
	Critical     int      `json:"critical"`
	Warnings     int      `json:"warnings"`
	BackedUp24h  int      `json:"backed_up_24h"`
	WithinSLAPct *float64 `json:"within_sla_pct,omitempty"`
}

// AgeRingJSON is the last-success age ring (components §4.4). Beyond is "over
// the stale limit, or never"; StaleAfterHours is the engine's default limit
// (the per-device limit is policy and does not travel with the verdict).
type AgeRingJSON struct {
	Under24h        int `json:"under_24h"`
	Within          int `json:"between_24h_and_limit"`
	Beyond          int `json:"over_limit_or_never"`
	Parked          int `json:"parked"`
	StaleAfterHours int `json:"stale_after_hours"`
}

// CustomerRingJSON counts customers by rollup status.
type CustomerRingJSON struct {
	Total      int            `json:"total"`
	ByStatus   map[string]int `json:"by_status"`
	Missing    int            `json:"missing"`
	Silent     int            `json:"silent"`
	WithinSLA  int            `json:"within_sla"`
	Warn       int            `json:"warn"`
	Critical   int            `json:"critical"`
	Quiet      int            `json:"onboarding_or_empty"`
	HealthyPct *float64       `json:"within_sla_pct,omitempty"`
}

// FreshnessJSON is the freshness stamp's "n of m agents": devices classified
// without an UNKNOWN verdict over non-retired devices.
type FreshnessJSON struct {
	InView          int        `json:"in_view"`
	Enrolled        int        `json:"enrolled"`
	LastHeartbeatAt *time.Time `json:"last_heartbeat_at,omitempty"`
}

// FleetSummaryResponse is GET /api/v1/fleet/summary.
type FleetSummaryResponse struct {
	StampJSON
	CustomerID string `json:"customer_id,omitempty"`
	// AllClear is the engine's answer for the whole scope: true only with no
	// banner, no alarming device or customer, complete data and a non-empty
	// fleet. It is never narrowed by the customer filter.
	AllClear     bool             `json:"all_clear"`
	FirstRun     bool             `json:"first_run"`
	Freshness    FreshnessJSON    `json:"freshness"`
	Devices      DeviceCountsJSON `json:"devices"`
	States       map[string]int   `json:"states"`
	AgeRing      AgeRingJSON      `json:"age_ring"`
	CustomerRing CustomerRingJSON `json:"customer_ring"`
	Overdue      int              `json:"overdue"`
	Unseen       int              `json:"unseen"`
	Banners      []BannerJSON     `json:"banners"`
}

func buildSummary(fd fleetData) FleetSummaryResponse {
	now := fd.Computed
	stale := state.DefaultThresholds().StaleAfter
	out := FleetSummaryResponse{StampJSON: fd.stamp(), CustomerID: fd.Customer, AllClear: fd.View.AllClear(), FirstRun: fd.View.FirstRun,
		States: map[string]int{}, Banners: bannersJSON(fd.View.Banners)}
	out.AgeRing.StaleAfterHours = int(stale / time.Hour)
	var lastHB time.Time
	for _, d := range fd.Devices {
		if d.Primary == state.StateRetired {
			continue
		}
		out.Devices.Total++
		out.Freshness.Enrolled++
		out.States[string(d.Primary)]++
		if d.Primary != state.StateUnknown {
			out.Freshness.InView++
		}
		healthy := d.Alarm == state.SeverityNone && !d.Detail.LastSuccess.IsZero()
		switch {
		case d.Primary == state.StatePaused:
			out.Devices.Paused++
			out.AgeRing.Parked++
		default:
			out.Devices.Active++
			if healthy {
				out.Devices.Healthy++
			}
			if d.Detail.LastSeen.After(lastHB) {
				lastHB = d.Detail.LastSeen
			}
			age := now.Sub(d.Detail.LastSuccess)
			switch {
			case d.Detail.LastSuccess.IsZero() || age > stale:
				out.AgeRing.Beyond++
			case age > 24*time.Hour:
				out.AgeRing.Within++
			default:
				out.AgeRing.Under24h++
			}
		}
		switch {
		case d.Alarm == state.SeverityCritical:
			out.Devices.Critical++
		case isWarning(d):
			out.Devices.Warnings++
		}
		if !d.Detail.LastSuccess.IsZero() && now.Sub(d.Detail.LastSuccess) <= 24*time.Hour {
			out.Devices.BackedUp24h++
		}
		if len(overdueChips(d)) > 0 {
			out.Overdue++
		}
		if hasState(d, state.StateUnseen) {
			out.Unseen++
		}
	}
	out.Freshness.LastHeartbeatAt = ptrTime(lastHB)
	if out.Devices.Active > 0 {
		v := 100 * float64(out.Devices.Healthy) / float64(out.Devices.Active)
		out.Devices.WithinSLAPct = &v
	}
	out.CustomerRing.ByStatus = map[string]int{}
	for _, c := range fd.Customers {
		out.CustomerRing.Total++
		out.CustomerRing.ByStatus[string(c.Status)]++
		if _, _, silent := health.Silence(c); silent {
			out.CustomerRing.Silent++
		}
		switch c.Status {
		case state.CustomerOK:
			out.CustomerRing.WithinSLA++
		case state.CustomerWarn:
			out.CustomerRing.Warn++
		case state.CustomerMissing:
			out.CustomerRing.Missing++
			out.CustomerRing.Critical++
		case state.CustomerAlarm, state.CustomerUnknown:
			out.CustomerRing.Critical++
		default:
			out.CustomerRing.Quiet++
		}
	}
	if n := out.CustomerRing.Total - out.CustomerRing.Quiet; n > 0 {
		v := 100 * float64(out.CustomerRing.WithinSLA) / float64(n)
		out.CustomerRing.HealthyPct = &v
	}
	return out
}

func (s *Service) fleetSummary(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	fd, ok := s.loadFleet(w, r, p)
	if !ok {
		return
	}
	httpx.WriteJSON(w, http.StatusOK, buildSummary(fd))
}

// ---- fleet/banners ----

// EntityBannerJSON is a banner about one customer: an expected customer with no
// devices (kind missing), or one whose devices went silent together (silence).
type EntityBannerJSON struct {
	Kind       string `json:"kind"`
	CustomerID string `json:"customer_id"`
	Customer   string `json:"customer"`
	Silent     int    `json:"silent,omitempty"`
	Total      int    `json:"total,omitempty"`
}

// FleetBannersResponse is GET /api/v1/fleet/banners. An empty Banners list is
// an answer only together with Complete true.
type FleetBannersResponse struct {
	StampJSON
	AllClear     bool               `json:"all_clear"`
	Banners      []BannerJSON       `json:"banners"`
	EntityBanner []EntityBannerJSON `json:"entity_banners"`
}

func (s *Service) fleetBanners(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	fd, ok := s.loadFleet(w, r, p)
	if !ok {
		return
	}
	out := FleetBannersResponse{StampJSON: fd.stamp(), AllClear: fd.View.AllClear(), Banners: bannersJSON(fd.View.Banners), EntityBanner: []EntityBannerJSON{}}
	for _, e := range health.EntityBanners(state.FleetView{Customers: fd.Customers}) {
		out.EntityBanner = append(out.EntityBanner, EntityBannerJSON{Kind: e.Kind, CustomerID: e.Customer.ID, Customer: e.Customer.Name, Silent: e.Silent, Total: e.Total})
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// ---- fleet/overdue ----

// OverdueRowJSON is a device on the overdue list with its reason chips.
type OverdueRowJSON struct {
	DeviceJSON
	Chips []string `json:"chips"`
	// LastSuccessAgeS is absent for a device that has never succeeded.
	LastSuccessAgeS *int64 `json:"last_success_age_s,omitempty"`
}

// FleetOverdueResponse is GET /api/v1/fleet/overdue, oldest success first
// (never-succeeded devices first of all).
type FleetOverdueResponse struct {
	StampJSON
	Total      int              `json:"total"`
	Truncated  bool             `json:"truncated"`
	NextCursor string           `json:"next_cursor,omitempty"`
	Devices    []OverdueRowJSON `json:"devices"`
}

func (s *Service) fleetOverdue(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	offset, limit, ok := pageParams(r.URL.Query(), 500)
	if !ok {
		httpx.Problem(w, r, api.CodeInvalidRequest, 0)
		return
	}
	fd, ok := s.loadFleet(w, r, p)
	if !ok {
		return
	}
	var rows []OverdueRowJSON
	for _, d := range fd.Devices {
		chips := overdueChips(d)
		if len(chips) == 0 {
			continue
		}
		row := OverdueRowJSON{DeviceJSON: deviceJSON(d, fd.customerName(d.CustomerID), store.Device{}), Chips: chips}
		if !d.Detail.LastSuccess.IsZero() {
			age := int64(fd.Computed.Sub(d.Detail.LastSuccess) / time.Second)
			row.LastSuccessAgeS = &age
		}
		rows = append(rows, row)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		switch {
		case (a.LastSuccessAgeS == nil) != (b.LastSuccessAgeS == nil):
			return a.LastSuccessAgeS == nil
		case a.LastSuccessAgeS != nil && *a.LastSuccessAgeS != *b.LastSuccessAgeS:
			return *a.LastSuccessAgeS > *b.LastSuccessAgeS
		}
		return a.Hostname < b.Hostname
	})
	out := FleetOverdueResponse{StampJSON: fd.stamp(), Total: len(rows)}
	out.Devices, out.NextCursor, out.Truncated = window(rows, offset, limit)
	httpx.WriteJSON(w, http.StatusOK, out)
}

// window cuts rows to [offset, offset+limit) and returns the next cursor.
func window[T any](rows []T, offset, limit int) (page []T, next string, truncated bool) {
	if offset > len(rows) {
		offset = len(rows)
	}
	end := offset + limit
	if end < len(rows) {
		next, truncated = "o"+strconv.Itoa(end), true
	} else {
		end = len(rows)
	}
	page = rows[offset:end]
	if page == nil {
		page = []T{}
	}
	return page, next, truncated
}

// ---- fleet/unseen ----

// UnseenRowJSON is one row of the devices-not-seen list: a device, or one row
// for a customer whose devices went silent together (architecture §4.3: at
// least 5 active devices, at least half unseen or deferred for the gateway).
type UnseenRowJSON struct {
	Kind       string      `json:"kind"` // device | customer_silence
	CustomerID string      `json:"customer_id"`
	Customer   string      `json:"customer"`
	Device     *DeviceJSON `json:"device,omitempty"`
	// Silence rows.
	Affected  int      `json:"affected,omitempty"`
	Total     int      `json:"total,omitempty"`
	Unseen    int      `json:"unseen,omitempty"`
	DeviceIDs []string `json:"device_ids,omitempty"`
	// Covered is true when the fleet-wide gateway banner (C01) already speaks for every affected device.
	Covered bool `json:"covered,omitempty"`
	// LastSeenAt is the device's last heartbeat; for a silence row the oldest one among the affected devices.
	LastSeenAt *time.Time `json:"last_seen_at,omitempty"`
}

// FleetUnseenResponse is GET /api/v1/fleet/unseen. Devices is the number of
// unseen devices, Rows what is listed after the collapse.
type FleetUnseenResponse struct {
	StampJSON
	Devices    int             `json:"devices"`
	Total      int             `json:"total"`
	Truncated  bool            `json:"truncated"`
	NextCursor string          `json:"next_cursor,omitempty"`
	Rows       []UnseenRowJSON `json:"rows"`
}

func (s *Service) fleetUnseen(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	offset, limit, ok := pageParams(r.URL.Query(), 500)
	if !ok {
		httpx.Problem(w, r, api.CodeInvalidRequest, 0)
		return
	}
	fd, ok := s.loadFleet(w, r, p)
	if !ok {
		return
	}
	byID := map[string]state.DeviceState{}
	for _, d := range fd.Devices {
		byID[d.ID] = d
	}
	var silence, devices []UnseenRowJSON
	collapsed := map[string]bool{}
	for _, c := range fd.Collapses {
		if c.Scope != state.CollapseCustomer {
			continue
		}
		row := UnseenRowJSON{Kind: "customer_silence", CustomerID: c.CustomerID, Customer: c.CustomerName, Affected: c.Affected, Total: c.Total,
			Unseen: c.Unseen, DeviceIDs: c.DeviceIDs, Covered: c.Covered}
		var oldest time.Time
		for _, id := range c.DeviceIDs {
			collapsed[id] = true
			if ls := byID[id].Detail.LastSeen; !ls.IsZero() && (oldest.IsZero() || ls.Before(oldest)) {
				oldest = ls
			}
		}
		row.LastSeenAt = ptrTime(oldest)
		silence = append(silence, row)
	}
	sort.SliceStable(silence, func(i, j int) bool { return silence[i].Affected > silence[j].Affected })
	unseen := 0
	for _, d := range fd.Devices {
		if !hasState(d, state.StateUnseen) {
			continue
		}
		unseen++
		if collapsed[d.ID] {
			continue
		}
		dj := deviceJSON(d, fd.customerName(d.CustomerID), store.Device{})
		devices = append(devices, UnseenRowJSON{Kind: "device", CustomerID: d.CustomerID, Customer: fd.customerName(d.CustomerID), Device: &dj, LastSeenAt: ptrTime(d.Detail.LastSeen)})
	}
	sort.SliceStable(devices, func(i, j int) bool {
		a, b := devices[i].LastSeenAt, devices[j].LastSeenAt
		switch {
		case (a == nil) != (b == nil):
			return a == nil
		case a != nil && !a.Equal(*b):
			return a.Before(*b)
		}
		return devices[i].Device.Hostname < devices[j].Device.Hostname
	})
	rows := append(silence, devices...)
	out := FleetUnseenResponse{StampJSON: fd.stamp(), Devices: unseen, Total: len(rows)}
	out.Rows, out.NextCursor, out.Truncated = window(rows, offset, limit)
	httpx.WriteJSON(w, http.StatusOK, out)
}

// ---- ten-day ----

// DayJSON is one cell of a ten-day strip: a calendar day in the customer's
// zone. Success and Failed are run counts of the day.
type DayJSON struct {
	Date    string `json:"date"`
	Kind    string `json:"kind"`
	Success int    `json:"success"`
	Failed  int    `json:"failed"`
}

func dayJSON(c state.StripCell) DayJSON {
	return DayJSON{Date: c.Date.Format("2006-01-02"), Kind: string(c.Kind), Success: c.Success, Failed: c.Failed}
}

func daysJSON(cells [state.StripDays]state.StripCell) []DayJSON {
	out := make([]DayJSON, 0, len(cells))
	for _, c := range cells {
		out = append(out, dayJSON(c))
	}
	return out
}

// DayRollupJSON is one day of a customer's strip: how many devices had each
// kind of day, and the worst of them. Worst is none only when no device had a
// day with data.
type DayRollupJSON struct {
	Date    string `json:"date"`
	Worst   string `json:"worst"`
	Success int    `json:"success"`
	Mixed   int    `json:"mixed"`
	Failed  int    `json:"failed"`
	Missed  int    `json:"missed"`
	None    int    `json:"none"`
}

var dayRank = map[state.DayKind]int{state.DayNone: 0, state.DaySuccess: 1, state.DayMixed: 2, state.DayMissed: 3, state.DayFailed: 4}

// rollupDays folds the strips of one customer's devices into one strip of
// device counts per kind. The strips of one customer share a zone, so the
// cells line up by index.
func rollupDays(strips [][state.StripDays]state.StripCell) []DayRollupJSON {
	if len(strips) == 0 {
		return []DayRollupJSON{}
	}
	out := make([]DayRollupJSON, state.StripDays)
	for i := range out {
		out[i].Date = strips[0][i].Date.Format("2006-01-02")
		worst := state.DayNone
		for _, st := range strips {
			k := st[i].Kind
			switch k {
			case state.DaySuccess:
				out[i].Success++
			case state.DayMixed:
				out[i].Mixed++
			case state.DayFailed:
				out[i].Failed++
			case state.DayMissed:
				out[i].Missed++
			default:
				out[i].None++
			}
			if dayRank[k] > dayRank[worst] {
				worst = k
			}
		}
		out[i].Worst = string(worst)
	}
	return out
}

// TenDayDeviceJSON is one device's strip.
type TenDayDeviceJSON struct {
	ID       string    `json:"id"`
	Hostname string    `json:"hostname"`
	State    string    `json:"state"`
	Days     []DayJSON `json:"days"`
}

// TenDayCustomerJSON is one customer's row of the activity grid.
type TenDayCustomerJSON struct {
	ID      string             `json:"id"`
	Name    string             `json:"name"`
	Status  string             `json:"status"`
	Zone    string             `json:"zone,omitempty"`
	Days    []DayRollupJSON    `json:"days"`
	Devices []TenDayDeviceJSON `json:"devices,omitempty"`
}

// FleetTenDayResponse is GET /api/v1/fleet/ten-day.
type FleetTenDayResponse struct {
	StampJSON
	Customers []TenDayCustomerJSON `json:"customers"`
}

// tenDay computes the strips of devices through the same code the board uses
// (health.TenDay over internal/state). customerScope bounds the store reads to
// the one customer when the answer is for one.
func (s *Service) tenDay(r *http.Request, p *auth.Principal, fd fleetData, devices []state.DeviceState) (map[string][state.StripDays]state.StripCell, error) {
	scope := p.Scope
	if fd.Customer != "" {
		scope = store.ScopeOf(fd.Customer) // already checked to be inside p.Scope
	}
	return health.TenDay(r.Context(), s.Store, scope, state.FleetView{Devices: devices}, fd.Computed, s.loc())
}

func (s *Service) fleetTenDay(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	withDevices, okb := boolParam(r.URL.Query().Get("devices"))
	if !okb {
		httpx.Problem(w, r, api.CodeInvalidRequest, 0)
		return
	}
	fd, ok := s.loadFleet(w, r, p)
	if !ok {
		return
	}
	strips, err := s.tenDay(r, p, fd, fd.Devices)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// Device rows are the detail view of one customer; the fleet-wide grid is customer rows unless asked.
	devs := fd.Customer != ""
	if withDevices != nil {
		devs = *withDevices
	}
	out := FleetTenDayResponse{StampJSON: fd.stamp(), Customers: make([]TenDayCustomerJSON, 0, len(fd.Customers))}
	for _, c := range fd.Customers {
		row := TenDayCustomerJSON{ID: c.ID, Name: c.Name, Status: string(c.Status)}
		var cs [][state.StripDays]state.StripCell
		for _, d := range c.Devices {
			cells, ok := strips[d.ID]
			if !ok {
				continue
			}
			cs = append(cs, cells)
			if row.Zone == "" {
				row.Zone = cells[0].Date.Location().String()
			}
			if devs {
				row.Devices = append(row.Devices, TenDayDeviceJSON{ID: d.ID, Hostname: d.Hostname, State: string(d.Primary), Days: daysJSON(cells)})
			}
		}
		row.Days = rollupDays(cs)
		out.Customers = append(out.Customers, row)
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// ---- customers/{id}/health ----

// SilenceJSON says a customer's devices went silent together.
type SilenceJSON struct {
	Silent int `json:"silent"`
	Total  int `json:"total"`
}

// CustomerHealthResponse is GET /api/v1/customers/{id}/health. Missing is the
// MISSING flag of architecture §4.5: an expected customer with no active device
// and no onboarding grace is critical, never an empty neutral row.
type CustomerHealthResponse struct {
	StampJSON
	CustomerID string `json:"customer_id"`
	Name       string `json:"name"`
	// Archived customers are not part of the evaluated view; no rollup is given.
	Archived bool         `json:"archived,omitempty"`
	Rollup   *RollupJSON  `json:"rollup,omitempty"`
	Missing  bool         `json:"missing"`
	Silence  *SilenceJSON `json:"silence,omitempty"`
	Retired  int          `json:"retired_devices"`
	// TenDay is the customer's strip folded over its devices, oldest day first.
	TenDay []DayRollupJSON `json:"ten_day"`
}

func (s *Service) customerHealth(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	id := r.PathValue("id")
	cust, err := s.Store.GetCustomer(r.Context(), p.Scope, id)
	if err != nil {
		s.failM2(w, r, err)
		return
	}
	f, err := s.Fleet.Fleet(r.Context(), p.Scope)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	fd := fleetData{Fleet: f, Customer: id, names: map[string]string{}}
	var cs *state.CustomerState
	for i := range f.View.Customers {
		if f.View.Customers[i].ID == id {
			cs = &f.View.Customers[i]
		}
	}
	out := CustomerHealthResponse{StampJSON: fd.stamp(), CustomerID: id, Name: cust.Name, TenDay: []DayRollupJSON{}}
	if cs == nil {
		out.Archived = true
		httpx.WriteJSON(w, http.StatusOK, out)
		return
	}
	out.Name, out.Rollup, out.Missing, out.Retired = cs.Name, rollupJSON(*cs), cs.Status == state.CustomerMissing, cs.Retired
	if silent, total, is := health.Silence(*cs); is {
		out.Silence = &SilenceJSON{Silent: silent, Total: total}
	}
	strips, err := s.tenDay(r, p, fd, cs.Devices)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var cells [][state.StripDays]state.StripCell
	for _, d := range cs.Devices {
		if c, ok := strips[d.ID]; ok {
			cells = append(cells, c)
		}
	}
	out.TenDay = rollupDays(cells)
	httpx.WriteJSON(w, http.StatusOK, out)
}

// ---- alarms ----

// AlarmJSON is one thing that is alarming now. Kind is device (a device with an
// alarming state), customer (an expected customer with no devices) or silence
// (a customer whose devices went silent together; its affected devices are not
// listed again).
type AlarmJSON struct {
	Kind         string     `json:"kind"`
	DeviceID     string     `json:"device_id,omitempty"`
	Hostname     string     `json:"hostname,omitempty"`
	State        string     `json:"state,omitempty"`
	Flags        []string   `json:"flags,omitempty"`
	Categories   []string   `json:"categories"`
	Severity     string     `json:"severity"`
	Reason       string     `json:"reason,omitempty"`
	Since        *time.Time `json:"since,omitempty"`
	Acknowledged bool       `json:"acknowledged,omitempty"`
	Affected     int        `json:"affected,omitempty"`
	Total        int        `json:"total,omitempty"`
}

// AlarmGroupJSON is a customer's alarms.
type AlarmGroupJSON struct {
	CustomerID string      `json:"customer_id"`
	Customer   string      `json:"customer"`
	Status     string      `json:"status"`
	Severity   string      `json:"severity"`
	Alarms     []AlarmJSON `json:"alarms"`
}

// AlarmsResponse is GET /api/v1/alarms. Banners are the console-level alarms
// (they belong to no customer). Nothing alarming is only an answer together with
// Complete true and no banner.
type AlarmsResponse struct {
	StampJSON
	Total    int              `json:"total"`
	Critical int              `json:"critical"`
	Groups   []AlarmGroupJSON `json:"groups"`
	Banners  []BannerJSON     `json:"banners"`
}

// alarmCategory maps an alarming state to the category the alarms page groups by.
func alarmCategory(st state.State) string {
	switch st {
	case state.StateStale, state.StateMissed, state.StateBlank:
		return "overdue"
	case state.StateUnseen:
		return "unseen"
	case state.StateFailed, state.StatePartial, state.StateStuck:
		return "failed"
	case state.StateEscrowUnverified:
		return "escrow"
	case state.StateMaintFailed:
		return "maintenance"
	case state.StateUnknown:
		return "unknown"
	case state.StateIdentityConflict:
		return "identity"
	}
	return "other"
}

func (s *Service) alarms(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	min := state.SeverityAlarm
	switch v := r.URL.Query().Get("severity"); v {
	case "", "alarm":
	case "critical":
		min = state.SeverityCritical
	default:
		httpx.Problem(w, r, api.CodeInvalidRequest, 0)
		return
	}
	fd, ok := s.loadFleet(w, r, p)
	if !ok {
		return
	}
	collapsed := map[string]bool{}
	silence := map[string]state.Collapse{}
	for _, c := range fd.Collapses {
		if c.Scope != state.CollapseCustomer {
			continue
		}
		silence[c.CustomerID] = c
		for _, id := range c.DeviceIDs {
			collapsed[id] = true
		}
	}
	out := AlarmsResponse{StampJSON: fd.stamp(), Groups: []AlarmGroupJSON{}, Banners: bannersJSON(fd.View.Banners)}
	for _, b := range fd.View.Banners {
		if b.Severity == state.SeverityCritical {
			out.Critical++
		}
	}
	for _, c := range fd.Customers {
		g := AlarmGroupJSON{CustomerID: c.ID, Customer: c.Name, Status: string(c.Status), Severity: c.Alarm.String(), Alarms: []AlarmJSON{}}
		if c.Status == state.CustomerMissing && state.SeverityCritical >= min {
			g.Alarms = append(g.Alarms, AlarmJSON{Kind: "customer", Categories: []string{"missing"}, Severity: state.SeverityCritical.String()})
		}
		if col, ok := silence[c.ID]; ok && state.SeverityCritical >= min {
			g.Alarms = append(g.Alarms, AlarmJSON{Kind: "silence", Categories: []string{"unseen"}, Severity: state.SeverityCritical.String(), Affected: col.Affected, Total: col.Total})
		}
		for _, d := range c.Devices {
			if d.Alarm < min || d.Primary == state.StateRetired || collapsed[d.ID] {
				continue
			}
			a := AlarmJSON{Kind: "device", DeviceID: d.ID, Hostname: d.Hostname, State: string(d.Primary), Flags: strs(d.AlarmFlags), Severity: d.Alarm.String(),
				Reason: string(d.Reason), Since: ptrTime(d.Since), Acknowledged: d.Ack != nil}
			seen := map[string]bool{}
			a.Categories = []string{}
			for _, f := range d.AlarmFlags {
				if cat := alarmCategory(f); !seen[cat] {
					seen[cat] = true
					a.Categories = append(a.Categories, cat)
				}
			}
			if len(a.Categories) == 0 {
				a.Categories = []string{alarmCategory(d.Primary)}
			}
			g.Alarms = append(g.Alarms, a)
		}
		if len(g.Alarms) > 0 {
			out.Total += len(g.Alarms)
			out.Groups = append(out.Groups, g)
		}
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}
