package adminapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/httpx"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// The M2 account routes of architecture §5.2 (ticket M2-W1A): the MFA step,
// the caller's own account (/me) and account administration (/admins). The
// logic lives in auth.Manager, shared with the UI; these handlers only decode,
// gate and encode.
const (
	PatternMFATOTP         = "POST /api/v1/auth/mfa/totp"
	PatternMFARecovery     = "POST /api/v1/auth/mfa/recovery"
	PatternMe              = "GET /api/v1/me"
	PatternMePatch         = "PATCH /api/v1/me"
	PatternTOTPBegin       = "POST /api/v1/me/totp/begin"
	PatternTOTPConfirm     = "POST /api/v1/me/totp/confirm"
	PatternRecoveryCodes   = "POST /api/v1/me/recovery-codes"
	PatternSessions        = "GET /api/v1/me/sessions"
	PatternSessionRevoke   = "DELETE /api/v1/me/sessions/{id}"
	PatternTokens          = "GET /api/v1/me/api-tokens"
	PatternTokenCreate     = "POST /api/v1/me/api-tokens"
	PatternTokenRevoke     = "DELETE /api/v1/me/api-tokens/{id}"
	PatternAdmins          = "GET /api/v1/admins"
	PatternAdminCreate     = "POST /api/v1/admins"
	PatternAdmin           = "GET /api/v1/admins/{id}"
	PatternAdminPatch      = "PATCH /api/v1/admins/{id}"
	PatternAdminDelete     = "DELETE /api/v1/admins/{id}"
	PatternAdminDisable    = "POST /api/v1/admins/{id}/disable"
	PatternAdminEnable     = "POST /api/v1/admins/{id}/enable"
	PatternAdminResetMFA   = "POST /api/v1/admins/{id}/reset-mfa"
	PatternAdminResetPassw = "POST /api/v1/admins/{id}/reset-password"
)

// AccountPatterns lists the account routes with the minimum role each needs
// (architecture §5.2 "who"): PENDING (a password-only session), SELF (any
// signed-in account), AD, OW.
func AccountPatterns() map[string]string {
	return map[string]string{
		PatternMFATOTP: "PENDING", PatternMFARecovery: "PENDING",
		PatternMe: "SELF", PatternMePatch: "SELF", PatternTOTPBegin: "SELF", PatternTOTPConfirm: "SELF", PatternRecoveryCodes: "SELF",
		PatternSessions: "SELF", PatternSessionRevoke: "SELF", PatternTokens: "SELF", PatternTokenCreate: "SELF", PatternTokenRevoke: "SELF",
		PatternAdmins: "AD", PatternAdmin: "AD",
		PatternAdminCreate: "OW", PatternAdminPatch: "OW", PatternAdminDelete: "OW", PatternAdminDisable: "OW", PatternAdminEnable: "OW",
		PatternAdminResetMFA: "OW", PatternAdminResetPassw: "OW",
	}
}

func accountPatternList() []string {
	return []string{PatternMFATOTP, PatternMFARecovery, PatternMe, PatternMePatch, PatternTOTPBegin, PatternTOTPConfirm, PatternRecoveryCodes,
		PatternSessions, PatternSessionRevoke, PatternTokens, PatternTokenCreate, PatternTokenRevoke, PatternAdmins, PatternAdminCreate,
		PatternAdmin, PatternAdminPatch, PatternAdminDelete, PatternAdminDisable, PatternAdminEnable, PatternAdminResetMFA, PatternAdminResetPassw}
}

