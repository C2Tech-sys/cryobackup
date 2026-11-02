package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	_ "time/tzdata" // a customer's display zone must resolve on hosts without an OS zone database

	"github.com/c2tech-sys/cryobackup/internal/state"
)

// This file is the M2 customer surface (ticket M2-W1C): the list with rollups,
// validated create and edit with the audit row in the same transaction, and the
// pure merge that joins the list with the state engine's customer rollups. The
// M0 CreateCustomer / UpdateCustomer in customers.go are untouched; the helpers
// they share (customerCols, scanCustomer, checkCustomerPolicy, Slugify) are reused.

// Contact is one person to reach at a customer. The column holds a JSON array
// of these (the CLI and fixtures write the same shape).
type Contact struct {
	Name  string `json:"name"`
	Email string `json:"email,omitempty"`
	Phone string `json:"phone,omitempty"`
	Role  string `json:"role,omitempty"`
}

// MaxContacts bounds the contacts of one customer.
const MaxContacts = 20

// ParseContacts decodes customers.contacts. Anything that is not an array of
// objects reads as no contacts rather than failing a page: the column is
// operator data and a page must still render.
func ParseContacts(raw json.RawMessage) []Contact {
	var out []Contact
	if len(raw) == 0 || json.Unmarshal(raw, &out) != nil {
		return nil
	}
	return out
}

// CustomerInput is a new customer. Storage target and key custody are owner
// level choices and optional here: the default storage target and escrowed
// custody apply when they are empty.
type CustomerInput struct {
	Name            string
	Slug            string // derived from Name when empty
	Expected        bool
	OnboardingUntil *time.Time
	DisplayTZ       string // IANA zone; "" = the global display zone
	Contacts        []Contact
	DeviceCap       *int
	PolicyID        string // "" = the default policy applies
	Notes           string
	StorageTargetID string
	KeyCustody      string
}

// CustomerPatch is a set of optional changes to a customer; nil fields are left
// alone. An empty string clears DisplayTZ, PolicyID and Notes; the Clear flags
// clear the optional onboarding window and device cap.
type CustomerPatch struct {
	Name            *string
	Slug            *string
	Expected        *bool
	OnboardingUntil *time.Time
	ClearOnboarding bool
	DisplayTZ       *string
	Contacts        *[]Contact
	DeviceCap       *int
	ClearDeviceCap  bool
	PolicyID        *string
	Notes           *string
}

// MaxDeviceCap bounds a customer's device cap (a sanity limit, not a quota).
const MaxDeviceCap = 100000

