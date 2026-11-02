package server_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/console/server"
)

// M2-W3D: the OpenAPI document (api/openapi.yaml) and the routes the console
// registers must agree, in both directions. This is the "OpenAPI diff clean
// for all M0-M2 routes" acceptance check (architecture section 10.2, M2).
//
// The repository has no YAML dependency and a test must not add one, so the
// document is read by a small scanner that relies on the layout this file keeps
// (two-space indent: a path at depth 2, an HTTP method at depth 4, the
// operation body deeper; components at depth 2 and 4). The scanner fails loudly
// when the layout is not what it expects, so a reformat cannot make the diff
// pass by reading nothing.

const specPath = "../../../api/openapi.yaml"

var httpMethods = map[string]bool{"get": true, "post": true, "put": true, "patch": true, "delete": true, "head": true, "options": true}

type specOp struct {
	method, path string
	body         []string
}

func (o specOp) pattern() string { return strings.ToUpper(o.method) + " " + o.path }

func (o specOp) has(substr string) bool {
	for _, l := range o.body {
		if strings.Contains(l, substr) {
			return true
		}
	}
	return false
}

type specDoc struct {
	ops        []specOp
	components map[string]bool // "schemas/Problem", "parameters/Id", ...
	refs       []string        // every '#/components/...' reference, as "group/name"
}

var (
	refRE      = regexp.MustCompile(`\$ref:\s*'#/components/([A-Za-z]+/[A-Za-z0-9_.-]+)'`)
	wildcardRE = regexp.MustCompile(`\{([A-Za-z0-9_]+)\}`)
)

func indentOf(l string) int { return len(l) - len(strings.TrimLeft(l, " ")) }

func readSpec(t *testing.T) specDoc {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash(specPath))
	if err != nil {
		t.Fatalf("the OpenAPI document is missing: %v", err)
	}
	text := string(raw)
	if strings.Contains(text, "\r") {
		t.Fatal("api/openapi.yaml has CR characters; keep it LF only")
	}
	doc := specDoc{components: map[string]bool{}}
	section, group, curPath := "", "", ""
	cur := -1
	for n, l := range strings.Split(text, "\n") {
		trim := strings.TrimSpace(l)
		if trim == "" || strings.HasPrefix(trim, "#") {
			continue
		}
		for _, m := range refRE.FindAllStringSubmatch(l, -1) {
			doc.refs = append(doc.refs, m[1])
		}
		ind := indentOf(l)
		if ind == 0 {
			section, group, cur = strings.TrimSuffix(trim, ":"), "", -1
			continue
		}
		switch section {
		case "paths":
			switch {
			case ind == 2 && strings.HasPrefix(trim, "/") && strings.HasSuffix(trim, ":"):
				curPath, cur = strings.TrimSuffix(trim, ":"), -1
			case ind == 4 && strings.HasSuffix(trim, ":") && httpMethods[strings.TrimSuffix(trim, ":")]:
				doc.ops = append(doc.ops, specOp{method: strings.TrimSuffix(trim, ":"), path: curPath})
				cur = len(doc.ops) - 1
			case ind == 4:
				cur = -1 // path-level keys such as parameters
			case ind > 4 && cur >= 0:
				doc.ops[cur].body = append(doc.ops[cur].body, l)
			case ind == 2:
				t.Fatalf("api/openapi.yaml line %d: unexpected key under paths: %q", n+1, trim)
			}
		case "components":
			switch {
			case ind == 2 && strings.HasSuffix(trim, ":"):
				group = strings.TrimSuffix(trim, ":")
			case ind == 4 && group != "":
				if name, _, ok := strings.Cut(trim, ":"); ok && !strings.HasPrefix(name, "#") {
					doc.components[group+"/"+name] = true
				}
			}
		}
	}
	return doc
}

