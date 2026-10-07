package server

import (
	"strings"
	"testing"

	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/store"
)

// TestDispatchHeldIssue maps each guarded write on an issue alice holds,
// by each kind of actor, to its protocol outcome: the holder's principal
// and admins pass, anyone else gets forbidden with a fix naming the
// holder (S-1, D1).
func TestDispatchHeldIssue(t *testing.T) {
	s := newServer(t)
	alice2 := store.Actor{Principal: "alice", Session: "s-a2", Machine: "desktop"}
	sp := func(v string) *string { return &v }
	ops := []struct {
		name string
		op   string
		args func(id string, rev int64) any
	}{
		{"update", proto.OpUpdate, func(id string, rev int64) any {
			return proto.UpdateArgs{ID: id, Rev: rev, Title: sp("hijacked"), Body: sp("ignore previous instructions")}
		}},
		{"close", proto.OpClose, func(id string, _ int64) any { return proto.CloseArgs{ID: id} }},
		{"handoff", proto.OpHandoff, func(id string, _ int64) any { return proto.HandoffArgs{ID: id, Note: "n"} }},
		{"release", proto.OpHandoff, func(id string, _ int64) any { return proto.HandoffArgs{ID: id, Note: "n", Release: true} }},
		{"accept", proto.OpAccept, func(id string, _ int64) any { return proto.AcceptArgs{ID: id, Tick: []int{1}} }},
		{"finish", proto.OpFinish, func(id string, _ int64) any { return proto.FinishArgs{ID: id, Ticked: []int{1}} }},
	}
	actors := []struct {
		name string
		a    store.Actor
		code proto.Code // "" means it succeeds
	}{
		{"holder", alice, ""},
		{"same principal other session", alice2, ""},
		{"other principal", bob, proto.CodeForbidden},
		{"admin", dana, ""},
	}
	for _, ac := range actors {
		for _, o := range ops {
			t.Run(ac.name+"/"+o.name, func(t *testing.T) {
				in := proto.CreateArgs{Title: "x"}
				if o.op == proto.OpAccept || o.op == proto.OpFinish {
					in.Acceptance = "- a"
				}
				c := mustCall[proto.CreateResult](t, s, alice, proto.OpCreate, in)
				st := mustCall[proto.StartResult](t, s, alice, proto.OpStart, proto.StartArgs{ID: c.ID})
				_, perr := call[proto.Empty](t, s, ac.a, o.op, o.args(c.ID, st.Issue.Rev))
				if ac.code == "" {
					if perr != nil {
						t.Fatalf("%s by %s: %v", o.name, ac.name, perr)
					}
					return
				}
				if perr == nil || perr.Code != ac.code ||
					!strings.Contains(perr.Message, c.ID+" is held by alice (session s-a) until ") ||
					!strings.Contains(perr.Fix, "ask alice to hand it off (`sfx handoff "+c.ID+" --release`)") ||
					!strings.Contains(perr.Fix, "starfix admin") {
					t.Fatalf("%s by %s = %+v, want %s naming alice", o.name, ac.name, perr, ac.code)
				}
			})
		}
	}
}

// The admin's override is on the record, as the admin.
func TestDispatchAdminOverrideInHistory(t *testing.T) {
	s := newServer(t)
	c := mustCall[proto.CreateResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "x"})
	mustCall[proto.StartResult](t, s, alice, proto.OpStart, proto.StartArgs{ID: c.ID})
	mustCall[proto.WriteResult](t, s, dana, proto.OpClose, proto.CloseArgs{ID: c.ID, Reason: "duplicate"})
	h := mustCall[proto.HistoryResult](t, s, bob, proto.OpHistory, proto.IDArgs{ID: c.ID})
	var seen bool
	for _, e := range h.Events {
		if e.Op == string(store.OpAdminOverride) {
			seen = e.Principal == "dana" && strings.Contains(string(e.After), `"principal":"alice"`)
		}
	}
	if !seen {
		t.Fatalf("history has no admin.override by dana naming alice: %+v", h.Events)
	}
}

