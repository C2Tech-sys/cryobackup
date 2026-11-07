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

	"github.com/c2tech-sys/cryobackup/internal/console/adminapi"
	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/enroll"
	"github.com/c2tech-sys/cryobackup/internal/console/health"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
)

// The enrollment wizard and token management (B16, S30; architecture §5.3
// /enroll, §5.2 /enrollment-tokens; tickets M2-W2B and M3-T6). /enroll is the
// token table; "New enrollment" opens the wizard on its own page, /enroll/new.
//
// The secret of a token exists in exactly one response: the page returned by
// the POST that created it. It is generated after the form validated, handed to
// the template and dropped. It is not in a redirect, a cookie, a flash message,
// the audit row, the log or any later page, and the response is no-store. The
// store keeps only its SHA-256.

// choice is one entry of a select with the value it stands for.
type choice struct {
	Value string
	Label string
	D     time.Duration
}

// expiryChoices are the lifetimes the form offers (the longest is store.MaxTokenLifetime).
var expiryChoices = []choice{
	{"1h", "1 hour", time.Hour}, {"4h", "4 hours", 4 * time.Hour}, {"1d", "24 hours", 24 * time.Hour}, {"3d", "3 days", 72 * time.Hour},
	{"7d", "7 days", 7 * 24 * time.Hour}, {"14d", "14 days", 14 * 24 * time.Hour}, {"30d", "30 days", 30 * 24 * time.Hour},
	{"90d", "90 days (the longest)", store.MaxTokenLifetime},
}

// useChoices are the use caps the form offers; "unlimited" has no cap.
var useChoices = []choice{
	{"1", "One device", 0}, {"5", "Up to 5 devices", 0}, {"10", "Up to 10 devices", 0}, {"25", "Up to 25 devices", 0},
	{"50", "Up to 50 devices", 0}, {"100", "Up to 100 devices", 0}, {"unlimited", "Unlimited, until it expires", 0},
}

const (
	defaultExpiry = "7d"
	defaultUses   = "10"
)

var classChoices = []choice{{"unknown", "Not set (the agent reports it)", 0}, {"desktop", "Desktop", 0}, {"laptop", "Laptop", 0}}

// EnrollForm is the wizard's form with its values and errors.
type EnrollForm struct {
	CSRF              string
	CustomerID        string
	NewCustomer       string
	Label             string
	PolicyID          string
	DeviceClass       string
	Expires           string
	Uses              string
	Customers         []Option
	Policies          []Option
	Classes           []Option
	Expiries          []Option
	UseOpts           []Option
	CanCreateCustomer bool
	Errors            map[string]string
	Summary           []store.FieldError
}

// Err returns the error text of a field ("" when it is fine).
func (f EnrollForm) Err(field string) string { return f.Errors[field] }

// HasErr reports whether a field was rejected.
func (f EnrollForm) HasErr(field string) bool { return f.Errors[field] != "" }

// EnrollRow is one line of the token list.
type EnrollRow struct {
	ID         string
	Customer   string
	CustomerID string
	Label      string
	Status     Chip
	Active     bool
	// CanRevoke: revoking still changes something (active or used up). CanDelete: the
	// token is revoked or expired, so it can be hidden from the list.
	CanRevoke  bool
	CanDelete  bool
	Deleted    string // when it was hidden from the list ("" = listed)
	Uses       string
	UsesNote   string
	Expires    string
	ExpiresISO string
	ExpiresRel string
	CreatedBy  string
	Created    string
	LastUsed   string
	LastRel    string
	Policy     string
	Class      string
	Aria       string
}

// TokenFilter is the filter bar of the token list.
type TokenFilter struct {
	Customers []Option
	Statuses  []Option
	Size      int
}

// TokenSeg is one status tab over the token table, with its count.
type TokenSeg struct {
	Label   string
	N       int
	Href    string
	Current bool
}

// TokenList is the "Tokens" section of the wizard page.
type TokenList struct {
	Rows      []EnrollRow
	Filter    TokenFilter
	Segs      []TokenSeg
	NewHref   string // the wizard, on the customer the list is filtered to
	Pager     PagerView
	Chips     []FilterChip
	ClearHref string
	Summary   string
	Empty     *EmptyView
	// Err is set when the list could not be read: an alarm, not an empty list.
	Err   bool
	Total int
	All   int
}

