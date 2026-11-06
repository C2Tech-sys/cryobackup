package ui

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	_ "time/tzdata" // the display zone must resolve on hosts without an OS zone database

	"github.com/c2tech-sys/cryobackup/internal/buildinfo"
	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/health"
	"github.com/c2tech-sys/cryobackup/internal/console/httpx"
	"github.com/c2tech-sys/cryobackup/internal/console/ratelimit"
	"github.com/c2tech-sys/cryobackup/internal/console/restores"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
	"github.com/c2tech-sys/cryobackup/internal/state"
	"github.com/c2tech-sys/cryobackup/web"
)

// Service renders the console UI (architecture §5.3, M0 pages) and owns the
// SSE hub. Pages are html/template output over embedded templates; the only
// script is the vendored htmx and first-party fresh.js.
type Service struct {
	Auth     *auth.Manager
	Store    *store.Store
	Fleet    *health.Evaluator
	Restores *restores.Service
	// Backend is the admin-path client: the restore wizard browses snapshots through it.
	Backend adminpath.Backend
	Hub     *Hub
	Clock   func() time.Time
	// Loc is the operator's display zone; Zone its name.
	Loc  *time.Location
	Zone string
	// Refresh is the refresh interval in seconds (config refresh_interval_s).
	Refresh int
	// Display, when set, is the live display zone and refresh interval (M2-W3A,
	// display.go): a settings change replaces them for the next request, and
	// loc(), zone() and refresh() read it. Nil keeps Loc, Zone and Refresh.
	Display *store.DisplayPrefs
	// Fixtures enables /design and the fixtures notice on the login page.
	Fixtures bool
	Log      *logx.Logger
	// MasterLoaded reports banner C12's input for the sidebar.
	MasterLoaded func() bool
	// Maintain reports the worker probe for the sidebar and C07.
	Maintain func() health.MaintainStatus
	// PublicURL is the configured public_url (the agent listener's public
	// address) and ArtifactsDir the artifacts_dir where the operator places the
	// agent MSI: the enrollment wizard and /downloads use both (M2-W2B).
	PublicURL    string
	ArtifactsDir string
	// WakeAgent, when set, is called after a command is queued so the device's
	// command long poll returns at once (agentapi.Notifier.Notify). Nil means the
	// device picks the command up when its current poll ends (at most 30 s).
	WakeAgent func(deviceID string)
	// SyncGateway, when set, is called after a device is retired so the gateway's
	// htpasswd file drops its login at once instead of at the next five-minute run.
	SyncGateway func()

	// Jobs is the console's job runner, for the System page's "Run now" and its
	// job table (M3-T4, m3t4.go). cli sets it once the runner exists, before the
	// first request; nil leaves "Run now" disabled and says so.
	Jobs jobRunner

	templates map[string]*template.Template
	assets    map[string]string
	// m3 is the private admin API the M3-T4 pages read through (m3t4.go).
	m3 m3State
	// winRL limits the live window validator per session (policy.go, M2-W2A).
	winRL *ratelimit.Bucket
}

// New parses the templates and hashes the static assets (for cache-busting URLs).
func New(s *Service) (*Service, error) {
	if s.Clock == nil {
		s.Clock = time.Now
	}
	if s.Loc == nil {
		s.Loc = time.UTC
	}
	if s.Zone == "" {
		s.Zone = s.Loc.String()
	}
	if s.Refresh == 0 {
		s.Refresh = 30
	}
	if s.Display == nil { // M2-W3A: the display settings are live; a service built without them gets its own
		s.Display = store.NewDisplayPrefs(s.Zone, s.Refresh)
	}
	if s.Log == nil {
		s.Log = logx.Default()
	}
	s.winRL = ratelimit.NewBucket(240, time.Minute, 40, s.Clock) // M2-W2A: typing at 250 ms debounce, with headroom
	s.assets = map[string]string{}
	_ = fs.WalkDir(web.Static, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		b, rerr := fs.ReadFile(web.Static, p)
		if rerr != nil {
			return nil
		}
		sum := sha256.Sum256(b)
		s.assets[strings.TrimPrefix(p, "static/")] = hex.EncodeToString(sum[:])[:8]
		return nil
	})
	var err error
	s.templates, err = parseTemplates(s.funcs())
	if err != nil {
		return nil, err
	}
	if s.Hub != nil {
		s.Hub.render = s.renderEvents
		s.Hub.freshness = s.freshnessPayload
		s.Hub.RefreshNow = func() time.Duration { return time.Duration(s.display().RefreshS) * time.Second } // M2-W3A: a changed interval reaches the stream
	}
	return s, nil
}

