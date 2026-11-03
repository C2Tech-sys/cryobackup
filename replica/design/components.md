# CryoBackup component specifications

Replica pipeline, stage: design, step 3 (component specs). Contract for the Phase 1 builders: every block below is implemented as a Go `html/template` partial plus hand-written CSS, then rendered in every state on the `/design` check page.

| | |
| --- | --- |
| Binds to | `replica/design/tokens.json` and `tokens.css` (approved 2026-10-21; dark-first, light mirrored). Components use token names only, never literal colours. |
| Visual reference | `replica/design/concepts-v0.3-orbitron.html` (class hooks such as `.st`, `.btn`, `.kpi`, `.banner`, `.strip` are kept so the concept CSS ports directly) |
| State vocabulary | `replica/architecture.md` section 4 (4.2 precedence, 4.3 suppression, 4.5 rollup, 4.6 conditions C01-C16) |
| Inventory | `replica/recon.md` "## Components" (33 rows; coverage table at the end) |
| Screens | `recon.md` "## Screens" ids (S/A/B). Where a listed screen is "not built" in architecture 10.4 the block says so. |

## 0. Global rules

### 0.1 The state language: four classes plus neutral, five glyphs

State is carried by **shape and word first, colour second**. Every state indicator uses one of these glyphs next to a text label. Colour is never the only carrier.

| class | glyph | glyph geometry | text / glyph token | soft fill token | meaning |
| --- | --- | --- | --- | --- | --- |
| `ok` | mint dot | 8px circle | `--success` | `--success-soft` | healthy, done |
| `info` | ice square | 8px square, radius 2px | `--accent` | `--accent-soft` | in progress, waiting by design |
| `warn` | amber triangle | 9px triangle (clip-path) | `--warning` | `--warning-soft` | needs attention, not yet an outage |
| `crit` | coral diamond | 8px square rotated 45deg, radius 2px | `--danger` | `--danger-soft` | act now |
| `neutral` | hollow ring | 8px circle, 1.5px border, no fill | `--text-muted` | none (border `--border`) | parked, retired, not applicable |

Severity to class: engine `critical` = `crit`; engine `alarm` = `warn`; no-alarm early warnings (MISSED, DEFERRED, OUTDATED) = `warn`; waiting-by-design (PENDING_FIRST, RUNNING) = `info`; OK = `ok`; PAUSED, RETIRED = `neutral`.

### 0.2 Orbitron rationing (restated)

Orbitron (`--font-display`) is used for exactly five roles: hero wordmark, wordmark, page title, big number, eyebrow chip (classes `.t-hero-wordmark`, `.t-wordmark`, `.t-page-title`, `.t-big-number`, `.t-eyebrow-chip`). Weight floor 600; never for sentences, table cells, hostnames, state chips, forms, buttons, email body or tray text.
Component blocks below name the display role by its class; any text not named as one of the five is `--font-sans` or `--font-mono`.
The data face (`--font-mono`) is Martian Mono (SIL OFL 1.1, vendored variable woff2, wdth pinned to 100); IBM Plex Sans stays the body face. Mono roles are unchanged: timestamps, counts, sizes, ids, code, `.label-mono`.

### 0.3 Rules every component obeys

- **Focus.** `:focus-visible` is a 2px solid `--accent` ring with 2px offset (`outline: 2px solid var(--accent); outline-offset: 2px`). Where an ancestor clips overflow (segmented control, tab bar, table rows, menu items) use `outline-offset: -2px`. Never `outline: none` without a replacement. Mouse clicks do not show the ring.
- **Hit target.** Minimum 32px in both axes for anything pointer-clickable. A control whose visible box is smaller (sm button 26px, sm chip, 10px strip cells, 24px chips) extends its hit area with a `::before { inset: -3px }` or padding to reach 32px, or is not a pointer target on its own (the parent group is).
- **Motion.** `--motion-fast` (120ms) for hover, `--motion-base` (200ms) for panels, dialogs, menus, `--motion-slow` (320ms) for ring and bar draws; easing `--motion-ease`. `@media (prefers-reduced-motion: reduce)` removes all transitions, pulses, shimmer, spinner rotation (spinner becomes a static "..." glyph) and slide-ins; state changes become instant.
- **Colour is never the only carrier of state.** Every state shows a glyph shape and a word; colour reinforces. Red/green or amber/red differences alone are never relied on (0.1).
- **Real elements.** Every interactive element is a real `<button>`, `<a href>`, `<input>`, `<select>`, `<textarea>` or `<summary>`. No `div` with a click handler. Custom widgets (tree, grid, tabs, menu) use the matching ARIA pattern on top of real focusable elements.
- **Accessible names.** Every icon-only control has `aria-label` (and a tooltip with the same words). Every input has a visible `<label>` (or `aria-label` for toolbar search). Decorative glyphs and icons are `aria-hidden="true"`.
- **Numerals.** `font-variant-numeric: tabular-nums` wherever digits align or update (body default, tables, counts, timestamps, sizes, progress). Timestamps, sizes, ids and versions use `--font-mono`.
- **Time.** All times render in the operator's zone (default `America/Chicago`) with the zone shown in the tooltip; storage is UTC; `<time datetime="...Z">` always. Day buckets for strips and grids use the customer's `display_tz`. Ages are relative ("40 s ago", "6 h ago", "3 d ago") with the absolute time in the tooltip.
- **Path-free.** No component ever renders, logs, caches or puts in a URL a file path, file name or folder name from a client machine. Error classes (`vss_snapshot`, `locked_file`, `permission`) and counts only. The one exception is the selection tree (see its block), which renders a transient listing and persists nothing.
- **Hostnames** render in `--font-sans` weight 600 (never Orbitron, never mono-substituted), so FRONTDESK-01 and FR0NTDESK-O1 stay distinguishable.
- **Both themes.** Components reference tokens only, so dark and light are identical in markup. Both themes are on the `/design` page.
- **No remote assets.** Fonts, icons, scripts vendored and `go:embed`ed. No CDN, no analytics.

### 0.4 State recipes (referenced by name in the blocks)

| recipe | definition |
| --- | --- |
| **hover** | transparent controls: background `--accent-soft`; surface controls: background `--surface-raised`; table rows: background `color-mix(in srgb, var(--accent) 5%, transparent)`. Transition `--motion-fast`. |
| **active** (pressed) | primary: `color-mix(in srgb, var(--accent) 85%, var(--bg))`; secondary: `color-mix(in srgb, var(--accent) 18%, var(--surface-raised))`; no movement. |
| **focus-visible** | see 0.3. |
| **disabled** | text `--text-muted`, border `--border`, background `--surface`, `cursor: not-allowed`, no opacity trick. Actions unavailable for a reason (offline, parked, role, master key) use `aria-disabled="true"` plus a reason (tooltip and `aria-describedby`) and stay focusable; native `disabled` is only for the transient "submitting" case. A control is never disabled without a stated reason. |
| **loading** | `aria-busy="true"`; label stays; a 14px spinner replaces the leading icon (or precedes the label); width does not change; clicks ignored. |
| **error** | 1px `--danger` border, `crit` glyph, message text next to the field (not colour alone), `aria-invalid="true"`, `aria-describedby` to the message. |
| **selected** | `--accent-soft` background, `--accent` border and text, plus a check or filled-dot cue. |
| **skeleton** | blocks of `--border` with a `--surface-raised` shimmer sweep (static when reduced motion); never contains digits, never resembles a real value. |

### 0.5 Dense type recipes (concept-derived)

The concept uses sizes between the `--text-xs` and `--text-sm` steps. They are named here so builders do not improvise. See the report: tokens need `--text-ui` and `--text-2xs` entries.

| recipe | face / size / weight | use |
| --- | --- | --- |
| `body` | `--font-sans`, `--text-base` 14/20/400 | paragraphs, form values |
| `ui` | `--font-sans` 12.5/18/500 | buttons (md), table cells, menu items, help text |
| `ui-sm` | `--font-sans` 11.5/1/600, tracking .02em | state chips, sm buttons |
| `title-sm` | `--font-sans` 16/1.2/600 | panel and dialog titles (wizard h3 is 18/1.3/600) |
| `label-mono` | `--font-mono` 10.5-11/1/500, uppercase, tracking .1em | table headers, KPI labels, section `h4` |
| `meta-mono` | `--font-mono` 11-11.5/1/500 | timestamps, ids, sizes, versions |

### 0.6 Copy rules (all components)

Plain, specific, active voice. State what happened, then what happens next, then what the operator can do. No exclamation marks, no emoji, no "Oops", no "Your data is safe". Numbers are exact ("23 files", "3 consecutive failures since Fri 02:10"). Errors never expose paths. Examples use the concept's fictional names only (Summit Eye Care, FRONTDESK-01, and so on). A positive statement ("Nothing overdue") always carries its evidence ("212 devices checked at 13:42:10").

### 0.7 Live-update contract (applies to every component marked "live")

One SSE connection per page: `GET /ui/events` (htmx `sse-connect`; `GET /api/v1/events` is the API twin). Named events (proposed names; builders and the `ui` package must agree on them once):

| event | payload | components that swap on it |
| --- | --- | --- |
| `fleet.state` | rendered row/chip fragment keyed by element id (`dev-<id>`, `cust-<id>`) | data table rows, state chip, rollup chips, rings, KPI tiles, health gauge |
| `run.progress` | rendered progress fragment keyed `run-<id>` (server coalesces to at most 1 per second per run) | live run progress |
| `banner.update` | full banner strip fragment (`#banners`) | condition banners |
| `freshness` | `ComputedAt`, `LoadedAt`, agents in view (`n`, `m`) | freshness stamp |
| `ping` | none, every 15 s | connection watchdog only |

Fallback polling when SSE is unavailable or after a drop: board partials `hx-trigger="every 30s"`, progress `every 5s`. The refresh interval is a console setting (default 30 s) and C10 triggers at 2x that interval.

**Stale rule (never freeze looking fresh).** A small vendored first-party script (`fresh.js`, about 1 KB, listed on the `/design` page) keeps one timer. If no event or successful poll has arrived for 2x the refresh interval, or the SSE stream reports close/error (`htmx:sseClose`, `htmx:sseError`, `htmx:sendError`), it: (1) flips the freshness stamp to its stale state "STALE since hh:mm:ss", (2) inserts the C10 banner "Live updates disconnected: data as of hh:mm", (3) stamps `data-stale="since hh:mm:ss"` on every live component, which shows its own "as of hh:mm:ss" marker and removes any pulse. On reconnect it triggers a full re-fetch of the visible partials before clearing the markers. With JavaScript disabled, `<noscript>` renders "Live updates need JavaScript. Data as of hh:mm:ss" and the page uses `<meta http-equiv="refresh">` at the refresh interval so it still never freezes silently.

### 0.8 Binding notes (tokens the specs rely on)

- `--accent-soft`, `--danger-soft`, `--warning-soft`, `--success-soft` are used by the approved concept (root block, with different alpha in light) but are not in `tokens.css`. They must be added as derived tokens before the `.st`, banner and hover recipes can ship.
- The concept names the glow shadow `--glow`; the token file calls it `--shadow-glow`. Specs use `--shadow-glow`.
- `tokens.css` emits no layout variables (sidebar 240, header 56, content-max 1440, gutter 24, breakpoints 640/900/1200 exist only in `tokens.json`). The app shell needs `--layout-sidebar`, `--layout-header`, `--layout-content-max`, `--layout-gutter` added to the generated CSS.
- Disabled, hover and active mixes above are expressed with `color-mix` over existing tokens; no new colour tokens are required beyond the four soft fills.

## 1. Foundations and navigation

### 1.1 Display type roles

```
Display type roles
  variants  hero-wordmark (900, 40px, .06em) | wordmark (800, nav 15px / email 16px / tray 12px, .12em) | page-title (600, 12.5px, .22em, uppercase) | big-number (700, 23-28px, .02em, tabular) | eyebrow-chip (600, 9.5px, .14em, uppercase)
  sizes     fixed per role, as listed; no other Orbitron size exists. Big number: 23px in tiles and rings, 28px only in the hero/digest summary
  states    static; wordmark "CRYO" in --accent, "BACKUP" in --text at weight 500; page-title may truncate with ellipsis (full text in title attr); big-number may show an unavailable value as an em dash in --text-muted, never as 0
  tokens    --font-display, classes .t-hero-wordmark .t-wordmark .t-page-title .t-big-number .t-eyebrow-chip, --accent, --text, --text-muted
  a11y      page-title is the single <h1> per page; wordmark link has aria-label "CryoBackup"; big numbers carry a visually hidden unit/label ("212 devices"); uppercase is CSS text-transform so the source reads normally
  used on   S01 (hero), every console screen (wordmark, page-title), B01 B03 B07 B08 B11 (big number), S23 B02 (eyebrow), A14 B14 (wordmark in tray)
  notes     Allowed places only: hero wordmark on login and /design; wordmark in sidebar, email header image, tray popover header; page-title in the topbar h1; big-number in KPI tiles, age-ring centre, digest summary counts; eyebrow-chip at most once per view (e.g. above a hero region). NEVER: sentences, table cells, hostnames, state chips, buttons, form labels or values, timestamps, sizes, email body, tray body text. If a number sits in a table cell it is mono, not Orbitron. Orbitron falls back to Michroma then IBM Plex Sans; layouts reserve width with ch units so a fallback face does not reflow the tile.
```

### 1.2 Card

```
Card
  variants  default (radius lg), compact (radius md, used for KPI tiles and ring cards), raised (surface-raised, used for dialog footers and wizard step bars), inset (bg, used for log lines and history rows)
  sizes     padding 18px default / 12px 14px compact / 10px 12px inset; gap between cards 16px (12px inside KPI/ring grids)
  states    default; hover only when the whole card is a link (border colour --border-input); focus-visible ring on the link; loading (skeleton content, card chrome stays); error (inline banner inside the card, not a red card)
  tokens    bg --surface, border 1px --border, radius --radius-lg | --radius-md, shadow --shadow-card, inset bg --bg
  a11y      a card is a plain container; give it a heading and use <section aria-labelledby> when it has a title; a clickable card is a single <a> wrapping the content or a stretched-link pattern with one link
  used on   B01 B03 B07 B08 B11 S26 S36 A14
  notes     Concept paddings (18, 14, 12, 10) are component-internal metrics and ship as written.
```

### 1.3 Key-value list

```
Key-value list
  variants  panel (120px key column), compact (110px key column, tray and dialogs), stacked (one column below 420px container width)
  sizes     row gap 6px, column gap 10px; text ui 12.5px
  states    default; value-truncated (id shown as first 8 and last 3 characters with an ellipsis, full value in tooltip and copy button); value-unavailable (em dash plus visually hidden "not available"); value-alarm (value carries an inline state chip sm, never colour alone)
  tokens    <dl>, dt --text-muted, dd --text, mono values --font-mono, chips 20px (.st sm)
  a11y      real <dl><dt><dd>; copy buttons are icon buttons with aria-label "Copy device id"; truncated values expose the full value via the copy button and title
  used on   B07 B08 B10 S22 B14 A14
  notes     Copy rule: keys are nouns in sentence case ("Last backup", "Managed by", "Repository"); values give a number and a time ("38.2 GB . 61 snapshots . prune in 12 d"). Never show file paths as values.
```

### 1.4 Section label

```
Section label
  variants  mono heading (h3/h4 label-mono), with trailing count or hint
  sizes     11px / 1 line, tracking .1em, uppercase; 8px space below
  states    static; collapsible (button with aria-expanded and chevron) when it heads a collapsible group
  tokens    --font-mono weight 600, --text-muted
  a11y      is a real heading at the correct level (never a div); collapsible variant is a <button> inside the heading
  used on   B07 B08 B04 B05 S22 B09 (and digest email sections)
  notes     This is the default section marker. The Orbitron eyebrow chip is the exception, not the default (see 1.1).
```

### 1.5 App shell

```
App shell
  variants  full (>=1200px: sidebar 240px), rail (900-1199px: 56px icon rail, labels in tooltips), drawer (<900px: sidebar becomes an overlay opened from a topbar menu button), single-column (<640px)
  sizes     sidebar 240px (tokens.json layout.sidebar), header 56px, content max-width 1440px, gutter 24px (16px below 640px); breakpoints sm 640, md 900, lg 1200
  states    default; drawer open (scrim, focus trapped, Esc closes, focus returns to the menu button); print (sidebar and topbar hidden)
  tokens    bg --bg, content ground = dot grid (radial-gradient of --accent at 9% over 22px, from tokens surface.content-ground) on the content area only, sidebar bg --surface with border-right --border, topbar bg --surface with hairline shadow (--accent at 22%)
  a11y      landmarks: <header>, <nav aria-label="Console navigation">, <main id="main">, skip link "Skip to content" as the first focusable element; banner strip region sits inside <main> before the h1 content
  used on   every console screen (S01 uses a bare centred shell with no sidebar)
  notes     Order inside <main>: condition banners (in the flow, not sticky: ADR 0015), page content. The freshness stamp lives in the topbar so it is on every page. Concept uses a 224px sidebar and tighter content padding; tokens (240px, 24px) govern.
```

