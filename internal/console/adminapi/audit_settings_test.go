package adminapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// M2-W3A: GET /api/v1/audit, GET /api/v1/settings, PUT /api/v1/settings/{key}.

func (e *env) withDisplay(zone string) *store.DisplayPrefs {
	e.t.Helper()
	d := store.NewDisplayPrefs(zone, 30)
	e.svc.Display = d
	return d
}

func (e *env) putSetting(s *session, key string, value any) (*http.Response, []byte) {
	return e.req("PUT", "/api/v1/settings/"+key, s, map[string]any{"value": value})
}

func TestAuditAndSettingsRoutesAreRoleGated(t *testing.T) {
	e := newEnv(t)
	ss := e.sessionsByRole()
	rank := map[string]int{"RO": 1, "OP": 2, "AD": 3, "OW": 4}
	type route struct {
		name, method, path string
		body               any
		min                string
	}
	routes := []route{
		{"audit list", "GET", "/api/v1/audit", nil, "AD"},
		{"settings list", "GET", "/api/v1/settings", nil, "AD"},
		{"setting put", "PUT", "/api/v1/settings/refresh_interval_s", map[string]any{"value": 40}, "OW"},
	}
	for _, rt := range routes {
		for _, role := range []string{"RO", "OP", "AD", "OW"} {
			res, b := e.req(rt.method, rt.path, ss[role], rt.body)
			if rank[role] >= rank[rt.min] {
				if res.StatusCode != http.StatusOK {
					t.Errorf("%s as %s: %d %s", rt.name, role, res.StatusCode, b)
				}
				continue
			}
			if res.StatusCode != http.StatusForbidden || problemCode(t, res, b) != api.CodeForbidden {
				t.Errorf("%s as %s: want 403 forbidden, got %d %s", rt.name, role, res.StatusCode, b)
			}
		}
		if res, _ := e.req(rt.method, rt.path, nil, rt.body); res.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s without a session: %d, want 401", rt.name, res.StatusCode)
		}
	}
	// A cookie session without the CSRF token cannot change a setting.
	bad := &session{cookies: ss["OW"].cookies, csrf: "wrong"}
	before := e.audits()
	if res, _ := e.putSetting(bad, "refresh_interval_s", 99); res.StatusCode != http.StatusForbidden {
		t.Errorf("PUT with a wrong CSRF token: %d, want 403", res.StatusCode)
	}
	if e.audits() != before {
		t.Error("a refused PUT wrote an audit row")
	}
}

