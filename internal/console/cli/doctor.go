package cli

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
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
// the database, the master key, the listener binds, the host's swap (ADR 0008),
// the maintenance worker, the mail setup and the heartbeat ping (ADR 0020,
// doctor_mail_m4.go).
// It prints OK, WARN, FAIL or NOT CHECKED per check and exits 1 on any FAIL.
// Doctor has no flag that skips network checks: the bind tests, the adminpath
// ping and the mail relay probe always run, except that a drill copy
// ("drill": true, ADR 0029) never probes the mail relay and reports it as
// NOT CHECKED (doctor_mail_m4.go).
// It changes no data it checks: the database is opened read-only without
// migrations or checkpoints (openCheckedDatabase, store.OpenNoMigrate), so doctor is
// safe to run before an upgrade and beside a running console. SQLite may create
// the -shm index and an empty -wal file to read a WAL database.
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
		fmt.Fprintf(io.Stdout, "%-11s %-22s %s\n", level, name, detail) // 11 fits "NOT CHECKED"
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
	if cfg.Drill { // ADR 0029, ruling C8: said every time, so a drill copy is never mistaken for production, nor left on
		report("WARN", "drill", "on: drill copy. serve opens no agent listener and sends no mail; maintain serves read methods only and runs no scheduler pass")
	} else {
		report("OK", "drill", "off")
	}

	st, dbWhy := openCheckedDatabase(cfg.DBPath, report)
	if st != nil {
		defer func() { _ = st.Close() }()
	}

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
	checkTrustedProxies(cfg, report)
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
	} else if p.Drill != cfg.Drill {
		// serve and maintain decide drill independently (P22-R2b B7): a drill console in front of a
		// production worker, or the reverse, is a failure, not a detail of the OK line.
		report("FAIL", "maintenance worker", fmt.Sprintf("reachable, but drill mode differs: this config says drill %v, the running worker says %v (set both the same: config.json \"drill\" and CRYO_DRILL in the worker's environment)", cfg.Drill, p.Drill))
	} else {
		report("OK", "maintenance worker", fmt.Sprintf("reachable, master key loaded: %v", p.MasterKeyLoaded))
	}
	cancel()
	c.Close()
	checkMail(cfg, st, dbWhy, report)
	checkHeartbeat(cfg, report)
	if failed {
		return ExitFailed
	}
	return ExitOK
}

// openForReading opens the file at path for reading and closes it again: whether this account can
// read the database's main file at all. A variable so that a test can inject the failure where
// making a file unreadable (an ACL on Windows) cannot be done hermetically.
var openForReading = func(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	return f.Close()
}

