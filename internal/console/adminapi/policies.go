package adminapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/httpx"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// M2-W2A routes (architecture §5.2 "Policies" and the per-device override rows):
// policies with a blast-radius preview whose token the confirmed change must
// carry, the effective policy of a device with per-field sources, and the
// sparse per-device override. Reads need read_only; every write needs admin
// (the §5 rows: PATCH/DELETE/POST policies, PUT/DELETE override).
const (
	PatternPolicies        = "GET /api/v1/policies"
	PatternPolicyCreate    = "POST /api/v1/policies"
	PatternPolicy          = "GET /api/v1/policies/{id}"
	PatternPolicyPatch     = "PATCH /api/v1/policies/{id}"
	PatternPolicyDelete    = "DELETE /api/v1/policies/{id}"
	PatternPolicyImpact    = "POST /api/v1/policies/{id}/impact"
	PatternDeviceEffective = "GET /api/v1/devices/{id}/effective-policy"
	PatternDeviceOverride  = "GET /api/v1/devices/{id}/override"
	PatternOverrideSet     = "PUT /api/v1/devices/{id}/override"
	PatternOverrideClear   = "DELETE /api/v1/devices/{id}/override"
)

// policyPatterns lists the patterns registerPolicies adds.
func policyPatterns() []string {
	return []string{PatternPolicies, PatternPolicyCreate, PatternPolicy, PatternPolicyPatch, PatternPolicyDelete, PatternPolicyImpact,
		PatternDeviceEffective, PatternDeviceOverride, PatternOverrideSet, PatternOverrideClear}
}

func (s *Service) registerPolicies(r httpx.Registrar) {
	ro := func(h http.HandlerFunc) http.Handler { return s.Auth.Require(auth.RoleReadOnly, auth.ModeAPI, h) }
	ad := func(h http.HandlerFunc) http.Handler { return s.Auth.Require(auth.RoleAdmin, auth.ModeAPI, h) }
	r.Handle(PatternPolicies, ro(s.listPolicies))
	r.Handle(PatternPolicyCreate, ad(s.createPolicy))
	r.Handle(PatternPolicy, ro(s.getPolicy))
	r.Handle(PatternPolicyPatch, ad(s.patchPolicy))
	r.Handle(PatternPolicyDelete, ad(s.deletePolicy))
	r.Handle(PatternPolicyImpact, ad(s.policyImpact))
	r.Handle(PatternDeviceEffective, ro(s.deviceEffective))
	r.Handle(PatternDeviceOverride, ro(s.getOverride))
	r.Handle(PatternOverrideSet, ad(s.putOverride))
	r.Handle(PatternOverrideClear, ad(s.deleteOverride))
}

// ---- shapes ----

// PolicyJSON is a policy as the API shows it. Sections holds the stored form:
// the default policy is a complete document; a template or customer policy
// holds only the fields that differ from the default (RFC 7386 merge patches
// per section, "{}" when a section equals the default).
type PolicyJSON struct {
	ID                  string                     `json:"id"`
	Scope               string                     `json:"scope"`
	CustomerID          string                     `json:"customer_id,omitempty"`
	CustomerName        string                     `json:"customer_name,omitempty"`
	Name                string                     `json:"name"`
	Description         string                     `json:"description,omitempty"`
	TemplateDeviceClass string                     `json:"template_device_class,omitempty"`
	Version             int                        `json:"version"`
	Sections            map[string]json.RawMessage `json:"sections"`
	Customers           *int                       `json:"customers,omitempty"`
	Devices             *int                       `json:"devices,omitempty"`
	CreatedAt           time.Time                  `json:"created_at"`
	UpdatedAt           time.Time                  `json:"updated_at"`
}

func policyJSON(r store.PolicyRecord) PolicyJSON {
	return PolicyJSON{ID: r.ID, Scope: r.Scope, CustomerID: r.CustomerID, Name: r.Name, Description: r.Description,
		TemplateDeviceClass: r.TemplateDeviceClass, Version: r.Version, Sections: r.Sections, CreatedAt: r.CreatedAt.UTC(), UpdatedAt: r.UpdatedAt.UTC()}
}

