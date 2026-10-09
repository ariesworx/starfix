package server

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/store"
)

// command is the CLI spelling of an op, for fix lines.
func command(op string) string {
	switch op {
	case proto.OpAck:
		return "inbox" // sfx inbox --ack
	case proto.OpPriceSet:
		return "admin prices set"
	case proto.OpPrices:
		return "admin prices"
	case proto.OpHoursLog, proto.OpHoursDelete, proto.OpHours:
		return "log"
	case proto.OpPlanSet:
		return "admin plans set"
	case proto.OpPlans:
		return "admin plans"
	}
	return strings.ReplaceAll(op, ".", " ")
}

// mapErr turns a store error into a typed protocol error whose message
// names the cause and whose fix names the next action. id and rev are the
// request's target and expected revision, when it has them.
func (s *Server) mapErr(ctx context.Context, op, id string, rev int64, err error) *proto.Error {
	text := err.Error()
	var stale *store.StaleEpochError
	var idem *store.IdemError
	var unmet *store.AcceptanceError
	var forbidden *store.ForbiddenError
	var state *store.StateError
	var batch *store.UsageBatchError
	var priceCap *store.PriceLimitError
	var planCap *store.PlanLimitError
	var hoursCap *store.HoursLimitError
	var dayFull *store.HoursDayError
	switch {
	case errors.Is(err, store.ErrNotFound) && op == proto.OpHoursDelete:
		return proto.Errf(proto.CodeNotFound, "find the entry id with `sfx log --issue ID` or `sfx log --by PRINCIPAL`",
			strings.Replace(text, ": "+store.ErrNotFound.Error(), " not found", 1))

	case errors.Is(err, store.ErrNotFound):
		return proto.Errf(proto.CodeNotFound, "find the id with `sfx list`",
			strings.Replace(text, ": "+store.ErrNotFound.Error(), " not found", 1))

	case errors.As(err, &forbidden):
		return forbiddenErr(forbidden)

	case errors.As(err, &dayFull):
		left := int64((store.MaxHoursEntry - dayFull.Logged) / time.Second)
		return proto.Errf(proto.CodeInvalid, fmt.Sprintf("you can log at most %s more on %s; undo an entry with `sfx log --undo ID` to change it",
			proto.Hours(left), dayFull.On.Format(time.DateOnly)), text)

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
		return proto.Errf(proto.CodeInvalid, fmt.Sprintf(proto.FixStart+" `sfx start %s`, which claims it", id),
			"status in_progress is set only by start")

	case errors.Is(err, store.ErrBusy):
		fix := "retry later"
		if op == proto.OpUsage {
			fix = "retry later: the cap counts the last 24 hours; the server admin can raise usage_per_day under limits: in starfixd's config"
		}
		return proto.Errf(proto.CodeBusy, fix, strings.TrimPrefix(text, store.ErrBusy.Error()+": "))

	case errors.As(err, &batch):
		return proto.Errf(proto.CodeInvalid, fmt.Sprintf("send at most %d records per call; nothing from the batch was stored", batch.Max),
			strings.TrimPrefix(text, store.ErrInvalid.Error()+": "))

	case errors.As(err, &priceCap):
		return proto.Errf(proto.CodeInvalid,
			"replace an existing price (the same model and --from) instead, or ask the server admin to raise prices under limits: in starfixd's config",
			strings.TrimPrefix(text, store.ErrInvalid.Error()+": "))

	case errors.As(err, &planCap):
		return proto.Errf(proto.CodeInvalid,
			"replace an existing plan row (the same name and --from) instead, or ask the server admin to raise plans under limits: in starfixd's config",
			strings.TrimPrefix(text, store.ErrInvalid.Error()+": "))

	case errors.As(err, &hoursCap):
		return proto.Errf(proto.CodeInvalid,
			"combine entries: undo some with `sfx log --undo ENTRY` (`sfx log --by "+hoursCap.Principal+
				"` lists them) and log their sum, or ask the server admin to raise hours_per_day under limits: in starfixd's config",
			strings.TrimPrefix(text, store.ErrInvalid.Error()+": "))

	case errors.Is(err, store.ErrInvalid) && op == proto.OpUsage:
		return proto.Errf(proto.CodeInvalid, "correct or drop that record and send the batch again; nothing from the batch was stored",
			strings.TrimPrefix(text, store.ErrInvalid.Error()+": "))

	case errors.Is(err, store.ErrInvalid):
		if errors.As(err, &state) {
			msg := state.Error()
			switch state.Reason {
			case store.StateClaimed:
				return proto.Errf(proto.CodeInvalid,
					fmt.Sprintf(proto.FixRelease+" with `sfx handoff %s --release` first; only the holder or an admin can", id),
					strings.TrimPrefix(msg, store.ErrInvalid.Error()+": "))
			case store.StateClosed:
				return proto.Errf(proto.CodeInvalid, fmt.Sprintf(proto.FixReopen+" it with `sfx reopen %s`", id),
					strings.TrimSuffix(msg, "; reopen it first"))
			case store.StateAlreadyClosed, store.StateNotClosed:
				return proto.Errf(proto.CodeInvalid, proto.FixNothing, msg)
			}
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
			fmt.Sprintf("tick them with `sfx accept %s %s`, or waive each with `sfx accept %s N --waive REASON`, "+proto.FixEditText,
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
	if e.Action != "" && e.ID == "" {
		return proto.Errf(proto.CodeForbidden, "ask a starfix admin (admins: in starfixd's config) to run it",
			fmt.Sprintf("%s is for starfix admins", e.Action))
	}
	if e.Action != "" {
		return proto.Errf(proto.CodeForbidden,
			fmt.Sprintf("tick or waive the open items with `sfx accept %s N`, or ask a starfix admin to run it", e.ID),
			fmt.Sprintf("%s is for starfix admins", e.Action))
	}
	h := e.Holder
	return proto.Errf(proto.CodeForbidden,
		fmt.Sprintf(proto.FixAsk+"%s to hand it off (`sfx handoff %s --release`), wait for the lease to run out, or ask a starfix admin; `sfx comment %s` works on any issue",
			h.Principal, e.ID, e.ID),
		fmt.Sprintf("%s is held by %s (session %s) until %s; only the holder or an admin may change it",
			e.ID, h.Principal, h.Session, e.Until.UTC().Format(time.RFC3339)))
}

// conflict explains a failed compare-and-swap: who moved the issue to
// which revision, or that concurrent writers kept winning.
func (s *Server) conflict(ctx context.Context, id string, rev int64) *proto.Error {
	reread := fmt.Sprintf(proto.FixReread+" with `sfx show %s`", id)
	if id == "" || rev < 1 {
		return proto.Errf(proto.CodeConflict, "retry", "the change lost to concurrent writes")
	}
	cur, err := s.cfg.Store.GetIssue(ctx, store.IssueID(id))
	if err != nil || int64(cur.Rev) == rev {
		return proto.Errf(proto.CodeConflict, "retry", fmt.Sprintf("%s kept changing under concurrent writes", id))
	}
	// Only issue.* events move rev; labels, deps and comments do not.
	by := ""
	if page, err := s.cfg.Store.HistoryPage(ctx, store.IssueID(id), "", 50); err == nil {
		for _, ev := range slices.Backward(page.Events) {
			if strings.HasPrefix(string(ev.Op), "issue.") {
				by = " by " + ev.Actor.Principal
				break
			}
		}
	}
	return proto.Errf(proto.CodeConflict, reread,
		fmt.Sprintf("%s changed since rev %d (now rev %d%s)", id, rev, cur.Rev, by))
}

// held explains a refused start: another session of the caller's own
// principal holds the issue, which --take overrides, or another principal
// does, and the next ready issue is named instead (design §12 item 5), as
// the caller a's own ready would rank it. Only start returns a HeldError,
// so start calls held rather than mapErr, which has no caller.
func (s *Server) held(ctx context.Context, a store.Actor, h *store.HeldError) *proto.Error {
	if h.Own {
		return proto.Errf(proto.CodeConflict,
			fmt.Sprintf(proto.FixTakeOver+" with `sfx start %s --take` only if that session has stopped; it loses the claim", h.ID),
			fmt.Sprintf("%s is held by your session %s", h.ID, h.Session))
	}
	msg := fmt.Sprintf("%s is in progress by %s", h.ID, h.By)
	next, err := s.cfg.Store.Ready(ctx, a, 1)
	if err != nil || len(next) == 0 {
		return proto.Errf(proto.CodeConflict, fmt.Sprintf(proto.FixLeaveIt+" %s; nothing else is ready, so see `sfx blocked`", h.By),
			msg+"; nothing else is ready")
	}
	return proto.Errf(proto.CodeConflict, fmt.Sprintf(proto.FixTakeNext+" with `sfx start %s`", next[0].ID),
		fmt.Sprintf("%s; next ready: %s", msg, next[0].ID))
}
