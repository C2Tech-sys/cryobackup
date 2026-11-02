package api

import (
	"encoding/json"
	"time"
)

// ReleaseResponse is the 200 body of GET /release (architecture §5.1, DH14,
// §6.1 "updater"); 204 means nothing newer for the device's ring. Manifest is
// the signed manifest exactly as the console holds it (raw JSON, so the
// signature stays verifiable) and Signature signs those bytes; the agent
// verifies both before it stages anything.
//
// The artifact names, sizes and SHA-256 hashes live inside the signed manifest
// and the agent takes them from nowhere else: this response has no unsigned
// artifact list, so a console that is not the signer cannot steer a download
// (architecture addendum §13.3 "Wire protocol"). Version, Channel and
// ResticVersion are conveniences for logging and the "newer?" decision; the
// agent trusts the manifest's own values once the signature verifies. Fetch a
// listed file with RouteReleaseArtifact(version, name).
type ReleaseResponse struct {
	Version       string          `json:"version"`
	Channel       Channel         `json:"channel"`
	ResticVersion string          `json:"restic_version"`
	Manifest      json.RawMessage `json:"manifest"`
	Signature     string          `json:"signature"`
}

// TokenRotateResponse is the 200 body of POST /token/rotate (architecture §5.1,
// DH4): the new device token, delivered once. The old token stays valid until
// GraceUntil so a lost response cannot lock the agent out. The request has no body.
type TokenRotateResponse struct {
	DeviceToken Secret    `json:"device_token"`
	GraceUntil  time.Time `json:"grace_until"`
	ServerTime  time.Time `json:"server_time"`
}

// Redacted returns a copy of r with the token blanked.
func (r TokenRotateResponse) Redacted() TokenRotateResponse {
	r.DeviceToken = ""
	return r
}
