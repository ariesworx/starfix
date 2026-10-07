package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"time"
)

// ImportOutcome is what an import write did, or would do.
type ImportOutcome string

// Import outcomes.
const (
	// ImportCreated: the row did not exist and was written.
	ImportCreated ImportOutcome = "created"
	// ImportUpdated: the row existed, differed, and the source was newer.
	ImportUpdated ImportOutcome = "updated"
	// ImportUnchanged: the row already held exactly this.
	ImportUnchanged ImportOutcome = "unchanged"
	// ImportStale: the row differs but is as new as the source or newer, or
	// is append-only; the stored row is kept.
	ImportStale ImportOutcome = "stale"
)

// ImportResult reports what ImportIssue did, or would do.
type ImportResult struct {
	Outcome ImportOutcome
	// LabelsAdded counts labels merged into the issue. Import never
	// removes a label: labels are set-like (design §8).
	LabelsAdded int
}

// commentIDPattern bounds comment IDs to the column and to characters that
// need no quoting anywhere.
var commentIDPattern = regexp.MustCompile(`^[a-z0-9]{1,32}$`)

// normalizeImport validates an imported issue and brings it to the form the
// store reads back: UTC times at microsecond precision, labels sorted and
// unique, and an updated_at no earlier than created_at.
func normalizeImport(in Issue) (Issue, error) {
	if err := in.ID.Validate(); err != nil {
		return Issue{}, err
	}
	bad := func(format string, a ...any) (Issue, error) {
		return Issue{}, fmt.Errorf("%w: issue %s: "+format, append([]any{ErrInvalid, in.ID}, a...)...)
	}
	for _, err := range []error{
		checkTitle(in.Title),
		checkLine("created_by", in.CreatedBy, maxName, true),
		checkLine("assignee", in.Assignee, maxName, false),
		checkLine("owner", in.Owner, maxName, false),
		checkLine("close reason", in.CloseReason, maxReason, false),
		checkText("body", in.Body, maxText, false),
		checkText("design", in.Design, maxText, false),
		checkText("acceptance", in.Acceptance, maxText, false),
		checkText("notes", in.Notes, maxText, false),
	} {
		if err != nil {
			return Issue{}, fmt.Errorf("issue %s: %w", in.ID, err)
		}
	}
	switch {
	case !in.Status.Valid():
		return bad("status %q", in.Status)
	case !in.Priority.Valid():
		return bad("priority %d", in.Priority)
	case !in.Type.Valid():
		return bad("type %q", in.Type)
	case in.CreatedAt.IsZero():
		return bad("created_at is required")
	}
	if in.ParentID != "" {
		if err := in.ParentID.Validate(); err != nil {
			return Issue{}, err
		}
		if in.ParentID == in.ID {
			return Issue{}, fmt.Errorf("%s is its own parent: %w", in.ID, ErrCycle)
		}
	}
	if _, err := nullJSON(in.Metadata); err != nil {
		return Issue{}, err
	}
	if string(in.Metadata) == "null" {
		in.Metadata = nil
	}
	labels := slices.Clone(in.Labels)
	for _, l := range labels {
		if err := validLabel(l); err != nil {
			return Issue{}, err
		}
	}
	slices.Sort(labels)
	in.Labels = slices.Compact(labels)

	norm := func(t time.Time) time.Time { return t.UTC().Truncate(time.Microsecond) }
	normPtr := func(t *time.Time) *time.Time {
		if t == nil || t.IsZero() {
			return nil
		}
		u := norm(*t)
		return &u
	}
	in.CreatedAt = norm(in.CreatedAt)
	in.UpdatedAt = norm(in.UpdatedAt)
	if in.UpdatedAt.Before(in.CreatedAt) {
		in.UpdatedAt = in.CreatedAt
	}
	in.DueAt, in.DeferUntil, in.ExpiresAt, in.ClosedAt = normPtr(in.DueAt), normPtr(in.DeferUntil), normPtr(in.ExpiresAt), normPtr(in.ClosedAt)
	in.Rev = 0
	return in, nil
}

// sameJSON reports whether two JSON documents are equal as values, so key
// order and spacing (which Dolt normalizes) do not count. Empty and null
// are equal.
func sameJSON(a, b []byte) bool {
	decode := func(m []byte) (any, bool) {
		if len(bytes.TrimSpace(m)) == 0 {
			return nil, true
		}
		d := json.NewDecoder(bytes.NewReader(m))
		d.UseNumber()
		var v any
		return v, d.Decode(&v) == nil
	}
	va, oka := decode(a)
	vb, okb := decode(b)
	return oka && okb && reflect.DeepEqual(va, vb)
}

