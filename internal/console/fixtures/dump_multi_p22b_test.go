package fixtures

import (
	"archive/zip"
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
)

// The fixtures Backend answers the P22 additions the way the worker does: a
// several-paths dump is one zip named relative to each selection's parent, and
// Ls returns a window with the total.

func TestFixtureDumpSeveralPaths(t *testing.T) {
	b := &Backend{}
	var out bytes.Buffer
	req := adminpath.DumpRequest{DeviceRequest: adminpath.DeviceRequest{DeviceID: "dev1"}, SnapshotID: "latest",
		Paths: []string{"/C/Users/Public/Documents", "/C/Users/Public/Desktop/Welcome.txt", "/C/Users/Public/Documents/Budget-2026.csv"}}
	if _, err := b.Dump(context.Background(), req, &out); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(out.Bytes()), int64(out.Len()))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	want := "Documents/|Documents/Quarterly-Report.txt|Documents/Budget-2026.csv|Welcome.txt"
	if strings.Join(names, "|") != want {
		t.Fatalf("entries = %q, want %q", names, want)
	}
	if c := b.Calls(); len(c) != 1 || len(c[0].Paths) != 3 {
		t.Fatalf("calls = %+v", c)
	}

	// A missing selection is not_found; 51 paths are invalid.
	req.Paths = []string{"/C/Users/Public/Desktop/Welcome.txt", "/C/nope"}
	if _, err := b.Dump(context.Background(), req, &out); !isCode(err, adminpath.CodeNotFound) {
		t.Fatalf("missing selection: %v", err)
	}
	req.Paths = make([]string, adminpath.MaxDumpPaths+1)
	for i := range req.Paths {
		req.Paths[i] = "/C/x" + strings.Repeat("y", i)
	}
	if _, err := b.Dump(context.Background(), req, &out); !isCode(err, adminpath.CodeInvalidRequest) {
		t.Fatalf("51 paths: %v", err)
	}
}

func TestFixtureLsWindow(t *testing.T) {
	b := &Backend{}
	req := adminpath.LsRequest{DeviceRequest: adminpath.DeviceRequest{DeviceID: "dev1"}, SnapshotID: "latest", Dir: "/C/Users/Public"}
	all, err := b.Ls(context.Background(), req)
	if err != nil || all.Total != 3 || len(all.Nodes) != 3 || all.Truncated {
		t.Fatalf("whole listing = %+v, %v", all, err)
	}
	req.Offset, req.Limit = 1, 1
	w, err := b.Ls(context.Background(), req)
	if err != nil || w.Total != 3 || len(w.Nodes) != 1 || w.Nodes[0].Name != all.Nodes[1].Name || !w.Truncated {
		t.Fatalf("window = %+v, %v", w, err)
	}
	req.Offset = 9
	if p, _ := b.Ls(context.Background(), req); len(p.Nodes) != 0 || p.Total != 3 || p.Truncated {
		t.Fatalf("past the end = %+v", p)
	}
}

func isCode(err error, code string) bool {
	e, ok := err.(*adminpath.Error)
	return ok && e.Code == code
}
