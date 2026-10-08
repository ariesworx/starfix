package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/store"
)

// Dispatch runs one operation for actor. args is the raw JSON of the op's
// *Args type; unknown fields are refused, since every field of every
// protocol version in range is known here. It returns the op's *Result,
// or a refusal: invalid for an unknown op or bad arguments, else the
// store's error as mapErr words it. Dispatch is safe for concurrent use.
// It applies no write rate limit, and refuses watch, which needs a
// connection to push to.
func (s *Server) Dispatch(ctx context.Context, actor store.Actor, op string, args json.RawMessage) (any, *proto.Error) {
	h, ok := handlers[op]
	if !ok {
		return nil, proto.Errf(proto.CodeInvalid, proto.FixUpgrade+" starfix to match the server, or check the operation name",
			fmt.Sprintf("unknown operation %q", op))
	}
	return h(ctx, s, actor, args)
}

// handler runs one op, as Dispatch does, for actor a.
type handler func(ctx context.Context, s *Server, a store.Actor, args json.RawMessage) (any, *proto.Error)

// handlers maps each op to its handler.
var handlers = map[string]handler{
	proto.OpCreate:   typed(create),
	proto.OpShow:     typed(show),
	proto.OpList:     typed(list),
	proto.OpReady:    typed(ready),
	proto.OpBlocked:  typed(blocked),
	proto.OpUpdate:   typed(update),
	proto.OpClose:    typed(closeIssue),
	proto.OpReopen:   typed(reopen),
	proto.OpDepAdd:   typed(depAdd),
	proto.OpDepRm:    typed(depRm),
	proto.OpLabelAdd: typed(labelAdd),
	proto.OpLabelRm:  typed(labelRm),
	proto.OpComment:  typed(comment),
	proto.OpComments: typed(comments),
	proto.OpHistory:  typed(history),
	proto.OpStart:    typed(start),
	proto.OpFinish:   typed(finish),
	proto.OpHandoff:  typed(handoff),
	proto.OpDigest:   typed(digest),
	proto.OpRenew:    typed(renew),
	proto.OpWho:      typed(who),
	proto.OpInbox:    typed(inbox),
	proto.OpAck:      typed(ack),
	proto.OpWatch:    typed(watchOp),
	proto.OpAccept:   typed(accept),
	proto.OpUsage:    typed(usage),
}

// typed decodes args strictly into A and calls fn.
func typed[A any](fn func(ctx context.Context, s *Server, a store.Actor, args A) (any, *proto.Error)) handler {
	return func(ctx context.Context, s *Server, a store.Actor, raw json.RawMessage) (any, *proto.Error) {
		var args A
		if len(raw) > 0 {
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&args); err != nil {
				return nil, proto.Errf(proto.CodeInvalid, proto.FixUpgrade+" starfix to match the server",
					fmt.Sprintf("bad arguments: %v", err))
			}
		}
		return fn(ctx, s, a, args)
	}
}

func create(ctx context.Context, s *Server, a store.Actor, in proto.CreateArgs) (any, *proto.Error) {
	n := store.NewIssue{
		ID: store.IssueID(in.ID), IdempotencyKey: in.Idem, ParentID: store.IssueID(in.Parent),
		Title: in.Title, Body: in.Body, Design: in.Design, Acceptance: in.Acceptance, Notes: in.Notes,
		Status: store.Status(in.Status), Type: store.IssueType(in.Type),
		Assignee: in.Assignee, Owner: in.Owner, Labels: in.Labels, Account: in.Account, Paths: in.Paths,
	}
	if in.Priority != nil {
		p := store.Priority(*in.Priority)
		n.Priority = &p
	}
	is, err := s.cfg.Store.CreateIssue(ctx, a, n)
	if err != nil {
		return nil, s.mapErr(ctx, proto.OpCreate, in.ID, 0, err)
	}
	return proto.CreateResult{WriteResult: proto.WriteResult{ID: string(is.ID), Rev: int64(is.Rev)},
		Similar: s.similar(ctx, is)}, nil
}

