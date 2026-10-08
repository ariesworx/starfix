package proto

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestCheckPath(t *testing.T) {
	tests := []struct {
		name string
		path string
		ok   bool
	}{
		{"file", "internal/store/paths.go", true},
		{"top-level file", "README.md", true},
		{"directory prefix", "internal/store/", true},
		{"space", "docs/design notes/a b.md", true},
		{"unicode", "docs/café/naïve.md", true},
		{"dotfile", ".github/workflows/ci.yml", true},
		{"dots inside a name", "a/b..c/d...e", true},
		{"longest", strings.Repeat("a", MaxPathLen), true},
		{"empty", "", false},
		{"too long", strings.Repeat("a", MaxPathLen+1), false},
		{"absolute", "/etc/passwd", false},
		{"root", "/", false},
		{"parent segment", "a/../b", false},
		{"leading parent", "../a", false},
		{"trailing parent", "a/..", false},
		{"dot segment", "a/./b", false},
		{"leading dot", "./a", false},
		{"empty segment", "a//b", false},
		{"backslash", `a\b`, false},
		{"windows drive", `C:\x`, false},
		{"nul", "a\x00b", false},
		{"newline", "a\nb", false},
		{"tab", "a\tb", false},
		{"escape", "a\x1b[31m", false},
		{"bidi override", "a\u202eb", false},
		{"zero width joiner", "a\u200db", false},
		{"no-break space", "a\u00a0b", false},
		{"invalid utf-8", "a\xffb", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := CheckPath(tc.path); (err == nil) != tc.ok {
				t.Errorf("CheckPath(%q) = %v, want ok %v", tc.path, err, tc.ok)
			}
		})
	}
}

// FuzzCheckPath checks that whatever CheckPath accepts is repo-relative,
// one line of printable UTF-8, and within MaxPathLen.
func FuzzCheckPath(f *testing.F) {
	for _, s := range []string{"a/b.go", "dir/", "../x", "/abs", "a//b", "a b/é", "a\x00", "a\u202e"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, p string) {
		if CheckPath(p) != nil {
			return
		}
		switch {
		case len(p) == 0 || len(p) > MaxPathLen:
			t.Fatalf("CheckPath(%q) = nil for length %d", p, len(p))
		case !utf8.ValidString(p):
			t.Fatalf("CheckPath(%q) = nil for invalid UTF-8", p)
		case strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\\\x00\n\r\t"):
			t.Fatalf("CheckPath(%q) = nil for an absolute path or a control character", p)
		}
		for seg := range strings.SplitSeq(strings.TrimSuffix(p, "/"), "/") {
			if seg == "" || seg == "." || seg == ".." {
				t.Fatalf("CheckPath(%q) = nil with segment %q", p, seg)
			}
		}
	})
}
