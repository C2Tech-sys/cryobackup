package notify

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"html"
	"image/png"
	"io"
	"math"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	netmail "net/mail"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
	"github.com/c2tech-sys/cryobackup/internal/console/health"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// P20-MD (ADR 0022): the four messages in the console's dark look, with the mark inline.

const mdURL = "https://console.example.com/"

// mdRendered is every kind of message over the test fixtures, by name.
func mdRendered(t *testing.T) map[string]Content {
	t.Helper()
	out := map[string]Content{}
	add := func(name string, c Content, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		out[name] = c
	}
	o := Options{Loc: cst, ConsoleURL: mdURL}
	c, err := RenderAlert(renderView(), alertBatch(), o)
	add("alert", c, err)
	c, err = RenderRecovery(renderView(), recoveryBatch(), o)
	add("recovery", c, err)
	c, err = RenderAlert(renderView(), alertBatch(), Options{Loc: cst})
	add("alert without a link", c, err)
	c, err = RenderDigest(greenView(3), nil, o)
	add("digest green", c, err)
	v, acks := mixedView()
	c, err = RenderDigest(v, acks, o)
	add("digest mixed", c, err)
	c, err = RenderDigest(bigView(), nil, Options{Loc: cst})
	add("digest 300 without a link", c, err)
	tm := &TestMailer{ConsoleURL: mdURL, Loc: func() *time.Location { return cst }}
	m := tm.message(renderNow, "ops@example.com")
	add("test", Content{Kind: m.Kind, Subject: m.Subject, Text: m.Text, HTML: m.HTML, Inline: m.Inline}, nil)
	tm.ConsoleURL = ""
	m = tm.message(renderNow, "ops@example.com")
	add("test without a link", Content{Kind: m.Kind, Subject: m.Subject, Text: m.Text, HTML: m.HTML, Inline: m.Inline}, nil)
	return out
}

func TestMarkIsThe64PixelPNG(t *testing.T) {
	cfg, err := png.DecodeConfig(bytes.NewReader(MarkPNG()))
	if err != nil {
		t.Fatalf("the embedded mark is not a PNG: %v", err)
	}
	if cfg.Width != 64 || cfg.Height != 64 {
		t.Fatalf("the mark is %dx%d, want 64x64", cfg.Width, cfg.Height)
	}
	if len(markPNG) > 64<<10 {
		t.Fatalf("the mark is %d bytes: over the inline cap", len(markPNG))
	}
	b := MarkPNG()
	b[0] ^= 0xff
	if markPNG[0] == b[0] {
		t.Fatal("MarkPNG hands out the embedded slice itself")
	}
}

// Every message carries the mark inside: one inline image whose content id is the cid: of the HTML
// part (written as cid:, never rewritten by html/template), and whose bytes are the embedded file.
func TestEveryMessageCarriesTheMarkInline(t *testing.T) {
	for name, c := range mdRendered(t) {
		if len(c.Inline) != 1 || c.Inline[0].ContentID != MarkContentID || c.Inline[0].MediaType != "image/png" || !bytes.Equal(c.Inline[0].Data, markPNG) {
			t.Errorf("%s: inline images %d", name, len(c.Inline))
			continue
		}
		if strings.Count(c.HTML, `<img `) != 1 || strings.Count(c.HTML, `src="cid:`+MarkContentID+`"`) != 1 {
			t.Errorf("%s: the HTML does not show the mark once by its content id", name)
		}
		if strings.Contains(c.HTML, "ZgotmplZ") {
			t.Errorf("%s: html/template rewrote a value (#ZgotmplZ)", name)
		}
		m := c.Message("ops@example.com")
		m.From = "CryoBackup <console@example.com>"
		raw, err := m.Build(renderNow)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		htmlPart, img := mdParse(t, raw)
		if string(img.data) != string(markPNG) || img.cid != "<"+MarkContentID+">" {
			t.Errorf("%s: the image part is not the mark under its content id (%q)", name, img.cid)
		}
		if !strings.Contains(htmlPart, `src="cid:`+strings.Trim(img.cid, "<>")+`"`) {
			t.Errorf("%s: the HTML part does not point at the image part", name)
		}
		if htmlPart != strings.ReplaceAll(c.HTML, "\n", "\r\n") {
			t.Errorf("%s: the HTML part is not the rendered HTML", name)
		}
	}
}

type mdImage struct {
	cid  string
	data []byte
}

