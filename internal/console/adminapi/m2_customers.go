package adminapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/httpx"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// M2 routes of ticket M2-W1C: customers, the devices of a customer, the device
// settings edit and the device run history (architecture §5.2).
const (
	PatternCustomers       = "GET /api/v1/customers"
	PatternCustomerCreate  = "POST /api/v1/customers"
	PatternCustomer        = "GET /api/v1/customers/{id}"
	PatternCustomerPatch   = "PATCH /api/v1/customers/{id}"
	PatternCustomerDevices = "GET /api/v1/customers/{id}/devices"
	PatternDevicePatch     = "PATCH /api/v1/devices/{id}"
	PatternDeviceRuns      = "GET /api/v1/devices/{id}/runs"
)

// m2Patterns lists the patterns registerM2 adds.
func m2Patterns() []string {
	return []string{PatternCustomers, PatternCustomerCreate, PatternCustomer, PatternCustomerPatch, PatternCustomerDevices, PatternDevicePatch, PatternDeviceRuns}
}

// registerM2 adds the M2 customer and device routes: reads need read_only,
// editing a device operator, creating and editing a customer admin.
func (s *Service) registerM2(r httpx.Registrar) {
	ro := func(h http.HandlerFunc) http.Handler { return s.Auth.Require(auth.RoleReadOnly, auth.ModeAPI, h) }
	op := func(h http.HandlerFunc) http.Handler { return s.Auth.Require(auth.RoleOperator, auth.ModeAPI, h) }
	ad := func(h http.HandlerFunc) http.Handler { return s.Auth.Require(auth.RoleAdmin, auth.ModeAPI, h) }
	r.Handle(PatternCustomers, ro(s.listCustomers))
	r.Handle(PatternCustomerCreate, ad(s.createCustomer))
	r.Handle(PatternCustomer, ro(s.getCustomer))
	r.Handle(PatternCustomerPatch, ad(s.patchCustomer))
	r.Handle(PatternCustomerDevices, ro(s.listCustomerDevices))
	r.Handle(PatternDevicePatch, op(s.patchDevice))
	r.Handle(PatternDeviceRuns, ro(s.listDeviceRuns))
}

// ---- shapes ----

// ContactJSON is one contact of a customer.
type ContactJSON struct {
	Name  string `json:"name"`
	Email string `json:"email,omitempty"`
	Phone string `json:"phone,omitempty"`
	Role  string `json:"role,omitempty"`
}

// RollupJSON is the state engine's rollup of a customer (architecture §4.5).
type RollupJSON struct {
	Status string         `json:"status"`
	Counts map[string]int `json:"counts"`
	Active int            `json:"active"`
	Paused int            `json:"paused"`
	// HealthPct is absent when the engine cannot compute it (a zero denominator is n/a, never 100).
	HealthPct *float64 `json:"health_pct,omitempty"`
	Severity  string   `json:"severity"`
	Muted     bool     `json:"muted"`
}

// CustomerJSON is a customer with its rollup.
type CustomerJSON struct {
	ID              string        `json:"id"`
	Name            string        `json:"name"`
	Slug            string        `json:"slug"`
	State           string        `json:"state"`
	Expected        bool          `json:"expected"`
	OnboardingUntil *time.Time    `json:"onboarding_until,omitempty"`
	DisplayTZ       string        `json:"display_tz,omitempty"`
	Contacts        []ContactJSON `json:"contacts"`
	DeviceCap       *int          `json:"device_cap,omitempty"`
	PolicyID        string        `json:"policy_id,omitempty"`
	PolicyName      string        `json:"policy_name,omitempty"`
	KeyCustody      string        `json:"key_custody"`
	Notes           string        `json:"notes,omitempty"`
	Devices         int           `json:"devices"`
	RetiredDevices  int           `json:"retired_devices"`
	LastSuccessAt   *time.Time    `json:"last_success_at,omitempty"`
	Rollup          *RollupJSON   `json:"rollup,omitempty"`
	CreatedAt       time.Time     `json:"created_at"`
	UpdatedAt       time.Time     `json:"updated_at"`
}

// CustomerListResponse is GET /api/v1/customers.
type CustomerListResponse struct {
	ComputedAt time.Time `json:"computed_at"`
	LoadedAt   time.Time `json:"loaded_at"`
	Complete   bool      `json:"complete"`
	// Total counts the customers that match, Of all the customers before the health filter.
	Total      int            `json:"total"`
	Of         int            `json:"of"`
	Truncated  bool           `json:"truncated"`
	NextCursor string         `json:"next_cursor,omitempty"`
	Customers  []CustomerJSON `json:"customers"`
}

