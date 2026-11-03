package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/agent/config"
	"github.com/c2tech-sys/cryobackup/internal/agent/journal"
	"github.com/c2tech-sys/cryobackup/internal/agent/secrets"
	"github.com/c2tech-sys/cryobackup/internal/agent/supervisor"
	"github.com/c2tech-sys/cryobackup/internal/agent/uplink"
	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/buildinfo"
	"github.com/c2tech-sys/cryobackup/internal/policy"
	"github.com/c2tech-sys/cryobackup/internal/ulid"
)

// ErrAlreadyEnrolled is returned by Enroll when agent.json already holds a
// device id. Re-enrolling needs the old state removed first (a new device).
var ErrAlreadyEnrolled = errors.New("agent: this device is already enrolled")

// EnrollOptions configures Enroll.
type EnrollOptions struct {
	// StateDir is the state directory; empty means config.DefaultStateDir().
	StateDir string
	// ServerURL is the console's agent listener base URL.
	ServerURL string
	// Token is the enrollment token (a Bearer credential, never logged).
	Token string
	// InsecureLab allows an http:// console and skips TLS verification.
	InsecureLab bool
	// SPKIPin and Proxy are carried into agent.json.
	SPKIPin string
	Proxy   string
	// InsecurePlaintextSecrets selects the plaintext secret store (CI only).
	InsecurePlaintextSecrets bool
	// AllowUnprotectedState is the lab flag --insecure-unprotected-state
	// (default false): enroll into a state directory that is not verified as
	// SYSTEM + Administrators only (an unelevated lab enrollment). Without it
	// such an enrollment is refused before the console is contacted.
	AllowUnprotectedState bool
	// RepairACL is --repair-acl (default false): an elevated enrollment first
	// moves an untrusted state directory aside whole (PrepareState) and then
	// enrolls into the fresh one.
	RepairACL bool
	// Clock is the device clock; nil means time.Now.
	Clock func() time.Time
	// HTTPClient replaces the built client (tests).
	HTTPClient *http.Client
	// Log, when set, receives one line per enrollment retry (the uplink error's
	// fixed vocabulary and the wait: no path, no token). Callers give it their
	// stderr, which the MSI log captures.
	Log io.Writer

	// RetryBudget bounds the time Enroll spends retrying answers that mean "try
	// again later" (enroll_retry.go): zero means EnrollRetryBudget (3 minutes),
	// negative means a single attempt. The commands set it from
	// CRYO_ENROLL_RETRY_SECONDS.
	RetryBudget time.Duration

	// Test seams for the retry loop: the sleep and the random source.
	sleep func(context.Context, time.Duration) error
	rand  func() float64
}

// EnrollResult summarises a successful enrollment (nothing secret).
type EnrollResult struct {
	DeviceID      string
	Custody       api.Custody
	PolicyVersion int
	GatewayURL    string
	// ACLApplied is false when the process was not elevated: the state
	// directory is then only as private as its parent.
	ACLApplied bool
	// StateWarning is non-empty when the enrollment went into an unverified
	// state directory under --insecure-unprotected-state (no path).
	StateWarning string
	// Quarantined is where --repair-acl moved an untrusted state directory
	// before this enrollment (empty when nothing was moved).
	Quarantined string
	// EventLogErr is non-empty when the Event Log could not be opened or
	// written during a --repair-acl (the repair itself went ahead). Valid on
	// an error return too. It names no path.
	EventLogErr string
}

