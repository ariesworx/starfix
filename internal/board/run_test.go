package board

import (
	"bytes"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
)

// screen is a fake terminal: keys written to it, output kept, a size a
// test may change, and restores counted.
type screen struct {
	keys     *io.PipeWriter
	in       *io.PipeReader
	mu       sync.Mutex // guards out, w and h
	out      bytes.Buffer
	w, h     int
	resized  chan os.Signal
	restored atomic.Int32
}

func newScreen(w, h int) *screen {
	in, keys := io.Pipe()
	return &screen{keys: keys, in: in, w: w, h: h, resized: make(chan os.Signal, 1)}
}

func (s *screen) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.out.Write(p)
}

func (s *screen) output() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.out.String()
}

func (s *screen) size() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w, s.h
}

func (s *screen) terminal() Terminal {
	return Terminal{In: s.in, Out: s, Size: s.size, Resized: s.resized, Restore: func() { s.restored.Add(1) }}
}

// run starts Run on a fake server and screen; wait returns its error once
// it has ended, and closes the keys so the key reader ends too.
func runBoard(t *testing.T, srv *fakeServer, sc *screen, term Terminal) (wait func() error) {
	t.Helper()
	l := NewLive(srv.dial)
	conn, err := l.Connect(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- Run(t.Context(), "demo", term, l, conn) }()
	return func() error {
		err := <-done
		_ = sc.keys.Close()
		return err
	}
}

// The board draws what the server lists, asks it again on r, redraws at a
// new size, and on q ends and restores the terminal once.
func TestRunDrawsAndQuits(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv, sc := &fakeServer{}, newScreen(80, 12)
		wait := runBoard(t, srv, sc, sc.terminal())
		synctest.Wait()
		if out := sc.output(); !strings.Contains(out, "> sf-r1 P0 ready") || !strings.Contains(out, "Held 1") {
			t.Fatalf("first frames lack the lists:\n%s", out)
		}
		srv.took()

		_, _ = sc.keys.Write([]byte("r"))
		synctest.Wait()
		if got := srv.took(); !slices.Equal(got, refresh) {
			t.Errorf("r made calls %v, want %v", got, refresh)
		}

		sc.mu.Lock()
		sc.w, sc.h = 40, 12
		sc.out.Reset()
		sc.mu.Unlock()
		sc.resized <- os.Interrupt // any signal will do
		synctest.Wait()
		if out := sc.output(); !strings.Contains(out, "Ready 1") || strings.Contains(out, "Ready 1  ") {
			t.Errorf("after a resize to 40 columns the board is not one column:\n%q", out)
		}

		_, _ = sc.keys.Write([]byte("q"))
		if err := wait(); err != nil {
			t.Errorf("Run after q = %v, want nil", err)
		}
		if n := sc.restored.Load(); n != 1 {
			t.Errorf("terminal restored %d times, want once", n)
		}
	})
}

// A panic in the loop restores the terminal before it goes on up.
func TestRunRestoresOnPanic(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv, sc := &fakeServer{}, newScreen(80, 12)
		term := sc.terminal()
		term.Size = func() (int, int) { panic("size failed") }
		l := NewLive(srv.dial)
		conn, err := l.Connect(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		func() {
			defer func() {
				if r := recover(); r != "size failed" {
					t.Errorf("recovered %v, want the loop's panic", r)
				}
			}()
			_ = Run(t.Context(), "demo", term, l, conn)
		}()
		_ = sc.keys.Close()
		if n := sc.restored.Load(); n != 1 {
			t.Errorf("terminal restored %d times after a panic, want once", n)
		}
	})
}

// When the terminal's input ends, the board ends with an error.
func TestRunInputEnds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv, sc := &fakeServer{}, newScreen(80, 12)
		wait := runBoard(t, srv, sc, sc.terminal())
		synctest.Wait()
		_ = sc.keys.Close()
		if err := wait(); err == nil {
			t.Error("Run after its input ended = nil, want an error")
		}
		if n := sc.restored.Load(); n != 1 {
			t.Errorf("terminal restored %d times, want once", n)
		}
	})
}

// Keys pressed while a read is slow do not pile up: refreshes pending
// collapse into one, and only the latest open or close of a detail is
// kept.
func TestRunCoalescesActions(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv, sc := &fakeServer{}, newScreen(80, 12)
		wait := runBoard(t, srv, sc, sc.terminal())
		synctest.Wait()
		srv.took()

		gate := make(chan struct{})
		srv.mu.Lock()
		srv.gate = gate
		srv.mu.Unlock()
		_, _ = sc.keys.Write([]byte("r")) // Live is now held in this refresh
		synctest.Wait()
		_, _ = sc.keys.Write([]byte(strings.Repeat("r", 50) + "\r\x1b[D\r\x1b[D\r"))
		synctest.Wait()
		srv.mu.Lock()
		srv.gate = nil
		srv.mu.Unlock()
		close(gate)
		synctest.Wait()

		want := append(append(slices.Clone(refresh), refresh...), "show sf-r1")
		if got := srv.took(); !slices.Equal(got, want) {
			t.Errorf("after 50 r and three opens and two closes during a slow read, calls = %v, want %v", got, want)
		}
		_, _ = sc.keys.Write([]byte("q"))
		if err := wait(); err != nil {
			t.Fatal(err)
		}
	})
}

// A line that fills the width is not followed by erase-to-end-of-line,
// which on xterm would clear its last cell (the pending wrap); a shorter
// one is.
func TestRunPaintsFullLines(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv, sc := &fakeServer{}, newScreen(30, 6)
		wait := runBoard(t, srv, sc, sc.terminal())
		synctest.Wait()
		out := sc.output()
		if !strings.Contains(out, "live\r\n") || strings.Contains(out, "live\x1b[K") {
			t.Errorf("the full-width header is drawn %q, want it ended by a newline alone", out)
		}
		if !strings.Contains(out, "Ready 1\x1b[K\r\n") {
			t.Errorf("a short line is not erased to its end: %q", out)
		}
		_, _ = sc.keys.Write([]byte("q"))
		if err := wait(); err != nil {
			t.Fatal(err)
		}
	})
}
