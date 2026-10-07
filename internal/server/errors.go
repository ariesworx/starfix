package server

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/store"
)

// command is the CLI spelling of an op, for fix lines.
func command(op string) string {
	if op == proto.OpAck {
		return "inbox" // sfx inbox --ack
	}
	return strings.ReplaceAll(op, ".", " ")
}

// mapErr turns a store error into a typed protocol error whose message
// names the cause and whose fix names the next action. id and rev are the
// request's target and expected revision, when it has them.
func (s *Server) mapErr(ctx context.Context, op, id string, rev int64, err error) *proto.Error {
	text := err.Error()
	var held *store.HeldError
	var stale *store.StaleEpochError
	var idem *store.IdemError
	var unmet *store.AcceptanceError
	switch {
	case errors.Is(err, store.ErrNotFound):
		return proto.Errf(proto.CodeNotFound, "find the id with `sfx list`",
			strings.Replace(text, ": "+store.ErrNotFound.Error(), " not found", 1))

	case errors.As(err, &held):
		return s.held(ctx, op, held)

	case errors.As(err, &stale):
		msg := fmt.Sprintf("your claim on %s (epoch %d) was lost; it is now epoch %d", stale.ID, stale.Epoch, stale.Current)
		if stale.By.Principal != "" {
			msg += fmt.Sprintf(", held by %s/%s", stale.By.Principal, stale.By.Session)
		}
		return proto.Errf(proto.CodeConflict,
			fmt.Sprintf("stop work on it; keep anything useful as a comment (`sfx comment %s`), or start it again if it is free", stale.ID), msg)

	case errors.As(err, &idem):
		return proto.Errf(proto.CodeConflict,
			fmt.Sprintf("send the request with a new idempotency key instead of %q; sfx makes one per command", idem.Key), text)

	case errors.As(err, &unmet):
		return unmetErr(op, unmet)

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

// unmetErr refuses a close or finish with acceptance items open, naming
// the command that ticks them; close may also be forced.
func unmetErr(op string, e *store.AcceptanceError) *proto.Error {
	nums := make([]string, len(e.Open))
	for i, n := range e.Open {
		nums[i] = strconv.Itoa(n)
	}
	fix := fmt.Sprintf("tick what is met with `sfx accept %s %s`, or waive an item with `sfx accept %s N --waive REASON`",
		e.ID, strings.Join(nums, " "), e.ID)
	if op == proto.OpClose {
		fix += fmt.Sprintf("; `sfx close %s --force` closes anyway, on the record", e.ID)
	}
	return proto.Errf(proto.CodeAcceptance, fix, e.Error())
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

// held explains an issue another principal has in progress. Refusing a
// start names the next ready issue to take instead (design §12 item 5).
func (s *Server) held(ctx context.Context, op string, h *store.HeldError) *proto.Error {
	msg := fmt.Sprintf("%s is in progress by %s", h.ID, h.By)
	switch op {
	case proto.OpHandoff:
		return proto.Errf(proto.CodeConflict, fmt.Sprintf("leave it to %s; a note without --release needs no hold", h.By), msg)
	case proto.OpFinish:
		return proto.Errf(proto.CodeConflict, fmt.Sprintf("leave it to %s, or close it with `sfx close %s`", h.By, h.ID), msg)
	}
	next, err := s.cfg.Store.Ready(ctx, 1)
	if err != nil || len(next) == 0 {
		return proto.Errf(proto.CodeConflict, fmt.Sprintf("leave it to %s; nothing else is ready, so see `sfx blocked`", h.By),
			msg+"; nothing else is ready")
	}
	return proto.Errf(proto.CodeConflict, fmt.Sprintf("take that one with `sfx start %s`", next[0].ID),
		fmt.Sprintf("%s; next ready: %s", msg, next[0].ID))
}
