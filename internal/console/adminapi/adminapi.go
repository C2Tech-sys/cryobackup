// Package adminapi is the admin JSON API of architecture §5.2 on the private
// listener. M0 routes: login and logout, the device list and detail, restore
// creation, status and download, and the version. Errors are RFC 9457
// application/problem+json exactly like the agent API (api.NewError, one error
// model for every route set); every success body carries computed_at (§4.6).
package adminapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/buildinfo"
	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/health"
	"github.com/c2tech-sys/cryobackup/internal/console/httpx"
	"github.com/c2tech-sys/cryobackup/internal/console/restores"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// Route patterns (architecture §5.2). They are exported so the route-coverage
// test and the UI refer to the same strings.
const (
	PatternLogin           = "POST /api/v1/auth/login"
	PatternLogout          = "POST /api/v1/auth/logout"
	PatternDevices         = "GET /api/v1/devices"
	PatternDevice          = "GET /api/v1/devices/{id}"
	PatternRestoreCreate   = "POST /api/v1/devices/{id}/restores"
	PatternRestore         = "GET /api/v1/restores/{id}"
	PatternRestoreDownload = "GET /api/v1/restores/{id}/download"
	// PatternRestoreMint mints the single-use download token and returns the GET URL.
	PatternRestoreMint = "POST /api/v1/restores/{id}/download"
	PatternVersion     = "GET /api/v1/version"
)

// Patterns lists the M0 admin API patterns.
func Patterns() []string {
	p := []string{PatternLogin, PatternLogout, PatternDevices, PatternDevice, PatternRestoreCreate, PatternRestore, PatternRestoreDownload, PatternRestoreMint, PatternVersion}
	p = append(p, m2Patterns()...)
	p = append(p, accountPatternList()...) // M2-W1A: accounts, MFA, API tokens (accounts.go)
	p = append(p, enrollPatternList()...)  // M2-W2B: enrollment tokens and the script generator (enroll_tokens.go)
	p = append(p, policyPatterns()...)     // M2-W2A: policies, impact, overrides (policies.go)
	p = append(p, commandPatterns()...)    // M2-W2C: commands, run cancel, identity resolve (commands.go)
	p = append(p, auditPatterns()...)      // M2-W3A: audit trail (audit.go)
	p = append(p, settingsPatterns()...)   // M2-W3A: console settings (settings.go)
	return p
}

// Service holds what the handlers need.
type Service struct {
	Auth     *auth.Manager
	Store    *store.Store
	Fleet    *health.Evaluator
	Restores *restores.Service
	// Backend is the admin-path client, used for the version route's worker probe.
	Backend adminpath.Backend
	Clock   func() time.Time
	Log     *logx.Logger
	// Loc is the operator's display zone, in which a device's time window is read
	// for the next-due estimate (nil = UTC).
	Loc *time.Location
	// Display, when set, is the live display zone and refresh interval shared
	// with the UI (M2-W3A, settings.go): loc() and zone() read it per request so
	// a settings change applies at once. Nil keeps Loc.
	Display *store.DisplayPrefs
	// PublicURL is the configured public_url (the agent listener's public
	// address) and ArtifactsDir the artifacts_dir where the operator places the
	// agent MSI: the deployment generator (enroll_snippets.go) uses both.
	PublicURL    string
	ArtifactsDir string
	// WakeAgent, when set, is called after a command is queued for a device so the
	// device's command long poll returns at once (agentapi.Notifier.Notify). Nil
	// means the device picks the command up when its current poll ends (at most
	// 30 s later).
	WakeAgent func(deviceID string)
	// SyncGateway, when set, is called after a device is retired so the gateway's
	// htpasswd file drops its login at once instead of at the next five-minute run.
	SyncGateway func()
}

func (s *Service) now() time.Time {
	if s.Clock != nil {
		return s.Clock().UTC()
	}
	return time.Now().UTC()
}