func (s *Service) registerAccounts(r httpx.Registrar) {
	m := s.Auth
	self := func(h http.HandlerFunc) http.Handler { return m.Require(auth.RoleReadOnly, auth.ModeAPI, h) }
	session := func(h http.HandlerFunc) http.Handler { return m.RequireSession(auth.RoleReadOnly, auth.ModeAPI, h) }
	ad := func(h http.HandlerFunc) http.Handler { return m.Require(auth.RoleAdmin, auth.ModeAPI, h) }
	// Every /admins change creates, resets or re-scopes credentials of another
	// account: a signed-in owner session only, never an API token of any scope.
	ow := func(h http.HandlerFunc) http.Handler { return m.RequireSession(auth.RoleOwner, auth.ModeAPI, h) }
	pending := func(h http.HandlerFunc) http.Handler { return m.RequirePending(auth.ModeAPI, h) }
	enroll := func(h http.HandlerFunc) http.Handler { return m.RequireEnrollment(auth.ModeAPI, h) }

	r.Handle(PatternMFATOTP, pending(s.mfaTOTP))
	r.Handle(PatternMFARecovery, pending(s.mfaRecovery))
	r.Handle(PatternMe, self(s.me))
	r.Handle(PatternMePatch, session(s.patchMe))
	r.Handle(PatternTOTPBegin, enroll(s.totpBegin))
	r.Handle(PatternTOTPConfirm, enroll(s.totpConfirm))
	r.Handle(PatternRecoveryCodes, session(s.recoveryCodes))
	r.Handle(PatternSessions, self(s.listSessions))
	// Credential management (sessions, tokens, password, TOTP, recovery codes)
	// is for a signed-in session only: an API token of any scope is refused.
	r.Handle(PatternSessionRevoke, session(s.revokeSession))
	r.Handle(PatternTokens, self(s.listTokens))
	r.Handle(PatternTokenCreate, session(s.createToken))
	r.Handle(PatternTokenRevoke, session(s.revokeToken))
	r.Handle(PatternAdmins, ad(s.listAdmins))
	r.Handle(PatternAdmin, ad(s.getAdmin))
	r.Handle(PatternAdminCreate, ow(s.createAdmin))
	r.Handle(PatternAdminPatch, ow(s.patchAdmin))
	r.Handle(PatternAdminDelete, ow(s.deleteAdmin))
	r.Handle(PatternAdminDisable, ow(s.disableAdmin))
	r.Handle(PatternAdminEnable, ow(s.enableAdmin))
	r.Handle(PatternAdminResetMFA, ow(s.resetMFA))
	r.Handle(PatternAdminResetPassw, ow(s.resetPassword))
}

// ---- JSON shapes ----

// CodeRequest carries a TOTP or recovery code.
type CodeRequest struct {
	Code string `json:"code"`
}

// MeJSON is GET /api/v1/me.
type MeJSON struct {
	ID                 string     `json:"id"`
	Username           string     `json:"username"`
	Email              string     `json:"email"`
	Role               string     `json:"role"`
	EffectiveRole      string     `json:"effective_role"`
	CustomerScope      []string   `json:"customer_scope"`
	TOTPEnrolled       bool       `json:"totp_enrolled"`
	TOTPEnrolledAt     *time.Time `json:"totp_enrolled_at,omitempty"`
	RecoveryCodesLeft  int        `json:"recovery_codes_left"`
	MustChangePassword bool       `json:"must_change_password"`
	LastLoginAt        *time.Time `json:"last_login_at,omitempty"`
	ViaToken           bool       `json:"via_token"`
	TokenName          string     `json:"token_name,omitempty"`
	ComputedAt         time.Time  `json:"computed_at"`
}

// PatchMeRequest is PATCH /api/v1/me: a new e-mail, a new password (with the
// current one), or both.
type PatchMeRequest struct {
	Email           *string `json:"email,omitempty"`
	CurrentPassword string  `json:"current_password,omitempty"`
	NewPassword     *string `json:"new_password,omitempty"`
}

// TOTPBeginResponse is POST /api/v1/me/totp/begin. The secret and the URI are
// shown once and never logged.
type TOTPBeginResponse struct {
	Secret     string    `json:"secret"`
	OTPAuthURI string    `json:"otpauth_uri"`
	ExpiresAt  time.Time `json:"expires_at"`
	ComputedAt time.Time `json:"computed_at"`
}

// TOTPConfirmRequest is POST /api/v1/me/totp/confirm. CurrentCode (a code of
// the old authenticator, or a recovery code) is required when re-enrolling.
type TOTPConfirmRequest struct {
	Code        string `json:"code"`
	CurrentCode string `json:"current_code,omitempty"`
}

// RecoveryCodesResponse carries recovery codes, shown once. A confirm that
// completed a login also carries the new session's CSRF token (the cookie
// changed).
type RecoveryCodesResponse struct {
	RecoveryCodes []string   `json:"recovery_codes"`
	LoginComplete bool       `json:"login_complete,omitempty"`
	CSRFToken     string     `json:"csrf_token,omitempty"`
	Admin         *AdminJSON `json:"admin,omitempty"`
	ComputedAt    time.Time  `json:"computed_at"`
}

