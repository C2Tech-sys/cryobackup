package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// ---- 3a: the cancel result says why nothing was stopped ----

func TestW3ECancelDetailSaysWhyNothingWasStopped(t *testing.T) {
	cmd := func(result string) store.Command {
		return store.Command{Type: store.CommandCancel, Result: json.RawMessage(result)}
	}
	for _, c := range []struct{ result, want string }{
		{`{"cancelled":false,"reason":"run_mismatch"}`, "A newer run had started; it was left running."},
		{`{"cancelled":false,"reason":"no_run"}`, "No run was active, so nothing was stopped."},
		{`{"cancelled":false}`, "No run was active, so nothing was stopped."},
		{`{"cancelled":true}`, "The run was stopped. Files already uploaded are kept."},
	} {
		if got := commandDetail(cmd(c.result), "succeeded"); got != c.want {
			t.Errorf("%s: %q, want %q", c.result, got, c.want)
		}
	}
	// A reason that is not a fixed token never reaches the page.
	if got := commandDetail(cmd(`{"cancelled":false,"reason":"C:\\Users\\x"}`), "succeeded"); got != "No run was active, so nothing was stopped." {
		t.Errorf("a non-token reason was used: %q", got)
	}
}

// ---- 3c: one display snapshot per request ----

// A handler registered through Register sees one snapshot for its whole
// request, however the settings change meanwhile; the next request sees the new one.
func TestW3EARequestKeepsOneDisplaySnapshot(t *testing.T) {
	e := newEnv(t)
	var seen []string
	reg := &probeReg{}
	snapRegistrar{Registrar: reg, s: e.svc}.Handle("GET /probe", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		seen = append(seen, e.svc.zone(ctx))
		e.setZone(t, "Pacific/Auckland") // another request saves a new zone mid-flight
		seen = append(seen, e.svc.zone(ctx), e.svc.fc(ctx).Zone, e.svc.loc(ctx).String())
	}))
	serve := func() { reg.h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/probe", nil)) }
	e.setZone(t, "America/Chicago")
	serve()
	if got := strings.Join(seen, ","); got != "America/Chicago,America/Chicago,America/Chicago,America/Chicago" {
		t.Fatalf("one request saw %s", got)
	}
	seen = nil
	serve()
	if seen[0] != "Pacific/Auckland" {
		t.Fatalf("the next request did not see the new zone: %v", seen)
	}
}

var shownZone = regexp.MustCompile(`data-zone="([^"]+)"|\((America/Chicago|Pacific/Auckland)\)`)

// While the zone flips between two values as fast as it can, no page names both:
// the shell, the stamp, the tooltip and the table agree within every response.
func TestW3EPagesNameOneZoneWhileTheZoneChanges(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setZone(t, "America/Chicago")
	dev, cust := e.dev["FRONTDESK-01"], e.customerID("Northwind Dental")
	paths := []string{"/", "/devices", "/customers", "/customers/" + cust, "/customers/" + cust + "/devices", "/devices/" + dev, "/devices/" + dev + "/policy", "/policies", "/audit"}

	var stop atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; !stop.Load(); i++ {
			z := "America/Chicago"
			if i%2 == 1 {
				z = "Pacific/Auckland"
			}
			e.svc.Display.Apply([]store.SettingChange{{Key: store.SettingDisplayTimezone, New: json.RawMessage(`"` + z + `"`)}})
			runtime.Gosched()
		}
	}()
	defer func() { stop.Store(true); wg.Wait() }()

	for round := 0; round < 25; round++ {
		for _, p := range paths {
			body := e.page(p, 200)
			names := map[string]bool{}
			for _, m := range shownZone.FindAllStringSubmatch(body, -1) {
				names[m[1]+m[2]] = true
			}
			if len(names) > 1 {
				t.Fatalf("%s names more than one zone in one response: %v", p, names)
			}
		}
	}
}

// ---- 3d / 3e: the audit viewer's secret and path guards ----

