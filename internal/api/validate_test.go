package api

import (
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// vcase is one row of a Validate table: mutate a valid base value and say which
// problem paths are expected. An empty want means "must pass".
type vcase[T any] struct {
	name string
	mut  func(*T)
	want []string
}

// runValidate runs the table against base. Every table must hold at least one
// passing and one failing row.
func runValidate[T interface{ Validate() error }](t *testing.T, base func() T, cases []vcase[T]) {
	t.Helper()
	pass, fail := 0, 0
	for _, c := range cases {
		c := c
		if len(c.want) == 0 {
			pass++
		} else {
			fail++
		}
		t.Run(c.name, func(t *testing.T) {
			v := base()
			if c.mut != nil {
				c.mut(&v)
			}
			err := v.Validate()
			if len(c.want) == 0 {
				if err != nil {
					t.Fatalf("want pass, got %v", err)
				}
				return
			}
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("want problems %v, got %v", c.want, err)
			}
			got := make([]string, len(ve.Problems))
			for i, p := range ve.Problems {
				got[i] = p.Path
			}
			want := append([]string(nil), c.want...)
			sort.Strings(want)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("problem paths = %v, want %v\n%v", got, want, err)
			}
		})
	}
	if pass == 0 || fail == 0 {
		t.Fatalf("table has %d passing and %d failing rows; needs both", pass, fail)
	}
}

func TestValidationErrorShape(t *testing.T) {
	var c collector
	c.add("b", "second")
	c.add("a", "first")
	c.add("a", "alpha")
	err := c.err()
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("not a ValidationError: %v", err)
	}
	if got, want := err.Error(), "a: alpha\na: first\nb: second"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	if (&collector{}).err() != nil {
		t.Error("an empty collector must return an untyped nil error")
	}
	if okEnrollRequest().Validate() != nil {
		t.Error("a valid request must return an untyped nil error")
	}
}

func TestValidateNeverEchoesValues(t *testing.T) {
	r := okEnrollRequest()
	r.Hostname = `C:\Users\patient-smith\x`
	r.OS = strings.Repeat("secret-os", 40)
	r.Nonce = "patient-smith"
	err := r.Validate()
	if err == nil {
		t.Fatal("want problems")
	}
	for _, leak := range []string{"patient", "smith", "secret-os", "Users"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("validation text repeats input %q: %v", leak, err)
		}
	}
}

func TestMinTimestampMatchesSchema(t *testing.T) {
	if got := MinTimestamp.UnixMilli(); got != 1_000_000_000_000 {
		t.Errorf("MinTimestamp = %d ms, want the schema bound 1000000000000", got)
	}
}

func TestIDFormats(t *testing.T) {
	good := []string{ulidA, "7ZZZZZZZZZZZZZZZZZZZZZZZZZ", "00000000000000000000000000"}
	bad := []string{"", ulidA[:25], ulidA + "0", strings.ToLower(ulidA), "8" + ulidA[1:], "01JAZK3Q8W5N6X7Y8Z9A0B1C2U",
		"01JAZK3Q8W5N6X7Y8Z9A0B1C2I", "01JAZK3Q8W5N6X7Y8Z9A0B1C2L", "01JAZK3Q8W5N6X7Y8Z9A0B1C2O", "01JAZK3Q8W5N6X7Y8Z9A0B1C2-"}
	for _, s := range good {
		if !isULID(s) {
			t.Errorf("isULID(%q) = false", s)
		}
	}
	for _, s := range bad {
		if isULID(s) {
			t.Errorf("isULID(%q) = true", s)
		}
	}
	if !isLowerHex("0a", 2, 2) || isLowerHex("0A", 2, 2) || isLowerHex("0g", 2, 2) || isLowerHex("0", 2, 2) || isLowerHex("000", 2, 2) {
		t.Error("isLowerHex misjudges a case")
	}
}

