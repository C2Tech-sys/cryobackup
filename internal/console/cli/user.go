package cli

import (
	"bufio"
	"context"
	"fmt"
	"net/mail"
	"strings"

	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

func runUser(args []string, io IO) int {
	if len(args) == 0 {
		fmt.Fprintln(io.Stderr, "usage: cryo-console user <add|passwd|list|disable|reset-mfa> [flags]")
		return ExitUsage
	}
	sub, rest := args[0], args[1:]
	fs := newFlags("user "+sub, io)
	cfgPath := fs.String("config", "config.json", "path of config.json")
	username := fs.String("username", "", "account username")
	role := fs.String("role", "operator", "add: owner | admin | operator | read_only")
	email := fs.String("email", "", "add: e-mail address")
	scope := fs.String("customer-scope", "", "add: comma-separated customer ids an operator or read_only account may see (empty = all)")
	pwStdin := fs.Bool("password-stdin", false, "add, passwd: read the new password from standard input instead of generating one")
	reason := fs.String("reason", "", "reset-mfa: why (recorded in the audit row)")
	if sub != "add" && sub != "passwd" && sub != "list" && sub != "disable" && sub != "reset-mfa" {
		fmt.Fprintf(io.Stderr, "cryo-console user: unknown subcommand %q\n", sanitizeToken(sub))
		return ExitUsage
	}
	if code, ok := parse(fs, rest); !ok {
		return code
	}
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintln(io.Stderr, "cryo-console user:", sanitize(err))
		return ExitFailed
	}
	ctx := context.Background()
	st, err := store.Open(ctx, store.Options{Path: cfg.DBPath})
	if err != nil {
		fmt.Fprintln(io.Stderr, "cryo-console user: cannot open the database:", sanitize(err))
		return ExitFailed
	}
	defer st.Close()

	password := func() (string, bool) {
		if *pwStdin {
			line, _ := bufio.NewReader(io.Stdin).ReadString('\n')
			line = strings.TrimRight(line, "\r\n")
			if len(line) < 12 {
				fmt.Fprintln(io.Stderr, "cryo-console user: the password must be at least 12 characters")
				return "", false
			}
			return line, true
		}
		p, err := auth.GeneratePassword(20)
		return p, err == nil
	}
	audit := func(action, id string) error {
		_, err := st.AppendAudit(ctx, store.AuditEvent{ActorType: "cli", Method: "cli", Action: action, ObjectType: "admin", ObjectID: id, Result: "ok"})
		return err
	}
	// pwNote is what an unaudited add or passwd says about the password: one
	// given on stdin is the operator's; a generated one is not shown.
	pwNote := func() string {
		if *pwStdin {
			return "the password is the one given on standard input"
		}
		return "the generated password is not shown (run user passwd once the audit log works)"
	}

	switch sub {
	case "list":
		as, err := st.ListAdmins(ctx)
		if err != nil {
			fmt.Fprintln(io.Stderr, "cryo-console user:", sanitize(err))
			return ExitFailed
		}
		fmt.Fprintf(io.Stdout, "%-24s %-10s %-9s %s\n", "USERNAME", "ROLE", "STATE", "SCOPE")
		for _, a := range as {
			state, sc := "active", "all customers"
			if a.DisabledAt != nil {
				state = "disabled"
			} else if a.LockedUntil != nil && a.LockedUntil.After(st.Now()) {
				state = "locked"
			}
			if a.CustomerScope != nil {
				sc = fmt.Sprintf("%d customer(s)", len(a.CustomerScope))
			}
			fmt.Fprintf(io.Stdout, "%-24s %-10s %-9s %s\n", a.Username, a.Role, state, sc)
		}
		return ExitOK
	case "add":
		if *username == "" {
			fmt.Fprintln(io.Stderr, "cryo-console user add: --username is required")
			return ExitUsage
		}
		if _, ok := auth.ParseRole(*role); !ok {
			fmt.Fprintln(io.Stderr, "cryo-console user add: --role must be owner, admin, operator or read_only")
			return ExitUsage
		}
		if *email == "" {
			*email = *username + "@localhost.invalid"
		}
		if _, err := mail.ParseAddress(*email); err != nil {
			fmt.Fprintln(io.Stderr, "cryo-console user add: --email is not an e-mail address")
			return ExitUsage
		}
		pw, ok := password()
		if !ok {
			return ExitFailed
		}
		h, err := auth.HashPassword(pw)
		if err != nil {
			fmt.Fprintln(io.Stderr, "cryo-console user add:", err)
			return ExitFailed
		}
		a := store.Admin{Username: *username, Email: *email, Role: *role, PasswordHash: h}
		if *scope != "" {
			for _, id := range strings.Split(*scope, ",") {
				a.CustomerScope = append(a.CustomerScope, strings.TrimSpace(id))
			}
		}
		made, err := st.CreateAdmin(ctx, a)
		if err != nil {
			fmt.Fprintln(io.Stderr, "cryo-console user add: the account was rejected (duplicate username, or a scope on an owner or admin):", sanitize(err))
			return ExitFailed
		}
		if err := audit("admin.create", made.ID); err != nil {
			return notAudited(io, "user add", fmt.Sprintf("account %s was created with role %s; %s", made.Username, made.Role, pwNote()), err)
		}
		fmt.Fprintf(io.Stdout, "Account %s created with role %s.\n", made.Username, made.Role)
		if !*pwStdin {
			fmt.Fprintf(io.Stdout, "Password (shown once): %s\n", pw)
		}
		return ExitOK
	case "passwd":
		a, err := st.GetAdminByUsername(ctx, *username)
		if err != nil {
			fmt.Fprintln(io.Stderr, "cryo-console user passwd: no such account")
			return ExitFailed
		}
		pw, ok := password()
		if !ok {
			return ExitFailed
		}
		h, err := auth.HashPassword(pw)
		if err != nil {
			fmt.Fprintln(io.Stderr, "cryo-console user passwd:", sanitize(err))
			return ExitFailed
		}
		// The new password and its audit row are one transaction (M1-FIX4): an
		// audit that fails rolls the change back and the old password stays.
		// Like every password reset (M2-W1A), the same transaction ends the
		// account's sessions and revokes its API tokens.
		var auditErr error
		var ended, revoked int64
		err = st.Write(ctx, func(tx *store.Tx) error {
			if err := tx.UpdateAdminPassword(a.ID, h, !*pwStdin); err != nil {
				return err
			}
			var err error
			if ended, err = tx.DeleteAdminSessions(a.ID, ""); err != nil {
				return err
			}
			if revoked, err = tx.RevokeAdminAPITokens(a.ID); err != nil {
				return err
			}
			details := []byte(fmt.Sprintf(`{"sessions_ended":%d,"tokens_revoked":%d}`, ended, revoked))
			_, auditErr = tx.AppendAudit(store.AuditEvent{ActorType: "cli", Method: "cli", Action: "admin.password_reset", ObjectType: "admin", ObjectID: a.ID, Result: "ok",
				Details: details})
			return auditErr
		})
		switch {
		case err != nil && auditErr != nil:
			fmt.Fprintf(io.Stderr, "cryo-console user passwd: the change could not be audited, so it was not made: the password of %s is unchanged (NOT AUDITED): %v\n", a.Username, sanitize(err))
			return ExitFailed
		case err != nil:
			fmt.Fprintln(io.Stderr, "cryo-console user passwd:", sanitize(err))
			return ExitFailed
		}
		fmt.Fprintf(io.Stdout, "Password of %s changed; the failure counter and any lock are cleared; %d session(s) ended and %d API token(s) revoked.\n", a.Username, ended, revoked)
		if !*pwStdin {
			fmt.Fprintf(io.Stdout, "New password (shown once): %s\n", pw)
		}
		return ExitOK
	case "reset-mfa":
		// Break-glass for a lost authenticator (architecture §5.2 reset-mfa, F16):
		// the account's TOTP enrollment and recovery codes are removed and its
		// sessions end, so its next login goes through enrollment again. The change
		// and its audit row are one transaction: unauditable means not done.
		if *username == "" {
			fmt.Fprintln(io.Stderr, "cryo-console user reset-mfa: --username is required")
			return ExitUsage
		}
		a, err := st.GetAdminByUsername(ctx, *username)
		if err != nil {
			fmt.Fprintln(io.Stderr, "cryo-console user reset-mfa: no such account")
			return ExitFailed
		}
		err = st.Write(ctx, func(tx *store.Tx) error {
			return auth.ResetMFATx(tx, auth.Actor{Type: "cli", Method: "cli"}, a.ID, *reason)
		})
		if err != nil {
			fmt.Fprintf(io.Stderr, "cryo-console user reset-mfa: the reset could not be made and audited, so nothing changed for %s (NOT AUDITED): %v\n", a.Username, sanitize(err))
			return ExitFailed
		}
		fmt.Fprintf(io.Stdout, "MFA of %s reset: its authenticator and recovery codes are removed and its sessions ended. It sets up a new authenticator at its next sign-in.\n", a.Username)
		return ExitOK
	default: // disable
		a, err := st.GetAdminByUsername(ctx, *username)
		if err != nil {
			fmt.Fprintln(io.Stderr, "cryo-console user disable: no such account")
			return ExitFailed
		}
		if err = st.DisableAdmin(ctx, a.ID); err != nil {
			fmt.Fprintln(io.Stderr, "cryo-console user disable:", sanitize(err))
			return ExitFailed
		}
		if err := audit("admin.disable", a.ID); err != nil {
			return notAudited(io, "user disable", fmt.Sprintf("account %s was disabled and its sessions ended", a.Username), err)
		}
		fmt.Fprintf(io.Stdout, "Account %s disabled and its sessions ended.\n", a.Username)
		return ExitOK
	}
}
