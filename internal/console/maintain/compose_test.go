package maintain

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/console/gateway"
)

// These tests validate the deployment files without Docker: docker-compose.yml is
// parsed with a small YAML-subset parser (block maps and lists, quoted and plain
// scalars, flow lists of scalars: everything the file uses and nothing else; a
// construct outside the subset is a parse error, so the file stays simple) and
// checked structurally against architecture 7.1 and 8.1, and every variable the
// file references must have an entry in .env.example. `docker compose config` is the
// real validator and runs on the lab host (ticket P1-T8).

func deployPath(t *testing.T, rel ...string) string {
	t.Helper()
	p := filepath.Join(append([]string{"..", "..", "..", "deploy"}, rel...)...)
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("deploy file missing: %v", err)
	}
	return p
}

func readDeploy(t *testing.T, rel ...string) string {
	t.Helper()
	b, err := os.ReadFile(deployPath(t, rel...))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// ---- the YAML subset -----------------------------------------------------------

type yline struct {
	indent int
	text   string
	no     int
}

var errYAML = errors.New("yaml subset")

func yerr(no int, msg string) error { return fmt.Errorf("%w: line %d: %s", errYAML, no, msg) }

// parseYAML parses the subset into map[string]any, []any and string values (nil for
// an empty value).
func parseYAML(src string) (map[string]any, error) {
	var lines []yline
	for i, raw := range strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n") {
		if strings.Contains(raw, "\t") {
			return nil, yerr(i+1, "tab character")
		}
		text := stripComment(raw)
		if strings.TrimSpace(text) == "" {
			continue
		}
		indent := len(text) - len(strings.TrimLeft(text, " "))
		lines = append(lines, yline{indent: indent, text: strings.TrimSpace(text), no: i + 1})
	}
	if len(lines) == 0 {
		return nil, yerr(0, "empty document")
	}
	v, next, err := parseBlock(lines, 0, lines[0].indent)
	if err != nil {
		return nil, err
	}
	if next != len(lines) {
		return nil, yerr(lines[next].no, "unexpected indentation")
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, yerr(1, "document is not a mapping")
	}
	return m, nil
}

// stripComment removes a trailing comment (a '#' at the start or after whitespace,
// outside quotes).
func stripComment(s string) string {
	var q byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case q != 0:
			if c == '\\' && q == '"' {
				i++
			} else if c == q {
				q = 0
			}
		case c == '"' || c == '\'':
			q = c
		case c == '#' && (i == 0 || s[i-1] == ' '):
			return s[:i]
		}
	}
	return s
}

func parseBlock(lines []yline, i, indent int) (any, int, error) {
	if strings.HasPrefix(lines[i].text, "- ") || lines[i].text == "-" {
		return parseList(lines, i, indent)
	}
	return parseMap(lines, i, indent)
}

func parseList(lines []yline, i, indent int) (any, int, error) {
	out := []any{}
	for i < len(lines) && lines[i].indent == indent && strings.HasPrefix(lines[i].text, "- ") {
		item := strings.TrimSpace(lines[i].text[2:])
		if item == "" || (strings.Contains(item, ": ") && !isQuoted(item) && !strings.HasPrefix(item, "[")) {
			return nil, 0, yerr(lines[i].no, "list items must be scalars")
		}
		v, err := parseScalar(item, lines[i].no)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, v)
		i++
	}
	if i < len(lines) && lines[i].indent == indent && !strings.HasPrefix(lines[i].text, "- ") {
		// a sibling key at the same indent ends a list that sits at its key's indent
		return out, i, nil
	}
	return out, i, nil
}

func parseMap(lines []yline, i, indent int) (any, int, error) {
	out := map[string]any{}
	for i < len(lines) && lines[i].indent == indent {
		l := lines[i]
		if strings.HasPrefix(l.text, "- ") {
			return nil, 0, yerr(l.no, "list item inside a mapping")
		}
		key, rest, ok := splitKey(l.text)
		if !ok {
			return nil, 0, yerr(l.no, "expected key: value")
		}
		if _, dup := out[key]; dup {
			return nil, 0, yerr(l.no, "duplicate key "+key)
		}
		i++
		if rest != "" {
			v, err := parseScalar(rest, l.no)
			if err != nil {
				return nil, 0, err
			}
			out[key] = v
			continue
		}
		switch {
		case i < len(lines) && lines[i].indent > indent:
			v, next, err := parseBlock(lines, i, lines[i].indent)
			if err != nil {
				return nil, 0, err
			}
			out[key], i = v, next
		case i < len(lines) && lines[i].indent == indent && strings.HasPrefix(lines[i].text, "- "):
			v, next, err := parseList(lines, i, indent)
			if err != nil {
				return nil, 0, err
			}
			out[key], i = v, next
		default:
			out[key] = nil
		}
	}
	if i < len(lines) && lines[i].indent > indent {
		return nil, 0, yerr(lines[i].no, "unexpected indentation")
	}
	return out, i, nil
}

