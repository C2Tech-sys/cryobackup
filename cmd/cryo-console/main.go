// Command cryo-console is the CryoBackup control plane: the web console and
// admin API, the public agent API, the maintenance worker and the admin CLI, all
// in one binary (ADR 0002 D1). This file is wiring only; the commands live in
// internal/console/cli.
//
// Commands:
//
//	init      first-run setup (data dir, database, master key, default policy, storage target, owner, config.json)
//	serve     run the console: agent API listener, admin listener, jobs
//	          (--config, --fixtures, --admin-addr, --agent-addr, --insecure-http-for-lab, --admin-allow-public)
//	target    list storage targets; set max_concurrent_backups (fleet start-lease cap, ADR 0005)
//	token     mint an enrollment token (--customer <slug> | --create-customer <name>, --policy, --expires, --uses)
//	user      add, passwd, list, disable admin accounts
//	maintain  server-side maintenance worker and admin-path socket
//	doctor    check configuration, database, master key, listener binds, worker
//	version   print the build stamp
//
// Every flag has a CRYO_* environment override through the config file's keys
// (internal/console/config). `cryo-console serve --fixtures` needs no config and
// no network: it serves a fictional fleet from an in-memory database.
package main

import (
	"os"

	"github.com/c2tech-sys/cryobackup/internal/console/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:], cli.IO{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr}))
}
