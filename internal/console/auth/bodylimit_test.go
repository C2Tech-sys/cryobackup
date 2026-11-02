package auth

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// M2-W3D: unit tests of bodylimit.go itself. The gates that use it are tested
// where they live (the CSRF and 413 tests of the UI); these pin the four size
// cases of the helper and what it promises the handler behind it.

const testCap = 64

// tripReader fails the test if anything reads it: a request refused on its
// declared length must not be read at all.
type tripReader struct{ t *testing.T }

func (r tripReader) Read([]byte) (int, error) {
	r.t.Helper()
	r.t.Error("the body was read although the declared length is over the cap")
	return 0, io.EOF
}

// chunked builds a request whose length is unknown to the server (Transfer-Encoding: chunked).
func chunked(method string, body io.Reader) *http.Request {
	r := httptest.NewRequest(method, "/x", body)
	r.ContentLength = -1
	r.TransferEncoding = []string{"chunked"}
	return r
}

// serve runs LimitBody around a handler that records what it saw.
func serve(max int64, r *http.Request) (rec *httptest.ResponseRecorder, reached bool, seen []byte, seenLen int64) {
	rec = httptest.NewRecorder()
	LimitBody(max, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		if r.Body != nil {
			seen, _ = io.ReadAll(r.Body)
		}
		seenLen = r.ContentLength
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rec, r)
	return rec, reached, seen, seenLen
}

func TestLimitBodyContentLengthOverTheCapIsRefusedUnread(t *testing.T) {
	r := httptest.NewRequest("POST", "/x", tripReader{t})
	r.ContentLength = testCap + 1
	rec, reached, _, _ := serve(testCap, r)
	if rec.Code != http.StatusRequestEntityTooLarge || reached {
		t.Fatalf("declared length over the cap: status %d, handler reached %v", rec.Code, reached)
	}
	if !strings.Contains(rec.Body.String(), "too large") {
		t.Fatalf("body %q", rec.Body.String())
	}
}

func TestLimitBodyChunkedOverTheCapIsRefused(t *testing.T) {
	for name, size := range map[string]int{"one byte over": testCap + 1, "far over": testCap * 100} {
		r := chunked("POST", bytes.NewReader(bytes.Repeat([]byte("a"), size)))
		rec, reached, _, _ := serve(testCap, r)
		if rec.Code != http.StatusRequestEntityTooLarge || reached {
			t.Errorf("%s: status %d, handler reached %v", name, rec.Code, reached)
		}
	}
}

func TestLimitBodyAtTheCapPassesWithTheBodyIntact(t *testing.T) {
	body := bytes.Repeat([]byte("b"), testCap)
	for name, r := range map[string]*http.Request{
		"content length": httptest.NewRequest("POST", "/x", bytes.NewReader(body)),
		"chunked":        chunked("POST", bytes.NewReader(body)),
	} {
		rec, reached, seen, seenLen := serve(testCap, r)
		if rec.Code != http.StatusNoContent || !reached {
			t.Errorf("%s: status %d, handler reached %v", name, rec.Code, reached)
			continue
		}
		if !bytes.Equal(seen, body) || seenLen != testCap {
			t.Errorf("%s: handler saw %d bytes (ContentLength %d), want %d", name, len(seen), seenLen, testCap)
		}
	}
}

func TestLimitBodyUnderTheCapPassesAndFixesTheLength(t *testing.T) {
	r := chunked("PUT", strings.NewReader("short"))
	rec, reached, seen, seenLen := serve(testCap, r)
	if rec.Code != http.StatusNoContent || !reached || string(seen) != "short" {
		t.Fatalf("status %d, reached %v, saw %q", rec.Code, reached, seen)
	}
	// An unknown length becomes the real one, so the handlers behind do not guess.
	if seenLen != int64(len("short")) {
		t.Fatalf("ContentLength after buffering = %d", seenLen)
	}
}

func TestLimitBodyLeavesSafeMethodsAndEmptyBodiesAlone(t *testing.T) {
	big := bytes.Repeat([]byte("c"), testCap*10)
	// A GET is not a write: it is not read and not refused, whatever it carries.
	r := httptest.NewRequest("GET", "/x", bytes.NewReader(big))
	if rec, reached, _, _ := serve(testCap, r); rec.Code != http.StatusNoContent || !reached {
		t.Errorf("GET: status %d, reached %v", rec.Code, reached)
	}
	for name, r := range map[string]*http.Request{
		"no body":   httptest.NewRequest("POST", "/x", nil),
		"NoBody":    func() *http.Request { r := httptest.NewRequest("POST", "/x", nil); r.Body = http.NoBody; return r }(),
		"nil Body":  func() *http.Request { r := httptest.NewRequest("POST", "/x", nil); r.Body = nil; return r }(),
		"zero size": httptest.NewRequest("DELETE", "/x", strings.NewReader("")),
	} {
		if rec, reached, _, _ := serve(testCap, r); rec.Code != http.StatusNoContent || !reached {
			t.Errorf("%s: status %d, reached %v", name, rec.Code, reached)
		}
	}
}

// brokenReader delivers some bytes and then a transport error, as a dropped connection does.
type brokenReader struct{ sent bool }

func (b *brokenReader) Read(p []byte) (int, error) {
	if !b.sent {
		b.sent = true
		return copy(p, "partial"), nil
	}
	return 0, errors.New("connection reset")
}

func TestLimitBodyAReadFailureIsABadRequestNotATooLarge(t *testing.T) {
	r := chunked("POST", &brokenReader{})
	rec, reached, _, _ := serve(testCap, r)
	if rec.Code != http.StatusBadRequest || reached {
		t.Fatalf("status %d, reached %v", rec.Code, reached)
	}
}

// A second, larger limit behind the first is a no-op: the body is already
// buffered and within the first limit (Require applies DefaultUIBodyLimit again).
func TestLimitBodyTwiceKeepsTheBodyAndTheStricterLimit(t *testing.T) {
	r := chunked("POST", strings.NewReader("twice"))
	rec := httptest.NewRecorder()
	var seen string
	inner := LimitBody(DefaultUIBodyLimit, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seen = string(b)
		w.WriteHeader(http.StatusNoContent)
	}))
	LimitBody(testCap, inner).ServeHTTP(rec, r)
	if rec.Code != http.StatusNoContent || seen != "twice" {
		t.Fatalf("status %d, saw %q", rec.Code, seen)
	}
	// And the stricter limit still wins when it is the outer one.
	rec = httptest.NewRecorder()
	LimitBody(3, inner).ServeHTTP(rec, chunked("POST", strings.NewReader("twice")))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("outer limit 3: status %d", rec.Code)
	}
}

func TestDefaultUIBodyLimitIsOneMebibyte(t *testing.T) {
	if DefaultUIBodyLimit != 1<<20 {
		t.Fatalf("DefaultUIBodyLimit = %d", DefaultUIBodyLimit)
	}
	over := httptest.NewRequest("POST", "/x", strings.NewReader("x"))
	over.ContentLength = DefaultUIBodyLimit + 1
	over.Body = io.NopCloser(tripReader{t})
	rec := httptest.NewRecorder()
	if limitBody(rec, over, DefaultUIBodyLimit) || rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("one byte over the default: status %d", rec.Code)
	}
}