func TestW3EAuditDetailsShowPlainFactsAndStillWithholdSecrets(t *testing.T) {
	raw := json.RawMessage(`{"exit_code":2,"status_code":503,"key":"display_timezone","tokens_revoked":3,"sessions_ended":1,` +
		`"recovery_codes_left":8,"file_count":12,"retry_count":0,` +
		`"token":"abc","api_token":"abc","recovery_codes":["a","b"],"totp_secret":"x","password":"p","passphrase":"q","secret_key":"s","code":"123456","private_key":"k","credential":"c",` +
		`"nested":{"exit_code":1,"token":"zzz"}}`)
	got := map[string]store.AuditDetail{}
	for _, d := range store.AuditDetails(raw) {
		got[d.Key] = d
	}
	for k, want := range map[string]string{"exit_code": "2", "status_code": "503", "key": "display_timezone", "tokens_revoked": "3", "sessions_ended": "1",
		"recovery_codes_left": "8", "file_count": "12", "retry_count": "0", "nested.exit_code": "1"} {
		if d := got[k]; d.Value != want || d.Withheld != "" {
			t.Errorf("%s: %+v, want the value %q shown", k, d, want)
		}
	}
	for _, k := range []string{"token", "api_token", "recovery_codes", "totp_secret", "password", "passphrase", "secret_key", "code", "private_key", "credential", "nested.token"} {
		if d := got[k]; d.Value != "" || d.Withheld == "" {
			t.Errorf("%s: %+v, want it withheld", k, d)
		}
	}
	// The API form agrees: safe keys stay, secrets are replaced.
	var safe map[string]any
	if err := json.Unmarshal(store.SafeAuditDetails(raw), &safe); err != nil {
		t.Fatal(err)
	}
	if safe["exit_code"] != float64(2) || safe["key"] != "display_timezone" || safe["token"] != "[withheld]" || safe["recovery_codes"] != "[withheld]" {
		t.Errorf("SafeAuditDetails: %v", safe)
	}
}

// The write guard sees JSON text, where a single backslash is doubled. The viewer
// and the API form check that form as well as the decoded string.
func TestW3EAuditDetailsWithholdASingleBackslashRelativePath(t *testing.T) {
	raw := json.RawMessage(`{"note":"Clients\\Smith","list":["a\\b"],"fine":"ok","slash":"a/b"}`)
	got := map[string]store.AuditDetail{}
	for _, d := range store.AuditDetails(raw) {
		got[d.Key] = d
	}
	for _, k := range []string{"note", "list"} {
		if d := got[k]; d.Value != "" || d.Withheld == "" {
			t.Errorf("%s: %+v, want it withheld as a path", k, d)
		}
	}
	if d := got["fine"]; d.Value != "ok" {
		t.Errorf("fine: %+v", d)
	}
	var safe map[string]any
	if err := json.Unmarshal(store.SafeAuditDetails(raw), &safe); err != nil {
		t.Fatal(err)
	}
	if safe["note"] != "[withheld]" || safe["fine"] != "ok" {
		t.Errorf("SafeAuditDetails: %v", safe)
	}
}

// ---- 3f: a policy save fails closed when the default policy cannot be read ----

func TestW3EPolicySaveIsRefusedWhenTheDefaultPolicyCannotBeRead(t *testing.T) {
	e := newEnv(t)
	e.signInAs("ad", "admin", nil)
	tpl := e.newTemplate("Default unreadable", map[string]string{"sla": `{"stale_after_hours":96,"unseen_after_hours":250}`})
	if err := e.st.Write(ctx2a, func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE policies SET schedule = ? WHERE id = ?`, `{"interval_minutes":1}`, tpl.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	form := e.editorForm(tpl.ID)
	form.Set("f.schedule.interval_minutes", "240")
	_, review := e.post("/ui/policies/"+tpl.ID+"/impact", form, false)
	mustContain(t, "healthy review", review, "Not carried over")
	token := hiddenOfPage(review)

	orig := defaultSectionsImpl
	defaultSectionsImpl = func(*Service, context.Context) (map[string]json.RawMessage, error) {
		return nil, fmt.Errorf("default policy unreadable")
	}
	defer func() { defaultSectionsImpl = orig }()

	before := e.auditTotal()
	res, body := e.post("/ui/policies/"+tpl.ID, token, false)
	if res.StatusCode != 500 || !strings.Contains(body, "Policy cannot be saved") {
		t.Fatalf("save with an unreadable default policy: %d %.300s", res.StatusCode, body)
	}
	cur, _ := e.st.GetPolicy(ctx2a, store.ScopeAll(), tpl.ID)
	if string(cur.Sections["schedule"]) != `{"interval_minutes":1}` || e.auditTotal() != before {
		t.Fatalf("something was saved: %s (audit %d -> %d)", cur.Sections["schedule"], before, e.auditTotal())
	}
}

// ---- helpers ----

// setZone saves the display zone as an owner would (store write, then the live prefs).
func (e *env) setZone(t *testing.T, zone string) {
	t.Helper()
	e.svc.Display.Apply([]store.SettingChange{{Key: store.SettingDisplayTimezone, New: json.RawMessage(`"` + zone + `"`)}})
}

// probeReg keeps the one handler registered on it so a test can call it.
type probeReg struct{ h http.Handler }

func (p *probeReg) Handle(_ string, h http.Handler) { p.h = h }
