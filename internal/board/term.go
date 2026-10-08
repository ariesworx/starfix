package board

import (
	"fmt"
	"io"
	"sync"

	"golang.org/x/term"
)

// fileDescriptor is a stream with a descriptor, such as an *os.File.
type fileDescriptor interface{ Fd() uintptr }

// fd returns v's descriptor, if it has one.
func fd(v any) (int, bool) {
	f, ok := v.(fileDescriptor)
	if !ok {
		return 0, false
	}
	return int(f.Fd()), true //nolint:gosec // a descriptor or handle fits an int on every platform sfx builds for
}

// IsTerminal reports whether in and out are both a terminal, which the
// board needs: keys one at a time from in, frames drawn on out.
func IsTerminal(in io.Reader, out io.Writer) bool {
	i, ok := fd(in)
	if !ok || !term.IsTerminal(i) {
		return false
	}
	o, ok := fd(out)
	return ok && term.IsTerminal(o)
}

// Screen is a terminal taken over for the board: in raw mode, so keys
// arrive one at a time and unechoed, on the alternate screen with the
// cursor hidden. Restore gives it back.
type Screen struct {
	in    io.Reader
	out   io.Writer
	outFd int
	undo  []func()
	once  sync.Once
}

// OpenScreen takes over the terminal that in and out are, which must
// both be one (IsTerminal).
func OpenScreen(in io.Reader, out io.Writer) (*Screen, error) {
	inFd, ok := fd(in)
	outFd, ok2 := fd(out)
	if !ok || !ok2 {
		return nil, fmt.Errorf("open screen: not a terminal")
	}
	s := &Screen{in: in, out: out, outFd: outFd}
	state, err := term.MakeRaw(inFd)
	if err != nil {
		return nil, fmt.Errorf("raw mode: %w", err)
	}
	s.undo = append(s.undo, func() { _ = term.Restore(inFd, state) })
	vt, err := enableEscapes(outFd)
	if err != nil {
		s.Restore()
		return nil, err
	}
	s.undo = append(s.undo, vt)
	_, _ = io.WriteString(out, "\x1b[?1049h\x1b[?25l") // alternate screen, cursor hidden
	s.undo = append(s.undo, func() { _, _ = io.WriteString(out, "\x1b[?25h\x1b[?1049l") })
	return s, nil
}

// Restore leaves the alternate screen, shows the cursor and puts the
// terminal's modes back, in the reverse order they were set. It is safe
// to call more than once.
func (s *Screen) Restore() {
	s.once.Do(func() {
		for i := len(s.undo) - 1; i >= 0; i-- {
			s.undo[i]()
		}
	})
}

// Size returns the terminal's width and height, or 80 by 24 when it
// cannot tell.
func (s *Screen) Size() (int, int) {
	w, h, err := term.GetSize(s.outFd)
	if err != nil || w < 1 || h < 1 {
		return 80, 24
	}
	return w, h
}

// Terminal returns the Terminal for Run on s; color allows color codes.
// stop ends the resize notices; call it once Run returns.
func (s *Screen) Terminal(color bool) (t Terminal, stop func()) {
	resized, stop := notifyResize()
	return Terminal{In: s.in, Out: s.out, Size: s.Size, Resized: resized, Color: color, Restore: s.Restore}, stop
}
