package mail

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"fmt"
	"io"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/logx"
)

// testHost is the name every test certificate carries and every Settings uses.
const testHost = "mail.example.test"

// pki is a throw-away CA and the server certificates the tests need.
type pki struct {
	pool      *x509.CertPool
	good      tls.Certificate // valid for testHost, issued by the test CA
	wrongName tls.Certificate // issued by the test CA for another name
	untrusted tls.Certificate // valid name, issued by a CA the client does not trust
	expired   tls.Certificate // valid name, test CA, expired yesterday
}

var (
	pkiOnce sync.Once
	pkiVal  *pki
)

func testPKI(t *testing.T) *pki {
	t.Helper()
	pkiOnce.Do(func() { pkiVal = newPKI(t) })
	return pkiVal
}

func newCA(t *testing.T, cn string) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-48 * time.Hour),
		NotAfter:              time.Now().Add(48 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return ca, key
}

func newLeaf(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, name string, notAfter time.Time) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: name},
		DNSNames:     []string{name},
		NotBefore:    time.Now().Add(-47 * time.Hour),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func newPKI(t *testing.T) *pki {
	ca, caKey := newCA(t, "Test CA")
	other, otherKey := newCA(t, "Other CA")
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return &pki{
		pool:      pool,
		good:      newLeaf(t, ca, caKey, testHost, time.Now().Add(24*time.Hour)),
		wrongName: newLeaf(t, ca, caKey, "other.example.test", time.Now().Add(24*time.Hour)),
		untrusted: newLeaf(t, other, otherKey, testHost, time.Now().Add(24*time.Hour)),
		expired:   newLeaf(t, ca, caKey, testHost, time.Now().Add(-time.Hour)),
	}
}

// fakeCfg scripts the fake relay. The zero value is a relay that greets, answers
// EHLO with 8BITMIME only, accepts everything and offers no TLS.
type fakeCfg struct {
	cert     *tls.Certificate // server certificate; needed by implicit and startTLS
	implicit bool             // TLS from the first byte
	startTLS bool             // offer and accept STARTTLS
	// authMech is the AUTH extension line ("PLAIN LOGIN"); empty offers none.
	authMech      string
	authBeforeTLS bool // offer AUTH on a clear connection too (a hostile or careless relay)
	user, pass    string

	greeting   string            // replaces "220 ..."
	noGreeting bool              // accept the connection and say nothing
	reply      map[string]string // verb (EHLO MAIL RCPT DATA DATAEND STARTTLS AUTH) -> reply text
	rcptReply  map[string]string // recipient address -> reply text
	hang       map[string]bool   // verb -> read the command and never answer
	dropOnQuit bool              // close on QUIT without a reply
}

// capture is one message the relay accepted.
type capture struct {
	from  string
	rcpts []string
	data  []byte
	tls   bool
}

// fakeSrv is an in-process SMTP relay. It records what happens so a test can
// say what the client did and did not do.
type fakeSrv struct {
	t   *testing.T
	ln  net.Listener
	cfg fakeCfg

	mu       sync.Mutex
	conns    int
	raw      bytes.Buffer // every byte the client wrote on a raw socket (clear text before TLS)
	cmds     []string     // every command verb received, in order
	mails    []capture
	auths    []string // "user:pass:tls" of every credential offered
	authClr  bool     // an AUTH command arrived on a clear connection
	wantPass string
}

func startFake(t *testing.T, cfg fakeCfg) *fakeSrv {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeSrv{t: t, ln: ln, cfg: cfg}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s.mu.Lock()
			s.conns++
			s.mu.Unlock()
			go s.serve(c)
		}
	}()
	return s
}

// settings returns Settings that dial this relay under testHost.
func (s *fakeSrv) settings(security string) Settings {
	p := testPKI(s.t)
	return Settings{
		Host: testHost, Port: 587, Security: security, From: "console@example.com",
		Roots: p.pool, Log: logx.New(io.Discard, logx.LevelDebug),
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, s.ln.Addr().String())
		},
		Now: func() time.Time { return time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC) },
	}
}

func (s *fakeSrv) connCount() int { s.mu.Lock(); defer s.mu.Unlock(); return s.conns }
func (s *fakeSrv) rawBytes() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.raw.Bytes()...)
}
func (s *fakeSrv) sawCmd(v string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.cmds {
		if c == v {
			return true
		}
	}
	return false
}
func (s *fakeSrv) captured() []capture {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]capture(nil), s.mails...)
}
func (s *fakeSrv) authList() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.auths...)
}

// recConn records what the client writes on the raw socket.
type recConn struct {
	net.Conn
	s *fakeSrv
}

func (r *recConn) Read(p []byte) (int, error) {
	n, err := r.Conn.Read(p)
	if n > 0 {
		r.s.mu.Lock()
		r.s.raw.Write(p[:n])
		r.s.mu.Unlock()
	}
	return n, err
}

func (s *fakeSrv) tlsConf() *tls.Config {
	return &tls.Config{Certificates: []tls.Certificate{*s.cfg.cert}, MinVersion: tls.VersionTLS12}
}

