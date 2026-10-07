package store

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// S-5: comments and history come a page at a time, newest page first, so
// no reply grows past the frame limit however much an issue collects.
func TestCommentsPage(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	is := mustCreate(t, s, NewIssue{Title: "talk"})
	for i := 1; i <= 7; i++ {
		if _, err := s.AddComment(ctx, bob, is.ID, fmt.Sprintf("c%d", i), ""); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	var before Cursor
	for page := 0; ; page++ {
		p, err := s.CommentsPage(ctx, is.ID, before, 3)
		if err != nil {
			t.Fatal(err)
		}
		if p.Total != 7 {
			t.Errorf("page %d: Total = %d, want 7", page, p.Total)
		}
		var bodies []string
		for _, c := range p.Comments {
			bodies = append(bodies, c.Body)
		}
		got = append([]string{strings.Join(bodies, " ")}, got...)
		if p.Earlier == "" {
			break
		}
		before = p.Earlier
		if page > 3 {
			t.Fatal("paging does not end")
		}
	}
	if want := []string{"c1", "c2 c3 c4", "c5 c6 c7"}; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("pages, oldest first = %q, want %q", got, want)
	}
	if _, err := s.CommentsPage(ctx, is.ID, "not a cursor", 3); !errors.Is(err, ErrInvalid) {
		t.Errorf("CommentsPage(bad cursor) = %v, want ErrInvalid", err)
	}
}

func TestPagesStayUnderByteBudget(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	is := mustCreate(t, s, NewIssue{Title: "flood"})
	body := strings.Repeat("D", maxText)
	n := MaxPageBytes/maxText + 5
	for range n {
		if _, err := s.AddComment(ctx, bob, is.ID, body, ""); err != nil {
			t.Fatal(err)
		}
	}
	p, err := s.CommentsPage(ctx, is.ID, "", MaxPage)
	if err != nil {
		t.Fatal(err)
	}
	size := 0
	for _, c := range p.Comments {
		size += len(c.Body)
	}
	if size > MaxPageBytes || p.Earlier == "" || len(p.Comments) == 0 {
		t.Errorf("page of %d comments holds %d bytes, earlier %q; want at most %d bytes and a cursor", len(p.Comments), size, p.Earlier, MaxPageBytes)
	}
	h, err := s.HistoryPage(ctx, is.ID, "", MaxPage)
	if err != nil {
		t.Fatal(err)
	}
	if h.Total != n+1 || len(h.Events) == 0 {
		t.Errorf("history page: %d events of %d", len(h.Events), h.Total)
	}
}

func TestHistoryPage(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	is := mustCreate(t, s, NewIssue{Title: "v0"})
	for i := 1; i <= 4; i++ {
		var err error
		if is, err = s.UpdateIssue(ctx, alice, is.ID, is.Rev, IssuePatch{Title: ptr(fmt.Sprintf("v%d", i))}); err != nil {
			t.Fatal(err)
		}
	}
	p1, err := s.HistoryPage(ctx, is.ID, "", 2)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := s.HistoryPage(ctx, is.ID, p1.Earlier, 2)
	if err != nil {
		t.Fatal(err)
	}
	p3, err := s.HistoryPage(ctx, is.ID, p2.Earlier, 2)
	if err != nil {
		t.Fatal(err)
	}
	seqs := func(evs []Event) string {
		var out []string
		for _, e := range evs {
			out = append(out, fmt.Sprint(e.Seq-p3.Events[0].Seq+1))
		}
		return strings.Join(out, ",")
	}
	if got := seqs(p3.Events) + "|" + seqs(p2.Events) + "|" + seqs(p1.Events); got != "1|2,3|4,5" || p3.Earlier != "" || p1.Total != 5 {
		t.Errorf("history pages = %s (earlier %q, total %d), want 1|2,3|4,5 and no earlier page", got, p3.Earlier, p1.Total)
	}
}

func TestWhoPage(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	for i := range 5 {
		if err := s.TouchAgent(ctx, Actor{Principal: fmt.Sprintf("p%d", i), Session: "s", Machine: "m"}, ""); err != nil {
			t.Fatal(err)
		}
	}
	as, more, err := s.WhoPage(ctx, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(as) != 2 || more != 3 {
		t.Errorf("WhoPage(limit 2) = %d agents, %d more; want 2 and 3", len(as), more)
	}
}
