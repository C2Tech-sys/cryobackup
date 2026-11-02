package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// M2-W3A: the audit viewer (/audit), the settings page (/settings), the live
// display zone, the copy buttons and the loading/empty/error states they add.

func (e *env) appendAudit(ev store.AuditEvent) store.AuditEvent {
	e.t.Helper()
	out, err := e.st.AppendAudit(context.Background(), ev)
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}

func (e *env) seedAudit(n int, action string, at time.Time, customerID string) {
	e.t.Helper()
	for i := 0; i < n; i++ {
		e.appendAudit(store.AuditEvent{OccurredAt: at.Add(time.Duration(i) * time.Second), ActorType: "admin", ActorLabel: "pat", Method: "web", SourceIP: "10.2.3.4",
			Action: action, ObjectType: "customer", ObjectID: customerID, CustomerID: customerID, Details: json.RawMessage(`{"fields":["notes"]}`)})
	}
}

func TestW3APagesRenderInFixturesMode(t *testing.T) {
	e := newEnv(t)
	e.login()
	summit := e.customerID("Summit Eye Care")
	ev := e.appendAudit(store.AuditEvent{ActorType: "admin", ActorLabel: "pat", Action: "customer.update", ObjectType: "customer", ObjectID: summit, CustomerID: summit,
		Details: json.RawMessage(`{"fields":["name","notes"]}`)})
	pages := map[string]string{
		"audit":           "/audit",
		"audit filtered":  "/audit?action=customer.*&result=ok&type=customer&from=2020-01-01&to=2099-12-31&size=25",
		"audit one event": "/audit?event=" + ev.ID,
		"audit fragment":  "/ui/audit/table?action=customer.update",
		"audit empty":     "/audit?actor=nobody-at-all",
		"settings":        "/settings",
	}
	for name, path := range pages {
		body := e.page(path, 200)
		if strings.Contains(body, "ZgotmplZ") {
			t.Errorf("%s: html/template rejected a value (ZgotmplZ)", name)
		}
		if absoluteURL.MatchString(body) {
			t.Errorf("%s: an absolute URL reference (no CDN, no third party)", name)
		}
		if m := pathLike.FindString(body); m != "" {
			t.Errorf("%s: shows a path-like string %q", name, m)
		}
		if !strings.HasPrefix(path, "/ui/") && !strings.Contains(body, `id="freshness"`) {
			t.Errorf("%s: no freshness stamp", name)
		}
	}
	mustContain(t, "audit", e.page("/audit", 200), "Audit log", `id="auditresults"`, `hx-get="/ui/audit/table`, `role="search"`, `data-loading-text="Loading the audit log"`,
		"Customer changed", "customer.update", `href="/audit" aria-current="page"`, "stored in UTC", "READ-ONLY")
	mustContain(t, "audit detail", e.page("/audit?event="+ev.ID, 200), `id="audit-detail"`, "Stored details", "fields", "name", "notes", "10.", ev.ID, "Close")
	mustContain(t, "settings", e.page("/settings", 200), "Display time zone", "America/Chicago", "Refresh interval", `action="/ui/settings"`, `name="csrf_token"`,
		"NOT EDITABLE YET", "Audit retention", "SET BY THE CONSOLE", `href="/settings" aria-current="page"`)
}

