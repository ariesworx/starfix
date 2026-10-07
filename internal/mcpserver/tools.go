package mcpserver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/ariesworx/starfix/internal/proto"
)

// Values the server accepts. The input schemas carry them as enums, so a
// client sees the choices and the SDK refuses anything else before a
// request is sent.
var (
	issueTypes   = []any{"task", "bug", "feature", "epic", "chore"}
	openStatuses = []any{"open", "in_progress", "blocked", "deferred"}
	allStatuses  = append(append([]any{}, openStatuses...), "closed")
	depTypes     = []any{"blocks", "conditional-blocks", "waits-for", "related", "discovered-from", "duplicates", "supersedes"}
	actions      = []any{"add", "rm"}
)

// Inputs. Field tags are the schema's descriptions; keep them terse, since
// every agent pays for them in every session.

// IDIn names one issue.
type IDIn struct {
	ID string `json:"id"`
}

// ShowIn reads one issue.
type ShowIn struct {
	ID   string `json:"id"`
	Full bool   `json:"full,omitempty" jsonschema:"all text; default cuts long fields"`
}

// LimitIn bounds a list.
type LimitIn struct {
	Limit int `json:"limit,omitempty" jsonschema:"default 10"`
}

// ListIn filters issues.
type ListIn struct {
	Status   []string `json:"status,omitempty" jsonschema:"default: all but closed"`
	Type     []string `json:"type,omitempty"`
	Priority []int    `json:"priority,omitempty" jsonschema:"0 is highest"`
	Assignee string   `json:"assignee,omitempty"`
	Parent   string   `json:"parent,omitempty"`
	Labels   []string `json:"labels,omitempty" jsonschema:"must have all"`
	Limit    int      `json:"limit,omitempty" jsonschema:"default 10"`
	Cursor   string   `json:"cursor,omitempty" jsonschema:"next from the previous page"`
}

// CreateIn creates an issue.
type CreateIn struct {
	Title      string   `json:"title"`
	Body       string   `json:"body,omitempty"`
	Type       string   `json:"type,omitempty" jsonschema:"default task"`
	Priority   *int     `json:"priority,omitempty" jsonschema:"0 is highest; default 2"`
	Parent     string   `json:"parent,omitempty"`
	Labels     []string `json:"labels,omitempty"`
	Assignee   string   `json:"assignee,omitempty"`
	Design     string   `json:"design,omitempty"`
	Acceptance string   `json:"acceptance,omitempty"`

	idem string // the same on every attempt of one call
}

// prepare gives the call its idempotency key before the first attempt, so
// a retry on a new connection returns the first attempt's issue instead of
// creating a second.
func (in *CreateIn) prepare() error {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Errorf("idempotency key: %w", err)
	}
	in.idem = "mcp-" + hex.EncodeToString(b[:])
	return nil
}

// UpdateIn changes the fields given.
type UpdateIn struct {
	ID         string  `json:"id"`
	Rev        int64   `json:"rev,omitempty" jsonschema:"rev you read; refused if stale. Default current"`
	Title      *string `json:"title,omitempty"`
	Body       *string `json:"body,omitempty"`
	Design     *string `json:"design,omitempty"`
	Acceptance *string `json:"acceptance,omitempty"`
	Notes      *string `json:"notes,omitempty"`
	Status     *string `json:"status,omitempty" jsonschema:"to close, use close"`
	Priority   *int    `json:"priority,omitempty"`
	Type       *string `json:"type,omitempty"`
	Assignee   *string `json:"assignee,omitempty" jsonschema:"empty clears"`
	Owner      *string `json:"owner,omitempty" jsonschema:"empty clears"`
	Parent     *string `json:"parent,omitempty" jsonschema:"empty clears"`
}

// CloseIn closes an issue.
type CloseIn struct {
	ID     string `json:"id"`
	Reason string `json:"reason,omitempty" jsonschema:"what was done"`
	Rev    int64  `json:"rev,omitempty" jsonschema:"refused if changed since"`
}

// ReopenIn reopens an issue.
type ReopenIn struct {
	ID  string `json:"id"`
	Rev int64  `json:"rev,omitempty" jsonschema:"refused if changed since"`
}

// DepIn adds or removes an edge.
type DepIn struct {
	Action    string `json:"action"`
	ID        string `json:"id" jsonschema:"the issue that depends"`
	DependsOn string `json:"depends_on"`
	Type      string `json:"type,omitempty" jsonschema:"default blocks"`
}

// LabelIn adds or removes labels.
type LabelIn struct {
	Action string   `json:"action"`
	ID     string   `json:"id"`
	Labels []string `json:"labels"`
}