func TestValidateEnrollRequest(t *testing.T) {
	runValidate(t, okEnrollRequest, []vcase[EnrollRequest]{
		{"valid", nil, nil},
		{"nonce 31", func(r *EnrollRequest) { r.Nonce = r.Nonce[:31] }, []string{"nonce"}},
		{"nonce 33", func(r *EnrollRequest) { r.Nonce += "0" }, []string{"nonce"}},
		{"nonce upper case", func(r *EnrollRequest) { r.Nonce = strings.ToUpper(strings.Repeat("ab", 16)) }, []string{"nonce"}},
		{"nonce not hex", func(r *EnrollRequest) { r.Nonce = strings.Repeat("g", 32) }, []string{"nonce"}},
		{"nonce empty", func(r *EnrollRequest) { r.Nonce = "" }, []string{"nonce"}},
		{"hostname empty", func(r *EnrollRequest) { r.Hostname = "" }, []string{"hostname"}},
		{"hostname slash", func(r *EnrollRequest) { r.Hostname = "a/b" }, []string{"hostname"}},
		{"hostname backslash", func(r *EnrollRequest) { r.Hostname = `a\b` }, []string{"hostname"}},
		{"hostname unc", func(r *EnrollRequest) { r.Hostname = `\\server\share` }, []string{"hostname"}},
		{"hostname 253", func(r *EnrollRequest) { r.Hostname = strings.Repeat("a", 253) }, nil},
		{"hostname 254", func(r *EnrollRequest) { r.Hostname = strings.Repeat("a", 254) }, []string{"hostname"}},
		{"hostname control", func(r *EnrollRequest) { r.Hostname = "a\x00b" }, []string{"hostname"}},
		{"hostname unicode", func(r *EnrollRequest) { r.Hostname = "büro-pc" }, nil},
		{"os empty", func(r *EnrollRequest) { r.OS = "" }, []string{"os"}},
		{"os too long", func(r *EnrollRequest) { r.OS = strings.Repeat("w", 129) }, []string{"os"}},
		{"os_version empty is fine", func(r *EnrollRequest) { r.OSVersion = "" }, nil},
		{"os_version too long", func(r *EnrollRequest) { r.OSVersion = strings.Repeat("1", 129) }, []string{"os_version"}},
		{"arch x86", func(r *EnrollRequest) { r.Arch = ArchX86 }, nil},
		{"arch unknown value", func(r *EnrollRequest) { r.Arch = "mips" }, []string{"arch"}},
		{"arch empty", func(r *EnrollRequest) { r.Arch = "" }, []string{"arch"}},
		{"class unknown is allowed", func(r *EnrollRequest) { r.DeviceClass = DeviceClassUnknown }, nil},
		{"class bogus", func(r *EnrollRequest) { r.DeviceClass = "tablet" }, []string{"device_class"}},
		{"class empty", func(r *EnrollRequest) { r.DeviceClass = "" }, []string{"device_class"}},
		{"fingerprint 16", func(r *EnrollRequest) { r.HWFingerprint = strings.Repeat("a", 16) }, nil},
		{"fingerprint 128", func(r *EnrollRequest) { r.HWFingerprint = strings.Repeat("a", 128) }, nil},
		{"fingerprint 15", func(r *EnrollRequest) { r.HWFingerprint = strings.Repeat("a", 15) }, []string{"hw_fingerprint"}},
		{"fingerprint 129", func(r *EnrollRequest) { r.HWFingerprint = strings.Repeat("a", 129) }, []string{"hw_fingerprint"}},
		{"fingerprint upper", func(r *EnrollRequest) { r.HWFingerprint = strings.Repeat("A", 32) }, []string{"hw_fingerprint"}},
		{"agent_version empty", func(r *EnrollRequest) { r.AgentVersion = "" }, []string{"agent_version"}},
		{"restic_version empty", func(r *EnrollRequest) { r.ResticVersion = "" }, []string{"restic_version"}},
		{"everything wrong", func(r *EnrollRequest) { *r = EnrollRequest{} },
			[]string{"nonce", "hostname", "os", "arch", "device_class", "hw_fingerprint", "agent_version", "restic_version"}},
	})
}

func TestValidateLobbyRequest(t *testing.T) {
	runValidate(t, okLobbyRequest, []vcase[LobbyRequest]{
		{"valid", nil, nil},
		{"nonce short", func(r *LobbyRequest) { r.Nonce = "abc" }, []string{"nonce"}},
		{"hostname slash", func(r *LobbyRequest) { r.Hostname = "a/b" }, []string{"hostname"}},
		{"hostname backslash", func(r *LobbyRequest) { r.Hostname = `a\b` }, []string{"hostname"}},
		{"os empty", func(r *LobbyRequest) { r.OS = "" }, []string{"os"}},
		{"arch bogus", func(r *LobbyRequest) { r.Arch = "sparc" }, []string{"arch"}},
		{"fingerprint short", func(r *LobbyRequest) { r.HWFingerprint = "ab" }, []string{"hw_fingerprint"}},
		{"agent_version empty", func(r *LobbyRequest) { r.AgentVersion = "" }, []string{"agent_version"}},
	})
}

