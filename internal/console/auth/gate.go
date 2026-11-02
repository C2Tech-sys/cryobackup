package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/console/httpx"
)

// The route gates (architecture §5.2 "who"). Every route of the admin listener
// except the login form, /static and /healthz sits behind one of them:
//
//	Require            a completed login (session cookie past the MFA step) or,
//	                   on API routes only, an API token. Role >= min.
//	RequireSession     like Require but a session only: an API token is refused
//	                   (a token cannot mint tokens or change the password).
//	RequirePending     a pending session only (the MFA step routes).
//	RequireEnrollment  a completed login, or a pending session of an account
//	                   that has no TOTP yet (the forced enrollment step).
//
// A pending session satisfies RequirePending and RequireEnrollment and nothing
// else: on every other route it is treated like no session at all (API 401; UI
// redirect to /login/mfa so the browser lands on the step it still owes).
// Cookie requests that are not GET/HEAD/OPTIONS must pass CheckCSRF. Bearer
// requests carry no cookie and need no CSRF token; they are refused on UI routes
// (ModeUI) with 403. An Authorization header of another scheme (Basic from an
// authenticating proxy in front of the console) is ignored.

type gateKind int

const (
	gateFull gateKind = iota
	gateSession
	gatePending
	gateEnroll
)

// Require wraps next: a completed login (or, in ModeAPI, an API token) with at
// least role min, CSRF for unsafe cookie requests; the Principal is stored in
// the request context.
func (m *Manager) Require(min Role, mode Mode, next http.Handler) http.Handler {
	return m.gate(gateFull, min, mode, next)
}

// RequireSession is Require without the API-token path.
func (m *Manager) RequireSession(min Role, mode Mode, next http.Handler) http.Handler {
	return m.gate(gateSession, min, mode, next)
}

// RequirePending admits pending sessions only (the MFA step).
func (m *Manager) RequirePending(mode Mode, next http.Handler) http.Handler {
	return m.gate(gatePending, RoleReadOnly, mode, next)
}

// RequireEnrollment admits a completed login, or a pending session whose
// account has no TOTP enrollment (the forced enrollment of its first login).
func (m *Manager) RequireEnrollment(mode Mode, next http.Handler) http.Handler {
	return m.gate(gateEnroll, RoleReadOnly, mode, next)
}

// bearerOf returns the token of an "Authorization: Bearer <token>" header and
// whether the header uses the Bearer scheme at all.
func bearerOf(r *http.Request) (string, bool) {
	h := r.Header.Get(api.HeaderAuthorization)
	if len(h) < len(api.BearerPrefix) || !strings.EqualFold(h[:len(api.BearerPrefix)], api.BearerPrefix) {
		return "", false
	}
	return strings.TrimSpace(h[len(api.BearerPrefix):]), true
}

func (m *Manager) gate(kind gateKind, min Role, mode Mode, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// M2-W2A fix 4: a console write is read at most DefaultUIBodyLimit, and refused
		// with 413 before the CSRF check parses the form (a smaller per-route cap is
		// applied earlier by LimitBody).
		if mode == ModeUI && !limitBody(w, r, DefaultUIBodyLimit) {
			return
		}
		deny := func(code, uiText string, status int) {
			if mode == ModeAPI {
				httpx.Problem(w, r, code, 0)
				return
			}
			http.Error(w, uiText, status)
		}
		if tok, isBearer := bearerOf(r); isBearer {
			if mode == ModeUI {
				deny(api.CodeForbidden, "Forbidden: API tokens are accepted on /api/v1 only; sign in to use the console.", http.StatusForbidden)
				return
			}
			if kind != gateFull {
				deny(api.CodeForbidden, "", http.StatusForbidden)
				return
			}
			p, err := m.AuthenticateBearer(r.Context(), tok)
			if err != nil {
				if !errors.Is(err, ErrNoSession) {
					httpx.Problem(w, r, api.CodeInternal, 0)
					return
				}
				httpx.Problem(w, r, api.CodeUnauthorized, 0)
				return
			}
			httpx.SetActor(r.Context(), "api_token", p.TokenID)
			if p.Role < min {
				httpx.Problem(w, r, api.CodeForbidden, 0)
				return
			}
			next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
			return
		}

		var p *Principal
		if c, err := r.Cookie(SessionCookie); err == nil {
			var aerr error
			p, aerr = m.sessionPrincipal(r.Context(), c.Value)
			if aerr != nil && !errors.Is(aerr, ErrNoSession) {
				deny(api.CodeInternal, "The console could not check the session. Try again shortly.", http.StatusInternalServerError)
				return
			}
		}
		if p == nil {
			if mode == ModeAPI {
				httpx.Problem(w, r, api.CodeUnauthorized, 0)
				return
			}
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		switch kind {
		case gateFull, gateSession:
			if p.Pending {
				if mode == ModeAPI {
					httpx.Problem(w, r, api.CodeUnauthorized, 0)
					return
				}
				http.Redirect(w, r, MFAPath, http.StatusSeeOther)
				return
			}
		case gatePending:
			if !p.Pending {
				if mode == ModeAPI {
					httpx.Problem(w, r, api.CodeConflict, 0)
					return
				}
				http.Redirect(w, r, "/", http.StatusSeeOther)
				return
			}
		case gateEnroll:
			if p.Pending && p.Enrolled() {
				if mode == ModeAPI {
					httpx.Problem(w, r, api.CodeUnauthorized, 0)
					return
				}
				http.Redirect(w, r, MFAPath, http.StatusSeeOther)
				return
			}
		}
		httpx.SetActor(r.Context(), "admin", p.Admin.ID)
		if p.Role < min {
			deny(api.CodeForbidden, "Forbidden: this action needs the "+min.String()+" role.", http.StatusForbidden)
			return
		}
		if err := m.CheckCSRF(r, p); err != nil {
			deny(api.CodeForbidden, "Forbidden: the request did not carry a valid CSRF token. Reload the page and try again.", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
	})
}

// MFAPath is the UI's MFA step page; the gates send a pending browser there.
const MFAPath = "/login/mfa"

// RequestIsTLS reports whether the request reached the console over TLS (the
// rule the session cookie's Secure flag follows), for building absolute links.
func RequestIsTLS(r *http.Request) bool { return secureConn(r) }

// PendingPrincipal resolves a cookie to a pending session's Principal; a
// completed or unknown session is ErrNoSession. The MFA page uses it to tell
// "try again" from "start again" after a wrong code.
func (m *Manager) PendingPrincipal(ctx context.Context, cookie string) (*Principal, error) {
	p, err := m.sessionPrincipal(ctx, cookie)
	if err != nil {
		return nil, err
	}
	if !p.Pending {
		return nil, ErrNoSession
	}
	return p, nil
}
