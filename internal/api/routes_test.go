package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestRouteConstants lists every route of architecture §5.1 literally, so a
// change to a constant is a change to this test and to the protocol.
func TestRouteConstants(t *testing.T) {
	if Version != "v1" || PathPrefix != "/api/v1/agent" {
		t.Fatalf("Version=%q PathPrefix=%q", Version, PathPrefix)
	}
	id, nonce := ulidA, hexN
	rows := []struct {
		name, got, want string
	}{
		{"enroll", RouteEnroll, "/api/v1/agent/enroll"},
		{"policy", RoutePolicy, "/api/v1/agent/policy"},
		{"heartbeat", RouteHeartbeat, "/api/v1/agent/heartbeat"},
		{"runs", RouteRuns, "/api/v1/agent/runs"},
		{"snapshots", RouteSnapshots, "/api/v1/agent/snapshots"},
		{"commands", RouteCommands, "/api/v1/agent/commands"},
		{"command result", RouteCommandResult(id), "/api/v1/agent/commands/" + id + "/result"},
		{"token rotate", RouteTokenRotate, "/api/v1/agent/token/rotate"},
		{"restore", RouteRestore(id), "/api/v1/agent/restores/" + id},
		{"restore artifact", RouteRestoreArtifact(id), "/api/v1/agent/restores/" + id + "/artifact"},
		{"progress", RouteProgress, "/api/v1/agent/progress"},
		{"release", RouteRelease, "/api/v1/agent/release"},
		{"release artifact", RouteReleaseArtifact("1.3.0", "cryo-agent.exe"), "/api/v1/agent/release/artifact/1.3.0/cryo-agent.exe"},
		{"lobby", RouteEnrollLobby, "/api/v1/agent/enroll/lobby"},
		{"lobby poll", RouteEnrollLobbyPoll(nonce), "/api/v1/agent/enroll/lobby/" + nonce},
		{"start lease", RouteStartLease, "/api/v1/agent/start-lease"},
	}
	for _, r := range rows {
		if r.got != r.want {
			t.Errorf("%s: %q, want %q", r.name, r.got, r.want)
		}
	}
	if len(rows) != 16 {
		t.Errorf("the table lists %d routes, §5.1 has 15 plus start-lease (ADR 0005)", len(rows))
	}
}

