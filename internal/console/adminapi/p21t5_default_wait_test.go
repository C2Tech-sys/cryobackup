package adminapi

import (
	"testing"
	"time"
)

// P21-T5 fix batch 1 (S3): the retire's wait is five seconds in the product, for the admin API and,
// through ui.Service, the pages' in-process one. A silent change of the default is a change of how long
// an operator waits at the retire button and how long a pending answer can be withheld, so it is pinned
// here and a change must be made on purpose, with this test and the docs that quote 5 s.
func TestDefaultGatewaySyncWaitIsFiveSeconds(t *testing.T) {
	if DefaultGatewaySyncWait != 5*time.Second {
		t.Fatalf("DefaultGatewaySyncWait = %v, want 5s: the docs and the retire page quote it", DefaultGatewaySyncWait)
	}
}