// registeredAPI returns the /api/v1 patterns of both listeners, from the real
// routers of a fixtures-mode console.
func registeredAPI(t *testing.T) (agent, admin []string) {
	t.Helper()
	a := fixturesApp(t, nil)
	for _, p := range a.Server.AgentPatterns() {
		if strings.HasPrefix(server.PathOf(p), api.PathPrefix) {
			agent = append(agent, p)
		}
	}
	for _, p := range a.Server.AdminPatterns() {
		if strings.HasPrefix(server.PathOf(p), "/api/v1/") {
			admin = append(admin, p)
		}
	}
	sort.Strings(agent)
	sort.Strings(admin)
	return agent, admin
}

func TestOpenAPISpecMatchesTheRegisteredRoutes(t *testing.T) {
	doc := readSpec(t)
	if len(doc.ops) < 60 {
		t.Fatalf("the scanner read %d operations from api/openapi.yaml; expected the whole document (layout changed?)", len(doc.ops))
	}
	agent, admin := registeredAPI(t)
	if len(agent) == 0 || len(admin) == 0 {
		t.Fatalf("no registered routes found (agent %d, admin %d)", len(agent), len(admin))
	}

	inSpec := map[string]bool{}
	for _, o := range doc.ops {
		if inSpec[o.pattern()] {
			t.Errorf("api/openapi.yaml lists %s twice", o.pattern())
		}
		inSpec[o.pattern()] = true
	}
	registered := map[string]bool{}
	for _, p := range append(append([]string(nil), agent...), admin...) {
		registered[p] = true
		if !inSpec[p] {
			t.Errorf("route %q is registered but has no entry in api/openapi.yaml", p)
		}
	}
	for _, o := range doc.ops {
		if !registered[o.pattern()] {
			t.Errorf("api/openapi.yaml documents %s, which is not a registered route", o.pattern())
		}
	}
}

// The document is hand-written, so its own consistency is checked too: no
// dangling $ref, an operationId on every operation (and unique), the role and
// the responses stated, every path wildcard declared, and CSRF documented on
// every cookie-authenticated write.
func TestOpenAPISpecIsInternallyConsistent(t *testing.T) {
	doc := readSpec(t)
	if len(doc.ops) < 60 {
		t.Fatalf("the scanner read %d operations from api/openapi.yaml; expected the whole document (layout changed?), so no check below means anything", len(doc.ops))
	}
	if len(doc.components) == 0 {
		t.Fatal("the scanner read no components; the $ref check would pass on nothing")
	}
	for _, r := range doc.refs {
		if !doc.components[r] {
			t.Errorf("api/openapi.yaml refers to #/components/%s, which is not defined", r)
		}
	}
	ids := map[string]string{}
	for _, o := range doc.ops {
		p := o.pattern()
		var id string
		for _, l := range o.body {
			if strings.HasPrefix(strings.TrimSpace(l), "operationId:") && indentOf(l) == 6 {
				id = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "operationId:"))
			}
		}
		switch {
		case id == "":
			t.Errorf("%s has no operationId", p)
		case ids[id] != "":
			t.Errorf("operationId %q is used by %s and %s", id, ids[id], p)
		default:
			ids[id] = p
		}
		for _, key := range []string{"summary:", "x-min-role:", "responses:"} {
			if !o.has("      " + key) {
				t.Errorf("%s has no %s", p, strings.TrimSuffix(key, ":"))
			}
		}
		for _, m := range wildcardRE.FindAllStringSubmatch(o.path, -1) {
			w := m[1]
			if !(w == "id" && o.has("parameters/Id")) && !o.has("name: "+w+",") && !o.has("name: "+w+"\n") {
				t.Errorf("%s: the path wildcard {%s} is not declared as a parameter", p, w)
			}
		}
		agentRoute := strings.HasPrefix(o.path, api.PathPrefix)
		write := o.method != "get"
		if write && !agentRoute && o.path != "/api/v1/auth/login" && !o.has("parameters/CsrfToken") {
			t.Errorf("%s is a cookie-authenticated write but does not document the CSRF header", p)
		}
		if agentRoute && !o.has("deviceToken") && !o.has("enrollmentToken") {
			t.Errorf("%s is an agent route but names no device or enrollment token", p)
		}
	}
}
