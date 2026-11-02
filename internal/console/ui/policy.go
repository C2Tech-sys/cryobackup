package ui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/health"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
	"github.com/c2tech-sys/cryobackup/internal/policy"
)

// Policies (S24 list, S25 editor, ticket M2-W2A). Reading the list needs
// read_only; the editor, every save and every delete need admin. A save is two
// steps: the editor posts to /impact, which shows the blast radius (devices and
// changed fields, old to new) and carries a token; the confirmed post applies
// exactly what was previewed or is refused.

// ---- the list ----

// PolRow is one line of the policy list.
type PolRow struct {
	ID, Name, Desc string
	Href           string // the editor, for an admin
	Scope          string // default | template | customer
	ScopeLabel     string
	Customer       string
	CustomerHref   string
	Version        int
	Customers      string
	Devices        string
	Updated, ISO   string
}

// PolicyListPage is the policy list.
type PolicyListPage struct {
	Page
	Rows    []PolRow
	CanEdit bool
	Empty   *EmptyView
	Summary string
	Saved   string
}

func polSavedText(code string) string {
	switch code {
	case "created":
		return "Policy created."
	case "1":
		return "Policy saved. Agents pick it up at their next check-in."
	case "deleted":
		return "Policy deleted."
	case "assigned":
		return "Policy assigned. Agents pick it up at their next check-in."
	case "override":
		return "Override saved. The agent picks it up at its next check-in."
	case "cleared":
		return "Override cleared. The device follows its policy again."
	}
	return ""
}

func (s *Service) policies(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	fleet, ok := s.fleetFor(w, r, p, "Policies")
	if !ok {
		return
	}
	list, err := s.Store.ListPolicySummaries(r.Context(), p.Scope)
	if err != nil {
		s.log().Error("ui: policy list failed", logx.Err(err))
		s.render(w, r, http.StatusServiceUnavailable, "error", "base", errorData(s.page(r.Context(), p, fleet, "Policies unavailable", "policies", ""), 503, "Policies unavailable",
			"The policy list could not be read. This is an alarm, not an empty list. Try again shortly."))
		return
	}
	c := s.fcAt(r.Context(), fleet.Computed)
	pg := PolicyListPage{Page: s.page(r.Context(), p, fleet, "Policies", "policies", ""), CanEdit: p.Role >= auth.RoleAdmin, Saved: polSavedText(r.URL.Query().Get("saved"))}
	for _, sm := range list {
		row := PolRow{ID: sm.ID, Name: sm.Name, Desc: sm.Description, Scope: sm.Scope, Version: sm.Version,
			Customers: plural(sm.Customers, "customer", "customers"), Devices: plural(sm.Devices, "device", "devices"),
			Updated: c.dayClock(sm.UpdatedAt), ISO: sm.UpdatedAt.UTC().Format(time.RFC3339)}
		switch sm.Scope {
		case "default":
			row.ScopeLabel = "DEFAULT"
		case "template":
			row.ScopeLabel = "TEMPLATE"
		default:
			row.ScopeLabel = "CUSTOMER"
			row.Customer, row.CustomerHref = sm.CustomerName, "/customers/"+sm.CustomerID+"/policy"
		}
		if pg.CanEdit {
			row.Href = "/policies/" + sm.ID
		}
		pg.Rows = append(pg.Rows, row)
	}
	pg.Summary = fmt.Sprintf("%s. Counts cover the customers this account can see.", plural(len(pg.Rows), "policy", "policies"))
	if len(pg.Rows) == 0 {
		// The default policy is created by init; a console without one cannot evaluate any device.
		pg.Empty = &EmptyView{Kind: "alarm", Title: "No default policy exists",
			Body: "Every device's effective policy starts from the default policy. Without one the fleet board cannot evaluate a device. Run cryo-console init, or restore the database."}
	}
	s.render(w, r, http.StatusOK, "policies", "base", pg)
}

// ---- the editor ----

// PolHead identifies the policy being edited.
type PolHead struct {
	ID, Name, Scope, ScopeLabel string
	Version                     int
	Customer, CustomerHref      string
}

// PolRevView is a revision line: who changed it and when.
type PolRevView struct {
	Version      int
	By, When     string
	ISO          string
	Note         string
	Current      bool
	Fields       string
	HasBy        bool
	RemovedActor bool
}

// PolHidden is a posted value carried through the review step.
type PolHidden struct{ Name, Value string }

