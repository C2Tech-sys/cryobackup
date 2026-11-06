package ui

import (
	"context"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/httpx"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
)

// The MFA step and the password-reset form (ticket M2-W1A, screen S01): the
// bare-shell pages a browser sees between the password and the console. The
// gates (auth.RequirePending) keep a pending session here; nothing else of the
// console answers it.

// registerAccounts adds the M2-W1A routes: the MFA step, the reset link, /account
// (SELF) and /settings/accounts (AD to view, OW to change). self, ad and ow are
// the role wrappers of Register.
func (s *Service) registerAccounts(r httpx.Registrar, self, ad, ow func(http.HandlerFunc) http.Handler) {
	pending := func(h http.HandlerFunc) http.Handler { return s.Auth.RequirePending(auth.ModeUI, h) }
	r.Handle("GET /login/mfa", pending(s.mfaPage))
	r.Handle("POST /login/mfa", pending(s.mfaPost))
	r.Handle("POST /login/mfa/recovery", pending(s.mfaRecoveryPost))
	r.Handle("POST /login/mfa/enroll", pending(s.mfaEnrollPost))
	r.Handle("POST /login/mfa/cancel", pending(s.mfaCancel))
	r.Handle("GET /login/reset", http.HandlerFunc(s.resetPage))
	r.Handle("POST /login/reset", http.HandlerFunc(s.resetPost))

	r.Handle("GET /account", self(s.accountPage))
	r.Handle("POST /ui/account/password", self(s.accountPassword))
	r.Handle("POST /ui/account/email", self(s.accountEmail))
	r.Handle("POST /ui/account/totp", self(s.accountTOTPBegin))
	r.Handle("POST /ui/account/totp/confirm", self(s.accountTOTPConfirm))
	r.Handle("POST /ui/account/recovery-codes", self(s.accountRecoveryCodes))
	r.Handle("POST /ui/account/sessions/{id}/revoke", self(s.accountSessionRevoke))
	r.Handle("POST /ui/account/tokens", self(s.accountTokenCreate))
	r.Handle("POST /ui/account/tokens/{id}/revoke", self(s.accountTokenRevoke))

	r.Handle("GET /settings/accounts", ad(s.accountsPage))
	r.Handle("POST /ui/accounts", ow(s.accountsCreate))
	r.Handle("POST /ui/accounts/{id}/role", ow(s.accountsRole))
	r.Handle("POST /ui/accounts/{id}/disable", ow(s.accountsDisable))
	r.Handle("POST /ui/accounts/{id}/enable", ow(s.accountsEnable))
	r.Handle("POST /ui/accounts/{id}/delete", ow(s.accountsDelete))
	r.Handle("POST /ui/accounts/{id}/reset-mfa", ow(s.accountsResetMFA))
	r.Handle("POST /ui/accounts/{id}/reset-password", ow(s.accountsResetPassword))
}

// AccountUIRoutes lists the routes registerAccounts adds with the minimum role
// each needs: ANON, PENDING, SELF, AD or OW (the role-matrix tests walk it).
func AccountUIRoutes() map[string]string {
	return map[string]string{
		"GET /login/mfa": "PENDING", "POST /login/mfa": "PENDING", "POST /login/mfa/recovery": "PENDING",
		"POST /login/mfa/enroll": "PENDING", "POST /login/mfa/cancel": "PENDING",
		"GET /login/reset": "ANON", "POST /login/reset": "ANON",
		"GET /account": "SELF", "POST /ui/account/password": "SELF", "POST /ui/account/email": "SELF", "POST /ui/account/totp": "SELF",
		"POST /ui/account/totp/confirm": "SELF", "POST /ui/account/recovery-codes": "SELF", "POST /ui/account/sessions/{id}/revoke": "SELF",
		"POST /ui/account/tokens": "SELF", "POST /ui/account/tokens/{id}/revoke": "SELF",
		"GET /settings/accounts": "AD",
		"POST /ui/accounts":      "OW", "POST /ui/accounts/{id}/role": "OW", "POST /ui/accounts/{id}/disable": "OW", "POST /ui/accounts/{id}/enable": "OW",
		"POST /ui/accounts/{id}/delete": "OW", "POST /ui/accounts/{id}/reset-mfa": "OW", "POST /ui/accounts/{id}/reset-password": "OW",
	}
}

func accountUIPatterns() []string {
	out := make([]string, 0, 24)
	for p := range AccountUIRoutes() {
		out = append(out, p)
	}
	return out
}

