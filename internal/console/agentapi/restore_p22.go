package agentapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/console/escrow"
	"github.com/c2tech-sys/cryobackup/internal/console/httpx"
	"github.com/c2tech-sys/cryobackup/internal/console/restores"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
	"github.com/c2tech-sys/cryobackup/internal/ulid"
)

// restoreFetchPerMinute bounds the selection fetches of one device: the agent
// fetches once per restore and retries a failed fetch with back-off, so a
// device asking more often than this is looping, and each fetch is an audited
// write. Not an operator setting.
const restoreFetchPerMinute = 6

// handleRestoreFetch is GET /api/v1/agent/restores/{id} (ADR 0027): the
// selection of a device restore, for the device whose token asks, and nothing
// else. The answer is the api.RestoreSelection, unwrapped from the master key
// and validated with the wire's own validator (restores.FetchForDevice). It is
// the only response of the agent API that carries paths, so it is never logged,
// never cached (Cache-Control: no-store) and travels only on this
// device-authenticated route.
//
// Refusals, each by its own code: an id that is not a restore of this device
// (another device's, a download, a finished one, an unknown or malformed id)
// 404 not_found, exactly alike; a request past its expiry 410 restore_expired;
// a retired device 409 device_retired_download_only; a device being purged 409
// device_purging; a customer-held key 409 custody_customer_held; no master key
// 503 master_key_unavailable. A fetch whose audit row cannot be written is 500
// and hands out nothing.
func (s *Service) handleRestoreFetch(w http.ResponseWriter, r *http.Request, d device) {
	if ok, wait := s.restoreFetch.Allow(d.ID); s.limited(w, r, ok, wait) {
		return
	}
	id := r.PathValue(api.PathValueID)
	if len(id) != ulid.Len || !ulid.Valid(id) {
		httpx.Problem(w, r, api.CodeNotFound, 0) // answered like a missing restore: nothing to learn from the shape
		return
	}
	var mk *escrow.MasterKey
	if s.Keys != nil {
		mk = s.Keys.Master
	}
	sel, err := restores.FetchForDevice(r.Context(), s.Store, mk, d.ID, id, httpx.ClientIP(r))
	switch {
	case err == nil:
	case errors.Is(err, store.ErrNotFound):
		httpx.Problem(w, r, api.CodeNotFound, 0)
		return
	case errors.Is(err, restores.ErrRestoreExpired):
		httpx.Problem(w, r, api.CodeRestoreExpired, 0)
		return
	case errors.Is(err, restores.ErrRetiredDownloadOnly):
		httpx.Problem(w, r, api.CodeDeviceRetiredDownloadOnly, 0)
		return
	case errors.Is(err, restores.ErrDevicePurging):
		httpx.Problem(w, r, api.CodeDevicePurging, 0)
		return
	case errors.Is(err, restores.ErrCustomerHeld):
		httpx.Problem(w, r, api.CodeCustodyCustomerHeld, 0)
		return
	case errors.Is(err, restores.ErrMasterKey):
		s.Log.Warn("restore fetch: master key unavailable", logx.ID("device_id", d.ID), logx.ID("restore_id", id), logx.Class("master_key"))
		httpx.Problem(w, r, api.CodeMasterKeyUnavailable, 30*time.Second)
		return
	case errors.Is(err, store.ErrAudit):
		s.Log.Error("restore fetch: refused, not audited", logx.ID("device_id", d.ID), logx.ID("restore_id", id), logx.Class("audit"))
		httpx.Problem(w, r, api.CodeInternal, 0)
		return
	case errors.Is(err, restores.ErrSelectionUnreadable):
		s.Log.Error("restore fetch: the stored selection cannot be read", logx.ID("device_id", d.ID), logx.ID("restore_id", id), logx.Class("selection_unreadable"))
		httpx.Problem(w, r, api.CodeInternal, 0)
		return
	default:
		s.Log.Error("restore fetch: failed", logx.ID("device_id", d.ID), logx.ID("restore_id", id), logx.Class("internal"))
		httpx.Problem(w, r, api.CodeInternal, 0)
		return
	}
	s.Log.Info("restore fetch: selection handed to the device", logx.ID("device_id", d.ID), logx.ID("restore_id", id), logx.Int("paths", len(sel.Paths)))
	s.notifyChanged(d.ID)
	httpx.WriteJSON(w, http.StatusOK, sel) // sets Cache-Control: no-store
}