// mdParse reads a built message: multipart/alternative { text/plain, multipart/related { text/html,
// image/png } }. It returns the decoded HTML and the image.
func mdParse(t *testing.T, raw []byte) (string, mdImage) {
	t.Helper()
	msg, err := netmail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	mt, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/alternative" {
		t.Fatalf("top Content-Type %q", msg.Header.Get("Content-Type"))
	}
	alt := multipart.NewReader(msg.Body, params["boundary"])
	if p, err := alt.NextRawPart(); err != nil || !strings.HasPrefix(p.Header.Get("Content-Type"), "text/plain") {
		t.Fatalf("first part: %v", err)
	}
	rel, err := alt.NextRawPart()
	if err != nil {
		t.Fatal(err)
	}
	rt, rp, err := mime.ParseMediaType(rel.Header.Get("Content-Type"))
	if err != nil || rt != "multipart/related" || rp["type"] != "text/html" {
		t.Fatalf("second part %q", rel.Header.Get("Content-Type"))
	}
	rr := multipart.NewReader(rel, rp["boundary"])
	hp, err := rr.NextRawPart()
	if err != nil || !strings.HasPrefix(hp.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("related first part: %v", err)
	}
	hb, err := io.ReadAll(quotedprintable.NewReader(hp))
	if err != nil {
		t.Fatal(err)
	}
	ip, err := rr.NextRawPart()
	if err != nil {
		t.Fatal(err)
	}
	if ip.Header.Get("Content-Transfer-Encoding") != "base64" || ip.Header.Get("Content-Disposition") != `inline; filename="cryobackup-mark.png"` {
		t.Fatalf("image part headers %v", ip.Header)
	}
	ib, err := io.ReadAll(ip)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range strings.Split(strings.TrimSpace(string(ib)), "\r\n") {
		if len(l) > 76 {
			t.Fatalf("a base64 line of %d characters", len(l))
		}
	}
	data, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(string(ib), "\r\n", ""))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rr.NextRawPart(); err != io.EOF {
		t.Fatalf("more than one image: %v", err)
	}
	if _, err := alt.NextRawPart(); err != io.EOF {
		t.Fatalf("more than two alternatives: %v", err)
	}
	for _, l := range strings.Split(string(raw), "\r\n") {
		if len(l) > 998 {
			t.Fatalf("a line of %d bytes", len(l))
		}
	}
	return string(hb), mdImage{cid: ip.Header.Get("Content-Id"), data: data}
}

var (
	srcAttrRE  = regexp.MustCompile(`(?i)\ssrc\s*=\s*"([^"]*)"`)
	hrefAttrRE = regexp.MustCompile(`(?i)\shref\s*=\s*"([^"]*)"`)
	bgAttrRE   = regexp.MustCompile(`(?i)\sbackground\s*=`)
	bgcolorRE  = regexp.MustCompile(`(?i)\sbgcolor\s*=\s*"([^"]*)"`)
	hex6RE     = regexp.MustCompile(`^#[0-9A-Fa-f]{6}$`)
)

// No part of any message references a remote resource: the only src is the mark's cid:, there is no
// background attribute, url(), @import or <link>, and the only absolute URL is the console's link.
func TestNoMessageReferencesARemoteResource(t *testing.T) {
	for name, c := range mdRendered(t) {
		h := c.HTML
		for _, m := range srcAttrRE.FindAllStringSubmatch(h, -1) {
			if !strings.HasPrefix(m[1], "cid:") || strings.Contains(m[1], "//") {
				t.Errorf("%s: src %q", name, m[1])
			}
		}
		if bgAttrRE.MatchString(h) {
			t.Errorf("%s: a background attribute", name)
		}
		low := strings.ToLower(h)
		for _, bad := range []string{"url(", "@import", "<link", "<script", "<iframe", "<object", "<embed", "@font-face"} {
			if strings.Contains(low, bad) {
				t.Errorf("%s: %q", name, bad)
			}
		}
		hrefs := hrefAttrRE.FindAllStringSubmatch(h, -1)
		for _, m := range hrefs {
			if m[1] != mdURL {
				t.Errorf("%s: a link to %q", name, m[1])
			}
		}
		if wantLink := !strings.Contains(name, "without a link"); (len(hrefs) == 1) != wantLink {
			t.Errorf("%s: %d links", name, len(hrefs))
		}
		if rest := strings.ReplaceAll(low, mdURL, ""); strings.Contains(rest, "http:") || strings.Contains(rest, "https:") || strings.Contains(rest, `="//`) {
			t.Errorf("%s: an absolute URL other than the console's", name)
		}
	}
}

