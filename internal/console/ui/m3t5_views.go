package ui

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/console/adminapi"
	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// M3-T5: saved views on the fleet board (B01) and the run search (S21), components 5.3.
//
// Every read and write goes through the admin API's /views handlers (the same
// rule as the M3-T4 pages): a view's owner, its explicit audience (all customers,
// or a list of customers), "locked" (the owner lost one of the audience's
// customers: only make private and delete are offered) and who may edit are
// decided there, once. The page adds no privilege.
//
// A view is a clause tree plus columns and sort (store/viewfilter.go). The board
// saves its filters (a state, a ring segment) as clauses and applies a loaded
// view through the same tree (m3t5_filter.go). The run search saves its
// conditions the same way.

const (
	patViewCreate  = "POST /ui/views"
	patViewUpdate  = "POST /ui/views/{id}/update"
	patViewRename  = "POST /ui/views/{id}/rename"
	patViewPrivate = "POST /ui/views/{id}/private"
	patViewDelete  = "POST /ui/views/{id}/delete"
)

// ViewItem is one row of the saved-views popover.
type ViewItem struct {
	ID, Name, Href string
	Builtin        bool
	Mine           bool
	Shared         bool
	Locked         bool
	Current        bool
	Audience       string // "all customers", "2 customers"
	AudienceTip    string // the customers' names
	Owner          string
	CanManage      bool // the owner, or an admin
	CanEdit        bool // not locked: rename and update are offered
	Updated        string
}

// ViewRowCtx is what the view_row template renders: the row and the control's fields it needs.
type ViewRowCtx struct {
	ViewItem
	CSRF, Entity string
	Fields       []SaveField
	SaveOK       bool
}

func viewRow(v ViewItem, c *ViewsControl) ViewRowCtx {
	return ViewRowCtx{ViewItem: v, CSRF: c.CSRF, Entity: c.Entity, Fields: c.Fields, SaveOK: c.SaveOK}
}

// SaveField is a hidden input that carries the current filter into a form.
type SaveField struct{ Name, Value string }

// ViewsControl is the saved-views dropdown (components 5.3).
type ViewsControl struct {
	Entity    string // devices | runs
	CSRF      string
	Label     string // the trigger text
	Modified  bool   // the filters differ from the loaded view
	Err       string // the list could not be read
	Builtins  []ViewItem
	Mine      []ViewItem
	Shared    []ViewItem
	Flash     *CauseView
	CurrentID string
	// CurrentEditable: the loaded view is the viewer's own or the viewer is an admin, and it is not locked.
	CurrentEditable bool
	Fields          []SaveField // the current filter, for Save and Update
	Return          string      // where a failed write goes back to
	SaveOK          bool
	SaveWhy         string
	CanAll          bool // the account sees every customer: "all customers" may be chosen
	Customers       []Option
}

// viewMsgs are the only texts the vok and verr codes can show.
var viewMsgs = map[string]CauseView{
	"saved":     {Class: ClassOK, Title: "View saved.", Text: "It is in the list now."},
	"updated":   {Class: ClassOK, Title: "View updated.", Text: "It now holds the current filters."},
	"renamed":   {Class: ClassOK, Title: "View renamed."},
	"private":   {Class: ClassOK, Title: "View is private now.", Text: "Only you see it."},
	"deleted":   {Class: ClassOK, Title: "View deleted."},
	"e_name":    {Class: ClassCrit, Title: "View not saved:", Text: "use a name of 1 to 100 characters on one line, without a file path. Your filters are unchanged."},
	"e_dup":     {Class: ClassCrit, Title: "View not saved:", Text: "you already have a view with that name. Your filters are unchanged."},
	"e_aud":     {Class: ClassCrit, Title: "View not saved:", Text: "say who a shared view is for: all customers (only an account that sees every customer can), or customers you can see. Your filters are unchanged."},
	"e_filter":  {Class: ClassCrit, Title: "View not saved:", Text: "the filters name a customer or device outside the people it is shared with, or something a view cannot hold. Your filters are unchanged."},
	"e_denied":  {Class: ClassCrit, Title: "View not changed:", Text: "this view is not yours to change, or it is locked: you can only make it private or delete it."},
	"e_missing": {Class: ClassWarn, Title: "View not found.", Text: "It was deleted, or it is not shared with you."},
	"e_cust":    {Class: ClassWarn, Title: "View not saved:", Text: "the customer ring filter is not part of a saved view. Clear it, or save the other filters. Your filters are unchanged."},
	"e_fail":    {Class: ClassCrit, Title: "View not saved:", Text: "the console could not write it. Your filters are unchanged."},
}

