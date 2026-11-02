package ui

import (
	"context"
	"strings"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// M2-W3A item 3: the gaps in the empty / loading / error / alarm states of the
// wave 1-2 screens.

func TestW3AAccountPageDoesNotSayNotSetUpWhenTheStatusCannotBeRead(t *testing.T) {
	e := newEnv(t)
	e.login()
	mustContain(t, "healthy", e.page("/account", 200), ">ON<")
	if err := e.st.Write(context.Background(), func(tx *store.Tx) error {
		_, err := tx.Exec(`ALTER TABLE admin_recovery_codes RENAME TO admin_recovery_codes_gone`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	b := e.page("/account", 200)
	mustContain(t, "unreadable status", b, "The authenticator status could not be read.", "UNKNOWN", `role="alert"`)
	if strings.Contains(b, "NOT SET UP") {
		t.Error("an unreadable authenticator status is shown as NOT SET UP")
	}
}

func TestW3ADeviceSummaryStatesAFailedReadAndABrokenPolicy(t *testing.T) {
	e := newEnv(t)
	e.login()
	// Rendered with the view the handler builds when the run list or a fact could not be read.
	html, err := e.svc.renderString("device", "device_live", struct{ Dev DeviceView }{DeviceView{ID: "d1", Host: "H", RunsErr: true, FactsErr: true}})
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, "failed reads", html, "Recent runs could not be read.", "alarm, not an empty list", "Some facts could not be read.", "a failed read, not a zero")
	if strings.Contains(html, "No runs recorded yet") {
		t.Error("a failed read is shown as 'No runs recorded yet'")
	}
	empty, err := e.svc.renderString("device", "device_live", struct{ Dev DeviceView }{DeviceView{ID: "d1", Host: "H"}})
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, "no runs", empty, "No runs recorded yet.")

	// A device whose effective policy does not compose says so in its facts.
	summit := e.policyByName("Summit Workstation")
	if err := e.st.Write(ctx2a, func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE policies SET schedule = ? WHERE id = ?`, `{"interval_minutes":1}`, summit.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	b := e.page("/devices/"+e.dev["FRONTDESK-01"], 200)
	mustContain(t, "broken policy fact", b, "NOT AVAILABLE", "does not compose")
}

func TestW3AHtmxRegionsAreLoadableAndFailLoudly(t *testing.T) {
	e := newEnv(t)
	e.login()
	for _, p := range []string{"/devices", "/audit"} {
		mustContain(t, p, e.page(p, 200), `class="stack loadable"`, `data-loading-text="Loading`, `<script src="/static/js/states.js?v=`)
	}
	res, js := e.do("GET", "/static/js/states.js", nil)
	if res.StatusCode != 200 {
		t.Fatalf("states.js: %d", res.StatusCode)
	}
	mustContain(t, "states.js", js, "htmx:beforeRequest", "htmx:afterRequest", "e.detail.failed", "Could not load the list", "alarm, not an empty list", "data-slow", "Retry")
	if strings.Contains(js, "fetch(") || strings.Contains(js, "XMLHttpRequest") || strings.Contains(js, "localStorage") {
		t.Error("states.js makes a request or stores something")
	}
	_, css := e.do("GET", "/static/app.css", nil)
	mustContain(t, "css", css, ".loadable.htmx-request table.fleet tbody td::after", `.loadable.htmx-request[data-slow="true"]::before`, "This is taking longer than usual")
}
