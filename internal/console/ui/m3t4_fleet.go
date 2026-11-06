package ui

import (
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminapi"
	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// Overdue (B04), devices not seen (B05) and the ten-day activity grid (S23),
// M3-T4. The engine decides which devices are overdue or unseen and which
// customers have gone silent; the admin API (fleet/overdue, fleet/unseen,
// fleet/ten-day) turns that into rows, and these pages only lay the rows out.
//
// An empty list here is an answer only with evidence beside it: how many
// devices were checked and when. A partial load, a fleet with no devices and a
// failed read are alarms, never a calm "nothing to see".

// chipForState is the state chip of a state name the API sent. A name the
// console does not know is UNKNOWN, as everywhere else.
func chipForState(id, name string) Chip {
	spec, ok := catalogue[state.State(name)]
	if !ok {
		return Chip{ID: id, Label: "UNKNOWN", Class: ClassCrit, Dashed: true, Small: true}
	}
	return Chip{ID: id, State: state.State(name), Label: spec.Label, Class: spec.Class, Small: true}
}

// customerStatusChip is the chip of a customer rollup status name.
func customerStatusChip(id, status string) Chip {
	switch status {
	case string(state.CustomerMissing):
		return Chip{ID: id, Label: "MISSING", Class: ClassCrit, Title: "MISSING", Reason: "Expected customer with no devices."}
	case string(state.CustomerOnboarding):
		return Chip{ID: id, Label: "ONBOARDING", Class: ClassInfo}
	case string(state.CustomerEmpty):
		return Chip{ID: id, Label: "NO DEVICES", Class: ClassNeutral}
	case string(state.CustomerUnknown):
		return Chip{ID: id, Label: "UNKNOWN", Class: ClassCrit, Dashed: true}
	case string(state.CustomerAlarm):
		return Chip{ID: id, Label: "ALARM", Class: ClassCrit}
	case string(state.CustomerWarn):
		return Chip{ID: id, Label: "WARNING", Class: ClassWarn}
	case string(state.CustomerOK):
		return Chip{ID: id, Label: "OK", Class: ClassOK}
	}
	return Chip{ID: id, Label: "UNKNOWN", Class: ClassCrit, Dashed: true}
}

// listEvidence is what an empty overdue or unseen list is judged against: the
// fleet summary of the same evaluation.
type listEvidence struct {
	Sum adminapi.FleetSummaryResponse
	Err error
}

func (s *Service) evidence(r *http.Request, customer string) listEvidence {
	v := url.Values{}
	if customer != "" {
		v.Set("customer", customer)
	}
	var e listEvidence
	e.Err = s.apiGet(r, "/api/v1/fleet/summary?"+v.Encode(), &e.Sum)
	return e
}

// clearOrAlarm classifies an empty list of "what" (overdue, unseen). The calm
// answer needs evidence; everything else is an alarm or a warning.
func (s *Service) clearOrAlarm(what, nothing string, ev listEvidence, complete bool, customer string, c fmtCtx) *EmptyView {
	switch {
	case ev.Err != nil:
		return &EmptyView{Kind: "alarm", Title: "Could not confirm that nothing is " + what,
			Body: "The list is empty, but the console could not read the fleet summary to say how many devices were checked. An empty list is not a clean bill: reload the page to try again."}
	case ev.Sum.Devices.Total == 0 && ev.Sum.FirstRun && customer == "":
		return &EmptyView{Kind: "neutral", Title: "No devices yet", Body: "Enroll a device and it is checked from its first heartbeat."}
	case ev.Sum.Devices.Total == 0:
		return &EmptyView{Kind: "alarm", Title: "No devices to check",
			Body: "There are no enrolled devices in this view, so nothing can be reported as " + what + ". Nothing is being backed up here: that is the alarm, not the empty list."}
	case !complete || !ev.Sum.Complete || ev.Sum.Freshness.InView < ev.Sum.Freshness.Enrolled:
		return &EmptyView{Kind: "partial", Title: "Not every device could be checked",
			Body: fmt.Sprintf("%d of %d devices were classified. The empty list covers only those; the rest show as UNKNOWN on the fleet board and count as unhealthy.", ev.Sum.Freshness.InView, ev.Sum.Freshness.Enrolled)}
	}
	return &EmptyView{Kind: "neutral", Title: nothing,
		Body: fmt.Sprintf("%s checked at %s (%s).", plural(ev.Sum.Devices.Total, "device", "devices"), c.clock(ev.Sum.ComputedAt), c.Zone)}
}

// fleetListQuery is the query of B04 and B05: an optional customer and the pager.
type fleetListQuery struct {
	Customer string
	Show     string // B05: the customer whose silence row is expanded into its devices
	Page     int
	Size     int
}

func parseFleetListQuery(v url.Values) (fleetListQuery, []store.FieldError) {
	q := fleetListQuery{Customer: strings.TrimSpace(v.Get("customer")), Page: atoiDefault(v.Get("page"), 1), Size: store.NormalPageSize(atoiDefault(v.Get("size"), 0))}
	var bad []store.FieldError
	if q.Customer != "" && !idParam(q.Customer) {
		bad = append(bad, store.FieldError{Field: "customer", Message: "Choose one of the listed customers."})
		q.Customer = ""
	}
	if q.Show = strings.TrimSpace(v.Get("show")); q.Show != "" && !idParam(q.Show) {
		q.Show = ""
	}
	return q, bad
}

func (q fleetListQuery) values() url.Values {
	out := url.Values{}
	if q.Customer != "" {
		out.Set("customer", q.Customer)
	}
	if q.Show != "" {
		out.Set("show", q.Show)
	}
	if q.Page > 1 {
		out.Set("page", strconv.Itoa(q.Page))
	}
	if q.Size != store.DefaultPageSize {
		out.Set("size", strconv.Itoa(q.Size))
	}
	return out
}

func (q fleetListQuery) apiValues(page int) url.Values {
	out := url.Values{}
	if q.Customer != "" {
		out.Set("customer", q.Customer)
	}
	out.Set("limit", strconv.Itoa(q.Size))
	if page > 1 {
		out.Set("cursor", "o"+strconv.Itoa((page-1)*q.Size))
	}
	return out
}

// FleetFilter is the customer filter bar of B04 and B05.
type FleetFilter struct {
	Action    string
	Customers []Option
	Customer  string
	ListErr   bool
	Size      int
}

// ---- B04 ----

// OverdueRow is one overdue device.
type OverdueRow struct {
	ID      string
	Host    string
	Meta    string
	Reasons []Chip
	State   Chip
	Age     string
	AgeSub  string
	AgeCls  string
	Glyph   string
	Ack     bool
}

// OverdueGroup is one customer's overdue devices on the page.
type OverdueGroup struct {
	ID    string
	Name  string
	Count string
	Rows  []OverdueRow
}

// OverduePage is B04.
type OverduePage struct {
	Page
	Filter  FleetFilter
	Groups  []OverdueGroup
	Total   int
	Summary string
	Pager   PagerView
	Empty   *EmptyView
	Warn    *EmptyView
	AsOf    string
	Zone    string
	Live    string // this page's own URL, polled to refresh the live region
}

func overdueReasonChips(id string, r adminapi.OverdueRowJSON, c fmtCtx) []Chip {
	var out []Chip
	for _, name := range r.Chips {
		ch := chipForState("od-"+id+"-"+name, name)
		ch.Small = false
		switch state.State(name) {
		case state.StateStale:
			if r.LastSuccessAgeS != nil {
				ch.Qual, ch.Joiner = ageShort(time.Duration(*r.LastSuccessAgeS)*time.Second), " "
				ch.Title, ch.Reason = "STALE", "No successful backup for "+ageShort(time.Duration(*r.LastSuccessAgeS)*time.Second)+"."
			}
		case state.StateMissed:
			ch.Title, ch.Reason = "MISSED RUN", "A scheduled run did not start. This becomes STALE if it continues."
		case state.StateBlank:
			ch.Title, ch.Reason = "NEVER BACKED UP", "No successful backup since enrollment, and the first-backup grace period is over."
		}
		out = append(out, ch)
	}
	return out
}

func (s *Service) overduePage(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	fleet, ok := s.fleetFor(w, r, p, "Overdue")
	if !ok {
		return
	}
	q, bad := parseFleetListQuery(r.URL.Query())
	if len(bad) > 0 {
		s.badQuery(w, r, p, fleet, bad)
		return
	}
	c := s.fcAt(r.Context(), fleet.Computed)
	pg := OverduePage{Page: s.page(r.Context(), p, fleet, "Overdue", "overdue", "/ui/events"), AsOf: c.clock(fleet.Computed), Zone: c.Zone, Live: r.URL.RequestURI()}
	pg.Filter = s.fleetFilter(r, p, "/fleet/overdue", q)
	var resp adminapi.FleetOverdueResponse
	page, clamped := q.Page, false
	err := s.apiGet(r, "/api/v1/fleet/overdue?"+q.apiValues(page).Encode(), &resp)
	if err == nil {
		if info := m3PageInfo(resp.Total, page, q.Size); info.Clamped && resp.Total > 0 {
			page, clamped = info.Page, true
			err = s.apiGet(r, "/api/v1/fleet/overdue?"+q.apiValues(page).Encode(), &resp)
		}
	}
	if err != nil {
		if apiStatus(err) == http.StatusNotFound {
			s.notFound(w, r, p, "This customer")
			return
		}
		s.log().Error("ui: overdue failed", logx.Class("render"), logx.Err(err))
		pg.Empty = apiFailure("the overdue list", err)
		s.render(w, r, http.StatusOK, "overdue", "base", pg)
		return
	}
	info := m3PageInfo(resp.Total, page, q.Size)
	info.Clamped = info.Clamped || clamped
	pg.Total = resp.Total
	qq := q
	pg.Pager = buildPager(info, "devices", func(pg2, sz int) string {
		m := qq
		m.Page, m.Size = pg2, sz
		return listURL("/fleet/overdue", m.values())
	}, nil)
	groups := map[string]*OverdueGroup{}
	var order []string
	for _, d := range resp.Devices {
		g := groups[d.CustomerID]
		if g == nil {
			g = &OverdueGroup{ID: d.CustomerID, Name: d.Customer}
			groups[d.CustomerID] = g
			order = append(order, d.CustomerID)
		}
		row := OverdueRow{ID: d.ID, Host: d.Hostname, Meta: m3DeviceMeta(d.DeviceClass, d.OSVersion),
			Reasons: overdueReasonChips(d.ID, d, c), State: chipForState("ds-"+d.ID, d.State), Ack: d.Acknowledged}
		switch {
		case d.LastSuccessAgeS == nil:
			row.Age, row.AgeCls, row.Glyph = "never", "bad", ClassCrit
		default:
			row.Age = ageLong(time.Duration(*d.LastSuccessAgeS) * time.Second)
			row.AgeCls, row.Glyph = "warn", ClassWarn
			if d.LastSuccessAt != nil {
				row.AgeSub = c.dayClock(*d.LastSuccessAt)
			}
			for _, name := range d.Chips {
				if name == string(state.StateStale) {
					row.AgeCls, row.Glyph = "bad", ClassCrit
				}
			}
		}
		g.Rows = append(g.Rows, row)
	}
	for _, id := range order {
		g := groups[id]
		g.Count = plural(len(g.Rows), "device", "devices")
		pg.Groups = append(pg.Groups, *g)
	}
	pg.Summary = pg.Pager.Text
	if !resp.Complete && resp.Total > 0 {
		pg.Warn = &EmptyView{Kind: "partial", Title: "Partial data", Body: "Not every device could be classified, so this list may be missing devices. They show as UNKNOWN on the fleet board."}
	}
	if resp.Total == 0 {
		pg.Empty = s.clearOrAlarm("overdue", "Nothing overdue", s.evidence(r, q.Customer), resp.Complete, q.Customer, c)
	}
	s.render(w, r, http.StatusOK, "overdue", "base", pg)
}

// m3DeviceMeta is "desktop · 10.0.22631": the device class and OS as the allow-lists pass them.
func m3DeviceMeta(class, os string) string {
	var out []string
	if class != "" {
		out = append(out, enumOrWithheld(class, "laptop", "desktop", "unknown"))
	}
	if os != "" {
		out = append(out, safeOS(os))
	}
	return strings.Join(out, " · ")
}

func (s *Service) fleetFilter(r *http.Request, p *auth.Principal, action string, q fleetListQuery) FleetFilter {
	opts, ok := s.filterCustomers(r.Context(), p, nonEmpty1(q.Customer), "All customers")
	return FleetFilter{Action: action, Customers: opts, Customer: q.Customer, ListErr: !ok, Size: q.Size}
}

func nonEmpty1(v string) []string {
	if v == "" {
		return nil
	}
	return []string{v}
}

// ---- B05 ----

// UnseenRow is one row of the devices-not-seen list: a device, or one customer
// whose devices went silent together.
type UnseenRow struct {
	Silence    bool
	ID         string // device id
	Host       string
	Meta       string
	CustomerID string
	Customer   string
	State      Chip
	Last       string
	LastSub    string
	LastCls    string
	LastGlyph  string
	Affected   string // "7 of 9 devices silent"
	Unseen     string
	Covered    bool
	Hint       string
	Ack        bool
	// Silence rows: how many devices the row speaks for, and the link that lists exactly those.
	N        int
	Expanded bool
	ShowHref string
	HideHref string
	Devices  []UnseenDev
}

// UnseenDev is one device of an expanded silence row.
type UnseenDev struct {
	ID    string
	Host  string
	State Chip
	Last  string
}

// UnseenPage is B05.
type UnseenPage struct {
	Page
	Filter  FleetFilter
	Rows    []UnseenRow
	Devices int
	Total   int
	Summary string
	Pager   PagerView
	Empty   *EmptyView
	Warn    *EmptyView
	AsOf    string
	Zone    string
	Live    string // this page's own URL, polled to refresh the live region
}

const (
	unseenDeviceHint  = "Likely cause: certificate, DNS, or the machine is off."
	unseenSilenceHint = "Several devices went silent together, so look at the site first: its connection, DNS or a certificate, before any single machine."
)

func (s *Service) unseenPage(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	fleet, ok := s.fleetFor(w, r, p, "Not seen")
	if !ok {
		return
	}
	q, bad := parseFleetListQuery(r.URL.Query())
	if len(bad) > 0 {
		s.badQuery(w, r, p, fleet, bad)
		return
	}
	c := s.fcAt(r.Context(), fleet.Computed)
	pg := UnseenPage{Page: s.page(r.Context(), p, fleet, "Devices not seen", "unseen", "/ui/events"), AsOf: c.clock(fleet.Computed), Zone: c.Zone, Live: r.URL.RequestURI()}
	pg.Filter = s.fleetFilter(r, p, "/fleet/unseen", q)
	var resp adminapi.FleetUnseenResponse
	page, clamped := q.Page, false
	err := s.apiGet(r, "/api/v1/fleet/unseen?"+q.apiValues(page).Encode(), &resp)
	if err == nil {
		if info := m3PageInfo(resp.Total, page, q.Size); info.Clamped && resp.Total > 0 {
			page, clamped = info.Page, true
			err = s.apiGet(r, "/api/v1/fleet/unseen?"+q.apiValues(page).Encode(), &resp)
		}
	}
	if err != nil {
		if apiStatus(err) == http.StatusNotFound {
			s.notFound(w, r, p, "This customer")
			return
		}
		s.log().Error("ui: unseen failed", logx.Class("render"), logx.Err(err))
		pg.Empty = apiFailure("the devices-not-seen list", err)
		s.render(w, r, http.StatusOK, "unseen", "base", pg)
		return
	}
	info := m3PageInfo(resp.Total, page, q.Size)
	info.Clamped = info.Clamped || clamped
	pg.Total, pg.Devices = resp.Total, resp.Devices
	qq := q
	pg.Pager = buildPager(info, "rows", func(pg2, sz int) string {
		m := qq
		m.Page, m.Size = pg2, sz
		return listURL("/fleet/unseen", m.values())
	}, nil)
	byID := map[string]state.DeviceState{}
	for _, d := range fleet.View.Devices {
		byID[d.ID] = d
	}
	for _, rw := range resp.Rows {
		row := unseenRow(rw, c)
		if row.Silence {
			// "Show the devices" lists exactly the devices the row counts: the engine's collapse set (DeviceIDs),
			// not a state filter, which matches primary states only and would list fewer.
			sq := q
			sq.Show = rw.CustomerID
			row.ShowHref = listURL("/fleet/unseen", sq.values()) + "#silence-" + rw.CustomerID
			hq := q
			hq.Show = ""
			row.HideHref = listURL("/fleet/unseen", hq.values()) + "#silence-" + rw.CustomerID
			row.N = len(rw.DeviceIDs)
			if q.Show == rw.CustomerID {
				row.Expanded = true
				for _, id := range rw.DeviceIDs {
					d, ok := byID[id]
					ud := UnseenDev{ID: id, Host: "(device not in this view)", State: Chip{ID: "ds-" + id, Label: "UNKNOWN", Class: ClassCrit, Dashed: true, Small: true}, Last: "never"}
					if ok {
						ud.Host, ud.State = d.Hostname, chipForState("ds-"+id, string(d.Primary))
						if !d.Detail.LastSeen.IsZero() {
							ud.Last = ageLong(c.Now.Sub(d.Detail.LastSeen))
						}
					}
					row.Devices = append(row.Devices, ud)
				}
				sort.Slice(row.Devices, func(i, j int) bool { return row.Devices[i].Host < row.Devices[j].Host })
			}
		}
		pg.Rows = append(pg.Rows, row)
	}
	pg.Summary = fmt.Sprintf("%s in %s. %s", plural(resp.Devices, "device", "devices"), plural(resp.Total, "row", "rows"), pg.Pager.Text)
	if !resp.Complete && resp.Total > 0 {
		pg.Warn = &EmptyView{Kind: "partial", Title: "Partial data", Body: "Not every device could be classified, so this list may be missing devices. They show as UNKNOWN on the fleet board."}
	}
	if resp.Total == 0 {
		pg.Empty = s.clearOrAlarm("unseen", "Nothing unseen", s.evidence(r, q.Customer), resp.Complete, q.Customer, c)
	}
	s.render(w, r, http.StatusOK, "unseen", "base", pg)
}

func unseenRow(rw adminapi.UnseenRowJSON, c fmtCtx) UnseenRow {
	row := UnseenRow{CustomerID: rw.CustomerID, Customer: rw.Customer}
	last := func(t *time.Time) {
		switch {
		case t == nil:
			row.Last, row.LastCls, row.LastGlyph = "never", "bad", ClassCrit
		default:
			row.Last, row.LastCls, row.LastGlyph = ageLong(c.Now.Sub(*t)), "bad", ClassCrit
			row.LastSub = c.dayClock(*t)
		}
	}
	if rw.Kind == "customer_silence" {
		row.Silence = true
		row.Affected = fmt.Sprintf("%d of %d devices silent", rw.Affected, rw.Total)
		row.Unseen = fmt.Sprintf("%d unseen", rw.Unseen)
		row.Covered = rw.Covered
		row.Hint = unseenSilenceHint
		last(rw.LastSeenAt)
		return row
	}
	if rw.Device != nil {
		d := rw.Device
		row.ID, row.Host = d.ID, d.Hostname
		row.Meta = m3DeviceMeta(d.DeviceClass, d.OSVersion)
		row.State = chipForState("ds-"+d.ID, d.State)
		row.Ack = d.Acknowledged
	}
	row.Hint = unseenDeviceHint
	last(rw.LastSeenAt)
	return row
}

// ---- S23 ----

// ActCell is one day block of the activity grid.
type ActCell struct {
	Kind  string // success | failed | mixed | missed | none
	Label string // the count shown in the block, "" for none and missed
	Today bool
	Title string // tooltip
	Aria  string
	Href  string // "" when the day has no job to open
}

// DayHead is a day column header.
type DayHead struct {
	Label string // Tue 20
	Full  string // Tue 20 Oct
	Today bool
}

// ActRow is one device row of the grid.
type ActRow struct {
	ID      string
	Host    string
	State   Chip
	Cells   []ActCell
	Missing bool // the answer had no strip for this device
}

// ActGroup is one customer: its header, an all-devices rollup row and its device rows.
type ActGroup struct {
	ID         string
	CustomerID string
	Name       string
	Status     Chip
	Variant    string
	ZoneNote   string
	Days       []DayHead
	Rollup     []ActCell
	Rows       []ActRow
	Note       *EmptyView
	Count      string
	Range      string // "devices 51-100 of 3000" when the page shows a window of a big customer
}

// ActFilter is the filter bar of S23.
type ActFilter struct {
	Customers []Option
	Customer  string
	ListErr   bool
	Problems  bool
	Size      int
}

// ActivityPage is S23.
type ActivityPage struct {
	Page
	Filter  ActFilter
	Groups  []ActGroup
	Summary string
	Pager   PagerView
	Empty   *EmptyView
	Warn    *EmptyView
	Legend  []LegendItem
	AsOf    string
	Zone    string
	Live    string // this page's own URL, polled to refresh the live region
}

// LegendItem explains a block kind; every kind has its own shape.
type LegendItem struct{ Kind, Label string }

type activityQuery struct {
	Customer string
	Problems bool
	Page     int
	Size     int
}

func parseActivityQuery(v url.Values) (activityQuery, []store.FieldError) {
	q := activityQuery{Customer: strings.TrimSpace(v.Get("customer")), Problems: v.Get("problems") == "1",
		Page: atoiDefault(v.Get("page"), 1), Size: store.NormalPageSize(atoiDefault(v.Get("size"), 0))}
	var bad []store.FieldError
	if q.Customer != "" && !idParam(q.Customer) {
		bad = append(bad, store.FieldError{Field: "customer", Message: "Choose one of the listed customers."})
		q.Customer = ""
	}
	return q, bad
}

func (q activityQuery) values() url.Values {
	out := url.Values{}
	if q.Customer != "" {
		out.Set("customer", q.Customer)
	}
	if q.Problems {
		out.Set("problems", "1")
	}
	if q.Page > 1 {
		out.Set("page", strconv.Itoa(q.Page))
	}
	if q.Size != store.DefaultPageSize {
		out.Set("size", strconv.Itoa(q.Size))
	}
	return out
}

func dayHeads(days []adminapi.DayRollupJSON) []DayHead {
	out := make([]DayHead, 0, len(days))
	for i, d := range days {
		h := DayHead{Label: d.Date, Full: d.Date, Today: i == len(days)-1}
		if t, err := time.Parse(dateLayout, d.Date); err == nil {
			h.Label, h.Full = t.Format("Mon 2"), t.Format("Mon 2 Jan")
		}
		out = append(out, h)
	}
	return out
}

// runsLink is the S21 URL of a device or customer on one calendar day, read in
// the zone the day belongs to.
func runsLink(field, id, date, zone, displayZone string) string {
	v := url.Values{}
	v.Set(field, id)
	v.Set("from", date)
	v.Set("to", date)
	if zone != "" && zone != displayZone {
		v.Set("tz", zone)
	}
	return listURL("/runs", v)
}

// actPick is what one customer contributes to a page of the grid: its all-devices
// row, a window of its devices and where that window sits among all of them.
type actPick struct {
	cu       adminapi.TenDayCustomerJSON
	devs     []adminapi.TenDayDeviceJSON // the window
	of       int                         // the customer's devices
	from, to int                         // 1-based first and last device of the window, 0 for none
	missing  []string                    // devices of the window the answer had no strip for
}

// winPart is the part of one customer a page shows: units [a, b) of its units.
type winPart struct{ idx, a, b int }

// unitWindow cuts the rows [lo, hi) out of customers laid end to end, units[i]
// rows for customer i (a customer with no device counts as one row, its alarm
// or note). It returns, for every customer with a row in the window, which of its
// rows: the window may start or end inside a customer, so a customer with
// thousands of devices is paged by device.
func unitWindow(units []int, lo, hi int) []winPart {
	var out []winPart
	off := 0
	for i, n := range units {
		if a, b := max(lo-off, 0), min(hi-off, n); a < b {
			out = append(out, winPart{i, a, b})
		}
		off += n
	}
	return out
}

func (s *Service) activityPage(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	fleet, ok := s.fleetFor(w, r, p, "Activity")
	if !ok {
		return
	}
	q, bad := parseActivityQuery(r.URL.Query())
	if len(bad) > 0 {
		s.badQuery(w, r, p, fleet, bad)
		return
	}
	c := s.fcAt(r.Context(), fleet.Computed)
	pg := ActivityPage{Page: s.page(r.Context(), p, fleet, "Activity", "activity", "/ui/events"), AsOf: c.clock(fleet.Computed), Zone: c.Zone, Live: r.URL.RequestURI()}
	opts, lok := s.filterCustomers(r.Context(), p, nonEmpty1(q.Customer), "All customers")
	pg.Filter = ActFilter{Customers: opts, Customer: q.Customer, ListErr: !lok, Problems: q.Problems, Size: q.Size}
	pg.Legend = []LegendItem{{"success", "All succeeded"}, {"failed", "Any failed"}, {"mixed", "Succeeded and failed"}, {"missed", "Expected, none ran"}, {"none", "No runs"}}

	// The customers of the grid are the engine's, in its exceptions-first order, from the one evaluation
	// of this request. The strips come from the ten-day answer, one customer at a time.
	var custs []state.CustomerState
	devices := 0 // active devices in the view the filter leaves
	for _, cs := range fleet.View.Customers {
		if q.Customer != "" && cs.ID != q.Customer {
			continue
		}
		custs = append(custs, cs)
		for _, d := range cs.Devices {
			if d.Primary != state.StateRetired {
				devices++
			}
		}
	}
	if q.Customer != "" && len(custs) == 0 {
		s.notFound(w, r, p, "This customer")
		return
	}
	live := func(cs state.CustomerState) []state.DeviceState {
		var out []state.DeviceState
		for _, d := range cs.Devices {
			if d.Primary != state.StateRetired {
				out = append(out, d)
			}
		}
		return out
	}
	fetch := func(customer string) (adminapi.FleetTenDayResponse, error) {
		v := url.Values{"devices": {"true"}}
		if customer != "" {
			v.Set("customer", customer)
		}
		var resp adminapi.FleetTenDayResponse
		return resp, s.apiGet(r, "/api/v1/fleet/ten-day?"+v.Encode(), &resp)
	}
	fail := func(err error) {
		s.log().Error("ui: activity failed", logx.Class("render"), logx.Err(err))
		pg.Empty = apiFailure("the activity grid", err)
		s.render(w, r, http.StatusOK, "activity", "base", pg)
	}

	var picks []actPick
	var rows int // device rows (a customer with none counts as one) over every page
	size := q.Size
	lo, hi := (q.Page-1)*size, q.Page*size
	complete := fleet.View.Complete
	if !q.Problems {
		// Page by device, across customers. Only the customers whose devices are on this page are read.
		units := make([]int, len(custs))
		for i, cs := range custs {
			units[i] = max(len(live(cs)), 1)
			rows += units[i]
		}
		for _, part := range unitWindow(units, lo, hi) {
			cs := custs[part.idx]
			ds := live(cs)
			n := len(ds)
			resp, err := fetch(cs.ID)
			if err != nil {
				fail(err)
				return
			}
			complete = complete && resp.Complete
			if len(resp.Customers) == 0 {
				fail(&apiError{Status: 0, Code: "empty_answer"})
				return
			}
			pk := actPick{cu: resp.Customers[0], of: n}
			byID := map[string]adminapi.TenDayDeviceJSON{}
			for _, d := range pk.cu.Devices {
				byID[d.ID] = d
			}
			if n > 0 {
				pk.from, pk.to = part.a+1, part.b
				for _, d := range ds[part.a:part.b] {
					if td, ok := byID[d.ID]; ok {
						pk.devs = append(pk.devs, td)
					} else {
						pk.missing = append(pk.missing, d.Hostname)
					}
				}
			}
			picks = append(picks, pk)
		}
	} else {
		// Only devices with a failed, mixed or missed day: that needs every strip, so every customer is read once.
		resp, err := fetch(q.Customer)
		if err != nil {
			fail(err)
			return
		}
		complete = complete && resp.Complete
		for _, cu := range resp.Customers {
			var hit []adminapi.TenDayDeviceJSON
			for _, d := range cu.Devices {
				for _, day := range d.Days {
					if day.Kind == "failed" || day.Kind == "mixed" || day.Kind == "missed" {
						hit = append(hit, d)
						break
					}
				}
			}
			if len(hit) == 0 {
				continue
			}
			a, b := max(lo-rows, 0), min(hi-rows, len(hit))
			if a < b {
				picks = append(picks, actPick{cu: cu, devs: hit[a:b], of: len(hit), from: a + 1, to: b})
			}
			rows += len(hit)
		}
	}
	info := m3PageInfo(rows, q.Page, size)
	pg.Pager = buildPager(info, "devices", func(pg2, sz int) string {
		m := q
		m.Page, m.Size = pg2, sz
		return listURL("/activity", m.values())
	}, nil)
	pg.Summary = pg.Pager.Text
	if rows > 0 {
		pg.Summary += " · " + plural(len(picks), "customer", "customers") + " on this page"
	}
	if info.Clamped && rows > 0 { // a page past the end shows the last one: re-run the window
		q.Page = info.Page
		http.Redirect(w, r, listURL("/activity", q.values()), http.StatusSeeOther)
		return
	}
	for _, pk := range picks {
		cu := pk.cu
		g := ActGroup{ID: "act-" + cu.ID, CustomerID: cu.ID, Name: cu.Name, Status: customerStatusChip("as-"+cu.ID, cu.Status), Days: dayHeads(cu.Days), Count: plural(pk.of, "device", "devices")}
		if cu.Zone != "" && cu.Zone != c.Zone {
			g.ZoneNote = "days follow " + cu.Zone
		}
		if pk.of > len(pk.devs)+len(pk.missing) {
			g.Range = fmt.Sprintf("devices %d-%d of %d", pk.from, pk.to, pk.of)
		}
		if q.Problems {
			g.Count = plural(pk.of, "device with a failed or missed day", "devices with a failed or missed day")
		}
		for i, day := range cu.Days {
			g.Rollup = append(g.Rollup, rollupCell(cu, day, i == len(cu.Days)-1, c.Zone))
		}
		for _, d := range pk.devs {
			row := ActRow{ID: d.ID, Host: d.Hostname, State: chipForState("ds-"+d.ID, d.State)}
			for i, day := range d.Days {
				row.Cells = append(row.Cells, deviceCell(d, day, i == len(d.Days)-1, cu.Zone, c.Zone))
			}
			g.Rows = append(g.Rows, row)
		}
		for _, host := range pk.missing {
			g.Rows = append(g.Rows, ActRow{Host: host, Missing: true})
		}
		switch {
		case pk.of == 0 && cu.Status == string(state.CustomerMissing):
			g.Variant = "missing"
			g.Note = &EmptyView{Kind: "alarm", Title: "MISSING: no devices", Body: cu.Name + " is expected but has no active devices, so there is no activity to show and every machine there is unmonitored."}
		case pk.of == 0:
			g.Note = &EmptyView{Kind: "neutral", Title: "No devices", Body: cu.Name + " has no devices yet."}
		}
		pg.Groups = append(pg.Groups, g)
	}
	if !complete {
		pg.Warn = &EmptyView{Kind: "partial", Title: "Partial data", Body: "Some devices could not be read, so their rows may be missing or blank. A blank block here is not a clean day."}
	}
	switch {
	case devices == 0 && fleet.View.FirstRun:
		pg.Empty = &EmptyView{Kind: "neutral", Title: "No customers yet", Body: "Add your first customer and enroll its devices: their last ten days appear here."}
	case devices == 0:
		// Enrolled before, and nothing active now (all retired, or none in this account's scope): not a quiet fleet.
		pg.Empty = &EmptyView{Kind: "alarm", Title: "No devices to show",
			Body: "There are no active devices in this view. That is an alarm, not a quiet fleet: nothing is being backed up here."}
	case q.Problems && rows == 0:
		pg.Empty = &EmptyView{Kind: "neutral", Title: "No device has a failed, mixed or missed day",
			Body: fmt.Sprintf("%s checked at %s (%s): none had a failed, mixed or missed day in the last ten days. Untick the filter to see them all.", plural(devices, "device", "devices"), c.clock(fleet.Computed), c.Zone)}
	}
	s.render(w, r, http.StatusOK, "activity", "base", pg)
}

func jobsText(success, failed int) string {
	n := success + failed
	switch {
	case n == 0:
		return "no jobs"
	case failed == 0:
		return plural(n, "job", "jobs")
	}
	return fmt.Sprintf("%s, %d failed", plural(n, "job", "jobs"), failed)
}

func deviceCell(d adminapi.TenDayDeviceJSON, day adminapi.DayJSON, today bool, zone, displayZone string) ActCell {
	t, _ := time.Parse(dateLayout, day.Date)
	full := t.Format("Mon 2 Jan")
	cell := ActCell{Kind: day.Kind, Today: today}
	switch day.Kind {
	case "success", "failed", "mixed", "missed", "none":
	default:
		cell.Kind = "none"
	}
	n := day.Success + day.Failed
	switch cell.Kind {
	case "missed":
		cell.Title = "Missed: a run was expected and none happened"
		cell.Aria = fmt.Sprintf("%s, %s: missed", d.Hostname, full)
	case "none":
		cell.Title = "No runs"
		cell.Aria = fmt.Sprintf("%s, %s: no runs", d.Hostname, full)
		if n > 0 { // a day with runs the engine did not class: say what the counts are
			cell.Title = jobsText(day.Success, day.Failed)
		}
	default:
		cell.Title = jobsText(day.Success, day.Failed)
		cell.Aria = fmt.Sprintf("%s, %s: %s", d.Hostname, full, jobsText(day.Success, day.Failed))
	}
	if n > 0 {
		cell.Label = strconv.Itoa(n)
		cell.Href = runsLink("device", d.ID, day.Date, zone, displayZone)
		if cell.Aria == "" || cell.Kind == "none" {
			cell.Aria = fmt.Sprintf("%s, %s: %s", d.Hostname, full, jobsText(day.Success, day.Failed))
		}
	}
	cell.Title = full + ": " + cell.Title
	return cell
}

// rollupCell is the all-devices block of a customer for one day: the worst kind,
// labelled with how many devices had it.
func rollupCell(cu adminapi.TenDayCustomerJSON, d adminapi.DayRollupJSON, today bool, displayZone string) ActCell {
	t, _ := time.Parse(dateLayout, d.Date)
	full := t.Format("Mon 2 Jan")
	cell := ActCell{Kind: d.Worst, Today: today}
	count := 0
	switch d.Worst {
	case "failed":
		count = d.Failed
	case "missed":
		count = d.Missed
	case "mixed":
		count = d.Mixed
	case "success":
		count = d.Success
	default:
		cell.Kind = "none"
	}
	if cell.Kind != "none" {
		cell.Label = strconv.Itoa(count)
	}
	parts := []string{}
	for _, x := range []struct {
		n int
		w string
	}{{d.Success, "all succeeded"}, {d.Mixed, "succeeded and failed"}, {d.Failed, "failed"}, {d.Missed, "missed"}, {d.None, "no runs"}} {
		if x.n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", x.n, x.w))
		}
	}
	summary := strings.Join(parts, ", ")
	cell.Title = full + ": " + summary
	cell.Aria = fmt.Sprintf("%s, all devices, %s: %s", cu.Name, full, summary)
	if cell.Kind != "none" {
		cell.Href = runsLink("customer", cu.ID, d.Date, cu.Zone, displayZone)
	}
	return cell
}