// CreatedToken is the one-time result of a successful create.
type CreatedToken struct {
	Token       string
	ID          string
	Customer    string
	CustomerID  string
	NewCustomer bool
	Label       string
	Expires     string
	ExpiresRel  string
	Uses        string
	Class       string
	Policy      string
	PolicyNote  string // set when the token names a policy (applied at enrollment)
	Custody     string // non-empty note when the customer holds its own key
	Snips       adminapi.Snippets
	MSI         MSIView
}

// EnrollPage is /enroll: the token list, or (right after a create) the one-time
// result.
type EnrollPage struct {
	Page
	Notes   []Note
	Saved   string
	Refused string
	Created *CreatedToken
	Tokens  TokenList
}

// EnrollNewPage is /enroll/new: the wizard's form.
type EnrollNewPage struct {
	Page
	Form  EnrollForm
	Notes []Note
}

// tokenQuery is the state of the token list, kept in the query string.
type tokenQuery struct {
	Customer string
	Status   string
	Page     int
	Size     int
}

func parseTokenQuery(v url.Values) (tokenQuery, []store.FieldError) {
	q := tokenQuery{Customer: v.Get("customer"), Status: v.Get("status"), Page: atoiDefault(v.Get("page"), 1), Size: store.NormalPageSize(atoiDefault(v.Get("size"), 0))}
	var bad []store.FieldError
	if q.Status != "" && !store.ValidTokenStatus(q.Status) {
		bad = append(bad, store.FieldError{Field: "status", Message: "Unknown token status."})
		q.Status = ""
	}
	return q, bad
}

func (q tokenQuery) values() url.Values {
	v := url.Values{}
	if q.Customer != "" {
		v.Set("customer", q.Customer)
	}
	if q.Status != "" {
		v.Set("status", q.Status)
	}
	if q.Page > 1 {
		v.Set("page", strconv.Itoa(q.Page))
	}
	if q.Size != store.DefaultPageSize {
		v.Set("size", strconv.Itoa(q.Size))
	}
	return v
}

func (q tokenQuery) filtered() bool { return q.Customer != "" || q.Status != "" }

// ---- small formatters ----

func tokenStatusChip(status string) Chip {
	switch status {
	case store.TokenActive:
		return Chip{Label: "ACTIVE", Class: ClassOK, Small: true}
	case store.TokenUsedUp:
		return Chip{Label: "USED UP", Class: ClassInfo, Small: true}
	case store.TokenExpired:
		return Chip{Label: "EXPIRED", Class: ClassNeutral, Small: true}
	case store.TokenRevoked:
		return Chip{Label: "REVOKED", Class: ClassNeutral, Small: true}
	}
	return Chip{Label: "UNKNOWN", Class: ClassCrit, Small: true}
}

var tokenStatusLabels = map[string]string{store.TokenActive: "Active", store.TokenUsedUp: "Used up", store.TokenExpired: "Expired", store.TokenRevoked: "Revoked"}

// untilText is "in 3 d" for a future time and "2 d ago" for a past one.
func untilText(d time.Duration) string {
	if d < 0 {
		return ageLong(-d)
	}
	return "in " + strings.TrimSuffix(ageLong(d), " ago")
}

func usesText(t store.EnrollmentToken) (text, note string) {
	if t.MaxUses == nil {
		return fmt.Sprintf("%d used", t.UsedCount), "no cap"
	}
	return fmt.Sprintf("%d of %d used", t.UsedCount, *t.MaxUses), fmt.Sprintf("%d left", t.UsesLeft())
}

func (s *Service) enrollRow(t store.EnrollTokenRow, c fmtCtx) EnrollRow {
	status := t.Status(c.Now)
	r := EnrollRow{ID: t.ID, Customer: t.CustomerName, CustomerID: t.CustomerID, Label: t.Label, Status: tokenStatusChip(status), Active: status == store.TokenActive,
		CanRevoke: status == store.TokenActive || status == store.TokenUsedUp, CanDelete: t.DeletedAt == nil && (status == store.TokenRevoked || status == store.TokenExpired),
		Expires: c.dateClock(t.ExpiresAt), ExpiresISO: t.ExpiresAt.UTC().Format(time.RFC3339), ExpiresRel: untilText(t.ExpiresAt.Sub(c.Now)),
		CreatedBy: t.CreatedByName, Created: c.dateClock(t.CreatedAt), Policy: t.PolicyName, Class: t.DeviceClassDefault, LastUsed: "Never"}
	if r.CreatedBy == "" {
		r.CreatedBy = "CLI or removed account"
	}
	if r.Label == "" {
		r.Label = "(no label)"
	}
	if r.Policy == "" {
		r.Policy = "Customer's policy"
	}
	r.Uses, r.UsesNote = usesText(t.EnrollmentToken)
	if t.LastUsedAt != nil {
		r.LastUsed, r.LastRel = c.dateClock(*t.LastUsedAt), ageLong(c.Now.Sub(*t.LastUsedAt))
	}
	if t.DeletedAt != nil {
		r.Deleted = c.dateClock(*t.DeletedAt)
	}
	r.Aria = fmt.Sprintf("Token %s for %s, %s", r.Label, r.Customer, strings.ToLower(tokenStatusLabels[status]))
	return r
}