// The markup is what classic Outlook lays out: every bgcolor a 6-digit hex value, set with a
// background in the same tag's style; no flex, grid, position, float, CSS variables or background
// images; one <style> block, the phone-width media query; the dark colour scheme declared.
func TestTemplatesAreOutlookSafe(t *testing.T) {
	tagRE := regexp.MustCompile(`<[a-z]+\s[^>]*bgcolor="[^"]*"[^>]*>`)
	for name, c := range mdRendered(t) {
		h := c.HTML
		bgs := bgcolorRE.FindAllStringSubmatch(h, -1)
		if len(bgs) < 5 {
			t.Errorf("%s: only %d bgcolor attributes", name, len(bgs))
		}
		for _, m := range bgs {
			if !hex6RE.MatchString(m[1]) {
				t.Errorf("%s: bgcolor %q", name, m[1])
			}
		}
		for _, tag := range tagRE.FindAllString(h, -1) {
			bg := bgcolorRE.FindStringSubmatch(tag)[1]
			if !strings.Contains(tag, "background:"+bg) {
				t.Errorf("%s: a bgcolor without the same background in its style: %s", name, tag)
			}
		}
		low := strings.ToLower(h)
		for _, bad := range []string{"display:flex", "display:grid", "position:", "float:", "var(--", "background-image", "linear-gradient"} {
			if strings.Contains(low, bad) {
				t.Errorf("%s: %q", name, bad)
			}
		}
		if strings.Count(low, "<style") != 1 {
			t.Errorf("%s: %d style blocks", name, strings.Count(low, "<style"))
		}
		block := styleBlock.FindString(h)
		inner := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(block), "<style>"), "</style>"))
		if !strings.HasPrefix(inner, "@media only screen and (max-width:480px){") || strings.Count(inner, "@") != 1 {
			t.Errorf("%s: the style block is not only the phone media query:\n%s", name, block)
		}
		for _, want := range []string{`<meta name="color-scheme" content="dark">`, `<meta name="supported-color-schemes" content="dark">`, `width="28" height="28"`, `CRYO<span`, `>BACKUP</span>`} {
			if !strings.Contains(h, want) {
				t.Errorf("%s: missing %q", name, want)
			}
		}
		for _, font := range []string{"Orbitron", "Martian", "fonts.googleapis"} {
			if strings.Contains(h, font) {
				t.Errorf("%s: names the font %q", name, font)
			}
		}
	}
}

// A customer or host name carrying markup, quotes and an ampersand is escaped in every new template.
func TestHostileNamesAreEscaped(t *testing.T) {
	const cust = `Summit <script>alert("x")</script> & 'Co'`
	const host = `"><img src=x onerror=alert(1)>`
	v := renderView()
	v.Customers[0].Name = cust
	v.Devices[0].Hostname = host
	mv, acks := mixedView()
	mv.Customers[0].Name = cust
	mv.Devices[0].Hostname = host
	var out []string
	for _, f := range []func() (Content, error){
		func() (Content, error) { return RenderAlert(v, alertBatch(), Options{ConsoleURL: mdURL}) },
		func() (Content, error) { return RenderRecovery(v, recoveryBatch(), Options{ConsoleURL: mdURL}) },
		func() (Content, error) { return RenderDigest(mv, acks, Options{ConsoleURL: mdURL}) },
	} {
		c, err := f()
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, c.HTML)
	}
	for i, h := range out {
		if strings.Contains(h, "<script") || strings.Contains(h, `<img src=x`) {
			t.Errorf("message %d: a name was not escaped", i)
		}
		if !strings.Contains(h, "&lt;script&gt;alert(&#34;x&#34;)&lt;/script&gt; &amp; &#39;Co&#39;") {
			t.Errorf("message %d: the customer name is not in its escaped form", i)
		}
		if i != 1 && !strings.Contains(h, "&#34;&gt;&lt;img src=x onerror=alert(1)&gt;") { // the recovery batch does not name that device
			t.Errorf("message %d: the host name is not in its escaped form", i)
		}
	}
}

// plainOf is the visible text of an HTML part: tags dropped, entities decoded, white space folded.
func plainOf(h string) string {
	h = regexp.MustCompile(`(?s)<head>.*?</head>`).ReplaceAllString(h, " ")
	h = regexp.MustCompile(`<[^>]*>`).ReplaceAllString(h, " ")
	return strings.Join(strings.Fields(html.UnescapeString(h)), " ")
}

func folded(s string) string { return strings.Join(strings.Fields(s), " ") }