// viewFlash is the message a vok or verr code in the query stands for.
func viewFlash(v url.Values) *CauseView {
	for _, k := range []string{"vok", "verr"} {
		if code := v.Get(k); code != "" {
			if m, ok := viewMsgs[code]; ok {
				return &m
			}
		}
	}
	return nil
}

// ---- reading ----

// listViews reads the viewer's saved views of one entity through the admin API.
func (s *Service) listViews(r *http.Request, entity string) ([]adminapi.SavedViewJSON, error) {
	var out adminapi.ViewListResponse
	if err := s.apiGet(r, "/api/v1/views?entity="+url.QueryEscape(entity), &out); err != nil {
		return nil, err
	}
	return out.Views, nil
}

func audienceText(a *adminapi.ViewAudienceJSON, names map[string]string) (text, tip string) {
	switch {
	case a == nil:
		return "", ""
	case a.AllCustomers:
		return "all customers", ""
	}
	var n []string
	for _, id := range a.CustomerIDs {
		if nm := names[id]; nm != "" {
			n = append(n, nm)
		}
	}
	return plural(len(a.CustomerIDs), "customer", "customers"), strings.Join(n, ", ")
}

// viewState is what the page knows about its current filter, for the control.
type viewState struct {
	Entity    string
	CurrentID string
	Modified  bool
	Label     string // the label when no saved view is loaded ("All devices", "Unsuccessful backups")
	Builtin   string // the id of the built-in view the filters equal, or ""
	Fields    []SaveField
	Return    string
	SaveOK    bool
	SaveWhy   string
	// HrefOf is the page link that loads the saved view v.
	HrefOf func(id string) string
	// Match, when set and no view is named in the link, marks the saved view whose filter equals the current one.
	Match func(filter json.RawMessage) bool
}

var unsuccessfulStates = func() []string {
	var out []string
	for _, st := range state.Precedence() {
		if c := ClassOf(st); c == ClassCrit || c == ClassWarn {
			out = append(out, string(st))
		}
	}
	return out
}()

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for _, x := range a {
		if !containsStr(b, x) {
			return false
		}
	}
	return true
}

// viewsControl builds the dropdown for one entity.
func (s *Service) viewsControl(r *http.Request, p *auth.Principal, vs viewState) *ViewsControl {
	c := &ViewsControl{Entity: vs.Entity, CSRF: p.CSRF, Label: vs.Label, Modified: vs.Modified, CurrentID: vs.CurrentID, Fields: vs.Fields,
		Return: vs.Return, SaveOK: vs.SaveOK, SaveWhy: vs.SaveWhy, CanAll: p.Scope.All, Flash: viewFlash(r.URL.Query())}
	c.Builtins = s.builtinViews(vs)
	names := map[string]string{}
	if cs, err := s.Store.ListCustomers(r.Context(), p.Scope); err == nil {
		for _, cu := range cs {
			names[cu.ID] = cu.Name
			c.Customers = append(c.Customers, Option{Value: cu.ID, Label: cu.Name})
		}
	}
	list, err := s.listViews(r, vs.Entity)
	if err != nil {
		s.log().Error("ui: saved views unreadable", logx.Class("render"), logx.Err(err))
		c.Err = "Saved views could not be loaded. The built-in views still work; your own are not shown."
		return c
	}
	canAdmin := p.Role >= auth.RoleAdmin
	matched := false
	for _, v := range list {
		cur := v.ID == vs.CurrentID
		if !cur && vs.CurrentID == "" && !matched && vs.Match != nil && vs.Match(v.Filter) {
			cur, matched = true, true
			c.CurrentID = v.ID
		}
		it := ViewItem{ID: v.ID, Name: v.Name, Href: vs.HrefOf(v.ID), Mine: v.Mine, Shared: v.Shared, Locked: v.Locked, Owner: v.Owner,
			Current: cur, CanManage: v.Mine || canAdmin}
		it.CanEdit = it.CanManage && !v.Locked
		it.Audience, it.AudienceTip = audienceText(v.Audience, names)
		it.Updated = v.UpdatedAt.In(s.loc(r.Context())).Format("2 Jan")
		if it.Current {
			c.CurrentEditable = it.CanEdit
			c.Label = v.Name
		}
		if v.Mine {
			c.Mine = append(c.Mine, it)
		} else {
			c.Shared = append(c.Shared, it)
		}
	}
	if vs.CurrentID != "" && !c.hasCurrent() {
		c.Flash = &CauseView{Class: ClassWarn, Title: "View not found.", Text: "The view in this link was deleted, or it is not shared with you. The filters shown are the ones in the link."}
	}
	if c.Modified && c.Label != "" {
		c.Label += " (modified)"
	}
	return c
}

