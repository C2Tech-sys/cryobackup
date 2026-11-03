package agentapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/console/enroll"
	"github.com/c2tech-sys/cryobackup/internal/console/escrow"
	"github.com/c2tech-sys/cryobackup/internal/console/gateway"
	"github.com/c2tech-sys/cryobackup/internal/console/httpx"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// enrollRetry is the Retry-After the agent is given when the repository could
// not be initialised: the device stays in place and the same nonce retries.
const enrollRetry = 10 * time.Second

func (s *Service) audit(ctx context.Context, ev store.AuditEvent) {
	_, _ = s.Store.AppendAudit(context.WithoutCancel(ctx), ev)
}

// handleEnroll is POST /api/v1/agent/enroll (architecture §5.1, §7.2, DH17, DH22).
//
// Order of work: per-IP limit, one enrollment at a time per token (a later one
// waits up to enrollWait, then 429), master key
// present (else 503, C12: an unwrapped key is never stored), strict body checks
// and AssertPathFree, token lookup, refusal of customer-held custody (501, DH22),
// device id generation, secret generation and wrapping under that id,
// store.AgentEnroll (one transaction that spends the token use, creates the
// device under the given id, token, gateway login, escrow row and schedule),
// repository init and a CatConfig confirmation through the admin path (503 with
// Retry-After on failure, device left in place for the retry), recorded as the
// escrow verification, the htpasswd-sync signal and the response.
func (s *Service) handleEnroll(w http.ResponseWriter, r *http.Request) {
	s.init()
	ctx := r.Context()
	ip := httpx.ClientIP(r)
	if ok, wait := s.enrollIP.Allow(ip); !ok {
		s.audit(ctx, store.AuditEvent{ActorType: "system", Method: "agent", SourceIP: ip, Action: "agent.enroll.denied", Result: "denied",
			Severity: "warning", Details: []byte(`{"reason":"rate_limited"}`)})
		httpx.Problem(w, r, api.CodeRateLimited, wait)
		return
	}
	if blocked, wait := s.authFail.Blocked(ip); blocked {
		httpx.Problem(w, r, api.CodeRateLimited, wait)
		return
	}
	plain, ok := bearer(r)
	if !ok || !enroll.WellFormed(plain) {
		s.authFailed(r, ip)
		httpx.Problem(w, r, api.CodeUnauthorized, 0)
		return
	}
	tokenHash := enroll.HashToken(plain)
	// One enrollment at a time per token: later ones wait their turn (F4: an RMM
	// push of a site on one token arrives in the same second) and are refused
	// only when the wait runs out.
	release, ok := s.enrollQueue.acquire(ctx, tokenHash, s.enrollWait)
	if !ok {
		if ctx.Err() == nil {
			s.audit(ctx, store.AuditEvent{ActorType: "system", Method: "agent", SourceIP: ip, Action: "agent.enroll.denied", Result: "denied",
				Severity: "warning", Details: []byte(`{"reason":"token_busy"}`)})
		}
		httpx.Problem(w, r, api.CodeRateLimited, enrollBusyRetry)
		return
	}
	defer release()

	if !s.Keys.MasterKeyLoaded() {
		httpx.Problem(w, r, api.CodeMasterKeyUnavailable, 30*time.Second)
		return
	}
	var req api.EnrollRequest
	if !decode(w, r, api.MaxEnrollBody, &req) || !checkBody(w, r, req) {
		return
	}

	tok, err := s.Store.GetEnrollmentTokenByHash(ctx, tokenHash)
	found := err == nil
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.Log.Error("enroll: token lookup failed", logx.Err(err))
		httpx.Problem(w, r, api.CodeInternal, 0)
		return
	}
	now := s.Clock()
	if !found || tok.RevokedAt != nil || !tok.ExpiresAt.After(now) {
		s.authFailed(r, ip)
		s.audit(ctx, store.AuditEvent{ActorType: "system", Method: "agent", SourceIP: ip, Action: "agent.enroll.denied", Result: "denied",
			Severity: "warning", Details: []byte(`{"reason":"token"}`)})
		httpx.Problem(w, r, api.CodeUnauthorized, 0)
		return
	}
	cust, err := s.Store.GetCustomer(ctx, store.ScopeAll(), tok.CustomerID)
	if err != nil {
		httpx.Problem(w, r, api.CodeInternal, 0)
		return
	}
	if cust.KeyCustody != "escrowed" {
		// DH22: representable from M0, refused until M13.
		httpx.ProblemStatus(w, r, api.CodeCustodyCustomerHeld, http.StatusNotImplemented, 0)
		return
	}

	// Secrets: shown to the agent once, held here only wrapped or hashed.
	deviceTok, deviceTokHash, err := enroll.NewToken()
	if err != nil {
		httpx.Problem(w, r, api.CodeInternal, 0)
		return
	}
	gwPlain, err := gateway.NewPassword(nil)
	if err != nil {
		httpx.Problem(w, r, api.CodeInternal, 0)
		return
	}
	gwHash, err := gateway.HashPassword(gwPlain)
	if err != nil {
		httpx.Problem(w, r, api.CodeInternal, 0)
		return
	}
	var rk [32]byte
	if _, err := rand.Read(rk[:]); err != nil {
		httpx.Problem(w, r, api.CodeInternal, 0)
		return
	}
	repoPlain := hex.EncodeToString(rk[:])
	// The device id is chosen here, before anything is wrapped, so the repository
	// key and the gateway secret are bound to the real id from the first write.
	// A replay of a known nonce keeps the stored device and its stored wraps.
	deviceID, err := s.ids.New()
	if err != nil {
		httpx.Problem(w, r, api.CodeInternal, 0)
		return
	}
	repoWrapped, err := s.Keys.Master.WrapRepoKey(deviceID, 1, []byte(repoPlain))
	if err != nil {
		httpx.Problem(w, r, api.CodeMasterKeyUnavailable, 30*time.Second)
		return
	}
	gwWrapped, err := s.Keys.Master.Wrap(escrow.ContextGatewaySecret, deviceID, 1, []byte(gwPlain))
	if err != nil {
		httpx.Problem(w, r, api.CodeMasterKeyUnavailable, 30*time.Second)
		return
	}

	res, err := s.Store.AgentEnroll(ctx, store.EnrollInput{
		TokenHash: tokenHash, Nonce: req.Nonce, DeviceID: deviceID, Hostname: req.Hostname, OSVersion: req.OSVersion, Arch: string(req.Arch),
		DeviceClass: string(req.DeviceClass), HWFingerprint: req.HWFingerprint, AgentVersion: req.AgentVersion, ResticVersion: req.ResticVersion,
		DeviceTokenHash: deviceTokHash, GatewayPasswordHash: gwHash, GatewaySecretWrapped: gwWrapped,
		RepoKeyWrapped: repoWrapped, WrapAlg: escrow.WrapAlg, MasterKeyID: s.Keys.Master.KeyID(), SourceIP: ip,
	})
	if err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			s.authFailed(r, ip)
			httpx.Problem(w, r, api.CodeUnauthorized, 0)
		case errors.Is(err, store.ErrConflict):
			// A spent, revoked or expired token is an authentication failure; a nonce
			// reused after confirmation (or by another token or machine) is a conflict.
			if known, _ := s.Store.EnrollNonceKnown(ctx, req.Nonce); tok.MaxUses != nil && tok.UsedCount >= *tok.MaxUses && !known {
				httpx.Problem(w, r, api.CodeUnauthorized, 0)
			} else {
				httpx.Problem(w, r, api.CodeConflict, 0)
			}
		case errors.Is(err, store.ErrInvalid):
			httpx.Problem(w, r, api.CodeInvalidRequest, 0)
		default:
			s.Log.Error("enroll: store failed", logx.Err(err))
			httpx.Problem(w, r, api.CodeInternal, 0)
		}
		return
	}
	dev := res.Device
	if res.TokenPolicy == "skipped" {
		// Not silent: the device enrolled under its customer's policy (M2-W3B).
		s.Log.Warn("enroll: token policy not applied", logx.ID("device_id", dev.ID), logx.ID("token_id", dev.EnrollmentTokenID), logx.Class(res.TokenPolicySkipReason))
	}
	if res.Replay {
		// Replay: the secrets come back from the store, not from this request.
		var err1, err2 error
		repoPlain, err1 = s.Keys.Unwrap(ctx, dev.ID)
		gwPlain, err2 = s.Keys.GatewaySecret(ctx, dev.ID)
		if err1 != nil || err2 != nil {
			s.Log.Error("enroll: replay could not unwrap", logx.ID("device_id", dev.ID))
			httpx.Problem(w, r, api.CodeInternal, 0)
			return
		}
	}
	if s.HtpasswdSync != nil {
		s.HtpasswdSync()
	}

	// The repository must exist and the key must open it before the agent is told it is enrolled.
	// Init is result-style for an existing repository whose key does not open it:
	// a nil error with Outcome failed and ErrorClass wrong_password. Any outcome
	// but success is treated exactly like an error: 503, the device stays in
	// place for the DH17 retry and nothing is delivered.
	initRes, err := s.Backend.Init(ctx, adminpath.InitRequest{DeviceRequest: adminpath.DeviceRequest{DeviceID: dev.ID, RequestID: httpx.RequestID(ctx)}})
	if err != nil || initRes.Outcome != restic.OutcomeSuccess {
		class := backendClass(err)
		if err == nil {
			class = string(initRes.ErrorClass)
			if class == "" {
				class = "init_" + string(initRes.Outcome)
			}
		}
		s.Log.Warn("enroll: repository init failed", logx.ID("device_id", dev.ID), logx.Class(class))
		httpx.Problem(w, r, api.CodeUnavailable, enrollRetry)
		return
	}
	if _, err := s.Backend.CatConfig(ctx, adminpath.DeviceRequest{DeviceID: dev.ID, RequestID: httpx.RequestID(ctx)}); err != nil {
		s.Log.Warn("enroll: repository check failed", logx.ID("device_id", dev.ID), logx.Class(backendClass(err)))
		httpx.Problem(w, r, api.CodeUnavailable, enrollRetry)
		return
	}
	// That cat config opened the repository with the escrowed key through the
	// admin path: it is the escrow verification (F7, ADR 0014). No audit row of
	// its own: agent.enroll below covers it. A failed write is logged and does
	// not fail the enrollment; the device then shows ESCROW_UNVERIFIED after the
	// grace, which is loud, and the maintenance tick verifies it again.
	if err := s.Store.RecordEscrowVerification(context.WithoutCancel(ctx), store.ScopeAll(), dev.ID, store.EscrowVerification{OK: true}); err != nil {
		s.Log.Error("enroll: escrow verification not recorded", logx.ID("device_id", dev.ID), logx.Err(err))
	}

	eff, err := s.Store.EffectivePolicyDoc(ctx, store.ScopeAll(), dev.ID)
	if err != nil {
		s.Log.Error("enroll: policy failed", logx.ID("device_id", dev.ID), logx.Err(err))
		httpx.Problem(w, r, api.CodeInternal, 0)
		return
	}
	gwURL, err := s.gatewayURL(ctx, cust)
	if err != nil {
		s.Log.Error("enroll: no gateway url", logx.ID("device_id", dev.ID))
		httpx.Problem(w, r, api.CodeUnavailable, enrollRetry)
		return
	}
	s.audit(ctx, store.AuditEvent{ActorType: "device", ActorID: dev.ID, ActorLabel: dev.Hostname, Method: "agent", SourceIP: ip, Action: "agent.enroll",
		ObjectType: "device", ObjectID: dev.ID, CustomerID: dev.CustomerID, Result: "ok", Details: replayDetails(res.Replay)})
	s.notifyChanged(dev.ID)

	httpx.WriteJSON(w, http.StatusOK, api.EnrollResponse{
		DeviceID:     dev.ID,
		CustomerID:   dev.CustomerID,
		DeviceToken:  api.Secret(deviceTok),
		Gateway:      api.Gateway{URL: gwURL, Username: res.GatewayUsername, Password: api.Secret(gwPlain)},
		RepoPassword: api.Secret(repoPlain),
		Custody:      api.CustodyEscrowed,
		Policy:       api.PolicyResponse{PolicyID: eff.PolicyID, Policy: eff.Policy, Version: eff.Version, ETag: eff.ETag, ServerTime: s.Clock().UTC()},
		ServerTime:   s.Clock().UTC(),
	})
}

func replayDetails(replay bool) []byte {
	if replay {
		return []byte(`{"replay":true}`)
	}
	return []byte(`{"replay":false}`)
}

func (s *Service) gatewayURL(ctx context.Context, c store.Customer) (string, error) {
	if s.GatewayURL != "" {
		return s.GatewayURL, nil
	}
	targets, err := s.Store.ListStorageTargets(ctx)
	if err != nil {
		return "", err
	}
	for _, t := range targets {
		if t.ID == c.StorageTargetID && t.GatewayURL != "" {
			return t.GatewayURL, nil
		}
	}
	return "", errors.New("agentapi: no gateway url configured")
}

// backendClass names a failed admin-path call without any text from it.
func backendClass(err error) string {
	var ae *adminpath.Error
	switch {
	case errors.As(err, &ae):
		return ae.Code
	case errors.Is(err, adminpath.ErrUnavailable):
		return "unavailable"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	}
	return "internal"
}