### 1.6 Sidebar navigation

```
Sidebar navigation
  variants  expanded (icon + label), rail (icon only + tooltip), item with count badge, system-status footer
  sizes     item min-height 36px (padding 8px 10px), icon 16px, gap 10px, item gap 4px; count badge: mono 10.5px/600, padding 3px 6px, pill
  states    default; hover; focus-visible (inset ring); current page (aria-current="page", --accent-soft background, --accent text, plus a 2px leading bar in --accent as a non-colour cue); count-badge present; count-badge zero (hidden, never "0"); loading counts (badge absent); stale (counts show "as of" in the tooltip)
  tokens    text --text-muted, current --accent on --accent-soft, badge bg --danger text --on-status, wordmark .t-wordmark (15px) with mark symbol 24px, footer border-top --border, footer text --text-muted at 11.5px, footer values carry a glyph and a word
  a11y      <nav> with a <ul>; badge text is part of the link name ("Fleet, 11 need attention"); the footer status is a list where each entry reads "Gateway: reachable"; a failing entry shows the crit glyph, the word "unreachable" and links to the matching banner
  used on   every console screen except S01
  notes     Items in order: Fleet, Customers, Devices, Runs, Alarms, Policies, Storage, Keys, Settings (Audit and System health sit under Settings and Security groups). Fleet badge = devices in a critical state; Alarms badge = open alarms; both update on fleet.state (live; stale rule applies). Footer (Gateway, Storage probe, SMTP, Maintenance, version) mirrors C01 C02 C03 C07: it can never read "ok" for a probe whose data is stale; stale shows "unknown since hh:mm".
```

### 1.7 Topbar

```
Topbar
  variants  default (title, search, freshness stamp, view and primary actions); compact (<900px: title, search icon button, compact stamp)
  sizes     height 56px; padding 0 18px; gap 12px; 1px --border bottom plus the accent hairline
  states    default; scrolled (no change); with unsaved-changes marker next to the title on editors ("Policy . unsaved")
  tokens    bg --surface, shadow topbar-hairline, h1 .t-page-title, controls per Button/Search, stamp per Freshness stamp
  a11y      exactly one <h1> (the page title); toolbar controls in DOM order title, search, stamp, saved views, primary action; the primary action is the last focusable element so Tab order matches visual order
  used on   every console screen except S01
  notes     The freshness stamp is right-aligned (margin-left auto) and is rendered before page content is, so it exists on error pages too. The page title is a noun phrase ("Fleet health", "Device detail", "Enroll devices").
```

### 1.8 Global search

```
Global search
  variants  topbar (default), page-local filter (tables have their own live filter; the topbar search is the cross-entity one)
  sizes     height 32px, max-width 360px, radius sm, icon 14px, shortcut hint kbd (mono 10px, border --border, radius 4px, padding 3px 5px)
  states    default (placeholder "Search devices, customers, states"); focus (ring, hint hidden, results popover opens after 2 characters); loading (spinner in the field after 200ms); results; no-results; error; disabled (never)
  tokens    bg --bg, border --border-input, text --text, placeholder --text-muted, popover per Menu (surface-raised, --shadow-pop, radius md), group labels per Section label
  a11y      combobox pattern: input role="combobox" aria-expanded aria-controls aria-activedescendant; results role="listbox" with groups (Devices, Customers, States); Up/Down move, Enter opens, Esc closes then clears; "/" focuses the field from anywhere except inside input, textarea, select or contenteditable; the shortcut is announced in the field's aria-keyshortcuts="/"
  used on   every console screen except S01
  notes     htmx: hx-get="/ui/search" hx-trigger="input changed delay:200ms" hx-sync="this:replace" (cancels in-flight). Typing a state word (failed, unseen, stale, parked) offers "State: FAILED" which applies that filter to the fleet table. No-results copy: "No device or customer matches 'xyz'. Searched 212 devices and 5 customers." Error copy: "Search is unavailable. Filter the table directly, or try again." Enter without a selection goes to /search?q= (milestone M10; until then it filters the current table). Queries are not logged with results.
```

### 1.9 Theme switch

```
Theme switch
  variants  three-way segmented (System, Dark, Light)
  sizes     segment height 32px; icon + label on desktop, icon + aria-label in the rail
  states    system (default; follows prefers-color-scheme, dark when the OS says dark), dark, light; focus-visible per segmented control
  tokens    see Segmented control; effect is data-theme="dark|light" on <html> (absent = system), colour-scheme follows
  a11y      aria-pressed on the active segment; announces "Theme: Dark"; the change is instant, no transition on colours
  used on   every console screen (sidebar footer); S38
  notes     Persistence is a per-viewer convenience: localStorage key wrapped in try/catch, and the page renders correctly without it. A vendored 300-byte theme.js in <head> applies the stored choice before first paint to avoid a flash. Default is dark (tokens: dark-first).
```

## 2. Controls

### 2.1 Button

```
Button
  variants  primary, secondary (default), ghost, danger (soft), danger-solid (final confirmation of a destructive dialog only); each may carry a leading icon or a trailing caret (menu trigger)
  sizes     sm 26px (hit area padded to 32px; dense tables and panel action bars), md 32px (default), lg 40px (login, key reveal, wizard primary on touch)
  states    default, hover, active, focus-visible (2px ring, --accent, 2px offset), disabled (reason required, aria-disabled), loading (spinner, label kept, aria-busy), pressed (aria-pressed, toggle buttons only)
  tokens    radius --radius-sm; md font ui 12.5/600, sm ui-sm 11.5/600, lg 14/600; padding sm 0 9px, md 0 12px, lg 0 16px; gap 6px. primary: bg --accent, text --on-accent, no border. secondary: bg --surface-raised, border 1px --border-input, text --text. ghost: transparent, text --text-muted, hover text --text. danger: text --danger, bg --danger-soft, border 1px --danger at 45%. danger-solid: bg --danger, text --on-status
  a11y      real <button type="button|submit"> (or <a class="btn"> when it navigates); label stays in the accessible name while loading; disabled-with-reason uses aria-disabled and aria-describedby; icon-only buttons are Icon button, not this
  used on   S01 S03 S04 S07 S08 S24 S25 B01 B04 B05 B07 B08 B10 B15 B16 B17
  notes     Label: verb first, sentence case ("Back up now", "Save policy", "Enroll devices"); ellipsis suffix when it opens a dialog ("Restore..."); "+" prefix only for creating ("+ Enroll devices"). A confirm button repeats the action ("Retire device"), never "OK". One primary per view region. htmx: hx-post with hx-disabled-elt="this"; success raises a toast, failure raises an inline banner in the nearest dialog or panel (never toast only). Double-submit is blocked by the loading state.
```

### 2.2 Icon button

```
Icon button
  variants  ghost (default), secondary, danger; toggle (aria-pressed); menu trigger (aria-haspopup="menu", aria-expanded)
  sizes     md 32x32px, sm 26x26px with hit area padded to 32px; icon 16px (md) / 14px (sm), 1.4px stroke
  states    default (full --text-muted, no opacity fade), hover, active, focus-visible, disabled (reason), loading (spinner replaces icon), pressed
  tokens    radius --radius-sm; ghost: transparent, --text-muted, hover --text on --accent-soft; secondary: --surface-raised with --border-input; danger: --danger
  a11y      always an aria-label naming the action and its target ("More actions for FRONTDESK-01", "Copy device id"); tooltip with the same words on hover (after 400ms) and on focus; the icon is aria-hidden
  used on   B01 S19 B07 B08 B05 S22 (row menu, copy, close, pager, column picker)
  notes     The concept fades the row "..." button to 55% opacity until row hover; that fails contrast and hides the control from keyboard users. It ships at full --text-muted at rest.
```

### 2.3 Link

```
Link
  variants  inline (in sentences), arrow ("Why this happens ->" with a trailing arrow glyph), hostname (device name link in tables: --text, weight 600, underline on hover), subtle (breadcrumb, footer)
  sizes     inherits surrounding text; hit area at least 32px high in lists and tables via padding
  states    default, hover (underline), focus-visible (ring), visited (no change), current (aria-current), disabled (rendered as plain text with the reason; a disabled link does not exist)
  tokens    inline/arrow --accent; hostname --text; subtle --text-muted; underline always on for inline links inside paragraphs (non-colour cue); text-decoration-thickness 1px, offset 2px
  a11y      real <a href>; link text makes sense out of context ("Open customer", not "here"); arrow glyph aria-hidden; links that download say so ("Download zip")
  used on   B07 B08 S21 S22 B09 every table
  notes     No external links appear in the console body; documentation links point to vendored docs. Copy: verb or noun phrase, no trailing punctuation.
```

### 2.4 Form field

```
Form field
  variants  stacked (default), inline (label left, 160px, in dense settings), two-up (.frow, two columns, one column below 640px)
  sizes     gap 6px between label, control, help; field gap 16px; label ui 12.5/600; help and error 12px
  states    default; focus-within; filled; readonly; disabled (reason); invalid (error message + crit glyph); warning (non-blocking, warn glyph); validating ("Checking...", spinner); locked by policy (lock icon + source label, see Inherit / override)
  tokens    label --text, help --text-muted, error text --danger with crit glyph, warning text --warning with warn glyph, control tokens per Input
  a11y      <label for>, help and error linked with aria-describedby, required fields marked with the word "(required)" in the label, optional with "(optional)"; error summary at the top of a dialog on submit lists each failing field as a link that focuses it; invalid fields get aria-invalid
  used on   S04 S05 S25 S27 S32 S38 B10 B15 B16
  notes     Validation timing: format checks on blur; mini-language fields (window, throttle, selection filters) on input, debounced 250ms; everything again on submit. Error copy says what is wrong and what is accepted: "Hour range 18-8 is not valid: start must be before end. For overnight use 18-24;0-8." Server rejection of a save keeps all entered values.
```

### 2.5 Input

```
Input
  variants  text, number (stepper buttons, inputmode numeric), password (show/hide icon button, autocomplete attributes set), search (clear button), textarea (min 3 rows, vertical resize), datetime (native datetime-local with the operator zone shown as a suffix), one-time code (6 digits, inputmode numeric, autocomplete one-time-code, mono 18px, tracking .3em), read-only copy field (value plus copy icon button)
  sizes     md 34px (forms), toolbar 32px, lg 40px (login and one-time code); padding 0 10px; icon 14px
  states    default, hover (border --text-muted), focus (2px ring plus border --accent), filled, readonly (text --text-muted), disabled (reason), invalid, warning, loading (validation spinner at the trailing edge)
  tokens    bg --bg, border 1px --border-input, radius --radius-sm, text --text (sans `ui` for prose values, --font-mono for technical values: tokens, ids, windows, version strings), placeholder --text-muted, caret --accent
  a11y      native elements; type and autocomplete set correctly (password managers, one-time-code); never placeholder-as-label; password show/hide is a button with aria-pressed; units are visible suffix text ("min", "GiB") and included in the label
  used on   S01 S04 S05 S25 S27 S32 S38 B10 B15 B16 B09 S21
  notes     Secrets are never echoed back into a value attribute after a failed post. spellcheck="false" on technical fields. Counters show only when the API defines a limit; surface the server's own error text for limits.
```

### 2.6 Select

```
Select
  variants  native (default), combobox (type-to-filter, for long lists such as customer pickers), multi (checkbox list in a popover)
  sizes     md 34px, toolbar 32px; popover rows 32px
  states    default, hover, focus, open, disabled (reason), invalid, loading options (combobox: "Loading..." row), no options ("No customers yet. Add one first." with a link)
  tokens    as Input; chevron glyph --text-muted; popover per Menu; selected option marked with a check glyph and --accent-soft
  a11y      native <select> by default (best keyboard and screen-reader support); combobox follows the ARIA 1.2 combobox/listbox pattern; multi-select follows the Column-picker pattern with a count in the button ("3 selected")
  used on   S04 S05 S25 S27 S21 B16 B15
  notes     Option text is sentence case; the empty choice is explicit ("Inherit from policy"), never a blank row.
```

### 2.7 Checkbox, radio and toggle

```
Checkbox / Radio / Toggle
  variants  checkbox (incl. indeterminate), radio, toggle (switch), option card (radio or checkbox presented as a bordered card with title, description and optional consequence line)
  sizes     checkbox 16x16px (radius xs), radio 16px, switch track 34x20px with 14px thumb; the whole row including label is the 32px hit target; option card padding 10px 12px, radius md
  states    unchecked, checked, indeterminate, hover, focus-visible, disabled (reason), invalid (group message), locked by policy (lock icon plus source label), card selected (--accent border and --accent-soft background plus the filled radio dot)
  tokens    border 1.5px --border-input, checked bg --accent with --on-accent mark (check, dash or 5px dot), switch off track --border-input, on track --accent with a visible "On"/"Off" word beside it, card border --border, selected border --accent, consequence line text --warning with warn glyph at weight 600
  a11y      native <input type="checkbox|radio">; switch is a checkbox with role="switch"; groups use <fieldset><legend>; a switch always shows its state as text as well as position; option cards are one <label> wrapping the input so the whole card is clickable
  used on   S04 S25 S27 S38 B10 B12 B16 B13
  notes     Policy toggles take effect on Save; only per-viewer display preferences apply immediately. Option card consequence copy states the cost plainly ("No server-side retention, no technician restore, no escrow check. If the key is lost, the data is lost.").
```

### 2.8 Segmented control

```
Segmented control
  variants  2 to 4 mutually exclusive options (Dark/Light, Grouped/Flat, Last/Worst, Issue only on/off)
  sizes     height 32px; segment padding 8px 12px; text ui 12/500
  states    default, hover, selected (--accent bg, --on-accent text), focus-visible (inset ring), disabled segment (reason)
  tokens    border 1px --border, radius --radius-sm, overflow hidden, unselected text --text-muted
  a11y      container role="group" with aria-label; each segment is a <button aria-pressed>; arrow keys move focus between segments, Enter/Space selects
  used on   B06 B01 S19 S38 (theme switch)
  notes     More than four options becomes a Select. The selected segment is also bold (weight 600) so it is distinguishable without colour.
```

### 2.9 Tooltip

```
Tooltip
  variants  text (one line or short sentence), reason popover (title + body, used by state chips), rich (sparkline day detail)
  sizes     max-width 280px (reason popover 320px), padding 6px 8px, radius sm, offset 6px from the trigger
  states    hidden, shown on hover (400ms delay) or focus (immediate), dismissed with Esc, hoverable (pointer can move onto it without it closing), persistent while focused
  tokens    bg --surface-raised, border 1px --border, text --text at ui 12px, title weight 600, shadow --shadow-pop
  a11y      role="tooltip" referenced by aria-describedby on the trigger; satisfies WCAG 1.4.13 (dismissible, hoverable, persistent); touch: tap on the trigger toggles it; never the only place a fact appears
  used on   every screen
  notes     Implemented with the native Popover API (popover="hint") where available. Reason popover copy: first line is the state and its start ("FAILED since Fri 02:10"), second line the reason in one sentence, optional third line what happens next.
```

### 2.10 Menu (actions dropdown and row menu)

```
Menu
  variants  actions dropdown (button with caret), row menu (icon button "..."), context group with a collapsible Advanced section, user menu
  sizes     min-width 200px, max-width 320px, item height 32px (padding 0 12px, gap 10px, icon 14px), radius md; max-height 70vh scrolls
  states    closed, open; item: default, hover/active-descendant, focus, disabled (reason line under the label), destructive (text --danger, always opens a confirm dialog), checked (check glyph), loading; Advanced group collapsed or expanded
  tokens    bg --surface-raised, border 1px --border, shadow --shadow-pop, text --text, disabled and reason --text-muted, separators 1px --border, group label per Section label
  a11y      trigger has aria-haspopup="menu", aria-expanded, aria-controls; popover role="menu", items role="menuitem" (real <button>), Down/Up/Home/End move, typeahead, Enter/Space activate, Esc closes and returns focus to the trigger, Tab closes; items stay focusable when disabled so the reason is reachable
  used on   S05 S24 B07 B08 S19 S03 (row and bulk actions)
  notes     Opening fetches a fresh fragment (hx-get on click) so availability is current. Advanced group heading is a button with aria-expanded; it is collapsed by default and the expanded state is remembered per viewer (try/catch storage).
```

## 3. State language

### 3.1 Device state chip

