package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/c2tech-sys/cryobackup/internal/buildinfo"
	"github.com/c2tech-sys/cryobackup/internal/logx"
)

// Exit codes: 0 success, 1 a failed operation, 2 a usage error, 3 not implemented
// in this build.
const (
	ExitOK       = 0
	ExitFailed   = 1
	ExitUsage    = 2
	ExitNotImpl  = 3
	usageSummary = `usage: cryo-console <command> [flags]

commands:
  init      first-run setup: data directory, database, master key, default policy, storage target, owner account, config.json
  serve     run the console (agent API listener, admin listener, jobs)
  token     mint an enrollment token for a customer
  target    list storage targets; set a target's fleet cap (max_concurrent_backups)
  user      manage admin accounts (add, passwd, list, disable)
  maintain  server-side maintenance worker and admin-path socket
  doctor    check configuration, database, master key and listener binds
  version   print the build stamp
`
)

// IO is what a command reads and writes.
type IO struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// Usage writes the command summary.
func Usage(w io.Writer) { _, _ = io.WriteString(w, usageSummary) }

// Main dispatches a subcommand and returns the process exit code. "maintain"
// runs internal/console/maintain with the console's key unwrapper (maintain.go).
func Main(args []string, io IO) int {
	if len(args) == 0 {
		Usage(io.Stderr)
		return ExitUsage
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "version":
		fmt.Fprintln(io.Stdout, buildinfo.String("cryo-console"))
		return ExitOK
	case "init":
		return runInit(rest, io)
	case "serve":
		return runServe(rest, io)
	case "token":
		return runToken(rest, io)
	case "target":
		return runTarget(rest, io)
	case "user":
		return runUser(rest, io)
	case "doctor":
		return runDoctor(rest, io)
	case "maintain":
		return runMaintain(rest, io)
	case "help", "-h", "--help":
		Usage(io.Stdout)
		return ExitOK
	}
	fmt.Fprintf(io.Stderr, "cryo-console: unknown command %q\n\n", sanitizeToken(cmd))
	Usage(io.Stderr)
	return ExitUsage
}

func newFlags(name string, io IO) *flag.FlagSet {
	fs := flag.NewFlagSet("cryo-console "+name, flag.ContinueOnError)
	fs.SetOutput(io.Stderr)
	return fs
}

// parse runs fs.Parse and maps its outcome to (exit code, continue).
func parse(fs *flag.FlagSet, args []string) (int, bool) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK, false
		}
		return ExitUsage, false
	}
	return 0, true
}

// sanitize returns err without any path an os error would carry.
func sanitize(err error) error {
	if err == nil {
		return nil
	}
	if s, ok := logx.PathFreeError(err); ok {
		return errors.New(s)
	}
	return err
}

// sanitizeToken keeps [A-Za-z0-9_.-] for use in identifiers and messages.
func sanitizeToken(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		}
	}
	out := b.String()
	if len(out) > 40 {
		out = out[:40]
	}
	return out
}

func hostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Host
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
