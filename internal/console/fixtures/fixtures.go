// Package fixtures is the console half of fixtures mode (architecture §3.5 item
// 6, §10.2 M0): the canned console conditions C01 to C16 for the /design page,
// the frozen clock, and an in-process adminpath.Backend that serves a fictional
// snapshot tree so the restore wizard works with no maintenance worker, no
// restic and no network. The fictional fleet itself lives in internal/console/store
// (LoadFixtures); this package adds only what is not data.
//
// The same fake Backend is what the agent API and UI tests run against, so the
// tests and `serve --fixtures` exercise one implementation.
package fixtures

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/restic"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// Now returns the frozen instant of fixtures mode: Tuesday 20 October 2026,
// 1:42:10 PM in America/Chicago, the "polled" time of the approved design.
func Now() time.Time { return time.Date(2026, 10, 20, 18, 42, 10, 0, time.UTC) }

// Clock returns a clock frozen at Now.
func Clock() func() time.Time { n := Now(); return func() time.Time { return n } }

// Banners returns the full C01 to C16 console condition set with the copy of
// components.md §9.2, timestamps relative to now. It is what /design renders and
// what the banner golden test checks.
func Banners(now time.Time) []state.Banner {
	hm := func(d time.Duration) string { return now.Add(-d).Format("15:04") }
	crit, warn := state.SeverityCritical, state.SeverityAlarm
	return []state.Banner{
		{ID: "C01", Severity: crit, Text: "Gateway unreachable: no agent can back up. Last OK " + hm(9*time.Minute) + "."},
		{ID: "C02", Severity: crit, Text: "Storage unreachable from the admin path: retention, restore and usage are impaired. Last OK " + hm(14*time.Minute) + "."},
		{ID: "C03", Severity: crit, Text: "Alert e-mail cannot be delivered: alarms may go unnoticed. Last send " + hm(26*time.Hour) + "."},
		// C04 and C05 follow ADR 0013 (the agent endpoint self-probe); /design shows
		// every ADR 0013 variant from health.EndpointExamples.
		{ID: "C04", Severity: crit, Count: 23, Text: "Agent API unreachable: agents.example.com:8443 does not resolve (DNS). No agent can check in or report. Failing for 23 min."},
		{ID: "C05", Severity: warn, Count: 12, Text: "Agent listener certificate expires in 12 days."},
		{ID: "C06", Severity: crit, Text: "Alerting is stalled: this view and notifications may be stale. Last evaluation " + hm(7*time.Minute) + "."},
		{ID: "C07", Severity: crit, Text: "Maintenance worker is not running: retention, restore browsing and usage sampling are off."},
		{ID: "C08", Severity: crit, Text: "No active devices enrolled. Nothing is being backed up."},
		{ID: "C09", Severity: crit, Count: 5, Text: "Partial data: 5 devices could not be classified. They show as UNKNOWN and count as unhealthy."},
		{ID: "C10", Severity: warn, Text: "Live updates disconnected: data as of " + hm(95*time.Second) + "."},
		{ID: "C11", Severity: warn, Text: "Console database needs attention: self-backup older than 26 h."},
		{ID: "C12", Severity: crit, Text: "Master key unavailable: enrollment, escrow reveal and server-side retention are disabled."},
		{ID: "C13", Severity: warn, Text: "Server clock may be wrong: device offsets agree."},
		{ID: "C14", Severity: crit, Text: "Daily digest was not delivered. It was due by 07:30."},
		{ID: "C15", Severity: warn, Count: 84, Text: "Storage at 84% of quota."},
		{ID: "C16", Severity: warn, Text: "Release manifest rejected."},
	}
}

// File is one fictional file of the fake snapshot.
type File struct {
	Path    string // absolute snapshot path, "/" separators, for example /C/Users/Public/Report.txt
	Content []byte
	Mtime   time.Time
}

