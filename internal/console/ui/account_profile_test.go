package ui

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// shellNames checks the two places the shell names the signed-in account: the
// sidebar "who" line and the account menu label.
func shellNames(t *testing.T, body, name, role string) {
	t.Helper()
	mustContain(t, "shell", body, `<span title="`+role+`">`+name+` · `+role+`</span>`, `<span class="um-name">`+name+`</span>`)
}

// /ui/account/profile (M3-PROFILE): the username and display name from the
// /account page, and the shell showing the display name when one is set.
func TestAccountProfileFormAndShell(t *testing.T) {
	e := newEnv(t)
	e.addAccount("pat", "operator")
	e.loginAs("pat", testPassword)

	// The page: a profile form with the current values and the sign-in hints.
	res, body := e.do("GET", "/account", nil)
	if res.StatusCode != 200 {
		t.Fatalf("GET /account: %d", res.StatusCode)
	}
	mustContain(t, "account", body, `action="/ui/account/profile"`, `name="username" type="text" value="pat" autocomplete="username" autocapitalize="none" spellcheck="false" required maxlength="100"`,
		`name="display_name" type="text" value=""`, `Save profile`, "Letters, digits, dot, dash, underscore and @. You sign in with this; your current sessions stay signed in.",
		"Shown in the console instead of your username. Leave empty to show the username.", `action="/ui/account/email"`, "Customers")
	shellNames(t, body, "pat", "operator")
	if strings.Contains(body, "<dt>Username</dt>") {
		t.Error("the username is still listed as a read-only row next to its form")
	}

	// Save: a new username and a display name, with markup that must be escaped.
	display := `Pat <b>K</b> & Co`
	res, _ = e.post("/ui/account/profile", url.Values{"username": {"pat.k"}, "display_name": {display}}, false)
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/account?done=profile" {
		t.Fatalf("save: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	if e.audits("admin.username_change") != 1 || e.audits("admin.display_name_change") != 1 {
		t.Fatalf("audit rows: username %d, display name %d", e.audits("admin.username_change"), e.audits("admin.display_name_change"))
	}
	res, body = e.do("GET", "/account?done=profile", nil)
	if res.StatusCode != 200 {
		t.Fatalf("session after the change: %d (a username change must not end the session)", res.StatusCode)
	}
	escaped := `Pat &lt;b&gt;K&lt;/b&gt; &amp; Co`
	mustContain(t, "account after save", body, "Profile saved.", `<h2>`+escaped+`</h2>`, `<div class="sub"><span>pat.k</span>`, `name="username" type="text" value="pat.k"`)
	if strings.Contains(body, "<b>K</b>") {
		t.Error("the display name reached the page unescaped")
	}
	// The shell, on the board and on /account.
	res, board := e.do("GET", "/", nil)
	if res.StatusCode != 200 {
		t.Fatalf("board: %d", res.StatusCode)
	}
	shellNames(t, board, escaped, "operator")
	if strings.Contains(board, `<span title="operator">pat.k`) {
		t.Error("the board shell still shows the username")
	}
	// The labels that name the account for assistive tech keep the username.
	mustContain(t, "board", board, `aria-label="Sign out pat.k"`, `aria-label="Account menu for pat.k"`)

	// Clearing the display name brings the username back.
	res, _ = e.post("/ui/account/profile", url.Values{"username": {"pat.k"}, "display_name": {"  "}}, false)
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("clear: %d", res.StatusCode)
	}
	_, body = e.do("GET", "/account", nil)
	shellNames(t, body, "pat.k", "operator")
	mustContain(t, "account after clear", body, `<h2>pat.k</h2>`)
	if strings.Contains(body, `<div class="sub"><span>pat.k</span>`) {
		t.Error("the username is repeated in the sub line when no display name is set")
	}

	// Sign-in follows the username: the old name fails, the new one works (any case).
	// The test's MFA helper keys authenticator secrets by the name typed at sign-in.
	e.mfa.mu.Lock()
	e.mfa.secrets["PAT.K"], e.mfa.codes["PAT.K"] = e.mfa.secrets["pat"], e.mfa.codes["pat"]
	e.mfa.mu.Unlock()
	e.sess, e.csrf = nil, ""
	if res, _ := e.do("POST", "/login", url.Values{"username": {"pat"}, "password": {testPassword}}); res.StatusCode == http.StatusSeeOther {
		t.Fatal("the old username still signs in")
	}
	e.loginAs("PAT.K", testPassword)
}