// PolicyListResponse is GET /api/v1/policies.
type PolicyListResponse struct {
	ComputedAt time.Time    `json:"computed_at"`
	Policies   []PolicyJSON `json:"policies"`
}

// PolicyResponse is the body of GET, POST and PATCH on a policy.
type PolicyResponse struct {
	ComputedAt time.Time `json:"computed_at"`
	PolicyJSON
	// Changed is false when a PATCH changed nothing (no version, no audit row).
	Changed *bool `json:"changed,omitempty"`
}

// CreatePolicyRequest is the body of POST /api/v1/policies. Scope is template,
// or customer with customer_id.
type CreatePolicyRequest struct {
	Scope               string                     `json:"scope"`
	CustomerID          string                     `json:"customer_id"`
	Name                string                     `json:"name"`
	Description         string                     `json:"description"`
	TemplateDeviceClass string                     `json:"template_device_class"`
	Sections            map[string]json.RawMessage `json:"sections"`
	Note                string                     `json:"note"`
}

// PatchPolicyRequest is the body of PATCH /api/v1/policies/{id}: the sections
// present replace the stored ones, the rest stay. ImpactToken is the token of
// the preview (POST .../impact) of exactly this change.
type PatchPolicyRequest struct {
	Name        optional[string]           `json:"name"`
	Description optional[string]           `json:"description"`
	Sections    map[string]json.RawMessage `json:"sections"`
	Note        string                     `json:"note"`
	ImpactToken string                     `json:"impact_token"`
}

// ImpactRequest is the body of POST /api/v1/policies/{id}/impact: the proposed
// change (as PATCH sends it), or {"delete": true} to preview a delete.
type ImpactRequest struct {
	Name        optional[string]           `json:"name"`
	Description optional[string]           `json:"description"`
	Sections    map[string]json.RawMessage `json:"sections"`
	Delete      bool                       `json:"delete"`
	// AssignCustomer previews assigning the policy in the path to this customer
	// (the default policy's id means "the default policy applies").
	AssignCustomer string `json:"assign_customer"`
}

// ImpactResponse is the blast radius of a change.
type ImpactResponse struct {
	ComputedAt time.Time `json:"computed_at"`
	store.ImpactReport
}

