package adminapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/console/enroll"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
)

const testPublicURL = "https://backup.example.test:8443"

// enrollEnv is the API test env with the generator configured and a log buffer
// the tests can search for a secret.
func enrollEnv(t *testing.T) (*env, *bytes.Buffer) {
	t.Helper()
	e := newEnv(t)
	var logs bytes.Buffer
	e.svc.Log = logx.New(&logs, logx.LevelDebug)
	e.svc.PublicURL = testPublicURL
	e.svc.ArtifactsDir = t.TempDir()
	return e, &logs
}

func createToken(t *testing.T, e *env, s *session, body map[string]any) (CreateEnrollTokenResponse, int, []byte) {
	t.Helper()
	res, b := e.req("POST", "/api/v1/enrollment-tokens", s, body)
	var out CreateEnrollTokenResponse
	if res.StatusCode == http.StatusCreated {
		out = decode[CreateEnrollTokenResponse](t, b)
	}
	return out, res.StatusCode, b
}

func TestEnrollRoutesNeedTheAdminRole(t *testing.T) {
	e, _ := enrollEnv(t)
	summit := e.customerNamed("Summit Eye Care")
	roles := map[string]*session{}
	for _, r := range []string{"read_only", "operator", "admin", "owner"} {
		e.addAdmin("u-"+r, r, nil)
		roles[r] = e.login("u-"+r, pw)
	}
	made := map[string]EnrollTokenJSON{}
	for _, role := range []string{"admin", "owner"} {
		tok, code, b := createToken(t, e, roles[role], map[string]any{"customer_id": summit.ID, "label": "matrix " + role})
		if code != http.StatusCreated {
			t.Fatalf("create as %s: %d %s", role, code, b)
		}
		made[role] = tok.EnrollTokenJSON
	}
	for pat, min := range EnrollPatterns() {
		if min != "AD" {
			t.Fatalf("%s: the table says %s, Â§5.2 says AD", pat, min)
		}
	}
	for _, role := range []string{"read_only", "operator", "admin", "owner"} {
		allowed := role == "admin" || role == "owner"
		check := func(name, method, path string, body any, hdr ...string) {
			res, b := e.req(method, path, roles[role], body, hdr...)
			switch {
			case allowed && res.StatusCode >= 300:
				t.Errorf("%s as %s: want success, got %d %s", name, role, res.StatusCode, b)
			case !allowed && (res.StatusCode != 403 || problemCode(t, res, b) != api.CodeForbidden):
				t.Errorf("%s as %s: want 403 forbidden, got %d %s", name, role, res.StatusCode, b)
			}
		}
		id := made["admin"].ID
		check("list", "GET", "/api/v1/enrollment-tokens", nil)
		check("get", "GET", "/api/v1/enrollment-tokens/"+id, nil)
		check("create", "POST", "/api/v1/enrollment-tokens", map[string]any{"customer_id": summit.ID})
		check("revoke", "DELETE", "/api/v1/enrollment-tokens/"+made["owner"].ID, nil)
		if !allowed { // script: a role below admin never reaches the handler, whatever the header says
			check("script", "GET", "/api/v1/enrollment-tokens/"+id+"/script?format=ps1", nil, EnrollTokenHeader, "x")
		}
	}
}

