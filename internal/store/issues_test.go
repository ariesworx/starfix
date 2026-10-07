package store

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestCreateIdempotent(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()

	first, err := s.CreateIssue(ctx, alice, NewIssue{Title: "once", IdempotencyKey: "k1", Labels: []string{"area:db"}})
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.CreateIssue(ctx, alice, NewIssue{Title: "once", IdempotencyKey: "k1", Labels: []string{"area:db"}})
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != first.ID || again.Rev != first.Rev {
		t.Errorf("retry returned %s rev %d, want %s rev %d", again.ID, again.Rev, first.ID, first.Rev)
	}
	if len(again.Labels) != 1 || again.Labels[0] != "area:db" {
		t.Errorf("labels = %v", again.Labels)
	}

	cases := []struct {
		name string
		in   NewIssue
		want error
	}{
		{"key reused for another id", NewIssue{Title: "once", IdempotencyKey: "k1", ID: "tst-other", Labels: []string{"area:db"}}, ErrConflict},
		{"key reused without the labels", NewIssue{Title: "once", IdempotencyKey: "k1"}, ErrConflict},
		{"client id taken", NewIssue{Title: "x", ID: first.ID}, ErrExists},
		{"bad id", NewIssue{Title: "x", ID: "Not An ID"}, ErrInvalid},
		{"empty title", NewIssue{Title: " "}, ErrInvalid},
		{"bad priority", NewIssue{Title: "x", Priority: prio(7)}, ErrInvalid},
		{"bad type", NewIssue{Title: "x", Type: "story"}, ErrInvalid},
		{"created closed", NewIssue{Title: "x", Status: StatusClosed}, ErrInvalid},
		{"missing parent", NewIssue{Title: "x", ParentID: "tst-nope"}, ErrNotFound},
		{"bad metadata", NewIssue{Title: "x", Metadata: json.RawMessage(`{`)}, ErrInvalid},
		{"bad label", NewIssue{Title: "x", Labels: []string{"two words"}}, ErrInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.CreateIssue(ctx, alice, tc.in); !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}

	creates := 0
	evs, err := s.Events(ctx, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		if e.Op == OpIssueCreate {
			creates++
		}
	}
	if creates != 1 {
		t.Errorf("%d create events, want 1", creates)
	}
}

// TestCreateIdempotentConcurrent retries one create from two store
// processes at once; one issue and one event result.
func TestCreateIdempotentConcurrent(t *testing.T) {
	dsn := newDSN(t)
	stores := []*Store{openStore(t, dsn, Options{}), openStore(t, dsn, Options{})}
	const n = 10
	ids := make([]IssueID, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			is, err := stores[i%2].CreateIssue(t.Context(), alice, NewIssue{Title: "dup", IdempotencyKey: "same"})
			if err != nil {
				t.Errorf("create %d: %v", i, err)
				return
			}
			ids[i] = is.ID
		})
	}
	wg.Wait()
	for _, id := range ids[1:] {
		if id != ids[0] {
			t.Fatalf("ids differ: %v", ids)
		}
	}
	page, err := stores[0].List(t.Context(), Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Issues) != 1 {
		t.Errorf("%d issues, want 1", len(page.Issues))
	}
	assertGapless(t, stores[0])
}

func TestEventPerMutation(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	a := mustCreate(t, s, NewIssue{Title: "a"})
	b := mustCreate(t, s, NewIssue{Title: "b"})

	rev := a.Rev
	cases := []struct {
		name   string
		do     func() error
		op     Op // empty: expect no event
		target IssueID
	}{
		{"create", func() error { _, err := s.CreateIssue(ctx, alice, NewIssue{Title: "c"}); return err }, OpIssueCreate, ""},
		{"update", func() error {
			is, err := s.UpdateIssue(ctx, alice, a.ID, rev, IssuePatch{Title: ptr("a2"), Assignee: ptr("bob")})
			rev = is.Rev
			return err
		}, OpIssueUpdate, a.ID},
		{"empty update", func() error { _, err := s.UpdateIssue(ctx, alice, a.ID, rev, IssuePatch{}); return err }, "", ""},
		{"add label", func() error { return s.AddLabel(ctx, alice, a.ID, "x") }, OpLabelAdd, a.ID},
		{"add label again", func() error { return s.AddLabel(ctx, alice, a.ID, "x") }, "", ""},
		{"remove label", func() error { return s.RemoveLabel(ctx, alice, a.ID, "x") }, OpLabelRemove, a.ID},
		{"remove absent label", func() error { return s.RemoveLabel(ctx, alice, a.ID, "x") }, "", ""},
		{"add dep", func() error { return s.AddDep(ctx, alice, a.ID, b.ID, DepBlocks) }, OpDepAdd, a.ID},
		{"add dep again", func() error { return s.AddDep(ctx, alice, a.ID, b.ID, DepBlocks) }, "", ""},
		{"remove dep", func() error { return s.RemoveDep(ctx, alice, a.ID, b.ID, DepBlocks) }, OpDepRemove, a.ID},
		{"remove absent dep", func() error { return s.RemoveDep(ctx, alice, a.ID, b.ID, DepBlocks) }, "", ""},
		{"comment", func() error { _, err := s.AddComment(ctx, alice, a.ID, "hello", ""); return err }, OpCommentAdd, a.ID},
		{"close", func() error { _, err := s.CloseIssue(ctx, alice, a.ID, 0, "done"); return err }, OpIssueClose, a.ID},
		{"reopen", func() error { _, err := s.ReopenIssue(ctx, alice, a.ID, 0); return err }, OpIssueReopen, a.ID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := lastSeq(t, s)
			if err := tc.do(); err != nil {
				t.Fatal(err)
			}
			evs, err := s.Events(ctx, before, 10)
			if err != nil {
				t.Fatal(err)
			}
			if tc.op == "" {
				if len(evs) != 0 {
					t.Fatalf("no-op wrote %d events", len(evs))
				}
				return
			}
			if len(evs) != 1 {
				t.Fatalf("%d events, want 1", len(evs))
			}
			e := evs[0]
			if e.Op != tc.op || (tc.target != "" && IssueID(e.Target) != tc.target) {
				t.Errorf("event %s on %s, want %s on %s", e.Op, e.Target, tc.op, tc.target)
			}
			if e.Actor != alice {
				t.Errorf("actor = %+v", e.Actor)
			}
		})
	}
	assertGapless(t, s)

	h, err := s.History(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	var upd Event
	for _, e := range h {
		if e.Op == OpIssueUpdate {
			upd = e
		}
	}
	var before, after map[string]any
	if err := json.Unmarshal(upd.Before, &before); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(upd.After, &after); err != nil {
		t.Fatal(err)
	}
	if len(before) != 2 || before["title"] != "a" || before["assignee"] != nil ||
		after["title"] != "a2" || after["assignee"] != "bob" {
		t.Errorf("update diff before=%v after=%v; want only title and assignee", before, after)
	}
}

