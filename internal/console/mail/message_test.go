package mail

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	netmail "net/mail"
	"regexp"
	"strings"
	"testing"
	"time"
)

var fixedNow = time.Date(2026, 11, 18, 6, 30, 0, 0, time.FixedZone("CST", -6*3600))

func mustBuild(t *testing.T, m Message) (*netmail.Message, []byte) {
	t.Helper()
	raw, err := m.Build(fixedNow)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	msg, err := netmail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("the built message does not parse as RFC 5322: %v\n%s", err, raw)
	}
	return msg, raw
}

// decodeSinglePart returns the decoded text of a single-part message (what the
// quoted-printable reader gives back: CRLF line ends).
func decodeSinglePart(t *testing.T, raw []byte) string {
	t.Helper()
	msg, err := netmail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if got := msg.Header.Get("Content-Transfer-Encoding"); got != "quoted-printable" {
		t.Fatalf("Content-Transfer-Encoding = %q", got)
	}
	b, err := io.ReadAll(quotedprintable.NewReader(msg.Body))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// parts returns the raw parts of a multipart/alternative message: their media
// type, charset, transfer encoding and decoded text.
type partInfo struct{ typ, charset, cte, text string }

func parts(t *testing.T, msg *netmail.Message) []partInfo {
	t.Helper()
	mt, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/alternative" {
		t.Fatalf("Content-Type = %q, %v", msg.Header.Get("Content-Type"), err)
	}
	mr := multipart.NewReader(msg.Body, params["boundary"])
	var out []partInfo
	for {
		p, err := mr.NextRawPart()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		pt, pp, err := mime.ParseMediaType(p.Header.Get("Content-Type"))
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(quotedprintable.NewReader(p))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, partInfo{pt, pp["charset"], p.Header.Get("Content-Transfer-Encoding"), string(b)})
	}
}

func TestBuildStructure(t *testing.T) {
	m := Message{
		From:    "Cryo Console <console@example.com>",
		To:      []string{"ops@example.com", "Night Desk <night@example.test>"},
		Subject: "Backup alert: 2 devices",
		Text:    "Two devices failed.\nSee the console.\n",
		HTML:    "<p>Two devices failed.</p>\n",
		Kind:    KindAlert,
	}
	msg, raw := mustBuild(t, m)
	h := msg.Header
	if d, err := h.Date(); err != nil || !d.Equal(fixedNow) || !strings.Contains(h.Get("Date"), "+0000") {
		t.Fatalf("Date = %q (%v): must be the build time in UTC", h.Get("Date"), err)
	}
	if h.Get("MIME-Version") != "1.0" || h.Get("Auto-Submitted") != "auto-generated" || h.Get("X-CryoBackup-Kind") != "alert" {
		t.Fatalf("headers = %v", h)
	}
	if id := h.Get("Message-ID"); !strings.HasPrefix(id, "<") || !strings.HasSuffix(id, "@example.com>") {
		t.Fatalf("Message-ID = %q, want one under the From domain", id)
	}
	from, err := h.AddressList("From")
	if err != nil || len(from) != 1 || from[0].Address != "console@example.com" || from[0].Name != "Cryo Console" {
		t.Fatalf("From = %v, %v", from, err)
	}
	to, err := h.AddressList("To")
	if err != nil || len(to) != 2 || to[1].Address != "night@example.test" {
		t.Fatalf("To = %v, %v", to, err)
	}
	if h.Get("Bcc") != "" || h.Get("Cc") != "" {
		t.Fatal("Build must not add Cc or Bcc")
	}
	ps := parts(t, msg)
	if len(ps) != 2 {
		t.Fatalf("%d parts", len(ps))
	}
	if ps[0].typ != "text/plain" || ps[1].typ != "text/html" {
		t.Fatalf("part order = %s, %s: text first, HTML second", ps[0].typ, ps[1].typ)
	}
	for _, p := range ps {
		if !strings.EqualFold(p.charset, "utf-8") || p.cte != "quoted-printable" {
			t.Fatalf("part %+v: want utf-8 quoted-printable", p)
		}
	}
	if ps[0].text != "Two devices failed.\r\nSee the console.\r\n" || ps[1].text != "<p>Two devices failed.</p>\r\n" {
		t.Fatalf("part texts = %q / %q", ps[0].text, ps[1].text)
	}
	assertWireLimits(t, raw)
}