// CustomerResponse is the body of GET, POST and PATCH on a customer.
type CustomerResponse struct {
	ComputedAt time.Time `json:"computed_at"`
	CustomerJSON
}

// CreateCustomerRequest is the body of POST /api/v1/customers.
type CreateCustomerRequest struct {
	Name            string        `json:"name"`
	Slug            string        `json:"slug"`
	Expected        bool          `json:"expected"`
	OnboardingUntil *time.Time    `json:"onboarding_until"`
	DisplayTZ       string        `json:"display_tz"`
	Contacts        []ContactJSON `json:"contacts"`
	DeviceCap       *int          `json:"device_cap"`
	PolicyID        string        `json:"policy_id"`
	Notes           string        `json:"notes"`
	StorageTargetID string        `json:"storage_target_id"`
	KeyCustody      string        `json:"key_custody"`
}

// optional is a JSON field that can be absent, null or a value: PATCH needs the
// three apart (absent leaves a field alone, null clears it).
type optional[T any] struct {
	Set, Null bool
	Value     T
}

// UnmarshalJSON implements json.Unmarshaler.
func (o *optional[T]) UnmarshalJSON(b []byte) error {
	o.Set = true
	if string(b) == "null" {
		o.Null = true
		return nil
	}
	return json.Unmarshal(b, &o.Value)
}

// PatchCustomerRequest is the body of PATCH /api/v1/customers/{id}. Absent
// fields are unchanged; null clears display_tz, policy_id, notes, device_cap and
// onboarding_until (policy_id null means "the default policy applies").
type PatchCustomerRequest struct {
	Name            optional[string]        `json:"name"`
	Slug            optional[string]        `json:"slug"`
	Expected        optional[bool]          `json:"expected"`
	OnboardingUntil optional[time.Time]     `json:"onboarding_until"`
	DisplayTZ       optional[string]        `json:"display_tz"`
	Contacts        optional[[]ContactJSON] `json:"contacts"`
	DeviceCap       optional[int]           `json:"device_cap"`
	PolicyID        optional[string]        `json:"policy_id"`
	Notes           optional[string]        `json:"notes"`
	// ImpactToken is the token of the assignment preview (POST /policies/{id}/impact with
	// assign_customer); a policy_id change is accepted only with it, and only on its own.
	ImpactToken string `json:"impact_token"`
}

// ---- helpers ----

// validationProblem is the problem document of a rejected input: the fixed
// invalid_request body plus one entry per rejected field. The sentences come
// from the store's validators, which never echo the input.
type validationProblem struct {
	*api.Error
	Errors []store.FieldError `json:"errors"`
}

// failM2 is fail with two additions the M2 routes need: a rejected input lists
// its fields, and a change refused because it could not be audited is a loud
// server error (the data is unchanged).
func (s *Service) failM2(w http.ResponseWriter, r *http.Request, err error) {
	if fe := store.FieldErrors(err); len(fe) > 0 {
		e := api.NewError(api.CodeInvalidRequest)
		e.RequestID = httpx.RequestID(r.Context())
		h := w.Header()
		h.Set("Content-Type", api.ContentTypeProblem)
		h.Set("Cache-Control", "no-store")
		w.WriteHeader(e.Status)
		_ = json.NewEncoder(w).Encode(validationProblem{Error: e, Errors: fe})
		return
	}
	if errors.Is(err, store.ErrAudit) {
		s.log().Error("admin api: change refused, not audited", logx.Class("audit"), logx.Err(err))
		httpx.Problem(w, r, api.CodeInternal, 0)
		return
	}
	s.fail(w, r, err)
}

// decodeBodyMax strictly decodes one JSON object of at most max bytes.
func decodeBodyMax(w http.ResponseWriter, r *http.Request, max int64, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, max)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			httpx.Problem(w, r, api.CodePayloadTooLarge, 0)
			return false
		}
		httpx.Problem(w, r, api.CodeInvalidRequest, 0)
		return false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		httpx.Problem(w, r, api.CodeInvalidRequest, 0)
		return false
	}
	return true
}

func actorOf(r *http.Request, p *auth.Principal) store.AuditActor {
	return store.AuditActor{AdminID: p.Admin.ID, Username: p.Admin.Username, IP: httpx.ClientIP(r), Method: "api"}
}

