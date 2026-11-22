package notify

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// P20-MD fix batch 2: what classic Outlook (the Word engine) did to the samples on the operator's
// machine. Vertical space sat in div padding, which Word ignores; the URL beside the button was left
// as text, which Outlook auto-links in its own blue; the state chips were clipped.

// M1: no div or p carries padding or margin (Word ignores both); every block that needs room is a
// row of the body table whose td carries it, in the rhythm the operator asked for.
func TestVerticalSpaceIsOnTableCells(t *testing.T) {
	boxRE := regexp.MustCompile(`(?i)<(div|p)\b[^>]*style="[^"]*(padding|margin)[^"]*"`)
	for name, c := range mdRendered(t) {
		h := c.HTML
		if m := boxRE.FindString(h); m != "" {
			t.Errorf("%s: vertical space on a div or p: %s", name, m)
		}
		if strings.Contains(strings.ToLower(h), "<p") {
			t.Errorf("%s: a <p> (Outlook gives it its own margins)", name)
		}
		var wants []string
		switch {
		case strings.HasPrefix(name, "digest"):
			wants = []string{`<td style="padding:18px 0 8px 0;[^"]*">Fleet</td>`, `<td style="padding:18px 0 8px 0;[^"]*">Console</td>`,
				`<td style="padding:18px 0 16px 0;[^"]*"><span style="font-weight:600;[^"]*">Totals:</span>`}
		case strings.HasPrefix(name, "alert"):
			wants = []string{`<td style="padding:18px 0 8px 0;[^"]*">Alert</td>`, `<td style="padding:14px 0 6px 0;[^"]*">Summit Eye Care</td>`,
				`<td style="padding:18px 0 16px 0;[^"]*">Each alarm is mailed once`}
		case name == "recovery":
			wants = []string{`<td style="padding:18px 0 8px 0;[^"]*">Recovery</td>`, `<td style="padding:18px 0 16px 0;[^"]*">These alarms have ended`}
		case strings.HasPrefix(name, "test"):
			wants = []string{`<td style="padding:14px 0 8px 0;[^"]*">This is a test message`}
		}
		if strings.Contains(name, "without a link") {
			if !strings.HasPrefix(name, "test") {
				wants = append(wants, `<tr><td style="padding:0 0 20px 0;[^"]*">Open the console to see the details.</td></tr>`)
			} else {
				wants = append(wants, `<td style="padding:0 0 20px 0;[^"]*">It shows that the console`)
			}
		} else {
			wants = append(wants, `<tr><td style="padding:0 0 20px 0;">\s*<table[^>]*><tr>\s*<td bgcolor="#5CC9F5"`)
		}
		wants = append(wants, `<tr><td class="cb-pad" bgcolor="#182642" style="padding:16px 20px 18px 20px;`)
		for _, w := range wants {
			if !regexp.MustCompile(w).MatchString(h) {
				t.Errorf("%s: no %s", name, w)
			}
		}
	}
}

// M2: the URL beside the button is a link in the accent colour with no underline, set on the a and
// on a span inside it (Outlook colours the span); no other address or URL is shown as text.
func TestTheURLBesideTheButtonIsAnAccentLink(t *testing.T) {
	acc := tokens["accent"]
	link := regexp.MustCompile(`<a href="` + regexp.QuoteMeta(mdURL) + `" style="color:` + acc + `;text-decoration:none;[^"]*"><span style="color:` + acc + `;text-decoration:none;">` + regexp.QuoteMeta(mdURL) + `</span></a>`)
	for name, c := range mdRendered(t) {
		h := c.HTML
		withLink := !strings.Contains(name, "without a link")
		if withLink && len(link.FindAllString(h, -1)) != 1 {
			t.Errorf("%s: the URL beside the button is not one accent link with an accent span", name)
		}
		// Shown as text, the URL appears only inside that span.
		visible := regexp.MustCompile(`<[^>]*>`).ReplaceAllString(h, "\x00")
		want := 0
		if withLink {
			want = 1
		}
		if n := strings.Count(visible, mdURL); n != want {
			t.Errorf("%s: the console URL is shown as text %d times, want %d", name, n, want)
		}
		plain := plainOf(h)
		for _, auto := range []string{"@", "www.", "http:", "ftp:", "mailto:"} {
			if strings.Contains(strings.ReplaceAll(plain, mdURL, ""), auto) {
				t.Errorf("%s: text a mail client may auto-link (%q) outside the console link", name, auto)
			}
		}
	}
}

