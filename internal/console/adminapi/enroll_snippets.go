package adminapi

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/config"
	"github.com/c2tech-sys/cryobackup/internal/console/enroll"
)

// The deployment generator (M2-W2B; architecture §5.2 /enrollment-tokens/{id}/script,
// §5.3 /enroll). It turns a token, the console's public agent URL and the agent
// MSI into the three things an operator pastes: the RMM wrapper's variables, a
// one-machine msiexec line and a Group Policy startup script. The web page and
// the admin API call the same functions, so they cannot disagree.
//
// Rules the output keeps (deploy/windows/README.md, rmm/install-cryobackup.ps1):
//   - The RMM form never puts the token on a command line. The wrapper installs
//     through the Windows Installer API in-process; the token goes into the RMM's
//     secret (password) script variable.
//   - The msiexec form cannot avoid it, and says so.
//   - Nothing is hard-coded: the URL is the configured public_url, the MSI name
//     and hash are the operator's file.

// Placeholders put into a snippet where a real value is not available. Each is a
// string a script cannot mistake for a value (the wrapper rejects it), so a
// forgotten edit fails loudly instead of enrolling nothing quietly.
const (
	PlaceholderServerURL = "<set public_url in the console configuration>"
	PlaceholderToken     = "<enrollment token>"
	PlaceholderMSIHash   = "<SHA-256 of the MSI, shown on the Downloads page>"
	DefaultMSIName       = "cryobackup-agent.msi"
	// DefaultMsiPath is where the wrapper's MsiPath variable points when the RMM
	// stages the file on the endpoint.
	DefaultMsiPath = `C:\Windows\Temp\cryobackup-agent.msi`
)

// MSIInfo describes the agent installer the snippets refer to.
type MSIInfo struct {
	// FileName is the bare file name (no directory).
	FileName string
	// Version is X.Y.Z[.N] taken from the file name or a .version file; "" when unknown.
	Version string
	// SHA256 is the lower-case hex digest of the file as it is on the console host.
	SHA256 string
	Size   int64
	// ModTime is when the file was placed.
	ModTime time.Time
}

// ErrNoMSI means the artifacts directory holds no agent MSI (or none is configured).
var ErrNoMSI = errors.New("adminapi: no agent MSI in the artifacts directory")

var (
	msiNameRE    = regexp.MustCompile(`^(?i)cryobackup-agent([-_]v?(\d+\.\d+\.\d+(?:\.\d+)?))?\.msi$`)
	msiVersionRE = regexp.MustCompile(`^\d+\.\d+\.\d+(?:\.\d+)?$`)

	msiHashMu    sync.Mutex
	msiHashCache = map[string]msiHashEntry{}
)

type msiHashEntry struct {
	size int64
	mod  time.Time
	sum  string
}

// CurrentMSI finds the agent MSI the operator placed in dir (the console's
// artifacts_dir) and returns its name, version and SHA-256. The console does not
// host or upload binaries in M2: this is only so the page can show the hash to
// compare against and pre-fill MsiSha256. Candidates are regular files named
// cryobackup-agent[-X.Y.Z[.N]].msi (symbolic links are ignored); the highest
// version wins, then the newest file. The digest is cached per file size and time.
// ErrNoMSI when none is placed (or no directory is configured); any other error,
// including a configured directory that is missing, is a read failure.
func CurrentMSI(dir string) (MSIInfo, error) {
	if dir == "" {
		return MSIInfo{}, ErrNoMSI
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		// A configured directory that is missing or unreadable is a broken
		// installation (init creates it), not "no installer placed yet".
		return MSIInfo{}, errors.New("adminapi: cannot read the artifacts directory")
	}
	var found []MSIInfo
	for _, e := range entries {
		m := msiNameRE.FindStringSubmatch(e.Name())
		if m == nil || !e.Type().IsRegular() {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		info := MSIInfo{FileName: e.Name(), Version: m[2], Size: fi.Size(), ModTime: fi.ModTime().UTC()}
		if info.Version == "" {
			if b, err := readSmall(filepath.Join(dir, e.Name()+".version")); err == nil && msiVersionRE.MatchString(strings.TrimSpace(string(b))) {
				info.Version = strings.TrimSpace(string(b))
			}
		}
		found = append(found, info)
	}
	if len(found) == 0 {
		return MSIInfo{}, ErrNoMSI
	}
	sort.SliceStable(found, func(i, j int) bool {
		if c := compareVersion(found[i].Version, found[j].Version); c != 0 {
			return c > 0
		}
		return found[i].ModTime.After(found[j].ModTime)
	})
	best := found[0]
	sum, err := fileSHA256(filepath.Join(dir, best.FileName), best.Size, best.ModTime)
	if err != nil {
		return MSIInfo{}, errors.New("adminapi: cannot read the agent MSI")
	}
	best.SHA256 = sum
	return best, nil
}

func readSmall(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, 64))
}

