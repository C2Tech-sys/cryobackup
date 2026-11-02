package api

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden files under testdata")

// TestRoundTrip marshals every wire type with all fields set, decodes it into a
// fresh value and requires equality and a byte-identical second encoding.
func TestRoundTrip(t *testing.T) {
	for _, v := range wireValues() {
		v := v
		typ := reflect.TypeOf(v)
		t.Run(typ.Name(), func(t *testing.T) {
			assertAllSet(t, v)
			first, err := json.Marshal(v)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			back := reflect.New(typ)
			if err := json.Unmarshal(first, back.Interface()); err != nil {
				t.Fatalf("unmarshal: %v\n%s", err, first)
			}
			if !reflect.DeepEqual(v, back.Elem().Interface()) {
				t.Errorf("round trip changed the value:\n  in:  %+v\n  out: %+v", v, back.Elem().Interface())
			}
			second, err := json.Marshal(back.Elem().Interface())
			if err != nil {
				t.Fatalf("re-marshal: %v", err)
			}
			if !bytes.Equal(first, second) {
				t.Errorf("second encoding differs:\n%s\n%s", first, second)
			}
		})
	}
}

// TestRoundTripSparse covers the optional fields: absent pointers and empty
// enums disappear from the JSON and come back absent.
func TestRoundTripSparse(t *testing.T) {
	cases := []any{
		HeartbeatRequest{AgentVersion: "1", DeviceTime: t0, ServiceState: ServiceIdle},
		RunRecord{ID: ulidA, Kind: "backup", Trigger: TriggerManual, StartedAt: t0, Attempt: 1},
		PolicyResponse{Version: 1, ETag: `"x"`, ServerTime: t0},
		LobbyResponse{Status: LobbyPending, PollURL: RouteEnrollLobbyPoll(hexN)},
		CommandResultRequest{Status: CommandAcked},
		Command{ID: ulidA, Type: CommandBackupNow, CreatedAt: t0, ExpiresAt: at(1)},
		RestoreSelection{ID: ulidA, SnapshotID: hex6, Mode: RestoreDeviceOriginal, Overwrite: OverwriteNever, ExpiresAt: t0},
		StartLeaseResponse{Granted: false, RetryAfterS: 60, ServerTime: t0},
		StartLeaseResponse{Granted: true, ServerTime: t0},
	}
	for _, v := range cases {
		typ := reflect.TypeOf(v)
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("%s: %v", typ.Name(), err)
		}
		back := reflect.New(typ)
		if err := json.Unmarshal(b, back.Interface()); err != nil {
			t.Fatalf("%s: %v", typ.Name(), err)
		}
		if !reflect.DeepEqual(v, back.Elem().Interface()) {
			t.Errorf("%s: sparse round trip changed the value: %s", typ.Name(), b)
		}
	}
	b, _ := json.Marshal(HeartbeatRequest{})
	for _, key := range []string{"on_ac", "battery_pct", "metered", "vpn", "progress", "last_error_class", "deferred_reason", "active_run_id"} {
		if bytes.Contains(b, []byte(`"`+key+`"`)) {
			t.Errorf("empty HeartbeatRequest still encodes %q: %s", key, b)
		}
	}
}

// goldenCases are the types whose exact JSON is pinned under testdata. The
// policy sub-document is masked: its shape is internal/policy's to pin.
var goldenCases = []struct {
	file string
	val  func() any
	mask [][]string // paths whose value is replaced by a placeholder
}{
	{"enroll_response.json", func() any { return okEnrollResponse() }, [][]string{{"policy", "policy"}}},
	{"policy_response.json", func() any { return okPolicyResponse() }, [][]string{{"policy"}}},
	{"heartbeat_request.json", func() any { return okHeartbeat() }, nil},
	{"run_record.json", func() any { return okRun() }, nil},
	{"commands_response.json", func() any { return okCommandsResponse() }, nil},
	{"start_lease_request.json", func() any { return okStartLeaseRequest() }, nil},
	{"start_lease_response.json", func() any { return okStartLeaseGranted() }, nil},
}

func TestGoldenJSON(t *testing.T) {
	for _, gc := range goldenCases {
		gc := gc
		t.Run(gc.file, func(t *testing.T) {
			raw, err := json.Marshal(gc.val())
			if err != nil {
				t.Fatal(err)
			}
			var doc any
			if err := json.Unmarshal(raw, &doc); err != nil {
				t.Fatal(err)
			}
			for _, p := range gc.mask {
				if !maskAt(doc, p) {
					t.Fatalf("mask path %v not found in the encoding", p)
				}
			}
			var buf bytes.Buffer
			enc := json.NewEncoder(&buf)
			enc.SetEscapeHTML(false)
			enc.SetIndent("", "  ")
			if err := enc.Encode(doc); err != nil {
				t.Fatal(err)
			}
			got := buf.Bytes()
			path := filepath.Join("testdata", gc.file)
			if *update {
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read golden (run with -update to create it): %v", err)
			}
			if !bytes.Equal(bytes.ReplaceAll(want, []byte("\r\n"), []byte("\n")), got) {
				t.Errorf("golden mismatch for %s (a JSON key or value changed; if intended, run go test -update ./internal/api and review the diff)\n--- got ---\n%s", gc.file, got)
			}
		})
	}
}

// maskAt replaces the value at path inside doc (decoded JSON) by a placeholder.
func maskAt(doc any, path []string) bool {
	m, ok := doc.(map[string]any)
	if !ok {
		return false
	}
	if len(path) == 1 {
		if _, ok := m[path[0]]; !ok {
			return false
		}
		m[path[0]] = "<policy.Policy: pinned by internal/policy>"
		return true
	}
	return maskAt(m[path[0]], path[1:])
}
