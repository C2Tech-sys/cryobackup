package mail

import (
	"context"
	"testing"
)

func TestFakeSenderRecordsAndFails(t *testing.T) {
	f := NewFakeSender()
	ctx := context.Background()
	m := Message{From: "a@example.com", To: []string{"b@example.com"}, Subject: "one", Text: "t", Kind: KindTest}
	if err := f.Send(ctx, m); err != nil {
		t.Fatal(err)
	}
	m.To[0] = "changed@example.com" // the fake must have copied the slice
	got := f.Sent()
	if len(got) != 1 || got[0].To[0] != "b@example.com" || got[0].Kind != KindTest {
		t.Fatalf("Sent = %+v", got)
	}
	got[0].Subject = "mutated"
	if f.Sent()[0].Subject != "one" {
		t.Fatal("Sent must return a copy")
	}

	f.FailWith(&SendError{Stage: StageRecipient, Class: ClassRecipientRejected})
	for i := 0; i < 2; i++ {
		wantErr(t, f.Send(ctx, m), StageRecipient, ClassRecipientRejected, false)
	}
	if len(f.Sent()) != 1 {
		t.Fatal("a failed send was recorded")
	}
	f.FailWith(nil)
	if err := f.Send(ctx, m); err != nil {
		t.Fatalf("after FailWith(nil): %v", err)
	}

	f.FailNext(2, &SendError{Stage: StageConnect, Class: ClassTimeout, Temporary: true})
	wantErr(t, f.Send(ctx, m), StageConnect, ClassTimeout, true)
	wantErr(t, f.Send(ctx, m), StageConnect, ClassTimeout, true)
	if err := f.Send(ctx, m); err != nil {
		t.Fatalf("third send after FailNext(2): %v", err)
	}

	f.FailWhen(func(m Message) *SendError {
		if m.To[0] == "bad@example.com" {
			return &SendError{Stage: StageRecipient, Class: ClassRecipientRejected}
		}
		return nil
	})
	bad := m
	bad.To = []string{"bad@example.com"}
	wantErr(t, f.Send(ctx, bad), StageRecipient, ClassRecipientRejected, false)
	if err := f.Send(ctx, m); err != nil {
		t.Fatalf("FailWhen must pass other messages: %v", err)
	}
	if f.Calls() != 9 {
		t.Fatalf("Calls = %d, want 9", f.Calls())
	}

	f.Reset()
	if f.Calls() != 0 || len(f.Sent()) != 0 {
		t.Fatal("Reset")
	}
}

func TestFakeSenderValidatesLikeTheRealSender(t *testing.T) {
	f := NewFakeSender()
	err := f.Send(context.Background(), Message{From: "a@example.com", To: []string{"b@example.com"}, Subject: "x\r\nBcc: e@example.net", Text: "t"})
	wantErr(t, err, StageConfig, ClassMessageInvalid, false)
	if len(f.Sent()) != 0 {
		t.Fatal("an invalid message was recorded")
	}
}