```
Device state chip
  variants  one per primary state in architecture 4.2 (17, catalogue below); RUNNING overlay (open run); qualifier suffix; count mode (rollup chips, 3.6)
  sizes     md 24px (tables, headers, banners, panel head), sm 20px (key-value lists, dense rows, secondary RUNNING chip); pill; glyph 8px (warn triangle 9px)
  states    default; hover or focus (reason tooltip); acknowledged (chip unchanged, an "ACK" attribute badge follows); snoozed (chip unchanged, "SNOOZED until T" attribute badge follows); updating (live swap: 1s --accent-soft flash, none when reduced motion); stale (data-stale: pulse removed, suffix "as of hh:mm"); pulse (RUNNING only, 1.6s ring, off when reduced motion)
  tokens    height 24px; padding 0 9px 0 7px; gap 6px; border 1px; radius --radius-pill; ui-sm 11.5/1/600 tracking .02em in --font-sans (never Orbitron). Colours by class: ok --success on --success-soft; info --accent on --accent-soft; warn --warning on --warning-soft; crit --danger on --danger-soft; neutral --text-muted on transparent with border --border. Border for coloured classes is the text colour at 35% (40% for warn) via color-mix over transparent. Glyphs per 0.1.
  a11y      the label text is the accessible name; the glyph is aria-hidden; inside a table State column the chip is a <button type="button"> that opens the reason tooltip so keyboard users get the reason; elsewhere it is a <span>. A live state change announces once through a polite region ("FRONTDESK-01 is now FAILED x3"), only for transitions into a crit class, at most one per 5 seconds
  used on   S06 S19 S23 B01 B02 B03 B04 B05 B07 B08 (and A14 B14 in tray form, 11.2)
  notes     Live (stale rule 0.7: fleet.state swaps the row's chip). Hostnames never sit inside the chip. Class derives from the engine, not from the UI: the template receives Primary, Flags, Since, Reason and Alarm from Classify and does no classification of its own. A device in an unknown or unrecognised state renders the UNKNOWN chip, never OK.
```

**Catalogue (primary states, precedence order; first applicable wins)**

| # | state | chip label | qualifier pattern (example) | class | engine severity | hover reason (line 2) |
| --- | --- | --- | --- | --- | --- | --- |
| 1 | RETIRED | `RETIRED` | `RETIRED . since 2 Oct` | neutral | none | "Retired. Excluded from rings and rollups." |
| 2 | PAUSED | `PARKED` | `PARKED . until Fri 18:00` or `PARKED . indefinite` | neutral | none (suppresses 6-9, 10-11, 14-16) | `Parked by {user}: "{reason}". Retention still runs.` |
| 3 | UNKNOWN | `UNKNOWN` | `UNKNOWN . facts unavailable` | crit, dashed border | critical | "Could not load this device's facts ({error class}). Counted as unhealthy until it loads." |
| 4 | IDENTITY_CONFLICT | `IDENTITY CONFLICT` | none | crit | critical | "A second machine presented this device's identity. Backups continue; resolve it to choose which machine keeps it." |
| 5 | PENDING_FIRST | `PENDING FIRST` | `PENDING FIRST . 41h left` or `PENDING FIRST . 62%` while running | info | none | "Waiting for the first backup. Grace ends {T}. {n} attempts, last error class {class}." |
| 6 | BLANK | `NEVER BACKED UP` | reason chip `never reported`, `no successful run` or `only failures` plus error class | crit | critical | "No successful backup since enrollment and the grace period ended {T}." |
| 7 | STUCK | `STUCK` | `STUCK . running 76 h (limit 12 h)` | crit | critical | "The run has exceeded its limit. 100% done is not health." |
| 8 | FAILED | `FAILED` | `FAILED x3` (consecutive failures) | crit | critical | "{n} consecutive failures since {T}; class {class}." |
| 9 | PARTIAL | `PARTIAL` | `PARTIAL . 23 files` | warn | alarm | "{n} files unreadable, over the tolerance of {limit}; classes {classes}." |
| 10 | UNSEEN | `UNSEEN` | `UNSEEN 9d` | crit | critical | "No heartbeat for 9 days. Likely cause: certificate, DNS or the machine is off." |
| 11 | STALE | `STALE` | `STALE 4d` | crit | critical | "No successful backup for 4 days; the limit is {stale_after}." |
| 12 | ESCROW_UNVERIFIED | `ESCROW UNVERIFIED` | none | warn | alarm | "The recovery key copy was not verified ({error class or age})." |
| 13 | MAINT_FAILED | `RETENTION FAILED` | `RETENTION FAILED . prune` | warn | alarm | "{operation} failed ({class}). Storage will keep growing until it succeeds." |
| 14 | MISSED | `MISSED RUN` | none | warn | none (early warning) | "A scheduled run did not start. This becomes STALE if it continues." |
| 15 | DEFERRED | `DEFERRED` | `DEFERRED . battery 14%` (gateway, lock, battery, metered, window, conditions) | warn | none until STALE | "Waiting on {reason} since {T}. Digest lists devices deferred over 24 h." |
| 16 | OUTDATED | `OUTDATED AGENT` | `OUTDATED . 1.4.2 to 1.5.0` | warn | none (alarm only if overridden) | "Agent {v} is older than the expected {v}." |
| 17 | OK | `OK` | none | ok | none | "Last success {age}. All checks passed." |

Overlay **RUNNING** (not a row in 4.2; shown when an open run exists): `RUNNING 62%`, class info, pulse. It replaces the OK chip; for any other primary state it appears as an additional sm chip after the primary chip, and the primary chip never changes. PENDING_FIRST shows its own progress qualifier instead.

### 3.2 Run outcome chip

```
Run outcome chip
  variants  success, partial, failed, cancelled, skipped, deferred, interrupted, abandoned (8 outcomes, table below); RUNNING (open run, live); UNKNOWN OUTCOME (value from a newer agent, treated as failure per 4.4); optional retry count suffix
  sizes     md 24px (run lists, panel history), sm 20px (log headers, calendar tooltips)
  states    default; with retry count ("FAILED . 2 retries"); hover or focus (tooltip with exit class and duration); live (RUNNING pulses, stale rule applies)
  tokens    same geometry, type and glyphs as the Device state chip; colours by class below
  a11y      text is the name; glyph aria-hidden; in a table it is plain text inside the cell (not a button) because the row link carries the action
  used on   S16 S21 S22 B06 B07 A09 (A03 not built in v1; tray uses state, not outcome)
  notes     Live only for RUNNING. Copy for qualifiers is classes and counts, never paths: "FAILED . vss_snapshot", "PARTIAL . 4 files unreadable . under tolerance".
```

| outcome | chip label | class | result class (4.4) | qualifier example |
| --- | --- | --- | --- | --- |
| success | `SUCCESS` | ok | success | `312 new . 1.1 GB added` |
| partial | `PARTIAL` | warn | success if within tolerance (add `under tolerance`), else partial | `4 files unreadable . under tolerance` |
| failed | `FAILED` | crit | failure | `vss_snapshot . 1 volume` |
| cancelled | `CANCELLED` | neutral | neutral | `cancelled by {user}` |
| skipped | `SKIPPED` | neutral | neutral | `already running` |
| deferred | `DEFERRED` | warn | neutral | `battery 14%` |
| interrupted | `INTERRUPTED` | neutral | neutral | `agent restarted` |
| abandoned | `ABANDONED` | crit | failure | `no progress for 180 min` |
| (open) | `RUNNING 62%` | info, pulse | n/a | `23,418 of 37,902 files` |
| (unrecognised) | `UNKNOWN OUTCOME` | crit | failure | `reported by agent 1.9.0` |

### 3.3 Attribute badge

```
Attribute badge
  variants  state-flag (any non-primary applicable state shown as a badge), fact (offline, customer-held key), reason (overdue list: missed run, no success in N days, interrupted, abandoned), override, suspended, acknowledgement (ACK, SNOOZED until T)
  sizes     height 20px; padding 0 7px; radius --radius-xs (rectangular, to read as "attribute" next to the pill chips); glyph 8px; gap 5px
  states    default; hover or focus (tooltip with the fact and its source); present or absent only; stale (as-of marker via tooltip)
  tokens    ui-sm 11.5/600 in --font-sans; border 1px --border; text --text-muted; glyph colour by class (0.1); background transparent, or the class soft fill for warn and crit badges
  a11y      plain <span> with the full text (no icon-only badges); never the only place a critical fact appears; tooltip optional
  used on   S23 B01 B04 B05 B07 B08 (S06 S19 as device lists)
  notes     Order after the primary chip: crit, warn, info, neutral; maximum 3 visible, then "+N" which opens a popover listing all (never silently dropped; the count is always shown).
```

| badge | text | class | source |
| --- | --- | --- | --- |
| offline | `OFFLINE` | neutral | no heartbeat for more than 2 min (connectivity is not health) |
| customer-held key | `CUSTOMER-HELD KEY` | neutral | custody mode |
| policy drift | `POLICY DRIFT . v11 vs v12` | warn | applied policy version differs |
| clock skew | `CLOCK SKEW +4 min` | warn | offset beyond 2 min |
| partial under tolerance | `PARTIAL . UNDER TOLERANCE` | info | last run, within tolerance |
| vss fallback | `VSS FALLBACK . C:` | info | volume name only |
| no schedule | `NO SCHEDULE` | crit | policy has no schedule (the device will never back up on its own) |
| escrow | `ESCROW UNVERIFIED` / `ESCROW MISSING` | warn | flag form of state 12 (missing = no active escrow row) |
| outdated agent | `OUTDATED AGENT` | warn | flag form of state 16 |
| any other applicable state | the chip label | its class | every applicable row is kept as a flag |
| acknowledged | `ACK . {user} . {T}` | neutral | mutes notifications only; the alarm stays visible |
| snoozed | `SNOOZED until {T}` or `SNOOZED until next success` | neutral | read-time evaluation |
| override | `OVERRIDE` | info | device value differs from policy |
| suspended | `CUSTOMER SUSPENDED` | neutral | notifications muted, states still shown |
| reason (overdue) | `MISSED RUN`, `NO SUCCESS 4 d`, `INTERRUPTED`, `ABANDONED` | warn or crit by severity | B04 |

### 3.4 Filter chip

```
Filter chip
  variants  removable (active filter), toggle (on/off quick filter such as Troubled, Warned), state-linked (carries a glyph)
  sizes     24px visible, hit area 32px; padding 0 6px 0 9px; remove button 20px inside
  states    default; hover; focus-visible; selected (toggle: --accent-soft, --accent border, check glyph); pressed removal; disabled (never)
  tokens    radius --radius-pill; border 1px --border-input; ui-sm 11.5/600; remove glyph --text-muted
  a11y      toggle chips are <button aria-pressed>; removable chips: the label is text and the remove control is a separate <button aria-label="Remove filter: Last success over 72 h">; the bar is a <ul> with a visible count "Showing 7 of 212"; "Clear all" is a ghost button
  used on   B01 B03 B04 B05 S19 S21
  notes     Selecting an age-ring segment, a KPI tile or a saved view adds the matching chips; removing a chip updates the ring selection and the URL (hx-push-url) so every filtered view is deep-linkable.
```

### 3.5 Customer group header

```
Customer group header
  variants  expanded, collapsed healthy row (one green row: name, device count, ok count, gauge), MISSING, ONBOARDING, EMPTY, customer-wide silence, with UNKNOWN devices
  sizes     row min-height 38px; padding 9px 10px; chevron button 32px; MISSING left border 3px
  states    default; hover; focus-visible; expanded or collapsed (aria-expanded); acknowledged (MISSING keeps its colour and gains an "ACK" badge with who and when; acknowledging mutes notifications only); stale (as-of marker)
  tokens    bg --surface, name --font-sans 600, count --font-mono 11px --text-muted, rollup chips right-aligned (3.6), MISSING: bg --danger-soft, border-left 3px solid --danger, chip crit; ONBOARDING chip info; EMPTY chip neutral; separators --border
  a11y      one <tbody> per customer with <tr><th scope="rowgroup" colspan="n">; the chevron is a <button aria-expanded aria-controls> named "Summit Eye Care, 31 devices, 3 critical"; MISSING reads "Missing: Blue Ridge Accounting is expected but has no devices"; customer-wide silence reads "Customer-wide silence: 38 of 44 devices unseen"
  used on   B01 B08 S03 S19
  notes     Live (fleet.state swaps cust-<id>). Sort order of groups: MISSING, groups containing UNKNOWN, ALARM, WARN, OK, ONBOARDING, EMPTY. Healthy customers default to collapsed; exception customers default to expanded. Copy: MISSING "MISSING . 0 devices . expected" with action "Acknowledge"; ONBOARDING "ONBOARDING until Fri 18:00"; EMPTY "NO DEVICES" (never green, never counted in health %); silence "CUSTOMER-WIDE SILENCE . 38 of 44 unseen" with the per-device rows listed underneath. An expected customer with zero devices is a critical alarm, never an empty state.
```

### 3.6 Customer rollup chips

```
Customer rollup chips
  variants  count chips (glyph + number, worst first: crit, warn, info, neutral, ok); customer state label (MISSING, ONBOARDING, ALARM, WARN, OK, EMPTY, with class below)
  sizes     md 24px; count chips are as narrow as glyph + digits; gap 6px; right-aligned in the group header
  states    default; hover (tooltip: "2 FAILED, 1 UNSEEN"); focus; selected when used as a filter; zero counts hidden except ok; all hidden shows the EMPTY label; stale
  tokens    same geometry and tokens as the Device state chip
  a11y      each chip's accessible name is "3 critical", "1 warning", "27 ok"; the group is a <ul> with aria-label "Devices by state"; a chip that filters is a <button>
  used on   B01 B08 S03 B03
  notes     Customer state labels: MISSING crit; ONBOARDING info; ALARM crit; WARN warn; OK ok; EMPTY neutral. A paused-only customer is WARN ("all devices parked"). Health percent uses the 4.5 formula; a zero denominator renders "n/a", never 100%. Live (fleet.state replaces cust-<id>); stale rule 0.7 applies.
```

## 4. History, health and live visuals

### 4.1 Ten-day strip

```
Ten-day strip
  variants  table (10px glyph cells), panel (14px glyph cells with day labels); cell kinds: success (mint dot), mixed (dot split mint and coral), missed (amber triangle), failed (coral diamond), none (hollow ring)
  sizes     table: 10px glyphs, 3px gap, strip height 32px hit area; panel: 14px glyphs, 6px gap, weekday initials below in meta-mono 10.5px; always exactly 10 cells, oldest left, today right
  states    default; hover or focus on the strip (tooltip per day: "Tue 20 Oct: 2 runs, 1 failed"); paused (all affected cells render as none, strip caption "Parked"); new device (leading cells none; caption "enrolled 3 d ago"); today partial (today's cell has a 1px --accent underline); loading (10 skeleton rings); error (10 dashed rings plus "history unavailable" text, never a clean green strip); stale
  tokens    success --success; mixed --success and --danger split on a diagonal; missed --warning triangle; failed --danger diamond; none 1.5px ring in --border-input; underline for today --accent
  a11y      the strip is one link (to the device History tab) with one tab stop; role="img" semantics are not used, instead the link has an accessible name "Last 10 days, oldest first: 7 success, 1 mixed, 2 failed; latest failed"; inside the panel variant the cells are a roving-tabindex list (arrow keys move, Enter opens that day in Runs); every cell kind has a distinct shape so colour is not the only carrier
  used on   B01 B02 B07 B08 S23
  notes     Buckets are calendar days in the customer's display_tz (a DST day has 23 or 25 hours). Cell rules (4.7): success = at least one success and no failure that day; mixed = success and failure; missed = a day the schedule expected a run and none happened, device not parked, retired or pending; none = before enrollment, parked or retired. A day with failures and zero successes is not defined by 4.7 and renders as failed (flagged in the report). Data from GET /api/v1/devices/{id}/ten-day, live via fleet.state; when the stream is lost the strip keeps its last data and the row shows "as of hh:mm". The concept renders all cells as colour-only circles; this spec adds shapes.
```

### 4.2 Day-block activity grid

```
Day-block activity grid
  variants  all-ok (mint), any-failed (coral), mixed (diagonal stripes of mint and coral), missed (amber with triangle), none yet (outlined empty block); with count label; row end shows attribute badges (OFFLINE, OUTDATED AGENT, NO SCHEDULE)
  sizes     block 56x28px, radius xs, 4px gap; day header columns 56px; first column (device) sticky, 240px; row height 36px
  states    default; hover (outline --border-input and tooltip "3 jobs: 2 success, 1 failed"); selected (2px --accent outline); focus-visible (roving tabindex); today (header marked with --accent underline); loading (skeleton rows); empty-is-alarm (zero devices renders the empty-fleet alarm panel, never a blank grid); stale
  tokens    all-ok bg --success-soft with --success dot and count; any-failed bg --danger-soft with --danger diamond and count; mixed bg stripe pattern of the two soft fills with a split glyph; missed bg --warning-soft with --warning triangle; none border 1.5px --border-input; count text meta-mono 11px --text; header label-mono 10.5px --text-muted
  a11y      role="grid" with row and column headers; each block is a <button> named "FRONTDESK-01, Tue 20 Oct: 3 jobs, 1 failed"; arrow keys move, Enter opens the day in Runs; counts are text, glyphs add shape
  used on   S23 B01
  notes     Click filters Runs to that device and day (hx-get /runs?device=&day=, pushed to the URL). Day columns follow each row's customer display_tz and a header notes when the zone differs from the operator's. A "Show retired" checkbox above the grid is off by default and says how many are hidden ("2 retired devices hidden"). Never silently cap rows: the pager states "Showing 1-50 of 212". Live (fleet.state replaces the device row, so today's block updates); stale rule 0.7 applies.
```