// CommentIn adds a comment.
type CommentIn struct {
	ID   string `json:"id"`
	Body string `json:"body"`
}

// PageIn names an issue and bounds a list of its records.
type PageIn struct {
	ID    string `json:"id"`
	Limit int    `json:"limit,omitempty" jsonschema:"newest N; default 10"`
}

// Outputs.

// Ref is the result of a write that has no revision.
type Ref struct {
	ID string `json:"id"`
}

// Issues is a list result. Next continues a list; More says ready or
// blocked had more than fit.
type Issues struct {
	Issues []proto.Summary `json:"issues"`
	Next   string          `json:"next,omitempty"`
	More   bool            `json:"more,omitempty"`
}

// Blocked is the blocked result.
type Blocked struct {
	Issues []proto.BlockedIssue `json:"issues"`
	More   bool                 `json:"more,omitempty"`
}

// Issue is the compact form of show: no timestamps or authorship, and
// edges as "ID type".
type Issue struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Status      string   `json:"status"`
	Priority    int      `json:"priority"`
	Type        string   `json:"type"`
	Rev         int64    `json:"rev"`
	Parent      string   `json:"parent,omitempty"`
	Assignee    string   `json:"assignee,omitempty"`
	Owner       string   `json:"owner,omitempty"`
	Labels      []string `json:"labels,omitempty"`
	Body        string   `json:"body,omitempty"`
	Design      string   `json:"design,omitempty"`
	Acceptance  string   `json:"acceptance,omitempty"`
	Notes       string   `json:"notes,omitempty"`
	CloseReason string   `json:"close_reason,omitempty"`
	DependsOn   []string `json:"depends_on,omitempty"`
	NeededBy    []string `json:"needed_by,omitempty"`
	// Truncated: long text was cut; full: true returns more.
	Truncated bool `json:"truncated,omitempty"`
}

// Comment is one comment, compact.
type Comment struct {
	Author string `json:"author"`
	At     string `json:"at"`
	Body   string `json:"body"`
}

// Comments lists an issue's newest comments, oldest first. Omitted counts
// the older ones left out.
type Comments struct {
	Comments []Comment `json:"comments"`
	Omitted  int       `json:"omitted,omitempty"`
}

// Event is one history entry, compact.
type Event struct {
	Seq     int64  `json:"seq"`
	At      string `json:"at"`
	By      string `json:"by"`
	Op      string `json:"op"`
	Changed string `json:"changed,omitempty"`
}

// History lists an issue's newest events, oldest first.
type History struct {
	Events  []Event `json:"events"`
	Omitted int     `json:"omitted,omitempty"`
}

