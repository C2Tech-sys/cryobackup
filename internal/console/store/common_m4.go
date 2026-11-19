package store

import "encoding/json"

// jsonBytes marshals the small, fixed-shape maps the M4 audit rows carry. A value that cannot be
// marshalled (none of ours) yields an empty object rather than failing the audited change.
func jsonBytes(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}