// Every fact of a message is still in its HTML part: every word of the text part (its bracket
// marks aside), and the sentences, times with their zone, totals and footer lines whole.
func TestEveryFactOfTheTextPartIsInTheHTML(t *testing.T) {
	trim := func(w string) string { return strings.ToLower(strings.Trim(w, "[]:,.;()")) }
	for name, c := range mdRendered(t) {
		plain := plainOf(c.HTML)
		words := map[string]bool{}
		for _, w := range strings.Fields(plain) {
			words[trim(w)] = true
		}
		for _, w := range strings.Fields(c.Text) {
			if tw := trim(w); tw != "" && !words[tw] {
				t.Errorf("%s: the word %q of the text part is not in the HTML", name, w)
			}
		}
		// Whole lines and sentences, as the text part has them (folded).
		var wants []string
		sc := bufio.NewScanner(strings.NewReader(c.Text))
		for sc.Scan() {
			l := strings.TrimSpace(sc.Text())
			switch {
			case strings.HasPrefix(l, "Totals: "), strings.HasPrefix(l, "None. "), strings.HasPrefix(l, "No console condition"):
				wants = append(wants, l)
			case strings.HasPrefix(l, "Polled "):
				wants = append(wants, "p"+l[1:])
			}
		}
		for _, s := range []string{
			"These alarms have ended. No further mail is sent about them.",
			"Each alarm is mailed once while it lasts. Acknowledge or snooze it in the console to mute it; a recovery notice follows when it ends.",
			"This digest is sent every day, also when everything is green: a day without it means the console or its mail has stopped.",
			"You receive this because this address receives CryoBackup recovery notices.",
			"You receive this because this address receives CryoBackup alerts.",
			"You receive this because this address receives the CryoBackup daily digest.",
			"You receive this because an administrator of the console asked for a test message to this address.",
			"It shows that the console can deliver e-mail to this address through its relay. Nothing needs to be done.",
			"Open the console to see the details.",
			"(suspended: notifications muted)", "paused with no end date",
		} {
			if strings.Contains(folded(c.Text), s) {
				wants = append(wants, s)
			}
		}
		for _, tm := range regexp.MustCompile(`(since|ended|sent) ([A-Z][a-z]{2} \d{1,2} [A-Z][a-z]{2} \d{4}, \d{1,2}:\d{2} [AP]M [A-Z]{3,4})`).FindAllString(folded(c.Text), -1) {
			wants = append(wants, tm)
		}
		if strings.Contains(c.Text, "Open the console: ") {
			wants = append(wants, "Open the console "+mdURL)
		}
		if len(wants) < 3 {
			t.Errorf("%s: only %d whole facts found in the text part: the test proves little", name, len(wants))
		}
		missing := 0
		for _, w := range wants {
			if !strings.Contains(plain, w) {
				t.Errorf("%s: the HTML part lacks %q", name, w)
				missing++
			}
		}
		if missing > 0 {
			t.Logf("%s HTML text:\n%s", name, plain)
		}
	}
}

// The digest's four tiles are the numbers of its totals line: backed up, the critical and warning
// device counts together, paused, and devices.
func TestDigestTilesAreTheTotalsNumbers(t *testing.T) {
	tileRE := regexp.MustCompile(`font-size:22px;[^"]*">(\d+)</div>\s*<div[^>]*>([A-Za-z ]+)</div>`)
	totalsRE := regexp.MustCompile(`Totals: (\d+) devices?: (\d+) backed up(.*?)\. Exceptions: (\d+) critical, (\d+) warnings?,`)
	pausedRE := regexp.MustCompile(`, (\d+) paused`)
	mv, acks := mixedView()
	paused := greenView(4)
	paused.Devices[1].Primary, paused.Devices[1].CurrentBackup = state.StatePaused, false
	paused.Devices[2].Primary, paused.Devices[2].CurrentBackup = state.StatePaused, false
	for name, f := range map[string]func() (Content, error){
		"green":  func() (Content, error) { return RenderDigest(greenView(3), nil, Options{}) },
		"mixed":  func() (Content, error) { return RenderDigest(mv, acks, Options{}) },
		"300":    func() (Content, error) { return RenderDigest(bigView(), nil, Options{}) },
		"paused": func() (Content, error) { return RenderDigest(paused, nil, Options{}) },
	} {
		c, err := f()
		if err != nil {
			t.Fatal(err)
		}
		tm := totalsRE.FindStringSubmatch(folded(c.Text))
		if tm == nil {
			t.Fatalf("%s: no totals line in %q", name, c.Text)
		}
		n := func(s string) int { v, _ := strconv.Atoi(s); return v }
		p := 0
		if pm := pausedRE.FindStringSubmatch(tm[3]); pm != nil {
			p = n(pm[1])
		}
		want := []string{fmt.Sprintf("%d Backed up", n(tm[2])), fmt.Sprintf("%d Attention", n(tm[4])+n(tm[5])), fmt.Sprintf("%d Paused", p), fmt.Sprintf("%d Devices", n(tm[1]))}
		var got []string
		for _, m := range tileRE.FindAllStringSubmatch(c.HTML, -1) {
			got = append(got, m[1]+" "+m[2])
		}
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("%s: tiles %v, want %v (from %q)", name, got, want, tm[0])
		}
		if name == "paused" && p != 2 {
			t.Errorf("paused: the totals line counts %d paused, want 2", p)
		}
	}
}

