package ui

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	gotoken "go/token"
	"html"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/c2tech-sys/cryobackup/web"
)

// P21-T3 (ADR 0023): every non-ASCII character the console can put on a page is drawn by a
// vendored face, never by a system fallback font.
//
// The character map of each vendored font file is kept in testdata/glyph_coverage.txt, read from
// the file's own cmap table. Go has no Brotli decoder (WOFF2 is Brotli inside), so the table is
// generated with the browser's: TestGlyphCoverageTableIsCurrent serves each file's compressed
// stream with Content-Encoding: br to the package's headless browser, which hands the decoded
// bytes back, and compares the cmap read from them with the committed table (CRYO_WRITE_GLYPHS=1
// rewrites it). TestEveryCharacterThePagesShowHasAVendoredGlyph needs no browser: it holds the
// table to the files by their sha256 and checks every character against the three font stacks.

const glyphTableFile = "testdata/glyph_coverage.txt"

// ---- WOFF2 table directory (W3C WOFF2, section 5) ----

type woff2Table struct {
	tag    string
	length int // bytes of this table in the decompressed stream (transformLength when transformed)
}

// woff2KnownTags are the tags a WOFF2 directory names by index (flags & 0x3f).
var woff2KnownTags = strings.Fields(`cmap head hhea hmtx maxp name OS/2 post cvt fpgm glyf loca prep CFF VORG EBDT EBLC gasp hdmx kern
LTSH PCLT VDMX vhea vmtx BASE GDEF GPOS GSUB EBSC JSTF MATH CBDT CBLC COLR CPAL SVG sbix acnt avar bdat bloc bsln cvar fdsc feat fmtx
fvar gvar hsty just lcar mort morx opbd prop trak Zapf Silf Glat Gloc Feat Sill`)

func woff2Base128(b []byte, i *int) (int, error) {
	v := 0
	for k := 0; k < 5; k++ {
		if *i >= len(b) {
			return 0, errors.New("truncated UIntBase128")
		}
		c := b[*i]
		*i++
		if k == 0 && c == 0x80 {
			return 0, errors.New("UIntBase128 with a leading zero")
		}
		v = v<<7 | int(c&0x7f)
		if c&0x80 == 0 {
			return v, nil
		}
	}
	return 0, errors.New("UIntBase128 longer than five bytes")
}

// woff2Directory reads a WOFF2 file's table directory: the tables in stream order and where the
// Brotli stream lies in the file.
func woff2Directory(b []byte) (tables []woff2Table, stream []byte, err error) {
	if len(b) < 48 || string(b[0:4]) != "wOF2" {
		return nil, nil, errors.New("not a WOFF2 file")
	}
	if string(b[4:8]) == "ttcf" {
		return nil, nil, errors.New("font collections are not supported")
	}
	n := int(binary.BigEndian.Uint16(b[12:]))
	compressed := int(binary.BigEndian.Uint32(b[20:]))
	i := 48
	for t := 0; t < n; t++ {
		if i >= len(b) {
			return nil, nil, errors.New("truncated table directory")
		}
		flags := b[i]
		i++
		var tag string
		if idx := int(flags & 0x3f); idx == 0x3f {
			if i+4 > len(b) {
				return nil, nil, errors.New("truncated tag")
			}
			tag = string(b[i : i+4])
			i += 4
		} else if idx < len(woff2KnownTags) {
			tag = woff2KnownTags[idx]
		} else {
			return nil, nil, fmt.Errorf("unknown known-tag index %d", idx)
		}
		orig, err := woff2Base128(b, &i)
		if err != nil {
			return nil, nil, err
		}
		version := flags >> 6
		transformed := version != 0 // for glyf and loca, version 0 is the transform and 3 the null one
		if tag == "glyf" || tag == "loca" {
			transformed = version == 0
		}
		length := orig
		if transformed {
			if length, err = woff2Base128(b, &i); err != nil {
				return nil, nil, err
			}
		}
		tables = append(tables, woff2Table{tag: tag, length: length})
	}
	if i+compressed > len(b) {
		return nil, nil, errors.New("the compressed stream runs past the end of the file")
	}
	return tables, b[i : i+compressed], nil
}

// ---- cmap (OpenType, formats 4 and 12) ----

