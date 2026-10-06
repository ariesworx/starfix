package store

import (
	"context"
	"fmt"
)

// AddLabel adds a label to an issue. Adding a present label is a no-op.
func (s *Store) AddLabel(ctx context.Context, actor Actor, id IssueID, label string) error {
	if err := labelArgs(id, label); err != nil {
		return err
	}
	return s.write(ctx, actor, func(w *wtx) error {
		if err := mustExist(ctx, w.tx, id); err != nil {
			return err
		}
		n, err := w.exec(ctx, `INSERT IGNORE INTO labels (issue_id, label, created_at) VALUES (?, ?, ?)`,
			string(id), label, w.now)
		if err != nil {
			return fmt.Errorf("insert label: %w", err)
		}
		if n == 0 {
			return nil
		}
		return w.event(ctx, OpLabelAdd, string(id), nil, map[string]string{"label": label}, "")
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
		return w.event(ctx, OpLabelRemove, string(id), map[string]string{"label": label}, nil, "")
	})
}

func labelArgs(id IssueID, label string) error {
	if err := id.Validate(); err != nil {
		return err
	}
	return validLabel(label)
}

// AddComment appends a comment, authored by the actor.
func (s *Store) AddComment(ctx context.Context, actor Actor, id IssueID, body string) (Comment, error) {
	if err := id.Validate(); err != nil {
		return Comment{}, err
	}
	if body == "" || len(body) > 65535 {
		return Comment{}, fmt.Errorf("%w: comment must be 1-65535 bytes", ErrInvalid)
	}
	var c Comment
	err := s.write(ctx, actor, func(w *wtx) error {
		if err := mustExist(ctx, w.tx, id); err != nil {
			return err
		}
		cid, err := newCommentID()
		if err != nil {
			return err
		}
		c = Comment{ID: cid, Issue: id, Author: w.actor.Principal, Session: w.actor.Session, Body: body, CreatedAt: w.now}
		if _, err := w.exec(ctx, `INSERT INTO comments (id, issue_id, author, session, body, created_at)
  VALUES (?, ?, ?, ?, ?, ?)`, c.ID, string(id), c.Author, c.Session, c.Body, c.CreatedAt); err != nil {
			return fmt.Errorf("insert comment: %w", err)
		}
		return w.event(ctx, OpCommentAdd, string(id), nil, c, "")
	})
	if err != nil {
		return Comment{}, err
	}
	return c, nil
}

// Comments returns an issue's comments, oldest first.
func (s *Store) Comments(ctx context.Context, id IssueID) ([]Comment, error) {
	if err := id.Validate(); err != nil {
		return nil, err
	}
	rows, err := s.r.QueryContext(ctx, `SELECT id, issue_id, author, session, body, created_at
  FROM comments WHERE issue_id = ? ORDER BY created_at, id`, string(id))
	if err != nil {
		return nil, fmt.Errorf("comments: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Comment
	for rows.Next() {
		var c Comment
		if err := rows.Scan(&c.ID, &c.Issue, &c.Author, &c.Session, &c.Body, &c.CreatedAt); err != nil {
			return nil, fmt.Errorf("comments: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("comments: %w", err)
	}
	return out, nil
}
