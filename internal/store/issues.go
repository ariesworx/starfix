package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"
)

const issueCols = `i.id, i.parent_id, i.title, i.body, i.design, i.acceptance, i.notes,
  i.status, i.priority, i.type, i.assignee, i.owner, i.due_at, i.defer_until,
  i.ephemeral, i.expires_at, i.pinned, i.template, i.metadata, i.close_reason,
  i.created_by, i.created_at, i.updated_at, i.closed_at, i.rev`

type scanner interface{ Scan(dest ...any) error }

func scanIssue(sc scanner, extra ...any) (Issue, error) {
	var (
		is                               Issue
		parent, assignee, owner, reason  sql.NullString
		due, deferUntil, expires, closed sql.NullTime
		meta                             []byte
	)
	dest := []any{&is.ID, &parent, &is.Title, &is.Body, &is.Design, &is.Acceptance, &is.Notes,
		&is.Status, &is.Priority, &is.Type, &assignee, &owner, &due, &deferUntil,
		&is.Ephemeral, &expires, &is.Pinned, &is.Template, &meta, &reason,
		&is.CreatedBy, &is.CreatedAt, &is.UpdatedAt, &closed, &is.Rev}
	if err := sc.Scan(append(dest, extra...)...); err != nil {
		return Issue{}, err
	}
	is.ParentID = IssueID(parent.String)
	is.Assignee = assignee.String
	is.Owner = owner.String
	is.CloseReason = reason.String
	is.DueAt = timePtr(due)
	is.DeferUntil = timePtr(deferUntil)
	is.ExpiresAt = timePtr(expires)
	is.ClosedAt = timePtr(closed)
	if len(meta) > 0 {
		is.Metadata = json.RawMessage(meta)
	}
	return is, nil
}

func timePtr(t sql.NullTime) *time.Time {
	if !t.Valid {
		return nil
	}
	u := t.Time.UTC()
	return &u
}

// nullStr maps "" to NULL.
func nullStr[T ~string](s T) any {
	if s == "" {
		return nil
	}
	return string(s)
}

// nullTime maps nil and the zero time to NULL.
func nullTime(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return t.UTC().Truncate(time.Microsecond)
}

func nullJSON(m json.RawMessage) (any, error) {
	if len(m) == 0 || string(m) == "null" {
		return nil, nil
	}
	if !json.Valid(m) {
		return nil, fmt.Errorf("%w: metadata is not valid JSON", ErrInvalid)
	}
	return string(m), nil
}

