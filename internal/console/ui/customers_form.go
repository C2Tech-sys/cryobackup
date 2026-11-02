package ui

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/health"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// The customer form (S04 create, the Settings tab of S05). The same form and the
// same validators serve both; the admin API shares the store functions, so a
// page and an API call can never accept different input.

// ContactRow is one row of the contacts fieldset.
type ContactRow struct {
	Name, Email, Phone, Role string
}

// CustForm is the customer form with its values and errors.
type CustForm struct {
	Action     string
	New        bool
	ReadOnly   bool
	CSRF       string
	Name       string
	Slug       string
	Expected   bool
	Onboarding string // datetime-local value in the display zone
	TZ         string
	DeviceCap  string
	PolicyID   string
	Notes      string
	Contacts   []ContactRow
	Policies   []Option
	Zone       string
	Errors     map[string]string
	// Summary lists every error for the top of the form (components 2.4).
	Summary []store.FieldError
	Cancel  string
	// PolicyHref is the customer's Policy tab (admins), where the assignment is changed.
	PolicyHref string
}

// Err returns the error text of a field ("" when it is fine).
func (f CustForm) Err(field string) string { return f.Errors[field] }

// HasErr reports whether a field was rejected.
func (f CustForm) HasErr(field string) bool { return f.Errors[field] != "" }

const onboardingLayout = "2006-01-02T15:04"

func (s *Service) newCustForm(ctx context.Context, p *auth.Principal, loc *time.Location, row *store.CustomerRow, choices []store.PolicyChoice) CustForm {
	f := CustForm{CSRF: p.CSRF, Zone: s.zone(ctx), ReadOnly: p.Role < auth.RoleAdmin, Errors: map[string]string{}}
	sel := ""
	if row != nil {
		f.Name, f.Slug, f.Expected, f.TZ, f.Notes, sel = row.Name, row.Slug, row.Expected, row.DisplayTZ, row.Notes, row.PolicyID
		if row.OnboardingUntil != nil {
			f.Onboarding = row.OnboardingUntil.In(loc).Format(onboardingLayout)
		}
		if row.DeviceCap != nil {
			f.DeviceCap = strconv.Itoa(*row.DeviceCap)
		}
		for _, ct := range store.ParseContacts(row.Contacts) {
			f.Contacts = append(f.Contacts, ContactRow(ct))
		}
	}
	f.PolicyID = sel
	if row != nil && p.Role >= auth.RoleAdmin {
		f.PolicyHref = "/customers/" + row.ID + "/policy"
	}
	f.Policies = []Option{{Value: "", Label: "Default policy", Selected: sel == ""}}
	for _, ch := range choices {
		lbl := ch.Name + " (template)"
		if ch.Scope == "customer" {
			lbl = ch.Name + " (this customer)"
		}
		f.Policies = append(f.Policies, Option{Value: ch.ID, Label: lbl, Selected: ch.ID == sel})
	}
	f.padContacts()
	return f
}

// padContacts keeps one empty row at the end (two on a new customer) so another
// contact can always be added without a script.
func (f *CustForm) padContacts() {
	want := 1
	if f.New {
		want = 2
	}
	blank := 0
	for _, c := range f.Contacts {
		if c == (ContactRow{}) {
			blank++
		}
	}
	for ; blank < want && len(f.Contacts) < store.MaxContacts; blank++ {
		f.Contacts = append(f.Contacts, ContactRow{})
	}
}

