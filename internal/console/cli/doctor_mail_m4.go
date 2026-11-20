package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/config"
	"github.com/c2tech-sys/cryobackup/internal/console/mail"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// The mail section of doctor (ADR 0020 decision 13) and its heartbeat line.
//
// Without an smtp block (no smtp.host) the section says that alert mail and the digest are off,
// as a WARN, and nothing in it can fail doctor: mail is optional. With one, it checks the block
// with the functions serve and every send use, the password file, then probes the relay live
// (mail.SMTPSender.Probe: connect, greet, TLS, sign in, QUIT; never a message). It prints the
// relay's host and port, which the operator typed and needs in order to act; never the user name,
// the password, the sender address or anything the relay answered. The recipients' switches are
// read from the database when it is at this binary's schema, read-only like every doctor read.

// probeMail is the live relay probe; a variable so that a test can aim it at a fake relay.
var probeMail = func(ctx context.Context, set mail.Settings) error {
	return mail.NewSMTPSender(set).Probe(ctx)
}

// doctorProbeTimeout bounds the whole probe; the sender's own dial and greeting clocks are shorter.
const doctorProbeTimeout = 30 * time.Second

// checkMail is the mail section. st is the database opened by openCheckedDatabase, or nil with
// dbWhy the reason the reads are not checked.
func checkMail(cfg config.Config, st *store.Store, dbWhy string, report func(level, name, detail string)) {
	present := cfg.SMTP.Host != "" // the condition under which config.Validate checks the block
	rep := report
	if !present {
		// Mail is optional: without an smtp block nothing in this section fails doctor.
		rep = func(level, name, detail string) {
			if level == "FAIL" {
				level = "WARN"
			}
			report(level, name, detail)
		}
		rep("WARN", "mail", "not configured: alert e-mail and the daily digest are off")
	} else {
		checkMailRelay(cfg, rep)
	}
	checkMailRecipients(present, st, dbWhy, rep)
}

// mailSettings are the sender's settings exactly as serve builds them (newMailer).
func mailSettings(cfg config.Config) mail.Settings {
	set := mail.SettingsFromConfig(cfg.SMTP)
	set.HeloName = heloName(cfg.PublicURL)
	return set
}

// checkMailRelay checks a present smtp block: its settings, the password file and the live probe.
func checkMailRelay(cfg config.Config, rep func(level, name, detail string)) {
	set := mailSettings(cfg)
	// The smtp problems of config.Validate (what serve refuses to start with), then
	// Settings.Validate (what every send checks first). Neither text carries a configured value.
	var problems []string
	var ce *config.Error
	if err := cfg.Validate(); errors.As(err, &ce) {
		for _, p := range ce.Problems {
			if strings.HasPrefix(p, "smtp.") {
				problems = append(problems, p)
			}
		}
	}
	if len(problems) == 0 {
		var se *mail.SendError
		if err := set.Validate(); errors.As(err, &se) {
			problems = append(problems, string(se.Class)+": "+se.Hint())
		} else if err != nil {
			problems = append(problems, "the smtp settings are not valid")
		}
	}
	if len(problems) > 0 {
		rep("FAIL", "mail config", strings.Join(problems, "; "))
		rep("NOT CHECKED", "mail relay", "the smtp settings are not valid (see mail config)")
		return
	}
	where := net.JoinHostPort(set.Host, strconv.Itoa(set.Port))
	signIn := "no sign-in (smtp.username is empty)"
	if set.Username != "" {
		signIn = "sign-in with smtp.username and smtp.password_file"
	}
	rep("OK", "mail config", fmt.Sprintf("relay %s, security %s, %s", where, set.Security, signIn))

	if set.Username == "" {
		rep("OK", "mail password file", "not used: without smtp.username the relay is used without signing in")
	} else if !checkMailPasswordFile(set.PasswordFile, rep) {
		rep("NOT CHECKED", "mail relay", "the password file cannot be used (see mail password file)")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), doctorProbeTimeout)
	err := probeMail(ctx, set)
	cancel()
	if err != nil {
		var se *mail.SendError
		if !errors.As(err, &se) {
			rep("FAIL", "mail relay", where+": the probe failed")
			return
		}
		rep("FAIL", "mail relay", fmt.Sprintf("%s: failed at %s: %s. %s", where, se.Stage, se.Class, mail.Hint(se.Class)))
		return
	}
	var how string
	switch set.Security {
	case "starttls":
		how = "STARTTLS"
	case "tls":
		how = "implicit TLS"
	default:
		how = "no TLS (smtp.allow_plaintext)"
	}
	done := "signed in"
	if set.Username == "" {
		done = "no sign-in configured"
	}
	rep("OK", "mail relay", fmt.Sprintf("%s: connected, greeted, %s, %s, quit; no message sent", where, how, done))
}