// The wire form: CRLF only, no line over 998, and the body parts wrapped at 76.
func assertWireLimits(t *testing.T, raw []byte) {
	t.Helper()
	if bytes.Contains(bytes.ReplaceAll(raw, []byte("\r\n"), nil), []byte("\n")) ||
		bytes.Contains(bytes.ReplaceAll(raw, []byte("\r\n"), nil), []byte("\r")) {
		t.Fatal("the message has a bare LF or CR")
	}
	for _, l := range strings.Split(string(raw), "\r\n") {
		if len(l) > 998 {
			t.Fatalf("a line of %d bytes", len(l))
		}
	}
	if !bytes.HasSuffix(raw, []byte("\r\n")) {
		t.Fatal("the message does not end with a line end")
	}
}

func TestBuildWithoutHTMLIsOneTextPart(t *testing.T) {
	msg, raw := mustBuild(t, Message{From: "a@example.com", To: []string{"b@example.com"}, Subject: "s", Text: "hello\n"})
	if mt, params, _ := mime.ParseMediaType(msg.Header.Get("Content-Type")); mt != "text/plain" || !strings.EqualFold(params["charset"], "utf-8") {
		t.Fatalf("Content-Type = %q", msg.Header.Get("Content-Type"))
	}
	if got := decodeSinglePart(t, raw); got != "hello\r\n" {
		t.Fatalf("body = %q", got)
	}
	if msg.Header.Get("X-CryoBackup-Kind") != "" {
		t.Fatal("no kind, no header")
	}
}

func TestBuildAddsNoRemoteContent(t *testing.T) {
	_, raw := mustBuild(t, Message{From: "a@example.com", To: []string{"b@example.com"}, Subject: "s", Text: "plain\n", HTML: "<p>plain</p>\n"})
	for _, needle := range []string{"http:", "https:", "src=", "<img", "<link", "url(", "@import", "<script"} {
		if bytes.Contains(bytes.ToLower(raw), []byte(needle)) {
			t.Fatalf("the builder added %q", needle)
		}
	}
}

func TestBuildNonASCIIRoundTrip(t *testing.T) {
	subjects := []string{
		"Sauvegarde échouée – 備份 ✓",
		// long enough to need several encoded words
		strings.Repeat("Überprüfung der Sicherung ", 8),
		"\U0001F6A8 alert",
	}
	text := "Grüße aus Köln\n.a line that starts with a dot\n" + strings.Repeat("long ascii line ", 20) + "\ntrailing space \n備份失敗\n"
	html := "<p>Grüße &amp; café =3D literal</p>\n"
	for _, subj := range subjects {
		msg, raw := mustBuild(t, Message{
			From: "Café Console <console@example.com>", To: []string{"Réception <ops@example.com>"},
			Subject: subj, Text: text, HTML: html,
		})
		got, err := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject"))
		if err != nil || got != subj {
			t.Fatalf("subject round trip = %q, %v; want %q", got, err, subj)
		}
		// The header block itself is pure ASCII.
		hdr := raw[:bytes.Index(raw, []byte("\r\n\r\n"))]
		if !isASCII(string(hdr)) {
			t.Fatalf("a header carries raw non-ASCII: %q", hdr)
		}
		from, err := msg.Header.AddressList("From")
		if err != nil || from[0].Name != "Café Console" {
			t.Fatalf("From name = %v, %v", from, err)
		}
		ps := parts(t, msg)
		if ps[0].text != strings.ReplaceAll(text, "\n", "\r\n") || ps[1].text != strings.ReplaceAll(html, "\n", "\r\n") {
			t.Fatalf("body round trip failed:\n%q\n%q", ps[0].text, ps[1].text)
		}
		assertWireLimits(t, raw)
		for _, l := range strings.Split(string(raw), "\r\n") {
			if len(l) > 78 && !strings.HasPrefix(l, "Subject:") && !strings.HasPrefix(l, "From:") && !strings.HasPrefix(l, "To:") && !strings.HasPrefix(l, "Content-Type:") && !strings.HasPrefix(l, " ") {
				t.Fatalf("a body line of %d bytes: quoted-printable must wrap at 76", len(l))
			}
		}
	}
}

