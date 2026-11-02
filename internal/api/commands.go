package api

import (
	"encoding/json"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// Command is one queued instruction for the agent (architecture §5.1
// /commands, §6.1 "command executor"). The ID is the idempotency key: the
// agent keeps a command log and does nothing twice for the same id, but still
// posts the result again. Params is a JSON object of at most MaxCommandParams
// bytes whose shape depends on Type (HoldParams, PauseParams, RestoreParams,
// UpdateAgentParams; the other types take none). Params never carry a path: a
// restore command carries only the id of a restore request, and the selection
// is fetched with GET /restores/{id} (§8.4 item 4). ExpiresAt is when the
// console stops waiting; the agent drops an expired command unexecuted.
type Command struct {
	ID        string          `json:"id"` // 26-character ULID
	Type      CommandType     `json:"type"`
	Params    json.RawMessage `json:"params,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
	ExpiresAt time.Time       `json:"expires_at"`
}

// Validate checks the command and, for the types that have parameters, the
// parameters.
func (cmd Command) Validate() error {
	var c collector
	c.ulid("id", cmd.ID)
	typeOK := has(commandTypes, cmd.Type)
	if !typeOK {
		c.add("type", msgUnknown)
	}
	if cmd.CreatedAt.IsZero() {
		c.add("created_at", msgRequired)
	}
	switch {
	case cmd.ExpiresAt.IsZero():
		c.add("expires_at", msgRequired)
	case !cmd.CreatedAt.IsZero() && !cmd.ExpiresAt.After(cmd.CreatedAt):
		c.add("expires_at", "must be after created_at")
	}
	c.rawObject("params", cmd.Params, MaxCommandParams)
	if typeOK && len(cmd.Params) > 0 && len(cmd.Params) <= MaxCommandParams && json.Valid(cmd.Params) {
		switch cmd.Type {
		case CommandHold:
			validateParams[HoldParams](&c, "params", cmd.Params)
		case CommandPause:
			validateParams[PauseParams](&c, "params", cmd.Params)
		case CommandRestore:
			validateParams[RestoreParams](&c, "params", cmd.Params)
		case CommandUpdateAgent:
			validateParams[UpdateAgentParams](&c, "params", cmd.Params)
		}
	}
	// A type whose parameters are not optional needs them even when Params is absent.
	if typeOK && len(cmd.Params) == 0 {
		switch cmd.Type {
		case CommandHold, CommandRestore, CommandUpdateAgent:
			c.add("params", msgRequired)
		}
	}
	return c.err()
}

// validateParams decodes raw into a T (unknown fields are ignored, the
// compatibility rule) and reports T's validation problems under path.
func validateParams[T interface{ Validate() error }](c *collector, path string, raw json.RawMessage) {
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		c.add(path, "does not match the command type")
		return
	}
	c.sub(path, v.Validate())
}

// HoldParams are the parameters of the hold command: delay new runs until
// Until (used by maintenance, architecture §6.1, DH11).
type HoldParams struct {
	Until time.Time `json:"until"`
}

// Validate checks the parameters.
func (p HoldParams) Validate() error {
	var c collector
	if p.Until.IsZero() {
		c.add("until", msgRequired)
	}
	return c.err()
}

// PauseParams are the parameters of the pause command: pause until Until, or
// indefinitely when Until is nil (DH11). Reason is a short operator note.
type PauseParams struct {
	Until  *time.Time `json:"until,omitempty"`
	Reason string     `json:"reason,omitempty"`
}

// Validate checks the parameters.
func (p PauseParams) Validate() error {
	var c collector
	c.text("reason", p.Reason, maxText, false)
	return c.err()
}

// RestoreParams are the parameters of the restore command: the id of the
// restore request whose selection the agent then fetches with GET /restores/{id}.
type RestoreParams struct {
	RestoreRequestID string `json:"restore_request_id"`
}

// Validate checks the parameters.
func (p RestoreParams) Validate() error {
	var c collector
	c.ulid("restore_request_id", p.RestoreRequestID)
	return c.err()
}

// UpdateAgentParams are the parameters of the update_agent command: the release
// version to move to.
type UpdateAgentParams struct {
	Version string `json:"version"`
}

// Validate checks the parameters.
func (p UpdateAgentParams) Validate() error {
	var c collector
	c.text("version", p.Version, maxText, true)
	return c.err()
}

// CommandsResponse is the 200 body of GET /commands (architecture §5.1): the
// queued commands, which the console marks delivered, or an empty list when the
// long poll (QueryWait seconds, at most CommandsLongPollMax) timed out. Nil
// Commands encodes as null; readers treat null as empty.
type CommandsResponse struct {
	Commands   []Command `json:"commands"`
	ServerTime time.Time `json:"server_time"`
}

// CommandResultRequest is the body of POST /commands/{id}/result (architecture
// §5.1, cap MaxCommandResultBody); the answer is 204 and the call is
// idempotent on the command id. ErrorClass is only for status failed. Result is
// an optional JSON object of at most MaxCommandResult bytes, path-free like
// everything else the agent sends.
type CommandResultRequest struct {
	Status     CommandStatus     `json:"status"`
	ErrorClass restic.ErrorClass `json:"error_class,omitempty"`
	Result     json.RawMessage   `json:"result,omitempty"`
}

// Validate checks the result.
func (r CommandResultRequest) Validate() error {
	var c collector
	if !has(commandStatuses, r.Status) {
		c.add("status", msgUnknown)
	}
	switch {
	case r.ErrorClass == "":
	case !r.ErrorClass.Valid():
		c.add("error_class", msgUnknown)
	case r.Status != CommandFailed:
		c.add("error_class", "is only allowed when status is failed")
	}
	c.rawObject("result", r.Result, MaxCommandResult)
	return c.err()
}
