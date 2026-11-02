package fakerestic

import (
	"fmt"
	"os"
	"os/signal"
	"time"
)

// Scenarios of ticket M1-FIX4.
func init() {
	// The gateway went away and restic is retrying (restic 0.19.1 retries
	// each backend operation for up to 15 minutes, and its lock refresh after
	// that): a few status lines, then nothing, and no exit until it is
	// interrupted (130). The agent's gateway watchdog is what ends it.
	backupFuncs["gateway_stall"] = gatewayStall

	// The same before restic printed its first status line: no output at all.
	backupFuncs["gateway_silent"] = func() int {
		c := make(chan os.Signal, 1)
		signal.Notify(c, os.Interrupt)
		select {
		case <-c:
			return 130
		case <-time.After(stallSafetyExit):
			return 0
		}
	}

	// A slow upload that never stops moving: a status line with more bytes
	// every 20 ms until it is interrupted (130).
	backupFuncs["progress_forever"] = progressForever

	// "restic version" takes three seconds (a binary check a cancel can land
	// in); the backup itself is backup_ok.
	versionDelay["version_slow"] = 3 * time.Second
	backupPlans["version_slow"] = backupPlans["backup_ok"]
}

// stallSafetyExit ends a fake that nobody interrupted, long after any test
// should have.
const stallSafetyExit = 2 * time.Minute

func gatewayStall() int {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	for i := 1; i <= 3; i++ {
		fmt.Printf(`{"message_type":"status","percent_done":%.2f,"total_files":10,"files_done":%d,"total_bytes":1000,"bytes_done":%d}`+"\n",
			float64(i)/10, i, i*100)
		time.Sleep(30 * time.Millisecond)
	}
	select {
	case <-c:
		return 130
	case <-time.After(stallSafetyExit):
		return 0
	}
}

func progressForever() int {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	deadline := time.After(stallSafetyExit)
	for i := 1; ; i++ {
		fmt.Printf(`{"message_type":"status","percent_done":0.5,"total_files":1000000,"files_done":%d,"total_bytes":1000000000000,"bytes_done":%d}`+"\n",
			i, i*1000)
		select {
		case <-c:
			return 130
		case <-deadline:
			return 0
		case <-time.After(20 * time.Millisecond):
		}
	}
}
