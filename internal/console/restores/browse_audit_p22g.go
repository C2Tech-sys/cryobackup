package restores

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// The browse audit (ADR 0027 ruling C5; ticket P22-G): looking into a device's snapshot is
// audited as restore.browse once per admin, device and snapshot per BrowseAuditWindow. The row
// names the device and the snapshot id, never a folder: what was opened inside the snapshot stays
// out of the audit trail exactly as it stays out of every log.
//
// The once-per-window memory is in this process. A console restart forgets it, so the first
// browse after a restart is audited again: the trail can hold an extra row, never a missing one.

// BrowseAuditWindow is how long one restore.browse row covers an admin's browsing of one
// snapshot of one device.
const BrowseAuditWindow = time.Hour

type browseKey struct {
	st                      *store.Store // one memory per console database (tests run many side by side)
	admin, device, snapshot string
}

var (
	browseMu   sync.Mutex
	browseSeen = map[browseKey]time.Time{}
)

// AuditBrowse records that who is browsing snapshotID of deviceID, unless a restore.browse row
// for the same admin, device and snapshot was written within BrowseAuditWindow. It refuses a
// device outside scope (store.ErrNotFound) and a malformed snapshot id (ErrInvalid). When the
// audit row cannot be written it returns an error wrapping store.ErrAudit and remembers nothing,
// so the caller refuses the listing ("a failed audit refuses") and the next browse tries again.
func (s *Service) AuditBrowse(ctx context.Context, who Actor, scope store.Scope, deviceID, snapshotID string) error {
	if !snapshotIDRE.MatchString(snapshotID) || who.AdminID == "" {
		return ErrInvalid
	}
	dev, err := s.Store.GetDevice(ctx, scope, deviceID)
	if err != nil {
		return err
	}
	now := s.now()
	k := browseKey{st: s.Store, admin: who.AdminID, device: deviceID, snapshot: snapshotID}
	browseMu.Lock()
	for old, at := range browseSeen { // keep the memory to the window: an entry past it decides nothing
		if now.Sub(at) >= BrowseAuditWindow || at.After(now) {
			delete(browseSeen, old)
		}
	}
	if _, seen := browseSeen[k]; seen {
		browseMu.Unlock()
		return nil
	}
	browseSeen[k] = now // claimed before the write, so two browses at once write one row
	browseMu.Unlock()

	method := who.Method
	if method == "" {
		method = "web"
	}
	details, _ := json.Marshal(map[string]any{"snapshot_id": snapshotID, "window_minutes": int(BrowseAuditWindow / time.Minute)})
	if _, err := s.Store.AppendAudit(ctx, store.AuditEvent{ActorType: "admin", ActorID: who.AdminID, ActorLabel: who.Username, Method: method, SourceIP: who.IP,
		Action: "restore.browse", ObjectType: "device", ObjectID: deviceID, CustomerID: dev.CustomerID, Result: "ok", Details: details}); err != nil {
		browseMu.Lock()
		if at, ok := browseSeen[k]; ok && at.Equal(now) {
			delete(browseSeen, k)
		}
		browseMu.Unlock()
		return fmt.Errorf("%w: restore.browse", store.ErrAudit)
	}
	return nil
}
