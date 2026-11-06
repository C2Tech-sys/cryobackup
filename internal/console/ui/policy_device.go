package ui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/health"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// The Policy tab of the device panel (B07/B12, ticket M2-W2A): the effective
// policy with the source of every field, and the per-device override. Reading
// needs read_only; setting or clearing an override needs admin (architecture
// 5.2: PUT and DELETE devices/{id}/override are AD).

// OvInfo describes the device's current override.
type OvInfo struct {
	Reason  string
	By      string
	When    string
	ISO     string
	Expires string
	Expired bool
	Fields  []string
}

// OvForm is the override editor.
type OvForm struct {
	Action     string
	Sections   []PolSectionView
	Reason     string
	ReasonErr  string
	Expires    string
	ExpiresErr string
	PatchErr   string
	Summary    []PolErr
	Zone       string
	Open       bool
}

// DevPolicyView is the Policy tab.
type DevPolicyView struct {
	PolicyText string
	PolicyHref string
	PolicyMeta string
	// PolicyScope and PolicyVersion are the parts of PolicyMeta the header shows on two lines (M3-T5 ruling 2).
	PolicyScope   string
	PolicyVersion int
	ETag          string
	Override      *OvInfo
	Groups        []EffGroup
	Set           int
	CanEdit       bool
	Form          *OvForm
	ClearAction   string
	CSRFToken     string
	Why           string
	DefaultsNote  string
}

const expiresLayout = "2006-01-02T15:04"

func shortETag(e string) string {
	e = strings.Trim(e, `"`)
	if len(e) > 12 {
		e = e[:12]
	}
	return e
}

// tokenOverridePrefix starts the reason of an override written at enrollment from
// the token's policy (store/enroll_policy_m2.go).
const tokenOverridePrefix = "Enrollment token policy:"

// overrideFromToken says the override is the enrollment token's policy: no admin
// set it and its reason says where it came from.
func overrideFromToken(o *store.DeviceOverride) bool {
	return o != nil && o.SetBy == "" && strings.HasPrefix(o.Reason, tokenOverridePrefix)
}

func ovInfoOf(o *store.DeviceOverride, c fmtCtx) *OvInfo {
	if o == nil {
		return nil
	}
	info := &OvInfo{Reason: o.Reason, By: o.SetByName, When: c.dayClock(o.UpdatedAt), ISO: o.UpdatedAt.UTC().Format(time.RFC3339), Expired: o.Expired}
	if info.By == "" {
		// No admin name: the enrollment token's policy (M2-W3B) has none by design;
		// any other row without one lost its author.
		info.By = "a removed account"
		if overrideFromToken(o) {
			info.By = "the enrollment token"
		}
	}
	if o.ExpiresAt != nil {
		info.Expires = c.dayClock(*o.ExpiresAt)
	}
	for _, path := range store.OverriddenPaths(o.Patch) {
		info.Fields = append(info.Fields, labelOf(path))
	}
	return info
}

// ovUnits maps the leaf paths of an override patch to the catalogue units that
// are therefore switched on.
func ovUnits(o *store.DeviceOverride) map[string]bool {
	on := map[string]bool{}
	if o == nil {
		return on
	}
	for _, path := range store.OverriddenPaths(o.Patch) {
		for _, s := range policySections {
			for _, f := range s.Fields {
				if path == f.Path || strings.HasPrefix(path, f.Path+".") {
					on[f.Path] = true
				}
			}
		}
	}
	return on
}

type ovFormState struct {
	vals   url.Values
	on     map[string]bool
	errs   map[string]string
	reason string
	expiry string
	extra  []PolErr
}

func (s *Service) devPolicyView(p *auth.Principal, dev store.DeviceEffective, c fmtCtx, st *ovFormState) *DevPolicyView {
	v := &DevPolicyView{CSRFToken: p.CSRF, ETag: shortETag(dev.ETag), CanEdit: p.Role >= auth.RoleAdmin}
	v.PolicyText = dev.PolicyName
	v.PolicyHref = "/customers/" + dev.CustomerID + "/policy"
	v.PolicyMeta = fmt.Sprintf("%s · version %d", scopeWord(dev.PolicyScope), dev.Version)
	v.PolicyScope, v.PolicyVersion = scopeWord(dev.PolicyScope), dev.Version
	v.Override = ovInfoOf(dev.Override, c)
	v.Groups = effGroups(dev.Fields, dev.PolicyName)
	if overrideFromToken(dev.Override) {
		for gi := range v.Groups {
			for ri := range v.Groups[gi].Rows {
				if v.Groups[gi].Rows[ri].Source == store.SourceOverride {
					v.Groups[gi].Rows[ri].SourceTxt = "this device's override (from the enrollment token)"
				}
			}
		}
	}
	for _, g := range v.Groups {
		v.Set += g.Set
	}
	v.ClearAction = "/ui/devices/" + dev.DeviceID + "/override/clear"
	if !v.CanEdit {
		v.Why = "Setting or clearing an override needs the admin role."
		return v
	}
	if st == nil {
		tree, err := treeOf(dev.Policy)
		if err == nil {
			st = &ovFormState{vals: formFromDoc(tree), on: ovUnits(dev.Override), errs: map[string]string{}}
		} else {
			st = &ovFormState{vals: url.Values{}, on: map[string]bool{}, errs: map[string]string{}}
		}
		if dev.Override != nil {
			st.reason = dev.Override.Reason
			if dev.Override.ExpiresAt != nil {
				st.expiry = dev.Override.ExpiresAt.In(c.loc()).Format(expiresLayout)
			}
		}
	}
	src := map[string]string{}
	for _, f := range dev.Fields {
		src[f.Path] = f.Source
	}
	ov := &ovCtx{On: st.on, Src: src, PolicyText: dev.PolicyName}
	form := &OvForm{Action: "/ui/devices/" + dev.DeviceID + "/override", Reason: st.reason, Expires: st.expiry, Zone: c.Zone,
		ReasonErr: st.errs["reason"], ExpiresErr: st.errs["expires_at"], PatchErr: st.errs["patch"]}
	form.Sections = buildSections(st.vals, st.errs, ov)
	form.Summary = append(polSummary(st.errs, nil), st.extra...)
	form.Open = len(form.Summary) > 0 || dev.Override != nil
	v.Form = form
	return v
}