func storeContacts(in []ContactJSON) []store.Contact {
	out := make([]store.Contact, len(in))
	for i, c := range in {
		out[i] = store.Contact(c)
	}
	return out
}

func contactsOf(raw json.RawMessage) []ContactJSON {
	cs := store.ParseContacts(raw)
	out := make([]ContactJSON, len(cs))
	for i, c := range cs {
		out[i] = ContactJSON(c)
	}
	return out
}

func rollupJSON(cs state.CustomerState) *RollupJSON {
	counts := map[string]int{}
	for st, n := range cs.Counts {
		if n > 0 {
			counts[string(st)] = n
		}
	}
	out := &RollupJSON{Status: string(cs.Status), Counts: counts, Active: cs.Active, Paused: cs.Paused, Severity: cs.Alarm.String(), Muted: cs.Muted}
	if cs.HealthKnown {
		v := cs.HealthPct
		out.HealthPct = &v
	}
	return out
}

func customerJSON(c store.Customer) CustomerJSON {
	out := CustomerJSON{ID: c.ID, Name: c.Name, Slug: c.Slug, State: c.State, Expected: c.Expected, DisplayTZ: c.DisplayTZ,
		Contacts: contactsOf(c.Contacts), DeviceCap: c.DeviceCap, PolicyID: c.PolicyID, KeyCustody: c.KeyCustody, Notes: c.Notes,
		CreatedAt: c.CreatedAt.UTC(), UpdatedAt: c.UpdatedAt.UTC()}
	if c.OnboardingUntil != nil {
		out.OnboardingUntil = ptrTime(*c.OnboardingUntil)
	}
	return out
}

func customerRowJSON(r store.CustomerTableRow) CustomerJSON {
	out := customerJSON(r.Customer)
	out.PolicyName, out.Devices, out.RetiredDevices = r.PolicyName, r.Devices, r.Retired
	if r.LastSuccessAt != nil {
		out.LastSuccessAt = ptrTime(*r.LastSuccessAt)
	}
	if r.InView {
		out.Rollup = rollupJSON(r.Rollup)
		if !r.LastSuccess.IsZero() {
			out.LastSuccessAt = ptrTime(r.LastSuccess) // the engine's effective last success wins
		}
	}
	return out
}

// pageParams reads limit (1..max, default 100) and cursor (the row offset) of a list.
func pageParams(q map[string][]string, max int) (offset, limit int, ok bool) {
	get := func(k string) string {
		if v := q[k]; len(v) > 0 {
			return v[0]
		}
		return ""
	}
	limit = 100
	if v := get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > max {
			return 0, 0, false
		}
		limit = n
	}
	if c := get("cursor"); c != "" {
		n, err := strconv.Atoi(strings.TrimPrefix(c, "o"))
		if err != nil || !strings.HasPrefix(c, "o") || n < 0 {
			return 0, 0, false
		}
		offset = n
	}
	return offset, limit, true
}

func nextCursor(info store.PageInfo) (string, bool) {
	if info.To > 0 && info.To < info.Total {
		return "o" + strconv.Itoa(info.To), true
	}
	return "", false
}

func boolParam(v string) (*bool, bool) {
	switch v {
	case "":
		return nil, true
	case "true":
		t := true
		return &t, true
	case "false":
		f := false
		return &f, true
	}
	return nil, false
}

// ---- customers ----