func (c *ViewsControl) hasCurrent() bool {
	for _, l := range [][]ViewItem{c.Mine, c.Shared} {
		for _, v := range l {
			if v.Current {
				return true
			}
		}
	}
	return false
}

// builtinViews are the locked views every account has.
func (s *Service) builtinViews(vs viewState) []ViewItem {
	var out []ViewItem
	add := func(id, name, href string) {
		out = append(out, ViewItem{ID: id, Name: name, Href: href, Builtin: true, Current: vs.CurrentID == "" && vs.Builtin == id})
	}
	if vs.Entity == "runs" {
		add("all", "All runs", "/runs")
		v := url.Values{}
		for _, o := range []string{"failed", "partial", "interrupted", "abandoned"} {
			v.Add("status", o)
		}
		add("unsuccessful", "Unsuccessful runs", "/runs?"+v.Encode())
		return out
	}
	add("all", "All devices", "/")
	add("unsuccessful", "Unsuccessful backups", href(boardQuery{States: unsuccessfulStates}))
	add("overdue", "Overdue", "/fleet/overdue")
	add("unseen", "Unseen", "/fleet/unseen")
	add("parked", "Parked", href(boardQuery{States: []string{string(state.StatePaused)}}))
	return out
}

// ---- writing ----

// apiSend serves one write on the private admin API as the caller of r, with a
// JSON body: the same forwarding rule as apiCall (session cookie, origin
// evidence and the session's CSRF token), so the handler gates it like a browser's call.
func (s *Service) apiSend(r *http.Request, method, target string, body any) (status int, resp []byte, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			s.log().Error("ui: in-process admin API write panicked", logx.Class("render"))
			status, resp, err = 0, nil, &apiError{Code: "panic"}
		}
	}()
	var rd *bytes.Reader
	if body != nil {
		b, merr := json.Marshal(body)
		if merr != nil {
			return 0, nil, &apiError{Code: "bad_request"}
		}
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, rerr := http.NewRequestWithContext(r.Context(), method, target, rd)
	if rerr != nil {
		return 0, nil, &apiError{Code: "bad_request"}
	}
	req.RemoteAddr, req.Host = r.RemoteAddr, r.Host
	req.Header.Set("Accept", api.ContentTypeJSON)
	req.Header.Set("Content-Type", api.ContentTypeJSON)
	if c := r.Header.Get("Cookie"); c != "" {
		req.Header.Set("Cookie", c)
	}
	for _, h := range []string{"Origin", "Referer", "Sec-Fetch-Site"} {
		if v := r.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	if p := principal(r); p != nil {
		req.Header.Set(auth.CSRFHeader, p.CSRF)
	}
	rec := &memResponse{h: http.Header{}}
	s.pageAPI().ServeHTTP(rec, req)
	if rec.over {
		return rec.status, nil, &apiError{Status: rec.status, Code: "too_large"}
	}
	return rec.status, rec.buf.Bytes(), nil
}

// viewErrCode maps an admin API refusal to the code of one fixed sentence.
func viewErrCode(status int, body []byte) string {
	switch status {
	case http.StatusNotFound:
		return "e_missing"
	case http.StatusForbidden:
		return "e_denied"
	case http.StatusConflict:
		return "e_dup"
	case http.StatusBadRequest:
		var p struct {
			Errors []struct {
				Field string `json:"field"`
			} `json:"errors"`
		}
		_ = json.Unmarshal(body, &p)
		for _, f := range p.Errors {
			switch f.Field {
			case "name":
				return "e_name"
			case "audience":
				return "e_aud"
			case "filter":
				return "e_filter"
			}
		}
		return "e_filter"
	}
	return "e_fail"
}

// viewWriteTarget is where a write returns to: the page it was made on, with the current filters.
func viewWriteTarget(entity string, form url.Values, extra url.Values) string {
	var base string
	var v url.Values
	switch entity {
	case "runs":
		q, _ := parseRunQuery(form)
		base, v = "/runs", q.values()
	default:
		base, v = "/", parseBoardQuery(form).values()
	}
	for k, vals := range extra {
		v[k] = vals
	}
	return listURL(base, v)
}

func (s *Service) viewForm(w http.ResponseWriter, r *http.Request) (url.Values, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "The form could not be read.", http.StatusBadRequest)
		return nil, false
	}
	return r.PostForm, true
}

