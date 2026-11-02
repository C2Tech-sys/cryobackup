package api

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// schemaPath is replica/schema.sql, read relative to this package (test code
// only; the package itself does no I/O).
var schemaPath = filepath.Join("..", "..", "replica", "schema.sql")

func readSchema(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(schemaPath)
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}

// schemaEnum extracts the IN (...) list of the CHECK on table.column from the
// column definition line of schema.sql.
func schemaEnum(t *testing.T, schema, table, column string) []string {
	t.Helper()
	start := strings.Index(schema, "CREATE TABLE "+table+" (")
	if start < 0 {
		t.Fatalf("table %s not found in schema", table)
	}
	body := schema[start:]
	if end := strings.Index(body, "\n)"); end >= 0 {
		body = body[:end]
	}
	re := regexp.MustCompile(`(?m)^\s+` + column + `\s+TEXT\b.*?CHECK \(` + column + ` IN \(([^)]*)\)\)`)
	m := re.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no CHECK (%s IN (...)) on %s.%s", column, table, column)
	}
	var out []string
	for _, part := range strings.Split(m[1], ",") {
		out = append(out, strings.Trim(strings.TrimSpace(part), "'"))
	}
	sort.Strings(out)
	return out
}

func sorted(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func subset(sub, super []string) (missing []string) {
	set := map[string]bool{}
	for _, s := range super {
		set[s] = true
	}
	for _, s := range sub {
		if !set[s] {
			missing = append(missing, s)
		}
	}
	return missing
}

// enumBinding says which schema column(s) an Enums() entry is held against.
// equal: the protocol exposes the whole list; otherwise it exposes a subset.
type enumBinding struct {
	enum    string
	columns [][2]string // table, column
	equal   bool
}

var enumBindings = []enumBinding{
	{EnumRunKind, [][2]string{{"runs", "kind"}}, true},
	{EnumRunTrigger, [][2]string{{"runs", "trigger_kind"}}, true},
	{EnumRunOutcome, [][2]string{{"runs", "outcome"}}, true},
	{EnumDeferredReason, [][2]string{{"runs", "deferred_reason"}}, true},
	{EnumErrorClass, [][2]string{{"runs", "error_class"}}, true},
	{EnumCommandType, [][2]string{{"commands", "type"}}, true},
	{EnumCommandResultStatus, [][2]string{{"commands", "status"}}, false},
	{EnumRestoreMode, [][2]string{{"restore_requests", "mode"}}, false},
	{EnumRestoreOverwrite, [][2]string{{"restore_requests", "overwrite"}}, true},
	{EnumServiceState, [][2]string{{"device_presence", "service_state"}, {"heartbeats", "service_state"}}, true},
	{EnumArch, [][2]string{{"devices", "arch"}, {"enrollment_requests", "arch"}}, true},
	{EnumDeviceClass, [][2]string{{"devices", "device_class"}}, true},
	{EnumCustody, [][2]string{{"customers", "key_custody"}, {"repo_key_escrow", "custody"}}, true},
	{EnumLobbyStatus, [][2]string{{"enrollment_requests", "status"}}, false},
}

// protocolOnly are enums with no database column: they exist on the wire only.
// EnumLeaseOutcome lives in runs.stats (JSON), not in a column of its own.
var protocolOnly = map[string]bool{EnumRunResultStatus: true, EnumReleaseChannel: true, EnumErrorCode: true, EnumLeaseOutcome: true}

func TestEnumsAgreeWithSchema(t *testing.T) {
	schema := readSchema(t)
	enums := Enums()
	seen := map[string]bool{}
	for _, b := range enumBindings {
		seen[b.enum] = true
		got, ok := enums[b.enum]
		if !ok {
			t.Errorf("Enums() has no %q", b.enum)
			continue
		}
		for _, col := range b.columns {
			want := schemaEnum(t, schema, col[0], col[1])
			if b.equal {
				if !reflect.DeepEqual(sorted(got), want) {
					t.Errorf("%s differs from %s.%s:\n  protocol: %v\n  schema:   %v", b.enum, col[0], col[1], sorted(got), want)
				}
			} else if missing := subset(got, want); len(missing) > 0 {
				t.Errorf("%s has values the schema (%s.%s) does not allow: %v", b.enum, col[0], col[1], missing)
			}
		}
	}
	// Every enum is either bound to the schema or declared protocol-only, so a
	// new enum cannot escape the comparison.
	for name := range enums {
		if !seen[name] && !protocolOnly[name] {
			t.Errorf("enum %q is neither compared with the schema nor declared protocol-only", name)
		}
	}
	for name := range protocolOnly {
		if _, ok := enums[name]; !ok {
			t.Errorf("protocol-only enum %q is missing from Enums()", name)
		}
	}
}

func TestSchemaEnumExtractionIsSound(t *testing.T) {
	schema := readSchema(t)
	// A spot check that the extractor reads the intended column and not a
	// table-level CHECK that mentions the same name.
	if got := schemaEnum(t, schema, "restore_requests", "mode"); len(got) != 5 {
		t.Errorf("restore_requests.mode = %v, want the 5 modes", got)
	}
	if got := schemaEnum(t, schema, "commands", "status"); len(got) != 7 {
		t.Errorf("commands.status = %v, want 7", got)
	}
	if got := schemaEnum(t, schema, "runs", "error_class"); len(got) != len(restic.AllClasses()) {
		t.Errorf("runs.error_class has %d values, restic has %d", len(got), len(restic.AllClasses()))
	}
}

func TestEnumsAreWellFormed(t *testing.T) {
	enums := Enums()
	for name, vals := range enums {
		if len(vals) == 0 {
			t.Errorf("%s is empty", name)
		}
		seen := map[string]bool{}
		for _, v := range vals {
			if v == "" || seen[v] {
				t.Errorf("%s has an empty or duplicate value %q", name, v)
			}
			seen[v] = true
		}
	}
	// Enums() returns copies.
	enums[EnumArch][0] = "mutated"
	if Enums()[EnumArch][0] == "mutated" {
		t.Error("Enums() shares its slices with the package")
	}
}

// The constants the validators use must be the ones Enums() reports.
func TestValidatorsUseTheEnumLists(t *testing.T) {
	for _, v := range Enums()[EnumArch] {
		r := okEnrollRequest()
		r.Arch = Arch(v)
		if r.Validate() != nil {
			t.Errorf("arch %q rejected", v)
		}
	}
	for _, v := range Enums()[EnumRunTrigger] {
		r := okRun()
		r.Trigger = Trigger(v)
		if r.Validate() != nil {
			t.Errorf("trigger %q rejected", v)
		}
	}
	for _, v := range Enums()[EnumRunOutcome] {
		r := okRun()
		r.Outcome, r.DeferredReason = restic.Outcome(v), ""
		if v == string(restic.OutcomeDeferred) {
			r.DeferredReason = restic.DeferredLock
		}
		if err := r.Validate(); err != nil {
			t.Errorf("outcome %q rejected: %v", v, err)
		}
	}
	for _, v := range Enums()[EnumCommandType] {
		c := Command{ID: ulidA, Type: CommandType(v), CreatedAt: t0, ExpiresAt: at(1)}
		switch c.Type {
		case CommandHold:
			c.Params = okCommand().Params
		case CommandRestore:
			c.Params = okCommandsResponse().Commands[1].Params
		case CommandUpdateAgent:
			c.Params = []byte(`{"version":"1"}`)
		}
		if err := c.Validate(); err != nil {
			t.Errorf("command %q rejected: %v", v, err)
		}
	}
	for _, v := range Enums()[EnumServiceState] {
		r := okHeartbeat()
		r.ServiceState = ServiceState(v)
		if v != string(ServiceDeferred) {
			r.DeferredReason = ""
		}
		if err := r.Validate(); err != nil {
			t.Errorf("service state %q rejected: %v", v, err)
		}
	}
	for _, v := range Enums()[EnumDeviceClass] {
		r := okEnrollRequest()
		r.DeviceClass = DeviceClass(v)
		if r.Validate() != nil {
			t.Errorf("device class %q rejected", v)
		}
	}
}