// SessionJSON is one of the caller's sessions.
type SessionJSON struct {
	ID                string    `json:"id"`
	Current           bool      `json:"current"`
	SourceIP          string    `json:"source_ip,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
	LastSeenAt        time.Time `json:"last_seen_at"`
	ExpiresAt         time.Time `json:"expires_at"`
	AbsoluteExpiresAt time.Time `json:"absolute_expires_at"`
}

// SessionsResponse is GET /api/v1/me/sessions.
type SessionsResponse struct {
	Sessions   []SessionJSON `json:"sessions"`
	ComputedAt time.Time     `json:"computed_at"`
}

// TokenJSON is an API token as listed: never the secret.
type TokenJSON struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	Scopes     []string   `json:"scopes"`
	State      string     `json:"state"` // active | expired | revoked
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

// TokensResponse is GET /api/v1/me/api-tokens.
type TokensResponse struct {
	Tokens     []TokenJSON `json:"tokens"`
	ComputedAt time.Time   `json:"computed_at"`
}

// TokenCreateRequest is POST /api/v1/me/api-tokens. Scope is "read" or
// "full"; Days one of 7, 30, 90, 365.
type TokenCreateRequest struct {
	Name  string `json:"name"`
	Scope string `json:"scope"`
	Days  int    `json:"days"`
}

// TokenCreateResponse carries the secret, shown once.
type TokenCreateResponse struct {
	Token      string    `json:"token"`
	APIToken   TokenJSON `json:"api_token"`
	ComputedAt time.Time `json:"computed_at"`
}

// AccountJSON is an account in /api/v1/admins.
type AccountJSON struct {
	ID            string     `json:"id"`
	Username      string     `json:"username"`
	Email         string     `json:"email"`
	Role          string     `json:"role"`
	CustomerScope []string   `json:"customer_scope"`
	State         string     `json:"state"` // active | disabled | locked
	TOTPEnrolled  bool       `json:"totp_enrolled"`
	LastLoginAt   *time.Time `json:"last_login_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
}

// AccountsResponse is GET /api/v1/admins.
type AccountsResponse struct {
	Admins     []AccountJSON `json:"admins"`
	ComputedAt time.Time     `json:"computed_at"`
}

// AccountRequest is POST /api/v1/admins (all fields) and PATCH
// /api/v1/admins/{id} (role and customer_scope). customer_scope null or absent
// means all customers.
type AccountRequest struct {
	Username      string   `json:"username,omitempty"`
	Email         string   `json:"email,omitempty"`
	Role          string   `json:"role"`
	CustomerScope []string `json:"customer_scope"`
}

// ResetLinkResponse carries a one-time password-reset link, shown once.
type ResetLinkResponse struct {
	Admin      *AccountJSON `json:"admin,omitempty"`
	ResetPath  string       `json:"reset_path"`
	ResetURL   string       `json:"reset_url"`
	ExpiresAt  time.Time    `json:"expires_at"`
	ComputedAt time.Time    `json:"computed_at"`
}

// ReasonRequest is the optional body of disable and reset-mfa.
type ReasonRequest struct {
	Reason string `json:"reason"`
}

// ---- helpers ----

// decodeBody decodes a JSON body strictly (unknown fields refused, 16 KiB cap).
// An empty body leaves v zero when optional is true.
func decodeBody(w http.ResponseWriter, r *http.Request, v any, optional bool) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if optional && errors.Is(err, io.EOF) {
			return true
		}
		httpx.Problem(w, r, api.CodeInvalidRequest, 0)
		return false
	}
	return true
}

