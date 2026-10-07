package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Taking work, before stage 3's leased claims: an issue is held by the
// principal it is assigned to while it is in_progress. StartIssue takes an
// issue with a compare-and-swap on its rev, so of two principals racing
// for one issue exactly one gets it; FinishIssue and a releasing
// HandoffIssue refuse an issue another principal holds.

// MaxDiscovered bounds the issues one FinishIssue may file.
const MaxDiscovered = 20

// holder returns who holds is, if that is someone other than principal.
func holder(is Issue, principal string) string {
	if is.Status == StatusInProgress && is.Assignee != "" && is.Assignee != principal {
		return is.Assignee
	}
	return ""
}

// StartIssue takes an issue for the actor: status in_progress, assigned to
// the actor's principal. With an empty id it takes the first issue Ready
// would list, or returns ErrNothingReady. An issue the actor already holds
// is returned unchanged; one another principal holds is refused with a
// *HeldError; a closed one with ErrInvalid.
func (s *Store) StartIssue(ctx context.Context, actor Actor, id IssueID) (Issue, error) {
	if id != "" {
		if err := id.Validate(); err != nil {
			return Issue{}, err
		}
	}
	var out Issue
	err := s.write(ctx, actor, func(w *wtx) error {
		target := id
		if target == "" {
			err := w.tx.QueryRowContext(ctx, blockedCTE+`SELECT i.id FROM issues i `+readyWhere, w.now, 1).Scan(&target)
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNothingReady
			}
			if err != nil {
				return fmt.Errorf("ready: %w", err)
			}
		}
		before, err := loadIssue(ctx, w.tx, target)
		if err != nil {
			return err
		}
		switch {
		case before.Status == StatusClosed:
			return fmt.Errorf("%w: issue %s is closed; reopen it first", ErrInvalid, target)
		case holder(before, w.actor.Principal) != "":
			return &HeldError{ID: target, By: before.Assignee}
		case before.Status == StatusInProgress && before.Assignee == w.actor.Principal:
			out = before
			return nil
		}
		out, err = casUpdate(ctx, w, before, []string{"status = ?", "assignee = ?"},
			[]any{string(StatusInProgress), w.actor.Principal})
		if err != nil {
			return err
		}
		b, a := diff(before, out)
		return w.event(ctx, OpIssueUpdate, string(target), b, a, "")
	})
	if err != nil {
		return Issue{}, err
	}
	return out, nil
}

// Finish is what FinishIssue records besides closing the issue.
type Finish struct {
	// Reason is the close reason.
	Reason string
	// Handoff, if set, is recorded as a handoff note.
	Handoff string
	// Discovered are new issues, each linked discovered-from the finished
	// one. IDs are generated; ID and IdempotencyKey must be empty.
	Discovered []NewIssue
}

// FinishIssue closes an issue, records its handoff note and files the work
// discovered while doing it, all in one transaction: either everything is
// written or nothing is. It returns the closed issue and the new IDs, in
// the order given. An issue another principal holds is refused with a
// *HeldError (close overrides that); a closed one with ErrInvalid.
func (s *Store) FinishIssue(ctx context.Context, actor Actor, id IssueID, f Finish) (Issue, []IssueID, error) {
	if err := id.Validate(); err != nil {
		return Issue{}, nil, err
	}
	if len(f.Reason) > 2000 {
		return Issue{}, nil, fmt.Errorf("%w: reason longer than 2000", ErrInvalid)
	}
	if f.Handoff != "" {
		if err := validBody(f.Handoff); err != nil {
			return Issue{}, nil, err
		}
	}
	if len(f.Discovered) > MaxDiscovered {
		return Issue{}, nil, fmt.Errorf("%w: at most %d discovered issues", ErrInvalid, MaxDiscovered)
	}
	ids := make([]IssueID, len(f.Discovered))
	metas := make([]any, len(f.Discovered))
	for i := range f.Discovered {
		d := &f.Discovered[i]
		if d.ID != "" || d.IdempotencyKey != "" {
			return Issue{}, nil, fmt.Errorf("%w: a discovered issue takes a generated id", ErrInvalid)
		}
		if err := d.normalize(); err != nil {
			return Issue{}, nil, err
		}
		var err error
		if metas[i], err = nullJSON(d.Metadata); err != nil {
			return Issue{}, nil, err
		}
		if ids[i], err = NewID(s.opts.Prefix); err != nil {
			return Issue{}, nil, err
		}
	}
	var out Issue
	err := s.write(ctx, actor, func(w *wtx) error {
		before, err := loadIssue(ctx, w.tx, id)
		if err != nil {
			return err
		}
		if before.Status == StatusClosed {
			return fmt.Errorf("%w: issue %s is already closed", ErrInvalid, id)
		}
		if by := holder(before, w.actor.Principal); by != "" {
			return &HeldError{ID: id, By: by}
		}
		for i, d := range f.Discovered {
			if _, err := insertIssue(ctx, w, ids[i], d, metas[i]); err != nil {
				return fmt.Errorf("discovered %q: %w", d.Title, err)
			}
			if err := insertDep(ctx, w, ids[i], id, DepDiscoveredFrom); err != nil {
				return err
			}
		}
		if f.Handoff != "" {
			if _, err := insertComment(ctx, w, id, f.Handoff, CommentHandoff); err != nil {
				return err
			}
		}
		out, err = closeTx(ctx, w, before, f.Reason)
		return err
	})
	if err != nil {
		return Issue{}, nil, err
	}
	return out, ids, nil
}

// HandoffIssue records a handoff note on an issue without closing it. With
// release it also lets the issue go, so another can start it: in_progress
// becomes open and the assignee is cleared. Releasing an issue another
// principal holds is refused with a *HeldError, and a closed one with
// ErrInvalid; a note alone is accepted on any issue.
func (s *Store) HandoffIssue(ctx context.Context, actor Actor, id IssueID, note string, release bool) (Issue, error) {
	if err := id.Validate(); err != nil {
		return Issue{}, err
	}
	if err := validBody(note); err != nil {
		return Issue{}, err
	}
	var out Issue
	err := s.write(ctx, actor, func(w *wtx) error {
		before, err := loadIssue(ctx, w.tx, id)
		if err != nil {
			return err
		}
		if release {
			if before.Status == StatusClosed {
				return fmt.Errorf("%w: issue %s is closed; reopen it first", ErrInvalid, id)
			}
			if by := holder(before, w.actor.Principal); by != "" {
				return &HeldError{ID: id, By: by}
			}
		}
		if _, err := insertComment(ctx, w, id, note, CommentHandoff); err != nil {
			return err
		}
		out = before
		if !release || (before.Status != StatusInProgress && before.Assignee == "") {
			return nil
		}
		status := before.Status
		if status == StatusInProgress {
			status = StatusOpen
		}
		out, err = casUpdate(ctx, w, before, []string{"status = ?", "assignee = ?"}, []any{string(status), nil})
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
