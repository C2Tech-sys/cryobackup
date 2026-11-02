package auth

import (
	"bytes"
	"errors"
	"io"
	"net/http"
)

// DefaultUIBodyLimit is the largest body any console (UI) write is read at:
// every form on the console is far below it. A route that takes less says so
// with LimitBody.
const DefaultUIBodyLimit = 1 << 20

// limitBody refuses a request whose body is larger than max with 413, before
// anything parses it (CheckCSRF reads the form field csrf_token, and net/http's
// own form parsing would otherwise take up to 10 MB first). A body within the
// limit is buffered and replaced, so the handlers read it as usual and a
// second call with a larger limit is a no-op. Safe methods pass untouched.
func limitBody(w http.ResponseWriter, r *http.Request, max int64) bool {
	if safeMethod(r.Method) || r.Body == nil || r.Body == http.NoBody {
		return true
	}
	if r.ContentLength > max {
		http.Error(w, "The request is too large.", http.StatusRequestEntityTooLarge)
		return false
	}
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, max))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			http.Error(w, "The request is too large.", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "The request could not be read.", http.StatusBadRequest)
		}
		return false
	}
	r.Body = io.NopCloser(bytes.NewReader(b))
	r.ContentLength = int64(len(b))
	return true
}

// LimitBody wraps next so that a write with a body over max bytes is answered
// 413 before the gate behind it (session, CSRF) reads or parses anything. Put it
// outside Require; Require itself applies DefaultUIBodyLimit to every UI write.
func LimitBody(max int64, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !limitBody(w, r, max) {
			return
		}
		next.ServeHTTP(w, r)
	})
}