// similar lists the closed issues like is, for create and show. A failure
// is logged and lists none: the request itself succeeded.
func (s *Server) similar(ctx context.Context, is store.Issue) []proto.Summary {
	sim, err := s.cfg.Store.SimilarClosed(ctx, is.Title, is.ID, store.MaxSimilar)
	if err != nil {
		s.cfg.Logger.Error("similar issues", "issue", is.ID, "err", err)
		return nil
	}
	var out []proto.Summary
	for _, x := range sim {
		out = append(out, proto.Summary{ID: string(x.ID), Title: x.Title, Status: string(store.StatusClosed), Priority: int(x.Priority)})
	}
	return out
}

// items reads is's acceptance items for start and show.
func (s *Server) items(ctx context.Context, is store.Issue) ([]proto.AcceptanceItem, error) {
	items, err := s.cfg.Store.AcceptanceItems(ctx, is.ID)
	if err != nil {
		return nil, err
	}
	return wireItems(items), nil
}

func wireItems(items []store.AcceptanceItem) []proto.AcceptanceItem {
	var out []proto.AcceptanceItem
	for _, it := range items {
		out = append(out, proto.AcceptanceItem{N: it.N, Text: it.Text, State: string(it.State), Reason: it.Reason,
			By: it.By, At: it.At})
	}
	return out
}

// compactText is how much of each long text field compact show returns.
const compactText = 500

func show(ctx context.Context, s *Server, a store.Actor, in proto.ShowArgs) (any, *proto.Error) {
	id := store.IssueID(in.ID)
	is, err := s.cfg.Store.GetIssue(ctx, id)
	if err != nil {
		return nil, s.mapErr(ctx, proto.OpShow, in.ID, 0, err)
	}
	deps, err := s.cfg.Store.Deps(ctx, id)
	if err != nil {
		return nil, s.mapErr(ctx, proto.OpShow, in.ID, 0, err)
	}
	claim, err := s.cfg.Store.ClaimOf(ctx, id)
	if err != nil {
		return nil, s.mapErr(ctx, proto.OpShow, in.ID, 0, err)
	}
	h, err := s.cfg.Store.LastHandoff(ctx, id)
	if err != nil {
		return nil, s.mapErr(ctx, proto.OpShow, in.ID, 0, err)
	}
	items, err := s.items(ctx, is)
	if err != nil {
		return nil, s.mapErr(ctx, proto.OpShow, in.ID, 0, err)
	}
	u, err := s.cfg.Store.IssueUsage(ctx, id)
	if err != nil {
		return nil, s.mapErr(ctx, proto.OpShow, in.ID, 0, err)
	}
	files, err := s.cfg.Store.IssueFiles(ctx, a, id, proto.MaxShowPaths)
	if err != nil {
		return nil, s.mapErr(ctx, proto.OpShow, in.ID, 0, err)
	}
	out := proto.ShowResult{Issue: wireIssue(is), Items: items, Similar: s.similar(ctx, is), Usage: s.wireIssueUsage(u),
		Files: wireFiles(files)}
	if claim != nil {
		c := wireClaim(*claim)
		out.Claim = &c
	}
	if h != nil {
		w := wireHandoff(*h, a)
		out.Handoff = &w
	}
	if !in.Full {
		for _, f := range []*string{&out.Issue.Body, &out.Issue.Design, &out.Issue.Acceptance, &out.Issue.Notes} {
			if cut, ok := truncate(*f, compactText); ok {
				*f = cut
				out.Issue.Truncated = true
			}
		}
	}
	if len(deps) > proto.MaxShowDeps {
		deps, out.DepsMore = deps[:proto.MaxShowDeps], len(deps)-proto.MaxShowDeps
	}
	for _, d := range deps {
		out.Deps = append(out.Deps, proto.Dep{From: string(d.From), To: string(d.To), Type: string(d.Type),
			CreatedBy: d.CreatedBy, CreatedAt: d.CreatedAt.UTC()})
	}
	return out, nil
}

// truncate cuts s to at most n bytes, on a rune boundary, and appends "…".
// It reports whether it cut anything.
func truncate(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…", true
}

