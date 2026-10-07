package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"unicode/utf8"
)

// Structured handoffs (design §3, §7): a handoff note is still a comment
// of kind handoff, and may carry fields the next person can act on
// without parsing prose: the state of the work, the next step, the branch
// and worktree it is on, and the principal it is handed to. The fields
// live in the handoffs table, keyed by the note's comment.

// HandoffState says how far the work got.
type HandoffState string

// Handoff states.
const (
	HandoffDone    HandoffState = "done"
	HandoffPartial HandoffState = "partial"
	HandoffBlocked HandoffState = "blocked"
)

// HandoffFields are a handoff's structured fields. Empty means not given.
type HandoffFields struct {
	State    HandoffState `json:"state,omitempty"`
	Next     string       `json:"next,omitempty"`
	Branch   string       `json:"branch,omitempty"`
	Worktree string       `json:"worktree,omitempty"`
	To       string       `json:"to,omitempty"`
}

// HandoffNote is what a handoff records: the free-text note and its
// optional fields. Fields need a note.
type HandoffNote struct {
	Note string
	HandoffFields
}

// Handoff is a recorded handoff note with its fields.
type Handoff struct {
	Comment
	HandoffFields
}

var (
	// handoffNext is one line of up to 500 bytes.
	handoffNext = regexp.MustCompile(`^[^\x00-\x1f\x7f]{1,500}$`)
	// handoffBranch is a git branch name that cannot be read as an option.
	handoffBranch = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/+@-]{0,254}$`)
	// handoffWorktree is a path on one line; validate caps it at 1024
	// bytes, more than RE2 will count.
	handoffWorktree = regexp.MustCompile(`^[^\x00-\x1f\x7f]+$`)
)

func (f HandoffFields) empty() bool { return f == HandoffFields{} }

// validate checks each given field against its pattern.
func (f HandoffFields) validate() error {
	switch f.State {
	case "", HandoffDone, HandoffPartial, HandoffBlocked:
	default:
		return fmt.Errorf("%w: handoff state %q must be done, partial or blocked", ErrInvalid, f.State)
	}
	for _, c := range []struct {
		name, v, rule string
		re            *regexp.Regexp
		max           int
	}{
		{"next", f.Next, "one line of up to 500 bytes", handoffNext, 500},
		{"branch", f.Branch, "a branch name of up to 255 letters, digits and ._/+@-, starting with a letter or digit", handoffBranch, 255},
		{"worktree", f.Worktree, "a path of up to 1024 bytes on one line", handoffWorktree, 1024},
		{"to", f.To, "a principal: lowercase letters, digits and ._-, starting with a letter", PrincipalPattern, 64},
	} {
		if c.v != "" && (len(c.v) > c.max || !utf8.ValidString(c.v) || !c.re.MatchString(c.v)) {
			return fmt.Errorf("%w: handoff %s must be %s", ErrInvalid, c.name, c.rule)
		}
	}
	return nil
}

// validate checks a note and its fields. An empty note is allowed only
// with no fields (a finish without a handoff).
func (h HandoffNote) validate() error {
	if h.Note == "" && !h.empty() {
		return fmt.Errorf("%w: a handoff's state, next, branch, worktree and to need a note", ErrInvalid)
	}
	if h.Note != "" {
		if err := validBody(h.Note); err != nil {
			return err
		}
	}
	return h.HandoffFields.validate()
}

// insertHandoff records h on issue id: the note as a handoff comment, its
// fields, an inbox item for the principal it is handed to, and items for
// the principals its note mentions.
func insertHandoff(ctx context.Context, w *wtx, id IssueID, h HandoffNote) error {
	c, err := insertNote(ctx, w, id, h.Note, CommentHandoff, h.HandoffFields)
	if err != nil {
		return err
	}
	if h.To != "" {
		body := h.Next
		if body == "" {
			body = h.Note
		}
		if err := w.notify(ctx, InboxItem{To: h.To, Kind: InboxHandoff, Issue: id, Body: body}); err != nil {
			return err
		}
	}
	return w.notifyMentions(ctx, id, c.Body, h.To)
}

// LastHandoff returns the newest handoff note on an issue, with its
// fields, or nil.
func (s *Store) LastHandoff(ctx context.Context, id IssueID) (*Handoff, error) {
	if err := id.Validate(); err != nil {
		return nil, err
	}
	var h Handoff
	var state, next, branch, worktree, to sql.NullString
	err := s.r.QueryRowContext(ctx, `SELECT c.id, c.issue_id, c.author, c.session, c.kind, c.body, c.created_at,
  h.state, h.next_step, h.branch, h.worktree, h.to_principal
  FROM comments c LEFT JOIN handoffs h ON h.comment_id = c.id
  WHERE c.issue_id = ? AND c.kind = ? ORDER BY c.created_at DESC, c.id DESC LIMIT 1`,
		string(id), string(CommentHandoff)).Scan(&h.ID, &h.Issue, &h.Author, &h.Session, &h.Kind, &h.Body, &h.CreatedAt,
		&state, &next, &branch, &worktree, &to)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("last handoff on %s: %w", id, err)
	}
	h.CreatedAt = h.CreatedAt.UTC()
	h.HandoffFields = HandoffFields{State: HandoffState(state.String), Next: next.String, Branch: branch.String,
		Worktree: worktree.String, To: to.String}
	return &h, nil
}
