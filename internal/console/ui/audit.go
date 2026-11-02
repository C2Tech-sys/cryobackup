package ui

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/health"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
)

// The audit viewer (screen B09, architecture §5.3 GET /audit, admin and above;
// M2-W3A). Read-only and scoped: the store shows an account only the events of
// its customers. Every filter lives in the query string so a view is a link;
// the results region is also served as a fragment (GET /ui/audit/table) that
// htmx swaps in. A row opens a detail panel (?event=ID) with the stored
// details, never a value the audit store would refuse (store.AuditDetails).

const (
	patAudit      = "GET /audit"
	patAuditTable = "GET /ui/audit/table"
)

func auditUIPatterns() []string { return []string{patAudit, patAuditTable} }

// auditQ is the state of the audit page.
type auditQ struct {
	Actor, Action, ObjectType, Customer, Result string
	From, To                                    string // dates (YYYY-MM-DD) in the display zone
	Page, Size                                  int
	Event                                       string
}

func (q auditQ) filtered() bool {
	return q.Actor != "" || q.Action != "" || q.ObjectType != "" || q.Customer != "" || q.Result != "" || q.From != "" || q.To != ""
}

// values encodes q without its defaults; withEvent keeps the open detail panel.
func (q auditQ) values(withEvent bool) url.Values {
	v := url.Values{}
	set := func(k, s string) {
		if s != "" {
			v.Set(k, s)
		}
	}
	set("actor", q.Actor)
	set("action", q.Action)
	set("type", q.ObjectType)
	set("customer", q.Customer)
	set("result", q.Result)
	set("from", q.From)
	set("to", q.To)
	if q.Page > 1 {
		v.Set("page", strconv.Itoa(q.Page))
	}
	if q.Size != store.DefaultPageSize {
		v.Set("size", strconv.Itoa(q.Size))
	}
	if withEvent {
		set("event", q.Event)
	}
	return v
}

func parseAuditQuery(v url.Values) auditQ {
	get := func(k string) string { return strings.TrimSpace(v.Get(k)) }
	return auditQ{Actor: get("actor"), Action: get("action"), ObjectType: get("type"), Customer: get("customer"), Result: get("result"),
		From: get("from"), To: get("to"), Page: atoiDefault(v.Get("page"), 1), Size: store.NormalPageSize(atoiDefault(v.Get("size"), 0)), Event: get("event")}
}

// toStore converts the dates in the display zone to the UTC range the store
// filters on: from is the start of that local day, to the end of that local
// day (the store's end is exclusive, so the start of the next day).
func (q auditQ) toStore(loc *time.Location) (store.AuditQuery, []store.FieldError) {
	sq := store.AuditQuery{Actor: q.Actor, Action: q.Action, ObjectType: q.ObjectType, CustomerID: q.Customer, Result: q.Result, Page: q.Page, Size: q.Size}
	var bad []store.FieldError
	if q.From != "" {
		if t, err := time.ParseInLocation("2006-01-02", q.From, loc); err == nil {
			sq.Since = t.UTC()
		} else {
			bad = append(bad, store.FieldError{Field: "from", Message: "Use a date such as 2026-10-31."})
		}
	}
	if q.To != "" {
		if t, err := time.ParseInLocation("2006-01-02", q.To, loc); err == nil {
			sq.Until = t.AddDate(0, 0, 1).UTC()
		} else {
			bad = append(bad, store.FieldError{Field: "to", Message: "Use a date such as 2026-10-31."})
		}
	}
	return sq, bad
}

// AuditForm is the filter bar.
type AuditForm struct {
	Actor, From, To string
	Actors          []string
	Actions         []Option
	Types           []Option
	Customers       []Option
	Results         []Option
	Size            int
	Errors          map[string]string
}

// Err returns the error text of a field.
func (f AuditForm) Err(field string) string { return f.Errors[field] }

// AuditRowV is one event row.
type AuditRowV struct {
	ID           string
	When, ISO    string
	WhenFull     string
	Actor        string
	ActorKind    string
	Action       string
	ActionCode   string
	Object       string
	ObjectHref   string
	Customer     string
	CustomerHref string
	Result       Chip
	Method       string
	Selected     bool
	Href         string // opens the detail panel
	Partial      string
}

// AuditDetailLine is a key and value of the detail panel.
type AuditDetailLine struct {
	Key, Value, Withheld string
}