// ---- the form ----

// customerOptions lists the customers in scope for a select; withArchived keeps archived ones.
func (s *Service) customerOptions(ctx context.Context, p *auth.Principal, selected string, withArchived bool, first string) ([]Option, []store.Customer, error) {
	cs, err := s.Store.ListCustomers(ctx, p.Scope)
	if err != nil {
		return nil, nil, err
	}
	opts := []Option{{Value: "", Label: first, Selected: selected == ""}}
	var kept []store.Customer
	for _, c := range cs {
		if c.State == "archived" && !withArchived {
			continue
		}
		lbl := c.Name
		if c.State == "archived" {
			lbl += " (archived)"
		}
		opts = append(opts, Option{Value: c.ID, Label: lbl, Selected: c.ID == selected})
		kept = append(kept, c)
	}
	return opts, kept, nil
}

func choiceOptions(cs []choice, selected string) []Option {
	out := make([]Option, len(cs))
	for i, c := range cs {
		out[i] = Option{Value: c.Value, Label: c.Label, Selected: c.Value == selected}
	}
	return out
}

func (s *Service) newEnrollForm(ctx context.Context, p *auth.Principal, customerID string) (EnrollForm, error) {
	f := EnrollForm{CSRF: p.CSRF, CustomerID: customerID, Expires: defaultExpiry, Uses: defaultUses, DeviceClass: "unknown", Errors: map[string]string{},
		CanCreateCustomer: p.Scope.All}
	return f, s.fillEnrollOptions(ctx, p, &f)
}

// fillEnrollOptions (re)builds every select from the form's current values.
func (s *Service) fillEnrollOptions(ctx context.Context, p *auth.Principal, f *EnrollForm) error {
	var err error
	f.Customers, _, err = s.customerOptions(ctx, p, f.CustomerID, false, "Choose a customer…")
	if err != nil {
		return err
	}
	choices, err := s.Store.PolicyChoicesFor(ctx, p.Scope, f.CustomerID)
	if err != nil {
		return err
	}
	f.Policies = []Option{{Value: "", Label: "The customer's policy (or the default)", Selected: f.PolicyID == ""}}
	for _, ch := range choices {
		lbl := ch.Name + " (template)"
		if ch.Scope == "customer" {
			lbl = ch.Name + " (this customer)"
		}
		f.Policies = append(f.Policies, Option{Value: ch.ID, Label: lbl, Selected: ch.ID == f.PolicyID})
	}
	f.Classes, f.Expiries, f.UseOpts = choiceOptions(classChoices, f.DeviceClass), choiceOptions(expiryChoices, f.Expires), choiceOptions(useChoices, f.Uses)
	return nil
}

func (f *EnrollForm) finish() {
	order := []string{"customer_id", "new_customer", "label", "policy_id", "device_class", "expires", "max_uses"}
	f.Summary = nil
	seen := map[string]bool{}
	for _, k := range order {
		if m := f.Errors[k]; m != "" {
			f.Summary = append(f.Summary, store.FieldError{Field: k, Message: m})
			seen[k] = true
		}
	}
	for k, m := range f.Errors {
		if !seen[k] {
			f.Summary = append(f.Summary, store.FieldError{Field: k, Message: m})
		}
	}
}

// pageNotes are the conditions that make the snippets or the enrollment itself unreliable.
func (s *Service) pageNotes() []Note {
	var out []Note
	if _, _, problem := adminapi.AgentServerURL(s.PublicURL); problem != "" {
		class, title := "warn", "Public URL needs attention."
		if strings.TrimSpace(s.PublicURL) == "" {
			class, title = "warn", "Public URL not set." // M3-T5 ruling 6: a warning, not critical
		}
		out = append(out, Note{Class: class, Title: title, Text: problem})
	}
	if s.MasterLoaded != nil && !s.MasterLoaded() {
		out = append(out, Note{Class: "crit", Title: "Master key unavailable.", Text: "Agents cannot enroll until the key is loaded: the console refuses with 503 rather than store an unwrapped repository key."})
	}
	return out
}

