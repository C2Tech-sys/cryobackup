package notify

import (
	"math/rand/v2"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/config"
)

// P21-T7 C (F1 of the P21-T6 blind check): a value config accepted for admin_url could be one the
// renderers re-parsed and dropped, so a message went out without its button. Every value the
// validator accepts must render a link, in every kind of message; a given link that cannot be
// rendered is an error, never a missing button.

// t7Candidates is a hand-picked table plus a seeded random spread of shapes around the validator's
// edges: host names, IPv4, bracketed text of hex digits, colons and dots, real IPv6 addresses,
// ports in and out of range, upper case, padding and a trailing slash.
func t7Candidates() []string {
	out := []string{
		"https://console.example.net", "https://console.example.net/", "http://console.example.net:8080",
		"https://192.0.2.10", "http://10.0.0.5:8443", "https://[2001:db8::1]", "https://[2001:db8::1]:8443/",
		"https://[::1]", "https://[::]", "https://[::ffff:10.0.0.5]:8080", "https://[::192.0.2.1]",
		"https://[2001:DB8::A]", "https://[10.0.0.5]:8080", "https://[1.2]", "https://[:::]", "https://a",
		"https://a-b.c.example:1", " https://console.example.net ", "https://9", "https://1.2.3",
		"https://console.example.net:65535", "https://xn--bcher-kva.example",
	}
	r := rand.New(rand.NewPCG(2026, 1124))
	pick := func(alphabet string, n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = alphabet[r.IntN(len(alphabet))]
		}
		return string(b)
	}
	for i := 0; i < 4000; i++ {
		scheme := []string{"https", "http", "HTTPS", "ftp"}[r.IntN(4)]
		var host string
		switch r.IntN(5) {
		case 0:
			host = pick("abcxyzABCXYZ0189.-", 1+r.IntN(24))
		case 1:
			host = netip.AddrFrom4([4]byte{byte(r.IntN(256)), byte(r.IntN(256)), byte(r.IntN(256)), byte(r.IntN(256))}).String()
		case 2:
			host = "[" + pick("0123456789abcdefABCDEF:.", 1+r.IntN(30)) + "]"
		case 3:
			var b [16]byte
			for j := range b {
				b[j] = byte(r.IntN(256))
			}
			if r.IntN(4) == 0 { // a run of zeros, so the :: form shows up
				for j := 4; j < 12; j++ {
					b[j] = 0
				}
			}
			host = "[" + netip.AddrFrom16(b).String() + "]"
		default:
			host = "[" + netip.AddrFrom16(netip.AddrFrom4([4]byte{byte(r.IntN(256)), 0, 2, byte(r.IntN(256))}).As16()).String() + "]"
		}
		v := scheme + "://" + host
		if r.IntN(2) == 0 {
			v += ":" + strconv.Itoa(r.IntN(70000))
		}
		if r.IntN(3) == 0 {
			v += "/"
		}
		if r.IntN(10) == 0 {
			v = " " + v + "\t"
		}
		out = append(out, v)
	}
	return out
}

func TestEveryValidatedAdminURLRendersALink(t *testing.T) {
	v := renderView()
	accepted, bracketed := 0, 0
	for _, raw := range t7Candidates() {
		if _, err := config.ParseAdminURL(raw); err != nil {
			continue
		}
		accepted++
		link := (config.Config{AdminURL: raw}).AdminLink()
		if link == "" {
			t.Errorf("%q: accepted by config but AdminLink is empty", raw)
			continue
		}
		if strings.Contains(link, "[") {
			bracketed++
		}
		o := Options{Loc: cst, ConsoleURL: link}
		for kind, render := range map[string]func() (Content, error){
			"alert":    func() (Content, error) { return RenderAlert(v, alertBatch(), o) },
			"recovery": func() (Content, error) { return RenderRecovery(v, recoveryBatch(), o) },
			"digest":   func() (Content, error) { return RenderDigest(greenView(2), nil, o) },
		} {
			c, err := render()
			if err != nil {
				t.Errorf("%q: %s render failed: %v", raw, kind, err)
				continue
			}
			if !strings.Contains(c.HTML, `href="`+link+`"`) || !strings.Contains(c.Text, link) {
				t.Errorf("%q: %s message carries no link to %s", raw, kind, link)
			}
		}
		m := (&TestMailer{ConsoleURL: link, Loc: func() *time.Location { return cst }}).message(renderNow, "ops@example.com")
		if !strings.Contains(m.HTML, `href="`+link+`"`) || !strings.Contains(m.Text, "Open the console: "+link) {
			t.Errorf("%q: test message carries no link to %s", raw, link)
		}
	}
	// The spread must actually reach the validator's accepted side, bracketed hosts included.
	if accepted < 500 || bracketed < 100 {
		t.Fatalf("only %d accepted values (%d bracketed): the generator no longer covers the validator", accepted, bracketed)
	}
}

// V2 (P21-T7 fix batch 1): a link that is given but cannot be rendered never costs the message.
// Every renderer returns the message exactly as with no link (no button, no "Open the console:"
// line), and no error; the sender logs it (TestNotifierSendsWithoutALinkThatCannotRender and the
// tests below). No link given: no error and no button, as before.
func TestAGivenLinkThatCannotRenderIsLeftOut(t *testing.T) {
	v := renderView()
	for _, bad := range []string{"javascript:alert(1)", "console.example.net", "https://", "https://[10.0.0.5]:8080", "ftp://console.example.net"} {
		if !consoleLinkDropped(bad) {
			t.Errorf("consoleLinkDropped(%q) = false", bad)
		}
		for kind, render := range map[string]func(Options) (Content, error){
			"alert":    func(o Options) (Content, error) { return RenderAlert(v, alertBatch(), o) },
			"recovery": func(o Options) (Content, error) { return RenderRecovery(v, recoveryBatch(), o) },
			"digest":   func(o Options) (Content, error) { return RenderDigest(greenView(2), nil, o) },
		} {
			c, err := render(Options{Loc: cst, ConsoleURL: bad})
			if err != nil {
				t.Errorf("%s with link %q: err %v, want the message without the link", kind, bad, err)
				continue
			}
			none, err := render(Options{Loc: cst})
			if err != nil {
				t.Fatalf("%s without a link: %v", kind, err)
			}
			if c.Text != none.Text || c.HTML != none.HTML || c.Subject != none.Subject || c.Text == "" || c.HTML == "" {
				t.Errorf("%s with link %q: not the message rendered without a link", kind, bad)
			}
			if strings.Contains(c.HTML, ">Open the console</a>") || strings.Contains(c.Text, "Open the console:") {
				t.Errorf("%s with link %q: a button or link line is present", kind, bad)
			}
		}
		m := (&TestMailer{ConsoleURL: bad, Loc: func() *time.Location { return cst }}).message(renderNow, "ops@example.com")
		none := (&TestMailer{Loc: func() *time.Location { return cst }}).message(renderNow, "ops@example.com")
		if m.Text != none.Text || m.HTML != none.HTML {
			t.Errorf("test message with link %q: not the message rendered without a link", bad)
		}
	}
	for _, unset := range []string{"", "  "} {
		if consoleLinkDropped(unset) {
			t.Errorf("consoleLinkDropped(%q) = true: no link given is not a dropped link", unset)
		}
	}
	if consoleLinkDropped("https://console.example.net/") {
		t.Error("a valid link reported as dropped")
	}
}
