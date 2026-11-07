package ui

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
)

// streamRecheck is the /ui/events credential check, the twin of revalidate on the
// admin stream (adminapi/m3_events.go): the hub asks it on every ping, so a session
// that was signed out, expired or revoked, or an account that was disabled, loses
// its open stream within one ping interval (15 s).
//
// A database error is not a verdict: the stream stays and the next ping asks again.
//
// A changed role or scope also ends the stream. The hub groups streams by the scope
// they subscribed with, so the correct answer is the reconnect the browser makes on
// its own 3 seconds later, which subscribes with the account's current scope.
func (s *Service) streamRecheck(r *http.Request, p *auth.Principal) func(context.Context) bool {
	cookie := ""
	if c, err := r.Cookie(auth.SessionCookie); err == nil {
		cookie = c.Value
	}
	role, scope := p.Role, scopeKey(p.Scope)
	return func(ctx context.Context) bool {
		np, err := s.Auth.Authenticate(ctx, cookie)
		switch {
		case err == nil:
			return np.Role >= auth.RoleReadOnly && np.Role == role && scopeKey(np.Scope) == scope
		case errors.Is(err, auth.ErrNoSession):
			return false
		}
		if ctx.Err() == nil {
			s.Log.Warn("ui event stream: credential not re-checked", logx.Class("auth"))
		}
		return true
	}
}

// scopeKey is a scope in a form that compares: the customer list in sorted order.
func scopeKey(sc store.Scope) string {
	if sc.All {
		return "all"
	}
	ids := append([]string(nil), sc.CustomerIDs...)
	sort.Strings(ids)
	return strings.Join(ids, ",")
}