// ---- the page ----

func (s *Service) enrollPageData(r *http.Request, p *auth.Principal, fleet health.Fleet, tq tokenQuery, bad []store.FieldError) EnrollPage {
	code := r.URL.Query().Get("saved")
	pg := EnrollPage{Page: s.page(r.Context(), p, fleet, "Enroll devices", "enroll", ""), Notes: s.pageNotes(), Saved: enrollSavedText(code), Refused: enrollRefusedText(code)}
	pg.Tokens = s.tokenList(r.Context(), p, tq, bad)
	for _, fe := range bad {
		pg.Notes = append(pg.Notes, Note{Class: "warn", Title: "Filters not valid.", Text: fe.Message + " The list below ignores that filter."})
	}
	return pg
}

func enrollSavedText(code string) string {
	switch code {
	case "revoked":
		return "Token revoked. It can no longer enroll devices; devices that already enrolled are unaffected."
	case "already_revoked":
		return "That token was already revoked."
	case "deleted":
		return "Token deleted. It is gone from this list; the audit log and the devices enrolled with it still point at it."
	case "already_deleted":
		return "That token was already deleted."
	}
	return ""
}

// enrollRefusedText says why a delete did not happen.
func enrollRefusedText(code string) string {
	if code == "not_deletable" {
		return "Not deleted. Only a revoked or expired token can be deleted: revoke it first."
	}
	return ""
}

func (s *Service) enrollPage(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	fleet, ok := s.fleetFor(w, r, p, "Enrollment")
	if !ok {
		return
	}
	tq, bad := parseTokenQuery(r.URL.Query())
	s.render(w, r, http.StatusOK, "enroll", "base", s.enrollPageData(r, p, fleet, tq, bad))
}

// enrollNewPage is GET /enroll/new: the wizard, preselecting ?customer=.
func (s *Service) enrollNewPage(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	fleet, ok := s.fleetFor(w, r, p, "Enrollment")
	if !ok {
		return
	}
	form, err := s.newEnrollForm(r.Context(), p, r.URL.Query().Get("customer"))
	if err != nil {
		s.log().Error("ui: enroll form failed", logx.Err(err))
		s.errorPage(w, r, p, http.StatusServiceUnavailable, "Enrollment unavailable", "The customers could not be read. This is an alarm, not an empty list. Try again shortly.")
		return
	}
	s.render(w, r, http.StatusOK, "enroll_new", "base", EnrollNewPage{Page: s.page(r.Context(), p, fleet, "New enrollment", "enroll", ""), Form: form, Notes: s.pageNotes()})
}

