package store

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func enqueueM4(t *testing.T, w *world, kind, key string) Delivery {
	t.Helper()
	d, created, err := w.s.EnqueueDelivery(w.ctx, ScopeAll(), DeliveryInput{Kind: kind, DedupeKey: key, SubjectType: "console"})
	ok(t, err)
	if !created {
		t.Fatalf("enqueue %s was not created", key)
	}
	return d
}

func TestM4EnqueueIsOnePerDedupeKey(t *testing.T) {
	w := seed(t)
	all := ScopeAll()
	d, created, err := w.s.EnqueueDelivery(w.ctx, all, DeliveryInput{Kind: DeliveryDigest, DedupeKey: "digest:2026-10-22:r1", SubjectType: "console"})
	ok(t, err)
	if !created || d.Result != DeliveryQueued || d.Attempt != 1 || d.Kind != DeliveryDigest || d.SubjectType != "console" || d.SubjectID != "" ||
		!d.SentAt.Equal(t0) || d.NextRetryAt != nil || d.ErrorClass != "" || d.RuleID != "" {
		t.Fatalf("enqueued = %+v created %v", d, created)
	}
	w.clk.Advance(time.Hour)
	again, created, err := w.s.EnqueueDelivery(w.ctx, all, DeliveryInput{Kind: DeliveryDigest, DedupeKey: "digest:2026-10-22:r1", SubjectType: "console"})
	ok(t, err)
	if created || again.ID != d.ID {
		t.Fatalf("second enqueue = %+v created %v, want the first row and created false", again, created)
	}
	// Even after the row was delivered, or failed for good, the key is taken.
	must[Delivery](t)(w.s.MarkDeliverySent(w.ctx, all, d.ID))
	if again, created, _ = w.s.EnqueueDelivery(w.ctx, all, DeliveryInput{Kind: DeliveryDigest, DedupeKey: "digest:2026-10-22:r1", SubjectType: "console"}); created || again.Result != DeliverySent {
		t.Fatalf("enqueue of a delivered key = %+v created %v", again, created)
	}
	if l := must[Listing[Delivery]](t)(w.s.ListRecentDeliveries(w.ctx, all, DeliveryFilter{}, Page{})); len(l.Items) != 1 {
		t.Fatalf("%d rows for one key", len(l.Items))
	}
	// A device or customer subject names an id; a console subject does not.
	dd, _, err := w.s.EnqueueDelivery(w.ctx, all, DeliveryInput{Kind: DeliveryAlert, DedupeKey: "alert:x", SubjectType: "device", SubjectID: w.devA.ID})
	if err != nil || dd.SubjectType != "device" || dd.SubjectID != w.devA.ID {
		t.Fatalf("device subject = %+v, %v", dd, err)
	}
}

func TestM4SuppressedDeliveriesAreRecordedAndNeverSent(t *testing.T) {
	w := seed(t)
	all := ScopeAll()
	d, created, err := w.s.EnqueueDelivery(w.ctx, all, DeliveryInput{Kind: DeliveryAlert, DedupeKey: "alert:s1", SubjectType: "device", SubjectID: w.devA.ID, SuppressReason: "acknowledged"})
	if err != nil || !created || d.Result != DeliverySuppressed || d.SuppressReason != "acknowledged" {
		t.Fatalf("suppressed = %+v %v %v", d, created, err)
	}
	if _, err := w.s.MarkDeliverySent(w.ctx, all, d.ID); !errors.Is(err, ErrInvalid) {
		t.Fatalf("sending a suppressed row = %v, want ErrInvalid", err)
	}
	if _, err := w.s.MarkDeliveryFailed(w.ctx, all, d.ID, "connect", nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("failing a suppressed row = %v, want ErrInvalid", err)
	}
	if ok, _ := w.s.HasDelivered(w.ctx, all, "alert:s1"); ok {
		t.Fatal("a suppressed row counts as delivered")
	}
	if due := must[[]Delivery](t)(w.s.ListDueRetries(w.ctx, all, t0.Add(24*time.Hour), 0)); len(due) != 0 {
		t.Fatalf("a suppressed row is due: %+v", due)
	}
	f := must[SendFacts](t)(w.s.LoadSendFacts(w.ctx, all))
	if f.PendingAlerts != 0 || f.ConsecutiveFailures != 0 {
		t.Fatalf("a suppressed row counts as pending or failed: %+v", f)
	}
}

