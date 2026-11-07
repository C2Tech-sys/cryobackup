package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// M3-HARDEN A6: a saved view whose customer clause names a MISSING customer is a
// question about that customer: the board answers with its alarm, not "No devices match".
func TestBoardViewNamingAMissingCustomerShowsItsAlarm(t *testing.T) {
	e := newEnv(t)
	e.login()
	id := e.customerID("Blue Ridge Accounting")
	v, err := e.st.CreateSavedViewAudited(context.Background(), store.ScopeAll(), store.SavedViewInput{Name: "Blue Ridge", Entity: "devices",
		Filter: json.RawMessage(fmt.Sprintf(`{"field":"customer","op":"in","value":[%q]}`, id))}, e.t5Actor())
	if err != nil {
		t.Fatal(err)
	}
	got := e.t5Board(boardQuery{View: v.ID})
	if got.Empty != nil {
		t.Errorf("the board says %q about a MISSING customer", got.Empty.Title)
	}
	if len(got.Groups) != 1 || got.Groups[0].Name != "Blue Ridge Accounting" || got.Groups[0].Variant != "missing" {
		t.Fatalf("groups = %v, want the MISSING customer's alarm group", groupNames(got))
	}
	page := e.page("/?view="+v.ID, 200)
	mustContain(t, "view of a MISSING customer", page, "Blue Ridge Accounting", "0 devices · expected")
	if strings.Contains(page, "No devices match") {
		t.Error("the page reads No devices match")
	}

	// Excluding the customer does not name it.
	v2, err := e.st.CreateSavedViewAudited(context.Background(), store.ScopeAll(), store.SavedViewInput{Name: "Not Blue Ridge", Entity: "devices",
		Filter: json.RawMessage(fmt.Sprintf(`{"field":"customer","op":"not_in","value":[%q]}`, id))}, e.t5Actor())
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range e.t5Board(boardQuery{View: v2.ID}).Groups {
		if g.Name == "Blue Ridge Accounting" {
			t.Error("a view that excludes the customer shows it")
		}
	}
}

// M3-HARDEN A6: a state filter cannot match a customer with no devices. The customer
// is left out, and the page names it and says why.
func TestBoardStateFilterSaysWhyAMissingCustomerIsLeftOut(t *testing.T) {
	e := newEnv(t)
	e.login()
	got := e.t5Board(boardQuery{States: []string{"FAILED"}})
	for _, g := range got.Groups {
		if g.Name == "Blue Ridge Accounting" {
			t.Fatalf("a state filter listed a customer with no devices: %v", groupNames(got))
		}
	}
	if len(got.Filter.MissingOut) != 1 || got.Filter.MissingOut[0] != "Blue Ridge Accounting" {
		t.Fatalf("MissingOut = %v", got.Filter.MissingOut)
	}
	page := e.page("/?state=FAILED", 200)
	mustContain(t, "state filter", page, `id="filter-missing-out"`, "Blue Ridge Accounting", "has none to match", `href="/?cust=crit"`)

	// No filter, nothing to explain.
	if out := e.t5Board(boardQuery{}).Filter.MissingOut; len(out) != 0 {
		t.Errorf("MissingOut without a filter = %v", out)
	}
	if page := e.page("/", 200); strings.Contains(page, "filter-missing-out") {
		t.Error("the unfiltered board explains a filter")
	}
}
