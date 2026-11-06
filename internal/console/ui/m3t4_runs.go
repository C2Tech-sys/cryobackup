package ui

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminapi"
	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/health"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
)

// Runs search (S21), the customer Runs tab (S16) and run detail (S22), M3-T4.
//
// The search is the flat clause builder of components 5.4: a list of
// conditions, each one a field and a value. Different fields narrow (AND);
// repeating a field widens (any of its values), which is what the store's run
// search does. The whole state lives in the query string (status, kind,
// customer, device, from, to, page, size), so every view is a link.
//
// Run pages are path-free by construction: rows and details carry counts,
// classes and ids, and every free-text-looking field passes safeWord.

// runQuery is the parsed state of a run search.
type runQuery struct {
	Clauses []runClause
	// TZ is the zone the from and to dates are read in; "" is the operator's display zone.
	TZ   string
	Page int
	Size int
}

// runClause is one condition. For from and to the value may be "" (a condition
// that was just added and has no date yet: it filters nothing and says so).
type runClause struct {
	Field string
	Value string
}

// The condition fields, in the order their rows are listed.
var runFields = []struct{ Param, Label, Op string }{
	{"status", "Outcome", "is"}, {"kind", "Kind", "is"}, {"customer", "Customer", "is"}, {"device", "Device", "is"},
	{"from", "Started", "on or after"}, {"to", "Started", "on or before"},
}

const maxClauseValues = 50

func runFieldOf(param string) (label, op string, ok bool) {
	for _, f := range runFields {
		if f.Param == param {
			return f.Label, f.Op, true
		}
	}
	return "", "", false
}

const dateLayout = "2006-01-02"

// parseRunQuery reads the query string. An unknown outcome or kind, a bad id, a
// bad date or an unknown zone is a field error (a 400 page); a range whose end
// precedes its start is not: the page says so on the clause.
func parseRunQuery(v url.Values) (runQuery, []store.FieldError) {
	q := runQuery{Page: atoiDefault(v.Get("page"), 1), Size: store.NormalPageSize(atoiDefault(v.Get("size"), 0))}
	var bad []store.FieldError
	flag := func(field, msg string) { bad = append(bad, store.FieldError{Field: field, Message: msg}) }
	seen := map[string]bool{}
	count := map[string]int{}
	add := func(field, val string) {
		switch field {
		case "status":
			if !containsStr(store.RunOutcomes, val) {
				flag("status", "Unknown outcome.")
				return
			}
		case "kind":
			if !containsStr(store.RunKinds, val) {
				flag("kind", "Unknown kind.")
				return
			}
		case "customer", "device":
			if !idParam(val) {
				flag(field, "Choose one of the listed values.")
				return
			}
		case "from", "to":
			if val != "" {
				if _, err := time.Parse(dateLayout, val); err != nil {
					flag(field, "Use a date as YYYY-MM-DD.")
					return
				}
			}
			if count[field] > 0 {
				return // one date per end of the range
			}
		}
		if key := field + "|" + val; seen[key] {
			return
		} else {
			seen[key] = true
		}
		if count[field] >= maxClauseValues {
			flag(field, "Use at most 50 values.")
			return
		}
		count[field]++
		q.Clauses = append(q.Clauses, runClause{Field: field, Value: val})
	}
	for _, f := range runFields {
		for _, val := range v[f.Param] {
			val = strings.TrimSpace(val)
			if val == "" && f.Param != "from" && f.Param != "to" {
				continue
			}
			add(f.Param, val)
		}
	}
	// "add" is the Add-condition control: "field:value", or "from:" and "to:" for a date with no value yet.
	if a := v.Get("add"); a != "" {
		if field, val, ok := strings.Cut(a, ":"); ok {
			if _, _, known := runFieldOf(field); known {
				add(field, val)
			}
		}
	}
	if tz := strings.TrimSpace(v.Get("tz")); tz != "" {
		if _, err := time.LoadLocation(tz); err != nil || len(tz) > 64 {
			flag("tz", "Unknown time zone.")
		} else {
			q.TZ = tz
		}
	}
	return q, bad
}

func (q runQuery) values() url.Values {
	out := url.Values{}
	for _, c := range q.Clauses {
		out.Add(c.Field, c.Value)
	}
	if q.TZ != "" {
		out.Set("tz", q.TZ)
	}
	if q.Page > 1 {
		out.Set("page", strconv.Itoa(q.Page))
	}
	if q.Size != store.DefaultPageSize {
		out.Set("size", strconv.Itoa(q.Size))
	}
	return out
}