func TestValidateProgress(t *testing.T) {
	runValidate(t, okProgress, []vcase[Progress]{
		{"valid", nil, nil},
		{"percent 0", func(p *Progress) { p.PercentDone = 0 }, nil},
		{"percent 1", func(p *Progress) { p.PercentDone = 1 }, nil},
		{"restore kind", func(p *Progress) { p.Kind = restic.KindRestore }, nil},
		{"run id bad", func(p *Progress) { p.RunID = "nope" }, []string{"run_id"}},
		{"kind not recorded", func(p *Progress) { p.Kind = restic.KindSnapshots }, []string{"kind"}},
		{"kind empty", func(p *Progress) { p.Kind = "" }, []string{"kind"}},
		{"percent negative", func(p *Progress) { p.PercentDone = -0.1 }, []string{"percent_done"}},
		{"percent above 1", func(p *Progress) { p.PercentDone = 1.01 }, []string{"percent_done"}},
		{"percent NaN", func(p *Progress) { p.PercentDone = math.NaN() }, []string{"percent_done"}},
		{"updated_at zero", func(p *Progress) { p.UpdatedAt = time.Time{} }, []string{"updated_at"}},
	})
}

func TestValidateHeartbeat(t *testing.T) {
	runValidate(t, okHeartbeat, []vcase[HeartbeatRequest]{
		{"valid (deferred with reason)", nil, nil},
		{"idle without reason", func(r *HeartbeatRequest) { r.ServiceState, r.DeferredReason = ServiceIdle, "" }, nil},
		{"sparse", func(r *HeartbeatRequest) {
			r.OnAC, r.BatteryPct, r.Metered, r.VPN, r.Progress, r.LastErrorClass, r.HWFingerprint, r.ResticVersion = nil, nil, nil, nil, nil, "", "", ""
		}, nil},
		{"agent_version empty", func(r *HeartbeatRequest) { r.AgentVersion = "" }, []string{"agent_version"}},
		{"restic_version too long", func(r *HeartbeatRequest) { r.ResticVersion = strings.Repeat("1", 129) }, []string{"restic_version"}},
		{"uptime negative", func(r *HeartbeatRequest) { r.UptimeS = -1 }, []string{"uptime_s"}},
		{"battery 0", func(r *HeartbeatRequest) { r.BatteryPct = ptr(0) }, nil},
		{"battery 100", func(r *HeartbeatRequest) { r.BatteryPct = ptr(100) }, nil},
		{"battery -1", func(r *HeartbeatRequest) { r.BatteryPct = ptr(-1) }, []string{"battery_pct"}},
		{"battery 101", func(r *HeartbeatRequest) { r.BatteryPct = ptr(101) }, []string{"battery_pct"}},
		{"queue negative", func(r *HeartbeatRequest) { r.QueueDepth = -1 }, []string{"queue_depth"}},
		{"journal negative", func(r *HeartbeatRequest) { r.JournalDepth = -1 }, []string{"journal_depth"}},
		{"device_time zero", func(r *HeartbeatRequest) { r.DeviceTime = time.Time{} }, []string{"device_time"}},
		{"state empty", func(r *HeartbeatRequest) { r.ServiceState = "" }, []string{"service_state", "deferred_reason"}},
		{"state bogus", func(r *HeartbeatRequest) { r.ServiceState = "sleeping" }, []string{"service_state", "deferred_reason"}},
		{"error class bogus", func(r *HeartbeatRequest) { r.LastErrorClass = "boom" }, []string{"last_error_class"}},
		{"deferred reason bogus", func(r *HeartbeatRequest) { r.DeferredReason = "tuesday" }, []string{"deferred_reason"}},
		{"deferred without reason", func(r *HeartbeatRequest) { r.DeferredReason = "" }, []string{"deferred_reason"}},
		{"reason while running", func(r *HeartbeatRequest) { r.ServiceState = ServiceRunning }, []string{"deferred_reason"}},
		{"progress invalid", func(r *HeartbeatRequest) { r.Progress.RunID = "x" }, []string{"progress.run_id"}},
		{"policy_etag too long", func(r *HeartbeatRequest) { r.PolicyETag = strings.Repeat("e", 129) }, []string{"policy_etag"}},
		{"policy version negative", func(r *HeartbeatRequest) { r.PolicyVersionApplied = -1 }, []string{"policy_version_applied"}},
		{"fingerprint bad", func(r *HeartbeatRequest) { r.HWFingerprint = "zz" }, []string{"hw_fingerprint"}},
		{"no active run", func(r *HeartbeatRequest) { r.ActiveRunID = "" }, nil},
		{"active run not a ulid", func(r *HeartbeatRequest) { r.ActiveRunID = "run-1" }, []string{"active_run_id"}},
		{"active run lower case", func(r *HeartbeatRequest) { r.ActiveRunID = strings.ToLower(ulidB) }, []string{"active_run_id"}},
	})
}