### 4.3 Calendar-matrix cell

```
Calendar-matrix cell
  variants  success (mint dot), warning (filled amber triangle: partial or under-tolerance), failed (coral diamond), missed (outlined amber triangle), scheduled (dashed ring, future), running (ice square, pulse), deleted (hatched, device retired that day); view toggle Last or Worst (what the cell shows when a day has several runs)
  sizes     cell 28x28px desktop (32px with 4px gap on touch), glyph 10px; device column sticky 240px; month header row sticky
  states    default; hover (tooltip: day, run count, last or worst outcome); selected (2px --accent outline, opens run detail); focus-visible; today (header underline --accent); weekend (column bg --surface); loading (skeleton matrix); error (crit inline banner, grid dimmed); month with gaps (row end count "3 missed" as a warn badge)
  tokens    success --success, warning --warning (filled), missed --warning (outline 1.5px), failed --danger, scheduled --border-input dashed, running --accent, deleted pattern of --border at 45deg; cell bg --surface, border 1px --border, radius xs
  a11y      role="grid"; roving tabindex; each cell is a <button> named "LAB-RETINA-02, 14 Oct: missed"; Enter opens the day's run in the side panel; legend lists every glyph with its word; Issue only (checkbox) and Last/Worst (segmented control) are real controls above the grid; month navigation buttons "Previous month" and "Next month"
  used on   B06
  notes     Controls above the grid: month stepper, Last/Worst, Issue only, run type select, Export CSV button. Export respects the filters and says so ("Exports 212 devices x 31 days"). Not built until milestone M11. Dates use the customer's display_tz per row.
```

### 4.4 Age ring (doughnut)

```
Age ring (doughnut)
  variants  last-success age (default), state mix, unseen devices, customers within SLA; sizes md 120px, lg 144px (when the centre text exceeds 3 characters)
  sizes     SVG viewBox 120, radius 46, stroke 14, segment gap 2px, centre big number 23px (.t-big-number) with a mono caption (9-10px) below; legend column beside it (ring card: 120px + 1fr, gap 14px)
  states    default; segment hover (stroke +2px, tooltip count and percent); segment selected (stroke +4px, --shadow-glow on the segment, filter chip appears); legend row hover or focus mirrors the segment; empty-is-alarm (zero devices: full dashed --danger ring, centre "0", caption "No devices", C08 banner present); n/a (only parked or retired devices: centre "n/a"); loading (skeleton ring); error (dashed --danger ring, "unavailable"); stale (as-of caption)
  tokens    track --border, segments --success (under 24 h), --warning (24 h to the stale limit), --danger (over the limit or never), --border-input ring (parked or retired); customer ring uses --accent for "fully within SLA" as in the concept; centre value .t-big-number in --text; caption --font-mono --text-muted; card per Card (compact)
  a11y      the SVG is role="img" with an aria-label summarising all buckets; the legend rows are the keyboard path: each is a <button> "199 devices, last success under 24 hours; filter" with a glyph (dot, triangle, diamond, ring) as well as colour; segments are mouse targets only and mirror the legend
  used on   B03 B01
  notes     Live (fleet.state). Click or Enter sets the board filter (hx-get /ui/fleet/customers?age=...) and pushes the URL. Bucket edges are computed from the policy sla (default: under 24 h, 24 h to stale_after_hours, beyond or never); legend labels print the real edges. Recon quotes 24-48 h and over 48 h; the concept and the 72 h default say 24-72 h; the legend always prints the active edges. An empty ring is never neutral: zero devices is the C08 alarm. Concept sets the ring centre at 18px, below the 23px Orbitron floor for big numbers; this spec uses 23px with the larger ring for long values.
```

### 4.5 Health gauge

```
Health gauge
  variants  inline (customer group header), card (customer page); each pairs a percent with a 7-day sparkline
  sizes     inline: track 72x6px, radius pill, percent in meta-mono 600 12px, sparkline 7 bars 3px wide, 2px gap, 18px tall; card: track 100% wide x 8px
  states    default; degraded (customer rollup ALARM or MISSING: the fill takes --danger and a crit glyph precedes the percent); warn (WARN rollup: --warning and a warn glyph); n/a (zero denominator: track empty, text "n/a", never 100%); loading (skeleton); stale (as-of in the tooltip)
  tokens    track --border; fill by the customer's rollup class (ok --success, warn --warning, crit --danger), never by a percent threshold; sparkline bars --border-input, latest bar --accent; text --text
  a11y      role="meter" aria-valuemin 0 aria-valuemax 100 aria-valuenow, aria-valuetext "94 percent of devices within SLA; 7-day trend 91, 92, 90, 93, 94, 94, 94"; sparkline is aria-hidden with the values in the valuetext
  used on   B01 B08
  notes     Percent = devices with a fresh success and no critical or alarm state, divided by active devices minus parked and retired (4.5). No colour threshold is defined by percent because thresholds are not specified; the colour is the rollup state. Live via fleet.state.
```

### 4.6 KPI tile

```
KPI tile
  variants  neutral, good, warn, bad (tone of the value); with unit text; with sparkline (7 bars); linked (a filter shortcut)
  sizes     min-width 0 in a grid of 5 columns at 900px and wider, 3 columns from 640 to 899px, 2 columns below 640px; padding 12px 14px; gap 12px between tiles; radius md; value 23px (.t-big-number), unit 12px sans; sparkline 22px tall, 2px bar gap
  states    default; hover and focus (only when linked: border --border-input, ring); loading (value skeleton); unavailable (value is an em dash, label "unavailable", crit glyph, never 0); zero (a real zero is shown, with the ok glyph for a good tone, e.g. Critical 0); stale (as-of caption under the value)
  tokens    bg --surface, border 1px --border, radius --radius-md; label label-mono 11px --text-muted; value .t-big-number, tone colour --text | --success | --warning | --danger; unit --font-sans 12px --text-muted; sparkline bars --border-input at 70% opacity, latest bar --accent
  a11y      a tile is a <dl>-style pair or a single <a>; the tone is also carried by a leading state glyph before the label (dot, triangle, diamond) so the colour is not alone; the value has a visually hidden unit ("94.3 percent"); the sparkline is aria-hidden and its values are in a visually hidden list "7-day: 85, 80, 60, 70, 88, 92, 94"
  used on   B01 B03 B07 B08 B11 (and digest email counts, 10.2)
  notes     Live (fleet.state). Five tiles on the board: Devices (customers count as unit), Within SLA (percent), Critical, Warnings, Backed up today (running count as unit). Devices equals 0 is the C08 alarm and the tile takes the bad tone with a diamond. Linked tiles apply the matching filter chip. Orbitron is used for the value only; labels and units are mono and sans.
```

### 4.7 Quota meter

```
Quota meter
  variants  bar (customer and target level), compact (device row); with growth rate and projection
  sizes     bar 8px tall, radius pill, full width; tick marks at 80% and 90%; label row above (usage text left, state chip right); growth line below in meta-mono
  states    under (below 80%, bar --accent, optional ok chip), approaching (80-89%, --warning, chip "APPROACHING" warn), nearly full (90-99%, --danger, chip "NEARLY FULL" crit), exceeded (100% or more, bar full --danger with hatching, chip "EXCEEDED" crit), growth alarm (projection reaches 100% in under 14 days: warn or crit glyph and line "full in about 9 d"), no quota set (text "No quota set" in --text-muted, never an empty bar), loading, unavailable ("usage unavailable", crit glyph)
  tokens    track --border, fill by level as above, ticks --border-input, text meta-mono 12px --text, copy sans 12px --text-muted
  a11y      role="meter" with aria-valuenow, aria-valuetext "62 percent of 2.0 TB, growing 1.4 GB per day"; level is carried by the chip text and glyph as well as colour
  used on   B11 B08 (condition banner C15 for the fleet level)
  notes     Thresholds are the C15 values (80% warn, 90% and 100% critical, growth alarm under 14 days). The quota is soft: copy says "Soft quota: backups continue." Usage is sampled (usage_samples), so the line also prints "sampled hh:mm". Next prune date: "Next prune in 12 d". Not live: usage is sampled, so the meter refreshes with its page and always prints the sample time; it has no pulse or push, so there is nothing to freeze.
```

### 4.8 Live run progress

```
Live run progress
  variants  panel (device summary and run page), row (compact in a table row: 6px bar plus percent), indeterminate (scanning phase, before a total is known)
  sizes     panel padding 10px 12px, radius md, bg --bg, border 1px --border; track 6px, radius 3px; meta row meta-mono 11px, gap 14px
  states    running; stalled (no progress for the policy stall limit: warn glyph and text "No progress for 12 min (limit 180 min)"); cancelling (Cancel button loading, text "Cancelling..."); done (collapses to the Run outcome chip after the final event, with the summary line); failed (crit chip, error class); stream-lost (bar freezes, pulse stops, caption "Progress as of 13:41:52. Live updates disconnected."); indeterminate; waiting-for-agent (command queued, device offline: "Queued. Starts when FRONTDESK-01 next checks in.")
  tokens    label "Live . backup . scheduled . started 13:40" ui 12px with the kind in --accent; percent meta-mono; track --border; fill --accent with a gradient to color-mix(--accent 60%, --text); meta items files, bytes processed, new bytes, rate, ETA in --text-muted; Cancel is a danger ghost sm button
  a11y      role="progressbar" aria-valuemin 0 aria-valuemax 100 aria-valuenow, aria-valuetext "62 percent, 23,418 of 37,902 files"; the updates are not announced per tick: a polite live region announces 25, 50, 75 and 100 percent, stall and completion
  used on   S21 S22 B07 (A09 and A03: agent-side progress is the tray status and CLI in v1)
  notes     Live: SSE run.progress keyed run-<id>, server-coalesced to 1 per second; fallback hx-trigger="every 5s" on a progress partial (route to be added to 5.3; it does not exist yet). If no event arrives for 30 s the caption shows "last update 40 s ago"; at 2x the refresh interval the stream-lost state applies (0.7). Initial runs may be hours long; the ETA is hidden until at least 2% is done. Cancel opens a confirm dialog ("Cancel this run? Files already uploaded are kept.") and queues a command; the control is aria-disabled with a reason for read-only roles. Copy never prints file names; counts only. Percent and bytes are mono, not Orbitron.
```

## 5. Data and tables

### 5.1 Data table

```
Data table
  variants  flat, grouped by customer (collapsible groups), with row selection, with column picker, with export, with live filter; dense (default 38px rows) and cards (below 640px: each row becomes a stacked card with name, state chip and last success)
  sizes     header row 30px; body row min 38px (padding 7px 10px, text ui 12.5px); group row 38px; checkbox column 32px; sticky header; horizontal scroll inside a focusable region when columns overflow
  states    default; row hover (recipe hover); row focus-within; row selected (--accent-soft, check in the checkbox column); sorted (aria-sort, arrow glyph); group collapsed or expanded; loading (header kept, skeleton rows equal to the page size); empty (three kinds below); partial-data (warn banner above the table, affected rows render as UNKNOWN); error (crit panel replaces the body, last good rows may show dimmed with an as-of stamp); stale (caption shows "as of hh:mm" and live swaps stop); updating (live row swap flashes --accent-soft for 1s)
  tokens    th: label-mono 10.5px --text-muted, border-bottom 1px --border; td: border-bottom 1px --border, vertical-align middle, no wrap; group row bg --surface weight 600; device cell: name --font-sans 500 (hostnames 600) with a second line meta-mono 11px --text-muted ("Windows 11 Pro . desktop"); ages --text-muted, over limit --danger weight 600 with a crit glyph, nearing --warning weight 600 with a warn glyph; sizes and versions --font-mono; sits on the dot-grid content ground; row action icon button full contrast; radius none (it sits on the page, not in a card)
  a11y      real <table> with <caption> (visually hidden: "Devices, grouped by customer, sorted by state"); <th scope="col"> with sort buttons (<button> inside the th; aria-sort ascending, descending or none); groups as <tbody> with a rowgroup header; aria-rowcount and aria-colcount when paged; row links are on the hostname (one link per row); selection: header checkbox is tri-state and named "Select all 50 rows on this page"; a polite live region announces "3 rows selected"
  used on   S03 S06 S16 S19 S21 B01 B04 B05 B09 S24 (S33 storage tables)
  notes     Live (fleet.state replaces tr id="dev-<id>"). Toolbar above the table, left to right: live filter (Input search, hx-get /ui/devices/table, hx-trigger="input changed delay:250ms, search", result text "7 of 212"), Grouped/Flat (Segmented control), column picker (Menu, multi-select, "Reset columns"), Saved views, Export (Menu: Copy TSV, Download CSV, Download XLSX only if the dependency budget allows; labelled "Exports all 212 matching rows, path-free"). Selection: shift-click range; selecting the page offers "Select all 212 matching" explicitly; the bulk bar replaces the toolbar ("3 selected . Acknowledge . Park . Clear") and every bulk action opens a confirm dialog naming the count. Default sort is exception-first: customer groups in rollup order, then rows by severity then age. Empty kinds: (1) filter-caused: neutral, "No devices match these filters. 0 of 212 shown." with Clear filters; (2) checked-and-clear list (Overdue, Unseen): neutral, "Nothing overdue. 212 devices checked at 13:42:10." with the evidence line; (3) NO DATA where data must exist (fleet with zero devices after enrollment, a customer expected to have devices, a failed or empty poll): ALARM, crit panel, never neutral, with the matching C08 or C09 banner. The first-run flag is the only neutral no-data case ("No customers yet. Add your first customer"). Do not truncate silently: every limited list states "Showing 1-50 of 212".
```

### 5.2 Pagination

```
Pagination
  variants  numbered (First, Previous, up to 5 page numbers, Next, Last), compact (Previous, "Page 2 of 5", Next; below 640px), page-size select
  sizes     buttons 32x32px; page-size select 32px; gap 4px; result text ui 12.5px
  states    default; hover; current page (aria-current="page", --accent bg, --on-accent text, weight 600); disabled First/Previous on page 1 and Next/Last on the last (aria-disabled, no reason needed beyond position); loading (the table region is aria-busy, buttons stay); beyond range (a page past the end redirects to the last page and says so)
  tokens    icon buttons per Icon button (secondary), text --text-muted, select per Select
  a11y      <nav aria-label="Pagination"> with a <ul>; each page is an <a> or <button> named "Page 3"; result text "Showing 51-100 of 212" is visible and in a polite live region; page-size options are explicit numbers (25, 50, 100, 200) and an "All" option when the total is under 500
  used on   S03 S19 S21 S16 B09 B04 B05
  notes     htmx: hx-get with hx-push-url so every page is deep-linkable. In grouped mode the pager counts customers and each page shows complete groups; the footer reads "Customers 1-5 of 5 . 212 devices". The text never omits the total.
```

### 5.3 Saved views control

```
Saved views control
  variants  dropdown (default), with built-in views (All, Unsuccessful backups as the default, Overdue, Unseen, Parked) and the viewer's own and shared views
  sizes     trigger is a Button md with a caret; popover min-width 260px, rows 32px
  states    closed; open; current view (check glyph); modified (trigger shows "Unsaved changes" dot and the label "Unsuccessful backups (modified)"); saving; delete confirm; empty custom list ("No saved views yet. Save the current filters to reuse them."); error (inline banner "View not saved: {reason}. Your filters are unchanged.")
  tokens    per Menu; shared views show an "Shared" attribute badge; built-ins are locked (no rename or delete) with a lock glyph
  a11y      Menu pattern; actions in the popover footer: "Save current as...", "Update this view", "Rename", "Delete", "Set as my default"; the save dialog is a Modal with one Input (name) and a "Share with other admins" checkbox
  used on   B01 B03 S19 S21 S03
  notes     A view is the query document (clause tree), columns, sort and grouping, stored through POST /api/v1/views (entity, name, filter, columns, sort, shared). Selecting a view navigates with the view id in the URL (hx-push-url), so a link restores it. The default view is applied on first load only; the page always says which view and which filters are active.
```

### 5.4 Query / clause builder