func (s *Service) listCustomers(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	q := r.URL.Query()
	offset, limit, ok := pageParams(q, 500)
	expected, ok2 := boolParam(q.Get("expected"))
	archived, ok3 := boolParam(q.Get("include_archived"))
	if !ok || !ok2 || !ok3 {
		httpx.Problem(w, r, api.CodeInvalidRequest, 0)
		return
	}
	sortKey := q.Get("sort")
	desc := q.Get("dir") == "desc"
	if d := q.Get("dir"); d != "" && d != "asc" && d != "desc" {
		httpx.Problem(w, r, api.CodeInvalidRequest, 0)
		return
	}
	fleet, err := s.Fleet.Fleet(r.Context(), p.Scope)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	rows, err := s.Store.ListCustomerRows(r.Context(), p.Scope, store.CustomerListQuery{Search: q.Get("q"), Expected: expected,
		Lifecycle: q.Get("state"), IncludeArchived: archived != nil && *archived})
	if err != nil {
		s.failM2(w, r, err)
		return
	}
	tb, err := store.BuildCustomerTable(rows, fleet.View.Customers, store.CustomerTableQuery{Health: q.Get("health"), Sort: sortKey, Desc: desc, Offset: offset, Limit: limit})
	if err != nil {
		s.failM2(w, r, err)
		return
	}
	resp := CustomerListResponse{ComputedAt: fleet.Computed, LoadedAt: fleet.View.LoadedAt, Complete: fleet.View.Complete,
		Total: tb.Page.Total, Of: tb.Total, Customers: make([]CustomerJSON, 0, len(tb.Rows))}
	resp.NextCursor, resp.Truncated = nextCursor(tb.Page)
	for _, row := range tb.Rows {
		resp.Customers = append(resp.Customers, customerRowJSON(row))
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}

// customerResponse reads one customer (with rollup) inside scope.
func (s *Service) customerResponse(r *http.Request, p *auth.Principal, id string) (CustomerResponse, error) {
	row, err := s.Store.GetCustomerRow(r.Context(), p.Scope, id)
	if err != nil {
		return CustomerResponse{}, err
	}
	fleet, err := s.Fleet.Fleet(r.Context(), p.Scope)
	if err != nil {
		return CustomerResponse{}, err
	}
	tb, err := store.BuildCustomerTable([]store.CustomerRow{row}, fleet.View.Customers, store.CustomerTableQuery{Limit: 1})
	if err != nil || len(tb.Rows) != 1 {
		return CustomerResponse{}, errors.New("adminapi: customer row lost")
	}
	return CustomerResponse{ComputedAt: fleet.Computed, CustomerJSON: customerRowJSON(tb.Rows[0])}, nil
}

func (s *Service) getCustomer(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	resp, err := s.customerResponse(r, p, r.PathValue("id"))
	if err != nil {
		s.failM2(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}

func (s *Service) createCustomer(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	var req CreateCustomerRequest
	if !decodeBodyMax(w, r, 32<<10, &req) {
		return
	}
	c, err := s.Store.CreateCustomerAudited(r.Context(), p.Scope, store.CustomerInput{Name: req.Name, Slug: req.Slug, Expected: req.Expected,
		OnboardingUntil: req.OnboardingUntil, DisplayTZ: req.DisplayTZ, Contacts: storeContacts(req.Contacts), DeviceCap: req.DeviceCap,
		PolicyID: req.PolicyID, Notes: req.Notes, StorageTargetID: req.StorageTargetID, KeyCustody: req.KeyCustody}, actorOf(r, p))
	if err != nil {
		s.failM2(w, r, err)
		return
	}
	resp, err := s.customerResponse(r, p, c.ID)
	if err != nil {
		s.failM2(w, r, err)
		return
	}
	w.Header().Set("Location", "/api/v1/customers/"+c.ID)
	httpx.WriteJSON(w, http.StatusCreated, resp)
}

func (s *Service) patchCustomer(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	var req PatchCustomerRequest
	if !decodeBodyMax(w, r, 32<<10, &req) {
		return
	}
	if req.PolicyID.Set {
		if handled := s.patchCustomerPolicy(w, r, p, req); handled {
			return
		}
	}
	var patch store.CustomerPatch
	str := func(o optional[string]) *string {
		if !o.Set {
			return nil
		}
		v := o.Value // null clears (the empty string)
		return &v
	}
	patch.Name, patch.Slug, patch.DisplayTZ, patch.Notes = str(req.Name), str(req.Slug), str(req.DisplayTZ), str(req.Notes)
	if req.Expected.Set {
		if req.Expected.Null {
			httpx.Problem(w, r, api.CodeInvalidRequest, 0)
			return
		}
		v := req.Expected.Value
		patch.Expected = &v
	}
	if req.OnboardingUntil.Set {
		if req.OnboardingUntil.Null {
			patch.ClearOnboarding = true
		} else {
			v := req.OnboardingUntil.Value
			patch.OnboardingUntil = &v
		}
	}
	if req.DeviceCap.Set {
		if req.DeviceCap.Null {
			patch.ClearDeviceCap = true
		} else {
			v := req.DeviceCap.Value
			patch.DeviceCap = &v
		}
	}
	if req.Contacts.Set {
		cs := storeContacts(req.Contacts.Value) // null is an empty list
		patch.Contacts = &cs
	}
	id := r.PathValue("id")
	if _, _, err := s.Store.UpdateCustomerAudited(r.Context(), p.Scope, id, patch, actorOf(r, p)); err != nil {
		s.failM2(w, r, err)
		return
	}
	resp, err := s.customerResponse(r, p, id)
	if err != nil {
		s.failM2(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}
