package ui

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"html"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/enroll"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
)

const wizardURL = "https://backup.example.test:8443"

var shownToken = regexp.MustCompile(`aria-label="Enrollment token, shown once">([A-Za-z0-9_-]{43})</pre>`)
var tokenShaped = regexp.MustCompile(`[A-Za-z0-9_-]{43}`)
var csrfField = regexp.MustCompile(`name="csrf_token" value="[^"]*"`)
var preBlock = regexp.MustCompile(`(?s)<pre class="code"[^>]*aria-label="([^"]*)">(.*?)</pre>`)

// wizardEnv is the fixtures env with the generator configured, signed in as the
// fixtures admin, with a log buffer to search for secrets.
func wizardEnv(t *testing.T) (*env, *bytes.Buffer) {
	t.Helper()
	e := newEnv(t)
	var logs bytes.Buffer
	e.svc.Log = logx.New(&logs, logx.LevelDebug)
	e.svc.PublicURL = wizardURL
	e.svc.ArtifactsDir = t.TempDir()
	e.login()
	return e, &logs
}

func (e *env) createToken(customerID string, extra url.Values) (*http.Response, string) {
	e.t.Helper()
	form := url.Values{"customer_id": {customerID}, "label": {"Front office"}, "policy_id": {""}, "device_class": {"unknown"}, "expires": {"7d"}, "max_uses": {"10"}}
	for k, v := range extra {
		form[k] = v
	}
	return e.post("/ui/enroll/token", form, false)
}

// enrollAudits counts the token audit rows (logins write audit rows too, so the total is no use here).
func (e *env) enrollAudits() int {
	e.t.Helper()
	return len(e.auditRows("enrollment_token.create", "")) + len(e.auditRows("enrollment_token.revoke", ""))
}

func (e *env) tokenCount() int {
	e.t.Helper()
	l, err := e.st.ListEnrollmentTokenRows(context.Background(), store.ScopeAll(), store.EnrollTokenFilter{Size: 200})
	if err != nil {
		e.t.Fatal(err)
	}
	return l.All
}

func (e *env) tokenIDOf(secret string) string {
	e.t.Helper()
	tok, err := e.st.GetEnrollmentTokenByHash(context.Background(), enroll.HashToken(secret))
	if err != nil {
		e.t.Fatalf("the shown token is not in the store: %v", err)
	}
	return tok.ID
}