func TestBuildFoldsALongASCIISubject(t *testing.T) {
	subj := strings.Repeat("backup status for the fleet ", 8) + "end"
	msg, raw := mustBuild(t, Message{From: "a@example.com", To: []string{"b@example.com"}, Subject: subj, Text: "t\n"})
	if got := msg.Header.Get("Subject"); got != subj {
		t.Fatalf("unfolded subject = %q", got)
	}
	for _, l := range strings.Split(string(raw), "\r\n") {
		if len(l) > 78 {
			t.Fatalf("header line of %d bytes: %q", len(l), l)
		}
	}
}

func TestBuildRejectsHeaderInjection(t *testing.T) {
	inj := []string{"\r\nBcc: evil@example.test", "\nBcc: evil@example.test", "\rBcc: evil@example.test", "\x00", "a\x1bb", "\x7f"}
	for _, bad := range inj {
		base := func() Message {
			return Message{From: "a@example.com", To: []string{"b@example.com"}, Subject: "s", Text: "t", Kind: KindAlert}
		}
		cases := map[string]func(*Message){
			"subject":      func(m *Message) { m.Subject = "hello" + bad },
			"from":         func(m *Message) { m.From = "a@example.com" + bad },
			"from name":    func(m *Message) { m.From = "\"Name" + bad + "\" <a@example.com>" },
			"to":           func(m *Message) { m.To = []string{"b@example.com" + bad} },
			"second to":    func(m *Message) { m.To = []string{"b@example.com", "c@example.com" + bad} },
			"to name":      func(m *Message) { m.To = []string{"\"N" + bad + "\" <b@example.com>"} },
			"kind":         func(m *Message) { m.Kind = Kind("alert" + bad) },
			"subject only": func(m *Message) { m.Subject = bad },
		}
		for name, mod := range cases {
			m := base()
			mod(&m)
			raw, err := m.Build(fixedNow)
			if err == nil {
				t.Fatalf("%s with %q: accepted:\n%s", name, bad, raw)
			}
			if !errors.Is(err, ErrInvalidMessage) {
				t.Fatalf("%s: error does not wrap ErrInvalidMessage: %v", name, err)
			}
			if strings.Contains(err.Error(), "evil") {
				t.Fatalf("%s: the error echoes the value: %v", name, err)
			}
			if m.Validate() == nil {
				t.Fatalf("%s: Validate accepted it", name)
			}
		}
	}
	// A tab is a legal header character.
	if _, err := (Message{From: "a@example.com", To: []string{"b@example.com"}, Subject: "a\tb", Text: "t"}).Build(fixedNow); err != nil {
		t.Fatalf("a tab in the subject: %v", err)
	}
}

