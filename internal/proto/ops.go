package proto

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

// Operations. Each names its argument and result types.
const (
	OpCreate   = "create"    // CreateArgs → CreateResult
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
	OpComments = "comments"  // PageArgs → CommentsResult
	OpHistory  = "history"   // PageArgs → HistoryResult
	OpStart    = "start"     // StartArgs → StartResult
	OpFinish   = "finish"    // FinishArgs → FinishResult
	OpHandoff  = "handoff"   // HandoffArgs → WriteResult
	OpDigest   = "digest"    // DigestArgs → DigestResult
	OpRenew    = "renew"     // RenewArgs → ClaimsResult (protocol 2)
	OpWho      = "who"       // WhoArgs → WhoResult (protocol 2)
	OpAccept   = "accept"    // AcceptArgs → AcceptResult (protocol 2)
	OpUsage    = "usage"     // UsageArgs → UsageResult (protocol 3)
)

// WhoArgs selects the agents seen within Since, a duration such as 5m,
// 2h or 1d (empty takes 5m, at most 7d), at most Limit of them (0 takes
// the server's default, 100; at most 500).
type WhoArgs struct {
	Since string `json:"since,omitempty"`
	Limit int    `json:"limit,omitempty"`
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
// Now. More counts the agents seen but left out by the limit.
type WhoResult struct {
	Now    time.Time `json:"now"`
	Agents []Agent   `json:"agents"`
	More   int       `json:"more,omitempty"`
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
	// Account is the issue's own account, empty when it inherits one;
	// ShowResult.Usage has the one that applies.
	Account string `json:"account,omitempty"`
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
// Idem, an idempotency key, makes a retry return the first result.
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
	// Account is a code name for the issue's time and tokens; empty
	// inherits the parent's, then the server's default (protocol 3).
	Account string `json:"account,omitempty"`
}

// WriteResult is what every issue write returns (design §9).
type WriteResult struct {
	ID  string `json:"id"`
	Rev int64  `json:"rev"`
}

// CreateResult is create's WriteResult and up to three similar closed
// issues, best first (protocol 2).
type CreateResult struct {
	WriteResult
	Similar []Summary `json:"similar,omitempty"`
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
	// Handoff is the issue's latest handoff note and its fields, if any.
	Handoff *Handoff `json:"handoff,omitempty"`
	// Items are the acceptance criteria and their state (protocol 2).
	Items []AcceptanceItem `json:"items,omitempty"`
	// Similar are up to three similar closed issues, best first
	// (protocol 2).
	Similar []Summary `json:"similar,omitempty"`
	// DepsMore counts the edges left out of Deps, which holds at most
	// MaxShowDeps (protocol 2).
	DepsMore int `json:"deps_more,omitempty"`
	// Usage is the time and tokens attributed to the issue and the account
	// they report against (protocol 3).
	Usage *IssueUsage `json:"usage,omitempty"`
}

// Tokens are token counts. A count left out is unknown, which is not 0.
// CacheWrite1h is the part of CacheWrite written with a one-hour
// lifetime.
type Tokens struct {
	Input        *int64 `json:"input,omitempty"`
	Output       *int64 `json:"output,omitempty"`
	CacheWrite   *int64 `json:"cache_write,omitempty"`
	CacheWrite1h *int64 `json:"cache_write_1h,omitempty"`
	CacheRead    *int64 `json:"cache_read,omitempty"`
}

// ModelTokens are one model's tokens. A count is left out when no record
// reported it.
type ModelTokens struct {
	Model string `json:"model"`
	Tokens
}

// MaxUsageModels is the most models show and digest list; the rest are
// summed into one entry named OtherModels, which no model can be named,
// since model names hold no parentheses.
const (
	MaxUsageModels = 20
	OtherModels    = "(other)"
)

// IssueUsage is what is attributed to one issue: HeldSeconds under
// claims, and tokens by model. Split says part of the tokens came from
// records shared by time with other issues, or with time nothing was
// held. Account is the account that applies: the issue's own, the
// nearest ancestor's (AccountFrom names it), or the server's default.
// Capped says more records matched than the server reads, so the tokens
// are a lower bound.
type IssueUsage struct {
	Account     string        `json:"account"`
	AccountFrom string        `json:"account_from,omitempty"`
	HeldSeconds int64         `json:"held_seconds"`
	Split       bool          `json:"split,omitempty"`
	Models      []ModelTokens `json:"models,omitempty"`
	Capped      bool          `json:"capped,omitempty"`
}

// UsageRecord is what a harness reported for one request, or for a turn
// or session ("granularity": request, turn or session). RequestID is the
// harness's id for it: the server keeps one record per session and id,
// so sending it again changes nothing. At is the time the source gives;
// SpanStart starts a turn's or session's span, at most 7 days before At.
type UsageRecord struct {
	Harness     string     `json:"harness"`
	RequestID   string     `json:"request_id"`
	Model       string     `json:"model"`
	At          time.Time  `json:"at"`
	Granularity string     `json:"granularity"`
	SpanStart   *time.Time `json:"span_start,omitempty"`
	Tokens
}

// UsageArgs reports usage records for the caller's session; the
// principal, session and machine come from the connection.
type UsageArgs struct {
	Records []UsageRecord `json:"records"`
}

// UsageResult counts the records stored and those already stored.
type UsageResult struct {
	Added      int `json:"added"`
	Duplicates int `json:"duplicates"`
}

// MaxShowDeps is the most edges show returns; MaxBlockers the most
// blockers blocked lists for one issue.
const (
	MaxShowDeps = 200
	MaxBlockers = 50
)

// AcceptanceItem is one acceptance criterion. State is empty while open,
// "ticked" or "waived" (with Reason). By and At say who set it and when;
// an item ticked in the acceptance text itself has neither.
type AcceptanceItem struct {
	N      int        `json:"n"`
	Text   string     `json:"text"`
	State  string     `json:"state,omitempty"`
	Reason string     `json:"reason,omitempty"`
	By     string     `json:"by,omitempty"`
	At     *time.Time `json:"at,omitempty"`
}

// AcceptArgs ticks, unticks and waives an issue's acceptance items by
// number; Waive maps a number to the reason it is waived.
type AcceptArgs struct {
	ID     string         `json:"id"`
	Tick   []int          `json:"tick,omitempty"`
	Untick []int          `json:"untick,omitempty"`
	Waive  map[int]string `json:"waive,omitempty"`
}

// AcceptResult lists the items and the numbers of those still open.
type AcceptResult struct {
	ID    string           `json:"id"`
	Items []AcceptanceItem `json:"items"`
	Open  []int            `json:"open,omitempty"`
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
	// More counts the blockers left out of BlockedBy, which holds at most
	// MaxBlockers (protocol 2).
	More int `json:"more,omitempty"`
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
	// Account sets the issue's account; empty clears it, to inherit
	// (protocol 3).
	Account *string `json:"account,omitempty"`
}

// CloseArgs closes an issue. Rev 0 skips the revision check: close wins
// over concurrent edits (design §8). Force closes despite acceptance
// items neither ticked nor waived, and records them in the event.
type CloseArgs struct {
	ID     string `json:"id"`
	Rev    int64  `json:"rev,omitempty"`
	Reason string `json:"reason,omitempty"`
	Force  bool   `json:"force,omitempty"`
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

// CommentArgs adds a comment. Idem, an idempotency key, makes a retry
// return the first comment.
type CommentArgs struct {
	ID   string `json:"id"`
	Body string `json:"body"`
	Idem string `json:"idem,omitempty"`
}

// CommentResult names the new comment.
type CommentResult struct {
	ID string `json:"id"`
}

// IDArgs names one issue.
type IDArgs struct {
	ID string `json:"id"`
}

// PageArgs reads an issue's comments or history a page at a time, newest
// page first: at most Limit entries (0 takes the server's default, 100;
// at most 500, and about 1 MiB of text) from before the cursor Before,
// which is a result's Earlier; empty starts at the newest (protocol 2).
type PageArgs struct {
	ID     string `json:"id"`
	Limit  int    `json:"limit,omitempty"`
	Before string `json:"before,omitempty"`
}

// CommentsResult lists one page of comments, oldest first. Earlier is
// the cursor of the page before it, empty when there is none; Total
// counts all the issue's comments (protocol 2).
type CommentsResult struct {
	Comments []Comment `json:"comments"`
	Earlier  string    `json:"earlier,omitempty"`
	Total    int       `json:"total,omitempty"`
}

// HistoryResult lists one page of events, oldest first, as
// CommentsResult.
type HistoryResult struct {
	Events  []Event `json:"events"`
	Earlier string  `json:"earlier,omitempty"`
	Total   int     `json:"total,omitempty"`
}

// StartArgs takes an issue: the one named, or the top ready one. Lease is
// how long the claim lasts unless renewed, as a duration such as 15m or 8h
// (1m to 24h); empty takes the server's default, 15m. Take takes over a
// live claim another session of the caller's own principal holds; without
// it such a start is refused.
type StartArgs struct {
	ID    string `json:"id,omitempty"`
	Lease string `json:"lease,omitempty"`
	Take  bool   `json:"take,omitempty"`
}

// StartResult is the issue taken, in full, its latest handoff note with
// its fields, the claim and the acceptance items (protocol 2).
type StartResult struct {
	Issue   Issue            `json:"issue"`
	Handoff *Handoff         `json:"handoff,omitempty"`
	Claim   *Claim           `json:"claim,omitempty"`
	Items   []AcceptanceItem `json:"items,omitempty"`
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
// takes 15m; at most 24h). All renews every session's claims, not only
// this one's, for up to 7d; the server accepts it only from CLISession, a
// person's own terminal.
type RenewArgs struct {
	Lease string `json:"lease,omitempty"`
	All   bool   `json:"all,omitempty"`
}

// CLISession is the session of a person's own `sfx` commands, run without
// a harness session id: one per principal and machine.
const CLISession = "cli"

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
// discovered while doing it, in one transaction. The handoff's fields
// need its note; To also puts it in that principal's inbox. Ticked and
// Waived settle acceptance items first; the close is refused while any
// is left open. Idem, an idempotency key, makes a retry return the
// first result.
type FinishArgs struct {
	ID string `json:"id"`
	// Epoch, if set, must be the issue's current claim epoch.
	Epoch      int64          `json:"epoch,omitempty"`
	Reason     string         `json:"reason,omitempty"`
	Handoff    string         `json:"handoff,omitempty"`
	Discovered []Discovered   `json:"discovered,omitempty"`
	Ticked     []int          `json:"ticked,omitempty"`
	Waived     map[int]string `json:"waived,omitempty"`
	Idem       string         `json:"idem,omitempty"`
	HandoffFields
}

// FinishResult is the closed issue's revision and the discovered issues'
// IDs, in the order given.
type FinishResult struct {
	ID      string   `json:"id"`
	Rev     int64    `json:"rev"`
	Created []string `json:"created,omitempty"`
}

// HandoffArgs records a handoff note and its fields. Release also lets
// the issue go, so another can start it; To also puts it in that
// principal's inbox. Idem, an idempotency key, makes a retry return the
// first result.
type HandoffArgs struct {
	ID string `json:"id"`
	// Epoch, if set, must be the issue's current claim epoch.
	Epoch   int64  `json:"epoch,omitempty"`
	Note    string `json:"note"`
	Release bool   `json:"release,omitempty"`
	Idem    string `json:"idem,omitempty"`
	HandoffFields
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
	// Usage totals the window's time and tokens (protocol 3).
	Usage *DigestUsage `json:"usage,omitempty"`
	// Truncated: a total is a lower bound, or items were left out to fit
	// a budget.
	Truncated bool `json:"truncated,omitempty"`
}

// DigestUsage is a digest window's time and tokens: HeldSeconds that
// issues were held within it, the tokens reported in it by model, and the
// part of those no issue was held for (Unattributed). A label filter
// keeps only labeled issues' time and tokens.
type DigestUsage struct {
	HeldSeconds  int64         `json:"held_seconds"`
	Models       []ModelTokens `json:"models,omitempty"`
	Unattributed []ModelTokens `json:"unattributed,omitempty"`
	// Split is set when a record in the window was split by time, among
	// issues or with time none was held, so the parts are estimates.
	Split bool `json:"split,omitempty"`
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

// TokenCount formats a token count compactly for people and agents: 950,
// 12.3k, 1.5M, 2B, cut rather than rounded.
func TokenCount(n int64) string {
	for _, u := range []struct {
		div    int64
		suffix string
	}{{1e9, "B"}, {1e6, "M"}, {1e3, "k"}} {
		if n >= u.div {
			return strconv.FormatFloat(float64(n/(u.div/10))/10, 'f', -1, 64) + u.suffix
		}
	}
	return strconv.FormatInt(n, 10)
}

// Empty is the result of writes that have nothing to report.
type Empty struct{}
