package store

import (
	"errors"
	"testing"
	"time"
)

// hostile are the review's payloads (S-8, C-3, T-5): terminal escapes
// (OSC title, OSC 52 clipboard, CSI erase), NUL, DEL, a C1 CSI, bidi
// controls and marks, a carriage return, a line separator and an invalid
// byte. Every field refuses them all.
var hostile = []string{
	"\x1b]0;pwned\x07", "\x1b]52;c;cm0gLXJmIH4=\x07", "\x1b[2K", "\x00", "\x7f", "\u009b2J",
	"\u202e", "\u2066", "\u200e", "\u061c", "\r", "\u2028", "\xff",
}

func TestControlCharsRefused(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	// is has no acceptance items, so a close that got past validation
	// would succeed and fail the test; acc has one, to waive.
	is, err := s.CreateIssue(ctx, alice, NewIssue{Title: "target"})
	if err != nil {
		t.Fatal(err)
	}
	acc, err := s.CreateIssue(ctx, alice, NewIssue{Title: "checklist", Acceptance: "- [ ] one"})
	if err != nil {
		t.Fatal(err)
	}
	str := func(v string) *string { return &v }
	update := func(p IssuePatch) error {
		cur, err := s.GetIssue(ctx, is.ID)
		if err != nil {
			t.Fatal(err)
		}
		_, err = s.UpdateIssue(ctx, alice, is.ID, cur.Rev, p)
		return err
	}
	handoff := func(f HandoffFields) error {
		_, err := s.HandoffIssue(ctx, alice, is.ID, 0, HandoffNote{Note: "n", HandoffFields: f}, false, "", nil)
		return err
	}
	// Single-line fields refuse newlines and tabs too.
	line := []struct {
		name  string
		write func(v string) error
	}{
		{"title", func(v string) error { _, err := s.CreateIssue(ctx, alice, NewIssue{Title: "t" + v}); return err }},
		{"updated title", func(v string) error { return update(IssuePatch{Title: str("t" + v)}) }},
		{"assignee", func(v string) error {
			_, err := s.CreateIssue(ctx, alice, NewIssue{Title: "t", Assignee: "a" + v})
			return err
		}},
		{"updated assignee", func(v string) error { return update(IssuePatch{Assignee: str("a" + v)}) }},
		{"owner", func(v string) error {
			_, err := s.CreateIssue(ctx, alice, NewIssue{Title: "t", Owner: "o" + v})
			return err
		}},
		{"updated owner", func(v string) error { return update(IssuePatch{Owner: str("o" + v)}) }},
		{"label", func(v string) error {
			_, err := s.CreateIssue(ctx, alice, NewIssue{Title: "t", Labels: []string{"l" + v}})
			return err
		}},
		{"added label", func(v string) error { return s.AddLabel(ctx, alice, is.ID, "l"+v) }},
		{"close reason", func(v string) error { _, err := s.CloseIssue(ctx, alice, is.ID, 0, "r"+v); return err }},
		{"finish reason", func(v string) error {
			_, _, err := s.FinishIssue(ctx, alice, is.ID, 0, Finish{Reason: "r" + v})
			return err
		}},
		{"discovered title", func(v string) error {
			_, _, err := s.FinishIssue(ctx, alice, is.ID, 0, Finish{Discovered: []NewIssue{{Title: "t" + v}}})
			return err
		}},
		{"handoff next", func(v string) error { return handoff(HandoffFields{Next: "n" + v}) }},
		{"handoff worktree", func(v string) error { return handoff(HandoffFields{Worktree: "/w" + v}) }},
		{"handoff branch", func(v string) error { return handoff(HandoffFields{Branch: "b" + v}) }},
		{"handoff to", func(v string) error { return handoff(HandoffFields{To: "bob" + v}) }},
		{"waive reason", func(v string) error {
			_, err := s.Accept(ctx, alice, acc.ID, Acceptance{Waive: map[int]string{1: "r" + v}})
			return err
		}},
		{"actor session", func(v string) error {
			_, err := s.CreateIssue(ctx, Actor{Principal: "alice", Session: "s" + v, Machine: "m"}, NewIssue{Title: "t"})
			return err
		}},
		{"actor machine", func(v string) error {
			_, err := s.CreateIssue(ctx, Actor{Principal: "alice", Session: "s", Machine: "m" + v}, NewIssue{Title: "t"})
			return err
		}},
		{"actor principal", func(v string) error {
			_, err := s.CreateIssue(ctx, Actor{Principal: "alice" + v, Session: "s", Machine: "m"}, NewIssue{Title: "t"})
			return err
		}},
		{"digest by", func(v string) error {
			_, err := s.Digest(ctx, DigestFilter{Window: time.Hour, By: "b" + v})
			return err
		}},
	}
	for _, f := range line {
		for _, v := range append([]string{"\n", "\t"}, hostile...) {
			if err := f.write(v); !errors.Is(err, ErrInvalid) {
				t.Errorf("%s %q: err = %v, want %v", f.name, v, err, ErrInvalid)
			}
		}
	}
	// Multi-line fields take newlines and tabs, and refuse the rest.
	text := []struct {
		name  string
		write func(v string) error
	}{
		{"body", func(v string) error { _, err := s.CreateIssue(ctx, alice, NewIssue{Title: "t", Body: v}); return err }},
		{"design", func(v string) error { _, err := s.CreateIssue(ctx, alice, NewIssue{Title: "t", Design: v}); return err }},
		{"acceptance", func(v string) error {
			_, err := s.CreateIssue(ctx, alice, NewIssue{Title: "t", Acceptance: v})
			return err
		}},
		{"notes", func(v string) error { _, err := s.CreateIssue(ctx, alice, NewIssue{Title: "t", Notes: v}); return err }},
		{"updated body", func(v string) error { return update(IssuePatch{Body: str(v)}) }},
		{"updated design", func(v string) error { return update(IssuePatch{Design: str(v)}) }},
		{"updated acceptance", func(v string) error { return update(IssuePatch{Acceptance: str(v)}) }},
		{"updated notes", func(v string) error { return update(IssuePatch{Notes: str(v)}) }},
		{"comment", func(v string) error { _, err := s.AddComment(ctx, alice, is.ID, v, ""); return err }},
		{"handoff note", func(v string) error {
			_, err := s.HandoffIssue(ctx, alice, is.ID, 0, HandoffNote{Note: v}, false, "", nil)
			return err
		}},
	}
	for _, f := range text {
		if err := f.write("line one\n\tline two"); err != nil {
			t.Errorf("%s with a newline and a tab: %v", f.name, err)
		}
		for _, v := range hostile {
			if err := f.write("x" + v); !errors.Is(err, ErrInvalid) {
				t.Errorf("%s %q: err = %v, want %v", f.name, v, err, ErrInvalid)
			}
		}
	}
}
