package server

import (
	"slices"
	"strings"
	"testing"

	"github.com/ariesworx/starfix/internal/proto"
)

// A create-type request sent twice with one key is applied once; the same
// key on another request is refused, naming the key.
func TestDispatchIdempotency(t *testing.T) {
	s := newServer(t)
	in := proto.CreateArgs{Title: "once", Idem: "cli-0001"}
	first := mustCall[proto.CreateResult](t, s, alice, proto.OpCreate, in)
	if again := mustCall[proto.CreateResult](t, s, alice, proto.OpCreate, in); again.WriteResult != first.WriteResult {
		t.Errorf("replayed create = %+v, want %+v", again, first)
	}
	c := proto.CommentArgs{ID: first.ID, Body: "note", Idem: "cli-0002"}
	one := mustCall[proto.CommentResult](t, s, alice, proto.OpComment, c)
	if two := mustCall[proto.CommentResult](t, s, alice, proto.OpComment, c); two != one {
		t.Errorf("replayed comment = %+v, want %+v", two, one)
	}
	mustCall[proto.StartResult](t, s, alice, proto.OpStart, proto.StartArgs{ID: first.ID})
	h := proto.HandoffArgs{ID: first.ID, Note: "later", Idem: "cli-0003"}
	if a, b := mustCall[proto.WriteResult](t, s, alice, proto.OpHandoff, h), mustCall[proto.WriteResult](t, s, alice, proto.OpHandoff, h); a != b {
		t.Errorf("replayed handoff = %+v, want %+v", b, a)
	}
	f := proto.FinishArgs{ID: first.ID, Discovered: []proto.Discovered{{Title: "more"}}, Idem: "cli-0004"}
	fa := mustCall[proto.FinishResult](t, s, alice, proto.OpFinish, f)
	if fb := mustCall[proto.FinishResult](t, s, alice, proto.OpFinish, f); fb.ID != fa.ID || fb.Rev != fa.Rev || !slices.Equal(fb.Created, fa.Created) {
		t.Errorf("replayed finish = %+v, want %+v", fb, fa)
	}
	cs := mustCall[proto.CommentsResult](t, s, alice, proto.OpComments, proto.IDArgs{ID: first.ID})
	if len(cs.Comments) != 2 {
		t.Errorf("%d comments, want the comment and the handoff once each", len(cs.Comments))
	}

	_, perr := call[proto.CommentResult](t, s, alice, proto.OpComment, proto.CommentArgs{ID: first.ID, Body: "other", Idem: "cli-0002"})
	if perr == nil || perr.Code != proto.CodeConflict || !strings.Contains(perr.Message, `"cli-0002"`) || !strings.Contains(perr.Fix, "cli-0002") {
		t.Errorf("key reused for another request = %+v", perr)
	}
	_, perr = call[proto.CommentResult](t, s, alice, proto.OpComment, proto.CommentArgs{ID: first.ID, Body: "x", Idem: "no spaces"})
	if perr == nil || perr.Code != proto.CodeInvalid {
		t.Errorf("bad key = %+v", perr)
	}
}

// Acceptance items come back from start and show, are set with accept or
// finish, and hold close and finish until each is ticked or waived;
// close's force overrides.
func TestDispatchAcceptance(t *testing.T) {
	s := newServer(t)
	a := mustCall[proto.CreateResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "work", Acceptance: "- one\n- [x] two\n- three"})
	st := mustCall[proto.StartResult](t, s, alice, proto.OpStart, proto.StartArgs{ID: a.ID})
	if len(st.Items) != 3 || st.Items[1].State != "ticked" || st.Items[0].Text != "one" || st.Items[0].State != "" {
		t.Fatalf("start items = %+v", st.Items)
	}

	_, perr := call[proto.FinishResult](t, s, alice, proto.OpFinish, proto.FinishArgs{ID: a.ID})
	if perr == nil || perr.Code != proto.CodeAcceptance || !strings.Contains(perr.Message, "1, 3") ||
		!strings.Contains(perr.Fix, "sfx accept "+a.ID+" 1 3") {
		t.Fatalf("finish with open items = %+v", perr)
	}
	_, perr = call[proto.WriteResult](t, s, alice, proto.OpClose, proto.CloseArgs{ID: a.ID})
	if perr == nil || perr.Code != proto.CodeAcceptance || !strings.Contains(perr.Fix, "--force") {
		t.Fatalf("close with open items = %+v", perr)
	}

	acc := mustCall[proto.AcceptResult](t, s, alice, proto.OpAccept, proto.AcceptArgs{ID: a.ID, Tick: []int{1}})
	if acc.ID != a.ID || !slices.Equal(acc.Open, []int{3}) || acc.Items[0].By != "alice" {
		t.Fatalf("accept = %+v", acc)
	}
	_, perr = call[proto.AcceptResult](t, s, alice, proto.OpAccept, proto.AcceptArgs{ID: a.ID, Tick: []int{9}})
	if perr == nil || perr.Code != proto.CodeInvalid || !strings.Contains(perr.Message, "no item 9") {
		t.Fatalf("accept item 9 = %+v", perr)
	}
	mustCall[proto.FinishResult](t, s, alice, proto.OpFinish, proto.FinishArgs{ID: a.ID, Waived: map[int]string{3: "not needed"}})
	show := mustCall[proto.ShowResult](t, s, alice, proto.OpShow, proto.ShowArgs{ID: a.ID})
	if len(show.Items) != 3 || show.Items[2].State != "waived" || show.Items[2].Reason != "not needed" {
		t.Fatalf("show items = %+v", show.Items)
	}

	b := mustCall[proto.CreateResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "other", Acceptance: "must be fast"})
	if _, perr := call[proto.WriteResult](t, s, bob, proto.OpClose, proto.CloseArgs{ID: b.ID, Force: true}); perr == nil ||
		perr.Code != proto.CodeForbidden || perr.Message != "close --force is for starfix admins" {
		t.Fatalf("force close by bob = %+v", perr)
	}
	mustCall[proto.WriteResult](t, s, dana, proto.OpClose, proto.CloseArgs{ID: b.ID, Force: true})
}

// create and show name up to three similar closed issues.
func TestDispatchSimilar(t *testing.T) {
	s := newServer(t)
	old := mustCall[proto.CreateResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "Login fails with expired token"})
	mustCall[proto.WriteResult](t, s, alice, proto.OpClose, proto.CloseArgs{ID: old.ID})
	mustCall[proto.CreateResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "Dark mode"})

	created := mustCall[proto.CreateResult](t, s, bob, proto.OpCreate, proto.CreateArgs{Title: "Expired token breaks login"})
	if len(created.Similar) != 1 || created.Similar[0].ID != old.ID || created.Similar[0].Status != "closed" ||
		created.Similar[0].Title != "Login fails with expired token" {
		t.Fatalf("create similar = %+v", created.Similar)
	}
	show := mustCall[proto.ShowResult](t, s, bob, proto.OpShow, proto.ShowArgs{ID: created.ID})
	if len(show.Similar) != 1 || show.Similar[0].ID != old.ID {
		t.Fatalf("show similar = %+v", show.Similar)
	}
	if self := mustCall[proto.ShowResult](t, s, bob, proto.OpShow, proto.ShowArgs{ID: old.ID}); len(self.Similar) != 0 {
		t.Fatalf("a closed issue is not similar to itself: %+v", self.Similar)
	}
}
