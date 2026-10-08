package cli

import (
	"bytes"
	"slices"
	"testing"

	"github.com/ariesworx/starfix/internal/proto"
)

func TestPrintFiles(t *testing.T) {
	tests := []struct {
		name  string
		files *proto.Files
		want  string
	}{
		{"none", nil, ""},
		{"paths and overlaps", &proto.Files{
			Paths: []proto.FilePath{{Path: "internal/store/", Source: proto.PathDeclared},
				{Path: "a b.go", Source: proto.PathCommit}},
			More:     3,
			Overlaps: []proto.Overlap{{ID: "sf-a1b2", By: "alice", Session: "s1"}, {ID: "sf-c3d4", By: "bob", Session: "s2"}}},
			"\nlikely files:\n  internal/store/ (declared)\n  a b.go\n  and 3 more\n" +
				"overlaps sf-a1b2, held by alice (s1)\noverlaps sf-c3d4, held by bob (s2)\n"},
		{"overlaps alone", &proto.Files{Overlaps: []proto.Overlap{{ID: "sf-a1b2", By: "alice", Session: "s1"}}},
			"\noverlaps sf-a1b2, held by alice (s1)\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var b bytes.Buffer
			printFiles(&b, tc.files)
			if got := b.String(); got != tc.want {
				t.Errorf("printFiles(%+v) =\n%q\nwant\n%q", tc.files, got, tc.want)
			}
		})
	}
}

func TestPrintReady(t *testing.T) {
	var b bytes.Buffer
	printReady(&b, []proto.Summary{{ID: "sf-a1b2", Priority: 1, Status: "open", Title: "free"},
		{ID: "sf-c3d4", Priority: 0, Status: "open", Title: "contested", Overlaps: []string{"sf-e5f6", "sf-g7h8"}}})
	want := "sf-a1b2  P1  open  free\nsf-c3d4  P0  open  contested  overlaps sf-e5f6, sf-g7h8\n"
	if got := b.String(); got != want {
		t.Errorf("printReady =\n%q\nwant\n%q", got, want)
	}
}

func TestDeclaredPaths(t *testing.T) {
	tests := []struct {
		in   []string
		want []string
	}{
		{nil, []string{}},
		{[]string{"./docs/x.md", "internal/store/"}, []string{"docs/x.md", "internal/store/"}},
	}
	for _, tc := range tests {
		if got := declaredPaths(tc.in); !slices.Equal(got, tc.want) || got == nil {
			t.Errorf("declaredPaths(%q) = %q, want %q (never nil)", tc.in, got, tc.want)
		}
	}
}
