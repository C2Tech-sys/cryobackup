package ui

import (
	"fmt"
	"net/http"
	"net/url"
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
	return q, bad
}

func (q fleetListQuery) values() url.Values {
	out := url.Values{}
	if q.Customer != "" {
		out.Set("customer", q.Customer)
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
	pg := OverduePage{Page: s.page(r.Context(), p, fleet, "Overdue", "overdue", "/ui/events"), AsOf: c.clock(fleet.Computed), Zone: c.Zone}
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
		row := OverdueRow{ID: d.ID, Host: d.Hostname, Meta: strings.TrimSpace(strings.Join(nonEmpty(d.DeviceClass, d.OSVersion), " · ")),
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

func nonEmpty(in ...string) []string {
	var out []string
	for _, s := range in {
		if s != "" {
			out = append(out, safeWordLoose(s))
		}
	}
	return out
}

// safeWordLoose is safeWord for a descriptive field that may hold spaces (an OS
// name like "Windows 11 Pro"): still no slash, colon, backslash or quote.
func safeWordLoose(s string) string {
	if len(s) > 60 || strings.ContainsAny(s, "/\\:\"'<>\r\n\x00") {
		return withheld
	}
	return s
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
	ShowHref   string
	Hint       string
	Ack        bool
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
	pg := UnseenPage{Page: s.page(r.Context(), p, fleet, "Devices not seen", "unseen", "/ui/events"), AsOf: c.clock(fleet.Computed), Zone: c.Zone}
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
	for _, rw := range resp.Rows {
		pg.Rows = append(pg.Rows, unseenRow(rw, c))
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
		row.ShowHref = "/devices?customer=" + url.QueryEscape(rw.CustomerID) + "&state=UNSEEN"
		last(rw.LastSeenAt)
		return row
	}
	if rw.Device != nil {
		d := rw.Device
		row.ID, row.Host = d.ID, d.Hostname
		row.Meta = strings.Join(nonEmpty(d.DeviceClass, d.OSVersion), " · ")
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
	ID    string
	Host  string
	State Chip
	Cells []ActCell
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
	pg := ActivityPage{Page: s.page(r.Context(), p, fleet, "Activity", "activity", "/ui/events"), AsOf: c.clock(fleet.Computed), Zone: c.Zone}
	opts, lok := s.filterCustomers(r.Context(), p, nonEmpty1(q.Customer), "All customers")
	pg.Filter = ActFilter{Customers: opts, Customer: q.Customer, ListErr: !lok, Problems: q.Problems, Size: q.Size}
	pg.Legend = []LegendItem{{"success", "All succeeded"}, {"failed", "Any failed"}, {"mixed", "Succeeded and failed"}, {"missed", "Expected, none ran"}, {"none", "No runs"}}
	v := url.Values{"devices": {"true"}}
	if q.Customer != "" {
		v.Set("customer", q.Customer)
	}
	var resp adminapi.FleetTenDayResponse
	if err := s.apiGet(r, "/api/v1/fleet/ten-day?"+v.Encode(), &resp); err != nil {
		if apiStatus(err) == http.StatusNotFound {
			s.notFound(w, r, p, "This customer")
			return
		}
		s.log().Error("ui: activity failed", logx.Class("render"), logx.Err(err))
		pg.Empty = apiFailure("the activity grid", err)
		s.render(w, r, http.StatusOK, "activity", "base", pg)
		return
	}
	devices, shown := 0, 0
	var groups []ActGroup
	for _, cu := range resp.Customers {
		g := ActGroup{ID: "act-" + cu.ID, CustomerID: cu.ID, Name: cu.Name, Status: customerStatusChip("as-"+cu.ID, cu.Status), Days: dayHeads(cu.Days)}
		if cu.Zone != "" && cu.Zone != c.Zone {
			g.ZoneNote = "days follow " + cu.Zone
		}
		for _, dc := range cu.Days {
			g.Rollup = append(g.Rollup, rollupCell(cu, dc, len(g.Rollup) == len(cu.Days)-1, c.Zone))
		}
		for _, d := range cu.Devices {
			devices++
			row := ActRow{ID: d.ID, Host: d.Hostname, State: chipForState("ds-"+d.ID, d.State)}
			problem := false
			for i, day := range d.Days {
				cell := deviceCell(d, day, i == len(d.Days)-1, cu.Zone, c.Zone)
				if day.Kind == "failed" || day.Kind == "mixed" || day.Kind == "missed" {
					problem = true
				}
				row.Cells = append(row.Cells, cell)
			}
			if q.Problems && !problem {
				continue
			}
			g.Rows = append(g.Rows, row)
			shown++
		}
		g.Count = plural(len(g.Rows), "device", "devices")
		switch {
		case len(cu.Devices) == 0 && cu.Status == string(state.CustomerMissing):
			g.Variant = "missing"
			g.Note = &EmptyView{Kind: "alarm", Title: "MISSING: no devices", Body: cu.Name + " is expected but has no active devices, so there is no activity to show and every machine there is unmonitored."}
		case len(cu.Devices) == 0:
			g.Note = &EmptyView{Kind: "neutral", Title: "No devices", Body: cu.Name + " has no devices yet."}
		case len(g.Rows) == 0 && q.Problems:
			continue // the filter removed every row of a healthy customer
		case len(g.Rows) == 0:
			g.Note = &EmptyView{Kind: "neutral", Title: "Only retired devices", Body: "Every device here is retired. Tick \"Show retired\" to see them."}
		}
		groups = append(groups, g)
	}
	// Customers per page; every page shows complete groups.
	info := m3PageInfo(len(groups), q.Page, q.Size)
	from, to := 0, 0
	if len(groups) > 0 {
		from, to = info.From-1, info.To
	}
	pg.Groups = groups[from:to]
	qq := q
	pg.Pager = buildPager(info, "customers", func(pg2, sz int) string {
		m := qq
		m.Page, m.Size = pg2, sz
		return listURL("/activity", m.values())
	}, nil)
	if len(groups) > 0 {
		pg.Pager.Text = fmt.Sprintf("Customers %d-%d of %d · %s", info.From, info.To, len(groups), plural(shown, "device", "devices"))
	}
	pg.Summary = pg.Pager.Text
	if !resp.Complete {
		pg.Warn = &EmptyView{Kind: "partial", Title: "Partial data", Body: "Some devices could not be read, so their rows may be missing or blank. A blank block here is not a clean day."}
	}
	if len(resp.Customers) == 0 || (devices == 0 && !hasMissingOrOnboarding(resp)) {
		ev := s.evidence(r, q.Customer)
		switch {
		case ev.Err == nil && ev.Sum.FirstRun && q.Customer == "":
			pg.Empty = &EmptyView{Kind: "neutral", Title: "No customers yet", Body: "Add your first customer and enroll its devices: their last ten days appear here."}
		default:
			pg.Empty = &EmptyView{Kind: "alarm", Title: "No devices to show",
				Body: "The grid is empty because there are no enrolled devices in this view. That is an alarm, not a quiet fleet: nothing is being backed up."}
		}
	}
	s.render(w, r, http.StatusOK, "activity", "base", pg)
}

func hasMissingOrOnboarding(resp adminapi.FleetTenDayResponse) bool {
	for _, cu := range resp.Customers {
		if cu.Status == string(state.CustomerMissing) || cu.Status == string(state.CustomerOnboarding) || cu.Status == string(state.CustomerEmpty) {
			return true
		}
	}
	return false
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