func TestPatterns(t *testing.T) {
	want := []string{
		"POST /api/v1/agent/enroll",
		"GET /api/v1/agent/policy",
		"POST /api/v1/agent/heartbeat",
		"POST /api/v1/agent/runs",
		"POST /api/v1/agent/snapshots",
		"GET /api/v1/agent/commands",
		"POST /api/v1/agent/commands/{id}/result",
		"POST /api/v1/agent/token/rotate",
		"GET /api/v1/agent/restores/{id}",
		"GET /api/v1/agent/restores/{id}/artifact",
		"POST /api/v1/agent/progress",
		"GET /api/v1/agent/release",
		"GET /api/v1/agent/release/artifact/{version}/{name}",
		"POST /api/v1/agent/enroll/lobby",
		"GET /api/v1/agent/enroll/lobby/{nonce}",
		"POST /api/v1/agent/start-lease",
	}
	got := Patterns()
	if len(got) != len(want) {
		t.Fatalf("Patterns() has %d entries, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Patterns()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestPatternsServeTheRoutes registers every pattern on a real ServeMux (which
// panics on a conflict) and requests every Route helper with its method: each
// must reach exactly its own handler, with the wildcards filled in.
func TestPatternsServeTheRoutes(t *testing.T) {
	type hit struct{ pattern, id, version, name, nonce string }
	var last hit
	mux := http.NewServeMux()
	for _, p := range Patterns() {
		p := p
		mux.HandleFunc(p, func(w http.ResponseWriter, r *http.Request) {
			last = hit{p, r.PathValue(PathValueID), r.PathValue(PathValueVersion), r.PathValue(PathValueName), r.PathValue(PathValueNonce)}
			w.WriteHeader(http.StatusNoContent)
		})
	}
	reqs := []struct {
		method, path string
		want         hit
	}{
		{"POST", RouteEnroll, hit{pattern: PatternEnroll}},
		{"GET", RoutePolicy, hit{pattern: PatternPolicy}},
		{"POST", RouteHeartbeat, hit{pattern: PatternHeartbeat}},
		{"POST", RouteRuns, hit{pattern: PatternRuns}},
		{"POST", RouteSnapshots, hit{pattern: PatternSnapshots}},
		{"GET", RouteCommands, hit{pattern: PatternCommands}},
		{"POST", RouteCommandResult(ulidA), hit{pattern: PatternCommandResult, id: ulidA}},
		{"POST", RouteTokenRotate, hit{pattern: PatternTokenRotate}},
		{"GET", RouteRestore(ulidB), hit{pattern: PatternRestore, id: ulidB}},
		{"GET", RouteRestoreArtifact(ulidB), hit{pattern: PatternRestoreArtifact, id: ulidB}},
		{"POST", RouteProgress, hit{pattern: PatternProgress}},
		{"GET", RouteRelease, hit{pattern: PatternRelease}},
		{"GET", RouteReleaseArtifact("1.3.0", "cryo agent.msi"), hit{pattern: PatternReleaseArtifact, version: "1.3.0", name: "cryo agent.msi"}},
		{"POST", RouteEnrollLobby, hit{pattern: PatternEnrollLobby}},
		{"GET", RouteEnrollLobbyPoll(hexN), hit{pattern: PatternEnrollLobbyPoll, nonce: hexN}},
		{"POST", RouteStartLease, hit{pattern: PatternStartLease}},
	}
	for _, r := range reqs {
		last = hit{}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(r.method, r.path, nil))
		if rec.Code != http.StatusNoContent {
			t.Errorf("%s %s: status %d", r.method, r.path, rec.Code)
			continue
		}
		if last != r.want {
			t.Errorf("%s %s reached %+v, want %+v", r.method, r.path, last, r.want)
		}
	}
	// Methods are part of the pattern: the wrong one is not routed.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", RouteHeartbeat, nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET on a POST route: %d, want 405", rec.Code)
	}
	// Every pattern is under the prefix and distinct.
	seen := map[string]bool{}
	for _, p := range Patterns() {
		if seen[p] {
			t.Errorf("duplicate pattern %q", p)
		}
		seen[p] = true
		if !strings.Contains(p, " "+PathPrefix+"/") {
			t.Errorf("pattern %q is outside %s", p, PathPrefix)
		}
	}
}

func TestRouteHelpersEscapeSegments(t *testing.T) {
	if got := RouteCommandResult("a/b"); strings.Contains(got, "a/b") {
		t.Errorf("an id with a slash must not add a path segment: %q", got)
	}
	if got := RouteReleaseArtifact("1.0", "../x"); strings.Contains(got, "../") {
		t.Errorf("a traversal name must be escaped: %q", got)
	}
}

func TestHeadersAndLimits(t *testing.T) {
	for name, got := range map[string]string{
		"server time": HeaderServerTime, "request id": HeaderRequestID, "agent": HeaderAgentVersion,
		"etag": HeaderETag, "if-none-match": HeaderIfNoneMatch, "retry-after": HeaderRetryAfter,
		"authorization": HeaderAuthorization, "bearer": BearerPrefix, "query": QueryWait,
	} {
		want := map[string]string{
			"server time": "X-CryoBackup-Server-Time", "request id": "X-Request-Id", "agent": "X-CryoBackup-Agent",
			"etag": "ETag", "if-none-match": "If-None-Match", "retry-after": "Retry-After",
			"authorization": "Authorization", "bearer": "Bearer ", "query": "wait",
		}[name]
		if got != want {
			t.Errorf("%s: %q, want %q", name, got, want)
		}
	}
	ints := []struct {
		name      string
		got, want int
	}{
		{"enroll body", MaxEnrollBody, 4 * 1024},
		{"runs body", MaxRunsBody, 512 * 1024},
		{"snapshots body", MaxSnapshotsBody, 2 * 1024 * 1024},
		{"heartbeat body", MaxHeartbeatBody, 16 * 1024},
		{"command result body", MaxCommandResultBody, 64 * 1024},
		{"progress body", MaxProgressBody, 4 * 1024},
		{"start lease body", MaxStartLeaseBody, 1024},
		{"start leases per minute", StartLeasePerMinute, 6},
		{"runs per minute", RunsPerMinute, 30},
		{"snapshots per minute", SnapshotsPerMinute, 6},
		{"policy per minute", PolicyPerMinute, 6},
		{"enroll per minute per IP", EnrollPerMinutePerIP, 10},
		{"runs per request", MaxRunsPerRequest, 200},
		{"snapshots per request", MaxSnapshotsPerRequest, 5000},
		{"paths per snapshot", MaxPathsPerSnapshot, 64},
		{"command params", MaxCommandParams, 4096},
		{"command result", MaxCommandResult, 16384},
	}
	for _, c := range ints {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", c.name, c.got, c.want)
		}
	}
	if HeartbeatMinInterval != 20*time.Second || ProgressMinInterval != 5*time.Second || CommandsLongPollMax != 30*time.Second {
		t.Errorf("intervals: %v %v %v", HeartbeatMinInterval, ProgressMinInterval, CommandsLongPollMax)
	}
	// ADR 0005: a 15-minute lease, a refusal's retry in [60 s, 900 s], a 10 s agent wait.
	if StartLeaseTTL != 15*time.Minute || StartLeaseRetryMin != 60*time.Second || StartLeaseRetryMax != 900*time.Second || StartLeaseTimeout != 10*time.Second {
		t.Errorf("start lease timing: %v %v %v %v", StartLeaseTTL, StartLeaseRetryMin, StartLeaseRetryMax, StartLeaseTimeout)
	}
}
