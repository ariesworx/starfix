package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Taking work: StartIssue leases an issue to the actor (claims.go) and
// marks it in_progress, assigned to the actor's principal, in one
// transaction, so of two principals racing for one issue exactly one gets
// it. FinishIssue and a releasing HandoffIssue refuse an issue another
// principal holds, or a stale epoch, and end the claim.

// MaxDiscovered bounds the issues one FinishIssue may file.
const MaxDiscovered = 20

// StartIssue takes an issue for the actor for lease (DefaultLease when
// zero): it claims it and sets it in_progress, assigned to the actor's
// principal. With an empty id it takes the first issue Ready would list,
// or returns ErrNothingReady. Taking an issue the actor's own session
// holds extends the lease and changes nothing else; taking one another
// session of the same principal holds takes it over under a new epoch;
// one another principal holds is refused with a *HeldError, and a closed
// one with ErrInvalid.
func (s *Store) StartIssue(ctx context.Context, actor Actor, id IssueID, lease time.Duration) (Issue, Claim, error) {
	if id != "" {
		if err := id.Validate(); err != nil {
			return Issue{}, Claim{}, err
		}
	}
	if lease == 0 {
		lease = DefaultLease
	}
	if err := checkLease(lease); err != nil {
		return Issue{}, Claim{}, err
	}
	var out Issue
	var claim Claim
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
		if before.Status == StatusClosed {
			return fmt.Errorf("%w: issue %s is closed; reopen it first", ErrInvalid, target)
		}
		c, err := loadClaim(ctx, w.tx, target)
		if err != nil {
			return err
		}
		if by := heldBy(c, before, w.now); by != "" && by != w.actor.Principal {
			return &HeldError{ID: target, By: by}
		}
		if claim, err = take(ctx, w, c, lease); err != nil {
			return err
		}
		out = before
		if before.Status == StatusInProgress && before.Assignee == w.actor.Principal {
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
		return Issue{}, Claim{}, err
	}
	return out, claim, nil
}

// Finish is what FinishIssue records besides closing the issue.
type Finish struct {
	// Reason is the close reason.
	Reason string
	// Handoff, if its note is set, is recorded as a handoff note with
	// its fields.
	Handoff HandoffNote
	// Discovered are new issues, each linked discovered-from the finished
	// one. IDs are generated; ID and IdempotencyKey must be empty.
	Discovered []NewIssue
}

// FinishIssue closes an issue, ends its claim, records its handoff note
// and files the work discovered while doing it, all in one transaction:
// either everything is written or nothing is. It returns the closed issue
// and the new IDs, in the order given. An issue another principal holds
// is refused with a *HeldError (close overrides that); a non-zero epoch
// that is not the claim's current one with a *StaleEpochError; a closed
// issue with ErrInvalid.
func (s *Store) FinishIssue(ctx context.Context, actor Actor, id IssueID, epoch int64, f Finish) (Issue, []IssueID, error) {
	if err := id.Validate(); err != nil {
		return Issue{}, nil, err
	}
	if len(f.Reason) > 2000 {
		return Issue{}, nil, fmt.Errorf("%w: reason longer than 2000", ErrInvalid)
	}
	if err := f.Handoff.validate(); err != nil {
		return Issue{}, nil, err
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
		c, err := loadClaim(ctx, w.tx, id)
		if err != nil {
			return err
		}
		if err := checkHold(c, before, w.actor, epoch, w.now); err != nil {
			return err
		}
		for i, d := range f.Discovered {
			if _, err := insertIssue(ctx, w, ids[i], d, metas[i]); err != nil {
				return fmt.Errorf("discovered %q: %w", d.Title, err)
			}
			if err := insertDep(ctx, w, ids[i], id, DepDiscoveredFrom); err != nil {
				return err
			}
		}
		if f.Handoff.Note != "" {
			if err := insertHandoff(ctx, w, id, f.Handoff); err != nil {
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

// HandoffIssue records a handoff note, with its fields, on an issue
// without closing it, telling the principal it is handed to and those its
// note mentions. With
// release it also lets the issue go, so another can start it: the claim
// ends, in_progress becomes open and the assignee is cleared. Releasing an
// issue another principal holds is refused with a *HeldError, a stale
// epoch with a *StaleEpochError, and a closed issue with ErrInvalid; a
// note alone is accepted on any issue.
func (s *Store) HandoffIssue(ctx context.Context, actor Actor, id IssueID, epoch int64, h HandoffNote, release bool) (Issue, error) {
	if err := id.Validate(); err != nil {
		return Issue{}, err
	}
	if err := validBody(h.Note); err != nil {
		return Issue{}, err
	}
	if err := h.HandoffFields.validate(); err != nil {
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
			c, err := loadClaim(ctx, w.tx, id)
			if err != nil {
				return err
			}
			if err := checkHold(c, before, w.actor, epoch, w.now); err != nil {
				return err
			}
			if err := releaseClaim(ctx, w, c); err != nil {
				return err
			}
		}
		if err := insertHandoff(ctx, w, id, h); err != nil {
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
