// Package fakerestic is a stand-in for restic.exe used only by the agent's
// tests. The test binary re-executes itself as the fake (the TestHelperProcess
// pattern): a TestMain calls MaybeRun first, and when the environment variable
// Scenario is set the process replays fixtures captured from restic 0.19.1
// (internal/restic/testdata/v0.19.1) and exits with the scenario's code.
//
// It lives under internal/agent/internal so nothing outside the agent can
// import it, and it is not part of any binary.
package fakerestic

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Environment variables that steer the fake.
const (
	// Scenario names what the fake does; unset means "not a fake process".
	Scenario = "CRYO_FAKE_RESTIC"
	// Out is a file the fake writes its argv and environment to (a test reads
	// it to prove what the child was given).
	Out = "CRYO_FAKE_OUT"
	// Tree is a directory whose file names the "pathy" scenario puts into its
	// error lines.
	Tree = "CRYO_FAKE_TREE"
)

// Dir is the fixture directory of restic 0.19.1.
func Dir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..", "restic", "testdata", "v0.19.1")
}

// Setter is the part of testing.T the helpers need.
type Setter interface {
	Setenv(key, value string)
}

// Use selects a scenario for the rest of the test.
func Use(t Setter, scenario string) {
	t.Setenv(Scenario, scenario)
}

type plan struct {
	stdout, stderr string // fixture file names ("" = nothing)
	exit           int
}

var backupPlans = map[string]plan{
	"backup_ok":      {"backup_ok.stdout.jsonl", "backup_ok.stderr.jsonl", 0},
	"backup_partial": {"backup_partial.stdout.jsonl", "backup_partial.stderr.jsonl", 3},
	"backup_locked":  {"backup_locked.stdout.jsonl", "backup_locked.stderr.jsonl", 3},
	"exit10":         {"", "exit10_repo_missing.stderr.jsonl", 10},
	"exit11":         {"", "exit11_lock.stderr.jsonl", 11},
	"exit12":         {"", "exit12_wrong_password.stderr.jsonl", 12},
}

// MaybeRun turns the process into the fake when the scenario variable is set
// and never returns in that case. Call it at the top of TestMain.
func MaybeRun() {
	scenario := os.Getenv(Scenario)
	if scenario == "" {
		return
	}
	args := os.Args[1:]
	if out := os.Getenv(Out); out != "" {
		dump(out, args)
	}
	if len(args) == 0 {
		os.Exit(2)
	}
	switch args[0] {
	case "version":
		if d := versionDelay[scenario]; d > 0 {
			time.Sleep(d)
		}
		if scenario == "version_bad" {
			fmt.Println(`{"message_type":"version","version":"0.18.0","go_version":"go1.24","go_os":"windows","go_arch":"amd64"}`)
			os.Exit(0)
		}
		replay(os.Stdout, "version.json", 0)
		os.Exit(0)
	case "snapshots":
		replay(os.Stdout, "snapshots.json", 0)
		os.Exit(0)
	case "backup":
		os.Exit(backup(scenario))
	}
	os.Exit(2)
}

// backupFuncs are backup scenarios written as code in the scenarios_*.go
// files; versionDelay makes "restic version" of a scenario take that long.
var (
	backupFuncs  = map[string]func() int{}
	versionDelay = map[string]time.Duration{}
)

