# ADR 0023 - The console's fonts become set D: Josefin Sans, Open Sans, JetBrains Mono

**Date:** 2026-11-22 - **Status:** Accepted (operator, 2026-11-22) - **Decider:** the operator, recorded by the foreman

## Context

Until now the console was set in Orbitron (display: the wordmarks, page titles, big numbers, eyebrow chips), IBM
Plex Sans (body) and Martian Mono (data). That choice was recorded in the design tokens
(`web/static/design/tokens.json`, approved 2026-10-21, with Martian Mono later replacing IBM Plex Mono), not in an
ADR, and ADR 0022 noted that the display face was being reconsidered.

The operator compared four sets on the two busiest screens (Fleet health and a failed device's detail), in both
themes, at identical sizes, weights and spacing, with only the three font variables changed:

| Set | Display | Body | Data |
|---|---|---|---|
| A (then current) | Orbitron | IBM Plex Sans | Martian Mono |
| B | Inter | Inter | JetBrains Mono |
| C | Space Grotesk | IBM Plex Sans | IBM Plex Mono |
| D | Josefin Sans | Open Sans | JetBrains Mono |

## Decision

1. **Set D everywhere in the console.** `--font-display` is Josefin Sans, `--font-sans` Open Sans, `--font-mono`
   JetBrains Mono. Orbitron, IBM Plex Sans and Martian Mono are removed from the tree.
2. **Vendored, never fetched.** Josefin Sans and Open Sans are the Fontsource builds of the Google Fonts releases
   (`@fontsource-variable/josefin-sans` and `@fontsource-variable/open-sans` 5.3.0): the variable fonts with their
   wght axis only, upright, as two files each (latin and latin-ext) declared with Fontsource's `unicode-range`, so a
   page fetches latin-ext only when it shows such a character. JetBrains Mono is the v2.304 release's four static
   upright faces, 400, 500, 600 and 700, so every weight the console asks of the data face is a real face and no
   synthetic bold is drawn. Each family's folder holds its OFL text and a `SOURCE.txt` with the upstream version and
   the sha256 of every file.
3. **The display face is for words only.** Josefin Sans has no tabular figures (no `tnum` feature): its "1" is about
   half as wide as its "0", so a number that changes in place jitters and numbers in a column do not align. The
   big-number role (`.t-big-number`: the KPI tiles on the fleet board and the ring centres) moves to the body face,
   Open Sans, with `font-variant-numeric: tabular-nums`. The display face keeps the hero wordmark, the sidebar
   wordmark, the page titles and the eyebrow chips. The design check that confines the display face to its roles
   (`DisplayFaceAudit`, formerly the Orbitron audit) no longer allows it in the big-number role.
4. **Symbol fallback rule.** JetBrains Mono stands second in the display and body stacks, ahead of any system font,
   because the Latin subsets of Josefin Sans and Open Sans lack a few glyphs the console uses (the table sort marks
   U+25B2 and U+25BC; also the arrows U+2191 to U+2193). Every non-ASCII character the templates, stylesheets,
   scripts and the ui package's strings can put on a page must be drawn by a vendored face in all three stacks;
   where no vendored face has a character, the character is replaced (by an inline SVG or a glyph that exists), not
   left to a system font. A test enforces this from a committed table of each font file's character map
   (`internal/console/ui/testdata/glyph_coverage.txt`), which a second test regenerates from the files.
5. **Mail names the new body face first.** The e-mail sans stack becomes `'Open Sans','Segoe UI',Arial,sans-serif`.
   Mail still embeds and loads no font (ADR 0022 decision 3), so a client without Open Sans uses the next family.
6. **Contextual ligatures stay off everywhere.** JetBrains Mono draws ligatures (for `->`, `!=`, `==` and others)
   through contextual alternates; the console's body rule already turned those off, but a `font` shorthand resets
   them, so the stylesheet now keeps them off on every element. Data is shown as typed.

## Consequences

Measured in Chrome, at identical sizes:

- **Bytes.** Set A was three files, 135,136 bytes of woff2. Set D is eight files, 508,344 bytes: Josefin Sans
  28,528 (latin) + 21,292 (latin-ext), Open Sans 48,320 + 35,156, JetBrains Mono 92,164 + 93,824 + 94,472 + 94,588.
  A page that shows only Latin-1 text and uses all four data-face weights fetches 451,896 bytes once; the files are
  served with the one-year immutable cache, so this is a first-visit cost.
- **Narrower text.** JetBrains Mono is 0.60 em per character against Martian Mono's 0.70 em, so every mono cell
  (hostnames, times, durations, exit codes, chips) is about 14% narrower; the fleet table with every customer group
  expanded now fits its wrapper at 1280 px, where set A overflowed it by 12 px. The wordmark and page titles are
  narrower too (CRYOBACKUP at 15 px: 120 px against 141 px).
- **One more line in long sentences.** Open Sans runs slightly wider than IBM Plex Sans at the same size: the
  "Purge not finished" banner text wraps to three lines at 1280 px where it took two. No other wrap changed on the
  compared screens. Nothing was resized to win the line back.
- **Josefin Sans's small x-height** (0.378 em) does not show, because the display roles are capitals only; its
  capitals sit above the centre of a `line-height: 1` box, which the display roles correct with padding and line
  height (`tokens.css`), keeping each box its old height.
- **Weights above 700 in the display face clamp to 700.** The wordmark asks for 800 and the sign-in hero for 900;
  Josefin Sans's wght axis ends at 700, which is drawn as a real instance, not a synthetic bold.
- **Licence.** All three families are under the SIL Open Font License 1.1; Josefin Sans reserves its font name. The
  files are shipped unmodified, as their upstreams publish them.

This supersedes the type choice recorded in the design tokens' approval of 2026-10-21 (and the later move from IBM
Plex Mono to Martian Mono); no earlier ADR named the fonts, so no ADR is superseded.
