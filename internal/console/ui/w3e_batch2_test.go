package ui

import (
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// The Commands tab shows when a command was queued to the second and how long delivery took, so the
// 30 s acceptance bound can be read off the row.
func TestW3ECommandRowShowsSecondsAndDeliveryDelay(t *testing.T) {
	chi, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Skip("no tzdata")
	}
	c := fmtCtx{Loc: chi, Zone: "America/Chicago"}
	created := time.Date(2026, 10, 21, 18, 12, 5, 0, time.UTC) // 1:12:05 PM CDT
	delivered := created.Add(2 * time.Second)
	if got := c.dayClockSec(created); got != "Wed 21 Oct, 1:12:05 PM" {
		t.Errorf("dayClockSec = %q", got)
	}
	cmd := store.Command{Type: store.CommandBackupNow, CreatedAt: created, DeliveredAt: &delivered, ExpiresAt: created.Add(4 * time.Hour)}
	if got := commandTiming(cmd, "delivered", c); !strings.Contains(got, "delivered 1:12:07 PM (after 2 s)") {
		t.Errorf("timing = %q", got)
	}
	for d, want := range map[time.Duration]string{400 * time.Millisecond: "<1 s", 29 * time.Second: "29 s", 95 * time.Second: "95 s", 245 * time.Second: "4 m 05 s"} {
		if got := afterText(d); got != want {
			t.Errorf("afterText(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestW3ECommandsTabQueuedTimeHasSeconds(t *testing.T) {
	e := newEnv(t)
	e.login()
	dev := e.dev["FRONTDESK-01"]
	e.postTo("/ui/devices/"+dev+"/backup-now", url.Values{"key": {"w3e-sec"}}, http.StatusSeeOther)
	body := e.page("/devices/"+dev+"/commands", 200)
	i := strings.Index(body, `id="device-commands"`)
	if i < 0 || !regexp.MustCompile(`\d{1,2}:\d{2}:\d{2} [AP]M`).MatchString(body[i:]) {
		t.Error("the command row has no queued time with seconds")
	}
}

// 12-hour clock everywhere a time is shown in the display zone.
func TestW3ETimesAreTwelveHourEverywhere(t *testing.T) {
	chi, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Skip("no tzdata")
	}
	c := fmtCtx{Loc: chi}
	at := time.Date(2026, 10, 21, 18, 42, 10, 0, time.UTC)
	if got := c.fullTime(at); got != "Wed 21 Oct 2026 1:42:10 PM CDT" {
		t.Errorf("fullTime = %q", got)
	}
	if got := c.dayTime(at); got != "Wed 1:42 PM" {
		t.Errorf("dayTime = %q", got)
	}
	e := newEnv(t)
	e.login()
	board := e.page("/", 200)
	if !strings.Contains(board, "SNOOZED until Wed 1:42 PM") {
		t.Errorf("snooze time not on a 12-hour clock")
	}
	dev := e.dev["FRONTDESK-01"]
	if body := e.page("/devices/"+dev, 200); regexp.MustCompile(`>(Mon|Tue|Wed|Thu|Fri|Sat|Sun) \d{2}:\d{2}<`).MatchString(body) {
		t.Errorf("a run date is on a 24-hour clock")
	}
}

// A counter-named detail key is shown only while its value is a JSON number.
func TestW3EAuditCounterKeysNeedNumbers(t *testing.T) {
	raw := json.RawMessage(`{"recovery_codes_left":8,"token_count":"abc123","file_count":"n/a","tokens_revoked":2}`)
	got := map[string]store.AuditDetail{}
	for _, d := range store.AuditDetails(raw) {
		got[d.Key] = d
	}
	if got["recovery_codes_left"].Value != "8" || got["tokens_revoked"].Value != "2" {
		t.Errorf("numbers under counter keys are shown: %+v %+v", got["recovery_codes_left"], got["tokens_revoked"])
	}
	if d := got["token_count"]; d.Value != "" || d.Withheld == "" {
		t.Errorf("a string under a secret-looking counter key is withheld: %+v", d)
	}
	if got["file_count"].Value != "n/a" {
		t.Errorf("a non-secret counter key keeps its value: %+v", got["file_count"])
	}
	var safe map[string]any
	_ = json.Unmarshal(store.SafeAuditDetails(raw), &safe)
	if safe["token_count"] != "[withheld]" || safe["recovery_codes_left"] != float64(8) {
		t.Errorf("SafeAuditDetails: %v", safe)
	}
}

// The sessions table: last active and signed-in share a cell, so no column squeezes a date onto four lines.
func TestW3EAccountSessionsTableLayout(t *testing.T) {
	e := newEnv(t)
	e.login()
	body := e.page("/account", 200)
	mustContain(t, "sessions", body, "THIS SESSION", "signed in ", `class="nowrap"`)
	if strings.Contains(body, `<th scope="col">Signed in</th>`) {
		t.Error("the squeezed Signed in column is back")
	}
}
