package adminapi

import (
	"encoding/json"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/httpx"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// atoiRange parses v as an integer in [lo, hi].
func atoiRange(v string, lo, hi int) (int, bool) {
	n, err := strconv.Atoi(v)
	return n, err == nil && n >= lo && n <= hi
}

// RunRowJSON is a run with the customer and device it belongs to. It holds
// counts and error classes, never a path.
type RunRowJSON struct {
	RunJSON
	CustomerID string `json:"customer_id"`
	Customer   string `json:"customer"`
	DeviceID   string `json:"device_id"`
	Hostname   string `json:"hostname"`
	// DurationMS is absent while the run is open.
	DurationMS *int64 `json:"duration_ms,omitempty"`
}

func runRowJSON(r store.RunRow) RunRowJSON {
	out := RunRowJSON{RunJSON: runJSON(r.Run), CustomerID: r.CustomerID, Customer: r.CustomerName, DeviceID: r.DeviceID, Hostname: r.Hostname}
	if r.FinishedAt != nil {
		d := r.FinishedAt.Sub(r.StartedAt).Milliseconds()
		out.DurationMS = &d
	}
	return out
}

// RunListResponse is GET /api/v1/runs, newest start first. Total counts the runs
// that match, Of every run in the caller's scope, so "0 of N" is expressible.
type RunListResponse struct {
	ComputedAt time.Time    `json:"computed_at"`
	Total      int          `json:"total"`
	Of         int          `json:"of"`
	Truncated  bool         `json:"truncated"`
	NextCursor string       `json:"next_cursor,omitempty"`
	Runs       []RunRowJSON `json:"runs"`
}

func (s *Service) searchRuns(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	q := r.URL.Query()
	offset, limit, ok := pageParams(q, 500)
	var since, until time.Time
	for _, f := range []struct {
		key string
		dst *time.Time
	}{{"since", &since}, {"until", &until}} {
		if v := q.Get(f.key); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				ok = false
			}
			*f.dst = t.UTC()
		}
	}
	if !ok {
		httpx.Problem(w, r, api.CodeInvalidRequest, 0)
		return
	}
	page, err := s.Store.SearchRunsAt(r.Context(), p.Scope, store.RunSearch{CustomerIDs: splitList(q["customer"]), DeviceIDs: splitList(q["device"]),
		Kinds: splitList(q["kind"]), Outcomes: splitList(q["status"]), Since: since, Until: until}, offset, limit)
	if err != nil {
		s.failM2(w, r, err)
		return
	}
	resp := RunListResponse{ComputedAt: s.now(), Total: page.Page.Total, Of: page.Of, Runs: make([]RunRowJSON, 0, len(page.Items))}
	resp.NextCursor, resp.Truncated = nextCursor(page.Page)
	for _, row := range page.Items {
		resp.Runs = append(resp.Runs, runRowJSON(row))
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}

// ErrorClassJSON is one error class of a run and how many files it affected.
type ErrorClassJSON struct {
	Class string `json:"class"`
	Count int    `json:"count"`
}

// RunDetailResponse is GET /api/v1/runs/{id}. Stats holds the run's numeric
// counters only (anything else the agent sent is dropped here); the error
// classes are the only account of what went wrong, so no path or message ever
// reaches this body.
type RunDetailResponse struct {
	ComputedAt time.Time `json:"computed_at"`
	RunRowJSON
	ReceivedAt     time.Time          `json:"received_at"`
	ClockOffsetMS  int64              `json:"clock_offset_ms"`
	SnapshotID     string             `json:"snapshot_id,omitempty"`
	DataAddedBytes *int64             `json:"data_added_bytes,omitempty"`
	PolicyVersion  *int               `json:"policy_version,omitempty"`
	AgentVersion   string             `json:"agent_version,omitempty"`
	Stats          map[string]float64 `json:"stats"`
	ErrorClasses   []ErrorClassJSON   `json:"error_classes"`
}

var statKey = regexp.MustCompile(`^[a-z0-9_]{1,48}$`)

// numericStats keeps the numeric counters of a run's stats. A value that is not
// a plain number (text, a list, an object) is dropped: stats is the one run
// field the agent fills with its own keys, and only numbers are known to be
// path-free.
func numericStats(raw json.RawMessage) map[string]float64 {
	out := map[string]float64{}
	var in map[string]any
	if json.Unmarshal(raw, &in) != nil {
		return out
	}
	for k, v := range in {
		if n, ok := v.(float64); ok && statKey.MatchString(k) {
			out[k] = n
		}
	}
	return out
}

func (s *Service) getRunDetail(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	row, err := s.Store.GetRunRow(r.Context(), p.Scope, r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := RunDetailResponse{ComputedAt: s.now(), RunRowJSON: runRowJSON(row), ReceivedAt: row.ReceivedAt, ClockOffsetMS: row.ClockOffsetMS, SnapshotID: row.SnapshotID,
		DataAddedBytes: row.DataAddedBytes, PolicyVersion: row.PolicyVersion, AgentVersion: row.AgentVersion, Stats: numericStats(row.Stats), ErrorClasses: []ErrorClassJSON{}}
	for class, n := range row.ErrorCounts {
		out.ErrorClasses = append(out.ErrorClasses, ErrorClassJSON{Class: class, Count: n})
	}
	sort.Slice(out.ErrorClasses, func(i, j int) bool {
		a, b := out.ErrorClasses[i], out.ErrorClasses[j]
		if a.Count != b.Count {
			return a.Count > b.Count
		}
		return a.Class < b.Class
	})
	httpx.WriteJSON(w, http.StatusOK, out)
}
