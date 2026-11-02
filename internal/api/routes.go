package api

import (
	"net/url"
	"time"
)

// Version is the protocol version carried in the URL path (architecture §5.1).
// A breaking change is a new version with a new path prefix; additive changes
// (see the compatibility rule in the package documentation) stay in v1.
const Version = "v1"

// PathPrefix is the root of every agent route (architecture §5, §5.1). The
// agent listener serves nothing else; the admin tree is never mounted there.
const PathPrefix = "/api/v1/agent"

// Route paths of the agent API, one per row of the architecture §5.1 table.
// Routes that carry a variable segment are functions (RouteCommandResult,
// RouteRestore, RouteRestoreArtifact, RouteReleaseArtifact,
// RouteEnrollLobbyPoll); the matching ServeMux patterns are the Pattern
// constants below, which the console registers verbatim.
const (
	// RouteEnroll is POST: one-time enrollment, idempotent on the nonce (§5.1, DH17).
	RouteEnroll = PathPrefix + "/enroll"
	// RoutePolicy is GET: ETag pull of the effective policy (§5.1, DH13).
	RoutePolicy = PathPrefix + "/policy"
	// RouteHeartbeat is POST: presence and health facts (§5.1, DH5).
	RouteHeartbeat = PathPrefix + "/heartbeat"
	// RouteRuns is POST: batch of run results, idempotent on run id (§5.1, §6.2).
	RouteRuns = PathPrefix + "/runs"
	// RouteSnapshots is POST: snapshot-list cache (§5.1).
	RouteSnapshots = PathPrefix + "/snapshots"
	// RouteCommands is GET: long-poll for queued commands (§5.1, §5.5).
	RouteCommands = PathPrefix + "/commands"
	// RouteTokenRotate is POST: rotate the device token (§5.1, DH4).
	RouteTokenRotate = PathPrefix + "/token/rotate"
	// RouteProgress is POST: live run/restore progress (§5.1, DH4).
	RouteProgress = PathPrefix + "/progress"
	// RouteRelease is GET: signed release manifest for the device's ring (§5.1).
	RouteRelease = PathPrefix + "/release"
	// RouteEnrollLobby is POST: lobby request for an install without a token (§5.1).
	RouteEnrollLobby = PathPrefix + "/enroll/lobby"
	// RouteStartLease is POST: ask for a fleet start lease before a scheduled
	// backup (ADR 0005; the M1 rulings addendum of the architecture adds it to §5.1).
	RouteStartLease = PathPrefix + "/start-lease"
)

// ServeMux patterns of the agent API (Go 1.22 syntax: method, space, path with
// {wildcards}). The console registers exactly these, so a route cannot drift
// from the constants, and Patterns lists them in §5.1 order. Handlers read the
// wildcards with Request.PathValue and the PathValue* names below.
const (
	PatternEnroll          = "POST " + RouteEnroll
	PatternPolicy          = "GET " + RoutePolicy
	PatternHeartbeat       = "POST " + RouteHeartbeat
	PatternRuns            = "POST " + RouteRuns
	PatternSnapshots       = "POST " + RouteSnapshots
	PatternCommands        = "GET " + RouteCommands
	PatternCommandResult   = "POST " + PathPrefix + "/commands/{" + PathValueID + "}/result"
	PatternTokenRotate     = "POST " + RouteTokenRotate
	PatternRestore         = "GET " + PathPrefix + "/restores/{" + PathValueID + "}"
	PatternRestoreArtifact = "GET " + PathPrefix + "/restores/{" + PathValueID + "}/artifact"
	PatternProgress        = "POST " + RouteProgress
	PatternRelease         = "GET " + RouteRelease
	PatternReleaseArtifact = "GET " + PathPrefix + "/release/artifact/{" + PathValueVersion + "}/{" + PathValueName + "}"
	PatternEnrollLobby     = "POST " + RouteEnrollLobby
	PatternEnrollLobbyPoll = "GET " + RouteEnrollLobby + "/{" + PathValueNonce + "}"
	PatternStartLease      = "POST " + RouteStartLease
)

// Names of the path wildcards in the Pattern constants.
const (
	PathValueID      = "id"      // command id or restore request id
	PathValueVersion = "version" // release version
	PathValueName    = "name"    // release artifact name
	PathValueNonce   = "nonce"   // lobby enrollment nonce
)

// Patterns returns the sixteen ServeMux patterns of the agent API: the fifteen
// of the architecture §5.1 table in its order, then start-lease (ADR 0005, M1).
func Patterns() []string {
	return []string{
		PatternEnroll, PatternPolicy, PatternHeartbeat, PatternRuns, PatternSnapshots,
		PatternCommands, PatternCommandResult, PatternTokenRotate, PatternRestore,
		PatternRestoreArtifact, PatternProgress, PatternRelease, PatternReleaseArtifact,
		PatternEnrollLobby, PatternEnrollLobbyPoll, PatternStartLease,
	}
}

// RouteCommandResult is the path of POST /commands/{id}/result for a command id (§5.1).
func RouteCommandResult(id string) string {
	return PathPrefix + "/commands/" + url.PathEscape(id) + "/result"
}

// RouteRestore is the path of GET /restores/{id}, the restore selection fetch (§5.1, DH4, §8.4 item 4).
func RouteRestore(id string) string {
	return PathPrefix + "/restores/" + url.PathEscape(id)
}

