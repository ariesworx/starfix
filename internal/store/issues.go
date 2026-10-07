package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"strings"
	"time"
)

// issueCols are the columns scanIssue reads, in its order, from issues
// aliased i.
const issueCols = `i.id, i.parent_id, i.title, i.body, i.design, i.acceptance, i.notes,
  i.status, i.priority, i.type, i.assignee, i.owner, i.due_at, i.defer_until,
  i.ephemeral, i.expires_at, i.pinned, i.template, i.metadata, i.close_reason,
  i.created_by, i.created_at, i.updated_at, i.closed_at, i.rev`

// scanner is a *sql.Row or *sql.Rows.
type scanner interface{ Scan(dest ...any) error }

// scanIssue scans issueCols, then extra, into an Issue. NULL becomes the
// zero value; Labels are left for attachLabels.
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

// timePtr maps NULL to nil and a time to UTC.
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

// nullJSON maps empty and JSON null metadata to NULL, and refuses
// invalid JSON with ErrInvalid.
func nullJSON(m json.RawMessage) (any, error) {
	if len(m) == 0 || string(m) == "null" {
		return nil, nil
	}
	if !json.Valid(m) {
		return nil, fmt.Errorf("%w: metadata is not valid JSON", ErrInvalid)
	}
	return string(m), nil
}

// loadIssue reads one issue with its labels, or returns ErrNotFound.
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

