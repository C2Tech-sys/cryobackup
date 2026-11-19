package mail

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/c2tech-sys/cryobackup/internal/console/config"
	"github.com/c2tech-sys/cryobackup/internal/logx"
)

// Sender sends one message. Every error it returns is a *SendError.
type Sender interface {
	Send(ctx context.Context, m Message) error
}

// Defaults of the three clocks of a send. The caller's context shortens all of
// them (its deadline is the outer limit), never lengthens them.
const (
	DefaultDialTimeout     = 10 * time.Second
	DefaultGreetingTimeout = 20 * time.Second
	DefaultTimeout         = 2 * time.Minute
)

// Settings configure an SMTPSender. Build them with SettingsFromConfig; the
// remaining fields exist for tests and the console's wiring.
type Settings struct {
	Host string
	Port int
	// Security is "starttls", "tls" (implicit TLS) or "plain".
	Security string
	// AllowPlaintext is smtp.allow_plaintext: the only way Security "plain" is
	// accepted. Credentials are refused with plain whatever its value.
	AllowPlaintext bool
	Username       string
	// PasswordFile is read at every send (ReadPasswordFile); with an empty
	// Username it is not read at all.
	PasswordFile string
	// From is the default sender for a Message with no From.
	From string
	// HeloName is the EHLO name; empty means "localhost".
	HeloName string
	// Roots verifies the relay's certificate; nil means the system roots.
	Roots *x509.CertPool
	// DialTimeout bounds the TCP connect (DefaultDialTimeout), GreetingTimeout
	// the time from connect to the relay's greeting, TLS handshake included
	// (DefaultGreetingTimeout), and Timeout the whole send (DefaultTimeout).
	DialTimeout     time.Duration
	GreetingTimeout time.Duration
	Timeout         time.Duration
	// Dial replaces the TCP dialer (tests).
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// Now supplies the Date header; nil means time.Now.
	Now func() time.Time
	// Log receives one line per send (kind, stage, class, counts); nil means
	// logx.Default().
	Log *logx.Logger
}

// SettingsFromConfig returns the Settings of the smtp block of config.json.
func SettingsFromConfig(c config.SMTP) Settings {
	return Settings{
		Host:           c.Host,
		Port:           c.Port,
		Security:       c.Security,
		AllowPlaintext: c.AllowPlaintext,
		Username:       c.Username,
		PasswordFile:   c.PasswordFile,
		From:           c.From,
	}
}

// Configured reports whether a relay is named at all (host and port).
func (s Settings) Configured() bool { return s.Host != "" && s.Port > 0 }

// Validate reports why these settings cannot send, as a *SendError at
// StageConfig, or nil. It does not touch the network or the password file.
func (s Settings) Validate() error {
	if s.Host == "" {
		return newErr(StageConfig, ClassNotConfigured, false)
	}
	if s.Port < 1 || s.Port > 65535 {
		return newErr(StageConfig, ClassConfigInvalid, false)
	}
	switch s.Security {
	case "starttls", "tls":
	case "plain":
		if !s.AllowPlaintext || s.Username != "" {
			return newErr(StageConfig, ClassPlaintextRefused, false)
		}
	default:
		return newErr(StageConfig, ClassConfigInvalid, false)
	}
	for _, v := range []string{s.Host, s.HeloName, s.From} {
		if headerSafe("setting", v) != nil {
			return newErr(StageConfig, ClassConfigInvalid, false)
		}
	}
	// The default sender becomes the envelope sender of every message without
	// its own From: it must be a plain address like any other (checkEnvelopeAddr).
	if s.From != "" {
		if f, err := parseAddr(s.From); err != nil || utf8.RuneCountInString(f.Name) > maxDisplayName {
			return newErr(StageConfig, ClassConfigInvalid, false)
		}
	}
	return nil
}

// SMTPSender sends through the relay of its Settings, one connection per
// message. It holds no state between sends and is safe for concurrent use.
type SMTPSender struct{ set Settings }