func TestBuildRejectsMalformedMessages(t *testing.T) {
	ok := Message{From: "a@example.com", To: []string{"b@example.com"}, Subject: "s", Text: "t"}
	cases := map[string]func(*Message){
		"no from":           func(m *Message) { m.From = "" },
		"bad from":          func(m *Message) { m.From = "not an address" },
		"no recipient":      func(m *Message) { m.To = nil },
		"bad recipient":     func(m *Message) { m.To = []string{"nobody"} },
		"non-ASCII address": func(m *Message) { m.To = []string{"jürgen@example.com"} },
		"empty text":        func(m *Message) { m.Text = " \n" },
		"long subject":      func(m *Message) { m.Subject = strings.Repeat("x", maxSubject+1) },
		"bad kind":          func(m *Message) { m.Kind = "Alert Now" },
	}
	for name, mod := range cases {
		m := ok
		mod(&m)
		if _, err := m.Build(fixedNow); !errors.Is(err, ErrInvalidMessage) {
			t.Errorf("%s: Build = %v", name, err)
		}
	}
	if err := ok.Validate(); err != nil {
		t.Fatalf("a valid message: %v", err)
	}
	// An address with a non-ASCII display name is fine.
	m := ok
	m.To = []string{"Jürgen <j@example.com>"}
	if err := m.Validate(); err != nil {
		t.Fatalf("non-ASCII display name: %v", err)
	}
}

func TestMessageIDsAreUniqueAndDateIsUTC(t *testing.T) {
	m := Message{From: "a@example.com", To: []string{"b@example.com"}, Subject: "s", Text: "t"}
	a, _ := mustBuild(t, m)
	b, _ := mustBuild(t, m)
	if a.Header.Get("Message-ID") == b.Header.Get("Message-ID") {
		t.Fatal("two builds share a Message-ID")
	}
	if got := a.Header.Get("Date"); got != "Wed, 18 Nov 2026 12:30:00 +0000" {
		t.Fatalf("Date = %q", got)
	}
}

// headerLines returns the lines of the header block of a built message.
func headerLines(raw []byte) []string {
	return strings.Split(string(raw[:bytes.Index(raw, []byte("\r\n\r\n"))]), "\r\n")
}

func assertHeaderLines(t *testing.T, raw []byte, max int) {
	t.Helper()
	for _, l := range headerLines(raw) {
		if len(l) > max {
			t.Fatalf("header line of %d octets (limit %d): %q", len(l), max, l)
		}
	}
	if !isASCII(string(raw[:bytes.Index(raw, []byte("\r\n\r\n"))])) {
		t.Fatal("a header carries raw non-ASCII")
	}
}

func TestEnvelopeAddressesMustBePlain(t *testing.T) {
	long := func(n int) string { return strings.Repeat("a", n) }
	bad := []string{
		`"x@evil.example> NOTIFY=NEVER <y"@example.com`,
		`"a b"@example.com`,
		`a b@example.com`,
		`a\b@example.com`,
		`a..b@example.com`,
		`.a@example.com`,
		`a.@example.com`,
		`a@example..com`,
		`a@.example.com`,
		`a@example.com.`,
		`a@-example.com`,
		`a@example-.com`,
		`a@exa_mple.com`,
		`a@[192.0.2.1]`,
		`a@[IPv6:2001:db8::1]`,
		long(65) + `@example.com`,
		`a@` + long(64) + `.com`, // label of 64
	}
	good := []string{
		"a@example.com", "a.b@example.com", "a+tag@example.com", "o'brien@example.com",
		"a=b@example.test", "x@localhost", long(64) + "@example.com", "A-b_c@Sub-Domain.example.com",
	}
	base := Message{From: "console@example.com", To: []string{"ops@example.com"}, Subject: "s", Text: "t\n"}
	for _, a := range bad {
		for name, mod := range map[string]func(*Message){
			"to":              func(m *Message) { m.To = []string{a} },
			"second to":       func(m *Message) { m.To = []string{"ops@example.com", a} },
			"to with name":    func(m *Message) { m.To = []string{"Ops <" + a + ">"} },
			"from":            func(m *Message) { m.From = a },
			"from with name":  func(m *Message) { m.From = "Console <" + a + ">" },
			"to quoted-first": func(m *Message) { m.To = []string{a, "ops@example.com"} },
		} {
			m := base
			mod(&m)
			err := m.Validate()
			if !errors.Is(err, ErrInvalidMessage) {
				t.Errorf("%s %q: Validate = %v", name, a, err)
			}
			if raw, err := m.Build(fixedNow); !errors.Is(err, ErrInvalidMessage) {
				t.Errorf("%s %q: Build = %v\n%s", name, a, err, raw)
			}
		}
	}
	for _, a := range good {
		m := base
		m.To, m.From = []string{a}, "Console <"+a+">"
		if err := m.Validate(); err != nil {
			t.Errorf("%q refused: %v", a, err)
		}
	}
	// 254 octets is the limit: "a@", three 63-letter labels with their dots and
	// a fourth label of 60 (254 octets) or 61 (255 octets) letters.
	three := strings.Repeat(long(63)+".", 3)
	for n, wantOK := range map[int]bool{60: true, 61: false} {
		m := base
		m.To = []string{"a@" + three + long(n)}
		if err := m.Validate(); (err == nil) != wantOK {
			t.Errorf("address of %d octets: Validate = %v, want ok=%v", len(m.To[0]), err, wantOK)
		}
	}
}