// placeholders returns n comma-separated question marks.
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// exists reports whether the issue id exists.
func exists(ctx context.Context, q querier, id IssueID) (bool, error) {
	var n int
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM issues WHERE id = ?`, string(id)).Scan(&n); err != nil {
		return false, fmt.Errorf("lookup %s: %w", id, err)
	}
	return n > 0, nil
}

// mustExist returns ErrNotFound unless the issue id exists.
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

// GetIssue returns one issue with its labels, or ErrNotFound.
func (s *Store) GetIssue(ctx context.Context, id IssueID) (Issue, error) {
	if err := id.Validate(); err != nil {
		return Issue{}, err
	}
	return loadIssue(ctx, s.r, id)
}

// normalize refuses, with ErrInvalid, a new issue whose fields are not
// valid, and fills in the defaults NewIssue documents.
func (n *NewIssue) normalize() error {
	if err := checkTitle(n.Title); err != nil {
		return err
	}
	for _, f := range []struct{ name, v string }{{"assignee", n.Assignee}, {"owner", n.Owner}} {
		if err := checkLine(f.name, f.v, maxName, false); err != nil {
			return err
		}
	}
	for _, f := range []struct{ name, v string }{{"body", n.Body}, {"design", n.Design}, {"acceptance", n.Acceptance}, {"notes", n.Notes}} {
		if err := checkText(f.name, f.v, maxText, false); err != nil {
			return err
		}
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
	if err := validIdem(n.IdempotencyKey); err != nil {
		return err
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

// checkNew refuses a new issue past the store's limits: too many distinct
// labels or acceptance items.
func (s *Store) checkNew(in NewIssue) error {
	distinct := map[string]bool{}
	for _, l := range in.Labels {
		distinct[l] = true
	}
	if len(distinct) > s.opts.Limits.Labels {
		return fmt.Errorf("%w: an issue has at most %d labels, not %d", ErrInvalid, s.opts.Limits.Labels, len(distinct))
	}
	return checkItems(in.Acceptance, s.opts.Limits.AcceptanceItems)
}

// checkTitle refuses a blank title or one that is not a single line of up
// to maxTitle bytes.
func checkTitle(t string) error {
	if strings.TrimSpace(t) == "" {
		return fmt.Errorf("%w: title must be 1-%d bytes, not blank", ErrInvalid, maxTitle)
	}
	return checkLine("title", t, maxTitle, true)
}

// CreateIssue creates an issue and returns it. With an IdempotencyKey, a
// repeat of the same create returns the issue the first one made and writes
// nothing, and the key reused for another request is refused with an
// [*IdemError]. An assignee other than the actor gets an inbox item.
//
// Invalid input, or more labels or acceptance items than the store's
// [Limits] allow, is refused with ErrInvalid; an ID in use with
// ErrExists; a parent that does not exist with ErrNotFound.
func (s *Store) CreateIssue(ctx context.Context, actor Actor, in NewIssue) (Issue, error) {
	if err := in.normalize(); err != nil {
		return Issue{}, err
	}
	if err := s.checkNew(in); err != nil {
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
		if done, err := w.replay(ctx, in.IdempotencyKey, "create", in, &out); done || err != nil {
			return err
		}
		var err error
		if out, err = insertIssue(ctx, w, id, in, meta); err != nil {
			return err
		}
		if err := w.settle(out); err != nil {
			return err
		}
		return w.notifyAssigned(ctx, out)
	})
	if err != nil {
		return Issue{}, err
	}
	return out, nil
}

// insertIssue writes a normalized new issue with the given ID and records
// its create event.
func insertIssue(ctx context.Context, w *wtx, id IssueID, in NewIssue, meta any) (Issue, error) {
	ok, err := exists(ctx, w.tx, id)
	if err != nil {
		return Issue{}, err
	}
	if ok {
		return Issue{}, fmt.Errorf("issue %s: %w", id, ErrExists)
	}
	if in.ParentID != "" {
		if err := mustExist(ctx, w.tx, in.ParentID); err != nil {
			return Issue{}, fmt.Errorf("parent: %w", err)
		}
	}
	wid, err := randomInt63()
	if err != nil {
		return Issue{}, err
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
			return Issue{}, fmt.Errorf("issue %s: %w", id, ErrExists)
		}
		return Issue{}, fmt.Errorf("insert issue: %w", err)
	}
	for _, l := range in.Labels {
		if _, err := w.exec(ctx, `INSERT IGNORE INTO labels (issue_id, label, created_at) VALUES (?, ?, ?)`,
			string(id), l, w.now); err != nil {
			return Issue{}, fmt.Errorf("insert label: %w", err)
		}
	}
	if err := tickInText(ctx, w, id, in.Acceptance); err != nil {
		return Issue{}, err
	}
	out, err := loadIssue(ctx, w.tx, id)
	if err != nil {
		return Issue{}, err
	}
	return out, w.event(ctx, OpIssueCreate, string(id), nil, out)
}

// UpdateIssue applies patch if the issue is still at rev expected, and
// returns the new state; an empty patch returns the issue unchanged. A
// new assignee other than the actor gets an inbox item. A stale rev, or a
// concurrent write that keeps winning, returns ErrConflict; an expected
// rev below 1 is refused with ErrInvalid.
//
// Holds come only from claims, so status in_progress is refused with
// [ErrStatusInProgress] (StartIssue sets it), and a change of status or
// assignee while the issue is claimed with ErrInvalid (finish, close or a
// releasing handoff ends the claim first). An issue another principal
// holds is refused with a [*ForbiddenError] unless the actor is an admin,
// a parent that does not exist with ErrNotFound, and a parent that would
// make a cycle with ErrCycle. New acceptance text cannot tick items ("[x]"
// counts only at create), and text that drops an item still open is
// refused with an [*AcceptanceError] (Dropped): tick or waive it first,
// so the change is on the record.
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
	if patch.Acceptance != nil {
		if err := checkItems(*patch.Acceptance, s.opts.Limits.AcceptanceItems); err != nil {
			return Issue{}, err
		}
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
		if len(sets) == 0 {
			out = before
			return nil
		}
		c, err := loadClaim(ctx, w.tx, id)
		if err != nil {
			return err
		}
		if err := w.guard(ctx, c, "update"); err != nil {
			return err
		}
		if c.active(w.now) && (patch.Status != nil && *patch.Status != before.Status ||
			patch.Assignee != nil && *patch.Assignee != before.Assignee) {
			return fmt.Errorf("%w: issue %s is claimed by %s/%s; %s", ErrInvalid, id, c.Holder.Principal, c.Holder.Session, errClaimedFields)
		}
		if patch.ParentID != nil && *patch.ParentID != "" {
			if err := mustExist(ctx, w.tx, *patch.ParentID); err != nil {
				return fmt.Errorf("parent: %w", err)
			}
			if err := checkEdge(ctx, w.tx, id, *patch.ParentID); err != nil {
				return err
			}
		}
		if patch.Acceptance != nil {
			if err := checkDropped(ctx, w.tx, before, *patch.Acceptance); err != nil {
				return err
			}
		}
		out, err = casUpdate(ctx, w, before, sets, args)
		if err != nil {
			return err
		}
		b, a := diff(before, out)
		if err := w.event(ctx, OpIssueUpdate, string(id), b, a); err != nil {
			return err
		}
		if out.Assignee != before.Assignee {
			return w.notifyAssigned(ctx, out)
		}
		return nil
	})
	if err != nil {
		return Issue{}, err
	}
	return out, nil
}

// errClaimedFields ends the refusal of a status or assignee change to a
// claimed issue. starfixd recognizes that refusal by its "is claimed by"
// text (internal/server/errors.go) to name the next step.
const errClaimedFields = "status and assignee change only through finish, close or a releasing handoff"

// ErrStatusInProgress refuses update's status in_progress: only start
// sets it, with a claim. It wraps ErrInvalid.
var ErrStatusInProgress = fmt.Errorf("%w: status in_progress is set only by start, which claims the issue", ErrInvalid)

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
	out, err := loadIssue(ctx, w.tx, before.ID)
	if err != nil {
		return Issue{}, err
	}
	if before.Status == StatusClosed || out.Status == StatusClosed {
		w.closedChanged = true
	}
	return out, nil
}

// columns validates the patch and turns it into SET clauses over a fixed
// column list, with their arguments.
func (p IssuePatch) columns() ([]string, []any, error) {
	var sets []string
	var args []any
	add := func(col string, v any) {
		sets = append(sets, col+" = ?")
		args = append(args, v)
	}
	if p.Title != nil {
		if err := checkTitle(*p.Title); err != nil {
			return nil, nil, err
		}
		add("title", *p.Title)
	}
	for _, f := range []struct {
		col string
		v   *string
	}{{"body", p.Body}, {"design", p.Design}, {"acceptance", p.Acceptance}, {"notes", p.Notes}} {
		if f.v != nil {
			if err := checkText(f.col, *f.v, maxText, false); err != nil {
				return nil, nil, err
			}
			add(f.col, *f.v)
		}
	}
	if p.Status != nil {
		if !p.Status.Valid() || *p.Status == StatusClosed {
			return nil, nil, fmt.Errorf("%w: status %q (close with CloseIssue)", ErrInvalid, *p.Status)
		}
		if *p.Status == StatusInProgress {
			return nil, nil, ErrStatusInProgress
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
	for _, f := range []struct {
		col string
		v   *string
	}{{"assignee", p.Assignee}, {"owner", p.Owner}} {
		if f.v != nil {
			if err := checkLine(f.col, *f.v, maxName, false); err != nil {
				return nil, nil, err
			}
			add(f.col, nullStr(*f.v))
		}
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

// CloseIssue closes an issue, ends any claim on it and returns it; when a
// live claim was another session's, that session gets a claim.lost inbox
// item. expected 0 skips the revision check: close wins over concurrent
// edits (design §8). An issue with acceptance items neither ticked nor
// waived is refused with an [*AcceptanceError], one another principal
// holds with a [*ForbiddenError] unless the actor is an admin, and one
// already closed with ErrInvalid.
func (s *Store) CloseIssue(ctx context.Context, actor Actor, id IssueID, expected Rev, reason string) (Issue, error) {
	return s.closeIssue(ctx, actor, id, expected, reason, false)
}

// ForceClose is CloseIssue that closes despite open acceptance items,
// listing them in the close event as acceptance_overridden. Only an admin
// may force; anyone else is refused with a [*ForbiddenError].
func (s *Store) ForceClose(ctx context.Context, actor Actor, id IssueID, expected Rev, reason string) (Issue, error) {
	return s.closeIssue(ctx, actor, id, expected, reason, true)
}

func (s *Store) closeIssue(ctx context.Context, actor Actor, id IssueID, expected Rev, reason string, force bool) (Issue, error) {
	if err := checkLine("reason", reason, maxReason, false); err != nil {
		return Issue{}, err
	}
	return s.setClosed(ctx, actor, id, expected, true, reason, force)
}

// ReopenIssue reopens a closed issue. expected 0 skips the revision check.
// An issue another principal holds is refused with a [*ForbiddenError]
// unless the actor is an admin, and one that is not closed with
// ErrInvalid.
func (s *Store) ReopenIssue(ctx context.Context, actor Actor, id IssueID, expected Rev) (Issue, error) {
	return s.setClosed(ctx, actor, id, expected, false, "", false)
}

// setClosed closes the issue id (closing) or reopens it, for CloseIssue,
// ForceClose and ReopenIssue.
func (s *Store) setClosed(ctx context.Context, actor Actor, id IssueID, expected Rev, closing bool, reason string, force bool) (Issue, error) {
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
		if force && !w.admin {
			return &ForbiddenError{ID: id, Action: "close --force"}
		}
		c, err := loadClaim(ctx, w.tx, id)
		if err != nil {
			return err
		}
		op := "reopen"
		if closing {
			op = "close"
		}
		if err := w.guard(ctx, c, op); err != nil {
			return err
		}
		if closing {
			out, err = closeTx(ctx, w, before, reason, force)
			return err
		}
		if before.Status != StatusClosed {
			return fmt.Errorf("%w: issue %s is not closed", ErrInvalid, id)
		}
		out, err = setStatus(ctx, w, before, OpIssueReopen, []any{string(StatusOpen), nil, nil}, nil)
		return err
	})
	if err != nil {
		return Issue{}, err
	}
	return out, nil
}

// closeTx closes before, read in w, ends any claim on it and records the
// event. Open acceptance items refuse it, unless force, which records
// them in the event.
func closeTx(ctx context.Context, w *wtx, before Issue, reason string, force bool) (Issue, error) {
	if before.Status == StatusClosed {
		return Issue{}, fmt.Errorf("%w: issue %s is already closed", ErrInvalid, before.ID)
	}
	items, err := acceptanceItems(ctx, w.tx, before)
	if err != nil {
		return Issue{}, err
	}
	var extra map[string]any
	if open := openItems(items); len(open) > 0 {
		if !force {
			return Issue{}, &AcceptanceError{ID: before.ID, Open: open}
		}
		extra = map[string]any{"acceptance_overridden": open}
	}
	c, err := loadClaim(ctx, w.tx, before.ID)
	if err != nil {
		return Issue{}, err
	}
	if err := w.ended(ctx, c, "closed"); err != nil {
		return Issue{}, err
	}
	if err := releaseClaim(ctx, w, c); err != nil {
		return Issue{}, err
	}
	return setStatus(ctx, w, before, OpIssueClose, []any{string(StatusClosed), w.now, nullStr(reason)}, extra)
}

// setStatus writes status, closed_at and close_reason and records op,
// with extra added to the event's after state.
func setStatus(ctx context.Context, w *wtx, before Issue, op Op, args []any, extra map[string]any) (Issue, error) {
	out, err := casUpdate(ctx, w, before, []string{"status = ?", "closed_at = ?", "close_reason = ?"}, args)
	if err != nil {
		return Issue{}, err
	}
	b, a := diff(before, out)
	maps.Copy(a, extra)
	return out, w.event(ctx, op, string(before.ID), b, a)
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

// fieldMap returns an issue's JSON fields without labels, rev and
// updated_at. An Issue always encodes, since its metadata is valid JSON,
// so the error returns are not reached.
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