func (s *Server) register() {
	add(s, tool{name: "prime", desc: "Start here: your in-progress issues, top ready work, notices.", ann: readOnly, retry: true},
		func(ctx context.Context, c Conn, _ struct{}) (*Prime, error) {
			return BuildPrime(ctx, c, s.opts.Version)
		})
	add(s, tool{name: "ready", desc: "Open issues nothing blocks, best first.", ann: readOnly, retry: true},
		func(ctx context.Context, c Conn, in LimitIn) (Issues, error) {
			var r proto.ListResult
			err := c.Call(ctx, proto.OpReady, proto.LimitArgs{Limit: orDefault(in.Limit, DefaultLimit)}, &r)
			out := Issues{Issues: r.Issues}
			for size(out) > MaxResultTokens && len(out.Issues) > 1 {
				out.Issues, out.More = out.Issues[:len(out.Issues)-1], true
			}
			return out, err
		})
	add(s, tool{name: "blocked", desc: "Issues held back, with their blockers.", ann: readOnly, retry: true},
		func(ctx context.Context, c Conn, in LimitIn) (Blocked, error) {
			var r proto.BlockedResult
			err := c.Call(ctx, proto.OpBlocked, proto.LimitArgs{Limit: orDefault(in.Limit, DefaultLimit)}, &r)
			out := Blocked{Issues: r.Issues}
			for size(out) > MaxResultTokens && len(out.Issues) > 1 {
				out.Issues, out.More = out.Issues[:len(out.Issues)-1], true
			}
			return out, err
		})
	add(s, tool{name: "list", desc: "Issues matching filters.", ann: readOnly, retry: true,
		enums: enums{"status": allStatuses, "type": issueTypes}},
		func(ctx context.Context, c Conn, in ListIn) (Issues, error) { return list(ctx, c, in) })
	add(s, tool{name: "show", desc: "One issue with its dependencies.", ann: readOnly, retry: true},
		func(ctx context.Context, c Conn, in ShowIn) (Issue, error) { return show(ctx, c, in) })
	add(s, tool{name: "create", desc: "Create an issue.", ann: write, retry: true, enums: enums{"type": issueTypes}},
		func(ctx context.Context, c Conn, in CreateIn) (proto.WriteResult, error) {
			args := proto.CreateArgs{Idem: in.idem, Title: in.Title, Body: in.Body, Type: in.Type, Priority: in.Priority,
				Parent: in.Parent, Labels: in.Labels, Assignee: in.Assignee, Design: in.Design, Acceptance: in.Acceptance}
			var out proto.WriteResult
			return out, c.Call(ctx, proto.OpCreate, args, &out)
		})
	add(s, tool{name: "update", desc: "Change the fields given.", ann: write,
		enums: enums{"status": openStatuses, "type": issueTypes}},
		func(ctx context.Context, c Conn, in UpdateIn) (proto.WriteResult, error) { return update(ctx, c, in) })
	add(s, tool{name: "close", desc: "Close a finished issue.", ann: write},
		func(ctx context.Context, c Conn, in CloseIn) (proto.WriteResult, error) {
			var out proto.WriteResult
			return out, c.Call(ctx, proto.OpClose, proto.CloseArgs{ID: in.ID, Rev: in.Rev, Reason: in.Reason}, &out)
		})
	add(s, tool{name: "reopen", desc: "Reopen a closed issue.", ann: write},
		func(ctx context.Context, c Conn, in ReopenIn) (proto.WriteResult, error) {
			var out proto.WriteResult
			return out, c.Call(ctx, proto.OpReopen, proto.ReopenArgs{ID: in.ID, Rev: in.Rev}, &out)
		})
	add(s, tool{name: "dep", desc: "Add or remove: id depends on depends_on. blocks keeps id out of ready.", ann: idem,
		enums: enums{"action": actions, "type": depTypes}},
		func(ctx context.Context, c Conn, in DepIn) (Ref, error) {
			op := proto.OpDepAdd
			if in.Action == "rm" {
				op = proto.OpDepRm
			}
			return Ref{ID: in.ID}, c.Call(ctx, op, proto.DepArgs{From: in.ID, To: in.DependsOn, Type: in.Type}, nil)
		})
	add(s, tool{name: "label", desc: "Add or remove labels.", ann: idem, enums: enums{"action": actions}},
		func(ctx context.Context, c Conn, in LabelIn) (Ref, error) {
			if len(in.Labels) == 0 {
				return Ref{}, proto.Errf(proto.CodeInvalid, "", "labels is empty")
			}
			op := proto.OpLabelAdd
			if in.Action == "rm" {
				op = proto.OpLabelRm
			}
			for _, l := range in.Labels {
				if err := c.Call(ctx, op, proto.LabelArgs{ID: in.ID, Label: l}, nil); err != nil {
					return Ref{}, err
				}
			}
			return Ref{ID: in.ID}, nil
		})
	add(s, tool{name: "comment", desc: "Comment: what changed, was decided or verified.", ann: write},
		func(ctx context.Context, c Conn, in CommentIn) (Ref, error) {
			var out proto.CommentResult
			err := c.Call(ctx, proto.OpComment, proto.CommentArgs{ID: in.ID, Body: in.Body}, &out)
			return Ref{ID: out.ID}, err
		})
	add(s, tool{name: "comments", desc: "An issue's newest comments.", ann: readOnly, retry: true},
		func(ctx context.Context, c Conn, in PageIn) (Comments, error) { return comments(ctx, c, in) })
	add(s, tool{name: "history", desc: "An issue's newest changes.", ann: readOnly, retry: true},
		func(ctx context.Context, c Conn, in PageIn) (History, error) { return history(ctx, c, in) })
}

func list(ctx context.Context, c Conn, in ListIn) (Issues, error) {
	args := proto.ListArgs{Status: in.Status, Type: in.Type, Priority: in.Priority, Assignee: in.Assignee,
		Parent: in.Parent, Labels: in.Labels, Limit: orDefault(in.Limit, DefaultLimit), Cursor: in.Cursor}
	if len(args.Status) == 0 {
		args.Status = []string{"open", "in_progress", "blocked", "deferred"}
	}
	for {
		var r proto.ListResult
		if err := c.Call(ctx, proto.OpList, args, &r); err != nil {
			return Issues{}, err
		}
		out := Issues{Issues: r.Issues, Next: r.Next}
		if size(out) <= MaxResultTokens || len(r.Issues) <= 1 {
			return out, nil
		}
		// Ask again for as many as fit, so the cursor resumes after the
		// last one returned.
		n := len(r.Issues) - 1
		for n > 1 && size(Issues{Issues: r.Issues[:n], Next: r.Next}) > MaxResultTokens {
			n--
		}
		args.Limit = n
	}
}

