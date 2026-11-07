package ui

import (
	"bytes"
	"context"
	"crypto/rand"
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
	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
	"github.com/c2tech-sys/cryobackup/internal/console/health"
	"github.com/c2tech-sys/cryobackup/internal/console/httpx"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
	"github.com/c2tech-sys/cryobackup/internal/restic"
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
// (jobs.Runner satisfies it, and so adminapi.JobRunner). cli sets Service.Jobs.
// In fixtures mode, where cli registers no job, the page uses the fictional jobs
// of fixtures.NewJobRunner, so "Run now" can be pressed with no real runner.
type jobRunner interface {
	Names() []string
	RunOnce(ctx context.Context, name string) (ran bool, err error)
}

// m3State is the private admin API of the M3-T4 pages, built when the service is.
type m3State struct {
	api *adminapi.Service
	mux http.Handler
	// fixJobs is the fixtures-mode runner (jobsOf), built on first use.
	fixOnce sync.Once
	fixJobs jobRunner
	// flashKey signs the "Run now" answer carried in the redirect (flash.go); random per process.
	flashKey [32]byte
}

// muxRegistrar adapts a ServeMux to httpx.Registrar.
type muxRegistrar struct{ m *http.ServeMux }

func (r muxRegistrar) Handle(pattern string, h http.Handler) { r.m.Handle(pattern, h) }

// jobsOf is the runner the System page uses now: the one cli set, or, in
// fixtures mode when that one has no job, the fictional one; nil when there is none.
func (s *Service) jobsOf() jobRunner {
	if s.Jobs != nil && (!s.Fixtures || len(s.Jobs.Names()) > 0) {
		return s.Jobs
	}
	if !s.Fixtures || s.Store == nil {
		return nil
	}
	s.m3.fixOnce.Do(func() { s.m3.fixJobs = newFixtureJobs(s.Store, s.Log) })
	return s.m3.fixJobs
}

// jobProxy is the admin API's JobRunner: it asks the service for its runner at
// each call, so the runner cli sets after the service is built is the one used.
type jobProxy struct{ s *Service }

func (p jobProxy) Names() []string {
	if j := p.s.jobsOf(); j != nil {
		return j.Names()
	}
	return nil
}

func (p jobProxy) RunOnce(ctx context.Context, name string) (bool, error) {
	if j := p.s.jobsOf(); j != nil {
		return j.RunOnce(ctx, name)
	}
	return false, errors.New("ui: no job runner")
}

// initM3 builds the private admin API (once, from New: nothing is built on a
// request, so BeginShutdown works from the first moment and no field is read
// racily) and, in fixtures mode, wires the "show every banner" switch into the
// evaluator unless something already supplies extra banners.
func (s *Service) initM3() {
	a := &adminapi.Service{Auth: s.Auth, Store: s.Store, Fleet: s.Fleet, Restores: s.Restores, Backend: s.Backend, Clock: s.Clock, Log: s.Log,
		Loc: s.Loc, Display: s.Display, Jobs: jobProxy{s}, SyncGateway: s.SyncGateway, WakeAgent: s.WakeAgent}
	mux := http.NewServeMux()
	a.Register(muxRegistrar{mux})
	s.m3.api, s.m3.mux = a, mux
	_, _ = rand.Read(s.m3.flashKey[:])
	if s.Fixtures && s.Fleet != nil && s.Fleet.Extra == nil {
		s.Fleet.Extra = fixtures.ExtraBanners
	}
}

// pageAPI returns the private admin API.
func (s *Service) pageAPI() http.Handler { return s.m3.mux }

// PageJobs is the job runner the pages use, for the admin API that serve mounts
// beside them. It matters in fixtures mode, where the App's own runner has no job:
// with it, GET /api/v1/system/jobs lists as registered the jobs the page offers to run.
func (s *Service) PageJobs() adminapi.JobRunner { return jobProxy{s} }

// BeginShutdown refuses new manual job runs started from the System page and
// cancels the ones in flight. serve calls it with the admin API's own
// BeginShutdown, before it stops the job runner.
func (s *Service) BeginShutdown() { s.m3.api.BeginShutdown() }

// WaitBackground waits at most d for manual job runs started from the System
// page and reports whether they all ended (see adminapi.Service.WaitBackground).
func (s *Service) WaitBackground(d time.Duration) bool { return s.m3.api.WaitBackground(d) }

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

// What the run and system pages print about a run or a device comes from the
// agent and is allow-listed here, whatever the intake accepted: a value that is
// not in its known set or shape is shown as "(withheld)", never as it came.
//
//   - error classes: the restic class list (restic.AllClasses), nothing else;
//   - deferral reasons, kinds, triggers and origins: their fixed sets;
//   - agent and engine versions: api.VersionShape (a dotted number, a "dev"
//     stamp or a git describe), so a name with an extension or a sentence never
//     passes, though a bare word like "payroll" still can;
//   - the OS: "Windows ..." as the fixtures write it, or the dotted build
//     number the agent sends.
//
// A bare word that is none of these (a name without an extension, say) is also
// withheld wherever a set applies; only version text allows an arbitrary word,
// because the build stamp can be one ("dev", a commit hash), so that rule is the
// one place a file name without its extension can still be shown.

const withheld = "(withheld)"

func inSet(s string, set ...string) bool {
	for _, x := range set {
		if s == x {
			return true
		}
	}
	return false
}

func enumOrWithheld(s string, set ...string) string {
	if inSet(s, set...) {
		return s
	}
	return withheld
}

// safeClass is an error class of a run: only the restic vocabulary.
func safeClass(s string) string {
	if restic.ErrorClass(s).Valid() {
		return s
	}
	return withheld
}

// safeDeferred is a deferral reason: only the intake's set.
func safeDeferred(s string) string {
	return enumOrWithheld(s, string(restic.DeferredLock), string(restic.DeferredGateway), string(restic.DeferredBattery), string(restic.DeferredMetered),
		string(restic.DeferredWindow), string(restic.DeferredConditions), string(restic.DeferredFleetCap))
}

func safeKind(s string) string { return enumOrWithheld(s, store.RunKinds...) }
func safeTrigger(s string) string {
	return enumOrWithheld(s, "schedule", "catchup", "wake", "boot", "manual", "retry")
}
func safeOrigin(s string) string { return enumOrWithheld(s, "agent", "console") }

// safeVersion is a build or engine version (api.VersionShape).
func safeVersion(s string) string {
	if api.VersionShape(s) {
		return s
	}
	return withheld
}

var (
	osWordRE   = regexp.MustCompile(`^(?:Windows(?: Server)?(?: [0-9]{1,4})?(?: R2)?(?: (?:Home|Pro|Enterprise|Education|Standard|Datacenter|Essentials|LTSC))?|windows|linux|darwin|[0-9]{1,3}(?:\.[0-9]{1,6}){1,3})$`)
	buildDate  = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$`)
	commitHash = regexp.MustCompile(`^[0-9a-f]{4,40}$`)
	jobClassRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
)

// safeOS is the OS text of a device.
func safeOS(s string) string {
	if osWordRE.MatchString(s) {
		return s
	}
	return withheld
}

// safeCommit and safeBuildDate are the build stamp (internal/buildinfo): a hex
// commit and an RFC 3339 UTC date, or "unknown".
func safeCommit(s string) string {
	if s == "unknown" || commitHash.MatchString(s) {
		return s
	}
	return withheld
}

func safeBuildDate(s string) string {
	if s == "unknown" || buildDate.MatchString(s) {
		return s
	}
	return withheld
}

// safeJobClass is the error class of a console job, which the console itself
// names (lower-case words), not an agent.
func safeJobClass(s string) string {
	if jobClassRE.MatchString(s) {
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
	// One request reads one evaluation of the fleet: the page's own read and every read of the admin
	// API it makes share the Memo of the request's context (health/memo.go).
	once := func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if m3Memo {
				r = r.WithContext(health.WithMemo(r.Context()))
			}
			h.ServeHTTP(w, r)
		})
	}
	ro := func(h http.HandlerFunc) http.Handler { return once(s.Auth.Require(auth.RoleReadOnly, auth.ModeUI, h)) }
	ad := func(h http.HandlerFunc) http.Handler { return once(s.Auth.Require(auth.RoleAdmin, auth.ModeUI, h)) }
	r.Handle(patRuns, ro(s.runsPage))
	r.Handle(patRunsTable, ro(s.runsTablePartial))
	r.Handle(patRun, ro(s.runDetailPage))
	r.Handle(patCustomerRun, ro(s.customerRuns))
	r.Handle(patActivity, ro(s.activityPage))
	r.Handle(patOverdue, ro(s.overduePage))
	r.Handle(patUnseen, ro(s.unseenPage))
	r.Handle(patSystem, ro(s.systemPage))
	r.Handle(patSystemRun, ad(s.systemJobRun))
	if s.Fixtures {
		r.Handle(patFixtures, ro(s.fixturesPage))
	}
}

// m3Memo switches the per-request evaluation memo. It is on; the scale measurement in the
// tests turns it off once to show the cost without it.
var m3Memo = true

// patFixtures is the fixtures-mode page that switches "show every console banner" on and off.
const patFixtures = "GET /fixtures"

// FixturesPatterns lists the pattern Register adds in fixtures mode for this ticket.
func fixturesM3Patterns() []string { return []string{patFixtures} }

// fixturesPage is GET /fixtures (fixtures mode only): a lab switch that makes
// every console banner (C00 to C16, every "b" warning, C11 critical) show on every
// page and in the system health rows, so each one can be looked at. The fleet is
// fictional, so the banners claim nothing about a real console. ?banners=all turns
// it on, ?banners=off turns it off; it changes nothing but what the pages show.
// ?health=ok and ?health=real do the same for the System page's healthy view, the
// one way to see overall OK and a not_applicable row on this fleet (adminapi.LabHealthy).
func (s *Service) fixturesPage(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Query().Get("banners") {
	case "all":
		fixtures.SetAllBanners(true)
	case "off":
		fixtures.SetAllBanners(false)
	}
	switch r.URL.Query().Get("health") {
	case "ok":
		fixtures.SetHealthyView(true)
	case "real":
		fixtures.SetHealthyView(false)
	}
	p := principal(r)
	fleet, ok := s.fleetFor(w, r, p, "Fixtures")
	if !ok {
		return
	}
	pg := struct {
		Page
		On, Healthy bool
	}{s.page(r.Context(), p, fleet, "Fixtures mode", "fixtures", ""), fixtures.AllBannersOn(), fixtures.HealthyViewOn()}
	s.render(w, r, http.StatusOK, "fixtures", "base", pg)
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
	"C17b": "GATEWAY LOGIN NOT CHECKED",
}

func init() {
	for id, label := range bannerLabelsM3 {
		if _, ok := bannerLabels[id]; !ok {
			bannerLabels[id] = label
		}
	}
}
