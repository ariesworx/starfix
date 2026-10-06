package store

import (
	"errors"
	"slices"
	"testing"
	"time"
)

func TestReadyAndBlocked(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	now := time.Now().UTC()
	past, future := now.Add(-time.Hour), now.Add(time.Hour)

	mk := func(title string, in NewIssue) IssueID {
		in.Title = title
		return mustCreate(t, s, in).ID
	}
	dep := func(from, to IssueID, typ DepType) {
		t.Helper()
		if err := s.AddDep(ctx, alice, from, to, typ); err != nil {
			t.Fatalf("dep %s -> %s: %v", from, to, err)
		}
	}

	free := mk("free", NewIssue{})
	blocker := mk("blocker", NewIssue{Priority: prio(P1)})
	blockedOpen := mk("blocked by open", NewIssue{})
	closedBlocker := mk("closed blocker", NewIssue{})
	blockedClosed := mk("blocked by closed", NewIssue{})
	condTarget := mk("cond target", NewIssue{Priority: prio(P3)})
	condBlocked := mk("cond blocked", NewIssue{})
	relatedOnly := mk("related only", NewIssue{})
	epic := mk("epic", NewIssue{Type: TypeEpic})
	child := mk("child", NewIssue{ParentID: epic})
	grandchild := mk("grandchild", NewIssue{ParentID: child})
	deferFuture := mk("defer future", NewIssue{DeferUntil: &future})
	deferPast := mk("defer past", NewIssue{DeferUntil: &past})
	deferParent := mk("defer parent", NewIssue{DeferUntil: &future})
	deferChild := mk("defer child", NewIssue{ParentID: deferParent})
	statusDeferred := mk("status deferred", NewIssue{Status: StatusDeferred})
	inProgress := mk("in progress", NewIssue{Status: StatusInProgress})
	template := mk("template", NewIssue{Template: true})
	done := mk("done", NewIssue{})
	urgent := mk("urgent", NewIssue{Priority: prio(P0)})

	dep(blockedOpen, blocker, DepBlocks)
	dep(blockedClosed, closedBlocker, DepBlocks)
	dep(condBlocked, condTarget, DepConditionalBlocks)
	dep(relatedOnly, blocker, DepRelated)
	dep(epic, blocker, DepBlocks)
	for _, id := range []IssueID{closedBlocker, done} {
		if _, err := s.CloseIssue(ctx, alice, id, 0, ""); err != nil {
			t.Fatal(err)
		}
	}

	ready := readyIDs(t, s)
	// Priority first, then creation order.
	want := []IssueID{urgent, blocker, free, blockedClosed, relatedOnly, deferPast, condTarget}
	if !slices.Equal(ready, want) {
		t.Errorf("ready =\n  %v\nwant\n  %v", ready, want)
	}

	blocked, err := s.Blocked(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := map[IssueID]BlockedIssue{}
	for _, b := range blocked {
		got[b.Issue.ID] = b
	}
	cases := []struct {
		name string
		id   IssueID
		by   []IssueID
		via  IssueID
	}{
		{"direct blocks", blockedOpen, []IssueID{blocker}, ""},
		{"conditional blocks", condBlocked, []IssueID{condTarget}, ""},
		{"blocked epic", epic, []IssueID{blocker}, ""},
		{"child of blocked epic", child, []IssueID{blocker}, epic},
		{"grandchild of blocked epic", grandchild, []IssueID{blocker}, epic},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, ok := got[tc.id]
			if !ok {
				t.Fatalf("%s not in Blocked", tc.id)
			}
			if !slices.Equal(b.BlockedBy, tc.by) || b.Via != tc.via {
				t.Errorf("blocked by %v via %q, want %v via %q", b.BlockedBy, b.Via, tc.by, tc.via)
			}
		})
	}
	if len(blocked) != len(cases) {
		t.Errorf("Blocked has %d issues, want %d", len(blocked), len(cases))
	}
	for _, id := range []IssueID{deferFuture, deferChild, statusDeferred, inProgress, template, done} {
		if slices.Contains(ready, id) {
			t.Errorf("%s should not be ready", id)
		}
	}

	// Closing the blocker frees everything it held, through the epic.
	if _, err := s.CloseIssue(ctx, alice, blocker, 0, ""); err != nil {
		t.Fatal(err)
	}
	ready = readyIDs(t, s)
	for _, id := range []IssueID{blockedOpen, epic, child, grandchild} {
		if !slices.Contains(ready, id) {
			t.Errorf("%s should be ready once the blocker closed", id)
		}
	}
	if slices.Contains(ready, condBlocked) {
		t.Errorf("conditional-blocks target still open; %s should not be ready", condBlocked)
	}
}