// AuditDetailV is the detail panel of one event.
type AuditDetailV struct {
	ID, Action, ActionCode string
	When, UTC, ISO         string
	Result                 Chip
	Severity               string
	Facts                  []CustFact
	Lines                  []AuditDetailLine
	CloseHref              string
	ClosePartial           string
	// Missing is set for a deep link to an event that does not exist or is out of scope.
	Missing bool
}

// AuditTableV is everything the audit templates need.
type AuditTableV struct {
	BaseURL, PartialURL string
	Form                AuditForm
	Rows                []AuditRowV
	Pager               PagerView
	Chips               []FilterChip
	ClearHref           string
	ClearPartial        string
	Empty               *EmptyView
	Detail              *AuditDetailV
	Summary             string
	AsOf                string
	Zone                string
	Of                  int
	Filtered            bool
	// Err is a failed filter (a link with a date that is not a date).
	Err string
}

// AuditPage is /audit.
type AuditPage struct {
	Page
	Table     AuditTableV
	CanChange bool
}

// humanAction turns an action code into words: "policy.update" becomes
// "Policy update". Known codes have a sharper phrase.
func humanAction(code string) string {
	if t, ok := auditActionLabels[code]; ok {
		return t
	}
	w := strings.NewReplacer(".", " ", "_", " ").Replace(code)
	if w == "" {
		return code
	}
	return capFirstRune(w)
}

var auditActionLabels = map[string]string{
	"auth.login": "Signed in", "customer.create": "Customer created", "customer.update": "Customer changed", "device.enroll": "Device enrolled",
	"device.update": "Device settings changed", "device.pause": "Device parked", "policy.create": "Policy created", "policy.update": "Policy changed",
	"policy.delete": "Policy deleted", "enrollment_token.create": "Enrollment token created", "enrollment_token.revoke": "Enrollment token revoked",
	"device.override_set": "Device override set", "device.override_clear": "Device override cleared", "device.identity_resolve": "Identity conflict resolved",
	"command.queue": "Command queued", "command.cancel": "Command cancelled", "settings.update": "Setting changed", "restore.request": "Restore requested",
	"restore.download": "Restore downloaded", "restore.failed": "Restore failed", "agent.enroll": "Agent enrolled", "agent.enroll.denied": "Agent enrollment denied",
	"agent.auth.blocked": "Agent sign-in blocked", "admin.password_reset": "Password reset", "console.init": "Console initialised",
	"gateway.htpasswd.drift": "Gateway login file drifted",
}

// fullClock is "Sat 4 Jul 2026, 3:04:05 PM CDT": the display zone on a 12-hour clock, with seconds.
func (c fmtCtx) fullClock(t time.Time) string {
	return t.In(c.loc()).Format("Mon 2 Jan 2006, 3:04:05 PM MST")
}

// capFirst upper-cases the first rune of s (and is safe on "").
func capFirstRune(s string) string {
	r, n := utf8.DecodeRuneInString(s)
	if n == 0 {
		return s
	}
	return string(unicode.ToUpper(r)) + s[n:]
}

func objectHref(typ, id string) string {
	if id == "" {
		return ""
	}
	switch typ {
	case "customer":
		return "/customers/" + url.PathEscape(id)
	case "device":
		return "/devices/" + url.PathEscape(id)
	case "policy":
		return "/policies/" + url.PathEscape(id)
	}
	return ""
}

func shortID(id string) string {
	if len(id) > 10 {
		return id[:8] + "…"
	}
	return id
}

func resultChip(id, result string) Chip {
	c := Chip{ID: "ar-" + id, Label: strings.ToUpper(result), Class: ClassOK, Small: true}
	switch result {
	case "denied":
		c.Class = ClassWarn
	case "error":
		c.Class = ClassCrit
	}
	return c
}

