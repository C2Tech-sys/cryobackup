package mail

import (
	"bufio"
	"io"
	"net/textproto"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestBareCodeReadsBothErrorForms pins the two texts net/textproto has used for
// a reply shorter than four characters: Go 1.26 writes the line as it is
// (short response: 250), Go 1.27 quotes it (short response: "250"). The sender
// reads the code back from the form this binary's textproto writes, and only
// from that form.
func TestBareCodeReadsBothErrorForms(t *testing.T) {
	for _, tc := range []struct {
		text   string
		quoted bool
		code   int
		ok     bool
	}{
		{`short response: 250`, false, 250, true},
		{`short response: "250"`, true, 250, true},
		{`short response: 421`, false, 421, true},
		{`short response: "550"`, true, 550, true},
		// A line that only looks like the other form is not a code.
		{`short response: "250"`, false, 0, false},
		{`short response: 250`, true, 0, false},
		{`short response: 25`, false, 0, false},
		{`short response: "25"`, true, 0, false},
		{`short response: 2500`, false, 0, false},
		{`short response: abc`, false, 0, false},
		{`short response: "abc"`, true, 0, false},
		{`short response: +25`, false, 0, false},
		{`short response: 199`, false, 0, false},
		{`short response: "600"`, true, 0, false},
		{`short response: `, false, 0, false},
		{`short response: ""`, true, 0, false},
		{`short response: "\x00\x00\x00"`, true, 0, false},
		{`invalid response code: 250`, false, 0, false},
		{`unexpected multi-line response: "250"`, true, 0, false},
	} {
		code, ok := bareCodeText(tc.text, tc.quoted)
		if code != tc.code || ok != tc.ok {
			t.Errorf("bareCodeText(%q, quoted=%v) = %d, %v; want %d, %v", tc.text, tc.quoted, code, ok, tc.code, tc.ok)
		}
	}
	// What this toolchain's net/textproto actually produces, single-line and as
	// the reply of a command, is read back.
	for _, in := range []string{"250\r\n", "250\n"} {
		_, _, err := textproto.NewReader(bufio.NewReader(strings.NewReader(in))).ReadResponse(250)
		if code, ok := bareCode(err); !ok || code != 250 {
			t.Fatalf("this toolchain's bare 250 (%v, quoted=%v) is not read as 250", err, shortQuoted)
		}
		if !bareOK(err) {
			t.Fatal("bareOK is false for this toolchain's bare 250")
		}
	}
	t.Logf("%s: textproto quotes a short response: %v", runtime.Version(), shortQuoted)
}

// TestReplyLinesPadsBareCodes is the line filter itself: a line that is nothing
// but three digits gets the space net/textproto requires; every other byte is
// passed through unchanged.
func TestReplyLinesPadsBareCodes(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"250\r\n", "250 \r\n"},
		{"250\n", "250 \n"},
		{"250 \r\n", "250 \r\n"},
		{"250-a\r\n250\r\n", "250-a\r\n250 \r\n"},
		{"250 ok\r\n", "250 ok\r\n"},
		{"25\r\n", "25\r\n"},
		{"2500\r\n", "2500\r\n"},
		{"abc\r\n", "abc\r\n"},
		{"x250\r\n", "x250\r\n"},
		{"250", "250"}, // no end of line: left for textproto to judge
		{"", ""},
	} {
		got, err := io.ReadAll(newReplyLines(bufio.NewReader(strings.NewReader(tc.in))))
		if err != nil {
			t.Fatalf("%q: %v", tc.in, err)
		}
		if string(got) != tc.want {
			t.Errorf("%q -> %q, want %q", tc.in, got, tc.want)
		}
	}
	// A line longer than the buffer is passed through in pieces; a piece that
	// happens to end a long line with "250\r\n" is not the start of a line.
	long := strings.Repeat("x", 40) + "250\r\n" + "250\r\n"
	got, err := io.ReadAll(newReplyLines(bufio.NewReaderSize(strings.NewReader(long), 16)))
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.Repeat("x", 40) + "250\r\n" + "250 \r\n"; string(got) != want {
		t.Fatalf("long line: %q, want %q", got, want)
	}
	// Read sizes smaller than a line still deliver every byte.
	r := newReplyLines(bufio.NewReader(strings.NewReader("250\r\n221 bye\r\n")))
	var b strings.Builder
	p := make([]byte, 1)
	for {
		n, err := r.Read(p)
		b.Write(p[:n])
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if b.String() != "250 \r\n221 bye\r\n" {
		t.Fatalf("one byte at a time: %q", b.String())
	}
}

