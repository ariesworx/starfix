package store

import (
	"context"
	"database/sql"
	"fmt"
)

// History returns every event recorded against an issue, oldest first.
func (s *Store) History(ctx context.Context, id IssueID) ([]Event, error) {
	if err := id.Validate(); err != nil {
		return nil, err
	}
	return s.events(ctx, `WHERE target = ? ORDER BY seq`, string(id))
}

// Events returns up to limit events with seq greater than after, in order.
// Sequence numbers are gapless, so a reader can tell it missed nothing.
func (s *Store) Events(ctx context.Context, after int64, limit int) ([]Event, error) {
	return s.events(ctx, `WHERE seq > ? ORDER BY seq LIMIT ?`, after, clampLimit(limit, 100, 1000))
}

const eventSelect = `SELECT seq, at, principal, session, machine, op, target,
  before_state, after_state, idem_key FROM events `

func (s *Store) events(ctx context.Context, where string, args ...any) ([]Event, error) {
	q := eventSelect + where //nolint:gosec // where is a constant from the callers above
	rows, err := s.r.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("events: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Event
	for rows.Next() {
		var e Event
		var before, after []byte
		var idem sql.NullString
		if err := rows.Scan(&e.Seq, &e.At, &e.Actor.Principal, &e.Actor.Session, &e.Actor.Machine,
			&e.Op, &e.Target, &before, &after, &idem); err != nil {
			return nil, fmt.Errorf("events: %w", err)
		}
		if len(before) > 0 {
			e.Before = before
		}
		if len(after) > 0 {
			e.After = after
		}
		e.IdemKey = idem.String
		e.At = e.At.UTC()
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("events: %w", err)
	}
	return out, nil
}