// cmapCodepoints returns every code point the cmap table maps to a glyph other than .notdef.
func cmapCodepoints(c []byte) ([]rune, error) {
	if len(c) < 4 {
		return nil, errors.New("cmap too short")
	}
	u16 := func(o int) int { return int(binary.BigEndian.Uint16(c[o:])) }
	u32 := func(o int) int { return int(binary.BigEndian.Uint32(c[o:])) }
	if u16(0) != 0 {
		return nil, fmt.Errorf("cmap version %d", u16(0))
	}
	best, bestRank := -1, 0
	for r := 0; r < u16(2); r++ {
		rec := 4 + 8*r
		if rec+8 > len(c) {
			return nil, errors.New("truncated cmap record")
		}
		platform, encoding, off := u16(rec), u16(rec+2), u32(rec+4)
		if off+2 > len(c) {
			return nil, errors.New("cmap subtable offset out of range")
		}
		rank := 0
		switch format := u16(off); {
		case format == 12 && (platform == 3 && encoding == 10 || platform == 0):
			rank = 2
		case format == 4 && (platform == 3 && encoding == 1 || platform == 0):
			rank = 1
		}
		if rank > bestRank {
			best, bestRank = off, rank
		}
	}
	if best < 0 {
		return nil, errors.New("no Unicode cmap subtable of format 4 or 12")
	}
	var out []rune
	switch u16(best) {
	case 12:
		groups := u32(best + 12)
		if best+16+12*groups > len(c) {
			return nil, errors.New("truncated format 12 subtable")
		}
		for g := 0; g < groups; g++ {
			o := best + 16 + 12*g
			start, end, glyph := u32(o), u32(o+4), u32(o+8)
			for cp := start; cp <= end; cp++ {
				if glyph+cp-start != 0 {
					out = append(out, rune(cp))
				}
			}
		}
	case 4:
		segs := u16(best+6) / 2
		ends, starts, deltas, ranges := best+14, best+16+2*segs, best+16+4*segs, best+16+6*segs
		if ranges+2*segs > len(c) {
			return nil, errors.New("truncated format 4 subtable")
		}
		for s := 0; s < segs; s++ {
			start, end, delta, ro := u16(starts+2*s), u16(ends+2*s), u16(deltas+2*s), u16(ranges+2*s)
			for cp := start; cp <= end && cp != 0xFFFF; cp++ {
				glyph := 0
				if ro == 0 {
					glyph = (cp + delta) & 0xFFFF
				} else {
					at := ranges + 2*s + ro + 2*(cp-start)
					if at+2 > len(c) {
						return nil, errors.New("format 4 glyph index out of range")
					}
					if glyph = u16(at); glyph != 0 {
						glyph = (glyph + delta) & 0xFFFF
					}
				}
				if glyph != 0 {
					out = append(out, rune(cp))
				}
			}
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a] < out[b] })
	return out, nil
}

// ---- the coverage table ----

type runeRange struct{ lo, hi rune }

func toRanges(cps []rune) []runeRange {
	var rs []runeRange
	for _, cp := range cps {
		if n := len(rs); n > 0 && cp <= rs[n-1].hi+1 {
			if cp > rs[n-1].hi {
				rs[n-1].hi = cp
			}
			continue
		}
		rs = append(rs, runeRange{cp, cp})
	}
	return rs
}

func inRanges(rs []runeRange, r rune) bool {
	i := sort.Search(len(rs), func(i int) bool { return rs[i].hi >= r })
	return i < len(rs) && rs[i].lo <= r
}

type glyphEntry struct {
	file   string // path under static/, as fonts.css names it
	sha256 string
	ranges []runeRange
}

func formatGlyphTable(entries []glyphEntry) string {
	var b strings.Builder
	b.WriteString("# The character map of every font file fonts.css names, read from each file's own cmap table.\n")
	b.WriteString("# Generated by TestGlyphCoverageTableIsCurrent (CRYO_WRITE_GLYPHS=1 go test -run GlyphCoverage ./internal/console/ui);\n")
	b.WriteString("# do not edit by hand. Ranges are hexadecimal code points, inclusive. ADR 0023.\n")
	for _, e := range entries {
		fmt.Fprintf(&b, "file %s sha256 %s\n", e.file, e.sha256)
		for i := 0; i < len(e.ranges); i += 10 {
			var parts []string
			for _, r := range e.ranges[i:min(i+10, len(e.ranges))] {
				if r.lo == r.hi {
					parts = append(parts, fmt.Sprintf("%04X", r.lo))
				} else {
					parts = append(parts, fmt.Sprintf("%04X-%04X", r.lo, r.hi))
				}
			}
			b.WriteString("  " + strings.Join(parts, " ") + "\n")
		}
	}
	return b.String()
}