// without returns q minus the clause at index i, back on page 1.
func (q runQuery) without(i int) runQuery {
	out := q
	out.Clauses = append(append([]runClause(nil), q.Clauses[:i]...), q.Clauses[i+1:]...)
	out.Page = 1
	return out
}

func (q runQuery) of(field string) []string {
	var out []string
	for _, c := range q.Clauses {
		if c.Field == field && c.Value != "" {
			out = append(out, c.Value)
		}
	}
	return out
}

func (q runQuery) date(field string) string {
	for _, c := range q.Clauses {
		if c.Field == field {
			return c.Value
		}
	}
	return ""
}

// zoneOf is the zone q's dates are read in.
func (q runQuery) zoneOf(display *time.Location) *time.Location {
	if q.TZ != "" {
		if l, err := time.LoadLocation(q.TZ); err == nil {
			return l
		}
	}
	return display
}

// rangeProblem is the sentence for a range that cannot match, or "".
func (q runQuery) rangeProblem() string {
	from, to := q.date("from"), q.date("to")
	if from != "" && to != "" && to < from {
		return "The end of the range is before its start."
	}
	return ""
}

// apiValues turns the clauses into the query of GET /api/v1/runs. The range
// is [from 00:00, to + 1 day 00:00) in the zone q reads dates in.
func (q runQuery) apiValues(loc *time.Location) url.Values {
	out := url.Values{}
	for _, f := range []string{"status", "kind", "customer", "device"} {
		for _, v := range q.of(f) {
			out.Add(f, v)
		}
	}
	zl := q.zoneOf(loc)
	if d := q.date("from"); d != "" {
		if t, err := time.ParseInLocation(dateLayout, d, zl); err == nil {
			out.Set("since", t.UTC().Format(time.RFC3339))
		}
	}
	if d := q.date("to"); d != "" {
		if t, err := time.ParseInLocation(dateLayout, d, zl); err == nil {
			out.Set("until", t.AddDate(0, 0, 1).UTC().Format(time.RFC3339))
		}
	}
	return out
}

// ---- view models ----

// ClauseView is one condition row of the builder.
type ClauseView struct {
	N          int
	Param      string
	FieldLabel string
	Op         string
	Options    []Option // an enumerated value; nil for a date
	Date       bool
	Value      string
	Err        string
	RemoveHref string
}

// AddGroup is one optgroup of the Add-condition select.
type AddGroup struct {
	Label   string
	Options []Option
}

// ClassCount is an error class and the number of files it affected.
type ClassCount struct {
	Class string
	N     int
}

// RunListRow is one line of the run list.
type RunListRow struct {
	ID         string
	Href       string
	Outcome    Chip
	Started    string
	StartedISO string
	Duration   string
	Host       string
	DeviceID   string
	Customer   string
	CustomerID string
	Kind       string
	Trigger    string
	Classes    []ClassCount
	Note       string // attempt, deferred reason
	Aria       string
}

// RunListView is the run search: the builder, the result table and the pager.
type RunListView struct {
	BaseURL    string
	PartialURL string
	PinID      string // the customer of the S16 tab: no customer condition
	Clauses    []ClauseView
	Add        []AddGroup
	Summary    string // the conditions as a sentence
	Count      string // "Matches 7 of 212 runs."
	Problem    string // the conditions cannot match; no query was made
	TZNote     string
	TZ         string
	Rows       []RunListRow
	Pager      PagerView
	ClearHref  string
	Empty      *EmptyView
	Warn       *CauseView
	ShowCust   bool
	AsOf       string
	Zone       string
	ListWarn   string // the select lists could not be loaded in full
}

// runsSpec says where a run list lives: /runs (S21), or one customer's tab (S16).
type runsSpec struct {
	Base string
	Pin  string // customer id of the tab
	Row  *store.CustomerRow
}

func (sp runsSpec) hrefFor(q runQuery) string { return listURL(sp.Base, q.values()) }

func (sp runsSpec) partialFor(q runQuery) string {
	v := q.values()
	if sp.Pin != "" {
		v.Set("pin", sp.Pin)
	}
	return listURL("/ui/runs/table", v)
}

