package store

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
)

func TestStart(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	low := mustCreate(t, s, NewIssue{Title: "low", Priority: prio(P3)})
	high := mustCreate(t, s, NewIssue{Title: "high", Priority: prio(P1)})
	blocker := mustCreate(t, s, NewIssue{Title: "blocker", Priority: prio(P2)})
	top := mustCreate(t, s, NewIssue{Title: "blocked", Priority: prio(P0)})
	if err := s.AddDep(ctx, alice, top.ID, blocker.ID, DepBlocks); err != nil {
		t.Fatal(err)
	}

	// Without an id, start takes what ready lists first.
	got, _, err := s.StartIssue(ctx, alice, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != high.ID || got.Status != StatusInProgress || got.Assignee != "alice" || got.Rev != 2 {
		t.Fatalf("start: %+v", got)
	}

	// The same principal again: unchanged, no event.
	seq := lastSeq(t, s)
	again, _, err := s.StartIssue(ctx, alice, high.ID, 0)
	if err != nil || again.Rev != got.Rev {
		t.Fatalf("start again: %+v, %v", again, err)
	}
	if lastSeq(t, s) != seq {
		t.Error("starting a held issue again wrote an event")
	}

	// Another principal is refused, with who holds it.
	_, _, err = s.StartIssue(ctx, bob, high.ID, 0)
	var held *HeldError
	if !errors.As(err, &held) || held.By != "alice" || held.ID != high.ID || !errors.Is(err, ErrConflict) {
		t.Fatalf("start held: %v", err)
	}

	// By id, an issue ready does not list can be taken too.
	if got, _, err := s.StartIssue(ctx, bob, top.ID, 0); err != nil || got.Assignee != "bob" {
		t.Fatalf("start blocked by id: %+v, %v", got, err)
	}
	if got, _, err := s.StartIssue(ctx, bob, "", 0); err != nil || got.ID != blocker.ID {
		t.Fatalf("start next: %+v, %v", got, err)
	}
	if got, _, err := s.StartIssue(ctx, bob, "", 0); err != nil || got.ID != low.ID {
		t.Fatalf("start last: %+v, %v", got, err)
	}
	if _, _, err := s.StartIssue(ctx, bob, "", 0); !errors.Is(err, ErrNothingReady) || !errors.Is(err, ErrNotFound) {
		t.Fatalf("start with nothing ready: %v", err)
	}

	if _, err := s.CloseIssue(ctx, bob, low.ID, 0, ""); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		id   IssueID
		want error
	}{
		{"closed", low.ID, ErrInvalid},
		{"missing", "tst-nope", ErrNotFound},
		{"bad id", "Not An ID", ErrInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := s.StartIssue(ctx, alice, tc.id, 0); !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
	assertGapless(t, s)
}

// Two principals in two store processes start the same issue in
// overlapping transactions: exactly one takes it, and the other is told
// who did.
func TestStartRace(t *testing.T) {
	dsn := newDSN(t)
	s1 := openStore(t, dsn, Options{})
	s2 := openStore(t, dsn, Options{})
	is := mustCreate(t, s1, NewIssue{Title: "contested"})

	b := newBarrier(2)
	s1.beforeCommit = b.hook
	s2.beforeCommit = b.hook
	errs := make([]error, 2)
	var wg sync.WaitGroup
	wg.Go(func() { _, _, errs[0] = s1.StartIssue(t.Context(), alice, is.ID, 0) })
	wg.Go(func() { _, _, errs[1] = s2.StartIssue(t.Context(), bob, is.ID, 0) })
	wg.Wait()
	assertOneWinner(t, errs)
	got, err := s1.GetIssue(t.Context(), is.ID)
	if err != nil {
		t.Fatal(err)
	}
	for i, err := range errs {
		var held *HeldError
		if err != nil && (!errors.As(err, &held) || held.By != got.Assignee) {
			t.Errorf("loser %d: %v, want held by %s", i, err, got.Assignee)
		}
	}
	if got.Rev != 2 || got.Status != StatusInProgress {
		t.Errorf("after the race: %+v", got)
	}
	assertUpdateEvents(t, s1, is.ID, 1)
	assertGapless(t, s1)
}

// Many sessions start without an id at once: each gets a different issue.
func TestStartTopReadyConcurrent(t *testing.T) {
	s := newStore(t)
	const n = 5
	for range n {
		mustCreate(t, s, NewIssue{})
	}
	ids := make([]IssueID, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			a := Actor{Principal: fmt.Sprintf("p%d", i), Session: "s", Machine: "m"}
			var is Issue
			is, _, errs[i] = s.StartIssue(t.Context(), a, "", 0)
			ids[i] = is.ID
		})
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
	slices.Sort(ids)
	if len(slices.Compact(ids)) != n {
		t.Errorf("two sessions took the same issue: %v", ids)
	}
}

func TestFinish(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	is := mustCreate(t, s, NewIssue{Title: "work"})
	if _, _, err := s.StartIssue(ctx, alice, is.ID, 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.FinishIssue(ctx, bob, is.ID, 0, Finish{}); !errors.Is(err, ErrConflict) {
		t.Fatalf("finish held by alice: %v", err)
	}

	got, created, err := s.FinishIssue(ctx, alice, is.ID, 0, Finish{Reason: "done", Handoff: "watch the cache",
		Discovered: []NewIssue{{Title: "flaky test", Type: TypeBug, Priority: prio(P1)}, {Title: "rename x"}}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusClosed || got.CloseReason != "done" || len(created) != 2 {
		t.Fatalf("finish: %+v %v", got, created)
	}
	for i, want := range []struct {
		title string
		typ   IssueType
		p     Priority
	}{{"flaky test", TypeBug, P1}, {"rename x", TypeTask, P2}} {
		d, err := s.GetIssue(ctx, created[i])
		if err != nil {
			t.Fatal(err)
		}
		if d.Title != want.title || d.Type != want.typ || d.Priority != want.p || d.Status != StatusOpen || d.CreatedBy != "alice" {
			t.Errorf("discovered %d: %+v", i, d)
		}
		deps, err := s.Deps(ctx, created[i])
		if err != nil {
			t.Fatal(err)
		}
		if len(deps) != 1 || deps[0].From != created[i] || deps[0].To != is.ID || deps[0].Type != DepDiscoveredFrom {
			t.Errorf("discovered %d deps: %+v", i, deps)
		}
	}
	h, err := s.LastHandoff(ctx, is.ID)
	if err != nil || h == nil || h.Body != "watch the cache" || h.Kind != CommentHandoff || h.Author != "alice" {
		t.Fatalf("handoff: %+v, %v", h, err)
	}
	if _, _, err := s.FinishIssue(ctx, alice, is.ID, 0, Finish{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("finish twice: %v", err)
	}
	assertGapless(t, s)
}

// A finish that fails part way writes nothing.
func TestFinishIsAtomic(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	is := mustCreate(t, s, NewIssue{Title: "work"})
	seq := lastSeq(t, s)
	tooMany := make([]NewIssue, MaxDiscovered+1)
	for i := range tooMany {
		tooMany[i].Title = "x"
	}
	cases := []struct {
		name string
		f    Finish
		want error
	}{
		{"missing parent", Finish{Handoff: "note", Discovered: []NewIssue{{Title: "fine"}, {Title: "orphan", ParentID: "tst-nope"}}}, ErrNotFound},
		{"bad discovered", Finish{Discovered: []NewIssue{{Title: " "}}}, ErrInvalid},
		{"discovered with id", Finish{Discovered: []NewIssue{{Title: "x", ID: "tst-mine"}}}, ErrInvalid},
		{"too many", Finish{Discovered: tooMany}, ErrInvalid},
		{"long reason", Finish{Reason: string(make([]byte, 2001))}, ErrInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := s.FinishIssue(ctx, alice, is.ID, 0, tc.f); !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
	if _, _, err := s.FinishIssue(ctx, alice, "tst-nope", 0, Finish{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("finish missing: %v", err)
	}
	if lastSeq(t, s) != seq {
		t.Fatal("a failed finish wrote events")
	}
	got, err := s.GetIssue(ctx, is.ID)
	if err != nil || got.Status != StatusOpen || got.Rev != 1 {
		t.Fatalf("issue after failed finishes: %+v, %v", got, err)
	}
	page, err := s.List(ctx, Filter{})
	if err != nil || len(page.Issues) != 1 {
		t.Fatalf("issues after failed finishes: %d, %v", len(page.Issues), err)
	}
}

func TestHandoff(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	is := mustCreate(t, s, NewIssue{Title: "work"})
	if h, err := s.LastHandoff(ctx, is.ID); err != nil || h != nil {
		t.Fatalf("no handoff yet: %+v, %v", h, err)
	}
	if _, _, err := s.StartIssue(ctx, alice, is.ID, 0); err != nil {
		t.Fatal(err)
	}

	// A note alone changes nothing about who holds the issue; anyone may add one.
	got, err := s.HandoffIssue(ctx, bob, is.ID, 0, "first", false)
	if err != nil || got.Rev != 2 || got.Assignee != "alice" {
		t.Fatalf("note: %+v, %v", got, err)
	}
	if _, err := s.HandoffIssue(ctx, bob, is.ID, 0, "mine now", true); !errors.Is(err, ErrConflict) {
		t.Fatalf("release by a non-holder: %v", err)
	}
	got, err = s.HandoffIssue(ctx, alice, is.ID, 0, "second", true)
	if err != nil || got.Status != StatusOpen || got.Assignee != "" || got.Rev != 3 {
		t.Fatalf("release: %+v, %v", got, err)
	}
	if h, err := s.LastHandoff(ctx, is.ID); err != nil || h.Body != "second" {
		t.Fatalf("last handoff: %+v, %v", h, err)
	}
	// Released, it can be started by someone else.
	if got, _, err := s.StartIssue(ctx, bob, is.ID, 0); err != nil || got.Assignee != "bob" {
		t.Fatalf("start after release: %+v, %v", got, err)
	}

	if _, err := s.AddComment(ctx, alice, is.ID, "plain"); err != nil {
		t.Fatal(err)
	}
	cs, err := s.Comments(ctx, is.ID)
	if err != nil || len(cs) != 3 {
		t.Fatalf("comments: %+v, %v", cs, err)
	}
	if kinds := []CommentKind{cs[0].Kind, cs[1].Kind, cs[2].Kind}; !slices.Equal(kinds, []CommentKind{CommentHandoff, CommentHandoff, CommentPlain}) {
		t.Errorf("kinds: %q", kinds)
	}
	if _, err := s.CloseIssue(ctx, bob, is.ID, 0, ""); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		note    string
		release bool
		want    error
	}{
		{"release closed", "x", true, ErrInvalid},
		{"empty note", "", false, ErrInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.HandoffIssue(ctx, bob, is.ID, 0, tc.note, tc.release); !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
	if _, err := s.HandoffIssue(ctx, bob, is.ID, 0, "after close", false); err != nil {
		t.Errorf("note on a closed issue: %v", err)
	}
	assertGapless(t, s)
}
