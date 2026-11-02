package server_test

import (
	"bytes"
	"encoding/base32"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminapi"
	"github.com/c2tech-sys/cryobackup/internal/console/auth"
)

// The MFA step for the server tests (M2-W1A): a fresh account enrolls TOTP on
// its first login; the console checks codes against the wall clock.

var mfaSecretRE = regexp.MustCompile(`<pre class="code secret"[^>]*>([A-Z2-7 ]+)</pre>`)

func decodeSecret(t *testing.T, grouped string) []byte {
	t.Helper()
	b, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ReplaceAll(grouped, " ", ""))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mergeCookies(old, fresh []*http.Cookie) []*http.Cookie {
	out := append([]*http.Cookie(nil), fresh...)
	for _, c := range old {
		dup := false
		for _, f := range fresh {
			dup = dup || f.Name == c.Name
		}
		if !dup {
			out = append(out, c)
		}
	}
	return out
}

// uiEnroll completes the forced enrollment of a password-only UI session and
// returns the cookies of the full session.
func uiEnroll(t *testing.T, client *http.Client, base string, cookies []*http.Cookie) []*http.Cookie {
	t.Helper()
	r, _ := http.NewRequest("GET", base+"/login/mfa", nil)
	for _, c := range cookies {
		r.AddCookie(c)
	}
	res, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	m := mfaSecretRE.FindStringSubmatch(string(b))
	if res.StatusCode != 200 || m == nil {
		t.Fatalf("mfa page: %d", res.StatusCode)
	}
	csrf := ""
	for _, c := range cookies {
		if c.Name == auth.CSRFCookie {
			csrf = c.Value
		}
	}
	form := url.Values{"csrf_token": {csrf}, "code": {auth.TOTPCode(decodeSecret(t, m[1]), time.Now())}}
	r, _ = http.NewRequest("POST", base+"/login/mfa/enroll", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", base)
	for _, c := range cookies {
		r.AddCookie(c)
	}
	res, err = client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || !strings.Contains(string(b), "Save your recovery codes") {
		t.Fatalf("enroll: %d", res.StatusCode)
	}
	return mergeCookies(cookies, res.Cookies())
}

// finishMFA completes an API login: enrollment the first time (keeping the
// recovery codes), a recovery code afterwards (the TOTP code of the current
// step was already used by the enrollment, and replay is refused).
func (a *adminCall) finishMFA(lr adminapi.LoginResponse) {
	a.t.Helper()
	post := func(path string, body any) (*http.Response, []byte) {
		raw, _ := json.Marshal(body)
		r, _ := http.NewRequest("POST", a.base+path, bytes.NewReader(raw))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", a.base)
		r.Header.Set(auth.CSRFHeader, a.csrf)
		for _, c := range a.cookies {
			r.AddCookie(c)
		}
		res, err := http.DefaultClient.Do(r)
		if err != nil {
			a.t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res, b
	}
	if lr.MFAEnrollmentRequired {
		res, b := post("/api/v1/me/totp/begin", struct{}{})
		var tb adminapi.TOTPBeginResponse
		if res.StatusCode != 200 || json.Unmarshal(b, &tb) != nil {
			a.t.Fatalf("totp begin: %d %s", res.StatusCode, b)
		}
		res, b = post("/api/v1/me/totp/confirm", adminapi.TOTPConfirmRequest{Code: auth.TOTPCode(decodeSecret(a.t, tb.Secret), time.Now())})
		var rc adminapi.RecoveryCodesResponse
		if res.StatusCode != 200 || json.Unmarshal(b, &rc) != nil || !rc.LoginComplete {
			a.t.Fatalf("totp confirm: %d %s", res.StatusCode, b)
		}
		a.recovery = rc.RecoveryCodes
		a.cookies, a.csrf = mergeCookies(a.cookies, res.Cookies()), rc.CSRFToken
		return
	}
	if len(a.recovery) == 0 {
		a.t.Fatal("no recovery code left for the test login")
	}
	code := a.recovery[0]
	a.recovery = a.recovery[1:]
	res, b := post("/api/v1/auth/mfa/recovery", adminapi.CodeRequest{Code: code})
	var done adminapi.LoginResponse
	if res.StatusCode != 200 || json.Unmarshal(b, &done) != nil {
		a.t.Fatalf("recovery login: %d %s", res.StatusCode, b)
	}
	a.cookies, a.csrf = mergeCookies(a.cookies, res.Cookies()), done.CSRFToken
}
