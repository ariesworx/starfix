package store

import (
	"encoding/json"
	"fmt"
	"regexp"
	"time"

	"github.com/ariesworx/starfix/internal/safetext"
)

// Rev is an issue's revision. It starts at 1 and rises by one with every
// write to the issue's own row; labels, comments and dependencies have
// rows of their own and do not move it.
type Rev int64

// Priority is 0 (critical) to 4 (backlog).
type Priority int

// Priorities, highest first.
const (
	P0 Priority = iota
	P1
	P2
	P3
	P4
)

// Valid reports whether p is in range.
func (p Priority) Valid() bool { return p >= P0 && p <= P4 }

// Status is an issue's workflow state.
type Status string

// Statuses.
const (
	StatusOpen       Status = "open"
	StatusInProgress Status = "in_progress"
	StatusBlocked    Status = "blocked"
	StatusDeferred   Status = "deferred"
	StatusClosed     Status = "closed"
)

// Valid reports whether s is a known status.
func (s Status) Valid() bool {
	switch s {
	case StatusOpen, StatusInProgress, StatusBlocked, StatusDeferred, StatusClosed:
		return true
	}
	return false
}

// IssueType classifies an issue.
type IssueType string

// Issue types.
const (
	TypeBug     IssueType = "bug"
	TypeFeature IssueType = "feature"
	TypeTask    IssueType = "task"
	TypeEpic    IssueType = "epic"
	TypeChore   IssueType = "chore"
)

// Valid reports whether t is a known type.
func (t IssueType) Valid() bool {
	switch t {
	case TypeBug, TypeFeature, TypeTask, TypeEpic, TypeChore:
		return true
	}
	return false
}

// DepType is the kind of a dependency edge. An edge From→To reads
// "From depends on To".
type DepType string

// Dependency types.
const (
	DepBlocks            DepType = "blocks"
	DepConditionalBlocks DepType = "conditional-blocks"
	DepWaitsFor          DepType = "waits-for"
	DepRelated           DepType = "related"
	DepDiscoveredFrom    DepType = "discovered-from"
	DepDuplicates        DepType = "duplicates"
	DepSupersedes        DepType = "supersedes"
)

// Valid reports whether t is a known dependency type.
func (t DepType) Valid() bool {
	switch t {
	case DepBlocks, DepConditionalBlocks, DepWaitsFor, DepRelated,
		DepDiscoveredFrom, DepDuplicates, DepSupersedes:
		return true
	}
	return false
}

// Blocking reports whether edges of type t take part in the cycle check.
func (t DepType) Blocking() bool {
	return t == DepBlocks || t == DepConditionalBlocks || t == DepWaitsFor
}

// readyBlocking lists the types that keep an issue out of Ready while the
// target is open. waits-for gates arrive with the gate runner.
const readyBlocking = `'blocks','conditional-blocks'`

// Op names a mutation in the event log.
type Op string

// Event operations.
const (
	OpIssueCreate Op = "issue.create"
	OpIssueUpdate Op = "issue.update"
	OpIssueClose  Op = "issue.close"
	OpIssueReopen Op = "issue.reopen"
	OpDepAdd      Op = "dep.add"
	OpDepRemove   Op = "dep.remove"
	OpLabelAdd    Op = "label.add"
	OpLabelRemove Op = "label.remove"
	OpCommentAdd  Op = "comment.add"
	// OpIssueImport records an issue written by an importer, created or
	// overwritten with the source's own timestamps and author.
	OpIssueImport Op = "issue.import"
)

// Actor identifies who made a change: the authenticated principal, the
// client session and the machine.
type Actor struct {
	Principal string `json:"principal"`
	Session   string `json:"session"`
	Machine   string `json:"machine"`
}

// validate refuses an actor whose principal, session or machine is
// empty, longer than maxName or not one line of safe text.
func (a Actor) validate() error {
	for _, f := range []struct{ name, v string }{
		{"principal", a.Principal}, {"session", a.Session}, {"machine", a.Machine},
	} {
		if err := checkLine("actor "+f.name, f.v, maxName, true); err != nil {
			return err
		}
	}
	return nil
}

