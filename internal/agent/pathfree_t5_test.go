package agent

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/agent/config"
	"github.com/c2tech-sys/cryobackup/internal/agent/internal/fakerestic"
	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/logx"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// hostileTokens are the fragments of the hostile fixture
// (internal/restic/testdata/handwritten/hostile_paths.stderr.jsonl): the
// distinctive names, and the structure of every path shape in it. None of them
// may appear anywhere the device is not.
var hostileTokens = []string{
	"ZQ7", "fileserver01", "clinic$", "GLOBALROOT", "HarddiskVolumeShadowCopy", "Patient Charts", "Clinic Share",
	"Jane", "jane", "salaries", "sum\u00e9", "M\u00fcller", "private", "/mnt/d", `D:\`, `D:/`, `C:\Data`, `\\?\`,
}

// hostileHits returns the tokens found in text, whatever the case: a value
// that survives a validator's alphabet is still a leak.
func hostileHits(text string) []string {
	var out []string
	low := strings.ToLower(text)
	for _, tok := range hostileTokens {
		if strings.Contains(low, strings.ToLower(tok)) {
			out = append(out, tok)
		}
	}
	return out
}

// dbHostileHits is hostileHits for the raw database and WAL files. Those hold
// random bytes and base64 secrets, where a 3- or 4-letter token turns up by
// chance in a case-insensitive scan (CI once found "zq7" there with no other
// token present). So the short tokens are matched only with the separator the
// fixture always puts next to them; every other token is unchanged.
func dbHostileHits(text string) []string {
	var out []string
	for _, tok := range hostileHits(text) {
		switch tok {
		case "ZQ7":
			if hits := hostileHitsOf(text, "ZQ7-", `\ZQ7`, "/ZQ7", "ZQ7 "); len(hits) == 0 {
				continue
			}
		case "Jane", "jane":
			if hits := hostileHitsOf(text, "Jane ", "Jane'", `\jane`, "/jane"); len(hits) == 0 {
				continue
			}
		}
		out = append(out, tok)
	}
	return out
}

// hostileHitsOf is hostileHits over an explicit token list.
func hostileHitsOf(text string, toks ...string) []string {
	var out []string
	low := strings.ToLower(text)
	for _, tok := range toks {
		if strings.Contains(low, strings.ToLower(tok)) {
			out = append(out, tok)
		}
	}
	return out
}

// jsonStrings returns every string value (and object key) of a JSON document,
// except under the named keys.
func jsonStrings(t *testing.T, raw []byte, skipKeys ...string) []string {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	var out []string
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case string:
			out = append(out, x)
		case []any:
			for _, e := range x {
				walk(e)
			}
		case map[string]any:
			for k, e := range x {
				skip := false
				for _, s := range skipKeys {
					skip = skip || k == s
				}
				out = append(out, k)
				if !skip {
					walk(e)
				}
			}
		}
	}
	walk(v)
	return out
}

// TestT5HostilePathsNeverReachTheConsole: a backup whose stderr is full of
// file names in every shape (drive, UNC, shadow copy, quoted names, POSIX,
// unicode, plain text) is run by the real agent and uploaded to the real
// console. Nothing path-like may be in the stored run, in the console's
// database file, in the console's logs, in the agent's logs or in any request
// the agent made; the device's own run log must still hold the lines (DH15),
// which is also what proves the scan below is looking for something real.
func TestT5HostilePathsNeverReachTheConsole(t *testing.T) {
	c := newRealConsole(t)
	dir, devID := c.enrollAgent()
	fakerestic.Use(t, "hostile_paths")
	agentLog := &syncBuffer{}
	o := fakeOptions(t, dir)
	o.Logger = logx.New(agentLog, logx.LevelDebug)
	o.heartbeatEvery, o.heartbeatMinGap = 1200*time.Millisecond, time.Second
	r := start(t, o)

	var run struct{ id string }
	waitFor(t, "the closed run in the console's store", 40*time.Second, func() bool {
		for _, sr := range c.deviceRuns(devID) {
			if sr.Outcome != "" {
				run.id = sr.ID
				return true
			}
		}
		return false
	})
	jr := openJournalRO(t, dir)
	waitFor(t, "the journal to mark the run delivered", 30*time.Second, func() bool {
		row, ok, _ := jr.Run(t.Context(), run.id)
		return ok && row.DeliveredAt != nil
	})
	waitFor(t, "the snapshot list and a heartbeat after the run", 30*time.Second, func() bool {
		snap, hb := false, 0
		sawRun := false
		for _, call := range c.calls() {
			switch call.Path {
			case api.RouteRuns:
				sawRun = sawRun || call.Status == 200
			case api.RouteSnapshots:
				snap = true
			case api.RouteHeartbeat:
				if sawRun {
					hb++
				}
			}
		}
		return snap && hb >= 1
	})
	r.stop()

	// The run arrived, partial, as classes and counts.
	stored, err := c.runRow(run.id)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Outcome != string(restic.OutcomePartial) || len(stored.SnapshotID) != 64 || len(stored.ErrorCounts) == 0 {
		t.Fatalf("stored run = %+v", stored)
	}
	var stats struct {
		Failed int64 `json:"failed_files"`
	}
	_ = json.Unmarshal(stored.Stats, &stats)
	if stats.Failed < 3 {
		t.Errorf("failed_files = %d; the hostile stream names more files than that", stats.Failed)
	}

	// Control: the hostile text did reach the device, in the run log only.
	runLog := readFile(t, config.Layout{Root: dir}.RunLog(run.id))
	if hits := hostileHits(runLog); len(hits) < 8 {
		t.Fatalf("the device's run log holds only %v of the hostile tokens: the scan below would prove nothing", hits)
	}
	if all, db := hostileHits(runLog), dbHostileHits(runLog); len(db) != len(all) {
		t.Fatalf("the database scan sees %v of the run log's %v: its narrower short-token rule would miss a real leak", db, all)
	}

	// 1. The stored run row.
	row, _ := json.Marshal(stored)
	if hits := hostileHits(string(row)); len(hits) > 0 {
		t.Errorf("the stored run row holds %v: %s", hits, row)
	}
	// 2. Everything the console database holds, rows and write-ahead log.
	sawRunID := false
	for _, f := range []string{c.dbPath, c.dbPath + "-wal"} {
		b, err := os.ReadFile(f)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatalf("reading the console database: %v", err)
		}
		sawRunID = sawRunID || strings.Contains(string(b), run.id)
		if hits := dbHostileHits(string(b)); len(hits) > 0 {
			t.Errorf("the console database file holds %v", hits)
		}
	}
	if !sawRunID {
		t.Error("the run id is not in the console database files: the scan is not reading the stored rows")
	}
	// 3. The console's logs and the agent's logs.
	if hits := hostileHits(c.logs.String()); len(hits) > 0 {
		t.Errorf("the console log holds %v:\n%s", hits, c.logs.String())
	}
	if hits := hostileHits(agentLog.String()); len(hits) > 0 {
		t.Errorf("the agent log holds %v:\n%s", hits, agentLog.String())
	}
	// 4. Every request the agent made: the tokens, the path guard on the typed
	// body, and no backslash in any string (the roots of a snapshot list are
	// the one place a path is meant to travel).
	kinds := map[string]int{}
	for _, call := range c.calls() {
		if call.Method != "POST" {
			continue
		}
		kinds[call.Path]++
		if hits := hostileHits(string(call.Body)); len(hits) > 0 {
			t.Errorf("a %s request holds %v", call.Path, hits)
		}
		var typed interface{ Validate() error }
		switch call.Path {
		case api.RouteRuns:
			typed = &api.RunsRequest{}
		case api.RouteHeartbeat:
			typed = &api.HeartbeatRequest{}
		case api.RouteSnapshots:
			typed = &api.SnapshotsRequest{}
		}
		if typed != nil {
			if err := json.Unmarshal(call.Body, typed); err != nil {
				t.Fatalf("%s body: %v", call.Path, err)
			}
			if err := api.AssertPathFree(typed); err != nil {
				t.Errorf("%s request is not path-free: %v", call.Path, err)
			}
		}
		for _, s := range jsonStrings(t, call.Body, "paths") {
			if strings.ContainsRune(s, '\\') {
				t.Errorf("a %s request carries a string with a backslash", call.Path)
			}
		}
	}
	for _, p := range []string{api.RouteRuns, api.RouteHeartbeat, api.RouteSnapshots} {
		if kinds[p] == 0 {
			t.Errorf("the agent made no %s request: the wire check saw too little", p)
		}
	}
}
