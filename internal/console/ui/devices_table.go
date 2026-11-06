package ui

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/health"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// The device table (B01 flat, S06, S19; components 5.1): host, customer, state
// chip, last success, next due, agent version and OS, with sort, filters,
// numbered paging and a query string that holds all of it. /devices shows every
// customer in scope; the customer page shows the same table for one customer.
// The table results are also served as a fragment (GET /ui/devices/table) that
// htmx swaps in and pushes the canonical URL for.

// DevHead is a column header; a sortable one links to its own sort.
type DevHead struct {
	Key      string
	Label    string
	Href     string
	Partial  string
	Sortable bool
	Sort     string // aria-sort: ascending | descending | none
}

// DevRow is one row of the device table.
type DevRow struct {
	DeviceRow
	Customer   string
	CustomerID string
	Friendly   string
	OS         string
	NextText   string
	NextTitle  string
	NextClass  string
	Retired    bool
}

// DevFilterForm is the filter bar.
type DevFilterForm struct {
	Q         string
	Retired   bool
	Customer  string
	Customers []Option
	States    []Option
	Sort      string
	Desc      bool
	Size      int
}

// DevTable is everything the table templates need.
type DevTable struct {
	BaseURL    string // the canonical page, "/devices" or "/customers/{id}/devices"
	PartialURL string // the fragment endpoint
	// CustomerID is set on the customer page: the table is already that
	// customer's, so the customer column and filter are hidden.
	CustomerID   string
	ShowCustomer bool
	Heads        []DevHead
	Rows         []DevRow
	Pager        PagerView
	Chips        []FilterChip
	ClearHref    string
	ClearPartial string
	Form         DevFilterForm
	Empty        *EmptyView
	Summary      string
	AsOf         string
	Zone         string
	Filtered     bool
	CSRF         string
	// ColSpan is the column count, for full-width rows.
	ColSpan int
}

// devTableSpec says what a table is a view of.
type devTableSpec struct {
	Base       string
	CustomerID string
	// Customer is the customer row on a customer page (its expected flag decides
	// what an empty table means); nil on /devices.
	Customer *store.CustomerRow
}

func (spec devTableSpec) partialFor(q devQuery) string {
	v := q.values()
	if spec.CustomerID != "" {
		v.Set("customer", spec.CustomerID)
		v.Set("tab", "1")
	}
	return listURL("/ui/devices/table", v)
}

func (spec devTableSpec) hrefFor(q devQuery) string {
	if spec.CustomerID != "" {
		q.Customer = ""
	}
	return listURL(spec.Base, q.values())
}

var devHeads = []struct {
	Key, Label string
}{{"host", "Device"}, {"customer", "Customer"}, {"state", "State"}, {"last_success", "Last success"}, {"next_due", "Next due"}, {"agent", "Agent"}, {"os", "OS"}}

// nextDueView renders a NextDue as text, a tooltip and a class.
func nextDueView(n store.NextDue, c fmtCtx) (text, title, class string) {
	switch n.Kind {
	case store.DueNow:
		return "due now", "A backup may start now.", ""
	case store.DueScheduled:
		return ageFuture(n.At.Sub(c.Now)), c.dayClock(n.At) + " (" + c.zoneAbbr(n.At) + "). An estimate from the schedule and the last success.", ""
	case store.DuePaused:
		return "parked", "Parked: nothing is due until it is resumed.", ""
	case store.DueRetired:
		return "-", "Retired.", ""
	}
	return "unknown", "No usable schedule could be read for this device.", "warn"
}