// buildAudit reads and shapes one results region. The status is the HTTP
// status for the full page (a deep link to a missing event is 404).
func (s *Service) buildAudit(ctx context.Context, p *auth.Principal, fleet health.Fleet, q auditQ) (AuditTableV, int, error) {
	c := s.fcAt(ctx, fleet.Computed)
	t := AuditTableV{BaseURL: "/audit", PartialURL: "/ui/audit/table", AsOf: c.clock(fleet.Computed), Zone: s.zone(ctx), Filtered: q.filtered()}
	href := func(mod func(*auditQ)) (string, string) {
		m := q
		mod(&m)
		v := m.values(m.Event != "")
		return listURL(t.BaseURL, v), listURL(t.PartialURL, v)
	}
	sq, bad := q.toStore(c.loc())
	if len(bad) > 0 { // a link with a date that is not a date: refused, never shown unfiltered
		t.Err = bad[0].Message
		return t, http.StatusBadRequest, store.ErrInvalid
	}
	tb, err := s.Store.AuditTable(ctx, p.Scope, sq)
	if err != nil {
		if errors.Is(err, store.ErrInvalid) {
			f := store.FieldErrors(err)
			t.Err = "These filters are not valid."
			if len(f) > 0 {
				t.Err = f[0].Message
			}
			return t, http.StatusBadRequest, err
		}
		return t, http.StatusServiceUnavailable, err
	}
	q.Page, q.Size = tb.Page.Page, tb.Page.Size
	t.Of = tb.Total

	custs, cerr := s.Store.ListCustomerRows(ctx, p.Scope, store.CustomerListQuery{IncludeArchived: true})
	if cerr != nil {
		return t, http.StatusServiceUnavailable, cerr
	}
	custName := map[string]string{}
	custOpts := []Option{{Value: "", Label: "All customers", Selected: q.Customer == ""}}
	for _, cu := range custs {
		custName[cu.ID] = cu.Name
		custOpts = append(custOpts, Option{Value: cu.ID, Label: cu.Name, Selected: cu.ID == q.Customer})
	}
	facets, ferr := s.Store.AuditFacets(ctx, p.Scope)
	if ferr != nil {
		return t, http.StatusServiceUnavailable, ferr
	}
	t.Form = AuditForm{Actor: q.Actor, From: q.From, To: q.To, Actors: facets.Actors, Customers: custOpts, Size: q.Size, Errors: map[string]string{}}
	t.Form.Actions = append(t.Form.Actions, Option{Value: "", Label: "Any action", Selected: q.Action == ""})
	families := map[string]bool{}
	var famList []string
	for _, a := range facets.Actions {
		// An action that does not start with a name (".x") has no family: it is still
		// listed on its own below, but "*.x" would be a filter the store refuses.
		if fam, _, ok := strings.Cut(a, "."); ok && fam != "" && !families[fam] {
			families[fam] = true
			famList = append(famList, fam)
		}
	}
	for _, fam := range famList {
		code := fam + ".*"
		t.Form.Actions = append(t.Form.Actions, Option{Value: code, Label: capFirstRune(fam) + " (all)", Selected: q.Action == code})
	}
	for _, a := range facets.Actions {
		t.Form.Actions = append(t.Form.Actions, Option{Value: a, Label: humanAction(a) + " · " + a, Selected: q.Action == a})
	}
	t.Form.Types = append(t.Form.Types, Option{Value: "", Label: "Any object", Selected: q.ObjectType == ""})
	for _, o := range facets.ObjectTypes {
		t.Form.Types = append(t.Form.Types, Option{Value: o, Label: strings.ReplaceAll(o, "_", " "), Selected: o == q.ObjectType})
	}
	t.Form.Results = []Option{{Value: "", Label: "Any result", Selected: q.Result == ""}}
	for _, r := range store.AuditResults {
		t.Form.Results = append(t.Form.Results, Option{Value: r, Label: strings.ToUpper(r), Selected: r == q.Result})
	}

	// rows
	for _, ev := range tb.Rows {
		actor := ev.ActorLabel
		if actor == "" {
			actor = ev.ActorType
		}
		row := AuditRowV{ID: ev.ID, When: c.dayClock(ev.OccurredAt), ISO: ev.OccurredAt.UTC().Format(time.RFC3339),
			WhenFull: c.fullClock(ev.OccurredAt), Actor: actor, ActorKind: ev.ActorType, Action: humanAction(ev.Action), ActionCode: ev.Action,
			Result: resultChip(ev.ID, ev.Result), Method: ev.Method, Selected: ev.ID == q.Event}
		if ev.ObjectType != "" {
			row.Object = strings.ReplaceAll(ev.ObjectType, "_", " ")
			if ev.ObjectID != "" {
				row.Object += " " + shortID(ev.ObjectID)
			}
			row.ObjectHref = objectHref(ev.ObjectType, ev.ObjectID)
		}
		if ev.CustomerID != "" {
			row.Customer, row.CustomerHref = custName[ev.CustomerID], "/customers/"+url.PathEscape(ev.CustomerID)
			if row.Customer == "" { // the customer was removed: the row outlives it
				row.Customer, row.CustomerHref = "removed customer "+shortID(ev.CustomerID), ""
			}
		}
		id := ev.ID
		row.Href, row.Partial = href(func(m *auditQ) { m.Event = id })
		t.Rows = append(t.Rows, row)
	}
	t.Pager = buildPager(tb.Page, "events",
		func(page, size int) string {
			m := q
			m.Page, m.Size = page, size
			return listURL(t.BaseURL, m.values(m.Event != ""))
		},
		func(page, size int) string {
			m := q
			m.Page, m.Size = page, size
			return listURL(t.PartialURL, m.values(m.Event != ""))
		})
	t.Summary = t.Pager.Text

	// removable chips
	addChip := func(label string, mod func(*auditQ)) {
		h, pt := href(mod)
		t.Chips = append(t.Chips, FilterChip{Label: label, Href: h, Partial: pt})
	}
	if q.Actor != "" {
		addChip(`Actor: "`+q.Actor+`"`, func(m *auditQ) { m.Actor, m.Page = "", 1 })
	}
	if q.Action != "" {
		addChip("Action: "+q.Action, func(m *auditQ) { m.Action, m.Page = "", 1 })
	}
	if q.ObjectType != "" {
		addChip("Object: "+strings.ReplaceAll(q.ObjectType, "_", " "), func(m *auditQ) { m.ObjectType, m.Page = "", 1 })
	}
	if q.Customer != "" {
		name := custName[q.Customer]
		if name == "" {
			name = "unknown customer"
		}
		addChip("Customer: "+name, func(m *auditQ) { m.Customer, m.Page = "", 1 })
	}
	if q.Result != "" {
		addChip("Result: "+strings.ToUpper(q.Result), func(m *auditQ) { m.Result, m.Page = "", 1 })
	}
	if q.From != "" {
		addChip("From "+q.From, func(m *auditQ) { m.From, m.Page = "", 1 })
	}
	if q.To != "" {
		addChip("Until "+q.To, func(m *auditQ) { m.To, m.Page = "", 1 })
	}
	t.ClearHref, t.ClearPartial = href(func(m *auditQ) { *m = auditQ{Size: m.Size, Page: 1} })

	status := http.StatusOK
	if q.Event != "" {
		d, st := s.auditDetail(ctx, p, q, c, href)
		t.Detail, status = d, st
	}
	t.Empty = s.auditEmpty(p, t, tb)
	return t, status, nil
}

