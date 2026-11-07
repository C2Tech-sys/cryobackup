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

// streamVerdict is what the credential check tells an open stream.
type streamVerdict int

const (
	streamKeep      streamVerdict = iota // the credential holds, or the check could not run
	streamReconnect                      // the credential holds under a changed role or scope
	streamEnded                          // the session is gone
)

// streamRecheck is the /ui/events credential check, the twin of revalidate on the
// admin stream (adminapi/m3_events.go): the hub asks it on every ping, so a session
// that expired, was signed out or revoked, or whose account was disabled, loses its
// open stream within one ping interval (15 s). It uses CheckSession: an open stream
// is no operator, so it does not slide the idle expiry.
//
// A database error is not a verdict: the stream stays and the next ping asks again.
//
// A changed role or scope ends the stream without a word. The hub groups streams by
// the scope they subscribed with, so the correct answer is the reconnect the browser
// makes on its own 3 seconds later, which subscribes with the current scope.
func (s *Service) streamRecheck(r *http.Request, p *auth.Principal) func(context.Context) streamVerdict {
	cookie := ""
	if c, err := r.Cookie(auth.SessionCookie); err == nil {
		cookie = c.Value
	}
	role, scope := p.Role, scopeKey(p.Scope)
	return func(ctx context.Context) streamVerdict {
		np, err := s.Auth.CheckSession(ctx, cookie)
		switch {
		case err == nil:
			if np.Role != role || scopeKey(np.Scope) != scope {
				return streamReconnect
			}
			return streamKeep
		case errors.Is(err, auth.ErrNoSession):
			return streamEnded
		}
		if ctx.Err() == nil {
			s.Log.Warn("ui event stream: credential not re-checked", logx.Class("auth"))
		}
		return streamKeep
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