func TestCreatedTokenIsShownOnceAndNeverLoggedOrAudited(t *testing.T) {
	e, logs := enrollEnv(t)
	s := e.login("admin", pw)
	summit := e.customerNamed("Summit Eye Care")
	before := e.audits()
	tok, code, b := createToken(t, e, s, map[string]any{"customer_id": summit.ID, "label": "Front office", "expires_in": "3d", "max_uses": 4, "device_class": "laptop"})
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, b)
	}
	if !enroll.WellFormed(tok.Token) {
		t.Fatalf("the response carries no secret: %s", b)
	}
	if tok.Status != store.TokenActive || tok.MaxUses == nil || *tok.MaxUses != 4 || tok.UsesLeft == nil || *tok.UsesLeft != 4 || tok.Customer != "Summit Eye Care" || tok.DeviceClass != "laptop" {
		t.Fatalf("token = %+v", tok.EnrollTokenJSON)
	}
	if d := tok.ExpiresAt.Sub(e.clk.Now()); d != 72*time.Hour {
		t.Fatalf("expires in %v, want 72h", d)
	}
	stored, err := e.st.GetEnrollmentTokenByHash(context.Background(), enroll.HashToken(tok.Token))
	if err != nil || stored.ID != tok.ID {
		t.Fatalf("the secret does not hash to the stored token: %v", err)
	}
	// Second look: no value anywhere.
	for _, path := range []string{"/api/v1/enrollment-tokens/" + tok.ID, "/api/v1/enrollment-tokens", "/api/v1/enrollment-tokens?status=active&customer=" + summit.ID} {
		_, b := e.get(path, s)
		for _, bad := range []string{tok.Token, stored.TokenHash, `"token"`, "token_hash"} {
			if strings.Contains(string(b), bad) {
				t.Errorf("GET %s shows %q: %s", path, bad, b)
			}
		}
	}
	if got := e.audits() - before; got != 1 {
		t.Fatalf("%d audit rows for one create, want 1", got)
	}
	rows := e.auditOf("enrollment_token.create", tok.ID)
	if len(rows) != 1 || rows[0].Method != "api" || rows[0].CustomerID != summit.ID {
		t.Fatalf("audit rows = %+v", rows)
	}
	everything := string(rows[0].Details) + rows[0].ObjectID + rows[0].ActorLabel + rows[0].SourceIP
	for _, bad := range []string{tok.Token, stored.TokenHash, "Front office"} {
		if strings.Contains(everything, bad) {
			t.Errorf("the audit row holds %q: %s", bad, everything)
		}
	}
	if strings.Contains(logs.String(), tok.Token) || strings.Contains(logs.String(), stored.TokenHash) {
		t.Errorf("the log holds the token or its hash:\n%s", logs.String())
	}
}

func TestCreateTokenRejectsBadInputWithFields(t *testing.T) {
	e, _ := enrollEnv(t)
	s := e.login("admin", pw)
	summit := e.customerNamed("Summit Eye Care")
	before := e.audits()
	for name, tc := range map[string]struct {
		body  map[string]any
		field string
	}{
		"bad duration":  {map[string]any{"customer_id": summit.ID, "expires_in": "soon"}, "expires_in"},
		"too long":      {map[string]any{"customer_id": summit.ID, "expires_in": "120d"}, "expires"},
		"zero uses":     {map[string]any{"customer_id": summit.ID, "max_uses": 0}, "max_uses"},
		"bad class":     {map[string]any{"customer_id": summit.ID, "device_class": "server"}, "device_class"},
		"missing":       {map[string]any{"label": "x"}, "customer_id"},
		"foreign class": {map[string]any{"customer_id": summit.ID, "policy_id": "01J9Y4K3A0B1C2D3E4F5G6H7AA"}, "policy_id"},
	} {
		_, code, b := createToken(t, e, s, tc.body)
		if code != 400 {
			t.Errorf("%s: %d %s", name, code, b)
			continue
		}
		if f := problemFields(t, b); f[tc.field] == "" {
			t.Errorf("%s: fields = %v, want %s", name, f, tc.field)
		}
	}
	if _, code, _ := createToken(t, e, s, map[string]any{"customer_id": "01J9Y4K3A0B1C2D3E4F5G6H7AA"}); code != 404 {
		t.Errorf("unknown customer: %d, want 404", code)
	}
	if e.audits() != before {
		t.Error("a rejected create wrote an audit row")
	}
	// Unknown JSON fields are refused, so a typo is not silently ignored.
	if res, b := e.req("POST", "/api/v1/enrollment-tokens", s, `{"customer_id":"`+summit.ID+`","max_use":3}`); res.StatusCode != 400 {
		t.Errorf("unknown field: %d %s", res.StatusCode, b)
	}
}

