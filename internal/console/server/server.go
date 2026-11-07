package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/buildinfo"
	"github.com/c2tech-sys/cryobackup/internal/console/config"
	"github.com/c2tech-sys/cryobackup/internal/console/httpx"
	"github.com/c2tech-sys/cryobackup/internal/logx"
)

// Timeouts of architecture §5.5 (read 30 s except the long poll, which lifts it
// for its own request; write 60 s; idle 120 s; headers 8 KiB).
const (
	ReadHeaderTimeout = 10 * time.Second
	ReadTimeout       = 30 * time.Second
	WriteTimeout      = 60 * time.Second
	IdleTimeout       = 120 * time.Second
	MaxHeaderBytes    = 8 << 10
	ShutdownGrace     = 10 * time.Second
)

// Options wire a Server to the route sets and to the facts it reports.
type Options struct {
	// RegisterAgent registers the agent API (api.Pattern* routes only) on the
	// agent listener's router; nil registers nothing.
	RegisterAgent func(r httpx.Registrar)
	// RegisterAdmin registers the admin API, the UI and /static on the admin router.
	RegisterAdmin func(r httpx.Registrar)
	// AgentDisabled leaves the agent listener closed (fixtures mode).
	AgentDisabled bool
	// Health answers /healthz (nil serves a bare liveness answer).
	Health func(ctx context.Context) Health
	Log    *logx.Logger
	Clock  func() time.Time
}

// Server is the two listeners with their disjoint handler trees (ADR 0002 D5).
type Server struct {
	cfg  config.Config
	opts Options
	log  *logx.Logger
	now  func() time.Time

	AgentRouter *Router
	AdminRouter *Router

	// agentErrs and adminErrs are the ErrorLog of each listener's http.Server.
	agentErrs *errorLog
	adminErrs *errorLog

	agentHandler http.Handler
	adminHandler http.Handler

	mu       sync.Mutex
	agentSrv *http.Server
	adminSrv *http.Server
	agentLn  net.Listener
	adminLn  net.Listener
	// agentCerts and adminCerts keep each listener's reloader reachable.
	agentCerts *certReloader
	adminCerts *certReloader
	// agentTLS and adminTLS record, at Listen time, whether each listener serves
	// TLS. TLSEnabled and AdminTLSEnabled read these and never the live
	// *http.Server: net/http fills in srv.TLSConfig (HTTP/2 setup) from inside
	// ServeTLS, so reading srv.TLSConfig while Serve runs is a data race.
	agentTLS bool
	adminTLS bool
	// serving is set by the first Serve; a server is started exactly once.
	serving bool
}