func TestValidateStartLease(t *testing.T) {
	runValidate(t, okStartLeaseRequest, []vcase[StartLeaseRequest]{
		{"valid", nil, nil},
		{"run id empty", func(r *StartLeaseRequest) { r.RunID = "" }, []string{"run_id"}},
		{"run id short", func(r *StartLeaseRequest) { r.RunID = ulidA[:25] }, []string{"run_id"}},
		{"run id lower case", func(r *StartLeaseRequest) { r.RunID = strings.ToLower(ulidA) }, []string{"run_id"}},
	})
	refused := func(r *StartLeaseResponse) { r.Granted, r.LeaseUntil, r.RetryAfterS = false, nil, 420 }
	runValidate(t, okStartLeaseGranted, []vcase[StartLeaseResponse]{
		{"granted with a lease", nil, nil},
		{"granted without a lease (no cap)", func(r *StartLeaseResponse) { r.LeaseUntil = nil }, nil},
		{"refused", refused, nil},
		{"refused at 60", func(r *StartLeaseResponse) { refused(r); r.RetryAfterS = 60 }, nil},
		{"refused at 900", func(r *StartLeaseResponse) { refused(r); r.RetryAfterS = 900 }, nil},
		{"refused at 59", func(r *StartLeaseResponse) { refused(r); r.RetryAfterS = 59 }, []string{"retry_after_s"}},
		{"refused at 901", func(r *StartLeaseResponse) { refused(r); r.RetryAfterS = 901 }, []string{"retry_after_s"}},
		{"refused without retry", func(r *StartLeaseResponse) { refused(r); r.RetryAfterS = 0 }, []string{"retry_after_s"}},
		{"refused with a lease", func(r *StartLeaseResponse) { refused(r); r.LeaseUntil = ptr(at(1)) }, []string{"lease_until"}},
		{"granted with retry", func(r *StartLeaseResponse) { r.RetryAfterS = 60 }, []string{"retry_after_s"}},
		{"granted with a zero lease", func(r *StartLeaseResponse) { r.LeaseUntil = &time.Time{} }, []string{"lease_until"}},
	})
}