// sameIssue compares every stored field except rev and labels.
func sameIssue(a, b Issue) bool {
	if !sameJSON(a.Metadata, b.Metadata) {
		return false
	}
	a.Rev, b.Rev = 0, 0
	a.Labels, b.Labels = nil, nil
	a.Metadata, b.Metadata = nil, nil
	ja, erra := json.Marshal(a)
	jb, errb := json.Marshal(b)
	return erra == nil && errb == nil && bytes.Equal(ja, jb)
}

// planIssue decides the outcome for in against the stored issue, and lists
// the labels to add.
func planIssue(before Issue, in Issue) (ImportOutcome, []string) {
	var add []string
	for _, l := range in.Labels {
		if !slices.Contains(before.Labels, l) {
			add = append(add, l)
		}
	}
	switch {
	case sameIssue(before, in) && len(add) == 0:
		return ImportUnchanged, nil
	case sameIssue(before, in):
		return ImportUpdated, add
	case in.UpdatedAt.After(before.UpdatedAt):
		return ImportUpdated, add
	}
	return ImportStale, add
}

// PlanImportIssue reports what ImportIssue would do with in, without
// writing. It does not check that the parent exists or would not make a
// cycle; the importer checks those against its whole input.
func (s *Store) PlanImportIssue(ctx context.Context, in Issue) (ImportResult, error) {
	in, err := normalizeImport(in)
	if err != nil {
		return ImportResult{}, err
	}
	before, err := loadIssue(ctx, s.r, in.ID)
	if errors.Is(err, ErrNotFound) {
		return ImportResult{Outcome: ImportCreated, LabelsAdded: len(in.Labels)}, nil
	}
	if err != nil {
		return ImportResult{}, err
	}
	out, add := planIssue(before, in)
	return ImportResult{Outcome: out, LabelsAdded: len(add)}, nil
}

// ImportIssue writes an issue from another tracker, keeping its ID, author,
// status (closed included) and timestamps, which CreateIssue would replace.
// A new issue is created at rev 1. An existing one is overwritten only when
// it differs and in.UpdatedAt is later than the stored updated_at;
// otherwise the stored issue is kept (ImportStale), so edits made here
// after an earlier import survive a re-import. Labels are merged either
// way. Importing the same issue twice changes nothing the second time.
// The parent, when set, must exist and must not make a cycle.
func (s *Store) ImportIssue(ctx context.Context, actor Actor, in Issue) (ImportResult, error) {
	in, err := normalizeImport(in)
	if err != nil {
		return ImportResult{}, err
	}
	meta, err := nullJSON(in.Metadata)
	if err != nil {
		return ImportResult{}, err
	}
	cols := []any{nullStr(in.ParentID), in.Title, in.Body, in.Design, in.Acceptance, in.Notes,
		string(in.Status), int(in.Priority), string(in.Type), nullStr(in.Assignee), nullStr(in.Owner),
		nullTime(in.DueAt), nullTime(in.DeferUntil), in.Ephemeral, nullTime(in.ExpiresAt), in.Pinned, in.Template,
		meta, nullStr(in.CloseReason), in.CreatedBy, in.CreatedAt, in.UpdatedAt, nullTime(in.ClosedAt)}
	var res ImportResult
	err = s.write(ctx, actor, func(w *wtx) error {
		res = ImportResult{}
		checkParent := func() error {
			if in.ParentID == "" {
				return nil
			}
			if err := mustExist(ctx, w.tx, in.ParentID); err != nil {
				return fmt.Errorf("parent: %w", err)
			}
			return checkEdge(ctx, w.tx, in.ID, in.ParentID)
		}
		wid, err := randomInt63()
		if err != nil {
			return err
		}
		before, err := loadIssue(ctx, w.tx, in.ID)
		var add []string
		switch {
		case errors.Is(err, ErrNotFound):
			if err := checkParent(); err != nil {
				return err
			}
			args := append([]any{string(in.ID)}, cols...)
			if _, err := w.exec(ctx, `INSERT INTO issues (id, parent_id, title, body, design, acceptance, notes,
  status, priority, type, assignee, owner, due_at, defer_until, ephemeral, expires_at, pinned, template,
  metadata, close_reason, created_by, created_at, updated_at, closed_at, rev, write_id)
  VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?)`,
				append(args, wid)...); err != nil {
				return fmt.Errorf("insert issue: %w", err)
			}
			if err := tickInText(ctx, w, in.ID, in.Acceptance); err != nil {
				return err
			}
			res.Outcome, add = ImportCreated, in.Labels
		case err != nil:
			return err
		default:
			res.Outcome, add = planIssue(before, in)
			if res.Outcome == ImportUpdated && !sameIssue(before, in) {
				if in.ParentID != before.ParentID {
					if err := checkParent(); err != nil {
						return err
					}
				}
				args := append(cols, wid, string(in.ID), int64(before.Rev))
				n, err := w.exec(ctx, `UPDATE issues SET parent_id = ?, title = ?, body = ?, design = ?,
  acceptance = ?, notes = ?, status = ?, priority = ?, type = ?, assignee = ?, owner = ?, due_at = ?,
  defer_until = ?, ephemeral = ?, expires_at = ?, pinned = ?, template = ?, metadata = ?, close_reason = ?,
  created_by = ?, created_at = ?, updated_at = ?, closed_at = ?, rev = rev + 1, write_id = ?
  WHERE id = ? AND rev = ?`, args...)
				if err != nil {
					return fmt.Errorf("update issue %s: %w", in.ID, err)
				}
				if n != 1 {
					return fmt.Errorf("issue %s changed during import: %w", in.ID, ErrConflict)
				}
				if err := tickInText(ctx, w, in.ID, in.Acceptance); err != nil {
					return err
				}
				after, err := loadIssue(ctx, w.tx, in.ID)
				if err != nil {
					return err
				}
				b, a := diff(before, after)
				w.closedChanged = true
				if err := w.event(ctx, OpIssueImport, string(in.ID), b, a); err != nil {
					return err
				}
			}
		}
		for _, l := range add {
			if _, err := w.exec(ctx, `INSERT IGNORE INTO labels (issue_id, label, created_at) VALUES (?, ?, ?)`,
				string(in.ID), l, w.now); err != nil {
				return fmt.Errorf("insert label: %w", err)
			}
			if res.Outcome != ImportCreated {
				if err := w.event(ctx, OpLabelAdd, string(in.ID), nil, map[string]string{"label": l}); err != nil {
					return err
				}
			}
		}
		res.LabelsAdded = len(add)
		if res.Outcome == ImportCreated {
			after, err := loadIssue(ctx, w.tx, in.ID)
			if err != nil {
				return err
			}
			w.closedChanged = true
			return w.event(ctx, OpIssueImport, string(in.ID), nil, after)
		}
		return nil
	})
	if err != nil {
		return ImportResult{}, err
	}
	return res, nil
}