// The headline strip takes the level of the day: green only for an all clear.
func TestDigestStripLevel(t *testing.T) {
	strip := func(c Content) string {
		m := regexp.MustCompile(`<tr><td class="cb-pad" bgcolor="(#[0-9A-F]{6})"[^>]*border-left:4px solid`).FindStringSubmatch(c.HTML)
		if m == nil {
			t.Fatalf("no strip in %s", c.Subject)
		}
		return m[1]
	}
	green, _ := RenderDigest(greenView(3), nil, Options{})
	mv, acks := mixedView()
	mixed, _ := RenderDigest(mv, acks, Options{})
	none, _ := RenderDigest(state.FleetView{ComputedAt: renderNow, Complete: true}, nil, Options{})
	if strip(green) != tokens["success-soft"] || strip(mixed) != tokens["danger-soft"] || strip(none) != tokens["warning-soft"] {
		t.Fatalf("strip colours: green %s, mixed %s, no devices %s", strip(green), strip(mixed), strip(none))
	}
}

// The dark map is web/static/tokens.css: each hex token equal, each soft (rgba) token that colour
// composited over the surface.
func TestDarkTokensMatchTokensCSS(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "web", "static", "tokens.css"))
	if err != nil {
		t.Fatal(err)
	}
	css := string(b)
	start := strings.Index(css, ":root {")
	end := strings.Index(css[start:], "}")
	if start < 0 || end < 0 {
		t.Fatal("no :root block in tokens.css")
	}
	root := css[start : start+end] // the first :root block: the dark (default) theme
	decl := regexp.MustCompile(`--([a-z-]+):\s*([^;]+);`)
	dark := map[string]string{}
	for _, m := range decl.FindAllStringSubmatch(root, -1) {
		dark[m[1]] = strings.TrimSpace(m[2])
	}
	hexOf := func(s string) [3]float64 {
		v, err := strconv.ParseUint(strings.TrimPrefix(s, "#"), 16, 32)
		if err != nil || len(s) != 7 {
			t.Fatalf("not a hex colour: %q", s)
		}
		return [3]float64{float64(v >> 16 & 0xff), float64(v >> 8 & 0xff), float64(v & 0xff)}
	}
	rgbaRE := regexp.MustCompile(`^rgba\((\d+),\s*(\d+),\s*(\d+),\s*([0-9.]+)\)$`)
	for name, got := range tokens {
		css, ok := dark[name]
		if !ok {
			t.Errorf("token %s is not in tokens.css", name)
			continue
		}
		want := strings.ToUpper(css)
		if m := rgbaRE.FindStringSubmatch(css); m != nil {
			surf := hexOf(dark["surface"])
			a, _ := strconv.ParseFloat(m[4], 64)
			var c [3]int
			for i := 0; i < 3; i++ {
				fg, _ := strconv.ParseFloat(m[i+1], 64)
				c[i] = int(math.Round(surf[i] + a*(fg-surf[i])))
			}
			want = fmt.Sprintf("#%02X%02X%02X", c[0], c[1], c[2])
		}
		if strings.ToUpper(got) != want {
			t.Errorf("token %s = %s, tokens.css gives %s (%s)", name, got, want, css)
		}
	}
	for _, need := range []string{"bg", "surface", "surface-raised", "border", "text", "text-muted", "accent", "on-accent", "danger", "warning", "success", "danger-soft", "warning-soft", "success-soft", "accent-soft"} {
		if _, ok := tokens[need]; !ok {
			t.Errorf("the dark map lacks %s", need)
		}
	}
}