func TestBuildFoldsLongNonASCIIHeaders(t *testing.T) {
	cjk := strings.Repeat("備", 166)     // 498 bytes
	twoByte := strings.Repeat("é", 250) // 500 bytes: the longest subject
	mixed := strings.Repeat("Sauvegarde échouée sur le poste 備份 ", 13)[:maxSubject]
	for name, subj := range map[string]string{"cjk": cjk, "two-byte": twoByte, "mixed": mixed, "emoji": strings.Repeat("\U0001F6A8", 125)} {
		subj = strings.ToValidUTF8(subj, "")
		msg, raw := mustBuild(t, Message{
			From: "console@example.com", To: []string{"ops@example.com"}, Subject: subj, Text: "t\n", HTML: "<p>t</p>\n",
		})
		got, err := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject"))
		if err != nil || got != subj {
			t.Fatalf("%s: subject round trip failed (%v): %d bytes in, %d bytes out", name, err, len(subj), len(got))
		}
		assertHeaderLines(t, raw, 78)
		assertWireLimits(t, raw)
		for _, l := range headerLines(raw) {
			// every encoded word is at most 75 characters
			for _, w := range strings.Fields(strings.TrimPrefix(l, "Subject:")) {
				if strings.HasPrefix(w, "=?") && len(w) > 75 {
					t.Fatalf("%s: encoded word of %d characters", name, len(w))
				}
			}
		}
	}
	// A long ASCII subject is folded at spaces within 78 octets.
	ascii := strings.Repeat("backup status for the whole fleet ", 14) + "end"
	msg, raw := mustBuild(t, Message{From: "a@example.com", To: []string{"b@example.com"}, Subject: ascii, Text: "t\n", HTML: "<p>t</p>\n"})
	if got := msg.Header.Get("Subject"); got != ascii {
		t.Fatalf("ASCII subject round trip = %q", got)
	}
	assertHeaderLines(t, raw, 78)
}

func TestSubjectOverTheByteLimitIsRefused(t *testing.T) {
	m := Message{From: "a@example.com", To: []string{"b@example.com"}, Subject: strings.Repeat("é", 300), Text: "t\n"}
	if _, err := m.Build(fixedNow); !errors.Is(err, ErrInvalidMessage) {
		t.Fatalf("a 600-byte subject: %v", err)
	}
}

