package store

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"
)

// A watch made with WatchEvents also receives each event committed on an
// issue, as history records it but without its states. A plain watch gets
// none; usage, which is no issue's, is not pushed, nor is a change that
// rolls back.
func TestWatchEvents(t *testing.T) {
	s, clk := clockStore(t)
	ctx := t.Context()
	we := s.WatchEvents("bob", "sess-b")
	defer we.Close()
	plain := s.Watch("bob", "sess-x")
	defer plain.Close()

	is := mustCreate(t, s, NewIssue{Title: "work"})
	mustStart(t, s, alice, is.ID)
	if _, err := s.AddUsage(ctx, alice, []UsageRecord{req("r1", clk.now(), 1, 1)}); err != nil {
		t.Fatal(err)
	}
	s.beforeCommit = func(context.Context) error { return errors.New("injected commit failure") }
	_, err := s.AddComment(ctx, alice, is.ID, "rolled back", "")
	s.beforeCommit = nil
	if err == nil {
		t.Fatal("AddComment with a failing commit succeeded")
	}

	hist, err := s.History(ctx, is.ID)
	if err != nil {
		t.Fatal(err)
	}
	var want []Event
	for _, e := range hist {
		e.Before, e.After = nil, nil
		want = append(want, e)
	}
	<-we.Ready()
	items, got, over := we.Take()
	if over || len(items) != 0 || !reflect.DeepEqual(got, want) {
		t.Errorf("Take() = %d items, events %+v, overflow %v; want no items and events %+v", len(items), got, over, want)
	}
	select {
	case <-plain.Ready():
		_, evs, _ := plain.Take()
		t.Errorf("a plain watch was offered %+v", evs)
	default:
	}
}

// Events count against the same bounded queue as items: a watch that
// falls behind overflows, once, and receives nothing more.
func TestWatchEventsOverflow(t *testing.T) {
	s := newStore(t)
	we := s.WatchEvents("bob", "sess-b")
	defer we.Close()
	for range WatchQueue + 1 {
		mustCreate(t, s, NewIssue{Title: "work"})
	}
	<-we.Ready()
	if items, evs, over := we.Take(); !over || len(items)+len(evs) != 0 {
		t.Fatalf("Take() after overflow = %d items, %d events, overflow %v; want none and true", len(items), len(evs), over)
	}
	mustCreate(t, s, NewIssue{Title: "after"})
	select {
	case <-we.Ready():
		_, evs, _ := we.Take()
		t.Fatalf("an overflowed watch got %d events", len(evs))
	default:
	}
}

// ActiveClaims lists every live claim, longest held first, up to a limit,
// and counts the rest; a lapsed or released claim holds nothing.
func TestActiveClaims(t *testing.T) {
	s, clk := clockStore(t)
	ctx := t.Context()
	one, two, lapsed := mustCreate(t, s, NewIssue{Title: "one"}), mustCreate(t, s, NewIssue{Title: "two"}),
		mustCreate(t, s, NewIssue{Title: "lapsed"})
	if _, _, err := s.StartIssue(ctx, alice, lapsed.ID, time.Minute, false); err != nil {
		t.Fatal(err)
	}
	clk.add(time.Minute)
	mustStart(t, s, alice, one.ID)
	clk.add(time.Minute)
	mustStart(t, s, bob, two.ID)

	type held struct {
		issue     IssueID
		holder    Actor
		claimedAt time.Time
	}
	get := func(limit int) ([]held, int) {
		t.Helper()
		cs, more, err := s.ActiveClaims(ctx, limit)
		if err != nil {
			t.Fatalf("ActiveClaims(%d): %v", limit, err)
		}
		var out []held
		for _, c := range cs {
			out = append(out, held{c.Issue, c.Holder, c.ClaimedAt})
		}
		return out, more
	}
	start := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	all := []held{{one.ID, alice, start.Add(time.Minute)}, {two.ID, bob, start.Add(2 * time.Minute)}}
	if got, more := get(0); !slices.Equal(got, all) || more != 0 {
		t.Errorf("ActiveClaims(0) = %+v, more %d; want %+v, more 0", got, more, all)
	}
	if got, more := get(1); !slices.Equal(got, all[:1]) || more != 1 {
		t.Errorf("ActiveClaims(1) = %+v, more %d; want %+v, more 1", got, more, all[:1])
	}
}