// splitKey splits "key: rest" at the first colon that is followed by a space or the
// end of the line.
func splitKey(s string) (key, rest string, ok bool) {
	for i := 0; i < len(s); i++ {
		if s[i] == ':' && (i == len(s)-1 || s[i+1] == ' ') {
			return strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+1:]), true
		}
	}
	return "", "", false
}

func isQuoted(s string) bool {
	return len(s) >= 2 && (s[0] == '"' && s[len(s)-1] == '"' || s[0] == '\'' && s[len(s)-1] == '\'')
}

func parseScalar(s string, no int) (any, error) {
	switch {
	case strings.HasPrefix(s, "["):
		if !strings.HasSuffix(s, "]") {
			return nil, yerr(no, "unterminated flow list")
		}
		return parseFlowList(s[1:len(s)-1], no)
	case strings.HasPrefix(s, `"`):
		if !strings.HasSuffix(s, `"`) || len(s) < 2 {
			return nil, yerr(no, "unterminated string")
		}
		var out strings.Builder
		body := s[1 : len(s)-1]
		for i := 0; i < len(body); i++ {
			if body[i] == '\\' && i+1 < len(body) {
				i++
			} else if body[i] == '"' {
				return nil, yerr(no, "unescaped quote in string")
			}
			out.WriteByte(body[i])
		}
		return out.String(), nil
	case strings.HasPrefix(s, "'"):
		if !strings.HasSuffix(s, "'") || len(s) < 2 {
			return nil, yerr(no, "unterminated string")
		}
		return strings.ReplaceAll(s[1:len(s)-1], "''", "'"), nil
	}
	if strings.Contains(s, ": ") {
		return nil, yerr(no, "plain scalar contains ': '")
	}
	return s, nil
}

func parseFlowList(body string, no int) (any, error) {
	out := []any{}
	var cur strings.Builder
	var q byte
	flush := func() error {
		item := strings.TrimSpace(cur.String())
		cur.Reset()
		if item == "" {
			return nil
		}
		v, err := parseScalar(item, no)
		if err != nil {
			return err
		}
		out = append(out, v)
		return nil
	}
	for i := 0; i < len(body); i++ {
		c := body[i]
		switch {
		case q != 0:
			cur.WriteByte(c)
			if c == '\\' && q == '"' && i+1 < len(body) {
				i++
				cur.WriteByte(body[i])
			} else if c == q {
				q = 0
			}
		case c == '"' || c == '\'':
			q = c
			cur.WriteByte(c)
		case c == ',':
			if err := flush(); err != nil {
				return nil, err
			}
		default:
			cur.WriteByte(c)
		}
	}
	if q != 0 {
		return nil, yerr(no, "unterminated quote in flow list")
	}
	return out, flush()
}

// ---- accessors -------------------------------------------------------------------

func asMap(t *testing.T, v any, what string) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%s is not a mapping (%T)", what, v)
	}
	return m
}

func asStrings(t *testing.T, v any, what string) []string {
	t.Helper()
	if v == nil {
		return nil
	}
	l, ok := v.([]any)
	if !ok {
		t.Fatalf("%s is not a list (%T)", what, v)
	}
	out := make([]string, len(l))
	for i, e := range l {
		s, ok := e.(string)
		if !ok {
			t.Fatalf("%s[%d] is not a string", what, i)
		}
		out[i] = s
	}
	return out
}

func loadCompose(t *testing.T) map[string]any {
	t.Helper()
	doc, err := parseYAML(readDeploy(t, "compose", "docker-compose.yml"))
	if err != nil {
		t.Fatalf("docker-compose.yml does not parse: %v", err)
	}
	return doc
}

func service(t *testing.T, doc map[string]any, name string) map[string]any {
	t.Helper()
	svcs := asMap(t, doc["services"], "services")
	s, ok := svcs[name]
	if !ok {
		t.Fatalf("service %s is missing", name)
	}
	return asMap(t, s, "service "+name)
}

func TestYAMLSubsetParserRejectsBrokenInput(t *testing.T) {
	for name, src := range map[string]string{
		"tab":            "a:\n\tb: 1\n",
		"bad indent":     "a:\n    b: 1\n  c: 2\n",
		"duplicate key":  "a: 1\na: 2\n",
		"colon in plain": "a: b: c\n",
		"open string":    "a: \"abc\n",
		"open flow":      "a: [1, 2\n",
		"map in list":    "a:\n  - b: 1\n",
	} {
		if _, err := parseYAML(src); err == nil {
			t.Errorf("%s: parsed without error", name)
		}
	}
	got, err := parseYAML("a:\n  - x\n  - \"y: z\"\nb: [\"p\", 'q', r]\nc:\nd: \"${V:-1}:2\" # comment\n")
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got["a"]) != "[x y: z]" || fmt.Sprint(got["b"]) != "[p q r]" || got["c"] != nil || got["d"] != "${V:-1}:2" {
		t.Fatalf("parsed = %v", got)
	}
}

