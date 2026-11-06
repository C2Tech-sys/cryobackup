package ui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/health"
	"github.com/c2tech-sys/cryobackup/internal/console/httpx"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// Customers (S03 list, B08/S05 page with Overview, Devices and Settings tabs,
// S04 create; M2-W1C). Reads need read_only; creating and editing need admin.
// Every read goes through the principal's Scope; a customer outside it is a 404.

// ---- the list ----

// custQuery is the state of the customer list.
type custQuery struct {
	Q        string
	Health   string
	Expected string // "" any | "yes" | "no"
	Sort     string
	Desc     bool
	Page     int
	Size     int
	Archived bool
}

func parseCustQuery(v url.Values) (custQuery, []store.FieldError) {
	q := custQuery{Q: strings.TrimSpace(v.Get("q")), Health: v.Get("health"), Expected: v.Get("expected"), Sort: v.Get("sort"),
		Desc: v.Get("dir") == "desc", Page: atoiDefault(v.Get("page"), 1), Size: store.NormalPageSize(atoiDefault(v.Get("size"), 0)), Archived: v.Get("archived") == "1"}
	var bad []store.FieldError
	if q.Health != "" && !store.ValidCustomerHealth(q.Health) {
		bad = append(bad, store.FieldError{Field: "health", Message: "Unknown customer state."})
		q.Health = ""
	}
	if q.Expected != "" && q.Expected != "yes" && q.Expected != "no" {
		bad = append(bad, store.FieldError{Field: "expected", Message: "Use yes or no."})
		q.Expected = ""
	}
	if q.Sort != "" {
		known := false
		for _, k := range store.CustomerSorts {
			known = known || k == q.Sort
		}
		if !known {
			bad = append(bad, store.FieldError{Field: "sort", Message: "Unknown sort key."})
			q.Sort = ""
		}
	}
	if q.Sort == "health" && !q.Desc {
		q.Sort = ""
	}
	return q, bad
}

func (q custQuery) values() url.Values {
	v := url.Values{}
	if q.Q != "" {
		v.Set("q", q.Q)
	}
	if q.Health != "" {
		v.Set("health", q.Health)
	}
	if q.Expected != "" {
		v.Set("expected", q.Expected)
	}
	if q.Sort != "" {
		v.Set("sort", q.Sort)
	}
	if q.Desc {
		v.Set("dir", "desc")
	}
	if q.Page > 1 {
		v.Set("page", strconv.Itoa(q.Page))
	}
	if q.Size != store.DefaultPageSize {
		v.Set("size", strconv.Itoa(q.Size))
	}
	if q.Archived {
		v.Set("archived", "1")
	}
	return v
}

func (q custQuery) filtered() bool {
	return q.Q != "" || q.Health != "" || q.Expected != "" || q.Archived
}

// CustRow is one line of the customer list.
type CustRow struct {
	ID          string
	Name        string
	Slug        string
	Status      *Chip
	Extra       []Chip // SUSPENDED, ARCHIVED
	Roll        []RollChip
	Gauge       *GaugeView
	Devices     string // "31 devices"
	LastText    string
	LastSub     string
	LastClass   string
	LastGlyph   string
	Expected    bool
	Onboarding  string
	Policy      string
	Variant     string
	Aria        string
	NeedsAction bool
}

// CustFilterForm is the filter bar of the customer list.
type CustFilterForm struct {
	Q        string
	Health   []Option
	Expected []Option
	Archived bool
	Sort     string
	Desc     bool
	Size     int
}

// CustomersPage is the customer list page.
type CustomersPage struct {
	Page
	Rows      []CustRow
	Heads     []DevHead
	Pager     PagerView
	Chips     []FilterChip
	ClearHref string
	Form      CustFilterForm
	Empty     *EmptyView
	Summary   string
	CanCreate bool
	AsOf      string
	Zone      string
}

var custHeads = []struct{ Key, Label string }{{"name", "Customer"}, {"health", "State"}, {"devices", "Devices"}, {"last_success", "Last success"}, {"expected", "Expected"}}

func healthOptions(sel string) []Option {
	out := []Option{{Value: "", Label: "Any state", Selected: sel == ""}}
	for _, h := range []struct{ v, l string }{{"missing", "Missing"}, {"unknown", "Unknown"}, {"alarm", "Alarm"}, {"warn", "Warning"}, {"ok", "OK"}, {"onboarding", "Onboarding"}, {"empty", "No devices"}} {
		out = append(out, Option{Value: h.v, Label: h.l, Selected: h.v == sel})
	}
	return out
}

