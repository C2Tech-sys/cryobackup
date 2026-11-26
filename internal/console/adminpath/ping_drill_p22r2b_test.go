package adminpath

import (
	"encoding/json"
	"testing"
)

// P22-R2b L2: a ping answer from a worker that predates the drill field decodes to Drill nil ("not
// reported"), not to false; true and false round-trip. Killed mutant: Drill back to a plain bool.
func TestPingResultDrillIsThreeValued(t *testing.T) {
	var old PingResult
	if err := json.Unmarshal([]byte(`{"version":"0.3.0","uptime_ns":1,"master_key_loaded":true}`), &old); err != nil || old.Drill != nil {
		t.Fatalf("an older worker answer = %+v, %v: want Drill nil", old, err)
	}
	for _, want := range []bool{true, false} {
		b, _ := json.Marshal(PingResult{Drill: &want})
		var got PingResult
		if err := json.Unmarshal(b, &got); err != nil || got.Drill == nil || *got.Drill != want {
			t.Fatalf("round trip of %v: %s -> %+v, %v", want, b, got, err)
		}
	}
	if b, _ := json.Marshal(PingResult{}); string(b) != `{"version":"","uptime_ns":0,"master_key_loaded":false}` {
		t.Fatalf("an unset Drill is not omitted: %s", b)
	}
}
