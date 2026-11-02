package maintain

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

// The gateway credential must not be able to defeat bucket versioning (security
// review of 5f4a72c; Phase 0: "append-only + versioning/lifecycle, not
// immutability"). These tests parse the policy out of minio-init.sh, the template
// production policies copy, and check the versioning and documentation rules.

// scriptPolicy extracts the JSON heredoc written to /tmp/<file> by minio-init.sh and
// expands its shell variables with sample values.
func scriptPolicy(t *testing.T, script, file string) map[string]any {
	t.Helper()
	re := regexp.MustCompile(`(?s)cat > /tmp/` + regexp.QuoteMeta(file) + ` <<EOF\n(.*?)\nEOF\n`)
	m := re.FindStringSubmatch(script)
	if m == nil {
		t.Fatalf("minio-init.sh has no heredoc for %s", file)
	}
	body := strings.NewReplacer(
		"$LISTPFX", `"pfx", "pfx/*"`,
		"$OBJECTS", "arn:aws:s3:::bkt/pfx/*",
		"$LOCKS", "arn:aws:s3:::bkt/pfx/*/locks/*",
		"$BUCKET", "bkt",
	).Replace(m[1])
	if strings.Contains(body, "$") {
		t.Fatalf("%s: unexpanded variable in policy:\n%s", file, body)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("%s is not valid JSON after expansion: %v\n%s", file, err, body)
	}
	return doc
}

func stringList(t *testing.T, v any) []string {
	t.Helper()
	if s, ok := v.(string); ok {
		return []string{s}
	}
	raw, ok := v.([]any)
	if !ok {
		t.Fatalf("not a string or list: %v", v)
	}
	out := make([]string, 0, len(raw))
	for _, e := range raw {
		s, ok := e.(string)
		if !ok {
			t.Fatalf("not a string: %v", e)
		}
		out = append(out, s)
	}
	return out
}

func TestGatewayPolicyHasNoVersionOrLifecycleRights(t *testing.T) {
	script := readDeploy(t, "compose", "minio-init.sh")
	pol := scriptPolicy(t, script, "cryo-gateway.json")
	allowed := map[string]bool{
		"s3:GetBucketLocation": true, "s3:ListBucket": true, "s3:GetObject": true,
		"s3:PutObject": true, "s3:DeleteObject": true,
	}
	forbidden := []string{"s3:DeleteObjectVersion", "s3:PutLifecycleConfiguration", "s3:PutBucketVersioning", "s3:BypassGovernanceRetention"}
	stmts, _ := pol["Statement"].([]any)
	if len(stmts) == 0 {
		t.Fatal("gateway policy has no statements")
	}
	for i, s := range stmts {
		st := s.(map[string]any)
		if st["Effect"] != "Allow" {
			t.Errorf("statement %d: effect %v (the template is allow-only)", i, st["Effect"])
		}
		if _, ok := st["NotAction"]; ok {
			t.Errorf("statement %d uses NotAction", i)
		}
		res := stringList(t, st["Resource"])
		for _, a := range stringList(t, st["Action"]) {
			if !allowed[a] {
				t.Errorf("statement %d allows %q, which the gateway policy may not hold", i, a)
			}
			for _, f := range forbidden {
				if strings.EqualFold(a, f) {
					t.Errorf("statement %d allows %s", i, f)
				}
			}
			if strings.Contains(a, "*") {
				t.Errorf("statement %d has wildcard action %q", i, a)
			}
			if a == "s3:DeleteObject" {
				for _, r := range res {
					if !strings.HasSuffix(r, "/locks/*") {
						t.Errorf("statement %d: s3:DeleteObject on %q, only <prefix>/*/locks/* is allowed", i, r)
					}
				}
			}
		}
	}
	// the admin policy is where those rights live
	admin := scriptPolicy(t, script, "cryo-admin.json")
	as := admin["Statement"].([]any)[0].(map[string]any)
	if got := stringList(t, as["Action"]); len(got) != 1 || got[0] != "s3:*" {
		t.Errorf("admin policy actions = %v, want [s3:*]", got)
	}
}

func TestMinioInitEnablesVersioningBeforePolicies(t *testing.T) {
	script := readDeploy(t, "compose", "minio-init.sh")
	mb := strings.Index(script, `mc mb --ignore-existing "$ALIAS/$BUCKET"`)
	ver := strings.Index(script, `mc version enable "$ALIAS/$BUCKET"`)
	pol := strings.Index(script, "mc admin policy create")
	if mb < 0 || ver < 0 || pol < 0 || !(mb < ver && ver < pol) {
		t.Fatalf("order wrong: mc mb %d, mc version enable %d, policy create %d", mb, ver, pol)
	}
	// a failure to enable versioning must stop the script (set -eu, no `|| true`)
	line := script[ver : ver+strings.Index(script[ver:], "\n")]
	if strings.Contains(line, "||") {
		t.Errorf("versioning failure is swallowed: %s", line)
	}
	if !strings.Contains(script, "mc ilm rule add --noncurrent-expire-days") {
		t.Error("minio-init.sh has no non-current-version lifecycle example")
	}
}

func TestDocsStateVersioningIsRequired(t *testing.T) {
	for name, src := range map[string]string{
		"compose/README.md":       readDeploy(t, "compose", "README.md"),
		"rclone/gateway-flags.md": readDeploy(t, "rclone", "gateway-flags.md"),
	} {
		for _, want := range []string{"REQUIRED", "versioning", "Wasabi", "Backblaze B2", "IDrive e2", "AWS", "MinIO",
			"s3:DeleteObjectVersion", "s3:PutLifecycleConfiguration", "s3:PutBucketVersioning", "s3:BypassGovernanceRetention",
			"admin", "L-GOV-1", "M9"} {
			if !strings.Contains(src, want) {
				t.Errorf("%s does not mention %q", name, want)
			}
		}
	}
	conf := readDeploy(t, "compose", "rclone.conf.example")
	store := strings.Index(conf, "\n[store]\n")
	if store < 0 || !strings.Contains(conf[:store], "SCOPE THIS CREDENTIAL") || !strings.Contains(conf[:store], "s3:DeleteObjectVersion") {
		t.Error("rclone.conf.example has no credential-scoping comment before [store]")
	}
}