```
Query / clause builder
  variants  flat (AND-only list, runs search in M3), full (nested groups with AND, OR, NOT; milestone M10); read-only summary sentence
  sizes     each clause row 34px: field select (180px), operator select (140px), value control (flex), remove icon button; group indent 16px with a 2px --border rail; group header with an AND / OR / NOT segmented control
  states    empty (no clauses means "all", stated as "No filters: showing all 212"); valid; invalid clause (error on the row: "Choose a value", "Operator 'older than' needs a duration"); invalid group (empty group is rejected); saved (named view loaded); modified; counting (match preview "Matches 7 of 212" with spinner); error (preview failed, shown, builder still usable)
  tokens    per Form field and Input; group rail --border; group header label-mono; add buttons ghost sm ("Add condition", "Add group"); summary sentence --text-muted
  a11y      clause rows are a list; each control has an aria-label including the clause number ("Clause 2 field"); adding a clause moves focus to its field select; removing moves focus to the previous row or the Add button; the group operator is a Segmented control with aria-label "Group 1 combines with"; the summary sentence is a live region (polite) updated on change
  used on   S21 S18 S27 S20 B03
  notes     Fields come from the entity (devices: state, customer, last success age, agent version, OS, tag; runs: outcome, customer, device, kind, date). The operator list and the value control adapt to the field type (enum multi-select, duration, date range in the operator zone, contains). Impossible combinations are not offered rather than rejected. The preview count is a debounced (300ms) hx-post to the same endpoint the table uses, so the count and the table cannot disagree. Nesting limits are whatever the API enforces; surface its error text. Reused by search, reports, campaigns and saved views (one component, four contexts).
```

### 5.5 Log viewer

```
Log viewer
  variants  run log (S22, B07 Errors tab), live (follows a running job), aggregated (repeated errors collapsed to one row with a count)
  sizes     line height 20px; line padding 4px 10px; timestamp 70px, severity chip sm 56px, message flex; toolbar 40px; max-height 60vh then scrolls inside a focusable region
  states    loading (skeleton lines); empty-expected (a completed successful run with only info lines still shows lines; zero lines for any run shows a warn banner "No log lines were recorded for this run. The agent may have been interrupted"); long (shows the newest 500 lines and "Showing 500 of 12,431 lines. Load earlier"); filtered (counts "37 of 12,431 lines"); live (Follow toggle on, stream-lost marker per 0.7); error (crit banner with Retry); exporting
  tokens    bg --bg, border 1px --border, radius --radius-md; message --font-mono 12px --text; timestamp meta-mono --text-muted; severity chips use the state chip sm with ERROR crit, WARNING warn, INFO neutral; aggregated count "x23" in meta-mono; toolbar controls per Input, Checkbox, Button
  a11y      semantic list (<ol>) of lines; the viewport is tabindex 0 with an accessible name "Run log, 12,431 lines"; severity is a word as well as a chip; Follow is a switch; live variant uses role="log" with aria-live="off" and a "N new lines" button instead of announcing every line; filters have labels
  used on   S22 B07 (A09 not built in v1; the tray and CLI show status only)
  notes     Path-free always in v1: lines carry error classes, counts and volume names, never file paths or file names (HIPAA rule; the nopaths analyzer enforces it upstream). The recon's "path-free by default" has no opt-in in v1; any later opt-in needs its own warning dialog and spec. Filters: severity checkboxes (Error, Warning, Info), text filter (debounced 250ms), "Group repeated errors" switch (default on). The live variant has no log-stream event in the contract, so it polls a tail fragment (hx-trigger="every 5s"; proposed partial) and on a missed poll shows "log as of hh:mm:ss". Export downloads the filtered lines as text with a header (run id, device, time range) and states "Exported 37 lines".
```

### 5.6 Run history list

```
Run history list
  variants  last ten (device Summary and History tabs), full list (History tab with pager)
  sizes     row grid 70px | 1fr | auto | auto, gap 10px, padding 6px 8px, radius sm, 6px between rows
  states    default; hover and focus (row is a link to Run detail); running row pinned at the top with the live progress; empty for an enrolled device (warn: "No runs recorded since enrollment {T}"); loading; error; stale
  tokens    bg --bg, border 1px --border, date meta-mono 11px --text-muted ("Sat 02:10"), outcome chip md, summary ui 12px ("vss_snapshot . 1 volume"), exit code and duration meta-mono 11px --text-muted
  a11y      <ol> of links; each row's accessible name reads "Saturday 02:10, failed, vss_snapshot, 2 minutes 4 seconds"; exit codes are labelled "exit 3"
  used on   B07 S16
  notes     Zero rows for a device that has been enrolled longer than its grace period is a warn banner, not an empty state (a missing history is a silence signal). Links to /runs/{id}. Live only for the pinned running row (run.progress); stream loss shows the as-of marker on that row.
```

## 6. Containers

### 6.1 Tabbed side panel

```
Tabbed side panel
  variants  device panel (tabs Summary, History, Errors, Keys, Policy, Restore, Audit; route segment for Policy is "settings"), customer panel (tabs Overview, Devices, Runs, Policy, Notifications), full-page twin (same partials rendered as a page at the deep link)
  sizes     width 440px at 900px and wider, full-screen sheet below 900px; head padding 14px 16px 0; tab height 36px (padding 8px 10px, ui 12px/500); body padding 14px 16px, gap 14px, scrolls inside
  states    open (slides in --motion-base from the right; the board behind dims to 45% opacity and 60% saturation and becomes inert); closed; tab loading (body skeleton, head stays); tab error (crit inline banner in the body with Retry, head still shows the list data); stale (head shows "data as of hh:mm"); tab with attention (a glyph on the tab: Errors shows a count, Keys shows a warn triangle when escrow is unverified); unsaved edits in a Policy tab (leave prompts)
  tokens    bg --surface, border-left 1px --border, shadow --shadow-pop; title 16px sans 600 (the device name is a hostname, so sans 600); sub-row meta-mono 11.5px --text-muted (customer, OS, kind, agent, "online . seen 40 s ago"); state chip md at the right of the title; tabs --text-muted, current --accent with a 2px bottom border --accent; dividers --border
  a11y      role="dialog" aria-modal="true" aria-labelledby the title; main content is inert while open; focus moves to the panel title on open and returns to the originating row on close; Esc closes; tabs follow the ARIA tabs pattern (role="tablist", role="tab" with href, aria-selected, Left/Right/Home/End, role="tabpanel" aria-labelledby); the close button is an Icon button "Close device panel"
  used on   B07 B08 S06 S16 (S09 and S11 are merged into B17 and S25)
  notes     Deep links: GET /devices/{id}/{tab} renders the tab fragment (hx-get, hx-push-url, hx-target="#panel-body"); a full page load of the same URL renders the panel over the board. Live content inside (state chip, progress, history) follows the stale rule. A device that no longer exists shows "This device was retired or removed" with a link back to the board, never an empty panel. The concept labels the tab "Policy"; the route fragment is /settings.
```

### 6.2 Modal

```
Modal
  variants  form (create, edit), confirm, destructive confirm (type the name to enable), information, blocking error; sizes sm 440px, md 560px, lg 720px
  sizes     radius lg; head padding 16px 22px; body padding 20px 22px; foot padding 14px 22px; max-height 90vh with the body scrolling; below 640px it becomes a full-width bottom sheet
  states    opening (fade and 8px rise, --motion-base); open; loading (body skeleton); submitting (buttons loading, fields read-only); error (inline banner at the top of the body, focus moves to it); closing; dirty (closing asks "Discard changes?")
  tokens    bg --surface, border 1px --border, shadow --shadow-pop, scrim color-mix(in srgb, var(--bg) 70%, transparent); title 18px/1.3 sans 600; foot bg --surface-raised with border-top 1px --border; buttons right-aligned (Cancel secondary, primary or danger-solid last)
  a11y      native <dialog> opened with showModal(); aria-labelledby title and aria-describedby lead text; initial focus on the first field, or on the least destructive button for confirms; focus trapped; Esc closes unless submitting; focus returns to the invoker; page scroll locked; only one modal is open at a time (a second request replaces the content, it never stacks)
  used on   S04 S08 B15 B10 B16 B17 S25 (impact dialog) S38
  notes     Content is fetched by htmx (hx-get into #modal-root) and submitted by hx-post; a successful post closes the dialog and raises a toast, a failed post re-renders the dialog with the error. Destructive confirm (retire, purge, revoke): states the object and consequence ("Retire FRONTDESK-01? Its existing backups follow the retention shown in the dialog. This cannot be undone."), requires typing the device name, the danger-solid button is aria-disabled with the reason "Type the device name to continue" until it matches. Blast-radius dialog for policy saves lists how many devices change and which fields.
```

### 6.3 Wizard (stepper)

```
Wizard (stepper)
  variants  horizontal 3 to 5 steps (enrollment, restore, replace device), compact numeric "Step 2 of 4" below 560px; the installer (A01) is the MSI's own dialog set and is not a console component
  sizes     box width 720px max, steps bar columns equal, step cell padding 12px 16px, step marker 22px circle, step label ui 12.5px, body padding 20px 22px (gap 16px), foot padding 14px 22px
  states    step: todo (ring), current (--accent fill, number, label weight 600), done (--success fill with check, clickable to revisit), error (crit diamond marker, label "needs attention"), locked (future step, aria-disabled); wizard: loading step, submitting, back (values kept), expired (token or session expired: step 1 with banner), complete (summary with next actions)
  tokens    bar bg --surface-raised, border-bottom 1px --border; marker border 1.5px --border-input; current marker --accent with --on-accent; done marker --success with --on-status; labels --text-muted, current and done --text; body per Form field; option cards per Checkbox/Radio/Toggle; foot Back ghost, primary "Continue to script ->"
  a11y      the bar is an <ol> with aria-current="step" on the current item; focus moves to the step heading (tabindex -1) after each step change and the change is announced "Step 2 of 3: Key custody"; done steps are links, future steps are not; validation errors summarised at the top of the step with links to fields
  used on   B16 B17 A10 (S09 merged into B17) (A01 MSI; A04 not built)
  notes     htmx: each step is hx-get into the box with hx-push-url; Continue posts the step and the server validates and re-renders (never client-only validation); values live server-side in a short-lived wizard session so Back and refresh are safe. Enrollment: Customer and policy, Key custody, Deployment script. Restore: Snapshot, Browse and select, Mode, Progress. Copy: step titles are nouns; the heading is the question ("Who holds the encryption keys for Summit Eye Care?"). The final step shows the Code block with the script and the verification note.
```

## 7. Operator actions

### 7.1 Device action menu

```
Device action menu
  variants  panel action bar (Back up now, Restore..., Park or Resume, Refresh policy, Acknowledge, More menu) and full menu (Menu pattern); items grouped as Run, Manage, Advanced
  sizes     per Menu (32px rows) and Button sm in the action bar
  states    per item: enabled; queued-when-offline (label suffix chip "QUEUED" info; hint "Runs when FRONTDESK-01 next checks in. Expires {T}."; after choosing, the item shows "Queued hh:mm" with Undo until delivered); disabled with reason (aria-disabled, reason line); running (spinner); done; menu state: loading (skeleton rows while the fragment fetches), error ("Actions are unavailable. Retry")
  tokens    per Menu; QUEUED chip per Device state chip sm; destructive items --danger; reason text --text-muted
  a11y      per Menu; the reason is in aria-describedby so a screen reader hears why an item is unavailable; Undo is a real button with a polite status message
  used on   S07 S08 B07 B15 (S19 row menu)
  notes     Opening calls GET /ui/devices/{id}/actions so availability reflects current presence. Role gate: OP for run, pause, acknowledge, restore; AD for Retire and Replace; OW for key reveal. Availability by presence and state: see table. Choosing an enabled item posts via hx-post (for example /ui/devices/{id}/backup-now); Retire and Replace open the destructive Modal.
```

| action | online | offline or unseen | parked | retired | extra rule |
| --- | --- | --- | --- | --- | --- |
| Back up now | enabled | queued | disabled: "Parked until {T}. Resume first." | disabled: "Retired" | disabled while a run is open: replaced by Cancel run |
| Cancel run | enabled (only when a run is open) | queued | n/a (pause cancels runs) | n/a | confirm dialog |
| Restore... | enabled | enabled (restore reads the repository, not the device) unless custody is customer-held | enabled | disabled | disabled with reason "Customer-held key: no technician restore" or when C02 or C12 is active |
| Park | enabled | enabled (console-owned) | shows Resume | disabled | opens the pause dialog (1, 2, 4, 8, 24 h, until date, indefinite, reason) |
| Refresh policy | enabled | queued | queued | disabled | |
| Acknowledge | enabled when there is an alarm | enabled | enabled | disabled | opens the Acknowledge / snooze control |
| Retire... | enabled (AD) | enabled | enabled | disabled | destructive confirm, final-backup option |
| Replace device... | enabled (AD) | enabled | enabled | disabled | opens the replacement wizard (M9) |
| Resolve identity conflict... | only when IDENTITY_CONFLICT | same | same | disabled | confirm |
| Reveal key... | see Key reveal dialog | same | same | disabled | OW, fresh MFA |

### 7.2 Acknowledge / snooze control

```
Acknowledge / snooze control
  variants  popover form (row and panel), inline (on the MISSING banner and group header), bulk (from the selection bar)
  sizes     popover width 320px; fields stacked with 12px gap; buttons sm in rows, md in the popover footer
  states    default (not acknowledged: button "Acknowledge"); open (form); submitting; acknowledged (line "Acknowledged by {user} . {T}: '{note}'" and an ACK badge); snoozed ("Snoozed until {T}" or "Snoozed until next success"); expired (alarm re-raised, line "Snooze ended {T}", button back to Acknowledge); error (inline banner in the popover); disabled for a role without OP (reason)
  tokens    popover per Menu surface; note per Input textarea; radio options per Checkbox/Radio/Toggle; ACK and SNOOZED badges per Attribute badge
  a11y      popover is a small dialog (role="dialog" aria-label "Acknowledge FRONTDESK-01 alarm"); focus lands on the note field; options are a radio group with a legend "After acknowledging"; snooze date uses a native datetime-local shown in the operator zone; Esc closes and returns focus to the button
  used on   B04 B05 B07 B01 (MISSING)
  notes     Options: Acknowledge only; Snooze until [date and time] (quick picks +4 h, tomorrow 08:00, +3 d); Snooze until next success. A note is required and audited. Copy under the options: "Mutes notifications for this alarm. The alarm stays visible until the cause clears." Posts to /ui/devices/{id}/ack (or the customer equivalent). Snooze expiry is evaluated at read time, so an expired snooze re-raises with no job; the UI recomputes on every render and on fleet.state. An acknowledged alarm is never hidden or recoloured to look healthy.
```

### 7.3 Key reveal dialog

```
Key reveal dialog
  variants  device key (Keys tab), master-key status view (read-only, no reveal); gates: fresh MFA (TOTP or WebAuthn), reason, reveal, expiry
  sizes     Modal md 560px; revealed key block full width (Code block secret variant); countdown bar 3px under the key
  states    locked (fresh MFA required: code input or "Use security key" button); reason (required reason text and audit notice); revealing (loading); revealed (key shown once, copy-once button, countdown to expires_at); expired (key removed from the DOM, message "The key was cleared. Reveal again to see it."); denied (role below OW, reason shown, button aria-disabled); unavailable (C12 master key unavailable, or customer-held custody: "This key is held by the customer. CryoBackup never stores it."); MFA failed (error "Code incorrect or expired. Try again.")
  tokens    Modal tokens; key text --font-mono 14px --text on --bg, border 1px --warning (the revealed state is deliberately cautionary), warn glyph and the word "Shown once"; countdown --warning track on --border; audit notice per Inline banner (info)
  a11y      focus starts on the code field; the code input uses autocomplete one-time-code; the countdown has role="timer" and is not announced each second (announced at 10 s and at expiry); the key is selectable text so screen readers can read it; Copy is a button with polite status "Key copied. Clipboard is not cleared automatically."; the dialog cannot be dismissed by clicking the scrim while the key is shown
  used on   B10 B07 (Keys tab)
  notes     Flow: POST /api/v1/auth/reauth (step up), then POST /api/v1/devices/{id}/escrow/reveal with the reason; the password is returned once and never written to logs, URLs or storage; the response is Cache-Control no-store. Copy-once: the first Copy disables the button ("Copied"); the key stays visible until expiry or close so the operator can read it, then the node is replaced (not hidden with CSS). Audit notice copy: "This reveal is recorded with your name, the reason and the time. The key is shown once." The expiry duration is the server's expires_at, not a UI constant. When the server refuses (role, MFA, lockout) the dialog shows the server's message verbatim.
```

### 7.4 Inherit / override control