// PolicyPage is the policy editor and the pages around it (new, review, delete).
type PolicyPage struct {
	Page
	// Mode is edit, new, review, delete or blocked.
	Mode      string
	Head      PolHead
	Action    string // the form's post target
	ReviewURL string
	EditURL   string // "Edit again" from the review
	ConfirmTo string
	Name      string
	Desc      string
	Note      string
	Class     string
	Classes   []Option
	CustOpts  []Option
	NameErr   string
	DescErr   string
	CustErr   string
	Sections  []PolSectionView
	Summary   []PolErr
	SecNav    []PolSectionView // section links
	Revisions []PolRevView
	Review    *PolReview
	Hidden    []PolHidden
	Saved     string
	Notice    *CauseView
	Repair    *PolRepair // M2-W3A: the stored parts a broken policy could not carry
	CanDelete bool
	WhyNot    string
	IsDefault bool
	Sparse    bool
	CSRFToken string
}

// editorState is everything the editor needs besides the policy itself.
type editorState struct {
	vals url.Values
	errs map[string]string
	name string
	desc string
	note string
}

// policyBase composes the policy a record stands for on its own (the editor
// shows the full document: the default's fields with the policy's on top) and
// returns the default policy too, which a sparse form is a diff against.
func (s *Service) policyBase(ctx context.Context, rec store.PolicyRecord) (base, def policy.Policy, defSections map[string]json.RawMessage, err error) {
	defSections, err = policy.Encode(policy.Default(policy.ClassUnknown))
	if err != nil {
		return
	}
	if d, derr := s.Store.DefaultPolicy(ctx); derr == nil {
		defSections = d.Sections
	} else if !errors.Is(derr, store.ErrNotFound) {
		err = derr
		return
	}
	if def, err = store.ComposePolicy(defSections, nil); err != nil {
		return
	}
	if rec.Scope == "default" {
		base, err = store.ComposePolicy(rec.Sections, nil)
		return
	}
	base, err = store.ComposePolicy(defSections, rec.Sections)
	if err != nil {
		// A stored policy that does not compose must stay editable (errPolicyBroken).
		// The form starts from the parts of it that still compose, so a repair keeps
		// every other setting; only what cannot be carried falls back to the default
		// (policy_repair.go names those parts).
		base = def
		if plan, ok := planRepair(defSections, rec.Sections); ok {
			base = plan.Base
		}
		err = errPolicyBroken
	}
	return
}

// errPolicyBroken: the stored policy does not compose over the default.
var errPolicyBroken = errors.New("ui: the stored policy does not compose")

func headOf(rec store.PolicyRecord, customer string) PolHead {
	h := PolHead{ID: rec.ID, Name: rec.Name, Scope: rec.Scope, Version: rec.Version, Customer: customer}
	switch rec.Scope {
	case "default":
		h.ScopeLabel = "DEFAULT"
	case "template":
		h.ScopeLabel = "TEMPLATE"
	default:
		h.ScopeLabel = "CUSTOMER"
		h.CustomerHref = "/customers/" + rec.CustomerID + "/policy"
	}
	return h
}

func (s *Service) customerNameOf(ctx context.Context, p *auth.Principal, id string) string {
	if id == "" {
		return ""
	}
	c, err := s.Store.GetCustomer(ctx, p.Scope, id)
	if err != nil {
		return ""
	}
	return c.Name
}

func (s *Service) revisionViews(ctx context.Context, p *auth.Principal, rec store.PolicyRecord, c fmtCtx) []PolRevView {
	revs, err := s.Store.ListPolicyRevisionViews(ctx, p.Scope, rec.ID, 12)
	if err != nil {
		return nil
	}
	out := make([]PolRevView, 0, len(revs))
	for _, r := range revs {
		v := PolRevView{Version: r.Version, When: c.dayClock(r.CreatedAt), ISO: r.CreatedAt.UTC().Format(time.RFC3339), Note: r.ChangeNote,
			Current: r.Version == rec.Version, By: r.ChangedByName, HasBy: r.ChangedByName != ""}
		if !v.HasBy {
			v.By = "system"
			if r.ChangedBy != "" {
				v.By, v.RemovedActor = "a removed account", true
			}
		}
		out = append(out, v)
	}
	return out
}

