package ui

import (
	"context"
	"errors"
	"html/template"
	"net/http"
	"strconv"
	"strings"

	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/httpx"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// /account (screen S38, SELF): profile, password, the authenticator and its
// recovery codes, signed-in sessions and personal API tokens. Every change is a
// form post to the same auth.Manager operations the /api/v1/me routes use.
// Secrets (a new token, new recovery codes, an enrollment QR) are rendered
// into the POST response once, never into a redirect target, and the page is
// no-store.

// StateWord is a word with a state glyph (components §0.1).
type StateWord struct {
	Class, Text string
}

// AccountPage is the data of /account.
type AccountPage struct {
	Page
	Me        AccountMe
	MFA       AccountMFA
	MFAErr    bool // the authenticator status could not be read (M2-W3A)
	Enroll    *EnrollView
	Codes     []string
	CodesTxt  string
	NewToken  *NewTokenView
	Sessions  []SessionRow
	Tokens    []TokenRow
	Notice    *Note
	Errs      map[string]*Note // per form: password, email, profile, totp, codes, token
	Profile   ProfileForm
	Lifetimes []int
	TokenForm TokenForm
	MasterOK  bool
	SessErr   bool
	TokErr    bool
}

// AccountMe is the profile card. DisplayName is "" when none is set.
type AccountMe struct {
	Username, DisplayName, Email, Role, Scope string
	LastLogin                                 When
	MustChange                                bool
}

// ProfileForm is what the username and display-name inputs hold: the stored
// values, or what was typed when a post failed.
type ProfileForm struct {
	Username, DisplayName string
}

// AccountMFA is the authenticator card.
type AccountMFA struct {
	Enrolled    bool
	Since       When
	Left, Total int
	Low         bool
}

// EnrollView is a TOTP enrollment in progress (QR, manual key).
type EnrollView struct {
	Secret  string
	QR      template.HTML
	Expires string
}

// NewTokenView is a token secret shown once.
type NewTokenView struct {
	Name, Secret string
}

// SessionRow is one signed-in session.
type SessionRow struct {
	ID      string
	Current bool
	IP      string
	Created When
	Seen    When
	Ends    When
}

// TokenRow is one API token (never the secret).
type TokenRow struct {
	ID, Name, Prefix, Scope string
	Created, LastUsed       When
	Expires, Revoked        When
	State                   StateWord
	Active                  bool
}

// TokenForm keeps the create form's values after a failed post.
type TokenForm struct {
	Name, Scope string
	Days        int
}

func roleLabel(role string) string {
	switch role {
	case "owner":
		return "Owner"
	case "admin":
		return "Admin"
	case "operator":
		return "Operator"
	case "read_only":
		return "Read-only"
	}
	return role
}

// scopeLabel names a customer scope with customer names where known.
func scopeLabel(scope []string, names map[string]string) string {
	if scope == nil {
		return "All customers"
	}
	parts := make([]string, 0, len(scope))
	for _, id := range scope {
		if n, ok := names[id]; ok {
			parts = append(parts, n)
		} else {
			parts = append(parts, "removed customer")
		}
	}
	if len(parts) == 0 {
		return "No customers"
	}
	return strings.Join(parts, ", ")
}

func (s *Service) customerNames(r *http.Request, p *auth.Principal) map[string]string {
	out := map[string]string{}
	cs, err := s.Store.ListCustomers(r.Context(), p.Scope)
	if err != nil {
		return out
	}
	for _, c := range cs {
		out[c.ID] = c.Name
	}
	return out
}

var accountNotices = map[string]Note{
	"password":       {"ok", "Password changed.", "Your other sessions were signed out and your API tokens were revoked; this session stays."},
	"email":          {"ok", "E-mail changed.", ""},
	"profile":        {"ok", "Profile saved.", ""},
	"session":        {"ok", "Session signed out.", "It ends on its next request."},
	"token_revoked":  {"ok", "Token revoked.", "It stopped working at once."},
	"recovery_login": {"warn", "You signed in with a recovery code.", "That code is used up. If your authenticator is lost, replace it below; if codes are running low, regenerate them."},
}

// accountData builds /account. errs and extras come from the post that renders it.
func (s *Service) accountData(r *http.Request, p *auth.Principal) (AccountPage, bool) {
	f, err := s.Fleet.Fleet(r.Context(), p.Scope)
	var pg Page
	if err != nil {
		pg = s.fallbackPage(r.Context(), p)
		pg.Title, pg.Nav = "My account", "account"
	} else {
		pg = s.page(r.Context(), p, f, "My account", "account", "/ui/events")
	}
	d := AccountPage{Page: pg, Errs: map[string]*Note{}, Lifetimes: auth.TokenLifetimes, TokenForm: TokenForm{Scope: auth.ScopeRead, Days: 90},
		MasterOK: s.Auth.Master.Loaded()}
	a, err := s.Store.GetAdmin(r.Context(), p.Admin.ID)
	if err != nil {
		return d, false
	}
	d.Me = AccountMe{Username: a.Username, DisplayName: a.DisplayName, Email: a.Email, Role: roleLabel(a.Role), Scope: scopeLabel(a.CustomerScope, s.customerNames(r, p)),
		LastLogin: s.when(r.Context(), a.LastLoginAt), MustChange: a.MustChangePassword}
	d.Profile = ProfileForm{Username: a.Username, DisplayName: a.DisplayName}
	if st, err := s.Auth.MFAStatusOf(r.Context(), a.ID); err == nil {
		d.MFA = AccountMFA{Enrolled: st.Enrolled, Since: s.when(r.Context(), st.EnrolledAt), Left: st.RecoveryLeft, Total: st.RecoveryTotal, Low: st.Enrolled && st.RecoveryLeft <= 3}
	} else {
		d.MFAErr = true // M2-W3A: say so rather than show "NOT SET UP"
	}
	if ss, err := s.Auth.ListSessions(r.Context(), a.ID); err == nil {
		for _, x := range ss {
			created, seen, ends := x.CreatedAt, x.LastSeenAt, x.AbsoluteExpiresAt
			d.Sessions = append(d.Sessions, SessionRow{ID: x.ID, Current: x.ID == p.SessionID, IP: x.SourceIP, Created: s.when(r.Context(), &created), Seen: s.when(r.Context(), &seen), Ends: s.when(r.Context(), &ends)})
		}
	} else {
		d.SessErr = true
	}
	if ts, err := s.Auth.ListAPITokens(r.Context(), a.ID); err == nil {
		for _, t := range ts {
			d.Tokens = append(d.Tokens, s.tokenRow(r.Context(), t))
		}
	} else {
		d.TokErr = true
	}
	if k := r.URL.Query().Get("done"); k != "" {
		if n, ok := accountNotices[k]; ok {
			if k == "recovery_login" {
				if left, err := strconv.Atoi(r.URL.Query().Get("left")); err == nil {
					n.Text = strconv.Itoa(left) + " recovery codes are left. " + n.Text
				}
			}
			d.Notice = &n
		}
	}
	return d, true
}

func (s *Service) tokenRow(ctx context.Context, t store.APIToken) TokenRow {
	row := TokenRow{ID: t.ID, Name: t.Name, Prefix: t.TokenPrefix, Created: s.when(ctx, &t.CreatedAt), LastUsed: s.when(ctx, t.LastUsedAt),
		Expires: s.when(ctx, t.ExpiresAt), Revoked: s.when(ctx, t.RevokedAt)}
	row.Scope = "Read-only"
	for _, sc := range t.Scopes {
		switch sc {
		case auth.ScopeFull:
			row.Scope = "Full (your role)"
		case auth.ScopeStatusRead:
			row.Scope = "Status endpoint only"
		case auth.ScopeMetricsRead:
			row.Scope = "Metrics endpoint only"
		}
	}
	switch {
	case t.RevokedAt != nil:
		row.State = StateWord{"neutral", "REVOKED"}
	case t.ExpiresAt != nil && !t.ExpiresAt.After(s.now()):
		row.State = StateWord{"neutral", "EXPIRED"}
	default:
		row.State, row.Active = StateWord{"ok", "ACTIVE"}, true
	}
	return row
}

func (s *Service) renderAccount(w http.ResponseWriter, r *http.Request, status int, mutate func(*AccountPage)) {
	p := principal(r)
	d, ok := s.accountData(r, p)
	if !ok {
		s.render(w, r, http.StatusServiceUnavailable, "error", "base", errorData(d.Page, 503, "Account unavailable", "Your account could not be loaded. This is an alarm, not an empty page. Try again shortly."))
		return
	}
	if mutate != nil {
		mutate(&d)
	}
	s.render(w, r, status, "account", "base", d)
}

func (s *Service) accountPage(w http.ResponseWriter, r *http.Request) {
	s.renderAccount(w, r, http.StatusOK, nil)
}

// profileNote words the errors the profile form can add to the shared ones in
// authNote, with the status each answers with. The username conflict says
// "taken" (authNote's conflict sentence covers token names and disabled
// accounts too). ok is false for any other error.
func profileNote(err error) (n Note, status int, ok bool) {
	switch {
	case errors.Is(err, auth.ErrBadUsername):
		return Note{"crit", "Profile not saved.", "A username is 1 to 100 letters, digits, dots, dashes, underscores or @, and starts with a letter or digit."}, http.StatusBadRequest, true
	case errors.Is(err, auth.ErrBadDisplayName):
		return Note{"crit", "Profile not saved.", "A display name is at most 100 characters, with no line breaks, control or invisible characters."}, http.StatusBadRequest, true
	case errors.Is(err, auth.ErrDisplayNameTaken):
		return Note{"crit", "Profile not saved.", "That display name is another account's username. Pick one that cannot be mistaken for them."}, http.StatusBadRequest, true
	case errors.Is(err, store.ErrConflict):
		return Note{"crit", "Profile not saved.", "That username is taken."}, http.StatusConflict, true
	}
	return Note{}, 0, false
}

// formErr re-renders /account with a form's error banner.
func (s *Service) formErr(w http.ResponseWriter, r *http.Request, form string, err error, keep func(*AccountPage)) {
	n, status := authNote(err), noteStatus(err)
	handled := false
	if form == "profile" {
		var pn Note
		var ps int
		if pn, ps, handled = profileNote(err); handled {
			n, status = pn, ps
		}
	}
	if !handled {
		s.logUnexpected("account "+form, err) // a refused input is not an incident; a 500 is
	}
	s.renderAccount(w, r, status, func(d *AccountPage) {
		d.Errs[form] = &n
		if keep != nil {
			keep(d)
		}
	})
}

func (s *Service) accountPassword(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	next := r.PostFormValue("new_password")
	if next != r.PostFormValue("confirm_password") {
		s.renderAccount(w, r, http.StatusBadRequest, func(d *AccountPage) {
			d.Errs["password"] = &Note{"crit", "Password not changed.", "The new password and its confirmation differ."}
		})
		return
	}
	if err := s.Auth.ChangePassword(r.Context(), p, r.PostFormValue("current_password"), next, "web", httpx.ClientIP(r)); err != nil {
		s.formErr(w, r, "password", err, nil)
		return
	}
	http.Redirect(w, r, "/account?done=password", http.StatusSeeOther)
}

func (s *Service) accountEmail(w http.ResponseWriter, r *http.Request) {
	if err := s.Auth.ChangeEmail(r.Context(), principal(r), r.PostFormValue("email"), "web", httpx.ClientIP(r)); err != nil {
		s.formErr(w, r, "email", err, nil)
		return
	}
	http.Redirect(w, r, "/account?done=email", http.StatusSeeOther)
}

// accountProfile sets the account's own username and display name. Sessions
// and API tokens stay: they belong to the account, not to its name.
func (s *Service) accountProfile(w http.ResponseWriter, r *http.Request) {
	username, display := r.PostFormValue("username"), r.PostFormValue("display_name")
	if err := s.Auth.ChangeProfile(r.Context(), principal(r), username, display, "web", httpx.ClientIP(r)); err != nil {
		s.formErr(w, r, "profile", err, func(d *AccountPage) { d.Profile = ProfileForm{Username: username, DisplayName: display} })
		return
	}
	http.Redirect(w, r, "/account?done=profile", http.StatusSeeOther)
}

// accountTOTPBegin shows the enrollment card (a new authenticator for an
// account that has one, or the first one).
func (s *Service) accountTOTPBegin(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	secret, qr, exp, err := s.enrollView(r.Context(), p)
	if err != nil {
		s.formErr(w, r, "totp", err, nil)
		return
	}
	s.renderAccount(w, r, http.StatusOK, func(d *AccountPage) { d.Enroll = &EnrollView{Secret: secret, QR: qr, Expires: exp} })
}

func (s *Service) accountTOTPConfirm(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	codes, _, err := s.Auth.ConfirmTOTP(r.Context(), p, r.PostFormValue("code"), r.PostFormValue("current_code"), "web", httpx.ClientIP(r), r.UserAgent())
	if err != nil {
		s.formErr(w, r, "totp", err, func(d *AccountPage) {
			if secret, qr, exp, eerr := s.enrollView(r.Context(), p); eerr == nil {
				d.Enroll = &EnrollView{Secret: secret, QR: qr, Expires: exp}
			}
		})
		return
	}
	s.renderAccount(w, r, http.StatusOK, func(d *AccountPage) {
		d.Codes, d.CodesTxt = codes, codesFile(p.Admin.Username, codes, s.fc(r.Context()).fullTime(s.now()))
		d.Notice = &Note{"ok", "Authenticator saved.", "Codes from the old authenticator no longer work. Save the new recovery codes below: the old ones were replaced."}
	})
}

func (s *Service) accountRecoveryCodes(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	codes, err := s.Auth.RegenerateRecoveryCodes(r.Context(), p, r.PostFormValue("code"), "web", httpx.ClientIP(r))
	if err != nil {
		s.formErr(w, r, "codes", err, nil)
		return
	}
	s.renderAccount(w, r, http.StatusOK, func(d *AccountPage) {
		d.Codes, d.CodesTxt = codes, codesFile(p.Admin.Username, codes, s.fc(r.Context()).fullTime(s.now()))
		d.Notice = &Note{"ok", "New recovery codes.", "The old codes no longer work."}
	})
}

func (s *Service) accountSessionRevoke(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	id := r.PathValue("id")
	if err := s.Auth.RevokeSession(r.Context(), p, id, "web", httpx.ClientIP(r)); err != nil {
		s.formErr(w, r, "sessions", err, nil)
		return
	}
	if id == p.SessionID {
		auth.ClearCookies(w, r)
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/account?done=session", http.StatusSeeOther)
}

func (s *Service) accountTokenCreate(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	days, _ := strconv.Atoi(r.PostFormValue("days"))
	form := TokenForm{Name: r.PostFormValue("name"), Scope: r.PostFormValue("scope"), Days: days}
	secret, t, err := s.Auth.CreateAPIToken(r.Context(), p, form.Name, form.Scope, form.Days, "web", httpx.ClientIP(r))
	if err != nil {
		s.formErr(w, r, "token", err, func(d *AccountPage) { d.TokenForm = form })
		return
	}
	s.renderAccount(w, r, http.StatusOK, func(d *AccountPage) {
		d.NewToken = &NewTokenView{Name: t.Name, Secret: secret}
		d.Notice = &Note{"ok", "Token created.", "Copy it now: it is not shown again."}
	})
}

func (s *Service) accountTokenRevoke(w http.ResponseWriter, r *http.Request) {
	if err := s.Auth.RevokeAPIToken(r.Context(), principal(r), r.PathValue("id"), "web", httpx.ClientIP(r)); err != nil {
		s.formErr(w, r, "tokens", err, nil)
		return
	}
	http.Redirect(w, r, "/account?done=token_revoked", http.StatusSeeOther)
}
