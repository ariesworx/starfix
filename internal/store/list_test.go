package store

import (
	"errors"
	"slices"
	"testing"
	"time"
)

func TestListPaging(t *testing.T) {
	// A frozen clock gives every issue the same created_at, so ordering and
	// the cursor fall back to the ID tie-break.
	frozen := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	s := openStore(t, newDSN(t), Options{Now: func() time.Time { return frozen }})
	ctx := t.Context()

	var all []IssueID
	for i := range 25 {
		in := NewIssue{Title: "p"}
		if i%5 == 0 {
			in.Labels = []string{"five"}
			in.Type = TypeBug
		}
		all = append(all, mustCreate(t, s, in).ID)
	}
	slices.Sort(all)

	var got []IssueID
	var sizes []int
	cur := Cursor("")
	for {
		page, err := s.List(ctx, Filter{Limit: 10, Cursor: cur})
		if err != nil {
			t.Fatal(err)
		}
		sizes = append(sizes, len(page.Issues))
		for _, is := range page.Issues {
			got = append(got, is.ID)
		}
		if page.Next == "" {
			break
		}
		cur = page.Next
	}
	if !slices.Equal(sizes, []int{10, 10, 5}) {
		t.Errorf("page sizes %v, want [10 10 5]", sizes)
	}
	if !slices.Equal(got, all) {
		t.Errorf("paged ids differ from the full ordered set")
	}
	if _, err := s.List(ctx, Filter{Cursor: "garbage!"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("bad cursor: %v, want ErrInvalid", err)
	}
}

func TestListFilters(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	parent := mustCreate(t, s, NewIssue{Title: "parent", Type: TypeEpic})
	a := mustCreate(t, s, NewIssue{Title: "a", Assignee: "alice", Labels: []string{"x", "y"}, ParentID: parent.ID})
	b := mustCreate(t, s, NewIssue{Title: "b", Type: TypeBug, Priority: prio(P0), Labels: []string{"x"}})
	c := mustCreate(t, s, NewIssue{Title: "c", Status: StatusInProgress})

	cases := []struct {
		name string
		f    Filter
		want []IssueID
	}{
		{"all", Filter{}, []IssueID{parent.ID, a.ID, b.ID, c.ID}},
		{"status", Filter{Statuses: []Status{StatusInProgress}}, []IssueID{c.ID}},
		{"type", Filter{Types: []IssueType{TypeBug, TypeEpic}}, []IssueID{parent.ID, b.ID}},
		{"priority", Filter{Priorities: []Priority{P0}}, []IssueID{b.ID}},
		{"assignee", Filter{Assignee: "alice"}, []IssueID{a.ID}},
		{"parent", Filter{ParentID: parent.ID}, []IssueID{a.ID}},
		{"one label", Filter{Labels: []string{"x"}}, []IssueID{a.ID, b.ID}},
		{"all labels", Filter{Labels: []string{"x", "y"}}, []IssueID{a.ID}},
		{"no match", Filter{Labels: []string{"z"}}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			page, err := s.List(ctx, tc.f)
			if err != nil {
				t.Fatal(err)
			}
			var ids []IssueID
			for _, is := range page.Issues {
				ids = append(ids, is.ID)
			}
			if !slices.Equal(ids, tc.want) {
				t.Errorf("got %v, want %v", ids, tc.want)
			}
		})
	}
	if _, err := s.List(ctx, Filter{Statuses: []Status{"nope"}}); !errors.Is(err, ErrInvalid) {
		t.Errorf("bad status: %v", err)
	}
}