// auditEmpty classifies an empty result (components 9.5): an empty trail is an
// alarm for an all-customer account (the console writes a row for every sign-in
// and change), a filter that matches nothing is neutral and shows the
// denominator.
func (s *Service) auditEmpty(p *auth.Principal, t AuditTableV, tb store.AuditTable) *EmptyView {
	if len(tb.Rows) > 0 {
		return nil
	}
	if tb.Total > 0 {
		body := "0 of " + strconv.Itoa(tb.Total) + " shown."
		if t.Err != "" {
			body = t.Err + " " + body
		}
		return &EmptyView{Kind: "neutral", Title: "No events match these filters", Body: body, Link: &Link{Href: t.ClearHref, Label: "Clear filters"}}
	}
	if p.Scope.All {
		return &EmptyView{Kind: "alarm", Title: "The audit trail is empty",
			Body: "Every sign-in and every change writes an audit row, so an empty trail means auditing is broken or the table was cleared. Check the console log and the database."}
	}
	return &EmptyView{Kind: "neutral", Title: "No events for your customers yet",
		Body: "This account sees the events of its own customers. None have been recorded."}
}

// auditDetail builds the detail panel for q.Event; a missing or out-of-scope
// event is a flagged panel with a 404 for the full page.
func (s *Service) auditDetail(ctx context.Context, p *auth.Principal, q auditQ, c fmtCtx, href func(func(*auditQ)) (string, string)) (*AuditDetailV, int) {
	closeHref, closePartial := href(func(m *auditQ) { m.Event = "" })
	d := &AuditDetailV{ID: q.Event, CloseHref: closeHref, ClosePartial: closePartial}
	ev, err := s.Store.GetAuditEvent(ctx, p.Scope, q.Event)
	if err != nil {
		d.Missing = true
		if !errors.Is(err, store.ErrNotFound) {
			s.log().Error("ui: audit event failed", logx.Err(err))
			return d, http.StatusServiceUnavailable
		}
		return d, http.StatusNotFound
	}
	d.Action, d.ActionCode = humanAction(ev.Action), ev.Action
	d.When, d.ISO, d.UTC = c.fullClock(ev.OccurredAt), ev.OccurredAt.UTC().Format(time.RFC3339), ev.OccurredAt.UTC().Format("2006-01-02 3:04:05 PM")+" UTC"
	d.Result, d.Severity = resultChip(ev.ID, ev.Result), ev.Severity
	actor := ev.ActorLabel
	if actor == "" {
		actor = ev.ActorType
	}
	fact := func(k, v string, mono bool) {
		if v != "" {
			d.Facts = append(d.Facts, CustFact{Key: k, Value: v, Mono: mono})
		}
	}
	fact("Actor", actor, false)
	fact("Actor type", ev.ActorType, true)
	fact("Actor id", ev.ActorID, true)
	fact("Method", ev.Method, true)
	fact("Source IP", ev.SourceIP, true)
	fact("Object", strings.ReplaceAll(ev.ObjectType, "_", " "), false)
	fact("Object id", ev.ObjectID, true)
	fact("Customer id", ev.CustomerID, true)
	fact("Event id", ev.ID, true)
	for _, l := range store.AuditDetails(ev.Details) {
		d.Lines = append(d.Lines, AuditDetailLine{Key: l.Key, Value: l.Value, Withheld: l.Withheld})
	}
	return d, http.StatusOK
}