func (s *fakeSrv) serve(raw net.Conn) {
	defer raw.Close()
	var conn net.Conn = &recConn{Conn: raw, s: s}
	tlsOn := false
	if s.cfg.implicit {
		tc := tls.Server(conn, s.tlsConf())
		if err := tc.Handshake(); err != nil {
			return
		}
		conn, tlsOn = tc, true
	}
	br := bufio.NewReader(conn)
	say := func(text string) {
		fmt.Fprint(conn, strings.ReplaceAll(text, "\n", "\r\n")+"\r\n")
	}
	if s.cfg.noGreeting {
		io.Copy(io.Discard, conn)
		return
	}
	if s.cfg.hang["GREETING"] {
		io.Copy(io.Discard, conn)
		return
	}
	g := s.cfg.greeting
	if g == "" {
		g = "220 " + testHost + " ESMTP ready"
	}
	say(g)
	if strings.HasPrefix(g, "4") || strings.HasPrefix(g, "5") {
		return
	}
	var from string
	var rcpts []string
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		verb := strings.ToUpper(strings.Fields(line + " x")[0])
		s.mu.Lock()
		s.cmds = append(s.cmds, verb)
		s.mu.Unlock()
		if s.cfg.hang[verb] {
			io.Copy(io.Discard, conn)
			return
		}
		switch verb {
		case "EHLO", "HELO":
			if r, ok := s.cfg.reply["EHLO"]; ok {
				say(r)
				continue
			}
			lines := []string{testHost}
			if s.cfg.startTLS && !tlsOn {
				lines = append(lines, "STARTTLS")
			}
			if s.cfg.authMech != "" && (tlsOn || s.cfg.authBeforeTLS) {
				lines = append(lines, "AUTH "+s.cfg.authMech)
			}
			lines = append(lines, "8BITMIME")
			var b strings.Builder
			for i, l := range lines {
				sep := "-"
				if i == len(lines)-1 {
					sep = " "
				}
				b.WriteString("250" + sep + l + "\r\n")
			}
			fmt.Fprint(conn, b.String())
		case "STARTTLS":
			if !s.cfg.startTLS || tlsOn {
				say("502 5.5.1 not offered")
				continue
			}
			if r, ok := s.cfg.reply["STARTTLS"]; ok {
				say(r)
				continue
			}
			say("220 2.0.0 ready to start TLS")
			tc := tls.Server(conn, s.tlsConf())
			if err := tc.Handshake(); err != nil {
				return
			}
			conn, tlsOn = tc, true
			br = bufio.NewReader(conn)
		case "AUTH":
			if !tlsOn {
				s.mu.Lock()
				s.authClr = true
				s.mu.Unlock()
			}
			if r, ok := s.cfg.reply["AUTH"]; ok {
				say(r)
				continue
			}
			user, pass, ok := s.readAuth(line, br, say)
			s.mu.Lock()
			s.auths = append(s.auths, fmt.Sprintf("%s:%s:%v", user, pass, tlsOn))
			s.mu.Unlock()
			if ok && user == s.cfg.user && pass == s.currentPass() {
				say("235 2.7.0 authenticated")
			} else {
				say("535 5.7.8 bad credentials LEAK-REPLY-TEXT")
			}
		case "MAIL":
			if r, ok := s.cfg.reply["MAIL"]; ok {
				say(r)
				continue
			}
			from = between(line)
			rcpts = nil
			say("250 2.1.0 ok")
		case "RCPT":
			a := between(line)
			if r, ok := s.cfg.reply["RCPT"]; ok {
				say(r)
				continue
			}
			if r, ok := s.cfg.rcptReply[a]; ok {
				say(r)
				continue
			}
			rcpts = append(rcpts, a)
			say("250 2.1.5 ok")
		case "DATA":
			if r, ok := s.cfg.reply["DATA"]; ok {
				say(r)
				continue
			}
			say("354 go ahead")
			var data bytes.Buffer
			for {
				l, err := br.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				l = strings.TrimPrefix(l, ".") // undo dot-stuffing (a leading ".." is "." on the wire)
				data.WriteString(l)
			}
			if r, ok := s.cfg.reply["DATAEND"]; ok {
				say(r)
				continue
			}
			s.mu.Lock()
			s.mails = append(s.mails, capture{from: from, rcpts: append([]string(nil), rcpts...), data: data.Bytes(), tls: tlsOn})
			s.mu.Unlock()
			say("250 2.0.0 queued")
		case "RSET":
			say("250 ok")
		case "NOOP":
			say("250 ok")
		case "QUIT":
			if s.cfg.dropOnQuit {
				return
			}
			say("221 bye")
			return
		default:
			say("502 5.5.2 unknown command")
		}
	}
}

// currentPass is the password the relay accepts; a test can change it between sends.
func (s *fakeSrv) currentPass() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.wantPass != "" {
		return s.wantPass
	}
	return s.cfg.pass
}

func (s *fakeSrv) setPass(p string) { s.mu.Lock(); s.wantPass = p; s.mu.Unlock() }

// readAuth runs the AUTH PLAIN or AUTH LOGIN exchange and returns what the client sent.
func (s *fakeSrv) readAuth(line string, br *bufio.Reader, say func(string)) (user, pass string, ok bool) {
	f := strings.Fields(line)
	if len(f) < 2 {
		return "", "", false
	}
	b64 := func(x string) string { d, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(x)); return string(d) }
	switch strings.ToUpper(f[1]) {
	case "PLAIN":
		var blob string
		if len(f) >= 3 {
			blob = b64(f[2])
		} else {
			say("334 ")
			l, _ := br.ReadString('\n')
			blob = b64(l)
		}
		parts := strings.Split(blob, "\x00")
		if len(parts) != 3 {
			return "", "", false
		}
		return parts[1], parts[2], true
	case "LOGIN":
		say("334 " + base64.StdEncoding.EncodeToString([]byte("Username:")))
		u, _ := br.ReadString('\n')
		say("334 " + base64.StdEncoding.EncodeToString([]byte("Password:")))
		p, _ := br.ReadString('\n')
		return b64(u), b64(p), true
	}
	return "", "", false
}

// between returns what is inside <...> of a MAIL FROM or RCPT TO line.
func between(line string) string {
	i, j := strings.Index(line, "<"), strings.LastIndex(line, ">")
	if i < 0 || j < i {
		return ""
	}
	return line[i+1 : j]
}
