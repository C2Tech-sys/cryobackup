package ui

import (
	"strings"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/console/adminapi"
)

// M3-T4 fix round 4.

// r4Row is the S36 HTML of one row.
func r4Row(t *testing.T, body, id string) string {
	t.Helper()
	i := strings.Index(body, `id="`+id+`"`)
	if i < 0 {
		t.Fatalf("no row %s", id)
	}
	return body[i : i+strings.Index(body[i:], "</tr>")]
}

// MEDIUM-1 on S36: a row of a job this process does not run reads as what it is. The maintenance
// scheduler is unregistered here too, but it is alive in another process: its real result stays.
func TestR4AnUnregisteredJobRowReadsNoLongerScheduled(t *testing.T) { // r4 M1
	e, _ := newM3Env(t)
	e.login()
	last := e.now.Add(-48 * 3600 * 1e9)
	e.canned(map[string]any{
		"/api/v1/system/health": adminapi.SystemHealthResponse{StampJSON: adminapi.StampJSON{ComputedAt: e.now, LoadedAt: e.now, Complete: true}, Overall: "ok",
			Components: []adminapi.SystemComponentJSON{{ID: "jobs", Label: "Background jobs", Status: "ok", Banners: []string{}}}},
		"/api/v1/system/jobs": adminapi.JobListResponse{ComputedAt: e.now, Jobs: []adminapi.JobJSON{
			{Name: "healthcheck-ping", Registered: false, Status: "error", LastErrorClass: "evaluator_unhealthy", LastFinishedAt: &last, NextDueAt: &last},
			{Name: "maintenance-scheduler", Registered: false, Status: "error", LastErrorClass: "internal", LastFinishedAt: &last},
			{Name: "session-expire", Registered: true, Status: "ok", LastFinishedAt: &last},
		}},
	})
	body := e.page("/system", 200)
	gone := r4Row(t, body, "job-healthcheck-ping")
	mustContain(t, "leftover", gone, "NO LONGER SCHEDULED", "no longer scheduled on this console")
	for _, s := range []string{">ERROR<", "OVERDUE", "Run now", "evaluator_unhealthy", "due "} {
		if strings.Contains(gone, s) {
			t.Errorf("the leftover row says %q: %s", s, gone)
		}
	}
	sched := r4Row(t, body, "job-maintenance-scheduler")
	mustContain(t, "scheduler", sched, ">ERROR<", "runs in the maintenance worker")
	if strings.Contains(sched, "NO LONGER SCHEDULED") {
		t.Errorf("the worker's scheduler reads as gone: %s", sched)
	}
	if r := r4Row(t, body, "job-session-expire"); !strings.Contains(r, "Run now") {
		t.Errorf("a registered job lost Run now: %s", r)
	}
}

// LOW 5: on a console that has never enrolled a device, OK does not stand alone.
func TestR4ANeverEnrolledConsoleSaysSoInTheOverallLine(t *testing.T) { // r4 low 5
	e, _ := newM3Env(t)
	e.login()
	e.canned(map[string]any{
		"/api/v1/system/health": map[string]any{"computed_at": e.now, "loaded_at": e.now, "complete": true, "overall": "ok", "first_run": true,
			"components": []adminapi.SystemComponentJSON{{ID: "fleet_data", Label: "Fleet data", Status: "not_applicable", Detail: "No devices enrolled yet: nothing is being backed up", Banners: []string{}}},
			"banners":    []any{}, "versions": map[string]any{}},
		"/api/v1/system/jobs": adminapi.JobListResponse{ComputedAt: e.now, Jobs: []adminapi.JobJSON{}},
	})
	body := e.page("/system", 200)
	i := strings.Index(body, `class="sys-sum">`)
	if i < 0 {
		t.Fatal("no overall line")
	}
	if line := body[i : i+strings.Index(body[i:], "</span>")]; !strings.Contains(line, "No devices enrolled yet: nothing is being backed up") {
		t.Errorf("the overall line: %s", line)
	}
}
