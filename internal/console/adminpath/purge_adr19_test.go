package adminpath

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// ADR 0019: purge_repo accepts a canonical ULID as the device id and nothing
// else, and its request has no field that could carry a location.

const purgeULID = "01HZY5X8M3K7Q2W9RTV4BN6CDE"

func TestPurgeRepoValidateADR19(t *testing.T) {
	ok := func(id string) PurgeRepoRequest {
		return PurgeRepoRequest{DeviceRequest: DeviceRequest{DeviceID: id}}
	}
	for _, id := range []string{purgeULID, "00000000000000000000000000", "7ZZZZZZZZZZZZZZZZZZZZZZZZZ"} {
		if err := ok(id).Validate(); err != nil {
			t.Errorf("Validate(%q) = %v, want nil", id, err)
		}
	}
	bad := map[string]string{
		"empty":           "",
		"probe":           ProbeRepoID,
		"short id":        "dev1",
		"25 characters":   purgeULID[:25],
		"27 characters":   purgeULID + "0",
		"lower case":      strings.ToLower(purgeULID),
		"first above 7":   "8" + purgeULID[1:],
		"letter I":        purgeULID[:25] + "I",
		"letter L":        purgeULID[:25] + "L",
		"letter O":        purgeULID[:25] + "O",
		"letter U":        purgeULID[:25] + "U",
		"dash":            purgeULID[:25] + "-",
		"underscore":      purgeULID[:25] + "_",
		"slash":           purgeULID[:25] + "/",
		"dot dot":         purgeULID[:24] + "..",
		"wildcard":        purgeULID[:25] + "*",
		"space":           purgeULID[:25] + " ",
		"leading dash":    "-" + purgeULID[1:],
		"non-ascii":       purgeULID[:24] + "é",
		"control":         purgeULID[:25] + "\x00",
		"colon":           purgeULID[:25] + ":",
		"traversal":       "../../" + purgeULID[6:],
		"a valid id, too": "01HZY5X8M3K7Q2W9RTV4BN6CDE/01HZY5X8M3K7Q2W9RTV4BN6CDE",
	}
	for name, id := range bad {
		err := ok(id).Validate()
		var ve *ValidationError
		if !errors.As(err, &ve) || ve.Field != "device_id" {
			t.Errorf("%s: Validate(%q) = %v, want invalid device_id", name, id, err)
		}
	}
	r := ok(purgeULID)
	r.RequestID = "../x"
	var ve *ValidationError
	if err := r.Validate(); !errors.As(err, &ve) || ve.Field != "request_id" {
		t.Errorf("bad request id: %v", err)
	}
}

// TestPurgeRepoWireADR19 pins the wire form: the request is {device_id,
// request_id} and a body with any other field (a path, a remote, a bucket, a
// prefix) is rejected by the handler's strict decoder; the result has exactly
// the documented fields and its request id is echoed by the handler.
func TestPurgeRepoWireADR19(t *testing.T) {
	req := PurgeRepoRequest{DeviceRequest: DeviceRequest{DeviceID: purgeULID, RequestID: "r1"}}
	b, err := json.Marshal(req)
	if err != nil || string(b) != `{"device_id":"`+purgeULID+`","request_id":"r1"}` {
		t.Fatalf("request JSON = %s, %v", b, err)
	}
	for _, extra := range []string{"path", "remote", "bucket", "prefix", "location", "target"} {
		var got PurgeRepoRequest
		body := `{"device_id":"` + purgeULID + `","` + extra + `":"x"}`
		if err := decodeStrict([]byte(body), &got); err == nil {
			t.Errorf("a request with %q was accepted", extra)
		}
	}

	res := PurgeRepoResult{RequestID: "r1", Elapsed: time.Second, ObjectsRemoved: 3, BytesRemoved: 4, RemainingObjects: 5, ErrorClass: restic.ClassStorage}
	b, err = json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	want := []string{"bytes_removed", "elapsed_ns", "error_class", "objects_removed", "remaining_objects", "request_id"}
	if !reflect.DeepEqual(keys, want) {
		t.Errorf("result keys = %v, want %v", keys, want)
	}
	var back PurgeRepoResult
	if err := json.Unmarshal(b, &back); err != nil || back != res {
		t.Errorf("round trip = %+v, %v", back, err)
	}
	if b, _ := json.Marshal(PurgeRepoResult{}); strings.Contains(string(b), "error_class") {
		t.Errorf("error_class must be omitted when empty: %s", b)
	}

	// The handler echoes the request id into the result (requestIDer).
	var r PurgeRepoResult
	if m, ok := any(&r).(requestIDer); !ok {
		t.Fatal("PurgeRepoResult does not take the echoed request id")
	} else {
		m.setRequestID("echo")
	}
	if r.RequestID != "echo" {
		t.Errorf("request id = %q", r.RequestID)
	}
	if MethodPurgeRepo != "purge_repo" {
		t.Errorf("method name = %q", MethodPurgeRepo)
	}
}

func fixPurge() PurgeRepoResult {
	return PurgeRepoResult{Elapsed: 3 * time.Second, ObjectsRemoved: 41, BytesRemoved: 1 << 20, RemainingObjects: 2, ErrorClass: restic.ClassAuth}
}

func (f *fakeBackend) PurgeRepo(ctx context.Context, req PurgeRepoRequest) (PurgeRepoResult, error) {
	if err := f.enter(ctx, MethodPurgeRepo, req); err != nil {
		return PurgeRepoResult{}, err
	}
	return fixPurge(), nil
}