func entityOf(form url.Values) string {
	if form.Get("entity") == "runs" {
		return "runs"
	}
	return "devices"
}

// currentFilter is the clause tree of the filters a form carries, with the view they started from.
func (s *Service) currentFilter(r *http.Request, p *auth.Principal, entity string, form url.Values) (json.RawMessage, string) {
	if entity == "runs" {
		q, _ := parseRunQuery(form)
		return runsFilterJSON(q), ""
	}
	q := parseBoardQuery(form)
	if q.Cust != "" {
		return nil, "e_cust"
	}
	var base json.RawMessage
	if q.View != "" {
		if v, err := s.Store.GetSavedView(r.Context(), p.Scope, p.Admin.ID, q.View); err == nil {
			base = v.Filter
		}
	}
	return q.filterJSON(base, state.DefaultThresholds().StaleAfter), ""
}

func (s *Service) viewCreate(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	form, ok := s.viewForm(w, r)
	if !ok {
		return
	}
	entity := entityOf(form)
	filter, code := s.currentFilter(r, p, entity, form)
	if code != "" {
		http.Redirect(w, r, viewWriteTarget(entity, form, url.Values{"verr": {code}}), http.StatusSeeOther)
		return
	}
	req := adminapi.CreateViewRequest{Name: strings.TrimSpace(form.Get("name")), Entity: entity, Filter: filter}
	if form.Get("shared") == "1" {
		req.Shared = true
		switch form.Get("audience") {
		case "all":
			req.Audience = &adminapi.ViewAudienceJSON{AllCustomers: true}
		default:
			if ids := form["customer_id"]; len(ids) > 0 {
				req.Audience = &adminapi.ViewAudienceJSON{CustomerIDs: ids}
			}
		}
		if req.Audience == nil {
			http.Redirect(w, r, viewWriteTarget(entity, form, url.Values{"verr": {"e_aud"}}), http.StatusSeeOther)
			return
		}
	}
	status, body, err := s.apiSend(r, http.MethodPost, "/api/v1/views", req)
	if s.writeFailed(w, r, p, status, err) {
		return
	}
	if status != http.StatusCreated {
		code := viewErrCode(status, body)
		s.log().Info("ui: saved view refused", logx.Str("entity", entity), logx.Int("status", status))
		http.Redirect(w, r, viewWriteTarget(entity, form, url.Values{"verr": {code}}), http.StatusSeeOther)
		return
	}
	var created adminapi.ViewResponse
	_ = json.Unmarshal(body, &created)
	extra := url.Values{"vok": {"saved"}}
	if entity == "devices" && idParam(created.ID) {
		// Land on the new view: its id replaces the filters it now holds.
		http.Redirect(w, r, listURL("/", url.Values{"view": {created.ID}, "vok": {"saved"}}), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, viewWriteTarget(entity, form, extra), http.StatusSeeOther)
}

// viewPatch is the shared tail of update, rename and private.
func (s *Service) viewPatch(w http.ResponseWriter, r *http.Request, okCode string, build func(form url.Values, entity string) (adminapi.PatchViewRequest, string)) {
	p := principal(r)
	form, ok := s.viewForm(w, r)
	if !ok {
		return
	}
	entity := entityOf(form)
	id := r.PathValue("id")
	if !idParam(id) {
		http.NotFound(w, r)
		return
	}
	req, code := build(form, entity)
	if code == "" && (req.Filter != nil) {
		f, c := s.currentFilter(r, p, entity, form)
		if c != "" {
			code = c
		} else {
			raw := json.RawMessage(f)
			req.Filter = &raw
		}
	}
	if code != "" {
		http.Redirect(w, r, viewWriteTarget(entity, form, url.Values{"verr": {code}}), http.StatusSeeOther)
		return
	}
	status, body, err := s.apiSend(r, http.MethodPatch, "/api/v1/views/"+id, req)
	if s.writeFailed(w, r, p, status, err) {
		return
	}
	if status != http.StatusOK {
		http.Redirect(w, r, viewWriteTarget(entity, form, url.Values{"verr": {viewErrCode(status, body)}}), http.StatusSeeOther)
		return
	}
	extra := url.Values{"vok": {okCode}}
	if entity == "devices" {
		extra.Set("view", id)
		http.Redirect(w, r, listURL("/", extra), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, viewWriteTarget(entity, form, extra), http.StatusSeeOther)
}

func (s *Service) viewUpdate(w http.ResponseWriter, r *http.Request) {
	s.viewPatch(w, r, "updated", func(form url.Values, entity string) (adminapi.PatchViewRequest, string) {
		raw := json.RawMessage(`{}`)
		return adminapi.PatchViewRequest{Filter: &raw}, "" // replaced with the form's filters by viewPatch
	})
}

func (s *Service) viewRename(w http.ResponseWriter, r *http.Request) {
	s.viewPatch(w, r, "renamed", func(form url.Values, entity string) (adminapi.PatchViewRequest, string) {
		name := strings.TrimSpace(form.Get("name"))
		return adminapi.PatchViewRequest{Name: &name}, ""
	})
}

func (s *Service) viewPrivate(w http.ResponseWriter, r *http.Request) {
	s.viewPatch(w, r, "private", func(form url.Values, entity string) (adminapi.PatchViewRequest, string) {
		off := false
		return adminapi.PatchViewRequest{Shared: &off}, ""
	})
}

func (s *Service) viewDelete(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	form, ok := s.viewForm(w, r)
	if !ok {
		return
	}
	entity := entityOf(form)
	id := r.PathValue("id")
	if !idParam(id) {
		http.NotFound(w, r)
		return
	}
	status, body, err := s.apiSend(r, http.MethodDelete, "/api/v1/views/"+id, nil)
	if s.writeFailed(w, r, p, status, err) {
		return
	}
	if status != http.StatusNoContent {
		http.Redirect(w, r, viewWriteTarget(entity, form, url.Values{"verr": {viewErrCode(status, body)}}), http.StatusSeeOther)
		return
	}
	// The deleted view is not loaded any more: drop its id from the return link.
	f := url.Values{}
	for k, v := range form {
		if k != "view" {
			f[k] = v
		}
	}
	http.Redirect(w, r, viewWriteTarget(entity, f, url.Values{"vok": {"deleted"}}), http.StatusSeeOther)
}

// ---- the board's current state, for the control ----

// boardViewState is the control's input for the fleet board.
func (s *Service) boardViewState(q boardQuery, loaded *store.SavedView) viewState {
	vs := viewState{Entity: "devices", CurrentID: q.View, Return: "/", SaveOK: q.Cust == "",
		HrefOf: func(id string) string { return href(boardQuery{View: id}) }}
	if q.Cust != "" {
		vs.SaveWhy = "The customer ring filter is not part of a saved view."
	}
	for _, st := range q.States {
		vs.Fields = append(vs.Fields, SaveField{"state", st})
	}
	if q.Age != "" {
		vs.Fields = append(vs.Fields, SaveField{"age", q.Age})
	}
	if q.Cust != "" { // not part of a saved view, but a failed Manage action must come back to the same board
		vs.Fields = append(vs.Fields, SaveField{"cust", q.Cust})
	}
	if q.View != "" {
		vs.Fields = append(vs.Fields, SaveField{"view", q.View})
	}
	own := boardQuery{States: q.States, Age: q.Age, Cust: q.Cust}
	switch {
	case q.View != "" && loaded != nil:
		vs.Modified = !own.empty()
	case q.View != "":
		// the view could not be loaded: the control says so; nothing named is current
	case own.empty():
		vs.Builtin, vs.Label = "all", "All devices"
	case own.Age == "" && own.Cust == "" && sameSet(own.States, unsuccessfulStates):
		vs.Builtin, vs.Label = "unsuccessful", "Unsuccessful backups"
	case own.Age == "" && own.Cust == "" && len(own.States) == 1 && own.States[0] == string(state.StatePaused):
		vs.Builtin, vs.Label = "parked", "Parked"
	default:
		vs.Label = "Custom filters"
	}
	return vs
}

// writeFailed answers a write the admin API could not carry out (it failed, or it could not
// write its audit row): an error page, never a redirect that reads as success. It reports
// whether it answered. A refusal the API explains (400, 403, 404, 409) is not this: the
// caller maps it to a message.
func (s *Service) writeFailed(w http.ResponseWriter, r *http.Request, p *auth.Principal, status int, err error) bool {
	if err == nil && status > 0 && status < 500 {
		return false
	}
	s.log().Error("ui: write refused by the admin API", logx.Class("render"), logx.Int("status", status))
	s.errorPage(w, r, p, http.StatusInternalServerError, "Not saved", "The change could not be saved, so nothing was changed. Try again; if it persists, check the console log and the database.")
	return true
}
