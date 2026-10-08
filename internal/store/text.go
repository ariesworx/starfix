package store

import (
	"fmt"

	"github.com/ariesworx/starfix/internal/safetext"
)

// Free text is checked here, one rule for every field (safetext): a
// single-line field (a title, a name, a label, a reason, a handoff's next
// step or path) refuses every control and bidi character, newline and tab
// included; a multi-line field (a body, design, acceptance criteria,
// notes, a comment or handoff note) allows newlines and tabs only. Text
// already stored is not rewritten: clients escape what they print.

// checkLine refuses, with ErrInvalid, a single-line field that is empty
// when required, longer than limit bytes, not UTF-8, or holds an unsafe
// character.
func checkLine(field, v string, limit int, required bool) error {
	switch {
	case v == "" && !required:
		return nil
	case v == "" || len(v) > limit:
		return fmt.Errorf("%w: %s must be 1-%d bytes", ErrInvalid, field, limit)
	case !safetext.ValidLine(v):
		return fmt.Errorf("%w: %s must be one line of UTF-8 without control or bidirectional characters", ErrInvalid, field)
	}
	return nil
}

// checkText is checkLine for a multi-line field: newlines and tabs are
// allowed.
func checkText(field, v string, limit int, required bool) error {
	switch {
	case v == "" && !required:
		return nil
	case v == "" || len(v) > limit:
		return fmt.Errorf("%w: %s must be 1-%d bytes", ErrInvalid, field, limit)
	case !safetext.ValidText(v):
		return fmt.Errorf("%w: %s must be UTF-8 without control or bidirectional characters other than newline and tab", ErrInvalid, field)
	}
	return nil
}

// Field limits, in bytes; the columns hold at least this much.
const (
	maxTitle  = 500
	maxName   = 255
	maxReason = 2000
	maxText   = 65535
)
