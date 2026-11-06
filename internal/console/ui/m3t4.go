package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/console/adminapi"
	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/httpx"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
)

// M3-T4: the seven M3 pages. Runs search (S21), run detail (S22), the customer
// Runs tab (S16), the activity grid (S23), overdue (B04), unseen (B05) and
// system health (S36).
//
// One rule shapes all of them: no page computes a fact of its own. Every number,
// state and verdict on these pages is read from the admin API's handlers
// (adminapi/m3*.go), which read the state engine's result and the M3 store
// queries. The pages, the board and GET /api/v1/* therefore cannot disagree,
// and an answer the API refuses (a customer outside the account's scope, a
// failed read) is refused here the same way.
//
// How the pages reach those handlers: pageAPI runs them in process, on a
// private ServeMux that holds a second adminapi.Service built from this
// service's own store, evaluator, auth manager and clock. It is never mounted
// on a listener. A page forwards the operator's own session cookie, so the
// handlers authenticate, scope and role-check the call exactly as they would a
// browser's request to /api/v1; the page adds no privilege. Nothing here reads a
// store query of its own except the select lists of the filter forms.

// jobRunner is the console's job runner as the System page needs it
// (jobs.Runner satisfies it, and so adminapi.JobRunner). cli sets
// Service.Jobs; nil leaves the job table unregistered and "Run now" disabled.
type jobRunner interface {
	Names() []string
	RunOnce(ctx context.Context, name string) (ran bool, err error)
}

// m3State is the lazily built private admin API of the M3-T4 pages.
type m3State struct {
	once sync.Once
	api  *adminapi.Service
	mux  *http.ServeMux
}

// muxRegistrar adapts a ServeMux to httpx.Registrar.
type muxRegistrar struct{ m *http.ServeMux }

func (r muxRegistrar) Handle(pattern string, h http.Handler) { r.m.Handle(pattern, h) }

// pageAPI returns the private admin API. It is built on first use, so Service.Jobs
// must be set before the first request (cli sets it right after the runner exists).
func (s *Service) pageAPI() http.Handler {
	s.m3.once.Do(func() {
		var jobs adminapi.JobRunner
		if s.Jobs != nil {
			jobs = s.Jobs
		}
		a := &adminapi.Service{Auth: s.Auth, Store: s.Store, Fleet: s.Fleet, Restores: s.Restores, Backend: s.Backend, Clock: s.Clock, Log: s.Log,
			Loc: s.Loc, Display: s.Display, Jobs: jobs}
		mux := http.NewServeMux()
		a.Register(muxRegistrar{mux})
		s.m3.api, s.m3.mux = a, mux
	})
	return s.m3.mux
}

// BeginShutdown refuses new manual job runs started from the System page and
// cancels the ones in flight. serve calls it with the admin API's own
// BeginShutdown, before it stops the job runner.
func (s *Service) BeginShutdown() {
	if s.m3.api != nil {
		s.m3.api.BeginShutdown()
	}
}

// WaitBackground waits at most d for manual job runs started from the System
// page and reports whether they all ended (see adminapi.Service.WaitBackground).
func (s *Service) WaitBackground(d time.Duration) bool {
	if s.m3.api == nil {
		return true
	}
	return s.m3.api.WaitBackground(d)
}

// memResponse is the in-memory ResponseWriter of an in-process API call.
type memResponse struct {
	h      http.Header
	status int
	buf    bytes.Buffer
	over   bool
}

// maxAPIBody bounds one in-process answer: a runaway handler cannot exhaust memory.
const maxAPIBody = 16 << 20

func (m *memResponse) Header() http.Header { return m.h }

func (m *memResponse) WriteHeader(code int) {
	if m.status == 0 {
		m.status = code
	}
}

func (m *memResponse) Write(b []byte) (int, error) {
	if m.status == 0 {
		m.status = http.StatusOK
	}
	if m.buf.Len()+len(b) > maxAPIBody {
		m.over = true
		return len(b), nil
	}
	return m.buf.Write(b)
}

// apiError is a refusal or failure of an in-process API call. It carries the
// status and the api error code, never a body: the problem text is the API's
// own fixed sentence and the pages say their own.
type apiError struct {
	Status int
	Code   string
}

func (e *apiError) Error() string {
	if e.Status == 0 {
		return "admin API: unusable answer"
	}
	return "admin API answered " + strconv.Itoa(e.Status) + " " + e.Code
}

// apiStatus is the status of an apiError, or 0 for any other error.
func apiStatus(err error) int {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.Status
	}
	return 0
}

