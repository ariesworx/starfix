package board

import (
	"context"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
)

// redraw is how often the board redraws unasked, for its relative times
// and, where no signal says so, a changed terminal size.
const redraw = time.Second

// Terminal is what Run draws on: a Screen's, or a test's.
type Terminal struct {
	// In delivers the keys as the terminal sends them, in raw mode.
	In io.Reader
	// Out takes the frames.
	Out io.Writer
	// Size returns the terminal's width and height.
	Size func() (width, height int)
	// Resized receives when the size changes; nil where nothing says so,
	// and the redraw each second notices instead.
	Resized <-chan os.Signal
	// Color allows color codes.
	Color bool
	// Restore puts the terminal back as it was. Run calls it once, on
	// every way out.
	Restore func()
}

// Run shows the board titled title on t, kept current by live through
// conn and the connections after it, until q, ctx ending, or t's input
// ending. It restores the terminal on its way out, a panic included, in
// its own loop or in the goroutines it starts.
//
// The goroutine reading t.In is not waited for: a read cannot be
// interrupted portably, so it ends at its next read after Run returns,
// or with the process. Everything else Run starts has ended when it
// returns.
func Run(ctx context.Context, title string, t Terminal, live *Live, conn Conn) error {
	var once sync.Once
	restore := func() { once.Do(t.Restore) }
	defer restore()
	// guard restores the terminal before a panic in a goroutine Run
	// started takes the process down with it.
	guard := func() {
		if r := recover(); r != nil {
			restore()
			panic(r)
		}
	}

	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer wg.Wait()
	defer cancel()

	updates := make(chan Update, 64)
	actions := make(chan Action)
	wg.Go(func() {
		defer guard()
		live.Run(ctx, conn, actions, updates)
	})
	keys := make(chan []byte)
	go func() {
		defer guard()
		readKeys(ctx, t.In, keys)
	}()

	m := New(title)
	var last []string
	draw := func() {
		w, h := t.Size()
		m.Resize(w, h)
		frame := m.Frame(w, h, time.Now(), t.Color)
		if slices.Equal(frame, last) {
			return
		}
		last = frame
		_, _ = io.WriteString(t.Out, paint(frame, w))
	}
	draw()

	tick := time.NewTicker(redraw)
	defer tick.Stop()
	var pending []Action // for live, sent as it takes them
	for {
		var out chan<- Action
		var next Action
		if len(pending) > 0 {
			out, next = actions, pending[0]
		}
		select {
		case <-ctx.Done():
			return nil
		case u := <-updates:
			m.Apply(u)
			draw()
		case out <- next:
			pending = pending[1:]
		case b, ok := <-keys:
			if !ok {
				return errors.New("the terminal's input ended")
			}
			for _, k := range Keys(b) {
				a := m.Key(k)
				switch a.Do {
				case DoQuit:
					return nil
				case DoNothing:
				default:
					pending = enqueue(pending, a)
				}
			}
			draw()
		case <-t.Resized:
			draw()
		case <-tick.C:
			draw()
		}
	}
}

// enqueue adds a to the actions waiting for Live, keeping the queue
// short however fast keys come while Live is busy: a refresh already
// waiting covers another, and only the latest open or close of a detail
// matters.
func enqueue(pending []Action, a Action) []Action {
	switch a.Do {
	case DoRefresh:
		if slices.Contains(pending, Action{Do: DoRefresh}) {
			return pending
		}
	case DoShow, DoHide:
		pending = slices.DeleteFunc(pending, func(p Action) bool { return p.Do == DoShow || p.Do == DoHide })
	}
	return append(pending, a)
}

// readKeys sends what in delivers to keys, until in ends, which closes
// keys, or ctx does.
func readKeys(ctx context.Context, in io.Reader, keys chan<- []byte) {
	buf := make([]byte, 256)
	for {
		n, err := in.Read(buf)
		if n > 0 {
			select {
			case keys <- slices.Clone(buf[:n]):
			case <-ctx.Done():
				return
			}
		}
		if err != nil {
			close(keys)
			return
		}
	}
}

// paint draws frame from the top left corner of a terminal width cells
// wide, clearing what each line and the screen below it held before. A
// line that fills the width is not erased to its end: the cursor waits
// on its last cell for the wrap, and xterm would erase that cell.
func paint(frame []string, width int) string {
	var b strings.Builder
	b.WriteString("\x1b[H")
	for i, l := range frame {
		if i > 0 {
			b.WriteString("\r\n")
		}
		b.WriteString(l)
		if visibleWidth(l) < width {
			b.WriteString("\x1b[K")
		}
	}
	b.WriteString("\x1b[J")
	return b.String()
}
