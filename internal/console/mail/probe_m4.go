package mail

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/smtp"
	"strconv"
	"time"
)

// The relay probe (ADR 0020 decision 13: `cryo-console doctor` checks the mail setup). Probe runs
// the first half of a send against the relay and stops before the envelope: connect, the
// greeting, EHLO, STARTTLS or implicit TLS as configured, AUTH when a user name is configured,
// then QUIT. No MAIL, RCPT or DATA command is ever written, so no message can leave.
//
// The conversation uses the sender's own pieces: the same Settings.Validate, ReadPasswordFile,
// dialer and three clocks (DialTimeout, GreetingTimeout, Timeout), the same tlsConfig, pickAuth
// and loginAuth, and the same failure classification (netFailure, tlsFailure, replyFailure). So a
// relay the probe passes is one Send gets past the sign-in with, and a failure carries the stage
// and class word a Send would.
//
// session below is the opening half of deliver in sender.go, step for step. sender.go is not
// rewritten to call it (that file belongs to the sender's own tickets); a change to how deliver
// opens a connection must be made here too, and TestProbeOpensAsDeliverDoes pins the pair.

// Probe connects to the relay of the sender's settings, says EHLO, starts TLS as configured,
// signs in when a user name is configured, and quits without sending a message. It returns nil
// when all of that succeeded, or a *SendError with the stage and class of the first failure; the
// relay's reply text, the host, the user name and the password are never in it. QUIT's own
// outcome is not judged: by then the relay has answered everything the probe asks. Probe writes
// no log line; the caller reports the outcome.
func (s *SMTPSender) Probe(ctx context.Context) error {
	if err := s.set.Validate(); err != nil {
		return err
	}
	var password string
	if s.set.Username != "" {
		var err error
		if password, err = ReadPasswordFile(s.set.PasswordFile); err != nil {
			return err
		}
	}
	ss, err := s.session(ctx, password)
	if err != nil {
		return err
	}
	defer ss.release()
	closing(ss.conn, ss.overall, ss.c.Quit)
	return nil
}

// smtpSession is an open, greeted, encrypted-as-configured and signed-in connection.
type smtpSession struct {
	c       *smtp.Client
	conn    net.Conn
	overall time.Time
	release func()
}

// session opens a connection exactly as deliver does, up to and including AUTH. On error every
// resource is already released; on success the caller calls release.
func (s *SMTPSender) session(ctx context.Context, password string) (out smtpSession, err error) {
	set := s.set
	overall := time.Now().Add(orDefault(set.Timeout, DefaultTimeout))
	if dl, ok := ctx.Deadline(); ok && dl.Before(overall) {
		overall = dl
	}
	ctx, cancel := context.WithDeadline(ctx, overall)
	var cleanup []func()
	release := func() {
		for i := len(cleanup) - 1; i >= 0; i-- {
			cleanup[i]()
		}
		cancel()
	}
	defer func() {
		if err != nil {
			release()
		}
	}()

	dial := set.Dial
	if dial == nil {
		d := &net.Dialer{}
		dial = d.DialContext
	}
	dctx, dcancel := context.WithTimeout(ctx, orDefault(set.DialTimeout, DefaultDialTimeout))
	conn, derr := dial(dctx, "tcp", net.JoinHostPort(set.Host, strconv.Itoa(set.Port)))
	dcancel()
	if derr != nil {
		return smtpSession{}, netFailure(ctx, StageConnect, derr)
	}
	cleanup = append(cleanup, func() { _ = conn.Close() })
	// As in deliver: an ended context closes the connection under a blocked read or write.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	cleanup = append(cleanup, func() { stop() })

	greet := time.Now().Add(orDefault(set.GreetingTimeout, DefaultGreetingTimeout))
	if greet.After(overall) {
		greet = overall
	}
	_ = conn.SetDeadline(greet)

	tcfg := s.tlsConfig()
	var nc net.Conn = conn
	encrypted := false
	if set.Security == "tls" {
		tc := tls.Client(conn, tcfg)
		if herr := tc.HandshakeContext(ctx); herr != nil {
			return smtpSession{}, tlsFailure(ctx, herr)
		}
		nc, encrypted = tc, true
	}
	c, cerr := smtp.NewClient(nc, set.Host)
	if cerr != nil {
		return smtpSession{}, replyFailure(ctx, StageConnect, ClassProtocol, cerr)
	}
	cleanup = append(cleanup, func() { _ = c.Close() })
	_ = conn.SetDeadline(overall)

	helo := set.HeloName
	if helo == "" {
		helo = "localhost"
	}
	if herr := c.Hello(helo); herr != nil {
		return smtpSession{}, replyFailure(ctx, StageConnect, ClassProtocol, herr)
	}
	if set.Security == "starttls" {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return smtpSession{}, newErr(StageTLS, ClassStartTLSMissing, false)
		}
		if terr := c.StartTLS(tcfg); terr != nil {
			return smtpSession{}, tlsFailure(ctx, terr)
		}
		encrypted = true
	}

	if set.Username != "" {
		if !encrypted {
			return smtpSession{}, newErr(StageAuth, ClassPlaintextRefused, false)
		}
		a := pickAuth(c, set.Host, set.Username, password)
		if a == nil {
			return smtpSession{}, newErr(StageAuth, ClassAuthUnsupported, false)
		}
		if aerr := c.Auth(a); aerr != nil {
			if errors.Is(aerr, errLoginChallenge) {
				return smtpSession{}, newErr(StageAuth, ClassProtocol, false)
			}
			return smtpSession{}, replyFailure(ctx, StageAuth, ClassAuthRejected, aerr)
		}
	}
	return smtpSession{c: c, conn: conn, overall: overall, release: release}, nil
}
