package agentapi

import "sync"

// Notifier wakes the commands long poll of a device the moment a command is
// enqueued for it. Chan returns the channel to wait on; Notify closes it. To
// avoid a lost wake-up, a poller calls Chan before it checks the queue.
type Notifier struct {
	mu sync.Mutex
	ch map[string]chan struct{}
}

// NewNotifier returns an empty Notifier.
func NewNotifier() *Notifier { return &Notifier{ch: map[string]chan struct{}{}} }

// Chan returns the current wake-up channel of the device.
func (n *Notifier) Chan(deviceID string) <-chan struct{} {
	n.mu.Lock()
	defer n.mu.Unlock()
	c, ok := n.ch[deviceID]
	if !ok {
		c = make(chan struct{})
		n.ch[deviceID] = c
	}
	return c
}

// Notify wakes every poller currently waiting on the device.
func (n *Notifier) Notify(deviceID string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if c, ok := n.ch[deviceID]; ok {
		close(c)
		delete(n.ch, deviceID)
	}
}
