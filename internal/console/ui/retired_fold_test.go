package ui

import (
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/state"
)

// The "Retired (n)" list is an aligned grid: a host name retired more than once reads newest first,
// and the customer column exists only when a row names a customer.
func TestRetiredFoldOrdersAHostNewestFirstAndShowsCustomersOnlyWhenNamed(t *testing.T) {
	t.Parallel()
	at := func(d int) time.Time { return time.Date(2026, 11, d, 16, 0, 0, 0, time.UTC) }
	dev := func(id, host string, d int) state.DeviceState {
		return state.DeviceState{ID: id, Hostname: host, CustomerID: "c1", Primary: state.StateRetired, Since: at(d)}
	}
	cs := &state.CustomerState{Name: "Sample Clinic", Devices: []state.DeviceState{dev("a", "LAB-B", 9), dev("b", "LAB-A", 9), dev("c", "LAB-B", 10)}}

	f := foldOf([]*state.CustomerState{cs}, fmtCtx{}, true, nil)
	var got []string
	for _, r := range f.Rows {
		got = append(got, r.ID)
	}
	if strings.Join(got, ",") != "b,c,a" {
		t.Errorf("order = %v, want LAB-A, then LAB-B newest first (b,c,a)", got)
	}
	if !f.ShowCustomer() {
		t.Error("a fold whose rows name a customer has no customer column")
	}
	if one := foldOf([]*state.CustomerState{cs}, fmtCtx{}, false, nil); one.ShowCustomer() {
		t.Error("one customer's fold has a customer column")
	}
}
