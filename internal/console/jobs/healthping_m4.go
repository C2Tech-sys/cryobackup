package jobs

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"syscall"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/buildinfo"
	"github.com/c2tech-sys/cryobackup/internal/console/config"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// HeartbeatPingTimeout bounds one outbound heartbeat ping, connect to last byte.
const HeartbeatPingTimeout = 10 * time.Second

// heartbeatBodyCap is the most of a reply body the ping reads; the rest is dropped unread.
const heartbeatBodyCap = 4 << 10

// ClassBadConfig is the class of a ping that was not sent because its configured URL or method is
// not one the job will use (not https, not loopback, no host, a method other than GET, HEAD or POST).
const ClassBadConfig = "bad_config"

// The class words of a failed ping. They are the only thing about a failure that reaches a log
// line, a job_state row, a fact or a banner: the URL carries the monitor's secret token, so neither
// it nor any text of an error that quotes it ever leaves this file.
const (
	pingTimeout   = ClassTimeout
	pingDNS       = "dns"
	pingRefused   = "refused"
	pingTLS       = "tls"
	pingStatus4xx = "status_4xx"
	pingStatus5xx = "status_5xx"
	pingRedirect  = "redirect"
	pingOther     = "other"
)

// HeartbeatPing is the healthcheck-ping job (ADR 0020 decision 12): an optional request to a URL
// the operator names, so an outside monitor (a dead man's switch) notices a console that died. It
// supersedes HealthcheckPing in monitor.go, which is the same idea without the hardening below.
//
// WHEN A PING IS WITHHELD. A ping is sent only while the console is doing its job, which means the
// newest pass of the alarm-evaluator ended ok and ended no longer ago than EvaluatorHealthyPeriods
// (three) of the evaluator's periods (EvaluatorHealthy). In every other case no request is sent and
// the run ends with the class evaluator_unhealthy: the evaluator never ran, its newest pass failed,
// or its newest pass is older than that. The missing ping is the alarm, raised by the outside
// monitor, because a console whose alerting has stopped cannot be trusted to say so. Right after
// the console starts, while the evaluator has not yet finished its first pass and still has its
// three periods to do it, the run is "skipped" instead: nothing is sent and nothing has failed.
//
// WHAT IS SENT. Exactly one request per run: the configured method (GET, HEAD or POST; GET when
// empty), no body, no cookies, no credentials of its own, and the fixed User-Agent
// "CryoBackup/<version>". https only; plain http is accepted only when the host is loopback (tests
// and a local relay). Redirects are not followed: a 3xx is a failure of class redirect. The total
// time is HeartbeatPingTimeout. The reply body is read up to 4 KiB and dropped. 2xx is success.
//
// WHAT IS KEPT. The facts between passes (Facts) live in this struct, behind a mutex, the way the
// health-prober keeps its probe results: job_state keeps only the newest run (status and class)
// and has no field for a streak or for the time of the last success. A restart therefore starts
// the streak over; the outside monitor, not this console, is the witness that matters across one.
//
// The job is registered only when a URL is set (RegisterHeartbeatPing).
type HeartbeatPing struct {
	// Store is read for the alarm-evaluator's row and for the clock.
	Store *store.Store
	// EvaluatorPeriod is the alarm-evaluator period; zero means AlarmEvaluatorEvery.
	EvaluatorPeriod time.Duration
	// Timeout replaces HeartbeatPingTimeout (tests).
	Timeout time.Duration
	// Transport replaces the default transport (tests). Nil builds a fresh one for each ping.
	Transport http.RoundTripper
	// Log is where a recovery is logged; nil means the runner's logger once registered, else the
	// process default.
	Log *logx.Logger

	rawURL  string
	method  string
	every   time.Duration
	started time.Time

	mu    sync.Mutex
	facts state.HeartbeatFacts
}

// NewHeartbeatPing returns the job for a ping to rawURL. method is GET, HEAD or POST (empty means
// GET); every is the period (zero means DefaultHealthcheckPingEvery); started is when this console
// process started (zero means unknown: a missing evaluation is then a failure at once). An empty
// rawURL gives a job that is not configured: it registers nothing and its facts say so.
func NewHeartbeatPing(st *store.Store, rawURL, method string, every time.Duration, started time.Time) *HeartbeatPing {
	return &HeartbeatPing{Store: st, rawURL: rawURL, method: method, every: every, started: started,
		facts: state.HeartbeatFacts{Configured: rawURL != ""}}
}

// Configured reports whether a URL is set.
func (j *HeartbeatPing) Configured() bool { return j.rawURL != "" }

// Job returns the schedule for the runner.
func (j *HeartbeatPing) Job() Job {
	every := j.every
	if every <= 0 {
		every = DefaultHealthcheckPingEvery
	}
	return Job{Name: NameHealthcheckPing, Every: every, Run: j.Run}
}

// RegisterHeartbeatPing adds the job to r and reports whether it did: a job with no URL is not
// added, since the feature is optional and off by default. It gives the job the runner's logger
// when it has none.
func RegisterHeartbeatPing(r *Runner, j *HeartbeatPing) bool {
	if j == nil || !j.Configured() {
		return false
	}
	if j.Log == nil {
		j.Log = r.Log
	}
	r.Add(j.Job())
	return true
}

// Facts returns the heartbeat facts for the banner C19 (state.HeartbeatFacts). It is safe to call
// while a ping is in flight.
func (j *HeartbeatPing) Facts() state.HeartbeatFacts {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.facts
}

func (j *HeartbeatPing) log() *logx.Logger {
	if j.Log != nil {
		return j.Log
	}
	return logx.Default()
}

