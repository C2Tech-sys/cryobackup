package api

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/policy"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// Fixed values shared by the tests. Every value is synthetic.
const (
	ulidA = "01JAZK3Q8W5N6X7Y8Z9A0B1C2D"
	ulidB = "01JAZK3Q8W5N6X7Y8Z9A0B1C2E"
	ulidC = "01JAZK3Q8W5N6X7Y8Z9A0B1C2F"
	ulidD = "01JAZK3Q8W5N6X7Y8Z9A0B1C2G" // a customer id
	ulidE = "01JAZK3Q8W5N6X7Y8Z9A0B1C2H" // a policy id

	secretToken  = "tok-SECRET-device-0123456789"
	secretGwPass = "gw-SECRET-password-0123456789"
	secretRepo   = "repo-SECRET-password-0123456789"
	secretPend   = "pend-SECRET-gateway-0123456789"
	secretRotate = "rot-SECRET-token-0123456789"
)

var (
	t0   = time.Date(2026, 10, 22, 14, 3, 9, 512_000_000, time.UTC)
	hex8 = "0a1b2c3d"
	hexN = strings.Repeat("0f", 16) // 32 hex: a nonce
	hexF = strings.Repeat("c3", 16) // 32 hex: a fingerprint
	hex6 = strings.Repeat("ab12", 16)
	hex7 = strings.Repeat("cd34", 16)
)

func ptr[T any](v T) *T { return &v }

func at(minutes int) time.Time { return t0.Add(time.Duration(minutes) * time.Minute) }

func okEnrollRequest() EnrollRequest {
	return EnrollRequest{
		Nonce: hexN, Hostname: "fixture-host", OS: "windows", OSVersion: "10.0.26300",
		Arch: ArchAMD64, DeviceClass: DeviceClassLaptop, HWFingerprint: hexF,
		AgentVersion: "1.2.3", ResticVersion: "0.19.1",
	}
}

func okLobbyRequest() LobbyRequest {
	return LobbyRequest{
		Nonce: hexN, Hostname: "fixture-host", OS: "windows", Arch: ArchARM64,
		HWFingerprint: hexF, AgentVersion: "1.2.3",
	}
}

func okProgress() Progress {
	return Progress{
		RunID: ulidA, Kind: restic.KindBackup, PercentDone: 0.42, BytesDone: 4200, TotalBytes: 10000,
		FilesDone: 42, TotalFiles: 100, ErrorCount: 2, SecondsElapsed: 61, Stalled: true, UpdatedAt: at(1),
	}
}

func okHeartbeat() HeartbeatRequest {
	p := okProgress()
	return HeartbeatRequest{
		AgentVersion: "1.2.3", ResticVersion: "0.19.1", UptimeS: 86400,
		OnAC: ptr(true), BatteryPct: ptr(87), Metered: ptr(true), VPN: ptr(true),
		QueueDepth: 3, JournalDepth: 2, DeviceTime: t0,
		ServiceState: ServiceDeferred, LastErrorClass: restic.ClassNetwork, DeferredReason: restic.DeferredBattery,
		Progress: &p, PolicyETag: `"` + hex6 + `"`, PolicyVersionApplied: 7, HWFingerprint: hexF,
		GatewaySecretAcked: true, ActiveRunID: ulidB,
	}
}

func okHeartbeatResponse() HeartbeatResponse {
	return HeartbeatResponse{
		ServerTime: t0, ClockOffsetMS: -1500, CommandsPending: true,
		PolicyETag: `"` + hex6 + `"`, ReleaseHint: "1.3.0",
	}
}

// okRun sets every field of RunRecord; it is a fixture, not a realistic run.
func okRun() RunRecord {
	return RunRecord{
		ID: ulidA, Kind: restic.KindBackup, Trigger: TriggerCatchup,
		StartedAt: at(0), FinishedAt: ptr(at(5)), Outcome: restic.OutcomeDeferred, ExitCode: ptr(11),
		SnapshotID: hex6,
		Stats: RunStats{
			FilesNew: 1, FilesChanged: 2, FilesUnmodified: 3, DirsNew: 4, DirsChanged: 5, DirsUnmodified: 6,
			TotalFilesProcessed: 7, DataAdded: 8, DataAddedPacked: 9, TotalBytesProcessed: 10,
			TotalDurationS: 11.5, FailedFiles: 12, VSSFallbackVolumes: []string{"C", "D"}, LongRun: true,
			Lease: LeaseGranted,
		},
		ErrorCounts: []restic.ErrorProjection{
			{During: "archival", Class: restic.ClassPermission, Ext: "docx", Count: 3},
			{During: "scan", Class: restic.ClassLocked, Ext: "pdf", Count: 1},
		},
		ErrorClass: restic.ClassLock, Attempt: 2, DeferredReason: restic.DeferredLock,
		PolicyVersion: 7, AgentVersion: "1.2.3", Initial: true,
	}
}

