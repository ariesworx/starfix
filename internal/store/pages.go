package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"time"
)

// Paged reads (S-5). A reply is one frame of at most 4 MiB; seventy 64 KiB
// comments made an issue's comments and history unreadable for everyone,
// and broke the connection that asked. These reads return the newest page
// first and a cursor to the one before it, each page at most limit rows
// and about MaxPageBytes of text.
const (
	DefaultPage  = 100
	MaxPage      = 500
	MaxPageBytes = 1 << 20
)

// CommentPage is one page of an issue's comments, oldest first. Earlier
// is the cursor of the page before, empty on the first; Total counts the
// issue's comments.
type CommentPage struct {
	Comments []Comment
	Earlier  Cursor
	Total    int
}

// EventPage is one page of an issue's history, oldest first, as
// CommentPage.
type EventPage struct {
	Events  []Event
	Earlier Cursor
	Total   int
}

// pagePos is a paged read's position: a comment's created_at and id, or
// an event's seq.
type pagePos struct {
	At  int64  `json:"t,omitempty"`
	ID  string `json:"c,omitempty"`
	Seq int64  `json:"s,omitempty"`
}

func encodePage(p pagePos) Cursor {
	b, _ := json.Marshal(p) //nolint:errchkjson // plain struct
	return Cursor(base64.RawURLEncoding.EncodeToString(b))
}

func decodePage(c Cursor) (pagePos, error) {
	var p pagePos
	b, err := base64.RawURLEncoding.DecodeString(string(c))
	if err == nil {
		err = json.Unmarshal(b, &p)
	}
	if err != nil || len(p.ID) > 255 || p.Seq < 0 {
		return pagePos{}, fmt.Errorf("%w: cursor", ErrInvalid)
	}
	return p, nil
}

// CommentsPage returns the newest page of id's comments before the
// cursor (empty: the newest of all), at most limit (0 means DefaultPage,
// at most MaxPage).
func (s *Store) CommentsPage(ctx context.Context, id IssueID, before Cursor, limit int) (CommentPage, error) {
	if err := id.Validate(); err != nil {
		return CommentPage{}, err
	}
	limit = clampLimit(limit, DefaultPage, MaxPage)
	where, args := `WHERE issue_id = ?`, []any{string(id)}
	if before != "" {
		p, err := decodePage(before)
		if err != nil {
			return CommentPage{}, err
		}
		at := time.UnixMicro(p.At).UTC()
		where += ` AND (created_at < ? OR (created_at = ? AND id < ?))`
		args = append(args, at, at, p.ID)
	}
	tx, err := s.r.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return CommentPage{}, fmt.Errorf("comments: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // read-only: nothing to keep
	var page CommentPage
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM comments WHERE issue_id = ?`, string(id)).Scan(&page.Total); err != nil {
		return CommentPage{}, fmt.Errorf("comments: %w", err)
	}
	cs, err := queryComments(ctx, tx, where+` ORDER BY created_at DESC, id DESC LIMIT ?`, append(args, limit+1)...)
	if err != nil {
		return CommentPage{}, err
	}
	n, more := fit(len(cs), limit, func(i int) int { return len(cs[i].Body) })
	cs = cs[:n]
	if more {
		last := cs[n-1]
		page.Earlier = encodePage(pagePos{At: last.CreatedAt.UnixMicro(), ID: last.ID})
	}
	slices.Reverse(cs)
	page.Comments = cs
	return page, nil
}

// HistoryPage returns the newest page of the events recorded against id
// before the cursor, as CommentsPage.
func (s *Store) HistoryPage(ctx context.Context, id IssueID, before Cursor, limit int) (EventPage, error) {
	if err := id.Validate(); err != nil {
		return EventPage{}, err
	}
	limit = clampLimit(limit, DefaultPage, MaxPage)
	where, args := `WHERE target = ?`, []any{string(id)}
	if before != "" {
		p, err := decodePage(before)
		if err != nil {
			return EventPage{}, err
		}
		where += ` AND seq < ?`
		args = append(args, p.Seq)
	}
	tx, err := s.r.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return EventPage{}, fmt.Errorf("events: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // read-only: nothing to keep
	var page EventPage
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE target = ?`, string(id)).Scan(&page.Total); err != nil {
		return EventPage{}, fmt.Errorf("events: %w", err)
	}
	evs, err := queryEvents(ctx, tx, where+` ORDER BY seq DESC LIMIT ?`, append(args, limit+1)...)
	if err != nil {
		return EventPage{}, err
	}
	n, more := fit(len(evs), limit, func(i int) int { return len(evs[i].Before) + len(evs[i].After) })
	evs = evs[:n]
	if more {
		page.Earlier = encodePage(pagePos{Seq: evs[n-1].Seq})
	}
	slices.Reverse(evs)
	page.Events = evs
	return page, nil
}

// fit returns how many of n rows (read limit+1 deep) make a page: at most
// limit, and no more than MaxPageBytes by size, though always one; and
// whether rows are left over.
func fit(n, limit int, size func(int) int) (int, bool) {
	total := 0
	for i := range min(n, limit) {
		total += size(i)
		if i > 0 && total > MaxPageBytes {
			return i, true
		}
	}
	return min(n, limit), n > limit
}

// WhoPage is Who capped at limit agents (0 means DefaultPage, at most
// MaxPage), with how many more were seen.
func (s *Store) WhoPage(ctx context.Context, since time.Duration, limit int) ([]Agent, int, error) {
	as, err := s.Who(ctx, since)
	if err != nil {
		return nil, 0, err
	}
	limit = clampLimit(limit, DefaultPage, MaxPage)
	if len(as) <= limit {
		return as, 0, nil
	}
	return as[:limit], len(as) - limit, nil
}