// checkMailPasswordFile checks smtp.password_file as the sender will read it and reports whether
// it is usable. The path and the contents are never printed.
func checkMailPasswordFile(path string, rep func(level, name, detail string)) bool {
	const name = "mail password file"
	fail := func(detail string) bool { rep("FAIL", name, detail); return false }
	if path == "" {
		return fail("smtp.username is set but smtp.password_file is not")
	}
	fi, err := os.Stat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return fail("smtp.password_file does not exist")
	case err != nil:
		return fail("smtp.password_file cannot be examined by this user")
	case !fi.Mode().IsRegular():
		return fail("smtp.password_file is not a regular file")
	}
	if f, err := os.Open(path); err != nil {
		return fail("smtp.password_file is not readable by this user")
	} else {
		_ = f.Close()
	}
	if fi.Size() == 0 {
		return fail("smtp.password_file is empty")
	}
	windows := runtime.GOOS == "windows"
	if !windows && fi.Mode().Perm()&0o077 != 0 {
		// The sender refuses such a file (class password_file_mode), so no mail would be sent.
		return fail(fmt.Sprintf("smtp.password_file is open to group or others (mode %04o): the sender refuses it, so no mail is sent; chmod 600 it", fi.Mode().Perm()))
	}
	if _, err := mail.ReadPasswordFile(path); err != nil {
		// What the sender refuses beyond the checks above: more than one line, a line end only,
		// a NUL, more than 4 KiB.
		return fail("smtp.password_file cannot be used: it must hold the password on one line")
	}
	if windows {
		rep("OK", name, "readable, one line (Windows has no file mode to check: protect the file with its ACL, readable by the service account only)")
	} else {
		rep("OK", name, fmt.Sprintf("readable, one line, mode %04o", fi.Mode().Perm()))
	}
	return true
}

// checkMailRecipients counts the recipients with each switch and warns where mail would reach
// nobody. smtpOn is whether an smtp block is present.
func checkMailRecipients(smtpOn bool, st *store.Store, dbWhy string, rep func(level, name, detail string)) {
	if st == nil {
		rep("NOT CHECKED", "mail recipients", dbWhy)
		return
	}
	ctx := context.Background()
	rs, err := st.ListRecipients(ctx, store.ScopeAll())
	if err != nil {
		rep("FAIL", "mail recipients", "cannot be read from the database")
		return
	}
	var alerts, recoveries, digest int
	for _, r := range rs {
		if r.Alerts {
			alerts++
		}
		if r.Recoveries {
			recoveries++
		}
		if r.Digest {
			digest++
		}
	}
	rep("OK", "mail recipients", fmt.Sprintf("%d in all: %d with alerts, %d with recoveries, %d with the digest", len(rs), alerts, recoveries, digest))
	if smtpOn && alerts == 0 {
		rep("WARN", "mail alerts", "no recipient has the alerts switch: an alarm sends no e-mail")
	}
	ds, err := st.ReadDigestSettings(ctx, store.ScopeAll())
	switch {
	case err != nil:
		rep("WARN", "mail digest", "the digest_enabled setting cannot be read as true or false: the console reports the digest as not checked")
	case !ds.Enabled:
		rep("OK", "mail digest", "off (digest_enabled is false)")
	case digest == 0:
		rep("WARN", "mail digest", "the daily digest is on and no recipient has the digest switch: nobody receives it")
	default:
		rep("OK", "mail digest", "on")
	}
}

// checkHeartbeat reports the outbound heartbeat ping (healthcheck_ping): whether a URL is
// configured, never the URL itself (it often carries the monitor's token): scheme and host only,
// with the method and interval. Doctor does not send the ping: a ping tells the outside monitor
// that the console is healthy, which doctor cannot vouch for.
func checkHeartbeat(cfg config.Config, report func(level, name, detail string)) {
	const name = "heartbeat ping"
	hp := cfg.HealthcheckPing
	if hp.URL == "" {
		report("OK", name, fmt.Sprintf("off: healthcheck_ping.url is empty, nothing is sent (interval_s %d when a URL is set)", hp.IntervalS))
		return
	}
	u, err := url.Parse(hp.URL)
	if err != nil || !config.HeartbeatURLAllowed(hp.URL) {
		report("NOT CHECKED", name, "healthcheck_ping.url is not valid (see config)")
		return
	}
	host := u.Hostname()
	if strings.Contains(host, ":") {
		host = "[" + host + "]" // an IPv6 literal, written as in a URL
	}
	report("NOT CHECKED", name, fmt.Sprintf("configured: %s://%s, %s every %d s; doctor does not ping it (a ping would tell the outside monitor the console is healthy)",
		u.Scheme, host, hp.Method, hp.IntervalS))
}
