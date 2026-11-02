package config

import (
	"strings"
	"testing"
)

// TestRestoreStreamGuard (M1-HIPAA2 item 1): the minimum-rate guard of a
// download defaults to 64 KiB in 120 s, is set by restore_stream.* or
// CRYO_RESTORE_STREAM_*, and out-of-range values are refused.
func TestRestoreStreamGuard(t *testing.T) {
	d := Default()
	if d.RestoreStream.MinBytes != 65536 || d.RestoreStream.WindowS != 120 || d.RestoreStream.FirstByteS != 300 {
		t.Fatalf("defaults = %+v, want 65536 bytes in 120 s and a 300 s first-byte bound", d.RestoreStream)
	}
	env := map[string]string{"CRYO_RESTORE_STREAM_MIN_BYTES": "1024", "CRYO_RESTORE_STREAM_WINDOW_S": "60", "CRYO_RESTORE_STREAM_FIRST_BYTE_S": "90"}
	c, err := Load("config.example.json", func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if c.RestoreStream.MinBytes != 1024 || c.RestoreStream.WindowS != 60 || c.RestoreStream.FirstByteS != 90 {
		t.Fatalf("env overrides: %+v", c.RestoreStream)
	}
	for _, bad := range []map[string]string{
		{"CRYO_RESTORE_STREAM_MIN_BYTES": "0"},
		{"CRYO_RESTORE_STREAM_WINDOW_S": "5"},
		{"CRYO_RESTORE_STREAM_WINDOW_S": "7200"},
		{"CRYO_RESTORE_STREAM_FIRST_BYTE_S": "1"},
		{"CRYO_RESTORE_STREAM_FIRST_BYTE_S": "86400"},
	} {
		_, err := Load("config.example.json", func(k string) string { return bad[k] })
		if err == nil || !strings.Contains(err.Error(), "restore_stream.") {
			t.Errorf("%v: err = %v, want a restore_stream problem", bad, err)
		}
	}
}
