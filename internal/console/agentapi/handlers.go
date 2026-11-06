package agentapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/console/httpx"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
)

func (s *Service) limited(w http.ResponseWriter, r *http.Request, ok bool, wait time.Duration) bool {
	if ok {
		return false
	}
	httpx.Problem(w, r, api.CodeRateLimited, wait)
	return true
}

// ---- GET /policy ----

// handlePolicy is the ETag pull of the effective policy (DH6). 304 when the
// agent's If-None-Match equals the current ETag and nothing else is pending; a
// pending gateway secret (DH13) or a pause always travels in a 200.
func (s *Service) handlePolicy(w http.ResponseWriter, r *http.Request, d device) {
	if ok, wait := s.policy.Allow(d.ID); s.limited(w, r, ok, wait) {
		return
	}
	ctx := r.Context()
	now := s.Clock().UTC()
	eff, err := s.Store.AgentEffectivePolicyDoc(ctx, d.ID)
	if err != nil {
		s.Log.Error("policy: compose failed", logx.ID("device_id", d.ID), logx.Err(err))
		httpx.Problem(w, r, api.CodeInternal, 0)
		return
	}
	dev, err := s.Store.GetDevice(ctx, store.ScopeAll(), d.ID)
	if err != nil {
		httpx.Problem(w, r, api.CodeInternal, 0)
		return
	}
	paused := pausedNow(dev, now)
	etag := effectiveETag(eff.ETag, paused)

	var pending *api.GatewaySecret
	if s.Keys.MasterKeyLoaded() {
		switch pw, err := s.Keys.GatewaySecret(ctx, d.ID); {
		case err == nil:
			pending = &api.GatewaySecret{Password: api.Secret(pw), IssuedAt: now}
		case errors.Is(err, store.ErrNotFound):
		default:
			s.Log.Warn("policy: pending gateway secret unreadable", logx.ID("device_id", d.ID), logx.Class("unwrap"))
		}
	}
	h := w.Header()
	h.Set(api.HeaderETag, etag)
	if pending == nil && !paused && r.Header.Get(api.HeaderIfNoneMatch) == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	resp := api.PolicyResponse{PolicyID: eff.PolicyID, Policy: eff.Policy, Version: eff.Version, ETag: etag, PendingGatewaySecret: pending, Paused: paused, ServerTime: now}
	if paused && dev.PausedUntil != nil {
		u := dev.PausedUntil.UTC()
		resp.PausedUntil = &u
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}

// ---- POST /heartbeat ----

// heartbeatProgress is the progress document kept in device_presence: counts
// only, and none of the fields that move on every heartbeat even when the run is
// stalled (the store derives "progress advanced" from changes in this document).
type heartbeatProgress struct {
	RunID       string  `json:"run_id"`
	Kind        string  `json:"kind"`
	PercentDone float64 `json:"percent_done"`
	BytesDone   uint64  `json:"bytes_done"`
	TotalBytes  uint64  `json:"total_bytes"`
	FilesDone   uint64  `json:"files_done"`
	TotalFiles  uint64  `json:"total_files"`
	ErrorCount  uint64  `json:"error_count"`
	Stalled     bool    `json:"stalled"`
}

// handleHeartbeat records presence and the 24 h ring. The first heartbeat after
// enrollment confirms it (AgentUpsertPresence stamps enroll_confirmed_at), a
// gateway_secret_acked flag erases the pending wrapped secret, and the response
// carries the clock offset (DH5), whether commands wait and the current policy ETag.
func (s *Service) handleHeartbeat(w http.ResponseWriter, r *http.Request, d device) {
	if ok, wait := s.heartbeatAllowed(r.Context(), d.ID); s.limited(w, r, ok, wait) {
		return
	}
	var req api.HeartbeatRequest
	if !decode(w, r, api.MaxHeartbeatBody, &req) || !checkBody(w, r, req) {
		return
	}
	ctx := r.Context()
	now := s.Clock().UTC()
	offset := api.ClockOffset(req.DeviceTime, now)
	p := store.Presence{
		DeviceID: d.ID, ReceivedAt: now, AgentVersion: req.AgentVersion, ResticVersion: req.ResticVersion,
		UptimeS: &req.UptimeS, OnAC: req.OnAC, BatteryPct: req.BatteryPct, Metered: req.Metered, VPN: req.VPN,
		QueueDepth: &req.QueueDepth, JournalDepth: &req.JournalDepth, ClockOffsetMS: &offset,
		ServiceState: string(req.ServiceState), LastErrorClass: string(req.LastErrorClass), SourceIP: httpx.ClientIP(r),
	}
	if req.PolicyVersionApplied > 0 {
		v := req.PolicyVersionApplied
		p.PolicyVersionApplied = &v
	}
	if pr := req.Progress; pr != nil {
		raw, _ := json.Marshal(heartbeatProgress{RunID: pr.RunID, Kind: string(pr.Kind), PercentDone: pr.PercentDone, BytesDone: pr.BytesDone,
			TotalBytes: pr.TotalBytes, FilesDone: pr.FilesDone, TotalFiles: pr.TotalFiles, ErrorCount: pr.ErrorCount, Stalled: pr.Stalled})
		p.Progress = raw
	}
	if err := s.Store.AgentUpsertPresence(ctx, d.ID, p); err != nil {
		s.Log.Error("heartbeat: store failed", logx.ID("device_id", d.ID), logx.Err(err))
		httpx.Problem(w, r, api.CodeInternal, 0)
		return
	}
	if req.GatewaySecretAcked {
		if err := s.Store.AgentAckGatewaySecret(ctx, d.ID); err != nil {
			s.Log.Error("heartbeat: ack failed", logx.ID("device_id", d.ID), logx.Err(err))
		}
	}
	s.noteFingerprint(ctx, d.ID, req.HWFingerprint, httpx.ClientIP(r))
	s.renewLease(ctx, d.ID, req, now)
	resp := api.HeartbeatResponse{ServerTime: now, ClockOffsetMS: offset, CommandsPending: s.commandsPending(ctx, d.ID, now)}
	if _, _, etag, err := s.Store.AgentEffectivePolicy(ctx, d.ID); err == nil {
		dev, derr := s.Store.GetDevice(ctx, store.ScopeAll(), d.ID)
		resp.PolicyETag = effectiveETag(etag, derr == nil && pausedNow(dev, now))
	}
	s.notifyChanged(d.ID)
	if req.Progress != nil && s.Pub != nil {
		s.Pub.RunProgress(d.ID, req.Progress.RunID)
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}

// conflictHeartbeatMachines is how many machines may heartbeat at the normal
// cadence with one device token while the device has an open identity conflict
// (F9). ADR 0011: detection must never lock anyone out, and a cloned pair (or a
// few copies of one image) shares the token and so the per-device bucket. A
// flagged device gets this many heartbeats per interval instead of one; a single
// client hammering faster than that is still refused.
const conflictHeartbeatMachines = 4

// heartbeatAllowed is the per-device heartbeat limit: one per interval, and up
// to conflictHeartbeatMachines while identity_conflict_at is set. The device is
// read only on the refusal path, so the normal heartbeat costs nothing extra.
func (s *Service) heartbeatAllowed(ctx context.Context, deviceID string) (bool, time.Duration) {
	ok, wait := s.heartbeat.Allow(deviceID)
	if ok {
		return true, 0
	}
	dev, err := s.Store.GetDevice(ctx, store.ScopeAll(), deviceID)
	if err != nil || dev.IdentityConflictAt == nil {
		return false, wait
	}
	return s.heartbeatConflict.Allow(deviceID)
}

// noteFingerprint compares the heartbeat's hardware fingerprint with the one
// recorded for the device (M2-W3B). A different fingerprint raises the identity
// conflict once; the heartbeat itself is never refused for it, so a cloned or
// reimaged machine keeps reporting and the board shows IDENTITY_CONFLICT. A
// store failure is logged at error level (the check is retried by the next
// heartbeat) and never fails the heartbeat. The fingerprint is never logged.
func (s *Service) noteFingerprint(ctx context.Context, deviceID, fp, ip string) {
	out, err := s.Store.AgentNoteFingerprint(ctx, deviceID, fp, ip)
	switch {
	case err != nil:
		s.Log.Error("heartbeat: identity check failed", logx.ID("device_id", deviceID), logx.Err(err))
	case out == store.FingerprintConflictRaised:
		s.Log.Warn("heartbeat: identity conflict raised", logx.ID("device_id", deviceID))
	}
}

// renewLease extends the start lease of the run the heartbeat names (ADR
// 0005): active_run_id, or the progress run id of an agent that predates it. A
// failure is logged and never fails the heartbeat: the lease then simply
// expires, which fails open.
func (s *Service) renewLease(ctx context.Context, deviceID string, req api.HeartbeatRequest, now time.Time) {
	runID := req.ActiveRunID
	if runID == "" && req.Progress != nil {
		runID = req.Progress.RunID
	}
	if runID == "" {
		return
	}
	if _, err := s.Store.AgentRenewStartLease(ctx, deviceID, runID, now); err != nil {
		s.Log.Warn("heartbeat: start lease not renewed", logx.ID("device_id", deviceID), logx.Err(err))
	}
}

// ---- POST /start-lease ----

// handleStartLease decides a fleet start lease (ADR 0005). Granted and refused
// are both 200; the refusal carries retry_after_s (the earliest live lease's
// expiry, clamped to 60..900 s, never random). A target without a cap answers
// granted with no lease_until and stores nothing. A run id that holds another
// device's lease is 409.
func (s *Service) handleStartLease(w http.ResponseWriter, r *http.Request, d device) {
	if ok, wait := s.leases.Allow(d.ID); s.limited(w, r, ok, wait) {
		return
	}
	var req api.StartLeaseRequest
	if !decode(w, r, api.MaxStartLeaseBody, &req) || !checkBody(w, r, req) {
		return
	}
	now := s.Clock().UTC()
	dec, err := s.Store.AgentStartLease(r.Context(), d.ID, req.RunID, now)
	switch {
	case err == nil:
	case errors.Is(err, store.ErrConflict):
		httpx.Problem(w, r, api.CodeConflict, 0)
		return
	case errors.Is(err, store.ErrInvalid):
		httpx.Problem(w, r, api.CodeInvalidRequest, 0)
		return
	default:
		s.Log.Error("start lease: store failed", logx.ID("device_id", d.ID), logx.Err(err))
		httpx.Problem(w, r, api.CodeInternal, 0)
		return
	}
	resp := api.StartLeaseResponse{Granted: dec.Granted, ServerTime: now}
	switch {
	case dec.Granted && !dec.Unlimited:
		until := dec.LeaseUntil.UTC()
		resp.LeaseUntil = &until
	case !dec.Granted:
		resp.RetryAfterS = int(dec.RetryAfter / time.Second)
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}

func (s *Service) commandsPending(ctx context.Context, deviceID string, now time.Time) bool {
	l, err := s.Store.ListCommands(ctx, store.ScopeAll(), deviceID, store.Page{Limit: 50})
	if err != nil {
		return false
	}
	for _, c := range l.Items {
		if (c.Status == "queued" || c.Status == "delivered") && c.ExpiresAt.After(now) {
			return true
		}
	}
	return false
}

// ---- POST /runs ----

// runInput maps a wire RunRecord onto the store's RunInput. The stats document
// is the api.RunStats marshalled as is, so it carries exactly the keys the facts
// loader reads (total_files_processed, failed_files, vss_fallback_volumes);
// error_counts is {class: n} summed from the path-free projection.
func runInput(rec api.RunRecord) (store.RunInput, error) {
	stats, err := json.Marshal(rec.Stats)
	if err != nil {
		return store.RunInput{}, err
	}
	counts := map[string]int{}
	for _, e := range rec.ErrorCounts {
		counts[string(e.Class)] += e.Count
	}
	added := int64(rec.Stats.DataAdded)
	in := store.RunInput{
		ID: rec.ID, Kind: string(rec.Kind), Trigger: string(rec.Trigger), StartedAt: rec.StartedAt, FinishedAt: rec.FinishedAt,
		Outcome: string(rec.Outcome), ResticExitCode: rec.ExitCode, SnapshotID: rec.SnapshotID, Stats: stats, DataAddedBytes: &added,
		ErrorCounts: counts, ErrorClass: string(rec.ErrorClass), Attempt: rec.Attempt, DeferredReason: string(rec.DeferredReason),
		PolicyVersion: rec.PolicyVersion, AgentVersion: rec.AgentVersion,
	}
	return in, nil
}

// handleRuns ingests a batch idempotently on the run id. Each record is
// validated on its own: an invalid record is rejected (reason invalid) without
// failing the batch. The body as a whole must pass AssertPathFree (400 otherwise).
func (s *Service) handleRuns(w http.ResponseWriter, r *http.Request, d device) {
	if ok, wait := s.runs.Allow(d.ID); s.limited(w, r, ok, wait) {
		return
	}
	var req api.RunsRequest
	if !decode(w, r, api.MaxRunsBody, &req) {
		return
	}
	if n := len(req.Runs); n < 1 || n > api.MaxRunsPerRequest {
		httpx.Problem(w, r, api.CodeInvalidRequest, 0)
		return
	}
	if err := api.AssertPathFree(req); err != nil {
		httpx.Problem(w, r, api.CodeInvalidRequest, 0)
		return
	}
	now := s.Clock().UTC()
	results := make([]api.RunResult, len(req.Runs))
	var inputs []store.RunInput
	var index []int
	unusable := false
	for i, rec := range req.Runs {
		results[i] = api.RunResult{ID: rec.ID}
		if err := rec.Validate(); err != nil {
			results[i].Status, results[i].Reason = api.RunRejected, api.ReasonInvalid
			continue
		}
		// A label that is not a version loses the label, never the run: the value is not stored, shown or logged.
		if rec.AgentVersion != "" && !api.VersionShape(rec.AgentVersion) {
			rec.AgentVersion, unusable = api.UnknownVersion, true
		}
		in, err := runInput(rec)
		if err != nil {
			results[i].Status, results[i].Reason = api.RunRejected, api.ReasonInvalid
			continue
		}
		inputs, index = append(inputs, in), append(index, i)
	}
	if unusable {
		s.Log.Warn("runs: an agent version was not a build version, so it is stored as unknown", logx.ID("device_id", d.ID), logx.Class("agent_version_unusable"))
	}
	if len(inputs) > 0 {
		out, err := s.Store.AgentIngestRuns(r.Context(), d.ID, inputs, now)
		if err != nil {
			s.Log.Error("runs: ingest failed", logx.ID("device_id", d.ID), logx.Err(err))
			httpx.Problem(w, r, api.CodeInternal, 0)
			return
		}
		for k, res := range out {
			i := index[k]
			results[i].Status = api.RunStatus(res.Status)
			switch res.Reason {
			case store.ReasonWrongDevice:
				results[i].Reason = api.ReasonConflict
			case "":
			default:
				results[i].Reason = api.ReasonInvalid
			}
		}
		s.notifyChanged(d.ID)
	}
	httpx.WriteJSON(w, http.StatusOK, api.RunsResponse{Results: results, ServerTime: now})
}

// ---- POST /snapshots ----

func (s *Service) handleSnapshots(w http.ResponseWriter, r *http.Request, d device) {
	if ok, wait := s.snapshots.Allow(d.ID); s.limited(w, r, ok, wait) {
		return
	}
	var req api.SnapshotsRequest
	if !decode(w, r, api.MaxSnapshotsBody, &req) || !checkBody(w, r, req) {
		return
	}
	list := make([]store.Snapshot, 0, len(req.Snapshots))
	for _, sn := range req.Snapshots {
		var summary json.RawMessage
		if sn.Summary != nil {
			summary, _ = json.Marshal(sn.Summary)
		}
		list = append(list, store.Snapshot{DeviceID: d.ID, SnapshotID: sn.ID, TakenAt: sn.Time, ParentID: sn.Parent, Hostname: sn.Hostname,
			Paths: sn.Paths, Tags: sn.Tags, ProgramVersion: sn.ProgramVersion, Summary: summary})
	}
	up, rm, err := s.Store.AgentReplaceSnapshots(r.Context(), d.ID, list)
	if err != nil {
		if errors.Is(err, store.ErrInvalid) {
			httpx.Problem(w, r, api.CodeInvalidRequest, 0)
			return
		}
		s.Log.Error("snapshots: store failed", logx.ID("device_id", d.ID), logx.Err(err))
		httpx.Problem(w, r, api.CodeInternal, 0)
		return
	}
	s.notifyChanged(d.ID)
	httpx.WriteJSON(w, http.StatusOK, api.SnapshotsResponse{Upserted: up, Removed: rm, ServerTime: s.Clock().UTC()})
}

// ---- GET /commands (long poll) and POST /commands/{id}/result ----

func toWire(c store.Command) api.Command {
	return api.Command{ID: c.ID, Type: api.CommandType(c.Type), Params: c.Params, CreatedAt: c.CreatedAt, ExpiresAt: c.ExpiresAt}
}

// handleCommands holds the request for up to ?wait= seconds (at most 30) until
// a command is queued for the device, which Notifier.Notify wakes at once. One
// poll per device may be in flight: a second answers 409.
func (s *Service) handleCommands(w http.ResponseWriter, r *http.Request, d device) {
	wait := time.Duration(0)
	if q := r.URL.Query().Get(api.QueryWait); q != "" {
		n, err := strconv.Atoi(q)
		if err != nil || n < 0 {
			httpx.Problem(w, r, api.CodeInvalidRequest, 0)
			return
		}
		wait = time.Duration(n) * time.Second
	}
	if maxWait := time.Duration(s.Limits.CommandsLongPollMaxS) * time.Second; wait > maxWait {
		wait = maxWait
	}
	release, ok := s.pollFlight.TryAcquire(d.ID)
	if !ok {
		httpx.Problem(w, r, api.CodeConflict, 0)
		return
	}
	defer release()
	ctx := r.Context()
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Time{})
	_ = rc.SetWriteDeadline(time.Now().Add(wait + 30*time.Second))

	wake := s.Notifier.Chan(d.ID) // before the check: no lost wake-up
	cmds, err := s.Store.AgentNextCommands(ctx, d.ID, 10)
	if err != nil {
		httpx.Problem(w, r, api.CodeInternal, 0)
		return
	}
	if len(cmds) == 0 && wait > 0 {
		t := time.NewTimer(wait)
		select {
		case <-wake:
			cmds, err = s.Store.AgentNextCommands(ctx, d.ID, 10)
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return
		}
		t.Stop()
		if err != nil {
			httpx.Problem(w, r, api.CodeInternal, 0)
			return
		}
	}
	out := make([]api.Command, 0, len(cmds))
	for _, c := range cmds {
		out = append(out, toWire(c))
	}
	now := s.Clock().UTC()
	w.Header().Set(api.HeaderServerTime, api.FormatServerTime(now))
	httpx.WriteJSON(w, http.StatusOK, api.CommandsResponse{Commands: out, ServerTime: now})
}

func (s *Service) handleCommandResult(w http.ResponseWriter, r *http.Request, d device) {
	id := r.PathValue(api.PathValueID)
	var req api.CommandResultRequest
	if !decode(w, r, api.MaxCommandResultBody, &req) || !checkBody(w, r, req) {
		return
	}
	result := req.Result
	if req.ErrorClass != "" {
		obj := map[string]json.RawMessage{}
		if len(result) > 0 {
			_ = json.Unmarshal(result, &obj)
		}
		obj["error_class"], _ = json.Marshal(string(req.ErrorClass))
		result, _ = json.Marshal(obj)
	}
	_, err := s.Store.AgentAckCommand(r.Context(), d.ID, id, store.CommandAck{Status: string(req.Status), Result: result})
	switch {
	case err == nil:
		s.notifyChanged(d.ID)
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrConflict):
		// A result the console cannot apply (an id it does not know or that belongs to
		// another device, a different final status, a command cancelled or expired before
		// delivery) is answered 204 and ignored, with a log line. The deployed agent keeps
		// a result unposted until it gets a 2xx and does not poll for commands while one is
		// unposted, so any 4xx here would stop that agent taking commands for good.
		s.Log.Warn("command result not applied", logx.Class("command_result_ignored"), logx.ID("command_id", id), logx.Str("status", string(req.Status)))
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, store.ErrInvalid):
		httpx.Problem(w, r, api.CodeInvalidRequest, 0)
	default:
		httpx.Problem(w, r, api.CodeInternal, 0)
	}
}