// tokenList reads and shapes the token list; a read failure is the alarm state.
func (s *Service) tokenList(ctx context.Context, p *auth.Principal, q tokenQuery, bad []store.FieldError) TokenList {
	c := s.fc(ctx)
	list := TokenList{Filter: TokenFilter{Size: q.Size}}
	list.ClearHref = "/enroll#tokens"
	list.NewHref = "/enroll/new"
	if q.Customer != "" {
		list.NewHref += "?customer=" + url.QueryEscape(q.Customer)
	}
	custOpts, _, cerr := s.customerOptions(ctx, p, q.Customer, true, "All customers")
	if cerr != nil {
		s.log().Error("ui: token list customers failed", logx.Err(cerr))
		list.Err = true
		return list
	}
	list.Filter.Customers = custOpts
	list.Filter.Statuses = []Option{{Value: "", Label: "Any status", Selected: q.Status == ""}}
	for _, st := range store.TokenStatuses {
		list.Filter.Statuses = append(list.Filter.Statuses, Option{Value: st, Label: tokenStatusLabels[st], Selected: q.Status == st})
	}
	res, err := s.Store.ListEnrollmentTokenRows(ctx, p.Scope, store.EnrollTokenFilter{CustomerID: q.Customer, Status: q.Status, Page: q.Page, Size: q.Size})
	if err != nil {
		s.log().Error("ui: token list failed", logx.Err(err))
		list.Err = true
		return list
	}
	list.Total, list.All = res.Page.Total, res.All
	list.Segs = tokenSegs(q, res.Counts)
	for _, t := range res.Rows {
		list.Rows = append(list.Rows, s.enrollRow(t, c))
	}
	href := func(page, size int) string {
		v := q.values()
		v.Del("page")
		v.Del("size")
		if page > 1 {
			v.Set("page", strconv.Itoa(page))
		}
		if size != store.DefaultPageSize {
			v.Set("size", strconv.Itoa(size))
		}
		return listURL("/enroll", v) + "#tokens"
	}
	list.Pager = buildPager(res.Page, "tokens", href, nil)
	list.Summary = list.Pager.Text
	if q.Customer != "" {
		for _, o := range custOpts {
			if o.Value == q.Customer {
				list.Chips = append(list.Chips, FilterChip{Label: "Customer: " + o.Label, Href: listURL("/enroll", withoutKey(q.values(), "customer")) + "#tokens"})
			}
		}
	}
	if q.Status != "" {
		list.Chips = append(list.Chips, FilterChip{Label: "Status: " + tokenStatusLabels[q.Status], Href: listURL("/enroll", withoutKey(q.values(), "status")) + "#tokens"})
	}
	switch {
	case len(bad) > 0 && len(list.Rows) == 0:
		list.Empty = &EmptyView{Kind: "partial", Title: "Filters not valid", Body: bad[0].Message + " Clear the filters to see every token."}
	case res.All == 0:
		// First run: no token has ever been made. Neutral, the only neutral no-data case.
		list.Empty = &EmptyView{Kind: "neutral", Title: "No enrollment tokens yet", Body: "A token lets installers on a customer's machines enroll without anyone approving each device.",
			Link: &Link{Href: list.NewHref, Label: "New enrollment"}}
	case len(res.Rows) == 0 && q.filtered():
		list.Empty = &EmptyView{Kind: "neutral", Title: "No tokens match these filters", Body: fmt.Sprintf("0 of %d tokens match. Clear the filters to see them all.", res.All)}
	}
	return list
}

// tokenSegs are the status tabs over the table: every token, then each status.
func tokenSegs(q tokenQuery, counts map[string]int) []TokenSeg {
	href := func(status string) string {
		m := q
		m.Status, m.Page = status, 1
		return listURL("/enroll", m.values()) + "#tokens"
	}
	total := 0
	segs := make([]TokenSeg, 0, len(store.TokenStatuses)+1)
	for _, st := range store.TokenStatuses {
		total += counts[st]
		segs = append(segs, TokenSeg{Label: tokenStatusLabels[st], N: counts[st], Href: href(st), Current: q.Status == st})
	}
	return append([]TokenSeg{{Label: "All", N: total, Href: href(""), Current: q.Status == ""}}, segs...)
}

func withoutKey(v url.Values, key string) url.Values {
	v.Del(key)
	return v
}

// ---- create ----

