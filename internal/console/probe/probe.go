package probe

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/logx"
)

// Stage names the step of the probe that failed.
type Stage string

// The probe stages, in the order they run.
const (
	StageDNS         Stage = "dns"
	StageConnect     Stage = "connect"
	StageTLS         Stage = "tls"
	StageCertExpired Stage = "certificate_expired"
	StageResponder   Stage = "responder"
)

// Limits: why the probe does not run, or runs only in part.
const (
	// LimitNoPublicURL: public_url is not set, so there is nothing to probe.
	LimitNoPublicURL = "no_public_url"
	// LimitCAFile: agent_probe.ca_file is set but cannot be read or holds no certificate.
	LimitCAFile = "ca_file_unusable"
	// LimitPlainHTTP: public_url is http:// (a lab): DNS, connect and the
	// responder are probed, TLS and the certificate cannot be.
	LimitPlainHTTP = "plain_http"
)

// Timeouts. StageTimeout bounds each network step; TotalTimeout the whole check.
const (
	StageTimeout    = 10 * time.Second
	TotalTimeout    = 30 * time.Second
	DefaultInterval = 3 * time.Minute
	// maxBody bounds the problem document the responder check reads.
	maxBody = 8 << 10
)

// Result is one probe run.
type Result struct {
	At    time.Time
	OK    bool
	Stage Stage  // the failing stage when !OK
	Class string // a path-free error class when !OK, for example "nxdomain", "refused", "untrusted", "hostname", "status_502"
	// NotAfter is the leaf certificate's expiry when the TLS handshake completed (zero otherwise).
	NotAfter time.Time
}

// Status is what the console knows about its agent endpoint.
type Status struct {
	// Runs is false when the probe cannot run at all; Limit says why.
	Runs bool
	// Limit is "", or one of the Limit* reasons (LimitPlainHTTP also with Runs).
	Limit string
	// Host is the probed host[:port] from public_url, for the banner copy.
	Host string
	// Interval is the probe period; Started is when Run began (zero before).
	Interval time.Duration
	Started  time.Time
	// Known is true once a run completed; Last is that run.
	Known bool
	Last  Result
	// Failures counts the consecutive failed runs; FailingSince is the time of
	// the first of them (zero while healthy).
	Failures     int
	FailingSince time.Time
	// LastOK is the time of the newest passing run.
	LastOK time.Time
}

// Settings configure a Prober (from config.Config: public_url and agent_probe).
type Settings struct {
	PublicURL      string
	CAFile         string
	ConnectAddress string
	Interval       time.Duration
}

// Prober runs the self-probe and keeps the last Status.
type Prober struct {
	// Clock is the console clock (certificate expiry and result times).
	Clock func() time.Time
	// Lookup resolves a host name (default net.DefaultResolver.LookupHost).
	Lookup func(ctx context.Context, host string) ([]string, error)
	// Dial opens a TCP connection (default a net.Dialer).
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// Roots verifies the chain; nil means the system roots.
	Roots *x509.CertPool
	// ConnectAddress, when set, is dialled instead of the resolved addresses.
	ConnectAddress string
	// StageTimeout bounds each network step; zero means the StageTimeout constant.
	StageTimeout time.Duration
	Log          *logx.Logger

	scheme, host, port string
	fixed              bool

	mu sync.Mutex
	st Status
}

func (p *Prober) stageTimeout() time.Duration {
	if p.StageTimeout > 0 {
		return p.StageTimeout
	}
	return StageTimeout
}

// New returns a Prober for the settings. It never fails: a probe that cannot
// run is a Prober whose Status says so (Runs false and a Limit), and whose Run
// returns at once.
func New(s Settings, clock func() time.Time, log *logx.Logger) *Prober {
	if log == nil {
		log = logx.Default()
	}
	if clock == nil {
		clock = time.Now
	}
	p := &Prober{Clock: clock, Log: log, ConnectAddress: s.ConnectAddress}
	p.st.Interval = s.Interval
	if p.st.Interval <= 0 {
		p.st.Interval = DefaultInterval
	}
	u, err := url.Parse(s.PublicURL)
	if s.PublicURL == "" || err != nil || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http") {
		p.st.Limit = LimitNoPublicURL
		return p
	}
	p.scheme, p.host, p.port = u.Scheme, u.Hostname(), u.Port()
	if p.port == "" {
		p.port = "443"
		if p.scheme == "http" {
			p.port = "80"
		}
	}
	p.st.Host = u.Host
	if s.CAFile != "" {
		pem, err := os.ReadFile(s.CAFile)
		pool := x509.NewCertPool()
		if err != nil || !pool.AppendCertsFromPEM(pem) {
			p.st.Limit = LimitCAFile
			return p
		}
		p.Roots = pool
	}
	p.st.Runs = true
	if p.scheme == "http" {
		p.st.Limit = LimitPlainHTTP
	}
	return p
}