// NewSMTPSender returns a sender for s. It cannot fail: a settings problem is
// reported by every Send (and by Settings.Validate), so a console with no relay
// still starts and says so where mail would have gone.
func NewSMTPSender(s Settings) *SMTPSender { return &SMTPSender{set: s} }

// Settings returns the sender's settings.
func (s *SMTPSender) Settings() Settings { return s.set }

func (s *SMTPSender) log() *logx.Logger {
	if s.set.Log != nil {
		return s.set.Log
	}
	return logx.Default()
}

// Send delivers m or returns a *SendError. A send is all or nothing: if any
// recipient is refused, nothing is transmitted. The relay's replies, the host,
// the addresses and the password are never in the error or in the log line.
func (s *SMTPSender) Send(ctx context.Context, m Message) error {
	kind := string(m.Kind)
	if kind == "" || !kindOK(kind) {
		kind = "other"
	}
	err := s.send(ctx, m)
	if err != nil {
		var se *SendError
		if errors.As(err, &se) {
			s.log().Warn("mail send failed", logx.Kind(kind), logx.Str("stage", string(se.Stage)),
				logx.Class(string(se.Class)), logx.Bool("temporary", se.Temporary))
		}
		return err
	}
	s.log().Info("mail sent", logx.Kind(kind), logx.Count("recipients", len(m.To)))
	return nil
}

func (s *SMTPSender) send(ctx context.Context, m Message) error {
	if err := s.set.Validate(); err != nil {
		return err
	}
	if m.From == "" {
		m.From = s.set.From
	}
	a, err := m.parse()
	if err != nil {
		return newErr(StageConfig, ClassMessageInvalid, false)
	}
	now := time.Now
	if s.set.Now != nil {
		now = s.set.Now
	}
	data, err := m.Build(now())
	if err != nil {
		return newErr(StageConfig, ClassMessageInvalid, false)
	}
	var password string
	if s.set.Username != "" {
		if password, err = ReadPasswordFile(s.set.PasswordFile); err != nil {
			return err
		}
	}
	return s.deliver(ctx, a, data, password)
}

// tlsConfig is the client side of every TLS handshake: 1.2 or later, the relay's
// certificate verified against the configured host name.
func (s *SMTPSender) tlsConfig() *tls.Config {
	return &tls.Config{ServerName: s.set.Host, RootCAs: s.set.Roots, MinVersion: tls.VersionTLS12}
}

func orDefault(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}

