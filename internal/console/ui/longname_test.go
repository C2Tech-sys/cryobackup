package ui

import (
	"net/http"
	"strings"
	"testing"
)

// The longest username the account form accepts (100 characters) enrolls
// through the UI: its otpauth:// URI needs a QR symbol beyond version 10
// (foreman verification notes, M2-W1A).
func TestLongestUsernameEnrollsThroughTheUI(t *testing.T) {
	e := newEnv(t)
	user := strings.Repeat("a", 96) + "@b.c"
	if len(user) != 100 {
		t.Fatalf("username is %d characters", len(user))
	}
	e.addAccount(user, "operator")
	e.loginAs(user, testPassword) // fails inside if the enrollment page shows no QR and key
	if res, body := e.do("GET", "/account", nil); res.StatusCode != http.StatusOK || !strings.Contains(body, `<span class="st sm ok"><i aria-hidden="true"></i>ON</span>`) {
		t.Fatalf("after enrollment: %d", res.StatusCode)
	}
}
