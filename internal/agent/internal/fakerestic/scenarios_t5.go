package fakerestic

// Scenarios of ticket M1-T5. Their fixtures are hand-written files under
// internal/restic/testdata/handwritten (a path relative to Dir(), as the plan
// table of fakerestic.go reads them).
func init() {
	// A backup that read a tree whose error lines name files in every shape a
	// path takes (drive, UNC, shadow copy, quoted names, POSIX, unicode, plain
	// text): it finishes with a snapshot and exit 3. Every distinctive token of
	// the fixture starts with "ZQ7".
	backupPlans["hostile_paths"] = plan{"backup_ok.stdout.jsonl", "../handwritten/hostile_paths.stderr.jsonl", 3}

	// The gateway went away in the middle of the backup and stayed away until
	// restic's own retries ran out: progress, then restic's retry messages and
	// its final "Fatal" for a refused connection, exit 1, no summary.
	backupPlans["gateway_midrun"] = plan{"../handwritten/gateway_midrun.stdout.jsonl", "../handwritten/gateway_midrun.stderr.jsonl", 1}
}
