package adminpath

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"reflect"
	"regexp"
	"sort"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/restic"
)

func withID(id string) DeviceRequest { return DeviceRequest{DeviceID: "dev1", RequestID: id} }

// TestRoundTripEveryMethod sends every method through a real unix socket to the
// fake backend and checks that request fields survive, that RequestID echoes
// and that Elapsed, ExitCode, Outcome, DeferredReason and ErrorClass round-trip.
func TestRoundTripEveryMethod(t *testing.T) {
	fb := newFake()
	c, _ := startServer(t, fb, ClientOptions{})
	ctx := context.Background()
	check := func(t *testing.T, method string, got, want any) {
		t.Helper()
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s result mismatch:\n got  %+v\n want %+v", method, got, want)
		}
	}
	recorded := func(t *testing.T, method string, want any) {
		t.Helper()
		if got := fb.last(method); !reflect.DeepEqual(got, want) {
			t.Errorf("%s request seen by the backend:\n got  %+v\n want %+v", method, got, want)
		}
	}

	t.Run(MethodPing, func(t *testing.T) {
		got, err := c.Ping(ctx)
		if err != nil {
			t.Fatal(err)
		}
		check(t, MethodPing, got, fixPing())
	})
	t.Run(MethodInit, func(t *testing.T) {
		req := InitRequest{withID("r-init")}
		got, err := c.Init(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		want := InitResult{ResultMeta: fixMeta(), RepositoryID: got.RepositoryID, Created: true}
		want.RequestID = "r-init"
		check(t, MethodInit, got, want)
		if got.RepositoryID == "" {
			t.Error("repository id lost")
		}
		recorded(t, MethodInit, req)
	})
	t.Run(MethodCatConfig, func(t *testing.T) {
		req := withID("r-cfg")
		got, err := c.CatConfig(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		check(t, MethodCatConfig, got, restic.Config{Version: 2, ID: got.ID, ChunkerPolynomial: "3da3358b4dc173"})
		recorded(t, MethodCatConfig, req)
	})
	t.Run(MethodLs, func(t *testing.T) {
		req := LsRequest{DeviceRequest: withID("r-ls"), SnapshotID: "latest", Dir: "/C/Users", Limit: 10}
		got, err := c.Ls(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		want := fixLs()
		want.RequestID = "r-ls"
		check(t, MethodLs, got, want)
		recorded(t, MethodLs, req)
	})
	t.Run(MethodFind, func(t *testing.T) {
		req := FindRequest{DeviceRequest: withID("r-find"), Pattern: "*.docx", IgnoreCase: true,
			SnapshotIDs: []string{"latest", "abababab"}, Limit: 50}
		got, err := c.Find(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		check(t, MethodFind, got, fixFind())
		recorded(t, MethodFind, req)
	})
	t.Run(MethodSnapshots, func(t *testing.T) {
		req := withID("r-snap")
		got, err := c.Snapshots(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		check(t, MethodSnapshots, got, []restic.Snapshot{fixSnapshot()})
		recorded(t, MethodSnapshots, req)
	})
	t.Run(MethodDump, func(t *testing.T) {
		req := DumpRequest{DeviceRequest: withID("r-dump"), SnapshotID: "latest:sub", Path: "/C/Users/a"}
		var buf bytes.Buffer
		got, err := c.Dump(ctx, req, &buf)
		if err != nil {
			t.Fatal(err)
		}
		// Bytes and SHA256 are what crossed the socket, not what the backend claimed.
		sum := sha256.Sum256(dumpPayload)
		want := DumpResult{ResultMeta: fixMeta(), Bytes: int64(len(dumpPayload)), SHA256: hex.EncodeToString(sum[:])}
		want.RequestID = "r-dump"
		check(t, MethodDump, got, want)
		if !bytes.Equal(buf.Bytes(), dumpPayload) {
			t.Errorf("streamed %d bytes, want the %d of the payload", buf.Len(), len(dumpPayload))
		}
		recorded(t, MethodDump, req)
	})
	t.Run(MethodStats, func(t *testing.T) {
		req := StatsRequest{DeviceRequest: withID("r-stats"), Mode: restic.StatsRawData}
		got, err := c.Stats(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		check(t, MethodStats, got, restic.StatsResult{TotalSize: 1 << 30, TotalFileCount: 99, TotalBlobCount: 1234, SnapshotsCount: 7, CompressionRatio: 1.5})
		recorded(t, MethodStats, req)
	})
	t.Run(MethodUnlock, func(t *testing.T) {
		req := UnlockRequest{DeviceRequest: withID("r-unlock"), RemoveAll: true, Quiesced: true}
		got, err := c.Unlock(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		want := UnlockResult{ResultMeta{RequestID: "r-unlock", Elapsed: time.Second, Outcome: restic.OutcomeSuccess}}
		check(t, MethodUnlock, got, want)
		recorded(t, MethodUnlock, req)
	})
	t.Run(MethodForget, func(t *testing.T) {
		req := ForgetRequest{DeviceRequest: withID("r-forget"), Rules: KeepRules{KeepDaily: 7, KeepYearly: -1},
			KeepWithin: "30d", KeepTags: []string{"legal"}, DryRun: true}
		got, err := c.Forget(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		want := fixForget()
		want.RequestID = "r-forget"
		check(t, MethodForget, got, want)
		if got.Outcome != restic.OutcomePartial || got.ExitCode != 3 || got.Elapsed != 1500e6 {
			t.Errorf("meta lost: %+v", got.ResultMeta)
		}
		recorded(t, MethodForget, req)
	})
	t.Run(MethodPrune, func(t *testing.T) {
		req := PruneRequest{DeviceRequest: withID("r-prune"), MaxUnused: "5%", RepackCacheableOnly: true, MaxRepackSize: "500M", DryRun: true}
		got, err := c.Prune(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		if got.Outcome != restic.OutcomeDeferred || got.DeferredReason != restic.DeferredLock || got.ExitCode != 11 ||
			got.ErrorClass != restic.ClassLock || got.RequestID != "r-prune" || got.Stats.RemoveBlobs != 7 || got.Stats.UnusedPercentAfter != 4.5 {
			t.Errorf("prune result: %+v", got)
		}
		recorded(t, MethodPrune, req)
	})
	t.Run(MethodCheck, func(t *testing.T) {
		req := CheckRequest{DeviceRequest: withID("r-check"), ReadDataSubset: "1/10", WithCache: true}
		got, err := c.Check(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		if got.Outcome != restic.OutcomeFailed || got.ExitCode != 1 || got.RequestID != "r-check" ||
			got.Summary != (restic.CheckSummary{NumErrors: 2, BrokenPacks: 1, SuggestRepairIndex: true, SuggestPrune: true}) {
			t.Errorf("check result: %+v", got)
		}
		recorded(t, MethodCheck, req)
	})

	var seen []string
	for m := range fb.calls {
		seen = append(seen, m)
	}
	sort.Strings(seen)
	want := Methods()
	sort.Strings(want)
	if !reflect.DeepEqual(seen, want) {
		t.Errorf("methods exercised %v, methods declared %v", seen, want)
	}
}

// TestClientGeneratesRequestID: an empty RequestID is filled with random hex,
// the backend sees it, the response echoes it and the caller's struct is not
// modified.
func TestClientGeneratesRequestID(t *testing.T) {
	fb := newFake()
	c, _ := startServer(t, fb, ClientOptions{})
	req := LsRequest{DeviceRequest: DeviceRequest{DeviceID: "dev1"}, SnapshotID: "latest", Dir: "/"}
	got, err := c.Ls(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	seen := fb.last(MethodLs).(LsRequest)
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(seen.RequestID) {
		t.Errorf("backend saw request id %q", seen.RequestID)
	}
	if got.RequestID != seen.RequestID {
		t.Errorf("response echoed %q, backend saw %q", got.RequestID, seen.RequestID)
	}
	if req.RequestID != "" {
		t.Error("the caller's request was modified")
	}
}

// TestHandlerBackstops: the handler cuts an over-long Ls to the request's limit
// and never serialises a nil list as null.
func TestHandlerBackstops(t *testing.T) {
	fb := newFake()
	big := &lsBackend{fakeBackend: fb, n: 12}
	c, _ := startServer(t, big, ClientOptions{})
	got, err := c.Ls(context.Background(), LsRequest{DeviceRequest: withID("x"), SnapshotID: "latest", Dir: "/", Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Nodes) != 5 || !got.Truncated {
		t.Errorf("nodes=%d truncated=%v, want 5 true", len(got.Nodes), got.Truncated)
	}
	got, err = c.Ls(context.Background(), LsRequest{DeviceRequest: withID("x"), SnapshotID: "latest", Dir: "/", Limit: 20})
	if err != nil || len(got.Nodes) != 12 {
		t.Errorf("limit above the count must not cut: n=%d err=%v", len(got.Nodes), err)
	}
}

type lsBackend struct {
	*fakeBackend
	n int
}

func (l *lsBackend) Ls(ctx context.Context, req LsRequest) (LsResult, error) {
	res, err := l.fakeBackend.Ls(ctx, req)
	res.Nodes = make([]restic.Node, l.n)
	res.Truncated = false
	return res, err
}

func (l *lsBackend) Find(ctx context.Context, req FindRequest) ([]restic.FindResult, error) {
	return nil, nil
}

// TestEmptyListsAreArraysNotNull: a backend that returns nil for Find or
// Snapshots yields [] on the wire.
func TestEmptyListsAreArraysNotNull(t *testing.T) {
	fb := newFake()
	b := &lsBackend{fakeBackend: fb}
	c, path := startServer(t, b, ClientOptions{})
	status, body, _ := rawDo(t, path, "POST", "/v1/find", "application/json", []byte(`{"device_id":"dev1","pattern":"x"}`))
	if status != 200 || string(body) != "[]" {
		t.Errorf("find: status %d body %q, want 200 []", status, body)
	}
	res, err := c.Find(context.Background(), FindRequest{DeviceRequest: withID("x"), Pattern: "x"})
	if err != nil || res == nil || len(res) != 0 {
		t.Errorf("client find: %v %v", res, err)
	}
}

func TestErrorIs(t *testing.T) {
	err := error(NewError(CodeLocked, restic.ClassLock))
	if !errors.Is(err, &Error{Code: CodeLocked}) {
		t.Error("errors.Is must match by code")
	}
	if errors.Is(err, &Error{Code: CodeTimeout}) {
		t.Error("errors.Is must not match another code")
	}
}
