package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/c2tech-sys/cryobackup/internal/console/config"
	"github.com/c2tech-sys/cryobackup/internal/logx"
)

// loadConfig reads the configuration for a command that only opens the
// database (token, user): the listener and TLS rules are serve's business, so a
// config that serve would still refuse (no certificate yet) works here.
func loadConfig(path string) (config.Config, error) {
	cfg, err := config.Read(path, nil)
	if err != nil {
		return config.Config{}, err
	}
	if cfg.DBPath == "" {
		return config.Config{}, errors.New("db_path is not set")
	}
	return cfg, nil
}

// serveContext is the context serve runs under: cancelled by SIGINT/SIGTERM.
// A variable so a test can run serve to completion without a signal.
var serveContext = func() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// printFixturesLogin tells the operator how to sign in to the fixtures console.
// A password the operator chose (CRYO_FIXTURES_PASSWORD) is never echoed to
// stdout, where it would land in terminal scrollback and service logs; only a
// generated one is printed, once, because nothing else could tell the operator.
func printFixturesLogin(w io.Writer, supplied bool, password string) {
	if supplied {
		fmt.Fprintln(w, "fixtures mode: the fleet is fictional. Sign in as admin with the password from CRYO_FIXTURES_PASSWORD")
		return
	}
	fmt.Fprintf(w, "fixtures mode: the fleet is fictional. Sign in as %q with password %q (set CRYO_FIXTURES_PASSWORD to choose one).\n", "admin", password)
}

func runServe(args []string, io IO) int {
	fs := newFlags("serve", io)
	cfgPath := fs.String("config", "", "path of config.json (optional with --fixtures)")
	fixturesMode := fs.Bool("fixtures", false, "serve the fictional fleet from an in-memory database; no network, no agent listener")
	adminAddr := fs.String("admin-addr", "", "admin listener address (default from the config, 127.0.0.1:8080)")
	agentAddr := fs.String("agent-addr", "", "agent listener address (default from the config, :8443)")
	insecure := fs.Bool("insecure-http-for-lab", false, "serve the agent API over plain HTTP; only on a loopback or private bind")
	allowPublic := fs.Bool("admin-allow-public", false, "allow the admin listener on a non-loopback, non-private address (put authentication in front of it)")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	cfg, err := config.Read(*cfgPath, nil)
	if err != nil {
		fmt.Fprintln(io.Stderr, "cryo-console serve:", sanitize(err))
		return ExitFailed
	}
	if *fixturesMode {
		cfg.Fixtures = true
	}
	if *adminAddr != "" {
		cfg.AdminListen = *adminAddr
	}
	if *agentAddr != "" {
		cfg.AgentListen = *agentAddr
	}
	if *insecure {
		cfg.InsecureHTTPLab = true
	}
	if *allowPublic {
		cfg.AdminAllowPublic = true
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintln(io.Stderr, "cryo-console serve:", err)
		return ExitFailed
	}
	logx.Install()
	ctx, stop := serveContext()
	defer stop()
	suppliedPassword := os.Getenv("CRYO_FIXTURES_PASSWORD")
	app, err := Build(ctx, cfg, BuildOptions{FixturesPassword: suppliedPassword})
	if err != nil {
		fmt.Fprintln(io.Stderr, "cryo-console serve:", sanitize(err))
		return ExitFailed
	}
	if err := app.Server.Listen(); err != nil {
		app.Close()
		fmt.Fprintln(io.Stderr, "cryo-console serve:", err)
		return ExitFailed
	}
	adminScheme := "http"
	if app.Server.AdminTLSEnabled() {
		adminScheme = "https"
	}
	fmt.Fprintf(io.Stdout, "admin listener: %s://%s/\n", adminScheme, app.Server.AdminAddr())
	if a := app.Server.AgentAddr(); a != "" {
		scheme := "https"
		if !app.Server.TLSEnabled() {
			scheme = "http (lab, no TLS)"
		}
		fmt.Fprintf(io.Stdout, "agent listener: %s on %s\n", scheme, a)
	}
	if cfg.Fixtures {
		printFixturesLogin(io.Stdout, suppliedPassword != "", app.FixturesPassword)
		fmt.Fprintf(io.Stdout, "design check page: %s://%s/design\n", adminScheme, app.Server.AdminAddr())
	}
	return serveExit(app.Run(ctx), io)
}

// serveExit is serve's exit code: a listener that failed or a database not closed
// cleanly is non-zero, so the supervisor restarts it. A clean stop is 0.
func serveExit(err error, io IO) int {
	if err != nil {
		fmt.Fprintln(io.Stderr, "cryo-console serve:", sanitize(err))
		return ExitFailed
	}
	return ExitOK
}
