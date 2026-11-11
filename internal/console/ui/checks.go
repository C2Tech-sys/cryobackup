package ui

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/c2tech-sys/cryobackup/web"
)

// Check is the result of one of the /design page's built-in checks
// (components §12.1): the same functions the repository tests call.
type Check struct {
	Name    string
	Pass    bool
	Summary string
	Details []string
}

var (
	cssCommentRE = regexp.MustCompile(`(?s)/\*.*?\*/`)
	hexColorRE   = regexp.MustCompile(`#[0-9a-fA-F]{3,8}\b`)
	funcColorRE  = regexp.MustCompile(`\b(?:rgba?|hsla?|hwb|lab|lch|oklab|oklch)\(`)
	ruleRE       = regexp.MustCompile(`([^{}]+)\{([^{}]*)\}`)
)

func stripComments(css string) string {
	// Keep newlines so line numbers survive.
	return cssCommentRE.ReplaceAllStringFunc(css, func(m string) string { return strings.Repeat("\n", strings.Count(m, "\n")) })
}

func cssFiles() []string {
	files, _ := fs.Glob(web.Static, "static/*.css")
	sort.Strings(files)
	return files
}

// TokenCoverage greps the component CSS for literal colours: every colour must
// be a token (tokens.css itself is where the values live and is not scanned).
func TokenCoverage() Check {
	c := Check{Name: "Token coverage", Pass: true}
	n := 0
	for _, f := range cssFiles() {
		if strings.HasSuffix(f, "/tokens.css") {
			continue
		}
		b, err := fs.ReadFile(web.Static, f)
		if err != nil {
			continue
		}
		n++
		for i, line := range strings.Split(stripComments(string(b)), "\n") {
			if m := hexColorRE.FindString(line); m != "" {
				c.Details = append(c.Details, fmt.Sprintf("%s:%d literal colour %s", strings.TrimPrefix(f, "static/"), i+1, m))
			} else if m := funcColorRE.FindString(line); m != "" {
				c.Details = append(c.Details, fmt.Sprintf("%s:%d colour function %s", strings.TrimPrefix(f, "static/"), i+1, m))
			}
		}
	}
	c.Pass = len(c.Details) == 0
	c.Summary = fmt.Sprintf("%d stylesheet(s) scanned, %d literal colour(s)", n, len(c.Details))
	return c
}

var orbitronRoles = map[string]bool{
	".t-hero-wordmark": true, ".t-wordmark": true, ".t-page-title": true, ".t-big-number": true, ".t-big-number.sm": true, ".t-eyebrow-chip": true,
}

// OrbitronAudit fails if the display face is referenced anywhere but the five
// .t-* roles (and the @font-face rule and the custom property definition), in
// any stylesheet or template.
// fontPreloadRE finds <link> tags; OrbitronAudit drops the font preloads among them.
var fontPreloadRE = regexp.MustCompile(`<link[^>]*>`)