func TestW3AAuditAndSettingsRoleGating(t *testing.T) {
	e := newEnv(t)
	gets := []string{"/audit", "/ui/audit/table", "/settings"}
	for _, p := range gets {
		if res, _ := e.do("GET", p, nil); res.StatusCode != 303 || res.Header.Get("Location") != "/login" {
			t.Errorf("anonymous GET %s: %d", p, res.StatusCode)
		}
	}
	form := url.Values{"display_timezone": {"Europe/Berlin"}, "refresh_interval_s": {"45"}}
	for _, rl := range []struct {
		name, role string
		read, save bool
	}{{"ro", "read_only", false, false}, {"op", "operator", false, false}, {"ad", "admin", true, false}, {"ow", "owner", true, true}} {
		e.signInAs(rl.name, rl.role, nil)
		for _, p := range gets {
			want := 403
			if rl.read {
				want = 200
			}
			e.page(p, want)
		}
		before := len(e.auditRows("settings.update", ""))
		f := url.Values{}
		for k, v := range form {
			f[k] = v
		}
		res, _ := e.post("/ui/settings", f, false)
		if rl.save && res.StatusCode != http.StatusSeeOther || !rl.save && res.StatusCode != http.StatusForbidden {
			t.Errorf("%s: POST /ui/settings: %d", rl.role, res.StatusCode)
		}
		if !rl.save && len(e.auditRows("settings.update", "")) != before {
			t.Errorf("%s: a denied save wrote an audit row", rl.role)
		}
		if !rl.save {
			continue
		}
		// An admin sees the page read-only; the owner gets the form enabled.
	}
	// Admin: the form is there but disabled, with the reason, and no Save button.
	e.signInAs("ad", "admin", nil)
	body := e.page("/settings", 200)
	mustContain(t, "admin settings", body, "View only.", "needs the owner role", " disabled")
	if strings.Contains(body, "Save settings") {
		t.Error("an admin is offered a Save button")
	}
	e.signInAs("ow", "owner", nil)
	if body = e.page("/settings", 200); strings.Contains(body, "View only.") || !strings.Contains(body, "Save settings") {
		t.Error("the owner does not get an editable form")
	}
	// Nav: admins and owners get the two links, read-only and operators do not.
	e.signInAs("ro", "read_only", nil)
	if b := e.page("/customers", 200); strings.Contains(b, `href="/audit"`) || strings.Contains(b, `href="/settings"`) {
		t.Error("a read-only account is offered the audit and settings links")
	}
	e.signInAs("ow", "owner", nil)
	if b := e.page("/customers", 200); !strings.Contains(b, `href="/audit"`) || !strings.Contains(b, `href="/settings"`) {
		t.Error("an owner is not offered the audit and settings links")
	}
	// A save needs the CSRF token and a same-origin request.
	before := len(e.auditRows("settings.update", ""))
	if res, _ := e.do("POST", "/ui/settings", form, "Origin", e.srv.URL); res.StatusCode != 403 {
		t.Errorf("POST without a CSRF token: %d", res.StatusCode)
	}
	f := url.Values{"display_timezone": {"Europe/Berlin"}, "refresh_interval_s": {"45"}, "csrf_token": {e.csrf}}
	if res, _ := e.do("POST", "/ui/settings", f, "Origin", "https://evil.example"); res.StatusCode != 403 {
		t.Errorf("cross-origin POST: %d", res.StatusCode)
	}
	if len(e.auditRows("settings.update", "")) != before {
		t.Error("a refused save wrote an audit row")
	}
}

// Audit and settings are admin pages and an admin always sees every customer
// (the schema allows a customer scope on operators and read-only accounts
// only), so a scoped account never reaches them: it is refused, and the
// store-level scope matrix covers the rows themselves.
func TestW3AScopedAccountsAreRefusedTheAuditLogAndSettings(t *testing.T) {
	e := newEnv(t)
	summit := e.customerID("Summit Eye Care")
	e.appendAudit(store.AuditEvent{ActorType: "admin", ActorLabel: "pat", Action: "scope.visible", CustomerID: summit})
	for _, role := range []string{"operator", "read_only"} {
		e.signInAs("scoped-"+role, role, []string{summit})
		for _, p := range []string{"/audit", "/ui/audit/table", "/settings"} {
			if b := e.page(p, 403); strings.Contains(b, "scope.visible") {
				t.Errorf("%s GET %s: the refusal page shows an event", role, p)
			}
		}
		if res, _ := e.post("/ui/settings", url.Values{"display_timezone": {"Europe/Berlin"}, "refresh_interval_s": {"45"}}, false); res.StatusCode != 403 {
			t.Errorf("%s POST /ui/settings: %d", role, res.StatusCode)
		}
	}
}