func backup(scenario string) int {
	if f, ok := backupFuncs[scenario]; ok {
		return f()
	}
	switch scenario {
	case "slow":
		return slow()
	case "hang_ignore":
		// Swallow the interrupt (signal.Ignore would let Windows' default
		// handler end the process with STATUS_CONTROL_C_EXIT).
		swallow := make(chan os.Signal, 1)
		signal.Notify(swallow, os.Interrupt)
		go func() {
			for range swallow {
			}
		}()
		fmt.Println(`{"message_type":"status","percent_done":0.1,"total_files":10,"files_done":1,"total_bytes":1000,"bytes_done":100}`)
		time.Sleep(time.Minute)
		return 0
	case "hang_graceful":
		c := make(chan os.Signal, 1)
		signal.Notify(c, os.Interrupt)
		fmt.Println(`{"message_type":"status","percent_done":0.1,"total_files":10,"files_done":1,"total_bytes":1000,"bytes_done":100}`)
		select {
		case <-c:
			return 130
		case <-time.After(time.Minute):
			return 0
		}
	case "exit1_racing_cancel":
		// A real failure that races a cancel: the interrupt never reaches
		// the child's logic (swallowed, as if it landed after the child had
		// decided to exit), and the child exits 1 by itself shortly after
		// the cancel was requested.
		swallow := make(chan os.Signal, 1)
		signal.Notify(swallow, os.Interrupt)
		go func() {
			for range swallow {
			}
		}()
		fmt.Println(`{"message_type":"status","percent_done":0.1,"total_files":10,"files_done":1,"total_bytes":1000,"bytes_done":100}`)
		time.Sleep(1500 * time.Millisecond)
		fmt.Fprintln(os.Stderr, `{"message_type":"exit_error","code":1,"message":"Fatal: unable to save snapshot: internal error"}`)
		return 1
	case "pathy":
		return pathy()
	case "exit_gateway":
		fmt.Fprintln(os.Stderr, `{"message_type":"exit_error","code":1,"message":"Fatal: unable to open repository at rest:https://gateway.example/dev/: dial tcp 10.0.0.9:443: connect: connection refused"}`)
		return 1
	case "exit_unknown":
		fmt.Fprintln(os.Stderr, `{"message_type":"exit_error","code":99,"message":"something odd"}`)
		return 99
	case "no_summary":
		fmt.Println(`{"message_type":"status","percent_done":1,"total_files":1,"files_done":1,"total_bytes":1,"bytes_done":1}`)
		return 0
	case "env_dump":
		replay(os.Stdout, "backup_ok.stdout.jsonl", 0)
		return 0
	case "vss_fallback":
		// The C: snapshot failed and restic read the volume live; nothing else
		// went wrong, so restic 0.19.1 exits 0 with a snapshot (VSSFallbackLine).
		replay(os.Stdout, "backup_ok.stdout.jsonl", 0)
		fmt.Fprintln(os.Stderr, VSSFallbackLine)
		return 0
	case "vss_fallback_partial":
		// The fallback plus a file the live read could not open: exit 3.
		fmt.Fprintln(os.Stderr, VSSFallbackLine)
		replay(os.Stdout, "backup_locked.stdout.jsonl", 0)
		replay(os.Stderr, "backup_locked.stderr.jsonl", 0)
		return 3
	case "alloc":
		return alloc()
	case "alloc_hang":
		return allocHang()
	}
	p, ok := backupPlans[scenario]
	if !ok {
		fmt.Fprintln(os.Stderr, "fakerestic: unknown scenario")
		return 2
	}
	if p.stdout != "" {
		replay(os.Stdout, p.stdout, 0)
	}
	if p.stderr != "" {
		replay(os.Stderr, p.stderr, 0)
	}
	return p.exit
}

// VSSFallbackLine is the stderr line restic 0.19.1 writes with --json when the
// VSS snapshot of a volume fails and it reads that volume live
// (internal/fs/fs_local_vss.go: errors.Errorf("failed to create snapshot for
// [%s]: %s", vssVolume, err) through the backup's error callback, rendered by
// internal/ui/backup/json.go as an "error" message with during "archival" and
// item = vssVolume). The inner text is one of restic's own VSS errors
// (callAsyncFunctionAndWait: "<name>() deadline exceeded").
const VSSFallbackLine = `{"message_type":"error","error":{"message":"failed to create snapshot for [c:\\]: VSS error: GatherWriterMetadata() deadline exceeded"},"during":"archival","item":"c:\\"}`

// allocSink keeps every chunk alloc touches reachable, so the collector cannot
// give the memory back.
var allocSink [][]byte

// AllocHangTracebackLines is how many lines of Go runtime traceback allocHang
// writes to stderr after its commit was refused (a real out-of-memory restic
// prints a traceback of this order, one line per frame).
const AllocHangTracebackLines = 61

// allocHangSafetyExit ends the fake on its own long after any supervisor
// should have ended it, so a broken supervisor cannot leave it running for
// ever.
const allocHangSafetyExit = 2 * time.Minute