// The preview form: the mark's cid: becomes the console's route, the style block goes, nothing else
// changes.
func TestPreviewHTMLPointsTheMarkAtTheRoute(t *testing.T) {
	c, err := RenderDigest(greenView(3), nil, Options{ConsoleURL: mdURL})
	if err != nil {
		t.Fatal(err)
	}
	p := PreviewHTML(c.HTML, "/mail/mark.png")
	if strings.Contains(p, "cid:") || !strings.Contains(p, `src="/mail/mark.png"`) || strings.Contains(p, "<style") {
		t.Fatal("the preview still has the cid: link or the style block")
	}
	back := strings.Replace(p, `src="/mail/mark.png"`, markAttr, 1)
	if back != styleBlock.ReplaceAllString(c.HTML, "") {
		t.Fatal("the preview changed more than the mark's src and the style block")
	}
	if q := PreviewHTML(c.HTML, `/x"onload="y`); strings.Contains(q, `"onload="`) {
		t.Fatal("the route is not escaped")
	}
}

// A sample writer for the operator (not a gate): with CRYO_MAIL_SAMPLES naming a directory, it writes
// one complete .eml per message, built by mail.Message.Build, to open in a mail client.
func TestWriteMailSamples(t *testing.T) {
	dir := os.Getenv("CRYO_MAIL_SAMPLES")
	if dir == "" {
		t.Skip("set CRYO_MAIL_SAMPLES to a directory to write the sample messages")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	f := fixtureWorld(t)
	o := Options{Loc: cst, ConsoleURL: mdURL}
	var crit, closed []store.Episode
	now := f.View.ComputedAt
	for _, d := range f.View.Devices {
		if IsCritical(string(d.Primary)) && len(crit) < 4 {
			e := store.Episode{ID: d.ID, SubjectType: store.EpisodeDevice, SubjectID: d.ID, CustomerID: d.CustomerID, AlarmType: string(d.Primary), StateSince: d.Since}
			crit = append(crit, e)
			e.ClosedAt = &now
			closed = append(closed, e)
		}
	}
	if len(crit) == 0 {
		t.Fatal("the fixture world has no critical device")
	}
	samples := []struct {
		file string
		f    func() (Content, error)
	}{
		{"1-alert.eml", func() (Content, error) { return RenderAlert(f.View, crit, o) }},
		{"2-recovery.eml", func() (Content, error) { return RenderRecovery(f.View, closed[:2], o) }},
		{"3-digest-problems.eml", func() (Content, error) { return RenderDigest(f.View, f.Acks, o) }},
		{"4-digest-all-green.eml", func() (Content, error) {
			g := greenView(12)
			g.ComputedAt = now
			return RenderDigest(g, nil, o)
		}},
		{"5-test-message.eml", func() (Content, error) {
			m := (&TestMailer{ConsoleURL: mdURL, Loc: func() *time.Location { return cst }}).message(now, "ops@example.com")
			return Content{Kind: m.Kind, Subject: m.Subject, Text: m.Text, HTML: m.HTML, Inline: m.Inline}, nil
		}},
	}
	for _, s := range samples {
		c, err := s.f()
		if err != nil {
			t.Fatalf("%s: %v", s.file, err)
		}
		m := c.Message("ops@example.com")
		m.From = "CryoBackup <console@example.com>"
		raw, err := m.Build(now)
		if err != nil {
			t.Fatalf("%s: %v", s.file, err)
		}
		mdParse(t, raw)
		if err := os.WriteFile(filepath.Join(dir, s.file), raw, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s (%d bytes): %s", s.file, len(raw), c.Subject)
	}
}

type mdWorld struct {
	View state.FleetView
	Acks []store.Ack
}

// fixtureWorld is the console's fixture fleet as the evaluator sees it, with its active
// acknowledgements.
func fixtureWorld(t *testing.T) mdWorld {
	t.Helper()
	ctx := context.Background()
	now := fixtures.Now()
	st, err := store.OpenMemory(ctx, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := store.LoadFixtures(ctx, st, now); err != nil {
		t.Fatal(err)
	}
	ev := &health.Evaluator{Store: st, Clock: func() time.Time { return now }, MasterLoaded: func() bool { return true }}
	f, err := ev.Alarms(ctx)
	if err != nil {
		t.Fatal(err)
	}
	acks, err := st.ListActiveAcks(ctx, store.ScopeAll())
	if err != nil {
		t.Fatal(err)
	}
	return mdWorld{View: f.View, Acks: acks}
}