// ---- handlers ----

func (s *Service) auditPage(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	fleet, ok := s.fleetFor(w, r, p, "Audit log")
	if !ok {
		return
	}
	q := parseAuditQuery(r.URL.Query())
	t, status, err := s.buildAudit(r.Context(), p, fleet, q)
	if err != nil {
		if status == http.StatusBadRequest {
			s.errorPage(w, r, p, http.StatusBadRequest, "Filters not valid", t.Err+" Return to the unfiltered log.")
			return
		}
		s.log().Error("ui: audit failed", logx.Err(err))
		s.render(w, r, http.StatusServiceUnavailable, "error", "base", errorData(s.page(r.Context(), p, fleet, "Audit log unavailable", "audit", ""), 503, "Audit log unavailable",
			"The audit trail could not be read. This is an alarm, not an empty list. Try again shortly."))
		return
	}
	pg := AuditPage{Page: s.page(r.Context(), p, fleet, "Audit log", "audit", "/ui/events"), Table: t, CanChange: p.Role >= auth.RoleOwner}
	s.render(w, r, status, "audit", "base", pg)
}

// auditTablePartial is GET /ui/audit/table: the results region for htmx. It
// answers HX-Push-Url with the canonical page URL so the address bar always
// holds a deep link; a failure is a 200 alarm panel (htmx does not swap an
// error status in).
func (s *Service) auditTablePartial(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	fleet, err := s.Fleet.Fleet(r.Context(), p.Scope)
	if err != nil {
		s.log().Error("ui: fleet failed", logx.Str("page", "audit-table"), logx.Err(err))
		s.renderAuditFragmentError(w, r, "The fleet could not be loaded, so the audit log cannot be shown.")
		return
	}
	q := parseAuditQuery(r.URL.Query())
	t, _, err := s.buildAudit(r.Context(), p, fleet, q)
	if err != nil {
		if errors.Is(err, store.ErrInvalid) {
			http.Error(w, "Filters not valid.", http.StatusBadRequest)
			return
		}
		s.log().Error("ui: audit failed", logx.Err(err))
		s.renderAuditFragmentError(w, r, "The audit trail could not be read. This is an alarm, not an empty list.")
		return
	}
	w.Header().Set("HX-Push-Url", listURL(t.BaseURL, q.values(q.Event != "")))
	s.render(w, r, http.StatusOK, "audit", "audit_results", t)
}

func (s *Service) renderAuditFragmentError(w http.ResponseWriter, r *http.Request, body string) {
	s.render(w, r, http.StatusOK, "audit", "panel_state", &EmptyView{Kind: "alarm", Title: "Could not load the audit log", Body: body + " Reload the page to retry.",
		Link: &Link{Href: r.Header.Get("HX-Current-URL"), Label: "Retry"}})
}