func TestValidateRunRecord(t *testing.T) {
	open := func(r *RunRecord) {
		r.Outcome, r.FinishedAt, r.ExitCode, r.SnapshotID, r.DeferredReason, r.ErrorClass = "", nil, nil, "", "", ""
	}
	runValidate(t, okRun, []vcase[RunRecord]{
		{"valid (deferred backup)", nil, nil},
		{"closed success", func(r *RunRecord) { *r = closedRun() }, nil},
		{"open run", open, nil},
		{"open run, retry trigger", func(r *RunRecord) { open(r); r.Trigger = TriggerRetry }, nil},
		{"finished exactly at start", func(r *RunRecord) { r.FinishedAt = ptr(r.StartedAt) }, nil},
		{"restore kind without snapshot", func(r *RunRecord) { r.Kind, r.SnapshotID = restic.KindRestore, "" }, nil},
		{"id lower case", func(r *RunRecord) { r.ID = strings.ToLower(r.ID) }, []string{"id"}},
		{"id short", func(r *RunRecord) { r.ID = r.ID[:25] }, []string{"id"}},
		{"kind bogus", func(r *RunRecord) { r.Kind = "bogus" }, []string{"kind", "snapshot_id"}},
		{"kind not recorded", func(r *RunRecord) { r.Kind, r.SnapshotID = restic.KindLs, "" }, []string{"kind"}},
		{"trigger bogus", func(r *RunRecord) { r.Trigger = "hunch" }, []string{"trigger"}},
		{"trigger empty", func(r *RunRecord) { r.Trigger = "" }, []string{"trigger"}},
		{"started_at zero", func(r *RunRecord) { r.StartedAt = time.Time{} }, []string{"started_at"}},
		{"started_at before 2001", func(r *RunRecord) { r.StartedAt = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC) }, []string{"started_at"}},
		{"started_at just before the bound", func(r *RunRecord) { r.StartedAt = MinTimestamp.Add(-time.Millisecond); r.FinishedAt = ptr(at(0)) }, []string{"started_at"}},
		{"started_at at the bound", func(r *RunRecord) { r.StartedAt = MinTimestamp; r.FinishedAt = ptr(at(0)) }, nil},
		{"finished before started", func(r *RunRecord) { r.FinishedAt = ptr(r.StartedAt.Add(-time.Second)) }, []string{"finished_at"}},
		{"finished before 2001", func(r *RunRecord) { r.FinishedAt = ptr(time.Date(1999, 1, 1, 0, 0, 0, 0, time.UTC)) }, []string{"finished_at"}},
		{"outcome bogus", func(r *RunRecord) { r.Outcome = "meh" }, []string{"outcome", "deferred_reason"}},
		{"outcome without finished_at", func(r *RunRecord) { r.FinishedAt = nil }, []string{"finished_at"}},
		{"finished_at without outcome", func(r *RunRecord) { r.Outcome, r.DeferredReason = "", "" }, []string{"outcome"}},
		{"deferred without reason", func(r *RunRecord) { r.DeferredReason = "" }, []string{"deferred_reason"}},
		{"reason on success", func(r *RunRecord) { r.Outcome = restic.OutcomeSuccess }, []string{"deferred_reason"}},
		{"reason bogus", func(r *RunRecord) { r.DeferredReason = "later" }, []string{"deferred_reason"}},
		{"snapshot on restore", func(r *RunRecord) { r.Kind = restic.KindRestore }, []string{"snapshot_id"}},
		{"snapshot on prune", func(r *RunRecord) { r.Kind = restic.KindPrune }, []string{"snapshot_id"}},
		{"snapshot short", func(r *RunRecord) { r.SnapshotID = hex6[:63] }, []string{"snapshot_id"}},
		{"snapshot upper case", func(r *RunRecord) { r.SnapshotID = strings.ToUpper(hex6) }, []string{"snapshot_id"}},
		{"snapshot not hex", func(r *RunRecord) { r.SnapshotID = strings.Repeat("g", 64) }, []string{"snapshot_id"}},
		{"error class bogus", func(r *RunRecord) { r.ErrorClass = "oops" }, []string{"error_class"}},
		{"attempt 0", func(r *RunRecord) { r.Attempt = 0 }, []string{"attempt"}},
		{"attempt 1", func(r *RunRecord) { r.Attempt = 1 }, nil},
		{"policy version negative", func(r *RunRecord) { r.PolicyVersion = -1 }, []string{"policy_version"}},
		{"policy version 0", func(r *RunRecord) { r.PolicyVersion = 0 }, nil},
		{"agent_version too long is a lost label, not a lost run", func(r *RunRecord) { r.AgentVersion = strings.Repeat("v", 129) }, nil},
		{"stats negative", func(r *RunRecord) { r.Stats.FilesNew = -1 }, []string{"stats.files_new"}},
		{"stats vss lower case", func(r *RunRecord) { r.Stats.VSSFallbackVolumes = []string{"c"} }, []string{"stats.vss_fallback_volumes[0]"}},
		{"error row class bogus", func(r *RunRecord) { r.ErrorCounts[0].Class = "x" }, []string{"error_counts[0].class"}},
		{"error row count 0", func(r *RunRecord) { r.ErrorCounts[1].Count = 0 }, []string{"error_counts[1].count"}},
		{"error row during path", func(r *RunRecord) { r.ErrorCounts[0].During = `C:\x` }, []string{"error_counts[0].during"}},
		{"error row ext not allowed", func(r *RunRecord) { r.ErrorCounts[0].Ext = "smith" }, []string{"error_counts[0].ext"}},
		{"error row ext docx", func(r *RunRecord) { r.ErrorCounts[0].Ext = "docx" }, nil},
		{"error row ext upper case", func(r *RunRecord) { r.ErrorCounts[0].Ext = "DOCX" }, []string{"error_counts[0].ext"}},
		{"too many error rows", func(r *RunRecord) {
			r.ErrorCounts = make([]restic.ErrorProjection, 129)
			for i := range r.ErrorCounts {
				r.ErrorCounts[i] = restic.ErrorProjection{During: "scan", Class: restic.ClassDisk, Count: 1}
			}
		}, []string{"error_counts"}},
	})
}

