// Package httpx holds the small HTTP helpers every console route set shares:
// request ids, RFC 9457 problem responses built from internal/api's error
// model, JSON writing, the client address, and the per-request actor record the
// access log reads. It has no dependency on any other console package.
package httpx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
)

type ctxKey int

const (
	keyRequestID ctxKey = iota
	keyActor
)

// NewRequestID returns a 16-hex-character random id.
func NewRequestID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// WithRequestID stores the request id in ctx.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, keyRequestID, id)
}

// RequestID returns the request id stored by the server middleware ("" if none).
func RequestID(ctx context.Context) string {
	s, _ := ctx.Value(keyRequestID).(string)
	return s
}

// Actor is the authenticated principal of one request, recorded for the access
// log. Handlers set it after authentication; it is read when the request ends.
type Actor struct {
	mu   sync.Mutex
	kind string
	id   string
}

// Set records who made the request ("device", "admin").
func (a *Actor) Set(kind, id string) {
	a.mu.Lock()
	a.kind, a.id = kind, id
	a.mu.Unlock()
}

// Get returns the recorded kind and id.
func (a *Actor) Get() (kind, id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.kind, a.id
}

// WithActor attaches a fresh Actor to ctx.
func WithActor(ctx context.Context) (context.Context, *Actor) {
	a := &Actor{}
	return context.WithValue(ctx, keyActor, a), a
}

// ActorFrom returns the Actor attached by WithActor.
func ActorFrom(ctx context.Context) (*Actor, bool) {
	a, ok := ctx.Value(keyActor).(*Actor)
	return a, ok
}

// SetActor records the actor on the request context, if one is attached.
func SetActor(ctx context.Context, kind, id string) {
	if a, ok := ctx.Value(keyActor).(*Actor); ok {
		a.Set(kind, id)
	}
}

// ClientIP returns the host part of the connection's remote address. The
// X-Forwarded-For header is deliberately not trusted: the agent listener is
// public and a client could forge it to dodge the per-IP limits.
func ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// Problem writes an RFC 9457 application/problem+json body for the api error
// code, with the matching status, the request id and, when retryAfter is
// positive, Retry-After (whole seconds, at least 1) and retry_after_s.
func Problem(w http.ResponseWriter, r *http.Request, code string, retryAfter time.Duration) {
	ProblemStatus(w, r, code, 0, retryAfter)
}

// ProblemStatus is Problem with an explicit HTTP status (0 keeps the one the
// api error table gives the code). The enroll route uses it for the 501 that
// refuses customer-held custody in M0.
func ProblemStatus(w http.ResponseWriter, r *http.Request, code string, status int, retryAfter time.Duration) {
	e := api.NewError(code)
	if status != 0 {
		e.Status = status
	}
	e.RequestID = RequestID(r.Context())
	h := w.Header()
	h.Set("Content-Type", api.ContentTypeProblem)
	h.Set("Cache-Control", "no-store")
	if retryAfter > 0 {
		secs := int((retryAfter + time.Second - 1) / time.Second)
		if secs < 1 {
			secs = 1
		}
		e.RetryAfterS = secs
		h.Set(api.HeaderRetryAfter, strconv.Itoa(secs))
	}
	w.WriteHeader(e.Status)
	_ = json.NewEncoder(w).Encode(e)
}

// WriteJSON writes v as JSON with the status, no-store.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	h := w.Header()
	h.Set("Content-Type", api.ContentTypeJSON)
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Registrar is what route sets register on. The server's Router records every
// pattern it is given, which is how the disjoint-mux test sees the route sets.
type Registrar interface {
	Handle(pattern string, h http.Handler)
}

// HandleFunc registers a function on r.
func HandleFunc(r Registrar, pattern string, f http.HandlerFunc) { r.Handle(pattern, f) }