func TestComposeStructure(t *testing.T) {
	doc := loadCompose(t)
	if doc["name"] != "cryobackup" {
		t.Errorf("project name = %v", doc["name"])
	}
	svcs := asMap(t, doc["services"], "services")
	var names []string
	for n := range svcs {
		names = append(names, n)
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "console,gateway,maintenance,minio,minio-init" {
		t.Fatalf("services = %v", names)
	}
	vols := asMap(t, doc["volumes"], "volumes")
	for _, v := range []string{"db", "gateway-auth", "artifacts", "adminpath", "minio-data"} {
		if _, ok := vols[v]; !ok {
			t.Errorf("volume %s is not declared", v)
		}
	}
	// ADR 0008: the worker keeps nothing on a persistent volume.
	if _, ok := vols["maintain-state"]; ok {
		t.Error("the maintain-state volume is back: the worker keeps no repository data on disk (ADR 0008)")
	}

	for _, n := range names {
		s := service(t, doc, n)
		hc, ok := s["healthcheck"]
		if !ok {
			t.Errorf("%s has no healthcheck", n)
			continue
		}
		if len(asStrings(t, asMap(t, hc, n+" healthcheck")["test"], n+" healthcheck.test")) < 2 {
			t.Errorf("%s healthcheck.test is too short", n)
		}
		if img, _ := s["image"].(string); img == "" || strings.Contains(img, ":latest") || !strings.Contains(img, ":") {
			t.Errorf("%s image %q is not pinned to a tag", n, img)
		}
	}

	// profiles: only the lab services carry one
	for _, n := range names {
		p := asStrings(t, service(t, doc, n)["profiles"], n+" profiles")
		lab := n == "minio" || n == "minio-init"
		if lab && (len(p) != 1 || p[0] != "lab") || !lab && len(p) != 0 {
			t.Errorf("%s profiles = %v", n, p)
		}
	}

	// the three app services are hardened
	for _, n := range []string{"console", "gateway", "maintenance"} {
		s := service(t, doc, n)
		if s["read_only"] != "true" {
			t.Errorf("%s is not read_only", n)
		}
		if got := asStrings(t, s["cap_drop"], n+" cap_drop"); len(got) != 1 || got[0] != "ALL" {
			t.Errorf("%s cap_drop = %v", n, got)
		}
		if got := asStrings(t, s["security_opt"], n+" security_opt"); len(got) != 1 || got[0] != "no-new-privileges:true" {
			t.Errorf("%s security_opt = %v", n, got)
		}
		if s["user"] != "10001:10001" {
			t.Errorf("%s user = %v", n, s["user"])
		}
		// ADR 0008: no crash dumps, soft and hard.
		core := asMap(t, asMap(t, s["ulimits"], n+" ulimits")["core"], n+" ulimits.core")
		if core["soft"] != "0" || core["hard"] != "0" {
			t.Errorf("%s ulimits.core = %v, want soft 0 and hard 0", n, core)
		}
	}
}

// TestComposeNoDataAtRest (ADR 0008): the maintenance worker's temp space is a
// sized tmpfs that CRYO_MAINTAIN_SCRATCH_DIR points into, it has no persistent
// state or artifacts volume, and the README says what lives where.
func TestComposeNoDataAtRest(t *testing.T) {
	doc := loadCompose(t)
	mt := service(t, doc, "maintenance")
	env := asMap(t, mt["environment"], "maintenance environment")
	if _, ok := env["CRYO_MAINTAIN_STATE_DIR"]; ok {
		t.Error("maintenance still sets CRYO_MAINTAIN_STATE_DIR")
	}
	scratch, _ := env["CRYO_MAINTAIN_SCRATCH_DIR"].(string)
	tmpfs := asStrings(t, mt["tmpfs"], "maintenance tmpfs")
	if len(tmpfs) != 1 {
		t.Fatalf("maintenance tmpfs = %v", tmpfs)
	}
	mount, opts, _ := strings.Cut(tmpfs[0], ":")
	if !strings.HasPrefix(scratch, mount+"/") || !strings.Contains(opts, "size=") {
		t.Errorf("scratch %q is not on the sized tmpfs %q", scratch, tmpfs[0])
	}
	mv := strings.Join(asStrings(t, mt["volumes"], "maintenance volumes"), "\n")
	for _, bad := range []string{"artifacts", "maintain-state", "/state"} {
		if strings.Contains(mv, bad) {
			t.Errorf("maintenance mounts %q", bad)
		}
	}
	readme := readDeploy(t, "compose", "README.md")
	for _, want := range []string{"## No data at rest", "ADR 0008", "swap", "--no-cache", "tmpfs", "core"} {
		if !strings.Contains(readme, want) {
			t.Errorf("deploy/compose/README.md lacks %q", want)
		}
	}
}

func TestComposeGatewayCommandIsArchitecture71(t *testing.T) {
	doc := loadCompose(t)
	gw := service(t, doc, "gateway")
	cmd := asStrings(t, gw["command"], "gateway command")
	want := []string{"serve", "restic", "--addr", ":${GATEWAY_PORT:-9443}",
		"--cert", "/tls/gateway.crt", "--key", "/tls/gateway.key", "--min-tls-version", "tls1.2",
		"--htpasswd", "/auth/htpasswd", "--append-only", "--private-repos"}
	if len(cmd) != len(want)+1 {
		t.Fatalf("gateway command = %v", cmd)
	}
	for i, w := range want {
		if cmd[i] != w {
			t.Errorf("gateway command[%d] = %q, want %q", i, cmd[i], w)
		}
	}
	last := cmd[len(cmd)-1]
	if !strings.HasPrefix(last, "store:${BUCKET:?") || !strings.HasSuffix(last, "}/${PREFIX:-}") {
		t.Errorf("gateway remote = %q", last)
	}

	// append-only is a flag, never an environment variable
	src := readDeploy(t, "compose", "docker-compose.yml")
	if strings.Contains(strings.ToUpper(src), "RCLONE_APPEND_ONLY") {
		t.Error("docker-compose.yml mentions RCLONE_APPEND_ONLY")
	}
	for k := range asMap(t, gw["environment"], "gateway environment") {
		if strings.Contains(strings.ToUpper(k), "APPEND") || strings.Contains(strings.ToUpper(k), "BYPASS") {
			t.Errorf("gateway environment sets %s", k)
		}
	}
	for _, a := range cmd {
		if a == "--user" || a == "--pass" || strings.Contains(a, "bypass-governance") {
			t.Errorf("gateway command contains %s", a)
		}
	}

	vols := strings.Join(asStrings(t, gw["volumes"], "gateway volumes"), "\n")
	for _, want := range []string{"gateway-auth:/auth:ro", "${RCLONE_GATEWAY_CONF:-./rclone.gateway.conf}:/config/rclone.conf:ro", "${TLS_DIR:-./tls}:/tls:ro"} {
		if !strings.Contains(vols, want) {
			t.Errorf("gateway volumes lack %q", want)
		}
	}
	for _, bad := range []string{"RCLONE_ADMIN_CONF", "/data", "db:", "MASTER_KEY"} {
		if strings.Contains(vols, bad) {
			t.Errorf("gateway mounts %q: the gateway must not reach the admin credential, the database or the master key", bad)
		}
	}
	if m := asMap(t, gw["depends_on"], "gateway depends_on"); m["minio-init"] == nil {
		t.Error("gateway does not wait for minio-init")
	} else if mi := asMap(t, m["minio-init"], "depends_on.minio-init"); mi["required"] != "false" || mi["condition"] != "service_completed_successfully" {
		t.Errorf("depends_on.minio-init = %v (it must be optional: production has no MinIO)", mi)
	}
	if ports := strings.Join(asStrings(t, gw["ports"], "gateway ports"), " "); !strings.Contains(ports, "${GATEWAY_PORT:-9443}:${GATEWAY_PORT:-9443}") {
		t.Errorf("gateway ports = %q", ports)
	}
}

func TestComposeTrustBoundaries(t *testing.T) {
	doc := loadCompose(t)

	con := service(t, doc, "console")
	cv := strings.Join(asStrings(t, con["volumes"], "console volumes"), "\n")
	for _, bad := range []string{"RCLONE_", "rclone"} {
		if strings.Contains(cv, bad) {
			t.Errorf("console mounts %q: it must hold no storage credential (DH1)", bad)
		}
	}
	for _, want := range []string{"db:/data", "gateway-auth:/auth\n", "artifacts:/artifacts", "adminpath:/run/cryo", ":/config/config.json:ro", "${MASTER_KEY_DIR:-./secrets}:/run/keys:ro", ":/tls:ro"} {
		if !strings.Contains(cv+"\n", want) {
			t.Errorf("console volumes lack %q", want)
		}
	}
	cports := strings.Join(asStrings(t, con["ports"], "console ports"), " ")
	for _, want := range []string{"${AGENT_PORT:-8443}:8443", "${ADMIN_BIND:-127.0.0.1}:${ADMIN_PORT:-8080}:8080"} {
		if !strings.Contains(cports, want) {
			t.Errorf("console ports lack %q", want)
		}
	}
	if got := asStrings(t, con["command"], "console command"); strings.Join(got, " ") != "serve --config /config/config.json" {
		t.Errorf("console command = %v", got)
	}

	mt := service(t, doc, "maintenance")
	if _, ok := mt["ports"]; ok {
		t.Error("maintenance publishes ports")
	}
	mv := strings.Join(asStrings(t, mt["volumes"], "maintenance volumes"), "\n")
	for _, want := range []string{"db:/data", "adminpath:/run/cryo", "${RCLONE_ADMIN_CONF:-./rclone.admin.conf}:/config/rclone.conf:ro", "${MASTER_KEY_DIR:-./secrets}:/run/keys:ro"} {
		if !strings.Contains(mv, want) {
			t.Errorf("maintenance volumes lack %q", want)
		}
	}
	if strings.Contains(mv, "RCLONE_GATEWAY_CONF") || strings.Contains(mv, "/tls") {
		t.Error("maintenance mounts the gateway credential or TLS")
	}
	if got := asStrings(t, mt["command"], "maintenance command"); strings.Join(got, " ") != "maintain --loop --config /config/config.json" {
		t.Errorf("maintenance command = %v", got)
	}
	env := asMap(t, mt["environment"], "maintenance environment")
	if env["CRYO_MAINTAIN_REMOTE"] != "admin" || env["RCLONE_CONFIG"] != "/config/rclone.conf" {
		t.Errorf("maintenance environment = %v", env)
	}
}

func TestComposeVariablesAreInEnvExample(t *testing.T) {
	envSrc := readDeploy(t, "compose", ".env.example")
	have := map[string]bool{}
	for _, l := range strings.Split(envSrc, "\n") {
		if m := regexp.MustCompile(`^([A-Z][A-Z0-9_]*)=`).FindStringSubmatch(l); m != nil {
			have[m[1]] = true
		}
	}
	src := readDeploy(t, "compose", "docker-compose.yml")
	used := map[string]bool{}
	// A "${NAME" preceded by "$" is an escaped literal for the container's shell.
	for _, m := range regexp.MustCompile(`(^|[^$])\$\{([A-Z][A-Z0-9_]*)`).FindAllStringSubmatch(src, -1) {
		used[m[2]] = true
	}
	if len(used) < 20 {
		t.Fatalf("found only %d variables in the compose file", len(used))
	}
	for v := range used {
		if !have[v] {
			t.Errorf("compose variable %s has no entry in .env.example", v)
		}
	}
	for v := range have {
		if !used[v] {
			t.Errorf(".env.example defines %s, which docker-compose.yml never references", v)
		}
	}
	for _, name := range []string{"RESTIC_SHA256", "RCLONE_SHA256"} {
		m := regexp.MustCompile(`(?m)^` + name + `=([0-9a-f]{64})$`).FindStringSubmatch(envSrc)
		if m == nil {
			t.Errorf("%s in .env.example is not 64 lower-case hex digits", name)
		}
	}
	// the minio-init script reads exactly the variables the compose service passes
	script := readDeploy(t, "compose", "minio-init.sh")
	for _, v := range []string{"MINIO_ROOT_USER", "MINIO_ROOT_PASSWORD", "BUCKET", "LAB_GATEWAY_ACCESS_KEY", "LAB_GATEWAY_SECRET_KEY", "LAB_ADMIN_ACCESS_KEY", "LAB_ADMIN_SECRET_KEY"} {
		if !strings.Contains(script, v) || !strings.Contains(src, v+":") {
			t.Errorf("%s is not wired between docker-compose.yml and minio-init.sh", v)
		}
	}
}

func TestDockerfilesVerifyChecksums(t *testing.T) {
	console := readDeploy(t, "compose", "console.Dockerfile")
	gw := readDeploy(t, "compose", "gateway.Dockerfile")
	for name, src := range map[string]string{"console": console, "gateway": gw} {
		if !regexp.MustCompile(`(?m)^ARG RCLONE_SHA256$`).MatchString(src) {
			t.Errorf("%s.Dockerfile: ARG RCLONE_SHA256 must be declared with no default", name)
		}
		if !strings.Contains(src, "sha256sum -c -") || !strings.Contains(src, `test -n "${RCLONE_SHA256}"`) {
			t.Errorf("%s.Dockerfile does not verify RCLONE_SHA256 (or accepts an empty one)", name)
		}
		if strings.Contains(src, ":latest") {
			t.Errorf("%s.Dockerfile uses :latest", name)
		}
	}
	if !regexp.MustCompile(`(?m)^ARG RESTIC_SHA256$`).MatchString(console) || !strings.Contains(console, `test -n "${RESTIC_SHA256}"`) {
		t.Error("console.Dockerfile does not verify RESTIC_SHA256")
	}
	if !strings.Contains(console, "CGO_ENABLED=0") {
		t.Error("console.Dockerfile does not build with CGO_ENABLED=0")
	}
	if !strings.Contains(console, "restic.sha256") {
		t.Error("console.Dockerfile does not record the restic binary's hash for the worker's start-up check")
	}
}

// TestMinioLabIsBuiltFromPinnedModules pins the lab S3 target. MinIO publishes no images
// or binaries any more (community edition: source only, repository archived), so the lab
// builds minio and mc from the Go module proxy at two immutable pseudo-versions
// (deploy/compose/minio.Dockerfile). The pseudo-versions must agree in the Dockerfile,
// the compose defaults and .env.example, every compose build arg must be a Dockerfile
// ARG, and nothing may point back at a registry image.
func TestMinioLabIsBuiltFromPinnedModules(t *testing.T) {
	const (
		minioVer = "v0.0.0-20250907161309-07c3a429bfed" // RELEASE.2025-09-07T16-13-09Z
		mcVer    = "v0.0.0-20250813083541-7394ce0dd2a8" // RELEASE.2025-08-13T08-35-41Z
	)
	doc := loadCompose(t)
	src := readDeploy(t, "compose", "docker-compose.yml")
	envSrc := readDeploy(t, "compose", ".env.example")
	dockerfile := readDeploy(t, "compose", "minio.Dockerfile")

	for _, bad := range []string{"quay.io", "docker.io", "MINIO_IMAGE", "MC_IMAGE"} {
		if strings.Contains(src, bad) || strings.Contains(envSrc, bad) {
			t.Errorf("compose file or .env.example still references %q: the lab image is built, not pulled", bad)
		}
	}

	mn := service(t, doc, "minio")
	mi := service(t, doc, "minio-init")
	const image = "${MINIO_LAB_IMAGE:-cryobackup/minio-lab}:${CRYO_VERSION:-dev}"
	if mn["image"] != image {
		t.Errorf("minio image = %v, want %s", mn["image"], image)
	}
	if mi["image"] != mn["image"] {
		t.Errorf("minio-init image = %v: it must be the image minio builds (mc is in it)", mi["image"])
	}
	if _, ok := mi["build"]; ok {
		t.Error("minio-init has its own build: it must reuse the minio image")
	}

	build := asMap(t, mn["build"], "minio build")
	if build["context"] != "." || build["dockerfile"] != "minio.Dockerfile" {
		t.Errorf("minio build = %v", build)
	}
	args := asMap(t, build["args"], "minio build args")
	wantArgs := map[string]string{
		"GO_IMAGE":             "${GO_IMAGE:-golang:1.26.10-bookworm}",
		"RUNTIME_IMAGE":        "${RUNTIME_IMAGE:-debian:12.11-slim}",
		"MINIO_MODULE_VERSION": "${MINIO_MODULE_VERSION:-" + minioVer + "}",
		"MC_MODULE_VERSION":    "${MC_MODULE_VERSION:-" + mcVer + "}",
	}
	if len(args) != len(wantArgs) {
		t.Errorf("minio build args = %v", args)
	}
	for k, want := range wantArgs {
		if args[k] != want {
			t.Errorf("minio build arg %s = %v, want %s", k, args[k], want)
		}
		if !regexp.MustCompile(`(?m)^ARG ` + k + `(=|$)`).MatchString(dockerfile) {
			t.Errorf("minio.Dockerfile declares no ARG %s", k)
		}
	}
	for name, want := range map[string]string{"MINIO_MODULE_VERSION": minioVer, "MC_MODULE_VERSION": mcVer} {
		if !regexp.MustCompile(`(?m)^` + name + `=` + regexp.QuoteMeta(want) + `$`).MatchString(envSrc) {
			t.Errorf(".env.example does not pin %s=%s", name, want)
		}
		if !regexp.MustCompile(`(?m)^ARG ` + name + `=` + regexp.QuoteMeta(want) + `$`).MatchString(dockerfile) {
			t.Errorf("minio.Dockerfile default for %s is not %s", name, want)
		}
	}
	if !regexp.MustCompile(`(?m)^MINIO_LAB_IMAGE=cryobackup/minio-lab$`).MatchString(envSrc) {
		t.Error(".env.example lacks MINIO_LAB_IMAGE=cryobackup/minio-lab")
	}

	// the Dockerfile builds both modules from source at the pinned versions, verified by the
	// default checksum database, and runs as a non-root user
	for _, want := range []string{
		"CGO_ENABLED=0",
		`"github.com/minio/minio@${MINIO_MODULE_VERSION}"`,
		`"github.com/minio/mc@${MC_MODULE_VERSION}"`,
		"USER 10002:10002",
		"VOLUME /data",
		"EXPOSE 9000 9001",
		"ca-certificates",
		"curl",
	} {
		if !strings.Contains(dockerfile, want) {
			t.Errorf("minio.Dockerfile lacks %q", want)
		}
	}
	for _, bad := range []string{":latest", "GONOSUMDB", "GONOSUMCHECK", "GOFLAGS=-insecure", "GOSUMDB=off", "GOINSECURE", "@latest"} {
		if strings.Contains(dockerfile, bad) {
			t.Errorf("minio.Dockerfile contains %q", bad)
		}
	}

	// the liveness check is MinIO's documented endpoint, not `mc ready` (no alias is configured)
	hc := strings.Join(asStrings(t, asMap(t, mn["healthcheck"], "minio healthcheck")["test"], "minio healthcheck.test"), " ")
	if !strings.Contains(hc, "curl") || !strings.Contains(hc, "http://127.0.0.1:9000/minio/health/live") || strings.Contains(hc, "mc ") {
		t.Errorf("minio healthcheck = %q", hc)
	}

	// --console-address is accepted by `minio server` at the pinned release (cmd/server-main.go),
	// so the console mapping stays and must match the flag
	cmd := strings.Join(asStrings(t, mn["command"], "minio command"), " ")
	if cmd != "server /data --console-address :9001" {
		t.Errorf("minio command = %q", cmd)
	}
	if ports := strings.Join(asStrings(t, mn["ports"], "minio ports"), " "); !strings.Contains(ports, ":${MINIO_CONSOLE_PORT:-9001}:9001") {
		t.Errorf("minio ports = %q", ports)
	}

	// minio-init keeps its script as the entrypoint
	if got := strings.Join(asStrings(t, mi["entrypoint"], "minio-init entrypoint"), " "); got != "/bin/sh /minio-init.sh" {
		t.Errorf("minio-init entrypoint = %q", got)
	}
}

// TestRcloneSelfCheckUnsetsRcloneVersion: the RCLONE_VERSION build arg is exported into the
// RUN step's environment, and rclone reads every RCLONE_<FLAG> variable as that flag
// (RCLONE_VERSION is --version, a bool), so `rclone version` dies unless the variable is
// removed for that one command. A lab build failed on exactly this.
func TestRcloneSelfCheckUnsetsRcloneVersion(t *testing.T) {
	for _, name := range []string{"console.Dockerfile", "gateway.Dockerfile"} {
		n := 0
		for _, l := range strings.Split(readDeploy(t, "compose", name), "\n") {
			if !strings.Contains(l, "/out/rclone version") {
				continue
			}
			n++
			if !strings.Contains(l, "env -u RCLONE_VERSION /out/rclone version") {
				t.Errorf("%s: rclone self-check does not unset RCLONE_VERSION: %q", name, strings.TrimSpace(l))
			}
		}
		if n == 0 {
			t.Errorf("%s has no rclone version self-check", name)
		}
	}
}

func TestConfigExampleJSON(t *testing.T) {
	var cfg map[string]any
	if err := json.Unmarshal([]byte(readDeploy(t, "compose", "config.example.json")), &cfg); err != nil {
		t.Fatalf("config.example.json: %v", err)
	}
	for _, k := range []string{"data_dir", "db_path", "master_key_path", "agent_listen", "admin_listen", "admin_allow_public",
		"tls", "public_url", "gateway_url", "gateway_auth_dir", "artifacts_dir", "adminpath_socket", "display_timezone",
		"refresh_interval_s", "session", "rate_limits", "fixtures"} {
		if _, ok := cfg[k]; !ok {
			t.Errorf("config.example.json lacks %q (keys of the P1-T5 config section)", k)
		}
	}
	// the container paths match the compose mounts
	for k, want := range map[string]string{"master_key_path": "/run/keys/master.key", "gateway_auth_dir": "/auth",
		"artifacts_dir": "/artifacts", "adminpath_socket": "/run/cryo/adminpath.sock"} {
		if cfg[k] != want {
			t.Errorf("%s = %v, want %s", k, cfg[k], want)
		}
	}
}

func TestSystemdUnits(t *testing.T) {
	hardening := []string{"ProtectSystem=strict", "NoNewPrivileges=yes", "PrivateTmp=yes", "CapabilityBoundingSet="}
	for _, u := range []string{"cryo-console.service", "cryo-maintain.service", "rclone-gateway.service"} {
		src := readDeploy(t, "systemd", u)
		for _, h := range hardening {
			if !strings.Contains(src, h) {
				t.Errorf("%s lacks %s", u, h)
			}
		}
		for _, sec := range []string{"[Unit]", "[Service]", "[Install]"} {
			if !strings.Contains(src, sec) {
				t.Errorf("%s lacks %s", u, sec)
			}
		}
		if regexp.MustCompile(`(?m)^Environment=.*(APPEND_ONLY|BYPASS)`).MatchString(src) {
			t.Errorf("%s sets a security flag through the environment", u)
		}
	}
	gw := readDeploy(t, "systemd", "rclone-gateway.service")
	for _, flag := range []string{"serve restic", "--min-tls-version tls1.2", "--htpasswd /var/lib/cryobackup-auth/htpasswd", "--append-only", "--private-repos", "store:${BUCKET}/${PREFIX}", "--cert", "--key"} {
		if !strings.Contains(gw, flag) {
			t.Errorf("rclone-gateway.service lacks %q", flag)
		}
	}
	// What a unit uses, not what it hides: the InaccessiblePaths= line (M2-W1B) names the
	// other tiers' files on purpose, and comments are not settings.
	uses := func(unit string) string {
		var keep []string
		for _, l := range unitLines(readDeploy(t, "systemd", unit)) {
			if !strings.HasPrefix(l, "InaccessiblePaths=") {
				keep = append(keep, l)
			}
		}
		return strings.Join(keep, "\n")
	}
	if strings.Contains(uses("rclone-gateway.service"), "rclone-admin") || strings.Contains(uses("cryo-console.service"), "rclone-") {
		t.Error("a unit that must not reach the admin credential references an rclone file")
	}
	mt := uses("cryo-maintain.service")
	if !strings.Contains(mt, "rclone-admin.conf") || strings.Contains(mt, "rclone-gateway.conf") {
		t.Error("cryo-maintain.service must use the admin rclone file only")
	}
}

func TestDeployHygiene(t *testing.T) {
	gi := readDeploy(t, ".gitignore")
	for _, want := range []string{".env", "rclone.conf", "*.crt", "*.key", "*.pem"} {
		if !strings.Contains(gi, want) {
			t.Errorf("deploy/.gitignore lacks %s", want)
		}
	}
	var files []string
	root := deployPath(t)
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range files {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "\r") {
			t.Errorf("%s has CR characters: deployment files use LF", p)
		}
		if strings.HasSuffix(p, ".sh") && !strings.HasPrefix(string(b), "#!") {
			t.Errorf("%s has no shebang", p)
		}
		// nothing installation-specific or secret-shaped (the Go module path, needed for
		// the build's -X flags, is the project's own name and is exempt)
		text := strings.ReplaceAll(string(b), "github.com/c2tech-sys/cryobackup", "")
		for _, bad := range []string{"c2tech", "C2 Tech", "c2techsys", "cullman", "AKIA", "BEGIN PRIVATE KEY", "BEGIN RSA PRIVATE KEY"} {
			if strings.Contains(strings.ToLower(text), strings.ToLower(bad)) {
				t.Errorf("%s contains %q", p, bad)
			}
		}
	}
}

