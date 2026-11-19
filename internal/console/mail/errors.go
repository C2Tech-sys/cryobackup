package mail

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/textproto"
	"syscall"
)

// Stage is where in the conversation a send failed.
type Stage string

// The stages of a send. StageConfig is anything wrong on our side before a
// connection is attempted: the settings, the password file, the message itself.
const (
	StageConnect   Stage = "connect"
	StageTLS       Stage = "tls"
	StageAuth      Stage = "auth"
	StageSender    Stage = "sender"
	StageRecipient Stage = "recipient"
	StageData      Stage = "data"
	StageConfig    Stage = "config"
)

// Class is the fixed word naming why a send failed. It is safe to log, store
// and show: it never carries text from the relay.
type Class string

// The class words. The list is closed; Classes returns it.
const (
	// ClassTimeout is a deadline that passed (dial, greeting or the whole send).
	ClassTimeout Class = "timeout"
	// ClassCanceled is a context cancelled by the caller (a shutdown).
	ClassCanceled Class = "canceled"
	// ClassRefused is a refused or reset connection.
	ClassRefused Class = "refused"
	// ClassDNS is a host name that did not resolve.
	ClassDNS Class = "dns"
	// ClassNetwork is any other network failure (unreachable, dropped).
	ClassNetwork Class = "network"
	// ClassCertInvalid is a relay certificate that did not verify: wrong name,
	// unknown issuer, expired.
	ClassCertInvalid Class = "cert_invalid"
	// ClassTLSHandshake is a TLS handshake that failed for another reason.
	ClassTLSHandshake Class = "tls_handshake"
	// ClassStartTLSMissing is a relay that does not offer STARTTLS while the
	// configuration requires it.
	ClassStartTLSMissing Class = "starttls_missing"
	// ClassStartTLSRejected is a relay that refused the STARTTLS command.
	ClassStartTLSRejected Class = "starttls_rejected"
	// ClassAuthRejected is a relay that refused the user name or password.
	ClassAuthRejected Class = "auth_rejected"
	// ClassAuthUnsupported is a relay that offers no mechanism we use (PLAIN,
	// LOGIN) although a user name is configured.
	ClassAuthUnsupported Class = "auth_unsupported"
	// ClassSenderRejected is a refused envelope sender (MAIL FROM).
	ClassSenderRejected Class = "sender_rejected"
	// ClassRecipientRejected is a refused recipient (RCPT TO).
	ClassRecipientRejected Class = "recipient_rejected"
	// ClassMessageRejected is a refused message (DATA, or the reply after it).
	ClassMessageRejected Class = "message_rejected"
	// ClassProtocol is a relay that answered outside the protocol: a greeting
	// that is not 220, a refused EHLO, an unexpected reply.
	ClassProtocol Class = "protocol"
	// ClassPasswordFile is a password file that cannot be used: missing,
	// unreadable, empty, not a regular file, or more than one line.
	ClassPasswordFile Class = "password_file"
	// ClassPasswordFileMode is a password file readable by group or others
	// (Unix only).
	ClassPasswordFileMode Class = "password_file_mode"
	// ClassPlaintextRefused is plain security without smtp.allow_plaintext, or
	// with a user name: credentials are never sent in clear.
	ClassPlaintextRefused Class = "plaintext_refused"
	// ClassNotConfigured is a send with no relay configured.
	ClassNotConfigured Class = "not_configured"
	// ClassConfigInvalid is settings that cannot work (bad security word, no
	// port, a header-unsafe value).
	ClassConfigInvalid Class = "config_invalid"
	// ClassMessageInvalid is a message that cannot be built: no recipient, no
	// text, a CR or LF in a header value, an address that does not parse.
	ClassMessageInvalid Class = "message_invalid"
)

// Classes lists every class word, in a fixed order.
func Classes() []Class {
	return []Class{
		ClassTimeout, ClassCanceled, ClassRefused, ClassDNS, ClassNetwork,
		ClassCertInvalid, ClassTLSHandshake, ClassStartTLSMissing, ClassStartTLSRejected,
		ClassAuthRejected, ClassAuthUnsupported, ClassSenderRejected, ClassRecipientRejected,
		ClassMessageRejected, ClassProtocol, ClassPasswordFile, ClassPasswordFileMode,
		ClassPlaintextRefused, ClassNotConfigured, ClassConfigInvalid, ClassMessageInvalid,
	}
}

// SendError is the one failure type of Send. It carries words only. Temporary
// follows the protocol: a 4xx reply, a timeout and a network failure are
// temporary (a retry may succeed); a 5xx reply, a certificate that does not
// verify and every problem on our side are not.
type SendError struct {
	Stage     Stage
	Class     Class
	Temporary bool
}

// Error implements error. The text is fixed words: no host, address, password
// or relay reply is ever in it.
func (e *SendError) Error() string {
	s := "mail: send failed at " + string(e.Stage) + ": " + string(e.Class)
	if e.Temporary {
		s += " (temporary)"
	}
	return s
}

// Hint is one fixed sentence that tells an operator what the class means and
// what to look at. It never contains relay text.
func (e *SendError) Hint() string { return Hint(e.Class) }

