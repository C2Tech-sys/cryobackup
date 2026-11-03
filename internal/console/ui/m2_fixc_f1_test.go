package ui

import (
	"net/url"
	"testing"
)

// M2-FIX-C (F1): the MFA step is a bare shell of its own; it showed the copy
// button for the manual key but never loaded copy.js, so the button did nothing.
func TestFixCMFAPageLoadsTheCopyScript(t *testing.T) {
	e := newEnv(t)
	res, _ := e.do("POST", "/login", url.Values{"username": {"admin"}, "password": {testPassword}})
	e.adopt(res)
	_, body := e.do("GET", "/login/mfa", nil)
	mustContain(t, "enroll page", body, `data-copy-from="cp-totp-key"`, `<script src="/static/js/copy.js?v=`)
}