// closedRun is a realistic closed backup.
func closedRun() RunRecord {
	r := okRun()
	r.Outcome = restic.OutcomeSuccess
	r.DeferredReason = ""
	r.ErrorClass = ""
	r.ExitCode = ptr(0)
	return r
}

func okRunsRequest() RunsRequest {
	second := okRun()
	second.ID = ulidB
	return RunsRequest{Runs: []RunRecord{okRun(), second}}
}

func okRunsResponse() RunsResponse {
	return RunsResponse{
		Results: []RunResult{
			{ID: ulidA, Status: RunRejected, Reason: ReasonConflict},
			{ID: ulidB, Status: RunRejected, Reason: ReasonInvalid},
		},
		ServerTime: t0,
	}
}

func okSnapshot() restic.Snapshot {
	return restic.Snapshot{
		ID: hex6, ShortID: hex8, Time: at(-10), Parent: hex7, Tree: hex7,
		Paths: []string{`C:\Users`, `D:\Data`}, Hostname: "fixture-host", Tags: []string{"cryo"},
		ProgramVersion: "restic 0.19.1",
		Summary: &restic.BackupSummary{
			DryRun: true, FilesNew: 1, FilesChanged: 2, FilesUnmodified: 3, DirsNew: 4, DirsChanged: 5,
			DirsUnmodified: 6, DataBlobs: 7, TreeBlobs: 8, DataAdded: 9, DataAddedPacked: 10,
			TotalFilesProcessed: 11, TotalBytesProcessed: 12, BackupStart: at(-12), BackupEnd: at(-10),
			TotalDuration: 13.5, SnapshotID: hex6,
		},
	}
}

func okSnapshotsRequest() SnapshotsRequest {
	return SnapshotsRequest{Snapshots: []restic.Snapshot{okSnapshot()}}
}

func okCommandsResponse() CommandsResponse {
	return CommandsResponse{
		Commands: []Command{
			{ID: ulidA, Type: CommandHold, Params: json.RawMessage(`{"until":"2026-10-22T15:00:00Z"}`), CreatedAt: t0, ExpiresAt: at(60)},
			{ID: ulidB, Type: CommandRestore, Params: json.RawMessage(`{"restore_request_id":"` + ulidC + `"}`), CreatedAt: t0, ExpiresAt: at(120)},
		},
		ServerTime: t0,
	}
}

func okCommand() Command { return okCommandsResponse().Commands[0] }

func okCommandResult() CommandResultRequest {
	return CommandResultRequest{
		Status: CommandFailed, ErrorClass: restic.ClassInternal,
		Result: json.RawMessage(`{"detail":"fixed","count":2}`),
	}
}

func okPolicyResponse() PolicyResponse {
	return PolicyResponse{
		Policy: policy.Default(policy.ClassLaptop), PolicyID: ulidE, Version: 7, ETag: `"` + hex6 + `"`,
		PendingGatewaySecret: &GatewaySecret{Password: secretPend, IssuedAt: at(-30)},
		HoldUntil:            ptr(at(30)), Paused: true, PausedUntil: ptr(at(90)),
		ReleaseTarget: "1.3.0", ServerTime: t0,
	}
}

func okEnrollResponse() EnrollResponse {
	return EnrollResponse{
		DeviceID: ulidA, CustomerID: ulidD, DeviceToken: secretToken,
		Gateway:      Gateway{URL: "https://gateway.example.test:9443", Username: ulidA, Password: secretGwPass},
		RepoPassword: secretRepo, Custody: CustodyEscrowed, Policy: okPolicyResponse(), ServerTime: t0,
	}
}

func okLobbyResponse() LobbyResponse {
	e := okEnrollResponse()
	return LobbyResponse{Status: LobbyApproved, PollURL: RouteEnrollLobbyPoll(hexN), Enroll: &e}
}