func (s *Service) enrollCreate(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		s.errorPage(w, r, p, http.StatusBadRequest, "Form not read", "The form could not be read.")
		return
	}
	fleet, ok := s.fleetFor(w, r, p, "Enrollment")
	if !ok {
		return
	}
	get := func(k string) string { return strings.TrimSpace(r.PostFormValue(k)) }
	form := EnrollForm{CSRF: p.CSRF, CustomerID: get("customer_id"), NewCustomer: get("new_customer"), Label: get("label"), PolicyID: get("policy_id"),
		DeviceClass: get("device_class"), Expires: get("expires"), Uses: get("max_uses"), Errors: map[string]string{}, CanCreateCustomer: p.Scope.All}
	fail := func(field, msg string) {
		if form.Errors[field] == "" {
			form.Errors[field] = msg
		}
	}
	var ttl time.Duration
	for _, c := range expiryChoices {
		if c.Value == form.Expires {
			ttl = c.D
		}
	}
	if ttl == 0 {
		fail("expires", "Choose one of the listed lifetimes.")
	}
	var maxUses *int
	switch {
	case form.Uses == "unlimited":
	default:
		n, err := strconv.Atoi(form.Uses)
		known := false
		for _, c := range useChoices {
			known = known || c.Value == form.Uses
		}
		if err != nil || !known {
			fail("max_uses", "Choose one of the listed limits.")
		} else {
			maxUses = &n
		}
	}
	if form.DeviceClass != "unknown" && form.DeviceClass != "desktop" && form.DeviceClass != "laptop" {
		fail("device_class", "Choose desktop, laptop or not set.")
	}
	switch {
	case form.CustomerID != "" && form.NewCustomer != "":
		fail("customer_id", "Choose an existing customer or name a new one, not both.")
	case form.CustomerID == "" && form.NewCustomer == "":
		fail("customer_id", "Choose a customer, or name a new one.")
	case form.NewCustomer != "" && !p.Scope.All:
		fail("new_customer", "Creating a customer needs access to every customer. Choose an existing one.")
	}
	for _, fe := range store.ValidateEnrollTokenInput(store.EnrollTokenInput{CustomerID: "-", Label: form.Label, DeviceClass: form.DeviceClass, MaxUses: maxUses,
		ExpiresAt: s.Store.Now().Add(max(ttl, time.Minute))}, s.Store.Now()) {
		fail(fe.Field, fe.Message)
	}
	if len(form.Errors) > 0 {
		s.enrollRejected(w, r, p, fleet, form)
		return
	}

	expiresAt := s.Store.Now().Add(ttl)
	custID, newCustomer := form.CustomerID, false
	var cust store.Customer
	if form.NewCustomer != "" {
		// A customer made here is expected to have devices; it reads ONBOARDING
		// (not MISSING) until the token that enrolls them expires.
		c, err := s.Store.CreateCustomerAudited(r.Context(), p.Scope, store.CustomerInput{Name: form.NewCustomer, Expected: true, OnboardingUntil: &expiresAt}, s.actor(r, p))
		if err != nil {
			if fes := store.FieldErrors(err); len(fes) > 0 {
				for _, fe := range fes {
					key := fe.Field
					if key == "name" || key == "slug" {
						key = "new_customer"
					}
					fail(key, fe.Message)
				}
				s.enrollRejected(w, r, p, fleet, form)
				return
			}
			s.storeFailed(w, r, p, err)
			return
		}
		cust, custID, newCustomer = c, c.ID, true
		s.changed()
	}
	plain, hash, err := enroll.NewToken()
	if err != nil {
		s.log().Error("ui: token generation failed", logx.Class("random"))
		s.errorPage(w, r, p, http.StatusInternalServerError, "Not created", "The token could not be generated. Nothing was created. Try again.")
		return
	}
	tok, err := s.Store.CreateEnrollmentTokenAudited(r.Context(), p.Scope, store.EnrollTokenInput{CustomerID: custID, Label: form.Label, PolicyID: form.PolicyID,
		DeviceClass: form.DeviceClass, ExpiresAt: expiresAt, MaxUses: maxUses, TokenHash: hash}, s.actor(r, p))
	if err != nil {
		if fes := store.FieldErrors(err); len(fes) > 0 {
			for _, fe := range fes {
				fail(fe.Field, fe.Message)
			}
			if newCustomer {
				// The customer exists now; keep the wizard on it so a retry does not create a second.
				form.CustomerID, form.NewCustomer = custID, ""
			}
			s.enrollRejected(w, r, p, fleet, form)
			return
		}
		if newCustomer {
			// storeFailed says "nothing was changed", which is not true here: say what was.
			s.log().Error("ui: token create failed after the customer was created", logx.Err(err))
			s.errorPage(w, r, p, http.StatusInternalServerError, "Token not created",
				"The new customer was created, but its token was not (the change could not be saved or audited). Choose the customer in the form and try again.")
			return
		}
		s.storeFailed(w, r, p, err)
		return
	}
	row, err := s.Store.GetEnrollmentTokenRow(r.Context(), p.Scope, tok.ID)
	if err != nil {
		// The token exists and its secret is lost with this response: say so.
		s.log().Error("ui: created token could not be read back", logx.Err(err))
		s.errorPage(w, r, p, http.StatusInternalServerError, "Token not shown",
			"The token was created but could not be displayed, so its value is lost. Revoke it from the token list and create another.")
		return
	}
	if !newCustomer {
		cust, _ = s.Store.GetCustomer(r.Context(), p.Scope, custID)
	}
	c := s.fc(r.Context())
	uses := "no limit on enrollments"
	if row.MaxUses != nil {
		uses = fmt.Sprintf("enrolls up to %d device%s", *row.MaxUses, map[bool]string{true: "", false: "s"}[*row.MaxUses == 1])
	}
	created := &CreatedToken{Token: plain, ID: tok.ID, Customer: row.CustomerName, CustomerID: custID, NewCustomer: newCustomer, Label: row.Label,
		Expires: c.dateClock(row.ExpiresAt), ExpiresRel: untilText(row.ExpiresAt.Sub(c.Now)), Uses: uses, Class: row.DeviceClassDefault, Policy: row.PolicyName}
	if created.Policy == "" {
		created.Policy = "The customer's policy"
	} else {
		created.PolicyNote = tokenPolicyNote
	}
	if cust.KeyCustody != "" && cust.KeyCustody != "escrowed" {
		created.Custody = "This customer holds its own repository key. The console refuses to enroll devices of such a customer until customer-held custody ships, so this token will be rejected at enrollment."
	}
	msi, msiErr := adminapi.CurrentMSI(s.ArtifactsDir)
	created.MSI = s.msiView(msi, msiErr, c)
	created.Snips = adminapi.GenerateSnippets(adminapi.SnippetInput{PublicURL: s.PublicURL, Token: plain, MSI: msi})

	pg := EnrollPage{Page: s.page(r.Context(), p, fleet, "Enrollment token created", "enroll", ""), Notes: s.pageNotes(), Created: created}
	h := w.Header()
	h.Set("Pragma", "no-cache")
	s.render(w, r, http.StatusOK, "enroll", "base", pg)
}

