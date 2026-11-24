package notify

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/mail"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// V2 (P21-T7 fix batch 1): an alert is never lost for the sake of a link. A console link that cannot
// be rendered reaches a sender only around config validation (which accepts nothing of the kind,
// TestEveryValidatedAdminURLRendersALink), so these tests hand the sender such a value directly: that
// is the seam, and it exists only here. The message goes out without the link and its button, exactly
// as with admin_url unset; the sender logs one line with class console_link; the alert is marked
// notified and the delivery sent, so nothing repeats.

// t7BadLink is a value that passed config before P21-T7 C and that net/url refuses.
const t7BadLink = "https://[10.0.0.5]:8080"

const t7LinkClass = `"class":"console_link"`

func noLinkIn(t *testing.T, what string, m mail.Message) {
	t.Helper()
	if strings.Contains(m.HTML, ">Open the console</a>") || strings.Contains(m.Text, "Open the console:") || strings.Contains(m.Text+m.HTML, "10.0.0.5") {
		t.Fatalf("%s carries a link or the bad value:\n%s", what, m.Text)
	}
	if m.Text == "" || m.HTML == "" {
		t.Fatalf("%s is empty", what)
	}
}

func TestNotifierSendsWithoutALinkThatCannotRender(t *testing.T) {
	w := newWorld(t)
	w.n.ConsoleURL = t7BadLink
	site := w.site("Summit Eye Care", 3)
	w.recipient("ops@example.test", true, true)

	// Pass 1: an alert. Delivered, without the link, one classed line; the episodes are marked.
	w.set(state.StateStale, site[0], site[1])
	res := w.pass()
	if res.Alerted != 2 || res.Sent != 1 || w.fake.Calls() != 1 {
		t.Fatalf("alert pass %+v, calls %d", res, w.fake.Calls())
	}
	noLinkIn(t, "the alert", w.fake.Sent()[0])
	if n := strings.Count(w.logs.String(), t7LinkClass); n != 1 {
		t.Fatalf("%d console_link lines after the alert pass, want 1:\n%s", n, w.logs.String())
	}
	if len(w.unnotified()) != 0 {
		t.Fatal("the alerted episodes are not marked notified: the alert would repeat")
	}
	for _, d := range w.deliveries() {
		if d.Result != store.DeliverySent {
			t.Fatalf("delivery row %+v, want sent", d)
		}
	}

	// Pass 2: a recovery and a new alert in the same pass. Both delivered, still ONE line for the pass.
	w.clk.Advance(time.Minute)
	w.set(state.StateOK, site[0], site[1])
	w.set(state.StateFailed, site[2])
	res = w.pass()
	if res.Recovered != 2 || res.Alerted != 1 || res.Sent != 2 || w.fake.Calls() != 3 {
		t.Fatalf("mixed pass %+v, calls %d", res, w.fake.Calls())
	}
	for _, m := range w.fake.Sent()[1:] {
		noLinkIn(t, "a message of the mixed pass", m)
	}
	if n := strings.Count(w.logs.String(), t7LinkClass); n != 2 {
		t.Fatalf("%d console_link lines after two sending passes, want 2 (one per pass):\n%s", n, w.logs.String())
	}

	// Pass 3: nothing to send. No mail, no repeat, no line.
	w.clk.Advance(time.Minute)
	if res := w.pass(); res.Messages != 0 || w.fake.Calls() != 3 {
		t.Fatalf("idle pass %+v, calls %d: something repeated", res, w.fake.Calls())
	}
	if n := strings.Count(w.logs.String(), t7LinkClass); n != 2 {
		t.Fatalf("an idle pass logged the link again: %d lines", n)
	}
	if strings.Contains(w.logs.String(), `"class":"internal"`) {
		t.Fatalf("logged as internal:\n%s", w.logs.String())
	}
	w.noLeaks()
}

// The same message as with admin_url unset, byte for byte (text and HTML), from a real pass.
func TestNotifierMessageWithoutLinkEqualsTheUnsetMessage(t *testing.T) {
	sent := func(link string) mail.Message {
		w := newWorld(t)
		w.n.ConsoleURL = link
		site := w.site("Summit Eye Care", 2)
		w.recipient("ops@example.test", true, false)
		w.set(state.StateFailed, site...)
		w.pass()
		if w.fake.Calls() != 1 {
			t.Fatalf("link %q: %d messages", link, w.fake.Calls())
		}
		return w.fake.Sent()[0]
	}
	bad, unset := sent(t7BadLink), sent("")
	if bad.Text != unset.Text || bad.HTML != unset.HTML || bad.Subject != unset.Subject {
		t.Fatal("the message without its link differs from the message with admin_url unset")
	}
}

