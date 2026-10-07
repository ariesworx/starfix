package store

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// item is the part of an inbox item a test compares.
type item struct {
	to, session string
	kind        InboxKind
	issue       IssueID
	from        string
}

func items(t *testing.T, s *Store, a Actor) []item {
	t.Helper()
	page, err := s.Inbox(t.Context(), a, true, MaxInboxLimit)
	if err != nil {
		t.Fatalf("Inbox(%s/%s): %v", a.Principal, a.Session, err)
	}
	var out []item
	for _, it := range page.Items {
		out = append(out, item{it.To, it.Session, it.Kind, it.Issue, it.From})
	}
	slices.Reverse(out) // oldest first reads better in a table
	return out
}

// register puts principals in the agents registry, which is what makes a
// principal known to mentions.
func register(t *testing.T, s *Store, as ...Actor) {
	t.Helper()
	for _, a := range as {
		if err := s.TouchAgent(t.Context(), a, ""); err != nil {
			t.Fatal(err)
		}
	}
}

var (
	alice2 = Actor{Principal: "alice", Session: "sess-a2", Machine: "desktop"}
	carol  = Actor{Principal: "carol", Session: "sess-c", Machine: "laptop-c"}
)

// Each kind of inbox item is created by the change that causes it, for
// the principal (or session) it concerns, and never for the actor itself.
func TestInboxKinds(t *testing.T) {
	tests := []struct {
		name string
		// do makes the change; it returns the issue the items name.
		do     func(t *testing.T, s *Store, clk *clock) IssueID
		reader Actor
		want   []item
	}{
		{
			name: "reaper expiry tells the losing session",
			do: func(t *testing.T, s *Store, clk *clock) IssueID {
				is := mustCreate(t, s, NewIssue{Title: "work"})
				mustStart(t, s, alice, is.ID)
				clk.add(DefaultLease + time.Second)
				if _, err := s.ReapClaims(t.Context()); err != nil {
					t.Fatal(err)
				}
				return is.ID
			},
			reader: alice,
			want:   []item{{"alice", "sess-a", InboxClaimLost, "", "starfixd"}},
		},
		{
			name: "takeover after expiry tells the losing session",
			do: func(t *testing.T, s *Store, clk *clock) IssueID {
				is := mustCreate(t, s, NewIssue{Title: "work"})
				mustStart(t, s, alice, is.ID)
				clk.add(DefaultLease + time.Second)
				mustStart(t, s, bob, is.ID)
				return is.ID
			},
			reader: alice,
			want:   []item{{"alice", "sess-a", InboxClaimLost, "", "bob"}},
		},
		{
			name: "takeover by another session of the same principal",
			do: func(t *testing.T, s *Store, _ *clock) IssueID {
				is := mustCreate(t, s, NewIssue{Title: "work"})
				mustStart(t, s, alice, is.ID)
				if _, _, err := s.StartIssue(t.Context(), alice2, is.ID, 0, true); err != nil {
					t.Fatal(err)
				}
				return is.ID
			},
			reader: alice,
			want:   []item{{"alice", "sess-a", InboxClaimLost, "", "alice"}},
		},
		{
			name: "the same session starting again loses nothing",
			do: func(t *testing.T, s *Store, _ *clock) IssueID {
				is := mustCreate(t, s, NewIssue{Title: "work"})
				mustStart(t, s, alice, is.ID)
				mustStart(t, s, alice, is.ID)
				return is.ID
			},
			reader: alice,
		},
		{
			name: "update assigns another principal",
			do: func(t *testing.T, s *Store, _ *clock) IssueID {
				is := mustCreate(t, s, NewIssue{Title: "work"})
				up, err := s.UpdateIssue(t.Context(), alice, is.ID, is.Rev, IssuePatch{Assignee: ptr("bob")})
				if err != nil {
					t.Fatal(err)
				}
				// Assigning bob again, or editing another field, says nothing new.
				if _, err := s.UpdateIssue(t.Context(), alice, is.ID, up.Rev, IssuePatch{Assignee: ptr("bob"), Title: ptr("x")}); err != nil {
					t.Fatal(err)
				}
				return is.ID
			},
			reader: bob,
			want:   []item{{"bob", "", InboxAssigned, "", "alice"}},
		},
		{
			name: "create assigned to another principal",
			do: func(t *testing.T, s *Store, _ *clock) IssueID {
				return mustCreate(t, s, NewIssue{Title: "work", Assignee: "bob"}).ID
			},
			reader: bob,
			want:   []item{{"bob", "", InboxAssigned, "", "alice"}},
		},
		{
			name: "assigning yourself is not news",
			do: func(t *testing.T, s *Store, _ *clock) IssueID {
				is := mustCreate(t, s, NewIssue{Title: "work", Assignee: "alice"})
				mustStart(t, s, alice, is.ID)
				return is.ID
			},
			reader: alice,
		},
		{
			name: "mention of a known principal, not of strangers, emails or yourself",
			do: func(t *testing.T, s *Store, _ *clock) IssueID {
				register(t, s, bob)
				is := mustCreate(t, s, NewIssue{Title: "work"})
				if _, err := s.AddComment(t.Context(), alice, is.ID,
					"@bob, take a look. cc @carol (not known), alice@example.com and @alice; again @bob.", ""); err != nil {
					t.Fatal(err)
				}
				return is.ID
			},
			reader: bob,
			want:   []item{{"bob", "", InboxMention, "", "alice"}},
		},
		{
			name: "a handoff to a principal, who is not also told of the mention",
			do: func(t *testing.T, s *Store, _ *clock) IssueID {
				register(t, s, bob, carol)
				is := mustCreate(t, s, NewIssue{Title: "work"})
				mustStart(t, s, alice, is.ID)
				if _, err := s.HandoffIssue(t.Context(), alice, is.ID, 0,
					HandoffNote{Note: "over to @bob, ask @carol", HandoffFields: HandoffFields{To: "bob"}}, true, ""); err != nil {
					t.Fatal(err)
				}
				return is.ID
			},
			reader: bob,
			want:   []item{{"bob", "", InboxHandoff, "", "alice"}},
		},
		{
			name: "finish hands off",
			do: func(t *testing.T, s *Store, _ *clock) IssueID {
				is := mustCreate(t, s, NewIssue{Title: "work"})
				mustStart(t, s, alice, is.ID)
				if _, _, err := s.FinishIssue(t.Context(), alice, is.ID, 0, Finish{
					Handoff: HandoffNote{Note: "done; deploy next", HandoffFields: HandoffFields{State: HandoffDone, To: "bob"}}}); err != nil {
					t.Fatal(err)
				}
				return is.ID
			},
			reader: bob,
			want:   []item{{"bob", "", InboxHandoff, "", "alice"}},
		},
		{
			name: "a handoff to yourself is not news",
			do: func(t *testing.T, s *Store, _ *clock) IssueID {
				is := mustCreate(t, s, NewIssue{Title: "work"})
				if _, err := s.HandoffIssue(t.Context(), alice, is.ID, 0,
					HandoffNote{Note: "note to self", HandoffFields: HandoffFields{To: "alice"}}, false, ""); err != nil {
					t.Fatal(err)
				}
				return is.ID
			},
			reader: alice,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, clk := clockStore(t)
			id := tc.do(t, s, clk)
			for i := range tc.want {
				tc.want[i].issue = id
			}
			if got := items(t, s, tc.reader); !slices.Equal(got, tc.want) {
				t.Errorf("Inbox(%s) = %+v\nwant %+v", tc.reader.Principal, got, tc.want)
			}
			assertGapless(t, s)
		})
	}
}