func validSlug(s string) bool {
	if s == "" || len(s) > 63 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

// normalizeCustomer validates and normalises the editable fields of c in place
// and returns one FieldError per rejected field.
func normalizeCustomer(c *Customer, contacts []Contact) []FieldError {
	var errs []FieldError
	name, ok := cleanLine(c.Name, 200)
	switch {
	case name == "":
		errs = append(errs, fieldf("name", "Enter the customer name."))
	case !ok:
		errs = append(errs, fieldf("name", "The name must be one line of at most 200 characters."))
	}
	c.Name = name
	c.Slug = strings.TrimSpace(c.Slug)
	if c.Slug == "" {
		c.Slug = Slugify(c.Name)
	}
	if !validSlug(c.Slug) {
		errs = append(errs, fieldf("slug", "Use 1 to 63 lower-case letters, digits and hyphens."))
	}
	c.DisplayTZ = strings.TrimSpace(c.DisplayTZ)
	if c.DisplayTZ != "" {
		if _, err := time.LoadLocation(c.DisplayTZ); err != nil || c.DisplayTZ == "Local" {
			errs = append(errs, fieldf("display_tz", "Use an IANA zone name such as America/Chicago, or leave it empty for the console's zone."))
		}
	}
	if c.DeviceCap != nil && (*c.DeviceCap < 1 || *c.DeviceCap > MaxDeviceCap) {
		errs = append(errs, fieldf("device_cap", "Use a whole number from 1 to %d, or leave it empty for no cap.", MaxDeviceCap))
	}
	if n, ok := cleanText(c.Notes, 2000); !ok {
		errs = append(errs, fieldf("notes", "Notes are limited to 2000 characters of plain text."))
	} else {
		c.Notes = n
	}
	if len(contacts) > MaxContacts {
		errs = append(errs, fieldf("contacts", "At most %d contacts.", MaxContacts))
	}
	for i := range contacts {
		ct := &contacts[i]
		var okName, okEmail, okPhone, okRole bool
		ct.Name, okName = cleanLine(ct.Name, 200)
		ct.Email, okEmail = cleanLine(ct.Email, 254)
		ct.Phone, okPhone = cleanLine(ct.Phone, 40)
		ct.Role, okRole = cleanLine(ct.Role, 100)
		switch {
		case ct.Name == "":
			errs = append(errs, fieldf("contacts", "Every contact needs a name."))
		case !okName || !okEmail || !okPhone || !okRole:
			errs = append(errs, fieldf("contacts", "A contact detail is too long or not plain text."))
		case ct.Email != "" && !plausibleEmail(ct.Email):
			errs = append(errs, fieldf("contacts", "A contact's email address is not valid."))
		}
		if len(errs) > 0 && errs[len(errs)-1].Field == "contacts" {
			break // one contacts message at a time
		}
	}
	return errs
}

func plausibleEmail(s string) bool {
	at := strings.LastIndex(s, "@")
	if at < 1 || at == len(s)-1 || strings.ContainsAny(s, " \t<>,;") {
		return false
	}
	return strings.Contains(s[at+1:], ".") && !strings.HasSuffix(s, ".")
}

func contactsJSON(contacts []Contact) (string, error) {
	if contacts == nil {
		contacts = []Contact{}
	}
	b, err := json.Marshal(contacts)
	return string(b), err
}

// policyFieldError turns a refused policy assignment into the policy_id field
// error with one fixed sentence, whatever the reason was (unknown id, another
// customer's policy): the answer must not reveal which policies exist.
func policyFieldError(err error) error {
	if errors.Is(err, ErrInvalid) {
		return validation([]FieldError{fieldf("policy_id", "Choose one of the listed policies.")})
	}
	return err
}

// checkCustomerUnique reports a duplicate name (case-insensitive) or slug as a
// field error on the field that collides, before the INSERT or UPDATE runs, so
// a renamed duplicate is a name error and not a confusing slug error. exceptID
// is the customer being edited ("" on create).
func checkCustomerUnique(tx *Tx, exceptID, name, slug string) error {
	var errs []FieldError
	var one int
	err := tx.QueryRow(`SELECT 1 FROM customers WHERE name = ? COLLATE NOCASE AND id <> ?`, name, exceptID).Scan(&one)
	if err == nil {
		errs = append(errs, fieldf("name", "Another customer already has this name."))
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	err = tx.QueryRow(`SELECT 1 FROM customers WHERE slug = ? AND id <> ?`, slug, exceptID).Scan(&one)
	if err == nil {
		errs = append(errs, fieldf("slug", "Another customer already uses this slug."))
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	return validation(errs)
}

// constraintFields maps a unique-constraint failure on customers to the field.
func constraintFields(err error) error {
	var ce *ConstraintError
	if errors.As(err, &ce) && ce.Kind == ErrConflict {
		switch {
		case strings.Contains(ce.Constraint, "customers.slug"):
			return validation([]FieldError{fieldf("slug", "Another customer already uses this slug.")})
		case strings.Contains(ce.Constraint, "customers.name"):
			return validation([]FieldError{fieldf("name", "Another customer already has this name.")})
		}
	}
	return err
}

// ValidateCustomerInput checks the editable fields of a customer without
// touching the database, so a page can show every field error at once (the
// uniqueness of the name and slug is only known to the store, at write time).
func ValidateCustomerInput(in CustomerInput) []FieldError {
	c := Customer{Name: in.Name, Slug: in.Slug, DisplayTZ: in.DisplayTZ, DeviceCap: in.DeviceCap, Notes: in.Notes}
	return normalizeCustomer(&c, append([]Contact(nil), in.Contacts...))
}

// CreateCustomerAudited creates a customer and writes its customer.create audit
// row in the same transaction. Creating a customer is a global act: it needs
// scope.All (ErrScope otherwise). Rejected input is a *ValidationError (an
// ErrInvalid); a duplicate name or slug is a *ValidationError on that field.
func (s *Store) CreateCustomerAudited(ctx context.Context, scope Scope, in CustomerInput, actor AuditActor) (Customer, error) {
	if err := scope.check(); err != nil {
		return Customer{}, err
	}
	if !scope.All {
		return Customer{}, ErrScope
	}
	c := Customer{Name: in.Name, Slug: in.Slug, State: "active", Expected: in.Expected, OnboardingUntil: in.OnboardingUntil,
		DisplayTZ: in.DisplayTZ, DeviceCap: in.DeviceCap, PolicyID: in.PolicyID, Notes: in.Notes,
		StorageTargetID: in.StorageTargetID, KeyCustody: in.KeyCustody}
	contacts := append([]Contact(nil), in.Contacts...)
	errs := normalizeCustomer(&c, contacts)
	if c.KeyCustody == "" {
		c.KeyCustody = "escrowed"
	}
	if c.KeyCustody != "escrowed" && c.KeyCustody != "customer_held" {
		errs = append(errs, fieldf("key_custody", "Use escrowed or customer_held."))
	}
	if err := validation(errs); err != nil {
		return Customer{}, err
	}
	contactsText, err := contactsJSON(contacts)
	if err != nil {
		return Customer{}, err
	}
	var out Customer
	err = s.Write(ctx, func(tx *Tx) error {
		c.ID = tx.id()
		now := tx.nowMS()
		if c.StorageTargetID == "" {
			err := tx.QueryRow(`SELECT id FROM storage_targets WHERE is_default = 1`).Scan(&c.StorageTargetID)
			if errors.Is(err, sql.ErrNoRows) {
				return invalidf("no storage target is the default")
			}
			if err != nil {
				return err
			}
		} else {
			var one int
			if err := tx.QueryRow(`SELECT 1 FROM storage_targets WHERE id = ?`, c.StorageTargetID).Scan(&one); errors.Is(err, sql.ErrNoRows) {
				return validation([]FieldError{fieldf("storage_target_id", "Choose one of the configured storage targets.")})
			} else if err != nil {
				return err
			}
		}
		if err := checkCustomerUnique(tx, "", c.Name, c.Slug); err != nil {
			return err
		}
		if err := checkCustomerPolicy(tx, c.ID, c.PolicyID); err != nil {
			return policyFieldError(err)
		}
		if _, err := tx.Exec(`INSERT INTO customers (`+customerCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			c.ID, c.Name, c.Slug, c.State, boolInt(c.Expected), nullTime(c.OnboardingUntil), contactsText, nullStr(c.PolicyID),
			c.StorageTargetID, nullStr(c.DisplayTZ), nullInt(c.DeviceCap), c.KeyCustody, nullStr(c.Notes), nil, now, now); err != nil {
			return constraintFields(mapErr(err))
		}
		ev, err := actor.event("customer.create", "customer", c.ID, c.ID, map[string]any{"expected": c.Expected})
		if err != nil {
			return err
		}
		if err := audited(tx, ev); err != nil {
			return err
		}
		out, err = one(tx, `SELECT `+customerCols+` FROM customers WHERE id = ?`, []any{c.ID}, scanCustomer)
		return err
	})
	if err != nil {
		return Customer{}, err
	}
	return out, nil
}

func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

// UpdateCustomerAudited applies p to a customer inside scope and writes a
// customer.update audit row in the same transaction, naming the changed fields
// (and the before and after of the expected flag, the field that moves the
// MISSING alarm). It reports whether anything changed: a patch that changes
// nothing writes no audit row, because nothing was mutated. An archived
// customer is not editable (ErrConflict). A scoped caller may edit the
// customers it can see, but not rename them (name and slug are unique across all
// customers) and not change the device cap (owner-level): both are ErrScope.
func (s *Store) UpdateCustomerAudited(ctx context.Context, scope Scope, id string, p CustomerPatch, actor AuditActor) (Customer, bool, error) {
	if err := scope.check(); err != nil {
		return Customer{}, false, err
	}
	if !scope.Allows(id) {
		return Customer{}, false, ErrNotFound
	}
	var out Customer
	changed := false
	err := s.Write(ctx, func(tx *Tx) error {
		cur, err := one(tx, `SELECT `+customerCols+` FROM customers WHERE id = ?`, []any{id}, scanCustomer)
		if err != nil {
			return err
		}
		if cur.State == "archived" {
			return conflictf("customer is archived")
		}
		next := cur
		if p.Name != nil {
			next.Name = *p.Name
		}
		if p.Slug != nil {
			next.Slug = *p.Slug
		}
		if p.Expected != nil {
			next.Expected = *p.Expected
		}
		switch {
		case p.ClearOnboarding:
			next.OnboardingUntil = nil
		case p.OnboardingUntil != nil:
			t := p.OnboardingUntil.UTC()
			next.OnboardingUntil = &t
		}
		if p.DisplayTZ != nil {
			next.DisplayTZ = *p.DisplayTZ
		}
		switch {
		case p.ClearDeviceCap:
			next.DeviceCap = nil
		case p.DeviceCap != nil:
			v := *p.DeviceCap
			next.DeviceCap = &v
		}
		if p.PolicyID != nil {
			// A policy moves the customer's devices to other settings, so it changes
			// only through AssignCustomerPolicyAudited (blast-radius preview + token).
			// Naming the current policy is not a change and is accepted.
			if *p.PolicyID != cur.PolicyID {
				return validation([]FieldError{fieldf("policy_id", "Change the policy through its impact preview and assignment.")})
			}
			next.PolicyID = *p.PolicyID
		}
		if p.Notes != nil {
			next.Notes = *p.Notes
		}
		contacts := ParseContacts(cur.Contacts)
		if p.Contacts != nil {
			contacts = append([]Contact(nil), (*p.Contacts)...)
		}
		// Validate what the caller touched. A stored row that the current rules
		// would refuse (an old slug, say) must not block an unrelated edit.
		if errs := normalizeCustomer(&next, contacts); len(errs) > 0 {
			touched := map[string]bool{"name": p.Name != nil, "slug": p.Slug != nil, "display_tz": p.DisplayTZ != nil,
				"device_cap": p.DeviceCap != nil, "notes": p.Notes != nil, "contacts": p.Contacts != nil}
			var report []FieldError
			for _, e := range errs {
				if touched[e.Field] {
					report = append(report, e)
				}
			}
			if len(report) > 0 {
				return validation(report)
			}
		}
		newContacts, err := contactsJSON(contacts)
		if err != nil {
			return err
		}
		var fields []string
		diff := func(name string, differs bool) {
			if differs {
				fields = append(fields, name)
			}
		}
		diff("name", next.Name != cur.Name)
		diff("slug", next.Slug != cur.Slug)
		diff("expected", next.Expected != cur.Expected)
		diff("onboarding_until", !sameTime(next.OnboardingUntil, cur.OnboardingUntil))
		diff("display_tz", next.DisplayTZ != cur.DisplayTZ)
		diff("device_cap", !sameIntPtr(next.DeviceCap, cur.DeviceCap))
		diff("policy_id", next.PolicyID != cur.PolicyID)
		diff("notes", next.Notes != cur.Notes)
		diff("contacts", newContacts != canonicalContacts(cur.Contacts))
		// A name and a slug are unique across ALL customers, so answering "already
		// taken" to a caller who can see only some of them would reveal the names of
		// the others. Renaming is therefore an All-scope act (the API gates edits to
		// admin and owner, who always have it); the store enforces it too.
		if !scope.All && (next.Name != cur.Name || next.Slug != cur.Slug) {
			return ErrScope
		}
		// The device cap, like the storage target and key custody, is an owner-level
		// field: a scoped caller may pass it back unchanged but not change it, as
		// UpdateCustomer already insists.
		if !scope.All && !sameIntPtr(next.DeviceCap, cur.DeviceCap) {
			return ErrScope
		}
		if len(fields) == 0 {
			out = cur
			return nil
		}
		if err := checkCustomerUnique(tx, id, next.Name, next.Slug); err != nil {
			return err
		}
		if err := checkCustomerPolicy(tx, id, next.PolicyID); err != nil {
			return policyFieldError(err)
		}
		now := tx.nowMS()
		if _, err := tx.Exec(`UPDATE customers SET name = ?, slug = ?, expected = ?, onboarding_until = ?, contacts = ?,
			policy_id = ?, display_tz = ?, device_cap = ?, notes = ?, updated_at = ? WHERE id = ?`,
			next.Name, next.Slug, boolInt(next.Expected), nullTime(next.OnboardingUntil), newContacts, nullStr(next.PolicyID),
			nullStr(next.DisplayTZ), nullInt(next.DeviceCap), nullStr(next.Notes), now, id); err != nil {
			return constraintFields(mapErr(err))
		}
		details := map[string]any{"fields": fields}
		for _, f := range fields {
			if f == "expected" {
				details["expected"] = map[string]bool{"from": cur.Expected, "to": next.Expected}
			}
		}
		ev, err := actor.event("customer.update", "customer", id, id, details)
		if err != nil {
			return err
		}
		if err := audited(tx, ev); err != nil {
			return err
		}
		changed = true
		out, err = one(tx, `SELECT `+customerCols+` FROM customers WHERE id = ?`, []any{id}, scanCustomer)
		return err
	})
	if err != nil {
		return Customer{}, false, err
	}
	return out, changed, nil
}

// canonicalContacts re-encodes stored contacts the way contactsJSON does, so a
// patch that sends the same contacts is recognised as no change.
func canonicalContacts(raw json.RawMessage) string {
	s, err := contactsJSON(ParseContacts(raw))
	if err != nil {
		return string(raw)
	}
	return s
}

// ---------------------------------------------------------------------------
// the list
// ---------------------------------------------------------------------------

// CustomerRow is one line of the customer list: the customer, its assigned
// policy's name and the SQL-level rollup. State counts come from the state
// engine (BuildCustomerTable merges them in).
type CustomerRow struct {
	Customer
	// PolicyName is the assigned policy's name; "" when the default policy applies.
	PolicyName string
	// Devices counts non-retired devices, Retired the retired ones.
	Devices, Retired int
	// LastSuccessAt is the newest finished successful backup over all the
	// customer's devices (nil when none ever succeeded).
	LastSuccessAt *time.Time
}

// CustomerListQuery filters ListCustomerRows. The zero value lists every
// customer that is not archived.
type CustomerListQuery struct {
	// Search is a case-insensitive substring of the name or slug.
	Search string
	// Expected, when non-nil, keeps only expected (true) or not expected (false) customers.
	Expected *bool
	// Lifecycle keeps one lifecycle state (active | suspended | archived) when set.
	Lifecycle string
	// IncludeArchived adds archived customers to an unfiltered lifecycle.
	IncludeArchived bool
}

type extraScanner struct {
	sc    scanner
	extra []any
}

func (e extraScanner) Scan(dest ...any) error { return e.sc.Scan(append(dest, e.extra...)...) }

// ListCustomerRows returns the customers inside scope that match q, ordered by
// name, each with its device counts, assigned policy name and newest success.
func (s *Store) ListCustomerRows(ctx context.Context, scope Scope, q CustomerListQuery) ([]CustomerRow, error) {
	if err := scope.check(); err != nil {
		return nil, err
	}
	switch q.Lifecycle {
	case "", "active", "suspended", "archived":
	default:
		return nil, validation([]FieldError{fieldf("state", "Use active, suspended or archived.")})
	}
	clause, args := scope.clause("c.id")
	where := []string{clause}
	if q.Search != "" {
		pat := "%" + escapeLike(strings.TrimSpace(q.Search)) + "%"
		where = append(where, `(c.name LIKE ? ESCAPE '\' OR c.slug LIKE ? ESCAPE '\')`)
		args = append(args, pat, pat)
	}
	if q.Expected != nil {
		where, args = append(where, "c.expected = ?"), append(args, boolInt(*q.Expected))
	}
	switch {
	case q.Lifecycle != "":
		where, args = append(where, "c.state = ?"), append(args, q.Lifecycle)
	case !q.IncludeArchived:
		where = append(where, "c.state <> 'archived'")
	}
	query := `SELECT ` + prefixCols(customerCols, "c.") + `, COALESCE(p.name, ''),
		(SELECT COUNT(*) FROM devices d WHERE d.customer_id = c.id AND d.retired_at IS NULL),
		(SELECT COUNT(*) FROM devices d WHERE d.customer_id = c.id AND d.retired_at IS NOT NULL),
		(SELECT MAX(r.finished_at) FROM runs r JOIN devices d ON d.id = r.device_id
			WHERE d.customer_id = c.id AND r.kind = 'backup' AND r.outcome = 'success')
		FROM customers c LEFT JOIN policies p ON p.id = c.policy_id
		WHERE ` + strings.Join(where, " AND ") + ` ORDER BY c.name COLLATE NOCASE, c.id`
	var out []CustomerRow
	err := s.Read(ctx, func(tx *Tx) error {
		var err error
		out, err = collect(tx, query, args, func(sc scanner) (CustomerRow, error) {
			var r CustomerRow
			var last sql.NullInt64
			c, err := scanCustomer(extraScanner{sc, []any{&r.PolicyName, &r.Devices, &r.Retired, &last}})
			if err != nil {
				return CustomerRow{}, err
			}
			r.Customer, r.LastSuccessAt = c, ptrTime(last)
			return r, nil
		})
		return err
	})
	return out, err
}

// GetCustomerRow is ListCustomerRows for one customer inside scope.
func (s *Store) GetCustomerRow(ctx context.Context, scope Scope, id string) (CustomerRow, error) {
	if err := scope.check(); err != nil {
		return CustomerRow{}, err
	}
	if !scope.Allows(id) {
		return CustomerRow{}, ErrNotFound
	}
	rows, err := s.ListCustomerRows(ctx, ScopeOf(id), CustomerListQuery{IncludeArchived: true})
	if err != nil {
		return CustomerRow{}, err
	}
	if len(rows) == 0 {
		return CustomerRow{}, ErrNotFound
	}
	return rows[0], nil
}

// PolicyChoice is one entry of the customer form's policy select.
type PolicyChoice struct {
	ID    string
	Name  string
	Scope string // template | customer
}

// PolicyChoicesFor lists the policies a customer may be assigned: the shared
// templates and, when customerID is set, that customer's own policy. The
// default policy is not listed; "no policy" means the default applies. Policies
// of other customers never appear (the store's scope rule).
func (s *Store) PolicyChoicesFor(ctx context.Context, scope Scope, customerID string) ([]PolicyChoice, error) {
	recs, err := s.ListPolicies(ctx, scope)
	if err != nil {
		return nil, err
	}
	var out []PolicyChoice
	for _, r := range recs {
		switch {
		case r.Scope == "template":
			out = append(out, PolicyChoice{ID: r.ID, Name: r.Name, Scope: r.Scope})
		case r.Scope == "customer" && customerID != "" && r.CustomerID == customerID:
			out = append(out, PolicyChoice{ID: r.ID, Name: r.Name, Scope: r.Scope})
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// the merged table
// ---------------------------------------------------------------------------

// CustomerTableQuery shapes BuildCustomerTable.
type CustomerTableQuery struct {
	// Health keeps one rollup status (missing, onboarding, alarm, warn, ok,
	// empty, unknown); "" keeps all.
	Health string
	// Sort is health (default: exceptions first), name, devices, last_success
	// or expected; Desc reverses it.
	Sort string
	Desc bool
	// Page and Size select a numbered page; when Limit > 0 the window is the
	// rows [Offset, Offset+Limit) instead (the JSON API's cursor paging).
	Page, Size    int
	Offset, Limit int
}

// CustomerTableRow is a customer row joined with its state-engine rollup.
type CustomerTableRow struct {
	CustomerRow
	// Rollup is the engine's verdict; InView is false for a customer the engine
	// does not list (archived), which has no state chip.
	Rollup state.CustomerState
	InView bool
	// LastSuccess is the newest effective last success among the customer's
	// devices according to the engine; zero when none.
	LastSuccess time.Time
}

// CustomerTable is one page of the customer list.
type CustomerTable struct {
	Rows []CustomerTableRow
	Page PageInfo
	// Total is the number of customers before the health filter; the table says
	// "N of Total" when a filter hides some.
	Total int
}

var customerHealthValues = []state.CustomerStatus{state.CustomerMissing, state.CustomerOnboarding, state.CustomerAlarm,
	state.CustomerWarn, state.CustomerOK, state.CustomerEmpty, state.CustomerUnknown}

// ValidCustomerHealth reports whether h is a customer rollup status the list can filter by.
func ValidCustomerHealth(h string) bool {
	for _, v := range customerHealthValues {
		if string(v) == h {
			return true
		}
	}
	return false
}

// CustomerSorts are the sort keys of the customer list.
var CustomerSorts = []string{"health", "name", "devices", "last_success", "expected"}

// BuildCustomerTable joins rows with the engine's rollups (view, keyed by
// customer id), filters by health, sorts and pages. It is pure: the caller
// decides the scope through the rows and the view it passes.
func BuildCustomerTable(rows []CustomerRow, view []state.CustomerState, q CustomerTableQuery) (CustomerTable, error) {
	if q.Health != "" && !ValidCustomerHealth(q.Health) {
		return CustomerTable{}, validation([]FieldError{fieldf("health", "Unknown customer state.")})
	}
	sortKey := q.Sort
	if sortKey == "" {
		sortKey = "health"
	}
	known := false
	for _, k := range CustomerSorts {
		known = known || k == sortKey
	}
	if !known {
		return CustomerTable{}, validation([]FieldError{fieldf("sort", "Unknown sort key.")})
	}
	byID := make(map[string]state.CustomerState, len(view))
	for _, c := range view {
		byID[c.ID] = c
	}
	all := make([]CustomerTableRow, 0, len(rows))
	for _, r := range rows {
		tr := CustomerTableRow{CustomerRow: r}
		if cs, ok := byID[r.ID]; ok {
			tr.Rollup, tr.InView = cs, true
			for _, d := range cs.Devices {
				if d.Detail.LastSuccess.After(tr.LastSuccess) {
					tr.LastSuccess = d.Detail.LastSuccess
				}
			}
		}
		all = append(all, tr)
	}
	t := CustomerTable{Total: len(all)}
	kept := all[:0:0]
	for _, r := range all {
		if q.Health != "" && (!r.InView || string(r.Rollup.Status) != q.Health) {
			continue
		}
		kept = append(kept, r)
	}
	less := func(a, b CustomerTableRow) bool {
		switch sortKey {
		case "name":
			return strings.ToLower(a.Name) < strings.ToLower(b.Name)
		case "devices":
			if a.Devices != b.Devices {
				return a.Devices < b.Devices
			}
		case "last_success":
			if !a.LastSuccess.Equal(b.LastSuccess) {
				return a.LastSuccess.Before(b.LastSuccess)
			}
		case "expected":
			if a.Expected != b.Expected {
				return a.Expected // expected first when ascending
			}
		default: // health: the engine's §4.5 order, customers it does not list last
			ka, kb := healthKey(a), healthKey(b)
			if ka != kb {
				return ka < kb
			}
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	}
	sort.SliceStable(kept, func(i, j int) bool {
		if q.Desc {
			return less(kept[j], kept[i])
		}
		return less(kept[i], kept[j])
	})
	off, end, info := windowOf(len(kept), q.Page, q.Size, q.Offset, q.Limit)
	t.Rows, t.Page = kept[off:end], info
	if t.Rows == nil {
		t.Rows = []CustomerTableRow{}
	}
	return t, nil
}

func healthKey(r CustomerTableRow) int {
	if !r.InView {
		return 100
	}
	return r.Rollup.SortKey
}
