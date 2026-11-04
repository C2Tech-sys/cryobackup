package jobs

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// The M3 monitoring jobs besides the health-prober (architecture §5.7).
const (
	NameAlarmEvaluator      = "alarm-evaluator"
	NameAbandonedRunSweeper = "abandoned-run-sweeper"
	NameHealthcheckPing     = "healthcheck-ping"
)

// Schedules (architecture §5.7).
const (
	// AlarmEvaluatorEvery is the alarm-evaluator period; banner C06 fires after three of them.
	AlarmEvaluatorEvery = time.Minute
	// AbandonedRunSweeperEvery is the abandoned-run-sweeper period.
	AbandonedRunSweeperEvery = 5 * time.Minute
	// DefaultHealthcheckPingEvery is the healthcheck-ping default period.
	DefaultHealthcheckPingEvery = 5 * time.Minute
	// HealthcheckPingTimeout bounds one outbound ping.
	HealthcheckPingTimeout = 10 * time.Second
	// EvaluatorHealthyPeriods: the evaluator is healthy while its newest finish is within this many periods.
	EvaluatorHealthyPeriods = 3
)

// ClassEvaluatorUnhealthy is healthcheck-ping's error class when it withheld the ping.
const ClassEvaluatorUnhealthy = "evaluator_unhealthy"

// AlarmEvaluator is the alarm-evaluator loop (architecture §5.7, "loop from
// M3"): every 60 s it evaluates the whole fleet and records its run in
// job_state, which is what banner C06 and healthcheck-ping read. It sends no
// notification (M4).
type AlarmEvaluator struct {
	// Evaluate runs one evaluation (health.Evaluator.Alarms); an error is the run's error class.
	Evaluate func(ctx context.Context) error
}

// Job returns the schedule for the runner.
func (j *AlarmEvaluator) Job() Job {
	return Job{Name: NameAlarmEvaluator, Every: AlarmEvaluatorEvery, Lease: 2 * time.Minute, Run: j.Evaluate}
}

// AbandonedRunSweeper closes open agent runs the device stopped reporting
// (store.SweepAbandonedRuns) every five minutes.
type AbandonedRunSweeper struct {
	Store *store.Store
	// Options tune the sweep; the zero value takes the store defaults.
	Options store.SweepOptions
}

// Job returns the schedule for the runner.
func (j *AbandonedRunSweeper) Job() Job {
	return Job{Name: NameAbandonedRunSweeper, Every: AbandonedRunSweeperEvery, Run: func(ctx context.Context) error {
		_, err := j.Store.SweepAbandonedRuns(ctx, j.Options)
		return err
	}}
}

// HealthcheckPing is the healthcheck-ping job (architecture §5.7): an outbound
// request to the operator's dead-man's-switch URL, sent only while the
// alarm-evaluator is healthy, meaning its newest job_state row finished ok
// within EvaluatorHealthyPeriods periods. A withheld ping is the run's error
// (evaluator_unhealthy), so the external monitor alerts exactly when alerting
// stops. The URL is never logged (it often carries the monitor's token); only
// the class of a failure is.
type HealthcheckPing struct {
	Store *store.Store
	// URL is healthcheck_ping.url; the job is registered only when it is set.
	URL string
	// Method is GET or POST (an empty POST body).
	Method string
	// Every is the period; zero means DefaultHealthcheckPingEvery.
	Every time.Duration
	// EvaluatorPeriod is the alarm-evaluator period; zero means AlarmEvaluatorEvery.
	EvaluatorPeriod time.Duration
	// Client replaces the HTTP client (tests).
	Client *http.Client
}

// Job returns the schedule for the runner.
func (j *HealthcheckPing) Job() Job {
	every := j.Every
	if every <= 0 {
		every = DefaultHealthcheckPingEvery
	}
	return Job{Name: NameHealthcheckPing, Every: every, Run: j.Run}
}

// EvaluatorHealthy reports whether the alarm-evaluator's newest run finished ok
// within periods x period of now.
func EvaluatorHealthy(ctx context.Context, st *store.Store, period time.Duration, now time.Time) (bool, error) {
	if period <= 0 {
		period = AlarmEvaluatorEvery
	}
	j, err := st.GetJobState(ctx, NameAlarmEvaluator)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if j.LastFinishedAt == nil || j.LastStatus != "ok" {
		return false, nil
	}
	age := now.Sub(*j.LastFinishedAt)
	return age >= -time.Minute && age <= time.Duration(EvaluatorHealthyPeriods)*period, nil
}

// Run is one ping.
func (j *HealthcheckPing) Run(ctx context.Context) error {
	ok, err := EvaluatorHealthy(ctx, j.Store, j.EvaluatorPeriod, j.Store.Now())
	if err != nil {
		return err
	}
	if !ok {
		return &ClassError{Class: ClassEvaluatorUnhealthy}
	}
	method := j.Method
	if method != http.MethodPost {
		method = http.MethodGet
	}
	cctx, cancel := context.WithTimeout(ctx, HealthcheckPingTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, method, j.URL, nil)
	if err != nil {
		return &ClassError{Class: "bad_url"} // never the error text: it would carry the URL
	}
	req.Header.Set("User-Agent", "cryo-console-healthcheck-ping")
	c := j.Client
	if c == nil {
		c = &http.Client{Timeout: HealthcheckPingTimeout}
	}
	resp, err := c.Do(req)
	if err != nil {
		return &ClassError{Class: pingClass(err)}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &ClassError{Class: "status_" + strconv.Itoa(resp.StatusCode)}
	}
	return nil
}

func pingClass(err error) string {
	var ne net.Error
	var de *net.DNSError
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return ClassTimeout
	case errors.As(err, &de):
		return "dns"
	}
	return "unreachable"
}