// authFail maps the account errors to problem codes; the rest goes to fail.
func (s *Service) authFail(w http.ResponseWriter, r *http.Request, err error) {
	var rl *auth.RateLimitedError
	switch {
	case errors.As(err, &rl):
		httpx.Problem(w, r, api.CodeRateLimited, rl.RetryAfter)
	case errors.Is(err, auth.ErrBadCode), errors.Is(err, auth.ErrBadPassword), errors.Is(err, auth.ErrNoSession):
		httpx.Problem(w, r, api.CodeUnauthorized, 0)
	case errors.Is(err, auth.ErrMasterKey):
		httpx.Problem(w, r, api.CodeMasterKeyUnavailable, 30*time.Second)
	case errors.Is(err, auth.ErrForbidden), errors.Is(err, auth.ErrSessionRequired):
		httpx.Problem(w, r, api.CodeForbidden, 0)
	case errors.Is(err, auth.ErrNoEnrollment), errors.Is(err, auth.ErrAlreadyEnrolled), errors.Is(err, auth.ErrNotEnrolled), errors.Is(err, auth.ErrSelf):
		httpx.Problem(w, r, api.CodeConflict, 0)
	case errors.Is(err, auth.ErrPasswordPolicy), errors.Is(err, auth.ErrBadEmail), errors.Is(err, auth.ErrTokenInvalid), errors.Is(err, auth.ErrAccountInvalid):
		httpx.Problem(w, r, api.CodeInvalidRequest, 0)
	case errors.Is(err, auth.ErrResetInvalid):
		httpx.Problem(w, r, api.CodeNotFound, 0)
	default:
		s.fail(w, r, err)
	}
}

func principalOf(r *http.Request) *auth.Principal {
	p, _ := auth.FromContext(r.Context())
	return p
}

func clientIP(r *http.Request) string { return httpx.ClientIP(r) }

func adminJSONOf(a store.Admin) *AdminJSON {
	return &AdminJSON{ID: a.ID, Username: a.Username, Role: a.Role}
}

func (s *Service) accountJSON(a store.Admin) AccountJSON {
	st := "active"
	switch {
	case a.DisabledAt != nil:
		st = "disabled"
	case a.LockedUntil != nil && a.LockedUntil.After(s.now()):
		st = "locked"
	}
	scope := a.CustomerScope
	return AccountJSON{ID: a.ID, Username: a.Username, Email: a.Email, Role: a.Role, CustomerScope: scope, State: st,
		TOTPEnrolled: a.TOTPEnrolledAt != nil, LastLoginAt: a.LastLoginAt, CreatedAt: a.CreatedAt}
}

func (s *Service) tokenJSON(t store.APIToken) TokenJSON {
	st := "active"
	switch {
	case t.RevokedAt != nil:
		st = "revoked"
	case t.ExpiresAt != nil && !t.ExpiresAt.After(s.now()):
		st = "expired"
	}
	return TokenJSON{ID: t.ID, Name: t.Name, Prefix: t.TokenPrefix, Scopes: t.Scopes, State: st, CreatedAt: t.CreatedAt,
		LastUsedAt: t.LastUsedAt, ExpiresAt: t.ExpiresAt, RevokedAt: t.RevokedAt}
}

// resetURL is the link as the owner's browser reaches this console.
func resetURL(r *http.Request, path string) string {
	scheme := "http"
	if auth.RequestIsTLS(r) {
		scheme = "https"
	}
	return scheme + "://" + r.Host + path
}

// ---- MFA step ----

func (s *Service) mfaTOTP(w http.ResponseWriter, r *http.Request) {
	var req CodeRequest
	if !decodeBody(w, r, &req, false) {
		return
	}
	res, err := s.Auth.VerifyTOTPLogin(r.Context(), principalOf(r), req.Code, "api", clientIP(r), r.UserAgent())
	if err != nil {
		s.authFail(w, r, err)
		return
	}
	s.Auth.SetCookies(w, r, res)
	httpx.WriteJSON(w, http.StatusOK, LoginResponse{CSRFToken: res.CSRF, ComputedAt: s.now(), Admin: *adminJSONOf(res.Admin)})
}

func (s *Service) mfaRecovery(w http.ResponseWriter, r *http.Request) {
	var req CodeRequest
	if !decodeBody(w, r, &req, false) {
		return
	}
	res, left, err := s.Auth.VerifyRecoveryLogin(r.Context(), principalOf(r), req.Code, "api", clientIP(r), r.UserAgent())
	if err != nil {
		s.authFail(w, r, err)
		return
	}
	s.Auth.SetCookies(w, r, res)
	httpx.WriteJSON(w, http.StatusOK, LoginResponse{CSRFToken: res.CSRF, ComputedAt: s.now(), Admin: *adminJSONOf(res.Admin), RecoveryCodesLeft: &left})
}

// ---- /me ----