func parseGlyphTable(s string) (map[string]glyphEntry, error) {
	out := map[string]glyphEntry{}
	var cur *glyphEntry
	flush := func() {
		if cur != nil {
			out[cur.file] = *cur
		}
	}
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "#") || strings.TrimSpace(line) == "":
		case strings.HasPrefix(line, "file "):
			flush()
			f := strings.Fields(line)
			if len(f) != 4 || f[2] != "sha256" {
				return nil, fmt.Errorf("bad line %q", line)
			}
			cur = &glyphEntry{file: f[1], sha256: f[3]}
		case strings.HasPrefix(line, "  ") && cur != nil:
			for _, p := range strings.Fields(line) {
				lo, hi, _ := strings.Cut(p, "-")
				if hi == "" {
					hi = lo
				}
				a, err1 := strconv.ParseUint(lo, 16, 32)
				z, err2 := strconv.ParseUint(hi, 16, 32)
				if err1 != nil || err2 != nil || z < a {
					return nil, fmt.Errorf("bad range %q", p)
				}
				cur.ranges = append(cur.ranges, runeRange{rune(a), rune(z)})
			}
		default:
			return nil, fmt.Errorf("bad line %q", line)
		}
	}
	flush()
	return out, sc.Err()
}

// ---- fonts.css and tokens.css ----

type fontFace struct {
	family string
	file   string      // path under static/
	ranges []runeRange // nil: no unicode-range (every code point)
}

var (
	faceRuleRE   = regexp.MustCompile(`(?s)@font-face\s*\{(.*?)\}`)
	faceFamilyRE = regexp.MustCompile(`font-family:\s*'([^']+)'`)
	faceSrcRE    = regexp.MustCompile(`url\('/static/([^'?]+)`)
	faceRangeRE  = regexp.MustCompile(`unicode-range:\s*([^;]+);`)
)

func parseFontFaces(css string) ([]fontFace, error) {
	var faces []fontFace
	for _, m := range faceRuleRE.FindAllStringSubmatch(stripComments(css), -1) {
		fam, src := faceFamilyRE.FindStringSubmatch(m[1]), faceSrcRE.FindStringSubmatch(m[1])
		if fam == nil || src == nil {
			return nil, fmt.Errorf("@font-face without a family or a local url: %s", m[1])
		}
		f := fontFace{family: fam[1], file: src[1]}
		if r := faceRangeRE.FindStringSubmatch(m[1]); r != nil {
			for _, p := range strings.Split(r[1], ",") {
				p = strings.TrimPrefix(strings.TrimSpace(p), "U+")
				lo, hi, _ := strings.Cut(p, "-")
				if hi == "" {
					hi = lo
				}
				a, err1 := strconv.ParseUint(lo, 16, 32)
				z, err2 := strconv.ParseUint(hi, 16, 32)
				if err1 != nil || err2 != nil {
					return nil, fmt.Errorf("unicode-range part %q", p)
				}
				f.ranges = append(f.ranges, runeRange{rune(a), rune(z)})
			}
			sort.Slice(f.ranges, func(a, b int) bool { return f.ranges[a].lo < f.ranges[b].lo })
		}
		faces = append(faces, f)
	}
	return faces, nil
}

// fontStacks reads --font-display, --font-sans and --font-mono from tokens.css's first :root.
func fontStacks(tokens string) (map[string][]string, error) {
	out := map[string][]string{}
	for _, name := range []string{"display", "sans", "mono"} {
		m := regexp.MustCompile(`--font-` + name + `:\s*([^;]+);`).FindStringSubmatch(tokens)
		if m == nil {
			return nil, fmt.Errorf("no --font-%s in tokens.css", name)
		}
		for _, f := range strings.Split(m[1], ",") {
			out[name] = append(out[name], strings.Trim(strings.TrimSpace(f), `'"`))
		}
	}
	return out, nil
}

// coveringFace walks a stack as the browser does for one character: the first family that has a
// face whose unicode-range takes the character and whose cmap maps it draws it. A family that is
// not vendored before that point would be asked first, so the character would come from the
// system: it returns that family's name and false.
func coveringFace(stack []string, faces []fontFace, table map[string]glyphEntry, r rune) (string, bool) {
	for _, fam := range stack {
		vendored := false
		for _, f := range faces {
			if f.family != fam {
				continue
			}
			vendored = true
			if (f.ranges == nil || inRanges(f.ranges, r)) && inRanges(table[f.file].ranges, r) {
				return f.file, true
			}
		}
		if !vendored {
			return fam, false
		}
	}
	return "(end of stack)", false
}

