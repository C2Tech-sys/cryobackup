package ui

import (
	"net/http"
	"slices"
	"strings"

	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/httpx"
)

// /settings/accounts (screen S32): every account with its role, customer
// scope, MFA and state. Admins see the list; owners change it. The changes are
// the auth.Manager operations behind /api/v1/admins: create (with a one-time
// password-reset link shown once), role and scope, disable / enable, delete
// (typed confirmation; never the last owner), reset MFA, reset password.

// AccountsPage is the data of /settings/accounts.
type AccountsPage struct {
	Page
	CanEdit   bool
	Rows      []AccountRow
	Customers []CustomerOpt
	Roles     []RoleOpt
	Link      *LinkView
	Notice    *Note
	FormErr   *Note
	Form      AccountForm
	ListErr   bool
	Owners    int
}

// AccountRow is one account in the table.
type AccountRow struct {
	ID, Username, Email, Role, RoleLabel, Scope string
	DisplayName                                 string // shown under the username when set
	MFA                                         StateWord
	State                                       StateWord
	LastLogin                                   When
	Self, Disabled, Scoped                      bool
	Customers                                   []CustomerOpt // the scope picker, with this account's choices checked
	Roles                                       []RoleOpt
	Err                                         *Note
}

// CustomerOpt is a customer in a scope picker.
type CustomerOpt struct {
	ID, Name string
	Checked  bool
}

// RoleOpt is a role in a select.
type RoleOpt struct {
	Value, Label string
	Selected     bool
}

// LinkView is a one-time password-reset link shown to the owner once.
type LinkView struct {
	Username, URL, Expires, What string
}

// AccountForm keeps the create form's values after a failed post.
type AccountForm struct {
	Username, Email, Role string
	Scope                 []string
}

var roleOrder = []string{"owner", "admin", "operator", "read_only"}

func roleOpts(sel string) []RoleOpt {
	out := make([]RoleOpt, 0, len(roleOrder))
	for _, r := range roleOrder {
		out = append(out, RoleOpt{Value: r, Label: roleLabel(r), Selected: r == sel})
	}
	return out
}

var accountsNotices = map[string]Note{
	"role":     {"ok", "Role saved.", "It applies to that account's next request."},
	"disabled": {"ok", "Account disabled.", "Its sessions ended and its API tokens were revoked."},
	"enabled":  {"ok", "Account enabled.", "It can sign in again; any lock was lifted."},
	"deleted":  {"ok", "Account deleted.", "Audit rows that name it keep their username."},
	"mfa":      {"ok", "MFA reset.", "Its sessions ended and its API tokens were revoked. It sets up a new authenticator at its next sign-in."},
}

func (s *Service) accountsData(r *http.Request, p *auth.Principal) AccountsPage {
	f, err := s.Fleet.Fleet(r.Context(), p.Scope)
	var pg Page
	if err != nil {
		pg = s.fallbackPage(r.Context(), p)
		pg.Title, pg.Nav = "Accounts", "accounts"
	} else {
		pg = s.page(r.Context(), p, f, "Accounts", "accounts", "/ui/events")
	}
	d := AccountsPage{Page: pg, CanEdit: p.Role >= auth.RoleOwner, Roles: roleOpts("operator"), Form: AccountForm{Role: "operator"}}
	cs, cerr := s.Store.ListCustomers(r.Context(), p.Scope)
	names := map[string]string{}
	if cerr == nil {
		for _, c := range cs {
			names[c.ID] = c.Name
			d.Customers = append(d.Customers, CustomerOpt{ID: c.ID, Name: c.Name})
		}
	}
	as, err := s.Auth.ListAccounts(r.Context(), p)
	if err != nil {
		d.ListErr = true
		return d
	}
	now := s.now()
	for _, a := range as {
		row := AccountRow{ID: a.ID, Username: a.Username, DisplayName: a.DisplayName, Email: a.Email, Role: a.Role, RoleLabel: roleLabel(a.Role), Scope: scopeLabel(a.CustomerScope, names),
			LastLogin: s.when(r.Context(), a.LastLoginAt), Self: a.ID == p.Admin.ID, Disabled: a.DisabledAt != nil, Scoped: a.CustomerScope != nil, Roles: roleOpts(a.Role)}
		row.MFA = StateWord{"warn", "NOT SET UP"}
		if a.TOTPEnrolledAt != nil {
			row.MFA = StateWord{"ok", "TOTP"}
		}
		switch {
		case a.DisabledAt != nil:
			row.State = StateWord{"neutral", "DISABLED"}
		case a.LockedUntil != nil && a.LockedUntil.After(now):
			row.State = StateWord{"warn", "LOCKED"}
		default:
			row.State = StateWord{"ok", "ACTIVE"}
		}
		if a.Role == "owner" && a.DisabledAt == nil {
			d.Owners++
		}
		for _, c := range d.Customers {
			row.Customers = append(row.Customers, CustomerOpt{ID: c.ID, Name: c.Name, Checked: slices.Contains(a.CustomerScope, c.ID)})
		}
		d.Rows = append(d.Rows, row)
	}
	if k := r.URL.Query().Get("done"); k != "" {
		if n, ok := accountsNotices[k]; ok {
			d.Notice = &n
		}
	}
	return d
}

func (s *Service) renderAccounts(w http.ResponseWriter, r *http.Request, status int, mutate func(*AccountsPage)) {
	d := s.accountsData(r, principal(r))
	if mutate != nil {
		mutate(&d)
	}
	if d.ListErr && status == http.StatusOK {
		status = http.StatusServiceUnavailable
	}
	s.render(w, r, status, "settings_accounts", "base", d)
}