func TestValidateRunStats(t *testing.T) {
	runValidate(t, func() RunStats { return okRun().Stats }, []vcase[RunStats]{
		{"valid", nil, nil},
		{"zero", func(s *RunStats) { *s = RunStats{} }, nil},
		{"files_new", func(s *RunStats) { s.FilesNew = -1 }, []string{"files_new"}},
		{"files_changed", func(s *RunStats) { s.FilesChanged = -1 }, []string{"files_changed"}},
		{"files_unmodified", func(s *RunStats) { s.FilesUnmodified = -1 }, []string{"files_unmodified"}},
		{"dirs_new", func(s *RunStats) { s.DirsNew = -1 }, []string{"dirs_new"}},
		{"dirs_changed", func(s *RunStats) { s.DirsChanged = -1 }, []string{"dirs_changed"}},
		{"dirs_unmodified", func(s *RunStats) { s.DirsUnmodified = -1 }, []string{"dirs_unmodified"}},
		{"total_files_processed", func(s *RunStats) { s.TotalFilesProcessed = -1 }, []string{"total_files_processed"}},
		{"failed_files", func(s *RunStats) { s.FailedFiles = -1 }, []string{"failed_files"}},
		{"duration negative", func(s *RunStats) { s.TotalDurationS = -0.5 }, []string{"total_duration_s"}},
		{"duration NaN", func(s *RunStats) { s.TotalDurationS = math.NaN() }, []string{"total_duration_s"}},
		{"duration Inf", func(s *RunStats) { s.TotalDurationS = math.Inf(1) }, []string{"total_duration_s"}},
		{"vss two letters", func(s *RunStats) { s.VSSFallbackVolumes = []string{"CD"} }, []string{"vss_fallback_volumes[0]"}},
		{"vss empty entry", func(s *RunStats) { s.VSSFallbackVolumes = []string{""} }, []string{"vss_fallback_volumes[0]"}},
		{"lease bypassed", func(s *RunStats) { s.Lease = LeaseBypassed }, nil},
		{"lease not required", func(s *RunStats) { s.Lease = LeaseNotRequired }, nil},
		{"lease absent", func(s *RunStats) { s.Lease = "" }, nil},
		{"lease bogus", func(s *RunStats) { s.Lease = "denied" }, []string{"lease"}},
		{"vss digit", func(s *RunStats) { s.VSSFallbackVolumes = []string{"1"} }, []string{"vss_fallback_volumes[0]"}},
		{"vss 26 letters", func(s *RunStats) {
			s.VSSFallbackVolumes = strings.Split("ABCDEFGHIJKLMNOPQRSTUVWXYZ", "")
		}, nil},
		{"vss 27 entries", func(s *RunStats) {
			s.VSSFallbackVolumes = append(strings.Split("ABCDEFGHIJKLMNOPQRSTUVWXYZ", ""), "A")
		}, []string{"vss_fallback_volumes"}},
	})
}

func TestValidateRunsRequest(t *testing.T) {
	many := func(n int) []RunRecord {
		out := make([]RunRecord, n)
		for i := range out {
			out[i] = closedRun()
		}
		return out
	}
	runValidate(t, okRunsRequest, []vcase[RunsRequest]{
		{"valid", nil, nil},
		{"one run", func(r *RunsRequest) { r.Runs = r.Runs[:1] }, nil},
		{"200 runs", func(r *RunsRequest) { r.Runs = many(200) }, nil},
		{"201 runs", func(r *RunsRequest) { r.Runs = many(201) }, []string{"runs"}},
		{"no runs", func(r *RunsRequest) { r.Runs = nil }, []string{"runs"}},
		{"empty list", func(r *RunsRequest) { r.Runs = []RunRecord{} }, []string{"runs"}},
		{"second run invalid", func(r *RunsRequest) { r.Runs[1].ID = "bad" }, []string{"runs[1].id"}},
		{"nested path of a coupling", func(r *RunsRequest) { r.Runs[0].Attempt = 0 }, []string{"runs[0].attempt"}},
		{"nested stats path", func(r *RunsRequest) { r.Runs[1].Stats.FailedFiles = -4 }, []string{"runs[1].stats.failed_files"}},
		{"nested error row path", func(r *RunsRequest) { r.Runs[0].ErrorCounts[1].Count = 0 }, []string{"runs[0].error_counts[1].count"}},
	})
}