// A refused post re-renders /account with the reason and what was typed, and
// changes nothing.
func TestAccountProfileErrorsKeepWhatWasTyped(t *testing.T) {
	e := newEnv(t)
	e.addAccount("pat", "operator")
	e.loginAs("pat", testPassword)

	cases := []struct {
		name, username, display string
		status                  int
		want                    string
	}{
		{"a username with a space", "pat k", "Typed Name", 400, "A username is 1 to 100 letters, digits, dots, dashes, underscores or @"},
		{"an empty username", "", "Typed Name", 400, "Profile not saved."},
		{"a display name with a line break", "pat.ok", "Two\nLines", 400, "A display name is at most 100 characters, with no line breaks"},
		{"a display name over 100 characters", "pat.ok", strings.Repeat("n", 101), 400, "A display name is at most 100 characters"},
		{"a taken username", "admin", "Typed Name", 409, "That username is taken."},
		{"a taken username in other case", "ADMIN", "Typed Name", 409, "That username is taken."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, body := e.post("/ui/account/profile", url.Values{"username": {c.username}, "display_name": {c.display}}, false)
			if res.StatusCode != c.status {
				t.Fatalf("status %d, want %d", res.StatusCode, c.status)
			}
			mustContain(t, c.name, body, c.want, `action="/ui/account/profile"`)
			// The inputs hold what was typed (the template escapes it; compare the escaped form).
			if c.username != "" {
				mustContain(t, c.name, body, `name="username" type="text" value="`+c.username+`"`)
			}
			if !strings.Contains(c.display, "\n") && len(c.display) <= 100 {
				mustContain(t, c.name, body, `name="display_name" type="text" value="`+c.display+`"`)
			}
			// The head and the shell still show the stored account.
			shellNames(t, body, "pat", "operator")
		})
	}
	if e.audits("admin.username_change")+e.audits("admin.display_name_change") != 0 {
		t.Fatal("a refused post wrote an audit row")
	}
	a, err := e.st.GetAdminByUsername(t.Context(), "pat")
	if err != nil || a.DisplayName != "" {
		t.Fatalf("a refused post changed the account: %v %q", err, a.DisplayName)
	}
}

// The profile post is a SELF form like the others: any signed-in role may use
// it for its own account, a post needs the CSRF token and a same-origin
// request, and nobody signed in is let through.
func TestAccountProfileCSRFAndSession(t *testing.T) {
	e := newEnv(t)
	e.addAccount("viewer", "read_only")
	e.loginAs("viewer", testPassword)
	form := url.Values{"username": {"viewer"}, "display_name": {"Viewer One"}}

	if res, _ := e.do("POST", "/ui/account/profile", form, "Origin", e.srv.URL); res.StatusCode != 403 {
		t.Errorf("POST without a CSRF token: %d, want 403", res.StatusCode)
	}
	withToken := url.Values{"username": {"viewer"}, "display_name": {"Viewer One"}, "csrf_token": {e.csrf}}
	if res, _ := e.do("POST", "/ui/account/profile", withToken, "Origin", "https://evil.example"); res.StatusCode != 403 {
		t.Errorf("POST from another origin: %d, want 403", res.StatusCode)
	}
	if e.audits("admin.display_name_change") != 0 {
		t.Fatal("a refused post changed the account")
	}
	// The lowest role may change its own profile.
	if res, _ := e.post("/ui/account/profile", form, false); res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/account?done=profile" {
		t.Fatalf("read-only own profile: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	// No session: sent to sign in, nothing changed.
	e.sess = nil
	if res, _ := e.do("POST", "/ui/account/profile", url.Values{"username": {"viewer"}, "display_name": {"Anon"}, "csrf_token": {"x"}}, "Origin", e.srv.URL); res.StatusCode == http.StatusOK ||
		strings.HasPrefix(res.Header.Get("Location"), "/account") {
		t.Errorf("POST with no session: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	a, _ := e.st.GetAdminByUsername(t.Context(), "viewer")
	if a.DisplayName != "Viewer One" {
		t.Fatalf("display name %q after the unauthenticated post", a.DisplayName)
	}
}