func TestEnrollPagesRenderInFixturesMode(t *testing.T) {
	e, _ := wizardEnv(t)
	summit := e.customerID("Summit Eye Care")
	tokID := ""
	if l, err := e.st.ListEnrollmentTokens(context.Background(), store.ScopeAll(), summit); err != nil || len(l) == 0 {
		t.Fatalf("fixture tokens: %v", err)
	} else {
		tokID = l[0].ID
	}
	for name, path := range map[string]string{
		"enroll":            "/enroll",
		"enroll customer":   "/enroll?customer=" + summit,
		"enroll status":     "/enroll?status=active&size=25",
		"enroll empty":      "/enroll?status=revoked",
		"enroll bad status": "/enroll?status=bogus",
		"enroll paged":      "/enroll?page=7",
		"token":             "/enroll/tokens/" + tokID,
		"downloads":         "/downloads",
	} {
		want := 200
		body := e.page(path, want)
		if strings.Contains(body, "ZgotmplZ") {
			t.Errorf("%s: html/template rejected a value", name)
		}
		if absoluteURL.MatchString(body) {
			t.Errorf("%s: an absolute URL reference (no CDN, no third party)", name)
		}
		if m := pathLike.FindString(body); m != "" {
			t.Errorf("%s: shows a path-like string %q", name, m)
		}
		if !strings.Contains(body, `id="freshness"`) {
			t.Errorf("%s: no freshness stamp", name)
		}
		mustContain(t, name, body, `href="/enroll"`, `href="/downloads"`) // the admin sees both nav items
	}
	body := e.page("/enroll", 200)
	mustContain(t, "enroll", body, "Enroll devices", "New enrollment", `href="/enroll/new"`, "summit-eye-care-2026q4", "ACTIVE", "Summit Eye Care", `name="csrf_token"`, "Showing 1-6 of 6")
	mustContain(t, "wizard", e.page("/enroll/new", 200), "New enrollment token", `name="customer_id"`, `name="new_customer"`, `name="expires"`, `name="max_uses"`, `name="device_class"`, `name="policy_id"`,
		"Applied to each device that enrolls with this token, on top of the customer's policy", `action="/ui/enroll/token"`, `name="csrf_token"`)
	mustContain(t, "enroll empty", e.page("/enroll?status=revoked", 200), "No tokens match these filters", "0 of 6 tokens match")
	mustContain(t, "enroll bad status", e.page("/enroll?status=bogus", 200), "Unknown token status")
	mustContain(t, "token detail", e.page("/enroll/tokens/"+tokID, 200), "summit-eye-care-2026q4", "cannot be shown again", "No device has enrolled yet", "Revoke this token", "Usage", "Customer&#39;s policy")
	if res, _ := e.do("GET", "/enroll/tokens/01J9Y4K3A0B1C2D3E4F5G6H7AA", nil); res.StatusCode != 404 {
		t.Errorf("unknown token: %d, want 404", res.StatusCode)
	}
	// Nothing token-shaped is in a page that is not the create response.
	for _, p := range []string{"/enroll", "/enroll/tokens/" + tokID, "/downloads"} {
		if m := tokenShaped.FindString(csrfField.ReplaceAllString(e.page(p, 200), "")); m != "" {
			t.Errorf("%s holds a 43-character token-shaped string %q", p, m)
		}
	}
}