// ---- the characters the pages can show ----

var (
	tmplCommentRE = regexp.MustCompile(`(?s)\{\{-?\s*/\*.*?\*/\s*-?\}\}`)
	htmlCommentRE = regexp.MustCompile(`(?s)<!--.*?-->`)
	cssEscapeRE   = regexp.MustCompile(`\\([0-9a-fA-F]{1,6})\s?`)
	jsEscapeRE    = regexp.MustCompile(`\\u(?:\{([0-9a-fA-F]{1,6})\}|([0-9a-fA-F]{4}))`)
)

// pageCharacters lists every non-ASCII character in the templates (text and entities), the
// stylesheets (comments left out, CSS escapes decoded), the scripts (escapes decoded) and the
// string literals of this package's Go source (which fill the templates), with where each was seen.
func pageCharacters(t *testing.T) map[rune][]string {
	t.Helper()
	seen := map[rune][]string{}
	add := func(where, s string) {
		for _, r := range s {
			if r > 0x7f && r != utf8.RuneError {
				if l := seen[r]; len(l) == 0 || l[len(l)-1] != where {
					seen[r] = append(seen[r], where)
				}
			}
		}
	}
	walk := func(fsys fs.FS, glob string, each func(name, body string)) {
		files, err := fs.Glob(fsys, glob)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			b, err := fs.ReadFile(fsys, f)
			if err != nil {
				t.Fatal(err)
			}
			each(f, string(b))
		}
	}
	nTmpl, nCSS, nJS, nGo := 0, 0, 0, 0
	for _, g := range []string{"templates/*.html", "templates/*/*.html"} {
		walk(web.Templates, g, func(name, body string) {
			nTmpl++
			body = htmlCommentRE.ReplaceAllString(tmplCommentRE.ReplaceAllString(body, ""), "")
			add(name, html.UnescapeString(body))
		})
	}
	walk(web.Static, "static/*.css", func(name, body string) {
		nCSS++
		body = cssEscapeRE.ReplaceAllStringFunc(stripComments(body), func(m string) string {
			v, _ := strconv.ParseUint(cssEscapeRE.FindStringSubmatch(m)[1], 16, 32)
			return string(rune(v))
		})
		add(name, body)
	})
	walk(web.Static, "static/js/*.js", func(name, body string) {
		nJS++
		body = jsEscapeRE.ReplaceAllStringFunc(body, func(m string) string {
			sm := jsEscapeRE.FindStringSubmatch(m)
			v, _ := strconv.ParseUint(sm[1]+sm[2], 16, 32)
			return string(rune(v))
		})
		add(name, body)
	})
	goFiles, _ := filepath.Glob("*.go")
	fset := gotoken.NewFileSet()
	for _, f := range goFiles {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		nGo++
		ast.Inspect(file, func(n ast.Node) bool {
			if lit, ok := n.(*ast.BasicLit); ok && (lit.Kind == gotoken.STRING || lit.Kind == gotoken.CHAR) {
				v := lit.Value
				if lit.Kind == gotoken.CHAR {
					v = `"` + strings.Trim(v, "'") + `"`
				}
				if s, err := strconv.Unquote(v); err == nil {
					add(fmt.Sprintf("internal/console/ui/%s:%d", f, fset.Position(lit.Pos()).Line), html.UnescapeString(s))
				}
			}
			return true
		})
	}
	if nTmpl < 20 || nCSS < 3 || nJS < 3 || nGo < 20 {
		t.Fatalf("walked %d templates, %d stylesheets, %d scripts, %d Go files: the walk is not seeing the sources", nTmpl, nCSS, nJS, nGo)
	}
	return seen
}

func loadGlyphInputs(t *testing.T) ([]fontFace, map[string][]string, map[string]glyphEntry) {
	t.Helper()
	css, err := fs.ReadFile(web.Static, "static/fonts.css")
	if err != nil {
		t.Fatal(err)
	}
	faces, err := parseFontFaces(string(css))
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := fs.ReadFile(web.Static, "static/tokens.css")
	if err != nil {
		t.Fatal(err)
	}
	stacks, err := fontStacks(string(tokens))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(glyphTableFile)
	if err != nil {
		t.Fatalf("%v: generate it with CRYO_WRITE_GLYPHS=1 go test -run GlyphCoverage ./internal/console/ui", err)
	}
	table, err := parseGlyphTable(string(raw))
	if err != nil {
		t.Fatalf("%s: %v", glyphTableFile, err)
	}
	return faces, stacks, table
}

