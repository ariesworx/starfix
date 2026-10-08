package store

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestIssueAccountSetAndClear(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	is := mustCreate(t, s, NewIssue{Title: "billed", Account: "acme-2026"})
	if is.Account != "acme-2026" {
		t.Fatalf("created account = %q, want acme-2026", is.Account)
	}
	got, err := s.UpdateIssue(ctx, alice, is.ID, is.Rev, IssuePatch{Account: ptr("ops")})
	if err != nil || got.Account != "ops" {
		t.Fatalf("UpdateIssue(account ops) = %q, %v; want ops", got.Account, err)
	}
	got, err = s.UpdateIssue(ctx, alice, is.ID, got.Rev, IssuePatch{Account: ptr("")})
	if err != nil || got.Account != "" {
		t.Fatalf("UpdateIssue(account \"\") = %q, %v; want it cleared, to inherit", got.Account, err)
	}
	hist, err := s.History(ctx, is.ID)
	if err != nil {
		t.Fatal(err)
	}
	var b, a map[string]any
	_ = json.Unmarshal(hist[1].Before, &b)
	_ = json.Unmarshal(hist[1].After, &a)
	if hist[1].Op != OpIssueUpdate || b["account"] != "acme-2026" || a["account"] != "ops" {
		t.Errorf("update event = %s %s → %s, want account acme-2026 → ops", hist[1].Op, hist[1].Before, hist[1].After)
	}
}

func TestIssueAccountRefusesNonCodeNames(t *testing.T) {
	s := newStore(t)
	is := mustCreate(t, s, NewIssue{Title: "x"})
	for _, a := range []string{"Acme", "acme corp", "acme_corp", "-acme", "acme-", "a--b", "acme\u202e", strings.Repeat("a", 65)} {
		if _, err := s.CreateIssue(t.Context(), alice, NewIssue{Title: "x", Account: a}); !errors.Is(err, ErrInvalid) {
			t.Errorf("CreateIssue(account %q) = %v, want ErrInvalid", a, err)
		}
		if _, err := s.UpdateIssue(t.Context(), alice, is.ID, is.Rev, IssuePatch{Account: ptr(a)}); !errors.Is(err, ErrInvalid) {
			t.Errorf("UpdateIssue(account %q) = %v, want ErrInvalid", a, err)
		}
		if ValidAccount(a) {
			t.Errorf("ValidAccount(%q) = true", a)
		}
	}
	for _, a := range []string{"internal", "acme", "acme-2026", "7th-floor", strings.Repeat("a", 64)} {
		if !ValidAccount(a) {
			t.Errorf("ValidAccount(%q) = false", a)
		}
	}
}

// An issue another principal holds keeps its account: changing it is a
// change to the issue, which only the holder or an admin may make.
func TestIssueAccountGuarded(t *testing.T) {
	s := newStore(t)
	is := mustCreate(t, s, NewIssue{Title: "held"})
	is, _, err := s.StartIssue(t.Context(), bob, is.ID, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateIssue(t.Context(), alice, is.ID, is.Rev, IssuePatch{Account: ptr("acme")}); !errors.As(err, new(*ForbiddenError)) {
		t.Fatalf("alice sets the account of bob's issue: %v, want a ForbiddenError", err)
	}
}

func TestResolveAccountInherits(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	epic := mustCreate(t, s, NewIssue{Title: "epic", Type: TypeEpic, Account: "acme"})
	task := mustCreate(t, s, NewIssue{Title: "task", ParentID: epic.ID})
	sub := mustCreate(t, s, NewIssue{Title: "sub", ParentID: task.ID})
	own := mustCreate(t, s, NewIssue{Title: "own", ParentID: epic.ID, Account: "beta"})
	orphan := mustCreate(t, s, NewIssue{Title: "orphan"})
	tests := []struct {
		id      IssueID
		account string
		from    IssueID
	}{
		{epic.ID, "acme", epic.ID},
		{task.ID, "acme", epic.ID},
		{sub.ID, "acme", epic.ID},
		{own.ID, "beta", own.ID},
		{orphan.ID, "", ""}, // the server's account: setting applies
	}
	for _, tc := range tests {
		a, from, err := s.ResolveAccount(ctx, tc.id)
		if err != nil || a != tc.account || from != tc.from {
			t.Errorf("ResolveAccount(%s) = %q from %q, %v; want %q from %q", tc.id, a, from, err, tc.account, tc.from)
		}
	}
	if _, _, err := s.ResolveAccount(ctx, "tst-nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("ResolveAccount(missing) = %v, want ErrNotFound", err)
	}
}
