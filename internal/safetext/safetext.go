// Package safetext keeps text written by other people from acting on a
// terminal or posing as a program's own output. It is the one definition
// of an unsafe character, used by the store to refuse such text, by the
// importer to clean it, and by every client to escape what it prints.
//
// Unsafe characters are the C0 controls (newline and tab included), DEL,
// the C1 controls, the Unicode bidirectional controls and marks, and the
// line and paragraph separators. Through them, text can clear a screen,
// set the clipboard (OSC 52), hide a link (OSC 8), rewrite a line, reorder
// what a reader sees, or start a line that looks like the program's own.
package safetext

import (
	"bufio"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// Unsafe reports whether r is a control or formatting character that can
// change how text displays.
func Unsafe(r rune) bool {
	switch {
	case r < 0x20, r >= 0x7f && r <= 0x9f: // C0, DEL, C1
		return true
	case r == 0x061c, r == 0x200e, r == 0x200f: // bidi marks
		return true
	case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069: // bidi embeddings, overrides, isolates
		return true
	case r == 0x2028, r == 0x2029: // line and paragraph separators
		return true
	}
	return false
}

// multiline reports whether r is allowed in multi-line text: newline and
// tab.
func multiline(r rune) bool { return r == '\n' || r == '\t' }

// ValidLine reports whether s is valid UTF-8 without unsafe characters:
// text that prints as one line and only as itself.
func ValidLine(s string) bool { return valid(s, false) }

// ValidText is ValidLine for multi-line text: newlines and tabs are
// allowed.
func ValidText(s string) bool { return valid(s, true) }

func valid(s string, lines bool) bool {
	for i, r := range s {
		if r == utf8.RuneError {
			if _, n := utf8.DecodeRuneInString(s[i:]); n == 1 {
				return false
			}
		}
		if Unsafe(r) && (!lines || !multiline(r)) {
			return false
		}
	}
	return true
}

// Line escapes every unsafe character and invalid byte in s, so it prints
// on one line exactly as written: \n, \t, \r, \x1b, \u202e.
func Line(s string) string { return escape(s, false) }

// Text is Line for multi-line text: newlines and tabs are kept.
func Text(s string) string { return escape(s, true) }

func escape(s string, lines bool) string {
	if valid(s, lines) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 16)
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && n == 1:
			fmt.Fprintf(&b, `\x%02x`, s[i])
		case !Unsafe(r) || lines && multiline(r):
			b.WriteString(s[i : i+n])
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\r':
			b.WriteString(`\r`)
		case r < 0x80:
			fmt.Fprintf(&b, `\x%02x`, r)
		default:
			fmt.Fprintf(&b, `\u%04x`, r)
		}
		i += n
	}
	return b.String()
}

// CleanLine makes s safe for a single-line field, for text that must be
// kept rather than refused (an import): invalid bytes become U+FFFD, each
// run of line breaks and tabs one space, and other unsafe characters are
// dropped.
func CleanLine(s string) string { return clean(s, false) }

// CleanText is CleanLine for multi-line text: CRLF becomes a newline, and
// newlines and tabs are kept.
func CleanText(s string) string { return clean(s, true) }

func clean(s string, lines bool) string {
	s = strings.ToValidUTF8(s, "\ufffd")
	if valid(s, lines) {
		return s
	}
	if lines {
		s = strings.ReplaceAll(s, "\r\n", "\n")
	}
	var b strings.Builder
	space := false
	for _, r := range s {
		switch {
		case !Unsafe(r) || lines && multiline(r):
			b.WriteRune(r)
			space = false
		case !lines && (r == '\n' || r == '\r' || r == '\t' || r == 0x2028 || r == 0x2029):
			if !space {
				b.WriteByte(' ')
			}
			space = true
		}
	}
	return b.String()
}

// JSON escapes the unsafe characters in a JSON document as \uXXXX. The
// document stays valid and decodes to the same value: encoding/json
// already escapes the C0 controls, and outside strings JSON has none of
// these characters.
func JSON(doc []byte) []byte {
	if valid(string(doc), false) {
		return doc
	}
	var b strings.Builder
	for _, r := range string(doc) {
		if Unsafe(r) {
			fmt.Fprintf(&b, `\u%04x`, r)
		} else {
			b.WriteRune(r)
		}
	}
	return []byte(b.String())
}

// Writer escapes what is written through it as Text does: newlines and
// tabs pass, every other unsafe character is shown as an escape. A rune
// split across writes is held until it is whole; Flush writes what is
// held.
type Writer struct {
	w    *bufio.Writer
	held []byte
}

// NewWriter returns a Writer to w.
func NewWriter(w io.Writer) *Writer { return &Writer{w: bufio.NewWriter(w)} }

// Write escapes p and writes it. Text up to the last whole rune is written
// before it returns.
func (w *Writer) Write(p []byte) (int, error) {
	data := append(w.held, p...)
	cut := len(data)
	for i := len(data) - 1; i >= 0 && i >= len(data)-utf8.UTFMax; i-- {
		if utf8.RuneStart(data[i]) {
			if !utf8.FullRune(data[i:]) {
				cut = i
			}
			break
		}
	}
	w.held = append([]byte(nil), data[cut:]...)
	if _, err := w.w.WriteString(Text(string(data[:cut]))); err != nil {
		return 0, err
	}
	return len(p), w.w.Flush()
}

// Flush writes any partial rune still held, escaped.
func (w *Writer) Flush() error {
	if len(w.held) > 0 {
		if _, err := w.w.WriteString(Text(string(w.held))); err != nil {
			return err
		}
		w.held = nil
	}
	return w.w.Flush()
}
