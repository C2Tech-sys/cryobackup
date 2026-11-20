package mail

import "context"

// The relay probe (ADR 0020 decision 13: `cryo-console doctor` checks the mail setup). Probe runs
// the first half of a send against the relay and stops before the envelope: connect, the
// greeting, EHLO, STARTTLS or implicit TLS as configured, AUTH when a user name is configured,
// then QUIT. No MAIL, RCPT or DATA command is ever written, so no message can leave.
//
// The conversation is the sender's own: the same Settings.Validate and ReadPasswordFile, and the
// same opening, session in sender.go (dialer, three clocks, TLS, the reply filter, EHLO, AUTH,
// failure classification), which deliver runs before the envelope. So a relay the probe passes is
// one Send gets past the sign-in with, and a failure carries the stage and class word a Send
// would; TestProbeOpensAsDeliverDoes pins the pair.

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