func list(ctx context.Context, s *Server, _ store.Actor, in proto.ListArgs) (any, *proto.Error) {
	f := store.Filter{Assignee: in.Assignee, ParentID: store.IssueID(in.Parent), Labels: in.Labels,
		Limit: in.Limit, Cursor: store.Cursor(in.Cursor)}
	for _, st := range in.Status {
		f.Statuses = append(f.Statuses, store.Status(st))
	}
	for _, t := range in.Type {
		f.Types = append(f.Types, store.IssueType(t))
	}
	for _, p := range in.Priority {
		f.Priorities = append(f.Priorities, store.Priority(p))
	}
	page, err := s.cfg.Store.List(ctx, f)
	if err != nil {
		return nil, s.mapErr(ctx, proto.OpList, "", 0, err)
	}
	return proto.ListResult{Issues: summaries(page.Issues), Next: string(page.Next)}, nil
}

func ready(ctx context.Context, s *Server, a store.Actor, in proto.LimitArgs) (any, *proto.Error) {
	rs, err := s.cfg.Store.Ready(ctx, a, in.Limit)
	if err != nil {
		return nil, s.mapErr(ctx, proto.OpReady, "", 0, err)
	}
	out := proto.ListResult{Issues: make([]proto.Summary, 0, len(rs))}
	for _, r := range rs {
		sm := summary(r.Issue)
		for _, id := range r.Overlaps {
			sm.Overlaps = append(sm.Overlaps, string(id))
		}
		out.Issues = append(out.Issues, sm)
	}
	return out, nil
}

// wireFiles is show's files, or nil when the issue has no paths and
// overlaps nothing.
func wireFiles(f store.Files) *proto.Files {
	if len(f.Paths) == 0 && len(f.Overlaps) == 0 {
		return nil
	}
	out := &proto.Files{More: f.More}
	for _, p := range f.Paths {
		out.Paths = append(out.Paths, proto.FilePath{Path: p.Path, Source: string(p.Source)})
	}
	for _, o := range f.Overlaps {
		out.Overlaps = append(out.Overlaps, proto.Overlap{ID: string(o.Issue), By: o.Holder.Principal, Session: o.Holder.Session})
	}
	return out
}

func blocked(ctx context.Context, s *Server, _ store.Actor, in proto.LimitArgs) (any, *proto.Error) {
	bs, err := s.cfg.Store.Blocked(ctx, in.Limit)
	if err != nil {
		return nil, s.mapErr(ctx, proto.OpBlocked, "", 0, err)
	}
	out := proto.BlockedResult{Issues: []proto.BlockedIssue{}}
	for _, b := range bs {
		bi := proto.BlockedIssue{Summary: summary(b.Issue), Via: string(b.Via), BlockedBy: []string{}}
		if len(b.BlockedBy) > proto.MaxBlockers {
			b.BlockedBy, bi.More = b.BlockedBy[:proto.MaxBlockers], len(b.BlockedBy)-proto.MaxBlockers
		}
		for _, id := range b.BlockedBy {
			bi.BlockedBy = append(bi.BlockedBy, string(id))
		}
		out.Issues = append(out.Issues, bi)
	}
	return out, nil
}

func update(ctx context.Context, s *Server, a store.Actor, in proto.UpdateArgs) (any, *proto.Error) {
	p := store.IssuePatch{Title: in.Title, Body: in.Body, Design: in.Design, Acceptance: in.Acceptance,
		Notes: in.Notes, Assignee: in.Assignee, Owner: in.Owner, Account: in.Account}
	if in.Status != nil {
		st := store.Status(*in.Status)
		p.Status = &st
	}
	if in.Priority != nil {
		pr := store.Priority(*in.Priority)
		p.Priority = &pr
	}
	if in.Type != nil {
		t := store.IssueType(*in.Type)
		p.Type = &t
	}
	if in.Parent != nil {
		pid := store.IssueID(*in.Parent)
		p.ParentID = &pid
	}
	p.Paths = in.Paths
	is, err := s.cfg.Store.UpdateIssue(ctx, a, store.IssueID(in.ID), store.Rev(in.Rev), p)
	if err != nil {
		return nil, s.mapErr(ctx, proto.OpUpdate, in.ID, in.Rev, err)
	}
	return proto.WriteResult{ID: string(is.ID), Rev: int64(is.Rev)}, nil
}

func closeIssue(ctx context.Context, s *Server, a store.Actor, in proto.CloseArgs) (any, *proto.Error) {
	closer := s.cfg.Store.CloseIssue
	if in.Force {
		closer = s.cfg.Store.ForceClose
	}
	is, err := closer(ctx, a, store.IssueID(in.ID), store.Rev(in.Rev), in.Reason)
	if err != nil {
		return nil, s.mapErr(ctx, proto.OpClose, in.ID, in.Rev, err)
	}
	return proto.WriteResult{ID: string(is.ID), Rev: int64(is.Rev)}, nil
}