// TestBareReplyCodesEverywhere: RFC 5321 4.2.1 makes the text of a reply
// optional, so "250" alone, "250 " and a multi-line reply whose last line is a
// bare "250" are all a success. A success read as a failure after the final
// dot makes the console retry and deliver the message twice; a multi-line
// reply that net/textproto cannot end hangs the send until its deadline.
func TestBareReplyCodesEverywhere(t *testing.T) {
	plain := func(cfg fakeCfg) (Settings, *fakeSrv) {
		srv := startFake(t, cfg)
		set := srv.settings("plain")
		set.AllowPlaintext = true
		// A hang shows as a timeout within seconds, not the two-minute default.
		set.Timeout = 5 * time.Second
		return set, srv
	}
	for _, tc := range []struct {
		name  string
		reply map[string]string
	}{
		{"bare 250 after the final dot", map[string]string{"DATAEND": "250"}},
		{"250 with a trailing space after the final dot", map[string]string{"DATAEND": "250 "}},
		{"multi-line, bare last line, after the final dot", map[string]string{"DATAEND": "250-2.0.0 queued\n250"}},
		{"multi-line, bare first and last lines, after the final dot", map[string]string{"DATAEND": "250-\n250"}},
		{"bare 250 for MAIL and RCPT", map[string]string{"MAIL": "250", "RCPT": "250 "}},
		{"multi-line bare for MAIL and RCPT", map[string]string{"MAIL": "250-ok\n250", "RCPT": "250-\n250"}},
		{"multi-line EHLO with a bare last line", map[string]string{"EHLO": "250-" + testHost + "\n250-8BITMIME\n250"}},
	} {
		set, srv := plain(fakeCfg{reply: tc.reply})
		if err := sendWith(t, set, testMsg()); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if !srv.sawCmd("QUIT") {
			t.Fatalf("%s: no QUIT after an accepted message", tc.name)
		}
	}
	// A refusal written as a bare code, single or multi-line, keeps its class.
	for _, tc := range []struct {
		verb, reply string
		stage       Stage
		class       Class
		temp        bool
	}{
		{"DATAEND", "451-try later\n451", StageData, ClassMessageRejected, true},
		{"DATAEND", "554-\n554", StageData, ClassMessageRejected, false},
		{"RCPT", "550-no such user\n550", StageRecipient, ClassRecipientRejected, false},
		{"MAIL", "421 ", StageSender, ClassSenderRejected, true},
	} {
		set, srv := plain(fakeCfg{reply: map[string]string{tc.verb: tc.reply}})
		wantErr(t, sendWith(t, set, testMsg()), tc.stage, tc.class, tc.temp)
		if len(srv.captured()) != 0 {
			t.Fatalf("%s %q: a message was kept", tc.verb, tc.reply)
		}
	}
}

// TestBareReplyCodesOverTLS: the same filter is in place on both TLS paths,
// including after STARTTLS replaces the connection under net/smtp.
func TestBareReplyCodesOverTLS(t *testing.T) {
	p := testPKI(t)
	for _, sec := range []string{"tls", "starttls"} {
		srv := startFake(t, fakeCfg{
			cert: &p.good, implicit: sec == "tls", startTLS: sec == "starttls",
			authMech: "PLAIN", user: "svc", pass: "pw",
			reply: map[string]string{"DATAEND": "250-2.0.0 queued\n250", "MAIL": "250"},
		})
		set := srv.settings(sec)
		set.Username, set.PasswordFile = "svc", pwFile(t, "pw")
		set.Timeout = 5 * time.Second
		if err := sendWith(t, set, testMsg()); err != nil {
			t.Fatalf("%s: %v", sec, err)
		}
		if !srv.sawCmd("QUIT") {
			t.Fatalf("%s: no QUIT after an accepted message", sec)
		}
	}
}
