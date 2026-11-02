package ui

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/health"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
)

// EffRow is one field of an effective policy with where its value came from.
type EffRow struct {
	Path, Label, Value  string
	Source, SrcLabel    string
	SrcClass, SourceTxt string
	Inherited           string
}

// EffGroup is the rows of one section.
type EffGroup struct {
	Key, Title string
	Rows       []EffRow
	Set        int // rows that do not come from the default
}

func srcLabelOf(src string) (label, class string) {
	switch src {
	case store.SourceOverride:
		return "OVERRIDE", ClassInfo
	case store.SourcePolicy:
		return "POLICY", ClassNeutral
	}
	return "DEFAULT", ClassNeutral
}

// effGroups groups an effective policy's fields by section, in catalogue order,
// each labelled with its source. policyText names the layer "policy" stands for.
func effGroups(fields []store.EffectiveField, policyText string) []EffGroup {
	byPath := map[string]store.EffectiveField{}
	paths := make([]string, 0, len(fields))
	for _, f := range fields {
		if f.Value == "{}" {
			continue // an empty map (no per-folder flags) says nothing
		}
		byPath[f.Path] = f
		paths = append(paths, f.Path)
	}
	sortByCatalogue(paths)
	var out []EffGroup
	idx := map[string]int{}
	for _, p := range paths {
		f := byPath[p]
		sec, _, _ := strings.Cut(p, ".")
		gi, ok := idx[sec]
		if !ok {
			gi = len(out)
			idx[sec] = gi
			out = append(out, EffGroup{Key: sec, Title: sectionTitle(sec)})
		}
		lbl, cls := srcLabelOf(f.Source)
		row := EffRow{Path: p, Label: labelOf(p), Value: fmtValue(p, f.Value), Source: f.Source, SrcLabel: lbl, SrcClass: cls}
		switch f.Source {
		case store.SourceOverride:
			row.SourceTxt = "this device's override"
			row.Inherited = "was " + fmtValue(p, f.Inherited)
		case store.SourcePolicy:
			row.SourceTxt = policyText
			row.Inherited = "default " + fmtValue(p, f.Inherited)
		default:
			row.SourceTxt = "the default policy"
		}
		if f.Source != store.SourceDefault {
			out[gi].Set++
		}
		out[gi].Rows = append(out[gi].Rows, row)
	}
	return out
}

// CustPolicyView is the Policy tab of the customer page (S17).
type CustPolicyView struct {
	Mode         string // view | review
	Assigned     string
	AssignedHref string
	AssignedMeta string
	IsDefault    bool
	DefaultHref  string
	Options      []Option
	Action       string
	ConfirmTo    string
	NewHref      string
	Devices      string
	Overridden   string
	OverrideNote string
	Groups       []EffGroup
	Set          int
	Review       *PolReview
	Hidden       []PolHidden
	Err          string
	Version      int
	CSRFToken    string
	CustomerID   string
	// Broken: the assigned policy does not compose over the default (an alarm).
	Broken bool
}

func scopeWord(scope string) string {
	switch scope {
	case "template":
		return "Template"
	case "customer":
		return "Customer policy"
	}
	return "Default policy"
}

func (s *Service) custPolicyView(csrf string, cp store.CustomerPolicy, selected string) *CustPolicyView {
	v := &CustPolicyView{Mode: "view", CSRFToken: csrf, CustomerID: cp.CustomerID, Action: "/ui/customers/" + cp.CustomerID + "/policy/impact", NewHref: "/policies/new?customer=" + cp.CustomerID,
		Devices: plural(cp.Devices, "active device", "active devices"), DefaultHref: "/policies/" + cp.Default.ID}
	v.Version = cp.Effective.Version
	v.Broken = cp.Broken
	v.Assigned = cp.Effective.PolicyName
	v.AssignedHref = "/policies/" + cp.Effective.PolicyID
	v.IsDefault = cp.Assigned == nil
	v.AssignedMeta = fmt.Sprintf("%s · version %d", scopeWord(cp.Effective.PolicyScope), cp.Effective.Version)
	if cp.Overridden > 0 {
		v.Overridden = plural(cp.Overridden, "device has", "devices have") + " its own override"
		v.OverrideNote = "An override changes only that device's own values; see its Policy tab."
	}
	v.Options = []Option{{Value: "", Label: fmt.Sprintf("Default policy (%s, version %d)", cp.Default.Name, cp.Default.Version), Selected: selected == ""}}
	for _, ch := range cp.Choices {
		lbl := ch.Name + " (template)"
		if ch.Scope == "customer" {
			lbl = ch.Name + " (this customer only)"
		}
		v.Options = append(v.Options, Option{Value: ch.ID, Label: lbl, Selected: ch.ID == selected})
	}
	text := cp.Effective.PolicyName
	v.Groups = effGroups(cp.Effective.Fields, text)
	for _, g := range v.Groups {
		v.Set += g.Set
	}
	return v
}