func reopen(ctx context.Context, s *Server, a store.Actor, in proto.ReopenArgs) (any, *proto.Error) {
	is, err := s.cfg.Store.ReopenIssue(ctx, a, store.IssueID(in.ID), store.Rev(in.Rev))
	if err != nil {
		return nil, s.mapErr(ctx, proto.OpReopen, in.ID, in.Rev, err)
	}
	return proto.WriteResult{ID: string(is.ID), Rev: int64(is.Rev)}, nil
}

// depType is a request's dependency type, blocks when it gives none.
func depType(t string) store.DepType {
	if t == "" {
		return store.DepBlocks
	}
	return store.DepType(t)
}

func depAdd(ctx context.Context, s *Server, a store.Actor, in proto.DepArgs) (any, *proto.Error) {
	if err := s.cfg.Store.AddDep(ctx, a, store.IssueID(in.From), store.IssueID(in.To), depType(in.Type)); err != nil {
		return nil, s.mapErr(ctx, proto.OpDepAdd, in.From, 0, err)
	}
	return proto.Empty{}, nil
}

func depRm(ctx context.Context, s *Server, a store.Actor, in proto.DepArgs) (any, *proto.Error) {
	if err := s.cfg.Store.RemoveDep(ctx, a, store.IssueID(in.From), store.IssueID(in.To), depType(in.Type)); err != nil {
		return nil, s.mapErr(ctx, proto.OpDepRm, in.From, 0, err)
	}
	return proto.Empty{}, nil
}

func labelAdd(ctx context.Context, s *Server, a store.Actor, in proto.LabelArgs) (any, *proto.Error) {
	if err := s.cfg.Store.AddLabel(ctx, a, store.IssueID(in.ID), in.Label); err != nil {
		return nil, s.mapErr(ctx, proto.OpLabelAdd, in.ID, 0, err)
	}
	return proto.Empty{}, nil
}

func labelRm(ctx context.Context, s *Server, a store.Actor, in proto.LabelArgs) (any, *proto.Error) {
	if err := s.cfg.Store.RemoveLabel(ctx, a, store.IssueID(in.ID), in.Label); err != nil {
		return nil, s.mapErr(ctx, proto.OpLabelRm, in.ID, 0, err)
	}
	return proto.Empty{}, nil
}

func comment(ctx context.Context, s *Server, a store.Actor, in proto.CommentArgs) (any, *proto.Error) {
	c, err := s.cfg.Store.AddComment(ctx, a, store.IssueID(in.ID), in.Body, in.Idem)
	if err != nil {
		return nil, s.mapErr(ctx, proto.OpComment, in.ID, 0, err)
	}
	return proto.CommentResult{ID: c.ID}, nil
}

func comments(ctx context.Context, s *Server, _ store.Actor, in proto.PageArgs) (any, *proto.Error) {
	id := store.IssueID(in.ID)
	page, err := s.cfg.Store.CommentsPage(ctx, id, store.Cursor(in.Before), in.Limit)
	if err == nil && page.Total == 0 {
		_, err = s.cfg.Store.GetIssue(ctx, id) // no comments, or no issue?
	}
	if err != nil {
		return nil, s.mapErr(ctx, proto.OpComments, in.ID, 0, err)
	}
	out := proto.CommentsResult{Comments: []proto.Comment{}, Earlier: string(page.Earlier), Total: page.Total}
	for _, c := range page.Comments {
		out.Comments = append(out.Comments, wireComment(c))
	}
	return out, nil
}

func history(ctx context.Context, s *Server, a store.Actor, in proto.PageArgs) (any, *proto.Error) {
	page, err := s.cfg.Store.HistoryPage(ctx, store.IssueID(in.ID), store.Cursor(in.Before), in.Limit)
	if err == nil && page.Total == 0 {
		_, err = s.cfg.Store.GetIssue(ctx, store.IssueID(in.ID))
	}
	if err != nil {
		return nil, s.mapErr(ctx, proto.OpHistory, in.ID, 0, err)
	}
	out := proto.HistoryResult{Events: []proto.Event{}, Earlier: string(page.Earlier), Total: page.Total}
	for _, e := range page.Events {
		out.Events = append(out.Events, proto.Event{Seq: e.Seq, At: e.At.UTC(), Principal: e.Actor.Principal,
			Session: e.Actor.Session, Machine: e.Actor.Machine, Op: string(e.Op),
			Before: eventState(e, e.Before, a), After: eventState(e, e.After, a)})
	}
	return out, nil
}