func fileSHA256(path string, size int64, mod time.Time) (string, error) {
	msiHashMu.Lock()
	if c, ok := msiHashCache[path]; ok && c.size == size && c.mod.Equal(mod) {
		msiHashMu.Unlock()
		return c.sum, nil
	}
	msiHashMu.Unlock()
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	msiHashMu.Lock()
	msiHashCache[path] = msiHashEntry{size: size, mod: mod, sum: sum}
	msiHashMu.Unlock()
	return sum, nil
}

// compareVersion orders X.Y.Z[.N] strings numerically; unknown ("") is lowest.
func compareVersion(a, b string) int {
	pa, pb := versionParts(a), versionParts(b)
	for i := 0; i < 4; i++ {
		if pa[i] != pb[i] {
			if pa[i] > pb[i] {
				return 1
			}
			return -1
		}
	}
	return 0
}

func versionParts(v string) [4]int {
	var out [4]int
	if v == "" {
		out[0] = -1
		return out
	}
	for i, p := range strings.Split(v, ".") {
		if i < 4 {
			out[i], _ = strconv.Atoi(p)
		}
	}
	return out
}

// SnippetInput is what the generator needs.
type SnippetInput struct {
	// PublicURL is the configured public_url (scheme, host and port of the agent listener).
	PublicURL string
	// Token is the enrollment secret just minted; "" yields a placeholder.
	Token string
	// MSI is the installer on the console host, if any.
	MSI MSIInfo
}

// SnippetVar is one variable of the RMM script.
type SnippetVar struct {
	Name   string
	Value  string
	Note   string
	Secret bool
}

// Snippets is the generated deployment material.
type Snippets struct {
	// ServerURL is the URL the snippets carry (a placeholder when public_url is unusable).
	ServerURL string
	// ServerProblem is non-empty when public_url is missing or unusable.
	ServerProblem string
	// Lab is true for a plain-http URL: the snippets then allow it for lab use only.
	Lab bool
	// TokenShown is false when the snippets carry a placeholder instead of a token.
	TokenShown bool
	// MSIKnown is false when no MSI hash is available for MsiSha256.
	MSIKnown bool

	Vars []SnippetVar
	// RMM is the variables block for the RMM script (token in the secret variable).
	RMM string
	// RMMNotes are the rules to read before pasting it.
	RMMNotes []string
	// MSIExec is the one-machine msiexec line.
	MSIExec string
	// MSIExecWarning says the token is visible to local administrators while msiexec runs.
	MSIExecWarning string
	// GPO is the Group Policy startup script.
	GPO      string
	GPONotes []string
}

// AgentServerURL reduces a configured public_url to the scheme://host[:port]
// the agent and the MSI take as SERVER, and says whether it is plain http.
func AgentServerURL(public string) (u string, lab bool, problem string) {
	public = strings.TrimSpace(public)
	if public == "" {
		return PlaceholderServerURL, false, "The console's public URL is not set. Set public_url in the console configuration; until then the snippets carry a placeholder that the installer refuses."
	}
	// Strict shape (config.ParsePublicURL): the value is pasted into PowerShell
	// and cmd text, so anything but scheme://host[:port] becomes the placeholder.
	u, err := config.ParsePublicURL(public)
	if err != nil {
		return PlaceholderServerURL, false, "The console's public URL (public_url) is not a valid http(s) address: use http(s)://host[:port] with no path, user or escapes. Correct it in the console configuration."
	}
	if strings.HasPrefix(u, "http://") {
		return u, true, "public_url is plain http. The agent refuses it unless the lab override is on, and the override turns off TLS verification: lab use only."
	}
	return u, false, ""
}

