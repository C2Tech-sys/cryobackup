package agentapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/console/config"
	"github.com/c2tech-sys/cryobackup/internal/console/enroll"
	"github.com/c2tech-sys/cryobackup/internal/console/escrow"
	"github.com/c2tech-sys/cryobackup/internal/console/httpx"
	"github.com/c2tech-sys/cryobackup/internal/console/ratelimit"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
	"github.com/c2tech-sys/cryobackup/internal/ulid"
)

// Publisher receives the facts that make a page stale: the UI's SSE hub
// implements it. Both methods must be cheap and must not block.
type Publisher interface {
	// DeviceChanged says the device's facts (presence, runs, snapshots) changed.
	DeviceChanged(deviceID string)
	// RunProgress says a heartbeat carried live progress for a run.
	RunProgress(deviceID, runID string)
}

// Service is the agent API of architecture §5.1: enroll, policy, heartbeat,
// runs, snapshots and the commands long poll. It is registered on the agent
// listener only.
type Service struct {
	Store *store.Store
	// Keys holds the master key; without one enrollment answers 503 (banner C12).
	Keys *escrow.Keys
	// Backend is the admin-path client (maintain): enrollment initialises the repository through it.
	Backend adminpath.Backend
	// GatewayURL is advertised to agents; when empty the customer's storage target's gateway_url is used.
	GatewayURL string
	Clock      func() time.Time
	Log        *logx.Logger
	Pub        Publisher
	// HtpasswdSync is signalled after an enrollment created a gateway login.
	HtpasswdSync func()
	// Notifier wakes long polls when a command is enqueued (see Notifier.Notify).
	Notifier *Notifier
	Limits   config.RateLimits

	once        sync.Once
	ids         *ulid.Generator
	enrollIP    *ratelimit.Bucket
	enrollQueue *tokenQueue
	enrollWait  time.Duration // 0: defaultEnrollWait (tests shorten it)
	authFail    *ratelimit.Blocker
	heartbeat   *ratelimit.Bucket
	// heartbeatConflict is the extra heartbeat allowance of a device with an
	// open identity conflict (F9, see conflictHeartbeatMachines).
	heartbeatConflict *ratelimit.Bucket
	runs              *ratelimit.Bucket
	snapshots         *ratelimit.Bucket
	policy            *ratelimit.Bucket
	leases            *ratelimit.Bucket
	pollFlight        *ratelimit.Flight
	touchMu           sync.Mutex
	touched           map[string]time.Time
}

func (s *Service) init() {
	s.once.Do(func() {
		if s.Clock == nil {
			s.Clock = time.Now
		}
		if s.Log == nil {
			s.Log = logx.Default()
		}
		if s.Notifier == nil {
			s.Notifier = NewNotifier()
		}
		l := s.Limits
		if l.EnrollPerMinPerIP == 0 {
			l = config.Default().RateLimits
		}
		s.Limits = l
		s.ids = ulid.NewGenerator(s.Clock, nil)
		now := s.Clock
		s.enrollIP = ratelimit.NewBucket(l.EnrollPerMinPerIP, time.Minute, l.EnrollPerMinPerIP, now)
		s.enrollQueue = newTokenQueue()
		if s.enrollWait <= 0 {
			s.enrollWait = defaultEnrollWait
		}
		s.authFail = ratelimit.NewBlocker(l.FailedAuthPerMinIP, time.Minute, time.Duration(l.MaxBlockMinutes)*time.Minute, now)
		s.heartbeat = ratelimit.NewBucket(1, time.Duration(l.HeartbeatIntervalS)*time.Second, 1, now)
		extra := conflictHeartbeatMachines - 1
		s.heartbeatConflict = ratelimit.NewBucket(extra, time.Duration(l.HeartbeatIntervalS)*time.Second, extra, now)
		s.runs = ratelimit.NewBucket(l.RunsPerMin, time.Minute, l.RunsPerMin, now)
		s.snapshots = ratelimit.NewBucket(l.SnapshotsPerMin, time.Minute, l.SnapshotsPerMin, now)
		s.policy = ratelimit.NewBucket(l.PolicyPerMin, time.Minute, l.PolicyPerMin, now)
		// Not an operator setting: the agent asks once per start and waits at
		// least api.StartLeaseRetryMin after a refusal.
		s.leases = ratelimit.NewBucket(api.StartLeasePerMinute, time.Minute, api.StartLeasePerMinute, now)
		s.pollFlight = ratelimit.NewFlight()
		s.touched = map[string]time.Time{}
	})
}

// Register adds the agent routes to r: the api.Pattern* constants for enroll,
// policy, heartbeat, runs, snapshots, commands and command result (M0) and
// start-lease (M1, ADR 0005). The other api patterns arrive with their
// milestones (token rotate M5, restores M6, progress M9, release M11, lobby M9)
// and answer 404 until then.
func (s *Service) Register(r httpx.Registrar) {
	s.init()
	r.Handle(api.PatternEnroll, http.HandlerFunc(s.handleEnroll))
	r.Handle(api.PatternPolicy, s.deviceAuth(s.handlePolicy))
	r.Handle(api.PatternHeartbeat, s.deviceAuth(s.handleHeartbeat))
	r.Handle(api.PatternRuns, s.deviceAuth(s.handleRuns))
	r.Handle(api.PatternSnapshots, s.deviceAuth(s.handleSnapshots))
	r.Handle(api.PatternCommands, s.deviceAuth(s.handleCommands))
	r.Handle(api.PatternCommandResult, s.deviceAuth(s.handleCommandResult))
	r.Handle(api.PatternStartLease, s.deviceAuth(s.handleStartLease))
}