func expectedOptions(sel string) []Option {
	return []Option{{Value: "", Label: "Expected or not", Selected: sel == ""}, {Value: "yes", Label: "Expected", Selected: sel == "yes"}, {Value: "no", Label: "Not expected", Selected: sel == "no"}}
}

// custRow shapes one table row.
func custRow(r store.CustomerTableRow, c fmtCtx, zone string) CustRow {
	row := CustRow{ID: r.ID, Name: r.Name, Slug: r.Slug, Expected: r.Expected, Policy: r.PolicyName,
		Devices: plural(r.Devices, "device", "devices")}
	if r.PolicyName == "" {
		row.Policy = "Default"
	}
	if r.OnboardingUntil != nil {
		row.Onboarding = c.dayClock(*r.OnboardingUntil)
	}
	switch r.State {
	case "suspended":
		row.Extra = append(row.Extra, Chip{ID: "sus-" + r.ID, Label: "SUSPENDED", Class: ClassNeutral, Small: true})
	case "archived":
		row.Extra = append(row.Extra, Chip{ID: "arc-" + r.ID, Label: "ARCHIVED", Class: ClassNeutral, Small: true})
	}
	row.Aria = r.Name
	if r.InView {
		g := groupView(r.Rollup, nil, c)
		row.Status, row.Roll, row.Gauge, row.Variant = g.Status, g.Roll, g.Gauge, g.Variant
		row.Aria = g.Aria
		row.NeedsAction = r.Rollup.Status == state.CustomerMissing || r.Rollup.Status == state.CustomerUnknown || r.Rollup.Status == state.CustomerAlarm
		if r.Devices > 0 && r.Rollup.Status == state.CustomerWarn {
			row.NeedsAction = true
		}
	}
	switch {
	case !r.LastSuccess.IsZero():
		row.LastText = ageLong(c.Now.Sub(r.LastSuccess))
		if r.Devices > 0 && r.Rollup.Status == state.CustomerAlarm {
			row.LastClass, row.LastGlyph = "bad", ClassCrit
		}
	case r.Devices > 0:
		row.LastText, row.LastClass, row.LastGlyph = "never", "bad", ClassCrit
	default:
		row.LastText = "-"
	}
	return row
}

func (s *Service) customers(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	fleet, ok := s.fleetFor(w, r, p, "Customers")
	if !ok {
		return
	}
	q, bad := parseCustQuery(r.URL.Query())
	if len(bad) > 0 {
		s.badQuery(w, r, p, fleet, bad)
		return
	}
	c := s.fcAt(r.Context(), fleet.Computed)
	lq := store.CustomerListQuery{Search: q.Q, IncludeArchived: q.Archived}
	switch q.Expected {
	case "yes":
		t := true
		lq.Expected = &t
	case "no":
		f := false
		lq.Expected = &f
	}
	rows, err := s.Store.ListCustomerRows(r.Context(), p.Scope, lq)
	if err == nil {
		var tb store.CustomerTable
		tb, err = store.BuildCustomerTable(rows, fleet.View.Customers, store.CustomerTableQuery{Health: q.Health, Sort: q.Sort, Desc: q.Desc, Page: q.Page, Size: q.Size})
		if err == nil {
			s.render(w, r, http.StatusOK, "customers", "base", s.customersPage(r.Context(), p, fleet, c, q, tb))
			return
		}
	}
	s.log().Error("ui: customer list failed", logx.Err(err))
	s.render(w, r, http.StatusServiceUnavailable, "error", "base", errorData(s.page(r.Context(), p, fleet, "Customers unavailable", "customers", ""), 503, "Customers unavailable",
		"The customer list could not be read. This is an alarm, not an empty list. Try again shortly."))
}