// RouteRestoreArtifact is the path of GET /restores/{id}/artifact, the relay
// restore archive stream (§5.1, DH28). Range is supported.
func RouteRestoreArtifact(id string) string {
	return RouteRestore(id) + "/artifact"
}

// RouteReleaseArtifact is the path of GET /release/artifact/{version}/{name},
// a release download (§5.1). Range is supported.
func RouteReleaseArtifact(version, name string) string {
	return PathPrefix + "/release/artifact/" + url.PathEscape(version) + "/" + url.PathEscape(name)
}

// RouteEnrollLobbyPoll is the path of GET /enroll/lobby/{nonce}, the poll of a
// lobby request (§5.1). The nonce is the only credential of this route.
func RouteEnrollLobbyPoll(nonce string) string {
	return RouteEnrollLobby + "/" + url.PathEscape(nonce)
}

// Header names (architecture §5, §6.1 "uplink", §5.5).
const (
	// HeaderServerTime is on every response: the console's clock, RFC 3339 with
	// milliseconds in UTC (FormatServerTime). The agent derives its clock offset
	// from it (DH5, §6.1 uplink).
	HeaderServerTime = "X-CryoBackup-Server-Time"
	// HeaderRequestID is echoed by the console on every response and quoted in
	// Error.RequestID so an operator can find the audit row.
	HeaderRequestID = "X-Request-Id"
	// HeaderAgentVersion is sent by the agent on every request (the build's
	// version string); the console also keeps it from the heartbeat body.
	HeaderAgentVersion = "X-CryoBackup-Agent"

	// HeaderETag, HeaderIfNoneMatch, HeaderRetryAfter and HeaderAuthorization
	// are the standard headers the protocol relies on (policy ETag pull,
	// 429 and 503 backoff, bearer device token).
	HeaderETag          = "ETag"
	HeaderIfNoneMatch   = "If-None-Match"
	HeaderRetryAfter    = "Retry-After"
	HeaderAuthorization = "Authorization"

	// BearerPrefix precedes the device token (or the enrollment token on
	// /enroll) in Authorization.
	BearerPrefix = "Bearer "
	// ContentTypeJSON is the content type of every JSON body, request and response.
	ContentTypeJSON = "application/json"
	// ContentTypeProblem is the content type of an Error body (RFC 9457).
	ContentTypeProblem = "application/problem+json"
)

// QueryWait is the query parameter of the command long poll: seconds to wait,
// 0 to CommandsLongPollMax (§5.1).
const QueryWait = "wait"

// Body caps of the public listener (architecture §5.5). The console wraps the
// request body in http.MaxBytesReader with these and answers 413
// payload_too_large. Routes not listed take no body (policy, commands, token
// rotate, restores, release) or share MaxEnrollBody (enroll, lobby).
const (
	MaxEnrollBody        = 4 << 10   // enroll and lobby, §5.5
	MaxRunsBody          = 512 << 10 // POST /runs
	MaxSnapshotsBody     = 2 << 20   // POST /snapshots
	MaxHeartbeatBody     = 16 << 10  // POST /heartbeat
	MaxCommandResultBody = 64 << 10  // POST /commands/{id}/result
	MaxProgressBody      = 4 << 10   // POST /progress
	MaxStartLeaseBody    = 1 << 10   // POST /start-lease
)

// Rate limits (architecture §5.5). Authenticated routes are limited per device;
// only the unauthenticated routes are limited per IP, because agents live
// behind customer NATs. HeartbeatMinInterval and ProgressMinInterval are
// minimum gaps between two calls of one device; the agent sends no faster and
// the console answers 429 with Retry-After when it does.
const (
	HeartbeatMinInterval = 20 * time.Second
	ProgressMinInterval  = 5 * time.Second
	CommandsLongPollMax  = 30 * time.Second // upper bound of QueryWait

	RunsPerMinute        = 30 // per device
	SnapshotsPerMinute   = 6  // per device
	PolicyPerMinute      = 6  // per device
	EnrollPerMinutePerIP = 10 // /enroll and /enroll/lobby*, per source IP
	StartLeasePerMinute  = 6  // per device; the agent asks once per start, retried no sooner than StartLeaseRetryMin
)

// Start lease timing (ADR 0005). A lease lives StartLeaseTTL from its grant or
// its last renewal; a refusal's retry_after_s lies in [StartLeaseRetryMin,
// StartLeaseRetryMax]; the agent waits at most StartLeaseTimeout for an answer
// and then starts anyway (fail open).
const (
	StartLeaseTTL      = 15 * time.Minute
	StartLeaseRetryMin = 60 * time.Second
	StartLeaseRetryMax = 900 * time.Second
	StartLeaseTimeout  = 10 * time.Second
)

// Further bounds of the protocol (ticket P1-T4c). They are the limits Validate
// enforces and the console's caps for the matching fields.
const (
	MaxRunsPerRequest      = 200  // RunsRequest.Runs
	MaxSnapshotsPerRequest = 5000 // SnapshotsRequest.Snapshots
	MaxPathsPerSnapshot    = 64   // restic.Snapshot.Paths: roots only
	MaxCommandParams       = 4 << 10
	MaxCommandResult       = 16 << 10
)