func parseTemplates(fm template.FuncMap) (map[string]*template.Template, error) {
	pages, err := fs.Glob(web.Templates, "templates/pages/*.html")
	if err != nil {
		return nil, err
	}
	out := map[string]*template.Template{}
	for _, p := range pages {
		name := strings.TrimSuffix(path.Base(p), ".html")
		t, err := template.New(name).Funcs(fm).ParseFS(web.Templates, "templates/base.html", "templates/partials/*.html", p)
		if err != nil {
			return nil, fmt.Errorf("ui: template %s: %w", name, err)
		}
		out[name] = t
	}
	return out, nil
}

func (s *Service) funcs() template.FuncMap {
	return template.FuncMap{
		"asset": func(name string) string {
			if h, ok := s.assets[name]; ok {
				return "/static/" + name + "?v=" + h
			}
			return "/static/" + name
		},
		"add":     func(a, b int) int { return a + b },
		"lower":   strings.ToLower,
		"upper":   strings.ToUpper,
		"join":    strings.Join,
		"urlq":    url.QueryEscape,
		"copybtn": copyButton, // M2-W3A (copy.go)
	}
}

func (s *Service) log() *logx.Logger { return s.Log }

func (s *Service) now() time.Time { return s.Clock().UTC() }

// fc is the formatting context of the request ctx belongs to: now, in the
// request's display zone.
func (s *Service) fc(ctx context.Context) fmtCtx { return s.fcAt(ctx, s.now()) }

// fcAt is fc with a chosen "now" (the fleet's computed time, the fixtures clock).
func (s *Service) fcAt(ctx context.Context, now time.Time) fmtCtx {
	d := s.displayOf(ctx)
	return fmtCtx{Now: now, Loc: d.Loc, Zone: d.Zone}
}

// Register adds the UI routes to r. /design is registered only in fixtures mode.
func (s *Service) Register(reg httpx.Registrar) {
	r := httpx.Registrar(snapRegistrar{Registrar: reg, s: s}) // every handler runs with one display snapshot (display.go)
	ro := func(h http.HandlerFunc) http.Handler { return s.Auth.Require(auth.RoleReadOnly, auth.ModeUI, h) }
	op := func(h http.HandlerFunc) http.Handler { return s.Auth.Require(auth.RoleOperator, auth.ModeUI, h) }
	ad := func(h http.HandlerFunc) http.Handler { return s.Auth.Require(auth.RoleAdmin, auth.ModeUI, h) }
	ow := func(h http.HandlerFunc) http.Handler { return s.Auth.Require(auth.RoleOwner, auth.ModeUI, h) }
	s.registerAccounts(r, ro, ad, ow) // M2-W1A: MFA step, /account, /settings/accounts (mfa.go)
	s.registerEnroll(r)               // M2-W2B: /enroll, token management, /downloads (enroll_routes.go)
	s.registerAuditSettings(r)        // M2-W3A: /audit and /settings (settings.go)
	r.Handle("GET /login", http.HandlerFunc(s.loginPage))
	r.Handle("POST /login", http.HandlerFunc(s.loginPost))
	r.Handle("POST /logout", ro(s.logout))
	r.Handle("GET /{$}", ro(s.board))
	r.Handle("GET /devices/{id}", ro(s.devicePanel))
	s.registerM2(r)
	s.registerM3T4(r)       // M3-T4: runs, activity, overdue, unseen, system (m3t4.go)
	s.registerPolicies(r)   // M2-W2A: policies, customer Policy tab, device overrides (policy_routes.go)
	s.registerCommandsUI(r) // M2-W2C (commands.go)
	r.Handle("GET /devices/{id}/restore", op(s.restorePage))
	r.Handle("POST /ui/devices/{id}/restore", op(s.restorePost))
	r.Handle("GET /restores/{id}", op(s.restoreStatus))
	r.Handle("POST /ui/restores/{id}/download", op(s.restoreDownload))
	r.Handle("GET /ui/events", ro(s.events))
	r.Handle("GET /ui/partials/board", ro(s.boardPartial))
	r.Handle("GET /ui/partials/run-progress/{run_id}", ro(s.runProgressPartial))
	r.Handle("GET /ui/partials/restore/{id}", op(s.restorePartial))
	r.Handle("GET /static/{path...}", http.HandlerFunc(s.static))
	if s.Fixtures {
		r.Handle("GET /design", http.HandlerFunc(s.design))
		r.Handle("GET /design/{component}", http.HandlerFunc(s.design))
	}
}