func (s *Service) customersPage(ctx context.Context, p *auth.Principal, fleet health.Fleet, c fmtCtx, q custQuery, tb store.CustomerTable) CustomersPage {
	q.Page, q.Size = tb.Page.Page, tb.Page.Size
	pg := CustomersPage{Page: s.page(ctx, p, fleet, "Customers", "customers", "/ui/events"), CanCreate: p.Role >= auth.RoleAdmin, AsOf: c.clock(fleet.Computed), Zone: c.Zone}
	href := func(mod func(*custQuery)) string {
		m := q
		mod(&m)
		return listURL("/customers", m.values())
	}
	for _, h := range custHeads {
		head := DevHead{Key: h.Key, Label: h.Label, Sortable: true, Sort: "none"}
		cur := q.Sort
		if cur == "" {
			cur = "health"
		}
		nextDesc := false
		if cur == h.Key {
			head.Sort = "ascending"
			if q.Desc {
				head.Sort = "descending"
			}
			nextDesc = !q.Desc
		}
		key := h.Key
		head.Href = href(func(m *custQuery) {
			m.Sort, m.Desc, m.Page = key, nextDesc, 1
			if key == "health" && !nextDesc {
				m.Sort = ""
			}
		})
		pg.Heads = append(pg.Heads, head)
	}
	for _, r := range tb.Rows {
		pg.Rows = append(pg.Rows, custRow(r, c, c.Zone))
	}
	pg.Pager = buildPager(tb.Page, "customers",
		func(page, size int) string { return href(func(m *custQuery) { m.Page, m.Size = page, size }) }, nil)
	pg.Summary = pg.Pager.Text
	pg.Form = CustFilterForm{Q: q.Q, Health: healthOptions(q.Health), Expected: expectedOptions(q.Expected), Archived: q.Archived, Sort: q.Sort, Desc: q.Desc, Size: q.Size}
	addChip := func(label string, mod func(*custQuery)) {
		pg.Chips = append(pg.Chips, FilterChip{Label: label, Href: href(mod)})
	}
	if q.Q != "" {
		addChip(`Text: "`+q.Q+`"`, func(m *custQuery) { m.Q, m.Page = "", 1 })
	}
	if q.Health != "" {
		addChip("State: "+q.Health, func(m *custQuery) { m.Health, m.Page = "", 1 })
	}
	if q.Expected != "" {
		lbl := "Expected"
		if q.Expected == "no" {
			lbl = "Not expected"
		}
		addChip(lbl, func(m *custQuery) { m.Expected, m.Page = "", 1 })
	}
	if q.Archived {
		addChip("Including archived", func(m *custQuery) { m.Archived, m.Page = false, 1 })
	}
	pg.ClearHref = href(func(m *custQuery) { *m = custQuery{Size: m.Size, Sort: m.Sort, Desc: m.Desc, Page: 1} })
	if len(tb.Rows) == 0 {
		switch {
		case tb.Total > 0:
			pg.Empty = &EmptyView{Kind: "neutral", Title: "No customers match these filters", Body: fmt.Sprintf("0 of %d shown.", tb.Total), Link: &Link{Href: pg.ClearHref, Label: "Clear filters"}}
		case q.filtered():
			pg.Empty = &EmptyView{Kind: "neutral", Title: "No customers match these filters", Body: "0 of 0 shown. There are no customers in this view at all.", Link: &Link{Href: pg.ClearHref, Label: "Clear filters"}}
		case fleet.View.FirstRun:
			pg.Empty = &EmptyView{Kind: "neutral", Title: "No customers yet", Body: "Add your first customer, then enroll its devices."}
			if pg.CanCreate {
				pg.Empty.Link = &Link{Href: "/ui/customers/new", Label: "Add a customer"}
			}
		default:
			pg.Empty = &EmptyView{Kind: "alarm", Title: "No customers are visible to this account",
				Body: "Customers exist but none is inside this account's scope, or they were all archived. An account with nothing to watch is a misconfiguration, not a quiet fleet."}
		}
	}
	return pg
}

// ---- the customer page ----

// CustFact is a line of the customer's facts.
type CustFact struct {
	Key   string
	Value string
	Mono  bool
	Chip  *Chip
	Class string
	Glyph string
	Sub   string
}

// AttnRow is one device on the Overview's "needs attention" list.
type AttnRow struct {
	ID       string
	Host     string
	Meta     string
	Chips    []Chip
	Age      string
	AgeCls   string
	AgeGlyph string
}

// CustOverview is the Overview tab.
type CustOverview struct {
	Facts      []CustFact
	Contacts   []store.Contact
	Attention  []AttnRow
	AttnTotal  int
	Checked    string // evidence line when nothing needs attention
	Empty      *EmptyView
	DevicesURL string
}

// CustTab is one tab of the customer page.
type CustTab struct {
	Key     string
	Label   string
	Href    string
	Current bool
}

// CustHead is the customer page header.
type CustHead struct {
	ID       string
	Name     string
	Status   *Chip
	Extra    []Chip
	Roll     []RollChip
	Gauge    *GaugeView
	Sub      []string
	Cause    *CauseView
	Tabs     []CustTab
	CanEdit  bool
	Expected bool
}

