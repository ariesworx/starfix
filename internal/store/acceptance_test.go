package store

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"testing"
)

func TestParseAcceptance(t *testing.T) {
	tests := []struct {
		name string
		text string
		want []parsedItem
	}{
		{"empty", "", nil},
		{"blank", "  \n\t\n", nil},
		{"prose is one item", "Logins survive a restart.\nNo data is lost.",
			[]parsedItem{{text: "Logins survive a restart. No data is lost."}}},
		{"dashes", "- one\n- two", []parsedItem{{text: "one"}, {text: "two"}}},
		{"stars and pluses", "* one\n+ two", []parsedItem{{text: "one"}, {text: "two"}}},
		{"numbered", "1. one\n2) two\n10. ten", []parsedItem{{text: "one"}, {text: "two"}, {text: "ten"}}},
		{"task list", "- [ ] open\n- [x] done\n- [X] also done",
			[]parsedItem{{text: "open"}, {text: "done", ticked: true}, {text: "also done", ticked: true}}},
		{"intro and outro lines are not items", "Done when:\n\n- one\n- two\n\nThanks.",
			[]parsedItem{{text: "one"}, {text: "two"}}},
		{"indented continuation joins its item", "- one\n  continued\n- two",
			[]parsedItem{{text: "one continued"}, {text: "two"}}},
		{"nested items are items", "- one\n  - one a\n- two",
			[]parsedItem{{text: "one"}, {text: "one a"}, {text: "two"}}},
		{"empty items are dropped", "- \n- [ ]\n- real", []parsedItem{{text: "real"}}},
		{"crlf", "- one\r\n- two\r\n", []parsedItem{{text: "one"}, {text: "two"}}},
		{"a rule is not an item", "---\nship it", []parsedItem{{text: "--- ship it"}}},
		{"inner spaces collapse", "-   spaced    out  ", []parsedItem{{text: "spaced out"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseAcceptance(tc.text); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parseAcceptance(%q) = %+v, want %+v", tc.text, got, tc.want)
			}
		})
	}
}

// states is each item's state, in order, for compact comparison.
func states(items []AcceptanceItem) []ItemState {
	var out []ItemState
	for _, it := range items {
		out = append(out, it.State)
	}
	return out
}

