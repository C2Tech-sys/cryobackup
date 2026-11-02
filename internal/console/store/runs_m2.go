package store

import (
	"context"
	"strings"
	"time"
)

// RunKinds and RunOutcomes are the values the run history can be filtered by.
// RunOutcomeRunning selects open runs (no outcome yet).
var (
	RunKinds    = []string{"backup", "restore", "forget", "prune", "check", "unlock", "maintenance"}
	RunOutcomes = []string{"success", "partial", "failed", "cancelled", "skipped", "deferred", "interrupted", "abandoned", RunOutcomeRunning}
)

// RunOutcomeRunning is the pseudo-outcome of an open run.
const RunOutcomeRunning = "running"

// RunFilter narrows a device's run history. Zero fields do not filter.
type RunFilter struct {
	Kind    string
	Outcome string
	// Since is inclusive and Until exclusive, both against the run's start (server time).
	Since time.Time
	Until time.Time
}

// RunPage is one numbered page of a device's run history, newest first.
type RunPage struct {
	Items []Run
	Page  PageInfo
}

// ListDeviceRuns returns one numbered page of a device's runs that match f,
// newest start first. The device must lie inside scope (ErrNotFound otherwise,
// the same answer as for a device that does not exist). Rejected filter values
// are a *ValidationError. page and size are normalised as Paginate does.
func (s *Store) ListDeviceRuns(ctx context.Context, scope Scope, deviceID string, f RunFilter, page, size int) (RunPage, error) {
	return s.listDeviceRuns(ctx, scope, deviceID, f, page, size, 0, 0)
}

// ListDeviceRunsAt is ListDeviceRuns for the rows [offset, offset+limit): the
// JSON API's cursor is a row offset.
func (s *Store) ListDeviceRunsAt(ctx context.Context, scope Scope, deviceID string, f RunFilter, offset, limit int) (RunPage, error) {
	if limit < 1 {
		return RunPage{}, validation([]FieldError{fieldf("limit", "Use a positive limit.")})
	}
	return s.listDeviceRuns(ctx, scope, deviceID, f, 0, 0, offset, limit)
}

func (s *Store) listDeviceRuns(ctx context.Context, scope Scope, deviceID string, f RunFilter, page, size, offset, limit int) (RunPage, error) {
	if err := scope.check(); err != nil {
		return RunPage{}, err
	}
	var bad []FieldError
	if f.Kind != "" && !oneOf(f.Kind, RunKinds) {
		bad = append(bad, fieldf("kind", "Use backup, restore, forget, prune, check, unlock or maintenance."))
	}
	if f.Outcome != "" && !oneOf(f.Outcome, RunOutcomes) {
		bad = append(bad, fieldf("outcome", "Unknown outcome."))
	}
	if !f.Since.IsZero() && !f.Until.IsZero() && !f.Until.After(f.Since) {
		bad = append(bad, fieldf("until", "The end of the range must be after its start."))
	}
	if err := validation(bad); err != nil {
		return RunPage{}, err
	}
	where := []string{"r.device_id = ?"}
	args := []any{deviceID}
	if f.Kind != "" {
		where, args = append(where, "r.kind = ?"), append(args, f.Kind)
	}
	switch f.Outcome {
	case "":
	case RunOutcomeRunning:
		where = append(where, "r.outcome IS NULL")
	default:
		where, args = append(where, "r.outcome = ?"), append(args, f.Outcome)
	}
	if !f.Since.IsZero() {
		where, args = append(where, "r.started_at >= ?"), append(args, msOf(f.Since))
	}
	if !f.Until.IsZero() {
		where, args = append(where, "r.started_at < ?"), append(args, msOf(f.Until))
	}
	cond := strings.Join(where, " AND ")
	var out RunPage
	err := s.Read(ctx, func(tx *Tx) error {
		if _, err := scopedDevice(tx, scope, deviceID); err != nil {
			return err
		}
		var total int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM runs r WHERE `+cond, args...).Scan(&total); err != nil {
			return err
		}
		from, to, info := windowOf(total, page, size, offset, limit)
		off, lim := from, to-from
		items, err := collect(tx, `SELECT `+qualifiedRunCols+` FROM runs r WHERE `+cond+
			` ORDER BY r.started_at DESC, r.id DESC LIMIT ? OFFSET ?`, append(append([]any(nil), args...), lim, off), scanRun)
		if err != nil {
			return err
		}
		if items == nil {
			items = []Run{}
		}
		out = RunPage{Items: items, Page: info}
		return nil
	})
	return out, err
}