// Hint returns the fixed sentence for a class word.
func Hint(c Class) string {
	switch c {
	case ClassTimeout:
		return "The mail relay did not answer in time. Check that the host and port are reachable from the console."
	case ClassCanceled:
		return "The send was cancelled, for example by a shutdown."
	case ClassRefused:
		return "The mail relay refused the connection. Check the host, the port and the relay's firewall."
	case ClassDNS:
		return "The mail relay's host name did not resolve. Check smtp.host and the console's DNS."
	case ClassNetwork:
		return "The network connection to the mail relay failed. Check routing and the relay's firewall."
	case ClassCertInvalid:
		return "The mail relay's certificate was not accepted. Check that smtp.host matches the certificate, that its issuer is trusted by this host and that it has not expired."
	case ClassTLSHandshake:
		return "The TLS handshake with the mail relay failed. The relay must support TLS 1.2 or later."
	case ClassStartTLSMissing:
		return "The mail relay does not offer STARTTLS. Use a relay that does, or smtp.security tls on its implicit-TLS port."
	case ClassStartTLSRejected:
		return "The mail relay refused to start TLS."
	case ClassAuthRejected:
		return "The mail relay rejected the user name or password. Check smtp.username and the password file."
	case ClassAuthUnsupported:
		return "The mail relay offers no sign-in method this console uses (PLAIN or LOGIN)."
	case ClassSenderRejected:
		return "The mail relay refused the sender address. Check smtp.from against what the relay allows."
	case ClassRecipientRejected:
		return "The mail relay refused a recipient address."
	case ClassMessageRejected:
		return "The mail relay refused the message."
	case ClassProtocol:
		return "The mail relay answered in a way this console does not understand. Check that smtp.host and smtp.port point at an SMTP relay."
	case ClassPasswordFile:
		return "The password file cannot be used: it must exist, be readable and hold the password on one line."
	case ClassPasswordFileMode:
		return "The password file is readable by group or others. Restrict it to its owner (chmod 600)."
	case ClassPlaintextRefused:
		return "Plain (unencrypted) SMTP needs smtp.allow_plaintext, and credentials are never sent over it."
	case ClassNotConfigured:
		return "No mail relay is configured. Set smtp.host in the configuration."
	case ClassConfigInvalid:
		return "The smtp settings are not valid."
	case ClassMessageInvalid:
		return "The message cannot be sent as built."
	}
	return ""
}

func newErr(stage Stage, class Class, temporary bool) *SendError {
	return &SendError{Stage: stage, Class: class, Temporary: temporary}
}

// netFailure classifies an error that is not an SMTP reply, at a stage. The
// context wins: when the caller's context has ended, that is the cause whatever
// error the closed connection reports.
func netFailure(ctx context.Context, stage Stage, err error) *SendError {
	if ctx != nil {
		switch ctx.Err() {
		case context.DeadlineExceeded:
			return newErr(stage, ClassTimeout, true)
		case context.Canceled:
			return newErr(stage, ClassCanceled, true)
		}
	}
	var de *net.DNSError
	switch {
	case isTimeout(err):
		return newErr(stage, ClassTimeout, true)
	case errors.As(err, &de):
		// A name that does not exist is not worth retrying at once; a lookup that
		// failed may be a passing resolver problem.
		return newErr(stage, ClassDNS, !de.IsNotFound)
	case refused(err):
		return newErr(stage, ClassRefused, true)
	}
	return newErr(stage, ClassNetwork, true)
}

// replyFailure classifies an error from a command: the reply class when the
// relay answered with a code, otherwise a network failure. class is what a
// refusal (a 4xx or 5xx reply) means at this stage.
func replyFailure(ctx context.Context, stage Stage, class Class, err error) *SendError {
	var te *textproto.Error
	if errors.As(err, &te) {
		return newErr(stage, class, te.Code >= 400 && te.Code < 500)
	}
	if notSMTP(err) {
		return newErr(stage, ClassProtocol, false)
	}
	return netFailure(ctx, stage, err)
}

// notSMTP reports a reply that is not an SMTP reply at all (no three-digit code
// and a space or hyphen), such as the "HTTP/1.1 400" of a web server on the
// mail port. The relay answered, so it is not a network failure, and a retry
// would get the same answer.
func notSMTP(err error) bool {
	var pe textproto.ProtocolError
	return errors.As(err, &pe)
}

// tlsFailure classifies a failed TLS handshake.
func tlsFailure(ctx context.Context, err error) *SendError {
	var ve *tls.CertificateVerificationError
	if errors.As(err, &ve) {
		return newErr(StageTLS, ClassCertInvalid, false)
	}
	var te *textproto.Error
	if errors.As(err, &te) {
		return newErr(StageTLS, ClassStartTLSRejected, te.Code >= 400 && te.Code < 500)
	}
	if notSMTP(err) {
		return newErr(StageTLS, ClassStartTLSRejected, false)
	}
	if f := netFailure(ctx, StageTLS, err); f.Class == ClassTimeout || f.Class == ClassCanceled {
		return f
	}
	return newErr(StageTLS, ClassTLSHandshake, true)
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout())
}

// wsaeconnrefused is WSAECONNREFUSED (winerror.h), which the syscall package
// does not name. On other systems the value is just a number that never
// matches.
const wsaeconnrefused = syscall.Errno(10061)

func refused(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, wsaeconnrefused)
}
