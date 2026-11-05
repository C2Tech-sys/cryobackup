package probe

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/smtp"
	"net/textproto"
	"strconv"
	"time"
)

// SMTPSettings configure the SMTP probe (config smtp: host, port, security).
type SMTPSettings struct {
	Host string
	Port int
	// Security is "starttls", "tls" (implicit TLS) or "plain".
	Security string
	// HeloName is the name sent with EHLO; empty means "localhost".
	HeloName string
	// Roots verifies the relay's certificate; nil means the system roots.
	Roots *x509.CertPool
	// Timeout bounds the probe; zero means ProbeTimeout.
	Timeout time.Duration
	// Dial replaces the TCP dialer (tests).
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

// Configured reports whether an SMTP relay is configured at all.
func (s SMTPSettings) Configured() bool { return s.Host != "" && s.Port > 0 }

// CheckSMTP is one SMTP probe: connect, the 220 greeting, EHLO, STARTTLS when
// security is starttls (or a TLS handshake first when it is tls), then QUIT.
// No MAIL FROM is sent and no message: the probe proves the relay answers
// and negotiates, nothing more (architecture §5.7 health-prober). It does not
// authenticate, so the relay password is never read here.
func CheckSMTP(ctx context.Context, s SMTPSettings) Outcome {
	if !s.Configured() {
		return Outcome{Class: "not_configured"}
	}
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = ProbeTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	dial := s.Dial
	if dial == nil {
		d := &net.Dialer{}
		dial = d.DialContext
	}
	dctx, dcancel := context.WithTimeout(ctx, StageTimeout)
	conn, err := dial(dctx, "tcp", net.JoinHostPort(s.Host, strconv.Itoa(s.Port)))
	dcancel()
	if err != nil {
		return Outcome{Class: netOrDNSClass(err)}
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	tcfg := &tls.Config{ServerName: s.Host, RootCAs: s.Roots, MinVersion: tls.VersionTLS12}
	var notAfter time.Time
	if s.Security == "tls" {
		tc := tls.Client(conn, tcfg)
		if err := tc.HandshakeContext(ctx); err != nil {
			return Outcome{Class: smtpTLSClass(err), NotAfter: unverifiedNotAfter(err)}
		}
		notAfter = leafNotAfter(tc.ConnectionState())
		conn = tc
	}
	c, err := smtp.NewClient(conn, s.Host)
	if err != nil {
		return Outcome{Class: smtpClass("greeting", err), NotAfter: notAfter}
	}
	defer c.Close()
	helo := s.HeloName
	if helo == "" {
		helo = "localhost"
	}
	if err := c.Hello(helo); err != nil {
		return Outcome{Class: smtpClass("ehlo", err), NotAfter: notAfter}
	}
	if s.Security == "starttls" {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return Outcome{Class: "starttls_unsupported"}
		}
		if err := c.StartTLS(tcfg); err != nil {
			return Outcome{Class: smtpTLSClass(err), NotAfter: unverifiedNotAfter(err)}
		}
		if st, ok := c.TLSConnectionState(); ok {
			notAfter = leafNotAfter(st)
		}
	}
	if err := c.Quit(); err != nil {
		return Outcome{Class: smtpClass("quit", err), NotAfter: notAfter}
	}
	return Outcome{OK: true, NotAfter: notAfter}
}

func leafNotAfter(st tls.ConnectionState) time.Time {
	if len(st.PeerCertificates) == 0 {
		return time.Time{}
	}
	return st.PeerCertificates[0].NotAfter.UTC()
}

// netOrDNSClass names a failed dial.
func netOrDNSClass(err error) string {
	var de *net.DNSError
	if errors.As(err, &de) && !de.IsTimeout {
		if de.IsNotFound {
			return "nxdomain"
		}
		return "lookup_failed"
	}
	return netClass(err)
}

// smtpTLSClass names a failed TLS handshake with the relay.
func smtpTLSClass(err error) string {
	var ve *tls.CertificateVerificationError
	if errors.As(err, &ve) {
		return verifyClass(ve.Err)
	}
	if isTimeout(err) {
		return "timeout"
	}
	return "tls_handshake"
}

// smtpClass names a failed SMTP step: the reply code class when the relay
// answered ("ehlo_5xx"), a timeout, or the step.
func smtpClass(step string, err error) string {
	var te *textproto.Error
	switch {
	case errors.As(err, &te):
		return step + "_" + strconv.Itoa(te.Code/100) + "xx"
	case isTimeout(err):
		return "timeout"
	}
	return step + "_failed"
}