// lease parses the lease a start or renew asks for. Empty means
// store.DefaultLease; anything else must be from store.MinLease to most,
// which is store.MaxClaimLease, or store.MaxLease for a renew with All.
func lease(op, s string, most time.Duration) (time.Duration, *proto.Error) {
	if s == "" {
		return store.DefaultLease, nil
	}
	upTo := "24h"
	if most == store.MaxLease {
		upTo = "7d"
	}
	d, err := proto.ParseDuration(s)
	if err != nil || d < store.MinLease || d > most {
		return 0, proto.Errf(proto.CodeInvalid, fmt.Sprintf("give a lease from 1m to %s; `sfx %s -h` lists the options", upTo, command(op)),
			fmt.Sprintf("lease %q is not a duration from 1m to %s", s, upTo))
	}
	return d, nil
}

func start(ctx context.Context, s *Server, a store.Actor, in proto.StartArgs) (any, *proto.Error) {
	d, perr := lease(proto.OpStart, in.Lease, store.MaxClaimLease)
	if perr != nil {
		return nil, perr
	}
	is, claim, err := s.cfg.Store.StartIssue(ctx, a, store.IssueID(in.ID), d, in.Take)
	if errors.Is(err, store.ErrNothingReady) {
		return nil, proto.Errf(proto.CodeNotFound, proto.FixSeeBlocked+" with `sfx blocked`, or create an issue",
			"nothing is ready to start")
	}
	if err != nil {
		return nil, s.mapErr(ctx, proto.OpStart, in.ID, 0, err)
	}
	c := wireClaim(claim)
	out := proto.StartResult{Issue: wireIssue(is), Claim: &c}
	// The issue is taken: a failed read of its items or its handoff is
	// logged rather than failing the start.
	if out.Items, err = s.items(ctx, is); err != nil {
		s.cfg.Logger.Error("read acceptance items", "issue", is.ID, "err", err)
	}
	h, err := s.cfg.Store.LastHandoff(ctx, is.ID)
	if err != nil {
		s.cfg.Logger.Error("read handoff", "issue", is.ID, "err", err)
	} else if h != nil {
		w := wireHandoff(*h, a)
		out.Handoff = &w
	}
	return out, nil
}

func finish(ctx context.Context, s *Server, a store.Actor, in proto.FinishArgs) (any, *proto.Error) {
	f := store.Finish{Reason: in.Reason, Handoff: storeHandoff(in.Handoff, in.HandoffFields), IdempotencyKey: in.Idem,
		Accept: store.Acceptance{Tick: in.Ticked, Waive: in.Waived}, Paths: in.Paths}
	for _, d := range in.Discovered {
		n := store.NewIssue{Title: d.Title, Type: store.IssueType(d.Type)}
		if d.Priority != nil {
			p := store.Priority(*d.Priority)
			n.Priority = &p
		}
		f.Discovered = append(f.Discovered, n)
	}
	is, ids, err := s.cfg.Store.FinishIssue(ctx, a, store.IssueID(in.ID), in.Epoch, f)
	if err != nil {
		return nil, s.mapErr(ctx, proto.OpFinish, in.ID, 0, err)
	}
	out := proto.FinishResult{ID: string(is.ID), Rev: int64(is.Rev)}
	for _, id := range ids {
		out.Created = append(out.Created, string(id))
	}
	return out, nil
}

func handoff(ctx context.Context, s *Server, a store.Actor, in proto.HandoffArgs) (any, *proto.Error) {
	is, err := s.cfg.Store.HandoffIssue(ctx, a, store.IssueID(in.ID), in.Epoch, storeHandoff(in.Note, in.HandoffFields), in.Release, in.Idem, in.Paths)
	if err != nil {
		return nil, s.mapErr(ctx, proto.OpHandoff, in.ID, 0, err)
	}
	return proto.WriteResult{ID: string(is.ID), Rev: int64(is.Rev)}, nil
}