// New builds both routers and handler chains. It refuses an admin bind that is
// not loopback or private unless admin_allow_public is set, and plain HTTP on
// the agent listener unless insecure_http_for_lab is set on a loopback or
// private bind (the same rules config.Validate states).
func New(cfg config.Config, o Options) (*Server, error) {
	if !cfg.AdminAllowPublic && !config.IsLoopbackOrPrivate(cfg.AdminListen) {
		return nil, fmt.Errorf("server: admin_listen %q is not a loopback or private address; bind it to 127.0.0.1 or a mesh IP, or pass --admin-allow-public", cfg.AdminListen)
	}
	if (cfg.Admin.TLS.Cert != "") != (cfg.Admin.TLS.Key != "") {
		// One half set would otherwise serve the admin UI in plain HTTP without a word.
		return nil, errors.New("server: admin.tls.cert and admin.tls.key must be set together")
	}
	if !o.AgentDisabled && !cfg.Fixtures {
		plain := cfg.TLS.Cert == "" || cfg.TLS.Key == ""
		if plain && !cfg.InsecureHTTPLab {
			return nil, errors.New("server: the agent listener needs tls.cert and tls.key (or --insecure-http-for-lab on a loopback or private bind)")
		}
		if cfg.InsecureHTTPLab && !config.IsLoopbackOrPrivate(cfg.AgentListen) {
			return nil, fmt.Errorf("server: --insecure-http-for-lab is allowed only on a loopback or private bind, not %q", cfg.AgentListen)
		}
	}
	if o.Log == nil {
		o.Log = logx.Default()
	}
	if o.Clock == nil {
		o.Clock = time.Now
	}
	s := &Server{cfg: cfg, opts: o, log: o.Log, now: o.Clock, AgentRouter: NewRouter(), AdminRouter: NewRouter(),
		agentErrs: newErrorLog(o.Log, "agent", o.Clock), adminErrs: newErrorLog(o.Log, "admin", o.Clock)}

	if o.RegisterAgent != nil {
		o.RegisterAgent(s.AgentRouter)
	}
	s.AgentRouter.Handle(CatchAll, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpx.Problem(w, r, api.CodeNotFound, 0)
	}))
	if o.RegisterAdmin != nil {
		o.RegisterAdmin(s.AdminRouter)
	}
	s.AdminRouter.Handle("GET /healthz", http.HandlerFunc(s.healthz))
	// An unmatched /api/ path is an API client's miss and answers RFC 9457
	// problem+json like the agent listener's; an unmatched UI path keeps the
	// plain-text 404.
	s.AdminRouter.Handle(CatchAll, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api" || strings.HasPrefix(r.URL.Path, "/api/") {
			httpx.Problem(w, r, api.CodeNotFound, 0)
			return
		}
		http.NotFound(w, r)
	}))

	s.agentHandler = chain(s.AgentRouter, withRequestID(), withAccessLog(s.log, s.now), withRecover(s.log), withServerTime(s.now))
	s.adminHandler = chain(s.AdminRouter, withRequestID(), withAccessLog(s.log, s.now), withRecover(s.log), withSecurityHeaders())
	return s, nil
}

// AgentHandler is the agent listener's full handler chain (tests use it with httptest).
func (s *Server) AgentHandler() http.Handler { return s.agentHandler }

// AdminHandler is the admin listener's full handler chain.
func (s *Server) AdminHandler() http.Handler { return s.adminHandler }

// AgentPatterns lists the patterns registered on the agent router, sorted.
func (s *Server) AgentPatterns() []string { return s.AgentRouter.Patterns() }

// AdminPatterns lists the patterns registered on the admin router, sorted.
func (s *Server) AdminPatterns() []string { return s.AdminRouter.Patterns() }

// baseServer builds a listener's http.Server. Its ErrorLog is the typed,
// path-free errorLog (errorlog.go): net/http's free-text lines, among them one
// per internet scanner that fails a TLS handshake, never reach the process log.
func baseServer(h http.Handler, el *errorLog) *http.Server {
	return &http.Server{
		Handler:           h,
		ErrorLog:          el.Logger(),
		ReadHeaderTimeout: ReadHeaderTimeout,
		ReadTimeout:       ReadTimeout,
		WriteTimeout:      WriteTimeout,
		IdleTimeout:       IdleTimeout,
		MaxHeaderBytes:    MaxHeaderBytes,
	}
}

// Listen binds the sockets. The admin address may use port 0 (tests, --fixtures);
// AdminAddr and AgentAddr report what was bound.
func (s *Server) Listen() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.adminLn != nil {
		return nil // already bound
	}
	max := s.cfg.RateLimits.MaxConns
	ln, err := net.Listen("tcp", s.cfg.AdminListen)
	if err != nil {
		return errors.New("server: cannot bind the admin listener")
	}
	s.adminLn = newLimitListener(ln, max)
	s.adminSrv = baseServer(s.adminHandler, s.adminErrs)
	if s.cfg.Admin.TLS.Cert != "" && s.cfg.Admin.TLS.Key != "" {
		cr, err := newCertReloader("admin", s.cfg.Admin.TLS.Cert, s.cfg.Admin.TLS.Key, s.now)
		if err != nil {
			_ = s.adminLn.Close()
			return err
		}
		s.adminCerts = cr
		s.adminSrv.TLSConfig = tlsConfig(cr)
		s.adminTLS = true
	}
	if s.opts.AgentDisabled {
		return nil
	}
	aln, err := net.Listen("tcp", s.cfg.AgentListen)
	if err != nil {
		_ = s.adminLn.Close()
		return errors.New("server: cannot bind the agent listener")
	}
	s.agentLn = newLimitListener(aln, max)
	s.agentSrv = baseServer(s.agentHandler, s.agentErrs)
	if s.cfg.TLS.Cert != "" && s.cfg.TLS.Key != "" {
		cr, err := newCertReloader("agent", s.cfg.TLS.Cert, s.cfg.TLS.Key, s.now)
		if err != nil {
			_ = s.adminLn.Close()
			_ = s.agentLn.Close()
			return err
		}
		s.agentCerts = cr
		s.agentSrv.TLSConfig = tlsConfig(cr)
		s.agentTLS = true
	}
	return nil
}