// runsLists reads the select lists of the builder: customers and devices
// inside the caller's scope (only the pinned customer's devices on its tab).
func (s *Service) runsLists(ctx context.Context, p *auth.Principal, spec runsSpec, q runQuery) (custs []Option, custLabel map[string]string, devs []store.Device, partial bool) {
	custLabel = map[string]string{}
	if spec.Pin == "" {
		cs, err := s.Store.ListCustomers(ctx, p.Scope)
		if err != nil {
			partial = true
		}
		for _, c := range cs {
			custs = append(custs, Option{Value: c.ID, Label: c.Name})
			custLabel[c.ID] = c.Name
		}
	}
	l, err := s.Store.ListDevices(ctx, p.Scope, store.ListDevicesOptions{CustomerID: spec.Pin, IncludeRetired: true, Page: store.Page{Limit: 1000}})
	if err != nil || l.NextCursor != "" {
		partial = true
	}
	return custs, custLabel, l.Items, partial
}

func runOutcomeLabel(v string) string {
	if v == store.RunOutcomeRunning {
		return "Running"
	}
	return strings.ToUpper(v[:1]) + v[1:]
}

// buildRunsView reads the run search through the admin API and shapes it.
func (s *Service) buildRunsView(r *http.Request, p *auth.Principal, fleet health.Fleet, q runQuery, spec runsSpec) RunListView {
	ctx := r.Context()
	c := s.fcAt(ctx, fleet.Computed)
	loc := s.loc(ctx)
	v := RunListView{BaseURL: spec.Base, PartialURL: spec.partialFor(q), PinID: spec.Pin, ShowCust: spec.Pin == "", AsOf: c.clock(fleet.Computed), Zone: c.Zone}
	v.ClearHref = spec.hrefFor(runQuery{Size: q.Size, TZ: q.TZ})
	zl := q.zoneOf(loc)
	zoneName := zl.String()
	v.TZ = q.TZ
	v.TZNote = "Dates are read in " + zoneName + "."

	custs, custLabel, devs, partial := s.runsLists(ctx, p, spec, q)
	if partial {
		v.ListWarn = "The customer or device lists could not be loaded in full, so some choices are missing. Conditions already set still apply."
	}
	hostOf := map[string]string{}
	for _, d := range devs {
		hostOf[d.ID] = d.Hostname
	}
	// condition rows
	n := 0
	for i, cl := range q.Clauses {
		label, op, _ := runFieldOf(cl.Field)
		n++
		cv := ClauseView{N: n, Param: cl.Field, FieldLabel: label, Op: op, Value: cl.Value, RemoveHref: spec.hrefFor(q.without(i))}
		switch cl.Field {
		case "status":
			cv.Options = []Option{}
			for _, o := range store.RunOutcomes {
				cv.Options = append(cv.Options, Option{Value: o, Label: runOutcomeLabel(o), Selected: o == cl.Value})
			}
		case "kind":
			for _, k := range store.RunKinds {
				cv.Options = append(cv.Options, Option{Value: k, Label: strings.ToUpper(k[:1]) + k[1:], Selected: k == cl.Value})
			}
		case "customer":
			cv.Options = optionsWith(custs, cl.Value, custLabel[cl.Value] == "")
		case "device":
			for _, d := range devs {
				cv.Options = append(cv.Options, Option{Value: d.ID, Label: d.Hostname, Selected: d.ID == cl.Value})
			}
			if hostOf[cl.Value] == "" {
				cv.Options = append([]Option{{Value: cl.Value, Label: "(not found)", Selected: true}}, cv.Options...)
			}
		default:
			cv.Date = true
			if cl.Value == "" {
				cv.Err = "Choose a date: this condition does nothing until you do."
			}
		}
		v.Clauses = append(v.Clauses, cv)
	}
	if msg := q.rangeProblem(); msg != "" {
		v.Problem = msg
		for i := range v.Clauses {
			if v.Clauses[i].Param == "to" {
				v.Clauses[i].Err = msg
			}
		}
	}
	// the Add-condition control
	g := func(label, field string, opts []Option) {
		if len(opts) == 0 {
			return
		}
		ag := AddGroup{Label: label}
		for _, o := range opts {
			ag.Options = append(ag.Options, Option{Value: field + ":" + o.Value, Label: o.Label})
		}
		v.Add = append(v.Add, ag)
	}
	var outs, kinds, devOpts []Option
	for _, o := range store.RunOutcomes {
		outs = append(outs, Option{Value: o, Label: runOutcomeLabel(o)})
	}
	for _, k := range store.RunKinds {
		kinds = append(kinds, Option{Value: k, Label: strings.ToUpper(k[:1]) + k[1:]})
	}
	for _, d := range devs {
		devOpts = append(devOpts, Option{Value: d.ID, Label: d.Hostname})
	}
	g("Outcome", "status", outs)
	g("Kind", "kind", kinds)
	if spec.Pin == "" {
		g("Customer", "customer", custs)
	}
	g("Device", "device", devOpts)
	if !hasClause(q, "from") {
		v.Add = append(v.Add, AddGroup{Label: "Date", Options: []Option{{Value: "from:", Label: "Started on or after..."}}})
	}
	if !hasClause(q, "to") {
		if len(v.Add) > 0 && v.Add[len(v.Add)-1].Label == "Date" {
			v.Add[len(v.Add)-1].Options = append(v.Add[len(v.Add)-1].Options, Option{Value: "to:", Label: "Started on or before..."})
		} else {
			v.Add = append(v.Add, AddGroup{Label: "Date", Options: []Option{{Value: "to:", Label: "Started on or before..."}}})
		}
	}
	v.Summary = s.runSentence(q, custLabel, hostOf, spec, c)
	if v.Problem != "" {
		v.Empty = &EmptyView{Kind: "partial", Title: "These conditions cannot match any run", Body: v.Problem + " Fix the dates above, or clear them.", Link: &Link{Href: v.ClearHref, Label: "Clear conditions"}}
		v.Count = "No query was made."
		v.Pager = buildPager(m3PageInfo(0, 1, q.Size), "runs", func(int, int) string { return "" }, nil)
		return v
	}
	// the query
	size := q.Size
	page := q.Page
	var resp adminapi.RunListResponse
	fetch := func(pg int) error {
		av := q.apiValues(loc)
		if spec.Pin != "" {
			av.Set("customer", spec.Pin)
		}
		av.Set("limit", strconv.Itoa(size))
		if pg > 1 {
			av.Set("cursor", "o"+strconv.Itoa((pg-1)*size))
		}
		resp = adminapi.RunListResponse{}
		return s.apiGet(r, "/api/v1/runs?"+av.Encode(), &resp)
	}
	clamped := false
	err := fetch(page)
	if err == nil {
		info := m3PageInfo(resp.Total, page, size)
		if info.Clamped && resp.Total > 0 { // a page past the end: show the last one, and say so
			page, clamped = info.Page, true
			err = fetch(page)
		}
	}
	if err != nil {
		s.log().Error("ui: run search failed", logx.Class("render"), logx.Err(err))
		v.Empty = apiFailure("the run list", err)
		v.Count = "The run list could not be read."
		v.Pager = buildPager(m3PageInfo(0, 1, size), "runs", func(int, int) string { return "" }, nil)
		return v
	}
	info := m3PageInfo(resp.Total, page, size)
	info.Clamped = info.Clamped || clamped
	for _, rr := range resp.Runs {
		v.Rows = append(v.Rows, runListRow(rr, c, zoneName))
	}
	qq := q
	v.Pager = buildPager(info, "runs",
		func(pg, sz int) string { m := qq; m.Page, m.Size = pg, sz; return spec.hrefFor(m) },
		func(pg, sz int) string { m := qq; m.Page, m.Size = pg, sz; return spec.partialFor(m) })
	// Of is every run in the caller's scope; on a customer's tab the whole is that customer's runs.
	of := resp.Of
	if spec.Pin != "" {
		of = resp.Total
		if len(q.Clauses) > 0 {
			var base adminapi.RunListResponse
			if s.apiGet(r, "/api/v1/runs?limit=1&customer="+url.QueryEscape(spec.Pin), &base) == nil {
				of = base.Total
			}
		}
	}
	resp.Of = of
	v.Count = fmt.Sprintf("Matches %d of %s.", resp.Total, plural(of, "run", "runs"))
	if len(q.Clauses) == 0 {
		v.Count = fmt.Sprintf("No conditions: showing all %s.", plural(of, "run", "runs"))
	}
	if !resp.Truncated && resp.Total != len(v.Rows) && info.Pages == 1 {
		v.Warn = &CauseView{Class: ClassWarn, Title: "Partial answer.", Text: "The console returned fewer runs than it counted."}
	}
	if len(v.Rows) == 0 {
		v.Empty = s.runsEmpty(q, resp, fleet, spec, v.ClearHref, c)
	}
	return v
}

