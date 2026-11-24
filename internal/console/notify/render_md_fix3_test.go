package notify

import (
	"regexp"
	"strings"
	"testing"
)

// P20-MD fix batch 3: Word (the engine of classic Outlook) does not inherit font-family from <body>
// into a table cell; a cell that names no family is drawn in Times New Roman. Every cell that holds
// text names its own stack.

// The two stacks as the templates write them. The sans stack names the console's body face first
// (Open Sans since ADR 0023; mail embeds no font, so a client without it uses the next one).
const (
	sansStack = `'Open Sans','Segoe UI',Arial,sans-serif`
	monoStack = `Consolas,'Courier New',monospace`
)

// tdOwnText walks the HTML part and returns, for every td, its opening tag and the text that belongs
// to it: the text inside the td but outside any td nested within it (a cell that only holds a table
// has none). Tags are dropped; character references are counted as text. The head is skipped.
func tdOwnText(h string) (tags []string, texts []string) {
	tagRE := regexp.MustCompile(`<(/?)([a-zA-Z][a-zA-Z0-9]*)\b[^>]*>`)
	var stack []int // indices into tags of the open tds
	var own []string
	body := h
	if i := strings.Index(body, "</head>"); i >= 0 {
		body = body[i:]
	}
	// Outlook reads the markup inside its conditional comments (the ghost table): keep that markup,
	// drop the comment delimiters and any other comment.
	body = strings.NewReplacer("<!--[if mso]>", "", "<![endif]-->", "").Replace(body)
	body = regexp.MustCompile(`(?s)<!--.*?-->`).ReplaceAllString(body, "")
	pos := 0
	for _, m := range tagRE.FindAllStringSubmatchIndex(body, -1) {
		if len(stack) > 0 {
			own[stack[len(stack)-1]] += body[pos:m[0]]
		}
		pos = m[1]
		closing := body[m[2]:m[3]] == "/"
		name := strings.ToLower(body[m[4]:m[5]])
		if name != "td" {
			continue
		}
		if closing {
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
			continue
		}
		tags = append(tags, body[m[0]:m[1]])
		own = append(own, "")
		stack = append(stack, len(tags)-1)
	}
	return tags, own
}

// N2: in every rendered message, every td whose own text is not empty names a font family (the sans
// or the mono stack) in its inline style.
func TestEveryTextCellNamesItsFont(t *testing.T) {
	famRE := regexp.MustCompile(`style="[^"]*font-family:([^;"]*)`)
	stacks := map[string]bool{sansStack: true, monoStack: true}
	for name, c := range mdRendered(t) {
		tags, texts := tdOwnText(c.HTML)
		if len(tags) < 10 {
			t.Fatalf("%s: only %d cells found", name, len(tags))
		}
		checked := 0
		for i, tag := range tags {
			if strings.TrimSpace(texts[i]) == "" {
				continue
			}
			checked++
			m := famRE.FindStringSubmatch(tag)
			if m == nil {
				t.Errorf("%s: a cell with text %q names no font family: %s", name, strings.Join(strings.Fields(texts[i]), " "), tag)
				continue
			}
			if !stacks[m[1]] {
				t.Errorf("%s: a cell names the family %q, not the sans or the mono stack: %s", name, m[1], tag)
			}
		}
		if checked < 5 {
			t.Errorf("%s: only %d cells with text", name, checked)
		}
	}
}

// N1: the two stacks are written once each (the shell's sans and mono defines); every font-family in
// a message is one of them, and every a or span that sets a font property of its own names one too.
func TestFontStacksComeFromOnePlace(t *testing.T) {
	for _, s := range []string{sansStack, monoStack} {
		if n := strings.Count(shellSource, s); n != 1 {
			t.Errorf("the shell writes the stack %q %d times, want once", s, n)
		}
		for name, src := range map[string]string{"message": htmlSource, "digest": digestHTMLSource, "test": testMailSource} {
			if strings.Contains(src, s) {
				t.Errorf("the %s template writes the stack %q itself", name, s)
			}
		}
	}
	famRE := regexp.MustCompile(`font-family:([^;"]*)`)
	inlineRE := regexp.MustCompile(`<(a|span)\b[^>]*style="([^"]*)"`)
	for name, c := range mdRendered(t) {
		for _, m := range famRE.FindAllStringSubmatch(c.HTML, -1) {
			if m[1] != sansStack && m[1] != monoStack {
				t.Errorf("%s: a font-family that is neither stack: %q", name, m[1])
			}
		}
		for _, m := range inlineRE.FindAllStringSubmatch(c.HTML, -1) {
			style := m[2]
			if regexp.MustCompile(`(^|;)font-(size|weight)\s*:`).MatchString(style) && !strings.Contains(style, "font-family:") {
				t.Errorf("%s: an <%s> sets a font property without its family: %s", name, m[1], style)
			}
		}
		if !strings.Contains(c.HTML, `<body bgcolor=`) || !regexp.MustCompile(`<body [^>]*font-family:`+regexp.QuoteMeta(sansStack)).MatchString(c.HTML) {
			t.Errorf("%s: the body lost the sans stack", name)
		}
	}
}