// CustomerPage is the customer page in any of its tabs.
type CustomerPage struct {
	Page
	H        CustHead
	Tab      string
	Overview *CustOverview
	Table    *DevTable
	Form     *CustForm
	Policy   *CustPolicyView // M2-W2A: the Policy tab (policy_customer.go)
	Saved    string
}

// custTabs lists the customer page's tabs. The Policy tab (S17, admin) is shown
// when canPolicy is true.
func custTabs(id, cur string, canPolicy bool) []CustTab {
	out := []CustTab{{Key: "overview", Label: "Overview", Href: "/customers/" + id}, {Key: "devices", Label: "Devices", Href: "/customers/" + id + "/devices"},
		{Key: "runs", Label: "Runs", Href: "/customers/" + id + "/runs"}, // M3-T4 (S16, m3t4_runs.go)
		{Key: "settings", Label: "Settings", Href: "/customers/" + id + "/settings"}}
	if canPolicy {
		out = append(out, CustTab{Key: "policy", Label: "Policy", Href: "/customers/" + id + "/policy"})
	}
	for i := range out {
		out[i].Current = out[i].Key == cur
	}
	return out
}

// custHead builds the page header from the customer row and the engine's rollup.
func (s *Service) custHead(p *auth.Principal, row store.CustomerRow, cs *state.CustomerState, c fmtCtx, tab string) CustHead {
	h := CustHead{ID: row.ID, Name: row.Name, CanEdit: p.Role >= auth.RoleAdmin, Expected: row.Expected, Tabs: custTabs(row.ID, tab, p.Role >= auth.RoleAdmin)}
	sub := []string{row.Slug}
	if row.DisplayTZ != "" {
		sub = append(sub, row.DisplayTZ)
	}
	pol := row.PolicyName
	if pol == "" {
		pol = "default policy"
	}
	sub = append(sub, pol, plural(row.Devices, "device", "devices"))
	h.Sub = sub
	switch row.State {
	case "suspended":
		h.Extra = append(h.Extra, Chip{ID: "sus-" + row.ID, Label: "SUSPENDED", Class: ClassNeutral, Small: true, Title: "SUSPENDED", Reason: "Notifications are muted; backups continue."})
	case "archived":
		h.Extra = append(h.Extra, Chip{ID: "arc-" + row.ID, Label: "ARCHIVED", Class: ClassNeutral, Small: true})
	}
	if cs == nil {
		return h
	}
	g := groupView(*cs, nil, c)
	h.Status, h.Roll, h.Gauge = g.Status, g.Roll, g.Gauge
	switch cs.Status {
	case state.CustomerMissing:
		h.Cause = &CauseView{Class: ClassCrit, Title: "MISSING.", Text: row.Name + " is marked expected but has no active devices: every machine there is unmonitored."}
	case state.CustomerUnknown:
		h.Cause = &CauseView{Class: ClassCrit, Title: "UNKNOWN.", Text: "At least one device here could not be classified. This is an alarm, not a quiet customer."}
	}
	if sl, n, is := health.Silence(*cs); is {
		h.Cause = &CauseView{Class: ClassCrit, Title: "CUSTOMER-WIDE SILENCE.", Text: fmt.Sprintf("%d of %d devices have gone silent at once.", sl, n)}
	}
	return h
}

func (s *Service) customerRow(w http.ResponseWriter, r *http.Request, p *auth.Principal) (store.CustomerRow, bool) {
	row, err := s.Store.GetCustomerRow(r.Context(), p.Scope, r.PathValue("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.notFound(w, r, p, "This customer")
			return store.CustomerRow{}, false
		}
		s.log().Error("ui: customer read failed", logx.Err(err))
		s.errorPage(w, r, p, http.StatusServiceUnavailable, "Customer unavailable", "The customer could not be read. Try again shortly.")
		return store.CustomerRow{}, false
	}
	return row, true
}

func rollupOf(fleet health.Fleet, id string) *state.CustomerState {
	for i := range fleet.View.Customers {
		if fleet.View.Customers[i].ID == id {
			return &fleet.View.Customers[i]
		}
	}
	return nil
}

func savedText(code string) string {
	switch code {
	case "created":
		return "Customer created."
	case "1":
		return "Changes saved."
	}
	return ""
}

