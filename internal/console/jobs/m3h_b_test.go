package jobs

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
	"github.com/c2tech-sys/cryobackup/internal/console/gateway"
)

// M3-HARDEN B (m06): a login counts as the retired device's only when the names are equal. A
// longer name that begins with it is another login, and one that it begins with is too.
func TestLoginFileCheckMatchesTheWholeName(t *testing.T) {
	j, st, id := loginRig(t)
	ctx := context.Background()
	retireForTest(t, st, id)
	if err := j.Run(ctx); err != nil { // a real file, for a real hash
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(j.Dir, gateway.FileName))
	if err != nil {
		t.Fatal(err)
	}
	_, _, real, err := gateway.Parse(raw)
	if err != nil || len(real) == 0 {
		t.Fatalf("parse: %v", err)
	}
	probeHash, hash := "", ""
	for _, l := range real {
		if l.Username == "probe" {
			probeHash = l.Hash
		} else {
			hash = l.Hash
		}
	}
	write := func(names ...string) {
		t.Helper()
		var logins []gateway.Login
		for _, n := range names {
			logins = append(logins, gateway.Login{Username: n, Hash: hash})
		}
		if err := os.WriteFile(filepath.Join(j.Dir, gateway.FileName), gateway.Render(logins, probeHash, 2), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(id+"0", id[:len(id)-1])
	if c := j.Check(ctx); c.ReadClass != "" || c.Has(id) || len(c.InFile) != 0 {
		t.Fatalf("names that only begin alike count as the login: %+v", c)
	}
	write(id)
	if c := j.Check(ctx); c.ReadClass != "" || !c.Has(id) {
		t.Fatalf("the login itself: %+v", c)
	}
	if (LoginFileCheck{InFile: []string{id + "0"}}).Has(id) {
		t.Error("Has matches a prefix")
	}
}

// M3-HARDEN B: fixtures cannot import this package, so its copy of the class is held here.
func TestFixturesAwaitingFirstEvaluationIsTheJobsConstant(t *testing.T) {
	if fixtures.AwaitingFirstEvaluation != ClassAwaitingFirstEvaluation {
		t.Fatalf("fixtures say %q, the job says %q", fixtures.AwaitingFirstEvaluation, ClassAwaitingFirstEvaluation)
	}
}
