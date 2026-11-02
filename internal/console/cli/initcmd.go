package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/config"
	"github.com/c2tech-sys/cryobackup/internal/console/escrow"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/policy"
)

type initOpts struct {
	configPath, dataDir, tz                                 string
	storageName, provider, endpoint, region, bucket, prefix string
	gatewayURL, credentialRef, owner, ownerEmail, publicURL string
	agentListen, adminListen, tlsCert, tlsKey               string
	adminTLSCert, adminTLSKey                               string
	nonInteractive, keyCopyConfirmed, force                 bool
	maxConcurrent                                           int
}

func runInit(args []string, io IO) int {
	var o initOpts
	fs := newFlags("init", io)
	fs.StringVar(&o.configPath, "config", "config.json", "where to write config.json")
	fs.StringVar(&o.dataDir, "data-dir", "", "data directory (database, master key, artifacts)")
	fs.StringVar(&o.tz, "timezone", "America/Chicago", "display time zone (IANA name)")
	fs.StringVar(&o.storageName, "storage-name", "Default storage", "name of the first storage target")
	fs.StringVar(&o.provider, "provider", "s3_compatible", "provider: wasabi | backblaze_b2 | idrive_e2 | aws_s3 | s3_compatible")
	fs.StringVar(&o.endpoint, "endpoint", "", "S3 endpoint host of the storage target")
	fs.StringVar(&o.region, "region", "", "S3 region (optional)")
	fs.StringVar(&o.bucket, "bucket", "", "bucket name")
	fs.StringVar(&o.prefix, "prefix", "cryobackup", "key prefix inside the bucket")
	fs.StringVar(&o.gatewayURL, "gateway-url", "", "public https URL of the gateway agents upload to")
	fs.StringVar(&o.credentialRef, "credential-ref", "CRYO_S3_ADMIN_CREDENTIALS", "NAME of the secret holding the admin credential (never the credential)")
	fs.IntVar(&o.maxConcurrent, "max-concurrent-backups", 0, "fleet cap of the first storage target: scheduled backups running at once (0 = unlimited; change later with cryo-console target set)")
	fs.StringVar(&o.owner, "owner", "owner", "username of the first owner account")
	fs.StringVar(&o.ownerEmail, "owner-email", "", "e-mail of the first owner account")
	fs.StringVar(&o.publicURL, "public-url", "", "public URL of the agent listener, for example https://backup.example.com:8443")
	fs.StringVar(&o.agentListen, "agent-listen", ":8443", "agent listener address")
	fs.StringVar(&o.adminListen, "admin-listen", "127.0.0.1:8080", "admin listener address")
	fs.StringVar(&o.tlsCert, "tls-cert", "", "agent listener certificate file")
	fs.StringVar(&o.tlsKey, "tls-key", "", "agent listener private key file")
	fs.StringVar(&o.adminTLSCert, "admin-tls-cert", "", "admin listener certificate file (optional; serves the admin UI over TLS)")
	fs.StringVar(&o.adminTLSKey, "admin-tls-key", "", "admin listener private key file")
	fs.BoolVar(&o.nonInteractive, "non-interactive", false, "never prompt; every value comes from flags")
	fs.BoolVar(&o.keyCopyConfirmed, "master-key-copy-confirmed", false, "confirm that an offline copy of the master key is (or will be) stored; required with --non-interactive")
	fs.BoolVar(&o.force, "force", false, "overwrite an existing config.json")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	if o.maxConcurrent < 0 {
		fmt.Fprintln(io.Stderr, "cryo-console init: --max-concurrent-backups must not be negative (0 = unlimited)")
		return ExitUsage
	}
	in := bufio.NewReader(io.Stdin)
	ask := func(label, def string, dst *string) {
		if o.nonInteractive || *dst != "" && *dst != def {
			return
		}
		if def != "" {
			fmt.Fprintf(io.Stdout, "%s [%s]: ", label, def)
		} else {
			fmt.Fprintf(io.Stdout, "%s: ", label)
		}
		line, _ := in.ReadString('\n')
		line = strings.TrimSpace(line)
		if line == "" {
			line = def
		}
		*dst = line
	}
	if !o.nonInteractive {
		fmt.Fprintln(io.Stdout, "CryoBackup first-run setup. Press Enter to accept a default.")
	}
	ask("Data directory", "./data", &o.dataDir)
	if o.dataDir == "" {
		o.dataDir = "./data"
	}
	ask("Display time zone", "America/Chicago", &o.tz)
	ask("Storage target name", "Default storage", &o.storageName)
	ask("Storage provider", "s3_compatible", &o.provider)
	ask("S3 endpoint host", "", &o.endpoint)
	ask("Bucket", "", &o.bucket)
	ask("Prefix", "cryobackup", &o.prefix)
	ask("Gateway URL (https)", "", &o.gatewayURL)
	ask("Owner username", "owner", &o.owner)
	ask("Public URL of the agent listener", "", &o.publicURL)
	ask("TLS certificate file (empty for none yet)", "", &o.tlsCert)
	ask("TLS key file", "", &o.tlsKey)

	if fileExists(o.configPath) && !o.force {
		fmt.Fprintln(io.Stderr, "cryo-console init: config.json already exists; use --force to overwrite it")
		return ExitFailed
	}
	if o.endpoint == "" || o.bucket == "" {
		fmt.Fprintln(io.Stderr, "cryo-console init: --endpoint and --bucket are required")
		return ExitUsage
	}
	if _, err := time.LoadLocation(o.tz); err != nil {
		fmt.Fprintln(io.Stderr, "cryo-console init: the time zone is not an IANA name")
		return ExitUsage
	}
	if (o.adminTLSCert != "") != (o.adminTLSKey != "") {
		fmt.Fprintln(io.Stderr, "cryo-console init: --admin-tls-cert and --admin-tls-key must be given together")
		return ExitUsage
	}
	if o.ownerEmail == "" {
		o.ownerEmail = o.owner + "@localhost.invalid"
	}
	if _, err := mail.ParseAddress(o.ownerEmail); err != nil {
		fmt.Fprintln(io.Stderr, "cryo-console init: --owner-email is not an e-mail address")
		return ExitUsage
	}

	cfg := config.Default()
	cfg.DataDir, cfg.DisplayTimezone, cfg.PublicURL, cfg.GatewayURL = o.dataDir, o.tz, o.publicURL, o.gatewayURL
	cfg.AgentListen, cfg.AdminListen = o.agentListen, o.adminListen
	cfg.TLS = config.TLS{Cert: o.tlsCert, Key: o.tlsKey}
	cfg.Admin.TLS = config.TLS{Cert: o.adminTLSCert, Key: o.adminTLSKey}
	cfg.Resolve()
	abs := func(p string) string {
		if a, err := filepath.Abs(p); err == nil {
			return a
		}
		return p
	}
	cfg.DataDir, cfg.DBPath, cfg.MasterKeyPath, cfg.ArtifactsDir, cfg.AdminpathSocket, cfg.GatewayAuthDir =
		abs(cfg.DataDir), abs(cfg.DBPath), abs(cfg.MasterKeyPath), abs(cfg.ArtifactsDir), abs(cfg.AdminpathSocket), abs(cfg.GatewayAuthDir)

	for _, d := range []string{cfg.DataDir, cfg.ArtifactsDir, cfg.GatewayAuthDir} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			fmt.Fprintln(io.Stderr, "cryo-console init: cannot create the data directories")
			return ExitFailed
		}
	}
	// The master key: created once, never overwritten.
	var mk *escrow.MasterKey
	created := false
	if fileExists(cfg.MasterKeyPath) {
		k, err := escrow.LoadMasterKey(cfg.MasterKeyPath)
		if err != nil {
			fmt.Fprintln(io.Stderr, "cryo-console init: the existing master key file is unreadable:", err)
			return ExitFailed
		}
		mk = k
	} else {
		k, err := escrow.GenerateMasterKey(cfg.MasterKeyPath, nil)
		if err != nil {
			fmt.Fprintln(io.Stderr, "cryo-console init:", err)
			return ExitFailed
		}
		mk, created = k, true
	}
	fmt.Fprintf(io.Stdout, "\nMaster key %s (id %s).\n", map[bool]string{true: "created", false: "found"}[created], mk.KeyID())
	fmt.Fprintln(io.Stdout, "Every escrowed repository password is wrapped under this key. If the file is lost, those passwords are unrecoverable.")
	fmt.Fprintln(io.Stdout, "Store an offline copy of the master key file in a safe place (it is not in the database, and the self-backup excludes it).")
	if !o.keyCopyConfirmed {
		if o.nonInteractive {
			fmt.Fprintln(io.Stderr, "cryo-console init: pass --master-key-copy-confirmed once an offline copy is stored; setup is not finished until the operator confirms")
			return ExitFailed
		}
		fmt.Fprint(io.Stdout, "Type yes when an offline copy is stored: ")
		line, _ := in.ReadString('\n')
		if strings.ToLower(strings.TrimSpace(line)) != "yes" {
			fmt.Fprintln(io.Stderr, "cryo-console init: not confirmed; run init again once a copy is stored (the key and database already exist)")
			return ExitFailed
		}
	}

	ctx := context.Background()
	st, err := store.Open(ctx, store.Options{Path: cfg.DBPath})
	if err != nil {
		fmt.Fprintln(io.Stderr, "cryo-console init: cannot open the database:", sanitize(err))
		return ExitFailed
	}
	defer st.Close()
	if _, err := st.DefaultPolicy(ctx); errors.Is(err, store.ErrNotFound) {
		sections, err := store.PolicySections(policy.Default(policy.ClassUnknown))
		if err == nil {
			_, err = st.CreatePolicy(ctx, store.ScopeAll(), store.PolicyInput{Scope: "default", Name: "Default", Sections: sections, Note: "created by init"})
		}
		if err != nil {
			fmt.Fprintln(io.Stderr, "cryo-console init: cannot create the default policy:", sanitize(err))
			return ExitFailed
		}
	}
	if _, err := st.DefaultStorageTarget(ctx); errors.Is(err, store.ErrNotFound) {
		_, err := st.CreateStorageTarget(ctx, store.StorageTarget{Name: o.storageName, Provider: o.provider, Endpoint: o.endpoint, Region: o.region, Bucket: o.bucket,
			Prefix: o.prefix, GatewayURL: o.gatewayURL, CredentialRef: o.credentialRef, IsDefault: true, MaxConcurrentBackups: o.maxConcurrent})
		if err != nil {
			fmt.Fprintln(io.Stderr, "cryo-console init: cannot create the storage target:", sanitize(err))
			return ExitFailed
		}
	}
	tz, _ := json.Marshal(o.tz)
	if err := st.PutOperatorSetting(ctx, store.SettingDisplayTimezone, tz, ""); err != nil { // the zone init stores is the operator's choice (ADR 0012)
		fmt.Fprintln(io.Stderr, "cryo-console init: cannot store the time zone:", sanitize(err))
		return ExitFailed
	}
	password := ""
	if _, err := st.GetAdminByUsername(ctx, o.owner); errors.Is(err, store.ErrNotFound) {
		password, _ = auth.GeneratePassword(20)
		h, err := auth.HashPassword(password)
		if err == nil {
			_, err = st.CreateAdmin(ctx, store.Admin{Username: o.owner, Email: o.ownerEmail, Role: "owner", PasswordHash: h})
		}
		if err != nil {
			fmt.Fprintln(io.Stderr, "cryo-console init: cannot create the owner account:", sanitize(err))
			return ExitFailed
		}
	}
	// The audit row is the last database write. If it fails, init still
	// writes the configuration (the database is set up), but it fails loudly
	// (notAudited) and does not show the owner's password.
	_, auditErr := st.AppendAudit(ctx, store.AuditEvent{ActorType: "cli", Method: "cli", Action: "console.init", Result: "ok"})

	raw, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(o.configPath, append(raw, '\n'), 0o600); err != nil {
		fmt.Fprintln(io.Stderr, "cryo-console init: cannot write config.json")
		return ExitFailed
	}
	fmt.Fprintf(io.Stdout, "\nConfiguration written (mode 0600). Database and storage target ready.\n")
	switch {
	case password != "" && auditErr != nil:
		fmt.Fprintf(io.Stdout, "\nOwner account: %s (its password is not shown: the setup could not be audited; set one with cryo-console user passwd once the audit log works)\n", o.owner)
	case password != "":
		fmt.Fprintf(io.Stdout, "\nOwner account: %s\nPassword (shown once, change it after signing in): %s\n", o.owner, password)
	default:
		fmt.Fprintf(io.Stdout, "\nOwner account %q already exists; its password is unchanged.\n", o.owner)
	}
	if cfg.TLS.Cert == "" {
		fmt.Fprintln(io.Stdout, "\nNo TLS certificate configured: serve needs tls.cert and tls.key (or --insecure-http-for-lab on a loopback or private bind).")
	}
	fmt.Fprintln(io.Stdout, "\nNext: cryo-console token --config <config.json> --create-customer \"Customer name\"")
	if auditErr != nil {
		return notAudited(io, "init", "the console was set up", auditErr)
	}
	return ExitOK
}

// notAudited reports a CLI change that was made but whose audit row could not
// be written (M1-FIX4 item 8): stderr says so, with NOT AUDITED, and the
// command exits ExitFailed. The change itself stands (it is reported, not
// undone), and no secret the unaudited change created is shown: a generated
// password or an enrollment token is never handed out without its audit row.
func notAudited(io IO, cmd, what string, err error) int {
	fmt.Fprintf(io.Stderr, "cryo-console %s: %s, but the change could not be audited (NOT AUDITED): %v\n", cmd, what, sanitize(err))
	return ExitFailed
}
