package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/c2tech-sys/cryobackup/internal/console/maintain"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

const probeRepoUsage = `usage: cryo-console probe-repo reset [--config config.json] [--confirm]

  reset  clear the storage probe repository marker (settings row storage_probe_repo, ADR 0017), so the
         maintenance worker creates a fresh probe repository on its next probe. Use it after an intentional
         move of the storage prefix or a new master key. Without --confirm it only prints what it would do.
         It never deletes anything on storage: remove the old probe repository yourself first.
`

// unknownLocation is said instead of a path when the marker does not record
// where the worker put the probe repository. The worker takes its location from
// its own environment (CRYO_MAINTAIN_BUCKET, CRYO_MAINTAIN_PREFIX), else the
// default storage target; this command runs in another process and cannot see
// that environment, so it never prints a path it would have to guess.
const unknownLocation = "the probe repository's location is not recorded in the marker (it predates M3-INT), and this command cannot read the maintenance worker's environment: " +
	"it is <bucket>/<prefix>/probe, where bucket and prefix are the worker's CRYO_MAINTAIN_BUCKET and CRYO_MAINTAIN_PREFIX, or the default storage target's when those are unset"

// probeRepoBeforeReset, when set, runs between reading the marker and deleting
// it (tests: another reset that wins the race).
var probeRepoBeforeReset func(*store.Store)

// runProbeRepo is the recovery tool of ADR 0017. Following the operator's
// script rules it is a dry run by default and idempotent: --confirm deletes
// the marker and writes one audit row in the same transaction (an audit that
// fails rolls the change back), and a second run finds nothing to do. The
// probe repository's location is the one the worker recorded in the marker.
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

	raw, gerr := st.GetSetting(ctx, maintain.SettingProbeRepo)
	switch {
	case errors.Is(gerr, store.ErrNotFound):
		fmt.Fprintln(io.Stdout, "no storage probe repository marker is set: nothing to reset.")
		fmt.Fprintln(io.Stdout, "the next probe creates the probe repository under the maintenance worker's bucket and prefix if it does not exist.")
		return ExitOK
	case gerr != nil:
		fmt.Fprintln(io.Stderr, "cryo-console probe-repo:", sanitize(gerr))
		return ExitFailed
	}
	location := markerLocation(raw.Value)
	if !*confirm {
		fmt.Fprintln(io.Stdout, "dry run: would delete the settings row storage_probe_repo and write one audit row.")
		fmt.Fprintln(io.Stdout, "before you confirm, remove the old probe repository on storage yourself: "+location)
		fmt.Fprintln(io.Stdout, "this command never deletes anything on storage. Re-run with --confirm to reset the marker.")
		return ExitOK
	}
	if probeRepoBeforeReset != nil {
		probeRepoBeforeReset(st)
	}
	var auditErr error
	deleted := false
	err = st.Write(ctx, func(tx *store.Tx) error {
		var err error
		deleted, err = tx.DeleteSetting(maintain.SettingProbeRepo)
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
	case !deleted:
		// Gone between the read and the delete: another reset (or the operator by hand) removed it.
		fmt.Fprintln(io.Stdout, "the storage probe repository marker was already gone when this run went to delete it: nothing was deleted and nothing was audited.")
		return ExitOK
	}
	fmt.Fprintln(io.Stdout, "storage probe repository marker deleted (audited).")
	fmt.Fprintln(io.Stdout, "nothing was deleted on storage: if the old probe repository is still there, remove it yourself: "+location)
	fmt.Fprintln(io.Stdout, "the next probe creates a fresh probe repository under the maintenance worker's bucket and prefix.")
	return ExitOK
}

// markerLocation is the probe repository's location as the worker recorded it
// in the marker, or unknownLocation when the marker does not say (an older
// marker, or one that does not parse).
func markerLocation(raw []byte) string {
	var m maintain.ProbeRepoMarker
	if json.Unmarshal(raw, &m) != nil {
		return unknownLocation
	}
	if loc, ok := m.Location(); ok {
		return loc
	}
	return unknownLocation
}