func (s *Service) customerOverview(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	fleet, ok := s.fleetFor(w, r, p, "Customer")
	if !ok {
		return
	}
	row, ok := s.customerRow(w, r, p)
	if !ok {
		return
	}
	c := s.fcAt(r.Context(), fleet.Computed)
	cs := rollupOf(fleet, row.ID)
	pg := CustomerPage{Page: s.page(r.Context(), p, fleet, row.Name, "customers", "/ui/events"), H: s.custHead(p, row, cs, c, "overview"), Tab: "overview", Saved: savedText(r.URL.Query().Get("saved"))}
	pg.Overview = s.overview(r.Context(), p, fleet, row, cs, c)
	s.render(w, r, http.StatusOK, "customer", "base", pg)
}

func (s *Service) overview(ctx context.Context, p *auth.Principal, fleet health.Fleet, row store.CustomerRow, cs *state.CustomerState, c fmtCtx) *CustOverview {
	o := &CustOverview{Contacts: store.ParseContacts(row.Contacts), DevicesURL: "/customers/" + row.ID + "/devices"}
	o.Facts = append(o.Facts, CustFact{Key: "Devices", Value: plural(row.Devices, "device", "devices") + func() string {
		if row.Retired > 0 {
			return fmt.Sprintf(" · %d retired", row.Retired)
		}
		return ""
	}()})
	if cs != nil {
		o.Facts[0].Value = fmt.Sprintf("%s · %d active · %d parked", plural(row.Devices, "device", "devices"), cs.Active, cs.Paused)
		if cs.HealthKnown {
			o.Facts = append(o.Facts, CustFact{Key: "Health", Value: fmt.Sprintf("%d%% of active devices within SLA", int(cs.HealthPct+0.5))})
		} else {
			o.Facts = append(o.Facts, CustFact{Key: "Health", Value: "n/a: no active devices to measure"})
		}
	}
	switch {
	case !latestSuccess(cs).IsZero():
		o.Facts = append(o.Facts, CustFact{Key: "Last success", Value: ageLong(c.Now.Sub(latestSuccess(cs))), Sub: c.dateClock(latestSuccess(cs)) + " (" + s.zone(ctx) + ")"})
	case row.Devices > 0:
		o.Facts = append(o.Facts, CustFact{Key: "Last success", Value: "never", Class: "bad", Glyph: ClassCrit})
	default:
		o.Facts = append(o.Facts, CustFact{Key: "Last success", Value: "-"})
	}
	exp := CustFact{Key: "Expected"}
	switch {
	case row.Expected && row.Devices == 0:
		exp.Value, exp.Class, exp.Glyph = "Yes, and it has no devices", "bad", ClassCrit
	case row.Expected:
		exp.Value = "Yes: zero active devices would raise MISSING"
	default:
		exp.Value = "No: an empty customer is not an alarm"
	}
	o.Facts = append(o.Facts, exp)
	if row.OnboardingUntil != nil {
		o.Facts = append(o.Facts, CustFact{Key: "Onboarding until", Value: c.dayClock(*row.OnboardingUntil) + " (" + s.zone(ctx) + ")", Mono: true})
	}
	pol := row.PolicyName
	if pol == "" {
		pol = "Default policy"
	}
	o.Facts = append(o.Facts, CustFact{Key: "Policy", Value: pol})
	if row.DeviceCap != nil {
		o.Facts = append(o.Facts, CustFact{Key: "Device cap", Value: fmt.Sprintf("%d of %d used", row.Devices, *row.DeviceCap), Mono: true})
	}
	custody := "Escrowed with the console"
	if row.KeyCustody == "customer_held" {
		custody = "Held by the customer"
	}
	o.Facts = append(o.Facts, CustFact{Key: "Key custody", Value: custody})
	if row.DisplayTZ != "" {
		o.Facts = append(o.Facts, CustFact{Key: "Display zone", Value: row.DisplayTZ, Mono: true})
	}
	if row.Notes != "" {
		o.Facts = append(o.Facts, CustFact{Key: "Notes", Value: row.Notes})
	}
	// devices needing attention: the engine's exceptions-first order
	if cs != nil {
		for _, d := range cs.Devices {
			if d.Primary == state.StateRetired || (d.Alarm == state.SeverityNone && d.Primary != state.StateUnknown) {
				continue
			}
			o.AttnTotal++
			if len(o.Attention) >= 8 {
				continue
			}
			ar := AttnRow{ID: d.ID, Host: d.Hostname, Chips: ChipsFor(d, c, RunInfo{})}
			switch {
			case !d.Detail.LastSuccess.IsZero():
				ar.Age = ageLong(c.Now.Sub(d.Detail.LastSuccess))
			default:
				ar.Age = "never"
			}
			if ClassOf(d.Primary) == ClassCrit {
				ar.AgeCls, ar.AgeGlyph = "bad", ClassCrit
			} else {
				ar.AgeCls, ar.AgeGlyph = "warn", ClassWarn
			}
			o.Attention = append(o.Attention, ar)
		}
	}
	switch {
	case row.Devices == 0:
		o.Empty = s.customerNoDevices(row, c)
	case o.AttnTotal == 0:
		o.Checked = fmt.Sprintf("Nothing needs attention. %s checked at %s (%s).", plural(row.Devices, "device", "devices"), c.clock(fleet.Computed), s.zone(ctx))
	}
	return o
}