func (s *Service) editorPage(ctx context.Context, p *auth.Principal, fleet health.Fleet, rec store.PolicyRecord, st editorState, status int) PolicyPage {
	c := s.fcAt(ctx, fleet.Computed)
	title := "Edit policy"
	pg := PolicyPage{Page: s.page(ctx, p, fleet, title, "policies", ""), Mode: "edit", Head: headOf(rec, s.customerNameOf(ctx, p, rec.CustomerID)),
		Action: "/ui/policies/" + rec.ID + "/impact", Name: st.name, Desc: st.desc, Note: st.note, IsDefault: rec.Scope == "default", Sparse: rec.Scope != "default"}
	pg.Class = rec.TemplateDeviceClass
	pg.Sections = buildSections(st.vals, st.errs, nil)
	if plan := s.repairPlanFor(ctx, rec); plan != nil { // M2-W3A: flag the stored parts the form could not carry
		flagSections(pg.Sections, plan)
		pg.Repair = repairView(plan)
	}
	pg.SecNav = pg.Sections
	pg.NameErr, pg.DescErr = st.errs["name"], st.errs["description"]
	pg.Summary = polSummary(st.errs, nil)
	pg.Revisions = s.revisionViews(ctx, p, rec, c)
	switch {
	case rec.Scope == "default":
		pg.WhyNot = "The default policy is the base of every other policy and cannot be deleted."
	default:
		pg.CanDelete = true
	}
	return pg
}

func (s *Service) loadPolicy(w http.ResponseWriter, r *http.Request, p *auth.Principal) (store.PolicyRecord, bool) {
	rec, err := s.Store.GetPolicy(r.Context(), p.Scope, r.PathValue("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.notFound(w, r, p, "This policy")
			return store.PolicyRecord{}, false
		}
		s.log().Error("ui: policy read failed", logx.Err(err))
		s.errorPage(w, r, p, http.StatusServiceUnavailable, "Policy unavailable", "The policy could not be read. Try again shortly.")
		return store.PolicyRecord{}, false
	}
	return rec, true
}

func (s *Service) policyEdit(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	fleet, ok := s.fleetFor(w, r, p, "Policy")
	if !ok {
		return
	}
	rec, ok := s.loadPolicy(w, r, p)
	if !ok {
		return
	}
	base, _, _, err := s.policyBase(r.Context(), rec)
	broken := errors.Is(err, errPolicyBroken)
	if err != nil && !broken {
		s.log().Error("ui: policy base failed", logx.Err(err))
		s.errorPage(w, r, p, http.StatusInternalServerError, "Policy unavailable", "The policy could not be read. Try again shortly.")
		return
	}
	tree, err := treeOf(base)
	if err != nil {
		s.storeFailed(w, r, p, err)
		return
	}
	pg := s.editorPage(r.Context(), p, fleet, rec, editorState{vals: formFromDoc(tree), errs: map[string]string{}, name: rec.Name, desc: rec.Description}, http.StatusOK)
	pg.Saved = polSavedText(r.URL.Query().Get("saved"))
	if broken {
		// An alarm, not a dead end: the form starts from the default and a save repairs the policy.
		s.log().Error("ui: stored policy does not compose", logx.Class("policy"))
		pg.Notice = &CauseView{Class: ClassCrit, Title: "This policy does not compose with the default policy.",
			Text: "Devices that use it cannot be served a valid policy. The form below starts from the settings of this policy that still work; the parts that do not are flagged and show the default. Saving replaces what is stored, after the usual review, which lists what is dropped."}
		if pg.Repair != nil {
			pg.Notice.Text = "Devices that use it cannot be served a valid policy. " + plural(len(pg.Repair.Items), "stored setting", "stored settings") +
				" could not be used and are flagged below, with the default shown in place. Everything else this policy carries is kept. Saving replaces what is stored, after the usual review, which lists what is dropped."
		}
	}
	s.render(w, r, http.StatusOK, "policy", "base", pg)
}