// The retry job: a failed alert retried with a link that cannot render is delivered without it, one
// classed line per run.
func TestRetrySendsWithoutALinkThatCannotRender(t *testing.T) {
	w := newWorld(t)
	w.r.ConsoleURL = t7BadLink
	site := w.site("Summit Eye Care", 2)
	w.recipient("ops@example.test", true, false)
	w.recipient("oncall@example.test", true, false)
	w.fake.FailNext(2, temporary())
	w.set(state.StateFailed, site...)
	if res := w.pass(); res.Failed != 2 {
		t.Fatalf("pass %+v", res)
	}
	if strings.Contains(w.logs.String(), t7LinkClass) {
		t.Fatal("the pass (with a good link) logged console_link")
	}
	w.clk.Advance(time.Minute)
	rr := w.retry()
	if rr.Sent != 2 || len(w.fake.Sent()) != 2 {
		t.Fatalf("retry %+v, sent %d", rr, len(w.fake.Sent()))
	}
	for _, m := range w.fake.Sent() {
		noLinkIn(t, "the retried alert", m)
	}
	if n := strings.Count(w.logs.String(), t7LinkClass); n != 1 {
		t.Fatalf("%d console_link lines for one retry run of two rows, want 1:\n%s", n, w.logs.String())
	}
	w.clk.Advance(store.QueuedStaleAfter)
	if rr := w.retry(); rr.Due != 0 || w.fake.Calls() != 4 { // 2 failed + 2 retried; nothing repeats
		t.Fatalf("second retry %+v, calls %d", rr, w.fake.Calls())
	}
	w.noLeaks()
}

// The digest: sent without the link, one classed line; never a silent drop.
func TestDigestSendsWithoutALinkThatCannotRender(t *testing.T) {
	w, g := digestWorld(t, "07:00")
	g.ConsoleURL = t7BadLink
	var buf bytes.Buffer
	g.Log = logx.New(&buf, logx.LevelInfo)
	w.digestRecipient("a@example.test")
	w.at("2026-11-18 05:00")
	res, err := g.SendDigestNow(context.Background(), false)
	if err != nil || res.Sent != 1 {
		t.Fatalf("send now %+v, %v", res, err)
	}
	noLinkIn(t, "the digest", w.fake.Sent()[0])
	if n := strings.Count(buf.String(), t7LinkClass); n != 1 || !strings.Contains(buf.String(), `"kind":"digest"`) {
		t.Fatalf("%d console_link lines, want 1 of kind digest:\n%s", n, buf.String())
	}
	if strings.Contains(buf.String(), "10.0.0.5") {
		t.Fatalf("the log carries the link:\n%s", buf.String())
	}
}

// The test message: sent without the link, one classed line.
func TestTestMailSendsWithoutALinkThatCannotRender(t *testing.T) {
	fake := mail.NewFakeSender()
	tm, _, _ := testMailer(t, fake)
	tm.ConsoleURL = t7BadLink
	var buf bytes.Buffer
	tm.Log = logx.New(&buf, logx.LevelInfo)
	res, err := tm.Send(context.Background(), "ops@example.com", nil)
	if err != nil || !res.Sent || fake.Calls() != 1 {
		t.Fatalf("send %+v, %v", res, err)
	}
	m := fake.Sent()[0]
	if strings.Contains(m.Text, "Open the console:") || strings.Contains(m.HTML, ">Open the console</a>") || strings.Contains(m.Text+m.HTML, "10.0.0.5") {
		t.Fatalf("the test message carries a link:\n%s", m.Text)
	}
	if n := strings.Count(buf.String(), t7LinkClass); n != 1 || !strings.Contains(buf.String(), `"kind":"test"`) {
		t.Fatalf("%d console_link lines, want 1 of kind test:\n%s", n, buf.String())
	}
}

// A good link and no link log nothing of the kind.
func TestNoConsoleLinkLineForAGoodOrUnsetLink(t *testing.T) {
	for _, link := range []string{"https://console.example.test/", ""} {
		w := newWorld(t)
		w.n.ConsoleURL = link
		site := w.site("Summit Eye Care", 1)
		w.recipient("ops@example.test", true, true)
		w.set(state.StateStale, site...)
		w.pass()
		if w.fake.Calls() != 1 || strings.Contains(w.logs.String(), t7LinkClass) {
			t.Fatalf("link %q: calls %d, logs:\n%s", link, w.fake.Calls(), w.logs.String())
		}
	}
}