// apiCall serves one request on the private admin API as the caller of r. It
// forwards the session cookie, the client address and, for a write, the
// origin evidence and the CSRF token of the session, so the handler applies
// the same gate it applies to the browser. It returns the status and the body.
func (s *Service) apiCall(r *http.Request, method, target string) (status int, body []byte, err error) {
	defer func() {
		if rec := recover(); rec != nil { // a handler panic is an alarm on the page, never a crashed request
			s.log().Error("ui: in-process admin API call panicked", logx.Class("render"))
			status, body, err = 0, nil, &apiError{Code: "panic"}
		}
	}()
	req, rerr := http.NewRequestWithContext(r.Context(), method, target, nil)
	if rerr != nil {
		return 0, nil, &apiError{Code: "bad_request"}
	}
	req.RemoteAddr, req.Host = r.RemoteAddr, r.Host
	req.Header.Set("Accept", api.ContentTypeJSON)
	if c := r.Header.Get("Cookie"); c != "" {
		req.Header.Set("Cookie", c)
	}
	if method != http.MethodGet {
		for _, h := range []string{"Origin", "Referer", "Sec-Fetch-Site"} {
			if v := r.Header.Get(h); v != "" {
				req.Header.Set(h, v)
			}
		}
		if p := principal(r); p != nil {
			req.Header.Set(auth.CSRFHeader, p.CSRF)
		}
	}
	rec := &memResponse{h: http.Header{}}
	s.pageAPI().ServeHTTP(rec, req)
	if rec.over {
		return rec.status, nil, &apiError{Status: rec.status, Code: "too_large"}
	}
	return rec.status, rec.buf.Bytes(), nil
}

// apiGet reads one admin API route as the caller and decodes the answer into out.
func (s *Service) apiGet(r *http.Request, target string, out any) error {
	status, body, err := s.apiCall(r, http.MethodGet, target)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return &apiError{Status: status, Code: problemCode(body)}
	}
	if err := json.Unmarshal(body, out); err != nil {
		return &apiError{Code: "bad_body"}
	}
	return nil
}

// problemCode extracts the api error code of a problem body ("" when it is not one).
func problemCode(body []byte) string {
	var p struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(body, &p) != nil || len(p.Code) > 40 {
		return ""
	}
	return p.Code
}

// ---- shared view helpers ----