func OrbitronAudit() Check {
	c := Check{Name: "Orbitron audit", Pass: true}
	for _, f := range cssFiles() {
		b, err := fs.ReadFile(web.Static, f)
		if err != nil {
			continue
		}
		css := stripComments(string(b))
		for _, m := range ruleRE.FindAllStringSubmatch(css, -1) {
			sel, body := strings.TrimSpace(m[1]), m[2]
			if !strings.Contains(body, "--font-display") && !strings.Contains(strings.ToLower(body), "orbitron") {
				continue
			}
			name := strings.TrimPrefix(f, "static/")
			switch {
			case strings.HasPrefix(sel, "@font-face"):
			case sel == ":root" || strings.HasPrefix(sel, ":root"):
				// the custom property definition (tokens.css) is not a use
				for _, decl := range strings.Split(body, ";") {
					d := strings.TrimSpace(decl)
					if (strings.Contains(d, "--font-display") && !strings.HasPrefix(d, "--font-display:")) || (strings.Contains(strings.ToLower(d), "orbitron") && !strings.HasPrefix(d, "--font-display:")) {
						c.Details = append(c.Details, name+": "+d+" in "+sel)
					}
				}
			default:
				ok := true
				for _, part := range strings.Split(sel, ",") {
					if !orbitronRoles[strings.TrimSpace(part)] {
						ok = false
					}
				}
				if !ok {
					c.Details = append(c.Details, name+": display face used by selector "+sel)
				}
			}
		}
	}
	tfiles, _ := fs.Glob(web.Templates, "templates/*.html")
	more, _ := fs.Glob(web.Templates, "templates/*/*.html")
	for _, f := range append(tfiles, more...) {
		b, err := fs.ReadFile(web.Templates, f)
		if err != nil {
			continue
		}
		// A font preload (<link rel="preload" as="font">) names the file so the browser
		// fetches it early: a fetch hint, not a use of the face. Only that tag is left
		// out of the scan; the rest of its line is still checked.
		t := fontPreloadRE.ReplaceAllStringFunc(strings.ToLower(string(b)), func(tag string) string {
			if strings.Contains(tag, `rel="preload"`) && strings.Contains(tag, `as="font"`) {
				return ""
			}
			return tag
		})
		if strings.Contains(t, "orbitron") || strings.Contains(t, "font-display") {
			c.Details = append(c.Details, f+": references the display face")
		}
	}
	c.Pass = len(c.Details) == 0
	c.Summary = "display face confined to .t-hero-wordmark, .t-wordmark, .t-page-title, .t-big-number, .t-eyebrow-chip"
	if !c.Pass {
		c.Summary = fmt.Sprintf("%d violation(s)", len(c.Details))
	}
	return c
}

// ContrastRow is one pair of the contrast table.
type ContrastRow struct {
	Theme string
	FG    string
	BG    string
	Ratio float64
	Need  float64
	Pass  bool
	Kind  string
}

type tokenFile struct {
	Color map[string]string `json:"color"`
	Pairs [][]string        `json:"pairs"`
}

func luminance(hex string) float64 {
	hex = strings.TrimPrefix(hex, "#")
	if len(hex) != 6 {
		return 0
	}
	ch := func(s string) float64 {
		v, _ := strconv.ParseUint(s, 16, 8)
		f := float64(v) / 255
		if f <= 0.03928 {
			return f / 12.92
		}
		return math.Pow((f+0.055)/1.055, 2.4)
	}
	return 0.2126*ch(hex[0:2]) + 0.7152*ch(hex[2:4]) + 0.0722*ch(hex[4:6])
}

// Ratio is the WCAG 2.x contrast ratio of two #rrggbb colours.
func Ratio(a, b string) float64 {
	la, lb := luminance(a), luminance(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

// ContrastReport computes the contrast of every pair in tokens.json (dark) and
// tokens.light.json: 4.5:1 for text pairs, 3:1 for pairs marked "ui".
func ContrastReport() ([]ContrastRow, Check) {
	var rows []ContrastRow
	c := Check{Name: "Contrast (WCAG AA)", Pass: true}
	for _, th := range []struct{ name, file string }{{"dark", "static/design/tokens.json"}, {"light", "static/design/tokens.light.json"}} {
		b, err := fs.ReadFile(web.Static, th.file)
		if err != nil {
			c.Details = append(c.Details, th.file+" is missing")
			continue
		}
		var tf tokenFile
		if err := json.Unmarshal(b, &tf); err != nil {
			c.Details = append(c.Details, th.file+" is not valid JSON")
			continue
		}
		for _, p := range tf.Pairs {
			if len(p) < 2 {
				continue
			}
			fg, bg := tf.Color[p[0]], tf.Color[p[1]]
			row := ContrastRow{Theme: th.name, FG: p[0], BG: p[1], Ratio: Ratio(fg, bg), Need: 4.5, Kind: "text"}
			if len(p) > 2 && p[2] == "ui" {
				row.Need, row.Kind = 3.0, "ui"
			}
			row.Pass = row.Ratio >= row.Need
			if !row.Pass {
				c.Details = append(c.Details, fmt.Sprintf("%s: %s on %s is %.2f:1, needs %.1f:1", th.name, p[0], p[1], row.Ratio, row.Need))
			}
			rows = append(rows, row)
		}
	}
	c.Pass = len(c.Details) == 0
	c.Summary = fmt.Sprintf("%d pairs checked in both themes, %d failure(s)", len(rows), len(c.Details))
	return rows, c
}