// TestEveryCharacterThePagesShowHasAVendoredGlyph: for every non-ASCII character in the sources,
// each of the three stacks reaches a vendored face that maps it before it reaches any system font.
func TestEveryCharacterThePagesShowHasAVendoredGlyph(t *testing.T) {
	t.Parallel()
	faces, stacks, table := loadGlyphInputs(t)
	// The table is the character map of exactly the files fonts.css serves, byte for byte.
	files := map[string]bool{}
	for _, f := range faces {
		files[f.file] = true
		b, err := fs.ReadFile(web.Static, "static/"+f.file)
		if err != nil {
			t.Fatalf("fonts.css names %s: %v", f.file, err)
		}
		sum := sha256.Sum256(b)
		if e, ok := table[f.file]; !ok || e.sha256 != hex.EncodeToString(sum[:]) || len(e.ranges) == 0 {
			t.Errorf("%s: not in %s, or the table was made from another file: regenerate it (CRYO_WRITE_GLYPHS=1)", f.file, glyphTableFile)
		}
	}
	for f := range table {
		if !files[f] {
			t.Errorf("%s lists %s, which fonts.css does not serve", glyphTableFile, f)
		}
	}
	for _, name := range []string{"display", "sans", "mono"} {
		if len(stacks[name]) == 0 || !files[firstFile(faces, stacks[name][0])] {
			t.Errorf("--font-%s does not start with a vendored face: %v", name, stacks[name])
		}
	}
	chars := pageCharacters(t)
	if len(chars) < 5 {
		t.Fatalf("only %d non-ASCII characters found: the walk is not seeing the sources", len(chars))
	}
	var keys []rune
	for r := range chars {
		keys = append(keys, r)
	}
	sort.Slice(keys, func(a, b int) bool { return keys[a] < keys[b] })
	for _, r := range keys {
		var drawn []string
		for _, name := range []string{"display", "sans", "mono"} {
			face, ok := coveringFace(stacks[name], faces, table, r)
			if !ok {
				t.Errorf("U+%04X %q (%s): --font-%s reaches %s before any vendored face that has it", r, r, strings.Join(firstN(chars[r], 3), ", "), name, face)
			}
			drawn = append(drawn, name+"="+filepath.Base(face))
		}
		t.Logf("U+%04X %q %s (seen in %d places, first %s)", r, r, strings.Join(drawn, " "), len(chars[r]), chars[r][0])
	}
	// The check can fail: without the symbol fallback the sort mark has no vendored glyph in the
	// body or display stack, and a character no vendored face has fails in every stack.
	if _, ok := coveringFace([]string{"Open Sans", "system-ui"}, faces, table, 0x25B2); ok {
		t.Error("Open Sans maps U+25B2: the mutant stack does not test the symbol fallback")
	}
	if _, ok := coveringFace([]string{"Josefin Sans", "system-ui"}, faces, table, 0x25B2); ok {
		t.Error("Josefin Sans maps U+25B2: the mutant stack does not test the symbol fallback")
	}
	if _, ok := coveringFace(stacks["sans"], faces, table, 0x4E00); ok {
		t.Error("U+4E00 is covered: the check cannot fail")
	}
}

func firstFile(faces []fontFace, family string) string {
	for _, f := range faces {
		if f.family == family {
			return f.file
		}
	}
	return ""
}

func firstN(s []string, n int) []string { return s[:min(n, len(s))] }