func TestShellScriptsParse(t *testing.T) {
	for _, tc := range []struct {
		shell string
		path  []string
	}{
		{"bash", []string{"..", "..", "..", "deploy", "rclone", "append-only-test.sh"}},
		{"bash", []string{"..", "..", "..", "test", "lab", "smoke-gateway.sh"}},
		{"sh", []string{"..", "..", "..", "deploy", "compose", "minio-init.sh"}},
	} {
		if _, err := exec.LookPath(tc.shell); err != nil {
			t.Logf("%s not found: %s not syntax-checked", tc.shell, filepath.Base(tc.path[len(tc.path)-1]))
			continue
		}
		p := filepath.Join(tc.path...)
		if out, err := exec.Command(tc.shell, "-n", p).CombinedOutput(); err != nil {
			t.Errorf("%s -n %s: %v\n%s", tc.shell, filepath.Base(p), err, out)
		}
	}
}

// TestNoSecretOnACommandLine checks the rule for every script under deploy/ and
// test/lab/: a command line is visible to every local process, so no secret may be
// placed on one.
func TestNoSecretOnACommandLine(t *testing.T) {
	scripts := map[string]string{
		"append-only-test.sh": readDeploy(t, "rclone", "append-only-test.sh"),
		"minio-init.sh":       readDeploy(t, "compose", "minio-init.sh"),
	}
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "test", "lab", "smoke-gateway.sh"))
	if err != nil {
		t.Fatal(err)
	}
	scripts["smoke-gateway.sh"] = string(b)
	for name, src := range scripts {
		for _, bad := range []string{"openssl kdf", "hexkey", "alias set", "--password ", "-u \"probe:", "Bearer $"} {
			if strings.Contains(src, bad) {
				t.Errorf("%s contains %q", name, bad)
			}
		}
		if strings.Contains(src, `sed -e "s/${`) {
			t.Errorf("%s puts a secret on sed's command line", name)
		}
	}
	if !strings.Contains(scripts["smoke-gateway.sh"], "--config -") {
		t.Error("smoke-gateway.sh must hand the probe password to curl on stdin (--config -)")
	}
}

// TestProbePasswordHelperMatchesGateway proves test/lab/probe-password.py (reads the
// key on stdin) computes what internal/console/gateway.ProbePassword computes.
func TestProbePasswordHelperMatchesGateway(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		if py, err = exec.LookPath("python"); err != nil {
			t.Skip("python not found")
		}
	}
	key := bytes.Repeat([]byte{0x5c}, 32)
	want, err := gateway.ProbePassword(key)
	if err != nil {
		t.Fatal(err)
	}
	helper, err := filepath.Abs(filepath.Join("..", "..", "..", "test", "lab", "probe-password.py"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(py, "-I", helper)
	cmd.Stdin = strings.NewReader(base64.StdEncoding.EncodeToString(key) + "\n")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helper: %v", err)
	}
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("helper = %q, gateway.ProbePassword = %q", strings.TrimSpace(string(out)), want)
	}
	cmd = exec.Command(py, "-I", helper)
	cmd.Stdin = strings.NewReader("not base64 !!")
	if err := cmd.Run(); err == nil {
		t.Fatal("helper accepted garbage")
	}
}