// Issue is one tracked item, as the store reads it back: times in UTC to
// the microsecond, and Labels sorted.
type Issue struct {
	ID          IssueID         `json:"id"`
	ParentID    IssueID         `json:"parent_id,omitempty"`
	Title       string          `json:"title"`
	Body        string          `json:"body,omitempty"`
	Design      string          `json:"design,omitempty"`
	Acceptance  string          `json:"acceptance,omitempty"`
	Notes       string          `json:"notes,omitempty"`
	Status      Status          `json:"status"`
	Priority    Priority        `json:"priority"`
	Type        IssueType       `json:"type"`
	Assignee    string          `json:"assignee,omitempty"`
	Owner       string          `json:"owner,omitempty"`
	DueAt       *time.Time      `json:"due_at,omitempty"`
	DeferUntil  *time.Time      `json:"defer_until,omitempty"`
	ExpiresAt   *time.Time      `json:"expires_at,omitempty"`
	Ephemeral   bool            `json:"ephemeral,omitempty"`
	Pinned      bool            `json:"pinned,omitempty"`
	Template    bool            `json:"template,omitempty"`
	Metadata    json.RawMessage `json:"metadata,omitempty"`
	CloseReason string          `json:"close_reason,omitempty"`
	CreatedBy   string          `json:"created_by"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
	ClosedAt    *time.Time      `json:"closed_at,omitempty"`
	Rev         Rev             `json:"rev"`
	Labels      []string        `json:"labels,omitempty"`
	// Account is the issue's own account, empty when it inherits one
	// ([Store.ResolveAccount]).
	Account string `json:"account,omitempty"`
}

// NewIssue is the input to CreateIssue. Zero values take defaults: a
// generated ID, status open, priority P2, type task.
type NewIssue struct {
	// ID is optional; clients that create offline supply their own.
	ID IssueID
	// IdempotencyKey makes a retried create return the first result.
	IdempotencyKey string
	ParentID       IssueID
	Title          string
	Body           string
	Design         string
	Acceptance     string
	Notes          string
	Status         Status
	Priority       *Priority
	Type           IssueType
	Assignee       string
	Owner          string
	DueAt          *time.Time
	DeferUntil     *time.Time
	ExpiresAt      *time.Time
	Ephemeral      bool
	Pinned         bool
	Template       bool
	Metadata       json.RawMessage
	Labels         []string
	// Account is empty to inherit the parent's, or the project default.
	Account string
}

// IssuePatch lists the fields UpdateIssue changes; nil leaves a field alone.
// An empty string clears Assignee, Owner, ParentID and Account; a zero time clears a
// timestamp; Metadata "null" clears it. Status cannot be set to or from
// closed here: use CloseIssue and ReopenIssue.
type IssuePatch struct {
	ParentID   *IssueID
	Title      *string
	Body       *string
	Design     *string
	Acceptance *string
	Notes      *string
	Status     *Status
	Priority   *Priority
	Type       *IssueType
	Assignee   *string
	Owner      *string
	DueAt      *time.Time
	DeferUntil *time.Time
	ExpiresAt  *time.Time
	Ephemeral  *bool
	Pinned     *bool
	Template   *bool
	Metadata   json.RawMessage
	Account    *string
}

// Dep is a dependency edge: From depends on To.
type Dep struct {
	From      IssueID   `json:"from"`
	To        IssueID   `json:"to"`
	Type      DepType   `json:"type"`
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
	// Metadata is the edge's JSON metadata, such as a waits-for gate.
	Metadata json.RawMessage `json:"metadata,omitempty"`
}

// CommentKind distinguishes notes with a role from plain comments.
type CommentKind string

// Comment kinds.
const (
	// CommentPlain is an ordinary comment.
	CommentPlain CommentKind = ""
	// CommentHandoff is a handoff note: what the next person to work on
	// the issue needs to know. [Store.LastHandoff] returns the latest.
	CommentHandoff CommentKind = "handoff"
)

// Comment is an append-only note on an issue.
type Comment struct {
	ID        string      `json:"id"`
	Issue     IssueID     `json:"issue"`
	Author    string      `json:"author"`
	Session   string      `json:"session"`
	Kind      CommentKind `json:"kind,omitempty"`
	Body      string      `json:"body"`
	CreatedAt time.Time   `json:"created_at"`
}

// Event is one entry in the gapless operation log. Before and After
// record what changed, as JSON (for an issue update, the changed fields'
// old and new values), and are absent when null; a long string in them is
// shortened (see [EventTextMax]). IdemKey is the idempotency key of the
// operation the event ends, if it had one.
type Event struct {
	Seq     int64           `json:"seq"`
	At      time.Time       `json:"at"`
	Actor   Actor           `json:"actor"`
	Op      Op              `json:"op"`
	Target  string          `json:"target"`
	Before  json.RawMessage `json:"before,omitempty"`
	After   json.RawMessage `json:"after,omitempty"`
	IdemKey string          `json:"idem_key,omitempty"`
}

// Filter selects issues for List. Empty fields match everything; Labels
// must all be present.
type Filter struct {
	Statuses   []Status
	Types      []IssueType
	Priorities []Priority
	Assignee   string
	ParentID   IssueID
	Labels     []string
	// Limit defaults to 50 and is capped at 500.
	Limit  int
	Cursor Cursor
}

// IssuePage is one page of List results. Next is empty on the last page.
type IssuePage struct {
	Issues []Issue
	Next   Cursor
}

// BlockedIssue is an unclosed issue held back by unclosed blockers, its
// own or an ancestor's (Via).
type BlockedIssue struct {
	Issue     Issue
	BlockedBy []IssueID
	// Via is the ancestor whose blockers apply; empty when they are direct.
	Via IssueID
}

var labelPattern = regexp.MustCompile(`^[^\s,\x00-\x1f]{1,64}$`)

// ValidLabel reports whether l may be stored as a label: 1-64 characters
// without spaces, commas, or control or bidirectional characters.
func ValidLabel(l string) bool { return labelPattern.MatchString(l) && safetext.ValidLine(l) }

// validLabel is ValidLabel as an error that wraps ErrInvalid.
func validLabel(l string) error {
	if !ValidLabel(l) {
		return fmt.Errorf("%w: label %q must be 1-64 characters without spaces, commas, or control or bidirectional characters", ErrInvalid, l)
	}
	return nil
}