// Register adds the routes to r.
func (s *Service) Register(r httpx.Registrar) {
	r.Handle(PatternLogin, http.HandlerFunc(s.login))
	ro := func(h http.HandlerFunc) http.Handler { return s.Auth.Require(auth.RoleReadOnly, auth.ModeAPI, h) }
	op := func(h http.HandlerFunc) http.Handler { return s.Auth.Require(auth.RoleOperator, auth.ModeAPI, h) }
	r.Handle(PatternLogout, ro(s.logout))
	r.Handle(PatternDevices, ro(s.listDevices))
	r.Handle(PatternDevice, ro(s.getDevice))
	r.Handle(PatternRestoreCreate, op(s.createRestore))
	r.Handle(PatternRestore, ro(s.getRestore))
	r.Handle(PatternRestoreDownload, op(s.download))
	r.Handle(PatternRestoreMint, op(s.mintDownload))
	r.Handle(PatternVersion, ro(s.version))
	s.registerM2(r)
	s.registerAccounts(r) // M2-W1A (accounts.go)
	s.registerEnroll(r)   // M2-W2B (enroll_tokens.go)
	s.registerPolicies(r) // M2-W2A (policies.go)
	s.registerCommands(r) // M2-W2C (commands.go)
	s.registerAudit(r)    // M2-W3A (audit.go)
	s.registerSettings(r) // M2-W3A (settings.go)
}

// display is one consistent reading of the display zone (the live preference,
// else Loc, else UTC). A handler that shows the zone and its name takes one
// snapshot, so a concurrent settings change cannot mix them.
func (s *Service) display() store.DisplaySnapshot {
	switch {
	case s.Display != nil:
		return s.Display.Snapshot()
	case s.Loc != nil:
		return store.DisplaySnapshot{Zone: s.Loc.String(), Loc: s.Loc}
	}
	return store.DisplaySnapshot{Zone: "UTC", Loc: time.UTC}
}

// loc is the operator's display zone now.
func (s *Service) loc() *time.Location { return s.display().Loc }

// ---- JSON shapes ----

// AdminJSON is the admin as shown to itself.
type AdminJSON struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Role     string `json:"role"`
}

// LoginRequest is the body of POST /api/v1/auth/login.
type LoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// LoginResponse is the answer of the password step (mfa_required true: the
// cookie is a pending session that can only do the MFA step, or, with
// mfa_enrollment_required, the forced TOTP enrollment) and of a completed MFA
// step (mfa_required false: a full session; csrf_token belongs to the new cookie).
type LoginResponse struct {
	MFARequired           bool      `json:"mfa_required"`
	MFAEnrollmentRequired bool      `json:"mfa_enrollment_required,omitempty"`
	CSRFToken             string    `json:"csrf_token"`
	Admin                 AdminJSON `json:"admin"`
	RecoveryCodesLeft     *int      `json:"recovery_codes_left,omitempty"`
	ComputedAt            time.Time `json:"computed_at"`
}

// DeviceJSON is a row of the device list.
type DeviceJSON struct {
	ID            string     `json:"id"`
	CustomerID    string     `json:"customer_id"`
	Customer      string     `json:"customer"`
	Hostname      string     `json:"hostname"`
	DeviceClass   string     `json:"device_class,omitempty"`
	OSVersion     string     `json:"os_version,omitempty"`
	AgentVersion  string     `json:"agent_version,omitempty"`
	State         string     `json:"state"`
	Flags         []string   `json:"flags"`
	Severity      string     `json:"severity"`
	Reason        string     `json:"reason,omitempty"`
	Since         *time.Time `json:"since,omitempty"`
	Attributes    []string   `json:"attributes"`
	LastSuccessAt *time.Time `json:"last_success_at,omitempty"`
	LastSeenAt    *time.Time `json:"last_seen_at,omitempty"`
	Acknowledged  bool       `json:"acknowledged"`
}

// DeviceListResponse is GET /api/v1/devices.
type DeviceListResponse struct {
	ComputedAt time.Time    `json:"computed_at"`
	LoadedAt   time.Time    `json:"loaded_at"`
	Complete   bool         `json:"complete"`
	Total      int          `json:"total"`
	Truncated  bool         `json:"truncated"`
	NextCursor string       `json:"next_cursor,omitempty"`
	Devices    []DeviceJSON `json:"devices"`
}