func show(ctx context.Context, c Conn, in ShowIn) (Issue, error) {
	var r proto.ShowResult
	if err := c.Call(ctx, proto.OpShow, proto.ShowArgs{ID: in.ID, Full: in.Full}, &r); err != nil {
		return Issue{}, err
	}
	is := r.Issue
	out := Issue{ID: is.ID, Title: is.Title, Status: is.Status, Priority: is.Priority, Type: is.Type, Rev: is.Rev,
		Parent: is.ParentID, Assignee: is.Assignee, Owner: is.Owner, Labels: is.Labels, Body: is.Body,
		Design: is.Design, Acceptance: is.Acceptance, Notes: is.Notes, CloseReason: is.CloseReason, Truncated: is.Truncated}
	for _, d := range r.Deps {
		if d.From == is.ID {
			out.DependsOn = append(out.DependsOn, d.To+" "+d.Type)
		} else {
			out.NeededBy = append(out.NeededBy, d.From+" "+d.Type)
		}
	}
	if fitTexts(func() bool { return size(out) <= MaxResultTokens }, &out.Body, &out.Design, &out.Acceptance, &out.Notes) {
		out.Truncated = true
	}
	for size(out) > MaxResultTokens && len(out.NeededBy) > 0 {
		out.NeededBy = out.NeededBy[:len(out.NeededBy)-1]
		out.Truncated = true
	}
	return out, nil
}

func update(ctx context.Context, c Conn, in UpdateIn) (proto.WriteResult, error) {
	args := proto.UpdateArgs{ID: in.ID, Rev: in.Rev, Title: in.Title, Body: in.Body, Design: in.Design,
		Acceptance: in.Acceptance, Notes: in.Notes, Status: in.Status, Priority: in.Priority, Type: in.Type,
		Assignee: in.Assignee, Owner: in.Owner, Parent: in.Parent}
	if args.Title == nil && args.Body == nil && args.Design == nil && args.Acceptance == nil && args.Notes == nil &&
		args.Status == nil && args.Priority == nil && args.Type == nil && args.Assignee == nil && args.Owner == nil &&
		args.Parent == nil {
		return proto.WriteResult{}, proto.Errf(proto.CodeInvalid, "", "nothing to update: give at least one field")
	}
	if args.Rev == 0 {
		var cur proto.ShowResult
		if err := c.Call(ctx, proto.OpShow, proto.ShowArgs{ID: in.ID}, &cur); err != nil {
			return proto.WriteResult{}, err
		}
		args.Rev = cur.Issue.Rev
	}
	var out proto.WriteResult
	return out, c.Call(ctx, proto.OpUpdate, args, &out)
}

// maxCommentBody is how much of each comment a list shows.
const maxCommentBody = 1000

func comments(ctx context.Context, c Conn, in PageIn) (Comments, error) {
	var r proto.CommentsResult
	if err := c.Call(ctx, proto.OpComments, proto.IDArgs{ID: in.ID}, &r); err != nil {
		return Comments{}, err
	}
	all := r.Comments
	keep := min(len(all), orDefault(in.Limit, DefaultLimit))
	out := Comments{Comments: []Comment{}, Omitted: len(all) - keep}
	for _, cm := range all[len(all)-keep:] {
		body, _ := cut(cm.Body, maxCommentBody)
		out.Comments = append(out.Comments, Comment{Author: cm.Author, At: stamp(cm.CreatedAt), Body: body})
	}
	for size(out) > MaxResultTokens && len(out.Comments) > 1 {
		out.Comments, out.Omitted = out.Comments[1:], out.Omitted+1
	}
	return out, nil
}

func history(ctx context.Context, c Conn, in PageIn) (History, error) {
	var r proto.HistoryResult
	if err := c.Call(ctx, proto.OpHistory, proto.IDArgs{ID: in.ID}, &r); err != nil {
		return History{}, err
	}
	all := r.Events
	keep := min(len(all), orDefault(in.Limit, DefaultLimit))
	out := History{Events: []Event{}, Omitted: len(all) - keep}
	for _, e := range all[len(all)-keep:] {
		changed, _ := cut(e.Changed(), 200)
		out.Events = append(out.Events, Event{Seq: e.Seq, At: stamp(e.At), By: e.Principal, Op: e.Op, Changed: changed})
	}
	for size(out) > MaxResultTokens && len(out.Events) > 1 {
		out.Events, out.Omitted = out.Events[1:], out.Omitted+1
	}
	return out, nil
}

func stamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04Z") }

func orDefault[T comparable](v, def T) T {
	var zero T
	if v == zero {
		return def
	}
	return v
}
