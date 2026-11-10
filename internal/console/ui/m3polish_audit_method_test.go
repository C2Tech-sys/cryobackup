package ui

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/console/auth"
)

// M3-POLISH-A: a retire or a token delete made from a console page goes to the
// admin API in process and must audit as "web", the same as the page's other
// actions. The same call made to the admin API without the page's marker is "api".

// directAPI sends one write to the private admin API handler as a network client
// would: the session cookie, the CSRF header and the origin, but not through
// apiSend, so without the in-process marker.
func (e *env) directAPI(method, path string, body string) int {
	e.t.Helper()
	r := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	r.Host = strings.TrimPrefix(e.srv.URL, "http://")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json")
	r.Header.Set("Origin", e.srv.URL)
	r.Header.Set(auth.CSRFHeader, e.csrf)
	for _, c := range e.sess {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	e.svc.pageAPI().ServeHTTP(w, r)
	return w.Code
}

func (e *env) methodOf(action, id string) string {
	e.t.Helper()
	rows := e.auditRows(action, id)
	if len(rows) != 1 {
		e.t.Fatalf("%s %s: %d audit rows, want 1", action, id, len(rows))
	}
	return rows[0].Method
}

func TestM3PolishA_RetireAndTokenDeleteFromThePageAuditAsWeb(t *testing.T) {
	e := newEnv(t)
	e.login()
	summit := e.customerID("Summit Eye Care")

	id := e.dev["FRONTDESK-01"]
	r := e.postForm("/ui/devices/"+id+"/retire", url.Values{"reason": {"dead lab machine"}, "confirm": {"FRONTDESK-01"}})
	if !strings.Contains(e.location(r), "cmd=retired_ok") {
		t.Fatalf("retire: %q", e.location(r))
	}
	tok := e.t6Token(summit, "page delete", true)
	r = e.postForm("/ui/enroll/tokens/"+tok+"/delete", url.Values{})
	if !strings.Contains(e.location(r), "saved=deleted") {
		t.Fatalf("token delete: %q", e.location(r))
	}

	for _, c := range []struct{ action, id string }{
		{"device.retire", id}, {"device.retire_sync", id}, {"enrollment_token.delete", tok},
	} {
		if m := e.methodOf(c.action, c.id); m != "web" {
			t.Errorf("%s from the page audits Method %q, want web", c.action, m)
		}
	}
}

func TestM3PolishA_TheSameCallsToTheAdminAPIWithoutThePageMarkerAuditAsAPI(t *testing.T) {
	e := newEnv(t)
	e.login()
	summit := e.customerID("Summit Eye Care")

	id := e.dev["FRONTDESK-02"]
	if c := e.directAPI("POST", "/api/v1/devices/"+id+"/retire", `{"reason":"dead lab machine"}`); c != http.StatusOK {
		t.Fatalf("API retire: %d", c)
	}
	tok := e.t6Token(summit, "api delete", true)
	if c := e.directAPI("POST", "/api/v1/enrollment-tokens/"+tok+"/delete", ``); c != http.StatusNoContent {
		t.Fatalf("API token delete: %d", c)
	}

	for _, c := range []struct{ action, id string }{
		{"device.retire", id}, {"device.retire_sync", id}, {"enrollment_token.delete", tok},
	} {
		if m := e.methodOf(c.action, c.id); m != "api" {
			t.Errorf("%s from the API audits Method %q, want api", c.action, m)
		}
	}
}