// readCustForm fills a form from a posted request and parses it into the store's
// types. Parse errors land on the form with their field.
func (s *Service) readCustForm(r *http.Request, loc *time.Location, f *CustForm, cur *store.CustomerRow) (store.CustomerInput, store.CustomerPatch) {
	get := func(k string) string { return strings.TrimSpace(r.PostFormValue(k)) }
	f.Name, f.Slug, f.TZ, f.DeviceCap = get("name"), get("slug"), get("display_tz"), get("device_cap")
	// M2-W2A: an existing customer's policy changes only through the Policy tab (blast-radius review); the form carries an initial policy on create only.
	if f.New {
		f.PolicyID = get("policy_id")
	}
	f.Notes = strings.TrimSpace(strings.ReplaceAll(r.PostFormValue("notes"), "\r\n", "\n"))
	f.Expected = r.PostFormValue("expected") != ""
	f.Onboarding = get("onboarding_until")
	f.Contacts = nil
	names, emails, phones, roles := r.PostForm["contact_name"], r.PostForm["contact_email"], r.PostForm["contact_phone"], r.PostForm["contact_role"]
	var contacts []store.Contact
	for i := range names {
		row := ContactRow{Name: strings.TrimSpace(names[i])}
		if i < len(emails) {
			row.Email = strings.TrimSpace(emails[i])
		}
		if i < len(phones) {
			row.Phone = strings.TrimSpace(phones[i])
		}
		if i < len(roles) {
			row.Role = strings.TrimSpace(roles[i])
		}
		f.Contacts = append(f.Contacts, row)
		if row != (ContactRow{}) { // a fully blank row is not a contact
			contacts = append(contacts, store.Contact(row))
		}
	}
	if len(f.Contacts) > store.MaxContacts+2 {
		f.Contacts = f.Contacts[:store.MaxContacts+2]
	}
	fail := func(field, msg string) { f.Errors[field] = msg }
	var until *time.Time
	if f.Onboarding != "" {
		t, err := time.ParseInLocation(onboardingLayout, f.Onboarding, loc)
		if err != nil {
			t, err = time.ParseInLocation("2006-01-02T15:04:05", f.Onboarding, loc)
		}
		if err != nil {
			fail("onboarding_until", "Use a date and time, for example 2026-12-01 5:00 PM.")
		} else {
			u := t.UTC()
			until = &u
			// The form shows the stored time to the minute. Submitting that same minute
			// is not an edit: keep the stored instant (and its seconds) so a notes-only
			// save does not rewrite the window or audit it as changed.
			if cur != nil && cur.OnboardingUntil != nil && cur.OnboardingUntil.In(loc).Format(onboardingLayout) == t.Format(onboardingLayout) {
				until = cur.OnboardingUntil
			}
		}
	}
	var capPtr *int
	if f.DeviceCap != "" {
		n, err := strconv.Atoi(f.DeviceCap)
		if err != nil {
			fail("device_cap", "Use a whole number, or leave it empty for no cap.")
		} else {
			capPtr = &n
		}
	}
	in := store.CustomerInput{Name: f.Name, Slug: f.Slug, Expected: f.Expected, OnboardingUntil: until, DisplayTZ: f.TZ, Contacts: contacts,
		DeviceCap: capPtr, PolicyID: f.PolicyID, Notes: f.Notes}
	patch := store.CustomerPatch{Name: &f.Name, Slug: &f.Slug, Expected: &f.Expected, DisplayTZ: &f.TZ, Contacts: &contacts, Notes: &f.Notes}
	if until != nil {
		patch.OnboardingUntil = until
	} else {
		patch.ClearOnboarding = true
	}
	if capPtr != nil {
		patch.DeviceCap = capPtr
	} else {
		patch.ClearDeviceCap = true
	}
	// Everything that can be checked without the database is checked now, so one
	// post shows every field error, not one per attempt.
	for _, fe := range store.ValidateCustomerInput(in) {
		if f.Errors[fe.Field] == "" {
			f.Errors[fe.Field] = fe.Message
		}
	}
	return in, patch
}

// absorb records the store's field errors on the form; it reports whether err
// was a form error (otherwise the caller answers with an error page).
func (f *CustForm) absorb(err error) bool {
	fe := store.FieldErrors(err)
	if len(fe) == 0 {
		return false
	}
	for _, e := range fe {
		if f.Errors[e.Field] == "" {
			f.Errors[e.Field] = e.Message
		}
	}
	return true
}

func (f *CustForm) finish() {
	order := []string{"name", "slug", "expected", "onboarding_until", "display_tz", "device_cap", "policy_id", "contacts", "notes"}
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
	f.padContacts()
}

// NewCustomerPage is the create page.
type NewCustomerPage struct {
	Page
	Form CustForm
}

func (s *Service) customerNew(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	fleet, ok := s.fleetFor(w, r, p, "New customer")
	if !ok {
		return
	}
	choices, err := s.Store.PolicyChoicesFor(r.Context(), p.Scope, "")
	if err != nil {
		s.storeFailed(w, r, p, err)
		return
	}
	f := s.newCustForm(r.Context(), p, s.loc(r.Context()), nil, choices)
	f.New, f.Action, f.Cancel = true, "/ui/customers", "/customers"
	f.padContacts()
	s.render(w, r, http.StatusOK, "customer_new", "base", NewCustomerPage{Page: s.page(r.Context(), p, fleet, "New customer", "customers", ""), Form: f})
}

