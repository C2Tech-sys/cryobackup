# ADR 0022 - The e-mail messages take the console's dark look, with the mark embedded

**Date:** 2026-11-20 - **Status:** Accepted (operator, 2026-11-20) - **Decider:** the operator, recorded by the foreman

## Context

The M4 build (ADR 0020) sent its four messages (alert, recovery notice, daily digest, test message) as a plain light
card: "messages use the light set only". `replica/design/components.md` 10.1 had specified a "light default with
dark-scheme override". The operator reviewed a mock with two looks: A, the whole message in the console's dark theme;
B, the console's dark top bar on a light body.

## Decision

1. **Look A.** The HTML part of every message is the console's dark theme: the dark top bar with the mark, the
   CRYOBACKUP wordmark and the polled time; the headline as a banner strip in the message's level colour; spaced-caps
   section labels; state chips; host names in mono; on the digest four counters (Backed up, Attention, Paused,
   Devices) taken from the same numbers as its totals line; one "Open the console" button. This replaces the
   "light only" choice of the M4 build and the "light default" of components.md 10.1. The colours are the dark tokens
   of `web/static/tokens.css`, kept in one map that a test holds equal to that file.
2. **The mark travels inside the message**, as an inline image under a content id (`multipart/related`, shown with
   `cid:`). It is part of the message, not remote content: showing it makes no request, so ADR 0020's decision on
   remote images stands. The console's previews show the same embedded file from a same-origin route
   (`GET /mail/mark.png`); the Content-Security-Policy is unchanged.
3. **The wordmark is text, and mail loads no font.** The templates use system font stacks; there is no image of
   lettering and no web font, because the console's display face is being reconsidered.
4. **What each message says does not change.** The text parts and the subjects are byte for byte what they were; every
   fact of the old HTML part is still in the new one.

## Consequences

- The known cost: a mail client's own dark or light transformation (classic Outlook on Windows, Gmail's dark mode)
  may recolour parts of a dark message unpredictably. Every colour is paired with a word (a state chip says FAILED,
  a counter is labelled), so a recoloured message loses no meaning, and the mark carries its own dark tile, so it
  stays legible on any background the client paints. The markup is inline-styled tables with every background set
  as a `bgcolor` attribute as well, which is what classic Outlook's Word engine lays out.
- The fallback not taken: Look B (the dark top bar on a light body) reads the same in every client and in print. It
  is the change to make if Look A proves unreadable in a client the operators use; it would be a new ADR.
