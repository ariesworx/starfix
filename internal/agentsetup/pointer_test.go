package agentsetup

import (
	"strings"
	"testing"
)

var block = strings.Join(pointerLines(""), "\n") + "\n"

func TestPointer(t *testing.T) {
	crlf := strings.Join(pointerLines("\r"), "\n") + "\n"
	tests := []struct {
		name, in, want string
		result         Result
		// removed is what Remove leaves of want.
		removed string
	}{
		{name: "new file", want: block, result: Added},
		{name: "blank file", in: "\n\n", want: block, result: Added},
		{name: "appends after other content", in: "# Project\n\nRules.\n",
			want: "# Project\n\nRules.\n\n" + block, result: Added, removed: "# Project\n\nRules.\n"},
		{name: "no final newline", in: "# Project",
			want: "# Project\n\n" + block, result: Added, removed: "# Project\n"},
		{name: "replaces an old block in place", result: Updated,
			in:      "# P\n\n<!-- starfix:begin -->\nold words\n<!-- starfix:end -->\n\n## Later\n",
			want:    "# P\n\n" + block + "\n## Later\n",
			removed: "# P\n\n\n## Later\n"},
		{name: "keeps a CRLF file CRLF", in: "# P\r\n", result: Added,
			want: "# P\r\n\r\n" + crlf, removed: "# P\r\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, res, err := applyPointer([]byte(tc.in), markdownBlock)
			if err != nil || res != tc.result || string(out) != tc.want {
				t.Fatalf("apply: %s %v\n%q\nwant %s\n%q", res, err, out, tc.result, tc.want)
			}
			if again, res, err := applyPointer(out, markdownBlock); err != nil || res != Unchanged || string(again) != string(out) {
				t.Fatalf("second apply: %s %v\n%q", res, err, again)
			}
			if !pointerRegistered(out, markdownBlock) || pointerRegistered([]byte(tc.in), markdownBlock) {
				t.Fatal("pointerRegistered disagrees")
			}
			rm, res, err := removePointer(out, markdownBlock)
			if err != nil || res != Removed || string(rm) != tc.removed {
				t.Fatalf("remove: %s %v\n%q\nwant %q", res, err, rm, tc.removed)
			}
			if again, res, _ := removePointer(rm, markdownBlock); res != Unchanged || string(again) != string(rm) {
				t.Fatalf("second remove: %s", res)
			}
		})
	}
}

func TestPointerRefusesBrokenMarkers(t *testing.T) {
	for _, in := range []string{
		"<!-- starfix:begin -->\nno end\n",
		"stray\n<!-- starfix:end -->\n",
		"<!-- starfix:begin -->\n<!-- starfix:begin -->\n<!-- starfix:end -->\n",
	} {
		if _, _, err := applyPointer([]byte(in), markdownBlock); err == nil {
			t.Errorf("apply accepted %q", in)
		}
		if _, _, err := removePointer([]byte(in), markdownBlock); err == nil {
			t.Errorf("remove accepted %q", in)
		}
	}
}

func TestPointerBlockIsShort(t *testing.T) {
	if n := strings.Count(block, "\n"); n < 3 || n > 5 {
		t.Fatalf("pointer block is %d lines", n)
	}
	for _, w := range []string{"starfix", "prime", "start", "finish"} {
		if !strings.Contains(block, w) {
			t.Errorf("pointer block lacks %q", w)
		}
	}
}

func TestCursorRule(t *testing.T) {
	a := Agents["cursor"]
	var rule Target
	for _, tg := range a.Targets(false) {
		if tg.Kind == KindPointer {
			rule = tg
		}
	}
	if rule.Path != ".cursor/rules/starfix.mdc" {
		t.Fatalf("cursor pointer target %+v", rule)
	}
	out, res, err := rule.Apply(nil, DefaultEntry)
	if err != nil || res != Added || !strings.HasPrefix(string(out), "---\ndescription: ") ||
		!strings.Contains(string(out), "\nalwaysApply: true\n---\n") || !strings.HasSuffix(string(out), block) {
		t.Fatalf("new rule: %s %v\n%s", res, err, out)
	}
	if again, res, _ := rule.Apply(out, DefaultEntry); res != Unchanged || string(again) != string(out) {
		t.Fatalf("second apply: %s", res)
	}
	if _, res, _ := rule.Apply([]byte("---\nalwaysApply: false\n---\nold\n"), DefaultEntry); res != Updated {
		t.Fatalf("stale rule: %s", res)
	}
	if !rule.Registered(out, DefaultEntry) {
		t.Fatal("not registered")
	}
	// The whole file is starfix's: removing leaves nothing.
	if rm, res, err := rule.Remove(out, DefaultEntry); err != nil || res != Removed || len(rm) != 0 {
		t.Fatalf("remove: %s %v %q", res, err, rm)
	}
}

func TestOnlyBlockFileEmptiesOnRemove(t *testing.T) {
	out, _, _ := applyPointer(nil, markdownBlock)
	if rm, res, err := removePointer(out, markdownBlock); err != nil || res != Removed || len(rm) != 0 {
		t.Fatalf("%s %v %q", res, err, rm)
	}
}
