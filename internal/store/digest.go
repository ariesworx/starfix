package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// Digest limits.
const (
	// StalledAfter is how long an in-progress issue may go without an
	// event (an edit, comment, handoff or edge) before Digest calls it
	// stalled.
	StalledAfter = 48 * time.Hour
	// MaxDigestWindow bounds how far back a digest reads.
	MaxDigestWindow = 366 * 24 * time.Hour
	// DigestItems is how many issues each digest section lists; its total
	// counts them all.
	DigestItems = 10
	// DigestCreated is how many created issues are listed, highest
	// priority first.
	DigestCreated = 5
	// digestNote is how much of a handoff note a digest quotes.
	digestNote = 200
	// digestRows bounds the rows one section reads; past it the section's
	// total is a lower bound and Digest.Capped is set.
	digestRows = 10000
)

// DigestFilter selects what Digest summarizes.
type DigestFilter struct {
	// Since starts the window. If zero, the window is Window back from the
	// server's now, and Window must be positive.
	Since  time.Time
	Window time.Duration
	// By keeps only what this principal did: the events they made, and the
	// issues assigned to them for in progress, stalled and blocked.
	By string
	// Label keeps only issues with this label (for discovered work, on
	// either the new issue or the one it came from).
	Label string
}

// DigestItem is one issue in a digest section. What By and At mean depends
// on the section; see Digest.
type DigestItem struct {
	ID       IssueID
	Title    string
	Priority Priority
	By       string
	At       time.Time
	// Note is the start of the latest handoff note (handed off only).
	Note string
	// From is the issue the work was discovered from (discovered only).
	From IssueID
	// BlockedBy lists the unclosed blockers (blocked only).
	BlockedBy []IssueID
}

// DigestSection is a capped list and the count of everything it matched.
type DigestSection struct {
	Total int
	Items []DigestItem
}

// Digest is a deterministic summary of a time window and of the work in
// flight at its end. Event sections list each issue once, newest event
// first, with By the principal who made the event and At its time.
type Digest struct {
	// Since and Now bound the window, on the server's clock.
	Since, Now time.Time
	// Events counts the events in the window that match the filter.
	Events int
	// Closed lists issues closed in the window and still closed.
	Closed DigestSection
	// Started lists issues set in_progress in the window.
	Started DigestSection
	// InProgress lists issues in progress now; By is the assignee, and At
	// when the issue was last set in_progress. Highest priority first,
	// then longest running.
	InProgress DigestSection
	// Stalled lists issues in progress with no event for StalledAfter; At
	// is the last event. Longest idle first.
	Stalled DigestSection
	// Blocked lists unclosed issues held back by unclosed blockers now; By
	// is the assignee, and At is zero.
	Blocked DigestSection
	// HandedOff lists issues with a handoff note in the window, quoting
	// the latest.
	HandedOff DigestSection
	// Created lists issues created in the window, highest priority first.
	Created DigestSection
	// Discovered lists issues linked discovered-from in the window; From
	// is the issue each came from.
	Discovered DigestSection
	// Usage totals the time held and the tokens reported in the window
	// (design §12.1), filtered like the sections.
	Usage DigestUsage
	// Capped reports that a section had more rows than a digest reads,
	// so its total is a lower bound.
	Capped bool
}

