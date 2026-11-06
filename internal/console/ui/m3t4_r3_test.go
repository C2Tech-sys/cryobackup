package ui

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminapi"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// M3-T4 fix round 3.

func TestR3NotInBuildAndNotApplicableChipsAreNeutralNotAmber(t *testing.T) { // M1
	for status, label := range map[string]string{"not_in_build": "NOT IN THIS BUILD", "not_applicable": "NOT APPLICABLE"} {
		c := sysStatusChip("x", status)
		if c.Label != label || c.Class != ClassNeutral || !c.Dashed {
			t.Errorf("%s chip: %+v", status, c)
		}
	}
}

// M1 on S36: rows that cannot be checked here are shown honestly, under an overall OK that says so.
func TestR3S36ShowsWhatCannotBeCheckedHereUnderAnOverallOK(t *testing.T) { // M1
	e, _ := newM3Env(t)
	e.login()
	e.canned(map[string]any{
		"/api/v1/system/health": adminapi.SystemHealthResponse{StampJSON: adminapi.StampJSON{ComputedAt: e.now, LoadedAt: e.now, Complete: true}, Overall: "ok",
			Components: []adminapi.SystemComponentJSON{
				{ID: "gateway", Label: "Gateway", Status: "ok", Banners: []string{}},
				{ID: "release_manifest", Label: "Release manifest", Status: "not_in_build", Detail: "this build does not run release-check", Banners: []string{}},
				{ID: "clock", Label: "Server clock", Status: "not_applicable", Detail: "needs at least 5 devices", Banners: []string{}},
			}},
		"/api/v1/system/jobs": adminapi.JobListResponse{ComputedAt: e.now, Jobs: []adminapi.JobJSON{}},
	})
	body := e.page("/system", 200)
	mustContain(t, "rows", body, "NOT IN THIS BUILD", "NOT APPLICABLE", "This build does not run release-check", "Needs at least 5 devices")
	mustContain(t, "overall says what it leaves out", body, "Every component this console can check has evidence and is healthy", "2 cannot be checked here")
	overall := regexp.MustCompile(`id="sys-h" class="sys-title">Overall</h2><span class="st ok"><i aria-hidden="true"></i>OK<`).FindString(body)
	if overall == "" {
		t.Error("the overall chip is not OK")
	}
	// This env's fleet is unwired, so the banner strip says C00; the overall line is what is judged here.
	if sum := regexp.MustCompile(`class="sys-sum">([^<]*)<`).FindStringSubmatch(body); sum == nil || strings.Contains(sum[1], "not being monitored") {
		t.Errorf("the overall line says something in this build is unmonitored: %v", sum)
	}
}

// A customer whose devices are all retired says that, not "no devices yet".
func TestR3AnAllRetiredCustomerSaysItsDevicesAreRetired(t *testing.T) { // low
	e, _ := newM3Env(t)
	e.login()
	pine := e.customerID("Pinecrest Legal")
	if err := e.st.Write(context.Background(), func(tx *store.Tx) error {
		if _, err := tx.Exec(`UPDATE devices SET paused_at = NULL, paused_until = NULL, pause_reason = NULL, paused_by = NULL WHERE customer_id = ?`, pine); err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE devices SET retired_at = ?, retired_reason = 'replaced' WHERE customer_id = ?`, e.now.Add(-time.Hour).UnixMilli(), pine)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	body := e.page("/activity?customer="+pine, 200)
	if strings.Contains(body, "has no devices yet") {
		t.Error("a customer whose devices are all retired reads as one that never had any")
	}
	mustContain(t, "retired", body, "All devices retired", "Every device of Pinecrest Legal is retired")
}

// /runs filtered to a MISSING customer shows the MISSING alarm, as /activity does.
func TestR3RunsFilteredToAMissingCustomerIsTheMissingAlarm(t *testing.T) { // low
	e, _ := newM3Env(t)
	e.login()
	blue := e.customerID("Blue Ridge Accounting")
	body := e.page("/runs?customer="+blue, 200)
	mustContain(t, "alarm", body, "MISSING: no devices", "panel-state alarm", "Blue Ridge Accounting")
	if strings.Contains(body, "No runs match these conditions") {
		t.Error("the page still says a calm no-match about a customer the engine calls MISSING")
	}
	// The fragment the page polls says the same.
	mustContain(t, "fragment", e.page("/ui/runs/table?customer="+blue, 200), "MISSING: no devices", "panel-state alarm")
	// No customer condition, or a healthy customer's, is not the MISSING alarm.
	if strings.Contains(e.page("/runs", 200), "MISSING: no devices") || strings.Contains(e.page("/runs?customer="+e.customerID("Summit Eye Care"), 200), "MISSING: no devices") {
		t.Error("the alarm shows where no MISSING customer is asked for")
	}
}

// M2: what a stored version shows.
func TestR3RunDetailShowsUnknownAndWithholdsARawLabel(t *testing.T) { // M2
	e, _ := newM3Env(t)
	e.login()
	fin := e.now.Add(-time.Hour)
	mk := func(n int, v string) string {
		e.ingest("FRONTDESK-01", store.RunInput{ID: ulid(n), Kind: "backup", Trigger: "schedule", StartedAt: fin.Add(-time.Minute), FinishedAt: &fin, Outcome: "success", AgentVersion: v})
		return ulid(n)
	}
	unknown := e.page("/runs/"+mk(901, "unknown"), 200)
	mustContain(t, "unknown", unknown, "Agent version", ">unknown<")
	raw := e.page("/runs/"+mk(902, "Patient_Chart.docx"), 200)
	if strings.Contains(raw, "Patient_Chart") {
		t.Error("a row with a raw label shows it")
	}
	mustContain(t, "withheld", raw, withheld)
}