func hasClause(q runQuery, field string) bool {
	for _, c := range q.Clauses {
		if c.Field == field {
			return true
		}
	}
	return false
}

// optionsWith is opts with value selected; when value is not among them (a
// customer outside the account's scope) a "(not found)" entry stands in for it
// so the row never silently shows a different customer.
func optionsWith(opts []Option, value string, missing bool) []Option {
	out := make([]Option, 0, len(opts)+1)
	if missing {
		out = append(out, Option{Value: value, Label: "(not found)", Selected: true})
	}
	for _, o := range opts {
		out = append(out, Option{Value: o.Value, Label: o.Label, Selected: o.Value == value})
	}
	return out
}

// runSentence reads the conditions aloud: "Runs where outcome is failed or
// partial, and customer is X, started on or after Sun 18 Oct 2026."
func (s *Service) runSentence(q runQuery, custLabel, hostOf map[string]string, spec runsSpec, c fmtCtx) string {
	var parts []string
	join := func(vals []string, label func(string) string) string {
		out := make([]string, len(vals))
		for i, x := range vals {
			out[i] = label(x)
		}
		return strings.Join(out, " or ")
	}
	if vals := q.of("status"); len(vals) > 0 {
		parts = append(parts, "outcome is "+join(vals, func(x string) string { return strings.ToLower(runOutcomeLabel(x)) }))
	}
	if vals := q.of("kind"); len(vals) > 0 {
		parts = append(parts, "kind is "+join(vals, func(x string) string { return x }))
	}
	if vals := q.of("customer"); len(vals) > 0 {
		parts = append(parts, "customer is "+join(vals, func(x string) string {
			if n := custLabel[x]; n != "" {
				return n
			}
			return "an unknown customer"
		}))
	}
	if vals := q.of("device"); len(vals) > 0 {
		parts = append(parts, "device is "+join(vals, func(x string) string {
			if n := hostOf[x]; n != "" {
				return n
			}
			return "an unknown device"
		}))
	}
	day := func(d string) string {
		if t, err := time.Parse(dateLayout, d); err == nil {
			return t.Format("Mon 2 Jan 2006")
		}
		return ""
	}
	if d := day(q.date("from")); d != "" {
		parts = append(parts, "it started on or after "+d)
	}
	if d := day(q.date("to")); d != "" {
		parts = append(parts, "it started on or before "+d)
	}
	scope := "Runs"
	if spec.Row != nil {
		scope = "Runs of " + spec.Row.Name
	}
	if len(parts) == 0 {
		if spec.Row != nil {
			return scope + "."
		}
		return ""
	}
	return scope + " where " + strings.Join(parts, ", and ") + "."
}