func TestValidateSnapshotsRequest(t *testing.T) {
	many := func(n int) []restic.Snapshot {
		s := okSnapshot()
		s.Summary = nil
		out := make([]restic.Snapshot, n)
		for i := range out {
			out[i] = s
		}
		return out
	}
	roots := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = `C:\Root`
		}
		return out
	}
	runValidate(t, okSnapshotsRequest, []vcase[SnapshotsRequest]{
		{"valid", nil, nil},
		{"empty list", func(r *SnapshotsRequest) { r.Snapshots = nil }, nil},
		{"5000 snapshots", func(r *SnapshotsRequest) { r.Snapshots = many(5000) }, nil},
		{"5001 snapshots", func(r *SnapshotsRequest) { r.Snapshots = many(5001) }, []string{"snapshots"}},
		{"id short", func(r *SnapshotsRequest) { r.Snapshots[0].ID = "abc" }, []string{"snapshots[0].id"}},
		{"id upper case", func(r *SnapshotsRequest) { r.Snapshots[0].ID = strings.ToUpper(hex6) }, []string{"snapshots[0].id"}},
		{"short id bad", func(r *SnapshotsRequest) { r.Snapshots[0].ShortID = "abc" }, []string{"snapshots[0].short_id"}},
		{"short id empty", func(r *SnapshotsRequest) { r.Snapshots[0].ShortID = "" }, nil},
		{"time zero", func(r *SnapshotsRequest) { r.Snapshots[0].Time = time.Time{} }, []string{"snapshots[0].time"}},
		{"time before 2001", func(r *SnapshotsRequest) { r.Snapshots[0].Time = time.Date(1999, 1, 1, 0, 0, 0, 0, time.UTC) }, []string{"snapshots[0].time"}},
		{"parent bad", func(r *SnapshotsRequest) { r.Snapshots[0].Parent = "xyz" }, []string{"snapshots[0].parent"}},
		{"parent empty", func(r *SnapshotsRequest) { r.Snapshots[0].Parent = "" }, nil},
		{"tree bad", func(r *SnapshotsRequest) { r.Snapshots[0].Tree = "xyz" }, []string{"snapshots[0].tree"}},
		{"64 roots", func(r *SnapshotsRequest) { r.Snapshots[0].Paths = roots(64) }, nil},
		{"65 roots", func(r *SnapshotsRequest) { r.Snapshots[0].Paths = roots(65) }, []string{"snapshots[0].paths"}},
		{"no roots", func(r *SnapshotsRequest) { r.Snapshots[0].Paths = nil }, nil},
		{"empty root", func(r *SnapshotsRequest) { r.Snapshots[0].Paths = []string{""} }, []string{"snapshots[0].paths[0]"}},
		{"root too long", func(r *SnapshotsRequest) { r.Snapshots[0].Paths = []string{strings.Repeat("a", 1025)} }, []string{"snapshots[0].paths[0]"}},
		{"root control char", func(r *SnapshotsRequest) { r.Snapshots[0].Paths = []string{"a\nb"} }, []string{"snapshots[0].paths[0]"}},
		{"hostname with slash", func(r *SnapshotsRequest) { r.Snapshots[0].Hostname = "a/b" }, []string{"snapshots[0].hostname"}},
		{"hostname empty", func(r *SnapshotsRequest) { r.Snapshots[0].Hostname = "" }, nil},
		{"username must be empty", func(r *SnapshotsRequest) { r.Snapshots[0].Username = `DESKTOP\user` }, []string{"snapshots[0].username"}},
		{"too many tags", func(r *SnapshotsRequest) { r.Snapshots[0].Tags = strings.Split(strings.Repeat("t,", 17), ",")[:17] }, []string{"snapshots[0].tags"}},
		{"tag too long", func(r *SnapshotsRequest) { r.Snapshots[0].Tags = []string{strings.Repeat("t", 65)} }, []string{"snapshots[0].tags[0]"}},
		{"program version too long", func(r *SnapshotsRequest) { r.Snapshots[0].ProgramVersion = strings.Repeat("1", 129) }, []string{"snapshots[0].program_version"}},
	})
}

func TestValidateCommand(t *testing.T) {
	big := json.RawMessage(`{"k":"` + strings.Repeat("x", MaxCommandParams) + `"}`)
	exact := func(n int) json.RawMessage {
		pad := n - len(`{"reason":""}`)
		return json.RawMessage(`{"reason":"` + strings.Repeat("x", pad) + `"}`)
	}
	runValidate(t, okCommand, []vcase[Command]{
		{"valid hold", nil, nil},
		{"backup_now no params", func(c *Command) { *c = Command{ID: ulidA, Type: CommandBackupNow, CreatedAt: t0, ExpiresAt: at(1)} }, nil},
		{"backup_now empty object", func(c *Command) { c.Type, c.Params = CommandBackupNow, json.RawMessage(`{}`) }, nil},
		{"check_in with ignored field", func(c *Command) { c.Type, c.Params = CommandCheckIn, json.RawMessage(`{"x":1}`) }, nil},
		{"pause without params", func(c *Command) { c.Type, c.Params = CommandPause, nil }, nil},
		{"pause with until and reason", func(c *Command) {
			c.Type, c.Params = CommandPause, json.RawMessage(`{"until":"2026-10-22T15:00:00Z","reason":"holiday"}`)
		}, nil},
		{"restore valid", func(c *Command) { *c = okCommandsResponse().Commands[1] }, nil},
		{"update_agent valid", func(c *Command) { c.Type, c.Params = CommandUpdateAgent, json.RawMessage(`{"version":"1.3.0"}`) }, nil},
		{"id bad", func(c *Command) { c.ID = "x" }, []string{"id"}},
		{"type bogus", func(c *Command) { c.Type = "format_disk" }, []string{"type"}},
		{"type empty", func(c *Command) { c.Type = "" }, []string{"type"}},
		{"created_at zero", func(c *Command) { c.CreatedAt = time.Time{} }, []string{"created_at"}},
		{"expires_at zero", func(c *Command) { c.ExpiresAt = time.Time{} }, []string{"expires_at"}},
		{"expires equals created", func(c *Command) { c.ExpiresAt = c.CreatedAt }, []string{"expires_at"}},
		{"expires before created", func(c *Command) { c.ExpiresAt = c.CreatedAt.Add(-time.Second) }, []string{"expires_at"}},
		{"params over 4 KiB", func(c *Command) { c.Type, c.Params = CommandBackupNow, big }, []string{"params"}},
		{"params exactly 4 KiB", func(c *Command) {
			c.Type = CommandPause
			c.Params = exact(MaxCommandParams)
		}, []string{"params.reason"}}, // within the size cap; the reason itself is too long
		{"params an array", func(c *Command) { c.Type, c.Params = CommandBackupNow, json.RawMessage(`[1]`) }, []string{"params"}},
		{"params null", func(c *Command) { c.Type, c.Params = CommandBackupNow, json.RawMessage(`null`) }, []string{"params"}},
		{"params not json", func(c *Command) { c.Type, c.Params = CommandBackupNow, json.RawMessage(`{`) }, []string{"params"}},
		{"hold without params", func(c *Command) { c.Params = nil }, []string{"params"}},
		{"hold with empty object", func(c *Command) { c.Params = json.RawMessage(`{}`) }, []string{"params.until"}},
		{"hold with wrong type", func(c *Command) { c.Params = json.RawMessage(`{"until":5}`) }, []string{"params"}},
		{"pause reason too long", func(c *Command) {
			c.Type, c.Params = CommandPause, json.RawMessage(`{"reason":"`+strings.Repeat("r", 129)+`"}`)
		}, []string{"params.reason"}},
		{"restore without id", func(c *Command) { c.Type, c.Params = CommandRestore, json.RawMessage(`{}`) }, []string{"params.restore_request_id"}},
		{"restore without params", func(c *Command) { c.Type, c.Params = CommandRestore, nil }, []string{"params"}},
		{"restore bad id", func(c *Command) { c.Type, c.Params = CommandRestore, json.RawMessage(`{"restore_request_id":"abc"}`) }, []string{"params.restore_request_id"}},
		{"update_agent without version", func(c *Command) { c.Type, c.Params = CommandUpdateAgent, json.RawMessage(`{}`) }, []string{"params.version"}},
		{"update_agent without params", func(c *Command) { c.Type, c.Params = CommandUpdateAgent, nil }, []string{"params"}},
	})
}

