package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/enroll"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

func runToken(args []string, io IO) int {
	fs := newFlags("token", io)
	cfgPath := fs.String("config", "config.json", "path of config.json")
	slug := fs.String("customer", "", "slug of the customer the token enrolls into")
	create := fs.String("create-customer", "", "create the customer with this name first (M0 has no customers page)")
	pol := fs.String("policy", "", "policy template name or id; applied at enrollment as each new device's override, on top of the customer's policy")
	expires := fs.String("expires", "7d", "token lifetime, for example 12h, 7d, 2w")
	uses := fs.Int("uses", 0, "maximum number of enrollments (0 = unlimited)")
	label := fs.String("label", "", "label shown in the token list")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	if (*slug == "") == (*create == "") {
		fmt.Fprintln(io.Stderr, "cryo-console token: give exactly one of --customer <slug> and --create-customer <name>")
		return ExitUsage
	}
	ttl, err := enroll.ParseDuration(*expires)
	if err != nil {
		fmt.Fprintln(io.Stderr, "cryo-console token:", err)
		return ExitUsage
	}
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintln(io.Stderr, "cryo-console token:", sanitize(err))
		return ExitFailed
	}
	ctx := context.Background()
	st, err := store.Open(ctx, store.Options{Path: cfg.DBPath})
	if err != nil {
		fmt.Fprintln(io.Stderr, "cryo-console token: cannot open the database:", sanitize(err))
		return ExitFailed
	}
	defer st.Close()

	var cust store.Customer
	if *create != "" {
		target, err := st.DefaultStorageTarget(ctx)
		if err != nil {
			fmt.Fprintln(io.Stderr, "cryo-console token: no storage target; run cryo-console init first")
			return ExitFailed
		}
		cust, err = st.CreateCustomer(ctx, store.ScopeAll(), store.Customer{Name: strings.TrimSpace(*create), StorageTargetID: target.ID, KeyCustody: "escrowed", Expected: true})
		if err != nil {
			fmt.Fprintln(io.Stderr, "cryo-console token: cannot create the customer:", sanitize(err))
			return ExitFailed
		}
		if _, err := st.AppendAudit(ctx, store.AuditEvent{ActorType: "cli", Method: "cli", Action: "customer.create", ObjectType: "customer", ObjectID: cust.ID, CustomerID: cust.ID, Result: "ok"}); err != nil {
			// Nothing more is done on an audit log that does not take rows.
			return notAudited(io, "token", fmt.Sprintf("customer %q was created (slug %s) and no token was minted", cust.Name, cust.Slug), err)
		}
	} else {
		cs, err := st.ListCustomers(ctx, store.ScopeAll())
		if err != nil {
			fmt.Fprintln(io.Stderr, "cryo-console token:", sanitize(err))
			return ExitFailed
		}
		for _, c := range cs {
			if c.Slug == *slug {
				cust = c
			}
		}
		if cust.ID == "" {
			fmt.Fprintln(io.Stderr, "cryo-console token: no customer with that slug (use --create-customer to make one)")
			return ExitFailed
		}
	}
	policyID := ""
	if *pol != "" {
		ps, err := st.ListPolicies(ctx, store.ScopeAll())
		if err != nil {
			fmt.Fprintln(io.Stderr, "cryo-console token:", sanitize(err))
			return ExitFailed
		}
		for _, p := range ps {
			if p.ID == *pol || p.Name == *pol {
				policyID = p.ID
			}
		}
		if policyID == "" {
			fmt.Fprintln(io.Stderr, "cryo-console token: no policy with that name or id")
			return ExitFailed
		}
	}
	opts := enroll.MintOptions{Label: *label, PolicyID: policyID, Expires: ttl}
	if *uses > 0 {
		opts.MaxUses = uses
	}
	plain, tok, err := enroll.Mint(ctx, st, store.ScopeAll(), cust.ID, opts)
	if err != nil {
		if errors.Is(err, store.ErrInvalid) {
			fmt.Fprintln(io.Stderr, "cryo-console token: the token was rejected (policy must be a template or belong to the customer)")
		} else {
			fmt.Fprintln(io.Stderr, "cryo-console token:", sanitize(err))
		}
		return ExitFailed
	}
	if _, err := st.AppendAudit(ctx, store.AuditEvent{ActorType: "cli", Method: "cli", Action: "enrollment_token.create", ObjectType: "enrollment_token", ObjectID: tok.ID, CustomerID: cust.ID, Result: "ok"}); err != nil {
		// The token exists but is not shown: nobody holds it, and it expires unused.
		return notAudited(io, "token", fmt.Sprintf("enrollment token %s was created for %q; it is not shown and expires unused at %s (mint another once the audit log works)",
			tok.ID, cust.Name, tok.ExpiresAt.UTC().Format(time.RFC3339)), err)
	}
	fmt.Fprintf(io.Stdout, "Customer:  %s (slug %s)\nExpires:   %s\nUses:      %s\n\nEnrollment token (shown once, never stored):\n%s\n",
		cust.Name, cust.Slug, tok.ExpiresAt.UTC().Format(time.RFC3339), map[bool]string{true: "unlimited", false: fmt.Sprint(*uses)}[*uses <= 0], plain)
	return ExitOK
}