func (s *Service) customerCreate(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		s.errorPage(w, r, p, http.StatusBadRequest, "Form not read", "The form could not be read.")
		return
	}
	fleet, ok := s.fleetFor(w, r, p, "New customer")
	if !ok {
		return
	}
	choices, err := s.Store.PolicyChoicesFor(r.Context(), p.Scope, "")
	if err != nil {
		s.storeFailed(w, r, p, err)
		return
	}
	f := s.newCustForm(r.Context(), p, s.loc(r.Context()), nil, choices)
	f.New, f.Action, f.Cancel = true, "/ui/customers", "/customers"
	in, _ := s.readCustForm(r, s.loc(r.Context()), &f, nil)
	f.markPolicy()
	if len(f.Errors) == 0 {
		var c store.Customer
		if c, err = s.Store.CreateCustomerAudited(r.Context(), p.Scope, in, s.actor(r, p)); err == nil {
			s.changed()
			http.Redirect(w, r, "/customers/"+c.ID+"?saved=created", http.StatusSeeOther)
			return
		}
		if !f.absorb(err) {
			s.storeFailed(w, r, p, err)
			return
		}
	}
	f.finish()
	s.render(w, r, http.StatusBadRequest, "customer_new", "base", NewCustomerPage{Page: s.page(r.Context(), p, fleet, "New customer", "customers", ""), Form: f})
}

// markPolicy keeps the posted policy selected after an error.
func (f *CustForm) markPolicy() {
	for i := range f.Policies {
		f.Policies[i].Selected = f.Policies[i].Value == f.PolicyID
	}
}

func (s *Service) customerSettings(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	fleet, ok := s.fleetFor(w, r, p, "Customer")
	if !ok {
		return
	}
	row, ok := s.customerRow(w, r, p)
	if !ok {
		return
	}
	choices, err := s.Store.PolicyChoicesFor(r.Context(), p.Scope, row.ID)
	if err != nil {
		s.storeFailed(w, r, p, err)
		return
	}
	f := s.newCustForm(r.Context(), p, s.loc(r.Context()), &row, choices)
	f.Action = "/ui/customers/" + row.ID
	s.renderCustomerSettings(w, r, p, fleet, row, f, http.StatusOK, savedText(r.URL.Query().Get("saved")))
}

func (s *Service) renderCustomerSettings(w http.ResponseWriter, r *http.Request, p *auth.Principal, fleet health.Fleet, row store.CustomerRow, f CustForm, status int, saved string) {
	c := s.fcAt(r.Context(), fleet.Computed)
	pg := CustomerPage{Page: s.page(r.Context(), p, fleet, row.Name, "customers", "/ui/events"), H: s.custHead(p, row, rollupOf(fleet, row.ID), c, "settings"), Tab: "settings", Form: &f, Saved: saved}
	s.render(w, r, status, "customer", "base", pg)
}

func (s *Service) customerUpdate(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		s.errorPage(w, r, p, http.StatusBadRequest, "Form not read", "The form could not be read.")
		return
	}
	fleet, ok := s.fleetFor(w, r, p, "Customer")
	if !ok {
		return
	}
	row, ok := s.customerRow(w, r, p)
	if !ok {
		return
	}
	choices, err := s.Store.PolicyChoicesFor(r.Context(), p.Scope, row.ID)
	if err != nil {
		s.storeFailed(w, r, p, err)
		return
	}
	f := s.newCustForm(r.Context(), p, s.loc(r.Context()), &row, choices)
	f.Action = "/ui/customers/" + row.ID
	_, patch := s.readCustForm(r, s.loc(r.Context()), &f, &row)
	f.markPolicy()
	if len(f.Errors) == 0 {
		_, changed, err := s.Store.UpdateCustomerAudited(r.Context(), p.Scope, row.ID, patch, s.actor(r, p))
		if err == nil {
			if changed {
				s.changed()
			}
			http.Redirect(w, r, "/customers/"+row.ID+"/settings?saved=1", http.StatusSeeOther)
			return
		}
		if !f.absorb(err) {
			s.storeFailed(w, r, p, err)
			return
		}
	}
	f.finish()
	s.renderCustomerSettings(w, r, p, fleet, row, f, http.StatusBadRequest, "")
}
