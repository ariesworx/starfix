package server

import (
	"strings"
	"testing"

	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/store"
)

// A change that does not fit the issue's state (store.StateError) is
// refused as invalid, with the store's message and a fix chosen by the
// reason, each pinned whole.
func TestDispatchStateErrors(t *testing.T) {
	s := newServer(t)
	held := mustCall[proto.WriteResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "held"})
	heldRev := mustCall[proto.StartResult](t, s, alice, proto.OpStart, proto.StartArgs{ID: held.ID}).Issue.Rev
	done := mustCall[proto.WriteResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "done"})
	doneRev := mustCall[proto.WriteResult](t, s, alice, proto.OpClose, proto.CloseArgs{ID: done.ID}).Rev
	open := mustCall[proto.WriteResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "open"})
	blocked := "blocked"
	tests := []struct {
		name     string
		op       string
		args     any
		msg, fix string
	}{
		{"status change of a claimed issue", proto.OpUpdate, proto.UpdateArgs{ID: held.ID, Rev: heldRev, Status: &blocked},
			"issue " + held.ID + " is claimed by alice/s-a; status and assignee change only through finish, close or a releasing handoff",
			"finish it, or let it go with `sfx handoff " + held.ID + " --release` first; only the holder or an admin can"},
		{"status change of a closed issue", proto.OpUpdate, proto.UpdateArgs{ID: done.ID, Rev: doneRev, Status: &blocked},
			"invalid input: issue " + done.ID + " is closed", "reopen it with `sfx reopen " + done.ID + "`"},
		{"close of a closed issue", proto.OpClose, proto.CloseArgs{ID: done.ID},
			"invalid input: issue " + done.ID + " is already closed", "nothing to do"},
		{"reopen of an open issue", proto.OpReopen, proto.ReopenArgs{ID: open.ID},
			"invalid input: issue " + open.ID + " is not closed", "nothing to do"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, perr := call[proto.Empty](t, s, alice, tc.op, tc.args)
			if perr == nil || perr.Code != proto.CodeInvalid || perr.Message != tc.msg || perr.Fix != tc.fix {
				t.Errorf("%s = %+v\nwant code %s, message %q and fix %q", tc.op, perr, proto.CodeInvalid, tc.msg, tc.fix)
			}
		})
	}
}

// The refusals that sfx mcp tells apart to name an agent's next step
// (internal/mcpserver's nextStep), each provoked here: every one has its
// code, and its proto.Fix phrase where sfx mcp reads the fix for it, so a
// reworded fix fails here rather than lose the agent its advice.
func TestFixPhrases(t *testing.T) {
	s := newServer(t)
	held := mustCall[proto.WriteResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "held"})
	heldRev := mustCall[proto.StartResult](t, s, alice, proto.OpStart, proto.StartArgs{ID: held.ID}).Issue.Rev
	mustCall[proto.WriteResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "ready"})
	done := mustCall[proto.WriteResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "done"})
	doneRev := mustCall[proto.WriteResult](t, s, alice, proto.OpClose, proto.CloseArgs{ID: done.ID}).Rev
	moved := mustCall[proto.WriteResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "moved"})
	title := "moved on"
	mustCall[proto.WriteResult](t, s, bob, proto.OpUpdate, proto.UpdateArgs{ID: moved.ID, Rev: moved.Rev, Title: &title})
	items := mustCall[proto.WriteResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "items", Acceptance: "- [ ] one\n- [ ] two"})
	// idle has one issue, which alice holds, so nothing else is ready.
	idle := newServer(t)
	only := mustCall[proto.WriteResult](t, idle, alice, proto.OpCreate, proto.CreateArgs{Title: "only"})
	mustCall[proto.StartResult](t, idle, alice, proto.OpStart, proto.StartArgs{ID: only.ID})

	alice2 := store.Actor{Principal: alice.Principal, Session: "s-a2", Machine: alice.Machine}
	blocked, inProgress, fewer := "blocked", "in_progress", "- [ ] one"
	tests := []struct {
		name   string
		s      *Server
		a      store.Actor
		op     string
		args   any
		code   proto.Code
		phrase string // the proto.Fix phrase sfx mcp reads in the fix
	}{
		{"nothing ready", idle, bob, proto.OpStart, proto.StartArgs{}, proto.CodeNotFound, proto.FixSeeBlocked},
		{"held, and another is ready", s, bob, proto.OpStart, proto.StartArgs{ID: held.ID}, proto.CodeConflict, proto.FixTakeNext},
		{"held by another session of yours", s, alice2, proto.OpStart, proto.StartArgs{ID: held.ID}, proto.CodeConflict, proto.FixTakeOver},
		{"held, and nothing else is ready", idle, bob, proto.OpStart, proto.StartArgs{ID: only.ID}, proto.CodeConflict, proto.FixLeaveIt},
		{"stale rev", s, alice, proto.OpUpdate, proto.UpdateArgs{ID: moved.ID, Rev: moved.Rev, Title: &title}, proto.CodeConflict, proto.FixReread},
		{"closed", s, alice, proto.OpUpdate, proto.UpdateArgs{ID: done.ID, Rev: doneRev, Status: &blocked}, proto.CodeInvalid, proto.FixReopen},
		{"in_progress by update", s, alice, proto.OpUpdate, proto.UpdateArgs{ID: items.ID, Rev: items.Rev, Status: &inProgress},
			proto.CodeInvalid, proto.FixStart},
		{"claimed", s, alice, proto.OpUpdate, proto.UpdateArgs{ID: held.ID, Rev: heldRev, Status: &blocked}, proto.CodeInvalid, proto.FixRelease},
		{"already closed", s, alice, proto.OpClose, proto.CloseArgs{ID: done.ID}, proto.CodeInvalid, proto.FixNothing},
		{"unknown operation", s, alice, "explode", struct{}{}, proto.CodeInvalid, proto.FixUpgrade},
		{"unknown argument", s, alice, proto.OpShow, struct {
			ID     string `json:"id"`
			Colour int    `json:"colour"`
		}{done.ID, 1}, proto.CodeInvalid, proto.FixUpgrade},
		{"edit drops open items", s, alice, proto.OpUpdate, proto.UpdateArgs{ID: items.ID, Rev: items.Rev, Acceptance: &fewer},
			proto.CodeAcceptance, proto.FixEditText},
		{"held by another principal", s, bob, proto.OpUpdate, proto.UpdateArgs{ID: held.ID, Rev: heldRev, Title: &title},
			proto.CodeForbidden, proto.FixAsk},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, perr := call[proto.Empty](t, tc.s, tc.a, tc.op, tc.args)
			if perr == nil || perr.Code != tc.code || !hasPhrase(perr.Fix, tc.phrase) {
				t.Errorf("%s = %+v, want code %s and a fix with %q where sfx mcp reads it", tc.op, perr, tc.code, tc.phrase)
			}
		})
	}
}

// hasPhrase reports whether fix has phrase where sfx mcp reads it:
// FixNothing as the whole fix, FixEditText at its end, and any other
// phrase at its start.
func hasPhrase(fix, phrase string) bool {
	switch phrase {
	case proto.FixNothing:
		return fix == phrase
	case proto.FixEditText:
		return strings.HasSuffix(fix, phrase)
	}
	return strings.HasPrefix(fix, phrase)
}