// Digest summarizes the events since f's window start and the issues in
// flight now, in one read-only snapshot. A zero Since with a Window that
// is not positive, a start in the future or more than MaxDigestWindow
// back, a By that is not one line of safe text, and a Label that is not
// valid are refused with ErrInvalid.
func (s *Store) Digest(ctx context.Context, f DigestFilter) (Digest, error) {
	now := s.now()
	d := Digest{Now: now, Since: f.Since.UTC()}
	if f.Since.IsZero() {
		if f.Window <= 0 {
			return Digest{}, fmt.Errorf("%w: digest window must be positive", ErrInvalid)
		}
		d.Since = now.Add(-f.Window)
	}
	switch {
	case d.Since.After(now):
		return Digest{}, fmt.Errorf("%w: since %s is in the future", ErrInvalid, d.Since.Format(time.RFC3339))
	case now.Sub(d.Since) > MaxDigestWindow:
		return Digest{}, fmt.Errorf("%w: since reaches back more than %d days", ErrInvalid, MaxDigestWindow/(24*time.Hour))
	}
	if err := checkLine("by", f.By, maxName, false); err != nil {
		return Digest{}, err
	}
	if f.Label != "" {
		if err := validLabel(f.Label); err != nil {
			return Digest{}, err
		}
	}
	f.Since = d.Since

	tx, err := s.r.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Digest{}, fmt.Errorf("digest: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // read-only: nothing to keep
	q := &digestQuery{tx: tx, f: f, now: now}
	if err := q.run(ctx, &d); err != nil {
		return Digest{}, fmt.Errorf("digest: %w", err)
	}
	return d, nil
}

// digestQuery runs a digest's queries in one read-only transaction, as
// of now. capped is set once a section reads more than digestRows rows.
type digestQuery struct {
	tx     *sql.Tx
	f      DigestFilter
	now    time.Time
	capped bool
}

// JSON fields of an event's after_state that digest sections test.
const (
	afterStatus = `JSON_UNQUOTE(JSON_EXTRACT(e.after_state, '$.status'))`
	afterKind   = `JSON_UNQUOTE(JSON_EXTRACT(e.after_state, '$.kind'))`
	afterType   = `JSON_UNQUOTE(JSON_EXTRACT(e.after_state, '$.type'))`
	afterTo     = `JSON_UNQUOTE(JSON_EXTRACT(e.after_state, '$.to'))`
	// hasLabel tests that the issue whose id the %s column holds has the
	// filter's label.
	hasLabel = `EXISTS (SELECT 1 FROM labels l WHERE l.issue_id = %s AND l.label = ?)`
)

// run fills in d: Events, every section and Capped.
func (q *digestQuery) run(ctx context.Context, d *Digest) error {
	if err := q.events(ctx, d); err != nil {
		return err
	}
	type section struct {
		out  *DigestSection
		op   Op
		cond string
		max  int
	}
	for _, sc := range []section{
		{&d.Closed, OpIssueClose, `AND i.status = 'closed'`, DigestItems},
		{&d.Started, OpIssueUpdate, `AND ` + afterStatus + ` = 'in_progress'`, DigestItems},
		{&d.HandedOff, OpCommentAdd, `AND ` + afterKind + ` = 'handoff'`, DigestItems},
		{&d.Created, OpIssueCreate, ``, DigestCreated},
		{&d.Discovered, OpDepAdd, `AND ` + afterType + ` = 'discovered-from'`, DigestItems},
	} {
		items, err := q.windowed(ctx, sc.op, sc.cond)
		if err != nil {
			return fmt.Errorf("%s: %w", sc.op, err)
		}
		if sc.op == OpIssueCreate {
			slices.SortStableFunc(items, func(a, b DigestItem) int { return int(a.Priority - b.Priority) })
		}
		*sc.out = DigestSection{Total: len(items), Items: items[:min(len(items), sc.max)]}
	}
	if err := q.inProgress(ctx, d); err != nil {
		return err
	}
	if err := q.blocked(ctx, d); err != nil {
		return err
	}
	if err := q.usage(ctx, d); err != nil {
		return err
	}
	d.Capped = q.capped
	return nil
}

// filters returns the by and label conditions and their arguments. by
// names the column holding the principal; labelOn the issue id columns,
// any of which may carry the label.
func (q *digestQuery) filters(by string, labelOn ...string) (string, []any) {
	var b strings.Builder
	var args []any
	if q.f.By != "" {
		b.WriteString(" AND " + by + " = ?")
		args = append(args, q.f.By)
	}
	if q.f.Label != "" && len(labelOn) > 0 {
		alts := make([]string, len(labelOn))
		for i, col := range labelOn {
			alts[i] = fmt.Sprintf(hasLabel, col)
			args = append(args, q.f.Label)
		}
		b.WriteString(" AND (" + strings.Join(alts, " OR ") + ")")
	}
	return b.String(), args
}

// events counts the events in the window that match the filter.
func (q *digestQuery) events(ctx context.Context, d *Digest) error {
	cond, args := q.filters("e.principal", "e.target")
	err := q.tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM events e WHERE e.at >= ?`+cond, //nolint:gosec // constant clauses; values are placeholders
		append([]any{q.f.Since}, args...)...).Scan(&d.Events)
	if err != nil {
		return fmt.Errorf("events: %w", err)
	}
	return nil
}

// windowed lists the issues with an op event in the window, once each,
// newest event first. cond is a constant condition on e and i.
func (q *digestQuery) windowed(ctx context.Context, op Op, cond string) ([]DigestItem, error) {
	labelOn := []string{"i.id"}
	if op == OpDepAdd {
		labelOn = append(labelOn, afterTo)
	}
	fcond, fargs := q.filters("e.principal", labelOn...)
	const head = `SELECT i.id, i.title, i.priority, e.principal, e.at, e.after_state
FROM events e JOIN issues i ON i.id = e.target
WHERE e.at >= ? AND e.op = ? `
	query := head + cond + fcond + " ORDER BY e.seq DESC LIMIT ?" //nolint:gosec // constant clauses; values are placeholders
	args := append([]any{q.f.Since, string(op)}, fargs...)
	rows, err := q.tx.QueryContext(ctx, query, append(args, digestRows+1)...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []DigestItem
	seen := map[IssueID]bool{}
	n := 0
	for rows.Next() {
		var it DigestItem
		var after []byte
		if err := rows.Scan(&it.ID, &it.Title, &it.Priority, &it.By, &it.At, &after); err != nil {
			return nil, err
		}
		if n++; n > digestRows {
			q.capped = true
			break
		}
		if seen[it.ID] {
			continue
		}
		seen[it.ID] = true
		it.At = it.At.UTC()
		switch op {
		case OpCommentAdd:
			var c Comment
			if err := json.Unmarshal(after, &c); err != nil {
				return nil, fmt.Errorf("handoff on %s: %w", it.ID, err)
			}
			it.Note = clip(c.Body, digestNote)
		case OpDepAdd:
			var dep Dep
			if err := json.Unmarshal(after, &dep); err != nil {
				return nil, fmt.Errorf("edge from %s: %w", it.ID, err)
			}
			it.From = dep.To
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// inProgress lists the issues in progress now, when each was taken and
// when it last had an event, and from that the stalled ones.
func (q *digestQuery) inProgress(ctx context.Context, d *Digest) error {
	fcond, fargs := q.filters("i.assignee", "i.id")
	const head = `SELECT i.id, i.title, i.priority, COALESCE(i.assignee, ''), i.updated_at,
  MAX(e.at), MAX(CASE WHEN ` + afterStatus + ` = 'in_progress' THEN e.at END)
FROM issues i LEFT JOIN events e ON e.target = i.id
WHERE i.status = 'in_progress'`
	query := head + fcond + " GROUP BY i.id, i.title, i.priority, i.assignee, i.updated_at ORDER BY i.id LIMIT ?" //nolint:gosec // constant clauses; values are placeholders
	rows, err := q.tx.QueryContext(ctx, query, append(fargs, digestRows+1)...)
	if err != nil {
		return fmt.Errorf("in progress: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var taken, idle []DigestItem
	for rows.Next() {
		var it DigestItem
		var updated time.Time
		var last, start sql.NullTime
		if err := rows.Scan(&it.ID, &it.Title, &it.Priority, &it.By, &updated, &last, &start); err != nil {
			return fmt.Errorf("in progress: %w", err)
		}
		if len(taken) == digestRows {
			q.capped = true
			break
		}
		it.At = orTime(start, updated)
		taken = append(taken, it)
		if l := orTime(last, updated); q.now.Sub(l) > StalledAfter {
			it.At = l
			idle = append(idle, it)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("in progress: %w", err)
	}
	slices.SortFunc(taken, func(a, b DigestItem) int {
		if a.Priority != b.Priority {
			return int(a.Priority - b.Priority)
		}
		if c := a.At.Compare(b.At); c != 0 {
			return c
		}
		return strings.Compare(string(a.ID), string(b.ID))
	})
	slices.SortFunc(idle, func(a, b DigestItem) int {
		if c := a.At.Compare(b.At); c != 0 {
			return c
		}
		return strings.Compare(string(a.ID), string(b.ID))
	})
	d.InProgress = DigestSection{Total: len(taken), Items: taken[:min(len(taken), DigestItems)]}
	d.Stalled = DigestSection{Total: len(idle), Items: idle[:min(len(idle), DigestItems)]}
	return nil
}

// blocked lists unclosed issues held back by unclosed blockers, as
// Blocked does.
func (q *digestQuery) blocked(ctx context.Context, d *Digest) error {
	fcond, fargs := q.filters("i.assignee", "i.id")
	rows, err := q.tx.QueryContext(ctx, blockedCTE+`SELECT i.id, i.title, i.priority, COALESCE(i.assignee, ''), b.via
FROM issues i JOIN blocked b ON b.id = i.id
WHERE i.status <> 'closed'`+fcond+`
ORDER BY i.priority, i.created_at, i.id, (b.via <> i.id)
LIMIT ?`, append(append([]any{q.now}, fargs...), digestRows+1)...) //nolint:gosec // constant clauses; values are placeholders
	if err != nil {
		return fmt.Errorf("blocked: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var items []DigestItem
	var vias []IssueID
	seen := map[IssueID]bool{}
	n := 0
	for rows.Next() {
		var it DigestItem
		var via IssueID
		if err := rows.Scan(&it.ID, &it.Title, &it.Priority, &it.By, &via); err != nil {
			return fmt.Errorf("blocked: %w", err)
		}
		if n++; n > digestRows {
			q.capped = true
			break
		}
		if seen[it.ID] {
			continue
		}
		seen[it.ID] = true
		items = append(items, it)
		vias = append(vias, via)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("blocked: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("blocked: %w", err)
	}
	by, err := blockerMap(ctx, q.tx)
	if err != nil {
		return err
	}
	d.Blocked = DigestSection{Total: len(items), Items: items[:min(len(items), DigestItems)]}
	for i := range d.Blocked.Items {
		d.Blocked.Items[i].BlockedBy = by[vias[i]]
	}
	return nil
}

// orTime returns t, or def when t is NULL, in UTC.
func orTime(t sql.NullTime, def time.Time) time.Time {
	if t.Valid {
		return t.Time.UTC()
	}
	return def.UTC()
}

// clip cuts s to at most n bytes on a rune boundary, adding "…".
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}
