package adminpath

import "testing"

// P22-E: a single Path is clean and absolute exactly like an entry of Paths, so
// "<snap>:<sub>" plus "/../X" can no longer climb out of the subfolder the
// worker joins it to. "/" stays allowed for a single Path (the snapshot root,
// or the whole subfolder).
func TestDumpSinglePathMustBeClean(t *testing.T) {
	dev := DeviceRequest{DeviceID: "dev1"}
	for _, p := range []string{"/../X", "/tree/../../X", "/tree/./report.docx", "/tree/", "//tree", "/tree//x", "tree/x", ""} {
		for _, snap := range []string{"latest", "latest:/C/fixture"} {
			if err := (DumpRequest{DeviceRequest: dev, SnapshotID: snap, Path: p}).Validate(); err == nil {
				t.Errorf("snapshot %q path %q: accepted", snap, p)
			}
		}
	}
	for _, p := range []string{"/", "/tree", "/tree/report.docx", "/C/Users/Public/a b.txt"} {
		if err := (DumpRequest{DeviceRequest: dev, SnapshotID: "latest:/C/fixture", Path: p}).Validate(); err != nil {
			t.Errorf("path %q: %v", p, err)
		}
	}
}
