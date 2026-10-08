package server

import (
	"testing"

	"github.com/ariesworx/starfix/internal/proto"
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
