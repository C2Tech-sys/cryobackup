// Package hubsig is the neutral type the SSE hub (internal/console/ui) and the
// admin event stream (internal/console/adminapi) share: a change signal. It
// exists so neither package imports the other for it; it holds no behaviour.
package hubsig

// Signal is what the hub tells a watcher: Kind is Change (some device changed),
// Progress (a run of DeviceID made progress) or Refresh (the periodic
// re-evaluation tick).
type Signal struct {
	Kind     string
	DeviceID string
	RunID    string
}

// Signal kinds.
const (
	Change   = "change"
	Progress = "progress"
	Refresh  = "refresh"
)