func TestRevokeIsAuditedOnceAndIdempotent(t *testing.T) {
	e, _ := enrollEnv(t)
	s := e.login("admin", pw)
	summit := e.customerNamed("Summit Eye Care")
	tok, _, _ := createToken(t, e, s, map[string]any{"customer_id": summit.ID})
	before := e.audits()
	for i := 0; i < 2; i++ {
		if res, b := e.req("DELETE", "/api/v1/enrollment-tokens/"+tok.ID, s, nil); res.StatusCode != 204 {
			t.Fatalf("revoke %d: %d %s", i+1, res.StatusCode, b)
		}
	}
	if got := e.audits() - before; got != 1 {
		t.Fatalf("%d audit rows for two revokes of one token, want 1", got)
	}
	_, b := e.get("/api/v1/enrollment-tokens/"+tok.ID, s)
	if got := decode[EnrollTokenResponse](t, b); got.Status != store.TokenRevoked || got.RevokedAt == nil {
		t.Fatalf("after revoke: %+v", got.EnrollTokenJSON)
	}
	_, b = e.get("/api/v1/enrollment-tokens?status=revoked", s)
	if l := decode[EnrollTokenListResponse](t, b); l.Total != 1 || l.Tokens[0].ID != tok.ID {
		t.Fatalf("revoked list: %s", b)
	}
	if res, _ := e.req("DELETE", "/api/v1/enrollment-tokens/01J9Y4K3A0B1C2D3E4F5G6H7AA", s, nil); res.StatusCode != 404 {
		t.Fatalf("revoke unknown: %d", res.StatusCode)
	}
	if res, _ := e.req("GET", "/api/v1/enrollment-tokens?status=bogus", s, nil); res.StatusCode != 400 {
		t.Fatalf("bad status filter: %d", res.StatusCode)
	}
}