```
Inherit / override control
  variants  setting row (label, effective value, source label, action), section header (override count for a whole policy section), chain view (Global, Customer, Device with the effective level marked)
  sizes     row min-height 44px; source label ui 12px; action button sm; left rail 3px when overridden
  states    inherited (value read-only in --text-muted, source "Inherited from Summit . Workstation v12" with a link, button "Override"); overridden (editable field, left rail --accent, OVERRIDE badge, button "Reset to inherited"); locked by policy (aria-readonly, lock glyph, source "Locked by Summit . Workstation v12: prevent override", no Override button); pending (changed, unsaved: dot and "unsaved"); upstream changed ("Inherited value changed to {x}. Your override stays."); invalid (field error, per Form field)
  tokens    rail 3px solid --accent, OVERRIDE badge info, locked badge neutral, inherited text --text-muted, source label --text-muted with the policy name as a link (--accent); field per Input
  a11y      the row is a <div role="group"> labelled by the setting name; the source label is associated with aria-describedby; lock state is conveyed in text ("Locked") as well as the icon; Override and Reset are real buttons with the setting name in their accessible name ("Override schedule window")
  used on   S25 B07 B12 S18
  notes     Overrides are sparse: only changed fields are stored; Reset removes the key. Save runs the policy impact preview. The same component serves the notification inherit chain (global, customer, device) with the chain view. Copy: always name the source and the version.
```

## 8. Policy editors

### 8.1 Window / throttle mini-language field

```
Window / throttle mini-language field
  variants  window (when backups may start), throttle (rate limits by window); text-only (compact, inside tables), text + grid (default); read-only summary
  sizes     text input 34px mono; grid: 7 rows (Mon to Sun) x 24 columns (00 to 23), cell 20x20px with 2px gap at 900px and wider, 14px cells below 640px; row header 44px; hour header every 6 columns labelled in meta-mono 10px; grid summary line below
  states    valid (grid mirrors the text); invalid (text error under the input with a caret marker at the failing column; the grid keeps the last valid window at 60% emphasis with the caption "Showing the last valid window"); empty (no hours allowed: warn "No hours are allowed: backups will never start"); all-week (shown as "*/0-24 (always)"); editing in the grid (text rewrites to the canonical shortest form, with Undo); validating (spinner, 250ms debounce); read-only; locked by policy (Inherit / override); overlap (throttle rows only)
  tokens    input per Input (mono); cell allowed = bg --accent; cell blocked = border 1px --border on --surface; hover outline --border-input; focus ring per 0.3; range-drag preview --accent-soft; error text --danger with crit glyph; row summary meta-mono --text-muted ("Mon 8-18 . 10 h"); throttle tiers use fills plus a pattern: unlimited = empty, limited = diagonal hatch in --accent, blocked (0) = solid --border-input
  a11y      grid is role="grid" with row and column headers; cells are gridcells with aria-selected for allowed; arrow keys move, Space toggles, Shift+arrows extend and toggle a range, Home/End jump to the row ends; day header buttons toggle a whole day ("Toggle Monday"), hour header buttons toggle an hour across the week ("Toggle 08:00"); these header buttons are 32px high and are the pointer-friendly path (the 20px cells are a drag-paint surface and every cell is also reachable by the text field and the headers); pointer drag paints with pointer events; aria-live polite announces the summary ("62 hours per week allowed"); the error is linked with aria-describedby
  used on   S25 S12 A15 B12 B07 (the agent windows A06 and A15 are policy sections; there is no endpoint UI)
  notes     Accepted grammar belongs to internal/policy: examples are Mon-Fri/8-18;Sat,Sun/0-24 and */0-24. This component never parses; it posts the text to the server validator (proposed route POST /ui/policy/window/validate, hx-trigger="input changed delay:250ms") and renders the returned grid, normalised text and error position. Copy under the field: "Hours are device-local time. A window decides when a run may start; it never stops a run that is already going." Error copy: "Hour range 18-8 is not valid: the start must be before the end. For overnight use 18-24;0-8." Throttle rows add two number inputs (upload, download; unit select KiB/s, MiB/s, unlimited); overlapping rows are flagged ("Rows 1 and 2 overlap on Mon 8-10; row 1 applies" using the server's decision), never silently resolved.
```

### 8.2 Schedule editor

```
Schedule editor
  variants  interval (default: every N minutes or hours inside a window) and fixed times (list of HH:MM); event triggers (run on wake or start, startup delay); retry block; start conditions (laptop panel: battery floor, metered, Wi-Fi or VPN, idle, run anyway after N minutes)
  sizes     two-column form at 900px and wider (labels above controls), one column below; section gap 24px; Advanced disclosure collapsed by default (jitter, lock wait, max runtime, retry base and cap)
  states    default; invalid (field errors plus a summary at the section top); inherited vs overridden per group (Inherit / override control on each of Schedule, Window, Retries, Conditions); locked by policy; saving; saved (toast "Policy saved. 31 devices pick up version 13 at their next check-in."); impact preview (blast-radius dialog before save); laptop vs desktop defaults loaded (caption "Defaults for laptops"); unvalidated default (placeholder text "Default 1440 min (24 h)", caption "proposed default")
  tokens    form tokens per Form field, Input, Select, Checkbox/Radio/Toggle; summary sentence in a Card (inset) with mono text ("Every 4 h in Mon-Fri/7-19;Sat/8-14 . catch-up on"); unit conversions in help text ("300 s = 5 min")
  a11y      fieldsets with legends per group; the Interval/Fixed choice is a radio group; the Advanced disclosure is a <button aria-expanded>; time list items are removable chips with a labelled remove button; changes to the summary sentence are announced politely
  used on   S25 S12 B12 B07 (A06 and A15 are not built: the agent has no schedule UI)
  notes     Fields map to the policy sections schedule and conditions: mode, interval_minutes, window, fixed_times, jitter_minutes, catchup, run_on_wake, startup_delay_s, retries (max, base_s, cap_s), lock_wait_s, max_runtime_hours, skip_if_running; battery_floor_pct, metered, wifi_ssid_block, vpn_required, idle_only, run_anyway_after_minutes. Interval presets are chips (1 h, 4 h, 12 h, 24 h) that fill the number field. No "next run" preview in interval mode: interval scheduling is due-based with catch-up and no stored next run; fixed mode lists its times. Copy for run-anyway-after-N: "Conditions delay a run; after 240 min of delay it runs anyway." Seconds and minutes fields always display the converted human value.
```

### 8.3 Retention rule list and presets

```
Retention rule list and presets
  variants  preset picker (cards: 30 days, 1 year GFS, 7 years, Custom), rule rows (keep last N, hourly, daily, weekly, monthly, yearly; keep within a duration; keep by tag), summary sentence
  sizes     preset card 160px min-width, padding 10px 12px, radius md; rule row 34px (rule select 200px, number 96px, remove icon button); keep-within row first and fixed
  states    default; preset selected (selected recipe); custom; overridden (Inherit / override); locked by policy ("Retention enforced for all devices"); invalid (a policy without keep-within is rejected: error "Keep within is required so the newest snapshots are never pruned"); guard warning (server guard tripped: warn Inline banner with the server's message, Save asks for explicit confirmation when the server marks it as confirmable); preview loading; saved
  tokens    cards per Checkbox/Radio/Toggle option card; rows per Form field and Select; summary sentence --text-muted; remove icon button; required row has a lock glyph and the word "Required" (not removable)
  a11y      preset cards are a radio group; rule rows are a list; adding a row moves focus to its select; the keep-within row's remove control is aria-disabled with the reason "Required"; the summary sentence updates a polite live region
  used on   S13 S15 S25 B13 (A07 not built: the agent has no retention UI)
  notes     Rules combine with OR: a snapshot is kept if any rule keeps it (stated in a help line). The rule set maps to retention.rules {keep_last, keep_hourly, keep_daily, keep_weekly, keep_monthly, keep_yearly} plus the required keep_within and keep_tag[]; a named preset maps to retention.preset. Summary sentence is computed by the server ("Keeps every snapshot from the last 30 days, then one per day for 14 days, one per month for 12 months"). Changing a rule requests the calendar preview (8.4) with a 400ms debounce. The newest successful snapshot is never removed: the copy says so.
```

### 8.4 Retention calendar preview

```
Retention calendar preview
  variants  year view (12 month grids, default), month view; dry-run diff summary above; guard warning slot
  sizes     day cell 32x32px (date number meta-mono 11px), month grid 7 columns, 12 months in a responsive grid (4 columns wide, 2 at 900px, 1 below 640px); legend row 32px
  states    loading (skeleton calendar, "Calculating from 61 snapshots..." with the hx-indicator); kept (mint dot under the date); will be pruned (date greyed with strikethrough and a hollow ring, never colour alone); no snapshot (plain date); newest successful (accent ring and the word "Always kept" in the legend); today (underline); guard warning (warn banner above); error (crit banner "Preview unavailable: {reason}. Rules are unchanged." with Retry); empty (a device with no snapshots: "No snapshots yet. Nothing to preview." with the evidence line)
  tokens    cell bg --surface, border 1px --border, radius xs; kept dot --success; pruned date text --text-muted with line-through and ring --border-input; newest ring --accent 2px; month title sans 600; legend per Section label
  a11y      each month is a table with a caption ("October 2026"); cells are <td> with the status in a visually hidden word ("kept", "will be pruned"); legend explains all four marks; the diff summary is a polite live region: "Kept 61, pruned 14, oldest kept 2025-11-02"
  used on   B13 S13 S25
  notes     Data comes from the preview endpoint, which must agree with `forget --dry-run --json` fixtures (policy tests). Dates are in the customer's display_tz. The preview reflects unsaved rule edits and says so ("Preview of unsaved rules"). Milestone M9.
```

### 8.5 Selection tree picker

```
Selection tree picker
  variants  preset mode (v1 baseline: preset chips for all user profiles, Documents and Desktop, fixed drives, plus include and exclude rows), tree mode (lazy tree from a reference device listing, if the architecture adds a device-listing command), exclusion list with defaults diff, per-path flags (required, optional, require snapshot, follow symlinks)
  sizes     tree row 32px, indent 20px per level, checkbox 16px tri-state, size estimate column 96px (meta-mono right-aligned); tree max-height 360px scrolls; exclusion rows 34px
  states    loading tree (skeleton rows with indentation); large selection (shows "Showing the first 200 of 4,311 entries. Use the filter to narrow." never silent); policy-locked (read-only with the lock source); estimate unavailable ("estimate unavailable without a reference device"); checked, unchecked, indeterminate; excluded by baseline (row shows a "baseline" badge); baseline entry removed (warn badge "removed from baseline"); invalid include or exclude pattern (row error); listing failed (crit banner, preset mode remains usable)
  tokens    rows per Data table row recipe; checkbox per Checkbox/Radio/Toggle; flags menu per Menu; size estimate meta-mono --text-muted ("pre-dedup"); baseline badge per Attribute badge (neutral); estimate total in a KPI-style line, big number not used
  a11y      role="tree" with treeitem rows, aria-expanded, aria-checked (true, false, mixed), aria-level; Right expands, Left collapses, Up/Down move, Space toggles, * expands siblings, typeahead; the flags menu button is named "Flags for Documents"; exclusion list is a normal list of inputs
  used on   S25 S11 (A04 and A05 are not built in the agent)
  notes     Path rule exception: this is the only component that shows folder names from a client machine. Listings are transient: rendered once, Cache-Control no-store, never in URLs, logs or the database, and the nopaths analyzer applies. Whether tree mode exists in v1 is an architecture decision (the editor needs a device-listing command; only the restore tree has an admin-path listing today), so the baseline ships preset mode and tree mode is behind that decision. Policy fields: selection.roots or presets, includes, excludes, path_flags, exclude_cloud_files, exclude_larger_than, vss. Copy: estimates say "pre-dedup".
```

## 9. Status, feedback and content

### 9.1 Freshness stamp

```
Freshness stamp
  variants  topbar status pill (operator decision 2026-10-24, "design A"): dot + state word + time in the data face + "n of m agents" (full: LIVE | 13:42:10 | 212 of 212 agents), compact (agents segment hidden below 640px), page-level "as of" caption for panels and tables
  sizes     height 20px; meta-mono 11.5px/1/500; glyph 8px; gap 8px; right-aligned in the topbar
  states    fresh (ok dot, word LIVE, time 13:42:10); partial (warn dot, word PARTIAL, 207 of 212 agents, links to the C09 banner); stale (warn dot, word POLLING, "last 13:41:40"); unreachable (crit dot, word NO DATA, "since 13:41:40", rendered client-side when polls fail); loading (first paint: word WAITING with the neutral dot, never a green dot); no JS (noscript text "Data as of 13:42:10"). The state word carries aria-live so only state changes are announced, not every time update.
  tokens    text --text-muted, time --text on stale and unreachable; glyph colours --success, --warning, --danger; the concept's 10px glow around the ok dot (box-shadow of --success) is kept and turned off when reduced motion
  a11y      a <time datetime> element inside role="status" (polite) that announces only state changes (fresh to stale, stale to unreachable), never each tick; the tooltip shows the full date, the zone (America/Chicago), the computed-at versus loaded-at times and the coverage; colour is never alone (the shape and the words STALE or unreachable accompany it)
  used on   every console screen (topbar); mirrored as "as of" captions on B01 B03 B04 B05 B07 S21
  notes     Live: SSE freshness event, or any successful partial poll, rewrites it. It shows ComputedAt from FleetView in the operator zone. The stale rule (0.7) is mandatory here: if nothing arrives within 2x the refresh interval it flips to STALE and raises C10; it must never keep an old time with a green dot. "agents in view" is the number classified over the number enrolled; a shortfall is a partial-data signal. Copy uses 24-hour time with seconds.
```

### 9.2 Condition banner (unreachable and system conditions)

```
Condition banner
  variants  console conditions C01 to C16 plus C04b (table below; C04, C04b and the agent half of C05 per ADR 0013), entity banners (MISSING customer, customer-wide silence), each at level critical or warn
  sizes     full width of the content area; padding 10px 12px; radius md; gap 10px; leading status chip md; body ui 13px; actions right-aligned sm buttons; one line at 900px and wider, wraps below
  states    critical (crit class: --danger-soft background, 1px border --danger at 45%; in the page flow, scrolls with the page; the sticky topbar pill carries loss of data at any scroll position: ADR 0015); warn (warn class; not sticky; collapsible to a one-line summary for the session, never removable); appearing (instant); updating (text refreshes in place); resolved (removed by the server on the next banner.update; no "dismissed" state exists); stacked (all critical banners render fully; if more than 3 are active the fourth onward collapse into "and N more critical conditions", a button that expands them inline, with the count always visible); stale (client adds C10)
  tokens    bg --danger-soft | --warning-soft, border 1px (--danger | --warning) at 45% via color-mix, text --text, bold subject weight 600, leading chip per Device state chip (crit or warn), "since hh:mm" meta-mono 11.5px --text-muted
  a11y      the strip is <section id="banners" aria-label="System conditions"> with a polite live region; a newly inserted critical banner carries role="alert" once; each banner is a list item with the level in words ("Critical: ..."); actions are links or buttons with names; collapse control is a <button aria-expanded>
  used on   every console screen (above page content); B01 B08 (entity banners)
  notes     Dismissibility: never for critical; warn can be collapsed, not dismissed. A banner is removed only when the condition clears. Live: banner.update replaces the whole strip; fallback poll GET /ui/fleet/banners every 30s. Placement: directly under the topbar, above KPI tiles; the strip renders before page content so an error page still shows it. AllClear() is true only with no banners of any severity; the page shows no "all clear" claim otherwise. Order: critical before warn; within level C01, C04, C08, C09, C12, C14 first (silence and loss of control), then the rest by id. Copy states the condition, the consequence, and (where the architecture defines one) the action.
```