// ---- authentication ----

type device struct {
	ID      string
	TokenID string
}

type handlerFunc func(w http.ResponseWriter, r *http.Request, d device)

func bearer(r *http.Request) (string, bool) {
	h := r.Header.Get(api.HeaderAuthorization)
	if !strings.HasPrefix(h, api.BearerPrefix) {
		return "", false
	}
	t := strings.TrimSpace(strings.TrimPrefix(h, api.BearerPrefix))
	return t, t != ""
}

// deviceAuth authenticates the device token. The device id comes from the token
// and nowhere else (architecture §3.5 item 4). Failures are counted per source
// IP; past the §5.5 threshold the IP is blocked with exponential back-off and
// one audit row is written per block, not per request.
func (s *Service) deviceAuth(next handlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.init()
		ip := httpx.ClientIP(r)
		if blocked, wait := s.authFail.Blocked(ip); blocked {
			httpx.Problem(w, r, api.CodeRateLimited, wait)
			return
		}
		tok, ok := bearer(r)
		if !ok || !enroll.WellFormed(tok) {
			s.authFailed(r, ip)
			httpx.Problem(w, r, api.CodeUnauthorized, 0)
			return
		}
		hash := enroll.HashToken(tok)
		devID, dt, err := s.Store.AgentLookupDeviceToken(r.Context(), hash)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				s.authFailed(r, ip)
				httpx.Problem(w, r, api.CodeUnauthorized, 0)
				return
			}
			s.Log.Error("token lookup failed", logx.Err(err))
			httpx.Problem(w, r, api.CodeInternal, 0)
			return
		}
		if subtle.ConstantTimeCompare([]byte(dt.TokenHash), []byte(hash)) != 1 {
			s.authFailed(r, ip)
			httpx.Problem(w, r, api.CodeUnauthorized, 0)
			return
		}
		httpx.SetActor(r.Context(), "device", devID)
		s.touch(r.Context(), dt.ID, ip)
		next(w, r, device{ID: devID, TokenID: dt.ID})
	})
}

func (s *Service) authFailed(r *http.Request, ip string) {
	if s.authFail.Fail(ip) {
		_, _ = s.Store.AppendAudit(context.WithoutCancel(r.Context()), store.AuditEvent{ActorType: "system", Method: "agent", SourceIP: ip,
			Action: "agent.auth.blocked", Result: "denied", Severity: "warning", Details: []byte(`{"reason":"repeated_failed_token"}`)})
	}
}

// touch records last_used at most once every five minutes per token: it is a
// write on the hot path.
func (s *Service) touch(ctx context.Context, tokenID, ip string) {
	now := s.Clock()
	s.touchMu.Lock()
	last := s.touched[tokenID]
	due := now.Sub(last) >= 5*time.Minute
	if due {
		s.touched[tokenID] = now
		if len(s.touched) > 8192 {
			for k, t := range s.touched {
				if now.Sub(t) > time.Hour {
					delete(s.touched, k)
				}
			}
		}
	}
	s.touchMu.Unlock()
	if due {
		_ = s.Store.AgentTouchDeviceToken(context.WithoutCancel(ctx), tokenID, ip)
	}
}

// ---- helpers ----

// decode reads a JSON body of at most max bytes into v. Unknown fields are
// tolerated (forward compatibility); trailing data, an oversized body and
// malformed JSON are not. It writes the problem response itself and returns false.
func decode(w http.ResponseWriter, r *http.Request, max int64, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, max)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			httpx.Problem(w, r, api.CodePayloadTooLarge, 0)
		} else {
			httpx.Problem(w, r, api.CodeInvalidRequest, 0)
		}
		return false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		httpx.Problem(w, r, api.CodeInvalidRequest, 0)
		return false
	}
	return true
}

// checkBody runs Validate and AssertPathFree on a decoded request; either failing is a 400.
func checkBody(w http.ResponseWriter, r *http.Request, v interface{ Validate() error }) bool {
	if err := v.Validate(); err != nil {
		httpx.Problem(w, r, api.CodeInvalidRequest, 0)
		return false
	}
	if err := api.AssertPathFree(v); err != nil {
		httpx.Problem(w, r, api.CodeInvalidRequest, 0)
		return false
	}
	return true
}

func (s *Service) notifyChanged(deviceID string) {
	if s.Pub != nil {
		s.Pub.DeviceChanged(deviceID)
	}
}

// effectiveETag is the ETag the console tells the agent: the policy's own ETag,
// with a marker while the device is paused so that pausing and resuming change
// the ETag (a 304 must never hide a pause transition).
func effectiveETag(policyETag string, paused bool) string {
	if !paused {
		return policyETag
	}
	return strings.TrimSuffix(policyETag, `"`) + `-paused"`
}

func pausedNow(d store.Device, now time.Time) bool {
	return d.PausedAt != nil && (d.PausedUntil == nil || d.PausedUntil.After(now))
}
