package api

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/restic"
)

func TestLooksLikePath(t *testing.T) {
	paths := []string{
		`C:\Users\x\Documents\a.docx`, `c:/Users/x`, `D:\`, `\\server\share\x`, `\\`,
		`/home/x/y`, `/var/log/x.log`, `//server/share`, ` /home/x/y`, `see C:\Users\x for details`,
		`file=/home/x/y`, `"/home/x/y"`, `%APPDATA%\x`, `%ProgramFiles(x86)%`, `%USERPROFILE%`,
		`DOMAIN\user`, `\Users`, `a\b`, `x;/etc/passwd/x`,
	}
	for _, s := range paths {
		if !looksLikePath(s) {
			t.Errorf("looksLikePath(%q) = false, want true", s)
		}
	}
	clean := []string{
		"", "docx", "windows/amd64", "https://example.com/a/b", "http://host", "10.0.26300", "1.2.3+abc",
		"restic 0.19.1 compiled with go1.25 on windows/amd64", "/etc", "/", "a:b", "C:", "x-y_z", "1/2",
		"50% of 100%", "100%", "idle", `"ab12ab12"`, "Microsoft Windows 11 Enterprise", "fixture-host",
		"2026-10-22T14:03:09.512Z", "and/or", "a.b/c.d",
	}
	for _, s := range clean {
		if looksLikePath(s) {
			t.Errorf("looksLikePath(%q) = true, want false", s)
		}
	}
}

