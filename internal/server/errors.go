package server

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/store"
)

// command is the CLI spelling of an op, for fix lines.
func command(op string) string { return strings.ReplaceAll(op, ".", " ") }

// mapErr turns a store error into a typed protocol error whose message
// names the cause and whose fix names the next action. id and rev are the
// request's target and expected revision, when it has them.
func (s *Server) mapErr(ctx context.Context, op, id string, rev int64, err error) *proto.Error {
	text := err.Error()
	switch {
	case errors.Is(err, store.ErrNotFound):
		return proto.Errf(proto.CodeNotFound, "find the id with `sfx list`",
			strings.Replace(text, ": "+store.ErrNotFound.Error(), " not found", 1))

	case errors.Is(err, store.ErrConflict):
		return s.conflict(ctx, id, rev)

	case errors.Is(err, store.ErrExists):
		return proto.Errf(proto.CodeExists, "omit --id to have one generated, or read it with `sfx show "+id+"`",
			strings.Replace(text, ": "+store.ErrExists.Error(), " already exists", 1))

	case errors.Is(err, store.ErrCycle):
		return proto.Errf(proto.CodeCycle, "remove an edge with `sfx dep rm`, or choose another parent",
			strings.TrimSuffix(text, ": "+store.ErrCycle.Error())+", so this would make a cycle")

	case errors.Is(err, store.ErrInvalid):
		switch {
		case strings.HasSuffix(text, " is closed; reopen it first"):
			return proto.Errf(proto.CodeInvalid, fmt.Sprintf("reopen it with `sfx reopen %s`", id),
				strings.TrimSuffix(text, "; reopen it first"))
		case strings.HasSuffix(text, " is already closed"):
			return proto.Errf(proto.CodeInvalid, "nothing to do", text)
		case strings.HasSuffix(text, " is not closed"):
			return proto.Errf(proto.CodeInvalid, "nothing to do", text)
		}
		return proto.Errf(proto.CodeInvalid, fmt.Sprintf("correct it and retry; `sfx %s -h` lists the options", command(op)), text)

	case errors.Is(err, store.ErrSchemaTooNew):
		s.cfg.Logger.Error("schema too new", "op", op, "err", err)
		return proto.Errf(proto.CodeUnavailable, "the server admin should run the starfixd release that migrated it",
			"the database was migrated by a newer starfixd")

	case errors.Is(err, context.DeadlineExceeded):
		return proto.Errf(proto.CodeUnavailable, "retry; if it persists, the server admin should check the starfixd log",
			fmt.Sprintf("the server timed out on %s", op))
	}
	s.cfg.Logger.Error("request failed", "op", op, "err", err)
	return proto.Errf(proto.CodeUnavailable, "retry; if it persists, the server admin should check the starfixd log",
		fmt.Sprintf("the server could not complete %s", op))
}

// conflict explains a failed compare-and-swap: who moved the issue to
// which revision, or that concurrent writers kept winning.
func (s *Server) conflict(ctx context.Context, id string, rev int64) *proto.Error {
	reread := fmt.Sprintf("re-read with `sfx show %s`", id)
	if id == "" || rev < 1 {
		return proto.Errf(proto.CodeConflict, "retry", "the change lost to concurrent writes")
	}
	cur, err := s.cfg.Store.GetIssue(ctx, store.IssueID(id))
	if err != nil || int64(cur.Rev) == rev {
		return proto.Errf(proto.CodeConflict, "retry", fmt.Sprintf("%s kept changing under concurrent writes", id))
	}
	// Only issue.* events move rev; labels, deps and comments do not.
	by := ""
	if evs, err := s.cfg.Store.History(ctx, store.IssueID(id)); err == nil {
		for i := len(evs) - 1; i >= 0; i-- {
			if strings.HasPrefix(string(evs[i].Op), "issue.") {
				by = " by " + evs[i].Actor.Principal
				break
			}
		}
	}
	return proto.Errf(proto.CodeConflict, reread,
		fmt.Sprintf("%s changed since rev %d (now rev %d%s)", id, rev, cur.Rev, by))
}