// dirWritable reports whether this account can create a file in dir, by creating an empty
// temporary file there and removing it again (there is no portable access check: on Windows an
// ACL decides). Doctor tests the directory this way before it blames it, and only on the failure
// path. A variable so that a test can inject an unwritable directory.
var dirWritable = func(dir string) bool {
	f, err := os.CreateTemp(dir, ".cryo-doctor-probe-*")
	if err != nil {
		return false
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return true
}

// openNoMigrate is store.OpenNoMigrate; a variable so that a test can inject an open failure
// whose cause the real file system cannot produce hermetically.
var openNoMigrate = store.OpenNoMigrate

// dbNotOpened is the NOT CHECKED reason of a check that reads the database when it could not be
// opened at all.
const dbNotOpened = "the database could not be opened (see the database line)"

// openCheckedDatabase opens the database without applying migrations or writing anything
// (store.OpenNoMigrate). A database older than this binary is not a failure: doctor
// says which version it records, which version the first start of this binary will
// migrate it to and the migrations that will run, and the checks that read the
// current schema (the default policy and storage target) are NOT CHECKED until
// then, never passed. A database newer than the binary fails, as the console would. A database that
// cannot be opened fails with its cause (dbOpenFailure).
//
// It returns the open store when the database is at this binary's schema, for the later checks
// that read it (the caller closes it); otherwise nil and the NOT CHECKED reason of those checks.
func openCheckedDatabase(path string, report func(level, name, detail string)) (*store.Store, string) {
	ctx := context.Background()
	st, pending, err := openNoMigrate(ctx, store.Options{Path: path})
	if errors.Is(err, store.ErrNotFound) {
		report("FAIL", "database", "missing: run cryo-console init")
		return nil, dbNotOpened
	}
	if err != nil {
		report("FAIL", "database", dbOpenFailure(path, err))
		return nil, dbNotOpened
	}
	keep := false
	defer func() {
		if !keep {
			_ = st.Close()
		}
	}()
	v, verr := st.SchemaVersion(ctx)
	integrity, _ := st.IntegrityCheck(ctx)
	switch {
	case verr != nil:
		report("FAIL", "database", "cannot read the schema version")
		return nil, "the schema version of the database cannot be read (see the database line)"
	case integrity != "ok":
		report("FAIL", "database", "integrity_check did not return ok")
		return nil, "the database did not pass integrity_check (see the database line)"
	}
	if len(pending) > 0 {
		target := v + len(pending)
		report("WARN", "database", fmt.Sprintf("schema version %d, quick_check and integrity_check ok; the first start of this binary migrates it to schema version %d (pending: %s). Doctor changes no data and applies no migration (SQLite may create the -shm and an empty -wal file); take the backup before you start the new binary",
			v, target, strings.Join(pending, ", ")))
		why := fmt.Sprintf("the database is at schema version %d and this binary reads schema version %d: checked after the upgrade", v, target)
		report("NOT CHECKED", "default policy", why)
		report("NOT CHECKED", "storage target", why)
		return nil, why
	}
	report("OK", "database", fmt.Sprintf("schema version %d, quick_check and integrity_check ok; read only: no data changed (SQLite may create the -shm and an empty -wal file)", v))
	if _, err := st.DefaultPolicy(ctx); err != nil {
		report("FAIL", "default policy", "none: run cryo-console init")
	}
	if _, err := st.DefaultStorageTarget(ctx); err != nil {
		report("WARN", "storage target", "no default storage target")
	}
	keep = true
	return st, ""
}

// dbOpenFailure is the FAIL detail of a database file that exists (OpenNoMigrate looked) but could
// not be opened read-only. It names a cause only where doctor can see one, in this order: a main
// file this account cannot read; a -wal beside it that is not a regular file (a directory at its
// name) or that this account cannot read; a -shm that is not a regular file; and, for SQLite's
// "unable to open database file", a -shm this account cannot read, or no -shm at all in a
// directory this account has been tested not to be able to create a file in (SQLite had to create
// the -shm there and could not). With none of those it says neutrally that the read-only open
// failed and blames nothing.
func dbOpenFailure(path string, err error) string {
	if openForReading(path) != nil {
		return "database file is not readable by this user"
	}
	if fi, serr := os.Lstat(path + "-wal"); serr == nil {
		if !fi.Mode().IsRegular() {
			return "database -wal file is not a regular file"
		}
		if openForReading(path+"-wal") != nil {
			return "database -wal file is not readable by this user"
		}
	}
	const asService = "; run doctor as the service account cryo-console serve runs as"
	fi, serr := os.Lstat(path + "-shm")
	if serr == nil && !fi.Mode().IsRegular() {
		return "cannot be read: to read the database SQLite must open its -shm index file next to it, and what stands at that name is not a regular file" + asService
	}
	if !strings.Contains(err.Error(), "unable to open database file") {
		return fmt.Sprint(sanitize(err))
	}
	switch {
	case serr == nil && openForReading(path+"-shm") != nil:
		return "database -shm file is not readable by this user" + asService
	case errors.Is(serr, os.ErrNotExist) && !dirWritable(filepath.Dir(path)):
		return "cannot be read: to read the database SQLite must create its -shm index file next to it, and this account cannot (the directory is not writable for it)" + asService
	}
	return "database could not be opened read-only" + asService
}