// proposal reads the posted editor into the store's types. The returned map holds
// the errors of the controls (by catalogue path, "name" and "description" too).
func (s *Service) proposal(ctx context.Context, rec store.PolicyRecord, vals url.Values) (store.ProposedPolicy, map[string]string) {
	errs := map[string]string{}
	base, def, _, err := s.policyBase(ctx, rec)
	broken := errors.Is(err, errPolicyBroken)
	if err != nil && !broken {
		errs["_"] = "The stored policy could not be read."
		return store.ProposedPolicy{}, errs
	}
	tree, err := treeOf(base)
	if err != nil {
		errs["_"] = "The stored policy could not be read."
		return store.ProposedPolicy{}, errs
	}
	for _, pe := range applyForm(tree, vals) {
		errs[pe.Key] = pe.Msg
	}
	name, desc := strings.TrimSpace(vals.Get("name")), strings.TrimSpace(strings.ReplaceAll(vals.Get("description"), "\r\n", "\n"))
	if name == "" {
		errs["name"] = "Name the policy (1 to 100 characters)."
	}
	// The controls that could not be read keep their stored value here, so the
	// engine still judges everything else and one post shows every problem.
	doc, err := json.Marshal(tree)
	if err != nil {
		errs["_"] = "The form could not be read."
		return store.ProposedPolicy{}, errs
	}
	pol, err := policy.DecodeDocument(doc, false)
	if err == nil {
		err = pol.Validate()
	}
	if err != nil {
		var ve *policy.ValidationError
		if errors.As(err, &ve) {
			for _, pr := range ve.Problems {
				k := fieldKeyFor(pr.Path)
				if k == "" {
					k = pr.Path
				}
				if errs[k] == "" {
					errs[k] = pr.Message
				}
			}
		} else {
			errs["_"] = "The settings do not form a valid policy."
		}
	}
	if len(errs) > 0 {
		return store.ProposedPolicy{}, errs
	}
	var sections map[string]json.RawMessage
	switch {
	case !broken && sameEffective(base, pol):
		// Nothing in the form differs from what is stored: send the stored sections
		// back verbatim, so an unchanged save is "Nothing to save" and an explicit
		// pin (a field set to the default's own value) is never dropped.
		sections = rec.Sections
	case rec.Scope == "default":
		sections, err = store.PolicySections(pol)
	default:
		sections, err = store.SparseWithPins(def, pol, rec.Sections)
	}
	if err != nil {
		errs["_"] = "The settings could not be stored."
		return store.ProposedPolicy{}, errs
	}
	return store.ProposedPolicy{Name: &name, Description: &desc, Sections: sections, Note: strings.TrimSpace(vals.Get("note"))}, errs
}

// sameEffective reports whether two policies are equal field for field.
func sameEffective(a, b policy.Policy) bool {
	x, err1 := policy.Canonical(a)
	y, err2 := policy.Canonical(b)
	return err1 == nil && err2 == nil && string(x) == string(y)
}

// absorbStoreErrs maps the store's field errors (document paths) onto catalogue keys.
func absorbStoreErrs(errs map[string]string, err error) bool {
	fe := store.FieldErrors(err)
	for _, f := range fe {
		k := f.Field
		switch f.Field {
		case "name", "description", "customer_id", "note":
		default:
			if kk := fieldKeyFor(f.Field); kk != "" {
				k = kk
			}
		}
		if errs[k] == "" {
			errs[k] = f.Message
		}
	}
	return len(fe) > 0
}