// queued -> failed -> failed -> failed -> failed (gave up) counts 1, 2, 3, 4 attempts; a success after
// failures counts the attempt that succeeded.
func TestM4RetryBookkeepingCountsAttempts(t *testing.T) {
	w := seed(t)
	all := ScopeAll()
	d := enqueueM4(t, w, DeliveryAlert, "alert:r1")
	next := func(att int) *time.Time {
		delay, more := DeliveryRetryDelay(att)
		if !more {
			return nil
		}
		at := w.clk.Now().Add(delay)
		return &at
	}
	for att := 1; att <= 4; att++ {
		w.clk.Advance(time.Minute)
		d = must[Delivery](t)(w.s.MarkDeliveryFailed(w.ctx, all, d.ID, "connect", next(att)))
		wantNext := att <= 3
		if d.Result != DeliveryFailed || d.Attempt != att || d.ErrorClass != "connect" || (d.NextRetryAt != nil) != wantNext || !d.SentAt.Equal(w.clk.Now()) {
			t.Fatalf("after failure %d: %+v", att, d)
		}
		if wantNext {
			if due := must[[]Delivery](t)(w.s.ListDueRetries(w.ctx, all, w.clk.Now(), 0)); len(due) != 0 {
				t.Fatalf("failure %d is due before its time: %+v", att, due)
			}
			w.clk.Advance(20 * time.Minute)
			due := must[[]Delivery](t)(w.s.ListDueRetries(w.ctx, all, w.clk.Now(), 0))
			if len(due) != 1 || due[0].ID != d.ID {
				t.Fatalf("failure %d is not due after its delay: %+v", att, due)
			}
		}
	}
	// Gave up: no longer due, and not pending.
	w.clk.Advance(24 * time.Hour)
	if due := must[[]Delivery](t)(w.s.ListDueRetries(w.ctx, all, w.clk.Now(), 0)); len(due) != 0 {
		t.Fatalf("a row that gave up is due: %+v", due)
	}
	f := must[SendFacts](t)(w.s.LoadSendFacts(w.ctx, all))
	if f.PendingAlerts != 0 || f.ConsecutiveFailures != 4 {
		t.Fatalf("after giving up: %+v", f)
	}

	// A second message that fails twice and then gets through: attempt 3, class and retry cleared.
	e := enqueueM4(t, w, DeliveryAlert, "alert:r2")
	must[Delivery](t)(w.s.MarkDeliveryFailed(w.ctx, all, e.ID, "tls", next(1)))
	must[Delivery](t)(w.s.MarkDeliveryFailed(w.ctx, all, e.ID, "tls", next(2)))
	sent := must[Delivery](t)(w.s.MarkDeliverySent(w.ctx, all, e.ID))
	if sent.Result != DeliverySent || sent.Attempt != 3 || sent.ErrorClass != "" || sent.NextRetryAt != nil {
		t.Fatalf("delivered after retries = %+v", sent)
	}
	if again := must[Delivery](t)(w.s.MarkDeliverySent(w.ctx, all, e.ID)); again.Attempt != 3 || again.Result != DeliverySent {
		t.Fatalf("marking sent twice changed the row: %+v", again)
	}
	if _, err := w.s.MarkDeliveryFailed(w.ctx, all, e.ID, "tls", nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("failing a delivered row = %v, want ErrConflict", err)
	}
	if _, err := w.s.MarkDeliverySent(w.ctx, all, "01JNOSUCHDELIVERY0000000000"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id = %v", err)
	}
}

func TestM4DeliveryRetrySchedule(t *testing.T) {
	want := []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute}
	for i, d := range want {
		got, more := DeliveryRetryDelay(i + 1)
		if !more || got != d {
			t.Errorf("delay after attempt %d = %v %v, want %v", i+1, got, more, d)
		}
	}
	for _, att := range []int{0, -1, 4, 9} {
		if _, more := DeliveryRetryDelay(att); more {
			t.Errorf("attempt %d has a retry", att)
		}
	}
}