// buildDevTable reads and shapes one table. fleet is the evaluated view for the
// principal's scope. The returned status is the HTTP status to answer with.
func (s *Service) buildDevTable(ctx context.Context, p *auth.Principal, fleet health.Fleet, q devQuery, spec devTableSpec) (DevTable, int, error) {
	c := s.fcAt(ctx, fleet.Computed)
	if spec.CustomerID != "" {
		q.Customer = spec.CustomerID
	}
	sortKey := q.Sort
	tb, err := s.Store.DeviceTable(ctx, p.Scope, store.DeviceFilter{CustomerID: q.Customer, Search: q.Q, IncludeRetired: q.Retired},
		fleet.View.Devices, store.DeviceTableQuery{States: q.States, Sort: sortKey, Desc: q.Desc, Page: q.Page, Size: q.Size}, fleet.Computed, s.loc(ctx))
	if err != nil {
		if errors.Is(err, store.ErrInvalid) {
			return DevTable{}, http.StatusBadRequest, err
		}
		return DevTable{}, http.StatusServiceUnavailable, err
	}
	q.Page, q.Size = tb.Page.Page, tb.Page.Size
	t := DevTable{BaseURL: spec.Base, PartialURL: "/ui/devices/table", CustomerID: spec.CustomerID, ShowCustomer: spec.CustomerID == "",
		AsOf: c.clock(fleet.Computed), Zone: s.zone(ctx), Filtered: q.filtered() || (q.Customer != "" && spec.CustomerID == ""), CSRF: p.CSRF}
	t.ColSpan = 7
	if !t.ShowCustomer {
		t.ColSpan = 6
	}
	href := func(mod func(*devQuery)) (string, string) {
		m := q
		m.States = append([]string(nil), q.States...)
		mod(&m)
		return spec.hrefFor(m), spec.partialFor(m)
	}
	// headers
	for _, h := range devHeads {
		if h.Key == "customer" && !t.ShowCustomer {
			continue
		}
		head := DevHead{Key: h.Key, Label: h.Label, Sortable: true, Sort: "none"}
		cur := q.Sort
		if cur == "" {
			cur = "state"
		}
		nextDesc := false
		if cur == h.Key { // clicking the active header flips its direction
			head.Sort = "ascending"
			if q.Desc {
				head.Sort = "descending"
			}
			nextDesc = !q.Desc
		}
		key := h.Key
		head.Href, head.Partial = href(func(m *devQuery) {
			m.Sort, m.Desc, m.Page = key, nextDesc, 1
			if key == "state" && !nextDesc {
				m.Sort = "" // the default order
			}
		})
		t.Heads = append(t.Heads, head)
	}
	// rows
	rows := make([]DevRow, 0, len(tb.Rows))
	for _, r := range tb.Rows {
		ds := r.State
		if !r.InView { // no verdict: UNKNOWN, loud
			ds = state.DeviceState{ID: r.ID, CustomerID: r.CustomerID, Hostname: r.Hostname, Primary: state.StateUnknown, Alarm: state.SeverityCritical}
		}
		dr := deviceRow(ds, deviceMeta{OS: r.OSVersion, Class: r.DeviceClass, Enrolled: r.EnrolledAt}, nil, RunInfo{}, c)
		if ds.Primary == state.StateRetired {
			dr.AgeText = "-"
			if !ds.Detail.LastSuccess.IsZero() {
				dr.AgeText = ageLong(c.Now.Sub(ds.Detail.LastSuccess))
			}
		}
		if dr.Agent == "" {
			dr.Agent = agentLabel(r.AgentVersion)
		}
		row := DevRow{DeviceRow: dr, Customer: r.CustomerName, CustomerID: r.CustomerID, Friendly: r.FriendlyName, OS: osLabel(r.OSVersion), Retired: r.RetiredAt != nil}
		row.NextText, row.NextTitle, row.NextClass = nextDueView(r.Next, c)
		rows = append(rows, row)
	}
	t.Rows = rows
	// pager
	t.Pager = buildPager(tb.Page, "devices",
		func(page, size int) string { m := q; m.Page, m.Size = page, size; return spec.hrefFor(m) },
		func(page, size int) string { m := q; m.Page, m.Size = page, size; return spec.partialFor(m) })
	t.Summary = t.Pager.Text
	// the filter bar and the removable chips
	custs, err := s.Store.ListCustomerRows(ctx, p.Scope, store.CustomerListQuery{})
	if err != nil {
		return DevTable{}, http.StatusServiceUnavailable, err
	}
	custOpts := []Option{{Value: "", Label: "All customers", Selected: q.Customer == ""}}
	custName := map[string]string{}
	for _, cu := range custs {
		custName[cu.ID] = cu.Name
		custOpts = append(custOpts, Option{Value: cu.ID, Label: cu.Name, Selected: cu.ID == q.Customer})
	}
	first := ""
	if len(q.States) > 0 {
		first = q.States[0]
	}
	t.Form = DevFilterForm{Q: q.Q, Retired: q.Retired, Customer: q.Customer, Customers: custOpts, States: stateOptions(first), Sort: q.Sort, Desc: q.Desc, Size: q.Size}
	addChip := func(label string, mod func(*devQuery)) {
		h, pt := href(mod)
		t.Chips = append(t.Chips, FilterChip{Label: label, Href: h, Partial: pt})
	}
	if q.Customer != "" && spec.CustomerID == "" {
		name := custName[q.Customer]
		if name == "" {
			name = "unknown customer"
		}
		addChip("Customer: "+name, func(m *devQuery) { m.Customer, m.Page = "", 1 })
	}
	for _, st := range q.States {
		st := st
		addChip("State: "+stateLabel(state.State(st)), func(m *devQuery) {
			var keep []string
			for _, x := range m.States {
				if x != st {
					keep = append(keep, x)
				}
			}
			m.States, m.Page = keep, 1
		})
	}
	if q.Q != "" {
		addChip(`Text: "`+q.Q+`"`, func(m *devQuery) { m.Q, m.Page = "", 1 })
	}
	if q.Retired {
		addChip("Including retired", func(m *devQuery) { m.Retired, m.Page = false, 1 })
	}
	t.ClearHref, t.ClearPartial = href(func(m *devQuery) { *m = devQuery{Size: m.Size, Sort: m.Sort, Desc: m.Desc, Page: 1} })
	t.Empty = s.devEmpty(ctx, p, fleet, spec, t, tb, c, q)
	return t, http.StatusOK, nil
}