// RunJSON is one run of the history; it holds counts and classes, never a path.
type RunJSON struct {
	ID             string         `json:"id"`
	Kind           string         `json:"kind"`
	Trigger        string         `json:"trigger"`
	Origin         string         `json:"origin"`
	StartedAt      time.Time      `json:"started_at"`
	FinishedAt     *time.Time     `json:"finished_at,omitempty"`
	Outcome        string         `json:"outcome,omitempty"`
	ExitCode       *int           `json:"exit_code,omitempty"`
	ErrorClass     string         `json:"error_class,omitempty"`
	ErrorCounts    map[string]int `json:"error_counts,omitempty"`
	DeferredReason string         `json:"deferred_reason,omitempty"`
	Attempt        int            `json:"attempt"`
}

// DeviceDetailJSON is GET /api/v1/devices/{id}.
type DeviceDetailJSON struct {
	DeviceJSON
	ComputedAt           time.Time  `json:"computed_at"`
	EnrolledAt           time.Time  `json:"enrolled_at"`
	EnrollConfirmedAt    *time.Time `json:"enroll_confirmed_at,omitempty"`
	PausedUntil          *time.Time `json:"paused_until,omitempty"`
	PolicyVersion        int        `json:"policy_version"`
	PolicyETag           string     `json:"policy_etag"`
	PolicyVersionApplied *int       `json:"policy_version_applied,omitempty"`
	Custody              string     `json:"custody,omitempty"`
	EscrowVerifiedAt     *time.Time `json:"escrow_verified_at,omitempty"`
	ServiceState         string     `json:"service_state,omitempty"`
	ClockOffsetMS        *int64     `json:"clock_offset_ms,omitempty"`
	Snapshots            int        `json:"snapshots"`
	Runs                 []RunJSON  `json:"runs"`
}

// RestoreJSON is a restore request as the API shows it. DownloadReady says an
// operator may mint a download token (POST .../download); the token itself never
// appears in a status body. A restore is ready as soon as it is created: the
// zip is produced only while a download streams it (ADR 0008), so Bytes and
// SHA256 describe the last completed download and are absent before one.
type RestoreJSON struct {
	ID            string     `json:"id"`
	DeviceID      string     `json:"device_id"`
	SnapshotID    string     `json:"snapshot_id"`
	Mode          string     `json:"mode"`
	Status        string     `json:"status"`
	ErrorClass    string     `json:"error_class,omitempty"`
	Bytes         *int64     `json:"bytes,omitempty"`
	SHA256        string     `json:"sha256,omitempty"`
	DownloadCount int        `json:"download_count"`
	CreatedAt     time.Time  `json:"created_at"`
	ExpiresAt     time.Time  `json:"expires_at"`
	PurgeAfter    *time.Time `json:"purge_after,omitempty"`
	DownloadReady bool       `json:"download_ready"`
	ComputedAt    time.Time  `json:"computed_at"`
}

// DownloadResponse is the answer of POST /api/v1/restores/{id}/download: a
// download URL whose token is bound to this session and the restore, valid for
// two minutes and replaced by the next mint.
type DownloadResponse struct {
	DownloadURL string    `json:"download_url"`
	ExpiresAt   time.Time `json:"expires_at"`
	ComputedAt  time.Time `json:"computed_at"`
}

// CreateRestoreRequest is the body of POST /api/v1/devices/{id}/restores.
type CreateRestoreRequest struct {
	SnapshotID string `json:"snapshot_id"`
	Mode       string `json:"mode"`
	Path       string `json:"path"`
}

// VersionResponse is GET /api/v1/version.
type VersionResponse struct {
	Console    string    `json:"console"`
	Commit     string    `json:"commit"`
	BuildDate  string    `json:"build_date"`
	Schema     int       `json:"schema"`
	Maintain   *Worker   `json:"maintain,omitempty"`
	ComputedAt time.Time `json:"computed_at"`
}