// tlsConfig is the server TLS configuration both listeners share: TLS 1.2+, the
// certificate from the reloader (a renewal needs no restart), h2 and http/1.1.
func tlsConfig(cr *certReloader) *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: cr.get, NextProtos: []string{"h2", "http/1.1"}}
}

// AdminAddr returns the bound admin address ("" before Listen).
func (s *Server) AdminAddr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.adminLn == nil {
		return ""
	}
	return s.adminLn.Addr().String()
}

// AgentAddr returns the bound agent address ("" when disabled or before Listen).
func (s *Server) AgentAddr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.agentLn == nil {
		return ""
	}
	return s.agentLn.Addr().String()
}

// TLSHandshakeFailures counts the TLS handshakes that failed on the agent and
// on the admin listener since start (scanners, wrong clients, expired pins).
func (s *Server) TLSHandshakeFailures() (agent, admin int64) {
	return s.agentErrs.TLSHandshakeFailures(), s.adminErrs.TLSHandshakeFailures()
}

// TLSEnabled reports whether the agent listener serves TLS.
func (s *Server) TLSEnabled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.agentTLS
}

// AdminTLSEnabled reports whether the admin listener serves TLS.
func (s *Server) AdminTLSEnabled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.adminTLS
}

// Serve runs both listeners until ctx is cancelled or one fails, then shuts them down
// gracefully (ShutdownGrace). It returns the first listener error that is not a
// normal close, naming the listener, and logs every one by name.
func (s *Server) Serve(ctx context.Context) error {
	s.mu.Lock()
	adminSrv, adminLn, agentSrv, agentLn := s.adminSrv, s.adminLn, s.agentSrv, s.agentLn
	adminTLS, agentTLS := s.adminTLS, s.agentTLS
	if adminSrv == nil {
		s.mu.Unlock()
		return errors.New("server: Listen was not called")
	}
	if s.serving {
		s.mu.Unlock()
		return errors.New("server: Serve was already called")
	}
	s.serving = true
	s.mu.Unlock()
	errc := make(chan error, 2)
	var wg sync.WaitGroup
	run := func(name string, srv *http.Server, ln net.Listener, tlsOn bool) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var err error
			if tlsOn {
				err = srv.ServeTLS(ln, "", "")
			} else {
				err = srv.Serve(ln)
			}
			if !errors.Is(err, http.ErrServerClosed) {
				s.log.Error("listener failed", logx.Str("listener", name), logx.Class("listener_failed"))
				errc <- fmt.Errorf("server: the %s listener failed: %w", name, err)
			}
		}()
	}
	run("admin", adminSrv, adminLn, adminTLS)
	if agentSrv != nil {
		run("agent", agentSrv, agentLn, agentTLS)
	}
	s.log.Info("listening", logx.Str("admin", "up"), logx.Bool("admin_tls", adminTLS), logx.Bool("agent", agentSrv != nil), logx.Bool("agent_tls", agentTLS))

	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-errc:
	}
	sctx, cancel := context.WithTimeout(context.Background(), ShutdownGrace)
	defer cancel()
	_ = adminSrv.Shutdown(sctx)
	if agentSrv != nil {
		_ = agentSrv.Shutdown(sctx)
	}
	wg.Wait()
	// A Shutdown that wins the race against a Serve goroutine's start makes that
	// Serve return before it takes the listener; close both so none leaks.
	_ = adminLn.Close()
	if agentLn != nil {
		_ = agentLn.Close()
	}
	return serveErr
}

