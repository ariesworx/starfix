package server

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/store"
)

// startMemories is how many relevant memories start returns with the
// issue it takes.
const startMemories = 5

func remember(ctx context.Context, s *Server, a store.Actor, in proto.RememberArgs) (any, *proto.Error) {
	m, err := s.cfg.Store.Remember(ctx, a, store.NewMemory{
		Scope: store.Scope(in.Scope), Key: in.Key, Body: in.Body, Tags: in.Tags, Issue: store.IssueID(in.Issue),
		Pinned: in.Pinned, Rev: store.Rev(in.Rev), IdempotencyKey: in.Idem,
	})
	if err != nil {
		return nil, s.memoryErr(ctx, proto.OpRemember, in.Issue, err)
	}
	return proto.WriteResult{ID: m.ID, Rev: int64(m.Rev)}, nil
}

func recall(ctx context.Context, s *Server, a store.Actor, in proto.RecallArgs) (any, *proto.Error) {
	if in.Prime {
		if in.Scope != "" || in.Key != "" || in.Tag != "" || in.Text != "" {
			return nil, proto.Errf(proto.CodeInvalid, "send prime with only a limit, or recall without prime",
				"prime ranks every memory, so it takes no scope, key, tag or text")
		}
		ms, err := s.cfg.Store.PrimeMemories(ctx, a, in.Limit)
		if err != nil {
			return nil, s.memoryErr(ctx, proto.OpRecall, "", err)
		}
		return proto.RecallResult{Memories: wireMemories(ms)}, nil
	}
	ms, more, err := s.cfg.Store.Recall(ctx, a, store.MemoryQuery{Scope: store.Scope(in.Scope), Key: in.Key, Tag: in.Tag,
		Text: in.Text, Limit: in.Limit})
	if err != nil {
		return nil, s.memoryErr(ctx, proto.OpRecall, "", err)
	}
	return proto.RecallResult{Memories: wireMemories(ms), More: more}, nil
}

func forget(ctx context.Context, s *Server, a store.Actor, in proto.ForgetArgs) (any, *proto.Error) {
	m, err := s.cfg.Store.Forget(ctx, a, store.Scope(in.Scope), in.Key, store.Rev(in.Rev), in.Idem)
	if err != nil {
		return nil, s.memoryErr(ctx, proto.OpForget, "", err)
	}
	return proto.WriteResult{ID: m.ID, Rev: int64(m.Rev)}, nil
}

func pin(ctx context.Context, s *Server, a store.Actor, in proto.PinArgs) (any, *proto.Error) {
	m, err := s.cfg.Store.PinMemory(ctx, a, store.Scope(in.Scope), in.Key, in.Pinned)
	if err != nil {
		return nil, s.memoryErr(ctx, proto.OpPin, "", err)
	}
	return proto.WriteResult{ID: m.ID, Rev: int64(m.Rev)}, nil
}

// issueMemories lists the memories relevant to the issue start took. A
// failure is logged and lists none: the issue is taken.
func (s *Server) issueMemories(ctx context.Context, a store.Actor, id store.IssueID) []proto.Memory {
	ms, err := s.cfg.Store.IssueMemories(ctx, a, id, startMemories)
	if err != nil {
		s.cfg.Logger.Error("read memories", "issue", id, "err", err)
		return nil
	}
	return wireMemories(ms)
}

// memoryErr words the refusals particular to memories, and leaves the
// rest to mapErr. issue is the issue a remember links, so a missing one
// is named as mapErr names any missing issue.
func (s *Server) memoryErr(ctx context.Context, op, issue string, err error) *proto.Error {
	if c, ok := errors.AsType[*store.MemoryConflictError](err); ok {
		return proto.Errf(proto.CodeConflict,
			fmt.Sprintf(proto.FixRecall+" with `sfx recall --key %s --scope %s`, merge, and retry with --rev %d", c.Key, c.Scope, c.Current),
			c.Error())
	}
	if nf, ok := errors.AsType[*store.MemoryNotFoundError](err); ok {
		return proto.Errf(proto.CodeNotFound,
			fmt.Sprintf(proto.FixFindMemory+" with `sfx memories --scope %s`; remember it with no rev to create it", nf.Scope),
			nf.Error())
	}
	if se, ok := errors.AsType[*store.SecretError](err); ok {
		return proto.Errf(proto.CodeInvalid,
			proto.FixSecret+" and remember the rest; name where it is kept (a vault path or an environment variable) instead",
			invalidText(se))
	}
	if le, ok := errors.AsType[*store.MemoryLimitError](err); ok {
		return proto.Errf(proto.CodeInvalid,
			fmt.Sprintf(proto.FixForgetSome+" no longer needed with `sfx forget KEY --scope %s`, or ask the server admin to raise memories_per_scope under limits: in starfixd's config", le.Scope),
			invalidText(le))
	}
	return s.mapErr(ctx, op, issue, 0, err)
}

// invalidText is err's message without the store's ErrInvalid prefix.
func invalidText(err error) string {
	return strings.TrimPrefix(err.Error(), store.ErrInvalid.Error()+": ")
}

func wireMemories(ms []store.Memory) []proto.Memory {
	out := make([]proto.Memory, len(ms))
	for i, m := range ms {
		out[i] = proto.Memory{ID: m.ID, Scope: string(m.Scope), Key: m.Key, Body: m.Body, Tags: m.Tags, Issue: string(m.Issue),
			Pinned: m.Pinned, Author: m.Author, UpdatedBy: m.UpdatedBy, CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt,
			Rev: int64(m.Rev), Relevant: m.Relevant}
	}
	return out
}