func accept(ctx context.Context, s *Server, a store.Actor, in proto.AcceptArgs) (any, *proto.Error) {
	items, err := s.cfg.Store.Accept(ctx, a, store.IssueID(in.ID), store.Acceptance{Tick: in.Tick, Untick: in.Untick, Waive: in.Waive})
	if err != nil {
		return nil, s.mapErr(ctx, proto.OpAccept, in.ID, 0, err)
	}
	out := proto.AcceptResult{ID: in.ID, Items: wireItems(items)}
	for _, it := range items {
		if it.State == store.ItemOpen {
			out.Open = append(out.Open, it.N)
		}
	}
	return out, nil
}

func renew(ctx context.Context, s *Server, a store.Actor, in proto.RenewArgs) (any, *proto.Error) {
	most := store.MaxClaimLease
	if in.All {
		// Renewing every session's claims is a person's call before going
		// away (`sfx away`), not an agent's: only their own terminal's
		// session may ask (S-12).
		if a.Session != proto.CLISession {
			return nil, proto.Errf(proto.CodeInvalid,
				"run `sfx away` from your own terminal, outside an agent session; an agent renews only its own claims",
				fmt.Sprintf("session %s may renew only its own claims", a.Session))
		}
		most = store.MaxLease
	}
	d, perr := lease(proto.OpRenew, in.Lease, most)
	if perr != nil {
		return nil, perr
	}
	var paths map[store.IssueID][]string
	if len(in.Paths) > 0 {
		paths = make(map[store.IssueID][]string, len(in.Paths))
		for id, ps := range in.Paths {
			paths[store.IssueID(id)] = ps
		}
	}
	cs, err := s.cfg.Store.RenewClaims(ctx, a, d, in.All, paths)
	if err != nil {
		return nil, s.mapErr(ctx, proto.OpRenew, "", 0, err)
	}
	// A running agent renews every minute, which keeps it in `who`. The
	// harness was recorded at the handshake; empty keeps it.
	s.touch(ctx, s.cfg.Logger.With("principal", a.Principal, "session", a.Session), a, "")
	out := proto.ClaimsResult{Claims: []proto.Claim{}}
	for _, c := range cs {
		out.Claims = append(out.Claims, wireClaim(c))
	}
	return out, nil
}

func wireClaim(c store.Claim) proto.Claim {
	return proto.Claim{ID: string(c.Issue), By: c.Holder.Principal, Session: c.Holder.Session,
		Machine: c.Holder.Machine, Epoch: c.Epoch, ExpiresAt: c.ExpiresAt.UTC()}
}

func wireComment(c store.Comment) proto.Comment {
	return proto.Comment{ID: c.ID, Author: c.Author, Session: c.Session, Kind: string(c.Kind), Body: c.Body,
		CreatedAt: c.CreatedAt.UTC()}
}

func summary(is store.Issue) proto.Summary {
	return proto.Summary{ID: string(is.ID), Title: is.Title, Status: string(is.Status), Priority: int(is.Priority)}
}

func summaries(issues []store.Issue) []proto.Summary {
	out := make([]proto.Summary, 0, len(issues))
	for _, is := range issues {
		out = append(out, summary(is))
	}
	return out
}

func wireIssue(is store.Issue) proto.Issue {
	return proto.Issue{
		ID: string(is.ID), ParentID: string(is.ParentID), Title: is.Title, Body: is.Body, Design: is.Design,
		Acceptance: is.Acceptance, Notes: is.Notes, Status: string(is.Status), Priority: int(is.Priority),
		Type: string(is.Type), Assignee: is.Assignee, Owner: is.Owner, DueAt: is.DueAt, DeferUntil: is.DeferUntil,
		ExpiresAt: is.ExpiresAt, Ephemeral: is.Ephemeral, Pinned: is.Pinned, Template: is.Template,
		Metadata: is.Metadata, CloseReason: is.CloseReason, CreatedBy: is.CreatedBy, CreatedAt: is.CreatedAt.UTC(),
		UpdatedAt: is.UpdatedAt.UTC(), ClosedAt: is.ClosedAt, Rev: int64(is.Rev), Labels: is.Labels,
		Account: is.Account,
	}
}
