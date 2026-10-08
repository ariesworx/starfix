package store

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Cursor is an opaque position in a paged read: [Store.List],
// [Store.CommentsPage] or [Store.HistoryPage]. Pass a cursor back only to
// the kind of read that returned it.
type Cursor string

// cursorPos is a List cursor's position: the last issue's created_at and
// ID, in List's order.
type cursorPos struct {
	At int64   `json:"t"` // created_at, Unix microseconds
	ID IssueID `json:"i"`
}

// encodeCursor makes the cursor of the page after is.
func encodeCursor(is Issue) Cursor {
	b, _ := json.Marshal(cursorPos{At: is.CreatedAt.UnixMicro(), ID: is.ID}) //nolint:errchkjson // plain struct
	return Cursor(base64.RawURLEncoding.EncodeToString(b))
}

// decodeCursor reads a cursor from encodeCursor, refusing a malformed one
// with ErrInvalid.
func decodeCursor(c Cursor) (cursorPos, error) {
	var p cursorPos
	b, err := base64.RawURLEncoding.DecodeString(string(c))
	if err == nil {
		err = json.Unmarshal(b, &p)
	}
	if err == nil {
		err = p.ID.Validate()
	}
	if err != nil {
		return cursorPos{}, fmt.Errorf("%w: cursor", ErrInvalid)
	}
	return p, nil
}

// List returns issues matching f, oldest first, one page at a time, each
// from one snapshot: pass the page's Next as f.Cursor to read the one
// after it. A status, type or label that is not valid, or a malformed
// cursor, is refused with ErrInvalid.
func (s *Store) List(ctx context.Context, f Filter) (IssuePage, error) {
	limit := clampLimit(f.Limit, 50, 500)
	var where []string
	var args []any
	in := func(col string, n int, val func(int) any) {
		if n == 0 {
			return
		}
		where = append(where, col+" IN ("+placeholders(n)+")")
		for i := range n {
			args = append(args, val(i))
		}
	}
	for _, st := range f.Statuses {
		if !st.Valid() {
			return IssuePage{}, fmt.Errorf("%w: status %q", ErrInvalid, st)
		}
	}
	for _, t := range f.Types {
		if !t.Valid() {
			return IssuePage{}, fmt.Errorf("%w: type %q", ErrInvalid, t)
		}
	}
	in("i.status", len(f.Statuses), func(i int) any { return string(f.Statuses[i]) })
	in("i.type", len(f.Types), func(i int) any { return string(f.Types[i]) })
	in("i.priority", len(f.Priorities), func(i int) any { return int(f.Priorities[i]) })
	if f.Assignee != "" {
		where = append(where, "i.assignee = ?")
		args = append(args, f.Assignee)
	}
	if f.ParentID != "" {
		where = append(where, "i.parent_id = ?")
		args = append(args, string(f.ParentID))
	}
	for _, l := range f.Labels {
		if err := validLabel(l); err != nil {
			return IssuePage{}, err
		}
		where = append(where, "EXISTS (SELECT 1 FROM labels l WHERE l.issue_id = i.id AND l.label = ?)")
		args = append(args, l)
	}
	if f.Cursor != "" {
		p, err := decodeCursor(f.Cursor)
		if err != nil {
			return IssuePage{}, err
		}
		at := time.UnixMicro(p.At).UTC()
		where = append(where, "(i.created_at > ? OR (i.created_at = ? AND i.id > ?))")
		args = append(args, at, at, string(p.ID))
	}
	q := `SELECT ` + issueCols + ` FROM issues i`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ") //nolint:gosec // fixed clauses; values are placeholders
	}
	q += " ORDER BY i.created_at, i.id LIMIT ?"
	args = append(args, limit+1)

	tx, end, err := s.beginRead(ctx)
	if err != nil {
		return IssuePage{}, fmt.Errorf("list: %w", err)
	}
	defer end()
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return IssuePage{}, fmt.Errorf("list: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var page IssuePage
	for rows.Next() {
		is, err := scanIssue(rows)
		if err != nil {
			return IssuePage{}, fmt.Errorf("list: %w", err)
		}
		page.Issues = append(page.Issues, is)
	}
	if err := rows.Err(); err != nil {
		return IssuePage{}, fmt.Errorf("list: %w", err)
	}
	if len(page.Issues) > limit {
		page.Issues = page.Issues[:limit]
		page.Next = encodeCursor(page.Issues[limit-1])
	}
	return page, withLabels(ctx, tx, page.Issues)
}
