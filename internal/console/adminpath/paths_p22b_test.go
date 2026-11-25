package adminpath

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// Tests of the P22 additions to the wire contract: DumpRequest.Paths (ADR
// 0028), CollapsePaths, LsRequest.Offset and LsResult.Total.

func manyPaths(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "/C/Users/Public/f" + strconv.Itoa(i)
	}
	return out
}

func TestDumpPathsValidation(t *testing.T) {
	good := DeviceRequest{DeviceID: "dev1", RequestID: "v1"}
	ok := []DumpRequest{
		{DeviceRequest: good, SnapshotID: "latest", Paths: []string{"/C/a"}},
		{DeviceRequest: good, SnapshotID: strings.Repeat("ab", 32), Paths: []string{"/C/a", "/C/b/c d.txt", "/C/a/inner"}},
		{DeviceRequest: good, SnapshotID: "latest", Paths: manyPaths(MaxDumpPaths)},
		{DeviceRequest: good, SnapshotID: "latest", Paths: []string{"/C/café/日本.txt"}},
		{DeviceRequest: good, SnapshotID: "latest:/C", Path: "/x"}, // the single-path form is unchanged
	}
	for i, r := range ok {
		if err := r.Validate(); err != nil {
			t.Errorf("valid request %d refused: %v", i, err)
		}
	}
	bad := map[string]DumpRequest{
		"51 paths":            {DeviceRequest: good, SnapshotID: "latest", Paths: manyPaths(MaxDumpPaths + 1)},
		"empty list":          {DeviceRequest: good, SnapshotID: "latest", Paths: []string{}},
		"both path and paths": {DeviceRequest: good, SnapshotID: "latest", Path: "/C/a", Paths: []string{"/C/b"}},
		"neither":             {DeviceRequest: good, SnapshotID: "latest"},
		"root":                {DeviceRequest: good, SnapshotID: "latest", Paths: []string{"/C/a", "/"}},
		"relative":            {DeviceRequest: good, SnapshotID: "latest", Paths: []string{"C/a"}},
		"dot dot":             {DeviceRequest: good, SnapshotID: "latest", Paths: []string{"/C/a/../b"}},
		"dot":                 {DeviceRequest: good, SnapshotID: "latest", Paths: []string{"/C/./a"}},
		"double slash":        {DeviceRequest: good, SnapshotID: "latest", Paths: []string{"/C//a"}},
		"trailing slash":      {DeviceRequest: good, SnapshotID: "latest", Paths: []string{"/C/a/"}},
		"empty path":          {DeviceRequest: good, SnapshotID: "latest", Paths: []string{""}},
		"control character":   {DeviceRequest: good, SnapshotID: "latest", Paths: []string{"/C/a\nb"}},
		"subfolder snapshot":  {DeviceRequest: good, SnapshotID: "latest:/C", Paths: []string{"/a"}},
		"flag-like snapshot":  {DeviceRequest: good, SnapshotID: "--target=x", Paths: []string{"/a"}},
		"bad device":          {DeviceRequest: DeviceRequest{DeviceID: "../x"}, SnapshotID: "latest", Paths: []string{"/a"}},
	}
	for name, r := range bad {
		err := r.Validate()
		var ve *ValidationError
		if !errors.As(err, &ve) {
			t.Errorf("%s: Validate = %v, want a ValidationError", name, err)
		}
	}
}

func TestCollapsePaths(t *testing.T) {
	for _, tc := range []struct{ in, want []string }{
		{[]string{"/C/a", "/C/a/b", "/C/c"}, []string{"/C/a", "/C/c"}},
		{[]string{"/C/a/b/c.txt", "/C/a"}, []string{"/C/a"}},
		{[]string{"/C/a", "/C/a", "/C/ab"}, []string{"/C/a", "/C/ab"}}, // a sibling with a common prefix stays
		{[]string{"/C/x.txt", "/D/x.txt"}, []string{"/C/x.txt", "/D/x.txt"}},
		{[]string{"/C/a/b", "/C/a/b/c", "/C/a/b"}, []string{"/C/a/b"}},
		{nil, []string{}},
	} {
		if got := CollapsePaths(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("CollapsePaths(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestLsOffsetValidationAndWire(t *testing.T) {
	good := DeviceRequest{DeviceID: "dev1", RequestID: "v1"}
	if err := (LsRequest{DeviceRequest: good, SnapshotID: "latest", Dir: "/", Offset: -1}).Validate(); err == nil {
		t.Fatal("a negative offset was accepted")
	}
	if err := (LsRequest{DeviceRequest: good, SnapshotID: "latest", Dir: "/", Offset: 1 << 30, Limit: MaxLsLimit}).Validate(); err != nil {
		t.Fatalf("a large offset was refused: %v", err)
	}
	// The new fields cross the socket.
	fb := newFake()
	c, _ := startServer(t, fb, ClientOptions{})
	res, err := c.Ls(context.Background(), LsRequest{DeviceRequest: good, SnapshotID: "latest", Dir: "/C", Offset: 500, Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	if got := fb.last(MethodLs).(LsRequest); got.Offset != 500 || got.Limit != 500 {
		t.Fatalf("the worker received %+v", got)
	}
	if res.Total != fixLs().Total {
		t.Fatalf("Total = %d, want %d", res.Total, fixLs().Total)
	}
	data, _ := json.Marshal(LsResult{Total: 7})
	if !bytes.Contains(data, []byte(`"total":7`)) {
		t.Fatalf("LsResult JSON lacks total: %s", data)
	}
	data, _ = json.Marshal(LsRequest{DeviceRequest: good, SnapshotID: "latest", Dir: "/"})
	if bytes.Contains(data, []byte("offset")) {
		t.Fatalf("a zero offset is on the wire: %s", data)
	}
}

func TestDumpPathsCrossTheSocket(t *testing.T) {
	fb := newFake()
	c, _ := startServer(t, fb, ClientOptions{})
	req := DumpRequest{DeviceRequest: DeviceRequest{DeviceID: "dev1"}, SnapshotID: "latest", Paths: []string{"/C/a", "/C/b.txt"}}
	var out bytes.Buffer
	if _, err := c.Dump(context.Background(), req, &out); err != nil {
		t.Fatal(err)
	}
	got := fb.last(MethodDump).(DumpRequest)
	if !reflect.DeepEqual(got.Paths, req.Paths) || got.Path != "" {
		t.Fatalf("the worker received %+v", got)
	}
	// The client refuses a 51st path before anything is sent.
	n := fb.count(MethodDump)
	req.Paths = manyPaths(MaxDumpPaths + 1)
	_, err := c.Dump(context.Background(), req, &out)
	var e *Error
	if !errors.As(err, &e) || e.Code != CodeInvalidRequest || fb.count(MethodDump) != n {
		t.Fatalf("51 paths: err %v, worker calls %d -> %d", err, n, fb.count(MethodDump))
	}
	data, _ := json.Marshal(DumpRequest{DeviceRequest: DeviceRequest{DeviceID: "dev1"}, SnapshotID: "latest", Paths: []string{"/a"}})
	if bytes.Contains(data, []byte(`"path"`)) {
		t.Fatalf("an empty path is on the wire next to paths: %s", data)
	}
}
