package probe

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// The gateway and SMTP probes of the health-prober (architecture §4.6 C01,
// C03, the gateway half of C05; §5.7 health-prober). Like the agent-endpoint
// self-probe they return a path-free class, never an error text, and every
// network step is bounded.

// GatewayProbePath is the request of the gateway probe: the probe login's own
// repository config. A 200 (the repository exists) or a 404 (it does not)
// proves the listener, TLS, authentication and the gateway-to-storage hop
// (architecture §7.2 "probe login").
const GatewayProbePath = "/probe/config"

// ProbeTimeout bounds one gateway or SMTP probe end to end: well inside the
// 60 s health-prober period, so a hung peer never overlaps the next run.
const ProbeTimeout = 20 * time.Second

// Outcome is one gateway or SMTP probe run.
type Outcome struct {
	OK bool
	// Class is a path-free failure class when !OK ("timeout", "refused", "unauthorized", "status_502", ...).
	Class string
	// NotAfter is the leaf certificate's expiry when a TLS handshake completed (zero otherwise).
	NotAfter time.Time
}

// GatewaySettings configure the gateway probe.
type GatewaySettings struct {
	// URL is gateway_url, the address agents use for the restic REST gateway.
	URL string
	// Roots verifies the gateway certificate; nil means the system roots.
	Roots *x509.CertPool
	// Timeout bounds the probe; zero means ProbeTimeout.
	Timeout time.Duration
	// Dial replaces the TCP dialer (tests).
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// Clock is the console clock for certificate verification; nil means time.Now.
	Clock func() time.Time
}

// CheckGateway is one gateway probe: TLS plus an authenticated GET of
// GatewayProbePath as the reserved probe login. 200 and 404 are healthy; 401,
// 403, a TLS error, a timeout and a 5xx are failures (§4.6 C01). The password
// is the derived probe password (gateway.ProbePassword); it is sent only to the
// configured gateway and never logged.
func CheckGateway(ctx context.Context, s GatewaySettings, username, password string) Outcome {
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = ProbeTimeout
	}
	u, err := url.Parse(s.URL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return Outcome{Class: "bad_url"}
	}
	target := u.JoinPath(GatewayProbePath)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	clock := s.Clock
	if clock == nil {
		clock = time.Now
	}
	dial := s.Dial
	if dial == nil {
		d := &net.Dialer{Timeout: StageTimeout}
		dial = d.DialContext
	}
	tr := &http.Transport{
		Proxy:               nil, // the console talks to its own gateway directly
		DialContext:         dial,
		TLSClientConfig:     &tls.Config{RootCAs: s.Roots, MinVersion: tls.VersionTLS12, Time: clock},
		TLSHandshakeTimeout: StageTimeout,
		DisableKeepAlives:   true,
		ForceAttemptHTTP2:   false,
	}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return Outcome{Class: "request"}
	}
	req.SetBasicAuth(username, password)
	req.Header.Set("User-Agent", "cryo-console-health-prober")
	resp, err := client.Do(req)
	if err != nil {
		return Outcome{Class: transportClass(err, clock()), NotAfter: unverifiedNotAfter(err)}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
	var notAfter time.Time
	if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		notAfter = resp.TLS.PeerCertificates[0].NotAfter.UTC()
	}
	switch c := resp.StatusCode; {
	case c == http.StatusOK || c == http.StatusNotFound:
		return Outcome{OK: true, NotAfter: notAfter}
	case c == http.StatusUnauthorized || c == http.StatusForbidden:
		return Outcome{Class: "unauthorized", NotAfter: notAfter}
	default:
		return Outcome{Class: "status_" + strconv.Itoa(c), NotAfter: notAfter}
	}
}

// transportClass names a failed HTTP round trip without its text.
func transportClass(err error, now time.Time) string {
	var ve *tls.CertificateVerificationError
	var de *net.DNSError
	var ue x509.UnknownAuthorityError
	var he x509.HostnameError
	var ci x509.CertificateInvalidError
	switch {
	case errors.As(err, &ve):
		if len(ve.UnverifiedCertificates) > 0 && now.After(ve.UnverifiedCertificates[0].NotAfter) {
			return "expired"
		}
		return verifyClass(ve.Err)
	case errors.As(err, &ue):
		return "untrusted"
	case errors.As(err, &he):
		return "hostname"
	case errors.As(err, &ci):
		if ci.Reason == x509.Expired {
			return "expired"
		}
		return "invalid"
	case isTimeout(err):
		return "timeout"
	case errors.As(err, &de):
		if de.IsNotFound {
			return "nxdomain"
		}
		return "lookup_failed"
	case refused(err):
		return "refused"
	}
	var re tls.RecordHeaderError
	if errors.As(err, &re) {
		return "not_tls"
	}
	return "unreachable"
}

// unverifiedNotAfter is the leaf's expiry from a failed verification, so an
// expired or untrusted gateway certificate still reports when it ran out.
func unverifiedNotAfter(err error) time.Time {
	var ve *tls.CertificateVerificationError
	if errors.As(err, &ve) && len(ve.UnverifiedCertificates) > 0 {
		return ve.UnverifiedCertificates[0].NotAfter.UTC()
	}
	return time.Time{}
}