func TestValidateCommandParams(t *testing.T) {
	if (HoldParams{Until: t0}).Validate() != nil || (HoldParams{}).Validate() == nil {
		t.Error("HoldParams: Until is required")
	}
	if (PauseParams{}).Validate() != nil || (PauseParams{Reason: strings.Repeat("r", 129)}).Validate() == nil {
		t.Error("PauseParams: reason is capped, Until is optional")
	}
	if (RestoreParams{RestoreRequestID: ulidC}).Validate() != nil || (RestoreParams{}).Validate() == nil {
		t.Error("RestoreParams: a ULID is required")
	}
	if (UpdateAgentParams{Version: "1"}).Validate() != nil || (UpdateAgentParams{}).Validate() == nil {
		t.Error("UpdateAgentParams: version is required")
	}
}

func TestValidateCommandResult(t *testing.T) {
	runValidate(t, okCommandResult, []vcase[CommandResultRequest]{
		{"valid failed with class", nil, nil},
		{"acked", func(r *CommandResultRequest) { *r = CommandResultRequest{Status: CommandAcked} }, nil},
		{"succeeded with result", func(r *CommandResultRequest) { r.Status, r.ErrorClass = CommandSucceeded, "" }, nil},
		{"failed without class", func(r *CommandResultRequest) { r.ErrorClass = "" }, nil},
		{"status bogus", func(r *CommandResultRequest) { r.Status = "queued" }, []string{"status", "error_class"}},
		{"status empty", func(r *CommandResultRequest) { r.Status = "" }, []string{"status", "error_class"}},
		{"status delivered is the console's", func(r *CommandResultRequest) { r.Status = "delivered" }, []string{"status", "error_class"}},
		{"class on success", func(r *CommandResultRequest) { r.Status = CommandSucceeded }, []string{"error_class"}},
		{"class bogus", func(r *CommandResultRequest) { r.ErrorClass = "oops" }, []string{"error_class"}},
		{"result an array", func(r *CommandResultRequest) { r.Result = json.RawMessage(`[]`) }, []string{"result"}},
		{"result not json", func(r *CommandResultRequest) { r.Result = json.RawMessage(`{"a":`) }, []string{"result"}},
		{"result 16 KiB", func(r *CommandResultRequest) {
			pad := MaxCommandResult - len(`{"k":""}`)
			r.Result = json.RawMessage(`{"k":"` + strings.Repeat("x", pad) + `"}`)
		}, nil},
		{"result over 16 KiB", func(r *CommandResultRequest) {
			pad := MaxCommandResult - len(`{"k":""}`) + 1
			r.Result = json.RawMessage(`{"k":"` + strings.Repeat("x", pad) + `"}`)
		}, []string{"result"}},
	})
}
