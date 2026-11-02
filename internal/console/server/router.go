package server

import (
	"net/http"
	"sort"
	"strings"
	"sync"
)

// Router is an http.ServeMux that remembers every pattern registered on it.
// ServeMux cannot list its patterns, and the disjoint-mux test (architecture
// §5: "a test walks both muxes and fails if a path appears in both") needs to.
type Router struct {
	mux      *http.ServeMux
	mu       sync.Mutex
	patterns []string
}

// NewRouter returns an empty Router.
func NewRouter() *Router { return &Router{mux: http.NewServeMux()} }

// Handle registers h under pattern (Go 1.22 "METHOD /path/{wildcard}" syntax)
// and records the pattern. Registering a conflicting pattern panics, as ServeMux does.
func (r *Router) Handle(pattern string, h http.Handler) {
	r.mu.Lock()
	r.patterns = append(r.patterns, pattern)
	r.mu.Unlock()
	r.mux.Handle(pattern, h)
}

// ServeHTTP implements http.Handler.
func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) { r.mux.ServeHTTP(w, req) }

// Patterns returns the registered patterns, sorted.
func (r *Router) Patterns() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := append([]string(nil), r.patterns...)
	sort.Strings(out)
	return out
}

// CatchAll is the pattern of the not-found handler each listener installs; the
// disjointness check ignores it because both listeners have one.
const CatchAll = "/"

// PathOf strips the method from a pattern ("GET /a/{id}" becomes "/a/{id}").
func PathOf(pattern string) string {
	if i := strings.IndexByte(pattern, ' '); i >= 0 {
		return strings.TrimSpace(pattern[i+1:])
	}
	return pattern
}
