package server

import (
	"slices"
	"testing"
	"time"

	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/store"
)

// events returns the issue events among ps, as "op issue".
func events(ps []proto.Push) []string {
	var out []string
	for _, p := range ps {
		if p.Event != nil {
			out = append(out, p.Event.Op+" "+p.Event.Issue)
		}
	}
	return out
}

// A watch with events is pushed every event committed on an issue, by
// anyone, as history records it but without its states; a plain watch is
// pushed none. Watching again without events stops them.
func TestPushEvents(t *testing.T) {
	s := newServer(t)
	b := dialPipe(t, s, bob)
	plain := dialPipe(t, s, store.Actor{Principal: "carol", Session: "s-c", Machine: "m"})
	if got := b.until(b.send(proto.OpWatch, proto.WatchArgs{Events: true})); len(got) != 0 {
		t.Fatalf("pushes before anything happened: %v", kinds(got))
	}
	plain.until(plain.send(proto.OpWatch, proto.WatchArgs{}))

	a := mustCall[proto.WriteResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "work"})
	mustCall[proto.StartResult](t, s, alice, proto.OpStart, proto.StartArgs{ID: a.ID})
	hist := mustCall[proto.HistoryResult](t, s, alice, proto.OpHistory, proto.PageArgs{ID: a.ID})

	got := b.until(b.send(proto.OpWho, proto.WhoArgs{}))
	var want []string
	for _, e := range hist.Events {
		want = append(want, e.Op+" "+a.ID)
	}
	if !slices.Equal(events(got), want) {
		t.Fatalf("pushed events = %v, want history's %v", events(got), want)
	}
	for i, p := range got {
		e, h := p.Event, hist.Events[i]
		if p.Op != proto.EvEvent || e.Seq != h.Seq || !e.At.Equal(h.At) || e.Principal != "alice" ||
			e.Session != alice.Session || len(e.Before)+len(e.After) != 0 {
			t.Errorf("push %d = %+v, want history's seq %d at %s by alice, with no states", i, e, h.Seq, h.At)
		}
	}
	if got := plain.until(plain.send(proto.OpWho, proto.WhoArgs{})); len(got) != 0 {
		t.Errorf("a watch without events was pushed %v", kinds(got))
	}

	b.until(b.send(proto.OpWatch, proto.WatchArgs{}))
	mustCall[proto.CommentResult](t, s, alice, proto.OpComment, proto.CommentArgs{ID: a.ID, Body: "quiet now"})
	if got := b.until(b.send(proto.OpWho, proto.WhoArgs{})); len(got) != 0 {
		t.Errorf("after watching without events, pushes = %v", kinds(got))
	}
}

// claims lists every live claim, longest held first, with its holder and
// when it was taken, and the server's clock; a limit leaves the rest
// counted in more.
func TestDispatchActiveClaims(t *testing.T) {
	s := newServer(t)
	one := mustCall[proto.WriteResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "one"})
	two := mustCall[proto.WriteResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "two"})
	mustCall[proto.CreateResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "free"})
	c1 := mustCall[proto.StartResult](t, s, alice, proto.OpStart, proto.StartArgs{ID: one.ID}).Claim
	c2 := mustCall[proto.StartResult](t, s, bob, proto.OpStart, proto.StartArgs{ID: two.ID}).Claim

	before := time.Now().Add(-time.Minute)
	got := mustCall[proto.ClaimsResult](t, s, bob, proto.OpClaims, proto.LimitArgs{})
	if got.Now.Before(before) || got.More != 0 {
		t.Errorf("claims now %s, more %d; want the server's clock and none more", got.Now, got.More)
	}
	ids := func(cs []proto.Claim) []string {
		var out []string
		for _, c := range cs {
			out = append(out, c.ID+" "+c.By+"/"+c.Session)
		}
		return out
	}
	if want := []string{one.ID + " alice/" + alice.Session, two.ID + " bob/" + bob.Session}; !slices.Equal(ids(got.Claims), want) {
		t.Fatalf("claims = %v, want %v", ids(got.Claims), want)
	}
	for i, c := range got.Claims {
		start := []*proto.Claim{c1, c2}[i]
		if c.ClaimedAt.IsZero() || c.ClaimedAt.After(c.ExpiresAt) || !c.ExpiresAt.Equal(start.ExpiresAt) || c.Epoch != start.Epoch {
			t.Errorf("claim %d = %+v, want start's %+v with when it was taken", i, c, start)
		}
	}
	if got := mustCall[proto.ClaimsResult](t, s, bob, proto.OpClaims, proto.LimitArgs{Limit: 1}); len(got.Claims) != 1 || got.More != 1 {
		t.Errorf("claims with limit 1 = %d claims, more %d; want 1 and 1", len(got.Claims), got.More)
	}
}
