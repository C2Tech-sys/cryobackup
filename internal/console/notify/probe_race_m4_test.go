//go:build race

package notify

// raceBuild is true in a build with the race detector (-race): TestC1InvariantProbe then runs its
// documented subset of cells (probeRaceCells), so the package fits the race gate's timeout.
const raceBuild = true
