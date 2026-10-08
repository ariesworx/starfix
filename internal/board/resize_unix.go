//go:build unix

package board

import (
	"os"
	"os/signal"
	"syscall"
)

// notifyResize returns a channel that receives when the terminal is
// resized (SIGWINCH), and a func that stops it.
func notifyResize() (<-chan os.Signal, func()) {
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGWINCH)
	return c, func() { signal.Stop(c) }
}