// Patterns lists the UI patterns Register adds (fixtures mode adds /design).
func Patterns(fixtures bool) []string {
	p := []string{"GET /login", "POST /login", "POST /logout", "GET /{$}", "GET /devices/{id}", "GET /devices/{id}/restore",
		"POST /ui/devices/{id}/restore", "GET /restores/{id}", "POST /ui/restores/{id}/download", "GET /ui/events", "GET /ui/partials/board",
		"GET /ui/partials/run-progress/{run_id}", "GET /ui/partials/restore/{id}", "GET /static/{path...}"}
	p = append(p, m2Patterns()...)
	p = append(p, m3t4Patterns()...)      // M3-T4
	p = append(p, policyUIPatterns()...)  // M2-W2A
	p = append(p, commandPatterns()...)   // M2-W2C
	p = append(p, accountUIPatterns()...) // M2-W1A
	p = append(p, enrollUIPatterns()...)  // M2-W2B
	p = append(p, auditUIPatterns()...)   // M2-W3A
	p = append(p, settingsUIPatterns()...)
	if fixtures {
		p = append(p, "GET /design", "GET /design/{component}")
	}
	return p
}

// ---- rendering ----

func (s *Service) render(w http.ResponseWriter, r *http.Request, status int, page, tmpl string, data any) {
	t, ok := s.templates[page]
	if !ok {
		http.Error(w, "template missing", http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, tmpl, data); err != nil {
		s.log().Error("template failed", logx.Str("page", page), logx.Str("tmpl", tmpl), logx.Class("render"))
		http.Error(w, "The page could not be rendered.", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "same-origin")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// renderString executes a template to a string (SSE payloads, partial fragments).
func (s *Service) renderString(page, tmpl string, data any) (string, error) {
	t, ok := s.templates[page]
	if !ok {
		return "", errors.New("ui: unknown page template")
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, tmpl, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func principal(r *http.Request) *auth.Principal {
	p, _ := auth.FromContext(r.Context())
	return p
}

func (s *Service) sysItems() []SysItem {
	var out []SysItem
	out = append(out, SysItem{Label: "Gateway", Value: "not probed", Class: ClassNeutral})
	m := health.MaintainStatus{}
	if s.Maintain != nil {
		m = s.Maintain()
	}
	switch {
	case !m.Known:
		out = append(out, SysItem{Label: "Maintenance", Value: "not probed", Class: ClassNeutral})
	case m.Reachable:
		out = append(out, SysItem{Label: "Maintenance", Value: "reachable", Class: ClassOK})
	default:
		out = append(out, SysItem{Label: "Maintenance", Value: "unreachable", Class: ClassCrit})
	}
	if s.MasterLoaded != nil && s.MasterLoaded() {
		out = append(out, SysItem{Label: "Master key", Value: "loaded", Class: ClassOK})
	} else {
		out = append(out, SysItem{Label: "Master key", Value: "unavailable", Class: ClassCrit})
	}
	out = append(out, SysItem{Label: "Version", Value: buildinfo.Version, Class: ClassNeutral})
	return out
}

func (s *Service) stamp(ctx context.Context, f health.Fleet) Stamp {
	c := s.fc(ctx)
	st := Stamp{State: "fresh", At: c.clock(f.Computed), ISO: f.Computed.UTC().Format(time.RFC3339), N: f.InView, M: f.Enrolled, Zone: c.Zone}
	if f.InView < f.Enrolled {
		st.State = "partial"
	}
	st.Tooltip = fmt.Sprintf("%s (%s). Computed %s, loaded %s. %d of %d agents classified.", c.fullTime(f.Computed), c.Zone, c.clock(f.Computed), c.clock(f.View.LoadedAt), f.InView, f.Enrolled)
	return st
}

// page builds the shell data shared by every authenticated page.
// The zone, the stamp and the refresh interval all come from the request's one
// display snapshot (M2-W3E).
func (s *Service) page(ctx context.Context, p *auth.Principal, f health.Fleet, title, nav, events string) Page {
	d := s.displayOf(ctx)
	return Page{Title: title, Nav: nav, User: &UserView{Username: p.Admin.Username, Role: p.Role.String()}, CSRF: p.CSRF,
		Banners: buildBanners(f, false), Stamp: s.stamp(ctx, f), Refresh: d.RefreshS, Events: events, Fixtures: s.Fixtures, Sys: s.sysItems(), Zone: d.Zone}
}

func (s *Service) errorPage(w http.ResponseWriter, r *http.Request, p *auth.Principal, status int, title, body string) {
	f, err := s.Fleet.Fleet(r.Context(), p.Scope)
	if err != nil {
		http.Error(w, title, status)
		return
	}
	data := struct {
		Page
		Status int
		Body   string
		Detail string
	}{s.page(r.Context(), p, f, title, "fleet", "/ui/events"), status, body, ""}
	s.render(w, r, status, "error", "base", data)
}

func (s *Service) notFound(w http.ResponseWriter, r *http.Request, p *auth.Principal, what string) {
	s.errorPage(w, r, p, http.StatusNotFound, "Not found", what+" was retired, removed, or never existed. Return to the fleet board.")
}

// ---- login ----

type loginData struct {
	Title    string
	Error    string
	Username string
	Fixtures bool
	Next     string
	Nonce    string // hidden login_nonce field value (auth.IssueLoginNonce)
}

// renderLogin issues a fresh login nonce (cookie plus hidden field) and renders
// the sign-in page; every login page the browser sees carries its own nonce.
func (s *Service) renderLogin(w http.ResponseWriter, r *http.Request, status int, d loginData) {
	d.Nonce = s.Auth.IssueLoginNonce(w, r)
	s.render(w, r, status, "login", "bare", d)
}

func (s *Service) loginPage(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(auth.SessionCookie); err == nil {
		if _, err := s.Auth.Authenticate(r.Context(), c.Value); err == nil {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
	}
	s.renderLogin(w, r, http.StatusOK, loginData{Title: "Sign in", Fixtures: s.Fixtures})
}

func (s *Service) loginPost(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if err := r.ParseForm(); err != nil {
		s.renderLogin(w, r, http.StatusBadRequest, loginData{Title: "Sign in", Error: "The form could not be read.", Fixtures: s.Fixtures})
		return
	}
	// Evidence precedence (FIX23b): a nonce only makes up for MISSING header
	// evidence. Headers that point at another site refuse the post whatever the
	// nonce says. Otherwise a valid nonce (the cookie this server issued plus its
	// MAC in the hidden field) carries a post whose browser stripped Origin,
	// Referer and Sec-Fetch-Site; without one the header rule decides.
	nonceOK := s.Auth.LoginNonceOK(r)
	if !nonceOK && auth.LoginNoncePresent(r) {
		// Log the fact only, never either value.
		s.log().Warn("login nonce did not match: the cookie is missing, expired, injected or different from the form field")
	}
	if s.Auth.HeadersContradict(r) || (!nonceOK && !s.Auth.LoginOriginOK(r)) {
		// Operators hit this when the browser reaches the console through a name or
		// proxy the console does not know (FIX21): say whether Origin and Referer
		// named this host. Derived fields only (loginRefusalAttrs), never a header
		// value: a Referer can carry a path and a User-Agent names a workstation.
		s.log().Warn("login refused: the request origin does not name this host",
			append(loginRefusalAttrs(r, s.Auth.PublicHost), logx.Str("client_ip", httpx.ClientIP(r)))...)
		s.renderLogin(w, r, http.StatusForbidden, loginData{Title: "Sign in", Error: "The request came from another site and was refused.", Fixtures: s.Fixtures})
		return
	}
	user, pw := strings.TrimSpace(r.PostFormValue("username")), r.PostFormValue("password")
	res, err := s.Auth.Login(r.Context(), user, pw, httpx.ClientIP(r), r.UserAgent())
	if err != nil {
		d := loginData{Title: "Sign in", Username: user, Fixtures: s.Fixtures}
		status := http.StatusUnauthorized
		var rl *auth.RateLimitedError
		switch {
		case errors.As(err, &rl):
			status = http.StatusTooManyRequests
			w.Header().Set("Retry-After", strconv.Itoa(int(rl.RetryAfter/time.Second)+1))
			d.Error = "Too many sign-in attempts from this address. Wait a minute and try again."
		case errors.Is(err, auth.ErrInvalidCredentials):
			d.Error = "The username or password was not recognised."
		default:
			s.log().Error("login failed", logx.Err(err))
			status = http.StatusInternalServerError
			d.Error = "Sign-in is unavailable. Try again shortly."
		}
		s.renderLogin(w, r, status, d)
		return
	}
	s.Auth.SetCookies(w, r, res)
	auth.ClearLoginNonce(w, r)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Service) logout(w http.ResponseWriter, r *http.Request) {
	_ = s.Auth.EndSession(r.Context(), principal(r), "web", httpx.ClientIP(r))
	auth.ClearCookies(w, r)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// ---- static ----

func (s *Service) static(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("path")
	if name == "" || strings.HasSuffix(name, "/") || strings.Contains(name, "..") || strings.ContainsRune(name, '\\') {
		http.NotFound(w, r)
		return
	}
	full := "static/" + path.Clean(name)
	info, err := fs.Stat(web.Static, full)
	if err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}
	b, err := fs.ReadFile(web.Static, full)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", contentType(name))
	h.Set("X-Content-Type-Options", "nosniff")
	if v := r.URL.Query().Get("v"); v != "" && v == s.assets[path.Clean(name)] {
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		h.Set("Cache-Control", "public, max-age=3600")
	}
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(b))
}

func contentType(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".css":
		return "text/css; charset=utf-8"
	case ".js":
		return "text/javascript; charset=utf-8"
	case ".woff2":
		return "font/woff2"
	case ".json":
		return "application/json"
	case ".svg":
		return "image/svg+xml"
	case ".txt":
		return "text/plain; charset=utf-8"
	}
	return "application/octet-stream"
}

// ---- board ----

func validStateName(s string) bool {
	for _, st := range state.Precedence() {
		if string(st) == s {
			return true
		}
	}
	return false
}

func (s *Service) runningInfo(ctx context.Context, p *auth.Principal, ds state.DeviceState) RunInfo {
	if ds.Detail.OpenRunAge <= 0 && ds.Detail.OpenRunLimit <= 0 {
		return RunInfo{}
	}
	info := RunInfo{Open: true, Pct: -1}
	if pr, err := s.Store.GetPresence(ctx, p.Scope, ds.ID); err == nil {
		if prog := parseProgress(pr.Progress); prog.RunID != "" || prog.Total > 0 || prog.Pct > 0 {
			info.Pct = prog.Pct
		}
	}
	return info
}

func (s *Service) deviceMetas(ctx context.Context, p *auth.Principal) map[string]deviceMeta {
	out := map[string]deviceMeta{}
	for cursor := ""; ; {
		l, err := s.Store.ListDevices(ctx, p.Scope, store.ListDevicesOptions{IncludeRetired: true, Page: store.Page{Limit: 1000, Cursor: cursor}})
		if err != nil {
			return out
		}
		for _, d := range l.Items {
			out[d.ID] = deviceMeta{OS: d.OSVersion, Class: d.DeviceClass, Enrolled: d.EnrolledAt}
		}
		if l.NextCursor == "" {
			return out
		}
		cursor = l.NextCursor
	}
}

func (s *Service) boardData(ctx context.Context, p *auth.Principal, f health.Fleet, stateFilter, events string, oob bool) BoardData {
	c := s.fc(ctx)
	strips, err := health.TenDay(ctx, s.Store, p.Scope, f.View, f.Computed, s.loc(ctx))
	if err != nil {
		s.log().Warn("ten day strips unavailable", logx.Err(err))
		strips = nil
	}
	metas := s.deviceMetas(ctx, p)
	pg := s.page(ctx, p, f, "Fleet health", "fleet", events)
	pg.Banners = buildBanners(f, oob)
	pg.Stamp.OOB = oob
	b := BoardData{Page: pg, Columns: 5, AsOf: c.clock(f.Computed)}
	if validStateName(stateFilter) {
		b.QS = "?state=" + url.QueryEscape(stateFilter)
		b.Filter = FilterView{Active: true, State: stateFilter, Label: stateLabel(state.State(stateFilter))}
	}
	running := 0
	total, shown := 0, 0
	rowsByCustomer := map[string][]DeviceRow{}
	for _, cs := range f.View.Customers {
		var rows []DeviceRow
		for _, ds := range cs.Devices {
			if ds.Primary == state.StateRetired {
				continue
			}
			total++
			ri := s.runningInfo(ctx, p, ds)
			if ri.Open {
				running++
			}
			if b.Filter.Active && string(ds.Primary) != b.Filter.State {
				continue
			}
			shown++
			rows = append(rows, deviceRow(ds, metas[ds.ID], strips, ri, c))
		}
		rowsByCustomer[cs.ID] = rows
	}
	b.Filter.Shown, b.Filter.Total = shown, total
	for _, cs := range f.View.Customers {
		rows := rowsByCustomer[cs.ID]
		if b.Filter.Active && len(rows) == 0 {
			continue
		}
		b.Groups = append(b.Groups, groupView(cs, rows, c))
	}
	b.KPIs = kpis(f, c, running)
	b.Rings = []Ring{ageRing(f, c), customerRing(f)}
	switch {
	case f.View.FirstRun:
		b.Empty = &EmptyView{Kind: "neutral", Title: "No customers yet",
			Body: "Create the first customer and an enrollment token with: cryo-console token --create-customer \"Customer name\". Devices appear here as pending their first backup."}
	case total == 0:
		b.Empty = &EmptyView{Kind: "alarm", Title: "No devices are enrolled",
			Body: "Nothing is being backed up. Enroll a customer or check the agent listener."}
	case b.Filter.Active && shown == 0:
		b.Empty = &EmptyView{Kind: "neutral", Title: "No devices match these filters",
			Body: fmt.Sprintf("0 of %d shown.", total), Link: &Link{Href: "/", Label: "Clear filters"}}
	}
	return b
}

func stateLabel(st state.State) string {
	if spec, ok := catalogue[st]; ok {
		return spec.Label
	}
	return string(st)
}

func (s *Service) board(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	f, err := s.Fleet.Fleet(r.Context(), p.Scope)
	if err != nil {
		s.log().Error("board: fleet failed", logx.Err(err))
		s.render(w, r, http.StatusServiceUnavailable, "error", "base", errorData(s.fallbackPage(r.Context(), p), 503, "Fleet unavailable", "The fleet could not be loaded. This is an alarm, not an empty fleet. Try again shortly."))
		return
	}
	data := s.boardData(r.Context(), p, f, r.URL.Query().Get("state"), "/ui/events", false)
	s.render(w, r, http.StatusOK, "board", "base", data)
}

func (s *Service) fallbackPage(ctx context.Context, p *auth.Principal) Page {
	d := s.displayOf(ctx)
	return Page{Title: "Fleet unavailable", Nav: "fleet", User: &UserView{Username: p.Admin.Username, Role: p.Role.String()}, CSRF: p.CSRF,
		Stamp: Stamp{State: "unreachable", At: "--:--:--", Zone: d.Zone}, Refresh: d.RefreshS, Events: "/ui/events", Fixtures: s.Fixtures, Sys: s.sysItems(), Zone: d.Zone}
}

func errorData(pg Page, status int, title, body string) any {
	pg.Title = title
	return struct {
		Page
		Status int
		Body   string
		Detail string
	}{pg, status, body, ""}
}

// boardPartial is the polling fallback of the board (components §0.7): the
// board fragment plus out-of-band replacements of the banner strip and the stamp.
func (s *Service) boardPartial(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	f, err := s.Fleet.Fleet(r.Context(), p.Scope)
	if err != nil {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	data := s.boardData(r.Context(), p, f, r.URL.Query().Get("state"), "", true)
	s.render(w, r, http.StatusOK, "board", "board_partial", data)
}

// ---- SSE payloads ----

// freshnessPayload is the JSON of the freshness event.
func (s *Service) freshnessPayload(f health.Fleet) string {
	return s.freshnessAt(context.Background(), f)
}

func (s *Service) freshnessAt(ctx context.Context, f health.Fleet) string {
	c := s.fc(ctx)
	return fmt.Sprintf(`{"at":%q,"iso":%q,"n":%d,"m":%d}`, c.clock(f.Computed), f.Computed.UTC().Format(time.RFC3339), f.InView, f.Enrolled)
}

// renderEvents renders what a subscriber needs after a change or on the refresh
// tick: the board (or the device summary), the banner strip and the freshness
// stamp. Events carry server-rendered HTML fragments keyed by element id.
func (s *Service) renderEvents(ctx context.Context, sub *Sub) ([]Event, error) {
	ctx = s.withDisplay(ctx) // one display snapshot for the whole render
	p := &auth.Principal{Scope: sub.Scope, Role: sub.Role}
	f, err := s.Fleet.Fleet(ctx, sub.Scope)
	if err != nil {
		return nil, err
	}
	var evs []Event
	ban, err := s.renderString("board", "banners", buildBanners(f, false))
	if err != nil {
		return nil, err
	}
	evs = append(evs, Event{Name: "banner.update", Data: ban})
	if sub.DeviceID != "" {
		if html, err := s.deviceLive(ctx, p, f, sub.DeviceID); err == nil {
			evs = append(evs, Event{Name: "fleet.state", Data: html})
		}
	} else {
		data := s.boardData(ctx, p, f, sub.Filter, "", false)
		html, err := s.renderString("board", "board", data)
		if err != nil {
			return nil, err
		}
		evs = append(evs, Event{Name: "fleet.state", Data: html})
	}
	evs = append(evs, Event{Name: "freshness", Data: s.freshnessAt(ctx, f)})
	return evs, nil
}

// events is GET /ui/events (architecture §13.1).
func (s *Service) events(w http.ResponseWriter, r *http.Request) {
	if s.Hub == nil {
		http.NotFound(w, r)
		return
	}
	p := principal(r)
	filter := ""
	if v := strings.ToUpper(r.URL.Query().Get("state")); validStateName(v) {
		filter = v
	}
	device := r.URL.Query().Get("device")
	if device != "" {
		if _, err := s.Store.GetDevice(r.Context(), p.Scope, device); err != nil {
			http.NotFound(w, r)
			return
		}
	}
	s.Hub.ServeSSE(w, r, &Sub{Scope: p.Scope, Role: p.Role, Filter: filter, DeviceID: device})
}
