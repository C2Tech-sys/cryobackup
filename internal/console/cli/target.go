package cli

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

const targetUsage = `usage: cryo-console target <list|set> [flags]

  list  print every storage target with its fleet cap and live start leases
  set   --max-concurrent-backups N [--name <target name>]
        set the fleet cap of a target (the default target without --name):
        at most N scheduled backups of its devices run at once; 0 = unlimited.
        Operator backup_now runs are never capped (ADR 0005).
`

// runTarget is the storage-target edit path of M1: the fleet concurrency cap
// (storage_targets.max_concurrent_backups, ADR 0005). Targets are created by
// init; this command reads them and sets the cap.
func runTarget(args []string, io IO) int {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		fmt.Fprint(io.Stderr, targetUsage)
		return ExitUsage
	}
	sub, rest := args[0], args[1:]
	fs := newFlags("target "+sanitizeToken(sub), io)
	cfgPath := fs.String("config", "config.json", "path of config.json")
	var name *string
	var capN *int
	switch sub {
	case "list":
	case "set":
		name = fs.String("name", "", "name of the storage target (default: the default target)")
		capN = fs.Int("max-concurrent-backups", -1, "fleet cap: live start leases at most (0 = unlimited)")
	case "help", "-h", "--help":
		fmt.Fprint(io.Stdout, targetUsage)
		return ExitOK
	default:
		fmt.Fprintf(io.Stderr, "cryo-console target: unknown subcommand %q\n\n%s", sanitizeToken(sub), targetUsage)
		return ExitUsage
	}
	if code, ok := parse(fs, rest); !ok {
		return code
	}
	if capN != nil && *capN < 0 {
		fmt.Fprintln(io.Stderr, "cryo-console target set: --max-concurrent-backups N is required (0 = unlimited)")
		return ExitUsage
	}
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintln(io.Stderr, "cryo-console target:", sanitize(err))
		return ExitFailed
	}
	ctx := context.Background()
	st, err := store.Open(ctx, store.Options{Path: cfg.DBPath})
	if err != nil {
		fmt.Fprintln(io.Stderr, "cryo-console target: cannot open the database:", sanitize(err))
		return ExitFailed
	}
	defer st.Close()

	if sub == "list" {
		ts, err := st.ListStorageTargets(ctx)
		if err != nil {
			fmt.Fprintln(io.Stderr, "cryo-console target:", sanitize(err))
			return ExitFailed
		}
		if len(ts) == 0 {
			fmt.Fprintln(io.Stdout, "no storage targets (run cryo-console init)")
			return ExitOK
		}
		for _, tg := range ts {
			leases, err := st.ListStartLeases(ctx, tg.ID, st.Now())
			if err != nil {
				fmt.Fprintln(io.Stderr, "cryo-console target:", sanitize(err))
				return ExitFailed
			}
			def := ""
			if tg.IsDefault {
				def = " (default)"
			}
			fmt.Fprintf(io.Stdout, "%s%s  id %s  max_concurrent_backups %s  live leases %d\n",
				tg.Name, def, tg.ID, capText(tg.MaxConcurrentBackups), len(leases))
		}
		return ExitOK
	}

	target, err := findTarget(ctx, st, *name)
	if err != nil {
		fmt.Fprintln(io.Stderr, "cryo-console target set:", err)
		return ExitFailed
	}
	// The cap and its audit row are one transaction: an audit that fails rolls
	// the change back, so an unaudited cap can never be left in place.
	var (
		before   int
		updated  store.StorageTarget
		auditErr error
	)
	err = st.Write(ctx, func(tx *store.Tx) error {
		var err error
		if before, updated, err = tx.SetStorageTargetMaxConcurrentBackups(target.ID, *capN); err != nil {
			return err
		}
		_, auditErr = tx.AppendAudit(store.AuditEvent{ActorType: "cli", Method: "cli", Action: "storage_target.update", ObjectType: "storage_target",
			ObjectID: updated.ID, Result: "ok",
			Details: []byte(`{"max_concurrent_backups":{"from":` + strconv.Itoa(before) + `,"to":` + strconv.Itoa(updated.MaxConcurrentBackups) + `}}`)})
		return auditErr
	})
	switch {
	case err != nil && auditErr != nil:
		fmt.Fprintln(io.Stderr, "cryo-console target set: the change could not be audited, so it was not made:", sanitize(err))
		fmt.Fprintf(io.Stderr, "cryo-console target set: %s: max_concurrent_backups is unchanged (%s)\n", target.Name, capText(target.MaxConcurrentBackups))
		return ExitFailed
	case err != nil:
		fmt.Fprintln(io.Stderr, "cryo-console target set:", sanitize(err))
		return ExitFailed
	}
	fmt.Fprintf(io.Stdout, "%s: max_concurrent_backups %s (was %s)\n", updated.Name, capText(updated.MaxConcurrentBackups), capText(before))
	return ExitOK
}

// findTarget returns the target with that name (case-insensitive), or the
// default target when name is empty.
func findTarget(ctx context.Context, st *store.Store, name string) (store.StorageTarget, error) {
	if strings.TrimSpace(name) == "" {
		t, err := st.DefaultStorageTarget(ctx)
		if errors.Is(err, store.ErrNotFound) {
			return t, errors.New("no default storage target; give --name")
		}
		return t, sanitize(err)
	}
	ts, err := st.ListStorageTargets(ctx)
	if err != nil {
		return store.StorageTarget{}, sanitize(err)
	}
	for _, t := range ts {
		if strings.EqualFold(t.Name, strings.TrimSpace(name)) {
			return t, nil
		}
	}
	return store.StorageTarget{}, errors.New("no storage target with that name (see cryo-console target list)")
}

func capText(n int) string {
	if n == 0 {
		return "0 (unlimited)"
	}
	return strconv.Itoa(n)
}