func TestDownloadsShowsTheInstallerAndItsHash(t *testing.T) {
	e, _ := wizardEnv(t)
	none := e.page("/downloads", 200)
	mustContain(t, "no installer", none, "No agent installer found", "Where to get it", "make msi", "does not serve installers")
	e.svc.ArtifactsDir = ""
	mustContain(t, "no dir", e.page("/downloads", 200), "No artifacts directory is configured")
	e.svc.ArtifactsDir = t.TempDir()
	body := []byte("installer bytes")
	if err := os.WriteFile(filepath.Join(e.svc.ArtifactsDir, "cryobackup-agent-1.4.2.msi"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	page := e.page("/downloads", 200)
	mustContain(t, "installer", page, "cryobackup-agent-1.4.2.msi", "1.4.2", hex.EncodeToString(sum[:]), "READY", "Get-FileHash")
	if strings.Contains(page, e.svc.ArtifactsDir) {
		t.Error("the downloads page shows the artifacts directory path")
	}
	// An unversioned file says so rather than inventing a version.
	only := t.TempDir()
	if err := os.WriteFile(filepath.Join(only, "cryobackup-agent.msi"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	e.svc.ArtifactsDir = only
	mustContain(t, "unversioned", e.page("/downloads", 200), "UNKNOWN", "cryobackup-agent-X.Y.Z.msi")
	// An unreadable directory is an alarm, not "none found".
	f := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.svc.ArtifactsDir = f
	mustContain(t, "unreadable", e.page("/downloads", 200), "Installer could not be read")
}

func TestCreatedTokenIsShownOnceAndNothingElseHasIt(t *testing.T) {
	e, logs := wizardEnv(t)
	body := []byte("msi bytes for the hash")
	if err := os.WriteFile(filepath.Join(e.svc.ArtifactsDir, "cryobackup-agent-2.0.0.msi"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	summit := e.customerID("Summit Eye Care")
	tpl := e.newTemplate("Slow first backup", map[string]string{"sla": `{"first_backup_grace_hours":200}`})
	auditsBefore := e.auditTotal()
	tokensBefore := e.tokenCount()
	res, page := e.createToken(summit, url.Values{"max_uses": {"5"}, "expires": {"3d"}, "device_class": {"laptop"}, "label": {"Front office"}, "policy_id": {tpl.ID}})
	if res.StatusCode != 200 {
		t.Fatalf("create: %d %s", res.StatusCode, page[:min(len(page), 400)])
	}
	if cc := res.Header.Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("Cache-Control = %q: the one page with the secret must be no-store", cc)
	}
	m := shownToken.FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("the create response does not show the token:\n%s", page[:min(len(page), 600)])
	}
	secret := m[1]
	if strings.Count(page, secret) < 3 {
		t.Errorf("the token should be in the page and in the generated RMM, msiexec and GPO blocks (found %d times)", strings.Count(page, secret))
	}
	id := e.tokenIDOf(secret)
	mustContain(t, "created", page, "Token created for Summit Eye Care", "Copy it now", "RMM deployment", "One machine: msiexec", "Group Policy startup script", "local administrator",
		"Front office", "enrolls up to 5 devices", "Slow first backup", "Applied to each device that enrolls with this token, on top of the customer&#39;s policy. It shows on the device&#39;s Policy tab as an override.", "cryobackup-agent-2.0.0.msi", hex.EncodeToString(sum[:]), "id=\"tok-h\"")
	mustContain(t, "detail of a token with a policy", e.page("/enroll/tokens/"+id, 200), "Slow first backup", "applied to each device at enrollment, on top of the customer")

	// The generated blocks: the configured URL, the MSI hash, the token only where it belongs.
	blocks := map[string]string{}
	for _, b := range preBlock.FindAllStringSubmatch(page, -1) {
		blocks[b[1]] = html.UnescapeString(b[2])
	}
	rmm, msi, gpo := blocks["RMM script variables, 4 lines"], blocks["msiexec command"], blocks["Group Policy startup script"]
	if rmm == "" || msi == "" || gpo == "" {
		t.Fatalf("missing blocks: %v", blocks)
	}
	for name, b := range map[string]string{"rmm": rmm, "msiexec": msi, "gpo": gpo} {
		if !strings.Contains(b, wizardURL) {
			t.Errorf("%s block lacks the configured public URL:\n%s", name, b)
		}
	}
	if !strings.Contains(rmm, "'"+hex.EncodeToString(sum[:])+"'") {
		t.Errorf("rmm block lacks the MSI hash:\n%s", rmm)
	}
	for _, line := range strings.Split(rmm, "\n") {
		if strings.Contains(line, secret) && !strings.HasPrefix(line, "$EnrollToken = '"+secret+"'") {
			t.Errorf("the token is on a line of the RMM block other than its secret variable: %q", line)
		}
	}
	for _, banned := range []string{"msiexec", "TOKEN=", "/qn", "Start-Process", "-ArgumentList"} {
		if strings.Contains(rmm, banned) {
			t.Errorf("the RMM block contains %q: the token would reach a command line", banned)
		}
	}
	if !strings.Contains(msi, `TOKEN="`+secret+`"`) || !strings.Contains(msi, `SERVER="`+wizardURL+`"`) {
		t.Errorf("msiexec block = %s", msi)
	}

	// Second look at every page that could carry it: nothing.
	hash := enroll.HashToken(secret)
	for _, p := range []string{"/enroll", "/enroll?customer=" + summit, "/enroll/tokens/" + id, "/downloads", "/", "/customers/" + summit} {
		got := e.page(p, 200)
		if strings.Contains(got, secret) || strings.Contains(got, hash) {
			t.Errorf("GET %s shows the token or its hash after creation", p)
		}
	}
	list := e.page("/enroll", 200)
	mustContain(t, "list", list, "Front office", "0 of 5 used", "5 left", "ACTIVE")

	// One audit row, and no secret in it, in the log or in the store beyond its hash.
	if got := e.auditTotal() - auditsBefore; got != 1 {
		t.Errorf("%d audit rows for one create, want 1", got)
	}
	rows := e.auditRows("enrollment_token.create", id)
	if len(rows) != 1 || rows[0].Method != "web" || rows[0].ActorLabel != "admin" || rows[0].CustomerID != summit || rows[0].Result != "ok" {
		t.Fatalf("audit rows = %+v", rows)
	}
	all := string(rows[0].Details) + rows[0].ObjectID + rows[0].ActorLabel
	for _, bad := range []string{secret, hash, "Front office"} {
		if strings.Contains(all, bad) {
			t.Errorf("the audit row holds %q: %s", bad, all)
		}
	}
	if strings.Contains(logs.String(), secret) || strings.Contains(logs.String(), hash) {
		t.Errorf("the log holds the token or its hash:\n%s", logs.String())
	}
	if e.tokenCount() != tokensBefore+1 {
		t.Error("exactly one token should have been created")
	}
	tok, _ := e.st.GetEnrollmentTokenByHash(context.Background(), hash)
	if tok.TokenHash != hash || tok.DeviceClassDefault != "laptop" || tok.MaxUses == nil || *tok.MaxUses != 5 {
		t.Errorf("stored token = %+v", tok)
	}
	if d := tok.ExpiresAt.Sub(e.now); d != 72*time.Hour {
		t.Errorf("expires in %v, want 72h", d)
	}
}

func TestWizardRejectsBadInputWithoutCreatingAnything(t *testing.T) {
	e, _ := wizardEnv(t)
	summit := e.customerID("Summit Eye Care")
	tokens, audits := e.tokenCount(), e.auditTotal()
	for name, tc := range map[string]struct {
		cust  string
		extra url.Values
		want  string
	}{
		"no customer":     {"", nil, "Choose a customer, or name a new one."},
		"both":            {summit, url.Values{"new_customer": {"Also New"}}, "not both"},
		"bad expiry":      {summit, url.Values{"expires": {"400d"}}, "Choose one of the listed lifetimes."},
		"bad uses":        {summit, url.Values{"max_uses": {"0"}}, "Choose one of the listed limits."},
		"bad class":       {summit, url.Values{"device_class": {"server"}}, "Choose desktop, laptop or not set."},
		"long label":      {summit, url.Values{"label": {strings.Repeat("x", 101)}}, "at most 100 characters"},
		"unknown policy":  {summit, url.Values{"policy_id": {"01J9Y4K3A0B1C2D3E4F5G6H7AA"}}, "Choose one of the listed policies."},
		"unknown company": {"01J9Y4K3A0B1C2D3E4F5G6H7AA", nil, ""},
	} {
		res, page := e.createToken(tc.cust, tc.extra)
		if name == "unknown company" {
			if res.StatusCode != 404 {
				t.Errorf("%s: %d, want 404", name, res.StatusCode)
			}
			continue
		}
		if res.StatusCode != 400 {
			t.Errorf("%s: %d, want 400", name, res.StatusCode)
			continue
		}
		mustContain(t, name, page, "The token was not created.", tc.want, `role="alert"`)
		if shownToken.MatchString(page) {
			t.Errorf("%s: a rejected form showed a token", name)
		}
	}
	// A rejected post keeps what was typed.
	_, page := e.createToken(summit, url.Values{"label": {"Kept label"}, "max_uses": {"0"}})
	mustContain(t, "kept", page, `value="Kept label"`)
	if e.tokenCount() != tokens || e.auditTotal() != audits {
		t.Errorf("rejected posts changed state: tokens %d->%d, audits %d->%d", tokens, e.tokenCount(), audits, e.auditTotal())
	}
}

func TestWizardCreatesANewCustomerAndKeepsItOnRetry(t *testing.T) {
	e, _ := wizardEnv(t)
	audits, tokens := e.auditTotal(), e.tokenCount()
	res, page := e.createToken("", url.Values{"new_customer": {"Wizard Dental"}, "expires": {"14d"}})
	if res.StatusCode != 200 || !shownToken.MatchString(page) {
		t.Fatalf("create with a new customer: %d", res.StatusCode)
	}
	mustContain(t, "new customer", page, "NEW CUSTOMER", "Token created for Wizard Dental")
	cid := e.customerID("Wizard Dental")
	row, err := e.st.GetCustomerRow(context.Background(), store.ScopeAll(), cid)
	if err != nil || !row.Expected || row.OnboardingUntil == nil || row.OnboardingUntil.Sub(e.now) != 14*24*time.Hour {
		t.Fatalf("customer = %+v, %v: want expected, onboarding until the token expires", row, err)
	}
	if got := e.auditTotal() - audits; got != 2 {
		t.Errorf("%d audit rows, want 2 (customer.create and enrollment_token.create)", got)
	}
	if len(e.auditRows("customer.create", cid)) != 1 {
		t.Error("no customer.create audit row")
	}
	if e.tokenCount() != tokens+1 {
		t.Error("one token expected")
	}
	// A duplicate name is a field error, creates nothing.
	tokens = e.tokenCount()
	res, page = e.createToken("", url.Values{"new_customer": {"wizard dental"}})
	if res.StatusCode != 400 || e.tokenCount() != tokens {
		t.Errorf("duplicate customer: %d, tokens %d->%d", res.StatusCode, tokens, e.tokenCount())
	}
	mustContain(t, "duplicate", page, "The token was not created.")
	// Customer created, token refused (unknown policy): a retry must not create the customer again.
	res, page = e.createToken("", url.Values{"new_customer": {"Retry Clinic"}, "policy_id": {"01J9Y4K3A0B1C2D3E4F5G6H7AA"}})
	if res.StatusCode != 400 {
		t.Fatalf("token refused after the customer was created: %d", res.StatusCode)
	}
	retry := e.customerID("Retry Clinic")
	mustContain(t, "retry", page, `value="`+retry+`" selected`)
	if res, page = e.createToken(retry, nil); res.StatusCode != 200 || !shownToken.MatchString(page) {
		t.Fatalf("retry with the created customer: %d", res.StatusCode)
	}
	if got := len(e.auditRows("customer.create", retry)); got != 1 {
		t.Errorf("%d customer.create rows for Retry Clinic, want 1", got)
	}
}

func TestRevokeFromTheListAndTheDetailPage(t *testing.T) {
	e, _ := wizardEnv(t)
	summit := e.customerID("Summit Eye Care")
	_, page := e.createToken(summit, nil)
	secret := shownToken.FindStringSubmatch(page)[1]
	id := e.tokenIDOf(secret)
	audits := e.auditTotal()
	res, _ := e.post("/ui/enroll/tokens/"+id+"/revoke", url.Values{"return": {"list"}}, false)
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/enroll?saved=revoked" {
		t.Fatalf("revoke from the list: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	mustContain(t, "after revoke", e.page("/enroll?saved=revoked", 200), "Token revoked.", "REVOKED")
	if got := e.auditTotal() - audits; got != 1 {
		t.Fatalf("%d audit rows for a revoke, want 1", got)
	}
	if rows := e.auditRows("enrollment_token.revoke", id); len(rows) != 1 || rows[0].CustomerID != summit {
		t.Fatalf("revoke audit rows = %+v", rows)
	}
	res, _ = e.post("/ui/enroll/tokens/"+id+"/revoke", url.Values{}, false)
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/enroll/tokens/"+id+"?saved=already_revoked" {
		t.Fatalf("second revoke: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	if got := e.auditTotal() - audits; got != 1 {
		t.Fatalf("a repeated revoke wrote an audit row (%d new rows)", got)
	}
	detail := e.page("/enroll/tokens/"+id+"?saved=already_revoked", 200)
	mustContain(t, "revoked detail", detail, "REVOKED", "already revoked")
	if strings.Contains(detail, "Revoke this token") {
		t.Error("a revoked token still offers Revoke")
	}
	mustContain(t, "revoked filter", e.page("/enroll?status=revoked", 200), "Front office", "REVOKED")
	if res, _ := e.post("/ui/enroll/tokens/01J9Y4K3A0B1C2D3E4F5G6H7AA/revoke", url.Values{}, false); res.StatusCode != 404 {
		t.Errorf("revoke of an unknown token: %d", res.StatusCode)
	}
}

func TestTokenDetailListsTheDevicesThatEnrolled(t *testing.T) {
	e, _ := wizardEnv(t)
	summit := e.customerID("Summit Eye Care")
	toks, _ := e.st.ListEnrollmentTokens(context.Background(), store.ScopeAll(), summit)
	dev := e.dev["FRONTDESK-01"]
	if err := e.st.Write(context.Background(), func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE devices SET enrollment_token_id = ? WHERE id = ?`, toks[0].ID, dev)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	page := e.page("/enroll/tokens/"+toks[0].ID, 200)
	mustContain(t, "detail", page, "FRONTDESK-01", `href="/devices/`+dev+`"`, "Devices enrolled with this token")
	mustContain(t, "list last used", e.page("/enroll", 200), "summit-eye-care-2026q4")
	if strings.Contains(e.page("/enroll/tokens/"+toks[0].ID, 200), "No device has enrolled yet") {
		t.Error("a used token says no device enrolled")
	}
}

func TestPublicURLMissingFailsLoudly(t *testing.T) {
	e, _ := wizardEnv(t)
	e.svc.PublicURL = ""
	mustContain(t, "enroll page", e.page("/enroll", 200), "Public URL not set.", "public_url")
	mustContain(t, "downloads page", e.page("/downloads", 200), "Public URL not set.")
	_, page := e.createToken(e.customerID("Summit Eye Care"), nil)
	if !shownToken.MatchString(page) {
		t.Fatal("the token is still created and shown")
	}
	mustContain(t, "created without a URL", page, "Public URL not set.", "&lt;set public_url in the console configuration&gt;")
	if strings.Contains(page, "https://backup") {
		t.Error("the snippets invented a URL")
	}
	e.svc.PublicURL = "http://10.0.0.5:8443"
	_, page = e.createToken(e.customerID("Summit Eye Care"), nil)
	mustContain(t, "plain http", page, "plain http", "InsecureLab", "CRYO_INSECURE_LAB=1")
}

func TestEnrollRoutesAreAdminOnlyAndCSRFChecked(t *testing.T) {
	e, _ := wizardEnv(t)
	summit := e.customerID("Summit Eye Care")
	toks, _ := e.st.ListEnrollmentTokens(context.Background(), store.ScopeAll(), summit)
	tokID := toks[0].ID
	roles := EnrollUIRoles()
	for _, p := range enrollUIPatterns() {
		if roles[p] != "AD" {
			t.Errorf("%s is not listed as AD", p)
		}
	}
	// Anonymous: sent to sign in, nothing changed.
	e.sess, e.csrf = nil, ""
	for _, path := range []string{"/enroll", "/enroll/tokens/" + tokID, "/downloads"} {
		if res, _ := e.do("GET", path, nil); res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/login" {
			t.Errorf("anonymous GET %s: %d %s", path, res.StatusCode, res.Header.Get("Location"))
		}
	}
	tokens, audits := e.tokenCount(), e.enrollAudits()
	for _, role := range []string{"read_only", "operator"} {
		e.signInAs("u-"+role, role, nil)
		for _, path := range []string{"/enroll", "/enroll?customer=" + summit, "/enroll/tokens/" + tokID, "/downloads"} {
			if res, _ := e.do("GET", path, nil); res.StatusCode != http.StatusForbidden {
				t.Errorf("%s GET %s: %d, want 403", role, path, res.StatusCode)
			}
		}
		if res, _ := e.createToken(summit, nil); res.StatusCode != http.StatusForbidden {
			t.Errorf("%s create: %d, want 403", role, res.StatusCode)
		}
		if res, _ := e.post("/ui/enroll/tokens/"+tokID+"/revoke", url.Values{}, false); res.StatusCode != http.StatusForbidden {
			t.Errorf("%s revoke: %d, want 403", role, res.StatusCode)
		}
		if body := e.page("/", 200); strings.Contains(body, `href="/enroll"`) || strings.Contains(body, `href="/downloads"`) {
			t.Errorf("%s sees the Enroll or Downloads navigation", role)
		}
	}
	if e.tokenCount() != tokens || e.enrollAudits() != audits {
		t.Error("a forbidden call changed state")
	}
	// An admin without the CSRF token cannot create or revoke.
	e.signInAs("ad", "admin", nil)
	form := url.Values{"customer_id": {summit}, "expires": {"7d"}, "max_uses": {"10"}, "device_class": {"unknown"}}
	if res, _ := e.do("POST", "/ui/enroll/token", form, "Origin", e.srv.URL); res.StatusCode != http.StatusForbidden {
		t.Errorf("create without the CSRF token: %d, want 403", res.StatusCode)
	}
	if res, _ := e.do("POST", "/ui/enroll/tokens/"+tokID+"/revoke", url.Values{}, "Origin", e.srv.URL); res.StatusCode != http.StatusForbidden {
		t.Errorf("revoke without the CSRF token: %d, want 403", res.StatusCode)
	}
	if e.tokenCount() != tokens || e.enrollAudits() != audits {
		t.Error("a CSRF failure changed state")
	}
	if body := e.page("/", 200); !strings.Contains(body, `href="/enroll"`) || !strings.Contains(body, `href="/downloads"`) {
		t.Error("an admin does not see the Enroll and Downloads navigation")
	}
}

func TestWizardRefusesWhenTheCreateIsNotAudited(t *testing.T) {
	e, _ := wizardEnv(t)
	summit := e.customerID("Summit Eye Care")
	if err := e.st.Write(context.Background(), func(tx *store.Tx) error {
		_, err := tx.Exec(`CREATE TRIGGER refuse_audits BEFORE INSERT ON audit_events BEGIN SELECT RAISE(ABORT, 'audit refused by the test'); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	tokens := e.tokenCount()
	res, page := e.createToken(summit, nil)
	if res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("unaudited create: %d, want 500", res.StatusCode)
	}
	mustContain(t, "not audited", page, "NOT AUDITED", "Nothing was changed")
	if shownToken.MatchString(page) {
		t.Fatal("a token was shown although its audit row could not be written")
	}
	if e.tokenCount() != tokens {
		t.Error("a token was created without its audit row")
	}
}

func TestWizardSaysWhatWasCreatedWhenTheTokenAuditFailsAfterANewCustomer(t *testing.T) {
	e, _ := wizardEnv(t)
	if err := e.st.Write(context.Background(), func(tx *store.Tx) error {
		_, err := tx.Exec(`CREATE TRIGGER refuse_token_audits BEFORE INSERT ON audit_events WHEN NEW.action = 'enrollment_token.create'
			BEGIN SELECT RAISE(ABORT, 'audit refused by the test'); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	tokens := e.tokenCount()
	res, page := e.createToken("", url.Values{"new_customer": {"Half Done Clinic"}})
	if res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500", res.StatusCode)
	}
	mustContain(t, "half done", page, "The new customer was created, but its token was not")
	if shownToken.MatchString(page) || e.tokenCount() != tokens {
		t.Fatal("a token exists or was shown although its audit row failed")
	}
	_ = e.customerID("Half Done Clinic") // the customer is there, as the page says
}
