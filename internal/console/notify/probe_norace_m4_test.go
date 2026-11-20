//go:build !race

package notify

// raceBuild is false in a build without the race detector: TestC1InvariantProbe runs every cell.
const raceBuild = false
