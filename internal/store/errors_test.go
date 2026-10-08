package store

import (
	"errors"
	"fmt"
	"testing"
)

// A change that does not fit the issue's state is refused with a
// *StateError naming the reason and the issue, so that starfixd chooses
// the fix without reading the message (internal/server/errors.go). It
// still matches ErrInvalid, and its text is the message these refusals
// had as plain errors.
func TestStateErrors(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	open := func(t *testing.T) Issue {
		t.Helper()
		return mustCreate(t, s, NewIssue{})
	}
	closed := func(t *testing.T) Issue {
		t.Helper()
		is, err := s.CloseIssue(ctx, alice, open(t).ID, 0, "")
		if err != nil {
			t.Fatal(err)
		}
		return is
	}
	tests := []struct {
		name   string
		run    func(t *testing.T) (IssueID, error)
		reason StateReason
		text   string // the message, with %s for the issue
	}{
		{"status change of a claimed issue", func(t *testing.T) (IssueID, error) {
			is, _, err := s.StartIssue(ctx, alice, open(t).ID, 0, false)
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.UpdateIssue(ctx, alice, is.ID, is.Rev, IssuePatch{Status: ptr(StatusBlocked)})
			return is.ID, err
		}, StateClaimed, "invalid input: issue %s is claimed by alice/sess-a; status and assignee change only through finish, close or a releasing handoff"},
		{"status change of a closed issue", func(t *testing.T) (IssueID, error) {
			is := closed(t)
			_, err := s.UpdateIssue(ctx, alice, is.ID, is.Rev, IssuePatch{Status: ptr(StatusOpen)})
			return is.ID, err
		}, StateClosed, "invalid input: issue %s is closed; reopen it first"},
		{"start of a closed issue", func(t *testing.T) (IssueID, error) {
			id := closed(t).ID
			_, _, err := s.StartIssue(ctx, alice, id, 0, false)
			return id, err
		}, StateClosed, "invalid input: issue %s is closed; reopen it first"},
		{"release of a closed issue", func(t *testing.T) (IssueID, error) {
			id := closed(t).ID
			_, err := s.HandoffIssue(ctx, alice, id, 0, HandoffNote{Note: "over to you"}, true, "", nil)
			return id, err
		}, StateClosed, "invalid input: issue %s is closed; reopen it first"},
		{"acceptance change on a closed issue", func(t *testing.T) (IssueID, error) {
			id := closed(t).ID
			_, err := s.Accept(ctx, alice, id, Acceptance{Tick: []int{1}})
			return id, err
		}, StateClosed, "invalid input: issue %s is closed; reopen it first"},
		{"close of a closed issue", func(t *testing.T) (IssueID, error) {
			id := closed(t).ID
			_, err := s.CloseIssue(ctx, alice, id, 0, "")
			return id, err
		}, StateAlreadyClosed, "invalid input: issue %s is already closed"},
		{"reopen of an open issue", func(t *testing.T) (IssueID, error) {
			id := open(t).ID
			_, err := s.ReopenIssue(ctx, alice, id, 0)
			return id, err
		}, StateNotClosed, "invalid input: issue %s is not closed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			id, err := tc.run(t)
			var se *StateError
			if !errors.As(err, &se) || se.Reason != tc.reason || se.ID != id {
				t.Fatalf("err = %v (%T), want a *StateError on %s with reason %q", err, err, id, tc.reason)
			}
			if !errors.Is(err, ErrInvalid) {
				t.Errorf("errors.Is(%v, ErrInvalid) = false, want true", err)
			}
			if want := fmt.Sprintf(tc.text, id); err.Error() != want {
				t.Errorf("err.Error() = %q, want %q", err.Error(), want)
			}
		})
	}
}
