# web/ - console templates and vendored assets

Everything the console serves to a browser lives here and is compiled into the
`cryo-console` binary by `embed.go`. The console makes no third-party request:
no CDN, no web-font service, no analytics. Every asset below is a file in this
tree.

## Layout

| Path | What it is |
|---|---|
| `templates/base.html` | Page shell: head, nav, freshness chip, banner region, toast region. |
| `templates/partials/` | `icons.html` (inline SVG sprite), `shared.html` (chips, run progress, banners), `visuals.html` (ten-day strip, spark bars). |
| `templates/pages/` | `login`, `board`, `device`, `restore`, `restore_status`, `error`, `design`. |
| `static/tokens.css` | Design tokens (colour, type, spacing, radius). The single source of truth; `app.css` only references them. |
| `static/design/tokens*.json` | The same tokens as data, used by the contrast check in `internal/console/ui`. |
| `static/app.css` | Component styles. No colour literal outside `tokens.css`. |
| `static/fonts.css`, `static/fonts/` | `@font-face` rules and the font files. |
| `static/js/theme.js` | Theme toggle (light, dark, system). First-party. |
| `static/js/fresh.js` | First-party. SSE client, the stale-data rule (C10), drawer, group collapse, toasts. |
| `static/js/design.js` | First-party. Behaviour of the `/design` review page only. |
| `static/js/htmx.min.js` | Vendored htmx. |

## Provenance and licences

| Asset | Version | Licence | Licence file |
|---|---|---|---|
| htmx | 2.0.10 | Zero-Clause BSD | `static/js/htmx.LICENSE` |
| IBM Plex Sans (variable, woff2) | as shipped by IBM | SIL OFL 1.1 | `static/fonts/ibmplexsans/OFL.txt` |
| Martian Mono (variable wght 100-800, woff2) | Evil Martians release v1.1.0, see `static/fonts/martianmono/SOURCE.txt` | SIL OFL 1.1 | `static/fonts/martianmono/OFL.txt` |
| Orbitron (variable, woff2) | The League of Moveable Type | SIL OFL 1.1 | `static/fonts/orbitron/OFL.txt` |

Updating htmx: replace `htmx.min.js` with the upstream minified build, update
the version in this table and the licence file if it changed, then run
`go test ./internal/console/ui/ ./internal/console/server/` (the ui tests fail
if a rendered page contains an `http://` or `https://` URL).

Orbitron is used for the wordmark and the state-chip labels only. The
`OrbitronAudit` check in `internal/console/ui` fails the build if it appears
anywhere else.

## Security notes for the scripts

- htmx is configured by a `<meta name="htmx-config">` tag in `base.html`, not by
  inline script, because the Content-Security-Policy is `script-src 'self'`.
- `fresh.js` assigns `innerHTML` only from two sources: HTML the console
  rendered itself (SSE fragments, htmx responses) and allow-listed constant
  strings. It never assigns text that came from a device, a customer or a URL.
- No script reads or writes a path. The restore browse listing is the single
  place a file path reaches the UI; it arrives in a POST response body and is
  never put in a URL, a log line or browser storage.

## The /design page

`cryo-console serve --fixtures` serves `/design` (it is not routed otherwise).
It renders every state chip, banner (C01 to C16), the ten-day strips and the
built-in checks (token coverage, Orbitron audit, contrast report) against the
frozen fixture clock, in both themes. Use it to review a visual change without
a database or a fleet:

    cryo-console serve --fixtures --admin-addr 127.0.0.1:8080

The fixtures password is printed at start (or set `CRYO_FIXTURES_PASSWORD`).
`internal/console/ui/testdata/state_chips.golden` pins the chip catalogue; a
deliberate change to a chip needs the golden regenerated and reviewed.