// The hardened rules of P1-FIX2: drive letters anywhere, POSIX paths after any
// non-word character, relative prefixes and file/smb/device URIs.
func TestLooksLikePathHardening(t *testing.T) {
	positive := []string{
		// (a) drive letter after a space, a colon, an equals sign, a quote, or at the start
		`x C:/foo`, `path:C:\foo`, `path:C:/foo`, `k=D:\x`, `"E:\x"`, `C:/`, `_C:\x`,
		// (b) POSIX path after any non-word character
		`path:/home/x/y`, `url=/etc/passwd`, `[/home/x/y]`, `{"/var/log/x"}`, `a|/etc/passwd/x`, `x-/opt/a/b`,
		`//server/share`, `x //server/share`,
		// (c) relative prefixes at a token start
		`~/`, `~/docs`, `./`, `./x`, `../`, `../x`, `see ../etc`, `(~/x)`, `a ./b`, `p:../x`,
		// (c) URI schemes and the device prefix
		`file:///x`, `file:x`, `FILE:x`, `x file:y`, `smb://srv/share`, `SMB:x`, `\\?\C:\x`, `\\?\`,
	}
	for _, s := range positive {
		if !looksLikePath(s) {
			t.Errorf("looksLikePath(%q) = false, want true", s)
		}
	}
	negative := []string{
		"windows/amd64", "linux/arm64", "12:30", "2026/10/22", "urn:cryobackup:error:x", "tag:value",
		"https://example.com/a/b", "http://host/a/b/c", "https://x.test", "profile:x", "a:b", "x:y/z",
		"~", ".", "..", "...", "~user", "x~/y", "v1.2./x", "a.b/c.d", "and/or", "1/2", "docx", "/etc",
		"10:30:15", "C:", "ab:/x", "wifi:x", "mailto:a@b.c",
	}
	for _, s := range negative {
		if looksLikePath(s) {
			t.Errorf("looksLikePath(%q) = true, want false", s)
		}
	}
}

// Documented limit of the net: a relative path with forward slashes and no
// leading '/' or "./" looks like "windows/amd64". If this ever starts failing,
// the guard got stricter: update the doc comment of AssertPathFree with it.
func TestLooksLikePathKnownMisses(t *testing.T) {
	for _, s := range []string{`Users/x/a.docx`, `a/b/c`, `a.docx`} {
		if looksLikePath(s) {
			t.Errorf("looksLikePath(%q) = true: the documented limit no longer holds", s)
		}
	}
}

func pathErr(t *testing.T, v any) *PathFreeError {
	t.Helper()
	err := AssertPathFree(v)
	var pe *PathFreeError
	if !errors.As(err, &pe) {
		t.Fatalf("AssertPathFree = %v, want a *PathFreeError", err)
	}
	return pe
}

func TestAssertPathFreeCleanBodies(t *testing.T) {
	// Every type the console ingests, filled, must pass: the guard cannot
	// be allowed to reject legitimate traffic. The snapshot roots are exempt.
	clean := []any{
		okEnrollRequest(), okLobbyRequest(), okHeartbeat(), okProgress(), okRun(), okRunsRequest(),
		okSnapshotsRequest(), okCommandResult(),
		// responses are not ingested but must also be clean of paths
		okRunsResponse(), okCommandsResponse(), okHeartbeatResponse(), okTokenRotate(), okError(),
		okStartLeaseRequest(), okStartLeaseGranted(),
		HoldParams{Until: t0}, RestoreParams{RestoreRequestID: ulidC},
	}
	for _, v := range clean {
		if err := AssertPathFree(v); err != nil {
			t.Errorf("%T: %v", v, err)
		}
	}
	if AssertPathFree(nil) != nil || AssertPathFree((*HeartbeatRequest)(nil)) != nil {
		t.Error("nil values must pass")
	}
	pointer := okHeartbeat()
	if err := AssertPathFree(&pointer); err != nil {
		t.Errorf("pointer to a clean body: %v", err)
	}
}

func TestAssertPathFreeFindsPaths(t *testing.T) {
	win := `C:\Users\x\Documents\a.docx`
	unc := `\\server\share\x`
	posix := `/home/x/y`
	env := `%APPDATA%\x`

	cases := []struct {
		name string
		v    any
		path string
	}{
		{"enroll hostname windows", func() any { r := okEnrollRequest(); r.Hostname = win; return r }(), "hostname"},
		{"enroll os version unc", func() any { r := okEnrollRequest(); r.OSVersion = unc; return r }(), "os_version"},
		{"lobby os posix", func() any { r := okLobbyRequest(); r.OS = posix; return r }(), "os"},
		{"lobby agent version env", func() any { r := okLobbyRequest(); r.AgentVersion = env; return r }(), "agent_version"},
		{"heartbeat etag posix", func() any { r := okHeartbeat(); r.PolicyETag = posix; return r }(), "policy_etag"},
		{"heartbeat enum-typed string", func() any { r := okHeartbeat(); r.LastErrorClass = restic.ErrorClass(env); return r }(), "last_error_class"},
		{"heartbeat nested progress", func() any { r := okHeartbeat(); r.Progress.RunID = win; return r }(), "progress.run_id"},
		{"progress run id", func() any { p := okProgress(); p.RunID = unc; return p }(), "run_id"},
		{"run agent version", func() any { r := okRun(); r.AgentVersion = win; return r }(), "agent_version"},
		{"run snapshot id", func() any { r := okRun(); r.SnapshotID = posix; return r }(), "snapshot_id"},
		{"run vss volume", func() any { r := okRun(); r.Stats.VSSFallbackVolumes = []string{`C:\`}; return r }(), "stats.vss_fallback_volumes[0]"},
		{"runs nested", func() any { r := okRunsRequest(); r.Runs[1].AgentVersion = unc; return r }(), "runs[1].agent_version"},
		{"error ext", func() any { r := okRunsRequest(); r.Runs[0].ErrorCounts[1].Ext = win; return r }(), "runs[0].error_counts[1].ext"},
		{"error during", func() any { r := okRunsRequest(); r.Runs[0].ErrorCounts[0].During = posix; return r }(), "runs[0].error_counts[0].during"},
		{"snapshot hostname", func() any { r := okSnapshotsRequest(); r.Snapshots[0].Hostname = unc; return r }(), "snapshots[0].hostname"},
		{"snapshot tag", func() any { r := okSnapshotsRequest(); r.Snapshots[0].Tags = []string{"ok", win}; return r }(), "snapshots[0].tags[1]"},
		{"snapshot username", func() any { r := okSnapshotsRequest(); r.Snapshots[0].Username = `HOST\user`; return r }(), "snapshots[0].username"},
		{"snapshot program version", func() any { r := okSnapshotsRequest(); r.Snapshots[0].ProgramVersion = posix; return r }(), "snapshots[0].program_version"},
		{"command params windows", func() any {
			c := okCommand()
			c.Params = json.RawMessage(`{"until":"2026-10-22T15:00:00Z","where":"C:\\Users\\x"}`)
			return c
		}(), "params.where"},
		{"command params nested posix", func() any {
			c := okCommand()
			c.Params = json.RawMessage(`{"a":{"b":["fine","/home/x/y"]}}`)
			return c
		}(), "params.a.b[1]"},
		{"commands response", func() any {
			r := okCommandsResponse()
			r.Commands[1].Params = json.RawMessage(`{"file":"\\\\server\\share\\x"}`)
			return r
		}(), "commands[1].params.file"},
		{"command result env", func() any {
			r := okCommandResult()
			r.Result = json.RawMessage(`{"dir":"%APPDATA%\\x"}`)
			return r
		}(), "result.dir"},
		{"command result key is a path", func() any {
			r := okCommandResult()
			r.Result = json.RawMessage(`{"C:\\Users\\x":1}`)
			return r
		}(), "result.*"},
		{"release manifest", func() any {
			r := okRelease()
			r.Manifest = json.RawMessage(`{"stage":"/opt/x/y"}`)
			return r
		}(), "manifest.stage"},
		{"map of any", map[string]any{"ok": "fine", "list": []any{"a", map[string]any{"deep": win}}}, "list[1].deep"},
		{"slice root", []string{"fine", posix}, "[1]"},
		{"bare string", win, ""},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			pe := pathErr(t, c.v)
			if pe.Path != c.path {
				t.Errorf("path = %q, want %q", pe.Path, c.path)
			}
			if pe.Reason != reasonPathLike {
				t.Errorf("reason = %q", pe.Reason)
			}
			msg := pe.Error()
			for _, leak := range []string{"Users", "home", "server", "APPDATA", "HOST", "opt"} {
				if strings.Contains(msg, leak) {
					t.Errorf("error text %q repeats the offending value (%s)", msg, leak)
				}
			}
		})
	}
}

func TestAssertPathFreeAllowedExtension(t *testing.T) {
	r := okRunsRequest()
	r.Runs[0].ErrorCounts[0].Ext = "docx"
	if err := AssertPathFree(r); err != nil {
		t.Errorf("ErrorProjection.Ext docx must pass: %v", err)
	}
	if err := AssertPathFree(restic.ErrorProjection{During: "archival", Class: restic.ClassPermission, Ext: "docx", Count: 1}); err != nil {
		t.Errorf("bare ErrorProjection: %v", err)
	}
}

// RestoreSelection is the one path-bearing type: the guard must not look inside
// it, whether it is passed directly, by pointer, in a slice, a map or a struct.
func TestAssertPathFreeSkipsRestoreSelection(t *testing.T) {
	sel := okRestoreSelection()
	if looksLikePath(sel.Target) == false {
		t.Fatal("the fixture must actually hold paths")
	}
	type wrapper struct {
		Sel  RestoreSelection  `json:"sel"`
		Ptr  *RestoreSelection `json:"ptr"`
		List []RestoreSelection
		Any  any
	}
	for name, v := range map[string]any{
		"value":   sel,
		"pointer": &sel,
		"slice":   []RestoreSelection{sel},
		"map":     map[string]RestoreSelection{"x": sel},
		"struct":  wrapper{Sel: sel, Ptr: &sel, List: []RestoreSelection{sel}, Any: sel},
		"any":     []any{"fine", sel},
	} {
		if err := AssertPathFree(v); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A sibling next to a selection is still checked.
	pe := pathErr(t, wrapper{Sel: sel, List: nil, Any: `C:\Users\x`})
	if pe.Path != "Any" {
		t.Errorf("sibling of a selection: path = %q, want Any", pe.Path)
	}
	// The marker is on the type, and only on that type.
	if _, ok := any(sel).(pathBearing); !ok {
		t.Error("RestoreSelection must implement pathBearing")
	}
	for _, v := range wireValues() {
		if _, ok := v.(pathBearing); ok {
			if _, isSel := v.(RestoreSelection); !isSel {
				t.Errorf("%T implements pathBearing; only RestoreSelection may", v)
			}
		}
	}
}

func TestAssertPathFreeSnapshotRootsExempt(t *testing.T) {
	r := okSnapshotsRequest()
	r.Snapshots[0].Paths = []string{`C:\Users`, `\\server\share`, `/home/x/y`, `%SYSTEMDRIVE%\`}
	if err := AssertPathFree(r); err != nil {
		t.Errorf("snapshot roots are exempt: %v", err)
	}
	r.Snapshots[0].Tags = []string{`D:\leak`}
	if pe := pathErr(t, r); pe.Path != "snapshots[0].tags[0]" {
		t.Errorf("other snapshot fields stay checked, path = %q", pe.Path)
	}
}

func TestAssertPathFreeFailsClosed(t *testing.T) {
	c := okCommand()
	c.Params = json.RawMessage(`{"a":`)
	if pe := pathErr(t, c); pe.Reason != reasonRawJSON || pe.Path != "params" {
		t.Errorf("invalid raw JSON: %+v", pe)
	}
	deep := json.RawMessage(strings.Repeat("[", 100) + strings.Repeat("]", 100))
	c.Params = deep
	if pe := pathErr(t, c); pe.Reason != reasonDepth {
		t.Errorf("deep nesting: %+v", pe)
	}
	// odd map key: shown as *, never echoed
	pe := pathErr(t, map[string]string{`C:\secret-patient\x`: "v"})
	if strings.Contains(pe.Error(), "patient") || pe.Path != "*" {
		t.Errorf("key leak: %v", pe)
	}
	// a clean key that is not an identifier is masked in the path too
	pe = pathErr(t, map[string]any{"two words": map[string]any{"k": `C:\x`}})
	if pe.Path != "*.k" {
		t.Errorf("path = %q, want *.k", pe.Path)
	}
}

func TestAssertPathFreeJSONNames(t *testing.T) {
	type inner struct {
		Kept    string `json:"kept"`
		Skipped string `json:"-"`
		Bare    string
		hidden  string
	}
	v := struct {
		In inner `json:"in"`
	}{In: inner{Skipped: `C:\x`, hidden: `C:\y`}}
	if err := AssertPathFree(v); err != nil {
		t.Errorf("json:\"-\" and unexported fields are not sent: %v", err)
	}
	v.In.Bare = `C:\x`
	if pe := pathErr(t, v); pe.Path != "in.Bare" {
		t.Errorf("untagged field uses its Go name, got %q", pe.Path)
	}
}