// devicePolicyPage renders the Policy tab; mut adjusts the form state (errors).
func (s *Service) devicePolicyPage(w http.ResponseWriter, r *http.Request, p *auth.Principal, fleet health.Fleet, id string, status int, st *ovFormState, saved string) {
	v, ok := s.deviceView(r.Context(), p, fleet, id)
	if !ok {
		s.notFound(w, r, p, "This device")
		return
	}
	eff, err := s.Store.GetDeviceEffective(r.Context(), p.Scope, id)
	if err != nil {
		s.policyTabFailed(w, r, p, err)
		return
	}
	c := s.fcAt(r.Context(), fleet.Computed)
	pg := DevPanelPage{Page: s.page(r.Context(), p, fleet, "Device detail", "devices", "/ui/events"), Dev: v, Tab: "policy", Tabs: s.panelTabs(id, "policy", p, v.CanRestore), Saved: saved}
	pg.Policy = s.devPolicyView(p, eff, c, st)
	s.render(w, r, status, "device", "base", pg)
}

func (s *Service) devicePolicy(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	fleet, ok := s.fleetFor(w, r, p, "Device")
	if !ok {
		return
	}
	s.devicePolicyPage(w, r, p, fleet, r.PathValue("id"), http.StatusOK, nil, polSavedText(r.URL.Query().Get("saved")))
}

// deviceOverrideSet is POST /ui/devices/{id}/override (admin): the checked
// settings become the override (a sparse patch), with a required reason.
func (s *Service) deviceOverrideSet(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	r.Body = http.MaxBytesReader(w, r.Body, 128<<10)
	if err := r.ParseForm(); err != nil {
		s.errorPage(w, r, p, http.StatusBadRequest, "Form not read", "The form could not be read.")
		return
	}
	id := r.PathValue("id")
	// A device outside scope is a 404 before anything about the form is judged.
	if _, err := s.Store.GetDevice(r.Context(), p.Scope, id); err != nil {
		s.storeFailed(w, r, p, err)
		return
	}
	fleet, ok := s.fleetFor(w, r, p, "Device")
	if !ok {
		return
	}
	vals := r.PostForm
	st := &ovFormState{vals: vals, on: map[string]bool{}, errs: map[string]string{}, reason: strings.TrimSpace(vals.Get("reason")), expiry: strings.TrimSpace(vals.Get("expires_at"))}
	patch := map[string]any{}
	for _, sec := range policySections {
		for _, f := range sec.Fields {
			if vals.Get("o."+f.Path) == "" {
				continue
			}
			st.on[f.Path] = true
			val, msg := parseField(f, vals, true)
			if msg != "" {
				st.errs[f.Path] = msg
				continue
			}
			setPath(patch, f.Path, val)
		}
	}
	if len(st.on) == 0 {
		st.errs["patch"] = "Choose at least one setting to override, or clear the override."
	}
	if st.reason == "" {
		st.errs["reason"] = "Say why this device differs from its policy."
	}
	var expires *time.Time
	if st.expiry != "" {
		t, err := time.ParseInLocation(expiresLayout, st.expiry, s.loc(r.Context()))
		if err != nil {
			st.errs["expires_at"] = "Use a date and time, for example 2026-12-01 5:00 PM, or leave it empty."
		} else {
			u := t.UTC()
			expires = &u
		}
	}
	if len(st.errs) == 0 {
		raw, err := json.Marshal(patch)
		if err == nil {
			_, changed, serr := s.Store.SetDeviceOverride(r.Context(), p.Scope, id, store.OverrideInput{Patch: raw, Reason: st.reason, ExpiresAt: expires}, s.actor(r, p))
			if serr == nil {
				if changed {
					s.changed()
				}
				http.Redirect(w, r, "/devices/"+id+"/policy?saved=override", http.StatusSeeOther)
				return
			}
			if !absorbStoreErrs(st.errs, serr) {
				s.storeFailed(w, r, p, serr)
				return
			}
		} else {
			st.errs["patch"] = "The override could not be read."
		}
	}
	// keep the values the admin typed; the unchecked rows show what was posted
	s.devicePolicyPage(w, r, p, fleet, id, http.StatusBadRequest, st, "")
}

// deviceOverrideClear is POST /ui/devices/{id}/override/clear (admin).
func (s *Service) deviceOverrideClear(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	id := r.PathValue("id")
	changed, err := s.Store.ClearDeviceOverride(r.Context(), p.Scope, id, s.actor(r, p))
	if err != nil {
		s.storeFailed(w, r, p, err)
		return
	}
	if changed {
		s.changed()
	}
	http.Redirect(w, r, "/devices/"+id+"/policy?saved=cleared", http.StatusSeeOther)
}