func normalizeDep(d Dep) (Dep, error) {
	if err := validEdge(d.From, d.To, d.Type); err != nil {
		return Dep{}, err
	}
	if err := checkLine("created_by", d.CreatedBy, maxName, true); err != nil {
		return Dep{}, fmt.Errorf("dep %s -> %s: %w", d.From, d.To, err)
	}
	if d.CreatedAt.IsZero() {
		return Dep{}, fmt.Errorf("%w: dep %s -> %s: created_at is required", ErrInvalid, d.From, d.To)
	}
	if _, err := nullJSON(d.Metadata); err != nil {
		return Dep{}, err
	}
	if string(d.Metadata) == "null" {
		d.Metadata = nil
	}
	d.CreatedAt = d.CreatedAt.UTC().Truncate(time.Microsecond)
	return d, nil
}

// planDep compares d with the stored edge of the same key, if any.
func planDep(ctx context.Context, q querier, d Dep) (ImportOutcome, error) {
	var meta []byte
	err := q.QueryRowContext(ctx, `SELECT metadata FROM deps WHERE from_id = ? AND to_id = ? AND type = ?`,
		string(d.From), string(d.To), string(d.Type)).Scan(&meta)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return ImportCreated, nil
	case err != nil:
		return "", fmt.Errorf("lookup dep: %w", err)
	case sameJSON(meta, d.Metadata):
		return ImportUnchanged, nil
	}
	return ImportStale, nil
}

// PlanImportDep reports what ImportDep would do with d, without writing.
// Like PlanImportIssue it leaves existence and cycles to the importer.
func (s *Store) PlanImportDep(ctx context.Context, d Dep) (ImportOutcome, error) {
	d, err := normalizeDep(d)
	if err != nil {
		return "", err
	}
	return planDep(ctx, s.r, d)
}