// TestGlyphCoverageTableIsCurrent regenerates the table from the vendored files, with the
// browser's Brotli decoder, and compares it with the committed one.
func TestGlyphCoverageTableIsCurrent(t *testing.T) {
	t.Parallel()
	chrome := chromeOrSkip(t)
	css, err := fs.ReadFile(web.Static, "static/fonts.css")
	if err != nil {
		t.Fatal(err)
	}
	faces, err := parseFontFaces(string(css))
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, f := range faces {
		files = append(files, f.file)
	}
	sort.Strings(files)
	dir := t.TempDir()
	type job struct {
		F     string `json:"f"`
		Off   int    `json:"off"`
		Len   int    `json:"len"`
		Total int    `json:"total"`
	}
	var jobs []job
	sums := map[string]string{}
	for i, f := range files {
		b, err := fs.ReadFile(web.Static, "static/"+f)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(b)
		sums[f] = hex.EncodeToString(sum[:])
		tables, stream, err := woff2Directory(b)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		// The decompressed stream is the tables' data in directory order, with no padding.
		j := job{F: fmt.Sprintf("f%d.br", i), Off: -1}
		for _, tb := range tables {
			if tb.tag == "cmap" {
				j.Off, j.Len = j.Total, tb.length
			}
			j.Total += tb.length
		}
		if j.Off < 0 {
			t.Fatalf("%s has no cmap table", f)
		}
		if err := os.WriteFile(filepath.Join(dir, j.F), stream, 0o644); err != nil {
			t.Fatal(err)
		}
		jobs = append(jobs, j)
	}
	spec, _ := json.Marshal(jobs)
	page := `<!doctype html><html><head><meta charset="utf-8"></head><body><script>
function done(t) { var pre = document.createElement('pre'); pre.id = '__r'; pre.textContent = t; document.body.appendChild(pre); if (parent !== window) parent.postMessage(t, '*'); }
Promise.all(` + string(spec) + `.map(function (j) {
  return fetch('/' + j.f).then(function (r) { return r.arrayBuffer(); }).then(function (buf) {
    var u = new Uint8Array(buf), c = u.subarray(j.off, j.off + j.len), s = '';
    for (var i = 0; i < c.length; i++) s += String.fromCharCode(c[i]);
    return {n: u.length, cmap: btoa(s)};
  });
})).then(function (r) { done(JSON.stringify(r)); }, function (e) { done(JSON.stringify([{error: String(e)}])); });
</script></body></html>`
	if err := os.WriteFile(filepath.Join(dir, "page.html"), []byte(page), 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := sharedLayoutBrowser(chrome)
	if err != nil {
		browserWouldNotRun(t, err)
	}
	out, err := b.run(dir, 200, 200, 2*time.Minute)
	if err != nil {
		t.Fatalf("the browser did not decode the fonts: %v", err)
	}
	var res []struct {
		N     int    `json:"n"`
		Cmap  string `json:"cmap"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil || len(res) != len(jobs) {
		t.Fatalf("browser answer %.200q: %v", out, err)
	}
	var entries []glyphEntry
	for i, f := range files {
		if res[i].Error != "" || res[i].N != jobs[i].Total {
			t.Fatalf("%s: decoded %d bytes, the directory adds up to %d (%s)", f, res[i].N, jobs[i].Total, res[i].Error)
		}
		c, err := base64.StdEncoding.DecodeString(res[i].Cmap)
		if err != nil || len(c) != jobs[i].Len {
			t.Fatalf("%s: cmap of %d bytes, want %d (%v)", f, len(c), jobs[i].Len, err)
		}
		cps, err := cmapCodepoints(c)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		// Sanity: a latin file maps A and 0, a latin-ext file maps U+0100 (A with macron).
		probes := []rune{'A', '0'}
		for _, fc := range faces {
			if fc.file == f && fc.ranges != nil && !inRanges(fc.ranges, 'A') {
				probes = []rune{0x100}
			}
		}
		for _, p := range probes {
			if len(cps) < 100 || !containsRune(cps, p) {
				t.Fatalf("%s: the cmap maps %d code points, not U+%04X: it was not read correctly", f, len(cps), p)
			}
		}
		entries = append(entries, glyphEntry{file: f, sha256: sums[f], ranges: toRanges(cps)})
	}
	want := formatGlyphTable(entries)
	if os.Getenv("CRYO_WRITE_GLYPHS") != "" {
		if err := os.WriteFile(glyphTableFile, []byte(want), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s", glyphTableFile)
		return
	}
	got, err := os.ReadFile(glyphTableFile)
	if err != nil {
		t.Fatalf("%v: generate it with CRYO_WRITE_GLYPHS=1", err)
	}
	if !bytes.Equal(bytes.ReplaceAll(got, []byte("\r\n"), []byte("\n")), []byte(want)) {
		t.Errorf("%s is not the character map of the vendored files: regenerate it with CRYO_WRITE_GLYPHS=1 and review the diff", glyphTableFile)
	}
}

func containsRune(s []rune, r rune) bool {
	i := sort.Search(len(s), func(i int) bool { return s[i] >= r })
	return i < len(s) && s[i] == r
}