| id | level | chip label | banner text (copy) | action(s) |
| --- | --- | --- | --- | --- |
| C01 | critical | GATEWAY UNREACHABLE | "Gateway unreachable: no agent can back up. Last OK {hh:mm}." | Open System health |
| C02 | critical | STORAGE PROBE FAILED | "Storage unreachable from the admin path: retention, restore and usage are impaired. Last OK {hh:mm}." | Open System health |
| C03 | critical | EMAIL NOT DELIVERING | "Alert e-mail cannot be delivered: alarms may go unnoticed. Last send {hh:mm}." | Open email settings |
| C04 | critical | AGENT API UNREACHABLE | ADR 0013 (supersedes AGENT API SILENT and its fleet-silence copy). "Agent API unreachable: {stage}. No agent can check in or report. Failing for {N} min." where {stage} is one of: "{host} does not resolve (DNS)"; "nothing accepts connections on {host} (connect)"; "the certificate of {host} is not trusted (TLS)"; "the certificate does not match {host} (TLS)"; "the key of {host} does not match agent_probe.spki_pin, so pinned agents refuse it (TLS)"; "the TLS handshake with {host} fails (TLS)"; "the agent listener certificate has expired"; "{host} answers, but not as this console's agent API (wrong responder)" | Open System health |
| C04b | warn | AGENT API NOT CHECKED | ADR 0013. "Agent API not self-checked: {public_url is not set / agent_probe.ca_file cannot be read or holds no certificate / agent_probe.spki_pin is not the base64 of a SHA-256 digest}, so a broken agent listener, certificate or DNS name would go unnoticed." Also "Agent API self-check has not completed for {N} min, ..." and "Agent API checked without TLS: public_url is plain HTTP, so certificate problems go unnoticed. Use https for anything but a lab." | Open System health |
| C05 | warn (under 21 d), critical (under 7 d) | CERTIFICATE EXPIRING | "{Gateway or Agent listener} certificate expires in {N} days." Agent listener, critical: adds "Agents will stop checking in when it does." (ADR 0013) | Open System health |
| C06 | critical | ALERTING STALLED | "Alerting is stalled: this view and notifications may be stale. Last evaluation {hh:mm}." | Open System health |
| C07 | critical | MAINTENANCE WORKER DOWN | "Maintenance worker is not running: retention, restore browsing and usage sampling are off." | Open System health |
| C08 | critical | NO ACTIVE DEVICES | "No active devices enrolled. Nothing is being backed up." Also renders the empty-fleet alarm panel in place of the grid. (Before the first enrollment ever, the neutral "Enroll your first customer" panel shows instead.) | Enroll devices |
| C09 | critical | PARTIAL DATA | "Partial data: {N} devices could not be classified. They show as UNKNOWN and count as unhealthy." Empty-poll variant: "The last load returned 0 devices; the previous load had {N}." | Show UNKNOWN devices |
| C10 | warn | LIVE UPDATES DISCONNECTED | "Live updates disconnected: data as of {hh:mm}." (rendered client-side too) | Reconnect |
| C11 | warn or critical | DATABASE NEEDS ATTENTION | "Console database needs attention: {integrity check failed, disk below 10%, WAL over 1 GiB, or self-backup older than 26 h}." | Open System health |
| C12 | critical | MASTER KEY UNAVAILABLE | "Master key unavailable: enrollment, escrow reveal and server-side retention are disabled." | Open System health |
| C13 | warn | CLOCK SUSPECT | "Server clock may be wrong: device offsets agree." | Open System health |
| C14 | critical | DIGEST NOT SENT | "Daily digest was not delivered. It was due by {hh:mm}." | Open email settings |
| C15 | warn (80%), critical (90%, 100%, or projected full in under 14 d) | STORAGE QUOTA | "Storage at {N}% of quota." plus "Projected full in about {d} days." when the growth alarm applies | Open Storage |
| C16 | warn | RELEASE MANIFEST REJECTED | "Release manifest rejected." | Open Releases |

Entity banners (not C-ids): **MISSING CUSTOMER** (critical): "{Customer} is marked expected but has zero active devices: every machine there is unmonitored." actions Acknowledge, Open customer; two or more collapse to "{N} expected customers have no devices" with the names listed. **CUSTOMER-WIDE SILENCE** (critical): "{N} of {M} devices at {Customer} have gone silent at once." action Open customer.

### 9.3 Inline banner

```
Inline banner
  variants  info, success, warning, error; with or without title; with an action; dense (panel, 8px 10px) or standard (10px 12px)
  sizes     full width of its container; radius md; glyph 8px at the lead; text ui 12.5px; max text width 72ch
  states    default; dismissible (info and success only, per session); persistent (warning and error stay until the cause clears); loading action; stale
  tokens    info: --accent-soft with --accent glyph (info square); success: --success-soft with ok dot; warning: --warning-soft with triangle; error: --danger-soft with diamond; border 1px class colour at 45%; text --text; titles weight 600
  a11y      role="status" for info and success, role="alert" for error and for warnings that block saving; the level word is in the text or a visually hidden prefix ("Error:"); actions are real buttons or links; focus moves to an error banner at the top of a dialog on failed submit
  used on   B07 B10 B13 B16 S25 S26 S27 S22 (all forms and panels)
  notes     Copy names what happened and what happens next: "Policy not saved: the keep-within rule is required. Your edits are kept." Restart-required (settings): "Saved. Restart the console to apply the listening port." with a Restart action; token shown once: "Copy this token now. It is not shown again." Consequence warnings in the key custody step use the warning level. Never use an inline banner for a state that belongs in a chip (a device being FAILED is a chip plus the cause banner in the panel).
```

### 9.4 Toast

```
Toast
  variants  info, success, warning, error; with an action ("Undo", "Open")
  sizes     width 360px (full width minus 16px gutters below 640px), padding 10px 12px, radius md, stack gap 8px, max 3 visible (the rest wait in a queue and a count shows "+2 more"), bottom-right
  states    entering (slide 12px, --motion-base); visible; paused (pointer over or focus within); leaving; auto-dismiss (info and success 5 s, warning 8 s); error never auto-dismisses; reduced motion (no slide)
  tokens    bg --surface-raised, border 1px --border, shadow --shadow-pop, glyph and left rule by class (3px), text --text ui 12.5px, close icon button sm
  a11y      region role="region" aria-label "Notifications"; info and success use role="status", warning and error role="alert"; the close button is labelled; a toast is never the only record: save confirmations also change the page, failures also raise an inline banner or audit row; focus is not stolen
  used on   all console screens
  notes     Triggered by an htmx response header (HX-Trigger: {"toast": {"level":"success","text":"Policy saved. 31 devices pick up version 13 at their next check-in."}}). Delivery failures (email, webhook) use error and name the destination and the consequence ("Digest not delivered to ops@example-msp.com. It will retry at 06:45.").
```

### 9.5 Empty, loading and error state pattern

```
Empty / loading / error state
  variants  empty-checked (a list evaluated and clear), empty-filtered, empty-first-run (neutral, only before the first enrollment ever), no-data alarm (data must exist but does not), loading (skeleton), error (could not load), partial (some data loaded), stale (last good data with an as-of stamp)
  sizes     panel padding 32px 24px, content centred max 440px, mark glyph 40px, title ui 14px/600, body ui 12.5px, action buttons md; inline form inside tables (rows) uses 16px padding
  states    each variant above; loading never exceeds 10 s before turning into error ("This is taking longer than usual." with Retry); error offers Retry and shows the error class and time, keeps previous data dimmed with its as-of stamp when it exists
  tokens    mark = the original brand mark outline at 40px in --text-muted (alarm variants: --danger with the diamond glyph beside the title); title --text; body --text-muted; panel bg --surface for neutral, --danger-soft with border --danger for the alarm, --warning-soft for partial; skeleton per recipe
  a11y      loading regions are aria-busy with a polite "Loading devices" status; error and alarm variants are role="alert"; empty states are headings plus text, not images; actions are focused first on the error variant
  used on   all list screens (S03 S19 S21 S16 B01 B04 B05 B06 B09 S33) and panels (B07 B08)
  notes     Classification rule (the product rule): "no data" is an alarm unless it is provably a clear result. (1) checked-and-clear lists are neutral and must print their evidence ("Nothing overdue. 212 devices checked at 13:42:10."); (2) filter-caused empties are neutral and show the filter and the denominator; (3) first-run is the only neutral no-data case; (4) everything else is the alarm variant: an empty fleet, a customer expected to have devices, a device that has been enrolled longer than its grace with no runs, a failed or empty poll. Alarm copy: title says what is missing, body says why that matters and what the operator can do ("No devices are enrolled. Nothing is being backed up. Enroll a customer or check the agent listener."). Never show a spinner without a timeout, a zero without its denominator, or a green empty ring.
```

### 9.6 Code block

```
Code block
  variants  script (multi-line, with copy), single value (token, id), secret (copy-once, masked afterwards), diff or log excerpt (read-only)
  sizes     padding 12px 14px; radius md; mono 12px/1.55; copy button sm at the top right; max 40 visible lines then "Show all 120 lines" (the count is stated); horizontal scroll, no wrapping
  states    default; highlighted (comment, variable and string tokens); copied (button label "Copied" for 2 s, polite status); secret masked (value shown as the first 12 characters plus bullets); secret shown (once, with the Shown once label); expired (replaced by "Cleared. Create a new token to see a value."); copy failed ("Copy failed. Select the text and press Ctrl+C.")
  tokens    bg --bg, border 1px --border, text --text, comments --text-muted, keywords and variables --accent, strings --success (syntax colours are decoration: the text stays legible with none), copy button per Icon button secondary
  a11y      <pre><code> with tabindex 0 and role="region" aria-label "Deployment script, 14 lines" so it scrolls by keyboard; the copy control is a real button "Copy script"; copying copies the plain text, never the highlighted markup; secrets are not in the page after expiry
  used on   B16 B10 S38 S34 S30
  notes     The block renders what the generator returns; the enrollment script content rules (discovery first, idempotent, plain variables at the top for the RMM header, exit codes) belong to the B16 generator, not to this component. The token inside a script preview is truncated ("summit-2026Q4-...") and the full token is fetched at copy time (hx-get) so it is not in the page source. Orbitron is never used here.
```

### 9.7 Copy / paste sticky action

```
Copy / paste sticky action
  variants  holding bar (bottom of the viewport): "Copied {item} from {source}" with Paste here and Clear
  sizes     bar 44px, max-width 640px centred, radius lg, buttons sm
  states    idle (not shown); holding; pasting (loading); pasted (toast and bar clears); error ("Could not paste: {reason}. The copied item is still held."); target not eligible (Paste aria-disabled with reason)
  tokens    bg --surface-raised, border 1px --accent, shadow --shadow-pop, text --text, source in meta-mono
  a11y      role="region" aria-label "Clipboard"; polite announcement "Holding Documents rule from FRONTDESK-01"; Clear and Paste are real buttons; Esc clears when focus is inside
  used on   S10 (not built in v1: no per-device protected items). Reserved pattern for copying a device override to another device if it is added.
  notes     Specified so the inventory is complete; it ships only if a copy feature is added. The held item lives in the viewer's session (server-side), never in localStorage, because it may reference client settings.
```

### 9.8 Credential enrolment (TOTP, WebAuthn, API tokens)

```
Credential enrolment
  variants  TOTP (QR code plus manual key plus confirm code, then recovery codes), WebAuthn (Add security key, name, saved), API tokens (list, create, show-once secret, revoke)
  sizes     inside a Card or Modal md; QR 176x176px on a --surface background with 12px quiet zone; token list rows 44px
  states    off; setting up (TOTP: QR shown); confirming (code input); on; saved (WebAuthn: "Security key 'Desk key' saved"); recovery codes shown once; error (wrong code: "Code incorrect or expired. Try again."); token created (secret shown once in the Code block secret variant); revoked; scope restricted; stale
  tokens    switch per Checkbox/Radio/Toggle with the On/Off word; QR is an inline SVG generated server-side; codes per Code block; list per Data table (compact); destructive revoke per Modal
  a11y      the QR has a text alternative (the manual key in a Code block, grouped in 4 characters); WebAuthn button states are announced ("Waiting for your security key"); recovery codes can be downloaded as text and copied; token secret is announced as "Shown once" with the Copy button
  used on   S38 (S39 deferred to M14)
  notes     The QR image and the manual key are served no-store and never logged. API token scopes are listed in words (metrics:read, read, operate). A token is shown once; the list shows name, scope, created, last used and expiry, never the value.
```

### 9.9 Branding preview surface

```
Branding preview surface
  variants  default CryoBackup brand, per-customer branded (reserved, milestone M14); surfaces previewed: login, report email header, download page
  sizes     preview frame 100% width with a 16:10 aspect ratio, radius lg; control column 320px
  states    default; branded; invalid (a colour pair fails AA contrast: the control shows the ratio and refuses to apply); loading; gated by permission (read-only with reason)
  tokens    frame per Card (raised); accent and on-accent shown as the chosen pair with a contrast readout in meta-mono; logo area 160x48px; sender name field per Input
  a11y      the preview is a labelled region ("Preview of the login page"), not interactive; colour pickers have text inputs and a contrast status that is words ("Contrast 5.0:1, passes AA") not colour
  used on   S28 S01 S30 (deferred to M14; email headers in 10.1)
  notes     The default wordmark stays Orbitron; a customer logo replaces it as an image and may not introduce another display face. Custom CSS or JS injection is not offered.
```

## 10. Email templates

Constraints common to all four templates (the shell, 10.1, enforces them):

- Inline CSS on every element (a `<style>` block is allowed only for the dark-mode and narrow-width media queries). Layout is nested `<table role="presentation">`; no flexbox, grid, `clip-path`, transforms, CSS variables, web fonts or `@font-face`.
- Colours are resolved at render time by a template function (`{{tok "danger"}}`) from the **light** token set as the default ground; a `prefers-color-scheme: dark` block swaps in the dark set for clients that honour it; `<meta name="color-scheme" content="light dark">`. No colour is hand-typed in a template.
- State glyphs are Unicode text, not CSS shapes: ok = a filled circle, info = a filled square, warn = a filled up-pointing triangle, crit = a filled diamond, neutral = an open circle, each in its class colour and always followed by the word. They render in every client and keep colour from being the only carrier.
- Fonts: body `--font-sans` stack (IBM Plex Sans, then Segoe UI, then Arial) at 14px minimum; timestamps and ids in the `--font-mono` stack. Orbitron appears only as the wordmark image and as the big-number role in the digest summary counts; the number role names Orbitron first in its stack and must read correctly in the fallback face (fixed-width cells, no dependence on Orbitron metrics). Orbitron is never used in email body text.
- Images: the mark and the wordmark are inline attachments referenced by `cid:`, never remote URLs (no tracking, no phone-home). Every image has alt text ("CryoBackup"). The message is complete with images blocked.
- Every message is multipart/alternative with a first-class plain-text part (same section order, 72-column lines, glyph words in brackets such as `[FAILED x3]`, the console URL in full).
- No file paths, file names or folder names. Hostnames and customer names are allowed in operator mail. Customer-facing mail carries only that customer's devices.
- Links open the console (login required). No link changes state: Acknowledge and Park links open the dialog in the console; nothing mutates from a click in email.
- Times in the operator's zone with the zone named; "polled hh:mm:ss" stamps the data time in every message.

### 10.1 Email shell

```
Email shell
  variants  operator mail (digest, immediate failure, storage report), customer mail (per-customer report); light default with dark-scheme override
  sizes     container 640px wide centred (100% below 480px), padding 20px; header row 56px; summary cells 4 across (2 across below 480px); line cards padding 8px 10px with a 3px left rule; footer padding 12px 20px; body 14px, small text 12px
  states    preview (rendered in a sandboxed iframe in the console with a "Preview" ribbon and no outbound requests); sent (logged in notification_log); delivery failed (console shows the failure on the notification row and raises C03/C14; the message is retried by the job, not by the template); data-degraded (top banner: "Data may be incomplete: 207 of 212 agents classified" warn, or "The console could not refresh since 06:10; this report uses data from 06:10" crit); no-data (fleet empty: crit banner "No active devices enrolled")
  tokens    page ground --bg; message card --surface with border 1px --border and radius --radius-lg (clients that ignore radius show square corners); section label ui 11px/600 uppercase tracking .1em in --text-muted (sans, not mono, because mono is not guaranteed); line rule colours by class; links --accent; wordmark image 16px tall (email size of the wordmark role); mark image 26px
  a11y      lang attribute set; one h1-equivalent (the subject-like line in the header); real text for every status (glyph plus word); table role="presentation"; link text is descriptive; contrast uses the same pairs as the console tokens; minimum 14px body
  used on   S18 S27 B09 (templates previewed and test-sent from S27; delivery history shown in B09)
  notes     Header row: mark image, wordmark image, then right-aligned "polled 06:29:40 . 212/212 agents" in 12px mono stack. Footer: "Open the console: {URL}" and the reason this message was sent (the rule name). Copy per section 0.6.
```

### 10.2 Daily digest

```
Email template: daily digest
  variants  with exceptions; all green (still sent: "a missing digest is itself an alarm"); data-degraded; MISSING customers present
  sizes     container 640px; summary row 4 counts (critical, warnings, customers missing, healthy); sections stacked
  states    preview, sent, delivery failed (see shell)
  tokens    subject "Backup digest . Tue 20 Oct . 7 critical, 5 warnings, 1 customer missing" (counts in the subject; "all clear" never appears unless AllClear() is true); summary counts use the big-number role (700, 23px, tabular) in class colours with the word beneath (label ui 11px); section labels per shell; line card: bold hostname (sans 600) plus customer, a detail line (12px, --text-muted), and the state word right-aligned in the class colour with its glyph; ok-summary paragraph at the end with a "healthy" count; footer per shell
  a11y      as shell; section order is the reading order; every count has its label adjacent
  used on   S18 S27 B09 (generated by the daily-digest job at digest_local_time in the display zone)
  notes     Section order, worst first: Silence (MISSING customers, UNKNOWN devices, UNSEEN, BLANK), Failures and staleness (STUCK, FAILED, PARTIAL, STALE), Parked devices (age and reason; indefinite ones flagged), Warnings (MISSED, DEFERRED over 24 h, OUTDATED, ESCROW, MAINT), Retention (ran, deferred, failed counts), then "All green:" healthy customers one line each. Silence is listed before failures because it is worse. Acknowledged items are listed and labelled "ACK by {user}", never omitted. Plain-text part mirrors this order. Counts and lines are the same as the board's (one classifier). If a section is empty it says so with evidence ("Silence: none. 212 devices checked at 06:29:40.").
```