func TestScriptRouteChecksTheSecretAndGeneratesTheSnippets(t *testing.T) {
	e, _ := enrollEnv(t)
	s := e.login("admin", pw)
	summit := e.customerNamed("Summit Eye Care")
	tok, _, _ := createToken(t, e, s, map[string]any{"customer_id": summit.ID})
	// The operator placed the MSI.
	body := []byte("msi bytes")
	if err := os.WriteFile(filepath.Join(e.svc.ArtifactsDir, "cryobackup-agent-1.4.2.msi"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	wantHash := hex.EncodeToString(sum[:])
	path := "/api/v1/enrollment-tokens/" + tok.ID + "/script?format="

	res, text := e.req("GET", path+"ps1", s, nil, EnrollTokenHeader, tok.Token)
	if res.StatusCode != 200 || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/plain") || res.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("ps1: %d %v", res.StatusCode, res.Header)
	}
	for _, want := range []string{"$ServerUrl   = '" + testPublicURL + "'", "$EnrollToken = '" + tok.Token + "'", "$MsiSha256   = '" + wantHash + "'"} {
		if !strings.Contains(string(text), want) {
			t.Errorf("ps1 lacks %q:\n%s", want, text)
		}
	}
	res, text = e.req("GET", path+"msi", s, nil, EnrollTokenHeader, tok.Token)
	if res.StatusCode != 200 || !strings.Contains(string(text), `SERVER="`+testPublicURL+`"`) || !strings.Contains(string(text), `TOKEN="`+tok.Token+`"`) || !strings.Contains(string(text), `"cryobackup-agent-1.4.2.msi"`) {
		t.Fatalf("msi: %d %s", res.StatusCode, text)
	}
	if res, text = e.req("GET", path+"gpo", s, nil, EnrollTokenHeader, tok.Token); res.StatusCode != 200 || !strings.Contains(string(text), "install-cryobackup.ps1") {
		t.Fatalf("gpo: %d %s", res.StatusCode, text)
	}

	// Refusals: no secret, a wrong secret, a secret in the query string, an unknown format.
	for name, tc := range map[string]struct {
		path string
		hdr  []string
	}{
		"no header":   {path + "ps1", nil},
		"wrong":       {path + "ps1", []string{EnrollTokenHeader, strings.Repeat("a", enroll.TokenLen)}},
		"in query":    {path + "ps1&token=" + tok.Token, nil},
		"bad format":  {path + "bash", []string{EnrollTokenHeader, tok.Token}},
		"no format":   {"/api/v1/enrollment-tokens/" + tok.ID + "/script", []string{EnrollTokenHeader, tok.Token}},
		"malformed":   {path + "ps1", []string{EnrollTokenHeader, "short"}},
		"wrong token": {"/api/v1/enrollment-tokens/01J9Y4K3A0B1C2D3E4F5G6H7AA/script?format=ps1", []string{EnrollTokenHeader, tok.Token}},
	} {
		res, b := e.req("GET", tc.path, s, nil, tc.hdr...)
		if res.StatusCode != 400 && res.StatusCode != 404 {
			t.Errorf("%s: %d %s", name, res.StatusCode, b)
		}
		if strings.Contains(string(b), tok.Token) {
			t.Errorf("%s: the refusal echoes the secret", name)
		}
	}
	// A revoked token is not turned into a script.
	if res, _ := e.req("DELETE", "/api/v1/enrollment-tokens/"+tok.ID, s, nil); res.StatusCode != 204 {
		t.Fatal("revoke")
	}
	if res, b := e.req("GET", path+"ps1", s, nil, EnrollTokenHeader, tok.Token); res.StatusCode != 409 {
		t.Fatalf("script for a revoked token: %d %s", res.StatusCode, b)
	}
}

func TestListPagesByCursor(t *testing.T) {
	e, _ := enrollEnv(t)
	s := e.login("admin", pw)
	summit := e.customerNamed("Summit Eye Care")
	for i := 0; i < 5; i++ {
		if _, code, b := createToken(t, e, s, map[string]any{"customer_id": summit.ID}); code != 201 {
			t.Fatalf("create %d: %d %s", i, code, b)
		}
	}
	_, b := e.get("/api/v1/enrollment-tokens?limit=2", s)
	first := decode[EnrollTokenListResponse](t, b)
	if len(first.Tokens) != 2 || !first.Truncated || first.NextCursor == "" || first.Total < 5 {
		t.Fatalf("first page: %s", b)
	}
	_, b = e.get("/api/v1/enrollment-tokens?limit=2&cursor="+first.NextCursor, s)
	second := decode[EnrollTokenListResponse](t, b)
	if len(second.Tokens) != 2 || second.Tokens[0].ID == first.Tokens[0].ID || second.Tokens[0].ID == first.Tokens[1].ID {
		t.Fatalf("second page: %s", b)
	}
}

// ---- the generator ----

func TestRMMSnippetNeverPutsTheTokenOnACommandLine(t *testing.T) {
	token := strings.Repeat("A", enroll.TokenLen)
	sn := GenerateSnippets(SnippetInput{PublicURL: testPublicURL, Token: token, MSI: MSIInfo{FileName: "cryobackup-agent-1.0.0.msi", SHA256: strings.Repeat("ab", 32)}})
	if !sn.TokenShown || !sn.MSIKnown || sn.ServerProblem != "" || sn.Lab {
		t.Fatalf("flags = %+v", sn)
	}
	for _, line := range strings.Split(sn.RMM, "\n") {
		if strings.Contains(line, token) && !strings.HasPrefix(line, "$EnrollToken = '"+token+"'") {
			t.Errorf("the token is on a line that is not its variable assignment: %q", line)
		}
		if strings.HasPrefix(line, "#") && strings.Contains(line, token) {
			t.Errorf("the token is in a comment: %q", line)
		}
	}
	for _, banned := range []string{"msiexec", "TOKEN=", "/qn", "-ArgumentList", "Start-Process"} {
		if strings.Contains(sn.RMM, banned) {
			t.Errorf("the RMM block contains %q:\n%s", banned, sn.RMM)
		}
	}
	for _, name := range []string{"MsiPath", "MsiSha256", "ServerUrl", "EnrollToken"} {
		if !strings.Contains(sn.RMM, "$"+name+" ") {
			t.Errorf("the RMM block lacks $%s", name)
		}
	}
	secrets := 0
	for _, v := range sn.Vars {
		if v.Secret {
			secrets++
			if v.Name != "EnrollToken" {
				t.Errorf("secret variable %s", v.Name)
			}
		}
	}
	if secrets != 1 {
		t.Errorf("%d secret variables, want exactly the token", secrets)
	}
	if !strings.Contains(sn.MSIExec, `TOKEN="`+token+`"`) || !strings.Contains(sn.MSIExecWarning, "local administrator") {
		t.Errorf("msiexec form: %q / %q", sn.MSIExec, sn.MSIExecWarning)
	}
	if !strings.Contains(sn.GPO, testPublicURL) || !strings.Contains(strings.Join(sn.GPONotes, " "), "use cap") {
		t.Errorf("gpo: %s", sn.GPO)
	}
}

func TestSnippetsFailLoudlyWhenAPieceIsMissing(t *testing.T) {
	sn := GenerateSnippets(SnippetInput{})
	if sn.ServerProblem == "" || sn.TokenShown || sn.MSIKnown {
		t.Fatalf("flags = %+v", sn)
	}
	for _, want := range []string{PlaceholderServerURL, PlaceholderToken, PlaceholderMSIHash} {
		if !strings.Contains(sn.RMM, want) {
			t.Errorf("the RMM block lacks the placeholder %q", want)
		}
	}
	if strings.Contains(sn.RMM, "https://") {
		t.Errorf("an invented URL: %s", sn.RMM)
	}
	for _, bad := range []string{"ftp://x", "https://", "not a url", "https://user:pw@host", "https://ho'st", "https://x$(calc)y:8443", "https://%25windir%25:8443", "%25windir%25",
		"https://host/path", "https://host/some/path/?x=1", "https://host?x=1", "https://host#frag", "https://host:0", "https://host:99999", "https://host:80a", "https://-host",
		"https://ho st", "https://host`calc`", "https://hos\"t", "https://höst", "https://host:8443\n$x", "https://[::1", "https://[xyz]:1"} {
		u, _, problem := AgentServerURL(bad)
		if problem == "" || u != PlaceholderServerURL {
			t.Errorf("public_url %q accepted as %q", bad, u)
		}
		sn := GenerateSnippets(SnippetInput{PublicURL: bad, Token: strings.Repeat("C", enroll.TokenLen)})
		for _, text := range []string{sn.RMM, sn.MSIExec, sn.GPO} {
			if strings.Contains(text, "calc") || strings.Contains(text, "windir") {
				t.Errorf("hostile public_url %q reached a snippet:\n%s", bad, text)
			}
		}
	}
	for in, want := range map[string]string{"https://backup.example.test:8443": testPublicURL, "https://backup.example.test:8443/": testPublicURL, "HTTPS://Host-1.example.test": "",
		"https://[2001:db8::1]:8443": "https://[2001:db8::1]:8443", "https://10.0.0.5": "https://10.0.0.5"} {
		u, _, problem := AgentServerURL(in)
		if want == "" { // an upper-case scheme is not accepted: the agent compares it literally
			if problem == "" {
				t.Errorf("%q accepted as %q", in, u)
			}
			continue
		}
		if u != want || problem != "" {
			t.Errorf("AgentServerURL(%q) = %q, %q", in, u, problem)
		}
	}
	u, lab, problem := AgentServerURL("http://10.0.0.5:8443")
	if u != "http://10.0.0.5:8443" || !lab || !strings.Contains(problem, "lab") {
		t.Errorf("plain http = %q lab=%v problem=%q", u, lab, problem)
	}
	lab2 := GenerateSnippets(SnippetInput{PublicURL: "http://10.0.0.5:8443", Token: strings.Repeat("B", enroll.TokenLen)})
	if !strings.Contains(lab2.RMM, "$InsecureLab = '1'") || !strings.Contains(lab2.MSIExec, "CRYO_INSECURE_LAB=1") {
		t.Errorf("lab snippets lack the override:\n%s\n%s", lab2.RMM, lab2.MSIExec)
	}
	// Single quotes in a value cannot break out of the PowerShell string.
	if got := psq("a'b"); got != "'a''b'" {
		t.Errorf("psq = %s", got)
	}
}

// PowerShell does not expand %VAR%: msiexec given "%windir%\Temp\x.log" from
// PowerShell exits 1622 and aborts the install. Every snippet must work pasted
// into PowerShell or cmd, so none may hold a %VAR%.
func TestNoSnippetHoldsAShellVariable(t *testing.T) {
	percentVar := regexp.MustCompile(`%[A-Za-z_][A-Za-z0-9_]*%`)
	token := strings.Repeat("D", enroll.TokenLen)
	for name, in := range map[string]SnippetInput{
		"https":   {PublicURL: testPublicURL, Token: token, MSI: MSIInfo{FileName: "cryobackup-agent-1.0.0.msi", SHA256: strings.Repeat("ab", 32)}},
		"lab":     {PublicURL: "http://10.0.0.5:8443", Token: token},
		"nothing": {},
	} {
		sn := GenerateSnippets(in)
		for kind, text := range map[string]string{"rmm": sn.RMM, "msiexec": sn.MSIExec, "gpo": sn.GPO} {
			if m := percentVar.FindString(text); m != "" {
				t.Errorf("%s/%s holds the shell variable %s:\n%s", name, kind, m, text)
			}
		}
		if !strings.Contains(sn.MSIExec, ` /l*v C:\Windows\Temp\cryobackup-install.log`) {
			t.Errorf("%s: msiexec log path is not the literal one: %s", name, sn.MSIExec)
		}
	}
}

func TestCurrentMSIPicksTheNewestVersionAndHashesIt(t *testing.T) {
	dir := t.TempDir()
	if _, err := CurrentMSI(dir); err != ErrNoMSI {
		t.Fatalf("empty dir: %v", err)
	}
	if _, err := CurrentMSI(""); err != ErrNoMSI {
		t.Fatalf("no dir configured: %v", err)
	}
	if _, err := CurrentMSI(filepath.Join(dir, "missing")); err == nil || err == ErrNoMSI {
		t.Fatalf("a configured directory that is missing must be a read error, not none-found: %v", err)
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("cryobackup-agent-1.9.0.msi", "old")
	write("cryobackup-agent-1.10.0.msi", "new")
	write("cryobackup-agent.msi", "unversioned")
	write("other.msi", "not ours")
	write("cryobackup-agent-9.9.9.msi.part", "partial download")
	got, err := CurrentMSI(dir)
	if err != nil || got.FileName != "cryobackup-agent-1.10.0.msi" || got.Version != "1.10.0" {
		t.Fatalf("got %+v, %v (1.10.0 must beat 1.9.0 numerically)", got, err)
	}
	sum := sha256.Sum256([]byte("new"))
	if got.SHA256 != hex.EncodeToString(sum[:]) || got.Size != 3 {
		t.Fatalf("hash %s size %d", got.SHA256, got.Size)
	}
	// The cache follows the file: a replaced file is hashed again.
	time.Sleep(10 * time.Millisecond)
	write("cryobackup-agent-1.10.0.msi", "newer!")
	if err := os.Chtimes(filepath.Join(dir, "cryobackup-agent-1.10.0.msi"), time.Now().Add(time.Minute), time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	again, _ := CurrentMSI(dir)
	sum = sha256.Sum256([]byte("newer!"))
	if again.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("stale digest after the file changed: %s", again.SHA256)
	}
	// An unversioned file takes its version from a .version file.
	only := t.TempDir()
	if err := os.WriteFile(filepath.Join(only, "cryobackup-agent.msi"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if v, _ := CurrentMSI(only); v.Version != "" || v.FileName != "cryobackup-agent.msi" {
		t.Fatalf("unversioned: %+v", v)
	}
	if err := os.WriteFile(filepath.Join(only, "cryobackup-agent.msi.version"), []byte("2.0.1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if v, _ := CurrentMSI(only); v.Version != "2.0.1" {
		t.Fatalf("sidecar version: %+v", v)
	}
}

func TestCurrentMSIIgnoresSymbolicLinks(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "elsewhere.msi")
	if err := os.WriteFile(outside, []byte("secret place"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "cryobackup-agent.msi")); err != nil {
		t.Skipf("cannot create a symbolic link here: %v", err)
	}
	if _, err := CurrentMSI(dir); err != ErrNoMSI {
		t.Fatalf("a symbolic link was followed: %v", err)
	}
}
