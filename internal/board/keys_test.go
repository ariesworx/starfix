package board

import (
	"slices"
	"testing"
)

func TestKeys(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []Key
	}{
		{"letters", "qjkr?", []Key{KeyQuit, KeyDown, KeyUp, KeyRefresh, KeyHelp}},
		{"ctrl-c", "\x03", []Key{KeyQuit}},
		{"enter, cr or lf", "\r\n", []Key{KeyEnter, KeyEnter}},
		{"arrows", "\x1b[A\x1b[B\x1b[C\x1b[D", []Key{KeyUp, KeyDown, KeyEnter, KeyBack}},
		{"application-mode arrows", "\x1bOA\x1bOB", []Key{KeyUp, KeyDown}},
		{"pages, home and end", "\x1b[5~\x1b[6~\x1b[H\x1b[F\x1b[1~\x1b[4~", []Key{KeyPageUp, KeyPageDown, KeyHome, KeyEnd, KeyHome, KeyEnd}},
		{"g and G", "gG", []Key{KeyHome, KeyEnd}},
		{"backspace, and esc alone", "\x7f\x08\x1b", []Key{KeyBack, KeyBack, KeyBack}},
		{"alt and a key is ignored", "\x1bj\x1b\x7fk", []Key{KeyUp}},
		{"unknown sequences are skipped", "\x1b[1;5A\x1b[200~x\x1bOPj", []Key{KeyDown}},
		{"unknown bytes are skipped", "zé\x00j", []Key{KeyDown}},
		{"a cut-off sequence is dropped", "j\x1b[", []Key{KeyDown}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Keys([]byte(tc.in)); !slices.Equal(got, tc.want) {
				t.Errorf("Keys(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// Keys never panics, and decodes at most one key per byte.
func FuzzKeys(f *testing.F) {
	for _, s := range []string{"q", "\x1b[A", "\x1bOB", "\x1b[5~", "\x1b[1;5A", "\x1b", "\x1b[", "\x1bO", "j\x1b[200~k"} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		if keys := Keys(b); len(keys) > len(b) {
			t.Errorf("Keys(%q) = %d keys from %d bytes", b, len(keys), len(b))
		}
	})
}