// ---- certificate reload ----

type certReloader struct {
	name              string // "agent" or "admin": names the listener in errors
	certPath, keyPath string
	now               func() time.Time
	mu                sync.Mutex
	cert              *tls.Certificate
	mtime             time.Time
	checked           time.Time
}

func newCertReloader(name, certPath, keyPath string, now func() time.Time) (*certReloader, error) {
	c := &certReloader{name: name, certPath: certPath, keyPath: keyPath, now: now}
	if err := c.load(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *certReloader) load() error {
	pair, err := tls.LoadX509KeyPair(c.certPath, c.keyPath)
	if err != nil {
		return fmt.Errorf("server: cannot load the %s listener certificate and key", c.name)
	}
	st, err := os.Stat(c.certPath)
	if err != nil {
		return fmt.Errorf("server: cannot stat the %s listener certificate", c.name)
	}
	c.cert, c.mtime, c.checked = &pair, st.ModTime(), c.now()
	return nil
}

// get serves the current certificate and re-reads the files when the
// certificate's modification time changed (checked at most once a minute), so a
// renewal needs no restart. A failed reload keeps the old certificate.
func (c *certReloader) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if now := c.now(); now.Sub(c.checked) >= time.Minute {
		c.checked = now
		if st, err := os.Stat(c.certPath); err == nil && st.ModTime().After(c.mtime) {
			oldCert, oldMtime := c.cert, c.mtime
			if err := c.load(); err != nil {
				c.cert, c.mtime = oldCert, oldMtime
			}
		}
	}
	return c.cert, nil
}

// ---- limiting listener ----

// limitListener holds Accept back while max connections are open. An Accept that
// is waiting for a slot does not touch the socket, so it must also end when the
// listener closes: otherwise http.Server.Serve never returns, Server.Serve hangs on
// its WaitGroup and the shutdown budget in cli/app.go is false. For the same reason
// a socket that dies while every slot is held is noticed when the next slot frees.
type limitListener struct {
	net.Listener
	sem       chan struct{}
	closed    chan struct{}
	closeOnce sync.Once
	closeErr  error
}

func newLimitListener(ln net.Listener, max int) net.Listener {
	if max <= 0 {
		return ln
	}
	return &limitListener{Listener: ln, sem: make(chan struct{}, max), closed: make(chan struct{})}
}

func (l *limitListener) Accept() (net.Conn, error) {
	select {
	case l.sem <- struct{}{}:
	case <-l.closed:
		return nil, net.ErrClosed
	}
	c, err := l.Listener.Accept()
	if err != nil {
		<-l.sem
		return nil, err
	}
	return &limitConn{Conn: c, release: func() { <-l.sem }}, nil
}

// Close closes the socket and ends every Accept waiting for a slot. http.Server and
// Serve both close the listener; the second call answers what the first did.
func (l *limitListener) Close() error {
	l.closeOnce.Do(func() {
		close(l.closed)
		l.closeErr = l.Listener.Close()
	})
	return l.closeErr
}

type limitConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *limitConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}

// ---- /healthz ----

// Health is the /healthz document. Status is "ok" only when the database
// answers, the schema is current and the master key is loaded.
type Health struct {
	Status        string            `json:"status"`
	Database      string            `json:"database"`
	Migrations    string            `json:"migrations"`
	MasterKey     string            `json:"master_key"`
	Version       string            `json:"version"`
	Jobs          map[string]string `json:"jobs,omitempty"`
	FixturesMode  bool              `json:"fixtures_mode,omitempty"`
	CheckedAtUnix int64             `json:"checked_at"`
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	h := Health{Status: "ok", Version: buildinfo.Version}
	if s.opts.Health != nil {
		h = s.opts.Health(r.Context())
		if h.Version == "" {
			h.Version = buildinfo.Version
		}
	}
	h.CheckedAtUnix = s.now().Unix()
	code := http.StatusOK
	if h.Status != "ok" {
		code = http.StatusServiceUnavailable
	}
	httpx.WriteJSON(w, code, h)
}