func TestUpdateCloseReopen(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	is := mustCreate(t, s, NewIssue{Title: "t", Assignee: "alice", Metadata: json.RawMessage(`{"k":1}`)})

	if _, err := s.UpdateIssue(ctx, alice, is.ID, 0, IssuePatch{Title: ptr("x")}); !errors.Is(err, ErrInvalid) {
		t.Errorf("rev 0: %v, want ErrInvalid", err)
	}
	if _, err := s.UpdateIssue(ctx, alice, is.ID, is.Rev+1, IssuePatch{Title: ptr("x")}); !errors.Is(err, ErrConflict) {
		t.Errorf("stale rev: %v, want ErrConflict", err)
	}
	if _, err := s.UpdateIssue(ctx, alice, "tst-missing", 1, IssuePatch{Title: ptr("x")}); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing: %v, want ErrNotFound", err)
	}
	if _, err := s.UpdateIssue(ctx, alice, is.ID, is.Rev, IssuePatch{Status: ptr(StatusClosed)}); !errors.Is(err, ErrInvalid) {
		t.Errorf("status closed via update: %v, want ErrInvalid", err)
	}

	future := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
	up, err := s.UpdateIssue(ctx, alice, is.ID, is.Rev, IssuePatch{
		Assignee: ptr(""), DeferUntil: &future, Metadata: json.RawMessage("null"), Status: ptr(StatusInProgress),
	})
	if err != nil {
		t.Fatal(err)
	}
	if up.Assignee != "" || up.Metadata != nil || up.Status != StatusInProgress || up.DeferUntil == nil || !up.DeferUntil.Equal(future) {
		t.Errorf("after update: %+v", up)
	}
	up, err = s.UpdateIssue(ctx, alice, is.ID, up.Rev, IssuePatch{DeferUntil: &time.Time{}})
	if err != nil {
		t.Fatal(err)
	}
	if up.DeferUntil != nil {
		t.Errorf("defer_until not cleared")
	}

	closed, err := s.CloseIssue(ctx, bob, is.ID, 0, "fixed")
	if err != nil {
		t.Fatal(err)
	}
	if closed.Status != StatusClosed || closed.ClosedAt == nil || closed.CloseReason != "fixed" || closed.Rev != up.Rev+1 {
		t.Errorf("closed: %+v", closed)
	}
	if _, err := s.CloseIssue(ctx, bob, is.ID, 0, ""); !errors.Is(err, ErrInvalid) {
		t.Errorf("double close: %v, want ErrInvalid", err)
	}
	if _, err := s.ReopenIssue(ctx, bob, is.ID, up.Rev); !errors.Is(err, ErrConflict) {
		t.Errorf("reopen at stale rev: %v, want ErrConflict", err)
	}
	re, err := s.ReopenIssue(ctx, bob, is.ID, closed.Rev)
	if err != nil {
		t.Fatal(err)
	}
	if re.Status != StatusOpen || re.ClosedAt != nil || re.CloseReason != "" {
		t.Errorf("reopened: %+v", re)
	}
}

func TestComments(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	is := mustCreate(t, s, NewIssue{})
	for _, body := range []string{"one", "two"} {
		if _, err := s.AddComment(ctx, bob, is.ID, body, ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.AddComment(ctx, bob, "tst-missing", "x", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("comment on missing issue: %v", err)
	}
	cs, err := s.Comments(ctx, is.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 2 || cs[0].Author != "bob" || cs[0].Session != bob.Session {
		t.Errorf("comments = %+v", cs)
	}
}

func TestWriteGuards(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	is := mustCreate(t, s, NewIssue{})
	cases := []struct {
		name string
		fn   func(*wtx) error
	}{
		{"update without write_id", func(w *wtx) error {
			_, err := w.exec(ctx, `UPDATE issues SET title = 'x', rev = rev + 1 WHERE id = ?`, string(is.ID))
			return err
		}},
		{"mutation without event", func(w *wtx) error {
			_, err := w.exec(ctx, `INSERT INTO labels (issue_id, label, created_at) VALUES (?, 'x', ?)`, string(is.ID), w.now)
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := s.write(ctx, alice, tc.fn); err == nil {
				t.Fatal("write succeeded, want refusal")
			}
		})
	}
	if err := s.write(ctx, Actor{}, func(*wtx) error { return nil }); !errors.Is(err, ErrInvalid) {
		t.Errorf("empty actor: %v, want ErrInvalid", err)
	}
}
