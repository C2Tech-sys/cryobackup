package restores

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// The browse audit (ADR 0027 ruling C5; ticket P22-G): looking into a device's snapshot is
// audited as restore.browse once per admin, device and snapshot per BrowseAuditWindow. The row
// names the device and the snapshot id, never a folder: what was opened inside the snapshot stays
// out of the audit trail exactly as it stays out of every log.
//
// The once-per-window memory is in this process, on the Service (Service.browseSeen). A console
// restart forgets it, so the first browse after a restart is audited again: the trail can hold an
// extra row, never a missing one. A key counts as seen only once its row is written: a browse that
// arrives while the row of the same key is being written waits for that write and shares its
// result, so a failed write refuses every browse that relied on it (ticket P22-R2a, A3).

// BrowseAuditWindow is how long one restore.browse row covers an admin's browsing of one
// snapshot of one device.
const BrowseAuditWindow = time.Hour

// errBrowseWritePanicked is the recorded result of a restore.browse write that panicked: the
// browses that waited on it are refused as NOT AUDITED.
var errBrowseWritePanicked = errors.New("restores: the restore.browse write panicked")

type browseKey struct {
	admin, device, snapshot string
}

// browseClaim is one key's row: being written until done is closed, then written (err nil) at
// at, or failed (err set; the claim is gone from the memory by then).
type browseClaim struct {
	at   time.Time
	done chan struct{}
	err  error
	// waiters counts the browses waiting on this write (read by the tests).
	waiters int
}

// written reports whether the claim's write has finished; call with browseMu held.
func (c *browseClaim) written() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

// AuditBrowse records that who is browsing snapshotID of deviceID, unless a restore.browse row
// for the same admin, device and snapshot was written within BrowseAuditWindow. It refuses a
// device outside scope (store.ErrNotFound) and a malformed snapshot id (ErrInvalid). When the
// audit row cannot be written it returns an error wrapping store.ErrAudit and remembers nothing,
// so the caller refuses the listing ("a failed audit refuses") and the next browse tries again;
// a browse that waited on that write gets the same error.
func (s *Service) AuditBrowse(ctx context.Context, who Actor, scope store.Scope, deviceID, snapshotID string) error {
	if !snapshotIDRE.MatchString(snapshotID) || who.AdminID == "" {
		return ErrInvalid
	}
	dev, err := s.Store.GetDevice(ctx, scope, deviceID)
	if err != nil {
		return err
	}
	now := s.now()
	k := browseKey{admin: who.AdminID, device: deviceID, snapshot: snapshotID}
	s.browseMu.Lock()
	if s.browseSeen == nil {
		s.browseSeen = map[browseKey]*browseClaim{}
	}
	for old, c := range s.browseSeen { // keep the memory to the window: an entry past it decides nothing
		if c.written() && (now.Sub(c.at) >= BrowseAuditWindow || c.at.After(now)) {
			delete(s.browseSeen, old)
		}
	}
	if c, ok := s.browseSeen[k]; ok {
		if c.written() {
			s.browseMu.Unlock()
			return nil
		}
		c.waiters++
		s.browseMu.Unlock()
		select {
		case <-c.done:
		case <-ctx.Done():
			return ctx.Err()
		}
		if c.err != nil {
			return fmt.Errorf("%w: restore.browse", store.ErrAudit)
		}
		return nil
	}
	c := &browseClaim{at: now, done: make(chan struct{})}
	s.browseSeen[k] = c // claimed before the write, so two browses at once write one row
	s.browseMu.Unlock()

	// The claim is settled on every way out of the write, a panic included: the result is
	// recorded, a failed claim is forgotten, and done is closed, so no browse of this key waits
	// for ever and the next one tries again. A panic is recorded as a failed write, then goes on.
	var werr error
	defer func() {
		p := recover()
		if p != nil {
			werr = errBrowseWritePanicked
		}
		s.browseMu.Lock()
		c.err = werr
		if werr != nil && s.browseSeen[k] == c {
			delete(s.browseSeen, k)
		}
		close(c.done)
		s.browseMu.Unlock()
		if p != nil {
			panic(p)
		}
	}()

	method := who.Method
	if method == "" {
		method = "web"
	}
	details, _ := json.Marshal(map[string]any{"snapshot_id": snapshotID, "window_minutes": int(BrowseAuditWindow / time.Minute)})
	write := s.browseAppend
	if write == nil {
		write = s.Store.AppendAudit
	}
	_, werr = write(ctx, store.AuditEvent{ActorType: "admin", ActorID: who.AdminID, ActorLabel: who.Username, Method: method, SourceIP: who.IP,
		Action: "restore.browse", ObjectType: "device", ObjectID: deviceID, CustomerID: dev.CustomerID, Result: "ok", Details: details})
	if werr != nil {
		return fmt.Errorf("%w: restore.browse", store.ErrAudit)
	}
	return nil
}