// safeWord passes a short identifier (an error class, a trigger, a version) and
// withholds anything else: a value with a slash, a colon, a backslash, a quote
// or a space-padded sentence could be a path or a message, and these pages
// carry classes and counts only. The agent API already validates what it
// stores; this is the page's own guard, so a run row that got in by another
// route still cannot put a file name on screen.
var safeWordRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.+-]{0,39}$`)

const withheld = "(withheld)"

func safeWord(s string) string {
	if safeWordRE.MatchString(s) {
		return s
	}
	return withheld
}

// words turns a snake_case token into words ("files_new" -> "files new").
func words(s string) string { return strings.ReplaceAll(s, "_", " ") }

// m3PageInfo is the page window of total rows at the 1-based page and size.
func m3PageInfo(total, page, size int) store.PageInfo {
	if size < 1 {
		size = store.DefaultPageSize
	}
	pages := (total + size - 1) / size
	if pages < 1 {
		pages = 1
	}
	info := store.PageInfo{Total: total, Size: size, Pages: pages, Page: page}
	if page < 1 {
		info.Page = 1
	}
	if info.Page > pages {
		info.Page, info.Clamped = pages, true
	}
	if total > 0 {
		info.From = (info.Page-1)*size + 1
		info.To = info.From + size - 1
		if info.To > total {
			info.To = total
		}
	}
	return info
}

// durText is a duration as "14 min", "2 min 4 s" or "38 s".
func durText(ms int64) string {
	d := time.Duration(ms) * time.Millisecond
	switch {
	case d < time.Second:
		return "under 1 s"
	case d < time.Minute:
		return strconv.Itoa(int(d/time.Second)) + " s"
	case d < time.Hour:
		if s := int(d % time.Minute / time.Second); s > 0 {
			return fmt.Sprintf("%d min %d s", int(d/time.Minute), s)
		}
		return strconv.Itoa(int(d/time.Minute)) + " min"
	}
	return fmt.Sprintf("%d h %d min", int(d/time.Hour), int(d%time.Hour/time.Minute))
}

// idParam is a customer or device id taken from a query string: short, no control characters.
func idParam(v string) bool {
	return v != "" && len(v) <= 40 && !strings.ContainsAny(v, "\r\n\x00/\\ ")
}

// filterCustomers lists the customers inside the principal's scope for a
// filter select, with "All customers" first. A failed read returns ok false:
// the form says the list could not be loaded instead of offering fewer choices.
func (s *Service) filterCustomers(ctx context.Context, p *auth.Principal, selected []string, all string) ([]Option, bool) {
	cs, err := s.Store.ListCustomers(ctx, p.Scope)
	if err != nil {
		return []Option{{Value: "", Label: all, Selected: len(selected) == 0}}, false
	}
	out := []Option{{Value: "", Label: all, Selected: len(selected) == 0}}
	for _, c := range cs {
		out = append(out, Option{Value: c.ID, Label: c.Name, Selected: containsStr(selected, c.ID)})
	}
	return out, true
}

func containsStr(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// apiFailure is the alarm panel of a read the admin API could not answer.
// Never an empty list: the page says it could not read, and why in one word.
func apiFailure(what string, err error) *EmptyView {
	body := "The console could not read " + what + ". This is an alarm, not an empty list. Reload the page to try again."
	switch apiStatus(err) {
	case http.StatusForbidden:
		body = "This account is not allowed to read " + what + "."
	case http.StatusServiceUnavailable:
		body = "The console is not able to answer for " + what + " right now (it is starting up or shutting down). This is an alarm, not an empty list. Reload the page to try again."
	}
	return &EmptyView{Kind: "alarm", Title: "Could not load " + what, Body: body}
}

// ---- routes ----

const (
	patRuns        = "GET /runs"
	patRunsTable   = "GET /ui/runs/table"
	patRun         = "GET /runs/{id}"
	patCustomerRun = "GET /customers/{id}/runs"
	patActivity    = "GET /activity"
	patOverdue     = "GET /fleet/overdue"
	patUnseen      = "GET /fleet/unseen"
	patSystem      = "GET /system"
	patSystemRun   = "POST /ui/system/jobs/{name}/run"
)

// m3t4Patterns lists the patterns registerM3T4 adds.
func m3t4Patterns() []string {
	return []string{patRuns, patRunsTable, patRun, patCustomerRun, patActivity, patOverdue, patUnseen, patSystem, patSystemRun}
}

// registerM3T4 adds the M3-T4 routes: reads need read_only; "Run now" admin
// (CSRF-checked by Require, then audited by the admin API's own handler).
func (s *Service) registerM3T4(r httpx.Registrar) {
	ro := func(h http.HandlerFunc) http.Handler { return s.Auth.Require(auth.RoleReadOnly, auth.ModeUI, h) }
	ad := func(h http.HandlerFunc) http.Handler { return s.Auth.Require(auth.RoleAdmin, auth.ModeUI, h) }
	r.Handle(patRuns, ro(s.runsPage))
	r.Handle(patRunsTable, ro(s.runsTablePartial))
	r.Handle(patRun, ro(s.runDetailPage))
	r.Handle(patCustomerRun, ro(s.customerRuns))
	r.Handle(patActivity, ro(s.activityPage))
	r.Handle(patOverdue, ro(s.overduePage))
	r.Handle(patUnseen, ro(s.unseenPage))
	r.Handle(patSystem, ro(s.systemPage))
	r.Handle(patSystemRun, ad(s.systemJobRun))
}

// queryValues copies the values of the named keys.
func queryValues(v url.Values, keys ...string) url.Values {
	out := url.Values{}
	for _, k := range keys {
		for _, x := range v[k] {
			out.Add(k, x)
		}
	}
	return out
}

// bannerLabelsM3 names the conditions the shared strip had no label for: C00
// (self-checks not wired) and the "b" not-checked warnings. Without a label the
// strip prints "CONDITION C01b", which says nothing to an operator at 3 a.m.
// They are added to bannerLabels only where it has no entry yet.
var bannerLabelsM3 = map[string]string{
	"C00": "SELF-CHECKS NOT WIRED", "C01b": "GATEWAY NOT CHECKED", "C02b": "STORAGE NOT CHECKED", "C03b": "EMAIL NOT CHECKED",
	"C05b": "CERTIFICATE NOT CHECKED", "C06b": "ALERTING NOT CHECKED", "C07b": "MAINTENANCE WORKER NOT CHECKED", "C12b": "MASTER KEY NOT CHECKED",
	"C14b": "DIGEST NOT CHECKED", "C15b": "STORAGE QUOTA NOT CHECKED", "C16b": "RELEASE MANIFEST NOT CHECKED",
}

func init() {
	for id, label := range bannerLabelsM3 {
		if _, ok := bannerLabels[id]; !ok {
			bannerLabels[id] = label
		}
	}
}