// allocHang is a capped restic.exe that the Go runtime leaves stuck: it commits
// memory outside the Go heap until the job object's process memory cap refuses
// a commit, prints what the Go runtime prints for that ("runtime: VirtualAlloc
// ... failed", "fatal error: out of memory" and a goroutine traceback), and
// then does not exit, with its pipes open. That is the measured behaviour the
// supervisor's memory kill exists for (supervisor.newControl), made
// deterministic: the real runtime sometimes exits by itself. Without a cap it
// stops at 4 GiB and exits 0.
func allocHang() int {
	fmt.Println(`{"message_type":"status","percent_done":0.1,"total_files":10,"files_done":1,"total_bytes":1000,"bytes_done":100}`)
	if !commitUntilRefused(4 << 30) {
		return 0
	}
	w := bufio.NewWriter(os.Stderr)
	fmt.Fprintln(w, "runtime: VirtualAlloc of 16777216 bytes failed with errno=1455")
	fmt.Fprintln(w, "fatal error: out of memory")
	fmt.Fprintln(w)
	n := 3
	for g := 1; n < AllocHangTracebackLines; g++ {
		fmt.Fprintf(w, "goroutine %d [running]:\n", g)
		n++
		for f := 0; f < 4 && n < AllocHangTracebackLines; f++ {
			fmt.Fprintf(w, "runtime.throw({0x7ff6a1b2c3d4, 0x%x})\n", 13+f)
			n++
			if n < AllocHangTracebackLines {
				fmt.Fprintf(w, "\tC:/Program Files/Go/src/runtime/panic.go:%d +0x4d fp=0xc000%04x sp=0xc000%04x pc=0x7ff6a1b2\n", 1101+f, f, f)
				n++
			}
		}
	}
	_ = w.Flush()
	time.Sleep(allocHangSafetyExit)
	return 0
}

// alloc commits memory until the process memory cap of the job object stops
// it: the Go runtime cannot commit the next arena and dies ("fatal error: out
// of memory", exit 2), which is how a capped restic.exe ends. Without a cap it
// stops at 4 GiB and exits 0.
func alloc() int {
	fmt.Println(`{"message_type":"status","percent_done":0.1,"total_files":10,"files_done":1,"total_bytes":1000,"bytes_done":100}`)
	const chunk = 16 << 20
	for total := 0; total < 4<<30; total += chunk {
		b := make([]byte, chunk)
		for i := 0; i < len(b); i += 4096 {
			b[i] = 1
		}
		allocSink = append(allocSink, b)
	}
	return 0
}

// slow prints status lines with pauses so a progress sampler sees several, then
// a summary.
func slow() int {
	for i := 1; i <= 8; i++ {
		fmt.Printf(`{"message_type":"status","percent_done":%.2f,"total_files":8,"files_done":%d,"total_bytes":800,"bytes_done":%d}`+"\n",
			float64(i)/8, i, i*100)
		time.Sleep(60 * time.Millisecond)
	}
	replayLast(os.Stdout, "backup_ok.stdout.jsonl")
	return 0
}

// pathy emits a successful snapshot plus error lines that carry the names of
// the files under CRYO_FAKE_TREE (and a plain-text line), then exits 3: the
// raw stream is full of paths, which must stay on the device.
func pathy() int {
	replay(os.Stdout, "backup_ok.stdout.jsonl", 0)
	tree := os.Getenv(Tree)
	var names []string
	_ = filepath.WalkDir(tree, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			names = append(names, p)
		}
		return nil
	})
	if len(names) == 0 {
		names = []string{`C:\Users\PHI-sentinel-Jane\diagnosis.docx`}
	}
	for _, n := range names {
		esc := strings.ReplaceAll(n, `\`, `\\`)
		fmt.Fprintf(os.Stderr, `{"message_type":"error","error":{"message":"open %s: Access is denied."},"during":"archival","item":"%s"}`+"\n", esc, esc)
	}
	fmt.Fprintf(os.Stderr, "%s does not exist, skipping\n", names[0])
	fmt.Fprintf(os.Stderr, `{"message_type":"exit_error","code":3,"message":"Warning: at least one source file could not be read: %s"}`+"\n",
		strings.ReplaceAll(names[0], `\`, `\\`))
	return 3
}

func replay(w io.Writer, name string, delay time.Duration) {
	f, err := os.Open(filepath.Join(Dir(), name))
	if err != nil {
		fmt.Fprintln(os.Stderr, "fakerestic: fixture missing")
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		fmt.Fprintln(w, sc.Text())
		if delay > 0 {
			time.Sleep(delay)
		}
	}
}

func replayLast(w io.Writer, name string) {
	b, err := os.ReadFile(filepath.Join(Dir(), name))
	if err != nil {
		return
	}
	lines := strings.Split(strings.TrimRight(string(b), "\r\n"), "\n")
	fmt.Fprintln(w, strings.TrimRight(lines[len(lines)-1], "\r"))
}

func dump(path string, args []string) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintln(f, "ARGS "+strings.Join(args, " "))
	for _, kv := range os.Environ() {
		fmt.Fprintln(f, "ENV "+kv)
	}
}
