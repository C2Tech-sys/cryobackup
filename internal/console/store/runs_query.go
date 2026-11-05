package store

import (
	"context"
	"strings"
	"time"
)

// Fleet-wide run search (M3-T3, architecture §5.2 GET /runs, screen S21). The
// clause builder of the job-logs page: every clause narrows (AND), and several
// values of one clause widen it (OR). Every row is read through the caller's
// Scope: a run of a device outside it is neither listed nor counted, and a
// customer or device filter that names something outside the scope matches
// nothing (it does not say that the thing exists).

// RunSearch is the filter of SearchRunsAt. Zero fields do not filter.
type RunSearch struct {
	CustomerIDs []string
	DeviceIDs   []string
	Kinds       []string // RunKinds
	Outcomes    []string // RunOutcomes, including RunOutcomeRunning for open runs
	// Since is inclusive and Until exclusive, both against the run's start (server time).
	Since time.Time
	Until time.Time
}

// RunRow is a run with the customer and device it belongs to.
type RunRow struct {
	Run
	CustomerID   string
	CustomerName string
	Hostname     string
}

// RunSearchPage is one window of a run search, newest first.
type RunSearchPage struct {
	Items []RunRow
	Page  PageInfo
	// Of is the number of runs in scope before any filter, so "0 of N" is expressible.
	Of int
}

const maxRunSearchValues = 50

func (f RunSearch) validate() []FieldError {
	var bad []FieldError
	for _, c := range []struct {
		field string
		vals  []string
		set   []string
		msg   string
	}{
		{"kind", f.Kinds, RunKinds, "Use backup, restore, forget, prune, check, unlock or maintenance."},
		{"status", f.Outcomes, RunOutcomes, "Unknown status."},
	} {
		if len(c.vals) > maxRunSearchValues {
			bad = append(bad, fieldf(c.field, "Use at most %d values.", maxRunSearchValues))
			continue
		}
		for _, v := range c.vals {
			if !oneOf(v, c.set) {
				bad = append(bad, fieldf(c.field, "%s", c.msg))
				break
			}
		}
	}
	for _, c := range []struct {
		field string
		vals  []string
	}{{"customer", f.CustomerIDs}, {"device", f.DeviceIDs}} {
		if len(c.vals) > maxRunSearchValues {
			bad = append(bad, fieldf(c.field, "Use at most %d values.", maxRunSearchValues))
			continue
		}
		for _, v := range c.vals {
			if len(v) > 40 || strings.ContainsAny(v, "\r\n\x00") {
				bad = append(bad, fieldf(c.field, "Choose one of the listed values."))
				break
			}
		}
	}
	if !f.Since.IsZero() && !f.Until.IsZero() && !f.Until.After(f.Since) {
		bad = append(bad, fieldf("until", "The end of the range must be after its start."))
	}
	return bad
}

// inClause renders "col IN (?,...)" for vals, or "" when there are none.
func inClause(col string, vals []string, where *[]string, args *[]any) {
	if len(vals) == 0 {
		return
	}
	*where = append(*where, col+" IN ("+placeholders(len(vals))+")")
	for _, v := range vals {
		*args = append(*args, v)
	}
}

// runRowScanner appends the joined columns to a run row scan.
type runRowScanner struct {
	sc    scanner
	extra []any
}

func (r runRowScanner) Scan(dest ...any) error { return r.sc.Scan(append(dest, r.extra...)...) }

const runRowJoin = ` FROM runs r JOIN devices d ON d.id = r.device_id JOIN customers c ON c.id = d.customer_id`

var runRowCols = qualifiedRunCols + `, d.customer_id, c.name, d.hostname`

func scanRunRow(sc scanner) (RunRow, error) {
	var row RunRow
	run, err := scanRun(runRowScanner{sc: sc, extra: []any{&row.CustomerID, &row.CustomerName, &row.Hostname}})
	row.Run = run
	return row, err
}

// SearchRunsAt returns the rows [offset, offset+limit) of the runs matching f
// inside scope, newest start first. A rejected filter is a *ValidationError.
func (s *Store) SearchRunsAt(ctx context.Context, scope Scope, f RunSearch, offset, limit int) (RunSearchPage, error) {
	if err := scope.check(); err != nil {
		return RunSearchPage{}, err
	}
	if limit < 1 {
		return RunSearchPage{}, validation([]FieldError{fieldf("limit", "Use a positive limit.")})
	}
	if err := validation(f.validate()); err != nil {
		return RunSearchPage{}, err
	}
	clause, sargs := scope.clause("d.customer_id")
	where, args := []string{clause}, append([]any(nil), sargs...)
	scopeOnly, scopeArgs := strings.Join(where, " AND "), append([]any(nil), args...)
	inClause("d.customer_id", f.CustomerIDs, &where, &args)
	inClause("r.device_id", f.DeviceIDs, &where, &args)
	inClause("r.kind", f.Kinds, &where, &args)
	if len(f.Outcomes) > 0 {
		var alts []string
		var named []string
		for _, o := range f.Outcomes {
			if o == RunOutcomeRunning {
				alts = append(alts, "r.outcome IS NULL")
			} else {
				named = append(named, o)
			}
		}
		if len(named) > 0 {
			alts = append(alts, "r.outcome IN ("+placeholders(len(named))+")")
			for _, o := range named {
				args = append(args, o)
			}
		}
		where = append(where, "("+strings.Join(alts, " OR ")+")")
	}
	if !f.Since.IsZero() {
		where, args = append(where, "r.started_at >= ?"), append(args, msOf(f.Since))
	}
	if !f.Until.IsZero() {
		where, args = append(where, "r.started_at < ?"), append(args, msOf(f.Until))
	}
	cond := strings.Join(where, " AND ")
	var out RunSearchPage
	err := s.Read(ctx, func(tx *Tx) error {
		var total int
		if err := tx.QueryRow(`SELECT COUNT(*)`+runRowJoin+` WHERE `+cond, args...).Scan(&total); err != nil {
			return err
		}
		if err := tx.QueryRow(`SELECT COUNT(*)`+runRowJoin+` WHERE `+scopeOnly, scopeArgs...).Scan(&out.Of); err != nil {
			return err
		}
		from, to, info := windowOf(total, 0, 0, offset, limit)
		items, err := collect(tx, `SELECT `+runRowCols+runRowJoin+` WHERE `+cond+
			` ORDER BY r.started_at DESC, r.id DESC LIMIT ? OFFSET ?`, append(append([]any(nil), args...), to-from, from), scanRunRow)
		if err != nil {
			return err
		}
		if items == nil {
			items = []RunRow{}
		}
		out.Items, out.Page = items, info
		return nil
	})
	return out, err
}

// GetRunRow returns one run with its customer and device, or ErrNotFound for a
// run that does not exist or whose device lies outside scope.
func (s *Store) GetRunRow(ctx context.Context, scope Scope, id string) (RunRow, error) {
	if err := scope.check(); err != nil {
		return RunRow{}, err
	}
	clause, args := scope.clause("d.customer_id")
	var out RunRow
	err := s.Read(ctx, func(tx *Tx) error {
		var err error
		out, err = one(tx, `SELECT `+runRowCols+runRowJoin+` WHERE r.id = ? AND `+clause, append([]any{id}, args...), scanRunRow)
		return err
	})
	return out, err
}