func hiddenOf(form url.Values) []PolHidden {
	keys := make([]string, 0, len(form))
	for k := range form {
		if k == "csrf_token" || k == "impact_token" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []PolHidden
	for _, k := range keys {
		for _, v := range form[k] {
			out = append(out, PolHidden{Name: k, Value: v})
		}
	}
	return out
}

func (s *Service) renderEditorAgain(w http.ResponseWriter, r *http.Request, p *auth.Principal, fleet health.Fleet, rec store.PolicyRecord, vals url.Values, errs map[string]string, status int, notice *CauseView) {
	pg := s.editorPage(r.Context(), p, fleet, rec, editorState{vals: vals, errs: errs, name: vals.Get("name"), desc: vals.Get("description"), note: vals.Get("note")}, status)
	pg.Notice = notice
	if len(pg.Summary) > 0 || errs["_"] != "" {
		if errs["_"] != "" {
			pg.Summary = append(pg.Summary, PolErr{Anchor: "pol-identity", Label: "Policy", Msg: errs["_"]})
		}
		if pg.Notice == nil {
			pg.Notice = &CauseView{Class: ClassCrit, Title: "Nothing was saved.", Text: fmt.Sprintf("Fix %s below.", plural(len(pg.Summary), "problem", "problems"))}
		}
	}
	s.render(w, r, status, "policy", "base", pg)
}

// policyImpact is POST /ui/policies/{id}/impact: validate the posted editor and
// show the blast radius before anything is saved.
func (s *Service) policyImpact(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	r.Body = http.MaxBytesReader(w, r.Body, 256<<10)
	if err := r.ParseForm(); err != nil {
		s.errorPage(w, r, p, http.StatusBadRequest, "Form not read", "The form could not be read.")
		return
	}
	fleet, ok := s.fleetFor(w, r, p, "Policy")
	if !ok {
		return
	}
	rec, ok := s.loadPolicy(w, r, p)
	if !ok {
		return
	}
	prop, errs := s.proposal(r.Context(), rec, r.PostForm)
	if len(errs) == 0 {
		rep, err := s.Store.PreviewPolicyChange(r.Context(), p.Scope, rec.ID, p.Admin.ID, prop)
		if err != nil {
			if !absorbStoreErrs(errs, err) {
				s.storeFailed(w, r, p, err)
				return
			}
		} else {
			s.renderReview(w, r, p, fleet, rec, r.PostForm, prop, rep)
			return
		}
	}
	s.renderEditorAgain(w, r, p, fleet, rec, r.PostForm, errs, http.StatusBadRequest, nil)
}

func (s *Service) renderReview(w http.ResponseWriter, r *http.Request, p *auth.Principal, fleet health.Fleet, rec store.PolicyRecord, form url.Values, prop store.ProposedPolicy, rep store.ImpactReport) {
	c := s.fcAt(r.Context(), fleet.Computed)
	pg := PolicyPage{Page: s.page(r.Context(), p, fleet, "Review policy change", "policies", ""), Mode: "review", Head: headOf(rec, s.customerNameOf(r.Context(), p, rec.CustomerID)),
		ConfirmTo: "/ui/policies/" + rec.ID, EditURL: "/ui/policies/" + rec.ID + "/edit", Hidden: hiddenOf(form)}
	rv := &PolReview{Kind: "update", Population: rep.Population, Devices: rep.Devices, Held: rep.Population - rep.Devices, Changes: changeRows(rep.Changes),
		Customers: impactCusts(rep.Customers), Unchanged: rep.Unchanged, Token: rep.Token, Expires: c.clock(rep.ExpiresAt),
		Confirm: fmt.Sprintf("Save %s (version %d)", rec.Name, rec.Version+1)}
	if rep.NameChange != nil {
		rv.Name = &PolChangeRow{Section: "Policy", Label: "Name", Old: rep.NameChange.Old, New: rep.NameChange.New}
	}
	switch {
	case rep.Unchanged:
		rv.Headline = "Nothing to save"
		rv.Lead = "These settings are the same as the stored policy. No version is created and no device is told anything."
	case rep.Population == 0:
		rv.Headline = "This change affects no devices"
		rv.Lead = noPopulationLead(rec)
	default:
		rv.Headline = "This change affects " + plural(rep.Devices, "device", "devices")
		rv.Lead = fmt.Sprintf("%s run under %s in %s. Agents pick up version %d at their next check-in.",
			plural(rep.Population, "device", "devices"), rec.Name, plural(len(rep.Customers), "customer", "customers"), rec.Version+1)
		if rec.Scope == "default" {
			rv.Lead = fmt.Sprintf("%s in %s inherit from the default policy. Agents pick up the change at their next check-in.",
				plural(rep.Population, "device", "devices"), plural(len(rep.Customers), "customer", "customers"))
		}
	}
	if rep.Repairs {
		rv.Lead += " The stored policy did not compose with the default policy; this change repairs it, and the changes below are measured against the default."
		dropped, changes, ok := repairReviewImpl(s, r.Context(), rec, prop) // M2-W3A: measured against what was kept, with what is dropped listed
		if !ok {
			// Never offer a repair whose drops cannot be listed: the review is the operator's only sight of them.
			s.log().Error("ui: repair review failed", logx.Class("policy"))
			s.errorPage(w, r, p, http.StatusInternalServerError, "Repair cannot be previewed",
				"This policy does not compose, and what a repair would drop could not be worked out, so nothing was saved and no review can be shown. Try again; if it persists, check the console log.")
			return
		}
		rv.Dropped, rv.Changes = dropped, changes
		rv.Lead = strings.Replace(rv.Lead, "and the changes below are measured against the default.",
			"the parts of it that still compose are kept, the parts listed under Not carried over are dropped, and the changes below are measured against what was kept.", 1)
	}
	pg.Review = rv
	pg.Name = rec.Name
	s.render(w, r, http.StatusOK, "policy", "base", pg)
}

func noPopulationLead(rec store.PolicyRecord) string {
	if rec.Scope == "default" {
		return "No enrolled device inherits from the default policy yet."
	}
	return "No customer uses this policy yet. Assign it to a customer to put it to work."
}

// policyEditAgain is POST /ui/policies/{id}/edit: the review's "Edit again".
func (s *Service) policyEditAgain(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	r.Body = http.MaxBytesReader(w, r.Body, 256<<10)
	if err := r.ParseForm(); err != nil {
		s.errorPage(w, r, p, http.StatusBadRequest, "Form not read", "The form could not be read.")
		return
	}
	fleet, ok := s.fleetFor(w, r, p, "Policy")
	if !ok {
		return
	}
	rec, ok := s.loadPolicy(w, r, p)
	if !ok {
		return
	}
	s.renderEditorAgain(w, r, p, fleet, rec, r.PostForm, map[string]string{}, http.StatusOK, nil)
}

// policySave is POST /ui/policies/{id}: the confirmed save, with the impact token.
func (s *Service) policySave(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	r.Body = http.MaxBytesReader(w, r.Body, 256<<10)
	if err := r.ParseForm(); err != nil {
		s.errorPage(w, r, p, http.StatusBadRequest, "Form not read", "The form could not be read.")
		return
	}
	fleet, ok := s.fleetFor(w, r, p, "Policy")
	if !ok {
		return
	}
	rec, ok := s.loadPolicy(w, r, p)
	if !ok {
		return
	}
	prop, errs := s.proposal(r.Context(), rec, r.PostForm)
	if len(errs) > 0 {
		s.renderEditorAgain(w, r, p, fleet, rec, r.PostForm, errs, http.StatusBadRequest, nil)
		return
	}
	broken, berr := s.isBroken(r.Context(), rec)
	if berr != nil { // M2-W3E: fail closed. Without the default policy nothing can say whether this save is a repair.
		s.log().Error("ui: policy save refused, default policy unreadable", logx.Err(berr), logx.Class("policy"))
		s.errorPage(w, r, p, http.StatusInternalServerError, "Policy cannot be saved",
			"The default policy could not be read, so this save could not be checked and nothing was saved. Try again; if it persists, check the console log.")
		return
	}
	if broken { // M2-W3A: a repair is saved only when its drops can be listed
		if _, _, ok := repairReviewImpl(s, r.Context(), rec, prop); !ok {
			s.log().Error("ui: repair save refused, drops unknown", logx.Class("policy"))
			s.errorPage(w, r, p, http.StatusInternalServerError, "Repair cannot be saved",
				"This policy does not compose, and what a repair would drop could not be worked out, so nothing was saved. Try again; if it persists, check the console log.")
			return
		}
	}
	_, changed, err := s.Store.UpdatePolicyAudited(r.Context(), p.Scope, rec.ID, prop, r.PostFormValue("impact_token"), s.actor(r, p))
	switch {
	case err == nil:
		if changed {
			s.changed()
		}
		http.Redirect(w, r, "/policies/"+rec.ID+"?saved=1", http.StatusSeeOther)
	case errors.Is(err, store.ErrImpactStale), errors.Is(err, store.ErrImpactMismatch):
		why := "The policy changed (or the preview expired) after you previewed it, so nothing was saved."
		if errors.Is(err, store.ErrImpactMismatch) {
			why = "The settings you sent are not the ones that were previewed, so nothing was saved."
		}
		// Reload so the editor shows the stored version, keep the posted values for another review.
		cur, gerr := s.Store.GetPolicy(r.Context(), p.Scope, rec.ID)
		if gerr != nil {
			cur = rec
		}
		s.renderEditorAgain(w, r, p, fleet, cur, r.PostForm, map[string]string{}, http.StatusConflict,
			&CauseView{Class: ClassWarn, Title: "Preview again.", Text: why + " Your values are kept below; review the change once more."})
	default:
		if absorbStoreErrs(errs, err) {
			s.renderEditorAgain(w, r, p, fleet, rec, r.PostForm, errs, http.StatusBadRequest, nil)
			return
		}
		s.storeFailed(w, r, p, err)
	}
}

// ---- new ----

func (s *Service) newPolicyPage(ctx context.Context, p *auth.Principal, fleet health.Fleet, st editorState, customerID string) PolicyPage {
	pg := PolicyPage{Page: s.page(ctx, p, fleet, "New policy", "policies", ""), Mode: "new", Action: "/ui/policies", Name: st.name, Desc: st.desc, Note: st.note, Sparse: true,
		Head: PolHead{Name: "New policy", Scope: "template", ScopeLabel: "NEW"}}
	pg.Sections = buildSections(st.vals, st.errs, nil)
	pg.SecNav = pg.Sections
	pg.NameErr, pg.DescErr, pg.CustErr = st.errs["name"], st.errs["description"], st.errs["customer_id"]
	pg.Summary = polSummary(st.errs, nil)
	pg.CustOpts = []Option{{Value: "", Label: "Template: any customer can use it", Selected: customerID == ""}}
	if cs, err := s.Store.ListCustomers(ctx, p.Scope); err == nil {
		for _, c := range cs {
			if c.State == "archived" {
				continue
			}
			pg.CustOpts = append(pg.CustOpts, Option{Value: c.ID, Label: "Only for " + c.Name, Selected: c.ID == customerID})
		}
	}
	pg.Classes = []Option{{Value: "", Label: "Any device", Selected: st.vals.Get("template_device_class") == ""},
		{Value: "laptop", Label: "Laptops", Selected: st.vals.Get("template_device_class") == "laptop"},
		{Value: "desktop", Label: "Desktops", Selected: st.vals.Get("template_device_class") == "desktop"}}
	return pg
}

func (s *Service) defaultFormValues(ctx context.Context) (url.Values, error) {
	var rec store.PolicyRecord
	if d, err := s.Store.DefaultPolicy(ctx); err == nil {
		rec = d
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	base, _, _, err := s.policyBase(ctx, store.PolicyRecord{Scope: "default", Sections: rec.Sections})
	if err != nil {
		return nil, err
	}
	tree, err := treeOf(base)
	if err != nil {
		return nil, err
	}
	return formFromDoc(tree), nil
}

func (s *Service) policyNew(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	fleet, ok := s.fleetFor(w, r, p, "New policy")
	if !ok {
		return
	}
	vals, err := s.defaultFormValues(r.Context())
	if err != nil {
		s.storeFailed(w, r, p, err)
		return
	}
	cust := r.URL.Query().Get("customer")
	if cust != "" {
		if _, err := s.Store.GetCustomer(r.Context(), p.Scope, cust); err != nil {
			s.notFound(w, r, p, "This customer")
			return
		}
	}
	s.render(w, r, http.StatusOK, "policy", "base", s.newPolicyPage(r.Context(), p, fleet, editorState{vals: vals, errs: map[string]string{}}, cust))
}

func (s *Service) policyCreate(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	r.Body = http.MaxBytesReader(w, r.Body, 256<<10)
	if err := r.ParseForm(); err != nil {
		s.errorPage(w, r, p, http.StatusBadRequest, "Form not read", "The form could not be read.")
		return
	}
	fleet, ok := s.fleetFor(w, r, p, "New policy")
	if !ok {
		return
	}
	cust := r.PostFormValue("customer_id")
	// the default document is the base of a new policy, so the sparse diff is against it
	prop, errs := s.proposal(r.Context(), store.PolicyRecord{Scope: "template"}, r.PostForm)
	if len(errs) == 0 {
		in := store.PolicyInput{Scope: "template", Name: *prop.Name, Description: *prop.Description, Sections: prop.Sections, Note: prop.Note,
			TemplateDeviceClass: r.PostFormValue("template_device_class")}
		if cust != "" {
			in.Scope, in.CustomerID, in.TemplateDeviceClass = "customer", cust, ""
		}
		rec, err := s.Store.CreatePolicyAudited(r.Context(), p.Scope, in, s.actor(r, p))
		if err == nil {
			s.changed()
			http.Redirect(w, r, "/policies/"+rec.ID+"?saved=created", http.StatusSeeOther)
			return
		}
		if !absorbStoreErrs(errs, err) {
			s.storeFailed(w, r, p, err)
			return
		}
	}
	pg := s.newPolicyPage(r.Context(), p, fleet, editorState{vals: r.PostForm, errs: errs, name: r.PostFormValue("name"), desc: r.PostFormValue("description"), note: r.PostFormValue("note")}, cust)
	if len(pg.Summary) > 0 {
		pg.Notice = &CauseView{Class: ClassCrit, Title: "The policy was not created.", Text: fmt.Sprintf("Fix %s below.", plural(len(pg.Summary), "problem", "problems"))}
	}
	s.render(w, r, http.StatusBadRequest, "policy", "base", pg)
}

// ---- delete ----

func (s *Service) policyDeletePage(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	fleet, ok := s.fleetFor(w, r, p, "Policy")
	if !ok {
		return
	}
	rec, ok := s.loadPolicy(w, r, p)
	if !ok {
		return
	}
	s.renderDelete(w, r, p, fleet, rec, http.StatusOK)
}

func (s *Service) renderDelete(w http.ResponseWriter, r *http.Request, p *auth.Principal, fleet health.Fleet, rec store.PolicyRecord, status int) {
	c := s.fcAt(r.Context(), fleet.Computed)
	pg := PolicyPage{Page: s.page(r.Context(), p, fleet, "Delete policy", "policies", ""), Mode: "delete", Head: headOf(rec, s.customerNameOf(r.Context(), p, rec.CustomerID)),
		ConfirmTo: "/ui/policies/" + rec.ID + "/delete"}
	rep, err := s.Store.PreviewPolicyDelete(r.Context(), p.Scope, rec.ID, p.Admin.ID)
	var inUse *store.PolicyInUseError
	switch {
	case err == nil:
		pg.Review = &PolReview{Kind: "delete", Headline: "This delete affects no devices", Population: rep.Population, Token: rep.Token, Expires: c.clock(rep.ExpiresAt),
			Lead:    fmt.Sprintf("%s is assigned to no customer. Its %s are deleted with it; devices keep following their own customer's policy.", rec.Name, plural(rec.Version, "revision", "revisions")),
			Confirm: "Delete " + rec.Name}
	case errors.As(err, &inUse):
		pg.Mode = "blocked"
		pg.Review = &PolReview{Kind: "delete", Headline: "This policy is still in use", Blockers: impactCusts(inUse.Customers),
			Lead: fmt.Sprintf("%s is assigned to %s. Move them to another policy first, then delete this one.", rec.Name, plural(len(inUse.Customers), "customer", "customers"))}
		for _, b := range inUse.Customers {
			pg.Review.Population += b.Devices
		}
	case errors.Is(err, store.ErrDefaultPolicy):
		pg.Mode = "blocked"
		pg.Review = &PolReview{Kind: "delete", Headline: "The default policy cannot be deleted", Lead: "Every other policy is a set of differences from it, and customers with no policy of their own run on it. Edit it instead."}
	default:
		s.storeFailed(w, r, p, err)
		return
	}
	s.render(w, r, status, "policy", "base", pg)
}

func (s *Service) policyDelete(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if err := r.ParseForm(); err != nil {
		s.errorPage(w, r, p, http.StatusBadRequest, "Form not read", "The form could not be read.")
		return
	}
	rec, ok := s.loadPolicy(w, r, p)
	if !ok {
		return
	}
	err := s.Store.DeletePolicyAudited(r.Context(), p.Scope, rec.ID, r.PostFormValue("impact_token"), s.actor(r, p))
	var inUse *store.PolicyInUseError
	switch {
	case err == nil:
		s.changed()
		http.Redirect(w, r, "/policies?saved=deleted", http.StatusSeeOther)
	case errors.As(err, &inUse), errors.Is(err, store.ErrDefaultPolicy), errors.Is(err, store.ErrImpactStale), errors.Is(err, store.ErrImpactMismatch), len(store.FieldErrors(err)) > 0:
		fleet, ok := s.fleetFor(w, r, p, "Policy")
		if !ok {
			return
		}
		s.renderDelete(w, r, p, fleet, rec, http.StatusConflict)
	default:
		s.storeFailed(w, r, p, err)
	}
}

// ---- live window validation ----

// WindowCheck is the fragment the live validator returns.
type WindowCheck struct {
	ID   string
	Grid *WinGrid
}

// policyWindowValidate is POST /ui/policy/window/validate (architecture 13.1):
// the window, or the throttle form, read by the policy parser; the answer is the
// parsed 7x24 grid and canonical text, or the first error with its position.
func (s *Service) policyWindowValidate(w http.ResponseWriter, r *http.Request) {
	if allowed, wait := s.winRL.Allow(principal(r).SessionID); !allowed {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait/time.Second)+1))
		http.Error(w, "Too many checks. Wait a moment and keep typing.", http.StatusTooManyRequests)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, windowCheckCap)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "The form could not be read.", http.StatusBadRequest)
		return
	}
	var id, text string
	for k, v := range r.PostForm {
		if strings.HasPrefix(k, "f.") && len(v) > 0 {
			id, text = fieldID(strings.TrimPrefix(k, "f.")), v[0]
		}
	}
	if id == "" {
		http.Error(w, "No field to check.", http.StatusBadRequest)
		return
	}
	optional := r.PostFormValue("optional") == "1"
	s.render(w, r, http.StatusOK, "policy", "pol_window_result", WindowCheck{ID: id, Grid: windowGrid(strings.TrimSpace(text), optional)})
}