// M3: a chip's cell has an explicit height and an exact line height with room for the word's descent
// (Word's render: 14px exact cut the underscore of IDENTITY_CONFLICT), and the cell that holds it has
// the vertical padding of its text cell (Word offsets the row's border otherwise). The
// acknowledgement tag has no line height of its own.
func TestChipsLeaveRoomForTheWord(t *testing.T) {
	chipRE := regexp.MustCompile(`<td bgcolor="#[0-9A-F]{6}" height="(\d+)" nowrap style="([^"]*border-radius:999px[^"]*)">`)
	holderRE := regexp.MustCompile(`<td width="1%" valign="top" style="width:1%;padding:9px 0 9px 12px;white-space:nowrap;[^"]*">`)
	textRE := regexp.MustCompile(`</table></td>\s*<td valign="top" style="padding:9px 12px;`)
	num := func(style, prop string) float64 {
		m := regexp.MustCompile(`(?:^|;)` + prop + `:([0-9.]+)px`).FindStringSubmatch(style)
		if m == nil {
			return -1
		}
		v, _ := strconv.ParseFloat(m[1], 64)
		return v
	}
	for name, c := range mdRendered(t) {
		h := c.HTML
		chips := chipRE.FindAllStringSubmatch(h, -1)
		if want := strings.Count(h, "border-radius:999px"); len(chips) != want || (want == 0) != strings.HasPrefix(name, "test") {
			t.Errorf("%s: %d chips in the expected form of %d", name, len(chips), want)
		}
		if n := len(holderRE.FindAllString(h, -1)); n != len(chips) {
			t.Errorf("%s: %d chip holders with the text cell's padding for %d chips", name, n, len(chips))
		}
		if n := len(textRE.FindAllString(h, -1)); n != len(chips) {
			t.Errorf("%s: %d text cells padded 9px 12px beside %d chips", name, n, len(chips))
		}
		for _, m := range chips {
			style := m[2]
			height, _ := strconv.Atoi(m[1])
			lh, fs := num(style, "line-height"), num(style, "font-size")
			if !strings.Contains(style, "mso-line-height-rule:exactly") || num(style, "height") != float64(height) {
				t.Errorf("%s: a chip without its exact line rule or matching height: %s", name, style)
			}
			// The height is the content height (padding and border add to it): at least the line.
			if lh < fs*1.5 || float64(height) < lh || !strings.Contains(style, "padding:2px ") {
				t.Errorf("%s: a chip's line height %vpx for a %vpx word in a %dpx cell leaves no room: %s", name, lh, fs, height, style)
			}
		}
		for _, tag := range regexp.MustCompile(`<span style="[^"]*border-radius:4px[^"]*">`).FindAllString(h, -1) {
			if strings.Contains(tag, "line-height") {
				t.Errorf("%s: the acknowledgement tag sets its own line height: %s", name, tag)
			}
		}
	}
}

// M4: classic Outlook's housekeeping: the VML and Office namespaces on <html>, the document settings
// (PNG allowed, 96 pixels per inch) in the head, and the button a table cell (no VML) whose padding
// Outlook keeps and whose text does not wrap.
func TestOutlookHousekeeping(t *testing.T) {
	button := regexp.MustCompile(`<td bgcolor="#5CC9F5" nowrap style="[^"]*padding:9px 16px;mso-padding-alt:9px 16px;white-space:nowrap;"><a href="[^"]*" style="[^"]*white-space:nowrap;">Open the console</a></td>`)
	for name, c := range mdRendered(t) {
		h := c.HTML
		if !strings.Contains(h, `<html lang="en" xmlns:v="urn:schemas-microsoft-com:vml" xmlns:o="urn:schemas-microsoft-com:office:office">`) {
			t.Errorf("%s: <html> lacks the VML and Office namespaces", name)
		}
		head := h[:max(strings.Index(h, "</head>"), 0)]
		if strings.Count(head, string(msoHead)) != 1 || !strings.Contains(string(msoHead), "<o:AllowPNG/><o:PixelsPerInch>96</o:PixelsPerInch>") {
			t.Errorf("%s: the head lacks Outlook's document settings block", name)
		}
		if strings.Contains(h, "<v:") {
			t.Errorf("%s: VML (Outlook's dark mode recolours its text)", name)
		}
		if n := len(button.FindAllString(h, -1)); n != map[bool]int{true: 0, false: 1}[strings.Contains(name, "without a link")] {
			t.Errorf("%s: %d buttons in the table-cell form with mso-padding-alt and nowrap", name, n)
		}
	}
}