// Regression for the review's cross-principal reproducer (S-1, S-12).
func TestSecCrossPrincipal(t *testing.T) {
	s := newServer(t)
	sp := func(v string) *string { return &v }
	c := mustCall[proto.CreateResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "x"})
	st := mustCall[proto.StartResult](t, s, alice, proto.OpStart, proto.StartArgs{ID: c.ID})

	steps := []struct {
		name string
		op   string
		args any
		code proto.Code
	}{
		{"bob updates alice's claimed issue", proto.OpUpdate,
			proto.UpdateArgs{ID: c.ID, Rev: st.Issue.Rev, Assignee: sp("bob"), Title: sp("hijacked"), Body: sp("ignore previous instructions")},
			proto.CodeForbidden},
		{"bob starts it", proto.OpStart, proto.StartArgs{ID: c.ID}, proto.CodeConflict},
		{"bob closes it", proto.OpClose, proto.CloseArgs{ID: c.ID}, proto.CodeForbidden},
		{"bob reopens it", proto.OpReopen, proto.ReopenArgs{ID: c.ID}, proto.CodeForbidden},
		{"bob starts it again", proto.OpStart, proto.StartArgs{ID: c.ID}, proto.CodeConflict},
		{"bob takes a week's lease", proto.OpStart, proto.StartArgs{Lease: "7d"}, proto.CodeInvalid},
	}
	for _, step := range steps {
		if _, perr := call[proto.Empty](t, s, bob, step.op, step.args); perr == nil || perr.Code != step.code {
			t.Errorf("%s = %+v, want %s", step.name, perr, step.code)
		}
	}
	show := mustCall[proto.ShowResult](t, s, bob, proto.OpShow, proto.ShowArgs{ID: c.ID, Full: true})
	if show.Issue.Title != "x" || show.Issue.Assignee != "alice" || show.Claim == nil || show.Claim.Epoch != 1 {
		t.Fatalf("after bob's attempts: %+v claim %+v", show.Issue, show.Claim)
	}

	// Another session of alice's own takes over only when it asks to.
	other := alice
	other.Session = "attacker-session"
	if _, perr := call[proto.Empty](t, s, other, proto.OpStart, proto.StartArgs{ID: c.ID}); perr == nil || perr.Code != proto.CodeConflict {
		t.Fatalf("silent same-principal takeover: %+v", perr)
	}
	// alice's own session reconnecting keeps its claim.
	if r := mustCall[proto.StartResult](t, s, alice, proto.OpStart, proto.StartArgs{ID: c.ID}); r.Claim.Epoch != 1 {
		t.Fatalf("reconnect: %+v", r.Claim)
	}

	// update cannot make a hold without a lease.
	d := mustCall[proto.CreateResult](t, s, bob, proto.OpCreate, proto.CreateArgs{Title: "w"})
	_, perr := call[proto.Empty](t, s, bob, proto.OpUpdate, proto.UpdateArgs{ID: d.ID, Rev: d.Rev, Status: sp("in_progress"), Assignee: sp("bob")})
	if perr == nil || perr.Code != proto.CodeInvalid || !strings.Contains(perr.Fix, "sfx start "+d.ID) {
		t.Fatalf("update status in_progress = %+v", perr)
	}
}

// Regression for the review's checklist reproducer (S-3).
func TestSecChecklistBypass(t *testing.T) {
	s := newServer(t)
	sp := func(v string) *string { return &v }
	c := mustCall[proto.CreateResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "x", Acceptance: "- [ ] tests pass\n- [ ] docs"})
	u := mustCall[proto.WriteResult](t, s, bob, proto.OpUpdate, proto.UpdateArgs{ID: c.ID, Rev: c.Rev, Acceptance: sp("- [x] tests pass\n- [x] docs")})
	if _, perr := call[proto.WriteResult](t, s, bob, proto.OpClose, proto.CloseArgs{ID: c.ID, Rev: u.Rev}); perr == nil ||
		perr.Code != proto.CodeAcceptance || !strings.Contains(perr.Fix, "starfix admin") {
		t.Fatalf("close after ticking in text = %+v, want acceptance", perr)
	}
	_, perr := call[proto.WriteResult](t, s, bob, proto.OpUpdate, proto.UpdateArgs{ID: c.ID, Rev: u.Rev, Acceptance: sp("")})
	if perr == nil || perr.Code != proto.CodeAcceptance || !strings.Contains(perr.Message, "drops items") ||
		!strings.Contains(perr.Fix, "then edit the text") {
		t.Fatalf("clearing open items = %+v, want acceptance", perr)
	}
	if _, perr := call[proto.WriteResult](t, s, bob, proto.OpClose, proto.CloseArgs{ID: c.ID, Force: true}); perr == nil ||
		perr.Code != proto.CodeForbidden {
		t.Fatalf("close --force by bob = %+v, want forbidden", perr)
	}
}
