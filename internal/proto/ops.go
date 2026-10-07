package proto

import (
	"encoding/json"
	"fmt"
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
	OpStart    = "start"     // StartArgs → StartResult
	OpFinish   = "finish"    // FinishArgs → FinishResult
	OpHandoff  = "handoff"   // HandoffArgs → WriteResult
	OpDigest   = "digest"    // DigestArgs → DigestResult
	OpRenew    = "renew"     // RenewArgs → ClaimsResult (protocol 2)
	OpWho      = "who"       // WhoArgs → WhoResult (protocol 2)
)

// WhoArgs selects the agents seen within Since, a duration such as 5m,
// 2h or 1d (empty takes 5m, at most 7d).
type WhoArgs struct {
	Since string `json:"since,omitempty"`
}

// Agent is one session in the registry and the issues it holds.
type Agent struct {
	Principal string    `json:"principal"`
	Session   string    `json:"session"`
	Machine   string    `json:"machine"`
	Harness   string    `json:"harness,omitempty"`
	Started   time.Time `json:"started"`
	LastSeen  time.Time `json:"last_seen"`
	Claims    []string  `json:"claims,omitempty"`
}

// WhoResult lists agents, most recently seen first, as of the server's
// Now.
type WhoResult struct {
	Now    time.Time `json:"now"`
	Agents []Agent   `json:"agents"`
}

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
	ID      string `json:"id"`
	Author  string `json:"author"`
	Session string `json:"session,omitempty"`
	// Kind is empty for a plain comment, "handoff" for a handoff note.
	Kind      string    `json:"kind,omitempty"`
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
	// Claim is the issue's active claim, if any (protocol 2).
	Claim *Claim `json:"claim,omitempty"`
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

// StartArgs takes an issue: the one named, or the top ready one. Lease is
// how long the claim lasts unless renewed, as a duration such as 15m, 8h
// or 2d (1m to 7d); empty takes the server's default, 15m.
type StartArgs struct {
	ID    string `json:"id,omitempty"`
	Lease string `json:"lease,omitempty"`
}

// StartResult is the issue taken, in full, its latest handoff note, and
// the claim (protocol 2).
type StartResult struct {
	Issue   Issue    `json:"issue"`
	Handoff *Comment `json:"handoff,omitempty"`
	Claim   *Claim   `json:"claim,omitempty"`
}

// Claim is a lease on an issue. Epoch rises each time a new holder takes
// it; finish and handoff may pass it to refuse acting on a claim since
// lost.
type Claim struct {
	ID        string    `json:"id"`
	By        string    `json:"by"`
	Session   string    `json:"session"`
	Machine   string    `json:"machine"`
	Epoch     int64     `json:"epoch"`
	ExpiresAt time.Time `json:"expires_at"`
}

// RenewArgs extends the caller's claims to at least now plus Lease (empty
// takes 15m). All renews every session's claims, not only this one's.
type RenewArgs struct {
	Lease string `json:"lease,omitempty"`
	All   bool   `json:"all,omitempty"`
}

// ClaimsResult lists claims, soonest to expire first.
type ClaimsResult struct {
	Claims []Claim `json:"claims"`
}

// Discovered is work found while doing an issue, filed by finish. Zero
// values take the server's defaults.
type Discovered struct {
	Title    string `json:"title"`
	Type     string `json:"type,omitempty"`
	Priority *int   `json:"priority,omitempty"`
}

// FinishArgs closes an issue with an optional handoff note and the work
// discovered while doing it, in one transaction.
type FinishArgs struct {
	ID string `json:"id"`
	// Epoch, if set, must be the issue's current claim epoch.
	Epoch      int64        `json:"epoch,omitempty"`
	Reason     string       `json:"reason,omitempty"`
	Handoff    string       `json:"handoff,omitempty"`
	Discovered []Discovered `json:"discovered,omitempty"`
}

// FinishResult is the closed issue's revision and the discovered issues'
// IDs, in the order given.
type FinishResult struct {
	ID      string   `json:"id"`
	Rev     int64    `json:"rev"`
	Created []string `json:"created,omitempty"`
}

// HandoffArgs records a handoff note. Release also lets the issue go, so
// another can start it.
type HandoffArgs struct {
	ID string `json:"id"`
	// Epoch, if set, must be the issue's current claim epoch.
	Epoch   int64  `json:"epoch,omitempty"`
	Note    string `json:"note"`
	Release bool   `json:"release,omitempty"`
}

// DigestArgs selects a digest. Since is an RFC 3339 time, a date
// (2006-01-02, midnight UTC) or a duration back from now such as 90m, 24h
// or 7d; empty means 24h. By keeps what one principal did; Label keeps
// issues with that label.
type DigestArgs struct {
	Since string `json:"since,omitempty"`
	By    string `json:"by,omitempty"`
	Label string `json:"label,omitempty"`
}

// DigestItem is one issue in a digest section. By and At are who made the
// section's event and when; for in progress, the holder and when it was
// taken; for stalled, the holder and the last event; for blocked, the
// assignee and no time.
type DigestItem struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Priority  int       `json:"priority"`
	By        string    `json:"by,omitempty"`
	At        time.Time `json:"at,omitzero"`
	Note      string    `json:"note,omitempty"`
	From      string    `json:"from,omitempty"`
	BlockedBy []string  `json:"blocked_by,omitempty"`
}

// DigestTotals counts everything each section matched; the sections list
// the first few. Events counts the events in the window.
type DigestTotals struct {
	Events     int `json:"events"`
	Closed     int `json:"closed"`
	Started    int `json:"started"`
	InProgress int `json:"in_progress"`
	Stalled    int `json:"stalled"`
	Blocked    int `json:"blocked"`
	HandedOff  int `json:"handed_off"`
	Created    int `json:"created"`
	Discovered int `json:"discovered"`
}

// DigestResult summarizes the window from Since to Until and the work in
// flight at Until. It is built from the event log and issue state; any
// narrative is the reader's to write.
type DigestResult struct {
	Since      time.Time    `json:"since"`
	Until      time.Time    `json:"until"`
	Totals     DigestTotals `json:"totals"`
	Closed     []DigestItem `json:"closed,omitempty"`
	Started    []DigestItem `json:"started,omitempty"`
	InProgress []DigestItem `json:"in_progress,omitempty"`
	Stalled    []DigestItem `json:"stalled,omitempty"`
	Blocked    []DigestItem `json:"blocked,omitempty"`
	HandedOff  []DigestItem `json:"handed_off,omitempty"`
	Created    []DigestItem `json:"created,omitempty"`
	Discovered []DigestItem `json:"discovered,omitempty"`
	// Truncated: a total is a lower bound, or items were left out to fit
	// a budget.
	Truncated bool `json:"truncated,omitempty"`
}

// Span formats a duration compactly for people and agents: 45m, 5h, 3d4h.
func Span(d time.Duration) string {
	d = d.Truncate(time.Minute)
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	days, hours := int(d/(24*time.Hour)), int(d%(24*time.Hour)/time.Hour)
	if hours == 0 || days >= 10 {
		return fmt.Sprintf("%dd", days)
	}
	return fmt.Sprintf("%dd%dh", days, hours)
}

// Empty is the result of writes that have nothing to report.
type Empty struct{}
