package ui

import (
	"context"
	"strings"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/console/auth"
)

// The freshness stamp is a status pill: a dot, a word, the time and "n of m agents".
// The word is the state's carrier (colour is never alone), so each state must say its own.
func TestStampStatesRenderTheirWords(t *testing.T) {
	e := newEnv(t)
	cases := []struct {
		name  string
		st    Stamp
		word  string
		time  string
		wants []string
	}{
		{"fresh", Stamp{State: "fresh", At: "08:37:16", N: 3, M: 3, Zone: "America/Chicago"}, "Live", "08:37:16",
			[]string{`data-state="fresh"`, `<b>3</b> of <b>3</b> agents`}},
		{"stale", Stamp{State: "stale", At: "08:37:16", N: 3, M: 3, Zone: "America/Chicago"}, "Polling", "last 08:37:16",
			[]string{`data-state="stale"`, `<b>3</b> of <b>3</b> agents`}},
		{"unreachable", Stamp{State: "unreachable", At: "08:37:16", Zone: "America/Chicago"}, "No data", "since 08:37:16",
			[]string{`data-state="unreachable"`}},
		{"partial", Stamp{State: "partial", At: "08:37:16", N: 207, M: 212, Zone: "America/Chicago"}, "Partial", "08:37:16",
			[]string{`data-state="partial"`, `<b>207</b> of <b>212</b> agents`}},
	}
	seen := map[string]string{}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.st.Tooltip = "Fri 24 Oct 2026 08:37:16 (America/Chicago)"
			out, err := e.svc.renderString("board", "stamp", c.st)
			if err != nil {
				t.Fatal(err)
			}
			want := []string{`id="freshness"`, `class="fresh"`, `role="status"`, `<span class="word" aria-live="polite">`, `data-zone="America/Chicago"`, `title="Fri 24 Oct 2026 08:37:16 (America/Chicago)"`,
				`<i class="dot" aria-hidden="true"></i>`, `<span class="word" aria-live="polite">` + c.word + `</span>`,
				`<span class="t" data-at="08:37:16">` + c.time + `</span>`}
			for _, w := range append(want, c.wants...) {
				if !strings.Contains(out, w) {
					t.Errorf("stamp lacks %q:\n%s", w, out)
				}
			}
			wantSeps := 1
			if c.st.M > 0 {
				wantSeps = 2
			} else if strings.Contains(out, `class="agents"`) {
				t.Errorf("no agents segment when the total is unknown:\n%s", out)
			}
			if n := strings.Count(out, `<i class="sep" aria-hidden="true"></i>`); n != wantSeps {
				t.Errorf("separators = %d, want %d:\n%s", n, wantSeps, out)
			}
			if strings.Contains(out, "polled") || strings.Contains(out, "STALE") || strings.Contains(out, "ZgotmplZ") {
				t.Errorf("old stamp copy or a rejected value in:\n%s", out)
			}
			if prev, dup := seen[c.word]; dup {
				t.Errorf("word %q is used by both %s and %s", c.word, prev, c.name)
			}
			seen[c.word] = c.name
		})
	}
	if len(seen) != 4 {
		t.Fatalf("distinct words = %v", seen)
	}
}

// An unknown state must not read healthy, and the out-of-band wrapper keeps the same markup.
func TestStampUnknownStateIsNoDataAndOOBKeepsMarkup(t *testing.T) {
	e := newEnv(t)
	out, err := e.svc.renderString("board", "stamp", Stamp{State: "bogus", At: "--:--:--", Zone: "UTC", OOB: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{`hx-swap-oob="outerHTML"`, `<span class="word" aria-live="polite">No data</span>`, `data-at="--:--:--"`} {
		if !strings.Contains(out, w) {
			t.Errorf("stamp lacks %q:\n%s", w, out)
		}
	}
	if strings.Contains(out, ">Live<") {
		t.Errorf("an unknown state said Live:\n%s", out)
	}
}

// The fallback page (fleet unavailable) renders the lost state server-side, so it is correct before any script runs.
func TestFallbackPageStampSaysNoData(t *testing.T) {
	e := newEnv(t)
	out, err := e.svc.renderString("error", "stamp", e.svc.fallbackPage(context.Background(), &auth.Principal{}).Stamp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `<span class="word" aria-live="polite">No data</span>`) || !strings.Contains(out, `data-state="unreachable"`) {
		t.Errorf("fallback stamp:\n%s", out)
	}
}