// A row left queued by a sender that died is picked up once it is stale; a fresh one is not.
func TestM4StaleQueuedRowsAreDue(t *testing.T) {
	w := seed(t)
	d := enqueueM4(t, w, DeliveryAlert, "alert:q1")
	if due := must[[]Delivery](t)(w.s.ListDueRetries(w.ctx, ScopeAll(), t0.Add(QueuedStaleAfter-time.Second), 0)); len(due) != 0 {
		t.Fatalf("a fresh queued row is due: %+v", due)
	}
	due := must[[]Delivery](t)(w.s.ListDueRetries(w.ctx, ScopeAll(), t0.Add(QueuedStaleAfter), 0))
	if len(due) != 1 || due[0].ID != d.ID {
		t.Fatalf("a stale queued row is not due: %+v", due)
	}
	// Its first outcome is still attempt 1: the send it stands for never happened.
	if f := must[Delivery](t)(w.s.MarkDeliveryFailed(w.ctx, ScopeAll(), d.ID, "connect", nil)); f.Attempt != 1 {
		t.Fatalf("attempt = %d", f.Attempt)
	}
}

func TestM4HasDeliveredIsTrueOnlyForSent(t *testing.T) {
	w := seed(t)
	all := ScopeAll()
	d := enqueueM4(t, w, DeliveryDigest, "digest:2026-10-22:r1")
	has := func() bool { return must[bool](t)(w.s.HasDelivered(w.ctx, all, "digest:2026-10-22:r1")) }
	if has() {
		t.Fatal("queued counts as delivered")
	}
	must[Delivery](t)(w.s.MarkDeliveryFailed(w.ctx, all, d.ID, "connect", nil))
	if has() {
		t.Fatal("failed counts as delivered")
	}
	must[Delivery](t)(w.s.MarkDeliverySent(w.ctx, all, d.ID))
	if !has() {
		t.Fatal("sent does not count as delivered")
	}
	if must[bool](t)(w.s.HasDelivered(w.ctx, all, "digest:2026-10-22:other")) {
		t.Fatal("another key counts")
	}
}