func TestW3AAuditFiltersPagingAndDeepLinks(t *testing.T) {
	e := newEnv(t)
	e.login()
	summit := e.customerID("Summit Eye Care")
	// 120 events: Jul 4 2026 03:30 UTC is 10:30 PM on Jul 3 in Chicago (CDT).
	e.seedAudit(120, "paging.probe", time.Date(2026, 7, 4, 3, 30, 0, 0, time.UTC), summit)
	e.appendAudit(store.AuditEvent{OccurredAt: time.Date(2026, 7, 4, 3, 40, 0, 0, time.UTC), ActorType: "admin", ActorLabel: "zoe", Action: "other.thing", Result: "denied", CustomerID: summit})

	b := e.page("/audit?action=paging.probe", 200)
	mustContain(t, "page 1", b, "Showing 1-50 of 120", `aria-current="page"`, `href="/audit?action=paging.probe&amp;page=2"`, "Action: paging.probe", "Clear all", `aria-label="Remove filter: Action: paging.probe"`)
	if n := strings.Count(b, `class="ev`); n != 50 {
		t.Errorf("rows on page 1 = %d, want 50", n)
	}
	b = e.page("/audit?action=paging.probe&page=3", 200)
	mustContain(t, "page 3", b, "Showing 101-120 of 120")
	b = e.page("/audit?action=paging.probe&page=9", 200)
	mustContain(t, "page past the end", b, "Showing 101-120 of 120", "That page is past the end")
	b = e.page("/audit?action=paging.probe&size=100", 200)
	mustContain(t, "size", b, "Showing 1-100 of 120")

	// Family and result filters; the actor filter matches the label case-insensitively.
	if b = e.page("/audit?action=other.*&result=denied&actor=ZOE", 200); !strings.Contains(b, "other.thing") || !strings.Contains(b, "DENIED") || !strings.Contains(b, "Showing 1-1 of 1") {
		t.Errorf("combined filters:\n%s", b[:min(len(b), 600)])
	}

	// Dates are days in the display zone. 10:30 PM Jul 3 Chicago is Jul 3, not Jul 4.
	if b = e.page("/audit?action=paging.probe&from=2026-07-03&to=2026-07-03", 200); !strings.Contains(b, "Showing 1-50 of 120") {
		t.Error("the Jul 3 (Chicago) range misses events at 10:30 PM Jul 3")
	}
	b = e.page("/audit?action=paging.probe&from=2026-07-04&to=2026-07-04", 200)
	if !strings.Contains(b, "No events match these filters") || !strings.Contains(b, "0 of ") {
		t.Errorf("the Jul 4 (Chicago) range must be empty, with its denominator:\n%s", b[:min(len(b), 400)])
	}
	mustContain(t, "range chips", b, "From 2026-07-04", "Until 2026-07-04", "Clear filters")

	// A filter that means nothing is refused, not shown unfiltered.
	for _, bad := range []string{"result=maybe", "from=yesterday", "to=2026-13-45", "action=a%20b"} {
		if b = e.page("/audit?"+bad, 400); !strings.Contains(b, "Filters not valid") {
			t.Errorf("%s: no 'Filters not valid' page", bad)
		}
	}

	// The fragment pushes the canonical URL and holds only the results region.
	res, frag := e.do("GET", "/ui/audit/table?action=paging.probe&page=2", nil, "HX-Request", "true")
	if res.StatusCode != 200 || res.Header.Get("HX-Push-Url") != "/audit?action=paging.probe&page=2" {
		t.Fatalf("fragment: %d push %q", res.StatusCode, res.Header.Get("HX-Push-Url"))
	}
	if strings.Contains(frag, "<html") || !strings.Contains(frag, "Showing 51-100 of 120") {
		t.Errorf("fragment content: %.300s", frag)
	}
	res, _ = e.do("GET", "/ui/audit/table?result=maybe", nil)
	if res.StatusCode != 400 {
		t.Errorf("fragment with a bad filter: %d", res.StatusCode)
	}
}

