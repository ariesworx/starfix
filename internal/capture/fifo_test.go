//go:build linux || darwin

package capture

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A transcript path that names a FIFO is refused, not opened: opening it
// would block until a writer came, past any deadline.
func TestClaudeRefusesFIFO(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fifo.jsonl")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := Claude(t.Context(), Input{Session: session, Files: []string{path},
			StateDir: filepath.Join(t.TempDir(), "starfix"), Send: (&sink{}).send})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Errorf("Claude(FIFO) = nil error, want a refusal")
		}
	case <-time.After(5 * time.Second):
		// Unblock the reader, so the goroutine ends before the test.
		if w, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil { //nolint:gosec // the test's FIFO
			_ = w.Close()
		}
		<-done
		t.Fatal("Claude(FIFO) blocked opening it")
	}
}