// Fixed returns a Prober that always reports st and never probes (fixtures mode).
func Fixed(st Status) *Prober {
	return &Prober{fixed: true, st: st, Clock: time.Now, Log: logx.Default()}
}

// Status returns the current status.
func (p *Prober) Status() Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.st
}

// Run probes at once and then every Interval until ctx is cancelled. It returns
// immediately when the probe cannot run (the Status says why) and logs that
// once, as a warning.
func (p *Prober) Run(ctx context.Context) {
	if p.fixed {
		return
	}
	p.mu.Lock()
	st := p.st
	if st.Runs {
		p.st.Started = p.Clock().UTC()
	}
	p.mu.Unlock()
	if !st.Runs {
		p.Log.Warn("agent endpoint probe is not running", logx.Class(st.Limit))
		return
	}
	if st.Limit == LimitPlainHTTP {
		p.Log.Warn("agent endpoint probe runs without TLS: the public URL is plain HTTP", logx.Class(st.Limit))
	}
	t := time.NewTicker(st.Interval)
	defer t.Stop()
	for {
		p.Once(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Once runs one probe, records it and returns it. A run cut short because the
// caller's context ended (shutdown) is returned but not recorded: it says
// nothing about the endpoint.
func (p *Prober) Once(ctx context.Context) Result {
	cctx, cancel := context.WithTimeout(ctx, TotalTimeout)
	defer cancel()
	r := p.check(cctx)
	r.At = p.Clock().UTC()
	if ctx.Err() == nil {
		p.record(r)
	}
	return r
}

func (p *Prober) record(r Result) {
	p.mu.Lock()
	prev := p.st
	p.st.Known, p.st.Last = true, r
	if r.OK {
		p.st.Failures, p.st.FailingSince, p.st.LastOK = 0, time.Time{}, r.At
	} else {
		if p.st.Failures == 0 {
			p.st.FailingSince = r.At
		}
		p.st.Failures++
	}
	p.mu.Unlock()
	// Log a change only: the first result, healthy <-> failing, or another stage.
	switch {
	case r.OK && (!prev.Known || !prev.Last.OK):
		p.Log.Info("agent endpoint probe passing")
	case !r.OK && (!prev.Known || prev.Last.OK || prev.Last.Stage != r.Stage || prev.Last.Class != r.Class):
		p.Log.Warn("agent endpoint probe failing", logx.Str("stage", string(r.Stage)), logx.Class(r.Class))
	}
}

func fail(st Stage, class string, notAfter time.Time) Result {
	return Result{Stage: st, Class: class, NotAfter: notAfter}
}

// check runs the stages in order and returns the first failure, or OK.
func (p *Prober) check(ctx context.Context) Result {
	if p.scheme == "" {
		return fail(StageDNS, "not_configured", time.Time{})
	}
	// 1. dns
	addrs := []string{p.host}
	if net.ParseIP(p.host) == nil {
		lookup := p.Lookup
		if lookup == nil {
			lookup = net.DefaultResolver.LookupHost
		}
		dctx, cancel := context.WithTimeout(ctx, p.stageTimeout())
		got, err := lookup(dctx, p.host)
		cancel()
		if err != nil || len(got) == 0 {
			return fail(StageDNS, dnsClass(err), time.Time{})
		}
		addrs = got
	}
	// 2. connect
	targets := make([]string, 0, len(addrs))
	if p.ConnectAddress != "" {
		targets = append(targets, p.ConnectAddress)
	} else {
		for _, a := range addrs {
			targets = append(targets, net.JoinHostPort(a, p.port))
		}
	}
	dial := p.Dial
	if dial == nil {
		d := &net.Dialer{}
		dial = d.DialContext
	}
	var conn net.Conn
	var derr error
	for _, t := range targets {
		cctx, cancel := context.WithTimeout(ctx, p.stageTimeout())
		conn, derr = dial(cctx, "tcp", t)
		cancel()
		if derr == nil {
			break
		}
	}
	if conn == nil {
		return fail(StageConnect, netClass(derr), time.Time{})
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	rw := io.ReadWriter(conn)

	// 3. tls and 4. certificate: the standard verification (chain and host
	// name) against the configured roots and the console clock. An expired
	// leaf is told apart from an untrusted one.
	var notAfter time.Time
	if p.scheme == "https" {
		tc := tls.Client(conn, &tls.Config{ServerName: p.host, RootCAs: p.Roots, Time: p.Clock, MinVersion: tls.VersionTLS12,
			NextProtos: []string{"http/1.1"}})
		hctx, cancel := context.WithTimeout(ctx, p.stageTimeout())
		err := tc.HandshakeContext(hctx)
		cancel()
		if err != nil {
			var ve *tls.CertificateVerificationError
			if errors.As(err, &ve) {
				if len(ve.UnverifiedCertificates) > 0 {
					leaf := ve.UnverifiedCertificates[0]
					notAfter = leaf.NotAfter.UTC()
					if p.Clock().After(leaf.NotAfter) {
						return fail(StageCertExpired, "expired", notAfter)
					}
				}
				return fail(StageTLS, verifyClass(ve.Err), notAfter)
			}
			return fail(StageTLS, handshakeClass(err), time.Time{})
		}
		if cs := tc.ConnectionState(); len(cs.PeerCertificates) > 0 {
			notAfter = cs.PeerCertificates[0].NotAfter.UTC()
		}
		rw = tc
	}

	// 5. responder
	if r := p.responder(ctx, rw); r != "" {
		return fail(StageResponder, r, notAfter)
	}
	return Result{OK: true, NotAfter: notAfter}
}

// responder sends the unauthenticated request and checks that the answer is
// this console's agent API. It returns "" or a failure class.
func (p *Prober) responder(ctx context.Context, rw io.ReadWriter) string {
	hostport := p.host
	if (p.scheme == "https" && p.port != "443") || (p.scheme == "http" && p.port != "80") {
		hostport = net.JoinHostPort(p.host, p.port)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.scheme+"://"+hostport+api.RoutePolicy, nil)
	if err != nil {
		return "request"
	}
	req.Close = true
	req.Header.Set("User-Agent", "cryo-console-self-probe")
	if err := req.Write(rw); err != nil {
		return "write"
	}
	resp, err := http.ReadResponse(bufio.NewReader(rw), req)
	if err != nil {
		return "not_http"
	}
	defer resp.Body.Close()
	want := map[int]string{http.StatusUnauthorized: api.CodeUnauthorized, http.StatusTooManyRequests: api.CodeRateLimited}
	code, ok := want[resp.StatusCode]
	if !ok {
		return "status_" + strconv.Itoa(resp.StatusCode)
	}
	if mt, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type")); err != nil || mt != api.ContentTypeProblem {
		return "content_type"
	}
	if _, err := api.ParseServerTime(resp.Header.Get(api.HeaderServerTime)); err != nil {
		return "server_time"
	}
	var e api.Error
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&e); err != nil {
		return "body"
	}
	if e.Code != code || e.Status != resp.StatusCode || e.Type != api.ErrorTypePrefix+code {
		return "body"
	}
	return ""
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout())
}

func dnsClass(err error) string {
	var de *net.DNSError
	switch {
	case err == nil:
		return "no_address"
	case isTimeout(err):
		return "timeout"
	case errors.As(err, &de) && de.IsNotFound:
		return "nxdomain"
	default:
		return "lookup_failed"
	}
}

func netClass(err error) string {
	switch {
	case err == nil:
		return "no_address"
	case isTimeout(err):
		return "timeout"
	case refused(err):
		return "refused"
	default:
		return "unreachable"
	}
}

func handshakeClass(err error) string {
	if isTimeout(err) {
		return "timeout"
	}
	return "handshake"
}

func verifyClass(err error) string {
	var he x509.HostnameError
	var ua x509.UnknownAuthorityError
	var ci x509.CertificateInvalidError
	switch {
	case errors.As(err, &he):
		return "hostname"
	case errors.As(err, &ua):
		return "untrusted"
	case errors.As(err, &ci) && ci.Reason == x509.Expired:
		return "chain_expired"
	default:
		return "invalid"
	}
}
