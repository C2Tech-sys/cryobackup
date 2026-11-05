package adminapi

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/httpx"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// Console settings (M2-W3A, architecture §5.2 "Settings, keys, system"):
// GET /api/v1/settings (admin and above) and PUT /api/v1/settings/{key}
// (owner). Only non-secret values live here. A change is validated, written
// with its audit row in one transaction, and applied to the live display
// preferences, so the next page and the next API answer use it.

// Route patterns.
const (
	PatternSettings   = "GET /api/v1/settings"
	PatternSettingPut = "PUT /api/v1/settings/{key}"
)

func settingsPatterns() []string { return []string{PatternSettings, PatternSettingPut} }

func (s *Service) registerSettings(r httpx.Registrar) {
	r.Handle(PatternSettings, s.Auth.Require(auth.RoleAdmin, auth.ModeAPI, http.HandlerFunc(s.listSettings)))
	r.Handle(PatternSettingPut, s.Auth.Require(auth.RoleOwner, auth.ModeAPI, http.HandlerFunc(s.putSetting)))
}

// SettingJSON is one setting. Value is the value in effect; Stored is false for
// a setting that has no row yet and runs on its default.
type SettingJSON struct {
	Key           string          `json:"key"`
	Label         string          `json:"label"`
	Value         json.RawMessage `json:"value"`
	Stored        bool            `json:"stored"`
	Editable      bool            `json:"editable"`
	TakesEffectIn string          `json:"takes_effect_in,omitempty"`
	UpdatedAt     *time.Time      `json:"updated_at,omitempty"`
	UpdatedBy     string          `json:"updated_by,omitempty"`
}

// SettingsResponse is GET /api/v1/settings.
type SettingsResponse struct {
	ComputedAt time.Time     `json:"computed_at"`
	Settings   []SettingJSON `json:"settings"`
}

// PutSettingRequest is the body of PUT /api/v1/settings/{key}.
type PutSettingRequest struct {
	Value json.RawMessage `json:"value"`
}

// PutSettingResponse echoes the setting after the change. Changed is false when
// the stored value already equalled the request (nothing is written or audited).
type PutSettingResponse struct {
	ComputedAt time.Time `json:"computed_at"`
	SettingJSON
	Changed bool `json:"changed"`
}

// effectiveValue is the value a setting runs on: the live display prefs for the
// two display settings (the stored row can differ from them only after a failed
// load, which the settings page reports), the stored row otherwise.
func (s *Service) effectiveValue(v store.SettingValue) (json.RawMessage, bool) {
	if s.Display != nil {
		snap := s.Display.Snapshot()
		switch v.Key {
		case store.SettingDisplayTimezone:
			b, _ := json.Marshal(snap.Zone)
			return b, v.Stored != nil
		case store.SettingRefreshInterval:
			return json.RawMessage(strconv.Itoa(snap.RefreshS)), v.Stored != nil
		}
	}
	if v.Stored == nil {
		return json.RawMessage("null"), false
	}
	return v.Stored, true
}

func (s *Service) settingJSON(v store.SettingValue) SettingJSON {
	val, stored := s.effectiveValue(v)
	out := SettingJSON{Key: v.Key, Label: v.Label, Value: val, Stored: stored, Editable: v.Editable, TakesEffectIn: v.Since}
	if v.Editable {
		out.TakesEffectIn = ""
	}
	if !v.UpdatedAt.IsZero() {
		t := v.UpdatedAt.UTC()
		out.UpdatedAt = &t
	}
	out.UpdatedBy = v.UpdatedByName
	return out
}

func (s *Service) listSettings(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	vals, err := s.Store.ListSettingValues(r.Context(), p.Scope)
	if err != nil {
		s.failM2(w, r, err)
		return
	}
	resp := SettingsResponse{ComputedAt: s.now(), Settings: make([]SettingJSON, 0, len(vals))}
	for _, v := range vals {
		resp.Settings = append(resp.Settings, s.settingJSON(v))
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}

func (s *Service) putSetting(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	var req PutSettingRequest
	if !decodeBodyMax(w, r, 4<<10, &req) {
		return
	}
	key := r.PathValue("key")
	changes, err := s.Store.SetSettingsAudited(r.Context(), p.Scope, []store.SettingInput{{Key: key, Value: req.Value}}, actorOf(r, p))
	if err != nil {
		s.failM2(w, r, err)
		return
	}
	s.committed(r)           // signal now: the read-back below can fail
	s.Display.Apply(changes) // the next request on either transport uses it
	vals, err := s.Store.ListSettingValues(r.Context(), p.Scope)
	if err != nil {
		s.failM2(w, r, err)
		return
	}
	for _, v := range vals {
		if v.Key == key {
			httpx.WriteJSON(w, http.StatusOK, PutSettingResponse{ComputedAt: s.now(), SettingJSON: s.settingJSON(v), Changed: !changes[0].Unchanged})
			return
		}
	}
	s.failM2(w, r, store.ErrNotFound)
}