// Enroll performs the one-time enrollment (architecture §5.1, DH17): it
// persists the idempotency nonce before the first request, calls POST /enroll
// with the enrollment token, and stores what the console delivers once: the
// device token, the gateway password and the repository password go to the
// secret store, the first policy to the journal, the connection facts to
// agent.json (written last, so a device id means a complete enrollment). A
// retry after a lost response reuses the nonce; the console then issues a
// fresh device token and revokes the earlier one, and the latest response wins.
// Enroll itself retries the answers that mean "try again later" (enroll_retry.go),
// each with that same nonce.
func Enroll(ctx context.Context, o EnrollOptions) (EnrollResult, error) {
	var res EnrollResult
	if o.StateDir == "" {
		o.StateDir = config.DefaultStateDir()
	}
	clock := o.Clock
	if clock == nil {
		clock = time.Now
	}
	if strings.TrimSpace(o.Token) == "" {
		return res, errors.New("agent: an enrollment token is required")
	}
	layout := config.Layout{Root: o.StateDir}
	// FIX20: a --repair-acl quarantine from enroll writes Event Log 1007 like
	// "run" does; a logging failure never stops the repair and is returned in
	// EventLogErr for the command to print.
	sink, closeEvents, eventWarn := repairSink(o.RepairACL)
	st, err := PrepareState(o.StateDir, StateOptions{AllowUnprotected: o.AllowUnprotectedState, Repair: o.RepairACL, sink: sink})
	closeEvents()
	res.EventLogErr = st.EventLogErr
	if res.EventLogErr == "" {
		res.EventLogErr = eventWarn
	}
	if err != nil {
		return res, err
	}
	res.ACLApplied, res.StateWarning, res.Quarantined = st.Applied, st.Warning, st.Quarantined

	cfg, err := config.Load(layout.ConfigFile())
	switch {
	case errors.Is(err, config.ErrNotFound):
		cfg = config.Config{}
	case err != nil:
		return res, err
	case cfg.Enrolled():
		return res, ErrAlreadyEnrolled
	}
	cfg.ServerURL, cfg.InsecureLab, cfg.SPKIPin, cfg.Proxy = o.ServerURL, o.InsecureLab, o.SPKIPin, o.Proxy
	if err := config.ValidateBaseURL(cfg.ServerURL, cfg.InsecureLab); err != nil {
		return res, fmt.Errorf("agent: server URL: %w", err)
	}
	if cfg.EnrollNonce == "" {
		var b [16]byte
		if _, err := rand.Read(b[:]); err != nil {
			return res, err
		}
		cfg.EnrollNonce = hex.EncodeToString(b[:])
	}
	// The nonce is on disk before the request: a lost response is retried with it.
	if err := config.Save(layout.ConfigFile(), cfg); err != nil {
		return res, err
	}

	client, err := uplink.New(uplink.Options{
		BaseURL: cfg.ServerURL, AgentVersion: buildinfo.Version, InsecureLab: cfg.InsecureLab,
		SPKIPin: cfg.SPKIPin, Proxy: cfg.Proxy, Clock: clock, HTTPClient: o.HTTPClient,
	})
	if err != nil {
		return res, err
	}
	h := collectHost()
	req := api.EnrollRequest{
		Nonce: cfg.EnrollNonce, Hostname: h.Hostname, OS: h.OS, OSVersion: h.OSVersion, Arch: h.Arch,
		DeviceClass: h.Class, HWFingerprint: h.HWFingerprint, AgentVersion: buildinfo.Version,
		ResticVersion: supervisor.PinnedVersion,
	}
	// Answers that mean "try again later" (429, 503, 502/504, transport
	// failures) are retried for a bounded time with the same request, nonce
	// included (enroll_retry.go).
	resp, err := enrollRetrier{budget: o.RetryBudget, clock: clock, sleep: o.sleep, rand: o.rand, log: o.Log}.call(ctx,
		func(ctx context.Context) (api.EnrollResponse, error) { return client.Enroll(ctx, o.Token, req) })
	if err != nil {
		return res, err
	}

	// Check what arrived before anything is stored.
	switch {
	case !ulid.Valid(resp.DeviceID):
		return res, errors.New("agent: the console returned an invalid device id")
	case !ulid.Valid(resp.CustomerID):
		// The snapshot tag cb.customer: enrollment is the only time it arrives.
		return res, errors.New("agent: the console returned no valid customer id")
	case resp.DeviceToken == "":
		return res, errors.New("agent: the console returned no device token")
	case resp.Gateway.Password == "":
		return res, errors.New("agent: the console returned no gateway credential")
	}
	if err := config.ValidateBaseURL(resp.Gateway.URL, cfg.InsecureLab); err != nil {
		return res, fmt.Errorf("agent: gateway URL from the console: %w", err)
	}
	if resp.Custody != api.CustodyEscrowed && resp.Custody != api.CustodyCustomerHeld {
		return res, errors.New("agent: the console returned an unknown custody")
	}
	if err := resp.Policy.Policy.Validate(); err != nil {
		return res, errors.New("agent: the first policy from the console does not validate")
	}

	store, err := secrets.New(secrets.Options{
		Dir: layout.SecretsDir(), InsecurePlaintext: o.InsecurePlaintextSecrets, AllowUnprotected: o.AllowUnprotectedState,
	})
	if err != nil {
		return res, err
	}
	if err := store.Put(secrets.NameDeviceToken, []byte(resp.DeviceToken)); err != nil {
		return res, err
	}
	if err := store.Put(secrets.NameGatewayCred, []byte(resp.Gateway.Password)); err != nil {
		return res, err
	}
	if resp.RepoPassword != "" {
		if err := store.Put(secrets.NameRepoKey, []byte(resp.RepoPassword)); err != nil {
			return res, err
		}
	} else {
		_ = store.Delete(secrets.NameRepoKey) // customer-held custody: the key is not ours to have
	}

	jr, err := journal.Open(ctx, journal.Options{Path: layout.JournalFile(), Clock: clock})
	if err != nil {
		return res, fmt.Errorf("agent: open journal: %w", err)
	}
	defer jr.Close()
	etag := resp.Policy.ETag
	if etag == "" {
		etag, _ = policy.ETag(resp.Policy.Policy)
	}
	if _, err := persistPolicy(ctx, jr, layout, resp.Policy, etag); err != nil {
		return res, err
	}

	cfg.DeviceID, cfg.GatewayURL, cfg.Custody = resp.DeviceID, strings.TrimRight(resp.Gateway.URL, "/"), string(resp.Custody)
	cfg.CustomerID = resp.CustomerID
	if err := config.Save(layout.ConfigFile(), cfg); err != nil {
		return res, err
	}
	res.DeviceID, res.Custody, res.PolicyVersion, res.GatewayURL = resp.DeviceID, resp.Custody, resp.Policy.Version, cfg.GatewayURL
	return res, nil
}

// CheckIn is the one-shot "cryo-agent check-in": one heartbeat, one policy
// pull and one outbox delivery, run from a separate process. It shares the
// journal with a running service (SQLite's lock) and does not touch its runs.
// The running service picks the new policy up from its next heartbeat.
func CheckIn(ctx context.Context, o Options) error {
	o.OneShot = true
	a, err := New(ctx, o)
	if err != nil {
		return err
	}
	defer a.Close()
	if err := a.sendHeartbeat(ctx); err != nil {
		return err
	}
	if err := a.pullPolicy(ctx); err != nil {
		return err
	}
	return a.deliverOutbox(ctx)
}
