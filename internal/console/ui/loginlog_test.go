package ui

import (
	"bytes"
	"net/url"
	"strings"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/logx"
)

// M2-W1B: a refused sign-in logs typed or derived fields, never a request
// header copied through (a Referer can carry a path, a User-Agent names a
// workstation, Host and Origin are whatever the client sent).
func TestLoginRefusalLogsDerivedFieldsNotHeaders(t *testing.T) {
	e := newEnv(t)
	var buf bytes.Buffer
	e.svc.Log = logx.New(&buf, logx.LevelInfo)

	const (
		foreignOrigin = "https://evil.example"
		referer       = "https://evil.example/customers/Alpha-Dental/devices"
		ua            = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) Gecko/20100101 Firefox/131.0 ZZ-WORKSTATION-17"
	)
	res, _ := e.loginPostWith(url.Values{"username": {"admin"}, "password": {"x"}}, nil,
		"Origin", foreignOrigin, "Referer", referer, "User-Agent", ua, "Sec-Fetch-Site", "cross-site", "Sec-Fetch-Mode", "navigate")
	if res.StatusCode != 403 {
		t.Fatalf("status = %d, want 403", res.StatusCode)
	}
	line := buf.String()
	if !strings.Contains(line, "login refused") {
		t.Fatalf("no refusal line logged: %q", line)
	}
	for _, leak := range []string{"evil.example", "Alpha-Dental", "ZZ-WORKSTATION", "Mozilla", e.srv.URL[len("http://"):]} {
		if strings.Contains(line, leak) {
			t.Errorf("the refusal line carries header text %q: %s", leak, line)
		}
	}
	for _, key := range []string{`"origin"`, `"referer"`, `"host"`, `"user_agent"`} {
		if strings.Contains(line, key) {
			t.Errorf("the refusal line still has the free-string field %s: %s", key, line)
		}
	}
	for _, want := range []string{`"origin_present":true`, `"origin_matches_host":false`, `"referer_present":true`, `"referer_matches_host":false`,
		`"sec_fetch_site":"cross-site"`, `"sec_fetch_mode":"navigate"`, `"ua_family":"firefox"`} {
		if !strings.Contains(line, want) {
			t.Errorf("the refusal line lacks %s: %s", want, line)
		}
	}
}

func TestLoginRefusalFieldsAreBounded(t *testing.T) {
	for ua, want := range map[string]string{
		"": "absent", "curl/8.4.0": "curl", "Mozilla/5.0 Chrome/120 Safari/537 Edg/120": "edge",
		"Mozilla/5.0 Chrome/120 Safari/537": "chrome", "Mozilla/5.0 Version/17 Safari/605": "safari", "SomethingElse/1.0": "other",
	} {
		if got := uaFamily(ua); got != want {
			t.Errorf("uaFamily(%q) = %q, want %q", ua, got, want)
		}
	}
	if got := oneOf("x; drop table", "none"); got != "other" {
		t.Errorf("oneOf of a foreign value = %q, want other", got)
	}
}