// DefaultFiles is the fictional tree fixtures mode browses. The names are
// invented; none resembles a real client's.
func DefaultFiles() []File {
	m := Now().Add(-26 * time.Hour)
	return []File{
		{Path: "/C/Users/Public/Documents/Quarterly-Report.txt", Content: []byte("Quarterly report (fixture)\n"), Mtime: m},
		{Path: "/C/Users/Public/Documents/Budget-2026.csv", Content: []byte("item,amount\nsupplies,120\n"), Mtime: m},
		{Path: "/C/Users/Public/Desktop/Welcome.txt", Content: []byte("Welcome (fixture)\n"), Mtime: m},
		{Path: "/C/Users/Public/Pictures/Logo.png", Content: []byte("not a real png"), Mtime: m},
		{Path: "/C/ProgramData/Sample/settings.ini", Content: []byte("[sample]\nkey=value\n"), Mtime: m},
	}
}

// Call records one Backend invocation (for assertions). Request texts that can
// hold a path are kept so tests can check them, and never logged.
type Call struct {
	Method   string
	DeviceID string
	Path     string
}

// Backend is an in-process adminpath.Backend with canned answers.
type Backend struct {
	// FailDumpAfter, when greater than zero, makes Dump fail with
	// storage_unreachable after it has streamed that many bytes (a run that
	// breaks mid-stream).
	FailDumpAfter int
	// Files is the fake snapshot content (DefaultFiles when nil).
	Files []File
	// Snapshot is the id every device's fake snapshot uses (a fixed 64-hex id when empty).
	SnapshotID string
	// Errors maps a method name (adminpath.Method*) to the error that method returns.
	Errors map[string]error
	// InitResult, when set, is what Init returns (with a nil error): the result-style
	// failure of an existing repository whose key does not open it.
	InitResult *adminpath.InitResult
	// PingStatus overrides Ping's result (a healthy worker when nil).
	PingStatus *adminpath.PingResult

	mu    sync.Mutex
	calls []Call
	inits map[string]bool
}

var _ adminpath.Backend = (*Backend)(nil)

// FixtureSnapshotID is the default snapshot id of the fake repository.
const FixtureSnapshotID = "f1f1f1f1a2a2a2a2b3b3b3b3c4c4c4c4d5d5d5d5e6e6e6e6f7f7f7f7a8a8a8a8"

func (b *Backend) files() []File {
	if b.Files != nil {
		return b.Files
	}
	return DefaultFiles()
}

func (b *Backend) snap() string {
	if b.SnapshotID != "" {
		return b.SnapshotID
	}
	return FixtureSnapshotID
}

func (b *Backend) record(c Call) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, c)
	return b.Errors[c.Method]
}

// Calls returns a copy of the recorded calls.
func (b *Backend) Calls() []Call {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]Call(nil), b.calls...)
}

// Count returns how many times method was called.
func (b *Backend) Count(method string) int {
	n := 0
	for _, c := range b.Calls() {
		if c.Method == method {
			n++
		}
	}
	return n
}

// Ping implements adminpath.Backend.
func (b *Backend) Ping(ctx context.Context) (adminpath.PingResult, error) {
	if err := b.record(Call{Method: adminpath.MethodPing}); err != nil {
		return adminpath.PingResult{}, err
	}
	if b.PingStatus != nil {
		return *b.PingStatus, nil
	}
	yes := true
	return adminpath.PingResult{Version: "fixtures", Uptime: time.Hour, MasterKeyLoaded: true, StorageReachable: &yes}, nil
}