func runListRow(rr adminapi.RunRowJSON, c fmtCtx, zoneName string) RunListRow {
	row := RunListRow{ID: rr.ID, Href: "/runs/" + rr.ID, Outcome: OutcomeChip("o-"+rr.ID, rr.Outcome, false), Started: c.dayClock(rr.StartedAt),
		StartedISO: rr.StartedAt.UTC().Format(time.RFC3339), Host: rr.Hostname, DeviceID: rr.DeviceID, Customer: rr.Customer, CustomerID: rr.CustomerID,
		Kind: safeWord(rr.Kind), Trigger: safeWord(rr.Trigger)}
	if rr.DurationMS != nil {
		row.Duration = durText(*rr.DurationMS)
	} else if rr.Outcome == "" {
		row.Duration = "running"
	}
	row.Classes = classCounts(rr.ErrorCounts)
	if len(row.Classes) == 0 && rr.ErrorClass != "" {
		row.Classes = []ClassCount{{Class: safeWord(rr.ErrorClass)}}
	}
	var notes []string
	if rr.Attempt > 1 {
		notes = append(notes, fmt.Sprintf("attempt %d", rr.Attempt))
	}
	if rr.DeferredReason != "" {
		notes = append(notes, "deferred: "+safeWord(rr.DeferredReason))
	}
	row.Note = strings.Join(notes, " · ")
	row.Aria = fmt.Sprintf("%s, %s, %s, %s on %s", c.dayClock(rr.StartedAt), strings.ToLower(row.Outcome.Label), row.Kind, rr.Hostname, rr.Customer)
	return row
}

// classCounts is the error classes of a run, biggest first, each name passed
// through safeWord.
func classCounts(m map[string]int) []ClassCount {
	var out []ClassCount
	other := 0
	for k, n := range m {
		if w := safeWord(k); w != withheld {
			out = append(out, ClassCount{Class: w, N: n})
		} else {
			other += n
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].N != out[j].N {
			return out[i].N > out[j].N
		}
		return out[i].Class < out[j].Class
	})
	if other > 0 {
		out = append(out, ClassCount{Class: withheld, N: other})
	}
	return out
}