// close and finish refuse while an item is neither ticked nor waived; a
// forced close records the items it overrode.
func TestAcceptanceGatesClose(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	is := mustCreate(t, s, NewIssue{Title: "login", Acceptance: "Done when:\n- [ ] survives restart\n- [x] has tests\n- docs updated"})

	items, err := s.AcceptanceItems(ctx, is.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := states(items); !slices.Equal(got, []ItemState{ItemOpen, ItemTicked, ItemOpen}) || items[2].Text != "docs updated" || items[2].N != 3 {
		t.Fatalf("items = %+v", items)
	}

	var ae *AcceptanceError
	_, err = s.CloseIssue(ctx, alice, is.ID, 0, "done")
	if !errors.As(err, &ae) || ae.ID != is.ID || !slices.Equal(ae.Open, []int{1, 3}) || !errors.Is(err, ErrInvalid) {
		t.Fatalf("close with open items: %v", err)
	}

	items, err = s.Accept(ctx, alice, is.ID, Acceptance{Tick: []int{1}, Waive: map[int]string{3: "no docs for this"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := states(items); !slices.Equal(got, []ItemState{ItemTicked, ItemTicked, ItemWaived}) ||
		items[2].Reason != "no docs for this" || items[0].By != "alice" || items[0].At == nil || items[1].By != "alice" {
		t.Fatalf("after accept: %+v", items)
	}
	seq := lastSeq(t, s)
	if _, err := s.Accept(ctx, alice, is.ID, Acceptance{Tick: []int{1, 2}}); err != nil {
		t.Fatal(err)
	}
	if lastSeq(t, s) != seq {
		t.Error("ticking ticked items wrote events")
	}
	if _, err := s.CloseIssue(ctx, alice, is.ID, 0, "done"); err != nil {
		t.Fatalf("close with every item met: %v", err)
	}

	var ops []Op
	evs, _ := s.History(ctx, is.ID)
	for _, e := range evs {
		ops = append(ops, e.Op)
	}
	if want := []Op{OpIssueCreate, OpAcceptTick, OpAcceptWaive, OpIssueClose}; !slices.Equal(ops, want) {
		t.Errorf("history = %v, want %v", ops, want)
	}

	// Forcing a close, an admin's call, records what it overrode.
	forced := mustCreate(t, s, NewIssue{Title: "forced", Acceptance: "- a\n- b"})
	if _, err := s.Accept(ctx, alice, forced.ID, Acceptance{Tick: []int{2}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ForceClose(ctx, dana, forced.ID, 0, "obsolete"); err != nil {
		t.Fatalf("force close: %v", err)
	}
	evs, _ = s.History(ctx, forced.ID)
	last := evs[len(evs)-1]
	var after map[string]any
	if err := json.Unmarshal(last.After, &after); err != nil || last.Op != OpIssueClose ||
		!reflect.DeepEqual(after["acceptance_overridden"], []any{1.0}) {
		t.Errorf("forced close event: %s %s (%v)", last.Op, last.After, err)
	}

	// No criteria, no gate.
	plain := mustCreate(t, s, NewIssue{Title: "plain"})
	if _, err := s.CloseIssue(ctx, alice, plain.ID, 0, ""); err != nil {
		t.Errorf("close without criteria: %v", err)
	}
}

func TestAcceptRefusals(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	is := mustCreate(t, s, NewIssue{Title: "x", Acceptance: "- a\n- b"})
	closed := mustCreate(t, s, NewIssue{Title: "closed", Acceptance: "- a"})
	if _, err := s.ForceClose(ctx, dana, closed.ID, 0, ""); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		id   IssueID
		a    Acceptance
		want error
	}{
		{"nothing to do", is.ID, Acceptance{}, ErrInvalid},
		{"no such item", is.ID, Acceptance{Tick: []int{3}}, ErrInvalid},
		{"item zero", is.ID, Acceptance{Untick: []int{0}}, ErrInvalid},
		{"waive without reason", is.ID, Acceptance{Waive: map[int]string{1: ""}}, ErrInvalid},
		{"multi-line reason", is.ID, Acceptance{Waive: map[int]string{1: "a\nb"}}, ErrInvalid},
		{"tick and waive one item", is.ID, Acceptance{Tick: []int{1}, Waive: map[int]string{1: "r"}}, ErrInvalid},
		{"closed issue", closed.ID, Acceptance{Tick: []int{1}}, ErrInvalid},
		{"missing issue", "tst-nope", Acceptance{Tick: []int{1}}, ErrNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.Accept(ctx, alice, tc.id, tc.a); !errors.Is(err, tc.want) {
				t.Errorf("Accept(%s, %+v) = %v, want %v", tc.id, tc.a, err, tc.want)
			}
		})
	}
}

// Ticks follow an item's text: editing the criteria keeps the state of
// items whose text is unchanged, wherever they move; untick reopens one,
// even one ticked in the text at create.
func TestAcceptanceFollowsEdits(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	is := mustCreate(t, s, NewIssue{Title: "x", Acceptance: "- a\n- b\n- [x] c"})
	if _, err := s.Accept(ctx, alice, is.ID, Acceptance{Tick: []int{2}}); err != nil {
		t.Fatal(err)
	}
	acc := "- new\n- a\n- b\n- [x] c"
	is, err := s.UpdateIssue(ctx, alice, is.ID, is.Rev, IssuePatch{Acceptance: &acc})
	if err != nil {
		t.Fatal(err)
	}
	items, err := s.Accept(ctx, alice, is.ID, Acceptance{Untick: []int{4}})
	if err != nil {
		t.Fatal(err)
	}
	if got := states(items); !slices.Equal(got, []ItemState{ItemOpen, ItemOpen, ItemTicked, ItemOpen}) {
		t.Errorf("after edit and untick: %+v", items)
	}
}

// finish ticks and waives in its own transaction: when items stay open it
// is refused and none of it is written.
func TestFinishAccepts(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	is := mustCreate(t, s, NewIssue{Title: "x", Acceptance: "- a\n- b"})
	mustStart(t, s, alice, is.ID)
	seq := lastSeq(t, s)
	var ae *AcceptanceError
	_, _, err := s.FinishIssue(ctx, alice, is.ID, 0, Finish{Accept: Acceptance{Tick: []int{1}}, Discovered: []NewIssue{{Title: "more"}}})
	if !errors.As(err, &ae) || !slices.Equal(ae.Open, []int{2}) {
		t.Fatalf("finish with an item open: %v", err)
	}
	if lastSeq(t, s) != seq {
		t.Fatal("a refused finish wrote events")
	}
	out, _, err := s.FinishIssue(ctx, alice, is.ID, 0, Finish{Accept: Acceptance{Tick: []int{1}, Waive: map[int]string{2: "covered by tst-1"}}})
	if err != nil || out.Status != StatusClosed {
		t.Fatalf("finish: %+v, %v", out, err)
	}
}

// Imported issues get their checklist from the same parse.
func TestImportedAcceptance(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	in := importedIssue("bd-imp1", func(is *Issue) { is.Acceptance = "- [x] one\n- two" })
	if _, err := s.ImportIssue(ctx, importer, in); err != nil {
		t.Fatal(err)
	}
	items, err := s.AcceptanceItems(ctx, "bd-imp1")
	if err != nil {
		t.Fatal(err)
	}
	if got := states(items); !slices.Equal(got, []ItemState{ItemTicked, ItemOpen}) {
		t.Errorf("imported items: %+v", items)
	}
}