func TestBuildFoldsLongDisplayNames(t *testing.T) {
	names := map[string]string{
		"cjk 128":   strings.Repeat("備", 128),
		"latin 128": strings.Repeat("Ünï ", 32),
		"ascii 128": strings.Repeat("Operations desk ", 8),
		"short":     "Réception",
	}
	for name, dn := range names {
		msg, raw := mustBuild(t, Message{
			From: dn + " <console@example.com>", To: []string{dn + " <ops@example.com>", "Second Person <more@example.com>"},
			Subject: "s", Text: "t\n", HTML: "<p>t</p>\n",
		})
		assertHeaderLines(t, raw, 78)
		from, err := msg.Header.AddressList("From")
		if err != nil || len(from) != 1 || from[0].Name != strings.TrimSpace(dn) || from[0].Address != "console@example.com" {
			t.Fatalf("%s: From = %+v, %v", name, from, err)
		}
		to, err := msg.Header.AddressList("To")
		if err != nil || len(to) != 2 || to[0].Name != strings.TrimSpace(dn) || to[1].Address != "more@example.com" {
			t.Fatalf("%s: To = %+v, %v", name, to, err)
		}
	}
	for name, dn := range map[string]string{
		"129 characters":    strings.Repeat("備", 129),
		"1200 bytes":        strings.Repeat("é", 600),
		"ascii 129":         strings.Repeat("a", 129),
		"to over the bound": strings.Repeat("é", 129),
	} {
		m := Message{From: dn + " <console@example.com>", To: []string{"ops@example.com"}, Subject: "s", Text: "t\n"}
		if name == "to over the bound" {
			m.From, m.To = "console@example.com", []string{dn + " <ops@example.com>"}
		}
		if _, err := m.Build(fixedNow); !errors.Is(err, ErrInvalidMessage) {
			t.Errorf("%s: Build = %v", name, err)
		}
		if strings.Contains(fmt.Sprint(m.Validate()), "é") {
			t.Errorf("%s: the error echoes the value", name)
		}
	}
}

// unfoldedSubject returns the Subject value as RFC 5322 3.2.2 unfolds it: only
// the CRLF is removed (net/textproto trims, which would hide the defects).
func unfoldedSubject(t *testing.T, raw []byte) string {
	t.Helper()
	head := string(raw[:bytes.Index(raw, []byte("\r\n\r\n"))])
	i := strings.Index(head, "\r\nSubject: ")
	if i < 0 {
		t.Fatal("no Subject header")
	}
	var out strings.Builder
	lines := strings.Split(head[i+len("\r\nSubject: "):], "\r\n")
	out.WriteString(lines[0])
	for _, l := range lines[1:] {
		if !strings.HasPrefix(l, " ") && !strings.HasPrefix(l, "\t") {
			break // the next header
		}
		out.WriteString(l)
	}
	return out.String()
}

func TestFoldingNeverMakesAWhitespaceOnlyLine(t *testing.T) {
	ws := regexp.MustCompile(`^[ \t]*$`)
	for n := 1; n <= maxSubject; n++ {
		a := strings.Repeat("a", n)
		for name, subj := range map[string]string{
			"trailing space": a + " ", "trailing spaces": a + "   ", "leading space": " " + a,
			"run at the fold": a + "    b", "words and run": a[:n/2] + " " + a[n/2:] + "   b  ",
		} {
			if len(subj) > maxSubject {
				continue
			}
			raw, err := (Message{From: "a@example.com", To: []string{"b@example.com"}, Subject: subj, Text: "t\n", HTML: "<p>t</p>\n"}).Build(fixedNow)
			if err != nil {
				t.Fatalf("%s n=%d: %v", name, n, err)
			}
			for _, l := range headerLines(raw) {
				if ws.MatchString(l) {
					t.Fatalf("%s n=%d: whitespace-only header line %q", name, n, l)
				}
			}
			if got := unfoldedSubject(t, raw); got != subj {
				t.Fatalf("%s n=%d: unfolded subject %q, want %q", name, n, got, subj)
			}
		}
	}
}

func TestMultipartContentTypeIsFolded(t *testing.T) {
	msg, raw := mustBuild(t, Message{From: "a@example.com", To: []string{"b@example.com"}, Subject: "s", Text: "t\n", HTML: "<p>t</p>\n"})
	assertHeaderLines(t, raw, 78)
	if mt, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type")); err != nil || mt != "multipart/alternative" || params["boundary"] == "" {
		t.Fatalf("Content-Type = %q, %v", msg.Header.Get("Content-Type"), err)
	}
	if len(parts(t, msg)) != 2 {
		t.Fatal("the folded Content-Type no longer parses to two parts")
	}
}