// ImportDep adds an edge with its source's author, time and metadata. An
// existing edge is never rewritten: the same edge is ImportUnchanged, one
// with other metadata ImportStale. Both ends must exist, and a blocking
// edge must not make a cycle.
func (s *Store) ImportDep(ctx context.Context, actor Actor, d Dep) (ImportOutcome, error) {
	d, err := normalizeDep(d)
	if err != nil {
		return "", err
	}
	meta, err := nullJSON(d.Metadata)
	if err != nil {
		return "", err
	}
	var out ImportOutcome
	err = s.write(ctx, actor, func(w *wtx) error {
		for _, id := range []IssueID{d.From, d.To} {
			if err := mustExist(ctx, w.tx, id); err != nil {
				return err
			}
		}
		if out, err = planDep(ctx, w.tx, d); err != nil || out != ImportCreated {
			return err
		}
		if d.Type.Blocking() {
			if err := checkEdge(ctx, w.tx, d.From, d.To); err != nil {
				return err
			}
		}
		wid, err := randomInt63()
		if err != nil {
			return err
		}
		if _, err := w.exec(ctx, `INSERT INTO deps (from_id, to_id, type, metadata, created_by, created_at, rev, write_id)
  VALUES (?, ?, ?, ?, ?, ?, 1, ?)`, string(d.From), string(d.To), string(d.Type), meta, d.CreatedBy, d.CreatedAt, wid); err != nil {
			return fmt.Errorf("insert dep: %w", err)
		}
		return w.event(ctx, OpDepAdd, string(d.From), nil, d)
	})
	if err != nil {
		return "", err
	}
	return out, nil
}

func normalizeComment(c Comment) (Comment, error) {
	if !commentIDPattern.MatchString(c.ID) {
		return Comment{}, fmt.Errorf("%w: comment id %q must be 1-32 lowercase letters and digits", ErrInvalid, c.ID)
	}
	if err := c.Issue.Validate(); err != nil {
		return Comment{}, err
	}
	for _, err := range []error{
		checkText("body", c.Body, maxText, true),
		checkLine("author", c.Author, maxName, true),
		checkLine("session", c.Session, maxName, false),
	} {
		if err != nil {
			return Comment{}, fmt.Errorf("comment %s: %w", c.ID, err)
		}
	}
	switch {
	case c.CreatedAt.IsZero():
		return Comment{}, fmt.Errorf("%w: comment %s: created_at is required", ErrInvalid, c.ID)
	}
	c.CreatedAt = c.CreatedAt.UTC().Truncate(time.Microsecond)
	return c, nil
}

func planComment(ctx context.Context, q querier, c Comment) (ImportOutcome, error) {
	var got Comment
	err := q.QueryRowContext(ctx, `SELECT id, issue_id, author, session, body, created_at FROM comments WHERE id = ?`,
		c.ID).Scan(&got.ID, &got.Issue, &got.Author, &got.Session, &got.Body, &got.CreatedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return ImportCreated, nil
	case err != nil:
		return "", fmt.Errorf("lookup comment: %w", err)
	}
	if got.Issue == c.Issue && got.Author == c.Author && got.Session == c.Session &&
		got.Body == c.Body && got.CreatedAt.Equal(c.CreatedAt) {
		return ImportUnchanged, nil
	}
	return ImportStale, nil
}

// PlanImportComment reports what ImportComment would do, without writing.
func (s *Store) PlanImportComment(ctx context.Context, c Comment) (ImportOutcome, error) {
	c, err := normalizeComment(c)
	if err != nil {
		return "", err
	}
	return planComment(ctx, s.r, c)
}

// ImportComment adds a comment with its source's ID, author and time.
// Comments are append-only: an existing ID is never rewritten, and is
// ImportUnchanged when identical, ImportStale when not.
func (s *Store) ImportComment(ctx context.Context, actor Actor, c Comment) (ImportOutcome, error) {
	c, err := normalizeComment(c)
	if err != nil {
		return "", err
	}
	var out ImportOutcome
	err = s.write(ctx, actor, func(w *wtx) error {
		if err := mustExist(ctx, w.tx, c.Issue); err != nil {
			return err
		}
		if out, err = planComment(ctx, w.tx, c); err != nil || out != ImportCreated {
			return err
		}
		if _, err := w.exec(ctx, `INSERT INTO comments (id, issue_id, author, session, body, created_at)
  VALUES (?, ?, ?, ?, ?, ?)`, c.ID, string(c.Issue), c.Author, c.Session, c.Body, c.CreatedAt); err != nil {
			return fmt.Errorf("insert comment: %w", err)
		}
		return w.event(ctx, OpCommentAdd, string(c.Issue), nil, c)
	})
	if err != nil {
		return "", err
	}
	return out, nil
}
