package ui

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/console/adminapi"
	"github.com/c2tech-sys/cryobackup/internal/console/auth"
)

// The UI and the JSON API share one DisplayPrefs per process (cli.Build wires
// it): a zone saved through either is used by the other on its next request.
func TestW3ADisplayZoneIsSharedBetweenTheUIAndTheAPI(t *testing.T) {
	e := newEnv(t)
	e.login()
	api := &adminapi.Service{Auth: e.svc.Auth, Store: e.st, Fleet: e.svc.Fleet, Clock: e.svc.Clock, Display: e.svc.Display}
	mux := http.NewServeMux()
	api.Register(muxReg{mux})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	call := func(method, path, body string) (int, string) {
		t.Helper()
		r, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		for _, c := range e.sess {
			r.AddCookie(c)
		}
		if method != "GET" {
			r.Header.Set("Origin", srv.URL)
			r.Header.Set(auth.CSRFHeader, e.csrf)
		}
		res, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b)
	}

	mustContain(t, "ui before", e.page("/devices", 200), `data-zone="America/Chicago"`)
	if st, b := call("GET", "/api/v1/audit?limit=1", ""); st != 200 || !strings.Contains(b, `"display_timezone":"America/Chicago"`) {
		t.Fatalf("api before: %d %s", st, b)
	}

	// API -> UI.
	if st, b := call("PUT", "/api/v1/settings/display_timezone", `{"value":"Europe/Berlin"}`); st != 200 {
		t.Fatalf("PUT: %d %s", st, b)
	}
	mustContain(t, "ui after the API change", e.page("/devices", 200), `data-zone="Europe/Berlin"`, "(Europe/Berlin)")
	mustContain(t, "settings page", e.page("/settings", 200), `value="Europe/Berlin"`)

	// UI -> API.
	res, _ := e.post("/ui/settings", map[string][]string{"display_timezone": {"Pacific/Auckland"}, "refresh_interval_s": {"45"}}, false)
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("UI save: %d", res.StatusCode)
	}
	if st, b := call("GET", "/api/v1/audit?limit=1", ""); st != 200 || !strings.Contains(b, `"display_timezone":"Pacific/Auckland"`) {
		t.Fatalf("api after the UI change: %d %s", st, b)
	}
	if st, b := call("GET", "/api/v1/settings", ""); st != 200 || !strings.Contains(b, `"value":"Pacific/Auckland"`) || !strings.Contains(b, `"key":"refresh_interval_s","label":"Refresh interval","value":45`) {
		t.Fatalf("api settings: %d %s", st, b)
	}
	mustContain(t, "ui refresh", e.page("/devices", 200), `data-refresh="45"`)
}