// OverrideJSON is a device's override.
type OverrideJSON struct {
	ID        string          `json:"id"`
	DeviceID  string          `json:"device_id"`
	Patch     json.RawMessage `json:"patch"`
	Reason    string          `json:"reason"`
	SetBy     string          `json:"set_by,omitempty"`
	SetByName string          `json:"set_by_name,omitempty"`
	ExpiresAt *time.Time      `json:"expires_at,omitempty"`
	Expired   bool            `json:"expired"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

func overrideJSON(o store.DeviceOverride) OverrideJSON {
	out := OverrideJSON{ID: o.ID, DeviceID: o.DeviceID, Patch: o.Patch, Reason: o.Reason, SetBy: o.SetBy, SetByName: o.SetByName,
		Expired: o.Expired, CreatedAt: o.CreatedAt.UTC(), UpdatedAt: o.UpdatedAt.UTC()}
	if o.ExpiresAt != nil {
		out.ExpiresAt = ptrTime(*o.ExpiresAt)
	}
	return out
}

// OverrideResponse is the body of GET and PUT on an override.
type OverrideResponse struct {
	ComputedAt time.Time `json:"computed_at"`
	OverrideJSON
	Changed *bool `json:"changed,omitempty"`
}

// SetOverrideRequest is the body of PUT /api/v1/devices/{id}/override. The patch
// is an RFC 7386 merge patch over the nine sections ({"sla":{"stale_after_hours":24}});
// the reason is required.
type SetOverrideRequest struct {
	Patch     json.RawMessage `json:"patch"`
	Reason    string          `json:"reason"`
	ExpiresAt *time.Time      `json:"expires_at"`
}

// EffectiveFieldJSON is one field of an effective policy and where it came from.
type EffectiveFieldJSON struct {
	Path      string          `json:"path"`
	Value     json.RawMessage `json:"value"`
	Source    string          `json:"source"` // default | policy | override
	Inherited json.RawMessage `json:"inherited,omitempty"`
}

// EffectivePolicyResponse is GET /api/v1/devices/{id}/effective-policy.
type EffectivePolicyResponse struct {
	ComputedAt  time.Time            `json:"computed_at"`
	DeviceID    string               `json:"device_id"`
	CustomerID  string               `json:"customer_id"`
	PolicyID    string               `json:"policy_id"`
	PolicyName  string               `json:"policy_name"`
	PolicyScope string               `json:"policy_scope"`
	Version     int                  `json:"version"`
	ETag        string               `json:"etag"`
	Policy      json.RawMessage      `json:"policy"`
	Fields      []EffectiveFieldJSON `json:"fields"`
	Override    *OverrideJSON        `json:"override,omitempty"`
}

// ---- errors ----

type conflictProblem struct {
	*api.Error
	Errors    []store.FieldError     `json:"errors,omitempty"`
	Customers []store.ImpactCustomer `json:"customers,omitempty"`
}

// failPolicy is failM2 plus the answers the policy routes add: a stale or foreign
// impact token says why (409), and a delete refused because customers use the
// policy names them.
func (s *Service) failPolicy(w http.ResponseWriter, r *http.Request, err error) {
	var inUse *store.PolicyInUseError
	var extra conflictProblem
	switch {
	case errors.As(err, &inUse):
		extra.Customers = inUse.Customers
		extra.Errors = []store.FieldError{{Field: "policy", Message: "The policy is assigned to customers; reassign them first."}}
	case errors.Is(err, store.ErrImpactStale):
		extra.Errors = []store.FieldError{{Field: "impact_token", Message: "The policy changed or the preview expired. Preview the change again."}}
	case errors.Is(err, store.ErrImpactMismatch):
		extra.Errors = []store.FieldError{{Field: "impact_token", Message: "This token belongs to another change. Preview exactly the change you send."}}
	case errors.Is(err, store.ErrDefaultPolicy):
		extra.Errors = []store.FieldError{{Field: "policy", Message: "The default policy cannot be deleted."}}
	default:
		s.failM2(w, r, err)
		return
	}
	e := api.NewError(api.CodeConflict)
	e.RequestID = httpx.RequestID(r.Context())
	extra.Error = e
	h := w.Header()
	h.Set("Content-Type", api.ContentTypeProblem)
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(e.Status)
	_ = json.NewEncoder(w).Encode(extra)
}

func proposalOf(name, desc optional[string], sections map[string]json.RawMessage, note string) store.ProposedPolicy {
	p := store.ProposedPolicy{Sections: sections, Note: note}
	if name.Set && !name.Null {
		v := name.Value
		p.Name = &v
	}
	if desc.Set {
		v := desc.Value // null clears
		p.Description = &v
	}
	return p
}

// ---- policies ----

func (s *Service) listPolicies(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	q := r.URL.Query()
	scope, cust := q.Get("scope"), q.Get("customer")
	if scope != "" && scope != "default" && scope != "template" && scope != "customer" {
		httpx.Problem(w, r, api.CodeInvalidRequest, 0)
		return
	}
	list, err := s.Store.ListPolicySummaries(r.Context(), p.Scope)
	if err != nil {
		s.failM2(w, r, err)
		return
	}
	resp := PolicyListResponse{ComputedAt: s.now(), Policies: make([]PolicyJSON, 0, len(list))}
	for _, sum := range list {
		if (scope != "" && sum.Scope != scope) || (cust != "" && sum.CustomerID != cust) {
			continue
		}
		j := policyJSON(sum.PolicyRecord)
		j.CustomerName = sum.CustomerName
		c, d := sum.Customers, sum.Devices
		j.Customers, j.Devices = &c, &d
		resp.Policies = append(resp.Policies, j)
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}

func (s *Service) getPolicy(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	rec, err := s.Store.GetPolicy(r.Context(), p.Scope, r.PathValue("id"))
	if err != nil {
		s.failM2(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, PolicyResponse{ComputedAt: s.now(), PolicyJSON: policyJSON(rec)})
}

func (s *Service) createPolicy(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	var req CreatePolicyRequest
	if !decodeBodyMax(w, r, 128<<10, &req) {
		return
	}
	rec, err := s.Store.CreatePolicyAudited(r.Context(), p.Scope, store.PolicyInput{Scope: req.Scope, CustomerID: req.CustomerID, Name: req.Name,
		Description: req.Description, TemplateDeviceClass: req.TemplateDeviceClass, Sections: req.Sections, Note: req.Note}, actorOf(r, p))
	if err != nil {
		s.failPolicy(w, r, err)
		return
	}
	w.Header().Set("Location", "/api/v1/policies/"+rec.ID)
	httpx.WriteJSON(w, http.StatusCreated, PolicyResponse{ComputedAt: s.now(), PolicyJSON: policyJSON(rec)})
}

func (s *Service) patchPolicy(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	var req PatchPolicyRequest
	if !decodeBodyMax(w, r, 128<<10, &req) {
		return
	}
	if req.Name.Set && req.Name.Null {
		httpx.Problem(w, r, api.CodeInvalidRequest, 0)
		return
	}
	rec, changed, err := s.Store.UpdatePolicyAudited(r.Context(), p.Scope, r.PathValue("id"),
		proposalOf(req.Name, req.Description, req.Sections, req.Note), req.ImpactToken, actorOf(r, p))
	if err != nil {
		s.failPolicy(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, PolicyResponse{ComputedAt: s.now(), PolicyJSON: policyJSON(rec), Changed: &changed})
}

func (s *Service) policyImpact(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	var req ImpactRequest
	if !decodeBodyMax(w, r, 128<<10, &req) {
		return
	}
	var rep store.ImpactReport
	var err error
	switch {
	case req.Delete:
		rep, err = s.Store.PreviewPolicyDelete(r.Context(), p.Scope, r.PathValue("id"), p.Admin.ID)
	case req.AssignCustomer != "":
		var rec store.PolicyRecord
		if rec, err = s.Store.GetPolicy(r.Context(), p.Scope, r.PathValue("id")); err == nil {
			target := rec.ID
			if rec.Scope == "default" {
				target = ""
			}
			rep, err = s.Store.PreviewPolicyAssignment(r.Context(), p.Scope, req.AssignCustomer, target, p.Admin.ID)
		}
	default:
		rep, err = s.Store.PreviewPolicyChange(r.Context(), p.Scope, r.PathValue("id"), p.Admin.ID, proposalOf(req.Name, req.Description, req.Sections, ""))
	}
	if err != nil {
		s.failPolicy(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, ImpactResponse{ComputedAt: s.now(), ImpactReport: rep})
}

func (s *Service) deletePolicy(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	token, _ := url.QueryUnescape(r.URL.Query().Get("impact_token"))
	if err := s.Store.DeletePolicyAudited(r.Context(), p.Scope, r.PathValue("id"), token, actorOf(r, p)); err != nil {
		s.failPolicy(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- device overrides ----

func (s *Service) deviceEffective(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	v, err := s.Store.GetDeviceEffective(r.Context(), p.Scope, r.PathValue("id"))
	if err != nil {
		s.failM2(w, r, err)
		return
	}
	doc, err := json.Marshal(v.Policy)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	resp := EffectivePolicyResponse{ComputedAt: s.now(), DeviceID: v.DeviceID, CustomerID: v.CustomerID, PolicyID: v.PolicyID, PolicyName: v.PolicyName,
		PolicyScope: v.PolicyScope, Version: v.Version, ETag: v.ETag, Policy: doc, Fields: make([]EffectiveFieldJSON, 0, len(v.Fields))}
	for _, f := range v.Fields {
		fj := EffectiveFieldJSON{Path: f.Path, Value: json.RawMessage(f.Value), Source: f.Source}
		if f.Inherited != "" {
			fj.Inherited = json.RawMessage(f.Inherited)
		}
		resp.Fields = append(resp.Fields, fj)
	}
	if v.Override != nil {
		o := overrideJSON(*v.Override)
		resp.Override = &o
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}

func (s *Service) getOverride(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	o, err := s.Store.GetDeviceOverride(r.Context(), p.Scope, r.PathValue("id"))
	if err != nil {
		s.failM2(w, r, err)
		return
	}
	if o == nil { // "override | 404": a device with none answers like a missing resource
		httpx.Problem(w, r, api.CodeNotFound, 0)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, OverrideResponse{ComputedAt: s.now(), OverrideJSON: overrideJSON(*o)})
}

func (s *Service) putOverride(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	var req SetOverrideRequest
	if !decodeBodyMax(w, r, 32<<10, &req) {
		return
	}
	o, changed, err := s.Store.SetDeviceOverride(r.Context(), p.Scope, r.PathValue("id"), store.OverrideInput{Patch: req.Patch, Reason: req.Reason, ExpiresAt: req.ExpiresAt}, actorOf(r, p))
	if err != nil {
		s.failPolicy(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, OverrideResponse{ComputedAt: s.now(), OverrideJSON: overrideJSON(o), Changed: &changed})
}

func (s *Service) deleteOverride(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	if _, err := s.Store.ClearDeviceOverride(r.Context(), p.Scope, r.PathValue("id"), actorOf(r, p)); err != nil {
		s.failPolicy(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// patchCustomerPolicy handles a policy_id member of PATCH /api/v1/customers/{id}
// (M2-W2A): changing a customer's policy moves its devices to other settings, so
// it needs the token of the assignment preview (POST /policies/{id}/impact with
// assign_customer) for exactly this assignment, and it is the only change the
// request may make. It reports whether it answered the request; false means the
// policy_id equals the current assignment (null is the default) and the rest of
// the patch proceeds as before.
func (s *Service) patchCustomerPolicy(w http.ResponseWriter, r *http.Request, p *auth.Principal, req PatchCustomerRequest) bool {
	id := r.PathValue("id")
	cur, err := s.Store.GetCustomer(r.Context(), p.Scope, id)
	if err != nil {
		s.failM2(w, r, err)
		return true
	}
	target := ""
	if !req.PolicyID.Null {
		target = req.PolicyID.Value
	}
	if target == cur.PolicyID {
		return false
	}
	var fields []store.FieldError
	if req.Name.Set || req.Slug.Set || req.Expected.Set || req.OnboardingUntil.Set || req.DisplayTZ.Set || req.Contacts.Set || req.DeviceCap.Set || req.Notes.Set {
		fields = append(fields, store.FieldError{Field: "policy_id", Message: "Change the policy on its own: send no other field in the same request."})
	}
	if req.ImpactToken == "" {
		fields = append(fields,
			store.FieldError{Field: "policy_id", Message: "Changing the policy needs its blast-radius preview first (POST /api/v1/policies/{id}/impact with assign_customer)."},
			store.FieldError{Field: "impact_token", Message: "Preview the assignment first: its impact token is required."})
	}
	if len(fields) > 0 {
		s.failPolicy(w, r, &store.ValidationError{Fields: fields})
		return true
	}
	if _, err := s.Store.AssignCustomerPolicyAudited(r.Context(), p.Scope, id, target, req.ImpactToken, actorOf(r, p)); err != nil {
		s.failPolicy(w, r, err)
		return true
	}
	resp, err := s.customerResponse(r, p, id)
	if err != nil {
		s.failM2(w, r, err)
		return true
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
	return true
}