func (s *Service) me(w http.ResponseWriter, r *http.Request) {
	p := principalOf(r)
	a, err := s.Store.GetAdmin(r.Context(), p.Admin.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	st, err := s.Auth.MFAStatusOf(r.Context(), a.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, MeJSON{ID: a.ID, Username: a.Username, Email: a.Email, Role: a.Role, EffectiveRole: p.Role.String(),
		CustomerScope: a.CustomerScope, TOTPEnrolled: st.Enrolled, TOTPEnrolledAt: st.EnrolledAt, RecoveryCodesLeft: st.RecoveryLeft,
		MustChangePassword: a.MustChangePassword, LastLoginAt: a.LastLoginAt, ViaToken: p.ViaToken(), TokenName: p.TokenName, ComputedAt: s.now()})
}

func (s *Service) patchMe(w http.ResponseWriter, r *http.Request) {
	var req PatchMeRequest
	if !decodeBody(w, r, &req, false) {
		return
	}
	if req.Email == nil && req.NewPassword == nil {
		httpx.Problem(w, r, api.CodeInvalidRequest, 0)
		return
	}
	// All or nothing: auth validates every field before one transaction applies them.
	if err := s.Auth.UpdateSelf(r.Context(), principalOf(r), auth.SelfUpdate{CurrentPassword: req.CurrentPassword, NewPassword: req.NewPassword,
		Email: req.Email}, "api", clientIP(r)); err != nil {
		s.authFail(w, r, err)
		return
	}
	s.me(w, r)
}

func (s *Service) totpBegin(w http.ResponseWriter, r *http.Request) {
	en, err := s.Auth.BeginTOTP(r.Context(), principalOf(r))
	if err != nil {
		s.authFail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, TOTPBeginResponse{Secret: en.Secret, OTPAuthURI: en.URI, ExpiresAt: en.Expires, ComputedAt: s.now()})
}

func (s *Service) totpConfirm(w http.ResponseWriter, r *http.Request) {
	var req TOTPConfirmRequest
	if !decodeBody(w, r, &req, false) {
		return
	}
	codes, res, err := s.Auth.ConfirmTOTP(r.Context(), principalOf(r), req.Code, req.CurrentCode, "api", clientIP(r), r.UserAgent())
	if err != nil {
		s.authFail(w, r, err)
		return
	}
	out := RecoveryCodesResponse{RecoveryCodes: codes, ComputedAt: s.now()}
	if res != nil {
		s.Auth.SetCookies(w, r, *res)
		out.LoginComplete, out.CSRFToken, out.Admin = true, res.CSRF, adminJSONOf(res.Admin)
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (s *Service) recoveryCodes(w http.ResponseWriter, r *http.Request) {
	var req CodeRequest
	if !decodeBody(w, r, &req, false) {
		return
	}
	codes, err := s.Auth.RegenerateRecoveryCodes(r.Context(), principalOf(r), req.Code, "api", clientIP(r))
	if err != nil {
		s.authFail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, RecoveryCodesResponse{RecoveryCodes: codes, ComputedAt: s.now()})
}

func (s *Service) listSessions(w http.ResponseWriter, r *http.Request) {
	p := principalOf(r)
	ss, err := s.Auth.ListSessions(r.Context(), p.Admin.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := SessionsResponse{Sessions: []SessionJSON{}, ComputedAt: s.now()}
	for _, x := range ss {
		out.Sessions = append(out.Sessions, SessionJSON{ID: x.ID, Current: x.ID == p.SessionID, SourceIP: x.SourceIP, CreatedAt: x.CreatedAt,
			LastSeenAt: x.LastSeenAt, ExpiresAt: x.ExpiresAt, AbsoluteExpiresAt: x.AbsoluteExpiresAt})
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (s *Service) revokeSession(w http.ResponseWriter, r *http.Request) {
	p := principalOf(r)
	id := r.PathValue("id")
	if err := s.Auth.RevokeSession(r.Context(), p, id, "api", clientIP(r)); err != nil {
		s.authFail(w, r, err)
		return
	}
	if id == p.SessionID {
		auth.ClearCookies(w, r)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) listTokens(w http.ResponseWriter, r *http.Request) {
	ts, err := s.Auth.ListAPITokens(r.Context(), principalOf(r).Admin.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := TokensResponse{Tokens: []TokenJSON{}, ComputedAt: s.now()}
	for _, t := range ts {
		out.Tokens = append(out.Tokens, s.tokenJSON(t))
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (s *Service) createToken(w http.ResponseWriter, r *http.Request) {
	var req TokenCreateRequest
	if !decodeBody(w, r, &req, false) {
		return
	}
	secret, t, err := s.Auth.CreateAPIToken(r.Context(), principalOf(r), req.Name, req.Scope, req.Days, "api", clientIP(r))
	if err != nil {
		s.authFail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, TokenCreateResponse{Token: secret, APIToken: s.tokenJSON(t), ComputedAt: s.now()})
}

func (s *Service) revokeToken(w http.ResponseWriter, r *http.Request) {
	if err := s.Auth.RevokeAPIToken(r.Context(), principalOf(r), r.PathValue("id"), "api", clientIP(r)); err != nil {
		s.authFail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- /admins ----

func (s *Service) listAdmins(w http.ResponseWriter, r *http.Request) {
	as, err := s.Auth.ListAccounts(r.Context(), principalOf(r))
	if err != nil {
		s.authFail(w, r, err)
		return
	}
	out := AccountsResponse{Admins: []AccountJSON{}, ComputedAt: s.now()}
	for _, a := range as {
		out.Admins = append(out.Admins, s.accountJSON(a))
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (s *Service) getAdmin(w http.ResponseWriter, r *http.Request) {
	a, err := s.Auth.GetAccount(r.Context(), principalOf(r), r.PathValue("id"))
	if err != nil {
		s.authFail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, s.accountJSON(a))
}

func (s *Service) createAdmin(w http.ResponseWriter, r *http.Request) {
	var req AccountRequest
	if !decodeBody(w, r, &req, false) {
		return
	}
	a, link, err := s.Auth.CreateAccount(r.Context(), principalOf(r), auth.AccountInput{Username: req.Username, Email: req.Email, Role: req.Role, Scope: req.CustomerScope}, "api", clientIP(r))
	if err != nil {
		s.authFail(w, r, err)
		return
	}
	aj := s.accountJSON(a)
	httpx.WriteJSON(w, http.StatusCreated, ResetLinkResponse{Admin: &aj, ResetPath: link.Path, ResetURL: resetURL(r, link.Path), ExpiresAt: link.Expires, ComputedAt: s.now()})
}

func (s *Service) patchAdmin(w http.ResponseWriter, r *http.Request) {
	var req AccountRequest
	if !decodeBody(w, r, &req, false) {
		return
	}
	if req.Username != "" || req.Email != "" {
		httpx.Problem(w, r, api.CodeInvalidRequest, 0)
		return
	}
	a, err := s.Auth.UpdateAccount(r.Context(), principalOf(r), r.PathValue("id"), req.Role, req.CustomerScope, "api", clientIP(r))
	if err != nil {
		s.authFail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, s.accountJSON(a))
}

func (s *Service) deleteAdmin(w http.ResponseWriter, r *http.Request) {
	if err := s.Auth.DeleteAccount(r.Context(), principalOf(r), r.PathValue("id"), "api", clientIP(r)); err != nil {
		s.authFail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) disableAdmin(w http.ResponseWriter, r *http.Request) {
	var req ReasonRequest
	if !decodeBody(w, r, &req, true) {
		return
	}
	if err := s.Auth.DisableAccount(r.Context(), principalOf(r), r.PathValue("id"), req.Reason, "api", clientIP(r)); err != nil {
		s.authFail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) enableAdmin(w http.ResponseWriter, r *http.Request) {
	if err := s.Auth.EnableAccount(r.Context(), principalOf(r), r.PathValue("id"), "api", clientIP(r)); err != nil {
		s.authFail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) resetMFA(w http.ResponseWriter, r *http.Request) {
	var req ReasonRequest
	if !decodeBody(w, r, &req, true) {
		return
	}
	if err := s.Auth.ResetAccountMFA(r.Context(), principalOf(r), r.PathValue("id"), req.Reason, "api", clientIP(r)); err != nil {
		s.authFail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) resetPassword(w http.ResponseWriter, r *http.Request) {
	link, err := s.Auth.ResetAccountPassword(r.Context(), principalOf(r), r.PathValue("id"), "api", clientIP(r))
	if err != nil {
		s.authFail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, ResetLinkResponse{ResetPath: link.Path, ResetURL: resetURL(r, link.Path), ExpiresAt: link.Expires, ComputedAt: s.now()})
}
