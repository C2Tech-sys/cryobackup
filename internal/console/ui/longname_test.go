package ui

import (
	"net/http"
	"net/url"
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

// P20-W3B: a recipient at the limits the store accepts (a 100-character name, a 254-octet address)
// is added through the e-mail page and shown in full: nothing is cut. m4_layout_test.go lays the
// same page out in a browser at 390 px and desktop width.
func TestLongestRecipientIsAddedAndShownInFull(t *testing.T) {
	e := newEnv(t)
	e.login()
	name, addr := longRecipient()
	res, _ := e.post("/ui/settings/email/recipients", url.Values{"name": {name}, "address": {addr}, "alerts": {"1"}}, false)
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("add the longest recipient: %d", res.StatusCode)
	}
	body := e.page("/settings/email", http.StatusOK)
	if !strings.Contains(body, ">"+name+"<") || !strings.Contains(body, ">"+addr+"<") {
		t.Fatal("the longest name or address is not shown in full")
	}
}

// longRecipient is a name of 100 characters and an address of 254 octets (64 in the local part, domain labels of 63).
func longRecipient() (string, string) {
	name := strings.Repeat("Escalations desk, after hours, ", 4)[:100]
	addr := strings.Repeat("a", 64) + "@" + strings.Repeat("x", 63) + "." + strings.Repeat("y", 63) + "." + strings.Repeat("z", 56) + ".test"
	return name, addr
}
