package fakerestic

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"time"
)

// Restore scenarios of ticket P22-D (ADR 0027). The fake reads the arguments
// the agent built (--target, --include-file, --dry-run, the snapshot
// reference "<id>:/<L>") and acts like restic 0.19.1 on them: for each
// include-file line it undoes the agent's escaping ("[[]" -> "[", "[$]" ->
// "$") and creates the item under the target, a file when the last name has a
// dot, else a folder holding "inner.txt". A dry run writes nothing and only
// prints the summary.
const (
	// NothingFor names drive letters (as in the snapshot reference, e.g. "D")
	// whose restore matches nothing: exit 0, an empty summary.
	NothingFor = "CRYO_FAKE_NOTHING"
	// LinkTarget is a folder the "restore_link" scenario points a junction at,
	// inside the target, as restic does for a junction in the snapshot.
	LinkTarget = "CRYO_FAKE_LINK_TARGET"
	// DryBytes overrides the total_bytes a dry run reports.
	DryBytes = "CRYO_FAKE_DRY_BYTES"
	// FailFor names drive letters whose real (not dry) run writes half of its
	// items and then fails: the gateway went away (exit 1, no summary).
	FailFor = "CRYO_FAKE_FAIL_FOR"
)

func restore(scenario string, args []string) int {
	var target, include, snap string
	dry := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--target":
			i++
			target = args[i]
		case "--include-file":
			i++
			include = args[i]
		case "--dry-run":
			dry = true
		}
	}
	if len(args) > 0 {
		snap = args[len(args)-1]
	}
	letter := ""
	if i := strings.Index(snap, ":/"); i >= 0 {
		letter = snap[i+2:]
	}
	for _, l := range strings.Split(os.Getenv(NothingFor), ",") {
		if l != "" && l == letter {
			fmt.Println(`{"message_type":"summary"}`)
			return 0
		}
	}
	switch scenario {
	case "restore_auth":
		fmt.Fprintln(os.Stderr, `{"message_type":"exit_error","code":1,"message":"Fatal: unable to open repository at rest:https://gateway.example/dev/: unexpected HTTP response (401): 401 Unauthorized"}`)
		return 1
	case "restore_unreachable":
		fmt.Fprintln(os.Stderr, `{"message_type":"exit_error","code":1,"message":"Fatal: unable to open repository at rest:https://gateway.example/dev/: dial tcp 10.0.0.9:443: connect: connection refused"}`)
		return 1
	case "restore_nothing":
		fmt.Println(`{"message_type":"summary"}`)
		return 0
	}
	lines := includeLines(include)
	if dry {
		total := 0
		for _, l := range lines {
			total += len("restored:" + l)
		}
		if v := os.Getenv(DryBytes); v != "" {
			fmt.Printf(`{"message_type":"summary","total_files":%d,"files_restored":%d,"total_bytes":%s,"bytes_restored":%s}`+"\n", len(lines), len(lines), v, v)
			return 0
		}
		fmt.Printf(`{"message_type":"summary","total_files":%d,"files_restored":%d,"total_bytes":%d,"bytes_restored":%d}`+"\n", len(lines), len(lines), total, total)
		return 0
	}
	for _, l := range strings.Split(os.Getenv(FailFor), ",") {
		if l != "" && l == letter {
			if len(lines) > 0 {
				makeItem(target, lines[0])
			}
			fmt.Fprintln(os.Stderr, `{"message_type":"exit_error","code":1,"message":"Fatal: load(<data/0123456789>): dial tcp 10.0.0.9:443: connect: connection refused"}`)
			return 1
		}
	}
	switch scenario {
	case "restore_stall":
		// Never a status line, never an exit of its own: the watchdog or a
		// cancel must stop it.
		c := make(chan os.Signal, 1)
		signal.Notify(c, os.Interrupt)
		select {
		case <-c:
			return 130
		case <-time.After(2 * time.Minute):
			return 0
		}
	case "restore_slow":
		c := make(chan os.Signal, 1)
		signal.Notify(c, os.Interrupt)
		for i := 0; i < 600; i++ {
			fmt.Printf(`{"message_type":"status","percent_done":0.1,"total_files":10,"files_restored":%d,"total_bytes":1000,"bytes_restored":%d}`+"\n", i, i*10)
			select {
			case <-c:
				return 130
			case <-time.After(50 * time.Millisecond):
			}
		}
	}
	total := 0
	for _, l := range lines {
		total += makeItem(target, l)
	}
	exit := 0
	switch scenario {
	case "restore_link":
		if t := os.Getenv(LinkTarget); t != "" {
			_ = exec.Command("cmd.exe", "/c", "mklink", "/J", filepath.Join(target, "junction-from-snapshot"), t).Run()
		}
	case "restore_file_error":
		// One item could not be written: an error line with an absolute path,
		// exit 1, the rest restored (p22-spike.md:172).
		esc := strings.ReplaceAll(`\\?\`+filepath.Join(target, "PHI-sentinel-locked.docx"), `\`, `\\`)
		fmt.Fprintf(os.Stderr, `{"message_type":"error","error":{"message":"open %s: The process cannot access the file because it is being used by another process."},"during":"restore","item":"\\PHI-sentinel-locked.docx"}`+"\n", esc)
		fmt.Fprintln(os.Stderr, `{"message_type":"exit_error","code":1,"message":"Fatal: There were 1 errors"}`)
		exit = 1
	}
	fmt.Printf(`{"message_type":"summary","total_files":%d,"files_restored":%d,"total_bytes":%d,"bytes_restored":%d}`+"\n", len(lines)+1, len(lines)+1, total, total)
	return exit
}

func includeLines(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		l := sc.Text()
		if l == "" {
			continue
		}
		l = strings.ReplaceAll(l, "[$]", "$")
		l = strings.ReplaceAll(l, "[[]", "[")
		out = append(out, l)
	}
	return out
}

// makeItem creates one include-file item under target and returns its byte count.
func makeItem(target, line string) int {
	p := filepath.Join(target, filepath.FromSlash(strings.TrimPrefix(line, "/")))
	body := "restored:" + line
	if !strings.Contains(filepath.Base(p), ".") {
		_ = os.MkdirAll(p, 0o755)
		p = filepath.Join(p, "inner.txt")
	}
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	_ = os.WriteFile(p, []byte(body), 0o644)
	return len(body)
}
