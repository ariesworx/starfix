package board

import (
	"testing"
	"unicode"
)

func TestRuneWidth(t *testing.T) {
	tests := []struct {
		r    rune
		want int
	}{
		{'a', 1}, {'…', 1}, {'é', 1},
		{'中', 2}, {'あ', 2}, {'한', 2}, {'\u3000', 2}, {'Ａ', 2}, // ideograph, kana, hangul, ideographic space, fullwidth A
		{'😀', 2}, {'🚀', 2}, // emoji presentation
		{'ｱ', 1},                                                                  // halfwidth katakana
		{'\u0301', 0}, {'\u20dd', 0}, {'\u200d', 0}, {'\u200b', 0}, {'\ufeff', 0}, // Mn, Me, ZWJ, ZWSP, BOM (Cf)
	}
	for _, tc := range tests {
		if got := runeWidth(tc.r); got != tc.want {
			t.Errorf("runeWidth(%U %q) = %d, want %d", tc.r, tc.r, got, tc.want)
		}
	}
}

// The table is sorted and its ranges neither overlap nor touch, which
// the binary search relies on; every Han and Hiragana letter is wide.
func TestWideRanges(t *testing.T) {
	for i, rg := range wideRanges {
		if rg[0] > rg[1] || i > 0 && rg[0] <= wideRanges[i-1][1]+1 {
			t.Errorf("wideRanges[%d] = %U..%U is out of order after %U..%U", i, rg[0], rg[1], wideRanges[max(0, i-1)][0], wideRanges[max(0, i-1)][1])
		}
	}
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if unicode.In(r, unicode.Han, unicode.Hiragana) && runeWidth(r) != 2 {
			t.Errorf("runeWidth(%U %q) = %d, want 2", r, r, runeWidth(r))
		}
	}
}

func TestCells(t *testing.T) {
	tests := []struct {
		in   string
		n    int
		cut  string // the longest prefix in n cells, a half-cut wide rune padded
		want int    // cells(in)
	}{
		{"abc", 2, "ab", 3},
		{"中文", 3, "中 ", 4},
		{"中文", 4, "中文", 4},
		{"e\u0301x", 1, "e\u0301", 2},
		{"😀a", 1, " ", 3},
		{"", 3, "", 0},
	}
	for _, tc := range tests {
		if got := cells(tc.in); got != tc.want {
			t.Errorf("cells(%q) = %d, want %d", tc.in, got, tc.want)
		}
		if got := fitCells(tc.in, tc.n); got != tc.cut {
			t.Errorf("fitCells(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.cut)
		}
	}
}