func (s *Service) accountsPage(w http.ResponseWriter, r *http.Request) {
	s.renderAccounts(w, r, http.StatusOK, nil)
}

// scopeFromForm reads the customer checkboxes: nil (all customers) unless the
// "limit" box is ticked.
func scopeFromForm(r *http.Request) []string {
	if r.PostFormValue("limit") != "1" {
		return nil
	}
	ids := r.PostForm["customer"]
	if ids == nil {
		ids = []string{}
	}
	return ids
}

// rowErr re-renders the page with an error on one row (or on the create form).
func (s *Service) rowErr(w http.ResponseWriter, r *http.Request, id string, err error) {
	s.logUnexpected("accounts", err)
	n := authNote(err)
	s.renderAccounts(w, r, noteStatus(err), func(d *AccountsPage) {
		for i := range d.Rows {
			if d.Rows[i].ID == id {
				d.Rows[i].Err = &n
				return
			}
		}
		d.FormErr = &n
	})
}

func (s *Service) accountsCreate(w http.ResponseWriter, r *http.Request) {
	in := auth.AccountInput{Username: r.PostFormValue("username"), Email: r.PostFormValue("email"), Role: r.PostFormValue("role"), Scope: scopeFromForm(r)}
	a, link, err := s.Auth.CreateAccount(r.Context(), principal(r), in, "web", httpx.ClientIP(r))
	if err != nil {
		s.logUnexpected("accounts create", err)
		n := authNote(err)
		s.renderAccounts(w, r, noteStatus(err), func(d *AccountsPage) {
			d.FormErr = &n
			d.Form = AccountForm{Username: in.Username, Email: in.Email, Role: in.Role, Scope: in.Scope}
			d.Roles = roleOpts(in.Role)
			for i := range d.Customers {
				d.Customers[i].Checked = slices.Contains(in.Scope, d.Customers[i].ID)
			}
		})
		return
	}
	s.renderAccounts(w, r, http.StatusOK, func(d *AccountsPage) {
		d.Link = &LinkView{Username: a.Username, URL: resetURLFor(r, link.Path), Expires: s.fc(r.Context()).fullTime(link.Expires), What: "created"}
		d.Notice = &Note{"ok", "Account created.", "Send the link below to " + a.Username + " over a channel you trust. They set their own password with it and set up an authenticator at their first sign-in."}
	})
}

func (s *Service) accountsRole(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.Auth.UpdateAccount(r.Context(), principal(r), id, r.PostFormValue("role"), scopeFromForm(r), "web", httpx.ClientIP(r)); err != nil {
		s.rowErr(w, r, id, err)
		return
	}
	http.Redirect(w, r, "/settings/accounts?done=role", http.StatusSeeOther)
}

func (s *Service) accountsDisable(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.Auth.DisableAccount(r.Context(), principal(r), id, r.PostFormValue("reason"), "web", httpx.ClientIP(r)); err != nil {
		s.rowErr(w, r, id, err)
		return
	}
	http.Redirect(w, r, "/settings/accounts?done=disabled", http.StatusSeeOther)
}

func (s *Service) accountsEnable(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.Auth.EnableAccount(r.Context(), principal(r), id, "web", httpx.ClientIP(r)); err != nil {
		s.rowErr(w, r, id, err)
		return
	}
	http.Redirect(w, r, "/settings/accounts?done=enabled", http.StatusSeeOther)
}

func (s *Service) accountsDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p := principal(r)
	a, err := s.Auth.GetAccount(r.Context(), p, id)
	if err != nil {
		s.rowErr(w, r, id, err)
		return
	}
	// Destructive confirm (components §6.2): the username must be typed.
	if !strings.EqualFold(strings.TrimSpace(r.PostFormValue("confirm")), a.Username) {
		s.renderAccounts(w, r, http.StatusBadRequest, func(d *AccountsPage) {
			for i := range d.Rows {
				if d.Rows[i].ID == id {
					d.Rows[i].Err = &Note{"crit", "Not deleted.", "Type the username " + a.Username + " to confirm."}
				}
			}
		})
		return
	}
	if err := s.Auth.DeleteAccount(r.Context(), p, id, "web", httpx.ClientIP(r)); err != nil {
		s.rowErr(w, r, id, err)
		return
	}
	http.Redirect(w, r, "/settings/accounts?done=deleted", http.StatusSeeOther)
}

func (s *Service) accountsResetMFA(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.Auth.ResetAccountMFA(r.Context(), principal(r), id, r.PostFormValue("reason"), "web", httpx.ClientIP(r)); err != nil {
		s.rowErr(w, r, id, err)
		return
	}
	http.Redirect(w, r, "/settings/accounts?done=mfa", http.StatusSeeOther)
}

func (s *Service) accountsResetPassword(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p := principal(r)
	link, err := s.Auth.ResetAccountPassword(r.Context(), p, id, "web", httpx.ClientIP(r))
	if err != nil {
		s.rowErr(w, r, id, err)
		return
	}
	a, _ := s.Store.GetAdmin(r.Context(), id)
	s.renderAccounts(w, r, http.StatusOK, func(d *AccountsPage) {
		d.Link = &LinkView{Username: a.Username, URL: resetURLFor(r, link.Path), Expires: s.fc(r.Context()).fullTime(link.Expires), What: "reset"}
		d.Notice = &Note{"ok", "Reset link issued.", "The old password keeps working until the link is used; using it signs the account out everywhere and revokes its API tokens. A new link replaces this one."}
	})
}