func mustStart(t *testing.T, s *Store, a Actor, id IssueID) {
	t.Helper()
	if _, _, err := s.StartIssue(t.Context(), a, id, 0, false); err != nil {
		t.Fatalf("start %s as %s/%s: %v", id, a.Principal, a.Session, err)
	}
}

// An item is written with its cause or not at all: a change that fails,
// before or at commit, leaves no item and pushes nothing.
func TestInboxRollsBackWithItsCause(t *testing.T) {
	s, _ := clockStore(t)
	ctx := t.Context()
	register(t, s, bob)
	w := s.Watch("bob", "sess-b")
	defer w.Close()
	is := mustCreate(t, s, NewIssue{Title: "work"})
	mustStart(t, s, alice, is.ID)

	// A finish whose discovered issue names a missing parent fails as a
	// whole, handoff item and all.
	_, _, err := s.FinishIssue(ctx, alice, is.ID, 0, Finish{
		Handoff:    HandoffNote{Note: "over to @bob", HandoffFields: HandoffFields{To: "bob"}},
		Discovered: []NewIssue{{Title: "orphan", ParentID: "tst-nope"}}})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("FinishIssue with a missing parent = %v, want ErrNotFound", err)
	}
	// A comment whose transaction fails at commit leaves no mention.
	s.beforeCommit = func(context.Context) error { return errors.New("injected commit failure") }
	_, err = s.AddComment(ctx, alice, is.ID, "@bob look", "")
	s.beforeCommit = nil
	if err == nil {
		t.Fatal("AddComment with a failing commit succeeded")
	}
	if got := items(t, s, bob); len(got) != 0 {
		t.Errorf("Inbox(bob) after failed changes = %+v, want none", got)
	}
	select {
	case <-w.Ready():
		got, over := w.Take()
		t.Errorf("failed changes pushed %+v (overflow %v)", got, over)
	default:
	}
	if h, err := s.LastHandoff(ctx, is.ID); err != nil || h != nil {
		t.Errorf("LastHandoff after a failed finish = %+v, %v; want none", h, err)
	}
}