// Worker is what the maintenance worker answered to a ping. ResticVersion and
// RcloneVersion are "unknown" until adminpath.PingResult carries them (it has no
// such fields today, so the worker cannot report them); a worker that does not
// answer has both empty.
type Worker struct {
	Reachable       bool   `json:"reachable"`
	Version         string `json:"version,omitempty"`
	MasterKeyLoaded bool   `json:"master_key_loaded"`
	ResticVersion   string `json:"restic_version,omitempty"`
	RcloneVersion   string `json:"rclone_version,omitempty"`
}

// toolUnknown is reported for a tool version the worker's ping does not carry.
const toolUnknown = "unknown"

// ---- helpers ----

func (s *Service) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		httpx.Problem(w, r, api.CodeNotFound, 0)
	case errors.Is(err, store.ErrScope):
		httpx.Problem(w, r, api.CodeForbidden, 0)
	case errors.Is(err, store.ErrInvalid), errors.Is(err, restores.ErrInvalid):
		httpx.Problem(w, r, api.CodeInvalidRequest, 0)
	case errors.Is(err, restores.ErrExpired):
		// Past expires_at: every token is refused and none is minted. 410 Gone
		// tells it apart from a restore that is merely not downloadable now.
		httpx.ProblemStatus(w, r, api.CodeNotFound, http.StatusGone, 0)
	case errors.Is(err, restores.ErrStreamsBusy):
		// Every restore stream of the maintenance worker is in use: refused,
		// not queued; the browser may try again with a new link.
		httpx.Problem(w, r, api.CodeUnavailable, 30*time.Second)
	case errors.Is(err, store.ErrConflict), errors.Is(err, restores.ErrNotReady), errors.Is(err, restores.ErrBusy):
		httpx.Problem(w, r, api.CodeConflict, 0)
	case errors.Is(err, restores.ErrCustomerHeld):
		httpx.Problem(w, r, api.CodeCustodyCustomerHeld, 0)
	case errors.Is(err, restores.ErrMasterKey):
		httpx.Problem(w, r, api.CodeMasterKeyUnavailable, 30*time.Second)
	case errors.Is(err, restores.ErrToken):
		httpx.Problem(w, r, api.CodeForbidden, 0)
	case errors.Is(err, store.ErrAudit):
		// The change was refused because its audit row could not be written; the
		// same class and wording as failM2, so an operator searches for one thing.
		s.log().Error("admin api: change refused, not audited", logx.Class("audit"), logx.Err(err))
		httpx.Problem(w, r, api.CodeInternal, 0)
	default:
		s.log().Error("admin api failed", logx.Str("route", "api"), logx.Err(err))
		httpx.Problem(w, r, api.CodeInternal, 0)
	}
}

func (s *Service) log() *logx.Logger {
	if s.Log != nil {
		return s.Log
	}
	return logx.Default()
}

func ptrTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

func strs[T ~string](in []T) []string {
	out := make([]string, len(in))
	for i, v := range in {
		out[i] = string(v)
	}
	return out
}

func deviceJSON(d state.DeviceState, custName string, meta store.Device) DeviceJSON {
	return DeviceJSON{
		ID: d.ID, CustomerID: d.CustomerID, Customer: custName, Hostname: d.Hostname, DeviceClass: meta.DeviceClass, OSVersion: meta.OSVersion,
		AgentVersion: d.Detail.AgentVersion, State: string(d.Primary), Flags: strs(d.Flags), Severity: d.Alarm.String(), Reason: string(d.Reason),
		Since: ptrTime(d.Since), Attributes: strs(d.Attrs), LastSuccessAt: ptrTime(d.Detail.LastSuccess), LastSeenAt: ptrTime(d.Detail.LastSeen),
		Acknowledged: d.Ack != nil,
	}
}