// loadIssue reads one issue with its labels.
func loadIssue(ctx context.Context, q querier, id IssueID) (Issue, error) {
	is, err := scanIssue(q.QueryRowContext(ctx, `SELECT `+issueCols+` FROM issues i WHERE i.id = ?`, string(id)))
	if errors.Is(err, sql.ErrNoRows) {
		return Issue{}, fmt.Errorf("issue %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return Issue{}, fmt.Errorf("get issue %s: %w", id, err)
	}
	if err := attachLabels(ctx, q, []*Issue{&is}); err != nil {
		return Issue{}, err
	}
	return is, nil
}

// attachLabels fills Labels for each issue, sorted.
func attachLabels(ctx context.Context, q querier, issues []*Issue) error {
	if len(issues) == 0 {
		return nil
	}
	byID := make(map[IssueID]*Issue, len(issues))
	args := make([]any, len(issues))
	for i, is := range issues {
		byID[is.ID] = is
		args[i] = string(is.ID)
	}
	rows, err := q.QueryContext(ctx, `SELECT issue_id, label FROM labels WHERE issue_id IN (`+
		placeholders(len(args))+`) ORDER BY issue_id, label`, args...)
	if err != nil {
		return fmt.Errorf("labels: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id IssueID
		var l string
		if err := rows.Scan(&id, &l); err != nil {
			return fmt.Errorf("labels: %w", err)
		}
		if is := byID[id]; is != nil {
			is.Labels = append(is.Labels, l)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("labels: %w", err)
	}
	return nil
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func exists(ctx context.Context, q querier, id IssueID) (bool, error) {
	var n int
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM issues WHERE id = ?`, string(id)).Scan(&n); err != nil {
		return false, fmt.Errorf("lookup %s: %w", id, err)
	}
	return n > 0, nil
}

func mustExist(ctx context.Context, q querier, id IssueID) error {
	ok, err := exists(ctx, q, id)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("issue %s: %w", id, ErrNotFound)
	}
	return nil
}

// GetIssue returns one issue with its labels.
func (s *Store) GetIssue(ctx context.Context, id IssueID) (Issue, error) {
	if err := id.Validate(); err != nil {
		return Issue{}, err
	}
	return loadIssue(ctx, s.r, id)
}

func (n *NewIssue) normalize() error {
	if strings.TrimSpace(n.Title) == "" || len(n.Title) > 500 {
		return fmt.Errorf("%w: title must be 1-500 characters", ErrInvalid)
	}
	if n.Status == "" {
		n.Status = StatusOpen
	}
	if !n.Status.Valid() || n.Status == StatusClosed {
		return fmt.Errorf("%w: status %q", ErrInvalid, n.Status)
	}
	if n.Priority == nil {
		p := P2
		n.Priority = &p
	}
	if !n.Priority.Valid() {
		return fmt.Errorf("%w: priority %d", ErrInvalid, *n.Priority)
	}
	if n.Type == "" {
		n.Type = TypeTask
	}
	if !n.Type.Valid() {
		return fmt.Errorf("%w: type %q", ErrInvalid, n.Type)
	}
	if len(n.IdempotencyKey) > 128 {
		return fmt.Errorf("%w: idempotency key longer than 128", ErrInvalid)
	}
	for _, l := range n.Labels {
		if err := validLabel(l); err != nil {
			return err
		}
	}
	if n.ParentID != "" {
		if err := n.ParentID.Validate(); err != nil {
			return err
		}
	}
	if n.ID != "" {
		return n.ID.Validate()
	}
	return nil
}

// CreateIssue creates an issue and returns it. With an IdempotencyKey, a
// repeat of the same create returns the issue the first one made and writes
// nothing.
func (s *Store) CreateIssue(ctx context.Context, actor Actor, in NewIssue) (Issue, error) {
	if err := in.normalize(); err != nil {
		return Issue{}, err
	}
	meta, err := nullJSON(in.Metadata)
	if err != nil {
		return Issue{}, err
	}
	id := in.ID
	if id == "" {
		if id, err = NewID(s.opts.Prefix); err != nil {
			return Issue{}, err
		}
	}
	var out Issue
	err = s.write(ctx, actor, func(w *wtx) error {
		if in.IdempotencyKey != "" {
			var op Op
			var target string
			err := w.tx.QueryRowContext(ctx, `SELECT op, target FROM events WHERE idem_key = ?`, in.IdempotencyKey).Scan(&op, &target)
			switch {
			case err == nil:
				if op != OpIssueCreate || (in.ID != "" && IssueID(target) != in.ID) {
					return fmt.Errorf("%w: idempotency key already used by %s on %s", ErrInvalid, op, target)
				}
				out, err = loadIssue(ctx, w.tx, IssueID(target))
				return err
			case !errors.Is(err, sql.ErrNoRows):
				return fmt.Errorf("idempotency lookup: %w", err)
			}
		}
		if ok, err := exists(ctx, w.tx, id); err != nil {
			return err
		} else if ok {
			return fmt.Errorf("issue %s: %w", id, ErrExists)
		}
		if in.ParentID != "" {
			if err := mustExist(ctx, w.tx, in.ParentID); err != nil {
				return fmt.Errorf("parent: %w", err)
			}
		}
		wid, err := randomInt63()
		if err != nil {
			return err
		}
		if _, err := w.exec(ctx, `INSERT INTO issues (id, parent_id, title, body, design, acceptance, notes,
  status, priority, type, assignee, owner, due_at, defer_until, ephemeral, expires_at, pinned, template,
  metadata, created_by, created_at, updated_at, rev, write_id)
  VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?)`,
			string(id), nullStr(in.ParentID), in.Title, in.Body, in.Design, in.Acceptance, in.Notes,
			string(in.Status), int(*in.Priority), string(in.Type), nullStr(in.Assignee), nullStr(in.Owner),
			nullTime(in.DueAt), nullTime(in.DeferUntil), in.Ephemeral, nullTime(in.ExpiresAt), in.Pinned, in.Template,
			meta, w.actor.Principal, w.now, w.now, wid); err != nil {
			if isDuplicate(err) {
				return fmt.Errorf("issue %s: %w", id, ErrExists)
			}
			return fmt.Errorf("insert issue: %w", err)
		}
		for _, l := range in.Labels {
			if _, err := w.exec(ctx, `INSERT IGNORE INTO labels (issue_id, label, created_at) VALUES (?, ?, ?)`,
				string(id), l, w.now); err != nil {
				return fmt.Errorf("insert label: %w", err)
			}
		}
		if out, err = loadIssue(ctx, w.tx, id); err != nil {
			return err
		}
		return w.event(ctx, OpIssueCreate, string(id), nil, out, in.IdempotencyKey)
	})
	if err != nil {
		return Issue{}, err
	}
	return out, nil
}

// UpdateIssue applies patch if the issue is still at rev expected, and
// returns the new state. A stale rev, or a concurrent write that keeps
// winning, returns ErrConflict.
func (s *Store) UpdateIssue(ctx context.Context, actor Actor, id IssueID, expected Rev, patch IssuePatch) (Issue, error) {
	if err := id.Validate(); err != nil {
		return Issue{}, err
	}
	if expected < 1 {
		return Issue{}, fmt.Errorf("%w: expected rev is required", ErrInvalid)
	}
	sets, args, err := patch.columns()
	if err != nil {
		return Issue{}, err
	}
	var out Issue
	err = s.write(ctx, actor, func(w *wtx) error {
		before, err := loadIssue(ctx, w.tx, id)
		if err != nil {
			return err
		}
		if before.Rev != expected {
			return fmt.Errorf("issue %s at rev %d, not %d: %w", id, before.Rev, expected, ErrConflict)
		}
		if patch.Status != nil && before.Status == StatusClosed {
			return fmt.Errorf("%w: issue %s is closed; reopen it first", ErrInvalid, id)
		}
		if patch.ParentID != nil && *patch.ParentID != "" {
			if err := checkEdge(ctx, w.tx, id, *patch.ParentID); err != nil {
				return err
			}
		}
		if len(sets) == 0 {
			out = before
			return nil
		}
		out, err = casUpdate(ctx, w, before, sets, args)
		if err != nil {
			return err
		}
		b, a := diff(before, out)
		return w.event(ctx, OpIssueUpdate, string(id), b, a, "")
	})
	if err != nil {
		return Issue{}, err
	}
	return out, nil
}

// casUpdateSQL is the one UPDATE of issues. {sets} comes from a fixed
// column list, never from input.
const casUpdateSQL = `UPDATE issues SET {sets}, updated_at = ?, rev = rev + 1, write_id = ? WHERE id = ? AND rev = ?`

// casUpdate writes sets to the issue at before.Rev, stamping rev and
// write_id, and returns the new state.
func casUpdate(ctx context.Context, w *wtx, before Issue, sets []string, args []any) (Issue, error) {
	wid, err := randomInt63()
	if err != nil {
		return Issue{}, err
	}
	q := strings.Replace(casUpdateSQL, "{sets}", strings.Join(sets, ", "), 1)
	all := append(append([]any{}, args...), w.now, wid, string(before.ID), int64(before.Rev))
	n, err := w.exec(ctx, q, all...)
	if err != nil {
		return Issue{}, fmt.Errorf("update issue %s: %w", before.ID, err)
	}
	if n != 1 {
		return Issue{}, fmt.Errorf("issue %s changed during update: %w", before.ID, ErrConflict)
	}
	return loadIssue(ctx, w.tx, before.ID)
}

// columns turns the patch into SET clauses over a fixed column list.
func (p IssuePatch) columns() ([]string, []any, error) {
	var sets []string
	var args []any
	add := func(col string, v any) {
		sets = append(sets, col+" = ?")
		args = append(args, v)
	}
	if p.Title != nil {
		if strings.TrimSpace(*p.Title) == "" || len(*p.Title) > 500 {
			return nil, nil, fmt.Errorf("%w: title must be 1-500 characters", ErrInvalid)
		}
		add("title", *p.Title)
	}
	for _, f := range []struct {
		col string
		v   *string
	}{{"body", p.Body}, {"design", p.Design}, {"acceptance", p.Acceptance}, {"notes", p.Notes}} {
		if f.v != nil {
			add(f.col, *f.v)
		}
	}
	if p.Status != nil {
		if !p.Status.Valid() || *p.Status == StatusClosed {
			return nil, nil, fmt.Errorf("%w: status %q (close with CloseIssue)", ErrInvalid, *p.Status)
		}
		add("status", string(*p.Status))
	}
	if p.Priority != nil {
		if !p.Priority.Valid() {
			return nil, nil, fmt.Errorf("%w: priority %d", ErrInvalid, *p.Priority)
		}
		add("priority", int(*p.Priority))
	}
	if p.Type != nil {
		if !p.Type.Valid() {
			return nil, nil, fmt.Errorf("%w: type %q", ErrInvalid, *p.Type)
		}
		add("type", string(*p.Type))
	}
	if p.ParentID != nil {
		if *p.ParentID != "" {
			if err := p.ParentID.Validate(); err != nil {
				return nil, nil, err
			}
		}
		add("parent_id", nullStr(*p.ParentID))
	}
	if p.Assignee != nil {
		add("assignee", nullStr(*p.Assignee))
	}
	if p.Owner != nil {
		add("owner", nullStr(*p.Owner))
	}
	for _, f := range []struct {
		col string
		v   *time.Time
	}{{"due_at", p.DueAt}, {"defer_until", p.DeferUntil}, {"expires_at", p.ExpiresAt}} {
		if f.v != nil {
			add(f.col, nullTime(f.v))
		}
	}
	for _, f := range []struct {
		col string
		v   *bool
	}{{"ephemeral", p.Ephemeral}, {"pinned", p.Pinned}, {"template", p.Template}} {
		if f.v != nil {
			add(f.col, *f.v)
		}
	}
	if p.Metadata != nil {
		m, err := nullJSON(p.Metadata)
		if err != nil {
			return nil, nil, err
		}
		add("metadata", m)
	}
	return sets, args, nil
}

// CloseIssue closes an issue. expected 0 skips the revision check: close
// wins over concurrent edits (design §8).
func (s *Store) CloseIssue(ctx context.Context, actor Actor, id IssueID, expected Rev, reason string) (Issue, error) {
	if len(reason) > 2000 {
		return Issue{}, fmt.Errorf("%w: reason longer than 2000", ErrInvalid)
	}
	return s.setClosed(ctx, actor, id, expected, true, reason)
}

// ReopenIssue reopens a closed issue. expected 0 skips the revision check.
func (s *Store) ReopenIssue(ctx context.Context, actor Actor, id IssueID, expected Rev) (Issue, error) {
	return s.setClosed(ctx, actor, id, expected, false, "")
}

func (s *Store) setClosed(ctx context.Context, actor Actor, id IssueID, expected Rev, closing bool, reason string) (Issue, error) {
	if err := id.Validate(); err != nil {
		return Issue{}, err
	}
	if expected < 0 {
		return Issue{}, fmt.Errorf("%w: negative rev", ErrInvalid)
	}
	var out Issue
	err := s.write(ctx, actor, func(w *wtx) error {
		before, err := loadIssue(ctx, w.tx, id)
		if err != nil {
			return err
		}
		if expected != 0 && before.Rev != expected {
			return fmt.Errorf("issue %s at rev %d, not %d: %w", id, before.Rev, expected, ErrConflict)
		}
		op := OpIssueClose
		sets := []string{"status = ?", "closed_at = ?", "close_reason = ?"}
		args := []any{string(StatusClosed), w.now, nullStr(reason)}
		if closing && before.Status == StatusClosed {
			return fmt.Errorf("%w: issue %s is already closed", ErrInvalid, id)
		}
		if !closing {
			if before.Status != StatusClosed {
				return fmt.Errorf("%w: issue %s is not closed", ErrInvalid, id)
			}
			op = OpIssueReopen
			args = []any{string(StatusOpen), nil, nil}
		}
		if out, err = casUpdate(ctx, w, before, sets, args); err != nil {
			return err
		}
		b, a := diff(before, out)
		return w.event(ctx, op, string(id), b, a, "")
	})
	if err != nil {
		return Issue{}, err
	}
	return out, nil
}

// diff returns the changed fields of an issue, old and new. Bookkeeping
// fields (rev, updated_at, labels) are left out.
func diff(a, b Issue) (map[string]any, map[string]any) {
	ma, mb := fieldMap(a), fieldMap(b)
	before, after := map[string]any{}, map[string]any{}
	for k := range mb {
		if _, ok := ma[k]; !ok {
			ma[k] = nil
		}
	}
	for k, va := range ma {
		vb := mb[k]
		if !reflect.DeepEqual(va, vb) {
			before[k], after[k] = va, vb
		}
	}
	return before, after
}

func fieldMap(is Issue) map[string]any {
	is.Labels = nil
	b, err := json.Marshal(is)
	if err != nil {
		return map[string]any{}
	}
	m := map[string]any{}
	if err := json.Unmarshal(b, &m); err != nil {
		return map[string]any{}
	}
	delete(m, "rev")
	delete(m, "updated_at")
	return m
}