// devEmpty classifies an empty table (components 9.5): filter-caused empties are
// neutral and show the denominator; a first run is the only neutral no-data
// case; everything else where devices must exist is an alarm.
func (s *Service) devEmpty(ctx context.Context, p *auth.Principal, fleet health.Fleet, spec devTableSpec, t DevTable, tb store.DeviceTable, c fmtCtx, q devQuery) *EmptyView {
	if len(tb.Rows) > 0 {
		return nil
	}
	// The denominator of "0 of N shown": every device the table could show without
	// its filters, that is the customer's own on a customer page and every device in
	// scope on /devices. A filter naming a customer that is unknown or outside scope
	// therefore reads exactly like any other filter that matches nothing: no alarm
	// and no way to tell the two apart.
	total := tb.Total
	if total == 0 && t.Filtered {
		if rows, err := s.Store.ListDeviceRows(ctx, p.Scope, store.DeviceFilter{CustomerID: spec.CustomerID, IncludeRetired: q.Retired}); err == nil {
			total = len(rows)
		}
	}
	if total > 0 { // devices exist, the filters hide them all
		return &EmptyView{Kind: "neutral", Title: "No devices match these filters",
			Body: fmt.Sprintf("0 of %d shown.", total), Link: &Link{Href: t.ClearHref, Label: "Clear filters"}}
	}
	if cu := spec.Customer; cu != nil {
		return s.customerNoDevices(*cu, c)
	}
	if fleet.View.FirstRun {
		return &EmptyView{Kind: "neutral", Title: "No customers yet",
			Body: "Create the first customer and an enrollment token with: cryo-console token --create-customer \"Customer name\". Devices appear here once they enroll."}
	}
	return &EmptyView{Kind: "alarm", Title: "No devices are enrolled",
		Body: "Nothing is being backed up. Enroll a customer or check the agent listener."}
}

// ---- handlers ----

// DevicesPage is the all-devices page.
type DevicesPage struct {
	Page
	Table DevTable
}

func (s *Service) fleetFor(w http.ResponseWriter, r *http.Request, p *auth.Principal, title string) (health.Fleet, bool) {
	f, err := s.Fleet.Fleet(r.Context(), p.Scope)
	if err != nil {
		s.log().Error("ui: fleet failed", logx.Str("page", r.URL.Path), logx.Err(err))
		s.render(w, r, http.StatusServiceUnavailable, "error", "base", errorData(s.fallbackPage(r.Context(), p), 503, title+" unavailable",
			"The fleet could not be loaded. This is an alarm, not an empty list. Try again shortly."))
		return health.Fleet{}, false
	}
	return f, true
}

