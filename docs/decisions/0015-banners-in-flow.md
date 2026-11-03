# ADR 0015 - Condition banners sit in the page flow, not sticky

**Date:** 2026-11-03 - **Status:** Accepted - **Decider:** foreman (M2 lab fix batch, ticket M2-FIX-C, finding F8), recorded for the operator

replica/design/components.md (1.5 App shell, 9.2 Condition banner) specified critical condition banners as sticky
below the topbar, so scrolling never hides them. In the M2 lab the sticky strip, whose banner backgrounds are
translucent soft tints, rendered page content visibly underneath itself while scrolling (finding F8). On a phone,
three stacked critical banners would also cover about half the screen.

## Decision

- The banner strip is `position: static`: it renders in the flow inside `<main>`, before the page content, and
  scrolls away with the page like any other content. Nothing renders under it.
- The persistent "something is wrong" signal at every scroll position is the topbar, which stays sticky: the
  freshness pill (NO DATA when the feed is lost) and the stamp live there.
- Everything else in 9.2 is unchanged: critical banners are never dismissible, warn banners collapse but are never
  removed, ordering and live replacement via `banner.update` are as before, and the strip still renders before
  page content on every screen, including error pages.

## Consequences

- A critical banner is always the first thing on a page load and after navigation; it is not on screen while the
  operator is scrolled deep into a long table. That is the accepted trade: the console is read top-down per page,
  and the topbar pill covers loss of data at any scroll position.
- components.md 1.5 and 9.2 are amended to point here.
