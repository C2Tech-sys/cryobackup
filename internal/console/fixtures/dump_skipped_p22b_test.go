package fixtures

import (
	"archive/zip"
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
)

// TestFixtureDumpCountsSkipped: a fake file with Skip set is listed with its
// node type, left out of the zip and counted in DumpResult.Skipped, as the
// worker does (ADR 0028), for one path and for several; a selection that is
// itself a link is found (counted), not not_found.
func TestFixtureDumpCountsSkipped(t *testing.T) {
	b := &Backend{Files: []File{
		{Path: "/C/Data/a.txt", Content: []byte("a")},
		{Path: "/C/Data/jn", Skip: adminpath.SkipSymlink},
		{Path: "/C/Data/pipe", Skip: adminpath.SkipPipe},
		{Path: "/C/Other/link", Skip: adminpath.SkipSymlink},
	}}
	ls, err := b.Ls(context.Background(), adminpath.LsRequest{DeviceRequest: adminpath.DeviceRequest{DeviceID: "dev1"}, SnapshotID: "latest", Dir: "/C/Data"})
	if err != nil {
		t.Fatal(err)
	}
	types := map[string]string{}
	for _, n := range ls.Nodes {
		types[n.Name] = n.Type
	}
	if types["a.txt"] != "file" || types["jn"] != "symlink" || types["pipe"] != "fifo" {
		t.Fatalf("node types = %v", types)
	}

	names := func(data []byte) string {
		zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			t.Fatal(err)
		}
		var n []string
		for _, f := range zr.File {
			n = append(n, f.Name)
		}
		return strings.Join(n, "|")
	}
	dev := adminpath.DeviceRequest{DeviceID: "dev1"}

	var one bytes.Buffer
	res, err := b.Dump(context.Background(), adminpath.DumpRequest{DeviceRequest: dev, SnapshotID: "latest", Path: "/C/Data"}, &one)
	if err != nil {
		t.Fatal(err)
	}
	// One path is built like a list of one (P22-E): the folder's own entry,
	// then its files, then the skipped-entries file.
	if got := names(one.Bytes()); got != "Data/|Data/a.txt|"+SkippedEntryName || res.Skipped != (adminpath.DumpSkipped{Symlinks: 1, Pipes: 1}) {
		t.Fatalf("one path: entries %q, skipped %+v", got, res.Skipped)
	}

	var many bytes.Buffer
	res, err = b.Dump(context.Background(), adminpath.DumpRequest{DeviceRequest: dev, SnapshotID: "latest", Paths: []string{"/C/Data", "/C/Other/link"}}, &many)
	if err != nil {
		t.Fatal(err)
	}
	if got := names(many.Bytes()); got != "Data/|Data/a.txt|"+SkippedEntryName || res.Skipped != (adminpath.DumpSkipped{Symlinks: 2, Pipes: 1}) {
		t.Fatalf("several paths: entries %q, skipped %+v", got, res.Skipped)
	}
}