func latestSuccess(cs *state.CustomerState) time.Time {
	var t time.Time
	if cs == nil {
		return t
	}
	for _, d := range cs.Devices {
		if d.Detail.LastSuccess.After(t) {
			t = d.Detail.LastSuccess
		}
	}
	return t
}

// customerNoDevices classifies a customer with no devices: expected is an alarm
// (unless inside its onboarding window), not expected is neutral.
func (s *Service) customerNoDevices(row store.CustomerRow, c fmtCtx) *EmptyView {
	switch {
	case row.Expected && row.OnboardingUntil != nil && row.OnboardingUntil.After(c.Now):
		return &EmptyView{Kind: "neutral", Title: "Onboarding: no devices yet",
			Body: "This customer is expected and has until " + c.dayClock(*row.OnboardingUntil) + " (" + c.Zone + ") to enroll its first device."}
	case row.Expected:
		return &EmptyView{Kind: "alarm", Title: "No devices are enrolled for an expected customer",
			Body: row.Name + " is marked expected but has no active devices, so every machine there is unmonitored. Enroll a device or mark the customer not expected in Settings."}
	}
	return &EmptyView{Kind: "neutral", Title: "No devices yet", Body: row.Name + " is not marked expected and has no devices. Enroll one to start backing it up."}
}

func (s *Service) customerDevices(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	fleet, ok := s.fleetFor(w, r, p, "Customer")
	if !ok {
		return
	}
	row, ok := s.customerRow(w, r, p)
	if !ok {
		return
	}
	q, bad := parseDevQuery(r.URL.Query())
	if len(bad) > 0 {
		s.badQuery(w, r, p, fleet, bad)
		return
	}
	c := s.fcAt(r.Context(), fleet.Computed)
	t, status, err := s.buildDevTable(r.Context(), p, fleet, q, devTableSpec{Base: "/customers/" + row.ID + "/devices", CustomerID: row.ID, Customer: &row})
	if err != nil {
		s.tableFailed(w, r, p, fleet, status, err)
		return
	}
	pg := CustomerPage{Page: s.page(r.Context(), p, fleet, row.Name, "customers", "/ui/events"), H: s.custHead(p, row, rollupOf(fleet, row.ID), c, "devices"), Tab: "devices", Table: &t}
	s.render(w, r, http.StatusOK, "customer", "base", pg)
}

// ---- mutations ----

func (s *Service) actor(r *http.Request, p *auth.Principal) store.AuditActor {
	return store.AuditActor{AdminID: p.Admin.ID, Username: p.Admin.Username, IP: httpx.ClientIP(r), Method: "web"}
}

// storeFailed answers a failed mutation that is not a form error.
func (s *Service) storeFailed(w http.ResponseWriter, r *http.Request, p *auth.Principal, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.notFound(w, r, p, "This item")
	case errors.Is(err, store.ErrScope):
		s.errorPage(w, r, p, http.StatusForbidden, "Not permitted", "This account's scope does not allow that change.")
	case errors.Is(err, store.ErrConflict):
		s.errorPage(w, r, p, http.StatusConflict, "Cannot change this", "The item is in a state that does not allow this change (archived or retired).")
	case errors.Is(err, store.ErrAudit):
		s.log().Error("ui: change refused, not audited", logx.Class("audit"), logx.Err(err))
		s.errorPage(w, r, p, http.StatusInternalServerError, "Not saved",
			"The change was not saved because its audit record could not be written (NOT AUDITED). Nothing was changed. Check the console database and try again.")
	default:
		s.log().Error("ui: change failed", logx.Err(err))
		s.errorPage(w, r, p, http.StatusInternalServerError, "Not saved", "The change could not be saved. Nothing was changed. Try again shortly.")
	}
}

func (s *Service) changed() {
	if s.Hub != nil {
		s.Hub.DeviceChanged("")
	}
}
