package adminapi

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// PATCH /api/v1/me with username and display_name (M3-PROFILE).
func TestAPIMeProfile(t *testing.T) {
	e := newEnv(t)
	e.addAdmin("sam", "operator", nil)
	a := e.login("admin", "pw-for-tests-1")
	me := func() MeJSON {
		t.Helper()
		r, b := e.req("GET", "/api/v1/me", a, nil)
		var m MeJSON
		if r.StatusCode != 200 || json.Unmarshal(b, &m) != nil {
			t.Fatalf("GET /me: %d %s", r.StatusCode, b)
		}
		return m
	}
	audits := func(action string) int {
		l, err := e.st.ListAudit(context.Background(), store.ScopeAll(), store.AuditFilter{Action: action}, store.Page{Limit: 500})
		if err != nil {
			t.Fatal(err)
		}
		return len(l.Items)
	}
	// The fixture owner starts with a display name: it is in GET /me.
	if m := me(); m.Username != "admin" || m.DisplayName != "Fixtures Admin" {
		t.Fatalf("start: %q / %q", m.Username, m.DisplayName)
	}

	t.Run("username and display name change together", func(t *testing.T) {
		r, b := e.req("PATCH", "/api/v1/me", a, map[string]string{"username": "admin.renamed", "display_name": "Admin Example"})
		var m MeJSON
		if r.StatusCode != 200 || json.Unmarshal(b, &m) != nil {
			t.Fatalf("PATCH: %d %s", r.StatusCode, b)
		}
		if m.Username != "admin.renamed" || m.DisplayName != "Admin Example" {
			t.Fatalf("the PATCH response shows %q / %q", m.Username, m.DisplayName)
		}
		if got := me(); got.Username != "admin.renamed" || got.DisplayName != "Admin Example" {
			t.Fatalf("GET /me shows %q / %q", got.Username, got.DisplayName)
		}
		if audits("admin.username_change") != 1 || audits("admin.display_name_change") != 1 {
			t.Fatalf("audit rows: username %d, display name %d", audits("admin.username_change"), audits("admin.display_name_change"))
		}
	})

	t.Run("an absent field is left alone, an empty display name clears it", func(t *testing.T) {
		if r, b := e.req("PATCH", "/api/v1/me", a, map[string]string{"username": "admin.again"}); r.StatusCode != 200 {
			t.Fatalf("username only: %d %s", r.StatusCode, b)
		}
		if got := me(); got.Username != "admin.again" || got.DisplayName != "Admin Example" {
			t.Fatalf("username-only PATCH gave %q / %q", got.Username, got.DisplayName)
		}
		if r, b := e.req("PATCH", "/api/v1/me", a, map[string]string{"display_name": ""}); r.StatusCode != 200 {
			t.Fatalf("clear: %d %s", r.StatusCode, b)
		}
		if got := me(); got.Username != "admin.again" || got.DisplayName != "" {
			t.Fatalf("clearing gave %q / %q", got.Username, got.DisplayName)
		}
		if _, b := e.req("GET", "/api/v1/me", a, nil); strings.Contains(string(b), "display_name") {
			t.Fatalf("a cleared display name is not omitted from /me: %s", b)
		}
		if r, b := e.req("PATCH", "/api/v1/me", a, `{"display_name":null}`); r.StatusCode != 400 {
			t.Fatalf("only a null field is nothing to change: %d %s", r.StatusCode, b)
		}
	})

	t.Run("a taken username is 409 and says so, case-insensitively", func(t *testing.T) {
		before := audits("admin.username_change")
		for _, name := range []string{"sam", "SAM"} {
			r, b := e.req("PATCH", "/api/v1/me", a, map[string]string{"username": name})
			if r.StatusCode != 409 || problemCode(t, r, b) != "conflict" {
				t.Fatalf("username %q: %d %s", name, r.StatusCode, b)
			}
			var p struct {
				Errors []store.FieldError `json:"errors"`
			}
			if err := json.Unmarshal(b, &p); err != nil || len(p.Errors) != 1 || p.Errors[0].Field != "username" || p.Errors[0].Message != "That username is taken." {
				t.Fatalf("the 409 does not say the username is taken: %s", b)
			}
		}
		if got := me(); got.Username != "admin.again" {
			t.Fatalf("a refused change altered the username: %q", got.Username)
		}
		if audits("admin.username_change") != before {
			t.Fatal("a refused change wrote an audit row")
		}
	})

	t.Run("a bad username or display name is 400", func(t *testing.T) {
		for name, body := range map[string]map[string]string{
			"empty username":    {"username": ""},
			"space in username": {"username": "two words"},
			"long username":     {"username": strings.Repeat("a", 101)},
			"newline in name":   {"display_name": "a\nb"},
			"long name":         {"display_name": strings.Repeat("n", 101)},
		} {
			r, b := e.req("PATCH", "/api/v1/me", a, body)
			if r.StatusCode != 400 || problemCode(t, r, b) != "invalid_request" {
				t.Errorf("%s: %d %s", name, r.StatusCode, b)
			}
		}
		if got := me(); got.Username != "admin.again" || got.DisplayName != "" {
			t.Fatalf("a refused change was stored: %q / %q", got.Username, got.DisplayName)
		}
	})

	t.Run("a token cannot change the profile", func(t *testing.T) {
		r, b := e.req("POST", "/api/v1/me/api-tokens", a, TokenCreateRequest{Name: "t1", Scope: "full", Days: 7})
		var tok TokenCreateResponse
		if r.StatusCode != 201 || json.Unmarshal(b, &tok) != nil || tok.Token == "" {
			t.Fatalf("create token: %d %s", r.StatusCode, b)
		}
		r, b = e.req("PATCH", "/api/v1/me", nil, map[string]string{"username": "via.token"}, "Authorization", "Bearer "+tok.Token)
		if r.StatusCode != 403 {
			t.Fatalf("PATCH /me by token: %d %s, want 403", r.StatusCode, b)
		}
		if got := me(); got.Username != "admin.again" {
			t.Fatalf("a token changed the username: %q", got.Username)
		}
	})
}
