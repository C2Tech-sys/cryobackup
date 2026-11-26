package health

// The banners of the console-versus-worker drill mode check (P22-R2b B7; cli/workermode.go raises them
// through Evaluator.WorkerMode).
const (
	// BannerWorkerMode is raised, critical, while the console's drill mode and its maintenance worker's differ.
	BannerWorkerMode = "worker_mode_mismatch"
	// BannerWorkerModeUnknown is raised, as a warning, while a production console's worker does not report its
	// drill mode (a worker older than the field). A drill console with such a worker gets BannerWorkerMode:
	// a worker that cannot prove it is in drill mode is not trusted to be.
	BannerWorkerModeUnknown = "worker_mode_unknown"
)