func (j *HeartbeatPing) setWithheld(w bool) {
	j.mu.Lock()
	j.facts.Withheld = w
	j.mu.Unlock()
}

func (j *HeartbeatPing) succeeded(at time.Time) (recovered int) {
	j.mu.Lock()
	defer j.mu.Unlock()
	recovered = j.facts.ConsecutiveFailures
	j.facts.LastSuccessAt, j.facts.ConsecutiveFailures, j.facts.LastFailureClass, j.facts.Withheld = ms(at), 0, "", false
	return recovered
}

func (j *HeartbeatPing) failed(class string) error {
	j.mu.Lock()
	j.facts.ConsecutiveFailures++
	j.facts.LastFailureClass, j.facts.Withheld = class, false
	j.mu.Unlock()
	return &ClassError{Class: class} // no cause: the cause's text may quote the URL
}

// awaitingFirstEvaluation reports whether the evaluator has yet to finish a pass on this console
// and still has its time to do it. Both jobs start within seconds of each other in no fixed order,
// so a ping at that moment says nothing about alerting. Past EvaluatorHealthyPeriods periods from
// the start, no evaluation is the failure it looks like.
func (j *HeartbeatPing) awaitingFirstEvaluation(ctx context.Context, now time.Time, period time.Duration) (bool, error) {
	if j.started.IsZero() || now.Sub(j.started) > time.Duration(EvaluatorHealthyPeriods)*period {
		return false, nil
	}
	row, err := j.Store.GetJobState(ctx, NameAlarmEvaluator)
	if errors.Is(err, store.ErrNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	// job_state holds milliseconds, hence the truncation.
	return row.LastFinishedAt == nil || row.LastFinishedAt.Before(j.started.Truncate(time.Millisecond)), nil
}

// Run is one pass: send nothing, or send one ping and record how it went.
func (j *HeartbeatPing) Run(ctx context.Context) error {
	if !j.Configured() {
		return errDidNotRun
	}
	period := j.EvaluatorPeriod
	if period <= 0 {
		period = AlarmEvaluatorEvery
	}
	now := j.Store.Now()
	// No evaluation, or one that cannot be read, means no ping.
	waiting, err := j.awaitingFirstEvaluation(ctx, now, period)
	if err != nil {
		j.setWithheld(true)
		return err
	}
	if waiting {
		j.setWithheld(true)
		return errSkipped
	}
	healthy, err := EvaluatorHealthy(ctx, j.Store, period, now)
	if err != nil {
		j.setWithheld(true)
		return err
	}
	if !healthy {
		j.setWithheld(true)
		return &ClassError{Class: ClassEvaluatorUnhealthy}
	}

	method, target, ok := j.request()
	if !ok {
		return j.failed(ClassBadConfig)
	}
	timeout := j.Timeout
	if timeout <= 0 {
		timeout = HeartbeatPingTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, method, target, nil)
	if err != nil {
		return j.failed(ClassBadConfig) // never err's text: it quotes the URL
	}
	req.Header.Set("User-Agent", "CryoBackup/"+buildinfo.Version)
	tr := j.Transport
	if tr == nil {
		own := http.DefaultTransport.(*http.Transport).Clone()
		own.DisableKeepAlives = true
		defer own.CloseIdleConnections()
		tr = own
	}
	client := &http.Client{
		Transport: tr,
		Timeout:   timeout,
		// A redirect is a failure to report, not a place to go: the target would receive the secret.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err() // the console is stopping: not a failed ping
		}
		return j.failed(heartbeatErrClass(err))
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, heartbeatBodyCap))
	if class := heartbeatStatusClass(resp.StatusCode); class != "" {
		return j.failed(class)
	}
	if n := j.succeeded(now); n > 0 {
		j.log().Info("heartbeat ping recovered", logx.Str("job", NameHealthcheckPing), logx.Count("failures", n))
	}
	return nil
}

// request validates the configured method and URL. ok is false for anything the job will not send.
func (j *HeartbeatPing) request() (method, target string, ok bool) {
	method = j.method
	switch method {
	case "":
		method = http.MethodGet
	case http.MethodGet, http.MethodHead, http.MethodPost:
	default:
		return "", "", false
	}
	if !config.HeartbeatURLAllowed(j.rawURL) {
		return "", "", false
	}
	return method, j.rawURL, true
}

// heartbeatStatusClass maps a reply status to a class word; "" is success (2xx).
func heartbeatStatusClass(code int) string {
	switch {
	case code >= 200 && code < 300:
		return ""
	case code >= 300 && code < 400:
		return pingRedirect
	case code >= 400 && code < 500:
		return pingStatus4xx
	case code >= 500 && code < 600:
		return pingStatus5xx
	}
	return pingOther
}

// heartbeatErrClass maps a transport error to a class word. It reads the error's type and never
// its text, which quotes the URL.
func heartbeatErrClass(err error) string {
	var (
		dns     *net.DNSError
		ne      net.Error
		errno   syscall.Errno
		unknown x509.UnknownAuthorityError
		host    x509.HostnameError
		invalid x509.CertificateInvalidError
		verify  *tls.CertificateVerificationError
		record  tls.RecordHeaderError
		alert   tls.AlertError
	)
	switch {
	case errors.As(err, &dns):
		return pingDNS
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return pingTimeout
	// 10061 is Windows' WSAECONNREFUSED, which syscall.ECONNREFUSED is not there.
	case errors.Is(err, syscall.ECONNREFUSED), errors.As(err, &errno) && errno == 10061:
		return pingRefused
	case errors.As(err, &verify), errors.As(err, &unknown), errors.As(err, &host), errors.As(err, &invalid),
		errors.As(err, &record), errors.As(err, &alert):
		return pingTLS
	}
	return pingOther
}