// runsEmpty classifies an empty run list. It is neutral only when the
// conditions caused it; an account that has devices and no runs at all is an
// alarm, because a missing history is a silence signal (components 5.6).
func (s *Service) runsEmpty(q runQuery, resp adminapi.RunListResponse, fleet health.Fleet, spec runsSpec, clear string, c fmtCtx) *EmptyView {
	switch {
	case resp.Of > 0:
		return &EmptyView{Kind: "neutral", Title: "No runs match these conditions", Body: fmt.Sprintf("0 of %s shown.", plural(resp.Of, "run", "runs")), Link: &Link{Href: clear, Label: "Clear conditions"}}
	case len(q.Clauses) > 0:
		return &EmptyView{Kind: "neutral", Title: "No runs match these conditions", Body: "0 of 0 shown. No run has been recorded in this view at all.", Link: &Link{Href: clear, Label: "Clear conditions"}}
	}
	devices := 0
	for _, d := range fleet.View.Devices {
		if spec.Pin != "" && d.CustomerID != spec.Pin {
			continue
		}
		devices++
	}
	switch {
	case spec.Pin != "" && spec.Row != nil && spec.Row.Devices == 0:
		return s.customerNoDevices(*spec.Row, c)
	case devices > 0:
		return &EmptyView{Kind: "alarm", Title: "No runs are recorded for " + plural(devices, "enrolled device", "enrolled devices"),
			Body: "Devices are enrolled but none has ever reported a run. A missing history is a silence signal, not a quiet fleet: check that the agents can reach the console."}
	case fleet.View.FirstRun:
		return &EmptyView{Kind: "neutral", Title: "No runs yet", Body: "Runs appear here once an enrolled device reports its first backup."}
	}
	return &EmptyView{Kind: "alarm", Title: "No runs and no devices to run them", Body: "This account sees no enrolled device, so no backup can be happening here."}
}

// ---- S21 ----

// RunsPage is the S21 page.
type RunsPage struct {
	Page
	Runs RunListView
}

func (s *Service) runsPage(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	fleet, ok := s.fleetFor(w, r, p, "Runs")
	if !ok {
		return
	}
	q, bad := parseRunQuery(r.URL.Query())
	if len(bad) > 0 {
		s.badQuery(w, r, p, fleet, bad)
		return
	}
	// A condition was added: answer with its canonical, deep-linkable URL.
	if r.URL.Query().Get("add") != "" {
		http.Redirect(w, r, runsSpec{Base: "/runs"}.hrefFor(q), http.StatusSeeOther)
		return
	}
	view := s.buildRunsView(r, p, fleet, q, runsSpec{Base: "/runs"})
	s.render(w, r, http.StatusOK, "runs", "base", RunsPage{Page: s.page(r.Context(), p, fleet, "Runs", "runs", "/ui/events"), Runs: view})
}

// runsTablePartial is GET /ui/runs/table: the results region for htmx paging.
// It answers HX-Push-Url with the canonical page URL so the address bar always
// holds a deep link. A failure inside the region is part of the region (an
// alarm panel), so htmx always has something honest to swap in.
func (s *Service) runsTablePartial(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	v := r.URL.Query()
	spec := runsSpec{Base: "/runs"}
	if pin := v.Get("pin"); pin != "" {
		row, err := s.Store.GetCustomerRow(r.Context(), p.Scope, pin)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		spec = runsSpec{Base: "/customers/" + row.ID + "/runs", Pin: row.ID, Row: &row}
	}
	fleet, err := s.Fleet.Fleet(r.Context(), p.Scope)
	if err != nil {
		s.log().Error("ui: fleet failed", logx.Str("page", "runs-table"), logx.Err(err))
		s.renderFragmentError(w, r, "The fleet could not be loaded. This is an alarm, not an empty list.")
		return
	}
	q, bad := parseRunQuery(v)
	if len(bad) > 0 {
		http.Error(w, "Conditions not valid.", http.StatusBadRequest)
		return
	}
	view := s.buildRunsView(r, p, fleet, q, spec)
	w.Header().Set("HX-Push-Url", spec.hrefFor(q))
	s.render(w, r, http.StatusOK, "runs", "runs_results", view)
}

// ---- S16 ----