### 10.3 Immediate failure email

```
Email template: immediate failure
  variants  failure (crit), alarm-level (warn), recovery notice (ok), customer-wide silence (one message for N devices)
  sizes     container 640px; one hero block, one facts table, one actions row
  states    preview, sent, delivery failed; recovery (sent once per episode when the rule asks)
  tokens    subject "FAILED x3 . FRONTDESK-01 . Summit Eye Care" (state word first, then host, then customer); hero: state chip line (glyph, word, qualifier) then the hostname at 18px sans 600 and the customer name; "What happened" paragraph (body 14px); facts table rows with 12px keys in --text-muted: Last success, First failure, Consecutive failures, Error class, Agent version, Policy; "What happens next" paragraph; actions row with two bulletproof link buttons (primary "Open device", secondary "Open fleet board") using --accent and --on-accent
  a11y      as shell; buttons are links with 44px tall hit areas (padding) and text labels
  used on   S18 S27 B09
  notes     Copy example: What happened: "Three consecutive backups failed since Fri 02:10. VSS snapshot failed on volume C:; restic fell back to live reads and the locked files failed." What happens next: "CryoBackup keeps retrying on schedule. You will get one recovery message when a backup succeeds. This is the only failure message for this episode." Deduplication is by episode (the same key as notification_log), so a flapping device does not repeat. No path or file name appears; volumes and classes only.
```

### 10.4 Per-customer report email (HTML and PDF)

```
Email template: per-customer report
  variants  weekly or monthly period; HTML body plus PDF attachment of the same content; with exceptions; clean period
  sizes     HTML container 640px; PDF is A4 or Letter by configuration, 16mm margins, same section order, page footer with page numbers
  states    preview, sent, delivery failed, generated (PDF attached); data-degraded (banner as in shell); no devices (a customer with zero devices states it plainly and the operator is alarmed separately, never "all good")
  tokens    header with the MSP's mark and the customer name; summary row of 4 counts in sans 600 22px (not Orbitron: this is a customer-facing document and the big-number role is reserved for the operator console and digest); device table (device, last backup, status word with glyph, last 10 days as ten glyph characters); exceptions list in plain words; footer with the MSP contact
  a11y      as shell; the PDF is tagged with a title, language and reading order; the ten-day glyph row has a text equivalent ("7 good days, 3 days with problems") beside it
  used on   S18 S27 B09 B08
  notes     Audience is the customer's contact, so copy maps engine terms to plain words ("Backed up today", "Needs attention", "Not seen for 9 days") and never shows error classes or other customers. Content is exactly one customer's devices, enforced in the query, not the template. The PDF is rendered from the same template through a print stylesheet; whether that needs a PDF library is a dependency-budget decision (flagged in the report).
```

### 10.5 Storage report email

```
Email template: storage report
  variants  fleet (operator) and per-customer; with growth alarm
  sizes     container 640px; usage bars are two-cell tables with widths in percent (no images), 10px tall
  states    preview, sent, delivery failed; quota unset (rows say "No quota set"); data-degraded
  tokens    summary counts (used, quota, growth per day, next prune) in the shell summary cells set in sans 600 22px (the Orbitron big-number role is limited to the digest in email); rows per customer: name, used, quota bar, level word with glyph (UNDER, APPROACHING, NEARLY FULL, EXCEEDED per the Quota meter), growth, projection
  a11y      as shell; each bar row has the percentage in text; levels are words as well as colour
  used on   S27 S18 B09 B11
  notes     Thresholds are the C15 values. Copy states the soft quota: "Soft quota: backups continue." The projection line appears only when the growth alarm applies ("Projected full in about 9 days").
```

## 11. Agent tray

The tray (`cryo-tray.exe`) is the only UI on the endpoint. It reads status over the named pipe `\\.\pipe\CryoBackup.Tray` (status, check in, back up now). Typeface: IBM Plex Sans if the tray renderer can bundle it, else the Windows system UI face; Orbitron appears only as the wordmark rendered from an image asset (never live text). No tray text uses Orbitron, and the tray has no settings: policy lives in the console.

### 11.1 Tray popover

```
Tray popover
  variants  status (default), running, deferred, attention, parked, console unreachable, service stopped, not enrolled (states in 11.2)
  sizes     width 320px; header 44px (padding 12px 14px); body padding 12px 14px; menu rows 36px; radius md; offset from the taskbar 12px
  states    ok ("PROTECTED"); running ("RUNNING 62%" with a progress bar and "about 6 min left"); deferred ("DEFERRED" with the reason and the latest start: "Waiting for power. Will back up when plugged in, or at 18:00 at the latest."); attention ("ATTENTION": "The last backup failed at 02:10. It will try again at 06:10."); parked ("PARKED": "Backups are paused until Fri 18:00."); console unreachable ("Cannot reach the console since 09:12. Backups continue on the last policy."); service stopped ("The backup service is not running." with the crit glyph); not enrolled ("This computer is not set up for backup."); stale (pipe unreadable: "Status unavailable since 13:41" replaces all values, never the last green); menu items disabled with a reason line
  tokens    bg --surface, border 1px --border, radius --radius-md, shadow --shadow-pop; header: mark 20px, wordmark "CRYO" 12px (.t-wordmark as an image), state chip md (Device state chip classes: ok, info, warn, crit, neutral) at the right; body: Key-value list (compact 110px): Last backup, Next, Managed by, Agent (version in mono, "service running"); menu: rows with 14px icons, text --text, hover --accent-soft; disabled --text-muted
  a11y      the popover is a dialog (role="dialog", aria-label "CryoBackup status") reached from the tray icon by Enter or Space and closed by Esc; focus moves to the first menu item; the state is in text as well as colour; every menu row is a real button; the whole popover is operable by keyboard alone; high-contrast mode uses system colours
  used on   A14 B14 A08 A09 (tray status and Back up now)
  notes     Menu: "Back up now" (shown only when agent_permissions allow it, otherwise omitted rather than disabled), "Check in with console", "Restore my files..." (only when allow_local_restore is set and the local restore UI exists, which is milestone M14; omitted before), nothing else. Live: the tray polls the pipe every 30 s while idle and every 2 s while the popover is open or a run is active. If the pipe cannot be read for two polls the popover shows the stale state above and the icon shows "service stopped" or "status unavailable". Copy is plain and specific: "Backed up today at 02:10", never "Your data is safe!". Errors state what happened and what happens next. The tray also raises one Windows notification per day when there has been no successful backup for longer than the nag threshold (the "no-recent-backup nag", r217; the threshold value is not yet specified in the architecture): "No backup has finished for 3 days. Your computer will try again when it is plugged in and online."
```

### 11.2 Tray icon states

```
Tray icon states
  variants  protected, running, deferred, attention, parked, console unreachable, service stopped, not enrolled (8)
  sizes     .ico with 16, 20, 24 and 32px frames; the mark fills the frame; a status badge 7px at the lower right of the 16px frame (9px at 24px and up)
  states    protected = mark in the ok colour with a mint dot badge; running = ice colour, ice square badge, animated progress arc only if the user has not requested reduced motion; deferred = amber with an amber triangle badge; attention = coral with a coral diamond badge; parked = muted with a hollow ring badge; console unreachable = amber with a triangle badge and a small slash; service stopped = coral with a diamond badge and a slash; not enrolled = muted with a ring badge
  tokens    mark tinted with --success, --accent, --warning, --danger, --text-muted (resolved to the Windows light or dark taskbar variant at build time); badge shapes per 0.1; tooltip text is the popover headline: "CryoBackup: protected. Last backup today 02:10."
  a11y      the icon is exposed with an accessible name and tooltip (state word plus last-backup fact), so colour is never the only carrier; the badge shape differs per class; in Windows high-contrast mode the icon switches to the system foreground colour with the badge shapes unchanged
  used on   A14 B14
  notes     The concept shows five icon states differing by colour only; the badges are added by this spec. Icon state follows the Primary device state mapped by class (OK protected; PENDING_FIRST and RUNNING running; DEFERRED deferred; critical and alarm states attention; PAUSED parked). The icon must not stay green when status is stale: two failed pipe reads change it to "service stopped" or "status unavailable".
```

## 12. Build order for primitives

Build each item as a template partial plus its CSS, add it to the `/design` page in every state before starting the next, and only then build screens. Items within a step may be built in parallel; steps are sequential because later steps compose earlier ones.

| step | items (block numbers) | gate before moving on |
| --- | --- | --- |
| 1 Foundations | `tokens.css` import with the four `-soft` tokens added; base reset; `.t-*` display classes; dense type recipes (0.5); focus, hover, active, disabled, loading, error, selected and skeleton recipes (0.4); reduced-motion rules; icon sprite (original set) and the five state glyphs (0.1) | `/design` renders the token page in both themes; contrast report has zero AA failures |
| 2 Surfaces | Card 1.2, Key-value list 1.3, Section label 1.4, Display type roles 1.1, Tooltip 2.9 | surfaces correct in both themes; Orbitron appears only in the five roles |
| 3 Controls | Button 2.1, Icon button 2.2, Link 2.3, Form field 2.4, Input 2.5, Select 2.6, Checkbox/Radio/Toggle 2.7, Segmented control 2.8, Theme switch 1.9, Menu 2.10 | keyboard-only pass over every control; hit targets 32px or padded |
| 4 State language | Device state chip 3.1 (all 17 states plus RUNNING), Run outcome chip 3.2, Attribute badge 3.3, Filter chip 3.4, Customer rollup chips 3.6 | golden-file test: one fixture per state from the `internal/state` test list renders the catalogue label, class and glyph |
| 5 Live and degraded | Freshness stamp 9.1 with `fresh.js` and the SSE/poll contract 0.7; Inline banner 9.3; Condition banner 9.2 (C01 to C16 fixtures); Toast 9.4; Empty/loading/error 9.5 | kill the SSE stream in the fixtures server: stamp flips to STALE, C10 appears, live components show "as of", pulses stop |
| 6 Data | Data table 5.1 and Customer group header 3.5, Pagination 5.2, Saved views 5.3, Query builder (flat) 5.4 | empty-fleet and partial-data fixtures render the alarm variants, never a neutral empty |
| 7 Visuals | KPI tile 4.6, Ten-day strip 4.1, Age ring 4.4, Health gauge 4.5, Quota meter 4.7, Day-block grid 4.2 | ring with zero devices renders the alarm ring; every glyph has a distinct shape |
| 8 Run views | Live run progress 4.8, Run history list 5.6, Log viewer 5.5, Code block 9.6 | stream-lost state verified; log viewer shows no paths in fixtures |
| 9 Layout shell | App shell 1.5, Sidebar 1.6, Topbar 1.7, Global search 1.8 | breakpoints 640, 900, 1200 verified; skip link works |
| 10 Containers and actions | Modal 6.2, Tabbed side panel 6.1, Wizard 6.3, Acknowledge/snooze 7.2, Device action menu 7.1 | focus return and Esc behaviour verified for each |
| 11 Policy editors | Inherit/override 7.4, Window/throttle field 8.1, Schedule editor 8.2, Retention list 8.3, Retention calendar preview 8.4, Selection picker 8.5 | validator round-trip with `internal/policy` fixtures; DST window cases |
| 12 Sensitive flows | Key reveal dialog 7.3, Credential enrolment 9.8, Calendar-matrix cell 4.3 (M11) | reveal key never present in the DOM after expiry |
| 13 Outside the console | Email shell 10.1, digest 10.2, immediate failure 10.3, per-customer report 10.4, storage report 10.5; Tray popover 11.1 and Tray icon states 11.2 | email renders with images blocked and in plain text; tray status never stays green on a stale pipe |
| 14 Reserved | Copy/paste sticky action 9.7, Branding preview 9.9 | built only when their screens are scheduled (S10 not built; S28 is M14) |

### 12.1 The `/design` check page (fixtures mode)

- Route `GET /design` (index) and `GET /design/{component}`; registered only when the console runs in fixtures mode (canned API responses, no live data), absent (404) otherwise, and served only on the private listener.
- For every component: a header with the block name and its `used on` ids; a **state matrix** rendering every variant (rows) in every state (columns) from canned data (state fixtures come from the `internal/state` table-driven cases, so the chips on the page are the classifier's real output); the markup snippet; and the a11y notes from the block.
- Page-level toggles: theme (System, Dark, Light), `prefers-reduced-motion` simulation, "force stale" (drives the stale rule), "force SSE drop", viewport width presets (320, 640, 900, 1200), and a "show hit targets" overlay that outlines everything below 32px.
- Built-in checks shown at the top: token coverage (CI greps component CSS for literal colours; zero allowed), Orbitron audit (CI fails if `--font-display` is referenced outside the five `.t-*` classes), contrast report (`contrast.py` for both token files), and a screenshot per theme saved as the replica-design Step 4 design-system check.
- Includes the full C01 to C16 banner set, the 17-state chip catalogue, the 8-outcome table, the empty-fleet alarm and partial-data fixtures, and sample emails (HTML and plain text) and tray popovers rendered as images from the same partials.

## 13. Coverage table (recon "## Components" row to block)

| # | recon component row | block(s) |
| --- | --- | --- |
| 1 | Data table | 5.1 Data table; 5.2 Pagination; 5.3 Saved views control; 2.10 Menu (column picker, export) |
| 2 | Device state badge | 3.1 Device state chip; 2.9 Tooltip (reason) |
| 3 | Run outcome badge | 3.2 Run outcome chip |
| 4 | Attribute badges | 3.3 Attribute badge |
| 5 | Day-block activity timeline | 4.2 Day-block activity grid |
| 6 | Ten-day strip | 4.1 Ten-day strip |
| 7 | Age ring (doughnut) | 4.4 Age ring; 3.4 Filter chip |
| 8 | Calendar-matrix cell | 4.3 Calendar-matrix cell |
| 9 | Health gauge | 4.5 Health gauge |
| 10 | Freshness stamp and unreachable banner | 9.1 Freshness stamp; 9.2 Condition banner |
| 11 | MISSING customer group header | 3.5 Customer group header; 3.6 Customer rollup chips; 9.2 (entity banners) |
| 12 | Live run progress | 4.8 Live run progress |
| 13 | Log viewer | 5.5 Log viewer; 5.6 Run history list |
| 14 | Query / clause builder | 5.4 Query / clause builder |
| 15 | Schedule editor | 8.2 Schedule editor |
| 16 | Window / throttle mini-language field | 8.1 Window / throttle mini-language field |
| 17 | Retention rule list and presets | 8.3 Retention rule list and presets; 8.4 Retention calendar preview |
| 18 | Selection editor | 8.5 Selection tree picker |
| 19 | Tabbed modal / side panel | 6.1 Tabbed side panel; 6.2 Modal |
| 20 | Wizard (stepper) | 6.3 Wizard (stepper) |
| 21 | Device action list | 7.1 Device action menu |
| 22 | Inherit / override control | 7.4 Inherit / override control |
| 23 | Key reveal dialog | 7.3 Key reveal dialog |
| 24 | Quota meter | 4.7 Quota meter |
| 25 | Acknowledge / snooze control | 7.2 Acknowledge / snooze control |
| 26 | Actions dropdown and row menu | 2.10 Menu |
| 27 | Copy / Paste sticky action | 9.7 Copy / paste sticky action (reserved; S10 not built) |
| 28 | Toggle + QR / token enrolment | 9.8 Credential enrolment; 2.7 Checkbox / Radio / Toggle |
| 29 | Branding preview surface | 9.9 Branding preview surface (reserved, M14) |
| 30 | Email / report templates | 10.1 Email shell; 10.2 Daily digest; 10.3 Immediate failure; 10.4 Per-customer report; 10.5 Storage report |
| 31 | Toast and inline banner | 9.4 Toast; 9.3 Inline banner |
| 32 | Empty, loading and error states | 9.5 Empty / loading / error state; skeleton recipe 0.4; empty kinds in 5.1 |
| 33 | Tray icon menu and status icon | 11.1 Tray popover; 11.2 Tray icon states |

Blocks added beyond the 33 recon rows (primitives and layout the rows assume): 1.1 Display type roles, 1.2 Card, 1.3 Key-value list, 1.4 Section label, 1.5 App shell, 1.6 Sidebar navigation, 1.7 Topbar, 1.8 Global search, 1.9 Theme switch, 2.1 Button, 2.2 Icon button, 2.3 Link, 2.4 Form field, 2.5 Input, 2.6 Select, 2.7 Checkbox / Radio / Toggle, 2.8 Segmented control, 2.9 Tooltip, 3.4 Filter chip, 3.6 Customer rollup chips, 4.6 KPI tile, 5.6 Run history list.