func TestM4DeliveryValidation(t *testing.T) {
	w := seed(t)
	all := ScopeAll()
	cases := map[string]DeliveryInput{
		"kind":            {Kind: "gossip", DedupeKey: "k", SubjectType: "console"},
		"dedupe_key":      {Kind: DeliveryAlert, DedupeKey: "has space", SubjectType: "console"},
		"empty key":       {Kind: DeliveryAlert, DedupeKey: "", SubjectType: "console"},
		"crlf key":        {Kind: DeliveryAlert, DedupeKey: "k\r\nBcc: x", SubjectType: "console"},
		"long key":        {Kind: DeliveryAlert, DedupeKey: strings.Repeat("k", 201), SubjectType: "console"},
		"subject type":    {Kind: DeliveryAlert, DedupeKey: "k", SubjectType: "fleet"},
		"device no id":    {Kind: DeliveryAlert, DedupeKey: "k", SubjectType: "device"},
		"console with id": {Kind: DeliveryAlert, DedupeKey: "k", SubjectType: "console", SubjectID: "x"},
		"suppress reason": {Kind: DeliveryAlert, DedupeKey: "k", SubjectType: "console", SuppressReason: "bored"},
	}
	for name, in := range cases {
		if _, _, err := w.s.EnqueueDelivery(w.ctx, all, in); !errors.Is(err, ErrInvalid) || len(FieldErrors(err)) == 0 {
			t.Errorf("%s: err = %v, want a *ValidationError", name, err)
		}
	}
	if _, _, err := w.s.EnqueueDelivery(w.ctx, all, DeliveryInput{Kind: DeliveryAlert, DedupeKey: "k", SubjectType: "console", RuleID: "01JNOSUCHRULE000000000000"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown rule = %v, want ErrNotFound", err)
	}
	d := enqueueM4(t, w, DeliveryAlert, "alert:v1")
	for _, class := range []string{"", "Connect", "550 5.1.1 user unknown", "a b", "tls-handshake", strings.Repeat("x", 41), "ok\n"} {
		if _, err := w.s.MarkDeliveryFailed(w.ctx, all, d.ID, class, nil); !errors.Is(err, ErrInvalid) {
			t.Errorf("error class %q = %v, want ErrInvalid (a class word, never server text)", class, err)
		}
	}
	if got := must[Delivery](t)(w.s.MarkDeliveryFailed(w.ctx, all, d.ID, "sender_refused", nil)); got.ErrorClass != "sender_refused" {
		t.Errorf("class = %q", got.ErrorClass)
	}
	if _, err := w.s.ListRecentDeliveries(w.ctx, all, DeliveryFilter{Kind: "gossip"}, Page{}); !errors.Is(err, ErrInvalid) {
		t.Errorf("unknown kind filter = %v", err)
	}
	if _, err := w.s.ListRecentDeliveries(w.ctx, all, DeliveryFilter{Result: "maybe"}, Page{}); !errors.Is(err, ErrInvalid) {
		t.Errorf("unknown result filter = %v", err)
	}
}

func TestM4ListRecentDeliveriesPagesNewestFirst(t *testing.T) {
	w := seed(t)
	all := ScopeAll()
	var ids []string
	for i, k := range []string{DeliveryAlert, DeliveryDigest, DeliveryAlert, DeliveryTest, DeliveryAlert} {
		w.clk.Advance(time.Minute)
		ids = append(ids, enqueueM4(t, w, k, "k"+string(rune('a'+i))).ID)
	}
	w.clk.Advance(time.Minute)
	must[Delivery](t)(w.s.MarkDeliveryFailed(w.ctx, all, ids[0], "connect", nil)) // the oldest row is touched last: it moves to the top
	p1 := must[Listing[Delivery]](t)(w.s.ListRecentDeliveries(w.ctx, all, DeliveryFilter{}, Page{Limit: 2}))
	if len(p1.Items) != 2 || p1.NextCursor == "" || p1.Items[0].ID != ids[0] || p1.Items[1].ID != ids[4] {
		t.Fatalf("page 1 = %+v next %q", p1.Items, p1.NextCursor)
	}
	p2 := must[Listing[Delivery]](t)(w.s.ListRecentDeliveries(w.ctx, all, DeliveryFilter{}, Page{Limit: 2, Cursor: p1.NextCursor}))
	if len(p2.Items) != 2 || p2.NextCursor == "" || p2.Items[0].ID != ids[3] || p2.Items[1].ID != ids[2] {
		t.Fatalf("page 2 = %+v", p2.Items)
	}
	p3 := must[Listing[Delivery]](t)(w.s.ListRecentDeliveries(w.ctx, all, DeliveryFilter{}, Page{Limit: 2, Cursor: p2.NextCursor}))
	if len(p3.Items) != 1 || p3.NextCursor != "" || p3.Items[0].ID != ids[1] {
		t.Fatalf("page 3 = %+v", p3.Items)
	}
	if l := must[Listing[Delivery]](t)(w.s.ListRecentDeliveries(w.ctx, all, DeliveryFilter{Kind: DeliveryAlert}, Page{})); len(l.Items) != 3 {
		t.Fatalf("kind filter = %d rows, want 3", len(l.Items))
	}
	if l := must[Listing[Delivery]](t)(w.s.ListRecentDeliveries(w.ctx, all, DeliveryFilter{Result: DeliveryFailed}, Page{})); len(l.Items) != 1 || l.Items[0].ID != ids[0] {
		t.Fatalf("result filter = %+v", l.Items)
	}
	if l := must[Listing[Delivery]](t)(w.s.ListRecentDeliveries(w.ctx, all, DeliveryFilter{Kind: DeliveryReport}, Page{})); len(l.Items) != 0 || l.Items == nil {
		t.Fatalf("an empty page must be a non-nil empty list: %+v", l.Items)
	}
	if _, err := w.s.ListRecentDeliveries(w.ctx, all, DeliveryFilter{}, Page{Cursor: "nonsense"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad cursor = %v", err)
	}
}