// Note is an inline banner (components §9.3): class ok | info | warn | crit.
type Note struct {
	Class, Title, Text string
}

// When is a timestamp as the account screens show it: in the operator's zone,
// 12-hour clock, with the UTC instant for <time datetime>.
type When struct {
	Text, ISO, Ago string
	// Short is Text without the year ("Tue 20 Oct, 1:42 PM"), for narrow cells.
	Short string
}

func (s *Service) when(ctx context.Context, t *time.Time) When {
	if t == nil || t.IsZero() {
		return When{}
	}
	c := s.fc(ctx)
	return When{Text: t.In(c.loc()).Format("Mon 2 Jan 2006, 3:04 PM"), Short: t.In(c.loc()).Format("Mon 2 Jan, 3:04 PM"), ISO: t.UTC().Format(time.RFC3339), Ago: ageLong(c.Now.Sub(*t))}
}

// authNote turns an auth or store error into the fixed sentence a form shows.
// It never echoes a code, a password or a token.
func authNote(err error) Note {
	var rl *auth.RateLimitedError
	switch {
	case errors.As(err, &rl):
		return Note{"crit", "Too many attempts.", "Wait a minute and try again."}
	case errors.Is(err, auth.ErrBadCode):
		return Note{"crit", "Code incorrect or expired.", "Enter the current code from your authenticator app. A code works once: if you just used it, wait for the next one."}
	case errors.Is(err, auth.ErrBadPassword):
		return Note{"crit", "Password not changed.", "The current password is not correct."}
	case errors.Is(err, auth.ErrPasswordPolicy):
		return Note{"crit", "Password not changed.", "Use 12 to 256 characters. A passphrase of several words is easiest to remember."}
	case errors.Is(err, auth.ErrBadEmail):
		return Note{"crit", "E-mail not changed.", "That is not an e-mail address."}
	case errors.Is(err, auth.ErrMasterKey):
		return Note{"crit", "The master key is not loaded.", "TOTP secrets are encrypted under the console master key, so no authenticator can be enrolled or checked until an operator loads it (banner C12). Recovery codes still work."}
	case errors.Is(err, auth.ErrNoEnrollment):
		return Note{"crit", "The enrollment expired.", "Start again: a new QR code is shown."}
	case errors.Is(err, auth.ErrNotEnrolled):
		return Note{"crit", "No authenticator is enrolled.", "Enroll one first."}
	case errors.Is(err, auth.ErrTokenInvalid):
		return Note{"crit", "Token not created.", "Give it a name of up to 100 characters, a scope and a lifetime."}
	case errors.Is(err, auth.ErrAccountInvalid):
		return Note{"crit", "Account not saved.", "Check the username (letters, digits, dot, dash, underscore, @), the e-mail and the role. Only operator and read-only accounts can be limited to customers, and at least one customer must be chosen."}
	case errors.Is(err, auth.ErrSelf):
		return Note{"crit", "Not on your own account.", "Use My account to change your own password and authenticator; another owner changes your role."}
	case errors.Is(err, store.ErrLastOwner):
		return Note{"crit", "The last owner stays.", "The console must keep at least one enabled owner. Make another account an owner first."}
	case errors.Is(err, store.ErrConflict):
		return Note{"crit", "Not saved: it conflicts with what exists.", "A username or token name is already in use, or the account is disabled."}
	case errors.Is(err, store.ErrNotFound):
		return Note{"crit", "Not found.", "It was removed or never existed. Reload the page."}
	case errors.Is(err, auth.ErrForbidden), errors.Is(err, auth.ErrSessionRequired):
		return Note{"crit", "Not allowed.", "Your role does not permit this."}
	}
	return Note{"crit", "The change was not made.", "The console could not complete it (nothing was changed). Try again shortly."}
}

