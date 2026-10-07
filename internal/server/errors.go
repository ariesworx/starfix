package server

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

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
	var forbidden *store.ForbiddenError
	switch {
	case errors.Is(err, store.ErrNotFound):
		return proto.Errf(proto.CodeNotFound, "find the id with `sfx list`",
			strings.Replace(text, ": "+store.ErrNotFound.Error(), " not found", 1))

	case errors.As(err, &held):
		return s.held(ctx, held)

	case errors.As(err, &forbidden):
		return forbiddenErr(forbidden)

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

	case errors.Is(err, store.ErrStatusInProgress):
		return proto.Errf(proto.CodeInvalid, fmt.Sprintf("take it with `sfx start %s`, which claims it", id),
			"status in_progress is set only by start")

	case errors.Is(err, store.ErrInvalid):
		switch {
		case strings.Contains(text, " is claimed by "):
			return proto.Errf(proto.CodeInvalid,
				fmt.Sprintf("finish it, or let it go with `sfx handoff %s --release` first; only the holder or an admin can", id),
				strings.TrimPrefix(text, store.ErrInvalid.Error()+": "))
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
// the command that ticks them; an admin may also force a close. An
// update that drops open items is told to settle them first.
func unmetErr(op string, e *store.AcceptanceError) *proto.Error {
	nums := make([]string, len(e.Open))
	for i, n := range e.Open {
		nums[i] = strconv.Itoa(n)
	}
	if e.Dropped {
		return proto.Errf(proto.CodeAcceptance,
			fmt.Sprintf("tick them with `sfx accept %s %s`, or waive each with `sfx accept %s N --waive REASON`, then edit the text",
				e.ID, strings.Join(nums, " "), e.ID), e.Error())
	}
	fix := fmt.Sprintf("tick what is met with `sfx accept %s %s`, or waive an item with `sfx accept %s N --waive REASON`",
		e.ID, strings.Join(nums, " "), e.ID)
	if op == proto.OpClose {
		fix += "; a starfix admin can close it anyway with --force, on the record"
	}
	return proto.Errf(proto.CodeAcceptance, fix, e.Error())
}

// forbiddenErr refuses a change to an issue another principal holds,
// naming the holder and the ways forward, or an admin-only action.
func forbiddenErr(e *store.ForbiddenError) *proto.Error {
	if e.Action != "" {
		return proto.Errf(proto.CodeForbidden,
			fmt.Sprintf("tick or waive the open items with `sfx accept %s N`, or ask a starfix admin to run it", e.ID),
			fmt.Sprintf("%s is for starfix admins", e.Action))
	}
	h := e.Holder
	return proto.Errf(proto.CodeForbidden,
		fmt.Sprintf("ask %s to hand it off (`sfx handoff %s --release`), wait for the lease to run out, or ask a starfix admin; `sfx comment %s` works on any issue",
			h.Principal, e.ID, e.ID),
		fmt.Sprintf("%s is held by %s (session %s) until %s; only the holder or an admin may change it",
			e.ID, h.Principal, h.Session, e.Until.UTC().Format(time.RFC3339)))
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

// held explains a refused start: another session of the caller's own
// principal holds the issue, which --take overrides, or another principal
// does, and the next ready issue is named instead (design §12 item 5).
func (s *Server) held(ctx context.Context, h *store.HeldError) *proto.Error {
	if h.Own {
		return proto.Errf(proto.CodeConflict,
			fmt.Sprintf("take it over with `sfx start %s --take` only if that session has stopped; it loses the claim", h.ID),
			fmt.Sprintf("%s is held by your session %s", h.ID, h.Session))
	}
	msg := fmt.Sprintf("%s is in progress by %s", h.ID, h.By)
	next, err := s.cfg.Store.Ready(ctx, 1)
	if err != nil || len(next) == 0 {
		return proto.Errf(proto.CodeConflict, fmt.Sprintf("leave it to %s; nothing else is ready, so see `sfx blocked`", h.By),
			msg+"; nothing else is ready")
	}
	return proto.Errf(proto.CodeConflict, fmt.Sprintf("take that one with `sfx start %s`", next[0].ID),
		fmt.Sprintf("%s; next ready: %s", msg, next[0].ID))
}