func (s *Service) customerPolicyPage(w http.ResponseWriter, r *http.Request, p *auth.Principal, fleet health.Fleet, row store.CustomerRow, cp store.CustomerPolicy, status int, mut func(*CustPolicyView), saved string) {
	c := s.fcAt(r.Context(), fleet.Computed)
	sel := ""
	if cp.Assigned != nil {
		sel = cp.Assigned.ID
	}
	v := s.custPolicyView(p.CSRF, cp, sel)
	if mut != nil {
		mut(v)
	}
	pg := CustomerPage{Page: s.page(r.Context(), p, fleet, row.Name, "customers", "/ui/events"), H: s.custHead(p, row, rollupOf(fleet, row.ID), c, "policy"), Tab: "policy", Policy: v, Saved: saved}
	s.render(w, r, status, "customer", "base", pg)
}

func (s *Service) customerPolicy(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	fleet, ok := s.fleetFor(w, r, p, "Customer")
	if !ok {
		return
	}
	row, ok := s.customerRow(w, r, p)
	if !ok {
		return
	}
	cp, err := s.Store.GetCustomerPolicy(r.Context(), p.Scope, row.ID)
	if err != nil {
		s.policyTabFailed(w, r, p, err)
		return
	}
	s.customerPolicyPage(w, r, p, fleet, row, cp, http.StatusOK, nil, polSavedText(r.URL.Query().Get("saved")))
}

// policyTabFailed answers a failed read of policy data: a policy that does not
// compose is an alarm, never an empty tab.
func (s *Service) policyTabFailed(w http.ResponseWriter, r *http.Request, p *auth.Principal, err error) {
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r, p, "This item")
		return
	}
	s.log().Error("ui: policy view failed", logx.Err(err))
	s.errorPage(w, r, p, http.StatusServiceUnavailable, "Policy unavailable",
		"The effective policy could not be composed. This is an alarm, not an empty tab: agents may be running the last good policy or none. Check the default policy and the assigned policy.")
}

// customerPolicyImpact is POST /ui/customers/{id}/policy/impact: show the blast
// radius of the chosen assignment before it is made.
func (s *Service) customerPolicyImpact(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
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
	cp, err := s.Store.GetCustomerPolicy(r.Context(), p.Scope, row.ID)
	if err != nil {
		s.policyTabFailed(w, r, p, err)
		return
	}
	target := r.PostFormValue("policy_id")
	rep, err := s.Store.PreviewPolicyAssignment(r.Context(), p.Scope, row.ID, target, p.Admin.ID)
	if err != nil {
		if len(store.FieldErrors(err)) > 0 {
			s.customerPolicyPage(w, r, p, fleet, row, cp, http.StatusBadRequest, func(v *CustPolicyView) {
				v.Err = "Choose one of the listed policies. Nothing was changed."
			}, "")
			return
		}
		s.storeFailed(w, r, p, err)
		return
	}
	c := s.fcAt(r.Context(), fleet.Computed)
	rv := &PolReview{Kind: "assign", Population: rep.Population, Devices: rep.Devices, Held: rep.Population - rep.Devices, Changes: changeRows(rep.Changes),
		Customers: impactCusts(rep.Customers), Unchanged: rep.Unchanged, Token: rep.Token, Expires: c.clock(rep.ExpiresAt), Confirm: "Assign " + rep.PolicyName}
	switch {
	case rep.Unchanged:
		rv.Headline = "Nothing to change"
		rv.Lead = fmt.Sprintf("%s already uses %s.", row.Name, rep.PolicyName)
	case rep.Population == 0:
		rv.Headline = "This assignment affects no devices"
		rv.Lead = fmt.Sprintf("%s has no active devices yet; the policy applies when the first one enrolls.", row.Name)
	default:
		rv.Headline = "This assignment affects " + plural(rep.Devices, "device", "devices")
		rv.Lead = fmt.Sprintf("%s of %s move to %s. Agents pick up the change at their next check-in.", plural(rep.Population, "device", "devices"), row.Name, rep.PolicyName)
	}
	s.customerPolicyPage(w, r, p, fleet, row, cp, http.StatusOK, func(v *CustPolicyView) {
		v.Mode, v.Review = "review", rv
		v.ConfirmTo = "/ui/customers/" + row.ID + "/policy"
		v.Hidden = []PolHidden{{Name: "policy_id", Value: target}}
	}, "")
}

// customerPolicyAssign is POST /ui/customers/{id}/policy: the confirmed assignment.
func (s *Service) customerPolicyAssign(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
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
	changed, err := s.Store.AssignCustomerPolicyAudited(r.Context(), p.Scope, row.ID, r.PostFormValue("policy_id"), r.PostFormValue("impact_token"), s.actor(r, p))
	switch {
	case err == nil:
		if changed {
			s.changed()
		}
		http.Redirect(w, r, "/customers/"+row.ID+"/policy?saved=assigned", http.StatusSeeOther)
	case errors.Is(err, store.ErrImpactStale), errors.Is(err, store.ErrImpactMismatch), len(store.FieldErrors(err)) > 0:
		cp, gerr := s.Store.GetCustomerPolicy(r.Context(), p.Scope, row.ID)
		if gerr != nil {
			s.policyTabFailed(w, r, p, gerr)
			return
		}
		s.customerPolicyPage(w, r, p, fleet, row, cp, http.StatusConflict, func(v *CustPolicyView) {
			v.Err = "The assignment changed (or the preview expired) after you previewed it, so nothing was changed. Choose the policy again to see the impact."
		}, "")
	default:
		s.storeFailed(w, r, p, err)
	}
}
