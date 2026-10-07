package store

import (
	"slices"
	"testing"
)

func TestTitleTokens(t *testing.T) {
	tests := []struct {
		title string
		want  []string
	}{
		{"", nil},
		{"Fix the login!", []string{"login"}},
		{"Login fails with expired token", []string{"login", "fail", "expir", "token"}},
		{"Token expiry: tokens expire", []string{"token", "expir"}},
		{"Add dark-mode to the UI", []string{"dark", "mod", "ui"}},
		{"v2 API, again; API", []string{"v2", "api", "again"}},
		{"a b c", nil},
	}
	for _, tc := range tests {
		if got := titleTokens(tc.title); !slices.Equal(got, tc.want) {
			t.Errorf("titleTokens(%q) = %q, want %q", tc.title, got, tc.want)
		}
	}
}

func TestSimilarClosed(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	closed := func(title string) Issue {
		t.Helper()
		is := mustCreate(t, s, NewIssue{Title: title})
		if _, err := s.CloseIssue(ctx, alice, is.ID, 0, ""); err != nil {
			t.Fatal(err)
		}
		return is
	}
	best := closed("Login fails with an expired token")
	next := closed("Token expiry breaks login page")
	weak := closed("Login page colors") // one word in common of five: below the bar
	closed("Add dark mode")
	mustCreate(t, s, NewIssue{Title: "Login token expired again"}) // open: never similar
	self := closed("Expired token on login")

	tests := []struct {
		name    string
		title   string
		exclude IssueID
		limit   int
		want    []IssueID
	}{
		{"ranked by overlap", "Login token expired", "", 3, []IssueID{self.ID, best.ID, next.ID}},
		{"excludes the issue itself", "Expired token on login", self.ID, 3, []IssueID{best.ID, next.ID}},
		{"limit", "Login token expired", "", 1, []IssueID{self.ID}},
		{"nothing in common", "Rotate the backup keys", "", 3, nil},
		{"only stopwords", "the and of", "", 3, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.SimilarClosed(ctx, tc.title, tc.exclude, tc.limit)
			if err != nil {
				t.Fatal(err)
			}
			var ids []IssueID
			for _, g := range got {
				ids = append(ids, g.ID)
				if g.Title == "" {
					t.Errorf("%s has no title", g.ID)
				}
			}
			if !slices.Equal(ids, tc.want) {
				t.Errorf("SimilarClosed(%q) = %v, want %v (weak %s)", tc.title, ids, tc.want, weak.ID)
			}
		})
	}
}