func TestW3AAuditDetailShowsStoredDetailsAndWithholdsTheRest(t *testing.T) {
	e := newEnv(t)
	e.login()
	summit := e.customerID("Summit Eye Care")
	ev := e.appendAudit(store.AuditEvent{ActorType: "admin", ActorLabel: "pat", ActorID: "01J0000000000000000000ACTR", Method: "api", SourceIP: "192.0.2.7", Action: "detail.probe",
		ObjectType: "customer", ObjectID: summit, CustomerID: summit, Result: "denied", Severity: "warning",
		Details: json.RawMessage(`{"reason":"policy says no","password":"hunter2","fields":["a","b"]}`)})
	b := e.page("/audit?event="+ev.ID+"&action=detail.probe", 200)
	mustContain(t, "detail", b, `id="audit-detail"`, "Detail probe", "detail.probe", "192.0.2.7", "01J0000000000000000000ACTR", "policy says no", "DENIED", "severity warning",
		"Not shown: the key name suggests a secret.", `href="/audit?action=detail.probe"`, "UTC")
	if strings.Contains(b, "hunter2") {
		t.Error("a secret-looking value is shown")
	}
	// The selected row is marked, and the panel survives the fragment.
	mustContain(t, "selected row", b, `class="ev sel"`, `aria-current="true"`)
	// A missing event is loud: 404 with a flagged panel on the page, a 200 panel in the fragment.
	b = e.page("/audit?event=01J00000000000000000000NOPE", 404)
	mustContain(t, "missing", b, "That event cannot be shown.", `role="alert"`)
	if res, frag := e.do("GET", "/ui/audit/table?event=01J00000000000000000000NOPE", nil); res.StatusCode != 200 || !strings.Contains(frag, "cannot be shown") {
		t.Errorf("fragment of a missing event: %d", res.StatusCode)
	}
	// Times: the row shows the display zone, the panel adds UTC and the full time.
	at := time.Date(2026, 1, 15, 3, 5, 9, 0, time.UTC)
	tev := e.appendAudit(store.AuditEvent{OccurredAt: at, ActorType: "system", Action: "time.probe"})
	b = e.page("/audit?event="+tev.ID+"&action=time.probe", 200)
	mustContain(t, "times", b, "2026-01-15 3:05:09 AM UTC", `datetime="2026-01-15T03:05:09Z"`, "9:05:09 PM")
}