func readyIDs(t *testing.T, s *Store) []IssueID {
	t.Helper()
	rs, err := s.Ready(t.Context(), 100)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]IssueID, len(rs))
	for i, r := range rs {
		ids[i] = r.ID
	}
	return ids
}

func TestCycleRefusal(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	type edge struct {
		from, to int
		typ      DepType
	}
	cases := []struct {
		name     string
		parents  map[int]int // child -> parent, set at creation
		setup    []edge
		add      edge
		reparent *[2]int // child, new parent, via UpdateIssue
		want     error
	}{
		{name: "self", add: edge{0, 0, DepBlocks}, want: ErrCycle},
		{name: "self related", add: edge{0, 0, DepRelated}, want: ErrInvalid},
		{name: "two-cycle", setup: []edge{{0, 1, DepBlocks}}, add: edge{1, 0, DepBlocks}, want: ErrCycle},
		{name: "three-cycle mixed types", setup: []edge{{0, 1, DepBlocks}, {1, 2, DepWaitsFor}},
			add: edge{2, 0, DepConditionalBlocks}, want: ErrCycle},
		{name: "related back-edge allowed", setup: []edge{{0, 1, DepBlocks}}, add: edge{1, 0, DepRelated}},
		{name: "discovered-from back-edge allowed", setup: []edge{{0, 1, DepBlocks}}, add: edge{1, 0, DepDiscoveredFrom}},
		{name: "chain without cycle", setup: []edge{{0, 1, DepBlocks}}, add: edge{1, 2, DepBlocks}},
		{name: "parent blocked by its child", parents: map[int]int{1: 0}, add: edge{0, 1, DepBlocks}, want: ErrCycle},
		{name: "grandparent blocked by grandchild", parents: map[int]int{1: 0, 2: 1}, add: edge{0, 2, DepBlocks}, want: ErrCycle},
		{name: "child blocked by parent's sibling", parents: map[int]int{1: 0}, add: edge{1, 2, DepBlocks}},
		{name: "parent loop", parents: map[int]int{1: 0}, reparent: &[2]int{0, 1}, want: ErrCycle},
		{name: "reparent across a block", setup: []edge{{1, 0, DepBlocks}}, reparent: &[2]int{0, 1}, want: ErrCycle},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ids := make([]IssueID, 3)
			for i := range ids {
				in := NewIssue{Title: tc.name}
				if p, ok := tc.parents[i]; ok {
					in.ParentID = ids[p]
				}
				ids[i] = mustCreate(t, s, in).ID
			}
			for _, e := range tc.setup {
				if err := s.AddDep(ctx, alice, ids[e.from], ids[e.to], e.typ); err != nil {
					t.Fatalf("setup: %v", err)
				}
			}
			var err error
			if tc.reparent != nil {
				is, gerr := s.GetIssue(ctx, ids[tc.reparent[0]])
				if gerr != nil {
					t.Fatal(gerr)
				}
				_, err = s.UpdateIssue(ctx, alice, is.ID, is.Rev, IssuePatch{ParentID: &ids[tc.reparent[1]]})
			} else {
				err = s.AddDep(ctx, alice, ids[tc.add.from], ids[tc.add.to], tc.add.typ)
			}
			if tc.want == nil && err != nil || tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
	if err := s.AddDep(ctx, alice, "tst-nope", "tst-nada", DepBlocks); !errors.Is(err, ErrNotFound) {
		t.Errorf("dep on missing issues: %v, want ErrNotFound", err)
	}
}