// badQuery answers a list URL whose filter or sort means nothing.
func (s *Service) badQuery(w http.ResponseWriter, r *http.Request, p *auth.Principal, fleet health.Fleet, bad []store.FieldError) {
	msg := "This link's filters are not valid"
	if len(bad) > 0 {
		msg += ": " + bad[0].Message
	}
	s.errorPage(w, r, p, http.StatusBadRequest, "Filters not valid", msg+" Return to the unfiltered list.")
}

func (s *Service) devicesPage(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	fleet, ok := s.fleetFor(w, r, p, "Devices")
	if !ok {
		return
	}
	q, bad := parseDevQuery(r.URL.Query())
	if len(bad) > 0 {
		s.badQuery(w, r, p, fleet, bad)
		return
	}
	t, status, err := s.buildDevTable(r.Context(), p, fleet, q, devTableSpec{Base: "/devices"})
	if err != nil {
		s.tableFailed(w, r, p, fleet, status, err)
		return
	}
	pg := s.page(r.Context(), p, fleet, "Devices", "devices", "/ui/events")
	s.render(w, r, http.StatusOK, "devices", "base", DevicesPage{Page: pg, Table: t})
}

func (s *Service) tableFailed(w http.ResponseWriter, r *http.Request, p *auth.Principal, fleet health.Fleet, status int, err error) {
	if status == http.StatusBadRequest {
		s.badQuery(w, r, p, fleet, store.FieldErrors(err))
		return
	}
	s.log().Error("ui: device table failed", logx.Err(err))
	s.render(w, r, http.StatusServiceUnavailable, "error", "base", errorData(s.page(r.Context(), p, fleet, "Devices unavailable", "devices", ""), 503, "Devices unavailable",
		"The device table could not be read. This is an alarm, not an empty list. Try again shortly."))
}

// devicesTablePartial is GET /ui/devices/table: the results region of the
// table for htmx. It answers HX-Push-Url with the canonical page URL so the
// address bar always holds a deep link, never the fragment URL. A failure is
// returned as a 200 alarm panel (htmx does not swap error statuses).
func (s *Service) devicesTablePartial(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	v := r.URL.Query()
	spec := devTableSpec{Base: "/devices"}
	if v.Get("tab") == "1" && v.Get("customer") != "" {
		row, err := s.Store.GetCustomerRow(r.Context(), p.Scope, v.Get("customer"))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		spec = devTableSpec{Base: "/customers/" + row.ID + "/devices", CustomerID: row.ID, Customer: &row}
	}
	fleet, err := s.Fleet.Fleet(r.Context(), p.Scope)
	if err != nil {
		s.log().Error("ui: fleet failed", logx.Str("page", "devices-table"), logx.Err(err))
		s.renderFragmentError(w, r, "The fleet could not be loaded. This is an alarm, not an empty list.")
		return
	}
	q, bad := parseDevQuery(v)
	if len(bad) > 0 {
		http.Error(w, "Filters not valid.", http.StatusBadRequest)
		return
	}
	t, _, err := s.buildDevTable(r.Context(), p, fleet, q, spec)
	if err != nil {
		if errors.Is(err, store.ErrInvalid) {
			http.Error(w, "Filters not valid.", http.StatusBadRequest)
			return
		}
		s.log().Error("ui: device table failed", logx.Err(err))
		s.renderFragmentError(w, r, "The device table could not be read. This is an alarm, not an empty list.")
		return
	}
	w.Header().Set("HX-Push-Url", spec.hrefFor(q))
	s.render(w, r, http.StatusOK, "devices", "devices_results", t)
}

// renderFragmentError answers an htmx fragment request that failed: an alarm
// panel with a 200 status, because htmx would not swap an error status in.
func (s *Service) renderFragmentError(w http.ResponseWriter, r *http.Request, body string) {
	s.render(w, r, http.StatusOK, "devices", "panel_state", &EmptyView{Kind: "alarm", Title: "Could not load the list", Body: body + " Reload the page to retry.", Link: &Link{Href: r.Header.Get("HX-Current-URL"), Label: "Retry"}})
}
