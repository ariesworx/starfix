package store

import (
	"context"
	"encoding/json"
	"fmt"
)

// AddDep records that from depends on to. Adding an existing edge is a
// no-op. Blocking edges (and parent links) may not form a cycle.
func (s *Store) AddDep(ctx context.Context, actor Actor, from, to IssueID, typ DepType) error {
	if err := validEdge(from, to, typ); err != nil {
		return err
	}
	return s.write(ctx, actor, func(w *wtx) error {
		for _, id := range []IssueID{from, to} {
			if err := mustExist(ctx, w.tx, id); err != nil {
				return err
			}
		}
		return insertDep(ctx, w, from, to, typ)
	})
}

// insertDep adds the edge from→to, recording an event, unless it is
// already there. Both issues must exist.
func insertDep(ctx context.Context, w *wtx, from, to IssueID, typ DepType) error {
	var n int
	if err := w.tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM deps WHERE from_id = ? AND to_id = ? AND type = ?`,
		string(from), string(to), string(typ)).Scan(&n); err != nil {
		return fmt.Errorf("lookup dep: %w", err)
	}
	if n > 0 {
		return nil
	}
	if typ.Blocking() {
		if err := checkEdge(ctx, w.tx, from, to); err != nil {
			return err
		}
	}
	wid, err := randomInt63()
	if err != nil {
		return err
	}
	if _, err := w.exec(ctx, `INSERT INTO deps (from_id, to_id, type, created_by, created_at, rev, write_id)
  VALUES (?, ?, ?, ?, ?, 1, ?)`, string(from), string(to), string(typ), w.actor.Principal, w.now, wid); err != nil {
		return fmt.Errorf("insert dep: %w", err)
	}
	d := Dep{From: from, To: to, Type: typ, CreatedBy: w.actor.Principal, CreatedAt: w.now}
	return w.event(ctx, OpDepAdd, string(from), nil, d, "")
}

// RemoveDep deletes an edge. Removing a missing edge is a no-op.
func (s *Store) RemoveDep(ctx context.Context, actor Actor, from, to IssueID, typ DepType) error {
	if err := validEdge(from, to, typ); err != nil {
		return err
	}
	return s.write(ctx, actor, func(w *wtx) error {
		n, err := w.exec(ctx, `DELETE FROM deps WHERE from_id = ? AND to_id = ? AND type = ?`,
			string(from), string(to), string(typ))
		if err != nil {
			return fmt.Errorf("delete dep: %w", err)
		}
		if n == 0 {
			return nil
		}
		return w.event(ctx, OpDepRemove, string(from), Dep{From: from, To: to, Type: typ}, nil, "")
	})
}

func validEdge(from, to IssueID, typ DepType) error {
	if err := from.Validate(); err != nil {
		return err
	}
	if err := to.Validate(); err != nil {
		return err
	}
	if !typ.Valid() {
		return fmt.Errorf("%w: dependency type %q", ErrInvalid, typ)
	}
	if from == to && !typ.Blocking() {
		return fmt.Errorf("%w: issue %s cannot depend on itself", ErrInvalid, from)
	}
	return nil
}

// checkEdge refuses a new waits-on edge from→to (a blocking dep, or a
// parent link) when to already reaches from.
func checkEdge(ctx context.Context, q querier, from, to IssueID) error {
	if from == to {
		return fmt.Errorf("%s depends on itself: %w", from, ErrCycle)
	}
	var n int
	err := q.QueryRowContext(ctx, `WITH RECURSIVE
edges (src, dst) AS (
  SELECT from_id, to_id FROM deps WHERE type IN ('blocks','conditional-blocks','waits-for')
  UNION ALL
  SELECT id, parent_id FROM issues WHERE parent_id IS NOT NULL
),
reach (id) AS (
  SELECT CAST(? AS CHAR(64))
  UNION
  SELECT e.dst FROM edges e JOIN reach r ON e.src = r.id
)
SELECT COUNT(*) FROM reach WHERE id = ?`, string(to), string(from)).Scan(&n)
	if err != nil {
		return fmt.Errorf("cycle check: %w", err)
	}
	if n > 0 {
		return fmt.Errorf("%s already depends on %s: %w", to, from, ErrCycle)
	}
	return nil
}

// Deps returns the edges into and out of an issue.
func (s *Store) Deps(ctx context.Context, id IssueID) ([]Dep, error) {
	if err := id.Validate(); err != nil {
		return nil, err
	}
	return s.deps(ctx, `WHERE from_id = ? OR to_id = ?`, string(id), string(id))
}

// AllDeps returns every edge, ordered by from, to and type.
func (s *Store) AllDeps(ctx context.Context) ([]Dep, error) {
	return s.deps(ctx, ``)
}

func (s *Store) deps(ctx context.Context, where string, args ...any) ([]Dep, error) {
	q := `SELECT from_id, to_id, type, created_by, created_at, metadata FROM deps ` + where + //nolint:gosec // where is a constant from the callers above
		` ORDER BY from_id, to_id, type`
	rows, err := s.r.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("deps: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Dep
	for rows.Next() {
		var d Dep
		var meta []byte
		if err := rows.Scan(&d.From, &d.To, &d.Type, &d.CreatedBy, &d.CreatedAt, &meta); err != nil {
			return nil, fmt.Errorf("deps: %w", err)
		}
		d.CreatedAt = d.CreatedAt.UTC()
		if len(meta) > 0 {
			d.Metadata = json.RawMessage(meta)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("deps: %w", err)
	}
	return out, nil
}

// blockedCTE yields blocked(id, via): open issues with an unclosed
// blocking target, and their descendants, with via naming the ancestor whose
// blockers apply. deferred(id) is the same for issues deferred by status or
// by a future defer_until (parameter 1: now).
const blockedCTE = `WITH RECURSIVE
direct (id) AS (
  SELECT DISTINCT d.from_id FROM deps d
  JOIN issues f ON f.id = d.from_id
  JOIN issues t ON t.id = d.to_id
  WHERE d.type IN (` + readyBlocking + `) AND f.status <> 'closed' AND t.status <> 'closed'
),
blocked (id, via) AS (
  SELECT id, id FROM direct
  UNION
  SELECT c.id, b.via FROM issues c JOIN blocked b ON c.parent_id = b.id
),
deferred (id) AS (
  SELECT id FROM issues WHERE status = 'deferred' OR defer_until > ?
  UNION
  SELECT c.id FROM issues c JOIN deferred d ON c.parent_id = d.id
)
`

// readyWhere selects and orders the ready issues of blockedCTE, best
// first; its one parameter is the limit. Ready and StartIssue share it, so
// start takes what ready shows first.
const readyWhere = `WHERE i.status = 'open' AND i.template = FALSE
  AND i.id NOT IN (SELECT id FROM blocked)
  AND i.id NOT IN (SELECT id FROM deferred)
ORDER BY i.priority, i.created_at, i.id
LIMIT ?`

func clampLimit(n, def, maxN int) int {
	if n <= 0 {
		return def
	}
	return min(n, maxN)
}

// Ready returns open issues that nothing holds back: no unclosed blocks or
// conditional-blocks target on the issue or an ancestor, not deferred (by
// status or a future defer_until) on the issue or an ancestor, and not a
// template. Ordered by priority, then age. Computed at read time.
func (s *Store) Ready(ctx context.Context, limit int) ([]Issue, error) {
	limit = clampLimit(limit, 10, 500)
	rows, err := s.r.QueryContext(ctx, blockedCTE+`SELECT `+issueCols+` FROM issues i
`+readyWhere, s.now(), limit)
	if err != nil {
		return nil, fmt.Errorf("ready: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Issue
	for rows.Next() {
		is, err := scanIssue(rows)
		if err != nil {
			return nil, fmt.Errorf("ready: %w", err)
		}
		out = append(out, is)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ready: %w", err)
	}
	return out, withLabels(ctx, s.r, out)
}

// Blocked returns unclosed issues held back by unclosed blockers, their own
// or an ancestor's, ordered by priority, then age.
func (s *Store) Blocked(ctx context.Context, limit int) ([]BlockedIssue, error) {
	limit = clampLimit(limit, 50, 500)
	rows, err := s.r.QueryContext(ctx, blockedCTE+`SELECT `+issueCols+`, b.via FROM issues i
JOIN blocked b ON b.id = i.id
WHERE i.status <> 'closed'
ORDER BY i.priority, i.created_at, i.id, (b.via <> i.id)`, s.now())
	if err != nil {
		return nil, fmt.Errorf("blocked: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []BlockedIssue
	seen := map[IssueID]bool{}
	for rows.Next() {
		var via IssueID
		is, err := scanIssue(rows, &via)
		if err != nil {
			return nil, fmt.Errorf("blocked: %w", err)
		}
		if seen[is.ID] || len(out) >= limit {
			continue
		}
		seen[is.ID] = true
		b := BlockedIssue{Issue: is}
		if via != is.ID {
			b.Via = via
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("blocked: %w", err)
	}
	if err := s.attachBlockers(ctx, out); err != nil {
		return nil, err
	}
	issues := make([]*Issue, len(out))
	for i := range out {
		issues[i] = &out[i].Issue
	}
	return out, attachLabels(ctx, s.r, issues)
}

func (s *Store) attachBlockers(ctx context.Context, bs []BlockedIssue) error {
	by, err := blockerMap(ctx, s.r)
	if err != nil {
		return err
	}
	for i := range bs {
		src := bs[i].Issue.ID
		if bs[i].Via != "" {
			src = bs[i].Via
		}
		bs[i].BlockedBy = by[src]
	}
	return nil
}

// blockerMap maps each issue to its open blocking targets, in id order.
func blockerMap(ctx context.Context, q querier) (map[IssueID][]IssueID, error) {
	rows, err := q.QueryContext(ctx, `SELECT d.from_id, d.to_id FROM deps d
JOIN issues t ON t.id = d.to_id
WHERE d.type IN (`+readyBlocking+`) AND t.status <> 'closed'
ORDER BY d.from_id, d.to_id`)
	if err != nil {
		return nil, fmt.Errorf("blockers: %w", err)
	}
	defer func() { _ = rows.Close() }()
	by := map[IssueID][]IssueID{}
	for rows.Next() {
		var f, t IssueID
		if err := rows.Scan(&f, &t); err != nil {
			return nil, fmt.Errorf("blockers: %w", err)
		}
		by[f] = append(by[f], t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("blockers: %w", err)
	}
	return by, nil
}

func withLabels(ctx context.Context, q querier, issues []Issue) error {
	ps := make([]*Issue, len(issues))
	for i := range issues {
		ps[i] = &issues[i]
	}
	return attachLabels(ctx, q, ps)
}