func TestInboxListAndAck(t *testing.T) {
	s, clk := clockStore(t)
	ctx := t.Context()
	register(t, s, alice)
	a := mustCreate(t, s, NewIssue{Title: "a"})
	b := mustCreate(t, s, NewIssue{Title: "b"})
	// One item for alice's session, two for alice anywhere.
	mustStart(t, s, alice, a.ID)
	clk.add(DefaultLease + time.Second)
	mustStart(t, s, bob, a.ID)
	if _, err := s.CreateIssue(ctx, bob, NewIssue{Title: "c", Assignee: "alice"}); err != nil {
		t.Fatal(err)
	}
	clk.add(time.Second)
	if _, err := s.AddComment(ctx, bob, b.ID, "@alice "+strings.Repeat("long ", 100), ""); err != nil {
		t.Fatal(err)
	}

	page, err := s.Inbox(ctx, alice, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	kinds := func(p InboxPage) []InboxKind {
		var out []InboxKind
		for _, it := range p.Items {
			out = append(out, it.Kind)
		}
		return out
	}
	if want := []InboxKind{InboxMention, InboxAssigned, InboxClaimLost}; !slices.Equal(kinds(page), want) || page.Unread != 3 {
		t.Fatalf("Inbox(alice) = %v, %d unread; want %v newest first, 3 unread", kinds(page), page.Unread, want)
	}
	if m := page.Items[0]; len(m.Body) > InboxBodyMax+len("…") || !strings.HasPrefix(m.Body, "@alice long") || m.Issue != b.ID ||
		m.From != "bob" || m.At.IsZero() || m.ReadAt != nil {
		t.Errorf("mention item = %+v, want a cut body naming %s from bob", m, b.ID)
	}
	// Another session sees the principal's items, not the first session's.
	other, err := s.Inbox(ctx, alice2, false, 0)
	if err != nil || !slices.Equal(kinds(other), []InboxKind{InboxMention, InboxAssigned}) {
		t.Fatalf("Inbox(alice2) = %v, %v; want the two principal-wide items", kinds(other), err)
	}
	if lim, err := s.Inbox(ctx, alice, false, 1); err != nil || len(lim.Items) != 1 || lim.Unread != 3 {
		t.Fatalf("Inbox(limit 1) = %+v, %v; want one item and 3 unread", lim, err)
	}

	// bob cannot ack alice's items; alice acks one, then the rest.
	if n, err := s.AckInbox(ctx, bob, []int64{page.Items[0].ID}, false); err != nil || n != 0 {
		t.Fatalf("AckInbox(bob, alice's item) = %d, %v; want 0", n, err)
	}
	seq := lastSeq(t, s)
	if n, err := s.AckInbox(ctx, alice, []int64{page.Items[0].ID}, false); err != nil || n != 1 {
		t.Fatalf("AckInbox(one) = %d, %v; want 1", n, err)
	}
	if lastSeq(t, s) != seq {
		t.Error("ack recorded an event; reading mail is not history")
	}
	if p, _ := s.Inbox(ctx, alice, false, 0); p.Unread != 2 || len(p.Items) != 2 {
		t.Fatalf("after one ack: %d unread, %d items; want 2, 2", p.Unread, len(p.Items))
	}
	if p, _ := s.Inbox(ctx, alice, true, 0); len(p.Items) != 3 || p.Items[0].ReadAt == nil {
		t.Fatalf("Inbox(all) = %+v; want 3 items, the first read", p.Items)
	}
	if n, err := s.AckInbox(ctx, alice2, nil, true); err != nil || n != 1 {
		t.Fatalf("AckInbox(alice2, all) = %d, %v; want 1 (alice2 cannot see alice's session item)", n, err)
	}
	if n, err := s.AckInbox(ctx, alice, nil, true); err != nil || n != 1 {
		t.Fatalf("AckInbox(alice, all) = %d, %v; want 1", n, err)
	}
	if p, _ := s.Inbox(ctx, alice, false, 0); p.Unread != 0 || len(p.Items) != 0 {
		t.Fatalf("after ack all: %+v", p)
	}

	for _, tc := range []struct {
		name string
		ids  []int64
		all  bool
	}{
		{"nothing to ack", nil, false},
		{"ids and all", []int64{1}, true},
		{"bad id", []int64{0}, false},
		{"too many", make([]int64, MaxInboxLimit+1), false},
	} {
		if _, err := s.AckInbox(ctx, alice, tc.ids, tc.all); !errors.Is(err, ErrInvalid) {
			t.Errorf("AckInbox(%s) = %v, want ErrInvalid", tc.name, err)
		}
	}
	if _, err := s.Inbox(ctx, alice, false, MaxInboxLimit+1); !errors.Is(err, ErrInvalid) {
		t.Errorf("Inbox(limit too big) = %v, want ErrInvalid", err)
	}
}

// A watch receives each new item for its principal and session as it is
// committed; its bounded queue overflows instead of blocking the writer.
func TestWatch(t *testing.T) {
	s, _ := clockStore(t)
	ctx := t.Context()
	register(t, s, bob)
	wb := s.Watch("bob", "sess-b")
	defer wb.Close()
	wa := s.Watch("alice", "sess-a")
	defer wa.Close()

	is := mustCreate(t, s, NewIssue{Title: "work"})
	if _, err := s.AddComment(ctx, alice, is.ID, "@bob one", ""); err != nil {
		t.Fatal(err)
	}
	<-wb.Ready()
	got, over := wb.Take()
	if over || len(got) != 1 || got[0].Kind != InboxMention || got[0].ID == 0 || got[0].Issue != is.ID {
		t.Fatalf("Take() = %+v, %v; want one mention with its id", got, over)
	}
	select {
	case <-wa.Ready():
		t.Errorf("alice's watch got bob's item")
	default:
	}

	// More items than the queue holds: the watch says it overflowed, once,
	// and gets nothing more until replaced.
	for range WatchQueue + 1 {
		if _, err := s.AddComment(ctx, alice, is.ID, "@bob again", ""); err != nil {
			t.Fatal(err)
		}
	}
	<-wb.Ready()
	if got, over := wb.Take(); !over || len(got) != 0 {
		t.Fatalf("Take() after overflow = %d items, overflow %v; want none and true", len(got), over)
	}
	if _, err := s.AddComment(ctx, alice, is.ID, "@bob after", ""); err != nil {
		t.Fatal(err)
	}
	select {
	case <-wb.Ready():
		got, over := wb.Take()
		t.Fatalf("an overflowed watch got %d items (overflow %v)", len(got), over)
	default:
	}
	w2 := s.Watch("bob", "other-session")
	defer w2.Close()
	if _, err := s.AddComment(ctx, alice, is.ID, "@bob fresh", ""); err != nil {
		t.Fatal(err)
	}
	<-w2.Ready()
	if got, over := w2.Take(); over || len(got) != 1 {
		t.Fatalf("a new watch Take() = %+v, %v; want one item", got, over)
	}
	w2.Close()
	if _, err := s.AddComment(ctx, alice, is.ID, "@bob closed", ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := w2.Take(); len(got) != 0 {
		t.Fatalf("a closed watch got %+v", got)
	}
}

// Structured handoff fields are stored with the note and returned by
// LastHandoff; each is checked against a strict pattern.
func TestStructuredHandoff(t *testing.T) {
	s, _ := clockStore(t)
	ctx := t.Context()
	is := mustCreate(t, s, NewIssue{Title: "work"})
	mustStart(t, s, alice, is.ID)
	f := HandoffFields{State: HandoffPartial, Next: "wire the CLI", Branch: "feature/tst-ab12-work",
		Worktree: "/home/alice/src/work", To: "bob"}
	if _, err := s.HandoffIssue(ctx, alice, is.ID, 0, HandoffNote{Note: "half done", HandoffFields: f}, false, ""); err != nil {
		t.Fatal(err)
	}
	h, err := s.LastHandoff(ctx, is.ID)
	if err != nil || h == nil || h.Body != "half done" || h.Author != "alice" || h.HandoffFields != f {
		t.Fatalf("LastHandoff = %+v, %v; want the note with %+v", h, err, f)
	}
	// A later plain handoff has no fields of its own.
	if _, err := s.HandoffIssue(ctx, alice, is.ID, 0, HandoffNote{Note: "later"}, false, ""); err != nil {
		t.Fatal(err)
	}
	if h, err := s.LastHandoff(ctx, is.ID); err != nil || h.Body != "later" || h.HandoffFields != (HandoffFields{}) {
		t.Fatalf("LastHandoff after a plain note = %+v, %v", h, err)
	}

	bad := []struct {
		name string
		h    HandoffNote
	}{
		{"unknown state", HandoffNote{Note: "n", HandoffFields: HandoffFields{State: "maybe"}}},
		{"next with a newline", HandoffNote{Note: "n", HandoffFields: HandoffFields{Next: "a\nb"}}},
		{"next too long", HandoffNote{Note: "n", HandoffFields: HandoffFields{Next: strings.Repeat("x", 501)}}},
		{"branch like an option", HandoffNote{Note: "n", HandoffFields: HandoffFields{Branch: "-D"}}},
		{"branch with a space", HandoffNote{Note: "n", HandoffFields: HandoffFields{Branch: "a b"}}},
		{"branch too long", HandoffNote{Note: "n", HandoffFields: HandoffFields{Branch: strings.Repeat("b", 256)}}},
		{"worktree with a control character", HandoffNote{Note: "n", HandoffFields: HandoffFields{Worktree: "/tmp/\x1b[2J"}}},
		{"worktree too long", HandoffNote{Note: "n", HandoffFields: HandoffFields{Worktree: "/" + strings.Repeat("w", 1024)}}},
		{"to who is not a principal", HandoffNote{Note: "n", HandoffFields: HandoffFields{To: "Bob Smith"}}},
		{"invalid UTF-8", HandoffNote{Note: "n", HandoffFields: HandoffFields{Next: "\xff"}}},
		{"fields without a note", HandoffNote{HandoffFields: HandoffFields{State: HandoffDone}}},
	}
	seq := lastSeq(t, s)
	for _, tc := range bad {
		if _, err := s.HandoffIssue(ctx, alice, is.ID, 0, tc.h, false, ""); !errors.Is(err, ErrInvalid) {
			t.Errorf("HandoffIssue(%s) = %v, want ErrInvalid", tc.name, err)
		}
		if _, _, err := s.FinishIssue(ctx, alice, is.ID, 0, Finish{Handoff: tc.h}); !errors.Is(err, ErrInvalid) {
			t.Errorf("FinishIssue(%s) = %v, want ErrInvalid", tc.name, err)
		}
	}
	if lastSeq(t, s) != seq {
		t.Error("refused handoffs wrote events")
	}
}
