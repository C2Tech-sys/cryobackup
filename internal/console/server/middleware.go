package server

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/console/httpx"
	"github.com/c2tech-sys/cryobackup/internal/logx"
)

// statusWriter records the status and byte count and stays transparent to
// http.Flusher (SSE), http.Hijacker and http.ResponseController.
type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
	n      int64
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wrote {
		w.status, w.wrote = code, true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.status, w.wrote = http.StatusOK, true
	}
	n, err := w.ResponseWriter.Write(b)
	w.n += int64(n)
	return n, err
}

// Flush implements http.Flusher.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack implements http.Hijacker.
func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := w.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, errors.New("server: hijack unsupported")
}

// Unwrap lets http.NewResponseController reach the real writer (deadlines).
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// middleware is one layer.
type middleware func(http.Handler) http.Handler

func chain(h http.Handler, ms ...middleware) http.Handler {
	for i := len(ms) - 1; i >= 0; i-- {
		h = ms[i](h)
	}
	return h
}

// withRequestID gives every request a fresh id (an inbound X-Request-Id is not
// trusted), echoes it in the response and stores it in the context.
func withRequestID() middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := httpx.NewRequestID()
			w.Header().Set(api.HeaderRequestID, id)
			ctx := httpx.WithRequestID(r.Context(), id)
			ctx, _ = httpx.WithActor(ctx)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// withRecover turns a handler panic into a 500 problem response and a path-free
// log line (the panic value is not printed: it may carry anything).
func withRecover(log *logx.Logger) middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if p := recover(); p != nil {
					if p == http.ErrAbortHandler {
						panic(p)
					}
					log.Error("handler panic", logx.ID("request_id", httpx.RequestID(r.Context())),
						logx.Str("route", sanitizePattern(routeOf(r))), logx.Class(fmt.Sprintf("%T", p)))
					if sw, ok := w.(*statusWriter); !ok || !sw.wrote {
						httpx.Problem(w, r, api.CodeInternal, 0)
					}
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

func routeOf(r *http.Request) string {
	if r.Pattern == "" {
		return "unmatched"
	}
	return r.Pattern
}

// withAccessLog logs method, route pattern, status, duration and the actor id,
// never the path or the query string.
func withAccessLog(log *logx.Logger, now func() time.Time) middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := now()
			sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
			defer func() {
				attrs := []logx.Attr{logx.Str("method", r.Method), logx.Str("route", sanitizePattern(routeOf(r))),
					logx.Int("status", sw.status), logx.Dur("took", now().Sub(start)), logx.ID("request_id", httpx.RequestID(r.Context()))}
				if a, ok := actorOf(r); ok {
					kind, id := a.Get()
					if kind != "" {
						attrs = append(attrs, logx.Str("actor", kind), logx.ID("actor_id", id))
					}
				}
				log.LogAttrs(r.Context(), logx.LevelInfo, "request", attrs...)
			}()
			next.ServeHTTP(sw, r)
		})
	}
}

// sanitizePattern keeps a pattern loggable as a Str token: "GET /api/v1/x/{id}"
// has spaces and braces, so it is reduced to method and path with the allow-list
// characters only.
func sanitizePattern(p string) string {
	out := make([]byte, 0, len(p))
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '.', c == '-':
			out = append(out, c)
		case c == '/', c == ' ', c == '{', c == '}', c == '$':
			out = append(out, '_')
		}
	}
	if len(out) > 64 {
		out = out[:64]
	}
	return string(out)
}

// withServerTime stamps X-CryoBackup-Server-Time (DH5) on every agent response.
// A handler that waits (the commands long poll) sets it again just before it
// writes, so the stamp is the time of the answer.
func withServerTime(now func() time.Time) middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set(api.HeaderServerTime, api.FormatServerTime(now()))
			next.ServeHTTP(w, r)
		})
	}
}

// ContentSecurityPolicy is the admin listener's CSP: everything from this
// origin only, no inline script, inline style attributes allowed (the
// progress-bar widths) but no <style> elements, no framing, no base tag.
// htmx is configured through a <meta> tag, so no nonce is needed.
const ContentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self'; style-src-attr 'unsafe-inline'; " +
	"img-src 'self'; font-src 'self'; connect-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

// withSecurityHeaders sets the admin listener's response headers.
func withSecurityHeaders() middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("Content-Security-Policy", ContentSecurityPolicy)
			h.Set("Referrer-Policy", "same-origin")
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("X-Frame-Options", "DENY")
			h.Set("Cross-Origin-Opener-Policy", "same-origin")
			h.Set("Cross-Origin-Resource-Policy", "same-origin")
			h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")
			h.Set("Cache-Control", "no-store")
			next.ServeHTTP(w, r)
		})
	}
}

func actorOf(r *http.Request) (*httpx.Actor, bool) { return httpx.ActorFrom(r.Context()) }