// customerRunsPage is the customer page in its Runs tab. It carries the same
// fields the other tabs do (the template reads them by name), plus the list.
type customerRunsPage struct {
	CustomerPage
	Runs *RunListView
}

func (s *Service) customerRuns(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	fleet, ok := s.fleetFor(w, r, p, "Customer")
	if !ok {
		return
	}
	row, ok := s.customerRow(w, r, p)
	if !ok {
		return
	}
	q, bad := parseRunQuery(r.URL.Query())
	if len(bad) > 0 {
		s.badQuery(w, r, p, fleet, bad)
		return
	}
	spec := runsSpec{Base: "/customers/" + row.ID + "/runs", Pin: row.ID, Row: &row}
	if r.URL.Query().Get("add") != "" {
		http.Redirect(w, r, spec.hrefFor(q), http.StatusSeeOther)
		return
	}
	c := s.fcAt(r.Context(), fleet.Computed)
	view := s.buildRunsView(r, p, fleet, q, spec)
	pg := customerRunsPage{CustomerPage: CustomerPage{Page: s.page(r.Context(), p, fleet, row.Name, "customers", "/ui/events"),
		H: s.custHead(p, row, rollupOf(fleet, row.ID), c, "runs"), Tab: "runs"}, Runs: &view}
	s.render(w, r, http.StatusOK, "customer", "base", pg)
}

// ---- S22 ----

// StatRow is one numeric counter of a run.
type StatRow struct {
	Label string
	Value string
}

// ErrClassRow is one error class of a run with a bar sized against the biggest.
type ErrClassRow struct {
	Class string
	N     int
	Pct   int
}

// RunDetailView is the run detail.
type RunDetailView struct {
	ID         string
	Outcome    Chip
	Kind       string
	Host       string
	DeviceID   string
	Customer   string
	CustomerID string
	Facts      []Fact
	Stats      []StatRow
	Errors     []ErrClassRow
	ErrTotal   int
	Cause      *CauseView
	Note       *CauseView
}

// RunDetailPage is the S22 page.
type RunDetailPage struct {
	Page
	Run RunDetailView
}

func (s *Service) runDetailPage(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	id := r.PathValue("id")
	fleet, ok := s.fleetFor(w, r, p, "Run")
	if !ok {
		return
	}
	if !idParam(id) {
		s.notFound(w, r, p, "This run")
		return
	}
	var d adminapi.RunDetailResponse
	if err := s.apiGet(r, "/api/v1/runs/"+url.PathEscape(id), &d); err != nil {
		if apiStatus(err) == http.StatusNotFound {
			s.notFound(w, r, p, "This run")
			return
		}
		s.log().Error("ui: run detail failed", logx.Class("render"), logx.Err(err))
		s.errorPage(w, r, p, http.StatusServiceUnavailable, "Run unavailable", "The run could not be read. This is an alarm, not an empty page. Try again shortly.")
		return
	}
	c := s.fcAt(r.Context(), fleet.Computed)
	s.render(w, r, http.StatusOK, "run", "base", RunDetailPage{Page: s.page(r.Context(), p, fleet, "Run detail", "runs", "/ui/events"), Run: runDetailView(d, c)})
}

// isHex reports whether s looks like a snapshot id: 8 to 64 lower-case hex digits.
func isHex(s string) bool {
	if len(s) < 8 || len(s) > 64 {
		return false
	}
	for _, ch := range s {
		if !(ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'f') {
			return false
		}
	}
	return true
}

func statValue(key string, v float64) string {
	if v >= 0 && v == float64(uint64(v)) {
		if strings.HasSuffix(key, "_bytes") || key == "data_added" {
			return byteSize(uint64(v))
		}
		return commas(uint64(v))
	}
	return strconv.FormatFloat(v, 'f', 2, 64)
}

