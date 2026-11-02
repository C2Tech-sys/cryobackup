package uplink

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/api"
)

// A command result the console answers with a final 4xx is refused (the
// caller drops it); auth problems, throttling, server errors and network
// failures are not (the caller retries). Real client, fake console, one status
// class at a time (M2-W3C).
func TestRefusedClassifiesCommandResultAnswers(t *testing.T) {
	const cmdID = "01JB0000000000000000000009"
	res := api.CommandResultRequest{Status: api.CommandSucceeded}
	cases := []struct {
		status  int
		refused bool
	}{
		{http.StatusNoContent, false},       // posted: not an error at all
		{http.StatusMovedPermanently, true}, // the client never follows a redirect: same post, same answer
		{http.StatusTemporaryRedirect, true},
		{http.StatusPermanentRedirect, true},
		{http.StatusBadRequest, true},
		{http.StatusNotFound, true},
		{http.StatusConflict, true},
		{http.StatusGone, true},
		{http.StatusRequestEntityTooLarge, true},
		{http.StatusUnprocessableEntity, true},
		{http.StatusMethodNotAllowed, true}, // any other 4xx that is about the request
		{http.StatusUnsupportedMediaType, true},
		{http.StatusUnauthorized, false}, // auth: back off and re-authenticate
		{http.StatusForbidden, false},
		{http.StatusProxyAuthRequired, false},
		{http.StatusRequestTimeout, false},
		{http.StatusTooEarly, false},
		{http.StatusTooManyRequests, false},
		{http.StatusInternalServerError, false},
		{http.StatusBadGateway, false},
		{http.StatusServiceUnavailable, false},
		{http.StatusGatewayTimeout, false},
	}
	for _, tc := range cases {
		t.Run(strconv.Itoa(tc.status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != api.RouteCommandResult(cmdID) {
					http.NotFound(w, r)
					return
				}
				stamp(w, now)
				w.Header().Set("Content-Type", api.ContentTypeProblem)
				w.WriteHeader(tc.status)
				if tc.status != http.StatusNoContent {
					_ = json.NewEncoder(w).Encode(map[string]string{"title": `attacker text C:\Users\PHI-sentinel\x.docx`})
				}
			}))
			defer srv.Close()
			c := clientFor(t, srv, nil)
			err := c.CommandResult(context.Background(), cmdID, res)
			if (tc.status == http.StatusNoContent) != (err == nil) {
				t.Fatalf("err = %v for %d", err, tc.status)
			}
			if got := Refused(err); got != tc.refused {
				t.Errorf("Refused(%v) = %v, want %v", err, got, tc.refused)
			}
			if err != nil && strings.Contains(err.Error(), "PHI-sentinel") {
				t.Errorf("the error repeats wire text: %q", err.Error())
			}
		})
	}
}

// Failures with no HTTP answer are never refusals; a body that can never be
// sent is (retrying the same bytes cannot succeed).
func TestRefusedLocalAndTransportFailures(t *testing.T) {
	const cmdID = "01JB0000000000000000000009"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	c := clientFor(t, srv, nil)
	srv.Close() // the console is unreachable now

	err := c.CommandResult(context.Background(), cmdID, api.CommandResultRequest{Status: api.CommandSucceeded})
	var ue *Error
	if !errors.As(err, &ue) || !ue.Network || Refused(err) {
		t.Errorf("network failure: err = %v, Refused = %v", err, Refused(err))
	}
	if Refused(nil) {
		t.Error("Refused(nil)")
	}
	if Refused(context.Canceled) || Refused(errors.New("boom")) {
		t.Error("an unrelated error is not a refusal")
	}

	huge := json.RawMessage(`{"a":"` + strings.Repeat("x", 70<<10) + `"}`)
	err = c.CommandResult(context.Background(), cmdID, api.CommandResultRequest{Status: api.CommandSucceeded, Result: huge})
	if !Refused(err) {
		t.Errorf("an oversized body is final: %v", err)
	}
	err = c.CommandResult(context.Background(), cmdID, api.CommandResultRequest{
		Status: api.CommandSucceeded, Result: json.RawMessage(`{"where":"C:\\Users\\jane"}`)})
	if !Refused(err) {
		t.Errorf("a path-bearing body is final: %v", err)
	}
}

// An id that is not a command id is never sent: the route built from it would
// not name a command (the console's mux redirects an empty, "." or ".." id).
func TestCommandResultWithABadIDIsNotSent(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	c := clientFor(t, srv, nil)
	for _, id := range []string{"", ".", "..", "../x", "01JB000000000000000000000", "01jb0000000000000000000009", "01JB0000000000000000000009/x"} {
		err := c.CommandResult(context.Background(), id, api.CommandResultRequest{Status: api.CommandSucceeded})
		if !errors.Is(err, ErrBadCommandID) || !Refused(err) {
			t.Errorf("id %q: err = %v, Refused = %v", id, err, Refused(err))
		}
	}
	if hits.Load() != 0 {
		t.Errorf("the server received %d requests", hits.Load())
	}
	if err := c.CommandResult(context.Background(), "01JB0000000000000000000009", api.CommandResultRequest{Status: api.CommandSucceeded}); errors.Is(err, ErrBadCommandID) {
		t.Errorf("a valid id was refused: %v", err)
	}
}