// psq quotes s as a PowerShell single-quoted string.
func psq(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// GenerateSnippets builds the deployment material. It never fails: a missing
// piece becomes a placeholder the target script refuses, and a note says what
// to fix.
func GenerateSnippets(in SnippetInput) Snippets {
	var sn Snippets
	sn.ServerURL, sn.Lab, sn.ServerProblem = AgentServerURL(in.PublicURL)
	token := in.Token
	if enroll.WellFormed(token) {
		sn.TokenShown = true
	} else {
		token = PlaceholderToken
	}
	msiName := in.MSI.FileName
	if msiName == "" {
		msiName = DefaultMSIName
	}
	hash := in.MSI.SHA256
	sn.MSIKnown = hash != ""
	if !sn.MSIKnown {
		hash = PlaceholderMSIHash
	}

	sn.Vars = []SnippetVar{
		{Name: "MsiPath", Value: DefaultMsiPath, Note: "Where your RMM puts the MSI on the endpoint. Change it to match your file distribution."},
		{Name: "MsiSha256", Value: hash, Note: "SHA-256 of the MSI. The wrapper refuses a file that does not match."},
		{Name: "ServerUrl", Value: sn.ServerURL, Note: "The console's agent address (public_url)."},
		{Name: "EnrollToken", Value: token, Note: "The enrollment token. Store it in the RMM's secret (password) variable type.", Secret: true},
	}
	if sn.Lab {
		sn.Vars = append(sn.Vars, SnippetVar{Name: "InsecureLab", Value: "1", Note: "Lab only: allows a plain-http console and turns off TLS verification."})
	}

	var b strings.Builder
	b.WriteString("# CryoBackup: variables for deploy\\windows\\rmm\\install-cryobackup.ps1\n")
	b.WriteString("# Define these as script variables in your RMM. EnrollToken is a SECRET: use the\n")
	b.WriteString("# RMM's password / secret variable type so it is not shown in run logs.\n")
	for _, v := range sn.Vars {
		b.WriteString("$" + v.Name + strings.Repeat(" ", 11-len(v.Name)) + " = " + psq(v.Value)) // aligned to the longest name, EnrollToken
		if v.Secret {
			b.WriteString("   # secret variable")
		}
		b.WriteString("\n")
	}
	sn.RMM = b.String()
	sn.RMMNotes = []string{
		"The wrapper installs the MSI through the Windows Installer API from its own process, so the token is on no command line and in no file.",
		"Put EnrollToken in the RMM's secret variable. If your RMM has no secret type, give the script the token only for the run that needs it and clear it afterwards.",
		"The wrapper is idempotent: run it as often as you like. It installs, upgrades or enrolls only what is missing.",
		"Stage the MSI on the endpoint first (your RMM's file distribution) and set MsiPath to where it lands.",
	}

	sn.MSIExecWarning = "The token is on msiexec's command line, so any local administrator can read it while the install runs. Use this for one machine you are standing at; use the RMM form for a fleet."
	line := "msiexec /i " + dq(msiName) + " /qn REBOOT=ReallySuppress SERVER=" + dq(sn.ServerURL) + " TOKEN=" + dq(token)
	if sn.Lab {
		line += " CRYO_INSECURE_LAB=1"
	}
	// A literal path: it works in cmd and in PowerShell, which does not expand %windir%.
	line += ` /l*v C:\Windows\Temp\cryobackup-install.log`
	sn.MSIExec = line

	var g strings.Builder
	g.WriteString("# CryoBackup: Group Policy startup script\n")
	g.WriteString("# Computer Configuration > Policies > Windows Settings > Scripts (Startup/Shutdown) > Startup > PowerShell Scripts\n")
	g.WriteString("$MsiPath     = " + psq(`\\<file-server>\<share>\`+msiName) + "\n")
	g.WriteString("$MsiSha256   = " + psq(hash) + "\n")
	g.WriteString("$ServerUrl   = " + psq(sn.ServerURL) + "\n")
	g.WriteString("$EnrollToken = " + psq(token) + "\n")
	if sn.Lab {
		g.WriteString("$InsecureLab = '1'   # lab only\n")
	}
	g.WriteString("& " + psq(`\\<file-server>\<share>\install-cryobackup.ps1`) + "\n")
	sn.GPO = g.String()
	sn.GPONotes = []string{
		"Replace <file-server> and <share> with a share that Domain Computers can read, and put install-cryobackup.ps1 and the MSI on it.",
		"The startup script is readable by every authenticated user in the domain (SYSVOL or the share), and it holds the token. Give the token a short expiry and a use cap equal to the number of machines, and revoke it when the rollout is done.",
		"Startup scripts run as SYSTEM, which the wrapper needs. If your execution policy blocks scripts from a network path, sign the wrapper or set the policy for this script.",
	}
	return sn
}

func dq(s string) string { return `"` + s + `"` }
