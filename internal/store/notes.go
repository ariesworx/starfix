package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// AddLabel adds a label to an issue. Adding a present label is a no-op. A
// label that is not valid, or past the Labels limit, is refused with
// ErrInvalid, and a missing issue with ErrNotFound.
func (s *Store) AddLabel(ctx context.Context, actor Actor, id IssueID, label string) error {
	if err := labelArgs(id, label); err != nil {
		return err
	}
	return s.write(ctx, actor, func(w *wtx) error {
		if err := mustExist(ctx, w.tx, id); err != nil {
			return err
		}
		var has, total int
		if err := w.tx.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(label = ?), 0) FROM labels WHERE issue_id = ?`,
			label, string(id)).Scan(&total, &has); err != nil {
			return fmt.Errorf("count labels: %w", err)
		}
		if has == 0 && total >= w.lim.Labels {
			return fmt.Errorf("%w: issue %s has %d labels; an issue has at most %d labels", ErrInvalid, id, total, w.lim.Labels)
		}
		n, err := w.exec(ctx, `INSERT IGNORE INTO labels (issue_id, label, created_at) VALUES (?, ?, ?)`,
			string(id), label, w.now)
		if err != nil {
			return fmt.Errorf("insert label: %w", err)
		}
		if n == 0 {
			return nil
		}
		return w.event(ctx, OpLabelAdd, string(id), nil, map[string]string{"label": label})
	})
}

// RemoveLabel removes a label from an issue. Removing an absent label is a
// no-op.
func (s *Store) RemoveLabel(ctx context.Context, actor Actor, id IssueID, label string) error {
	if err := labelArgs(id, label); err != nil {
		return err
	}
	return s.write(ctx, actor, func(w *wtx) error {
		n, err := w.exec(ctx, `DELETE FROM labels WHERE issue_id = ? AND label = ?`, string(id), label)
		if err != nil {
			return fmt.Errorf("delete label: %w", err)
		}
		if n == 0 {
			return nil
		}
		return w.event(ctx, OpLabelRemove, string(id), map[string]string{"label": label}, nil)
	})
}

// labelArgs validates the issue id and label of AddLabel and RemoveLabel.
func labelArgs(id IssueID, label string) error {
	if err := id.Validate(); err != nil {
		return err
	}
	return validLabel(label)
}

// AddComment appends a comment, authored by the actor, and tells the
// principals it mentions (@name). With an idempotency key (idem), a repeat
// returns the first comment and writes nothing, and the key reused for
// another request is refused with an [*IdemError]. An empty or invalid
// body is refused with ErrInvalid, and a missing issue with ErrNotFound.
// Comments need no hold: anyone may comment on any issue.
func (s *Store) AddComment(ctx context.Context, actor Actor, id IssueID, body, idem string) (Comment, error) {
	if err := id.Validate(); err != nil {
		return Comment{}, err
	}
	if err := validBody(body); err != nil {
		return Comment{}, err
	}
	if err := validIdem(idem); err != nil {
		return Comment{}, err
	}
	var c Comment
	err := s.write(ctx, actor, func(w *wtx) error {
		if done, err := replay(ctx, w, idem, "comment", struct {
			ID   IssueID
			Body string
		}{id, body}, &c); done || err != nil {
			return err
		}
		if err := mustExist(ctx, w.tx, id); err != nil {
			return err
		}
		var err error
		if c, err = insertComment(ctx, w, id, body, CommentPlain); err != nil {
			return err
		}
		if err := w.settle(c); err != nil {
			return err
		}
		return w.notifyMentions(ctx, id, body)
	})
	if err != nil {
		return Comment{}, err
	}
	return c, nil
}

// validBody checks a comment or handoff note: required, and at most
// maxText bytes of safe text, newlines and tabs allowed.
func validBody(body string) error {
	return checkText("comment", body, maxText, true)
}

// insertComment appends a comment of the given kind, authored by w's actor,
// and records the event. The issue must exist.
func insertComment(ctx context.Context, w *wtx, id IssueID, body string, kind CommentKind) (Comment, error) {
	return insertNote(ctx, w, id, body, kind, HandoffFields{})
}

// insertNote is insertComment for a note that may carry handoff fields:
// they are stored in handoffs and recorded in the comment's event.
func insertNote(ctx context.Context, w *wtx, id IssueID, body string, kind CommentKind, f HandoffFields) (Comment, error) {
	cid := newCommentID()
	// Notes are ordered by created_at, so keep it strictly increasing per
	// issue: two notes in the same microsecond (or under a test clock) would
	// otherwise tie, and the random id would decide which is the latest.
	at := w.now
	var last sql.NullTime
	if err := w.tx.QueryRowContext(ctx, `SELECT MAX(created_at) FROM comments WHERE issue_id = ?`, string(id)).Scan(&last); err != nil {
		return Comment{}, fmt.Errorf("latest comment: %w", err)
	}
	if last.Valid && !at.After(last.Time) {
		at = last.Time.Add(time.Microsecond)
	}
	c := Comment{ID: cid, Issue: id, Author: w.actor.Principal, Session: w.actor.Session, Kind: kind, Body: body, CreatedAt: at}
	if _, err := w.exec(ctx, `INSERT INTO comments (id, issue_id, author, session, kind, body, created_at)
  VALUES (?, ?, ?, ?, ?, ?, ?)`, c.ID, string(id), c.Author, c.Session, string(c.Kind), c.Body, c.CreatedAt); err != nil {
		return Comment{}, fmt.Errorf("insert comment: %w", err)
	}
	var after any = c
	if !f.empty() {
		if _, err := w.exec(ctx, `INSERT INTO handoffs (comment_id, issue_id, state, next_step, branch, worktree, to_principal)
  VALUES (?, ?, ?, ?, ?, ?, ?)`, c.ID, string(id), string(f.State), f.Next, f.Branch, f.Worktree, f.To); err != nil {
			return Comment{}, fmt.Errorf("insert handoff: %w", err)
		}
		after = struct {
			Comment
			Handoff HandoffFields `json:"handoff"`
		}{c, f}
	}
	return c, w.event(ctx, OpCommentAdd, string(id), nil, after)
}

// Comments returns an issue's comments, oldest first.
func (s *Store) Comments(ctx context.Context, id IssueID) ([]Comment, error) {
	if err := id.Validate(); err != nil {
		return nil, err
	}
	return s.comments(ctx, `WHERE issue_id = ? ORDER BY created_at, id`, string(id))
}

// AllComments returns every comment, by issue, then oldest first.
func (s *Store) AllComments(ctx context.Context) ([]Comment, error) {
	return s.comments(ctx, `ORDER BY issue_id, created_at, id`)
}

func (s *Store) comments(ctx context.Context, rest string, args ...any) ([]Comment, error) {
	return queryComments(ctx, s.r, rest, args...)
}

// queryComments reads the comments that rest, a constant WHERE or ORDER
// BY clause with placeholders for args, selects.
func queryComments(ctx context.Context, qr querier, rest string, args ...any) ([]Comment, error) {
	q := `SELECT id, issue_id, author, session, kind, body, created_at FROM comments ` + rest //nolint:gosec // rest is a constant from the callers
	rows, err := qr.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("comments: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Comment
	for rows.Next() {
		var c Comment
		if err := rows.Scan(&c.ID, &c.Issue, &c.Author, &c.Session, &c.Kind, &c.Body, &c.CreatedAt); err != nil {
			return nil, fmt.Errorf("comments: %w", err)
		}
		c.CreatedAt = c.CreatedAt.UTC()
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("comments: %w", err)
	}
	return out, nil
}