func (s *Service) deviceMeta(ctx context.Context, scope store.Scope) (map[string]store.Device, error) {
	out := map[string]store.Device{}
	for cursor := ""; ; {
		l, err := s.Store.ListDevices(ctx, scope, store.ListDevicesOptions{IncludeRetired: true, Page: store.Page{Limit: 1000, Cursor: cursor}})
		if err != nil {
			return nil, err
		}
		for _, d := range l.Items {
			out[d.ID] = d
		}
		if l.NextCursor == "" {
			return out, nil
		}
		cursor = l.NextCursor
	}
}

// ---- auth ----

func (s *Service) login(w http.ResponseWriter, r *http.Request) {
	if !s.Auth.LoginOriginOK(r) {
		httpx.Problem(w, r, api.CodeForbidden, 0)
		return
	}
	var req LoginRequest
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || req.Username == "" || req.Password == "" {
		httpx.Problem(w, r, api.CodeInvalidRequest, 0)
		return
	}
	res, err := s.Auth.Login(r.Context(), req.Username, req.Password, httpx.ClientIP(r), r.UserAgent())
	if err != nil {
		var rl *auth.RateLimitedError
		switch {
		case errors.As(err, &rl):
			httpx.Problem(w, r, api.CodeRateLimited, rl.RetryAfter)
		case errors.Is(err, auth.ErrInvalidCredentials):
			httpx.Problem(w, r, api.CodeUnauthorized, 0)
		default:
			s.fail(w, r, err)
		}
		return
	}
	s.Auth.SetCookies(w, r, res)
	httpx.WriteJSON(w, http.StatusOK, LoginResponse{MFARequired: res.MFARequired, MFAEnrollmentRequired: res.EnrollRequired, CSRFToken: res.CSRF, ComputedAt: s.now(),
		Admin: AdminJSON{ID: res.Admin.ID, Username: res.Admin.Username, Role: res.Admin.Role}})
}

func (s *Service) logout(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	if err := s.Auth.EndSession(r.Context(), p, "api", httpx.ClientIP(r)); err != nil {
		s.fail(w, r, err)
		return
	}
	auth.ClearCookies(w, r)
	w.WriteHeader(http.StatusNoContent)
}

// ---- devices ----

func (s *Service) listDevices(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	q := r.URL.Query()
	limit := 100
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 500 {
			httpx.Problem(w, r, api.CodeInvalidRequest, 0)
			return
		}
		limit = n
	}
	offset := 0
	if c := q.Get("cursor"); c != "" {
		n, err := strconv.Atoi(strings.TrimPrefix(c, "o"))
		if err != nil || !strings.HasPrefix(c, "o") || n < 0 {
			httpx.Problem(w, r, api.CodeInvalidRequest, 0)
			return
		}
		offset = n
	}
	wantState := strings.ToUpper(q.Get("state"))
	if wantState != "" && !validState(wantState) {
		httpx.Problem(w, r, api.CodeInvalidRequest, 0)
		return
	}
	includeRetired := q.Get("include_retired") == "true"
	search := strings.ToLower(q.Get("q"))

	fleet, err := s.Fleet.Fleet(r.Context(), p.Scope)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	meta, err := s.deviceMeta(r.Context(), p.Scope)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	names := map[string]string{}
	for _, c := range fleet.View.Customers {
		names[c.ID] = c.Name
	}
	var rows []DeviceJSON
	for _, d := range fleet.View.Devices {
		if d.Primary == state.StateRetired && !includeRetired {
			continue
		}
		if wantState != "" && string(d.Primary) != wantState {
			continue
		}
		if cid := q.Get("customer_id"); cid != "" && d.CustomerID != cid {
			continue
		}
		if search != "" && !strings.Contains(strings.ToLower(d.Hostname), search) {
			continue
		}
		rows = append(rows, deviceJSON(d, names[d.CustomerID], meta[d.ID]))
	}
	total := len(rows)
	if offset > total {
		offset = total
	}
	end := offset + limit
	resp := DeviceListResponse{ComputedAt: fleet.Computed, LoadedAt: fleet.View.LoadedAt, Complete: fleet.View.Complete, Total: total}
	if end < total {
		resp.NextCursor, resp.Truncated = "o"+strconv.Itoa(end), true
	} else {
		end = total
	}
	resp.Devices = rows[offset:end]
	if resp.Devices == nil {
		resp.Devices = []DeviceJSON{}
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}