func TestW3AEmptyAndErrorStates(t *testing.T) {
	e := newEnv(t)
	e.login()
	// A trail with nothing in it is an alarm, not a blank table.
	if err := e.st.Write(context.Background(), func(tx *store.Tx) error {
		_, err := tx.Exec(`DELETE FROM audit_events`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	b := e.page("/audit", 200)
	mustContain(t, "empty trail", b, "The audit trail is empty", `panel-state alarm`, `role="alert"`)
	// The read fails: an alarm page with a 503, never an empty list.
	if err := e.st.Write(context.Background(), func(tx *store.Tx) error {
		_, err := tx.Exec(`ALTER TABLE audit_events RENAME TO audit_events_gone`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	b = e.page("/audit", 503)
	mustContain(t, "audit read failure", b, "Audit log unavailable", "alarm, not an empty list")
	res, frag := e.do("GET", "/ui/audit/table", nil, "HX-Request", "true")
	if res.StatusCode != 200 || !strings.Contains(frag, "Could not load the audit log") || !strings.Contains(frag, "panel-state alarm") {
		t.Errorf("fragment failure: %d %.200s", res.StatusCode, frag)
	}
}

func TestW3ASettingsChangeTheZoneOnTheNextPageAndIsAudited(t *testing.T) {
	e := newEnv(t)
	e.login()
	// Before: Chicago. The fixtures clock is a fixed instant; its 12-hour time in Chicago is the stamp.
	before := e.page("/devices", 200)
	mustContain(t, "before", before, `data-zone="America/Chicago"`, "(America/Chicago)")
	chicago := regexp.MustCompile(`class="t" data-at="([^"]*)"`).FindStringSubmatch(before)
	if len(chicago) != 2 {
		t.Fatal("no freshness time on the page")
	}

	n := len(e.auditRows("settings.update", ""))
	f := url.Values{"display_timezone": {"Asia/Tokyo"}, "refresh_interval_s": {"90"}}
	res, _ := e.post("/ui/settings", f, false)
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/settings?done=saved" {
		t.Fatalf("save: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	rows := e.auditRows("settings.update", "")
	if len(rows) != n+2 { // one row per changed setting
		t.Fatalf("audit rows %d -> %d, want two more", n, len(rows))
	}
	for _, r := range rows[:2] {
		if r.ActorLabel != "admin" || r.Method != "web" || r.ObjectType != "setting" || r.SourceIP == "" {
			t.Errorf("audit row: %+v", r)
		}
	}

	// After: every page uses Tokyo at once - the stamp, its tooltip, the refresh interval.
	after := e.page("/devices", 200)
	mustContain(t, "after", after, `data-zone="Asia/Tokyo"`, "(Asia/Tokyo)", `data-refresh="90"`)
	if strings.Contains(after, `data-zone="America/Chicago"`) {
		t.Error("the old zone is still on the page")
	}
	tokyo := regexp.MustCompile(`class="t" data-at="([^"]*)"`).FindStringSubmatch(after)
	if len(tokyo) != 2 || tokyo[1] == chicago[1] {
		t.Errorf("the page time did not move with the zone: %v vs %v", chicago, tokyo)
	}
	// The audit log shows the change itself in the new zone, with the old and new values.
	b := e.page("/audit?action=settings.update", 200)
	mustContain(t, "audit of the change", b, "Setting changed", "settings.update", "Asia/Tokyo", "(Asia/Tokyo)")
	b = e.page("/settings?done=saved", 200)
	mustContain(t, "settings after", b, `value="Asia/Tokyo"`, `value="90"`, "Settings saved.", "Last changed by admin")

	// Saving the same values again changes nothing and writes no audit row.
	res, _ = e.post("/ui/settings", url.Values{"display_timezone": {"Asia/Tokyo"}, "refresh_interval_s": {"90"}}, false)
	if res.Header.Get("Location") != "/settings?done=nochange" || len(e.auditRows("settings.update", "")) != n+2 {
		t.Errorf("no-op save: %s, %d audit rows", res.Header.Get("Location"), len(e.auditRows("settings.update", "")))
	}
	mustContain(t, "no change", e.page("/settings?done=nochange", 200), "Nothing changed.")
}

func TestW3ASettingsRefuseBadValuesAtTheirFields(t *testing.T) {
	e := newEnv(t)
	e.login()
	n := len(e.auditRows("settings.update", ""))
	cases := []struct {
		name         string
		zone, refr   string
		wantZone     bool
		wantRefresh  bool
		echoedBack   string
		mustNotShown string
	}{
		{"unknown zone", "Mars/Base", "30", true, false, `value="Mars/Base"`, ""},
		{"empty zone", "", "30", true, false, "", ""},
		{"short interval", "UTC", "2", false, true, `value="2"`, ""},
		{"not a number", "UTC", "soon", false, true, `value="soon"`, ""},
		{"both", "Local", "99999", true, true, "", ""},
	}
	for _, c := range cases {
		res, body := e.post("/ui/settings", url.Values{"display_timezone": {c.zone}, "refresh_interval_s": {c.refr}}, false)
		if res.StatusCode != 400 {
			t.Errorf("%s: %d", c.name, res.StatusCode)
			continue
		}
		mustContain(t, c.name, body, "Nothing was saved.")
		if c.wantZone != strings.Contains(body, `id="set-zone-err"`) || c.wantRefresh != strings.Contains(body, `id="set-refresh-err"`) {
			t.Errorf("%s: field errors misplaced (zone %v, refresh %v)", c.name, strings.Contains(body, `id="set-zone-err"`), strings.Contains(body, `id="set-refresh-err"`))
		}
		if c.echoedBack != "" && !strings.Contains(body, c.echoedBack) {
			t.Errorf("%s: the posted value is not kept in the form", c.name)
		}
	}
	if len(e.auditRows("settings.update", "")) != n {
		t.Error("a refused save wrote audit rows")
	}
	// The zone did not move.
	mustContain(t, "still chicago", e.page("/devices", 200), `data-zone="America/Chicago"`)
}

func TestW3ASettingsAreRefusedWhenTheyCannotBeAudited(t *testing.T) {
	e := newEnv(t)
	e.login()
	if err := e.st.Write(context.Background(), func(tx *store.Tx) error {
		_, err := tx.Exec(`CREATE TRIGGER t_no_audit_w3a BEFORE INSERT ON audit_events BEGIN SELECT RAISE(ABORT, 'audit down'); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	res, body := e.post("/ui/settings", url.Values{"display_timezone": {"Asia/Tokyo"}, "refresh_interval_s": {"30"}}, false)
	if res.StatusCode != http.StatusInternalServerError || !strings.Contains(body, "could not be audited") {
		t.Fatalf("not audited: %d %.300s", res.StatusCode, body)
	}
	mustContain(t, "unchanged", e.page("/devices", 200), `data-zone="America/Chicago"`)
}

func TestW3ASettingsNoteAWrongStoredZone(t *testing.T) {
	e := newEnv(t)
	e.login()
	// Boot the prefs from a corrupt stored value: the console must say it fell back.
	if err := e.st.PutSetting(context.Background(), store.SettingDisplayTimezone, json.RawMessage(`"Not/AZone"`), ""); err != nil {
		t.Fatal(err)
	}
	prefs, err := e.st.LoadDisplayPrefs(context.Background(), "America/Chicago", 30)
	if err != nil {
		t.Fatal(err)
	}
	e.svc.Display = prefs
	b := e.page("/settings", 200)
	mustContain(t, "fallback note", b, "Stored display settings were not used.", "not a valid IANA zone", `value="America/Chicago"`)
}

// Copy buttons: markup on every secret and snippet, hidden-until-script, with the select-on-click fallback.
func TestW3ACopyButtonsOnSecretsAndSnippets(t *testing.T) {
	e, _ := wizardEnv(t)
	check := func(name, body string, ids ...string) {
		t.Helper()
		for _, id := range ids {
			btn := fmt.Sprintf(`data-copy-from="%s"`, id)
			if !strings.Contains(body, btn) {
				t.Errorf("%s: no copy button for #%s", name, id)
			}
			if !regexp.MustCompile(`<(pre|ol)[^>]* id="` + id + `"`).MatchString(body) {
				t.Errorf("%s: the copy target #%s does not exist", name, id)
			}
		}
		mustContain(t, name, body, `class="btn sm copybtn"`, `class="copyhint meta-mono">Select the text and press Ctrl+C to copy it.`, `<span class="copylabel">Copy</span>`)
	}
	summit := e.customerID("Summit Eye Care")
	res, body := e.createToken(summit, nil)
	if res.StatusCode != 200 {
		t.Fatalf("create token: %d", res.StatusCode)
	}
	check("enrollment token page", body, "cp-enroll-token", "cp-rmm", "cp-msiexec", "cp-gpo")
	mustContain(t, "copy script", body, `<script src="/static/js/copy.js?v=`)
	// The secret is still only in the text, never in an attribute of the button.
	tok := shownToken.FindStringSubmatch(body)
	if len(tok) != 2 || strings.Count(body, tok[1]) < 1 {
		t.Fatal("token not found")
	}
	for _, m := range regexp.MustCompile(`<button[^>]*copybtn[^>]*>`).FindAllString(body, -1) {
		if strings.Contains(m, tok[1]) {
			t.Error("the token is in a button attribute")
		}
	}

	res, body = e.post("/ui/account/tokens", url.Values{"name": {"rmm"}, "scope": {"read"}, "days": {"30"}}, false)
	if res.StatusCode != 200 {
		t.Fatalf("api token: %d", res.StatusCode)
	}
	check("api token", body, "cp-api-token")
	// Recovery codes, the authenticator key and the one-time reset link share partials; render them with known data.
	codes, err := e.svc.renderString("account", "acct_codes", struct {
		Codes    []string
		CodesTxt string
	}{[]string{"aaaa-bbbb", "cccc-dddd"}, "aaaa-bbbb%0Acccc-dddd"})
	if err != nil {
		t.Fatal(err)
	}
	check("recovery codes", codes, "cp-recovery")
	key, err := e.svc.renderString("account", "acct_enroll", &EnrollView{Secret: "JBSWY3DPEHPK3PXP", Expires: "soon"})
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{`data-copy-from="cp-totp-key"`, `id="cp-totp-key"`, "JBSWY3DPEHPK3PXP"} {
		if !strings.Contains(key, w) {
			t.Errorf("authenticator key block misses %q", w)
		}
	}
	res, body = e.post("/ui/accounts", url.Values{"username": {"newhire"}, "email": {"newhire@example.invalid"}, "role": {"operator"}}, false)
	if res.StatusCode != 200 {
		t.Fatalf("create account: %d", res.StatusCode)
	}
	check("reset link", body, "cp-reset-link")

	// The script is served, first-party, and the page works without it.
	res, js := e.do("GET", "/static/js/copy.js", nil)
	if res.StatusCode != 200 || !strings.Contains(js, "navigator.clipboard") || !strings.Contains(js, "execCommand") || !strings.Contains(js, "Copied") {
		t.Fatalf("copy.js: %d", res.StatusCode)
	}
	if regexp.MustCompile(`(?i)https?://|XMLHttpRequest|fetch\(|localStorage|sendBeacon`).MatchString(js) {
		t.Error("copy.js makes a request or stores something")
	}
	_, css := e.do("GET", "/static/app.css", nil)
	mustContain(t, "css", css, ".copybtn { display: none; }", ".js .copybtn { display: inline-flex; }", ".js .copyhint { display: none; }", "user-select: all")

	// Downloads: the hash and its check command.
	res, _ = e.do("GET", "/downloads", nil)
	if res.StatusCode != 200 {
		t.Fatalf("downloads: %d", res.StatusCode)
	}
}