// Init implements adminpath.Backend: idempotent, Created only the first time.
func (b *Backend) Init(ctx context.Context, req adminpath.InitRequest) (adminpath.InitResult, error) {
	if err := b.record(Call{Method: adminpath.MethodInit, DeviceID: req.DeviceID}); err != nil {
		return adminpath.InitResult{}, err
	}
	if b.InitResult != nil {
		return *b.InitResult, nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.inits == nil {
		b.inits = map[string]bool{}
	}
	created := !b.inits[req.DeviceID]
	b.inits[req.DeviceID] = true
	sum := sha256.Sum256([]byte("repo/" + req.DeviceID))
	return adminpath.InitResult{ResultMeta: adminpath.ResultMeta{RequestID: req.RequestID, Outcome: restic.OutcomeSuccess},
		RepositoryID: hex.EncodeToString(sum[:]), Created: created}, nil
}

// CatConfig implements adminpath.Backend. It fails with not_found for a device
// that was never initialised through this Backend.
func (b *Backend) CatConfig(ctx context.Context, req adminpath.DeviceRequest) (restic.Config, error) {
	if err := b.record(Call{Method: adminpath.MethodCatConfig, DeviceID: req.DeviceID}); err != nil {
		return restic.Config{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.inits) > 0 && !b.inits[req.DeviceID] {
		return restic.Config{}, adminpath.NewError(adminpath.CodeNotFound, "")
	}
	sum := sha256.Sum256([]byte("repo/" + req.DeviceID))
	return restic.Config{Version: 2, ID: hex.EncodeToString(sum[:]), ChunkerPolynomial: "3da3358b4dc173"}, nil
}

func clean(p string) string {
	if p == "" || p == "/" {
		return "/"
	}
	return path.Clean("/" + strings.TrimPrefix(p, "/"))
}

// Ls implements adminpath.Backend: one directory level of the fake tree.
func (b *Backend) Ls(ctx context.Context, req adminpath.LsRequest) (adminpath.LsResult, error) {
	if err := b.record(Call{Method: adminpath.MethodLs, DeviceID: req.DeviceID, Path: req.Dir}); err != nil {
		return adminpath.LsResult{}, err
	}
	dir := clean(req.Dir)
	seen := map[string]restic.Node{}
	for _, f := range b.files() {
		rest, ok := childOf(dir, f.Path)
		if !ok {
			continue
		}
		name, tail, nested := strings.Cut(rest, "/")
		full := path.Join(dir, name)
		if nested && tail != "" {
			seen[name] = restic.Node{Name: name, Type: "dir", Path: full, Mtime: f.Mtime}
		} else {
			seen[name] = restic.Node{Name: name, Type: "file", Path: full, Size: uint64(len(f.Content)), Mtime: f.Mtime}
		}
	}
	nodes := make([]restic.Node, 0, len(seen))
	for _, n := range seen {
		nodes = append(nodes, n)
	}
	sort.Slice(nodes, func(i, j int) bool {
		if (nodes[i].Type == "dir") != (nodes[j].Type == "dir") {
			return nodes[i].Type == "dir"
		}
		return nodes[i].Name < nodes[j].Name
	})
	if limit := req.Limit; limit > 0 && len(nodes) > limit {
		return adminpath.LsResult{Snapshot: restic.LsSnapshot{ID: req.SnapshotID}, Nodes: nodes[:limit], Truncated: true}, nil
	}
	return adminpath.LsResult{ResultMeta: adminpath.ResultMeta{Outcome: restic.OutcomeSuccess}, Snapshot: restic.LsSnapshot{ID: req.SnapshotID}, Nodes: nodes}, nil
}

func childOf(dir, file string) (string, bool) {
	if dir == "/" {
		return strings.TrimPrefix(file, "/"), true
	}
	prefix := dir + "/"
	if strings.HasPrefix(file, prefix) {
		return strings.TrimPrefix(file, prefix), true
	}
	return "", false
}

// Find implements adminpath.Backend (no matches).
func (b *Backend) Find(ctx context.Context, req adminpath.FindRequest) ([]restic.FindResult, error) {
	if err := b.record(Call{Method: adminpath.MethodFind, DeviceID: req.DeviceID}); err != nil {
		return nil, err
	}
	return []restic.FindResult{}, nil
}

// Snapshots implements adminpath.Backend: one snapshot of the fake tree.
func (b *Backend) Snapshots(ctx context.Context, req adminpath.DeviceRequest) ([]restic.Snapshot, error) {
	if err := b.record(Call{Method: adminpath.MethodSnapshots, DeviceID: req.DeviceID}); err != nil {
		return nil, err
	}
	return []restic.Snapshot{{ID: b.snap(), ShortID: b.snap()[:8], Time: Now().Add(-26 * time.Hour), Paths: []string{"C:\\Users"}, Hostname: "fixture"}}, nil
}

// Dump implements adminpath.Backend like the maintain worker does (ADR 0008):
// it streams a real zip into w and writes nothing to disk. A file is a
// one-entry archive named after its base name; a directory holds every fake
// file below it, by its path relative to the directory.
func (b *Backend) Dump(ctx context.Context, req adminpath.DumpRequest, w io.Writer) (adminpath.DumpResult, error) {
	if err := b.record(Call{Method: adminpath.MethodDump, DeviceID: req.DeviceID, Path: req.Path}); err != nil {
		return adminpath.DumpResult{}, err
	}
	want := clean(req.Path)
	type entry struct {
		name    string
		content []byte
	}
	var entries []entry
	for _, f := range b.files() {
		fp := clean(f.Path)
		switch {
		case fp == want:
			entries = append(entries, entry{path.Base(fp), f.Content})
		case want == "/" || strings.HasPrefix(fp, want+"/"):
			entries = append(entries, entry{strings.TrimPrefix(strings.TrimPrefix(fp, want), "/"), f.Content})
		}
	}
	if len(entries) == 0 {
		return adminpath.DumpResult{}, adminpath.NewError(adminpath.CodeNotFound, "")
	}
	out := io.Writer(w)
	if b.FailDumpAfter > 0 {
		out = &failAfter{w: w, left: b.FailDumpAfter}
	}
	zw := zip.NewWriter(out)
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return adminpath.DumpResult{}, err
		}
		fw, err := zw.Create(e.name)
		if err == nil {
			_, err = fw.Write(e.content)
		}
		if err == nil {
			err = zw.Flush()
		}
		if err != nil {
			return adminpath.DumpResult{}, b.dumpErr(out)
		}
	}
	if err := zw.Close(); err != nil {
		return adminpath.DumpResult{}, b.dumpErr(out)
	}
	return adminpath.DumpResult{ResultMeta: adminpath.ResultMeta{RequestID: req.RequestID, Outcome: restic.OutcomeSuccess}}, nil
}

