package proto

import (
	"encoding/json"
	"time"
)

// Operations. Each names its argument and result types.
const (
	OpCreate   = "create"    // CreateArgs → WriteResult
	OpShow     = "show"      // ShowArgs → ShowResult
	OpList     = "list"      // ListArgs → ListResult
	OpReady    = "ready"     // LimitArgs → ListResult
	OpBlocked  = "blocked"   // LimitArgs → BlockedResult
	OpUpdate   = "update"    // UpdateArgs → WriteResult
	OpClose    = "close"     // CloseArgs → WriteResult
	OpReopen   = "reopen"    // ReopenArgs → WriteResult
	OpDepAdd   = "dep.add"   // DepArgs → Empty
	OpDepRm    = "dep.rm"    // DepArgs → Empty
	OpLabelAdd = "label.add" // LabelArgs → Empty
	OpLabelRm  = "label.rm"  // LabelArgs → Empty
	OpComment  = "comment"   // CommentArgs → CommentResult
	OpComments = "comments"  // IDArgs → CommentsResult
	OpHistory  = "history"   // IDArgs → HistoryResult
)

// Issue is the full form of an issue, returned by show.
type Issue struct {
	ID          string          `json:"id"`
	ParentID    string          `json:"parent_id,omitempty"`
	Title       string          `json:"title"`
	Body        string          `json:"body,omitempty"`
	Design      string          `json:"design,omitempty"`
	Acceptance  string          `json:"acceptance,omitempty"`
	Notes       string          `json:"notes,omitempty"`
	Status      string          `json:"status"`
	Priority    int             `json:"priority"`
	Type        string          `json:"type"`
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
	Rev         int64           `json:"rev"`
	Labels      []string        `json:"labels,omitempty"`
	// Truncated is set when compact show cut a long text field.
	Truncated bool `json:"truncated,omitempty"`
}

// Summary is the compact form lists return (design §9).
type Summary struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Status   string `json:"status"`
	Priority int    `json:"priority"`
}

// Dep is a dependency edge: From depends on To.
type Dep struct {
	From      string    `json:"from"`
	To        string    `json:"to"`
	Type      string    `json:"type"`
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
}

// Comment is one comment on an issue.
type Comment struct {
	ID        string    `json:"id"`
	Author    string    `json:"author"`
	Session   string    `json:"session,omitempty"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

// Event is one entry of an issue's history.
type Event struct {
	Seq       int64           `json:"seq"`
	At        time.Time       `json:"at"`
	Principal string          `json:"principal"`
	Session   string          `json:"session,omitempty"`
	Machine   string          `json:"machine,omitempty"`
	Op        string          `json:"op"`
	Before    json.RawMessage `json:"before,omitempty"`
	After     json.RawMessage `json:"after,omitempty"`
}

// CreateArgs creates an issue. Zero values take the server's defaults.
type CreateArgs struct {
	ID         string   `json:"id,omitempty"`
	Idem       string   `json:"idem,omitempty"`
	Parent     string   `json:"parent,omitempty"`
	Title      string   `json:"title"`
	Body       string   `json:"body,omitempty"`
	Design     string   `json:"design,omitempty"`
	Acceptance string   `json:"acceptance,omitempty"`
	Notes      string   `json:"notes,omitempty"`
	Status     string   `json:"status,omitempty"`
	Priority   *int     `json:"priority,omitempty"`
	Type       string   `json:"type,omitempty"`
	Assignee   string   `json:"assignee,omitempty"`
	Owner      string   `json:"owner,omitempty"`
	Labels     []string `json:"labels,omitempty"`
}

// WriteResult is what every issue write returns (design §9).
type WriteResult struct {
	ID  string `json:"id"`
	Rev int64  `json:"rev"`
}

// ShowArgs reads one issue. Without Full, long text fields are cut.
type ShowArgs struct {
	ID   string `json:"id"`
	Full bool   `json:"full,omitempty"`
}

// ShowResult is an issue and its edges in both directions.
type ShowResult struct {
	Issue Issue `json:"issue"`
	Deps  []Dep `json:"deps,omitempty"`
}

// ListArgs filters issues. Empty fields match everything; all Labels must
// be present.
type ListArgs struct {
	Status   []string `json:"status,omitempty"`
	Type     []string `json:"type,omitempty"`
	Priority []int    `json:"priority,omitempty"`
	Assignee string   `json:"assignee,omitempty"`
	Parent   string   `json:"parent,omitempty"`
	Labels   []string `json:"labels,omitempty"`
	Limit    int      `json:"limit,omitempty"`
	Cursor   string   `json:"cursor,omitempty"`
}

// ListResult is one page of issues. Next is empty on the last page.
type ListResult struct {
	Issues []Summary `json:"issues"`
	Next   string    `json:"next,omitempty"`
}

// LimitArgs bounds ready and blocked.
type LimitArgs struct {
	Limit int `json:"limit,omitempty"`
}

// BlockedIssue is an issue held back by unclosed blockers, its own or an
// ancestor's (Via).
type BlockedIssue struct {
	Summary
	BlockedBy []string `json:"blocked_by"`
	Via       string   `json:"via,omitempty"`
}

// BlockedResult lists blocked issues.
type BlockedResult struct {
	Issues []BlockedIssue `json:"issues"`
}

// UpdateArgs changes the fields that are set. Rev is the revision the
// caller read; a stale one is a conflict. An empty string clears Assignee,
// Owner and Parent.
type UpdateArgs struct {
	ID         string  `json:"id"`
	Rev        int64   `json:"rev"`
	Title      *string `json:"title,omitempty"`
	Body       *string `json:"body,omitempty"`
	Design     *string `json:"design,omitempty"`
	Acceptance *string `json:"acceptance,omitempty"`
	Notes      *string `json:"notes,omitempty"`
	Status     *string `json:"status,omitempty"`
	Priority   *int    `json:"priority,omitempty"`
	Type       *string `json:"type,omitempty"`
	Assignee   *string `json:"assignee,omitempty"`
	Owner      *string `json:"owner,omitempty"`
	Parent     *string `json:"parent,omitempty"`
}

// CloseArgs closes an issue. Rev 0 skips the revision check: close wins
// over concurrent edits (design §8).
type CloseArgs struct {
	ID     string `json:"id"`
	Rev    int64  `json:"rev,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// ReopenArgs reopens a closed issue. Rev 0 skips the revision check.
type ReopenArgs struct {
	ID  string `json:"id"`
	Rev int64  `json:"rev,omitempty"`
}

// DepArgs names an edge: From depends on To. Type defaults to "blocks".
type DepArgs struct {
	From string `json:"from"`
	To   string `json:"to"`
	Type string `json:"type,omitempty"`
}

// LabelArgs names one label on one issue.
type LabelArgs struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// CommentArgs adds a comment.
type CommentArgs struct {
	ID   string `json:"id"`
	Body string `json:"body"`
}

// CommentResult names the new comment.
type CommentResult struct {
	ID string `json:"id"`
}

// IDArgs names one issue.
type IDArgs struct {
	ID string `json:"id"`
}

// CommentsResult lists comments, oldest first.
type CommentsResult struct {
	Comments []Comment `json:"comments"`
}

// HistoryResult lists events, oldest first.
type HistoryResult struct {
	Events []Event `json:"events"`
}

// Empty is the result of writes that have nothing to report.
type Empty struct{}
