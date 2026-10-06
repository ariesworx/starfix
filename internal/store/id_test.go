package store

import (
	"errors"
	"regexp"
	"testing"
)

func TestNewID(t *testing.T) {
	re := regexp.MustCompile(`^proj-[a-z2-7]{8}$`)
	seen := map[IssueID]bool{}
	for range 1000 {
		id, err := NewID("proj")
		if err != nil {
			t.Fatal(err)
		}
		if !re.MatchString(string(id)) {
			t.Fatalf("id %q has the wrong shape", id)
		}
		if err := id.Validate(); err != nil {
			t.Fatalf("own id rejected: %v", err)
		}
		if seen[id] {
			t.Fatalf("duplicate id %s", id)
		}
		seen[id] = true
	}
	for _, p := range []string{"", "Proj", "1abc", "a_b", "a-", "-a", "this-prefix-is-much-too-long-to-use"} {
		if _, err := NewID(p); !errors.Is(err, ErrInvalid) {
			t.Errorf("prefix %q accepted", p)
		}
	}
}

func TestValidateID(t *testing.T) {
	cases := []struct {
		id string
		ok bool
	}{
		{"sf-abcd2345", true},
		{"bd-a1b", true},      // imported bd hash id
		{"my-site-kch", true}, // hyphenated prefix
		{"bd-a1b.2", true},    // bd child id
		{"bd-a1b.2.10", true}, // nested child
		{"", false},
		{"noprefix", false},
		{"Upper-abc", false},
		{"sf-", false},
		{"sf-abc.", false},
		{"sf abc", false},
		{"sf-abc;drop", false},
	}
	for _, tc := range cases {
		if err := IssueID(tc.id).Validate(); (err == nil) != tc.ok {
			t.Errorf("Validate(%q) = %v, want ok=%v", tc.id, err, tc.ok)
		}
	}
}

func TestShortestUnique(t *testing.T) {
	cases := []struct {
		name string
		ids  []IssueID
		want map[IssueID]string
	}{
		{"single", []IssueID{"sf-abcdefgh"}, map[IssueID]string{"sf-abcdefgh": "sf-abcd"}},
		{"shared prefix",
			[]IssueID{"sf-abcdefgh", "sf-abcdxyzq", "sf-zzzzzzzz"},
			map[IssueID]string{"sf-abcdefgh": "sf-abcde", "sf-abcdxyzq": "sf-abcdx", "sf-zzzzzzzz": "sf-zzzz"}},
		{"long overlap",
			[]IssueID{"sf-aaaaaaab", "sf-aaaaaaac"},
			map[IssueID]string{"sf-aaaaaaab": "sf-aaaaaaab", "sf-aaaaaaac": "sf-aaaaaaac"}},
		{"different prefixes do not collide",
			[]IssueID{"a-abcdefgh", "b-abcdefgh"},
			map[IssueID]string{"a-abcdefgh": "a-abcd", "b-abcdefgh": "b-abcd"}},
		{"short bd ids stay whole", []IssueID{"bd-a1b"}, map[IssueID]string{"bd-a1b": "bd-a1b"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ShortestUnique(tc.ids)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v", got)
			}
			for id, w := range tc.want {
				if got[id] != w {
					t.Errorf("%s -> %q, want %q", id, got[id], w)
				}
			}
		})
	}
}
