package ui

import "github.com/c2tech-sys/cryobackup/internal/console/health"

// The banner that says the console and its maintenance worker disagree about drill mode
// (health.BannerWorkerMode, cli/workermode.go). It is critical and has no action: the fix is on
// the host, in the worker's configuration.
func init() {
	bannerLabels[health.BannerWorkerMode] = "WORKER DRILL MODE MISMATCH"
	bannerLabels[health.BannerWorkerModeUnknown] = "WORKER MODE UNKNOWN"
}