// dumpErr is the error of a Dump whose writer failed: the scripted mid-stream
// failure, or the destination going away.
func (b *Backend) dumpErr(out io.Writer) error {
	if f, ok := out.(*failAfter); ok && f.tripped {
		return adminpath.NewError(adminpath.CodeStorageUnreachable, restic.ClassNetwork)
	}
	return adminpath.NewError(adminpath.CodeUnavailable, "")
}

// failAfter passes left bytes, then fails every write.
type failAfter struct {
	w       io.Writer
	left    int
	tripped bool
}

func (f *failAfter) Write(p []byte) (int, error) {
	if len(p) > f.left {
		n, _ := f.w.Write(p[:f.left])
		f.left = 0
		f.tripped = true
		return n, errFixtureCut
	}
	f.left -= len(p)
	return f.w.Write(p)
}

var errFixtureCut = errors.New("fixtures: scripted mid-stream failure")

// Stats implements adminpath.Backend.
func (b *Backend) Stats(ctx context.Context, req adminpath.StatsRequest) (restic.StatsResult, error) {
	if err := b.record(Call{Method: adminpath.MethodStats, DeviceID: req.DeviceID}); err != nil {
		return restic.StatsResult{}, err
	}
	return restic.StatsResult{TotalSize: 38 << 30, SnapshotsCount: 61}, nil
}

func (b *Backend) unavailable(method, device string) error {
	if err := b.record(Call{Method: method, DeviceID: device}); err != nil {
		return err
	}
	return adminpath.NewError(adminpath.CodeUnavailable, "")
}

// Unlock implements adminpath.Backend (unavailable in fixtures).
func (b *Backend) Unlock(ctx context.Context, req adminpath.UnlockRequest) (adminpath.UnlockResult, error) {
	return adminpath.UnlockResult{}, b.unavailable(adminpath.MethodUnlock, req.DeviceID)
}

// Forget implements adminpath.Backend (unavailable in fixtures).
func (b *Backend) Forget(ctx context.Context, req adminpath.ForgetRequest) (adminpath.ForgetResult, error) {
	return adminpath.ForgetResult{}, b.unavailable(adminpath.MethodForget, req.DeviceID)
}

// Prune implements adminpath.Backend (unavailable in fixtures).
func (b *Backend) Prune(ctx context.Context, req adminpath.PruneRequest) (adminpath.PruneResult, error) {
	return adminpath.PruneResult{}, b.unavailable(adminpath.MethodPrune, req.DeviceID)
}

// Check implements adminpath.Backend (unavailable in fixtures).
func (b *Backend) Check(ctx context.Context, req adminpath.CheckRequest) (adminpath.CheckResult, error) {
	return adminpath.CheckResult{}, b.unavailable(adminpath.MethodCheck, req.DeviceID)
}

// String describes the Backend without any content.
func (b *Backend) String() string { return fmt.Sprintf("fixtures.Backend(%d calls)", len(b.Calls())) }