func okTokenRotate() TokenRotateResponse {
	return TokenRotateResponse{DeviceToken: secretRotate, GraceUntil: at(60), ServerTime: t0}
}

func okRestoreSelection() RestoreSelection {
	return RestoreSelection{
		ID: ulidA, SnapshotID: hex6, Mode: RestoreDeviceRelay, Target: `C:\Restore\Out`,
		Includes: []string{`C:\Users\x\Documents`, `/home/x/y`}, IncludeFile: true, Overwrite: OverwriteIfNewer,
		Verify: true, Simulate: true, ArtifactURL: RouteRestoreArtifact(ulidA), ArtifactSHA256: hex7, ExpiresAt: at(240),
	}
}

func okRelease() ReleaseResponse {
	return ReleaseResponse{
		Version: "1.3.0", Channel: ChannelPilot, ResticVersion: "0.19.1",
		Manifest:  json.RawMessage(`{"version":"1.3.0"}`),
		Signature: "c2ln",
	}
}

func okStartLeaseRequest() StartLeaseRequest { return StartLeaseRequest{RunID: ulidA} }

// okStartLeaseGranted is a granted lease (valid).
func okStartLeaseGranted() StartLeaseResponse {
	return StartLeaseResponse{Granted: true, LeaseUntil: ptr(at(15)), ServerTime: t0}
}

// okStartLeaseAll sets every field of StartLeaseResponse for the round trip; it
// is a fixture, not a valid answer (a grant carries no retry_after_s).
func okStartLeaseAll() StartLeaseResponse {
	return StartLeaseResponse{Granted: true, LeaseUntil: ptr(at(15)), RetryAfterS: 420, ServerTime: t0}
}

func okError() Error {
	e := NewError(CodeRateLimited)
	e.RetryAfterS, e.RequestID = 30, "req-1"
	return *e
}

// wireValues lists a filled value of every wire type of the package.
func wireValues() []any {
	return []any{
		okEnrollRequest(), okEnrollResponse(), okPolicyResponse(), okHeartbeat(), okHeartbeatResponse(),
		okProgress(), okRun(), okRunsRequest(), okRunsResponse(), okSnapshotsRequest(),
		SnapshotsResponse{Upserted: 3, Removed: 1, ServerTime: t0}, okCommandsResponse(), okCommandResult(),
		okTokenRotate(), okRestoreSelection(), okRelease(), okLobbyRequest(), okLobbyResponse(), okError(),
		HoldParams{Until: at(10)}, PauseParams{Until: ptr(at(10)), Reason: "holiday"},
		RestoreParams{RestoreRequestID: ulidC}, UpdateAgentParams{Version: "1.3.0"},
		okStartLeaseRequest(), okStartLeaseAll(),
	}
}

// assertAllSet fails the test for every exported field of v that holds its zero
// value, so a round-trip test cannot pass on a field it never exercised. It
// does not look inside policy.Policy, which has its own tests.
func assertAllSet(t *testing.T, v any) {
	t.Helper()
	var walk func(rv reflect.Value, path string)
	walk = func(rv reflect.Value, path string) {
		switch rv.Kind() {
		case reflect.Pointer, reflect.Interface:
			if rv.IsNil() {
				t.Errorf("%s is nil", path)
				return
			}
			if e := rv.Elem(); e.Kind() == reflect.Struct || e.Kind() == reflect.Pointer || e.Kind() == reflect.Interface {
				walk(e, path)
			}
			return
		}
		if rv.Type() == reflect.TypeOf(policy.Policy{}) {
			return
		}
		if strings.HasSuffix(path, ".Username") {
			return // restic.Snapshot.Username must stay empty on the wire
		}
		if rv.IsZero() && !strings.HasSuffix(path, ".Username") {
			t.Errorf("%s is the zero value; the fixture must set it", path)
			return
		}
		switch rv.Kind() {
		case reflect.Struct:
			for i := 0; i < rv.NumField(); i++ {
				if rv.Type().Field(i).IsExported() {
					walk(rv.Field(i), path+"."+rv.Type().Field(i).Name)
				}
			}
		case reflect.Slice:
			if rv.Type() == reflect.TypeOf(json.RawMessage(nil)) {
				return
			}
			for i := 0; i < rv.Len(); i++ {
				walk(rv.Index(i), path+"[]")
			}
		}
	}
	walk(reflect.ValueOf(v), reflect.TypeOf(v).Name())
}
