package adminapi

import (
	"net/http"
	"strings"

	"github.com/c2tech-sys/cryobackup/internal/console/httpx"
)

// Fleet changes (M3-T3 fix round 4). The admin event stream shares one fleet
// evaluation per scope and reuses it only while the change generation it was
// made at is current (m3_events.go). So every API mutation that can change what
// the evaluation reads (customers, devices, policies and assignments, overrides,
// settings, runs and commands, enrollment, restores, probe facts) must advance
// the generation, and tell the hub so open streams and pages re-render, before
// its answer reaches the client. fleetChanged is the one place that does both.
//
// Register wraps every mutating route (any method but GET) in fleetSignal
// unless the route is listed in fleetNeutral with the reason it cannot change
// the fleet. A new mutating route therefore signals by default, and the test
// TestEveryFleetMutationAdvancesTheChangeGeneration fails until it is named on
// one side or the other.

// fleetChanged advances the change generation and signals the hub. deviceID is
// the device that changed, or "" for a change that is not one device's.
func (s *Service) fleetChanged(deviceID string) {
	s.apiGen.Add(1)
	if s.Events != nil {
		s.Events.DeviceChanged(deviceID)
	}
}

// fleetNeutral lists the mutating routes that cannot change anything the fleet
// evaluation reads, each with the reason. Everything the evaluation loads is
// customers, devices, runs, policies, settings and their relatives
// (store.LoadWorld); none of these routes writes them.
var fleetNeutral = map[string]string{
	PatternLogin:           "creates a session",
	PatternLogout:          "ends a session",
	PatternMFATOTP:         "completes a sign-in",
	PatternMFARecovery:     "completes a sign-in with a recovery code",
	PatternMePatch:         "changes the caller's own profile",
	PatternTOTPBegin:       "starts the caller's MFA enrollment",
	PatternTOTPConfirm:     "finishes the caller's MFA enrollment",
	PatternRecoveryCodes:   "replaces the caller's recovery codes",
	PatternSessionRevoke:   "ends a session",
	PatternTokenCreate:     "creates an API token",
	PatternTokenRevoke:     "revokes an API token",
	PatternAdminCreate:     "creates an account; a scope is evaluated on its own key",
	PatternAdminPatch:      "changes an account's role or scope, not fleet data",
	PatternAdminDelete:     "deletes an account",
	PatternAdminDisable:    "disables an account",
	PatternAdminEnable:     "enables an account",
	PatternAdminResetMFA:   "resets an account's MFA",
	PatternAdminResetPassw: "resets an account's password",
	PatternViewCreate:      "saved views are display settings, not fleet data",
	PatternViewPatch:       "saved views are display settings, not fleet data",
	PatternViewDelete:      "saved views are display settings, not fleet data",
	PatternPolicyImpact:    "previews a policy change and writes only a one-time impact token",
	PatternRestoreMint:     "mints a download token for a finished restore",
	PatternRestoreDownload: "streams a finished restore's archive and counts the download (a GET: never wrapped)",
	PatternSystemJobRun:    "only accepts the run (202); the job writes its facts later, and the hub's refresh tick re-evaluates them",
}

// fleetRegistrar wraps the mutating routes it registers in fleetSignal.
type fleetRegistrar struct {
	httpx.Registrar
	s *Service
}

func (r fleetRegistrar) Handle(pattern string, h http.Handler) {
	if _, neutral := fleetNeutral[pattern]; !neutral && !strings.HasPrefix(pattern, "GET ") {
		h = r.s.fleetSignal(h)
	}
	r.Registrar.Handle(pattern, h)
}

// fleetSignal calls fleetChanged when h answers with a 2xx status, at the
// moment the status is written: after the handler's store write has committed
// and before any byte of the answer reaches the client, so a client that reads
// the answer and then opens a stream never gets an evaluation from before the
// change. A failed request signals nothing.
func (s *Service) fleetSignal(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(&fleetWriter{ResponseWriter: w, s: s}, r)
	})
}

type fleetWriter struct {
	http.ResponseWriter
	s       *Service
	written bool
}

func (w *fleetWriter) status(code int) {
	if w.written {
		return
	}
	w.written = true
	if code >= 200 && code < 300 {
		w.s.fleetChanged("")
	}
}

func (w *fleetWriter) WriteHeader(code int) {
	w.status(code)
	w.ResponseWriter.WriteHeader(code)
}

func (w *fleetWriter) Write(b []byte) (int, error) {
	w.status(http.StatusOK) // an implicit 200
	return w.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the connection's own writer.
func (w *fleetWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