func noteStatus(err error) int {
	var rl *auth.RateLimitedError
	switch {
	case errors.As(err, &rl):
		return http.StatusTooManyRequests
	case errors.Is(err, auth.ErrBadCode), errors.Is(err, auth.ErrBadPassword):
		return http.StatusUnauthorized
	case errors.Is(err, auth.ErrMasterKey):
		return http.StatusServiceUnavailable
	case errors.Is(err, store.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, store.ErrConflict), errors.Is(err, auth.ErrSelf), errors.Is(err, auth.ErrNoEnrollment):
		return http.StatusConflict
	case errors.Is(err, auth.ErrForbidden), errors.Is(err, auth.ErrSessionRequired):
		return http.StatusForbidden
	case errors.Is(err, auth.ErrPasswordPolicy), errors.Is(err, auth.ErrBadEmail), errors.Is(err, auth.ErrTokenInvalid), errors.Is(err, auth.ErrAccountInvalid), errors.Is(err, auth.ErrNotEnrolled):
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

// logUnexpected logs an error the user sees as the generic failure. Classes
// only: no code, password, token or path.
func (s *Service) logUnexpected(what string, err error) {
	if noteStatus(err) == http.StatusInternalServerError {
		s.log().Error(what+" failed", logx.Err(err))
	}
}

// ---- MFA step (bare shell) ----

type mfaData struct {
	Title    string
	Fixtures bool
	Mode     string // verify | enroll | codes
	Username string
	CSRF     string
	Error    *Note
	Secret   string
	QR       template.HTML
	Expires  string
	Codes    []string
	CodesTxt string // the codes as a text file body, for the download link
	MasterOK bool
}

func (s *Service) mfaBase(p *auth.Principal) mfaData {
	return mfaData{Title: "Verify it is you", Fixtures: s.Fixtures, Username: p.Admin.Username, CSRF: p.CSRF, MasterOK: s.Auth.Master.Loaded()}
}

// enrollView begins (or shows again) the session's TOTP enrollment.
func (s *Service) enrollView(ctx context.Context, p *auth.Principal) (secret string, qr template.HTML, expires string, err error) {
	en, err := s.Auth.BeginTOTP(ctx, p)
	if err != nil {
		return "", "", "", err
	}
	qr, err = qrSVG(en.URI, "QR code for your authenticator app. The key below holds the same secret.")
	if err != nil {
		return "", "", "", err
	}
	return en.Secret, qr, s.fc(ctx).clock(en.Expires), nil
}

func (s *Service) renderMFA(w http.ResponseWriter, r *http.Request, p *auth.Principal, status int, errNote *Note) {
	d := s.mfaBase(p)
	d.Error = errNote
	if p.Enrolled() {
		d.Mode = "verify"
	} else {
		d.Mode, d.Title = "enroll", "Set up your authenticator"
		secret, qr, exp, err := s.enrollView(r.Context(), p)
		if err != nil {
			n := authNote(err)
			if d.Error == nil {
				d.Error = &n
			}
			if status == http.StatusOK {
				status = noteStatus(err)
			}
		} else {
			d.Secret, d.QR, d.Expires = secret, qr, exp
		}
	}
	s.render(w, r, status, "mfa", "bare", d)
}

func (s *Service) mfaPage(w http.ResponseWriter, r *http.Request) {
	s.renderMFA(w, r, principal(r), http.StatusOK, nil)
}

func (s *Service) mfaPost(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	res, err := s.Auth.VerifyTOTPLogin(r.Context(), p, r.PostFormValue("code"), "web", httpx.ClientIP(r), r.UserAgent())
	if err != nil {
		s.mfaFailed(w, r, p, err)
		return
	}
	s.setSessionCookies(w, r, res)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Service) mfaRecoveryPost(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	res, left, err := s.Auth.VerifyRecoveryLogin(r.Context(), p, r.PostFormValue("code"), "web", httpx.ClientIP(r), r.UserAgent())
	if err != nil {
		s.mfaFailed(w, r, p, err)
		return
	}
	s.setSessionCookies(w, r, res)
	http.Redirect(w, r, "/account?done=recovery_login&left="+strconv.Itoa(left), http.StatusSeeOther)
}

// mfaFailed re-renders the step with the error, or sends the browser back to
// the password form when the pending session is gone (too many wrong codes,
// expired, disabled account).
func (s *Service) mfaFailed(w http.ResponseWriter, r *http.Request, p *auth.Principal, err error) {
	s.logUnexpected("mfa step", err)
	if errors.Is(err, auth.ErrNoSession) {
		auth.ClearCookies(w, r)
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if c, cerr := r.Cookie(auth.SessionCookie); cerr == nil {
		if np, aerr := s.Auth.PendingPrincipal(r.Context(), c.Value); aerr == nil {
			p = np
		} else {
			// Too many wrong codes ended the pending session: start again.
			auth.ClearCookies(w, r)
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
	}
	n := authNote(err)
	s.renderMFA(w, r, p, noteStatus(err), &n)
}

func (s *Service) mfaEnrollPost(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	codes, res, err := s.Auth.ConfirmTOTP(r.Context(), p, r.PostFormValue("code"), "", "web", httpx.ClientIP(r), r.UserAgent())
	if err != nil {
		s.mfaFailed(w, r, p, err)
		return
	}
	if res != nil {
		s.setSessionCookies(w, r, *res)
	}
	d := s.mfaBase(p)
	d.Mode, d.Title, d.Codes, d.CodesTxt = "codes", "Save your recovery codes", codes, codesFile(p.Admin.Username, codes, s.fc(r.Context()).fullTime(s.now()))
	s.render(w, r, http.StatusOK, "mfa", "bare", d)
}

func (s *Service) mfaCancel(w http.ResponseWriter, r *http.Request) {
	_ = s.Auth.EndSession(r.Context(), principal(r), "web", httpx.ClientIP(r))
	auth.ClearCookies(w, r)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// codesFile is the text of the recovery-codes download (no secret but the
// codes themselves, which the user asked to keep).
func codesFile(username string, codes []string, at string) string {
	var b strings.Builder
	b.WriteString("CryoBackup recovery codes for " + username + "\n")
	b.WriteString("Created " + at + ". Each code works once. Keep them somewhere safe and offline.\n\n")
	for _, c := range codes {
		b.WriteString(c + "\n")
	}
	return b.String()
}

// ---- password reset link (bare shell) ----

type resetData struct {
	Title    string
	Fixtures bool
	Nonce    string
	Token    string
	Username string
	Error    *Note
	Done     bool
	Invalid  bool
}

func (s *Service) renderReset(w http.ResponseWriter, r *http.Request, status int, d resetData) {
	d.Title, d.Fixtures = "Set your password", s.Fixtures
	if !d.Done && !d.Invalid {
		d.Nonce = s.Auth.IssueLoginNonce(w, r)
	}
	s.render(w, r, status, "login_reset", "bare", d)
}

func (s *Service) resetPage(w http.ResponseWriter, r *http.Request) {
	tok := r.URL.Query().Get("token")
	a, err := s.Auth.ResetLinkAccount(r.Context(), tok)
	if err != nil {
		s.renderReset(w, r, http.StatusNotFound, resetData{Invalid: true})
		return
	}
	s.renderReset(w, r, http.StatusOK, resetData{Token: tok, Username: a.Username})
}

func (s *Service) resetPost(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if err := r.ParseForm(); err != nil {
		s.renderReset(w, r, http.StatusBadRequest, resetData{Invalid: true})
		return
	}
	// The same origin rule as the login form (FIX23b): contrary headers refuse,
	// a valid nonce makes up for missing ones.
	if s.Auth.HeadersContradict(r) || (!s.Auth.LoginNonceOK(r) && !s.Auth.LoginOriginOK(r)) {
		s.renderReset(w, r, http.StatusForbidden, resetData{Invalid: true})
		return
	}
	tok := r.PostFormValue("token")
	a, err := s.Auth.ResetLinkAccount(r.Context(), tok)
	if err != nil {
		s.renderReset(w, r, http.StatusNotFound, resetData{Invalid: true})
		return
	}
	pw := r.PostFormValue("password")
	if pw != r.PostFormValue("confirm") {
		s.renderReset(w, r, http.StatusBadRequest, resetData{Token: tok, Username: a.Username,
			Error: &Note{"crit", "Password not set.", "The two passwords differ."}})
		return
	}
	if _, err := s.Auth.CompletePasswordReset(r.Context(), tok, pw, httpx.ClientIP(r)); err != nil {
		s.logUnexpected("password reset", err)
		if errors.Is(err, auth.ErrResetInvalid) {
			s.renderReset(w, r, http.StatusNotFound, resetData{Invalid: true})
			return
		}
		n := authNote(err)
		n.Title = "Password not set."
		s.renderReset(w, r, noteStatus(err), resetData{Token: tok, Username: a.Username, Error: &n})
		return
	}
	auth.ClearLoginNonce(w, r)
	s.renderReset(w, r, http.StatusOK, resetData{Done: true, Username: a.Username})
}

// resetURLFor is the absolute link shown to the owner who issued it.
func resetURLFor(r *http.Request, path string) string {
	scheme := "http"
	if auth.RequestIsTLS(r) {
		scheme = "https"
	}
	return (&url.URL{Scheme: scheme, Host: r.Host}).String() + path
}
