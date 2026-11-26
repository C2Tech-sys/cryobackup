package health

// BannerWorkerMode is the ID of the banner raised while the console's drill mode and its
// maintenance worker's differ (P22-R2b; cli/workermode.go raises it through Evaluator.Extra).
const BannerWorkerMode = "worker_mode_mismatch"