// enrollRejected re-renders the wizard with the field errors and every value kept.
func (s *Service) enrollRejected(w http.ResponseWriter, r *http.Request, p *auth.Principal, fleet health.Fleet, form EnrollForm) {
	if err := s.fillEnrollOptions(r.Context(), p, &form); err != nil {
		s.log().Error("ui: enroll form failed", logx.Err(err))
		s.errorPage(w, r, p, http.StatusServiceUnavailable, "Enrollment unavailable", "The customers could not be read. Try again shortly.")
		return
	}
	form.finish()
	s.render(w, r, http.StatusBadRequest, "enroll_new", "base", EnrollNewPage{Page: s.page(r.Context(), p, fleet, "New enrollment", "enroll", ""), Form: form, Notes: s.pageNotes()})
}

// ---- detail and revoke ----

// TokenDevice is a device that enrolled through the token.
type TokenDevice struct {
	ID       string
	Hostname string
	Enrolled string
}

// EnrollTokenPage is /enroll/tokens/{id}.
type EnrollTokenPage struct {
	Page
	Token      EnrollRow
	TokenID    string
	Saved      string
	Facts      []Fact
	Devices    []TokenDevice
	DevicesErr bool
	CanRevoke  bool
	CanDelete  bool
	// Retired are the token's retired devices, collapsed under "Retired (n)".
	Retired *RetiredFold
}

func (s *Service) enrollTokenRowFor(w http.ResponseWriter, r *http.Request, p *auth.Principal) (store.EnrollTokenRow, bool) {
	row, err := s.Store.GetEnrollmentTokenRow(r.Context(), p.Scope, r.PathValue("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.notFound(w, r, p, "This enrollment token")
			return store.EnrollTokenRow{}, false
		}
		s.log().Error("ui: token read failed", logx.Err(err))
		s.errorPage(w, r, p, http.StatusServiceUnavailable, "Token unavailable", "The token could not be read. Try again shortly.")
		return store.EnrollTokenRow{}, false
	}
	return row, true
}

func (s *Service) enrollTokenPage(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	fleet, ok := s.fleetFor(w, r, p, "Enrollment token")
	if !ok {
		return
	}
	row, ok := s.enrollTokenRowFor(w, r, p)
	if !ok {
		return
	}
	s.renderEnrollToken(w, r, p, fleet, row, http.StatusOK)
}