func validState(s string) bool {
	for _, st := range state.Precedence() {
		if string(st) == s {
			return true
		}
	}
	return false
}

func (s *Service) getDevice(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	id := r.PathValue("id")
	ctx := r.Context()
	dev, err := s.Store.GetDevice(ctx, p.Scope, id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	fleet, err := s.Fleet.Fleet(ctx, p.Scope)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var ds *state.DeviceState
	for i := range fleet.View.Devices {
		if fleet.View.Devices[i].ID == id {
			ds = &fleet.View.Devices[i]
		}
	}
	if ds == nil {
		httpx.Problem(w, r, api.CodeNotFound, 0)
		return
	}
	custName := ""
	for _, c := range fleet.View.Customers {
		if c.ID == dev.CustomerID {
			custName = c.Name
		}
	}
	out := DeviceDetailJSON{DeviceJSON: deviceJSON(*ds, custName, dev), ComputedAt: fleet.Computed, EnrolledAt: dev.EnrolledAt.UTC(),
		EnrollConfirmedAt: ptrTime(zeroIfNil(dev.EnrollConfirmedAt)), PausedUntil: ptrTime(zeroIfNil(dev.PausedUntil)), PolicyVersionApplied: dev.PolicyVersionApplied}
	if _, ver, etag, err := s.Store.EffectivePolicyFor(ctx, p.Scope, id); err == nil {
		out.PolicyVersion, out.PolicyETag = ver, etag
	}
	if esc, err := s.Store.ActiveEscrow(ctx, p.Scope, id); err == nil {
		out.Custody = esc.Custody
		if esc.VerifiedAt != nil {
			out.EscrowVerifiedAt = ptrTime(*esc.VerifiedAt)
		}
	}
	if pr, err := s.Store.GetPresence(ctx, p.Scope, id); err == nil {
		out.ServiceState, out.ClockOffsetMS = pr.ServiceState, pr.ClockOffsetMS
	}
	if sn, err := s.Store.ListSnapshots(ctx, p.Scope, id); err == nil {
		out.Snapshots = len(sn)
	}
	runs, err := s.Store.ListRuns(ctx, p.Scope, id, store.Page{Limit: 10})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out.Runs = make([]RunJSON, 0, len(runs.Items))
	for _, ru := range runs.Items {
		out.Runs = append(out.Runs, RunJSON{ID: ru.ID, Kind: ru.Kind, Trigger: ru.Trigger, Origin: ru.Origin, StartedAt: ru.StartedAt, FinishedAt: ru.FinishedAt,
			Outcome: ru.Outcome, ExitCode: ru.ResticExitCode, ErrorClass: ru.ErrorClass, ErrorCounts: ru.ErrorCounts, DeferredReason: ru.DeferredReason, Attempt: ru.Attempt})
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func zeroIfNil(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

// ---- restores ----

func (s *Service) restoreJSON(p *auth.Principal, rr store.RestoreRequest) RestoreJSON {
	out := RestoreJSON{ID: rr.ID, DeviceID: rr.DeviceID, SnapshotID: rr.SnapshotID, Mode: rr.Mode, Status: rr.Status, ErrorClass: rr.ErrorClass, Bytes: rr.Bytes,
		DownloadCount: rr.DownloadCount, CreatedAt: rr.CreatedAt, ExpiresAt: rr.ExpiresAt, PurgeAfter: rr.PurgeAfter, ComputedAt: s.now()}
	var pr struct {
		SHA256 string `json:"sha256"`
	}
	if len(rr.Progress) > 0 && json.Unmarshal(rr.Progress, &pr) == nil {
		out.SHA256 = pr.SHA256
	}
	out.DownloadReady = s.Restores.Downloadable(rr) && p.Role >= auth.RoleOperator
	return out
}

func (s *Service) createRestore(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	var req CreateRestoreRequest
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		httpx.Problem(w, r, api.CodeInvalidRequest, 0)
		return
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		httpx.Problem(w, r, api.CodeInvalidRequest, 0)
		return
	}
	if req.Mode != "download_zip" {
		httpx.Problem(w, r, api.CodeInvalidRequest, 0)
		return
	}
	rr, err := s.Restores.Create(r.Context(), restores.Actor{AdminID: p.Admin.ID, Username: p.Admin.Username, IP: httpx.ClientIP(r), Method: "api"},
		p.Scope, r.PathValue("id"), req.SnapshotID, req.Path)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Location", "/api/v1/restores/"+rr.ID)
	httpx.WriteJSON(w, http.StatusAccepted, s.restoreJSON(p, rr))
}

func (s *Service) getRestore(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	rr, err := s.Store.GetRestoreRequest(r.Context(), p.Scope, r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, s.restoreJSON(p, rr))
}

// streamStall is how long one write of a download may block before the
// connection is cut: a stalled browser must not hold a restic run open.
const streamStall = 2 * time.Minute

// streamWriter is the HTTP side of a streamed download (ADR 0008). It sends
// the headers with the first byte - so a failure before any byte is still an
// ordinary problem answer - and gives every write its own deadline in place of
// the server's whole-response WriteTimeout. There is no Content-Length and no
// Range: the size is known only at the end, and a dropped download starts
// again from the beginning. A slow-but-steady reader that never trips
// streamStall is cut by the minimum-rate guard of restores.Service.Download,
// which calls Abort.
type streamWriter struct {
	w       http.ResponseWriter
	rc      *http.ResponseController
	id      string
	started bool
	aborted atomic.Bool
}

// errStreamAborted is what Write returns once Abort was called.
var errStreamAborted = errors.New("adminapi: the download stream was aborted")

// Abort cuts the stream from another goroutine: a write blocked on a browser
// that does not read fails at once (the write deadline moves to now), and every
// later write fails.
func (sw *streamWriter) Abort() {
	sw.aborted.Store(true)
	_ = sw.rc.SetWriteDeadline(time.Now())
}

func (sw *streamWriter) Write(b []byte) (int, error) {
	if sw.aborted.Load() {
		return 0, errStreamAborted
	}
	if !sw.started {
		sw.started = true
		h := sw.w.Header()
		h.Set("Content-Type", "application/zip")
		h.Set("Content-Disposition", `attachment; filename="restore-`+sw.id+`.zip"`)
		h.Set("Cache-Control", "no-store")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Content-Type-Options", "nosniff")
		sw.w.WriteHeader(http.StatusOK)
	}
	_ = sw.rc.SetWriteDeadline(time.Now().Add(streamStall))
	if sw.aborted.Load() {
		// Abort ran while the deadline above was being set: keep it cut.
		_ = sw.rc.SetWriteDeadline(time.Now())
		return 0, errStreamAborted
	}
	return sw.w.Write(b)
}

func (s *Service) mintDownload(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	rr, err := s.Store.GetRestoreRequest(r.Context(), p.Scope, r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	tok, exp, err := s.Restores.MintToken(rr, p.SessionID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, DownloadResponse{DownloadURL: "/api/v1/restores/" + rr.ID + "/download?token=" + tok, ExpiresAt: exp.UTC(), ComputedAt: s.now()})
}

// download is GET /api/v1/restores/{id}/download?token=: it consumes the
// single-use token and streams the zip from the repository straight into the
// response (restores.Service.Download). Nothing is written to disk. A failure
// before the first byte is a problem answer; a failure after it aborts the
// connection, so the browser reports a broken download instead of saving a
// truncated zip as if it were complete.
//
// When every stream slot is in use the token is not spent: an API client gets
// 503 with Retry-After and may retry the same URL; a browser navigation is sent
// back to the restore's page, which says so in plain words (BusyNotice).
func (s *Service) download(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	ctx := r.Context()
	rr, err := s.Store.GetRestoreRequest(ctx, p.Scope, r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	rc := http.NewResponseController(w)
	// The stream outlives the server's read and write timeouts: the read
	// deadline would cancel the request context mid-stream, and the write
	// deadline is replaced by one per write (streamWriter).
	if rc.SetReadDeadline(time.Time{}) != nil || rc.SetWriteDeadline(time.Time{}) != nil {
		// A writer chain that hides the connection: long downloads would be cut
		// by the server's timeouts. Loud, because it is a wiring bug.
		s.log().Warn("restore: the download cannot lift the server timeouts", logx.ID("restore_id", rr.ID), logx.Class("deadline_unsupported"))
	}
	sw := &streamWriter{w: w, rc: rc, id: rr.ID}
	who := restores.Actor{AdminID: p.Admin.ID, Username: p.Admin.Username, IP: httpx.ClientIP(r)}
	_, err = s.Restores.DownloadWithToken(ctx, who, rr, r.URL.Query().Get("token"), p.SessionID, sw)
	if err != nil {
		if sw.started {
			panic(http.ErrAbortHandler) // recorded and logged by Download; the client sees a cut transfer
		}
		_ = rc.SetWriteDeadline(time.Now().Add(time.Minute))
		if errors.Is(err, restores.ErrStreamsBusy) && wantsPage(r) {
			h := w.Header()
			h.Set("Cache-Control", "no-store")
			h.Set("Referrer-Policy", "no-referrer")
			http.Redirect(w, r, "/restores/"+rr.ID+"?notice="+BusyNotice+"&at="+strconv.FormatInt(s.now().Unix(), 10), http.StatusSeeOther)
			return
		}
		var fe *restores.FailedError
		if errors.As(err, &fe) {
			switch fe.Code {
			case adminpath.CodeCustodyCustomerHeld:
				httpx.Problem(w, r, api.CodeCustodyCustomerHeld, 0)
			case adminpath.CodeNotFound, adminpath.CodeRepoMissing:
				httpx.Problem(w, r, api.CodeNotFound, 0)
			case adminpath.CodeUnavailable, adminpath.CodeStorageUnreachable, adminpath.CodeTimeout, adminpath.CodeLocked:
				httpx.Problem(w, r, api.CodeUnavailable, 30*time.Second)
			default:
				httpx.Problem(w, r, api.CodeInternal, 0)
			}
			return
		}
		s.fail(w, r, err)
		return
	}
	if _, err := s.Store.AppendAudit(context.WithoutCancel(ctx), store.AuditEvent{ActorType: "admin", ActorID: p.Admin.ID, ActorLabel: p.Admin.Username, Method: "api",
		SourceIP: httpx.ClientIP(r), Action: "restore.download", ObjectType: "restore", ObjectID: rr.ID, Result: "ok"}); err != nil {
		s.log().Error("restore: cannot audit the download", logx.ID("restore_id", rr.ID), logx.Err(err))
	}
}

// BusyNotice is the notice query value of the restore page (GET
// /restores/{id}?notice=busy&at=<unix seconds>) a browser is sent back to when
// every restore stream is in use; the page shows it for a minute after at.
// Nothing from the request travels in it.
const BusyNotice = "busy"

// wantsPage reports whether the request is a browser navigation (the restore
// page's Download button), which should get a page rather than a problem body.
func wantsPage(r *http.Request) bool {
	return r.Header.Get("Sec-Fetch-Mode") == "navigate" || strings.Contains(r.Header.Get("Accept"), "text/html")
}

// ---- version ----

func (s *Service) version(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	v, _ := s.Store.SchemaVersion(ctx)
	out := VersionResponse{Console: buildinfo.Version, Commit: buildinfo.Commit, BuildDate: buildinfo.Date, Schema: v, ComputedAt: s.now()}
	if s.Backend != nil {
		pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		if pr, err := s.Backend.Ping(pctx); err == nil {
			// adminpath.PingResult has no restic or rclone version today: say so, never guess.
			out.Maintain = &Worker{Reachable: true, Version: pr.Version, MasterKeyLoaded: pr.MasterKeyLoaded,
				ResticVersion: toolUnknown, RcloneVersion: toolUnknown}
		} else {
			out.Maintain = &Worker{}
		}
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}