func commas(n uint64) string {
	s := strconv.FormatUint(n, 10)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

func runDetailView(d adminapi.RunDetailResponse, c fmtCtx) RunDetailView {
	v := RunDetailView{ID: d.ID, Outcome: OutcomeChip("o-"+d.ID, d.Outcome, false), Kind: safeWord(d.Kind), Host: d.Hostname, DeviceID: d.DeviceID, Customer: d.Customer, CustomerID: d.CustomerID}
	add := func(k, val string, mono bool) { v.Facts = append(v.Facts, Fact{Key: k, Value: val, Mono: mono}) }
	oc := d.Outcome
	if oc == "" {
		oc = "running"
	}
	chip := v.Outcome
	v.Facts = append(v.Facts, Fact{Key: "Outcome", Chip: &chip})
	add("Kind", v.Kind+" · "+safeWord(d.Trigger)+" · "+safeWord(d.Origin), false)
	add("Started", c.dateClock(d.StartedAt)+" ("+c.Zone+")", false)
	switch {
	case d.FinishedAt != nil:
		add("Finished", c.dateClock(*d.FinishedAt)+" ("+c.Zone+")", false)
		if d.DurationMS != nil {
			add("Duration", durText(*d.DurationMS), false)
		}
	default:
		add("Finished", "still running: no result has been reported", false)
	}
	add("Reported to the console", c.dateClock(d.ReceivedAt)+" ("+c.Zone+")", false)
	if off := d.ClockOffsetMS; off > 1000 || off < -1000 {
		dir := "ahead of"
		if off < 0 {
			dir, off = "behind", -off
		}
		add("Device clock", durText(off)+" "+dir+" the console", false)
	}
	add("Attempt", strconv.Itoa(max(d.Attempt, 1)), false)
	if d.ExitCode != nil {
		add("Engine exit code", strconv.Itoa(*d.ExitCode), true)
	}
	if d.SnapshotID != "" {
		if isHex(d.SnapshotID) {
			add("Snapshot", d.SnapshotID, true)
		} else {
			add("Snapshot", withheld, false)
		}
	}
	if d.DataAddedBytes != nil && *d.DataAddedBytes >= 0 {
		add("Data added", byteSize(uint64(*d.DataAddedBytes)), false)
	}
	if d.PolicyVersion != nil {
		add("Policy version", strconv.Itoa(*d.PolicyVersion), true)
	}
	if d.AgentVersion != "" {
		add("Agent version", safeWord(d.AgentVersion), true)
	}
	if d.DeferredReason != "" {
		add("Deferred because", safeWord(d.DeferredReason), false)
	}
	keys := make([]string, 0, len(d.Stats))
	for k := range d.Stats {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v.Stats = append(v.Stats, StatRow{Label: capFirst(words(k)), Value: statValue(k, d.Stats[k])})
	}
	biggest := 0
	var other int
	rows := map[string]int{}
	for _, ec := range d.ErrorClasses {
		w := safeWord(ec.Class)
		if w == withheld {
			other += ec.Count
			continue
		}
		rows[w] += ec.Count
	}
	for cl, n := range rows {
		v.Errors = append(v.Errors, ErrClassRow{Class: cl, N: n})
	}
	sort.Slice(v.Errors, func(i, j int) bool {
		if v.Errors[i].N != v.Errors[j].N {
			return v.Errors[i].N > v.Errors[j].N
		}
		return v.Errors[i].Class < v.Errors[j].Class
	})
	if other > 0 {
		v.Errors = append(v.Errors, ErrClassRow{Class: withheld, N: other})
	}
	for _, e := range v.Errors {
		v.ErrTotal += e.N
		biggest = max(biggest, e.N)
	}
	for i := range v.Errors {
		if biggest > 0 {
			v.Errors[i].Pct = max(v.Errors[i].N*100/biggest, 2)
		}
	}
	switch oc {
	case "failed", "abandoned":
		text := "The run did not finish its backup."
		if d.ErrorClass != "" {
			text += " Error class " + safeWord(d.ErrorClass) + "."
		}
		v.Cause = &CauseView{Class: ClassCrit, Title: strings.ToUpper(oc[:1]) + oc[1:] + ".", Text: text}
	case "partial":
		text := "Some files could not be read."
		if n, ok := d.Stats["failed_files"]; ok {
			text = fmt.Sprintf("%s could not be read.", plural(int(n), "file", "files"))
		}
		v.Cause = &CauseView{Class: ClassWarn, Title: "Partial.", Text: text}
	case "interrupted":
		v.Cause = &CauseView{Class: ClassWarn, Title: "Interrupted.", Text: "The run was cut short, for example by a restart or a shutdown."}
	case "running":
		v.Cause = &CauseView{Class: ClassInfo, Title: "Running.", Text: "No result has been reported yet. A run that stays open past its limit is reported as STUCK on the fleet board."}
	}
	if len(v.Errors) == 0 && (oc == "failed" || oc == "partial" || oc == "abandoned") {
		v.Note = &CauseView{Class: ClassWarn, Title: "No error class was recorded.", Text: "The agent reported a " + oc + " run without an error class. It may have been interrupted before it could say why."}
	}
	return v
}