func (s *SMTPSender) deliver(ctx context.Context, a addrs, data []byte, password string) error {
	set := s.set
	overall := time.Now().Add(orDefault(set.Timeout, DefaultTimeout))
	if dl, ok := ctx.Deadline(); ok && dl.Before(overall) {
		overall = dl
	}
	ctx, cancel := context.WithDeadline(ctx, overall)
	defer cancel()

	dial := set.Dial
	if dial == nil {
		d := &net.Dialer{}
		dial = d.DialContext
	}
	dctx, dcancel := context.WithTimeout(ctx, orDefault(set.DialTimeout, DefaultDialTimeout))
	conn, err := dial(dctx, "tcp", net.JoinHostPort(set.Host, strconv.Itoa(set.Port)))
	dcancel()
	if err != nil {
		return netFailure(ctx, StageConnect, err)
	}
	defer conn.Close()
	// A cancelled or expired context closes the connection under whatever read
	// or write is blocked; netFailure then reads the cause from the context.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

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
		if err := tc.HandshakeContext(ctx); err != nil {
			return tlsFailure(ctx, err)
		}
		nc, encrypted = tc, true
	}
	c, err := smtp.NewClient(nc, set.Host)
	if err != nil {
		return replyFailure(ctx, StageConnect, ClassProtocol, err)
	}
	defer c.Close()
	_ = conn.SetDeadline(overall)

	helo := set.HeloName
	if helo == "" {
		helo = "localhost"
	}
	if err := c.Hello(helo); err != nil {
		return replyFailure(ctx, StageConnect, ClassProtocol, err)
	}
	if set.Security == "starttls" {
		offered, _ := c.Extension("STARTTLS")
		if !offered {
			ext, err := ehloExtensions(c, helo)
			if err != nil {
				return replyFailure(ctx, StageConnect, ClassProtocol, err)
			}
			_, offered = ext["STARTTLS"]
		}
		if !offered {
			return newErr(StageTLS, ClassStartTLSMissing, false)
		}
		if err := c.StartTLS(tcfg); err != nil {
			return tlsFailure(ctx, err)
		}
		encrypted = true
	}

	if set.Username != "" {
		// Defence in depth: Validate already refuses plain with a user name, and
		// nothing here authenticates before the connection is encrypted.
		if !encrypted {
			return newErr(StageAuth, ClassPlaintextRefused, false)
		}
		if err := authenticate(ctx, c, helo, set.Host, set.Username, password); err != nil {
			return err
		}
	}

	// The envelope is checked again here, one address at a time, immediately
	// before it is written: whatever built addrs, nothing but a plain address
	// goes between the angle brackets.
	if checkEnvelopeAddr(a.from.Address) != nil {
		return newErr(StageConfig, ClassMessageInvalid, false)
	}
	if err := c.Mail(a.from.Address); err != nil {
		return replyFailure(ctx, StageSender, ClassSenderRejected, err)
	}
	for _, r := range a.to {
		if checkEnvelopeAddr(r.Address) != nil {
			closing(conn, overall, c.Reset) // all or nothing, as for a refused recipient
			return newErr(StageConfig, ClassMessageInvalid, false)
		}
		if err := c.Rcpt(r.Address); err != nil {
			closing(conn, overall, c.Reset) // all or nothing: no DATA after a refused recipient
			return replyFailure(ctx, StageRecipient, ClassRecipientRejected, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return replyFailure(ctx, StageData, ClassMessageRejected, err)
	}
	if _, err := w.Write(data); err != nil {
		return netFailure(ctx, StageData, err)
	}
	if err := w.Close(); err != nil {
		return replyFailure(ctx, StageData, ClassMessageRejected, err)
	}
	// The relay has accepted the message. A QUIT that fails or hangs now must not
	// turn a delivered message into a failure (a retry would send it twice) nor
	// hold Send until the whole deadline: it gets closeTimeout and its outcome is
	// dropped.
	closing(conn, overall, c.Quit)
	return nil
}

// closing runs a closing command (QUIT, RSET) under its own short deadline,
// closeTimeout, never later than the deadline of the whole send. Its outcome is
// ignored: by now the result of the send is already decided.
func closing(conn net.Conn, overall time.Time, cmd func() error) {
	dl := time.Now().Add(closeTimeout)
	if dl.After(overall) {
		dl = overall
	}
	_ = conn.SetDeadline(dl)
	_ = cmd()
}

// ehloExtensions asks the relay for its extensions once more and returns them
// with upper-case keywords. net/smtp keeps the keywords as the relay wrote them
// and looks them up in upper case, so a relay that writes "250-auth plain"
// (keywords are case-insensitive, RFC 5321 2.4) would look as if it offered
// neither AUTH nor STARTTLS. Only used when that lookup found nothing.
func ehloExtensions(c *smtp.Client, helo string) (map[string]string, error) {
	_, msg, err := textCmd(c, 250, "EHLO %s", helo)
	if err != nil {
		return nil, err
	}
	ext := map[string]string{}
	lines := strings.Split(msg, "\n")
	for _, l := range lines[1:] { // the first line is the greeting
		k, v, _ := strings.Cut(l, " ")
		ext[strings.ToUpper(k)] = v
	}
	return ext, nil
}

// textCmd sends one command and reads its reply, as net/smtp's own (unexported)
// cmd does.
func textCmd(c *smtp.Client, expect int, format string, args ...any) (int, string, error) {
	id, err := c.Text.Cmd(format, args...)
	if err != nil {
		return 0, "", err
	}
	c.Text.StartResponse(id)
	defer c.Text.EndResponse(id)
	return c.Text.ReadResponse(expect)
}

// authenticate signs in with PLAIN, or LOGIN when that is all the relay offers.
// Only called on an encrypted connection. The error is a *SendError.
func authenticate(ctx context.Context, c *smtp.Client, helo, host, user, pass string) error {
	offered, params := c.Extension("AUTH")
	native := offered // net/smtp saw the AUTH keyword, so its own Auth can run
	if !offered {
		ext, err := ehloExtensions(c, helo)
		if err != nil {
			return replyFailure(ctx, StageConnect, ClassProtocol, err)
		}
		params, offered = ext["AUTH"]
	}
	auth := pickAuth(offered, params, host, user, pass)
	if auth == nil {
		return newErr(StageAuth, ClassAuthUnsupported, false)
	}
	var err error
	if native {
		err = c.Auth(auth)
	} else {
		err = authExchange(c, host, auth, strings.Fields(params))
	}
	if err != nil {
		if errors.Is(err, errLoginChallenge) {
			return newErr(StageAuth, ClassProtocol, false)
		}
		return replyFailure(ctx, StageAuth, ClassAuthRejected, err)
	}
	return nil
}

// authExchange is net/smtp's Client.Auth for the one case it cannot serve: it
// refuses to run unless it saw the AUTH keyword in upper case. The exchange is
// the same (RFC 4954), on an encrypted connection.
func authExchange(c *smtp.Client, host string, a smtp.Auth, mechs []string) error {
	enc := base64.StdEncoding
	mech, resp, err := a.Start(&smtp.ServerInfo{Name: host, TLS: true, Auth: mechs})
	if err != nil {
		return err
	}
	line := strings.TrimSpace(fmt.Sprintf("AUTH %s %s", mech, enc.EncodeToString(resp)))
	code, msg64, err := textCmd(c, 0, "%s", line)
	for err == nil {
		var msg []byte
		switch code {
		case 334:
			msg, err = enc.DecodeString(msg64)
		case 235:
			msg = []byte(msg64) // the last message is not a challenge, so not base64
		default:
			err = &textproto.Error{Code: code, Msg: msg64}
		}
		if err != nil {
			return err
		}
		if resp, err = a.Next(msg, code == 334); err != nil {
			_, _, _ = textCmd(c, 501, "*") // abort the exchange
			return err
		}
		if resp == nil {
			return nil
		}
		code, msg64, err = textCmd(c, 0, "%s", enc.EncodeToString(resp))
	}
	return err
}

// pickAuth chooses PLAIN, or LOGIN when that is all the relay offers (params is
// the mechanism list of its AUTH line, in any case); nil when it offers neither.
func pickAuth(offered bool, params, host, user, pass string) smtp.Auth {
	if !offered {
		return nil
	}
	var plain, login bool
	for _, m := range strings.Fields(strings.ToUpper(params)) {
		switch m {
		case "PLAIN":
			plain = true
		case "LOGIN":
			login = true
		}
	}
	switch {
	case plain:
		return smtp.PlainAuth("", user, pass, host)
	case login:
		return loginAuth{user: user, pass: pass}
	}
	return nil
}

// errLoginChallenge is a LOGIN challenge that is neither "Username:" nor
// "Password:".
var errLoginChallenge = errors.New("mail: unexpected LOGIN challenge")

// loginAuth is the AUTH LOGIN mechanism, which net/smtp does not provide. It is
// used only when a relay offers LOGIN and not PLAIN, and never in clear.
type loginAuth struct{ user, pass string }

func (a loginAuth) Start(si *smtp.ServerInfo) (string, []byte, error) {
	if !si.TLS {
		return "", nil, errors.New("mail: LOGIN refused on an unencrypted connection")
	}
	return "LOGIN", nil, nil
}

func (a loginAuth) Next(challenge []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	c := strings.ToLower(string(challenge))
	switch {
	case strings.Contains(c, "user"):
		return []byte(a.user), nil
	case strings.Contains(c, "pass"):
		return []byte(a.pass), nil
	}
	return nil, errLoginChallenge
}

// closingTimeout bounds the closing QUIT and RSET (see closeTimeout).
const closingTimeout = 5 * time.Second

// closeTimeout is closingTimeout; a variable only so a test can shorten it.
var closeTimeout = closingTimeout