func TestAuditListFiltersPagesAndShowsLocalTime(t *testing.T) {
	e := newEnv(t)
	e.withDisplay("America/Chicago")
	s := e.login("admin", pw)
	summit := e.customerNamed("Summit Eye Care")
	ctx := context.Background()
	at := time.Date(2026, 7, 4, 18, 30, 0, 0, time.UTC) // 1:30 PM CDT
	for i := 0; i < 3; i++ {
		if _, err := e.st.AppendAudit(ctx, store.AuditEvent{OccurredAt: at.Add(time.Duration(i) * time.Minute), ActorType: "admin", ActorLabel: "Pat", Method: "web", SourceIP: "10.1.1.1",
			Action: "customer.update", ObjectType: "customer", ObjectID: summit.ID, CustomerID: summit.ID, Details: json.RawMessage(`{"fields":["notes"]}`)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.st.AppendAudit(ctx, store.AuditEvent{OccurredAt: at, ActorType: "system", Action: "probe.leak", Details: json.RawMessage(`{"password":"hunter2","note":"fine"}`)}); err != nil {
		t.Fatal(err)
	}

	status, b := e.get("/api/v1/audit?action=customer.update&limit=2", s)
	if status != 200 {
		t.Fatalf("%d %s", status, b)
	}
	r := decode[AuditListResponse](t, b)
	if r.Total != 3 || len(r.Events) != 2 || !r.Truncated || r.NextCursor != "o2" || r.Of < 4 || r.DisplayTimezone != "America/Chicago" {
		t.Fatalf("page 1: total %d events %d truncated %v cursor %q of %d zone %q", r.Total, len(r.Events), r.Truncated, r.NextCursor, r.Of, r.DisplayTimezone)
	}
	top := r.Events[0]
	if !top.OccurredAt.Equal(at.Add(2*time.Minute)) || top.OccurredLocal != "Sat 4 Jul 2026, 1:32:00 PM CDT" || top.Actor != "Pat" || top.SourceIP != "10.1.1.1" {
		t.Fatalf("newest first, UTC instant plus the 12-hour local form: %+v", top)
	}
	_, b = e.get("/api/v1/audit?action=customer.update&limit=2&cursor=o2", s)
	if r2 := decode[AuditListResponse](t, b); len(r2.Events) != 1 || r2.Truncated || r2.NextCursor != "" {
		t.Fatalf("page 2: %+v", r2)
	}

	// Filters: date range, customer, family, result.
	q := "/api/v1/audit?customer_id=" + summit.ID + "&action=customer.*&since=" + at.Add(time.Minute).Format(time.RFC3339) + "&until=" + at.Add(2*time.Minute).Format(time.RFC3339)
	if _, b = e.get(q, s); decode[AuditListResponse](t, b).Total != 1 {
		t.Fatalf("range filter: %s", b)
	}
	if _, b = e.get("/api/v1/audit?result=denied", s); decode[AuditListResponse](t, b).Total != 0 {
		t.Fatalf("denied: %s", b)
	}
	// An empty result carries the denominator and an empty array, never null.
	if _, b = e.get("/api/v1/audit?actor=nobody-at-all", s); !strings.Contains(string(b), `"events":[]`) || decode[AuditListResponse](t, b).Of < 4 {
		t.Fatalf("empty result: %s", b)
	}

	// A value under a secret-looking key never leaves the API.
	_, b = e.get("/api/v1/audit?action=probe.leak", s)
	if strings.Contains(string(b), "hunter2") || !strings.Contains(string(b), `"[withheld]"`) || !strings.Contains(string(b), `"note":"fine"`) {
		t.Fatalf("details of a secret-looking key: %s", b)
	}

	for _, bad := range []string{"result=maybe", "action=a%20b", "object_type=Bad%20Thing", "since=yesterday", "limit=0", "cursor=3", "until=2020-01-01"} {
		res, body := e.req("GET", "/api/v1/audit?"+bad, s, nil)
		if res.StatusCode != http.StatusBadRequest || problemCode(t, res, body) != api.CodeInvalidRequest {
			t.Errorf("%s: %d %s, want 400 invalid_request", bad, res.StatusCode, body)
		}
	}
	// The field errors name the filter and never echo it.
	_, body := e.req("GET", "/api/v1/audit?result=SECRETISH", s, nil)
	if f := problemFields(t, body); f["result"] == "" || strings.Contains(string(body), "SECRETISH") {
		t.Fatalf("field errors: %s", body)
	}
}

func TestSettingsPutChangesTheZoneEverywhereAndIsAudited(t *testing.T) {
	e := newEnv(t)
	prefs := e.withDisplay("UTC")
	s := e.login("admin", pw)
	at := time.Date(2026, 1, 15, 3, 5, 0, 0, time.UTC)
	if _, err := e.st.AppendAudit(context.Background(), store.AuditEvent{OccurredAt: at, ActorType: "system", Action: "zone.probe"}); err != nil {
		t.Fatal(err)
	}
	local := func() string {
		_, b := e.get("/api/v1/audit?action=zone.probe", s)
		r := decode[AuditListResponse](t, b)
		if len(r.Events) != 1 || !r.Events[0].OccurredAt.Equal(at) {
			t.Fatalf("the stored instant must never change: %s", b)
		}
		return r.DisplayTimezone + " | " + r.Events[0].OccurredLocal
	}
	if got := local(); got != "UTC | Thu 15 Jan 2026, 3:05:00 AM UTC" {
		t.Fatalf("before: %s", got)
	}

	before := len(e.auditOf("settings.update", "display_timezone"))
	res, b := e.putSetting(s, "display_timezone", "Asia/Tokyo")
	if res.StatusCode != 200 {
		t.Fatalf("%d %s", res.StatusCode, b)
	}
	put := decode[PutSettingResponse](t, b)
	if !put.Changed || string(put.Value) != `"Asia/Tokyo"` || !put.Stored || !put.Editable || put.UpdatedBy != "admin" {
		t.Fatalf("response: %s", b)
	}
	if got := local(); got != "Asia/Tokyo | Thu 15 Jan 2026, 12:05:00 PM JST" {
		t.Fatalf("after the change, the next API answer must use the new zone: %s", got)
	}
	if prefs.Zone() != "Asia/Tokyo" {
		t.Fatalf("the shared prefs were not updated: %s", prefs.Zone())
	}
	evs := e.auditOf("settings.update", "display_timezone")
	if len(evs) != before+1 || evs[0].ActorLabel != "admin" || evs[0].Method != "api" || evs[0].ObjectType != "setting" || evs[0].Result != "ok" {
		t.Fatalf("audit rows: %+v", evs)
	}
	if !strings.Contains(string(evs[0].Details), `"to":"Asia/Tokyo"`) || !strings.Contains(string(evs[0].Details), `"from":"UTC"`) {
		t.Fatalf("audit details: %s", evs[0].Details)
	}

	// GET /settings reports what is in effect.
	_, b = e.get("/api/v1/settings", s)
	list := decode[SettingsResponse](t, b)
	by := map[string]SettingJSON{}
	for _, st := range list.Settings {
		by[st.Key] = st
	}
	if string(by["display_timezone"].Value) != `"Asia/Tokyo"` || string(by["refresh_interval_s"].Value) != "30" || by["refresh_interval_s"].Stored ||
		by["audit_retention_days"].Editable || by["audit_retention_days"].TakesEffectIn != "M5" {
		t.Fatalf("settings: %s", b)
	}

	// The same value again is not a change: no audit row.
	n := len(e.auditOf("settings.update", "display_timezone"))
	_, b = e.putSetting(s, "display_timezone", "Asia/Tokyo")
	if decode[PutSettingResponse](t, b).Changed || len(e.auditOf("settings.update", "display_timezone")) != n {
		t.Fatalf("a no-op PUT: %s", b)
	}

	// Refused values change nothing, audit nothing, and do not echo the input.
	total := e.audits()
	for name, tc := range map[string]struct {
		key   string
		value any
		code  int
		field string
	}{
		"bad zone":       {"display_timezone", "Mars/Base", 400, "value"},
		"zone not text":  {"display_timezone", 12, 400, "value"},
		"low interval":   {"refresh_interval_s", 1, 400, "value"},
		"read-only":      {"audit_retention_days", 120, 400, "key"},
		"unknown":        {"no_such_setting", 1, 404, ""},
		"fractional":     {"refresh_interval_s", 12.5, 400, "value"},
		"huge interval":  {"refresh_interval_s", 100000, 400, "value"},
		"quoted integer": {"refresh_interval_s", "30", 400, "value"},
	} {
		res, body := e.putSetting(s, tc.key, tc.value)
		if res.StatusCode != tc.code {
			t.Errorf("%s: %d %s, want %d", name, res.StatusCode, body, tc.code)
			continue
		}
		if tc.field != "" {
			if f := problemFields(t, body); f[tc.field] == "" || strings.Contains(string(body), "Mars") {
				t.Errorf("%s: field errors %s", name, body)
			}
		}
	}
	if e.audits() != total || prefs.Zone() != "Asia/Tokyo" || prefs.RefreshS() != 30 {
		t.Fatalf("refused PUTs changed something: audits %d -> %d, zone %s, refresh %d", total, e.audits(), prefs.Zone(), prefs.RefreshS())
	}
	// An unknown field in the body is refused, not ignored.
	res, _ = e.req("PUT", "/api/v1/settings/refresh_interval_s", s, `{"value":30,"force":true}`)
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown body field: %d", res.StatusCode)
	}

	// The refresh interval applies to the shared prefs too.
	if res, b = e.putSetting(s, "refresh_interval_s", 90); res.StatusCode != 200 || prefs.RefreshS() != 90 {
		t.Fatalf("refresh: %d %s (prefs %d)", res.StatusCode, b, prefs.RefreshS())
	}
}

// A service built without shared prefs still stores and audits a change (it
// simply has no live copy to update).
func TestSettingsPutWithoutLivePrefs(t *testing.T) {
	e := newEnv(t)
	s := e.login("admin", pw)
	if res, b := e.putSetting(s, "display_timezone", "Europe/Paris"); res.StatusCode != 200 {
		t.Fatalf("%d %s", res.StatusCode, b)
	}
	_, b := e.get("/api/v1/settings", s)
	if !strings.Contains(string(b), `"value":"Europe/Paris"`) {
		t.Fatalf("stored value is reported: %s", b)
	}
	if len(e.auditOf("settings.update", "display_timezone")) != 1 {
		t.Fatal("not audited")
	}
}
