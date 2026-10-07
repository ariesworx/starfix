package safetext

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// Hostile payloads from the security review (C-2, C-3, S-8, T-5).
const (
	osc52    = "\x1b]52;c;cm0gLXJmIH4=\x07"                                // writes the clipboard
	osc8     = "\x1b]8;;https://evil.example.com\x1b\\click\x1b]8;;\x1b\\" // disguised link
	csiErase = "\x1b[2K\rfake line"                                        // rewrites the line
	title    = "ok\x1b]0;pwned\x07\x1b[2J\rfake line\nP0 urgent"           // T-5
	c1CSI    = "m\u009b2J"                                                 // C1 CSI
	bidi     = "/home/alice/\u202esecret"                                  // right-to-left override
)

func TestUnsafe(t *testing.T) {
	for _, r := range []rune{0x00, 0x07, '\t', '\n', '\r', 0x1b, 0x1f, 0x7f, 0x80, 0x9b, 0x9f,
		0x061c, 0x200e, 0x200f, 0x202a, 0x202e, 0x2066, 0x2069, 0x2028, 0x2029} {
		if !Unsafe(r) {
			t.Errorf("Unsafe(%U) = false, want true", r)
		}
	}
	for _, r := range []rune{' ', 'a', '~', 0xa0, 0xe9, 0x2014, 0x2026, 0x4e2d, 0x1f600, 0x200d, 0x2065, 0x206a} {
		if Unsafe(r) {
			t.Errorf("Unsafe(%U) = true, want false", r)
		}
	}
}

func TestValid(t *testing.T) {
	tests := []struct {
		in         string
		line, text bool
	}{
		{"", true, true},
		{"Fix login — café 中文 😀", true, true},
		{"two\nlines\tand a tab", false, true},
		{"crlf\r\n", false, false},
		{title, false, false},
		{osc52, false, false},
		{c1CSI, false, false},
		{bidi, false, false},
		{"a\x7fb", false, false},
		{"\x00nul", false, false},
		{"bad \xff utf-8", false, false},
		{"mark\u200e", false, false},
	}
	for _, tc := range tests {
		if got := ValidLine(tc.in); got != tc.line {
			t.Errorf("ValidLine(%q) = %v, want %v", tc.in, got, tc.line)
		}
		if got := ValidText(tc.in); got != tc.text {
			t.Errorf("ValidText(%q) = %v, want %v", tc.in, got, tc.text)
		}
	}
}

func TestEscape(t *testing.T) {
	tests := []struct {
		in, line, text string
	}{
		{"plain — café", "plain — café", "plain — café"},
		{"a\nb\tc", `a\nb\tc`, "a\nb\tc"},
		{title, `ok\x1b]0;pwned\x07\x1b[2J\rfake line\nP0 urgent`, "ok\\x1b]0;pwned\\x07\\x1b[2J\\rfake line\nP0 urgent"},
		{c1CSI, `m\u009b2J`, `m\u009b2J`},
		{bidi, `/home/alice/\u202esecret`, `/home/alice/\u202esecret`},
		{"a\x7fb\x00", `a\x7fb\x00`, `a\x7fb\x00`},
		{"bad \xff", `bad \xff`, `bad \xff`},
		{"sep\u2028x", `sep\u2028x`, `sep\u2028x`},
	}
	for _, tc := range tests {
		if got := Line(tc.in); got != tc.line {
			t.Errorf("Line(%q) = %q, want %q", tc.in, got, tc.line)
		}
		if got := Text(tc.in); got != tc.text {
			t.Errorf("Text(%q) = %q, want %q", tc.in, got, tc.text)
		}
		if !ValidLine(Line(tc.in)) || !ValidText(Text(tc.in)) {
			t.Errorf("escaped %q is still unsafe", tc.in)
		}
	}
}

func TestClean(t *testing.T) {
	tests := []struct {
		in, line, text string
	}{
		{"plain", "plain", "plain"},
		{"two\r\nlines\n\nthree", "two lines three", "two\nlines\n\nthree"},
		{title, "ok]0;pwned[2J fake line P0 urgent", "ok]0;pwned[2Jfake line\nP0 urgent"},
		{bidi, "/home/alice/secret", "/home/alice/secret"},
		{"tab\there", "tab here", "tab\there"},
		{"bad \xff", "bad \ufffd", "bad \ufffd"},
	}
	for _, tc := range tests {
		if got := CleanLine(tc.in); got != tc.line {
			t.Errorf("CleanLine(%q) = %q, want %q", tc.in, got, tc.line)
		}
		if got := CleanText(tc.in); got != tc.text {
			t.Errorf("CleanText(%q) = %q, want %q", tc.in, got, tc.text)
		}
		if !ValidLine(CleanLine(tc.in)) || !ValidText(CleanText(tc.in)) {
			t.Errorf("cleaned %q is still unsafe", tc.in)
		}
	}
}

func TestWriter(t *testing.T) {
	in := "title: " + title + "\n" + osc52 + osc8 + csiErase + "\n" + c1CSI + " " + bidi + " 中文 \xff\n"
	want := Text(in)
	// Every split point, so a rune or escape cut across two writes still
	// comes out escaped.
	for i := range len(in) + 1 {
		var b bytes.Buffer
		w := NewWriter(&b)
		for _, part := range []string{in[:i], in[i:]} {
			if n, err := w.Write([]byte(part)); err != nil || n != len(part) {
				t.Fatalf("Write(%q) = %d, %v", part, n, err)
			}
		}
		if err := w.Flush(); err != nil {
			t.Fatal(err)
		}
		if got := b.String(); got != want {
			t.Fatalf("split at %d: wrote %q, want %q", i, got, want)
		}
		for _, raw := range []string{"\x1b", "\x07", "\r", "\u009b", "\u202e", "\xff"} {
			if strings.Contains(b.String(), raw) {
				t.Fatalf("split at %d: %q reached the output raw", i, raw)
			}
		}
	}
}

// JSON keeps a document valid and equal in value, with no raw unsafe rune.
func TestJSON(t *testing.T) {
	v := map[string]string{"title": title + osc52 + c1CSI + bidi + "\u2028\x7f"}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	got := JSON(b)
	var back map[string]string
	if err := json.Unmarshal(got, &back); err != nil {
		t.Fatalf("JSON(%s) = %s, not valid JSON: %v", b, got, err)
	}
	if back["title"] != v["title"] {
		t.Errorf("JSON changed the value: %q, want %q", back["title"], v["title"])
	}
	if !ValidLine(string(got)) {
		t.Errorf("JSON(%s) = %q still has unsafe runes", b, got)
	}
}
