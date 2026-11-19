package cli

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/console/config"
	"github.com/c2tech-sys/cryobackup/internal/console/escrow"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// checkAdminTLS reports whether the admin listener serves TLS. With a
// certificate configured it proves the pair loads (a broken pair stops serve at
// start, so say so now). Without one it warns when admin_listen is anything but
// loopback: a shared-address mesh (Tailscale, Headscale) encrypts the hop, a
// plain private LAN does not, and doctor cannot tell which it is.
func checkAdminTLS(cfg config.Config, report func(level, name, detail string)) {
	const name = "admin tls"
	cert, key := cfg.Admin.TLS.Cert, cfg.Admin.TLS.Key
	switch {
	case cert != "" && key != "":
		if _, err := tls.LoadX509KeyPair(cert, key); err != nil {
			report("FAIL", name, "on, but admin.tls.cert and admin.tls.key do not load as a certificate and key pair")
			return
		}
		report("OK", name, "on (TLS 1.2 or newer; the session cookie is Secure)")
	case cert != "" || key != "":
		report("FAIL", name, "admin.tls.cert and admin.tls.key must be set together")
	case isLoopbackBind(cfg.AdminListen):
		report("OK", name, "off (admin_listen is loopback)")
	default:
		report("WARN", name, "off, and admin_listen is not loopback: passwords and session cookies cross the network in clear text unless the path is an encrypted mesh; set admin.tls.cert and admin.tls.key")
	}
}

// isLoopbackBind reports whether the host of a host:port bind address is
// "localhost" or a loopback IP literal. Private and mesh addresses are not.
func isLoopbackBind(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// runDoctor checks what a stranger would get wrong first: the configuration,
// the database, the master key, the listener binds, the host's swap (ADR 0008)
// and the maintenance worker.
// It prints OK, WARN, FAIL or NOT CHECKED per check and exits 1 on any FAIL.
// It changes nothing it checks: the database is opened without migrations
// (checkDatabase), so doctor is safe to run before an upgrade.
func runDoctor(args []string, io IO) int {
	fs := newFlags("doctor", io)
	cfgPath := fs.String("config", "config.json", "path of config.json")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	failed := false
	report := func(level, name, detail string) {
		if level == "FAIL" {
			failed = true
		}
		fmt.Fprintf(io.Stdout, "%-5s %-22s %s\n", level, name, detail)
	}
	cfg, err := config.Read(*cfgPath, nil)
	if err != nil {
		report("FAIL", "config", fmt.Sprint(sanitize(err)))
		return ExitFailed
	}
	if err := cfg.Validate(); err != nil {
		report("FAIL", "config", err.Error())
	} else {
		report("OK", "config", "valid")
	}

	checkDatabase(cfg.DBPath, report)

	if mk, err := escrow.LoadMasterKey(cfg.MasterKeyPath); err != nil {
		report("FAIL", "master key", err.Error()+" (banner C12: enrollment is disabled)")
	} else {
		detail := "loaded, id " + mk.KeyID()
		if fi, err := os.Stat(cfg.MasterKeyPath); err == nil && fi.Mode().Perm()&0o077 != 0 && os.PathSeparator == '/' {
			report("WARN", "master key file mode", "readable by group or others; chmod 0400")
		}
		report("OK", "master key", detail)
		report("WARN", "master key copy", "doctor cannot know whether an offline copy exists; confirm it yourself")
	}

	for _, l := range []struct{ name, addr string }{{"admin listen", cfg.AdminListen}, {"agent listen", cfg.AgentListen}} {
		ln, err := net.Listen("tcp", l.addr)
		if err != nil {
			report("WARN", l.name, "cannot bind now (already running, or the address is not on this host)")
			continue
		}
		_ = ln.Close()
		report("OK", l.name, "bind test succeeded")
	}
	if !cfg.AdminAllowPublic && !config.IsLoopbackOrPrivate(cfg.AdminListen) {
		report("FAIL", "admin exposure", "admin_listen is public and admin_allow_public is off")
	}
	checkAdminTLS(cfg, report)
	if fi, err := os.Stat(cfg.GatewayAuthDir); err != nil || !fi.IsDir() {
		report("WARN", "gateway auth dir", "missing: htpasswd-sync cannot write the gateway's login file")
	} else {
		f, err := os.CreateTemp(cfg.GatewayAuthDir, ".doctor-")
		if err != nil {
			report("FAIL", "gateway auth dir", "not writable")
		} else {
			_ = f.Close()
			_ = os.Remove(f.Name())
			report("OK", "gateway auth dir", "writable")
		}
	}
	checkSwap(procSwapsPath, sysBlockPath, report)
	c := adminpath.NewClient(cfg.AdminpathSocket, adminpath.ClientOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	if p, err := c.Ping(ctx); err != nil {
		report("WARN", "maintenance worker", "not reachable on the adminpath socket (banner C07; restore browsing is off)")
	} else {
		report("OK", "maintenance worker", fmt.Sprintf("reachable, master key loaded: %v", p.MasterKeyLoaded))
	}
	cancel()
	c.Close()
	if failed {
		return ExitFailed
	}
	return ExitOK
}

// checkDatabase opens the database without applying migrations or writing anything
// (store.OpenNoMigrate). A database older than this binary is not a failure: doctor
// says which version it records, which version the first start of this binary will
// migrate it to and the migrations that will run, and the checks that read the
// current schema (the default policy and storage target) are NOT CHECKED until
// then, never passed. A database newer than the binary fails, as the console would.
func checkDatabase(path string, report func(level, name, detail string)) {
	ctx := context.Background()
	st, pending, err := store.OpenNoMigrate(ctx, store.Options{Path: path})
	if errors.Is(err, store.ErrNotFound) {
		report("FAIL", "database", "missing: run cryo-console init")
		return
	}
	if err != nil {
		report("FAIL", "database", fmt.Sprint(sanitize(err)))
		return
	}
	defer func() { _ = st.Close() }()
	v, verr := st.SchemaVersion(ctx)
	integrity, _ := st.IntegrityCheck(ctx)
	switch {
	case verr != nil:
		report("FAIL", "database", "cannot read the schema version")
		return
	case integrity != "ok":
		report("FAIL", "database", "integrity_check did not return ok")
		return
	}
	if len(pending) > 0 {
		target := v + len(pending)
		report("WARN", "database", fmt.Sprintf("schema version %d, quick_check and integrity_check ok; the first start of this binary migrates it to schema version %d (pending: %s). Doctor changed nothing; take the backup before you start the new binary",
			v, target, strings.Join(pending, ", ")))
		why := fmt.Sprintf("the database is at schema version %d and this binary reads schema version %d: checked after the upgrade", v, target)
		report("NOT CHECKED", "default policy", why)
		report("NOT CHECKED", "storage target", why)
		return
	}
	report("OK", "database", fmt.Sprintf("schema version %d, quick_check and integrity_check ok", v))
	if _, err := st.DefaultPolicy(ctx); err != nil {
		report("FAIL", "default policy", "none: run cryo-console init")
	}
	if _, err := st.DefaultStorageTarget(ctx); err != nil {
		report("WARN", "storage target", "no default storage target")
	}
}
