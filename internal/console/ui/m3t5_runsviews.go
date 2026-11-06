package ui

import (
	"encoding/json"
	"net/http"
	"net/url"
	"sort"

	"github.com/c2tech-sys/cryobackup/internal/console/auth"
)

// M3-T5: saved views on the run search (S21). The search keeps its whole state in
// the query string (m3t4_runs.go), so a view is a link: /runs?view={id} redirects
// to the equivalent search URL, and the control marks the saved view whose
// conditions equal the current ones.

func sortedQuery(v url.Values) string {
	keys := make([]string, 0, len(v))
	for k := range v {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := url.Values{}
	for _, k := range keys {
		vals := append([]string(nil), v[k]...)
		sort.Strings(vals)
		out[k] = vals
	}
	return out.Encode()
}

// runsConditions is the part of a run search a view holds: its conditions, without paging or zone.
func runsConditions(q runQuery) url.Values {
	v := q.values()
	v.Del("page")
	v.Del("size")
	v.Del("tz")
	return v
}

// runsViewRedirect answers /runs?view={id}: it redirects to the search the view holds. A view it
// cannot show (an or/not group, a clause the builder has no row for) is not applied, and the
// page says so instead of listing a different search.
func (s *Service) runsViewRedirect(w http.ResponseWriter, r *http.Request, p *auth.Principal) (done bool, warn *CauseView) {
	id := r.URL.Query().Get("view")
	if id == "" {
		return false, nil
	}
	if !idParam(id) {
		return false, &CauseView{Class: ClassWarn, Title: "View not found.", Text: "The link names no saved view. Every run is listed."}
	}
	v, err := s.Store.GetSavedView(r.Context(), p.Scope, p.Admin.ID, id)
	if err != nil || v.Entity != "runs" {
		return false, &CauseView{Class: ClassWarn, Title: "View not found.", Text: "The view in this link was deleted, or it is not shared with you. Every run is listed."}
	}
	vals, ok := runsQueryOf(v.Filter)
	if !ok {
		return false, &CauseView{Class: ClassCrit, Title: "This view cannot be applied.", Text: "It uses an or / not group, or a condition the run search has no row for, so every run is listed. Open it through the API."}
	}
	http.Redirect(w, r, listURL("/runs", vals), http.StatusSeeOther)
	return true, nil
}

// runsViews is the saved-views control of the run search for the search q.
func (s *Service) runsViews(r *http.Request, p *auth.Principal, q runQuery, warn *CauseView) *ViewsControl {
	cond := runsConditions(q)
	cur := sortedQuery(cond)
	vs := viewState{Entity: "runs", Return: "/runs", SaveOK: true,
		HrefOf: func(id string) string { return "/runs?view=" + url.QueryEscape(id) }}
	for _, c := range q.Clauses {
		if c.Value != "" {
			vs.Fields = append(vs.Fields, SaveField{c.Field, c.Value})
		}
	}
	switch {
	case len(q.Clauses) == 0:
		vs.Builtin, vs.Label = "all", "All runs"
	case sortedQuery(url.Values{"status": {"failed", "partial", "interrupted", "abandoned"}}) == cur:
		vs.Builtin, vs.Label = "unsuccessful", "Unsuccessful runs"
	default:
		vs.Label = "Custom conditions"
	}
	vs.Match = func(raw json.RawMessage) bool {
		v, ok := runsQueryOf(raw)
		return ok && len(v) > 0 && sortedQuery(v) == cur
	}
	c := s.viewsControl(r, p, vs)
	if warn != nil {
		c.Flash = warn
	}
	return c
}
