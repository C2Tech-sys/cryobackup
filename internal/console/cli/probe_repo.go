package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/console/maintain"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

const probeRepoUsage = `usage: cryo-console probe-repo reset [--config config.json] [--confirm]

  reset  clear the storage probe repository marker (settings row storage_probe_repo, ADR 0017), so the
         maintenance worker creates a fresh probe repository on its next probe. Use it after an intentional
         move of the storage prefix or a new master key. Without --confirm it only prints what it would do.
         It never deletes anything on storage: remove the old probe repository yourself first.
`

// runProbeRepo is the recovery tool of ADR 0017. Following the operator's
// script rules it is a dry run by default and idempotent: --confirm deletes
// the marker and writes one audit row in the same transaction (an audit that
// fails rolls the change back), and a second run finds nothing to do.
func runProbeRepo(args []string, io IO) int {
	if len(args) == 0 || args[0] != "reset" {
		if len(args) > 0 && (args[0] == "help" || args[0] == "-h" || args[0] == "--help") {
			fmt.Fprint(io.Stdout, probeRepoUsage)
			return ExitOK
		}
		fmt.Fprint(io.Stderr, probeRepoUsage)
		return ExitUsage
	}
	fs := newFlags("probe-repo reset", io)
	cfgPath := fs.String("config", "config.json", "path of config.json")
	confirm := fs.Bool("confirm", false, "delete the marker (without it: a dry run that changes nothing)")
	if code, ok := parse(fs, args[1:]); !ok {
		return code
	}
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintln(io.Stderr, "cryo-console probe-repo:", sanitize(err))
		return ExitFailed
	}
	ctx := context.Background()
	st, err := store.Open(ctx, store.Options{Path: cfg.DBPath})
	if err != nil {
		fmt.Fprintln(io.Stderr, "cryo-console probe-repo: cannot open the database:", sanitize(err))
		return ExitFailed
	}
	defer st.Close()

	location := probeRepoLocation(ctx, st)
	_, gerr := st.GetSetting(ctx, maintain.SettingProbeRepo)
	switch {
	case errors.Is(gerr, store.ErrNotFound):
		fmt.Fprintln(io.Stdout, "no storage probe repository marker is set: nothing to reset.")
		fmt.Fprintf(io.Stdout, "the next probe creates the probe repository at %s if it does not exist.\n", location)
		return ExitOK
	case gerr != nil:
		fmt.Fprintln(io.Stderr, "cryo-console probe-repo:", sanitize(gerr))
		return ExitFailed
	}
	if !*confirm {
		fmt.Fprintln(io.Stdout, "dry run: would delete the settings row storage_probe_repo and write one audit row.")
		fmt.Fprintf(io.Stdout, "before you confirm, remove the old probe repository on storage yourself: %s\n", location)
		fmt.Fprintln(io.Stdout, "this command never deletes anything on storage. Re-run with --confirm to reset the marker.")
		return ExitOK
	}
	var auditErr error
	err = st.Write(ctx, func(tx *store.Tx) error {
		deleted, err := tx.DeleteSetting(maintain.SettingProbeRepo)
		if err != nil || !deleted {
			return err
		}
		_, auditErr = tx.AppendAudit(store.AuditEvent{ActorType: "cli", Method: "cli", Action: "storage_probe.reset", ObjectType: "setting",
			ObjectID: maintain.SettingProbeRepo, Result: "ok", Severity: "warning", Details: []byte(`{"storage_deleted":false}`)})
		return auditErr
	})
	switch {
	case err != nil && auditErr != nil:
		fmt.Fprintln(io.Stderr, "cryo-console probe-repo: the reset could not be audited, so it was not made:", sanitize(err))
		return ExitFailed
	case err != nil:
		fmt.Fprintln(io.Stderr, "cryo-console probe-repo:", sanitize(err))
		return ExitFailed
	}
	fmt.Fprintln(io.Stdout, "storage probe repository marker deleted (audited).")
	fmt.Fprintf(io.Stdout, "nothing was deleted on storage: if the old probe repository is still there, remove it yourself: %s\n", location)
	fmt.Fprintln(io.Stdout, "the next probe creates a fresh probe repository there.")
	return ExitOK
}

// probeRepoLocation is <bucket>/<prefix>/probe as the maintenance worker
// addresses it: CRYO_MAINTAIN_BUCKET and CRYO_MAINTAIN_PREFIX when set (the
// worker's own override), else the default storage target. Only the configured
// bucket and prefix are used.
func probeRepoLocation(ctx context.Context, st *store.Store) string {
	bucket, prefix := os.Getenv("CRYO_MAINTAIN_BUCKET"), os.Getenv("CRYO_MAINTAIN_PREFIX")
	if bucket == "" {
		if tgt, err := st.DefaultStorageTarget(ctx); err == nil {
			bucket, prefix = tgt.Bucket, tgt.Prefix
		}
	}
	if bucket == "" {
		return "<bucket>/<prefix>/" + adminpath.ProbeRepoID + " (no default storage target is configured)"
	}
	loc := strings.Trim(bucket, "/")
	if p := strings.Trim(prefix, "/"); p != "" {
		loc += "/" + p
	}
	return loc + "/" + adminpath.ProbeRepoID
}