func (s *Service) renderEnrollToken(w http.ResponseWriter, r *http.Request, p *auth.Principal, fleet health.Fleet, row store.EnrollTokenRow, status int) {
	c := s.fc(r.Context())
	tr := s.enrollRow(row, c)
	title := tr.Label
	if row.Label == "" {
		title = "Enrollment token"
	}
	pg := EnrollTokenPage{Page: s.page(r.Context(), p, fleet, title, "enroll", ""), Token: tr, TokenID: row.ID, Saved: enrollSavedText(r.URL.Query().Get("saved")), CanRevoke: tr.CanRevoke, CanDelete: tr.CanDelete}
	chip := tr.Status
	pg.Facts = []Fact{
		{Key: "Status", Chip: &chip},
		{Key: "Customer", Value: tr.Customer},
		{Key: "Uses", Value: tr.Uses + " (" + tr.UsesNote + ")"},
		{Key: "Expires", Value: tr.Expires + ", " + tr.ExpiresRel},
		{Key: "Device class", Value: strings.ToUpper(row.DeviceClassDefault[:1]) + row.DeviceClassDefault[1:]},
		{Key: "Policy", Value: tr.Policy + tokenPolicySuffix(row.PolicyName)},
		{Key: "Created", Value: tr.Created + " by " + tr.CreatedBy},
		{Key: "Last used", Value: tr.LastUsed + func() string {
			if tr.LastRel != "" {
				return " (" + tr.LastRel + ")"
			}
			return ""
		}()},
		{Key: "Token ID", Value: row.ID, Mono: true},
	}
	if row.RevokedAt != nil {
		pg.Facts = append(pg.Facts, Fact{Key: "Revoked", Value: c.dateClock(*row.RevokedAt)})
	}
	if tr.Deleted != "" {
		pg.Facts = append(pg.Facts, Fact{Key: "Deleted", Value: tr.Deleted + " (hidden from the token list; kept for the audit trail and the devices below)"})
	}
	devs, err := s.Store.ListEnrollmentTokenDevices(r.Context(), p.Scope, row.ID)
	if err != nil {
		s.log().Error("ui: token devices failed", logx.Err(err))
		pg.DevicesErr = true
	}
	var retired []RetiredRow
	for _, d := range devs {
		if d.Retired {
			retired = append(retired, RetiredRow{ID: d.ID, Host: d.Hostname, CustomerID: row.CustomerID})
			continue
		}
		pg.Devices = append(pg.Devices, TokenDevice{ID: d.ID, Hostname: d.Hostname, Enrolled: c.dateClock(d.EnrolledAt)})
	}
	if len(retired) > 0 {
		pg.Retired = &RetiredFold{N: len(retired), Rows: retired, Chip: retiredChip()}
	}
	s.render(w, r, status, "enroll_token", "base", pg)
}

func (s *Service) enrollRevoke(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if err := r.ParseForm(); err != nil {
		s.errorPage(w, r, p, http.StatusBadRequest, "Form not read", "The form could not be read.")
		return
	}
	id := r.PathValue("id")
	changed, err := s.Store.RevokeEnrollmentTokenAudited(r.Context(), p.Scope, id, s.actor(r, p))
	if err != nil {
		s.storeFailed(w, r, p, err)
		return
	}
	code := "revoked"
	if !changed {
		code = "already_revoked"
	}
	dest := "/enroll/tokens/" + url.PathEscape(id)
	if r.PostFormValue("return") == "list" {
		dest = "/enroll"
	}
	http.Redirect(w, r, dest+"?saved="+code, http.StatusSeeOther)
}

// enrollDelete hides a revoked or expired token (M3-T6) through POST
// /api/v1/enrollment-tokens/{id}/delete: the one handler that decides and audits.
func (s *Service) enrollDelete(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if err := r.ParseForm(); err != nil {
		s.errorPage(w, r, p, http.StatusBadRequest, "Form not read", "The form could not be read.")
		return
	}
	id := r.PathValue("id")
	status, _, err := s.apiSend(r, http.MethodPost, "/api/v1/enrollment-tokens/"+url.PathEscape(id)+"/delete", nil)
	if s.writeFailed(w, r, p, status, err) {
		return
	}
	switch status {
	case http.StatusNoContent:
		http.Redirect(w, r, "/enroll?saved=deleted", http.StatusSeeOther)
	case http.StatusConflict:
		http.Redirect(w, r, "/enroll?saved=not_deletable", http.StatusSeeOther)
	case http.StatusNotFound:
		s.notFound(w, r, p, "This enrollment token")
	default:
		s.log().Error("ui: token delete failed", logx.Class("render"), logx.Int("status", status))
		s.errorPage(w, r, p, http.StatusInternalServerError, "Not deleted", "The token could not be deleted. Nothing was changed.")
	}
}

// tokenPolicyNote says what a policy named on a token does (M2-W3B). It is shown
// only for a token that names one; a token without keeps the customer's policy.
const tokenPolicyNote = "Applied to each device that enrolls with this token, on top of the customer's policy. It shows on the device's Policy tab as an override."

// tokenPolicySuffix is the detail page's short form of tokenPolicyNote.
func tokenPolicySuffix(policyName string) string {
	if policyName == "" {
		return ""
	}
	return " (applied to each device at enrollment, on top of the customer's policy)"
}
